package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/logging"
)

func newTestClient(t *testing.T, baseURL string, opts ...func(*Config)) *Client {
	t.Helper()
	cfg := Config{BaseURL: baseURL, Model: "llama3.1:latest"}
	for _, opt := range opts {
		opt(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("ollama.New error = %v, want nil", err)
	}
	return c
}

func sampleRequest() llm.Request {
	return llm.Request{
		Model: "llama3.1:latest",
		Messages: []llm.Message{
			llm.NewTextMessage(llm.RoleSystem, "You are helpful."),
			llm.NewTextMessage(llm.RoleUser, "list the files"),
		},
		Tools: []llm.Tool{
			{
				Name:        "ls",
				Description: "List files.",
				Parameters:  json.RawMessage(`{"type":"object"}`),
			},
		},
	}
}

func TestChatSuccessRoundTrip(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotReq map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Errorf("unmarshal request body: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "llama3.1:latest",
			"created_at": "2026-01-01T00:00:00Z",
			"message": {"role": "assistant", "content": "here are your files"},
			"done": true,
			"done_reason": "stop",
			"prompt_eval_count": 12,
			"eval_count": 5
		}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	resp, err := c.Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Chat error = %v, want nil", err)
	}

	if gotPath != "/api/chat" {
		t.Errorf("request path = %q, want %q", gotPath, "/api/chat")
	}

	if got, want := gotReq["model"], "llama3.1:latest"; got != want {
		t.Errorf("request model = %v, want %v", got, want)
	}

	messages, ok := gotReq["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("request messages = %#v, want 2 entries", gotReq["messages"])
	}
	first, _ := messages[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "You are helpful." {
		t.Errorf("messages[0] = %v, want the system prompt", first)
	}

	tools, ok := gotReq["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("request tools = %#v, want 1 entry", gotReq["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	if tool["type"] != "function" || fn["name"] != "ls" {
		t.Errorf("tool definition = %v, want the function envelope for ls", tool)
	}

	if resp.Message.Role != llm.RoleAssistant || resp.Message.Content != "here are your files" {
		t.Errorf("resp.Message = %+v, want assistant text", resp.Message)
	}
	if resp.FinishReason != llm.FinishStop {
		t.Errorf("resp.FinishReason = %q, want %q", resp.FinishReason, llm.FinishStop)
	}
	if resp.Usage.PromptTokens != 12 || resp.Usage.CompletionTokens != 5 || resp.Usage.TotalTokens != 17 {
		t.Errorf("resp.Usage = %+v, want prompt 12, completion 5, total 17", resp.Usage)
	}
	if resp.ID != "" {
		t.Errorf("resp.ID = %q, want empty (Ollama has no response id)", resp.ID)
	}
}

func TestChatModelDefaultsFromConfig(t *testing.T) {
	t.Parallel()

	var gotModel any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("unmarshal request: %v", err)
		}
		gotModel = req["model"]
		_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"x"},"done":true,"done_reason":"stop"}`))
	}))
	defer srv.Close()

	if _, err := newTestClient(t, srv.URL).Chat(context.Background(), llm.Request{}); err != nil {
		t.Fatalf("Chat error = %v, want nil", err)
	}
	if gotModel != "llama3.1:latest" {
		t.Errorf("request model = %v, want the Config default", gotModel)
	}
}

func TestChatThinkFieldOnWire(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("unmarshal request: %v", err)
		}
		if got, ok := req["think"]; !ok || got != "medium" {
			t.Errorf("think = %v (present: %v), want %q", got, ok, "medium")
		}
		_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"x"},"done":true,"done_reason":"stop"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, func(cfg *Config) { cfg.ReasoningEffort = "medium" })
	if _, err := c.Chat(context.Background(), llm.Request{}); err != nil {
		t.Fatalf("Chat error = %v, want nil", err)
	}
}

func TestChatBearerHeader(t *testing.T) {
	t.Parallel()

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"x"},"done":true,"done_reason":"stop"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, func(cfg *Config) { cfg.APIKey = "sekrit" })
	if _, err := c.Chat(context.Background(), llm.Request{}); err != nil {
		t.Fatalf("Chat error = %v, want nil", err)
	}
	if gotAuth != "Bearer sekrit" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer sekrit")
	}

	// Without a key the header must be absent entirely.
	var noAuth string
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"x"},"done":true,"done_reason":"stop"}`))
	}))
	defer srv2.Close()

	if _, err := newTestClient(t, srv2.URL).Chat(context.Background(), llm.Request{}); err != nil {
		t.Fatalf("Chat error = %v, want nil", err)
	}
	if noAuth != "" {
		t.Errorf("Authorization = %q, want absent without an API key", noAuth)
	}
}

func TestChatToolCallsRoundTrip(t *testing.T) {
	t.Parallel()

	var gotReq struct {
		Messages []wireMessage `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Errorf("unmarshal request: %v", err)
		}
		_, _ = w.Write([]byte(`{
			"model":"m",
			"message": {
				"role": "assistant",
				"content": "",
				"tool_calls": [{"function": {"name": "ls", "arguments": {"path": "."}}}]
			},
			"done": true,
			"done_reason": "tool_calls"
		}`))
	}))
	defer srv.Close()

	req := llm.Request{
		Model: "m",
		Messages: []llm.Message{
			{
				Role: llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{
					{ID: "call_9", Type: "function", FunctionName: "ls", FunctionArgs: `{"path":"."}`},
				},
			},
			llm.NewToolResultMessage("call_9", "a.txt", false),
		},
	}

	resp, err := newTestClient(t, srv.URL).Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat error = %v, want nil", err)
	}

	if len(gotReq.Messages) != 2 {
		t.Fatalf("wire messages = %d, want 2", len(gotReq.Messages))
	}
	if got := gotReq.Messages[1].ToolCallID; got != "call_9" {
		t.Errorf("wire tool_call_id = %q, want %q", got, "call_9")
	}
	if string(gotReq.Messages[0].ToolCalls[0].Function.Arguments) != `{"path":"."}` {
		t.Errorf("wire tool call arguments = %s, want the embedded object", gotReq.Messages[0].ToolCalls[0].Function.Arguments)
	}

	// The response tool call has no id: one is synthesized.
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("resp tool calls = %d, want 1", len(resp.Message.ToolCalls))
	}
	tc := resp.Message.ToolCalls[0]
	if !strings.HasPrefix(tc.ID, "call_") {
		t.Errorf("tool call id = %q, want a synthesized call_ id", tc.ID)
	}
	if tc.FunctionName != "ls" {
		t.Errorf("tool call name = %q, want ls", tc.FunctionName)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(tc.FunctionArgs), &args); err != nil {
		t.Fatalf("FunctionArgs not valid JSON: %v (%s)", err, tc.FunctionArgs)
	}
	if args["path"] != "." {
		t.Errorf("FunctionArgs = %q, want the arguments object as a string", tc.FunctionArgs)
	}
	if resp.FinishReason != llm.FinishToolCalls {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, llm.FinishToolCalls)
	}
}

// TestTruncate pins the error-body truncation helper: short bodies pass
// through unchanged, long ones cut at the limit — backing off to a rune
// boundary so multibyte sequences are not split — and gain a marker.
func TestTruncate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		data  string
		limit int
		want  string
	}{
		{name: "short body passes through", data: "small", limit: 10, want: "small"},
		{name: "exactly the limit passes through", data: "12345", limit: 5, want: "12345"},
		{
			name:  "ascii cut at the limit",
			data:  "abcdefghij",
			limit: 4,
			want:  "abcd...(truncated)",
		},
		{
			name:  "multibyte rune not split",
			data:  "héllo",
			limit: 2,
			want:  "h...(truncated)",
		},
		{
			name:  "cut landing mid-rune backs off",
			data:  "éééé",
			limit: 3,
			want:  "é...(truncated)",
		},
		{
			name:  "empty body",
			data:  "",
			limit: 4,
			want:  "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := truncate([]byte(tc.data), tc.limit)
			if got != tc.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tc.data, tc.limit, got, tc.want)
			}
		})
	}
}

func TestChatNotFoundBodyInError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).Chat(context.Background(), llm.Request{})
	if err == nil {
		t.Fatal("Chat succeeded, want error")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %q, want it to contain 404", err)
	}
	if !strings.Contains(err.Error(), "model not found") {
		t.Errorf("error = %q, want the body snippet", err)
	}
}

// TestChatBodyOverLimitErrors pins the response-size cap: a 2xx body
// larger than the read limit errors instead of accumulating unbounded.
func TestChatBodyOverLimitErrors(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		chunk := `{"model":"m","message":{"role":"assistant","content":"x"}}`
		// maxBodyLen worth of payload plus padding: well over the cap.
		_, _ = io.WriteString(w, strings.Repeat(chunk, maxBodyLen/len(chunk)+2))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).Chat(context.Background(), llm.Request{})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %v, want an exceeds-limit error", err)
	}
}

// TestChatBodyReadError pins the mid-body failure path: a server that
// drops the connection after a partial response surfaces as a read
// error, not a decode of the truncated body.
func TestChatBodyReadError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m","mes`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Drop the connection mid-body: the client's body read fails.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).Chat(context.Background(), llm.Request{})
	if err == nil || !strings.Contains(err.Error(), "read response body") {
		t.Errorf("error = %v, want a read-response-body error", err)
	}
}

// TestChatInvalidJSONResponse pins the decode-error path: a 2xx body that
// is not a chatResponse surfaces as a decode error carrying the body.
func TestChatInvalidJSONResponse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json at all`))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).Chat(context.Background(), llm.Request{})
	if err == nil || !strings.Contains(err.Error(), "decode response") || !strings.Contains(err.Error(), "not json at all") {
		t.Errorf("error = %v, want a decode error carrying the body", err)
	}
}

// TestChatInvalidToolCallArgsError pins the request-building failure path:
// a history tool call whose arguments are not valid JSON errors before any
// request is sent.
func TestChatInvalidToolCallArgsError(t *testing.T) {
	t.Parallel()

	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
	}))
	defer srv.Close()

	req := llm.Request{
		Messages: []llm.Message{{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{
				{ID: "call_1", Type: "function", FunctionName: "ping", FunctionArgs: `{oops`},
			},
		}},
	}

	_, err := newTestClient(t, srv.URL).Chat(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("Chat error = %v, want an invalid-arguments error", err)
	}
	_, err = newTestClient(t, srv.URL).ChatStream(context.Background(), req, func(llm.Delta) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("ChatStream error = %v, want an invalid-arguments error", err)
	}
	if got := atomic.LoadInt32(&requests); got != 0 {
		t.Errorf("requests = %d, want 0 (the failure precedes the POST)", got)
	}
}

func TestChatDoneFalseErrors(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"x"},"done":false}`))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).Chat(context.Background(), llm.Request{})
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Errorf("error = %v, want a not-done decode error", err)
	}
}

func TestChatToolCallWithoutNameErrors(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","tool_calls":[{"function":{"arguments":{}}}]},"done":true,"done_reason":"tool_calls"}`))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).Chat(context.Background(), llm.Request{})
	if err == nil || !strings.Contains(err.Error(), "missing name") {
		t.Errorf("error = %v, want a missing-name decode error", err)
	}
}

func TestNewRejectsBadBaseURL(t *testing.T) {
	t.Parallel()

	for _, base := range []string{"", "ftp://example.com", "http://", "http://[::1"} {
		if _, err := New(Config{BaseURL: base}); err == nil {
			t.Errorf("New(base_url %q) succeeded, want error", base)
		}
	}
}

// TestChatRetriesEOFBeforeResponse pins the keep-alive recovery: a server
// that closes the connection without responding (a connection the server
// FINs after its previous response, handed to the next request by the
// transport's idle pool) surfaces as io.EOF from the POST. The client
// retries once — the request never reached the server — and succeeds.
func TestChatRetriesEOFBeforeResponse(t *testing.T) {
	t.Parallel()

	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&requests, 1) == 1 {
			// Simulate a pooled connection the server already closed:
			// take over the socket and drop it without responding.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("test server does not support hijacking")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"recovered"},"done":true,"done_reason":"stop"}`))
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL).Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Chat error = %v, want the EOF retry to recover", err)
	}
	if resp.Message.Content != "recovered" {
		t.Errorf("content = %q, want %q", resp.Message.Content, "recovered")
	}
	if got := atomic.LoadInt32(&requests); got != 2 {
		t.Errorf("requests = %d, want 2 (dropped connection retried once)", got)
	}
}

// TestChatStreamRetriesEOFBeforeResponse is the streaming variant of the
// keep-alive recovery test: the same dropped-then-retried connection, but
// the recovering response is an NDJSON body.
func TestChatStreamRetriesEOFBeforeResponse(t *testing.T) {
	t.Parallel()

	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&requests, 1) == 1 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("test server does not support hijacking")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		ndjsonChunks(w,
			`{"model":"m","message":{"role":"assistant","content":"recovered"}}`,
			`{"model":"m","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop"}`,
		)
	}))
	defer srv.Close()

	var got strings.Builder
	resp, err := newTestClient(t, srv.URL).ChatStream(context.Background(), sampleRequest(), func(d llm.Delta) error {
		got.WriteString(d.Content)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream error = %v, want the EOF retry to recover", err)
	}
	if resp.Message.Content != "recovered" {
		t.Errorf("content = %q, want %q", resp.Message.Content, "recovered")
	}
	if got.String() != "recovered" {
		t.Errorf("streamed text = %q, want %q", got.String(), "recovered")
	}
	if n := atomic.LoadInt32(&requests); n != 2 {
		t.Errorf("requests = %d, want 2 (dropped connection retried once)", n)
	}
}

// TestChatDoesNotRetryNonEOF pins that only io.EOF triggers the retry: a
// request that reaches the server and gets a real error response must not
// be sent twice.
func TestChatDoesNotRetryNonEOF(t *testing.T) {
	t.Parallel()

	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error": "boom"}`))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).Chat(context.Background(), sampleRequest())
	if err == nil || !strings.Contains(err.Error(), "unexpected status 500") {
		t.Fatalf("Chat error = %v, want an unexpected-status error", err)
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Errorf("requests = %d, want 1 (no retry for a delivered error)", got)
	}
}

func TestChatLogsRequestAndResponse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Trace", "tr-1")
		_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"hi"},"done":true,"done_reason":"stop"}`))
	}))
	defer srv.Close()

	sink := logging.NewBuffered(0)
	c := newTestClient(t, srv.URL, func(cfg *Config) { cfg.Sink = sink })
	_, err := c.Chat(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("Chat error = %v, want nil", err)
	}

	recs := sink.Records()
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2 (request + response)", len(recs))
	}

	reqRec, respRec := recs[0], recs[1]
	if reqRec.Kind != logging.KindLLMRequest {
		t.Errorf("record 0 kind = %q, want llm-request", reqRec.Kind)
	}
	if respRec.Kind != logging.KindLLMResponse {
		t.Errorf("record 1 kind = %q, want llm-response", respRec.Kind)
	}
	if reqRec.Method != http.MethodPost {
		t.Errorf("request method = %q, want POST", reqRec.Method)
	}
	if want := srv.URL + "/api/chat"; reqRec.URL != want {
		t.Errorf("request url = %q, want %q", reqRec.URL, want)
	}
	if got := reqRec.Headers["Content-Type"]; len(got) != 1 || got[0] != "application/json" {
		t.Errorf("request Content-Type = %v, want application/json", got)
	}

	var wire map[string]any
	if err := json.Unmarshal(reqRec.Body, &wire); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if wire["model"] != "llama3.1:latest" {
		t.Errorf("logged request model = %v, want llama3.1:latest", wire["model"])
	}

	if got := respRec.Headers["Status"]; len(got) != 1 || got[0] != "200" {
		t.Errorf("response Status header = %v, want 200", got)
	}
	if got := respRec.Headers["X-Trace"]; len(got) != 1 || got[0] != "tr-1" {
		t.Errorf("response X-Trace header = %v, want tr-1", got)
	}
	if string(respRec.Body) != `{"model":"m","message":{"role":"assistant","content":"hi"},"done":true,"done_reason":"stop"}` {
		t.Errorf("response body = %q, want the exact server bytes", respRec.Body)
	}
}

func TestChatLogsNon2xxResponse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"bad key"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	sink := logging.NewBuffered(0)
	c := newTestClient(t, srv.URL, func(cfg *Config) { cfg.Sink = sink })
	_, err := c.Chat(context.Background(), llm.Request{})
	if err == nil {
		t.Fatal("Chat succeeded, want error")
	}

	recs := sink.Records()
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	respRec := recs[1]
	if got := respRec.Headers["Status"]; len(got) != 1 || got[0] != "401" {
		t.Errorf("response Status header = %v, want 401", got)
	}
	if !strings.Contains(string(respRec.Body), "bad key") {
		t.Errorf("response body = %q, want the server error body", respRec.Body)
	}
}

// ndjsonChunks writes each payload as one NDJSON line.
func ndjsonChunks(w io.Writer, payloads ...string) {
	for _, p := range payloads {
		_, _ = fmt.Fprintf(w, "%s\n", p)
	}
}

func TestChatStreamContentDeltas(t *testing.T) {
	t.Parallel()

	var gotReq map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Errorf("unmarshal request: %v", err)
		}
		ndjsonChunks(w,
			`{"model":"m","message":{"role":"assistant","content":"Hel"}}`,
			`{"model":"m","message":{"role":"assistant","content":"lo, "}}`,
			`{"model":"m","message":{"role":"assistant","content":"world"}}`,
			`{"model":"m","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":10,"eval_count":4}`,
		)
	}))
	defer srv.Close()

	var deltas []llm.Delta
	resp, err := newTestClient(t, srv.URL).ChatStream(context.Background(), sampleRequest(), func(d llm.Delta) error {
		deltas = append(deltas, d)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream error = %v, want nil", err)
	}

	var content strings.Builder
	for _, d := range deltas {
		if d.ToolCall != nil {
			t.Errorf("unexpected tool call delta: %+v", d.ToolCall)
		}
		content.WriteString(d.Content)
	}
	if content.String() != "Hello, world" {
		t.Errorf("concatenated content = %q, want %q", content.String(), "Hello, world")
	}
	if got := resp.Message.Content; got != content.String() {
		t.Errorf("final content = %q, want the concatenation %q", got, content.String())
	}
	if resp.FinishReason != llm.FinishStop {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, llm.FinishStop)
	}
	if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 4 || resp.Usage.TotalTokens != 14 {
		t.Errorf("Usage = %+v, want prompt 10, completion 4, total 14", resp.Usage)
	}
	if got, ok := gotReq["stream"].(bool); !ok || !got {
		t.Errorf("request stream = %v, want true", gotReq["stream"])
	}
}

func TestChatStreamReasoningDeltas(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ndjsonChunks(w,
			`{"model":"m","message":{"role":"assistant","content":"thinking","thinking":"2+2..."}}`,
			`{"model":"m","message":{"role":"assistant","content":" = 4","thinking":" obviously"},"done":true,"done_reason":"stop"}`,
		)
	}))
	defer srv.Close()

	var deltas []llm.Delta
	resp, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{}, func(d llm.Delta) error {
		deltas = append(deltas, d)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream error = %v, want nil", err)
	}

	// Deltas interleave in chunk order: content and reasoning
	// fragments from the same chunk surface together, reasoning first
	// (thinking precedes text in generation order).
	var reasoning strings.Builder
	var content strings.Builder
	for _, d := range deltas {
		content.WriteString(d.Content)
		reasoning.WriteString(d.Reasoning)
	}
	if content.String() != "thinking = 4" {
		t.Errorf("content = %q, want %q", content.String(), "thinking = 4")
	}
	if reasoning.String() != "2+2... obviously" {
		t.Errorf("reasoning = %q, want %q", reasoning.String(), "2+2... obviously")
	}
	if got := resp.Message.Reasoning; got != "2+2... obviously" {
		t.Errorf("final Reasoning = %q, want the concatenation", got)
	}
}

func TestChatStreamToolCallFragments(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ndjsonChunks(w,
			`{"model":"m","message":{"role":"assistant","content":""}}`,
			`{"model":"m","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"ls","arguments":{"path":"."}}}]},"done":true,"done_reason":"tool_calls"}`,
		)
	}))
	defer srv.Close()

	var deltas []llm.Delta
	resp, err := newTestClient(t, srv.URL).ChatStream(context.Background(), sampleRequest(), func(d llm.Delta) error {
		deltas = append(deltas, d)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream error = %v, want nil", err)
	}

	// Only the tool call chunk emits a delta: the earlier chunk carries
	// empty content, which produces nothing.
	if len(deltas) != 1 {
		t.Fatalf("deltas = %d, want 1 (tool call only)", len(deltas))
	}
	tcDelta := deltas[0].ToolCall
	if tcDelta == nil {
		t.Fatalf("delta 0 carries no tool call: %+v", deltas[0])
	}
	if tcDelta.Index != 0 || tcDelta.Name != "ls" || tcDelta.Type != llm.ToolCallType {
		t.Errorf("tool call delta = %+v, want index 0, function ls", tcDelta)
	}
	if tcDelta.ID != "" {
		t.Errorf("tool call delta ID = %q, want empty (Ollama sends none; synthesized at the end)", tcDelta.ID)
	}

	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("final tool calls = %d, want 1", len(resp.Message.ToolCalls))
	}
	tc := resp.Message.ToolCalls[0]
	if !strings.HasPrefix(tc.ID, "call_") {
		t.Errorf("final tool call id = %q, want a synthesized call_ id", tc.ID)
	}
	if tc.FunctionName != "ls" {
		t.Errorf("final tool call name = %q, want ls", tc.FunctionName)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(tc.FunctionArgs), &args); err != nil {
		t.Fatalf("FunctionArgs not valid JSON: %v (%s)", err, tc.FunctionArgs)
	}
	if args["path"] != "." {
		t.Errorf("FunctionArgs = %q, want the arguments object as a string", tc.FunctionArgs)
	}
	if resp.FinishReason != llm.FinishToolCalls {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, llm.FinishToolCalls)
	}
}

func TestChatStreamTwoToolCallsOneChunk(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ndjsonChunks(w,
			`{"model":"m","message":{"role":"assistant","tool_calls":[{"function":{"name":"ls","arguments":{"path":"."}}},{"function":{"name":"cat","arguments":{"path":"a.txt"}}}]},"done":true,"done_reason":"tool_calls"}`,
		)
	}))
	defer srv.Close()

	var indexes []int
	resp, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{}, func(d llm.Delta) error {
		if d.ToolCall != nil {
			indexes = append(indexes, d.ToolCall.Index)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream error = %v, want nil", err)
	}
	if fmt.Sprint(indexes) != "[0 1]" {
		t.Errorf("tool call delta indexes = %v, want [0 1]", indexes)
	}
	if len(resp.Message.ToolCalls) != 2 {
		t.Fatalf("final tool calls = %d, want 2", len(resp.Message.ToolCalls))
	}
	if resp.Message.ToolCalls[0].FunctionName != "ls" || resp.Message.ToolCalls[1].FunctionName != "cat" {
		t.Errorf("final tool calls = %+v, want ls then cat", resp.Message.ToolCalls)
	}
}

func TestChatStreamThinkFieldOnWire(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("unmarshal request: %v", err)
		}
		if got, ok := req["think"]; !ok || got != "medium" {
			t.Errorf("think = %v (present: %v), want %q", got, ok, "medium")
		}
		ndjsonChunks(w, `{"model":"m","message":{"role":"assistant","content":"x"},"done":true,"done_reason":"stop"}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, func(cfg *Config) { cfg.ReasoningEffort = "medium" })
	if _, err := c.ChatStream(context.Background(), llm.Request{}, func(llm.Delta) error { return nil }); err != nil {
		t.Fatalf("ChatStream error = %v, want nil", err)
	}
}

func TestChatStreamNon2xx(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{}, func(llm.Delta) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "model not found") {
		t.Errorf("error = %v, want a status-and-body error", err)
	}
}

// TestChatStreamNon2xxBodyOverLimitErrors is the streaming variant of the
// response-size cap: an error body larger than the read limit errors
// instead of accumulating unbounded.
func TestChatStreamNon2xxBodyOverLimitErrors(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		chunk := `{"error":"boom"}`
		// maxBodyLen worth of payload plus padding: well over the cap.
		_, _ = io.WriteString(w, strings.Repeat(chunk, maxBodyLen/len(chunk)+2))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{}, func(llm.Delta) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %v, want an exceeds-limit error", err)
	}
}

// TestChatStreamNon2xxBodyReadError is the streaming variant of the
// mid-body failure path: an error response whose body drops mid-read
// surfaces as a read error.
func TestChatStreamNon2xxBodyReadError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"bo`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{}, func(llm.Delta) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "read response body") {
		t.Errorf("error = %v, want a read-response-body error", err)
	}
}

func TestChatStreamOnDeltaErrorAborts(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ndjsonChunks(w,
			`{"model":"m","message":{"role":"assistant","content":"first"}}`,
			`{"model":"m","message":{"role":"assistant","content":"second"}}`,
			`{"model":"m","message":{"role":"assistant","content":"x"},"done":true,"done_reason":"stop"}`,
		)
	}))
	defer srv.Close()

	boom := errors.New("boom")
	_, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{}, func(llm.Delta) error {
		return boom
	})
	if err == nil || !errors.Is(err, boom) {
		t.Errorf("error = %v, want the onDelta error", err)
	}
}

// TestChatStreamOnDeltaErrorOnReasoningAndToolCall pins the remaining
// abort paths: an onDelta error raised by a reasoning fragment and by a
// tool call fragment both abort the stream and surface the error.
func TestChatStreamOnDeltaErrorOnReasoningAndToolCall(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		chunks []string
	}{
		{
			name: "reasoning fragment",
			chunks: []string{
				`{"model":"m","message":{"role":"assistant","thinking":"pondering"}}`,
				`{"model":"m","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop"}`,
			},
		},
		{
			name: "tool call fragment",
			chunks: []string{
				`{"model":"m","message":{"role":"assistant","tool_calls":[{"id":"srv-1","function":{"name":"ls","arguments":{"path":"."}}}]},"done":true,"done_reason":"tool_calls"}`,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ndjsonChunks(w, tc.chunks...)
			}))
			defer srv.Close()

			boom := errors.New("boom")
			_, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{}, func(llm.Delta) error {
				return boom
			})
			if err == nil || !errors.Is(err, boom) {
				t.Errorf("error = %v, want the onDelta error", err)
			}
		})
	}
}

// TestChatStreamToolCallIDFilledInLater pins the fragment-accumulation
// rule: a first fragment without an id followed by a later fragment that
// carries one adopts the later id. (Within one NDJSON line a chunk's
// arguments must be a complete JSON value, so the realistic split is the
// name in the first chunk and the id and arguments in the second.)
func TestChatStreamToolCallIDFilledInLater(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ndjsonChunks(w,
			`{"model":"m","message":{"role":"assistant","tool_calls":[{"function":{"name":"ls"}}]}}`,
			`{"model":"m","message":{"role":"assistant","tool_calls":[{"id":"srv-9","function":{"arguments":{"path":"."}}}]},"done":true,"done_reason":"tool_calls"}`,
		)
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{}, func(llm.Delta) error { return nil })
	if err != nil {
		t.Fatalf("ChatStream error = %v, want nil", err)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(resp.Message.ToolCalls))
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "srv-9" {
		t.Errorf("tool call id = %q, want the later fragment's id", tc.ID)
	}
	if got, want := tc.FunctionArgs, `{"path":"."}`; got != want {
		t.Errorf("FunctionArgs = %q, want %q", got, want)
	}
}

// TestChatStreamLengthFinishReasonLogged pins the assembled log record's
// done_reason inversion: a length-limited stream logs "length".
func TestChatStreamLengthFinishReasonLogged(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ndjsonChunks(w,
			`{"model":"m","message":{"role":"assistant","content":"trunc"}}`,
			`{"model":"m","message":{"role":"assistant","content":""},"done":true,"done_reason":"length","prompt_eval_count":1,"eval_count":2}`,
		)
	}))
	defer srv.Close()

	sink := logging.NewBuffered(0)
	c := newTestClient(t, srv.URL, func(cfg *Config) { cfg.Sink = sink })
	resp, err := c.ChatStream(context.Background(), llm.Request{}, func(llm.Delta) error { return nil })
	if err != nil {
		t.Fatalf("ChatStream error = %v, want nil", err)
	}
	if resp.FinishReason != llm.FinishLength {
		t.Fatalf("FinishReason = %q, want %q", resp.FinishReason, llm.FinishLength)
	}

	recs := sink.Records()
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2", len(recs))
	}
	var wire chatResponse
	if err := json.Unmarshal(recs[1].Body, &wire); err != nil {
		t.Fatalf("assembled log body not JSON: %v (%s)", err, recs[1].Body)
	}
	if wire.DoneReason != "length" {
		t.Errorf("logged done_reason = %q, want length (the inverted finish reason)", wire.DoneReason)
	}
	if wire.PromptEvalCount != 1 || wire.EvalCount != 2 {
		t.Errorf("logged counts = %d/%d, want 1/2", wire.PromptEvalCount, wire.EvalCount)
	}
}

func TestChatStreamDoneLessEOF(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ndjsonChunks(w, `{"model":"m","message":{"role":"assistant","content":"partial"}}`)
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{}, func(llm.Delta) error { return nil })
	if err != nil {
		t.Fatalf("ChatStream error = %v, want nil (EOF without done is tolerated)", err)
	}
	if resp.Message.Content != "partial" {
		t.Errorf("content = %q, want the accumulated partial", resp.Message.Content)
	}
	if resp.FinishReason != "" || resp.Usage != (llm.Usage{}) {
		t.Errorf("FinishReason/Usage = %q/%+v, want empty (server never finished)", resp.FinishReason, resp.Usage)
	}
}

func TestChatStreamEmptyBodyErrors(t *testing.T) {
	t.Parallel()

	for _, body := range []string{"", "\n", "not json\n"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, body)
		}))

		_, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{}, func(llm.Delta) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "no data in stream") {
			t.Errorf("body %q: error = %v, want a no-data error", body, err)
		}
		srv.Close()
	}
}

func TestChatStreamMalformedLinesSkipped(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ndjsonChunks(w,
			"",
			": keepalive",
			`{"model":"m","message":{"role":"assistant","content":"real"}}`,
			"garbage line",
			`{"model":"m","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop"}`,
		)
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{}, func(llm.Delta) error { return nil })
	if err != nil {
		t.Fatalf("ChatStream error = %v, want nil (malformed lines are skipped)", err)
	}
	if resp.Message.Content != "real" {
		t.Errorf("content = %q, want %q", resp.Message.Content, "real")
	}
}

func TestChatStreamContextCancellation(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, `{"model":"m","message":{"role":"assistant","content":"chunk"}}`+"\n")
		w.(http.Flusher).Flush()
		<-release
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := newTestClient(t, srv.URL).ChatStream(ctx, llm.Request{}, func(llm.Delta) error { return nil })
	if err == nil {
		t.Fatal("ChatStream succeeded, want error")
	}
	if !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Errorf("error = %q, want it to reference cancellation", err)
	}
}

// oversizedStream builds a server that streams the given NDJSON fragment
// repeatedly (flushing each line), for the accumulation limit tests below.
func oversizedStream(t *testing.T, fragment string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		for i := 0; i < 17; i++ {
			_, _ = fmt.Fprintf(w, "%s\n", fragment)
			w.(http.Flusher).Flush()
		}
	}))
}

func TestChatStreamContentOverLimitErrors(t *testing.T) {
	t.Parallel()

	fragment := `{"model":"m","message":{"role":"assistant","content":"` + strings.Repeat("x", 1<<20) + `"}}`
	srv := oversizedStream(t, fragment)
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{}, func(llm.Delta) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "content exceeds") {
		t.Errorf("error = %v, want a content limit error", err)
	}
}

func TestChatStreamReasoningOverLimitErrors(t *testing.T) {
	t.Parallel()

	fragment := `{"model":"m","message":{"role":"assistant","thinking":"` + strings.Repeat("x", 1<<20) + `"}}`
	srv := oversizedStream(t, fragment)
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{}, func(llm.Delta) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "reasoning exceeds") {
		t.Errorf("error = %v, want a reasoning limit error", err)
	}
}

func TestChatStreamToolCallArgumentsOverLimitErrors(t *testing.T) {
	t.Parallel()

	// Enough argument fragments at the same index to exceed the 16 MiB
	// accumulated cap for a single tool call.
	fragment := `{"model":"m","message":{"role":"assistant","tool_calls":[{"function":{"name":"big","arguments":{"blob":"` + strings.Repeat("x", 1<<20) + `"}}}]}}`
	srv := oversizedStream(t, fragment)
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{}, func(llm.Delta) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "tool call arguments exceed") {
		t.Errorf("error = %v, want a tool call arguments limit error", err)
	}
}

func TestChatStreamToolCallWithoutNameErrors(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ndjsonChunks(w,
			`{"model":"m","message":{"role":"assistant","tool_calls":[{"function":{"arguments":{}}}]},"done":true,"done_reason":"tool_calls"}`,
		)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{}, func(llm.Delta) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "missing name") {
		t.Errorf("error = %v, want a missing-name decode error", err)
	}
}

func TestChatStreamLogsRequestAndAssembledResponse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("X-Trace", "tr-1")
		ndjsonChunks(w,
			`{"model":"m","message":{"role":"assistant","content":"Hel"}}`,
			`{"model":"m","message":{"role":"assistant","content":"lo"},"done":true,"done_reason":"stop","prompt_eval_count":3,"eval_count":2}`,
		)
	}))
	defer srv.Close()

	sink := logging.NewBuffered(0)
	c := newTestClient(t, srv.URL, func(cfg *Config) { cfg.Sink = sink })
	resp, err := c.ChatStream(context.Background(), llm.Request{}, func(llm.Delta) error { return nil })
	if err != nil {
		t.Fatalf("ChatStream error = %v, want nil", err)
	}
	if resp.Message.Content != "Hello" {
		t.Fatalf("content = %q, want Hello", resp.Message.Content)
	}

	recs := sink.Records()
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}

	reqRec, respRec := recs[0], recs[1]
	if reqRec.Kind != logging.KindLLMRequest || respRec.Kind != logging.KindLLMResponse {
		t.Fatalf("kinds = %q, %q, want llm-request, llm-response", reqRec.Kind, respRec.Kind)
	}
	if !strings.Contains(string(reqRec.Body), `"stream":true`) {
		t.Errorf("request body = %q, want stream:true", reqRec.Body)
	}
	if want := srv.URL + "/api/chat"; reqRec.URL != want {
		t.Errorf("request url = %q, want %q", reqRec.URL, want)
	}
	if got := respRec.Headers["Status"]; len(got) != 1 || got[0] != "200" {
		t.Errorf("response Status header = %v, want 200", got)
	}
	if got := respRec.Headers["X-Trace"]; len(got) != 1 || got[0] != "tr-1" {
		t.Errorf("response X-Trace header = %v, want tr-1", got)
	}

	// The response record carries the assembled response marshalled back
	// into a final chatResponse, not the raw NDJSON lines.
	var wire struct {
		Message struct {
			Role     string `json:"role"`
			Content  string `json:"content"`
			Thinking string `json:"thinking"`
		} `json:"message"`
		Done            bool   `json:"done"`
		DoneReason      string `json:"done_reason"`
		PromptEvalCount int    `json:"prompt_eval_count"`
		EvalCount       int    `json:"eval_count"`
	}
	if err := json.Unmarshal(respRec.Body, &wire); err != nil {
		t.Fatalf("response body is not assembled JSON: %v (body: %s)", err, respRec.Body)
	}
	if !wire.Done || wire.DoneReason != "stop" {
		t.Errorf("done/done_reason = %v/%q, want true/stop", wire.Done, wire.DoneReason)
	}
	if wire.Message.Content != "Hello" || wire.Message.Role != "assistant" {
		t.Errorf("assembled message = %+v, want assistant Hello", wire.Message)
	}
	if wire.PromptEvalCount != 3 || wire.EvalCount != 2 {
		t.Errorf("assembled counts = %d/%d, want 3/2", wire.PromptEvalCount, wire.EvalCount)
	}
}

func TestChatStreamLogsAbortedStream(t *testing.T) {
	t.Parallel()

	// Chunks are flushed with pauses so the abort lands while "never
	// seen" is still unsent; otherwise it streams into the same TCP
	// read as the aborting chunk and legitimately shows up in the log.
	// (No t.Parallel: the handler sleeps on the test's clock.)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flush := func(s string) {
			_, _ = io.WriteString(w, s+"\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		flush(`{"model":"m","message":{"role":"assistant","content":"first"}}`)
		time.Sleep(200 * time.Millisecond)
		flush(`{"model":"m","message":{"role":"assistant","content":"second"}}`)
		time.Sleep(200 * time.Millisecond)
		flush(`{"model":"m","message":{"role":"assistant","content":"never seen"}}`)
	}))
	defer srv.Close()

	sink := logging.NewBuffered(0)
	c := newTestClient(t, srv.URL, func(cfg *Config) { cfg.Sink = sink })

	// Aborting readStream drops the connection so the server's late
	// chunks do not block the response log write.
	wantErr := errors.New("stop streaming")
	var saw int
	_, err := c.ChatStream(context.Background(), llm.Request{}, func(d llm.Delta) error {
		saw++
		if saw == 2 {
			return wantErr
		}
		return nil
	})
	if err != wantErr {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}

	recs := sink.Records()
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2 (the aborted stream still logs)", len(recs))
	}
	body := string(recs[1].Body)
	if !strings.Contains(body, "first") || !strings.Contains(body, "second") {
		t.Errorf("logged body = %q, want the content received up to the abort", body)
	}
	if strings.Contains(body, "never seen") {
		t.Errorf("logged body = %q, want it cut off at the abort", body)
	}
	// The record carries the assembled response, not raw NDJSON lines.
	var wire struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(recs[1].Body, &wire); err != nil {
		t.Fatalf("partial response body is not assembled JSON: %v (body: %s)", err, body)
	}
	if got := wire.Message.Content; got != "firstsecond" {
		t.Errorf("partial content = %q, want the concatenated firstsecond", got)
	}
}

func TestChatStreamLogsNoDataError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "\n")
	}))
	defer srv.Close()

	sink := logging.NewBuffered(0)
	c := newTestClient(t, srv.URL, func(cfg *Config) { cfg.Sink = sink })
	_, err := c.ChatStream(context.Background(), llm.Request{}, func(llm.Delta) error { return nil })
	if err == nil {
		t.Fatal("ChatStream succeeded, want error")
	}

	recs := sink.Records()
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	if !strings.Contains(string(recs[1].Body), "error: ") {
		t.Errorf("response body = %q, want an error body", recs[1].Body)
	}
}

func TestChatStreamLogsNon2xx(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"bad key"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	sink := logging.NewBuffered(0)
	c := newTestClient(t, srv.URL, func(cfg *Config) { cfg.Sink = sink })
	_, err := c.ChatStream(context.Background(), llm.Request{}, func(llm.Delta) error { return nil })
	if err == nil {
		t.Fatal("ChatStream succeeded, want error")
	}

	recs := sink.Records()
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	if got := recs[1].Headers["Status"]; len(got) != 1 || got[0] != "401" {
		t.Errorf("response Status header = %v, want 401", got)
	}
}

func TestChatCapturesCallStats(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "llama3.1:latest",
			"created_at": "2026-01-01T00:00:00Z",
			"message": {
				"role": "assistant",
				"content": "the answer",
				"thinking": "hmm",
				"tool_calls": [{"function": {"name": "ls", "arguments": {"path": "."}}}]
			},
			"done": true,
			"done_reason": "tool_calls",
			"prompt_eval_count": 12,
			"eval_count": 5
		}`))
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL).Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Chat error = %v, want nil", err)
	}

	// Recomputed from the fixture's expected content/reasoning/tool
	// call, pinning the component numbers themselves. The tool call's
	// arguments are the wire bytes as-is (the decode preserves them),
	// space included.
	wantOutput := llm.OutputBytes{
		Content:   len("the answer"),
		Reasoning: len("hmm"),
		ToolCalls: len("ls") + len(`{"path": "."}`),
	}
	if resp.Stats.Output != wantOutput {
		t.Errorf("Stats.Output = %+v, want %+v", resp.Stats.Output, wantOutput)
	}
	if resp.Stats.Elapsed <= 0 {
		t.Errorf("Stats.Elapsed = %v, want > 0", resp.Stats.Elapsed)
	}
}

func TestChatStreamCapturesCallStats(t *testing.T) {
	t.Parallel()

	// Multi-line NDJSON stream: lines assemble to the same message the
	// whole-body path would return, so streamed and non-streamed stats
	// must agree — the cross-path consistency pin.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ndjsonChunks(w,
			`{"model":"m","message":{"role":"assistant","thinking":"think "}}`,
			`{"model":"m","message":{"role":"assistant","thinking":"hard"}}`,
			`{"model":"m","message":{"role":"assistant","content":"Hello, "}}`,
			`{"model":"m","message":{"role":"assistant","content":"world"}}`,
			`{"model":"m","message":{"role":"assistant","tool_calls":[{"function":{"name":"ls","arguments":{"pa":"th"}}}]}}`,
			`{"model":"m","message":{"role":"assistant","content":""},"done":true,"done_reason":"tool_calls","prompt_eval_count":10,"eval_count":4}`,
		)
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL).ChatStream(context.Background(), sampleRequest(), func(d llm.Delta) error {
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream error = %v, want nil", err)
	}

	// Ollama re-encodes arguments as JSON, so the streamed call's args
	// decode to the neutral `{"pa":"th"}` form.
	wantOutput := llm.OutputBytes{
		Content:   len("Hello, world"),
		Reasoning: len("think hard"),
		ToolCalls: len("ls") + len(`{"pa":"th"}`),
	}
	if resp.Stats.Output != wantOutput {
		t.Errorf("Stats.Output = %+v, want %+v", resp.Stats.Output, wantOutput)
	}
	if resp.Stats.Elapsed <= 0 {
		t.Errorf("Stats.Elapsed = %v, want > 0", resp.Stats.Elapsed)
	}
	if got := resp.Message.OutputBytes(); got != wantOutput {
		t.Errorf("Message.OutputBytes() = %+v, want %+v", got, wantOutput)
	}
}
