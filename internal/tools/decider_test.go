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
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/logging"
	"github.com/overspecific/blorb/internal/tools"
)

// fakeDeciderRunner records calls and returns canned results.
type fakeDeciderRunner struct {
	mu sync.Mutex

	// calls records one entry per RunDecider invocation.
	calls []fakeDeciderCall

	// result is returned for every call.
	result tools.DeciderResult
	// err is returned as the Go error for every call, when non-nil.
	err error
	// block, when non-nil, is called with the ctx before returning, so
	// tests can hold the run open.
	block func(ctx context.Context)
}

type fakeDeciderCall struct {
	deciderName string
	state       json.RawMessage
}

func (f *fakeDeciderRunner) RunDecider(ctx context.Context, deciderName string, state json.RawMessage) (tools.DeciderResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeDeciderCall{deciderName: deciderName, state: append(json.RawMessage(nil), state...)})
	f.mu.Unlock()

	if f.block != nil {
		f.block(ctx)
	}
	if err := ctx.Err(); err != nil {
		return tools.DeciderResult{}, err
	}
	return f.result, f.err
}

func (f *fakeDeciderRunner) runCalls() []fakeDeciderCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeDeciderCall(nil), f.calls...)
}

func deciderEntry(name, decider string, schema json.RawMessage) config.ToolEntry {
	return config.ToolEntry{
		Type:        config.ToolTypeDecider,
		Name:        name,
		Description: "Evaluates " + decider + ".",
		Decider:     decider,
		ArgsSchema:  schema,
	}
}

func TestDeciderRegistry(t *testing.T) {
	t.Parallel()

	t.Run("registers a decider entry", func(t *testing.T) {
		t.Parallel()
		r, err := tools.NewRegistry(
			[]config.ToolEntry{deciderEntry("triage", "t", nil)},
			tools.WithDeciderRunner(&fakeDeciderRunner{}),
		)
		if err != nil {
			t.Fatalf("NewRegistry error = %v, want nil", err)
		}
		if got := r.Names(); len(got) != 1 || got[0] != "triage" {
			t.Errorf("Names() = %v, want [triage]", got)
		}
	})

	t.Run("rejects a missing runner", func(t *testing.T) {
		t.Parallel()
		_, err := tools.NewRegistry([]config.ToolEntry{deciderEntry("triage", "t", nil)})
		if err == nil || !strings.Contains(err.Error(), "decider tools require a decider runner") {
			t.Errorf("error = %v, want a missing-runner error", err)
		}
	})

	t.Run("rejects a missing decider", func(t *testing.T) {
		t.Parallel()
		e := deciderEntry("triage", "", nil)
		_, err := tools.NewRegistry([]config.ToolEntry{e}, tools.WithDeciderRunner(&fakeDeciderRunner{}))
		if err == nil || !strings.Contains(err.Error(), "decider is required") {
			t.Errorf("error = %v, want a decider-required error", err)
		}
	})

	t.Run("rejects a bad decider name", func(t *testing.T) {
		t.Parallel()
		e := deciderEntry("triage", "has space", nil)
		_, err := tools.NewRegistry([]config.ToolEntry{e}, tools.WithDeciderRunner(&fakeDeciderRunner{}))
		if err == nil || !strings.Contains(err.Error(), "must match") {
			t.Errorf("error = %v, want a name pattern error", err)
		}
	})
}

func TestDeciderDefinitions(t *testing.T) {
	t.Parallel()

	customSchema := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`)
	r, err := tools.NewRegistry([]config.ToolEntry{
		deciderEntry("default_schema", "t", nil),
		deciderEntry("custom_schema", "t", customSchema),
	}, tools.WithDeciderRunner(&fakeDeciderRunner{}))
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	defs := r.Definitions()
	if len(defs) != 2 {
		t.Fatalf("len(Definitions()) = %d, want 2", len(defs))
	}
	if !strings.Contains(string(defs[0].Parameters), `"state"`) {
		t.Errorf("default schema = %s, want the built-in state schema", defs[0].Parameters)
	}
	if string(defs[1].Parameters) != string(customSchema) {
		t.Errorf("custom schema = %s, want it verbatim %s", defs[1].Parameters, customSchema)
	}
}

func TestDeciderRunDefaultSchema(t *testing.T) {
	t.Parallel()

	fake := &fakeDeciderRunner{result: tools.DeciderResult{Output: `{"priority":"high"}` + "\n"}}
	r, err := tools.NewRegistry([]config.ToolEntry{deciderEntry("triage", "t", nil)},
		tools.WithDeciderRunner(fake))
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	t.Run("extracts the state", func(t *testing.T) {
		res, err := r.Run(context.Background(), "triage", json.RawMessage(`{"state":"the ticket"}`))
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if res.Err {
			t.Error("res.Err = true, want false")
		}
		if res.Output != `{"priority":"high"}` {
			t.Errorf("res.Output = %q, want the trailing newline trimmed", res.Output)
		}
		calls := fake.runCalls()
		if len(calls) != 1 || calls[0].deciderName != "t" {
			t.Fatalf("runner calls = %+v, want one call to t", calls)
		}
		if string(calls[0].state) != `"the ticket"` {
			t.Errorf("state = %s, want the state as a JSON string", calls[0].state)
		}
	})

	t.Run("rejects a missing state", func(t *testing.T) {
		res, err := r.Run(context.Background(), "triage", json.RawMessage(`{"nope":1}`))
		if err != nil {
			t.Fatalf("Run error = %v, want nil (bad args are a tool result)", err)
		}
		if !res.Err {
			t.Error("res.Err = false, want true")
		}
		if !strings.Contains(res.Output, `"state"`) {
			t.Errorf("res.Output = %q, want a state mention", res.Output)
		}
	})

	t.Run("rejects a whitespace-only state", func(t *testing.T) {
		res, err := r.Run(context.Background(), "triage", json.RawMessage(`{"state":"   "}`))
		if err != nil {
			t.Fatalf("Run error = %v, want nil (bad args are a tool result)", err)
		}
		if !res.Err {
			t.Error("res.Err = false, want true")
		}
	})

	t.Run("rejects non-object args", func(t *testing.T) {
		res, err := r.Run(context.Background(), "triage", json.RawMessage(`[1,2]`))
		if err != nil {
			t.Fatalf("Run error = %v, want nil (bad args are a tool result)", err)
		}
		if !res.Err {
			t.Error("res.Err = false, want true")
		}
	})
}

func TestDeciderRunCustomSchemaPassesArgsThrough(t *testing.T) {
	t.Parallel()

	schema := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`)
	fake := &fakeDeciderRunner{result: tools.DeciderResult{Output: "ok"}}
	r, err := tools.NewRegistry([]config.ToolEntry{deciderEntry("triage", "t", schema)},
		tools.WithDeciderRunner(fake))
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	args := json.RawMessage(`{"q":"raw JSON"}`)
	res, err := r.Run(context.Background(), "triage", args)
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if res.Err || res.Output != "ok" {
		t.Errorf("res = %+v, want clean ok", res)
	}
	calls := fake.runCalls()
	if len(calls) != 1 || string(calls[0].state) != string(args) {
		t.Errorf("runner calls = %+v, want the raw args %s passed through", calls, args)
	}
}

func TestDeciderResultMapping(t *testing.T) {
	t.Parallel()

	t.Run("propagates Err", func(t *testing.T) {
		t.Parallel()
		fake := &fakeDeciderRunner{result: tools.DeciderResult{Output: "server rejected", Err: true}}
		r, err := tools.NewRegistry([]config.ToolEntry{deciderEntry("triage", "t", nil)},
			tools.WithDeciderRunner(fake))
		if err != nil {
			t.Fatalf("NewRegistry error = %v, want nil", err)
		}
		res, err := r.Run(context.Background(), "triage", json.RawMessage(`{"state":"go"}`))
		if err != nil {
			t.Fatalf("Run error = %v, want nil (decider failure is a result)", err)
		}
		if !res.Err || res.Output != "server rejected" {
			t.Errorf("res = %+v, want {server rejected, Err:true}", res)
		}
	})

	t.Run("propagates a Go error", func(t *testing.T) {
		t.Parallel()
		fake := &fakeDeciderRunner{err: errors.New("infrastructure exploded")}
		r, err := tools.NewRegistry([]config.ToolEntry{deciderEntry("triage", "t", nil)},
			tools.WithDeciderRunner(fake))
		if err != nil {
			t.Fatalf("NewRegistry error = %v, want nil", err)
		}
		_, err = r.Run(context.Background(), "triage", json.RawMessage(`{"state":"go"}`))
		if err == nil || !strings.Contains(err.Error(), "infrastructure exploded") {
			t.Errorf("error = %v, want the runner error", err)
		}
	})
}

func TestDeciderUsageAndDecisionEvents(t *testing.T) {
	t.Parallel()

	usage := []tools.SubagentUsageRecord{
		{Agent: "triage", Model: "jev-1.13.0", Usage: llm.Usage{PromptTokens: 5, CompletionTokens: 6, TotalTokens: 11},
			Stats: llm.CallStats{Elapsed: time.Second}},
	}
	fake := &fakeDeciderRunner{result: tools.DeciderResult{Output: `{"priority":"high"}`, Usage: usage}}

	var got []tools.SubagentEvent
	r, err := tools.NewRegistry([]config.ToolEntry{deciderEntry("triage_tool", "triage", nil)},
		tools.WithDeciderRunner(fake),
		tools.WithSubagentEvents(func(ev tools.SubagentEvent) error {
			got = append(got, ev)
			return nil
		}))
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	res, err := r.Run(context.Background(), "triage_tool", json.RawMessage(`{"state":"go"}`))
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if len(res.Usage) != 1 || res.Usage[0] != usage[0] {
		t.Errorf("res.Usage = %+v, want %+v copied through", res.Usage, usage)
	}

	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (usage then decision)", len(got))
	}
	if got[0].Kind != tools.SubagentUsage || got[0].Depth != 1 || got[0].Agent != "triage" || got[0].Model != "jev-1.13.0" {
		t.Errorf("event 0 = %+v, want a usage event at Depth 1", got[0])
	}
	if got[1].Kind != tools.SubagentDecision || got[1].Depth != 1 || got[1].Agent != "triage" {
		t.Errorf("event 1 = %+v, want a decision event at Depth 1", got[1])
	}
	if got[1].Output != `{"priority":"high"}` {
		t.Errorf("event 1 Output = %q, want the answers JSON", got[1].Output)
	}
}

func TestDeciderFailedRunEmitsNoDecisionEvent(t *testing.T) {
	t.Parallel()

	fake := &fakeDeciderRunner{result: tools.DeciderResult{Output: "server rejected", Err: true}}
	var got []tools.SubagentEvent
	r, err := tools.NewRegistry([]config.ToolEntry{deciderEntry("triage_tool", "triage", nil)},
		tools.WithDeciderRunner(fake),
		tools.WithSubagentEvents(func(ev tools.SubagentEvent) error {
			got = append(got, ev)
			return nil
		}))
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	if _, err := r.Run(context.Background(), "triage_tool", json.RawMessage(`{"state":"go"}`)); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	for _, ev := range got {
		if ev.Kind == tools.SubagentDecision {
			t.Errorf("got a decision event %+v, want none for a failed run", ev)
		}
	}
}

func TestDeciderNilEventsCallbackTolerated(t *testing.T) {
	t.Parallel()

	fake := &fakeDeciderRunner{result: tools.DeciderResult{
		Output: `{"priority":"high"}`,
		Usage:  []tools.SubagentUsageRecord{{Agent: "triage", Model: "jev"}},
	}}
	r, err := tools.NewRegistry([]config.ToolEntry{deciderEntry("triage_tool", "triage", nil)},
		tools.WithDeciderRunner(fake))
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}
	res, err := r.Run(context.Background(), "triage_tool", json.RawMessage(`{"state":"go"}`))
	if err != nil {
		t.Fatalf("Run error = %v, want nil (nil events is a no-op)", err)
	}
	if res.Output != `{"priority":"high"}` {
		t.Errorf("res.Output = %q, want the answers JSON", res.Output)
	}
}

func TestDeciderEventCallbackErrorPropagates(t *testing.T) {
	t.Parallel()

	fake := &fakeDeciderRunner{result: tools.DeciderResult{Output: `{"a":1}`}}
	r, err := tools.NewRegistry([]config.ToolEntry{deciderEntry("triage_tool", "triage", nil)},
		tools.WithDeciderRunner(fake),
		tools.WithSubagentEvents(func(ev tools.SubagentEvent) error {
			return errors.New("display failed")
		}))
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}
	_, err = r.Run(context.Background(), "triage_tool", json.RawMessage(`{"state":"go"}`))
	if err == nil || !strings.Contains(err.Error(), "display failed") {
		t.Errorf("error = %v, want the callback error", err)
	}
}

func TestDeciderUnderPerToolTimeout(t *testing.T) {
	t.Parallel()

	fake := &fakeDeciderRunner{
		block: func(ctx context.Context) {
			select {
			case <-time.After(300 * time.Millisecond):
			case <-ctx.Done():
			}
		},
	}
	r, err := tools.NewRegistry([]config.ToolEntry{deciderEntry("triage", "t", nil)},
		tools.WithDeciderRunner(fake), tools.WithTimeout(100*time.Millisecond))
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	_, err = r.Run(context.Background(), "triage", json.RawMessage(`{"state":"go"}`))
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want a deadline error", err)
	}
}

func TestDeciderLogsRequestAndResult(t *testing.T) {
	t.Parallel()

	fake := &fakeDeciderRunner{result: tools.DeciderResult{Output: `{"priority":"high"}`}}
	sink := logging.NewBuffered(0)
	r, err := tools.NewRegistry([]config.ToolEntry{deciderEntry("triage", "t", nil)},
		tools.WithDeciderRunner(fake), tools.WithSink(sink))
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	res, err := r.Run(context.Background(), "triage", json.RawMessage(`{"state":"go"}`))
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	recs := sink.Records()
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2 (request + result)", len(recs))
	}
	if recs[0].Kind != logging.KindToolRequest || recs[0].URL != "triage" {
		t.Errorf("record 0 = %+v, want a triage tool request", recs[0])
	}
	if recs[1].Kind != logging.KindToolResult || string(recs[1].Body) != res.Output {
		t.Errorf("record 1 = %+v, want the result body %q", recs[1], res.Output)
	}
}
