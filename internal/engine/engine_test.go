package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/engine"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/tools"
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

// streamingClient is a streaming fake: each canned response is broken into
// deltas that are emitted in order before the complete response is returned.
type streamingClient struct {
	responses []llm.Response
	err       error
	requests  []llm.Request
}

func (f *streamingClient) Chat(_ context.Context, req llm.Request) (*llm.Response, error) {
	f.requests = append(f.requests, req)
	if len(f.responses) > 0 {
		resp := f.responses[0]
		f.responses = f.responses[1:]
		return &resp, nil
	}
	if f.err != nil {
		return nil, f.err
	}
	return nil, errors.New("streamingClient: no more canned responses")
}

func (f *streamingClient) ChatStream(_ context.Context, req llm.Request, onDelta func(llm.Delta) error) (*llm.Response, error) {
	f.requests = append(f.requests, req)
	if len(f.responses) == 0 {
		if f.err != nil {
			return nil, f.err
		}
		return nil, errors.New("streamingClient: no more canned responses")
	}
	resp := f.responses[0]
	f.responses = f.responses[1:]

	msg := resp.Message
	if msg.Reasoning != "" {
		if err := onDelta(llm.Delta{Reasoning: msg.Reasoning}); err != nil {
			return nil, err
		}
	}
	if msg.Content != "" {
		if err := onDelta(llm.Delta{Content: msg.Content}); err != nil {
			return nil, err
		}
	}
	for i, tc := range msg.ToolCalls {
		if err := onDelta(llm.Delta{ToolCall: &llm.ToolCallDelta{
			Index:     i,
			ID:        tc.ID,
			Type:      tc.Type,
			Name:      tc.FunctionName,
			Arguments: tc.FunctionArgs,
		}}); err != nil {
			return nil, err
		}
	}
	return &resp, nil
}

func toolRegistry(t *testing.T) *tools.Registry {
	t.Helper()

	r, err := tools.NewRegistry([]config.ToolEntry{
		{Type: config.ToolTypeCommand, Name: "echo", Description: "Echo stdin.", Command: []string{"sh", "-c", `printf '%s' "$(cat)"`}},
		{Type: config.ToolTypeCommand, Name: "recorder", Description: "Records invocations.", Command: []string{"true"}},
	}, tools.WithTimeout(5_000_000_000))
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}
	return r
}

func textResp(content string) llm.Response {
	return llm.Response{
		ID:           "resp",
		Message:      llm.NewTextMessage(llm.RoleAssistant, content),
		FinishReason: llm.FinishStop,
	}
}

func usageResp(content string, usage llm.Usage) llm.Response {
	resp := textResp(content)
	resp.Usage = usage
	return resp
}

func statsResp(content string, usage llm.Usage, stats llm.CallStats) llm.Response {
	resp := usageResp(content, usage)
	resp.Stats = stats
	return resp
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

	if len(events) != 2 || events[0].Kind != engine.EventUsage || events[1].Kind != engine.EventAssistantText || events[1].Text != "the answer" {
		t.Errorf("events = %+v, want a usage event then one assistant text event", events)
	}
}

// TestEngineSamplingParamsOnEveryRequest pins that the configured
// Sampling reaches every llm.Request the engine builds: the initial call
// and the follow-up call after a tool round.
func TestEngineSamplingParamsOnEveryRequest(t *testing.T) {
	t.Parallel()

	temp := 0.0 // explicit zero must survive the plumbing
	maxTokens := 64
	sampling := llm.SamplingParams{Temperature: &temp, MaxTokens: &maxTokens}

	r := toolRegistry(t)
	fc := &fakeClient{responses: []llm.Response{
		toolCallResp(call("call_1", "echo", `{"msg":"hello"}`)),
		textResp("done"),
	}}
	e := engine.New(engine.EngineConfig{Client: fc, Tools: r, Sampling: sampling})

	if _, err := e.RunTurn(context.Background(), "go", func(engine.Event) error { return nil }); err != nil {
		t.Fatalf("RunTurn error = %v, want nil", err)
	}
	if len(fc.requests) != 2 {
		t.Fatalf("API calls = %d, want 2", len(fc.requests))
	}
	for i, req := range fc.requests {
		if req.Sampling.Temperature == nil || *req.Sampling.Temperature != 0 {
			t.Errorf("request %d Sampling.Temperature = %v, want the explicit 0", i, req.Sampling.Temperature)
		}
		if req.Sampling.MaxTokens == nil || *req.Sampling.MaxTokens != 64 {
			t.Errorf("request %d Sampling.MaxTokens = %v, want 64", i, req.Sampling.MaxTokens)
		}
	}
}

// TestEngineForcedToolMode pins the force-mode engine behavior: the
// request carries the configured ToolChoice; the forced tool must be
// granted at construction; a response calling the forced tool proceeds
// normally; a plain-text response fails the turn naming the tool.
func TestEngineForcedToolMode(t *testing.T) {
	t.Parallel()

	r := toolRegistry(t)

	t.Run("construction rejects a forced tool that is not granted", func(t *testing.T) {
		t.Parallel()

		e := engine.New(engine.EngineConfig{
			Client:     &fakeClient{},
			Tools:      r,
			ToolChoice: &llm.ToolChoice{Mode: llm.ToolChoiceForce, ForceTool: "no_such_tool"},
		})
		_, err := e.RunTurn(context.Background(), "go", func(engine.Event) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "no_such_tool") {
			t.Errorf("error = %v, want a clear error naming the ungranted forced tool", err)
		}
	})

	t.Run("request carries the configured ToolChoice", func(t *testing.T) {
		t.Parallel()

		choice := &llm.ToolChoice{Mode: llm.ToolChoiceForce, ForceTool: "echo"}
		fc := &fakeClient{responses: []llm.Response{
			toolCallResp(call("call_1", "echo", `{"msg":"hello"}`)),
			textResp("done"),
		}}
		e := engine.New(engine.EngineConfig{Client: fc, Tools: r, ToolChoice: choice})

		if _, err := e.RunTurn(context.Background(), "go", func(engine.Event) error { return nil }); err != nil {
			t.Fatalf("RunTurn error = %v, want nil", err)
		}
		if len(fc.requests) != 2 {
			t.Fatalf("API calls = %d, want 2", len(fc.requests))
		}
		for i, req := range fc.requests {
			if req.ToolChoice != choice {
				t.Errorf("request %d ToolChoice = %+v, want the configured choice on every call", i, req.ToolChoice)
			}
		}
	})

	t.Run("force mode proceeds when the model calls the forced tool", func(t *testing.T) {
		t.Parallel()

		fc := &fakeClient{responses: []llm.Response{
			toolCallResp(call("call_1", "echo", `{"msg":"hello"}`)),
			textResp("done"),
		}}
		e := engine.New(engine.EngineConfig{
			Client:     fc,
			Tools:      r,
			ToolChoice: &llm.ToolChoice{Mode: llm.ToolChoiceForce, ForceTool: "echo"},
		})

		final, err := e.RunTurn(context.Background(), "go", func(engine.Event) error { return nil })
		if err != nil {
			t.Fatalf("RunTurn error = %v, want nil", err)
		}
		if final != "done" {
			t.Errorf("final = %q, want %q", final, "done")
		}
	})

	t.Run("force mode fails the turn on a plain-text response", func(t *testing.T) {
		t.Parallel()

		fc := &fakeClient{responses: []llm.Response{textResp("I would rather not")}}
		e := engine.New(engine.EngineConfig{
			Client:     fc,
			Tools:      r,
			ToolChoice: &llm.ToolChoice{Mode: llm.ToolChoiceForce, ForceTool: "echo"},
		})

		_, err := e.RunTurn(context.Background(), "go", func(engine.Event) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "echo") || !strings.Contains(err.Error(), "I would rather not") {
			t.Errorf("error = %v, want an error naming the forced tool and what came back instead", err)
		}
	})
}

func TestRunTurnSingleToolCall(t *testing.T) {
	t.Parallel()

	r := toolRegistry(t)
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
		if ev.Kind == engine.EventUsage {
			continue
		}
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

func TestRunTurnBuiltinToolCall(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "note.txt"), []byte("engine builtin content"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := tools.NewRegistry([]config.ToolEntry{{
		Type:        config.ToolTypeBuiltin,
		Name:        "read",
		Description: "Read a file.",
		Builtin:     "read",
		Config:      json.RawMessage(`{"base_dir":"` + base + `"}`),
	}})
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	fc := &fakeClient{responses: []llm.Response{
		toolCallResp(call("call_1", "read", `{"path":"note.txt"}`)),
		textResp("done"),
	}}
	e := engine.New(engine.EngineConfig{Client: fc, Tools: r})

	var events []engine.Event
	final, err := e.RunTurn(context.Background(), "read the note", func(ev engine.Event) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("RunTurn error = %v, want nil", err)
	}
	if final != "done" {
		t.Errorf("final = %q, want %q", final, "done")
	}

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
	if toolMsg.Content != "engine builtin content" {
		t.Errorf("tool result content = %q, want %q (read through the builtin)", toolMsg.Content, "engine builtin content")
	}

	kinds := []engine.EventKind{}
	for _, ev := range events {
		if ev.Kind == engine.EventUsage {
			continue
		}
		kinds = append(kinds, ev.Kind)
	}
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

	r := toolRegistry(t)
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

func TestRunTurnEmitsThinkingBeforeText(t *testing.T) {
	t.Parallel()

	resp := llm.Response{
		ID: "resp",
		Message: llm.Message{
			Role:      llm.RoleAssistant,
			Content:   "the answer",
			Reasoning: "step by step...",
		},
		FinishReason: llm.FinishStop,
	}
	fc := &fakeClient{responses: []llm.Response{resp}}
	e := engine.New(engine.EngineConfig{Client: fc})

	var events []engine.Event
	final, err := e.RunTurn(context.Background(), "q", func(ev engine.Event) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("RunTurn error = %v, want nil", err)
	}
	if final != "the answer" {
		t.Errorf("final = %q, want %q", final, "the answer")
	}

	want := []struct {
		kind engine.EventKind
		text string
	}{
		{engine.EventUsage, ""},
		{engine.EventAssistantThinking, "step by step..."},
		{engine.EventAssistantText, "the answer"},
	}
	if len(events) != len(want) {
		t.Fatalf("events = %+v, want %d events", events, len(want))
	}
	for i, w := range want {
		if events[i].Kind != w.kind || events[i].Text != w.text {
			t.Errorf("event[%d] = %+v, want kind %v text %q", i, events[i], w.kind, w.text)
		}
	}

	// Reasoning is kept in history for subsequent requests.
	if h := e.History(); h[1].Reasoning != "step by step..." {
		t.Errorf("history reasoning = %q, want the model's thinking", h[1].Reasoning)
	}
}

func TestRunTurnStreamTextDeltas(t *testing.T) {
	t.Parallel()

	fc := &streamingClient{responses: []llm.Response{
		llm.Response{
			ID: "resp",
			Message: llm.Message{
				Role:      llm.RoleAssistant,
				Content:   "the answer",
				Reasoning: "hmm...",
			},
			FinishReason: llm.FinishStop,
		},
	}}
	e := engine.New(engine.EngineConfig{Client: fc, Stream: true})

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

	var text, thinking strings.Builder
	for _, ev := range events {
		switch ev.Kind {
		case engine.EventAssistantTextDelta:
			text.WriteString(ev.Text)
		case engine.EventAssistantThinkingDelta:
			thinking.WriteString(ev.Text)
		case engine.EventUsage:
			// One usage event per completed call arrives after the call's
			// deltas; checked in TestRunTurnEmitsUsageStreamed.
		default:
			t.Errorf("unexpected event kind %v", ev.Kind)
		}
	}
	if text.String() != "the answer" {
		t.Errorf("text deltas concatenate to %q, want %q", text.String(), "the answer")
	}
	if thinking.String() != "hmm..." {
		t.Errorf("thinking deltas concatenate to %q, want %q", thinking.String(), "hmm...")
	}

	// Reasoning fragments arrive before text, in arrival order; the
	// streaming path's usage event follows both deltas.
	nonUsage := []engine.Event{}
	for _, ev := range events {
		if ev.Kind != engine.EventUsage {
			nonUsage = append(nonUsage, ev)
		}
	}
	if len(nonUsage) != 2 || nonUsage[0].Kind != engine.EventAssistantThinkingDelta ||
		nonUsage[1].Kind != engine.EventAssistantTextDelta {
		t.Errorf("events = %+v, want thinking delta then text delta in order", nonUsage)
	}

	// The whole-message events must not be emitted when streaming.
	for _, ev := range events {
		if ev.Kind == engine.EventAssistantText || ev.Kind == engine.EventAssistantThinking {
			t.Errorf("whole-message event %v emitted during streaming", ev.Kind)
		}
	}
}

func TestRunTurnStreamToolCallDeltas(t *testing.T) {
	t.Parallel()

	r := toolRegistry(t)
	fc := &streamingClient{responses: []llm.Response{
		toolCallResp(call("call_1", "echo", `{"msg":"streamed"}`)),
		textResp("done"),
	}}
	e := engine.New(engine.EngineConfig{Client: fc, Tools: r, Stream: true})

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

	var deltaCalls []engine.Event
	for _, ev := range events {
		if ev.Kind == engine.EventToolCallDelta {
			deltaCalls = append(deltaCalls, ev)
		}
	}
	if len(deltaCalls) != 1 {
		t.Fatalf("tool call delta events = %+v, want 1", deltaCalls)
	}
	if deltaCalls[0].Name != "echo" || deltaCalls[0].Args != `{"msg":"streamed"}` {
		t.Errorf("tool call delta = %+v, want echo with %q", deltaCalls[0], `{"msg":"streamed"}`)
	}

	// The turn still drove tool execution and the whole-message flow.
	if len(fc.requests) != 2 {
		t.Fatalf("API calls = %d, want 2", len(fc.requests))
	}
	toolMsg := fc.requests[1].Messages[2]
	if toolMsg.Role != llm.RoleTool || toolMsg.ToolCallID != "call_1" || toolMsg.Content != `{"msg":"streamed"}` {
		t.Errorf("tool result message = %+v, want echo result via streamed call", toolMsg)
	}
	if !hasEvent(events, engine.EventToolCall) || !hasEvent(events, engine.EventToolResult) {
		t.Errorf("events = %+v, want tool call and result events", events)
	}
}

func TestRunTurnStreamMultiToolCallDeltaIndexes(t *testing.T) {
	t.Parallel()

	r := toolRegistry(t)
	fc := &streamingClient{responses: []llm.Response{
		toolCallResp(
			call("call_1", "echo", `{"msg":"one"}`),
			call("call_2", "recorder", `{"msg":"two"}`),
		),
		textResp("done"),
	}}
	e := engine.New(engine.EngineConfig{Client: fc, Tools: r, Stream: true})

	var deltaCalls []engine.Event
	_, err := e.RunTurn(context.Background(), "run it", func(ev engine.Event) error {
		if ev.Kind == engine.EventToolCallDelta {
			deltaCalls = append(deltaCalls, ev)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RunTurn error = %v, want nil", err)
	}

	if len(deltaCalls) != 2 {
		t.Fatalf("tool call delta events = %+v, want 2", deltaCalls)
	}
	want := []struct {
		index int
		name  string
	}{{0, "echo"}, {1, "recorder"}}
	for i, w := range want {
		if deltaCalls[i].Index != w.index || deltaCalls[i].Name != w.name {
			t.Errorf("delta %d = index %d name %q, want index %d name %q",
				i, deltaCalls[i].Index, deltaCalls[i].Name, w.index, w.name)
		}
	}
}

func TestRunTurnStreamInterleavingAcrossRounds(t *testing.T) {
	t.Parallel()

	r := toolRegistry(t)
	fc := &streamingClient{responses: []llm.Response{
		llm.Response{
			ID: "resp",
			Message: llm.Message{
				Role:    llm.RoleAssistant,
				Content: "checking",
				ToolCalls: []llm.ToolCall{
					call("call_1", "echo", "{}"),
				},
			},
			FinishReason: llm.FinishToolCalls,
		},
		textResp("final"),
	}}
	e := engine.New(engine.EngineConfig{Client: fc, Tools: r, Stream: true})

	var textEvents []string
	_, err := e.RunTurn(context.Background(), "go", func(ev engine.Event) error {
		if ev.Kind == engine.EventAssistantTextDelta {
			textEvents = append(textEvents, ev.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RunTurn error = %v, want nil", err)
	}
	if len(textEvents) != 2 || textEvents[0] != "checking" || textEvents[1] != "final" {
		t.Errorf("text delta events = %v, want [checking final]", textEvents)
	}
}

func TestRunTurnStreamOffUsesWholeMessage(t *testing.T) {
	t.Parallel()

	fc := &streamingClient{responses: []llm.Response{textResp("whole message")}}
	e := engine.New(engine.EngineConfig{Client: fc, Stream: false})

	var events []engine.Event
	final, err := e.RunTurn(context.Background(), "hi", func(ev engine.Event) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("RunTurn error = %v, want nil", err)
	}
	if final != "whole message" {
		t.Errorf("final = %q, want %q", final, "whole message")
	}
	if len(events) != 2 || events[0].Kind != engine.EventUsage || events[1].Kind != engine.EventAssistantText || events[1].Text != "whole message" {
		t.Errorf("events = %+v, want a usage event then one whole-message assistant text event", events)
	}
}

func TestRunTurnStreamOnDeltaErrorAborts(t *testing.T) {
	t.Parallel()

	boom := errors.New("stop streaming")
	fc := &streamingClient{responses: []llm.Response{textResp("never shown")}}
	e := engine.New(engine.EngineConfig{Client: fc, Stream: true})

	_, err := e.RunTurn(context.Background(), "hi", func(ev engine.Event) error {
		if ev.Kind == engine.EventAssistantTextDelta {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want the onDelta error", err)
	}
}

func hasEvent(events []engine.Event, kind engine.EventKind) bool {
	for _, ev := range events {
		if ev.Kind == kind {
			return true
		}
	}
	return false
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

	r := toolRegistry(t)
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

	r := toolRegistry(t)
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
	if final != "let me check\nfinal" {
		t.Errorf("final = %q, want accumulated text %q", final, "let me check\nfinal")
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

func TestRunTurnLengthTruncatedResponseFails(t *testing.T) {
	t.Parallel()

	truncated := llm.Response{
		ID: "resp",
		Message: llm.Message{
			Role:    llm.RoleAssistant,
			Content: "this answer was cut o",
		},
		FinishReason: llm.FinishLength,
	}
	fc := &fakeClient{responses: []llm.Response{truncated}}
	e := engine.New(engine.EngineConfig{Client: fc})

	var events []engine.Event
	_, err := e.RunTurn(context.Background(), "explain", func(ev engine.Event) error {
		events = append(events, ev)
		return nil
	})
	if err == nil {
		t.Fatal("RunTurn succeeded, want a truncation error")
	}
	if !strings.Contains(err.Error(), "truncated") || !strings.Contains(err.Error(), "length") {
		t.Errorf("error = %q, want a truncation mention with finish_reason", err)
	}

	// The partial text must still have been emitted before failing.
	var texts []string
	for _, ev := range events {
		if ev.Kind == engine.EventAssistantText {
			texts = append(texts, ev.Text)
		}
	}
	if len(texts) != 1 || texts[0] != "this answer was cut o" {
		t.Errorf("text events = %v, want the partial text", texts)
	}

	// A failed turn rolls back history, so no truncated assistant message
	// lingers; the turn can be retried cleanly.
	h := e.History()
	if len(h) != 0 {
		t.Errorf("history after failed turn = %+v, want empty", h)
	}

	// The next turn must succeed normally.
	fc.responses = []llm.Response{textResp("full answer")}
	final, err := e.RunTurn(context.Background(), "continue", func(engine.Event) error { return nil })
	if err != nil {
		t.Fatalf("RunTurn error = %v, want nil", err)
	}
	if final != "full answer" {
		t.Errorf("final = %q, want %q", final, "full answer")
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

	r := toolRegistry(t)
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

	// The second call failed, so the whole turn is rolled back: no user
	// message, assistant message, or tool results remain in history.
	h := e.History()
	if len(h) != 0 {
		t.Fatalf("history after failed turn = %+v, want empty (turn rolled back)", h)
	}
}

func TestRunTurnFailedTurnDoesNotLeaveOrphanedUserMessage(t *testing.T) {
	t.Parallel()

	fc := &fakeClient{err: errors.New("api down")}
	e := engine.New(engine.EngineConfig{Client: fc})

	if _, err := e.RunTurn(context.Background(), "first turn", func(engine.Event) error { return nil }); err == nil {
		t.Fatal("RunTurn succeeded, want the injected error")
	}

	// The turn failed before any response, so history must be as it was
	// before the turn: no orphaned user message.
	h := e.History()
	if len(h) != 0 {
		t.Fatalf("history after failed turn = %+v, want empty", h)
	}

	// A subsequent turn must work and carry only its own user message.
	fc.err = nil
	fc.responses = []llm.Response{textResp("second answer")}
	final, err := e.RunTurn(context.Background(), "second turn", func(engine.Event) error { return nil })
	if err != nil {
		t.Fatalf("RunTurn error = %v, want nil", err)
	}
	if final != "second answer" {
		t.Errorf("final = %q, want %q", final, "second answer")
	}

	if len(fc.requests) == 0 {
		t.Fatal("no API calls recorded")
	}
	msgs := fc.requests[len(fc.requests)-1].Messages
	if len(msgs) != 1 || msgs[0].Role != llm.RoleUser || msgs[0].Content != "second turn" {
		t.Errorf("last request messages = %+v, want a single user message from the second turn", msgs)
	}
}

func TestRepairUnansweredToolCallsPreservesCallOrder(t *testing.T) {
	t.Parallel()

	fc := &fakeClient{}
	e := engine.New(engine.EngineConfig{Client: fc})

	// Seed history directly: an assistant message with two unanswered tool
	// calls in non-sorted order. The engine has no mutation API for this,
	// so call the repair pass directly (white-box).
	e.SeedHistoryForTest([]llm.Message{
		llm.NewTextMessage(llm.RoleUser, "go"),
		{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{
				call("call_b", "echo", `{"n":1}`),
				call("call_a", "echo", `{"n":2}`),
			},
		},
	})

	e.RepairUnansweredToolCallsForTest()

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
	if toolResults[0].ToolCallID != "call_b" || toolResults[1].ToolCallID != "call_a" {
		t.Errorf("tool result ids = [%s %s], want [call_b call_a] (call order)", toolResults[0].ToolCallID, toolResults[1].ToolCallID)
	}
}

func TestRunTurnEmitsUsageWholeMessage(t *testing.T) {
	t.Parallel()

	wantUsage := llm.Usage{PromptTokens: 120, CompletionTokens: 34, TotalTokens: 154}
	fc := &fakeClient{responses: []llm.Response{usageResp("the answer", wantUsage)}}
	e := engine.New(engine.EngineConfig{
		Client:    fc,
		AgentName: "main",
		Model:     "gpt-test",
	})

	var events []engine.Event
	_, err := e.RunTurn(context.Background(), "hi", func(ev engine.Event) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("RunTurn error = %v, want nil", err)
	}

	var usageEvents []engine.Event
	for _, ev := range events {
		if ev.Kind == engine.EventUsage {
			usageEvents = append(usageEvents, ev)
		}
	}
	if len(usageEvents) != 1 {
		t.Fatalf("usage events = %+v, want exactly one", usageEvents)
	}
	if usageEvents[0].Usage != wantUsage {
		t.Errorf("usage = %+v, want %+v", usageEvents[0].Usage, wantUsage)
	}
	if usageEvents[0].AgentName != "main" {
		t.Errorf("AgentName = %q, want %q", usageEvents[0].AgentName, "main")
	}
	if usageEvents[0].Model != "gpt-test" {
		t.Errorf("Model = %q, want %q", usageEvents[0].Model, "gpt-test")
	}
}

func TestRunTurnEmitsUsageStatsWholeMessage(t *testing.T) {
	t.Parallel()

	wantStats := llm.CallStats{
		Output:  llm.OutputBytes{Content: 10, Reasoning: 3, ToolCalls: 21},
		Elapsed: 1500 * time.Millisecond,
	}
	fc := &fakeClient{responses: []llm.Response{statsResp("the answer", llm.Usage{TotalTokens: 44}, wantStats)}}
	e := engine.New(engine.EngineConfig{Client: fc, AgentName: "main", Model: "gpt-test"})

	var events []engine.Event
	_, err := e.RunTurn(context.Background(), "hi", func(ev engine.Event) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("RunTurn error = %v, want nil", err)
	}

	var usageEvents []engine.Event
	for _, ev := range events {
		if ev.Kind == engine.EventUsage {
			usageEvents = append(usageEvents, ev)
		}
	}
	if len(usageEvents) != 1 {
		t.Fatalf("usage events = %+v, want exactly one", usageEvents)
	}
	if usageEvents[0].Stats != wantStats {
		t.Errorf("Stats = %+v, want %+v", usageEvents[0].Stats, wantStats)
	}
}

func TestRunTurnEmitsUsageStreamed(t *testing.T) {
	t.Parallel()

	wantUsage := llm.Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30}
	wantStats := llm.CallStats{Output: llm.OutputBytes{Content: 7}, Elapsed: 2 * time.Second}
	resp := statsResp("the answer", wantUsage, wantStats)
	fc := &streamingClient{responses: []llm.Response{resp}}
	e := engine.New(engine.EngineConfig{Client: fc, Stream: true})

	var events []engine.Event
	_, err := e.RunTurn(context.Background(), "hi", func(ev engine.Event) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("RunTurn error = %v, want nil", err)
	}

	usageIndexes := []int{}
	for i, ev := range events {
		if ev.Kind == engine.EventUsage {
			usageIndexes = append(usageIndexes, i)
		}
	}
	if len(usageIndexes) != 1 {
		t.Fatalf("usage events = %d, want exactly one", len(usageIndexes))
	}
	idx := usageIndexes[0]
	if events[idx].Usage != wantUsage {
		t.Errorf("usage = %+v, want %+v", events[idx].Usage, wantUsage)
	}
	if events[idx].Stats != wantStats {
		t.Errorf("stats = %+v, want %+v", events[idx].Stats, wantStats)
	}
	// For streamed calls the usage event arrives after the call's deltas,
	// since the provider reports usage in the stream's final chunk.
	if idx == 0 || events[idx-1].Kind != engine.EventAssistantTextDelta {
		t.Errorf("usage event at index %d of %+v, want it after the text delta", idx, events)
	}
}

func TestRunTurnEmitsUsagePerCallInOrder(t *testing.T) {
	t.Parallel()

	r := toolRegistry(t)
	first := usageResp("", llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15})
	first.Message = llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call("call_1", "echo", "{}")}}
	first.FinishReason = llm.FinishToolCalls
	second := usageResp("done", llm.Usage{PromptTokens: 50, CompletionTokens: 7, TotalTokens: 57})
	fc := &fakeClient{responses: []llm.Response{first, second}}
	e := engine.New(engine.EngineConfig{Client: fc, Tools: r})

	var usageEvents []engine.Event
	_, err := e.RunTurn(context.Background(), "run it", func(ev engine.Event) error {
		if ev.Kind == engine.EventUsage {
			usageEvents = append(usageEvents, ev)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RunTurn error = %v, want nil", err)
	}

	if len(usageEvents) != 2 {
		t.Fatalf("usage events = %+v, want 2 in call order", usageEvents)
	}
	if usageEvents[0].Usage != first.Usage || usageEvents[1].Usage != second.Usage {
		t.Errorf("usage events = [%+v %+v], want call order [%+v %+v]",
			usageEvents[0].Usage, usageEvents[1].Usage, first.Usage, second.Usage)
	}
}

func TestRunTurnUsageEventErrorAborts(t *testing.T) {
	t.Parallel()

	boom := errors.New("stop on usage")
	fc := &fakeClient{responses: []llm.Response{usageResp("answer", llm.Usage{TotalTokens: 1})}}
	e := engine.New(engine.EngineConfig{Client: fc})

	_, err := e.RunTurn(context.Background(), "hi", func(ev engine.Event) error {
		if ev.Kind == engine.EventUsage {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want the onEvent error", err)
	}
}

func TestRunTurnFailedCallEmitsNoUsage(t *testing.T) {
	t.Parallel()

	fc := &fakeClient{err: errors.New("api down")}
	e := engine.New(engine.EngineConfig{Client: fc})

	var events []engine.Event
	if _, err := e.RunTurn(context.Background(), "hi", func(ev engine.Event) error {
		events = append(events, ev)
		return nil
	}); err == nil {
		t.Fatal("RunTurn succeeded, want the injected error")
	}
	for _, ev := range events {
		if ev.Kind == engine.EventUsage {
			t.Errorf("usage event emitted on failed call: %+v", ev)
		}
	}
}
