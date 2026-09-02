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
	// model is the configured model name, recorded on llm spans when the
	// engine's request does not name one (the provider injects its model
	// at the wire layer, so the engine leaves Request.Model empty).
	model string
}

// NewTracingClient wraps inner for the given turn. model is the configured
// provider model, used when the request's model is empty. The concrete
// *tracingClient type stays unexported; its ChatStream passthrough
// preserves streaming capability through the returned llm.Client.
func NewTracingClient(inner llm.Client, turn *prefactor.Turn, model string) llm.Client {
	return &tracingClient{inner: inner, turn: turn, model: model}
}

// spanRequest returns req with the model filled in for tracing when empty.
func (c *tracingClient) spanRequest(req llm.Request) llm.Request {
	if req.Model == "" && c.model != "" {
		req.Model = c.model
	}
	return req
}

// Chat implements llm.Client, wrapping the call in a blorb:llm span.
func (c *tracingClient) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	span, err := c.turn.LLMCall(ctx, c.spanRequest(req))
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
	sc, ok := c.inner.(llm.StreamingClient)
	if !ok {
		return c.Chat(ctx, req)
	}

	span, err := c.turn.LLMCall(ctx, c.spanRequest(req))
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

// TraceEvent composes the printing callback with tracing. It maps engine
// events onto the turn's spans:
//
//   - EventToolCall starts an active blorb:tool span (the engine emits it
//     immediately before running the tool, in both streaming and
//     whole-message modes);
//   - EventToolResult completes the pending span with the outcome, paired
//     in order (tools run sequentially within a turn). When no span is
//     pending — the engine emits tool results without a preceding tool
//     call for malformed arguments and "no tools are configured" — a
//     single-shot completed span records the outcome instead, so a
//     recoverable model failure does not kill the turn.
//   - EventAssistantText records a completed blorb:assistant_message span
//     (whole-message events only, not deltas).
//
// The printing callback always runs first. A tracer error is returned,
// which the engine propagates as a turn failure.
func TraceEvent(turn *prefactor.Turn, print func(engine.Event) error) func(engine.Event) error {
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
				// Orphan result: no matching tool call event. Record a
				// single-shot span so the failure is still audited, and
				// let the turn proceed as it would without tracing.
				return turn.OrphanToolResult(context.Background(), ev.Name, ev.Output, ev.Failed)
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
		// EventUsage and the delta kinds fall through intentionally: usage
		// is accounted by the display layer, not traced.
		return nil
	}
}
