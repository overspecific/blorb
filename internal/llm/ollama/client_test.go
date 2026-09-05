package ollama

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

	for _, base := range []string{"", "ftp://example.com", "http://"} {
		if _, err := New(Config{BaseURL: base}); err == nil {
			t.Errorf("New(base_url %q) succeeded, want error", base)
		}
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
