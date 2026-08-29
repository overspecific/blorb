package openai_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/overspecific/goblorb/internal/llm"
	"github.com/overspecific/goblorb/internal/llm/openai"
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
