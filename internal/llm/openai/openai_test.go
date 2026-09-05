package openai_test

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
	"github.com/overspecific/blorb/internal/llm/openai"
	"github.com/overspecific/blorb/internal/logging"
)

func newTestClient(t *testing.T, baseURL string, opts ...func(*openai.Config)) *openai.Client {
	t.Helper()
	cfg := openai.Config{BaseURL: baseURL, Model: "gpt-4o-mini"}
	for _, opt := range opts {
		opt(&cfg)
	}
	c, err := openai.New(cfg)
	if err != nil {
		t.Fatalf("openai.New error = %v, want nil", err)
	}
	return c
}

func sampleRequest() llm.Request {
	return llm.Request{
		Model: "gpt-4o-mini",
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
			"id": "resp-1",
			"choices": [{
				"message": {"role": "assistant", "content": "here are your files"},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 12, "completion_tokens": 5, "total_tokens": 17}
		}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	resp, err := c.Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Chat error = %v, want nil", err)
	}

	if gotPath != "/chat/completions" {
		t.Errorf("request path = %q, want %q", gotPath, "/chat/completions")
	}

	if got, want := gotReq["model"], "gpt-4o-mini"; got != want {
		t.Errorf("request model = %v, want %v", got, want)
	}

	messages, ok := gotReq["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("request messages = %#v, want 2 entries", gotReq["messages"])
	}
	first, _ := messages[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "You are helpful." {
		t.Errorf("messages[0] = %v, want system prompt", first)
	}

	tools, ok := gotReq["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("request tools = %#v, want 1 entry", gotReq["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	if tool["type"] != "function" || fn["name"] != "ls" {
		t.Errorf("tool definition = %v, want function ls", tool)
	}

	if resp.ID != "resp-1" {
		t.Errorf("resp.ID = %q, want %q", resp.ID, "resp-1")
	}
	if resp.Message.Role != llm.RoleAssistant || resp.Message.Content != "here are your files" {
		t.Errorf("resp.Message = %+v, want assistant text", resp.Message)
	}
	if resp.FinishReason != llm.FinishStop {
		t.Errorf("resp.FinishReason = %q, want %q", resp.FinishReason, llm.FinishStop)
	}
	if resp.Usage.TotalTokens != 17 {
		t.Errorf("resp.Usage.TotalTokens = %d, want 17", resp.Usage.TotalTokens)
	}
}

func TestChatToolResultMessagesOnWire(t *testing.T) {
	t.Parallel()

	var gotReq struct {
		Messages []wireMessageForTest `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Errorf("unmarshal request: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r","choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`))
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

	if _, err := newTestClient(t, srv.URL).Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat error = %v, want nil", err)
	}

	if len(gotReq.Messages) != 2 {
		t.Fatalf("wire messages = %+v, want 2", gotReq.Messages)
	}
	if got := gotReq.Messages[0].ToolCalls[0].Function.Arguments; got != `{"path":"."}` {
		t.Errorf("wire tool call arguments = %q, want %q", got, `{"path":"."}`)
	}
	if gotReq.Messages[1].ToolCallID != "call_9" {
		t.Errorf("wire tool_call_id = %q, want %q", gotReq.Messages[1].ToolCallID, "call_9")
	}
}

// wireMessageForTest mirrors the wire message shape for asserting on exact
// wire JSON, without importing the unexported openai wire types.
type wireMessageForTest struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Reasoning string `json:"reasoning_content"`
	ToolCalls []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
	ToolCallID string `json:"tool_call_id"`
}

func TestChatBaseURLTrailingSlash(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		baseURL func(srvURL string) string
	}{
		{name: "without trailing slash", baseURL: func(srvURL string) string { return srvURL }},
		{name: "with trailing slash", baseURL: func(srvURL string) string { return srvURL + "/" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				_, _ = w.Write([]byte(`{"id":"r","choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}]}`))
			}))
			defer srv.Close()

			c := newTestClient(t, tc.baseURL(srv.URL))
			if _, err := c.Chat(context.Background(), llm.Request{Model: "m"}); err != nil {
				t.Fatalf("Chat error = %v, want nil", err)
			}
			if gotPath != "/chat/completions" {
				t.Errorf("path = %q, want %q", gotPath, "/chat/completions")
			}
		})
	}
}

func TestChat401Error(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"bad key"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).Chat(context.Background(), llm.Request{Model: "m"})
	if err == nil {
		t.Fatal("Chat succeeded, want error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %q, want it to contain 401", err)
	}
}

func TestListModels(t *testing.T) {
	t.Parallel()

	t.Run("maps id to name and sorts by name", func(t *testing.T) {
		t.Parallel()

		var gotPath string
		var gotMethod string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotMethod = r.Method
			_, _ = w.Write([]byte(`{"object":"list","data":[
				{"id":"zeta-model","created":1700000000,"owned_by":"org"},
				{"id":"alpha-model","created":1699000000,"owned_by":"org"}
			]}`))
		}))
		defer srv.Close()

		c := newTestClient(t, srv.URL)
		got, err := c.ListModels(context.Background())
		if err != nil {
			t.Fatalf("ListModels error = %v, want nil", err)
		}
		if gotPath != "/models" {
			t.Errorf("request path = %q, want %q", gotPath, "/models")
		}
		if gotMethod != http.MethodGet {
			t.Errorf("request method = %q, want GET", gotMethod)
		}
		if len(got) != 2 {
			t.Fatalf("got %d models, want 2", len(got))
		}
		if got[0].Name != "alpha-model" || got[1].Name != "zeta-model" {
			t.Errorf("names = [%s %s], want [alpha-model zeta-model] (sorted by name)", got[0].Name, got[1].Name)
		}
		if got[0].ModifiedAt != "" || got[0].SizeBytes != 0 {
			t.Errorf("alpha = %q/%d, want zero ModifiedAt and SizeBytes (the list carries only the id)", got[0].ModifiedAt, got[0].SizeBytes)
		}
	})

	t.Run("bearer header present when the key is set", func(t *testing.T) {
		t.Parallel()

		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))
		defer srv.Close()

		c := newTestClient(t, srv.URL, func(cfg *openai.Config) { cfg.APIKey = "sekrit" })
		if _, err := c.ListModels(context.Background()); err != nil {
			t.Fatalf("ListModels error = %v, want nil", err)
		}
		if gotAuth != "Bearer sekrit" {
			t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer sekrit")
		}
	})

	t.Run("non-2xx carries status and body snippet", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "server says no", http.StatusForbidden)
		}))
		defer srv.Close()

		_, err := newTestClient(t, srv.URL).ListModels(context.Background())
		if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "server says no") {
			t.Errorf("error = %v, want a status and body snippet", err)
		}
	})
}

func TestChat500HTMLBodyTruncated(t *testing.T) {
	t.Parallel()

	html := "<html>" + strings.Repeat("x", 10_000) + "</html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(html))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).Chat(context.Background(), llm.Request{Model: "m"})
	if err == nil {
		t.Fatal("Chat succeeded, want error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %q, want it to contain 500", err)
	}
	if strings.Contains(err.Error(), html) {
		t.Error("error contains the full body, want truncation")
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("error = %q, want a truncation marker", err)
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
		_, _ = w.Write([]byte(`{
			"id": "resp-1",
			"choices": [{
				"message": {"role": "assistant", "content": "recovered"},
				"finish_reason": "stop"
			}]
		}`))
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
// the recovering response is an SSE body.
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
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"role":"assistant","content":"recovered"}}]}` + "\n\n"))
		_, _ = w.Write([]byte(`data: [DONE]` + "\n\n"))
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

// TestChatRetriesEOFOnlyOnce pins that the retry is a single attempt: a
// server that drops every connection fails after exactly two requests.
func TestChatRetriesEOFOnlyOnce(t *testing.T) {
	t.Parallel()

	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
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

	_, err := newTestClient(t, srv.URL).Chat(context.Background(), sampleRequest())
	if err == nil || !errors.Is(err, io.EOF) {
		t.Fatalf("Chat error = %v, want the EOF to surface after one retry", err)
	}
	if got := atomic.LoadInt32(&requests); got != 2 {
		t.Errorf("requests = %d, want 2 (one attempt plus one retry)", got)
	}
}

func TestChatContextCancellation(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := newTestClient(t, srv.URL).Chat(ctx, llm.Request{Model: "m"})
	if err == nil {
		t.Fatal("Chat succeeded, want error")
	}
	if !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Errorf("error = %q, want it to reference cancellation", err)
	}
}

func TestChatAPIKeyHeader(t *testing.T) {
	t.Parallel()

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"id":"r","choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, func(cfg *openai.Config) { cfg.APIKey = "sekrit" })
	if _, err := c.Chat(context.Background(), llm.Request{Model: "m"}); err != nil {
		t.Fatalf("Chat error = %v, want nil", err)
	}
	if gotAuth != "Bearer sekrit" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer sekrit")
	}
}

// TestChatReasoningEffort pins the thinking-effort request field: set on
// Config it reaches the wire on both the Chat and ChatStream paths, and
// unset it is omitted entirely so the server default applies.
func TestChatReasoningEffort(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var gotReq map[string]any
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Errorf("unmarshal request: %v", err)
		}
		if got, ok := gotReq["reasoning_effort"]; ok {
			if got != "max" {
				t.Errorf("reasoning_effort = %v, want %q", got, "max")
			}
		} else {
			t.Error("request carries no reasoning_effort field, want it set")
		}
		if got, _ := gotReq["stream"].(bool); got {
			w.Header().Set("Content-Type", "text/event-stream")
			sseChunks(w, `{"id":"r","choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r","choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, func(cfg *openai.Config) { cfg.ReasoningEffort = "max" })
	if _, err := c.Chat(context.Background(), llm.Request{Model: "m"}); err != nil {
		t.Fatalf("Chat error = %v, want nil", err)
	}
	if _, err := c.ChatStream(context.Background(), llm.Request{Model: "m"}, func(llm.Delta) error { return nil }); err != nil {
		t.Fatalf("ChatStream error = %v, want nil", err)
	}
}

// TestChatNoReasoningEffortOmitted pins that an unset reasoning effort
// leaves the field off the wire entirely.
func TestChatNoReasoningEffortOmitted(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var gotReq map[string]any
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Errorf("unmarshal request: %v", err)
		}
		if _, ok := gotReq["reasoning_effort"]; ok {
			t.Error("request carries reasoning_effort, want it omitted when unset")
		}
		_, _ = w.Write([]byte(`{"id":"r","choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	if _, err := newTestClient(t, srv.URL).Chat(context.Background(), llm.Request{Model: "m"}); err != nil {
		t.Fatalf("Chat error = %v, want nil", err)
	}
}

func TestChatInvalidJSONResponse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{oops`))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).Chat(context.Background(), llm.Request{Model: "m"})
	if err == nil {
		t.Fatal("Chat succeeded, want error")
	}
	if !strings.Contains(err.Error(), "decode response") {
		t.Errorf("error = %q, want a decode error", err)
	}
}

func TestChatEmptyChoices(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"r","choices":[]}`))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).Chat(context.Background(), llm.Request{Model: "m"})
	if err == nil {
		t.Fatal("Chat succeeded, want error")
	}
	if !strings.Contains(err.Error(), "no choices") {
		t.Errorf("error = %q, want a no-choices error", err)
	}
}

func TestChatToolCallMissingID(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"r","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"","type":"function","function":{"name":"ls","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).Chat(context.Background(), llm.Request{Model: "m"})
	if err == nil {
		t.Fatal("Chat succeeded, want error")
	}
	if !strings.Contains(err.Error(), "missing id") {
		t.Errorf("error = %q, want a missing-id error", err)
	}
}

// TestChatEmptyRoleDefaultsAssistant pins the role fallback: a response
// message with no role still decodes as an assistant message.
func TestChatEmptyRoleDefaultsAssistant(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"r","choices":[{"message":{"content":"roleless"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL).Chat(context.Background(), llm.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Chat error = %v, want nil", err)
	}
	if resp.Message.Role != llm.RoleAssistant {
		t.Errorf("Role = %q, want %q (the fallback)", resp.Message.Role, llm.RoleAssistant)
	}
	if resp.Message.Content != "roleless" {
		t.Errorf("Content = %q, want %q", resp.Message.Content, "roleless")
	}
}

// TestChatStreamToolCallTypeDefaultsToFunction pins the assembled type
// fallback: fragments without a type still assemble as function calls.
func TestChatStreamToolCallTypeDefaultsToFunction(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseChunks(w,
			`{"id":"r","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"ls","arguments":"{}"}}]},"finish_reason":null}]}`,
			`{"id":"r","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		)
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{Model: "m"}, func(llm.Delta) error { return nil })
	if err != nil {
		t.Fatalf("ChatStream error = %v, want nil", err)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(resp.Message.ToolCalls))
	}
	tc := resp.Message.ToolCalls[0]
	if tc.Type != llm.ToolCallType {
		t.Errorf("Type = %q, want %q (the fallback)", tc.Type, llm.ToolCallType)
	}
}

func TestChatToolCallMissingName(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"r","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"","arguments":""}}]},"finish_reason":"tool_calls"}]}`))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).Chat(context.Background(), llm.Request{Model: "m"})
	if err == nil {
		t.Fatal("Chat succeeded, want error")
	}
	if !strings.Contains(err.Error(), "missing name") {
		t.Errorf("error = %q, want a missing-name error", err)
	}
}

func TestChatToolCallsRoundTripNestedShape(t *testing.T) {
	t.Parallel()

	var gotReq struct {
		Messages []json.RawMessage `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Errorf("unmarshal request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r","choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"current_time","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`))
	}))
	defer srv.Close()

	req := llm.Request{
		Model: "m",
		Messages: []llm.Message{
			{
				Role: llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{
					{ID: "call_0", Type: "function", FunctionName: "echo", FunctionArgs: `{"msg":"hi"}`},
				},
			},
			llm.NewToolResultMessage("call_0", "echo: hi", false),
		},
	}
	resp, err := newTestClient(t, srv.URL).Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat error = %v, want nil", err)
	}

	// Outgoing history uses the nested wire shape, with content always
	// present (servers reject non-assistant messages lacking the field).
	if len(gotReq.Messages) != 2 {
		t.Fatalf("wire messages = %d, want 2", len(gotReq.Messages))
	}
	historyCall := `{"role":"assistant","content":"","tool_calls":[{"id":"call_0","type":"function","function":{"name":"echo","arguments":"{\"msg\":\"hi\"}"}}]}`
	if string(gotReq.Messages[0]) != historyCall {
		t.Errorf("wire assistant message = %s, want %s", gotReq.Messages[0], historyCall)
	}

	// Incoming tool calls decode from the nested wire shape.
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("decoded tool calls = %d, want 1", len(resp.Message.ToolCalls))
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.FunctionName != "current_time" || tc.FunctionArgs != "{}" || tc.Type != "function" {
		t.Errorf("decoded tool call = %+v, want call_1/current_time/{}", tc)
	}
}

func TestChatReasoningContentRoundTrip(t *testing.T) {
	t.Parallel()

	var gotReq struct {
		Messages []wireMessageForTest `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Errorf("unmarshal request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r","choices":[{"message":{"role":"assistant","content":"2 + 2 = 4","reasoning_content":"The user asks a sum. 2+2 = 4."},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	// An assistant message carrying reasoning survives the round trip:
	// decoded reasoning_content comes back as Reasoning, and re-sent
	// history includes it under the wire field name.
	c := newTestClient(t, srv.URL)
	first, err := c.Chat(context.Background(), llm.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Chat error = %v, want nil", err)
	}
	if got, want := first.Message.Reasoning, "The user asks a sum. 2+2 = 4."; got != want {
		t.Errorf("decoded reasoning = %q, want %q", got, want)
	}
	if got, want := first.Message.Content, "2 + 2 = 4"; got != want {
		t.Errorf("decoded content = %q, want %q", got, want)
	}

	req := llm.Request{
		Model: "m",
		Messages: []llm.Message{
			// Tool-call round: reasoning is resent alongside the calls.
			{
				Role:      llm.RoleAssistant,
				Content:   "",
				Reasoning: "The user asks a sum. 2+2 = 4.",
				ToolCalls: []llm.ToolCall{
					{ID: "call_0", Type: "function", FunctionName: "echo", FunctionArgs: "{}"},
				},
			},
			llm.NewToolResultMessage("call_0", "echo: {}", false),
			// Final answer: reasoning is dropped, not resent.
			{
				Role:      llm.RoleAssistant,
				Content:   "2 + 2 = 4",
				Reasoning: "The user asks a sum. 2+2 = 4.",
			},
			llm.NewTextMessage(llm.RoleUser, "and 3 + 3?"),
		},
	}
	if _, err := c.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat error = %v, want nil", err)
	}

	if len(gotReq.Messages) != 4 {
		t.Fatalf("wire messages = %d, want 4", len(gotReq.Messages))
	}
	toolRound := gotReq.Messages[0]
	if got, want := toolRound.Reasoning, "The user asks a sum. 2+2 = 4."; got != want {
		t.Errorf("wire reasoning_content on tool-call message = %q, want %q", got, want)
	}
	finalAnswer := gotReq.Messages[2]
	if got := finalAnswer.Reasoning; got != "" {
		t.Errorf("wire reasoning_content on final answer = %q, want empty and omitted", got)
	}
	// Messages without reasoning must not carry the field at all.
	if gotReq.Messages[3].Reasoning != "" {
		t.Errorf("wire reasoning_content on user message = %q, want empty and omitted", gotReq.Messages[3].Reasoning)
	}
}

// sseChunks writes each payload as an SSE data line followed by a blank
// line, then [DONE].
func sseChunks(w io.Writer, payloads ...string) {
	for _, p := range payloads {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", p)
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func TestChatStreamContentDeltas(t *testing.T) {
	t.Parallel()

	var gotReq map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Errorf("unmarshal request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		sseChunks(w,
			`{"id":"resp-1","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`{"id":"resp-1","choices":[{"delta":{"content":"Hel"},"finish_reason":null}]}`,
			`{"id":"resp-1","choices":[{"delta":{"content":"lo, "},"finish_reason":null}]}`,
			`{"id":"resp-1","choices":[{"delta":{"content":"world"},"finish_reason":null}]}`,
			`{"id":"resp-1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14}}`,
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
		content.WriteString(d.Content)
	}
	if content.String() != "Hello, world" {
		t.Errorf("concatenated content = %q, want %q", content.String(), "Hello, world")
	}
	for i, d := range deltas {
		if d.Reasoning != "" || d.ToolCall != nil {
			t.Errorf("deltas[%d] = %+v, want content-only", i, d)
		}
	}

	if resp.ID != "resp-1" {
		t.Errorf("resp.ID = %q, want %q", resp.ID, "resp-1")
	}
	if resp.Message.Role != llm.RoleAssistant || resp.Message.Content != "Hello, world" {
		t.Errorf("resp.Message = %+v, want assistant 'Hello, world'", resp.Message)
	}
	if resp.FinishReason != llm.FinishStop {
		t.Errorf("resp.FinishReason = %q, want %q", resp.FinishReason, llm.FinishStop)
	}
	if resp.Usage.TotalTokens != 14 {
		t.Errorf("resp.Usage.TotalTokens = %d, want 14", resp.Usage.TotalTokens)
	}

	// The request must carry the streaming flags.
	if gotReq["stream"] != true {
		t.Errorf("request stream = %v, want true", gotReq["stream"])
	}
	opts, ok := gotReq["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Errorf("request stream_options = %v, want include_usage true", gotReq["stream_options"])
	}
}

func TestChatStreamReasoningContent(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseChunks(w,
			`{"id":"r","choices":[{"delta":{"reasoning_content":"think "},"finish_reason":null}]}`,
			`{"id":"r","choices":[{"delta":{"reasoning_content":"hard"},"finish_reason":null}]}`,
			`{"id":"r","choices":[{"delta":{"content":"42"},"finish_reason":null}]}`,
			`{"id":"r","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		)
	}))
	defer srv.Close()

	var deltas []llm.Delta
	resp, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{Model: "m"}, func(d llm.Delta) error {
		deltas = append(deltas, d)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream error = %v, want nil", err)
	}

	if len(deltas) != 3 {
		t.Fatalf("deltas = %+v, want 3", deltas)
	}
	// Reasoning fragments arrive before the content fragment, in order.
	if got, want := deltas[0].Reasoning+deltas[1].Reasoning, "think hard"; got != want {
		t.Errorf("reasoning fragments = %q, want %q", got, want)
	}
	if got := deltas[2].Content; got != "42" {
		t.Errorf("content fragment = %q, want %q", got, "42")
	}
	if got, want := resp.Message.Reasoning, "think hard"; got != want {
		t.Errorf("resp.Message.Reasoning = %q, want %q", got, want)
	}
	if got := resp.Message.Content; got != "42" {
		t.Errorf("resp.Message.Content = %q, want %q", got, "42")
	}
}

func TestChatStreamToolCallFragments(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseChunks(w,
			`{"id":"r","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"ls","arguments":""}}]},"finish_reason":null}]}`,
			`{"id":"r","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"pa"}}]},"finish_reason":null}]}`,
			`{"id":"r","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\".\"}"}}]},"finish_reason":null}]}`,
			`{"id":"r","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		)
	}))
	defer srv.Close()

	var deltas []llm.Delta
	resp, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{Model: "m"}, func(d llm.Delta) error {
		deltas = append(deltas, d)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream error = %v, want nil", err)
	}

	// Fragments are emitted in order, carrying what each chunk held.
	if len(deltas) != 3 {
		t.Fatalf("deltas = %+v, want 3", deltas)
	}
	first := deltas[0].ToolCall
	if first == nil || first.Index != 0 || first.ID != "call_1" || first.Type != "function" || first.Name != "ls" || first.Arguments != "" {
		t.Errorf("deltas[0].ToolCall = %+v, want index 0 call_1/ls with empty args", first)
	}
	if got := deltas[1].ToolCall; got == nil || got.Index != 0 || got.Arguments != `{"pa` {
		t.Errorf("deltas[1].ToolCall = %+v, want index 0 fragment %q", got, `{"pa`)
	}
	if got := deltas[2].ToolCall; got == nil || got.Index != 0 || got.Arguments != `th":"."}` {
		t.Errorf("deltas[2].ToolCall = %+v, want index 0 fragment %q", got, `th":"."}`)
	}

	// The final response assembles the fragments into one complete call.
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want 1", resp.Message.ToolCalls)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Type != "function" || tc.FunctionName != "ls" || tc.FunctionArgs != `{"path":"."}` {
		t.Errorf("assembled tool call = %+v, want call_1/ls with full args", tc)
	}
	if resp.FinishReason != llm.FinishToolCalls {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, llm.FinishToolCalls)
	}
}

func TestChatStreamMultipleToolCalls(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseChunks(w,
			`{"id":"r","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"ls","arguments":"{}"}}]},"finish_reason":null}]}`,
			`{"id":"r","choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"pwd","arguments":""}}]},"finish_reason":null}]}`,
			`{"id":"r","choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{}"}}]},"finish_reason":null}]}`,
			`{"id":"r","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		)
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{Model: "m"}, func(d llm.Delta) error {
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream error = %v, want nil", err)
	}

	if len(resp.Message.ToolCalls) != 2 {
		t.Fatalf("tool calls = %+v, want 2", resp.Message.ToolCalls)
	}
	if got := resp.Message.ToolCalls[0]; got.ID != "call_1" || got.FunctionName != "ls" {
		t.Errorf("tool call 0 = %+v, want call_1/ls", got)
	}
	if got := resp.Message.ToolCalls[1]; got.ID != "call_2" || got.FunctionName != "pwd" || got.FunctionArgs != "{}" {
		t.Errorf("tool call 1 = %+v, want call_2/pwd with full args", got)
	}
}

func TestChatStreamNon2xx(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"bad key"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{Model: "m"}, func(d llm.Delta) error {
		return nil
	})
	if err == nil {
		t.Fatal("ChatStream succeeded, want error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %q, want it to contain 401", err)
	}
}

func TestChatStreamOnDeltaErrorAborts(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseChunks(w,
			`{"id":"r","choices":[{"delta":{"content":"first"},"finish_reason":null}]}`,
			`{"id":"r","choices":[{"delta":{"content":"second"},"finish_reason":null}]}`,
		)
	}))
	defer srv.Close()

	wantErr := errors.New("stop streaming")
	var saw int
	_, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{Model: "m"}, func(d llm.Delta) error {
		saw++
		if saw == 2 {
			return wantErr
		}
		return nil
	})
	if err == nil || err != wantErr {
		t.Errorf("error = %v, want %v (returned as-is)", err, wantErr)
	}
	// The third chunk, if any, must not have been delivered.
	if saw > 2 {
		t.Errorf("onDelta called %d times, want the stream aborted after the error", saw)
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
				`{"id":"r","choices":[{"delta":{"reasoning_content":"pondering"},"finish_reason":null}]}`,
				`{"id":"r","choices":[{"delta":{},"finish_reason":"stop"}]}`,
			},
		},
		{
			name: "tool call fragment",
			chunks: []string{
				`{"id":"r","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"ls","arguments":"{}"}}]},"finish_reason":null}]}`,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				sseChunks(w, tc.chunks...)
			}))
			defer srv.Close()

			wantErr := errors.New("stop streaming")
			_, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{Model: "m"}, func(d llm.Delta) error {
				return wantErr
			})
			if err == nil || err != wantErr {
				t.Errorf("error = %v, want %v (returned as-is)", err, wantErr)
			}
		})
	}
}

func TestChatStreamDoneLessEOF(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// No [DONE] terminator: the stream simply ends.
		_, _ = io.WriteString(w, "data: "+`{"id":"r","choices":[{"delta":{"content":"eof"},"finish_reason":null}]}`+"\n\n")
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{Model: "m"}, func(d llm.Delta) error {
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream error = %v, want nil", err)
	}
	if got := resp.Message.Content; got != "eof" {
		t.Errorf("content = %q, want %q", got, "eof")
	}
}

func TestChatStreamContextCancellation(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	var closed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+`{"id":"r","choices":[{"delta":{"content":"one"},"finish_reason":null}]}`+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
	}))
	defer srv.Close()
	defer func() {
		if !closed {
			close(release)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	_, err := newTestClient(t, srv.URL).ChatStream(ctx, llm.Request{Model: "m"}, func(d llm.Delta) error {
		return nil
	})
	closed = true
	close(release)
	if err == nil {
		t.Fatal("ChatStream succeeded, want error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestChatStreamMalformedDataLineIgnored(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": comment\n")
		_, _ = io.WriteString(w, "event: ping\n\n")
		_, _ = io.WriteString(w, "data: not-json\n\n")
		sseChunks(w,
			`{"id":"r","choices":[{"delta":{"content":"ok"},"finish_reason":null}]}`,
			`{"id":"r","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		)
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{Model: "m"}, func(d llm.Delta) error {
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream error = %v, want nil", err)
	}
	if got := resp.Message.Content; got != "ok" {
		t.Errorf("content = %q, want %q", got, "ok")
	}
}

func TestChatStreamBodyOverLimitErrors(t *testing.T) {
	t.Parallel()

	// Enough chunks of content to exceed the 16 MiB accumulated cap.
	chunk := strings.Repeat("x", 1<<20)
	var payloads []string
	for range 17 {
		payloads = append(payloads,
			`{"id":"r","choices":[{"delta":{"content":"`+chunk+`"},"finish_reason":null}]}`)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseChunks(w, payloads...)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{Model: "m"}, func(d llm.Delta) error {
		return nil
	})
	if err == nil {
		t.Fatal("ChatStream succeeded, want a limit error")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error = %q, want a limit mention", err)
	}
}

func TestChatStreamToolCallMissingID(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseChunks(w,
			`{"id":"r","choices":[{"delta":{"tool_calls":[{"index":0,"id":"","type":"function","function":{"name":"ls","arguments":"{}"}}]},"finish_reason":null}]}`,
			`{"id":"r","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{Model: "m"}, func(d llm.Delta) error {
		return nil
	})
	if err == nil {
		t.Fatal("ChatStream succeeded, want error")
	}
	if !strings.Contains(err.Error(), "missing id") {
		t.Errorf("error = %q, want a missing-id error", err)
	}
}

func TestChatStreamToolCallMissingName(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseChunks(w,
			`{"id":"r","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"","arguments":""}}]},"finish_reason":null}]}`,
			`{"id":"r","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{Model: "m"}, func(d llm.Delta) error {
		return nil
	})
	if err == nil {
		t.Fatal("ChatStream succeeded, want error")
	}
	if !strings.Contains(err.Error(), "missing name") {
		t.Errorf("error = %q, want a missing-name error", err)
	}
}

func TestChatStreamReasoningOverLimitErrors(t *testing.T) {
	t.Parallel()

	// Enough chunks of reasoning to exceed the 16 MiB accumulated cap.
	chunk := strings.Repeat("x", 1<<20)
	var payloads []string
	for range 17 {
		payloads = append(payloads,
			`{"id":"r","choices":[{"delta":{"reasoning_content":"`+chunk+`"},"finish_reason":null}]}`)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseChunks(w, payloads...)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{Model: "m"}, func(d llm.Delta) error {
		return nil
	})
	if err == nil {
		t.Fatal("ChatStream succeeded, want a limit error")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error = %q, want a limit mention", err)
	}
}

func TestChatStreamToolCallArgumentsOverLimitErrors(t *testing.T) {
	t.Parallel()

	// Enough argument fragments to exceed the 16 MiB accumulated cap for
	// a single tool call.
	chunk := strings.Repeat("y", 1<<20)
	var payloads []string
	for range 17 {
		payloads = append(payloads,
			`{"id":"r","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"ls","arguments":"`+chunk+`"}}]},"finish_reason":null}]}`)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseChunks(w, payloads...)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{Model: "m"}, func(d llm.Delta) error {
		return nil
	})
	if err == nil {
		t.Fatal("ChatStream succeeded, want a limit error")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error = %q, want a limit mention", err)
	}
}

// TestChatStreamToolCallArgumentsOverLimitOnLaterFragment pins the
// existing-index branch of the arguments cap: the first fragment is small,
// and later fragments at the same index push the accumulation over. Each
// fragment stays under the scanner's line cap; the sum crosses the limit.
func TestChatStreamToolCallArgumentsOverLimitOnLaterFragment(t *testing.T) {
	t.Parallel()

	frag := strings.Repeat("y", 1<<20)
	var payloads []string
	payloads = append(payloads,
		`{"id":"r","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"ls","arguments":"{}"}}]},"finish_reason":null}]}`)
	for range 16 {
		payloads = append(payloads,
			`{"id":"r","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"`+frag+`"}}]},"finish_reason":null}]}`)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseChunks(w, payloads...)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{Model: "m"}, func(d llm.Delta) error {
		return nil
	})
	if err == nil {
		t.Fatal("ChatStream succeeded, want a limit error")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error = %q, want a limit mention", err)
	}
}

func TestChatStreamEmptyBodyErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"comments and blank lines", ": keepalive\n\n\n: another comment\n\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			_, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{Model: "m"}, func(d llm.Delta) error {
				return nil
			})
			if err == nil {
				t.Fatal("ChatStream succeeded, want a no-data error")
			}
			if !strings.Contains(err.Error(), "no data in stream") {
				t.Errorf("error = %q, want a no-data-in-stream error", err)
			}
		})
	}
}

func TestChatStreamNon2xxBodyOverLimitErrors(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 17 MiB error body, exceeding the 16 MiB read cap.
		http.Error(w, strings.Repeat("x", 17<<20), http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{Model: "m"}, func(d llm.Delta) error {
		return nil
	})
	if err == nil {
		t.Fatal("ChatStream succeeded, want error")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error = %q, want a limit mention", err)
	}
}

func TestChatImplementsStreamingClient(t *testing.T) {
	t.Parallel()

	var _ llm.StreamingClient = (*openai.Client)(nil)
}

func TestNewRejectsBadBaseURL(t *testing.T) {
	t.Parallel()

	cases := []string{
		"",
		"ftp://example.test",
		"/just/a/path",
	}

	for _, baseURL := range cases {
		t.Run(baseURL, func(t *testing.T) {
			t.Parallel()

			if _, err := openai.New(openai.Config{BaseURL: baseURL}); err == nil {
				t.Errorf("New(%q) succeeded, want error", baseURL)
			}
		})
	}
}

func TestChatBodyOverLimitErrors(t *testing.T) {
	t.Parallel()

	// 17 MiB response, exceeding the 16 MiB read cap.
	big := strings.Repeat("x", 17<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r","choices":[{"message":{"role":"assistant","content":"`))
		_, _ = w.Write([]byte(big))
		_, _ = w.Write([]byte(`"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).Chat(context.Background(), llm.Request{Model: "m"})
	if err == nil {
		t.Fatal("Chat succeeded, want a limit error")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error = %q, want a limit mention", err)
	}
}

// TestChatBodyReadError pins the mid-body failure path: a server that
// drops the connection after a partial response surfaces as a read
// error, not a decode of the truncated body.
func TestChatBodyReadError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r","choi`))
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

	_, err := newTestClient(t, srv.URL).Chat(context.Background(), llm.Request{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "read response body") {
		t.Errorf("error = %v, want a read-response-body error", err)
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

	_, err := newTestClient(t, srv.URL).ChatStream(context.Background(), llm.Request{Model: "m"}, func(llm.Delta) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "read response body") {
		t.Errorf("error = %v, want a read-response-body error", err)
	}
}

func TestChatHungServerTimesOut(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	// Custom HTTPClient with a short timeout instead of waiting the default
	// 5 minutes.
	httpClient := &http.Client{Timeout: 200 * time.Millisecond}
	c := newTestClient(t, srv.URL, func(cfg *openai.Config) { cfg.HTTPClient = httpClient })

	start := time.Now()
	_, err := c.Chat(context.Background(), llm.Request{Model: "m"})
	if err == nil {
		t.Fatal("Chat succeeded, want a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Chat took %s, want a prompt timeout", elapsed)
	}
	if !strings.Contains(err.Error(), "Timeout") && !strings.Contains(err.Error(), "timeout") && !strings.Contains(err.Error(), "deadline") {
		t.Errorf("error = %q, want a timeout/deadline mention", err)
	}
}

func TestChatNonFunctionToolCallTypePreserved(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r","choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"custom_tool","function":{"name":"ls","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`))
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL).Chat(context.Background(), llm.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Chat error = %v, want nil", err)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(resp.Message.ToolCalls))
	}
	if got := resp.Message.ToolCalls[0].Type; got != "custom_tool" {
		t.Errorf("tool call type = %q, want the server-sent custom_tool", got)
	}
}

// TestChatToolCallTypeDefaultsToFunction pins the decode fallback: a
// non-streaming tool call with no type decodes as a function call.
func TestChatToolCallTypeDefaultsToFunction(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r","choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","function":{"name":"ls","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`))
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL).Chat(context.Background(), llm.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Chat error = %v, want nil", err)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(resp.Message.ToolCalls))
	}
	if got := resp.Message.ToolCalls[0].Type; got != llm.ToolCallType {
		t.Errorf("tool call type = %q, want %q (the fallback)", got, llm.ToolCallType)
	}
}

// failingSink is a logging.Sink whose Write always returns an error.
type failingSink struct{}

func (failingSink) Write(logging.Record) error { return errors.New("disk exploded") }

func TestChatLogsRequestAndResponse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Trace", "tr-1")
		_, _ = w.Write([]byte(`{"id":"resp-1","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	sink := logging.NewBuffered(0)
	c := newTestClient(t, srv.URL, func(cfg *openai.Config) { cfg.Sink = sink })
	resp, err := c.Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Chat error = %v, want nil", err)
	}
	if resp.ID != "resp-1" {
		t.Fatalf("resp.ID = %q, want resp-1", resp.ID)
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
	if !reqRec.Time.Before(respRec.Time) {
		t.Errorf("request time %v not before response time %v", reqRec.Time, respRec.Time)
	}
	if reqRec.Method != http.MethodPost {
		t.Errorf("request method = %q, want POST", reqRec.Method)
	}
	if want := srv.URL + "/chat/completions"; reqRec.URL != want {
		t.Errorf("request url = %q, want %q", reqRec.URL, want)
	}
	if got := reqRec.Headers["Content-Type"]; len(got) != 1 || got[0] != "application/json" {
		t.Errorf("request Content-Type = %v, want application/json", got)
	}

	var wire map[string]any
	if err := json.Unmarshal(reqRec.Body, &wire); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if wire["model"] != "gpt-4o-mini" {
		t.Errorf("logged request model = %v, want gpt-4o-mini", wire["model"])
	}
	msgs, ok := wire["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("logged request messages = %#v, want non-empty", wire["messages"])
	}
	first, _ := msgs[0].(map[string]any)
	if first["content"] != "You are helpful." {
		t.Errorf("logged first message = %v, want the system prompt", first)
	}

	if got := respRec.Headers["Status"]; len(got) != 1 || got[0] != "200" {
		t.Errorf("response Status header = %v, want 200", got)
	}
	if got := respRec.Headers["X-Trace"]; len(got) != 1 || got[0] != "tr-1" {
		t.Errorf("response X-Trace header = %v, want tr-1", got)
	}
	if string(respRec.Body) != `{"id":"resp-1","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}` {
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
	c := newTestClient(t, srv.URL, func(cfg *openai.Config) { cfg.Sink = sink })
	_, err := c.Chat(context.Background(), llm.Request{Model: "m"})
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

func TestChatStreamLogsAssembledResponse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseChunks(w,
			`{"id":"r","choices":[{"delta":{"role":"assistant","reasoning_content":"thinking hard"},"finish_reason":null}]}`,
			`{"id":"r","choices":[{"delta":{"content":"Hel"},"finish_reason":null}]}`,
			`{"id":"r","choices":[{"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
		)
	}))
	defer srv.Close()

	sink := logging.NewBuffered(0)
	c := newTestClient(t, srv.URL, func(cfg *openai.Config) { cfg.Sink = sink })
	resp, err := c.ChatStream(context.Background(), sampleRequest(), func(d llm.Delta) error {
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream error = %v, want nil", err)
	}
	if resp.Message.Content != "Hello" {
		t.Fatalf("resp content = %q, want Hello", resp.Message.Content)
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

	// The response record carries the assembled response, not the raw
	// SSE stream: same envelope shape as a non-streaming response body.
	var wire struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage llm.Usage `json:"usage"`
	}
	if err := json.Unmarshal(respRec.Body, &wire); err != nil {
		t.Fatalf("response body is not assembled JSON: %v (body: %s)", err, respRec.Body)
	}
	if len(wire.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(wire.Choices))
	}
	choice := wire.Choices[0]
	if wire.ID != "r" {
		t.Errorf("response id = %q, want r (round-trips)", wire.ID)
	}
	if choice.Message.Content != "Hello" {
		t.Errorf("response content = %q, want the concatenated Hello", choice.Message.Content)
	}
	if choice.Message.Role != "assistant" {
		t.Errorf("response role = %q, want assistant", choice.Message.Role)
	}
	// The log record is full-fidelity: the reasoning appears even though
	// this final answer has no tool calls (the request path would drop it).
	if choice.Message.Reasoning != "thinking hard" {
		t.Errorf("response reasoning_content = %q, want thinking hard", choice.Message.Reasoning)
	}
	if choice.FinishReason != "stop" {
		t.Errorf("response finish_reason = %q, want stop", choice.FinishReason)
	}
	if strings.Contains(string(respRec.Body), "data:") || strings.Contains(string(respRec.Body), "[DONE]") {
		t.Errorf("response body = %q, want no raw SSE markers", respRec.Body)
	}
	if !reqRec.Time.Before(respRec.Time) {
		t.Errorf("request time %v not before response time %v", reqRec.Time, respRec.Time)
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
			_, _ = io.WriteString(w, "data: "+s+"\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flush(`{"id":"r","choices":[{"delta":{"content":"first"},"finish_reason":null}]}`)
		time.Sleep(200 * time.Millisecond)
		flush(`{"id":"r","choices":[{"delta":{"content":"second"},"finish_reason":null}]}`)
		time.Sleep(200 * time.Millisecond)
		flush(`{"id":"r","choices":[{"delta":{"content":"never seen"},"finish_reason":null}]}`)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	sink := logging.NewBuffered(0)
	c := newTestClient(t, srv.URL, func(cfg *openai.Config) { cfg.Sink = sink })

	// Aborting readStream drops the connection so the server's late
	// chunks do not block the response log write.
	wantErr := errors.New("stop streaming")
	var saw int
	_, err := c.ChatStream(context.Background(), llm.Request{Model: "m"}, func(d llm.Delta) error {
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
	// The record carries the assembled response, not raw SSE lines. The
	// partial response is still valid assembled JSON with content.
	if strings.Contains(body, "data:") || strings.Contains(body, "[DONE]") {
		t.Errorf("logged body = %q, want assembled JSON, not raw SSE lines", body)
	}
	var wire struct {
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(recs[1].Body, &wire); err != nil {
		t.Fatalf("partial response body is not assembled JSON: %v (body: %s)", err, body)
	}
	if len(wire.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(wire.Choices))
	}
	if got := wire.Choices[0].Message.Content; got != "firstsecond" {
		t.Errorf("partial content = %q, want the concatenated firstsecond", got)
	}
}

func TestChatStreamLogsNoDataError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// A 200 with an empty SSE body: no data lines at all.
	}))
	defer srv.Close()

	sink := logging.NewBuffered(0)
	c := newTestClient(t, srv.URL, func(cfg *openai.Config) { cfg.Sink = sink })

	_, err := c.ChatStream(context.Background(), llm.Request{Model: "m"}, func(d llm.Delta) error {
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "no data in stream") {
		t.Fatalf("error = %v, want the no-data error", err)
	}

	recs := sink.Records()
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	body := string(recs[1].Body)
	if !strings.HasPrefix(body, "error:") || !strings.Contains(body, "no data in stream") {
		t.Errorf("response record body = %q, want error: ... mentioning no data", body)
	}
}

func TestChatStreamLogsNon2xx(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"bad key"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	sink := logging.NewBuffered(0)
	c := newTestClient(t, srv.URL, func(cfg *openai.Config) { cfg.Sink = sink })
	_, err := c.ChatStream(context.Background(), llm.Request{Model: "m"}, func(d llm.Delta) error {
		return nil
	})
	if err == nil {
		t.Fatal("ChatStream succeeded, want error")
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

func TestLoggingSinkErrorsAreIgnored(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	baselineClient := newTestClient(t, srv.URL)
	wantResp, err := baselineResp(t, baselineClient)
	if err != nil {
		t.Fatalf("baseline Chat error = %v, want nil", err)
	}

	failingClient := newTestClient(t, srv.URL, func(cfg *openai.Config) { cfg.Sink = failingSink{} })
	gotResp, err := failingClient.Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Errorf("Chat with failing sink error = %v, want nil", err)
	}
	if gotResp != nil && gotResp.Message.Content != wantResp.Message.Content {
		t.Errorf("Chat response content = %q, want %q (same as no sink)", gotResp.Message.Content, wantResp.Message.Content)
	}

	// ChatStream needs an SSE server, so it gets its own.
	streamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseChunks(w,
			`{"id":"r","choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
		)
	}))
	defer streamSrv.Close()

	sinkClient := newTestClient(t, streamSrv.URL, func(cfg *openai.Config) { cfg.Sink = failingSink{} })
	gotStream, gotStreamErr := sinkStream(t, sinkClient)
	if gotStreamErr != nil {
		t.Errorf("ChatStream with failing sink error = %v, want nil", gotStreamErr)
	}
	if gotStream != nil && gotStream.Message.Content != wantResp.Message.Content {
		t.Errorf("ChatStream response content = %q, want %q", gotStream.Message.Content, wantResp.Message.Content)
	}
}

// baselineResp captures the no-sink results the sink-error tests compare
// against.
func baselineResp(t *testing.T, c *openai.Client) (*llm.Response, error) {
	t.Helper()
	return c.Chat(context.Background(), sampleRequest())
}

func sinkStream(t *testing.T, c *openai.Client) (*llm.Response, error) {
	t.Helper()
	return c.ChatStream(context.Background(), sampleRequest(), func(d llm.Delta) error {
		return nil
	})
}

func TestChatCapturesCallStats(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r","choices":[{"message":{"role":"assistant","content":"the answer","reasoning_content":"hmm","tool_calls":[{"id":"call_1","type":"function","function":{"name":"ls","arguments":"{\"path\":\".\"}"}}]},"finish_reason":"tool_calls"}]}`))
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL).Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Chat error = %v, want nil", err)
	}

	// Recomputed from the fixture's expected content/reasoning/tool
	// call, pinning the component numbers themselves.
	wantOutput := llm.OutputBytes{
		Content:   len("the answer"),
		Reasoning: len("hmm"),
		ToolCalls: len("ls") + len(`{"path":"."}`),
	}
	if resp.Stats.Output != wantOutput {
		t.Errorf("Stats.Output = %+v, want %+v", resp.Stats.Output, wantOutput)
	}
	if resp.Stats.Elapsed <= 0 {
		t.Errorf("Stats.Elapsed = %v, want > 0", resp.Stats.Elapsed)
	}
	if resp.Stats.Output.Total() != resp.Stats.Output.Content+resp.Stats.Output.Reasoning+resp.Stats.Output.ToolCalls {
		t.Errorf("Output total %d inconsistent with components", resp.Stats.Output.Total())
	}
}

func TestChatStreamCapturesCallStats(t *testing.T) {
	t.Parallel()

	// Multi-chunk stream: deltas concatenate to the same message the
	// whole-message path would return, so streamed and non-streamed
	// stats must agree — the cross-path consistency pin.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseChunks(w,
			`{"id":"r","choices":[{"delta":{"reasoning_content":"think "},"finish_reason":null}]}`,
			`{"id":"r","choices":[{"delta":{"reasoning_content":"hard"},"finish_reason":null}]}`,
			`{"id":"r","choices":[{"delta":{"content":"Hello, "},"finish_reason":null}]}`,
			`{"id":"r","choices":[{"delta":{"content":"world"},"finish_reason":null}]}`,
			`{"id":"r","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"ls","arguments":"{\"pa"}}]},"finish_reason":null}]}`,
			`{"id":"r","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\".\"}"}}]},"finish_reason":null}]}`,
			`{"id":"r","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		)
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL).ChatStream(context.Background(), sampleRequest(), func(d llm.Delta) error {
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream error = %v, want nil", err)
	}

	wantOutput := llm.OutputBytes{
		Content:   len("Hello, world"),
		Reasoning: len("think hard"),
		ToolCalls: len("ls") + len(`{"path":"."}`),
	}
	if resp.Stats.Output != wantOutput {
		t.Errorf("Stats.Output = %+v, want %+v", resp.Stats.Output, wantOutput)
	}
	if resp.Stats.Elapsed <= 0 {
		t.Errorf("Stats.Elapsed = %v, want > 0", resp.Stats.Elapsed)
	}
	// Cross-path consistency: the assembled message measures the same
	// as the helper says it should.
	if got := resp.Message.OutputBytes(); got != wantOutput {
		t.Errorf("Message.OutputBytes() = %+v, want %+v", got, wantOutput)
	}
}
