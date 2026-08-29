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
	"sync"
	"time"
	"unicode/utf8"

	"github.com/overspecific/blorb/internal/llm"
)

const chatCompletionsPath = "/chat/completions"

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
	// BaseURL is the API root; /chat/completions is appended.
	BaseURL string
	// Model is the default model sent with each request when the request
	// does not override it.
	Model string
	// APIKey, when non-empty, is sent as a Bearer token.
	APIKey string
	// HTTPClient is optional; a client with a 5 minute timeout is used
	// when nil, so a hung server cannot block a turn forever.
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

// Chat sends a chat completion request and returns the provider-neutral
// response.
func (c *Client) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	body := wireRequest{
		Model:    firstNonEmpty(req.Model, c.cfg.Model),
		Messages: wireMessages(req.Messages),
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

	respBody, err := io.ReadAll(io.LimitReader(httpResp.Body, maxBodyLen))
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if len(respBody) == maxBodyLen {
		return nil, fmt.Errorf("read response body: response exceeds %d byte limit", maxBodyLen)
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
		Message:      neutralMessage(choice.Message),
		FinishReason: choice.FinishReason,
		Usage:        envelope.Usage,
	}
	if resp.Message.Role == "" {
		resp.Message.Role = llm.RoleAssistant
	}
	if hasToolCallWithoutID(resp.Message.ToolCalls) {
		return nil, fmt.Errorf("decode response: tool call missing id (body: %s)", truncate(respBody, maxErrorBodyLen))
	}
	if hasToolCallWithoutName(resp.Message.ToolCalls) {
		return nil, fmt.Errorf("decode response: tool call missing name (body: %s)", truncate(respBody, maxErrorBodyLen))
	}
	return resp, nil
}

// wireRequest is the OpenAI-compatible request envelope.
type wireRequest struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
	Tools    []wireTool    `json:"tools,omitempty"`
}

// wireMessage is the OpenAI-compatible message shape. Messages carrying tool
// calls or tool results use the nested forms here; the neutral llm.Message is
// converted to and from this shape by wireMessages and neutralMessage.
//
// Content has no omitempty: servers (llama-server among them) reject
// non-assistant messages that lack a content field altogether, so empty
// content is sent as "".
type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// Reasoning carries server-extracted chain-of-thought. It is decoded
	// from responses and re-sent only with assistant messages that carry
	// tool calls: reasoning exists to keep the model's thinking continuous
	// across tool call rounds, while a final answer's reasoning is stale
	// by the next request and is dropped. See wireMessages.
	Reasoning  string         `json:"reasoning_content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// wireToolCall is the OpenAI-compatible tool call shape: id and type at the
// top level, function name and arguments in a nested "function" object.
type wireToolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function wireToolCallFn `json:"function"`
}

type wireToolCallFn struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// wireMessages converts neutral messages to the wire shape.
func wireMessages(msgs []llm.Message) []wireMessage {
	out := make([]wireMessage, 0, len(msgs))
	for _, m := range msgs {
		// Only tool-call rounds carry reasoning back to the server. A
		// final answer's thinking served its purpose and resending it
		// pollutes the next prompt (some servers reject it outright).
		reasoning := m.Reasoning
		if len(m.ToolCalls) == 0 {
			reasoning = ""
		}
		wm := wireMessage{Role: string(m.Role), Content: m.Content, Reasoning: reasoning, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
				ID:       tc.ID,
				Type:     llm.ToolCallType,
				Function: wireToolCallFn{Name: tc.FunctionName, Arguments: tc.FunctionArgs},
			})
		}
		out = append(out, wm)
	}
	return out
}

// neutralMessage converts a wire message back to the neutral shape.
func neutralMessage(wm wireMessage) llm.Message {
	m := llm.Message{Role: llm.Role(wm.Role), Content: wm.Content, Reasoning: wm.Reasoning, ToolCallID: wm.ToolCallID}
	for _, wtc := range wm.ToolCalls {
		callType := wtc.Type
		if callType == "" {
			callType = llm.ToolCallType
		}
		m.ToolCalls = append(m.ToolCalls, llm.ToolCall{
			ID:           wtc.ID,
			Type:         callType,
			FunctionName: wtc.Function.Name,
			FunctionArgs: wtc.Function.Arguments,
		})
	}
	return m
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
		Message      wireMessage `json:"message"`
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

// hasToolCallWithoutID reports whether any tool call is missing its id.
func hasToolCallWithoutID(calls []llm.ToolCall) bool {
	for _, tc := range calls {
		if tc.ID == "" {
			return true
		}
	}
	return false
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

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
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
