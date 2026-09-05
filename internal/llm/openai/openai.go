// Package openai implements llm.Client for OpenAI-compatible
// /chat/completions servers over plain net/http + JSON.
package openai

import (
	"bufio"
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
	// ReasoningEffort, when non-empty, is sent as the request's
	// reasoning_effort field, asking the backend for a given thinking
	// effort. Empty omits the field and the server default applies. It is
	// a per-model setting, not a per-request override. Which values a
	// given model accepts is the server's business.
	ReasoningEffort string
	// HTTPClient is optional; a client with a 5 minute timeout is used
	// when nil, so a hung server cannot block a turn forever.
	HTTPClient *http.Client
	// Sink, when non-nil, receives one KindLLMRequest record per call
	// and one KindLLMResponse record per response (see logging.Sink).
	// Sink write errors are ignored: logging is best-effort.
	Sink logging.Sink
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
		Model:           firstNonEmpty(req.Model, c.cfg.Model),
		Messages:        wireMessages(req.Messages),
		Tools:           wireTools(req.Tools),
		ReasoningEffort: c.cfg.ReasoningEffort,
	}

	httpResp, endpoint, err := c.post(ctx, body)
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

// wireStreamChunk is one SSE chunk from a streaming chat completion.
type wireStreamChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Delta struct {
			Role string `json:"role"`
			// Reasoning holds server-extracted chain-of-thought
			// fragments (the reasoning_content field).
			Reasoning string          `json:"reasoning_content"`
			Content   string          `json:"content"`
			ToolCalls []wireToolDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage llm.Usage `json:"usage"`
}

// wireToolDelta is the streaming variant of a tool call fragment, keyed by
// index rather than carrying a complete call.
type wireToolDelta struct {
	Index    int            `json:"index"`
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function wireToolCallFn `json:"function"`
}

// maxContentLen bounds how much content is accumulated across a stream, so
// a runaway stream cannot exhaust memory; beyond this the stream errors.
const maxContentLen = maxBodyLen

// streamAccumulator folds stream chunks into the complete neutral response.
type streamAccumulator struct {
	id           string
	finishReason string
	usage        llm.Usage
	content      strings.Builder
	reasoning    strings.Builder
	toolCalls    map[int]*wireToolCall
	order        []int
}

func newStreamAccumulator() *streamAccumulator {
	return &streamAccumulator{toolCalls: map[int]*wireToolCall{}}
}

// addDelta folds one wire chunk into the accumulation and emits its deltas
// via onDelta. An onDelta error aborts the stream and is returned.
func (a *streamAccumulator) addDelta(chunk *wireStreamChunk, onDelta func(llm.Delta) error) error {
	if chunk.ID != "" && a.id == "" {
		a.id = chunk.ID
	}
	if len(chunk.Choices) > 0 {
		choice := chunk.Choices[0]
		delta := choice.Delta

		if delta.Reasoning != "" {
			if a.reasoning.Len()+len(delta.Reasoning) > maxContentLen {
				return fmt.Errorf("read stream: reasoning exceeds %d byte limit", maxContentLen)
			}
			a.reasoning.WriteString(delta.Reasoning)
			if err := onDelta(llm.Delta{Reasoning: delta.Reasoning}); err != nil {
				return err
			}
		}
		if delta.Content != "" {
			if a.content.Len()+len(delta.Content) > maxContentLen {
				return fmt.Errorf("read stream: content exceeds %d byte limit", maxContentLen)
			}
			a.content.WriteString(delta.Content)
			if err := onDelta(llm.Delta{Content: delta.Content}); err != nil {
				return err
			}
		}
		for i := range delta.ToolCalls {
			wtd := &delta.ToolCalls[i]
			tc, ok := a.toolCalls[wtd.Index]
			if !ok {
				if len(wtd.Function.Arguments) > maxContentLen {
					return fmt.Errorf("read stream: tool call arguments exceed %d byte limit", maxContentLen)
				}
				tc = &wireToolCall{ID: wtd.ID, Type: wtd.Type, Function: wireToolCallFn{Name: wtd.Function.Name, Arguments: wtd.Function.Arguments}}
				a.toolCalls[wtd.Index] = tc
				a.order = append(a.order, wtd.Index)
			} else {
				// Fragments arrive in order for a given index; id,
				// type, and name are typically complete in the first
				// fragment, while arguments concatenate in order.
				if wtd.ID != "" {
					tc.ID = wtd.ID
				}
				if wtd.Type != "" {
					tc.Type = wtd.Type
				}
				if wtd.Function.Name != "" {
					tc.Function.Name = wtd.Function.Name
				}
				if len(tc.Function.Arguments)+len(wtd.Function.Arguments) > maxContentLen {
					return fmt.Errorf("read stream: tool call arguments exceed %d byte limit", maxContentLen)
				}
				tc.Function.Arguments += wtd.Function.Arguments
			}
			if err := onDelta(llm.Delta{ToolCall: &llm.ToolCallDelta{
				Index:     wtd.Index,
				ID:        wtd.ID,
				Type:      wtd.Type,
				Name:      wtd.Function.Name,
				Arguments: wtd.Function.Arguments,
			}}); err != nil {
				return err
			}
		}

		if choice.FinishReason != "" {
			a.finishReason = choice.FinishReason
		}
	}
	if chunk.Usage != (llm.Usage{}) {
		a.usage = chunk.Usage
	}
	return nil
}

// response builds the complete neutral response from what accumulated.
func (a *streamAccumulator) response() *llm.Response {
	msg := llm.Message{Role: llm.RoleAssistant, Content: a.content.String(), Reasoning: a.reasoning.String()}
	for _, idx := range a.order {
		wtc := a.toolCalls[idx]
		callType := wtc.Type
		if callType == "" {
			callType = llm.ToolCallType
		}
		msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{
			ID:           wtc.ID,
			Type:         callType,
			FunctionName: wtc.Function.Name,
			FunctionArgs: wtc.Function.Arguments,
		})
	}
	return &llm.Response{ID: a.id, Message: msg, FinishReason: a.finishReason, Usage: a.usage}
}

// marshalWireResponse renders a completed neutral response back into the
// OpenAI wire response envelope, so streaming and non-streaming
// llm-response log records carry directly comparable bodies.
//
// The message is built directly rather than via wireMessages: the log is
// full-fidelity — what the model actually produced — and deliberately
// keeps Reasoning even for a final answer without tool calls, where
// wireMessages would drop it per its request-side re-send rule (a final
// answer's reasoning is stale by the next *request*, but the *log* must
// record it).
func marshalWireResponse(resp *llm.Response) ([]byte, error) {
	envelope := wireResponse{
		ID:    resp.ID,
		Usage: resp.Usage,
	}
	envelope.Choices = append(envelope.Choices, struct {
		Message      wireMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	}{
		Message:      wireMessageForLog(resp.Message),
		FinishReason: resp.FinishReason,
	})
	return json.Marshal(envelope)
}

// wireMessageForLog converts a neutral message to the wire shape without
// the request-side reasoning rule: reasoning is kept unconditionally.
func wireMessageForLog(m llm.Message) wireMessage {
	wm := wireMessage{Role: string(m.Role), Content: m.Content, Reasoning: m.Reasoning, ToolCallID: m.ToolCallID}
	for _, tc := range m.ToolCalls {
		wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
			ID:       tc.ID,
			Type:     llm.ToolCallType,
			Function: wireToolCallFn{Name: tc.FunctionName, Arguments: tc.FunctionArgs},
		})
	}
	return wm
}

// ChatStream sends a chat completion request with stream:true and reads the
// response as a Server-Sent Events stream, calling onDelta for each
// incremental piece of content, reasoning, or tool call as it arrives (in
// order; see llm.Delta). It returns the complete response once the stream
// finishes. An onDelta error aborts the stream and is returned. It
// implements llm.StreamingClient.
func (c *Client) ChatStream(ctx context.Context, req llm.Request, onDelta func(llm.Delta) error) (*llm.Response, error) {
	body := wireRequest{
		Model:           firstNonEmpty(req.Model, c.cfg.Model),
		Messages:        wireMessages(req.Messages),
		Tools:           wireTools(req.Tools),
		Stream:          true,
		StreamOptions:   &streamOptions{IncludeUsage: true},
		ReasoningEffort: c.cfg.ReasoningEffort,
	}

	httpResp, endpoint, err := c.post(ctx, body)
	if err != nil {
		return nil, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, httpResp.Body)
		_ = httpResp.Body.Close()
	}()

	if httpResp.StatusCode < 200 || httpResp.StatusCode > 299 {
		respBody, err := io.ReadAll(io.LimitReader(httpResp.Body, maxBodyLen))
		if err != nil {
			return nil, fmt.Errorf("read response body: %w", err)
		}
		if len(respBody) == maxBodyLen {
			return nil, fmt.Errorf("read response body: response exceeds %d byte limit", maxBodyLen)
		}
		// Logged here rather than via the assembled response: this body
		// was already read whole. Write errors are ignored.
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
		return nil, fmt.Errorf("chat completions: unexpected status %d: %s",
			httpResp.StatusCode, truncate(respBody, maxErrorBodyLen))
	}

	return c.readStream(httpResp, endpoint, onDelta)
}

// readStream consumes an SSE body, emitting deltas and accumulating the
// complete response. It errors when the stream carries no data chunks at
// all: an empty stream would otherwise surface as a silent empty answer.
//
// The response log record is written here, where the accumulator's
// assembled response exists: the assembled response on the happy path, the
// partial response on abort/error paths after data arrived, and an
// "error: ..." body when the stream carried no data at all. Write errors
// are ignored — logging is best-effort.
func (c *Client) readStream(httpResp *http.Response, endpoint string, onDelta func(llm.Delta) error) (*llm.Response, error) {
	logResponse := func(body []byte) {
		if c.cfg.Sink == nil {
			return
		}
		c.cfg.Sink.Write(logging.Record{
			Time:    time.Now(),
			Kind:    logging.KindLLMResponse,
			Method:  http.MethodPost,
			URL:     endpoint,
			Headers: responseHeaders(httpResp),
			Body:    body,
		})
	}

	acc := newStreamAccumulator()
	sawData := false

	fail := func(err error) (*llm.Response, error) {
		// Log what the model had produced so far; with no data at all
		// there is nothing assembled, so log the error itself.
		if !sawData {
			logResponse([]byte("error: " + err.Error()))
			return nil, err
		}
		resp := acc.response()
		body, marshalErr := marshalWireResponse(resp)
		if marshalErr != nil {
			body = []byte("error: " + marshalErr.Error())
		}
		logResponse(body)
		return nil, err
	}

	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxBodyLen)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}

		var chunk wireStreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// Not valid JSON (e.g. a keepalive or comment payload);
			// skip it rather than aborting the stream.
			continue
		}
		sawData = true

		if err := acc.addDelta(&chunk, onDelta); err != nil {
			return fail(err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fail(fmt.Errorf("read stream: %w", err))
	}
	if !sawData {
		return fail(fmt.Errorf("decode response: no data in stream"))
	}

	resp := acc.response()
	if hasToolCallWithoutID(resp.Message.ToolCalls) {
		return fail(fmt.Errorf("decode response: tool call missing id"))
	}
	if hasToolCallWithoutName(resp.Message.ToolCalls) {
		return fail(fmt.Errorf("decode response: tool call missing name"))
	}

	// A marshal failure still leaves a record: error: <marshal error>.
	body, err := marshalWireResponse(resp)
	if err != nil {
		logResponse([]byte("error: " + err.Error()))
	} else {
		logResponse(body)
	}
	return resp, nil
}

// wireRequest is the OpenAI-compatible request envelope.
type wireRequest struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
	Tools    []wireTool    `json:"tools,omitempty"`
	// Stream and StreamOptions are set only by the streaming path; Chat
	// leaves them unset so the server returns a whole-message response.
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
	// ReasoningEffort, when non-empty, asks the backend for a given
	// thinking effort; empty omits the field and the server default
	// applies. Populated from Config on both the Chat and ChatStream
	// paths, which share request building.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// streamOptions carries per-server streaming behaviour. IncludeUsage asks
// the server to append a final chunk carrying token usage.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// post marshals the wire request and POSTs it to /chat/completions. The
// caller owns the response body. Shared by the Chat and ChatStream paths so
// their request building cannot drift. The final endpoint URL is returned
// alongside the response so callers can use it in response log records.
func (c *Client) post(ctx context.Context, body wireRequest) (*http.Response, string, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, "", fmt.Errorf("marshal request: %w", err)
	}

	endpoint, err := url.JoinPath(strings.TrimSuffix(c.cfg.BaseURL, "/"), chatCompletionsPath)
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
			return nil, "", fmt.Errorf("chat completions: %w", err)
		}
		body, getErr := httpReq.GetBody()
		if getErr != nil {
			return nil, "", fmt.Errorf("chat completions: retry unavailable: %w", err)
		}
		httpReq.Body = body
		httpResp, err = c.httpClient().Do(httpReq)
		if err != nil {
			return nil, "", fmt.Errorf("chat completions: %w", err)
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
