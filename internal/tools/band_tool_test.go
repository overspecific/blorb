package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/logging"
	"github.com/overspecific/blorb/internal/tools"
)

// fakeBandExecutor records calls and returns canned results.
type fakeBandExecutor struct {
	mu sync.Mutex

	calls []fakeBandCall

	result tools.ToolResult
	err    error
	// sleep holds each call open, for the timeout test.
	block time.Duration
}

type fakeBandCall struct {
	name string
	args json.RawMessage
	sink logging.Sink
}

func (f *fakeBandExecutor) RunBandTool(ctx context.Context, name string, args json.RawMessage, sink logging.Sink) (tools.ToolResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeBandCall{name: name, args: args, sink: sink})
	f.mu.Unlock()

	if f.block > 0 {
		select {
		case <-ctx.Done():
			return tools.ToolResult{}, ctx.Err()
		case <-time.After(f.block):
		}
	}
	return f.result, f.err
}

func (f *fakeBandExecutor) getCalls() []fakeBandCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeBandCall(nil), f.calls...)
}

// bandEntry builds a programmatically-valid band tool entry.
func bandEntry(name, selector string) config.ToolEntry {
	return config.ToolEntry{
		Type:        config.ToolTypeBand,
		Name:        name,
		Description: "Calls the Band platform tool " + selector + ".",
		Band:        selector,
	}
}

func TestBandRegistry(t *testing.T) {
	t.Run("registers a band entry with an executor", func(t *testing.T) {
		r, err := tools.NewRegistry(
			[]config.ToolEntry{bandEntry("band_send_message", "band_send_message")},
			tools.WithBandExecutor(&fakeBandExecutor{}),
		)
		if err != nil {
			t.Fatalf("NewRegistry error = %v, want nil", err)
		}
		if got := r.Names(); len(got) != 1 || got[0] != "band_send_message" {
			t.Errorf("Names() = %v, want [band_send_message]", got)
		}
	})

	t.Run("rejects a missing executor", func(t *testing.T) {
		_, err := tools.NewRegistry([]config.ToolEntry{bandEntry("t1", "band_send_message")})
		if err == nil || !strings.Contains(err.Error(), "band tools require a band executor") {
			t.Errorf("error = %v, want a missing-executor error", err)
		}
	})

	t.Run("rejects a missing band selector", func(t *testing.T) {
		e := bandEntry("t1", "band_send_message")
		e.Band = ""
		_, err := tools.NewRegistry([]config.ToolEntry{e}, tools.WithBandExecutor(&fakeBandExecutor{}))
		if err == nil || !strings.Contains(err.Error(), "band is required") {
			t.Errorf("error = %v, want a band-required error", err)
		}
	})
}

func TestBandRunPath(t *testing.T) {
	t.Run("passes the selector, args, and result through", func(t *testing.T) {
		exec := &fakeBandExecutor{result: tools.ToolResult{Output: `{"sent":true}`}}
		r, err := tools.NewRegistry(
			[]config.ToolEntry{bandEntry("band_send_message", "band_send_message")},
			tools.WithBandExecutor(exec),
		)
		if err != nil {
			t.Fatalf("NewRegistry error = %v, want nil", err)
		}

		sink := logging.NewBuffered(4)
		r2, err := tools.NewRegistry(
			[]config.ToolEntry{bandEntry("band_send_message", "band_send_message")},
			tools.WithBandExecutor(exec),
			tools.WithSink(sink),
		)
		if err != nil {
			t.Fatalf("NewRegistry error = %v, want nil", err)
		}
		_ = r
		res, err := r2.Run(context.Background(), "band_send_message", json.RawMessage(`{"content":"hi","mentions":[{"id":"u"}]}`))
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if res.Output != `{"sent":true}` {
			t.Errorf("Output = %q, want the executor's result", res.Output)
		}

		calls := exec.getCalls()
		if len(calls) != 1 {
			t.Fatalf("executor calls = %d, want 1", len(calls))
		}
		if calls[0].name != "band_send_message" {
			t.Errorf("call name = %q, want band_send_message", calls[0].name)
		}
		if string(calls[0].args) != `{"content":"hi","mentions":[{"id":"u"}]}` {
			t.Errorf("call args = %s, want the raw args verbatim", calls[0].args)
		}
		if len(sink.Records()) == 0 {
			t.Error("no sink records written for the run; want request and result records")
		}
	})

	t.Run("surfaces executor Err", func(t *testing.T) {
		exec := &fakeBandExecutor{result: tools.ToolResult{Output: "the platform rejected the call", Err: true}}
		r, err := tools.NewRegistry(
			[]config.ToolEntry{bandEntry("t1", "band_send_message")},
			tools.WithBandExecutor(exec),
		)
		if err != nil {
			t.Fatalf("NewRegistry error = %v, want nil", err)
		}
		res, err := r.Run(context.Background(), "t1", json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Run error = %v, want nil (Err is a tool result)", err)
		}
		if !res.Err {
			t.Error("Run result Err = false, want true")
		}
	})

	t.Run("surfaces executor error", func(t *testing.T) {
		exec := &fakeBandExecutor{err: context.DeadlineExceeded}
		r, err := tools.NewRegistry(
			[]config.ToolEntry{bandEntry("t1", "band_send_message")},
			tools.WithBandExecutor(exec),
		)
		if err != nil {
			t.Fatalf("NewRegistry error = %v, want nil", err)
		}
		if _, err := r.Run(context.Background(), "t1", json.RawMessage(`{}`)); err == nil {
			t.Error("Run error = nil, want the executor's error")
		}
	})

	t.Run("timeout is enforced", func(t *testing.T) {
		exec := &fakeBandExecutor{block: 2 * time.Second}
		r, err := tools.NewRegistry(
			[]config.ToolEntry{bandEntry("t1", "band_send_message")},
			tools.WithBandExecutor(exec),
			tools.WithTimeout(50*time.Millisecond),
		)
		if err != nil {
			t.Fatalf("NewRegistry error = %v, want nil", err)
		}
		if _, err := r.Run(context.Background(), "t1", json.RawMessage(`{}`)); err == nil {
			t.Error("Run error = nil for a blocking executor, want the timeout error")
		}
	})
}
