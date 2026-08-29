package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/overspecific/goblorb/internal/config"
	"github.com/overspecific/goblorb/internal/engine"
	"github.com/overspecific/goblorb/internal/llm"
	"github.com/overspecific/goblorb/internal/tools"
)

// fakeClient returns canned responses in order and records requests.
type fakeClient struct {
	responses []llm.Response
	err       error
	requests  []llm.Request
}

func (f *fakeClient) Chat(_ context.Context, req llm.Request) (*llm.Response, error) {
	f.requests = append(f.requests, req)
	if len(f.responses) > 0 {
		resp := f.responses[0]
		f.responses = f.responses[1:]
		return &resp, nil
	}
	if f.err != nil {
		return nil, f.err
	}
	return nil, errors.New("fakeClient: no more canned responses")
}

func toolRegistry(t *testing.T) (*tools.Registry, *[]string) {
	t.Helper()

	var calls []string
	r, err := tools.NewRegistry([]config.ToolEntry{
		{Name: "echo", Description: "Echo stdin.", Command: []string{"sh", "-c", `printf '%s' "$(cat)"`}},
		{Name: "recorder", Description: "Records invocations.", Command: []string{"true"}},
	}, tools.WithTimeout(5_000_000_000))
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}
	return r, &calls
}

func textResp(content string) llm.Response {
	return llm.Response{
		ID:           "resp",
		Message:      llm.NewTextMessage(llm.RoleAssistant, content),
		FinishReason: llm.FinishStop,
	}
}

func toolCallResp(calls ...llm.ToolCall) llm.Response {
	return llm.Response{
		ID:           "resp",
		Message:      llm.Message{Role: llm.RoleAssistant, ToolCalls: calls},
		FinishReason: llm.FinishToolCalls,
	}
}

func call(id, name, encodedArgs string) llm.ToolCall {
	return llm.ToolCall{ID: id, Type: "function", FunctionName: name, FunctionArgs: encodedArgs}
}

func TestRunTurnPlainReply(t *testing.T) {
	t.Parallel()

	fc := &fakeClient{responses: []llm.Response{textResp("the answer")}}
	e := engine.New(engine.EngineConfig{
		Client:       fc,
		SystemPrompt: "You are helpful.",
	})

	var events []engine.Event
	final, err := e.RunTurn(context.Background(), "hi", func(ev engine.Event) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("RunTurn error = %v, want nil", err)
	}
	if final != "the answer" {
		t.Errorf("final = %q, want %q", final, "the answer")
	}

	if len(fc.requests) != 1 {
		t.Fatalf("API calls = %d, want 1", len(fc.requests))
	}
	msgs := fc.requests[0].Messages
	if len(msgs) != 2 {
		t.Fatalf("request messages = %d, want 2 (system, user)", len(msgs))
	}
	if msgs[0].Role != llm.RoleSystem || msgs[0].Content != "You are helpful." {
		t.Errorf("messages[0] = %+v, want system prompt", msgs[0])
	}
	if msgs[1].Role != llm.RoleUser || msgs[1].Content != "hi" {
		t.Errorf("messages[1] = %+v, want user message", msgs[1])
	}

	if len(events) != 1 || events[0].Kind != engine.EventAssistantText || events[0].Text != "the answer" {
		t.Errorf("events = %+v, want one assistant text event", events)
	}
}

func TestRunTurnSingleToolCall(t *testing.T) {
	t.Parallel()

	requireShell(t)

	r, ran := toolRegistry(t)
	fc := &fakeClient{responses: []llm.Response{
		toolCallResp(call("call_1", "echo", `{"msg":"hello"}`)),
		textResp("done"),
	}}
	e := engine.New(engine.EngineConfig{Client: fc, Tools: r})

	var events []engine.Event
	final, err := e.RunTurn(context.Background(), "run it", func(ev engine.Event) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("RunTurn error = %v, want nil", err)
	}
	if final != "done" {
		t.Errorf("final = %q, want %q", final, "done")
	}
	if len(*ran) != 0 {
		t.Errorf("unexpected recorder calls: %v", *ran)
	}

	// History: user, assistant-with-tool-call, tool-result. No system prompt
	// is configured, so the tool result is the third message sent.
	if len(fc.requests) != 2 {
		t.Fatalf("API calls = %d, want 2", len(fc.requests))
	}
	second := fc.requests[1].Messages
	if len(second) != 3 {
		t.Fatalf("second request messages = %d, want 3", len(second))
	}
	toolMsg := second[2]
	if toolMsg.Role != llm.RoleTool || toolMsg.ToolCallID != "call_1" {
		t.Errorf("tool result message = %+v, want role tool with tool_call_id call_1", toolMsg)
	}

	var gotArgs struct {
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal([]byte(second[1].ToolCalls[0].FunctionArgs), &gotArgs); err != nil {
		t.Fatalf("unmarshal assistant tool call args: %v", err)
	}
	if gotArgs.Msg != "hello" {
		t.Errorf("assistant tool call args = %+v, want msg=hello", gotArgs)
	}

	kinds := []engine.EventKind{}
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
	}
	// The tool response carries no content, so the sequence is tool call,
	// tool result, then the final assistant text.
	want := []engine.EventKind{engine.EventToolCall, engine.EventToolResult, engine.EventAssistantText}
	if len(kinds) != len(want) {
		t.Fatalf("event kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("event kind[%d] = %v, want %v", i, kinds[i], want[i])
		}
	}
}

func TestRunTurnTwoToolsInOneReply(t *testing.T) {
	t.Parallel()

	requireShell(t)

	r, _ := toolRegistry(t)
	fc := &fakeClient{responses: []llm.Response{
		toolCallResp(
			call("call_a", "echo", `{"n":1}`),
			call("call_b", "echo", `{"n":2}`),
		),
		textResp("both done"),
	}}
	e := engine.New(engine.EngineConfig{Client: fc, Tools: r})

	if _, err := e.RunTurn(context.Background(), "go", func(engine.Event) error { return nil }); err != nil {
		t.Fatalf("RunTurn error = %v, want nil", err)
	}

	if len(fc.requests) != 2 {
		t.Fatalf("API calls = %d, want 2", len(fc.requests))
	}
	toolMsgs := fc.requests[1].Messages[2:]
	if len(toolMsgs) != 2 {
		t.Fatalf("tool result messages = %d, want 2", len(toolMsgs))
	}
	if toolMsgs[0].ToolCallID != "call_a" || toolMsgs[1].ToolCallID != "call_b" {
		t.Errorf("tool call ids = [%s %s], want [call_a call_b]", toolMsgs[0].ToolCallID, toolMsgs[1].ToolCallID)
	}
}

func maybeTwoCalls(t *testing.T) *tools.Registry {
	t.Helper()
	r, _ := toolRegistry(t)
	return r
}

func TestRunTurnToolInfraErrorContinues(t *testing.T) {
	t.Parallel()

	// Registry without the tool the model calls: an infrastructure error.
	r, err := tools.NewRegistry(nil)
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	fc := &fakeClient{responses: []llm.Response{
		toolCallResp(call("call_x", "missing_tool", "{}")),
		textResp("recovered"),
	}}
	e := engine.New(engine.EngineConfig{Client: fc, Tools: r})

	final, err := e.RunTurn(context.Background(), "go", func(engine.Event) error { return nil })
	if err != nil {
		t.Fatalf("RunTurn error = %v, want nil", err)
	}
	if final != "recovered" {
		t.Errorf("final = %q, want %q", final, "recovered")
	}

	// History: user, assistant-with-tool-call, tool-result. No system prompt
	// is configured, so the request carries those three directly.
	toolMsg := fc.requests[1].Messages[2]
	if toolMsg.Role != llm.RoleTool || !strings.Contains(toolMsg.Content, "unknown tool") {
		t.Errorf("tool result message = %+v, want error content mentioning unknown tool", toolMsg)
	}
}

func TestRunTurnMalformedToolCallArgsRecover(t *testing.T) {
	t.Parallel()

	// A degenerate tool call with invalid JSON args must not abort the
	// turn: the engine feeds the decode error back as a failed tool result
	// so the model gets another chance.
	fc := &fakeClient{responses: []llm.Response{
		toolCallResp(call("call_bad", "echo", `not json`)),
		textResp("recovered"),
	}}
	e := engine.New(engine.EngineConfig{Client: fc})

	var events []engine.Event
	final, err := e.RunTurn(context.Background(), "go", func(ev engine.Event) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("RunTurn error = %v, want nil", err)
	}
	if final != "recovered" {
		t.Errorf("final = %q, want %q", final, "recovered")
	}

	if len(fc.requests) != 2 {
		t.Fatalf("API calls = %d, want 2", len(fc.requests))
	}
	toolMsg := fc.requests[1].Messages[2]
	if toolMsg.Role != llm.RoleTool || toolMsg.ToolCallID != "call_bad" {
		t.Errorf("tool result message = %+v, want role tool with tool_call_id call_bad", toolMsg)
	}

	var failed []engine.Event
	for _, ev := range events {
		if ev.Kind == engine.EventToolResult && ev.Failed {
			failed = append(failed, ev)
		}
	}
	if len(failed) != 1 || failed[0].Name != "echo" {
		t.Errorf("failed tool result events = %+v, want one for echo", failed)
	}
}

func TestRunTurnTooManyTurns(t *testing.T) {
	t.Parallel()

	loopResp := toolCallResp(call("call_loop", "echo", "{}"))
	fc := &fakeClient{
		responses: []llm.Response{loopResp, loopResp},
	}
	e := engine.New(engine.EngineConfig{
		Client:   fc,
		MaxTurns: 2,
	})

	_, err := e.RunTurn(context.Background(), "go", func(engine.Event) error { return nil })
	if err == nil {
		t.Fatal("RunTurn succeeded, want ErrTooManyTurns")
	}
	if !errors.Is(err, engine.ErrTooManyTurns) {
		t.Errorf("error = %v, want ErrTooManyTurns", err)
	}
}

func TestRunTurnTooManyTurnsWithPartialText(t *testing.T) {
	t.Parallel()

	requireShell(t)

	r := maybeTwoCalls(t)
	// First call returns content plus a tool call; the content is emitted as
	// partial text before execution continues.
	respWithBoth := llm.Response{
		ID: "resp",
		Message: llm.Message{
			Role:    llm.RoleAssistant,
			Content: "partial answer",
			ToolCalls: []llm.ToolCall{
				call("call_1", "echo", "{}"),
			},
		},
		FinishReason: llm.FinishToolCalls,
	}
	fc := &fakeClient{responses: []llm.Response{respWithBoth}}
	e := engine.New(engine.EngineConfig{Client: fc, Tools: r, MaxTurns: 1})

	_, err := e.RunTurn(context.Background(), "go", func(engine.Event) error { return nil })
	if err == nil || !errors.Is(err, engine.ErrTooManyTurns) {
		t.Fatalf("error = %v, want ErrTooManyTurns", err)
	}
	if !strings.Contains(err.Error(), "partial answer") {
		t.Errorf("error = %q, want it to wrap the last assistant message", err)
	}
}

func TestRunTurnContentThenToolCalls(t *testing.T) {
	t.Parallel()

	requireShell(t)

	r := maybeTwoCalls(t)
	respWithBoth := llm.Response{
		ID: "resp",
		Message: llm.Message{
			Role:    llm.RoleAssistant,
			Content: "let me check",
			ToolCalls: []llm.ToolCall{
				call("call_1", "echo", "{}"),
			},
		},
		FinishReason: llm.FinishToolCalls,
	}
	fc := &fakeClient{responses: []llm.Response{respWithBoth, textResp("final")}}
	e := engine.New(engine.EngineConfig{Client: fc, Tools: r})

	var texts []string
	final, err := e.RunTurn(context.Background(), "go", func(ev engine.Event) error {
		if ev.Kind == engine.EventAssistantText {
			texts = append(texts, ev.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RunTurn error = %v, want nil", err)
	}
	if final != "final" {
		t.Errorf("final = %q, want %q", final, "final")
	}
	if len(texts) != 2 || texts[0] != "let me check" || texts[1] != "final" {
		t.Errorf("text events = %v, want [let me check final]", texts)
	}
}

func TestRunTurnOnEventErrorAborts(t *testing.T) {
	t.Parallel()

	fc := &fakeClient{responses: []llm.Response{textResp("should not finish")}}
	e := engine.New(engine.EngineConfig{Client: fc})
	boom := errors.New("stop please")

	_, err := e.RunTurn(context.Background(), "hi", func(engine.Event) error { return boom })
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want the onEvent error", err)
	}
}

func TestRunTurnContextCancellation(t *testing.T) {
	t.Parallel()

	fc := &fakeClient{err: context.Canceled}
	e := engine.New(engine.EngineConfig{Client: fc})

	_, err := e.RunTurn(context.Background(), "hi", func(engine.Event) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestHistoryDefensiveCopy(t *testing.T) {
	t.Parallel()

	fc := &fakeClient{responses: []llm.Response{textResp("answer")}}
	e := engine.New(engine.EngineConfig{Client: fc})

	if _, err := e.RunTurn(context.Background(), "q", func(engine.Event) error { return nil }); err != nil {
		t.Fatalf("RunTurn error = %v, want nil", err)
	}

	h := e.History()
	if len(h) != 2 {
		t.Fatalf("history length = %d, want 2", len(h))
	}
	h[0].Content = "tampered"
	h = append(h[:0], llm.Message{})

	again := e.History()
	if len(again) != 2 {
		t.Fatalf("history length after mutation = %d, want 2", len(again))
	}
	if again[0].Content == "tampered" {
		t.Error("History() returns a reference to internal state, want a copy")
	}
}

func TestRunTurnAbortedTurnRepairsHistory(t *testing.T) {
	t.Parallel()

	requireShell(t)

	r, _ := toolRegistry(t)
	// First response makes two tool calls, then the fake client errors as
	// if the turn was cancelled mid-tool-loop.
	fc := &fakeClient{
		responses: []llm.Response{
			toolCallResp(
				call("call_a", "echo", `{"n":1}`),
				call("call_b", "echo", `{"n":2}`),
			),
		},
		err: context.Canceled,
	}
	e := engine.New(engine.EngineConfig{Client: fc, Tools: r})

	if _, err := e.RunTurn(context.Background(), "go", func(engine.Event) error { return nil }); err == nil {
		t.Fatal("RunTurn succeeded, want the injected error")
	}

	// The second call failed, so only one tool result was recorded. The
	// repair pass must have backfilled the missing one so the next
	// request's message sequence is protocol-valid.
	h := e.History()
	var toolResults []llm.Message
	for _, m := range h {
		if m.Role == llm.RoleTool {
			toolResults = append(toolResults, m)
		}
	}
	if len(toolResults) != 2 {
		t.Fatalf("tool result messages after repair = %d, want 2", len(toolResults))
	}
	if toolResults[0].ToolCallID != "call_a" || toolResults[1].ToolCallID != "call_b" {
		t.Errorf("tool result ids = [%s %s], want [call_a call_b]", toolResults[0].ToolCallID, toolResults[1].ToolCallID)
	}
}

func requireShell(t *testing.T) {
	t.Helper()
}
