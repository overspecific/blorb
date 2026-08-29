// Package openai implements llm.Client for OpenAI-compatible
// /chat/completions servers over plain net/http + JSON.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/overspecific/goblorb/internal/llm"
)

const chatCompletionsPath = "/chat/completions"

// maxErrorBodyLen bounds how much of an error response body is included in
// error messages.
const maxErrorBodyLen = 4 << 10

// Config carries the client's settings.
type Config struct {
	// BaseURL is the API root; /chat/completions is appended.
	BaseURL string
	// Model is the default model sent with each request when the request
	// does not override it.
	Model string
	// APIKey, when non-empty, is sent as a Bearer token.
	APIKey string
	// HTTPClient is optional; http.DefaultClient is used when nil.
	HTTPClient *http.Client
}

// Client implements llm.Client against an OpenAI-compatible server.
type Client struct {
	cfg Config
}

func (c *Client) httpClient() *http.Client {
	if c.cfg.HTTPClient != nil {
		return c.cfg.HTTPClient
	}
	return http.DefaultClient
}

// New returns a Client for the given configuration. It returns an error when
// the base URL is unusable.
func New(cfg Config) (*Client, error) {
	if err := validateBaseURL(cfg.BaseURL); err != nil {
		return nil, err
	}
	return &Client{cfg: cfg}, nil
}

// Chat sends a chat completion request and returns the provider-neutral
// response.
func (c *Client) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	body := wireRequest{
		Model:    firstNonEmpty(req.Model, c.cfg.Model),
		Messages: req.Messages,
		Tools:    wireTools(req.Tools),
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	endpoint, err := url.JoinPath(strings.TrimSuffix(c.cfg.BaseURL, "/"), chatCompletionsPath)
	if err != nil {
		return nil, fmt.Errorf("join base_url %q: %w", c.cfg.BaseURL, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	httpResp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("chat completions: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, httpResp.Body)
		_ = httpResp.Body.Close()
	}()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode > 299 {
		return nil, fmt.Errorf("chat completions: unexpected status %d: %s",
			httpResp.StatusCode, truncate(respBody, maxErrorBodyLen))
	}

	var envelope wireResponse
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, fmt.Errorf("decode response: %w (body: %s)", err, truncate(respBody, maxErrorBodyLen))
	}
	if len(envelope.Choices) == 0 {
		return nil, fmt.Errorf("decode response: no choices in response (body: %s)", truncate(respBody, maxErrorBodyLen))
	}

	choice := envelope.Choices[0]
	resp := &llm.Response{
		ID:           envelope.ID,
		Message:      choice.Message,
		FinishReason: choice.FinishReason,
		Usage:        envelope.Usage,
	}
	for i := range resp.Message.ToolCalls {
		resp.Message.ToolCalls[i].Type = llm.ToolCallType
	}
	if resp.Message.Role == "" {
		resp.Message.Role = llm.RoleAssistant
	}
	if hasToolCallWithoutID(resp.Message.ToolCalls) {
		return nil, fmt.Errorf("decode response: tool call missing id (body: %s)", truncate(respBody, maxErrorBodyLen))
	}
	return resp, nil
}

// wireRequest is the OpenAI-compatible request envelope. Messages reuse the
// neutral llm types, whose JSON shape matches the wire format.
type wireRequest struct {
	Model    string        `json:"model"`
	Messages []llm.Message `json:"messages"`
	Tools    []wireTool    `json:"tools,omitempty"`
}

// wireTool is the OpenAI-compatible tool definition shape, which wraps the
// function fields in a nested object tagged with type "function".
type wireTool struct {
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

func wireTools(tools []llm.Tool) []wireTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]wireTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, wireTool{
			Type: llm.ToolCallType,
			Function: wireFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	return out
}

// wireResponse is the OpenAI-compatible response envelope; these field names
// do not match the neutral types, so server-specific structs are warranted.
type wireResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Message      llm.Message `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage llm.Usage `json:"usage"`
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

func hasToolCallWithoutID(calls []llm.ToolCall) bool {
	for _, tc := range calls {
		if tc.ID == "" {
			return true
		}
	}
	return false
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func truncate(data []byte, limit int) string {
	s := string(data)
	if len(s) > limit {
		return s[:limit] + "...(truncated)"
	}
	return s
}
