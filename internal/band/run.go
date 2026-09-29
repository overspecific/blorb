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
	"time"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/engine"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/logging"
	"github.com/overspecific/blorb/internal/usage"
)

// Default knob values for the long-running frontend.
const (
	DefaultHeartbeatInterval = 30 * time.Second
	DefaultReconnectBase     = 1 * time.Second
	DefaultReconnectMax      = 30 * time.Second
)

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
}

// Runner is one Run's live state: the shared REST client, the current
// socket, and the rooms with their processing goroutines.
type Runner struct {
	opts   Options
	client *Client
	diag   io.Writer

	mu      sync.Mutex
	rooms   map[string]*Room
	cancels map[string]context.CancelFunc

	// reconnectBase doubles to ReconnectMax across failures and resets
	// after a successful join round.
	reconnectBase time.Duration

	// sessionAccount accumulates the session's usage: every engine
	// EventUsage event across rooms, subagents, and judges.
	sessionAccount *usage.Account
}

// Run is the long-running frontend loop: validate the key, connect the
// subscriptions socket, sync the rooms, and dispatch live events until
// ctx is cancelled. A socket death reconnects with exponential backoff
// (re-joining and re-draining everything); a startup error returns
// immediately with it.
func Run(ctx context.Context, opts Options) error {
	opts.applyDefaults()

	// 1. Validate the key up front: a 401 means the agent API key or
	// the agent id is wrong, said plainly.
	client := NewClient(opts.RESTURL, opts.APIKey, opts.Sink)
	profile, err := client.Me(ctx)
	if err != nil {
		var apiErr *APIError
		if asAPIError(err, &apiErr) && apiErr.Status == http.StatusUnauthorized {
			return errors.New("the Band agent API key or agent id was rejected (401); check the key in the environment variable and agent_id in the band config block")
		}
		return fmt.Errorf("validate agent api key: %w", err)
	}

	st := &Runner{
		opts:           opts,
		client:         client,
		diag:           opts.Stderr,
		rooms:          map[string]*Room{},
		cancels:        map[string]context.CancelFunc{},
		reconnectBase:  opts.ReconnectBase,
		sessionAccount: &usage.Account{},
	}
	defer st.closeAllRooms()

	for {
		err := st.connectAndServe(ctx, profile)
		if err == nil {
			return nil // graceful shutdown
		}
		if ctx.Err() != nil {
			return nil
		}
		// 4. Any socket death reconnects with backoff: the platform's
		// last-connection-wins policy evicts the stale connection
		// server-side, so reconnecting is always safe.
		wait := st.nextBackoff()
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
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

// room returns the room state for roomID, or nil.
func (st *Runner) room(roomID string) *Room {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.rooms[roomID]
}

// connectRoom creates the room's state machine and joins its channel.
// An existing state is reused (a reconnect re-joins without rebuilding).
func (st *Runner) connectRoom(ctx context.Context, socket *Socket, roomsTopic, roomID string) error {
	topic := "chat_room:" + roomID
	if err := socket.Join(ctx, topic); err != nil {
		return fmt.Errorf("join %s: %w", topic, err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.rooms[roomID]; ok {
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
		return fmt.Errorf("build room %s: %w", roomID, err)
	}
	st.rooms[roomID] = room
	return nil
}

// dropRoom cancels the room's worker, closes the room, and drops its
// state.
func (st *Runner) dropRoom(roomID string) {
	st.mu.Lock()
	cancel := st.cancels[roomID]
	room := st.rooms[roomID]
	delete(st.cancels, roomID)
	delete(st.rooms, roomID)
	st.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if room != nil {
		room.Close()
	}
}

// closeAllRooms closes every room state and releases their registries.
func (st *Runner) closeAllRooms() {
	st.mu.Lock()
	rooms := st.rooms
	st.rooms = map[string]*Room{}
	st.mu.Unlock()
	for _, room := range rooms {
		room.Close()
	}
}

// drainRoom claims and processes every queued message until the queue
// is empty (204). Messages the platform stuck in processing (a crash
// mid-handle) are re-served here too.
func (st *Runner) drainRoom(ctx context.Context, roomID string) error {
	room := st.room(roomID)
	if room == nil {
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
		if err := room.Handle(ctx, *msg); err != nil {
			return fmt.Errorf("process %s: %w", msg.ID, err)
		}
		if ctx.Err() != nil {
			return nil
		}
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
		room := st.room(roomID)
		if room == nil {
			// Unknown room: not joined (yet), nothing to do.
			return
		}
		var msg ChatMessage
		if err := json.Unmarshal(ev.Payload, &msg); err != nil {
			st.diagf("decode message_created on %s: %v", ev.Topic, err)
			return
		}
		if err := room.Handle(ctx, msg); err != nil && ctx.Err() == nil {
			st.diagf("process %s in %s: %v", msg.ID, roomID, err)
		}
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
