package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/logging"
)

const chatPath = "/api/chat"

// maxErrorBodyLen bounds how much of an error response body is included in
// error messages.
const maxErrorBodyLen = 4 << 10

// maxBodyLen bounds how much of any response body is read into memory at
// all; beyond this the read is cut short and surfaces as an error.
const maxBodyLen = 16 << 20

// defaultTimeout bounds each request when HTTPClient is nil, so a hung
// server cannot block a turn forever.
const defaultTimeout = 5 * time.Minute

// Config carries the client's settings.
type Config struct {
	// BaseURL is the Ollama server root, e.g. http://localhost:11434;
	// /api/chat is appended.
	BaseURL string
	// Model is the default model sent with each request when the request
	// does not override it.
	Model string
	// ReasoningEffort maps onto the request's think field: empty omits it
	// (server default), "none" becomes think: false (Ollama rejects the
	// string "none"), and every other value passes through verbatim as
	// the level string. Which values a given model accepts is the
	// server's business.
	ReasoningEffort string
	// APIKey, when non-empty, is sent as a Bearer token. Ollama cloud
	// needs it; local Ollama ignores the header.
	APIKey string
	// HTTPClient is optional; a client with a 5 minute timeout is used
	// when nil, so a hung server cannot block a turn forever.
	HTTPClient *http.Client
	// Sink, when non-nil, receives one KindLLMRequest record per call
	// and one KindLLMResponse record per response (see logging.Sink).
	// Sink write errors are ignored: logging is best-effort.
	Sink logging.Sink
}

// Client implements llm.Client against Ollama's native /api/chat API.
type Client struct {
	cfg Config
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

// defaultHTTPClient caches the fallback client with its timeout.
var (
	defaultHTTPClient     *http.Client
	defaultHTTPClientOnce sync.Once
)

// New returns a Client for the given configuration. It returns an error when
// the base URL is unusable.
func New(cfg Config) (*Client, error) {
	if err := validateBaseURL(cfg.BaseURL); err != nil {
		return nil, err
	}
	return &Client{cfg: cfg}, nil
}

// Chat sends a chat request and returns the provider-neutral response. The
// non-streaming Ollama response is a single JSON object, which the decoder
// accepts as-is.
func (c *Client) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	wire, err := wireRequest(req, c.cfg.Model, c.cfg.ReasoningEffort)
	if err != nil {
		return nil, err
	}

	httpResp, endpoint, err := c.post(ctx, wire)
	if err != nil {
		return nil, err
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

	// Logged before the status check so non-2xx responses are logged
	// with their body too. Write errors are ignored.
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
		return nil, fmt.Errorf("ollama chat: unexpected status %d: %s",
			httpResp.StatusCode, truncate(respBody, maxErrorBodyLen))
	}

	var chunk chatResponse
	dec := json.NewDecoder(bytes.NewReader(respBody))
	if err := dec.Decode(&chunk); err != nil {
		return nil, fmt.Errorf("decode response: %w (body: %s)", err, truncate(respBody, maxErrorBodyLen))
	}
	if !chunk.Done {
		return nil, fmt.Errorf("decode response: server did not finish the response (done is false) (body: %s)",
			truncate(respBody, maxErrorBodyLen))
	}
	resp := neutralResponse(&chunk)
	if hasToolCallWithoutName(resp.Message.ToolCalls) {
		return nil, fmt.Errorf("decode response: tool call missing name (body: %s)", truncate(respBody, maxErrorBodyLen))
	}
	return resp, nil
}

// post marshals the wire request and POSTs it to /api/chat. The caller owns
// the response body. Shared by the Chat and ChatStream paths so their
// request building cannot drift. The final endpoint URL is returned
// alongside the response so callers can use it in response log records.
func (c *Client) post(ctx context.Context, body chatRequest) (*http.Response, string, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, "", fmt.Errorf("marshal request: %w", err)
	}

	endpoint, err := url.JoinPath(strings.TrimSuffix(c.cfg.BaseURL, "/"), chatPath)
	if err != nil {
		return nil, "", fmt.Errorf("join base_url %q: %w", c.cfg.BaseURL, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, "", fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	// The record owns its body and header copies: the transport may
	// mutate both. Write errors are ignored — logging is best-effort.
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
		// A keep-alive connection the server closed right after its
		// previous response — before the transport noticed while it sat
		// idle — surfaces as io.EOF here, and Go only auto-retries its
		// own server-closed-idle sentinel, not this in-flight close.
		// The request never reached the server, so send it once more.
		if !errors.Is(err, io.EOF) {
			return nil, "", fmt.Errorf("ollama chat: %w", err)
		}
		body, getErr := httpReq.GetBody()
		if getErr != nil {
			return nil, "", fmt.Errorf("ollama chat: retry unavailable: %w", err)
		}
		httpReq.Body = body
		httpResp, err = c.httpClient().Do(httpReq)
		if err != nil {
			return nil, "", fmt.Errorf("ollama chat: %w", err)
		}
	}
	return httpResp, endpoint, nil
}

// cloneHeader deep-copies a header map so a log record is immune to later
// mutation by the HTTP client.
func cloneHeader(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, v := range h {
		out[textproto.CanonicalMIMEHeaderKey(k)] = append([]string(nil), v...)
	}
	return out
}

// responseHeaders builds the header map for a response log record: a copy
// of the response headers plus a synthetic Status key carrying the status
// code (not a header, but it belongs in the log's header block).
func responseHeaders(resp *http.Response) map[string][]string {
	out := cloneHeader(resp.Header)
	out["Status"] = []string{fmt.Sprint(resp.StatusCode)}
	return out
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

// hasToolCallWithoutName reports whether any tool call is missing its
// function name. Degenerate tool calls like these occasionally come from
// smaller models; using them would poison the conversation history (the
// follow-up request with the empty-name call in it is rejected by servers).
func hasToolCallWithoutName(calls []llm.ToolCall) bool {
	for _, tc := range calls {
		if tc.FunctionName == "" {
			return true
		}
	}
	return false
}

func truncate(data []byte, limit int) string {
	s := string(data)
	if len(s) > limit {
		for limit > 0 && !utf8.RuneStart(s[limit]) {
			limit--
		}
		return s[:limit] + "...(truncated)"
	}
	return s
}
