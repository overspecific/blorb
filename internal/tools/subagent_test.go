package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/logging"
	"github.com/overspecific/blorb/internal/tools"
)

// fakeSubagentRunner records calls and returns canned results and events.
type fakeSubagentRunner struct {
	mu sync.Mutex

	// calls records one entry per RunSubagent invocation.
	calls []fakeSubagentCall

	// result is returned for every call.
	result tools.SubagentResult
	// err is returned as the Go error for every call, when non-nil.
	err error
	// events are emitted via onEvent before returning.
	events []tools.SubagentEvent
	// block, when non-nil, is called with the ctx before returning, so
	// tests can hold the run open.
	block func(ctx context.Context)
}

type fakeSubagentCall struct {
	agentName   string
	userMessage string
}

func (f *fakeSubagentRunner) RunSubagent(ctx context.Context, agentName, userMessage string, onEvent func(tools.SubagentEvent) error) (tools.SubagentResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeSubagentCall{agentName: agentName, userMessage: userMessage})
	f.mu.Unlock()

	if f.block != nil {
		f.block(ctx)
	}
	for _, ev := range f.events {
		if onEvent != nil {
			if err := onEvent(ev); err != nil {
				return tools.SubagentResult{}, err
			}
		}
	}
	return f.result, f.err
}

func (f *fakeSubagentRunner) runCalls() []fakeSubagentCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeSubagentCall(nil), f.calls...)
}

func subagentEntry(name, agent string, schema json.RawMessage) config.ToolEntry {
	return config.ToolEntry{
		Type:        config.ToolTypeSubagent,
		Name:        name,
		Description: "Delegates to " + agent + ".",
		Agent:       agent,
		ArgsSchema:  schema,
	}
}

func TestSubagentRegistry(t *testing.T) {
	t.Parallel()

	t.Run("registers a subagent entry", func(t *testing.T) {
		t.Parallel()
		r, err := tools.NewRegistry(
			[]config.ToolEntry{subagentEntry("ask_x", "x", nil)},
			tools.WithSubagentRunner(&fakeSubagentRunner{}),
		)
		if err != nil {
			t.Fatalf("NewRegistry error = %v, want nil", err)
		}
		if got := r.Names(); len(got) != 1 || got[0] != "ask_x" {
			t.Errorf("Names() = %v, want [ask_x]", got)
		}
	})

	t.Run("rejects a missing runner", func(t *testing.T) {
		t.Parallel()
		_, err := tools.NewRegistry([]config.ToolEntry{subagentEntry("ask_x", "x", nil)})
		if err == nil || !strings.Contains(err.Error(), "subagent tools require a subagent runner") {
			t.Errorf("error = %v, want a missing-runner error", err)
		}
	})

	t.Run("rejects a missing agent", func(t *testing.T) {
		t.Parallel()
		e := subagentEntry("ask_x", "", nil)
		_, err := tools.NewRegistry([]config.ToolEntry{e}, tools.WithSubagentRunner(&fakeSubagentRunner{}))
		if err == nil || !strings.Contains(err.Error(), "agent is required") {
			t.Errorf("error = %v, want an agent-required error", err)
		}
	})

	t.Run("rejects a bad agent name", func(t *testing.T) {
		t.Parallel()
		e := subagentEntry("ask_x", "has space", nil)
		_, err := tools.NewRegistry([]config.ToolEntry{e}, tools.WithSubagentRunner(&fakeSubagentRunner{}))
		if err == nil || !strings.Contains(err.Error(), "must match") {
			t.Errorf("error = %v, want a name pattern error", err)
		}
	})
}

func TestSubagentDefinitions(t *testing.T) {
	t.Parallel()

	customSchema := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`)
	r, err := tools.NewRegistry([]config.ToolEntry{
		subagentEntry("default_schema", "x", nil),
		subagentEntry("custom_schema", "x", customSchema),
	}, tools.WithSubagentRunner(&fakeSubagentRunner{}))
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	defs := r.Definitions()
	if len(defs) != 2 {
		t.Fatalf("len(Definitions()) = %d, want 2", len(defs))
	}
	if !strings.Contains(string(defs[0].Parameters), `"prompt"`) {
		t.Errorf("default schema = %s, want the built-in prompt schema", defs[0].Parameters)
	}
	if string(defs[1].Parameters) != string(customSchema) {
		t.Errorf("custom schema = %s, want it verbatim %s", defs[1].Parameters, customSchema)
	}
}

func TestSubagentRunDefaultSchema(t *testing.T) {
	t.Parallel()

	fake := &fakeSubagentRunner{result: tools.SubagentResult{Output: "the answer\n"}}
	r, err := tools.NewRegistry([]config.ToolEntry{subagentEntry("ask_x", "x", nil)},
		tools.WithSubagentRunner(fake))
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	t.Run("extracts the prompt", func(t *testing.T) {
		res, err := r.Run(context.Background(), "ask_x", json.RawMessage(`{"prompt":"what up"}`))
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if res.Err {
			t.Error("res.Err = true, want false")
		}
		if res.Output != "the answer" {
			t.Errorf("res.Output = %q, want the trailing newline trimmed", res.Output)
		}
		calls := fake.runCalls()
		if len(calls) != 1 || calls[0].agentName != "x" || calls[0].userMessage != "what up" {
			t.Errorf("runner calls = %+v, want one call to x with the prompt as the message", calls)
		}
	})

	t.Run("rejects a missing prompt", func(t *testing.T) {
		res, err := r.Run(context.Background(), "ask_x", json.RawMessage(`{"nope":1}`))
		if err != nil {
			t.Fatalf("Run error = %v, want nil (bad args are a tool result)", err)
		}
		if !res.Err {
			t.Error("res.Err = false, want true")
		}
		if !strings.Contains(res.Output, `"prompt"`) {
			t.Errorf("res.Output = %q, want a prompt mention", res.Output)
		}
	})

	t.Run("rejects a whitespace-only prompt", func(t *testing.T) {
		res, err := r.Run(context.Background(), "ask_x", json.RawMessage(`{"prompt":"   "}`))
		if err != nil {
			t.Fatalf("Run error = %v, want nil (bad args are a tool result)", err)
		}
		if !res.Err {
			t.Error("res.Err = false, want true")
		}
	})

	t.Run("rejects non-object args", func(t *testing.T) {
		res, err := r.Run(context.Background(), "ask_x", json.RawMessage(`[1,2]`))
		if err != nil {
			t.Fatalf("Run error = %v, want nil (bad args are a tool result)", err)
		}
		if !res.Err {
			t.Error("res.Err = false, want true")
		}
	})
}

func TestSubagentRunCustomSchemaPassesArgsThrough(t *testing.T) {
	t.Parallel()

	schema := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`)
	fake := &fakeSubagentRunner{result: tools.SubagentResult{Output: "ok"}}
	r, err := tools.NewRegistry([]config.ToolEntry{subagentEntry("ask_x", "x", schema)},
		tools.WithSubagentRunner(fake))
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	args := json.RawMessage(`{"q":"raw JSON"}`)
	res, err := r.Run(context.Background(), "ask_x", args)
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if res.Err || res.Output != "ok" {
		t.Errorf("res = %+v, want clean ok", res)
	}
	calls := fake.runCalls()
	if len(calls) != 1 || calls[0].userMessage != string(args) {
		t.Errorf("runner calls = %+v, want the raw args %s passed through", calls, args)
	}
}

func TestSubagentResultMapping(t *testing.T) {
	t.Parallel()

	t.Run("propagates Err", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSubagentRunner{result: tools.SubagentResult{Output: "gave up", Err: true}}
		r, err := tools.NewRegistry([]config.ToolEntry{subagentEntry("ask_x", "x", nil)},
			tools.WithSubagentRunner(fake))
		if err != nil {
			t.Fatalf("NewRegistry error = %v, want nil", err)
		}
		res, err := r.Run(context.Background(), "ask_x", json.RawMessage(`{"prompt":"go"}`))
		if err != nil {
			t.Fatalf("Run error = %v, want nil (subagent failure is a result)", err)
		}
		if !res.Err || res.Output != "gave up" {
			t.Errorf("res = %+v, want {gave up, Err:true}", res)
		}
	})

	t.Run("propagates a Go error", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSubagentRunner{err: errors.New("infrastructure exploded")}
		r, err := tools.NewRegistry([]config.ToolEntry{subagentEntry("ask_x", "x", nil)},
			tools.WithSubagentRunner(fake))
		if err != nil {
			t.Fatalf("NewRegistry error = %v, want nil", err)
		}
		_, err = r.Run(context.Background(), "ask_x", json.RawMessage(`{"prompt":"go"}`))
		if err == nil || !strings.Contains(err.Error(), "infrastructure exploded") {
			t.Errorf("error = %v, want the runner error", err)
		}
	})
}

func TestSubagentEventForwarding(t *testing.T) {
	t.Parallel()

	fake := &fakeSubagentRunner{
		result: tools.SubagentResult{Output: "done"},
		events: []tools.SubagentEvent{
			{Agent: "deepest", Depth: 0, Kind: tools.SubagentText, Text: "hello"},
			{Agent: "deepest", Depth: 2, Kind: tools.SubagentToolCall, Name: "t"},
		},
	}
	var got []tools.SubagentEvent
	r, err := tools.NewRegistry([]config.ToolEntry{subagentEntry("ask_x", "x", nil)},
		tools.WithSubagentRunner(fake),
		tools.WithSubagentEvents(func(ev tools.SubagentEvent) error {
			got = append(got, ev)
			return nil
		}))
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}
	if _, err := r.Run(context.Background(), "ask_x", json.RawMessage(`{"prompt":"go"}`)); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	for _, ev := range got {
		if ev.Agent != "deepest" {
			t.Errorf("event Agent = %q, want deepest (preserved)", ev.Agent)
		}
	}
	if got[0].Depth != 1 || got[1].Depth != 3 {
		t.Errorf("event Depths = %d and %d, want each bumped by one (1 and 3)", got[0].Depth, got[1].Depth)
	}
}

func TestSubagentNilEventsCallbackTolerated(t *testing.T) {
	t.Parallel()

	fake := &fakeSubagentRunner{
		result: tools.SubagentResult{Output: "done"},
		events: []tools.SubagentEvent{{Agent: "x", Kind: tools.SubagentText, Text: "hi"}},
	}
	r, err := tools.NewRegistry([]config.ToolEntry{subagentEntry("ask_x", "x", nil)},
		tools.WithSubagentRunner(fake))
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}
	res, err := r.Run(context.Background(), "ask_x", json.RawMessage(`{"prompt":"go"}`))
	if err != nil {
		t.Fatalf("Run error = %v, want nil (nil events is a no-op)", err)
	}
	if res.Output != "done" {
		t.Errorf("res.Output = %q, want done", res.Output)
	}
}

func TestSubagentExemptFromPerToolTimeout(t *testing.T) {
	t.Parallel()

	fake := &fakeSubagentRunner{
		result: tools.SubagentResult{Output: "finally"},
		block: func(ctx context.Context) {
			select {
			case <-time.After(300 * time.Millisecond):
			case <-ctx.Done():
			}
		},
	}
	r, err := tools.NewRegistry([]config.ToolEntry{subagentEntry("ask_x", "x", nil)},
		tools.WithSubagentRunner(fake), tools.WithTimeout(100*time.Millisecond))
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	res, err := r.Run(context.Background(), "ask_x", json.RawMessage(`{"prompt":"go"}`))
	if err != nil {
		t.Fatalf("Run error = %v, want nil (subagent tools outlive the per-tool timeout)", err)
	}
	if res.Output != "finally" {
		t.Errorf("res.Output = %q, want finally", res.Output)
	}
}

func TestSubagentLogsRequestAndResult(t *testing.T) {
	t.Parallel()

	fake := &fakeSubagentRunner{result: tools.SubagentResult{Output: "the answer"}}
	sink := logging.NewBuffered(0)
	r, err := tools.NewRegistry([]config.ToolEntry{subagentEntry("ask_x", "x", nil)},
		tools.WithSubagentRunner(fake), tools.WithSink(sink))
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	res, err := r.Run(context.Background(), "ask_x", json.RawMessage(`{"prompt":"go"}`))
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	recs := sink.Records()
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2 (request + result)", len(recs))
	}
	if recs[0].Kind != logging.KindToolRequest || recs[0].URL != "ask_x" {
		t.Errorf("record 0 = %+v, want an ask_x tool request", recs[0])
	}
	if recs[1].Kind != logging.KindToolResult || recs[1].URL != "ask_x" {
		t.Errorf("record 1 = %+v, want an ask_x tool result", recs[1])
	}
	if string(recs[1].Body) != res.Output {
		t.Errorf("result body = %q, want the ToolResult.Output %q", recs[1].Body, res.Output)
	}
}

func TestSubagentLogsFailureResult(t *testing.T) {
	t.Parallel()

	fake := &fakeSubagentRunner{err: errors.New("boom")}
	sink := logging.NewBuffered(0)
	r, err := tools.NewRegistry([]config.ToolEntry{subagentEntry("ask_x", "x", nil)},
		tools.WithSubagentRunner(fake), tools.WithSink(sink))
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	if _, err := r.Run(context.Background(), "ask_x", json.RawMessage(`{"prompt":"go"}`)); err == nil {
		t.Fatal("Run succeeded, want error")
	}

	recs := sink.Records()
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	if !strings.Contains(string(recs[1].Body), "error:") || !strings.Contains(string(recs[1].Body), "boom") {
		t.Errorf("result body = %q, want an error body mentioning boom", recs[1].Body)
	}
}
