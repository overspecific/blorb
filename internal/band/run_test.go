package band_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/overspecific/blorb/internal/band"
	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/prefactor"
)

// runnerConfig points the config's provider base_url at the LLM fake so
// the room engines reach it through the real openai client.
func runnerConfig(t *testing.T, llmBaseURL string) (config.Config, config.Agent) {
	t.Helper()
	cfg, agent := roomTestConfig()
	cfg.Providers[0].BaseURL = llmBaseURL
	return cfg, agent
}

// stderrWriter writes each line to the test log.
type stderrWriter struct{ t *testing.T }

func (w stderrWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}

// syncBuffer is a mutex-guarded strings.Builder.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitFor polls cond until it holds or the wait expires.
func waitFor(t *testing.T, wait time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for the condition")
}

func TestRunnerFullFlow(t *testing.T) {
	ws := newRunnerWSFake(t)
	rest := newRunnerRestFake(t, roomListJSON("room-1"), map[string][]string{
		"room-1": drainBodies("queued one", "queued two"),
	})

	llmSrv, llmFactory := newRunnerLLMFake(t)
	cfg, agent := runnerConfig(t, llmSrv.srv.URL)

	opts := band.Options{
		Config:            cfg,
		Agent:             agent,
		Stderr:            io.Discard,
		APIKey:            "k",
		RESTURL:           rest.srv.URL,
		WSURL:             "ws://" + ws.addr,
		NewClient:         llmFactory,
		Getenv:            func(string) string { return "test-key" },
		ReconnectBase:     5 * time.Millisecond,
		ReconnectMax:      50 * time.Millisecond,
		HeartbeatInterval: 30 * time.Millisecond,
	}

	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, err := band.Run(ctx, opts)
		done <- err
	}()

	// The startup drain processes the two queued messages: two replies
	// and four marks (processing and processed per message).
	waitFor(t, 5*time.Second, func() bool {
		return len(rest.sentMessages()) == 2 && len(rest.marks()) == 4
	})
	for _, m := range rest.sentMessages() {
		if m.Content != "the reply" {
			t.Errorf("drained reply = %q, want the tool-path reply", m.Content)
		}
		if len(m.Mentions) != 1 || m.Mentions[0].ID != "u-1" {
			t.Errorf("drained reply mentions = %+v, want u-1", m.Mentions)
		}
	}
	if marks := rest.marks(); marks[0] != "processing room-1" || marks[1] != "processed room-1" {
		t.Errorf("startup marks = %v, want processing then processed per message", marks)
	}

	// A live push flows through the room to a sent reply and the
	// processed mark.
	ws.pushMessageCreated("room-1", mentionFor("room-1", "m-live", "live hello"))
	waitFor(t, 5*time.Second, func() bool {
		return len(rest.sentMessages()) == 3 && len(rest.marks()) == 6
	})

	// room_added mid-run joins the new room; a later push is
	// processed there.
	ws.pushRoomAdded(t, "room-9")
	ws.pushMessageCreated("room-9", mentionFor("room-9", "m-9", "new room hello"))
	waitFor(t, 5*time.Second, func() bool {
		return len(rest.sentMessages()) == 4 && len(rest.marks()) == 8
	})

	// room_removed tears the room down; a later push on it is ignored
	// (its state is gone) without crashing the runner.
	ws.pushRoomRemoved(t, "room-9")
	time.Sleep(50 * time.Millisecond)
	ws.pushMessageCreated("room-9", mentionFor("room-9", "m-9b", "after removal"))
	time.Sleep(50 * time.Millisecond)
	if got := len(rest.sentMessages()); got != 4 {
		t.Errorf("sends after room_removed = %d, want 4 (the removed room is ignored)", got)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run error = %v, want nil on graceful shutdown", err)
	}
}

func TestRunnerTracedTurn(t *testing.T) {
	// One process-level Prefactor session: register and start once, a
	// turn span per handled message, and the instance finished on
	// shutdown.
	ws := newRunnerWSFake(t)
	rest := newRunnerRestFake(t, roomListJSON("room-1"), map[string][]string{
		"room-1": drainBodies("hello"),
	})
	llmSrv, llmFactory := newRunnerLLMFake(t)
	cfg, agent := runnerConfig(t, llmSrv.srv.URL)
	pf := newPFFake(t)

	opts := band.Options{
		Config:            cfg,
		Agent:             agent,
		Stderr:            io.Discard,
		APIKey:            "k",
		RESTURL:           rest.srv.URL,
		WSURL:             "ws://" + ws.addr,
		NewClient:         llmFactory,
		Getenv:            func(string) string { return "test-key" },
		ReconnectBase:     5 * time.Millisecond,
		ReconnectMax:      50 * time.Millisecond,
		HeartbeatInterval: 30 * time.Millisecond,
		Tracer: prefactor.NewTracer(prefactor.TracerConfig{
			Client:    prefactor.New(prefactor.Config{BaseURL: pf.srv.URL, Token: "t"}),
			AgentName: agent.Name,
		}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := band.Run(ctx, opts)
		done <- err
	}()

	waitFor(t, 5*time.Second, func() bool {
		return len(rest.sentMessages()) == 1 && pf.count("/agent_instance/register") == 1
	})

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run error = %v, want nil on graceful shutdown", err)
	}
	if got := pf.count("/agent_instance/register"); got != 1 {
		t.Errorf("register calls = %d, want 1 (one process session)", got)
	}
	if !pf.hasSuffix("/agent_instance/inst-1/start") {
		t.Error("instance start not called")
	}
	if pf.count("/agent_spans") == 0 {
		t.Error("no turn spans created")
	}
	if !pf.hasSuffix("/agent_instance/inst-1/finish") {
		t.Error("instance finish not called on shutdown")
	}
}

func TestRunnerPlatformTerminateStopsCleanly(t *testing.T) {
	// A platform terminate mid-turn stops the process gracefully and
	// does not mark the message failed (the instance is marked
	// server-side).
	ws := newRunnerWSFake(t)
	rest := newRunnerRestFake(t, roomListJSON("room-1"), map[string][]string{
		"room-1": drainBodies("hello"),
	})
	llmSrv, llmFactory := newRunnerLLMFake(t)
	cfg, agent := runnerConfig(t, llmSrv.srv.URL)
	pf := newPFFake(t)
	// Let StartTurn's own two span creates (turn span, user message
	// span) succeed, then terminate on the turn's first LLM span, so the
	// terminate is observed inside the turn.
	pf.setTerminateAfter(2)

	opts := band.Options{
		Config:            cfg,
		Agent:             agent,
		Stderr:            io.Discard,
		APIKey:            "k",
		RESTURL:           rest.srv.URL,
		WSURL:             "ws://" + ws.addr,
		NewClient:         llmFactory,
		Getenv:            func(string) string { return "test-key" },
		ReconnectBase:     5 * time.Millisecond,
		ReconnectMax:      50 * time.Millisecond,
		HeartbeatInterval: 30 * time.Millisecond,
		Tracer: prefactor.NewTracer(prefactor.TracerConfig{
			Client:    prefactor.New(prefactor.Config{BaseURL: pf.srv.URL, Token: "t"}),
			AgentName: agent.Name,
		}),
	}

	done := make(chan error, 1)
	go func() {
		_, err := band.Run(context.Background(), opts)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run error = %v, want nil after a platform terminate", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the terminate to stop the run")
	}

	// The message was claimed but not failed, and the instance was not
	// finished a second time (the platform already ended it).
	for _, m := range rest.marks() {
		if strings.HasPrefix(m, "failed") {
			t.Errorf("marks = %v, want no failed mark after terminate", rest.marks())
		}
	}
	if pf.hasSuffix("/agent_instance/inst-1/finish") {
		t.Error("instance finish called after a platform terminate")
	}
}

func TestRunnerTracedSessionFailureMarksFailed(t *testing.T) {
	// A tracing session that cannot start is a hard dependency failure:
	// the message is marked failed so the platform is told.
	ws := newRunnerWSFake(t)
	rest := newRunnerRestFake(t, roomListJSON("room-1"), map[string][]string{
		"room-1": drainBodies("hello"),
	})
	llmSrv, llmFactory := newRunnerLLMFake(t)
	cfg, agent := runnerConfig(t, llmSrv.srv.URL)
	pf := newPFFake(t)
	pf.setRegisterFail(http.StatusInternalServerError)

	opts := band.Options{
		Config:            cfg,
		Agent:             agent,
		Stderr:            io.Discard,
		APIKey:            "k",
		RESTURL:           rest.srv.URL,
		WSURL:             "ws://" + ws.addr,
		NewClient:         llmFactory,
		Getenv:            func(string) string { return "test-key" },
		ReconnectBase:     5 * time.Millisecond,
		ReconnectMax:      50 * time.Millisecond,
		HeartbeatInterval: 30 * time.Millisecond,
		Tracer: prefactor.NewTracer(prefactor.TracerConfig{
			Client:    prefactor.New(prefactor.Config{BaseURL: pf.srv.URL, Token: "t"}),
			AgentName: agent.Name,
		}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := band.Run(ctx, opts)
		done <- err
	}()

	waitFor(t, 5*time.Second, func() bool {
		for _, m := range rest.marks() {
			if strings.HasPrefix(m, "failed room-1:") && strings.Contains(m, "prefactor:") {
				return true
			}
		}
		return false
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run error = %v, want nil on graceful shutdown", err)
	}
}

func TestRunnerStopsDrainingARepeatedMessage(t *testing.T) {
	// The platform re-serves failed messages from /next. A message this
	// process already handled must stop the drain, not loop on it.
	ws := newRunnerWSFake(t)
	rest := newRunnerRestFake(t, roomListJSON("room-1"), nil)
	rest.repeat = map[string]string{"room-1": drainBodies("boom")[0]}
	cfg, agent := roomTestConfig()

	opts := band.Options{
		Config:            cfg,
		Agent:             agent,
		Stderr:            io.Discard,
		APIKey:            "k",
		RESTURL:           rest.srv.URL,
		WSURL:             "ws://" + ws.addr,
		NewClient:         func(config.Config, config.Agent) (llm.Client, error) { return failingLLMClient{}, nil },
		ReconnectBase:     5 * time.Millisecond,
		ReconnectMax:      50 * time.Millisecond,
		HeartbeatInterval: 30 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := band.Run(ctx, opts)
		done <- err
	}()

	waitFor(t, 5*time.Second, func() bool {
		marks := rest.marks()
		return len(marks) >= 2 && strings.HasPrefix(marks[1], "failed room-1:")
	})

	// Give a runaway drain time to hammer /next, then assert it stopped.
	time.Sleep(200 * time.Millisecond)
	if got := rest.nextCalls(); got > 10 {
		t.Errorf("/messages/next calls = %d, want the drain to stop on a repeated message", got)
	}
	// The repeated id was deduplicated: one processing attempt only.
	proc := 0
	for _, m := range rest.marks() {
		if m == "processing room-1" {
			proc++
		}
	}
	if proc != 1 {
		t.Errorf("processing marks = %d, want 1 across the repeated delivery", proc)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run error = %v, want nil on graceful shutdown", err)
	}
}

func TestRunnerFailedMessageDoesNotReconnect(t *testing.T) {
	// A turn failure marks the message failed and keeps the connection:
	// the runner must not treat one bad message as a socket death.
	ws := newRunnerWSFake(t)
	rest := newRunnerRestFake(t, roomListJSON("room-1"), map[string][]string{
		"room-1": drainBodies("boom"),
	})
	cfg, agent := roomTestConfig()

	opts := band.Options{
		Config:            cfg,
		Agent:             agent,
		Stderr:            io.Discard,
		APIKey:            "k",
		RESTURL:           rest.srv.URL,
		WSURL:             "ws://" + ws.addr,
		NewClient:         func(config.Config, config.Agent) (llm.Client, error) { return failingLLMClient{}, nil },
		ReconnectBase:     5 * time.Millisecond,
		ReconnectMax:      50 * time.Millisecond,
		HeartbeatInterval: 30 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := band.Run(ctx, opts)
		done <- err
	}()

	waitFor(t, 5*time.Second, func() bool {
		marks := rest.marks()
		return len(marks) == 2 && strings.HasPrefix(marks[1], "failed room-1:")
	})

	// Give a reconnect (if it were going to happen) time to show up.
	time.Sleep(150 * time.Millisecond)
	joins := 0
	for _, env := range ws.seenEnvelopes() {
		if bandUnquote(env[3]) == "phx_join" && bandUnquote(env[2]) == "agent_rooms:"+runnerAgentID {
			joins++
		}
	}
	if joins != 1 {
		t.Errorf("agent_rooms joins = %d, want 1 (no reconnect on a failed message)", joins)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run error = %v, want nil on graceful shutdown", err)
	}
}

// failingLLMClient always fails, to drive a turn failure.
type failingLLMClient struct{}

func (failingLLMClient) Chat(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("llm down")
}

func TestRunnerInvalidKeyFailsFast(t *testing.T) {
	ws := newRunnerWSFake(t)
	rest := newRunnerRestFakeUnauthorized(t)
	opts := band.Options{
		APIKey:  "wrong",
		RESTURL: rest.srv.URL,
		WSURL:   "ws://" + ws.addr,
		Stderr:  stderrWriter{t},
	}

	start := time.Now()
	_, err := band.Run(context.Background(), opts)
	if err == nil {
		t.Fatal("Run error = nil with a rejected key, want the clear message")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %v, want it to name the 401", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Run took %v, want a fast startup failure", elapsed)
	}
}

func TestRunnerProfileWithoutIDFailsFast(t *testing.T) {
	ws := newRunnerWSFake(t)
	rest := newRunnerRestFakeNoID(t)
	opts := band.Options{
		APIKey:  "k",
		RESTURL: rest.srv.URL,
		WSURL:   "ws://" + ws.addr,
		Stderr:  stderrWriter{t},
	}

	_, err := band.Run(context.Background(), opts)
	if err == nil {
		t.Fatal("Run error = nil with an id-less profile, want an error")
	}
	if !strings.Contains(err.Error(), "no id") {
		t.Errorf("error = %v, want it to name the missing id", err)
	}
}

func TestRunnerGracefulShutdownOnCtxCancel(t *testing.T) {
	ws := newRunnerWSFake(t)
	rest := newRunnerRestFake(t, roomListJSON("room-1"), nil)

	var stderr syncBuffer
	opts := band.Options{
		APIKey:            "k",
		RESTURL:           rest.srv.URL,
		WSURL:             "ws://" + ws.addr,
		Stderr:            &stderr,
		ReconnectBase:     5 * time.Millisecond,
		ReconnectMax:      50 * time.Millisecond,
		HeartbeatInterval: 30 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := band.Run(ctx, opts)
		done <- err
	}()

	// Let it come up, then cancel: Run returns nil.
	time.Sleep(100 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run error = %v, want nil on ctx cancel", err)
	}
}

func TestRunnerReconnectsAfterSocketDeath(t *testing.T) {
	ws := newRunnerWSFake(t)
	rest := newRunnerRestFake(t, roomListJSON("room-1"), nil)

	var stderr syncBuffer
	opts := band.Options{
		APIKey:            "k",
		RESTURL:           rest.srv.URL,
		WSURL:             "ws://" + ws.addr,
		Stderr:            &stderr,
		ReconnectBase:     5 * time.Millisecond,
		ReconnectMax:      50 * time.Millisecond,
		HeartbeatInterval: 30 * time.Millisecond,
	}

	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, err := band.Run(ctx, opts)
		done <- err
	}()

	// Wait for the first join round, then kill the socket: the runner
	// must dial again and re-join, so the fake sees the agent_rooms
	// join twice.
	ws.awaitSeen(t, 1)
	ws.pushClose()
	waitFor(t, 5*time.Second, func() bool {
		joins := 0
		for _, env := range ws.seenEnvelopes() {
			if bandUnquote(env[3]) == "phx_join" && bandUnquote(env[2]) == "agent_rooms:"+runnerAgentID {
				joins++
			}
		}
		return joins >= 2
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run error = %v after a reconnect cycle, want nil", err)
	}
}
