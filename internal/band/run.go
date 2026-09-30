package band

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/engine"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/logging"
	"github.com/overspecific/blorb/internal/prefactor"
	"github.com/overspecific/blorb/internal/usage"
)

// Default knob values for the long-running frontend.
const (
	DefaultHeartbeatInterval = 30 * time.Second
	DefaultReconnectBase     = 1 * time.Second
	DefaultReconnectMax      = 30 * time.Second
)

// roomMsgBuffer bounds how many queued messages one room worker holds
// before an enqueue blocks.
const roomMsgBuffer = 64

// shutdownGrace bounds how long shutdown waits for in-flight room work
// before closing rooms out from under their workers.
const shutdownGrace = 5 * time.Second

// Options configure the long-running band frontend.
type Options struct {
	Config config.Config
	// Agent is the resolved agent the rooms run.
	Agent config.Agent
	// Stderr receives operational diagnostics. Nil discards them.
	Stderr io.Writer
	// NewClient overrides LLM client construction. Tests only; nil
	// builds the real client from the config.
	NewClient func(cfg config.Config, agent config.Agent) (llm.Client, error)
	// Getenv overrides the environment lookup for api_key_env; nil
	// means os.Getenv. Tests only.
	Getenv func(string) string
	// ConfigPath anchors sink resolution, like run and chat use it.
	ConfigPath string
	// Sink is the resolved sink; nil means no wire logging.
	Sink logging.Sink
	// BandAgentID is the Band agent UUID this process serves (the
	// config band block's agent_id).
	BandAgentID string
	// APIKey is the resolved agent API key (read from the configured
	// env var by the caller).
	APIKey string
	// RESTURL and WSURL are the resolved Band endpoints (the config
	// defaults applied by the caller).
	RESTURL string
	WSURL   string
	// HeartbeatInterval, ReconnectBase, and ReconnectMax override the
	// timing knobs; zero applies the defaults. Settable for tests.
	HeartbeatInterval time.Duration
	ReconnectBase     time.Duration
	ReconnectMax      time.Duration
	// Tracer, when non-nil, records the session to Prefactor: one agent
	// instance for the process, one turn span per handled message.
	// Tracing is a hard dependency like run and chat: a tracing failure
	// fails the turn, and a platform terminate stops the process.
	Tracer *prefactor.Tracer
}

// Runner is one Run's live state: the shared REST client, the current
// socket, and the rooms with their processing goroutines.
type Runner struct {
	opts   Options
	client *Client
	diag   io.Writer

	// rootCtx is Run's context. Each room worker derives a cancellable
	// context from it, so workers outlive individual socket connections.
	rootCtx context.Context

	mu    sync.Mutex
	rooms map[string]*roomState
	wg    sync.WaitGroup

	// reconnectBase doubles to ReconnectMax across failures and resets
	// after a successful join round.
	reconnectBase time.Duration

	// sessionAccount accumulates the session's usage: every engine
	// EventUsage event across rooms, subagents, and judges.
	sessionAccount *usage.Account

	// trace is the Prefactor session shared by every room (nil when
	// tracing is off); terminated reports a platform terminate, and
	// onTerminate cancels the run so the pump stops.
	trace       *traceSession
	terminated  atomic.Bool
	onTerminate func()
}

// roomState is one room's worker: the per-room runtime, its inbound
// message queue, and the cancel that stops the worker.
type roomState struct {
	room   *Room
	msgs   chan ChatMessage
	cancel context.CancelFunc
}

// Run is the long-running frontend loop: validate the key, connect the
// subscriptions socket, sync the rooms, and dispatch live events until
// ctx is cancelled or the Prefactor platform terminates the session. A
// socket death reconnects with exponential backoff (re-joining and
// re-draining everything); a startup error returns immediately with it.
// The returned account carries the session's LLM usage for the caller's
// footer; it is non-nil even on a startup error.
func Run(ctx context.Context, opts Options) (*usage.Account, error) {
	opts.applyDefaults()

	// 1. Validate the key up front: a 401 means the agent API key or
	// the agent id is wrong, said plainly.
	client := NewClient(opts.RESTURL, opts.APIKey, opts.Sink)
	profile, err := client.Me(ctx)
	if err != nil {
		var apiErr *APIError
		if asAPIError(err, &apiErr) && apiErr.Status == http.StatusUnauthorized {
			return &usage.Account{}, errors.New("the Band agent API key or agent id was rejected (401); check the key in the environment variable and agent_id in the band config block")
		}
		return &usage.Account{}, fmt.Errorf("validate agent api key: %w", err)
	}

	// runCtx is ctx plus a platform-terminate cancel: a terminate stops
	// the pump and the loop without looking like a caller cancellation.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	st := &Runner{
		opts:           opts,
		client:         client,
		diag:           opts.Stderr,
		rootCtx:        runCtx,
		rooms:          map[string]*roomState{},
		reconnectBase:  opts.ReconnectBase,
		sessionAccount: &usage.Account{},
		trace:          newTraceSession(opts.Tracer),
	}
	st.onTerminate = func() {
		st.terminated.Store(true)
		cancelRun()
	}
	defer st.shutdownRooms()

	for {
		err := st.connectAndServe(runCtx, profile)
		if err == nil || runCtx.Err() != nil {
			return st.sessionAccount, st.finishSession()
		}
		// 4. Any socket death reconnects with backoff: the platform's
		// last-connection-wins policy evicts the stale connection
		// server-side, so reconnecting is always safe.
		wait := st.nextBackoff()
		select {
		case <-runCtx.Done():
			return st.sessionAccount, st.finishSession()
		case <-time.After(wait):
		}
	}
}

// finishSession closes the Prefactor session when tracing is on and the
// platform did not terminate it (a terminate is already recorded
// server-side, and a second finish would be rejected). The finish uses a
// background context: when the run ended by ctx cancellation the run ctx
// is already dead, and a clean exit must still record its terminal state.
func (st *Runner) finishSession() error {
	if st.opts.Tracer == nil || st.terminated.Load() {
		return nil
	}
	if st.trace == nil || !st.trace.opened.Load() {
		return nil
	}
	if err := st.opts.Tracer.FinishSession(context.Background(), prefactor.InstanceComplete); err != nil {
		return fmt.Errorf("finish prefactor session: %w", err)
	}
	return nil
}

// applyDefaults fills the timing knobs' zero values.
func (o *Options) applyDefaults() {
	if o.HeartbeatInterval == 0 {
		o.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if o.ReconnectBase == 0 {
		o.ReconnectBase = DefaultReconnectBase
	}
	if o.ReconnectMax == 0 {
		o.ReconnectMax = DefaultReconnectMax
	}
}

// nextBackoff returns the wait for one reconnect attempt: the base
// doubling to the max.
func (st *Runner) nextBackoff() time.Duration {
	wait := st.reconnectBase
	st.reconnectBase *= 2
	if st.reconnectBase > st.opts.ReconnectMax {
		st.reconnectBase = st.opts.ReconnectMax
	}
	return wait
}

// resetBackoff returns the backoff base to its configured start; called
// after a successful join round.
func (st *Runner) resetBackoff() {
	st.reconnectBase = st.opts.ReconnectBase
}

// connectAndServe is one socket lifetime: dial, join everything, sync
// the rooms, and dispatch live events. Returns nil on graceful ctx
// shutdown; a non-nil error means the socket died.
func (st *Runner) connectAndServe(ctx context.Context, profile AgentProfile) error {
	socket, err := Connect(ctx, st.opts.WSURL, st.opts.APIKey,
		WithHeartbeatInterval(st.opts.HeartbeatInterval),
		WithSocketSink(socketSinkOrNop(st.opts.Sink)))
	if err != nil {
		return fmt.Errorf("connect subscriptions socket: %w", err)
	}
	defer socket.Close()

	roomsTopic := "agent_rooms:" + st.opts.BandAgentID
	if err := socket.Join(ctx, roomsTopic); err != nil {
		return fmt.Errorf("join %s: %w", roomsTopic, err)
	}

	// 2. Startup sync: list rooms, create state, join each room's
	// channel, and drain each queue.
	rooms, err := st.client.ListChats(ctx)
	if err != nil {
		return fmt.Errorf("list chats: %w", err)
	}
	for _, room := range rooms {
		if err := st.connectRoom(ctx, socket, roomsTopic, room.ID); err != nil {
			return err
		}
		if err := st.drainRoom(ctx, room.ID); err != nil {
			return fmt.Errorf("drain %s: %w", room.ID, err)
		}
	}

	// A full join round succeeded: reconnect backoff resets.
	st.resetBackoff()

	// 3. Live loop until the socket dies or ctx is cancelled.
	return st.pumpUntilDead(ctx, socket, roomsTopic)
}

// room returns the room runtime for roomID, or nil.
func (st *Runner) room(roomID string) *Room {
	st.mu.Lock()
	defer st.mu.Unlock()
	if s, ok := st.rooms[roomID]; ok {
		return s.room
	}
	return nil
}

// connectRoom creates the room's state machine and worker and joins its
// channel. An existing state is reused (a reconnect re-joins without
// rebuilding or restarting the worker).
func (st *Runner) connectRoom(ctx context.Context, socket *Socket, roomsTopic, roomID string) error {
	topic := "chat_room:" + roomID
	if err := socket.Join(ctx, topic); err != nil {
		return fmt.Errorf("join %s: %w", topic, err)
	}
	st.mu.Lock()
	if _, ok := st.rooms[roomID]; ok {
		st.mu.Unlock()
		return nil
	}
	room, err := NewRoom(RoomOptions{
		Config:      st.opts.Config,
		Agent:       st.opts.Agent,
		Client:      st.client,
		RoomID:      roomID,
		AgentID:     st.opts.BandAgentID,
		Sink:        st.opts.Sink,
		Diagnostics: st.diag,
		NewClient:   st.opts.NewClient,
		OnEvent: func(ev engine.Event) error {
			// Usage accounting flows to the session account; anything
			// else is discarded (the room has no turn printer).
			if ev.Kind == engine.EventUsage {
				st.sessionAccount.Add(usage.Record{Agent: ev.AgentName, Model: ev.Model, Usage: ev.Usage, Stats: ev.Stats})
			}
			return nil
		},
		Getenv: st.opts.Getenv,
	})
	if err != nil {
		st.mu.Unlock()
		return fmt.Errorf("build room %s: %w", roomID, err)
	}
	room.setTracer(st.trace, st.opts.Tracer, st.onTerminate)
	roomCtx, cancel := context.WithCancel(st.rootCtx)
	state := &roomState{room: room, msgs: make(chan ChatMessage, roomMsgBuffer), cancel: cancel}
	st.rooms[roomID] = state
	st.mu.Unlock()

	st.wg.Add(1)
	go st.serveRoom(roomCtx, state)
	return nil
}

// serveRoom processes one room's queued messages until its context is
// cancelled, then closes the room. One worker per room means a slow turn
// in one room never blocks another room or the socket pump.
func (st *Runner) serveRoom(ctx context.Context, state *roomState) {
	defer st.wg.Done()
	defer state.room.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-state.msgs:
			if err := state.room.Handle(ctx, msg); err != nil && ctx.Err() == nil {
				st.diagf("process %s in %s: %v", msg.ID, state.room.roomID, err)
			}
		}
	}
}

// enqueue hands one message to the room's worker, blocking while the
// worker is behind. An unknown room is dropped: its state was torn down.
// The send honors Run's context so shutdown is never blocked.
func (st *Runner) enqueue(roomID string, msg ChatMessage) {
	st.mu.Lock()
	state := st.rooms[roomID]
	st.mu.Unlock()
	if state == nil {
		return
	}
	select {
	case state.msgs <- msg:
	case <-st.rootCtx.Done():
	}
}

// dropRoom stops the room's worker and drops its state. The worker
// closes the room after its in-flight turn finishes.
func (st *Runner) dropRoom(roomID string) {
	st.mu.Lock()
	state := st.rooms[roomID]
	delete(st.rooms, roomID)
	st.mu.Unlock()
	if state != nil {
		state.cancel()
	}
}

// shutdownRooms cancels every room worker and waits (bounded) for their
// in-flight work, then closes any room the workers did not reach.
func (st *Runner) shutdownRooms() {
	st.mu.Lock()
	states := make([]*roomState, 0, len(st.rooms))
	for _, s := range st.rooms {
		states = append(states, s)
	}
	st.rooms = map[string]*roomState{}
	st.mu.Unlock()

	for _, s := range states {
		s.cancel()
	}
	done := make(chan struct{})
	go func() {
		st.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownGrace):
		st.diagf("shutdown: timed out waiting for in-flight room work")
	}
	for _, s := range states {
		s.room.Close()
	}
}

// drainRoom claims every queued message until the queue is empty (204)
// and hands each to the room's worker. Messages the platform stuck in
// processing (a crash mid-handle) are re-served here too.
func (st *Runner) drainRoom(ctx context.Context, roomID string) error {
	if st.room(roomID) == nil {
		return fmt.Errorf("room %s has no state", roomID)
	}
	for {
		msg, err := st.client.NextMessage(ctx, roomID)
		if err != nil {
			return fmt.Errorf("next message: %w", err)
		}
		if msg == nil {
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}
		st.enqueue(roomID, *msg)
	}
}

// pumpUntilDead dispatches socket events until the socket's Done fires
// (returning the terminal error) or ctx ends (returning nil).
func (st *Runner) pumpUntilDead(ctx context.Context, socket *Socket, roomsTopic string) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-socket.Done():
			return err
		case ev, ok := <-socket.Events():
			if !ok {
				// Events closed; Done carries the reason and fires
				// alongside.
				select {
				case err := <-socket.Done():
					return err
				default:
					return errors.New("band socket events channel closed")
				}
			}
			st.dispatch(ctx, socket, roomsTopic, ev)
		}
	}
}

// dispatch routes one server-initiated event to its room or the rooms
// management topic. Unknown events and topics are ignored: the wire
// records the socket sink already wrote are the debug trail.
func (st *Runner) dispatch(ctx context.Context, socket *Socket, roomsTopic string, ev Event) {
	switch {
	case ev.Topic == roomsTopic && ev.Event == "room_added":
		var payload struct {
			Room struct {
				ID string `json:"id"`
			} `json:"room"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil || payload.Room.ID == "" {
			st.diagf("decode room_added on %s (payload %s): %v", roomsTopic, ev.Payload, err)
			return
		}
		if err := st.connectRoom(ctx, socket, roomsTopic, payload.Room.ID); err != nil {
			st.diagf("room_added %s: %v", payload.Room.ID, err)
			return
		}
		if err := st.drainRoom(ctx, payload.Room.ID); err != nil {
			st.diagf("drain %s after room_added: %v", payload.Room.ID, err)
		}

	case ev.Topic == roomsTopic && ev.Event == "room_removed":
		var payload struct {
			Room struct {
				ID string `json:"id"`
			} `json:"room"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil || payload.Room.ID == "" {
			st.diagf("decode room_removed on %s (payload %s): %v", roomsTopic, ev.Payload, err)
			return
		}
		st.dropRoom(payload.Room.ID)

	case ev.Event == "message_created" && strings.HasPrefix(ev.Topic, "chat_room:"):
		roomID := strings.TrimPrefix(ev.Topic, "chat_room:")
		if st.room(roomID) == nil {
			// Unknown room: not joined (yet), nothing to do.
			return
		}
		var msg ChatMessage
		if err := json.Unmarshal(ev.Payload, &msg); err != nil {
			st.diagf("decode message_created on %s: %v", ev.Topic, err)
			return
		}
		st.enqueue(roomID, msg)
	}
}

// diagf writes one best-effort diagnostic line; a write failure is
// ignored by contract.
func (st *Runner) diagf(format string, args ...any) {
	if st.diag == nil {
		return
	}
	fmt.Fprintf(st.diag, "band runner: "+format+"\n", args...)
}

// socketSinkOrNop returns a non-nil sink so withSocketSink always gets
// one (the socket logs unconditionally through it).
func socketSinkOrNop(s logging.Sink) logging.Sink {
	if s == nil {
		return logging.NewNop()
	}
	return s
}
