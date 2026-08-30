package chat

import (
	"context"
	"errors"
	"fmt"

	"github.com/overspecific/blorb/internal/engine"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/prefactor"
)

// clientHolder is an llm.Client that delegates to a swappable inner client.
// The engine is constructed once and keeps its client for the session; the
// holder lets each turn install a tracing wrapper (or trace nothing) without
// engine changes.
type clientHolder struct {
	inner llm.Client
}

// Chat implements llm.Client by delegation.
func (h *clientHolder) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return h.inner.Chat(ctx, req)
}

// ChatStream implements llm.StreamingClient when the inner client does, so
// the engine's streaming detection through the holder stays correct.
func (h *clientHolder) ChatStream(ctx context.Context, req llm.Request, onDelta func(llm.Delta) error) (*llm.Response, error) {
	if sc, ok := h.inner.(llm.StreamingClient); ok {
		return sc.ChatStream(ctx, req, onDelta)
	}
	// Unreachable whenever the inner client was streaming-capable; fall
	// back to the whole-message path otherwise.
	return h.inner.Chat(ctx, req)
}

// tracingClient wraps an llm.Client for one turn, recording one blorb:llm
// span around each Chat call. It implements llm.StreamingClient when the
// inner client does; a streamed span covers the whole stream and completes
// with the assembled response (which ChatStream returns in full).
type tracingClient struct {
	inner llm.Client
	turn  *prefactor.Turn
}

// newTracingClient wraps inner for the given turn.
func newTracingClient(inner llm.Client, turn *prefactor.Turn) *tracingClient {
	return &tracingClient{inner: inner, turn: turn}
}

// Chat implements llm.Client, wrapping the call in a blorb:llm span.
func (c *tracingClient) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	span, err := c.turn.LLMCall(ctx, req)
	if err != nil {
		return nil, err
	}
	resp, chatErr := c.inner.Chat(ctx, req)
	if chatErr != nil {
		if failErr := span.Fail(chatErr); failErr != nil {
			if isTerminated(failErr) {
				return nil, failErr
			}
			return nil, fmt.Errorf("%w (additionally, tracing failed: %v)", chatErr, failErr)
		}
		return nil, chatErr
	}
	if err := span.Complete(*resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// ChatStream implements llm.StreamingClient when the inner client does.
// The span wraps the whole stream; the assembled response completes it and
// deltas are passed through untraced.
func (c *tracingClient) ChatStream(ctx context.Context, req llm.Request, onDelta func(llm.Delta) error) (*llm.Response, error) {
	if sc, ok := c.inner.(llm.StreamingClient); !ok {
		return c.Chat(ctx, req)
	} else {
		span, err := c.turn.LLMCall(ctx, req)
		if err != nil {
			return nil, err
		}
		resp, streamErr := sc.ChatStream(ctx, req, onDelta)
		if streamErr != nil {
			if failErr := span.Fail(streamErr); failErr != nil {
				if isTerminated(failErr) {
					return nil, failErr
				}
				return nil, fmt.Errorf("%w (additionally, tracing failed: %v)", streamErr, failErr)
			}
			return nil, streamErr
		}
		if err := span.Complete(*resp); err != nil {
			return nil, err
		}
		return resp, nil
	}
}

// isTerminated reports whether err is the tracer's termination sentinel.
func isTerminated(err error) bool {
	return errors.Is(err, prefactor.ErrTerminated)
}

// terminationReason extracts the platform's reason from a termination
// error for display.
func terminationReason(err error) string {
	var te *prefactor.TerminatedError
	if errors.As(err, &te) {
		return te.Reason
	}
	return ""
}

// unwrapTracingClient removes a tracingClient wrapper from a client chain,
// restoring the inner client. Used to uninstall the per-turn wrapper.
func unwrapTracingClient(c llm.Client) llm.Client {
	if tc, ok := c.(*tracingClient); ok {
		return tc.inner
	}
	return c
}

// traceEvent composes the printing callback with tracing. It maps engine
// events onto the turn's spans:
//
//   - EventToolCall starts an active blorb:tool span (the engine emits it
//     immediately before running the tool, in both streaming and
//     whole-message modes);
//   - EventToolResult completes the pending span with the outcome, paired
//     in order (tools run sequentially within a turn);
//   - EventAssistantText records a completed blorb:assistant_message span
//     (whole-message events only, not deltas).
//
// The printing callback always runs first. A tracer error is returned,
// which the engine propagates as a turn failure.
func traceEvent(turn *prefactor.Turn, print func(engine.Event) error) func(engine.Event) error {
	var pending []*prefactor.ToolSpan

	return func(ev engine.Event) error {
		if print != nil {
			if err := print(ev); err != nil {
				return err
			}
		}
		switch ev.Kind {
		case engine.EventToolCall:
			span, err := turn.ToolCall(context.Background(), ev.Name, ev.Args)
			if err != nil {
				return err
			}
			pending = append(pending, span)
		case engine.EventToolResult:
			if len(pending) == 0 {
				return fmt.Errorf("prefactor: tool result for %q with no pending tool call span", ev.Name)
			}
			span := pending[0]
			pending = pending[1:]
			if err := span.Complete(ev.Output, ev.Failed); err != nil {
				return err
			}
		case engine.EventAssistantText:
			if err := turn.AssistantMessage(context.Background(), ev.Text, ""); err != nil {
				return err
			}
		}
		return nil
	}
}
