package ollama

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

// Client satisfies both provider seams.
var (
	_ llm.Client          = (*Client)(nil)
	_ llm.StreamingClient = (*Client)(nil)
)

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

	httpResp, endpoint, startedAt, err := c.post(ctx, wire)
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
	// The clock stops once the body has been fully received; parsing
	// below is local work, not part of the call.
	elapsed := time.Since(startedAt)

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
	resp.Stats = llm.CallStats{Output: resp.Message.OutputBytes(), Elapsed: elapsed}
	return resp, nil
}

// maxContentLen bounds how much content is accumulated across a stream, so
// a runaway stream cannot exhaust memory; beyond this the stream errors.
const maxContentLen = maxBodyLen

// streamAccumulator folds NDJSON stream chunks into the complete neutral
// response.
type streamAccumulator struct {
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
func (a *streamAccumulator) addDelta(chunk *chatResponse, onDelta func(llm.Delta) error) error {
	if chunk.Message.Thinking != "" {
		if a.reasoning.Len()+len(chunk.Message.Thinking) > maxContentLen {
			return fmt.Errorf("read stream: reasoning exceeds %d byte limit", maxContentLen)
		}
		a.reasoning.WriteString(chunk.Message.Thinking)
		if err := onDelta(llm.Delta{Reasoning: chunk.Message.Thinking}); err != nil {
			return err
		}
	}
	if chunk.Message.Content != "" {
		if a.content.Len()+len(chunk.Message.Content) > maxContentLen {
			return fmt.Errorf("read stream: content exceeds %d byte limit", maxContentLen)
		}
		a.content.WriteString(chunk.Message.Content)
		if err := onDelta(llm.Delta{Content: chunk.Message.Content}); err != nil {
			return err
		}
	}
	for i := range chunk.Message.ToolCalls {
		wtc := &chunk.Message.ToolCalls[i]
		args := string(wtc.Function.Arguments)
		// Ollama delivers a message's tool calls within a single chunk,
		// so the index is the call's position within that chunk's array;
		// streamed fragments for the same index accumulate like openai's:
		// the first establishes id and name, later ones concatenate
		// arguments.
		idx := i
		tc, ok := a.toolCalls[idx]
		if !ok {
			if len(args) > maxContentLen {
				return fmt.Errorf("read stream: tool call arguments exceed %d byte limit", maxContentLen)
			}
			a.toolCalls[idx] = &wireToolCall{
				ID: wtc.ID,
				Function: wireFnCall{
					Name:      wtc.Function.Name,
					Arguments: json.RawMessage(args),
				},
			}
			a.order = append(a.order, idx)
		} else {
			if wtc.ID != "" {
				tc.ID = wtc.ID
			}
			if wtc.Function.Name != "" {
				tc.Function.Name = wtc.Function.Name
			}
			if len(string(tc.Function.Arguments))+len(args) > maxContentLen {
				return fmt.Errorf("read stream: tool call arguments exceed %d byte limit", maxContentLen)
			}
			tc.Function.Arguments = append(tc.Function.Arguments, args...)
		}
		if err := onDelta(llm.Delta{ToolCall: &llm.ToolCallDelta{
			Index:     idx,
			ID:        wtc.ID,
			Type:      llm.ToolCallType,
			Name:      wtc.Function.Name,
			Arguments: args,
		}}); err != nil {
			return err
		}
	}
	if chunk.Done {
		a.finishReason = mapFinishReason(chunk.DoneReason)
		a.usage = llm.Usage{
			PromptTokens:     chunk.PromptEvalCount,
			CompletionTokens: chunk.EvalCount,
			TotalTokens:      chunk.PromptEvalCount + chunk.EvalCount,
		}
	}
	return nil
}

// response builds the complete neutral response from what accumulated.
func (a *streamAccumulator) response() *llm.Response {
	msg := llm.Message{Role: llm.RoleAssistant, Content: a.content.String(), Reasoning: a.reasoning.String()}
	for _, idx := range a.order {
		wtc := a.toolCalls[idx]
		args := string(wtc.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		id := wtc.ID
		if id == "" {
			id = "call_" + randomHex()
		}
		msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{
			ID:           id,
			Type:         llm.ToolCallType,
			FunctionName: wtc.Function.Name,
			FunctionArgs: args,
		})
	}
	return &llm.Response{Message: msg, FinishReason: a.finishReason, Usage: a.usage}
}

// marshalWireResponse renders a completed neutral response back into a
// synthetic final chatResponse, so streaming and non-streaming
// llm-response log records carry directly comparable bodies.
//
// The message is built directly rather than via wireRequest: the log is
// full-fidelity — what the model actually produced — and deliberately
// keeps Thinking even though wireRequest would never re-send it (a final
// answer's reasoning is stale by the next *request*, but the *log* must
// record it).
func marshalWireResponse(resp *llm.Response) ([]byte, error) {
	msg := wireMessage{Role: string(resp.Message.Role), Content: resp.Message.Content, Thinking: resp.Message.Reasoning}
	for _, tc := range resp.Message.ToolCalls {
		args := json.RawMessage(tc.FunctionArgs)
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		msg.ToolCalls = append(msg.ToolCalls, wireToolCall{
			ID: tc.ID,
			Function: wireFnCall{
				Name:      tc.FunctionName,
				Arguments: args,
			},
		})
	}
	chunk := chatResponse{
		Message:         msg,
		Done:            true,
		DoneReason:      mapWireFinishReason(resp.FinishReason),
		PromptEvalCount: resp.Usage.PromptTokens,
		EvalCount:       resp.Usage.CompletionTokens,
	}
	return json.Marshal(chunk)
}

// mapWireFinishReason inverts mapFinishReason for the assembled log
// record; unknown values pass through unchanged.
func mapWireFinishReason(finishReason string) string {
	switch finishReason {
	case llm.FinishLength:
		return "length"
	case llm.FinishToolCalls:
		return "tool_calls"
	default:
		return finishReason
	}
}

// ChatStream sends a chat request with stream:true and reads the response
// as an NDJSON stream — one chatResponse object per line, terminating with
// a done:true line — calling onDelta for each incremental piece of
// content, reasoning, or tool call as it arrives (in order; see llm.Delta).
// It returns the complete response once the stream finishes. An onDelta
// error aborts the stream and is returned. It implements
// llm.StreamingClient.
func (c *Client) ChatStream(ctx context.Context, req llm.Request, onDelta func(llm.Delta) error) (*llm.Response, error) {
	wire, err := wireRequest(req, c.cfg.Model, c.cfg.ReasoningEffort)
	if err != nil {
		return nil, err
	}
	wire.Stream = true

	httpResp, endpoint, startedAt, err := c.post(ctx, wire)
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
		return nil, fmt.Errorf("ollama chat: unexpected status %d: %s",
			httpResp.StatusCode, truncate(respBody, maxErrorBodyLen))
	}

	return c.readStream(httpResp, endpoint, startedAt, onDelta)
}

// readStream consumes an NDJSON body, emitting deltas and accumulating the
// complete response. It errors when the stream carries no parseable chunks
// at all: an empty stream would otherwise surface as a silent empty answer.
//
// startedAt is the clock start taken by post; the clock stops when the
// stream has been fully read, to measure the call's elapsed time.
//
// The response log record is written here, where the accumulator's
// assembled response exists: the assembled response on the happy path, the
// partial response on abort/error paths after data arrived, and an
// "error: ..." body when the stream carried no data at all. Write errors
// are ignored — logging is best-effort.
func (c *Client) readStream(httpResp *http.Response, endpoint string, startedAt time.Time, onDelta func(llm.Delta) error) (*llm.Response, error) {
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
		if strings.TrimSpace(line) == "" {
			continue
		}

		var chunk chatResponse
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			// Not a JSON line (e.g. a keepalive comment); skip it rather
			// than aborting the stream.
			continue
		}
		sawData = true

		if err := acc.addDelta(&chunk, onDelta); err != nil {
			return fail(err)
		}
		if chunk.Done {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return fail(fmt.Errorf("read stream: %w", err))
	}
	if !sawData {
		return fail(fmt.Errorf("decode response: no data in stream"))
	}

	resp := acc.response()
	if hasToolCallWithoutName(resp.Message.ToolCalls) {
		return fail(fmt.Errorf("decode response: tool call missing name"))
	}

	// The clock stops once the stream has been fully read and the
	// assembled response validated.
	resp.Stats = llm.CallStats{Output: resp.Message.OutputBytes(), Elapsed: time.Since(startedAt)}

	// A marshal failure still leaves a record: error: <marshal error>.
	body, err := marshalWireResponse(resp)
	if err != nil {
		logResponse([]byte("error: " + err.Error()))
	} else {
		logResponse(body)
	}
	return resp, nil
}

// post marshals the wire request and POSTs it to /api/chat. The caller owns
// the response body. Shared by the Chat and ChatStream paths so their
// request building cannot drift. The final endpoint URL is returned
// alongside the response so callers can use it in response log records,
// together with the clock start for the call's elapsed measurement: just
// before the request is built and sent. Callers stop the clock when the
// response body has been fully read.
func (c *Client) post(ctx context.Context, body chatRequest) (*http.Response, string, time.Time, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, "", time.Time{}, fmt.Errorf("marshal request: %w", err)
	}

	endpoint, err := url.JoinPath(strings.TrimSuffix(c.cfg.BaseURL, "/"), chatPath)
	if err != nil {
		return nil, "", time.Time{}, fmt.Errorf("join base_url %q: %w", c.cfg.BaseURL, err)
	}

	startedAt := time.Now()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, "", time.Time{}, fmt.Errorf("build request: %w", err)
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
			return nil, "", time.Time{}, fmt.Errorf("ollama chat: %w", err)
		}
		body, getErr := httpReq.GetBody()
		if getErr != nil {
			return nil, "", time.Time{}, fmt.Errorf("ollama chat: retry unavailable: %w", err)
		}
		httpReq.Body = body
		httpResp, err = c.httpClient().Do(httpReq)
		if err != nil {
			return nil, "", time.Time{}, fmt.Errorf("ollama chat: %w", err)
		}
	}
	return httpResp, endpoint, startedAt, nil
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
