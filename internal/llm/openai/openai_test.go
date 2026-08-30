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
	"testing"
	"time"

	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/llm/openai"
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
