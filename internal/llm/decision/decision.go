// Package decision implements llm.DeciderClient for the System One wire
// protocol: POST {base_url}/systemone, the shape TypeSafe, OpenRouter,
// Venice and other vendors share.
//
// The request body carries the state and typed questions verbatim; the
// response carries one typed answer per question plus token usage.
package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/logging"
)

const systemOnePath = "/systemone"

// maxErrorBodyLen bounds how much of an error response body is included in
// error messages.
const maxErrorBodyLen = 4 << 10

// maxBodyLen bounds how much of any response body is read into memory at
// all; beyond this the read is cut short and surfaces as an error.
const maxBodyLen = 16 << 20

// defaultTimeout bounds each request when HTTPClient is nil, so a hung
// server cannot block a call forever.
const defaultTimeout = 5 * time.Minute

// Config configures a Client.
type Config struct {
	// BaseURL is the API root; /systemone is appended.
	BaseURL string
	// Model is the model id sent in the request body; empty omits
	// the field and the server applies its default.
	Model string
	// APIKey is sent as a Bearer token; empty sends no header.
	APIKey string
	// HTTPClient overrides the transport; nil uses a shared default.
	HTTPClient *http.Client
	// Sink receives wire logs; nil disables them.
	Sink logging.Sink
}

// Client implements llm.DeciderClient against a System One server.
type Client struct {
	cfg Config
}

var _ llm.DeciderClient = (*Client)(nil)

// New returns a Client for the given configuration. It returns an error
// when the base URL is unusable.
func New(cfg Config) (*Client, error) {
	if err := validateBaseURL(cfg.BaseURL); err != nil {
		return nil, err
	}
	return &Client{cfg: cfg}, nil
}

func (c *Client) httpClient() *http.Client {
	if c.cfg.HTTPClient != nil {
		return c.cfg.HTTPClient
	}
	defaultHTTPClientOnce.Do(func() {
		defaultHTTPClient = &http.Client{Timeout: defaultTimeout}
	})
	return defaultHTTPClient
}

var (
	defaultHTTPClient     *http.Client
	defaultHTTPClientOnce sync.Once
)

// wireRequest is the System One request envelope. State and each
// question's instructions and criteria are raw JSON passed through
// verbatim.
type wireRequest struct {
	Model     string                          `json:"model,omitempty"`
	State     json.RawMessage                 `json:"state"`
	Questions map[string]llm.DecisionQuestion `json:"questions"`
}

// wireResponse is the System One response envelope.
type wireResponse struct {
	Model   string                        `json:"model"`
	Answers map[string]llm.DecisionAnswer `json:"answers"`
	Usage   wireUsage                     `json:"usage"`
}

// wireUsage is the wire token-usage shape.
type wireUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// wireError is the System One error envelope.
type wireError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Decide evaluates the request against the server and returns the typed
// answers.
func (c *Client) Decide(ctx context.Context, req llm.DecisionRequest) (*llm.DecisionResponse, error) {
	body := wireRequest{
		Model:     firstNonEmpty(req.Model, c.cfg.Model),
		State:     req.State,
		Questions: req.Questions,
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	endpoint, err := url.JoinPath(strings.TrimSuffix(c.cfg.BaseURL, "/"), systemOnePath)
	if err != nil {
		return nil, fmt.Errorf("join base_url %q: %w", c.cfg.BaseURL, err)
	}

	startedAt := time.Now()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	if c.cfg.Sink != nil {
		c.cfg.Sink.Write(logging.Record{
			Time:    time.Now(),
			Kind:    logging.KindLLMRequest,
			Method:  http.MethodPost,
			URL:     endpoint,
			Headers: cloneHeader(httpReq.Header),
			Body:    append([]byte(nil), payload...),
		})
	}

	httpResp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("systemone: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, httpResp.Body)
		_ = httpResp.Body.Close()
	}()

	respBody, err := io.ReadAll(io.LimitReader(httpResp.Body, maxBodyLen))
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if len(respBody) == maxBodyLen {
		return nil, fmt.Errorf("read response body: response exceeds %d byte limit", maxBodyLen)
	}
	// The clock stops once the body has been fully received; parsing
	// below is local work, not part of the call.
	elapsed := time.Since(startedAt)

	if c.cfg.Sink != nil {
		c.cfg.Sink.Write(logging.Record{
			Time:    time.Now(),
			Kind:    logging.KindLLMResponse,
			Method:  http.MethodPost,
			URL:     endpoint,
			Headers: responseHeaders(httpResp),
			Body:    append([]byte(nil), respBody...),
		})
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode > 299 {
		return nil, statusError(httpResp.StatusCode, respBody)
	}

	var envelope wireResponse
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, fmt.Errorf("decode response: %w (body: %s)", err, truncate(respBody, maxErrorBodyLen))
	}

	usage := llm.Usage{
		PromptTokens:     envelope.Usage.InputTokens,
		CompletionTokens: envelope.Usage.OutputTokens,
		TotalTokens:      envelope.Usage.InputTokens + envelope.Usage.OutputTokens,
	}
	return &llm.DecisionResponse{
		Model:   envelope.Model,
		Answers: envelope.Answers,
		Usage:   usage,
		Stats:   llm.CallStats{Elapsed: elapsed},
	}, nil
}

// statusError builds an error for a non-2xx response, preferring the
// System One error envelope and falling back to the status line.
func statusError(status int, body []byte) error {
	var wire wireError
	if err := json.Unmarshal(body, &wire); err == nil && (wire.Type != "" || wire.Message != "") {
		if wire.Type != "" && wire.Message != "" {
			return fmt.Errorf("systemone: unexpected status %d: %s: %s", status, wire.Type, wire.Message)
		}
		return fmt.Errorf("systemone: unexpected status %d: %s%s", status, wire.Type, wire.Message)
	}
	return fmt.Errorf("systemone: unexpected status %d: %s", status, truncate(body, maxErrorBodyLen))
}

func validateBaseURL(baseURL string) error {
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("parse base_url %q: %w", baseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("base_url %q must use http or https scheme", baseURL)
	}
	if u.Host == "" {
		return fmt.Errorf("base_url %q must include a host", baseURL)
	}
	return nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func cloneHeader(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, v := range h {
		out[textproto.CanonicalMIMEHeaderKey(k)] = append([]string(nil), v...)
	}
	return out
}

func responseHeaders(resp *http.Response) map[string][]string {
	out := cloneHeader(resp.Header)
	out["Status"] = []string{fmt.Sprint(resp.StatusCode)}
	return out
}

func truncate(data []byte, limit int) string {
	s := string(data)
	if len(s) > limit {
		return s[:limit] + "...(truncated)"
	}
	return s
}
