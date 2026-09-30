package band

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/prefactor"
	"github.com/overspecific/blorb/internal/tools"
)

// traceSession coordinates the process-level Prefactor session shared by
// every room: one agent instance per blorb band process, one turn span
// per handled message. The span schema comes from whichever room opens
// the session first; every room's registry carries the same band tools,
// so the schema is identical across rooms.
type traceSession struct {
	tracer *prefactor.Tracer
	once   sync.Once
	err    error
	opened atomic.Bool
}

// newTraceSession wraps a tracer, or returns nil when tracing is off.
func newTraceSession(t *prefactor.Tracer) *traceSession {
	if t == nil {
		return nil
	}
	return &traceSession{tracer: t}
}

// start registers the session on first use, using reg for the tool span
// schema. Later calls return the first result.
func (s *traceSession) start(ctx context.Context, agentName string, reg *tools.Registry) error {
	s.once.Do(func() {
		s.err = s.tracer.StartSession(ctx, prefactor.DefaultAgentSchemaVersion(agentName, reg))
		s.opened.Store(s.err == nil)
	})
	return s.err
}

// clientHolder is an llm.Client delegating to a swappable inner client,
// so a room's engine can install a per-turn tracing wrapper without
// being rebuilt. A room is served by one goroutine, so inner needs no
// lock.
type clientHolder struct {
	inner llm.Client
}

// Chat implements llm.Client by delegation.
func (h *clientHolder) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return h.inner.Chat(ctx, req)
}
