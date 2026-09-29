package band

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/overspecific/blorb/internal/logging"
	"github.com/overspecific/blorb/internal/ws"
)

// Socket is one live Phoenix Channels connection over a WebSocket: it
// joins topics, receives server-initiated events, and heartbeats. One
// goroutine reads; sends serialize on a mutex. After a protocol error,
// dead connection, or phx_close/phx_error the Events channel closes and
// Done carries the terminal error.
type Socket struct {
	conn *ws.Conn

	events chan Event
	done   chan error
	closed chan struct{}
	once   sync.Once

	writeMu sync.Mutex
	nextRef int

	// pending routes phx_reply messages to their roundTrip caller by
	// ref. Guarded by pendingMu.
	pendingMu sync.Mutex
	pending   map[string]chan reply

	// joinRefs remembers the join_ref each joined topic used, so
	// non-join messages on the topic reuse it.
	joinRefsMu sync.Mutex
	joinRefs   map[string]string

	heartbeats time.Duration
	sink       logging.Sink
}

// Event is one server-initiated Phoenix push: a topic with a named event
// and its raw JSON payload.
type Event struct {
	Topic   string
	Event   string
	Payload json.RawMessage
}

// reply is one resolved phx_reply delivered to the waiting roundTrip.
type reply struct {
	payload json.RawMessage
	err     error
}

// SocketOption customizes a Connect. The zero value keeps defaults.
type SocketOption func(*socketSettings)

type socketSettings struct {
	heartbeats time.Duration
	sink       logging.Sink
}

// WithHeartbeatInterval overrides how often the socket heartbeats.
// Settable for tests.
func WithHeartbeatInterval(d time.Duration) SocketOption {
	return func(s *socketSettings) { s.heartbeats = d }
}

// WithSocketSink wires best-effort wire logging: every envelope in both
// directions is written to the sink.
func WithSocketSink(sink logging.Sink) SocketOption {
	return func(s *socketSettings) { s.sink = sink }
}

// Connect dials wsURL (adding the api_key and vsn query parameters the
// Band subscriptions endpoint requires) and wraps the connection in the
// Phoenix Channels protocol. An HTTP-level rejection (a 401 for a bad
// agent API key) surfaces here with a clear message.
func Connect(ctx context.Context, wsURL, apiKey string, opts ...SocketOption) (*Socket, error) {
	settings := socketSettings{heartbeats: heartBeatIntervalDefault, sink: logging.NewNop()}
	for _, opt := range opts {
		opt(&settings)
	}

	u, err := url.Parse(wsURL)
	if err != nil {
		return nil, fmt.Errorf("parse band websocket url: %w", err)
	}
	q := u.Query()
	q.Set("api_key", apiKey)
	q.Set("vsn", "2.0.0")
	u.RawQuery = q.Encode()

	conn, err := ws.Dial(ctx, u.String())
	if err != nil {
		if strings.Contains(err.Error(), "401") {
			return nil, errors.New("the Band agent API key was rejected")
		}
		return nil, fmt.Errorf("dial band websocket: %w", err)
	}

	s := &Socket{
		conn: conn,
		// Buffered so a slow events consumer never blocks replies:
		// a stalled readLoop would deadlock round-trips.
		events:     make(chan Event, 64),
		done:       make(chan error, 1),
		closed:     make(chan struct{}),
		pending:    map[string]chan reply{},
		joinRefs:   map[string]string{},
		heartbeats: settings.heartbeats,
		sink:       settings.sink,
	}
	go s.readLoop()
	go s.heartbeatLoop()
	return s, nil
}

// Join sends phx_join for the topic and waits for the matching reply.
func (s *Socket) Join(ctx context.Context, topic string) error {
	_, err := s.roundTrip(ctx, topic, "phx_join", map[string]any{})
	return err
}

// Leave sends phx_leave for the topic and waits for the matching reply.
func (s *Socket) Leave(ctx context.Context, topic string) error {
	_, err := s.roundTrip(ctx, topic, "phx_leave", map[string]any{})
	return err
}

// Events delivers server-initiated pushes (null join_ref and ref).
// Replies to our own messages and unrelated traffic are dropped. The
// channel closes when the socket dies.
func (s *Socket) Events() <-chan Event {
	return s.events
}

// Done delivers the terminal error after the socket dies, then the
// Events channel closes. Reconnect is the runner's business.
func (s *Socket) Done() <-chan error {
	return s.done
}

// roundTrip sends one ref'd message and waits for its phx_reply.
func (s *Socket) roundTrip(ctx context.Context, topic, event string, payload any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.writeMu.Lock()
	s.nextRef++
	ref := strconv.Itoa(s.nextRef)
	s.writeMu.Unlock()

	s.joinRefsMu.Lock()
	// join_ref is unique per joined channel: a join uses its own ref as
	// the join_ref and remembers it; everything else on a joined topic
	// reuses that join ref. Both must be strings or null per the
	// protocol. An unjoined topic falls back to the message ref.
	joinRef, ok := s.joinRefs[topic]
	if !ok || joinRef == "" {
		joinRef = ref
		if event == "phx_join" {
			s.joinRefs[topic] = ref
		}
	}
	s.joinRefsMu.Unlock()

	wait := make(chan reply, 1)
	s.pendingMu.Lock()
	s.pending[ref] = wait
	s.pendingMu.Unlock()
	defer func() {
		s.pendingMu.Lock()
		delete(s.pending, ref)
		s.pendingMu.Unlock()
	}()

	if err := s.send(joinRef, ref, topic, event, payload); err != nil {
		return nil, fmt.Errorf("send %s to %s: %w", event, topic, err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, errors.New("band socket closed")
	case r := <-wait:
		return r.payload, r.err
	}
}

// send encodes and writes one envelope. Callers hold whatever
// serialization they need; the ws.Conn already serializes writers, so
// this is lock-free here.
func (s *Socket) send(joinRef, ref, topic, event string, payload any) error {
	body, err := json.Marshal([]any{joinRef, ref, topic, event, payload})
	if err != nil {
		return fmt.Errorf("encode %s envelope: %w", event, err)
	}
	s.logWire("band-request", topic, body)
	if err := s.conn.WriteText(context.Background(), body); err != nil {
		s.shutdown(fmt.Errorf("write %s: %w", event, err))
		return err
	}
	return nil
}

// readLoop decodes envelopes until the connection dies, routing replies
// to their callers and server pushes to Events.
func (s *Socket) readLoop() {
	for {
		_, data, err := s.conn.ReadMessage(context.Background())
		if err != nil {
			s.shutdown(fmt.Errorf("read: %w", err))
			return
		}
		s.logWire("band-response", "phoenix", data)

		env, err := decodeEnvelope(data)
		if err != nil {
			s.shutdown(fmt.Errorf("decode envelope: %w", err))
			return
		}

		switch env.event {
		case "phx_reply":
			s.routeReply(env)
		case "phx_close", "phx_error":
			s.shutdown(fmt.Errorf("received %s on %s: %s", env.event, env.topic, env.payload))
			return
		case "heartbeat":
			// Liveness: the inbound frame itself refreshed the read.
		default:
			// Server-initiated pushes carry null refs; anything
			// ref'd that is not a reply to us is dropped.
			if env.joinRef == nil && env.ref == nil {
				select {
				case s.events <- Event{Topic: env.topic, Event: env.event, Payload: env.payload}:
				default:
					// Events is bounded; a consumer this far
					// behind would drop live traffic, but 64
					// pending pushes means it already stopped.
				}
			}
		}
	}
}

// rawEnvelope is one decoded five-element Phoenix array.
type rawEnvelope struct {
	joinRef *string
	ref     *string
	topic   string
	event   string
	payload json.RawMessage
}

// decodeEnvelope parses [join_ref, ref, topic, event, payload].
func decodeEnvelope(data []byte) (rawEnvelope, error) {
	var parts []json.RawMessage
	if err := json.Unmarshal(data, &parts); err != nil {
		return rawEnvelope{}, err
	}
	if len(parts) < 5 {
		return rawEnvelope{}, fmt.Errorf("envelope has %d elements, want 5", len(parts))
	}

	env := rawEnvelope{
		topic:   unquote(parts[2]),
		event:   unquote(parts[3]),
		payload: parts[4],
	}
	if v, ok := nullableString(parts[0]); ok {
		env.joinRef = &v
	}
	if v, ok := nullableString(parts[1]); ok {
		env.ref = &v
	}
	return env, nil
}

// nullableString decodes a JSON value that is either a string or null,
// reporting whether a string was present.
func nullableString(raw json.RawMessage) (string, bool) {
	if string(raw) == "null" {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func unquote(raw json.RawMessage) string {
	s, _ := nullableString(raw)
	return s
}

// routeReply resolves the roundTrip waiting on the envelope's ref.
func (s *Socket) routeReply(env rawEnvelope) {
	if env.ref == nil {
		return
	}
	s.pendingMu.Lock()
	wait, ok := s.pending[*env.ref]
	s.pendingMu.Unlock()
	if !ok {
		return
	}

	var inner struct {
		Status   string          `json:"status"`
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(env.payload, &inner); err != nil {
		wait <- reply{err: fmt.Errorf("decode phx_reply: %w", err)}
		return
	}
	if inner.Status != "ok" {
		wait <- reply{err: fmt.Errorf("rejected: %s", strings.Trim(string(inner.Response), `"`))}
		return
	}
	wait <- reply{payload: inner.Response}
}

// heartbeatLoop sends a heartbeat every interval. A write failure kills
// the socket; the read side staying alive signals liveness.
func (s *Socket) heartbeatLoop() {
	ticker := time.NewTicker(s.heartbeats)
	defer ticker.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-ticker.C:
			s.writeMu.Lock()
			s.nextRef++
			ref := strconv.Itoa(s.nextRef)
			err := s.send("", ref, "phoenix", "heartbeat", map[string]any{})
			s.writeMu.Unlock()
			if err != nil {
				s.shutdown(fmt.Errorf("heartbeat: %w", err))
				return
			}
		}
	}
}

// logWire writes one best-effort record to the sink.
func (s *Socket) logWire(kind, topic string, body []byte) {
	// Best-effort per the logging contract; an IO failure here never
	// fails the interaction being logged.
	_ = s.sink.Write(logging.Record{
		Time:   time.Now(),
		Kind:   logging.Kind(kind),
		Method: "websocket",
		URL:    topic,
		Body:   body,
	})
}

// shutdown records the terminal error once and closes the event channel.
// A nil err means a deliberate close, not a failure.
func (s *Socket) shutdown(err error) {
	s.once.Do(func() {
		if err == nil {
			err = errors.New("band socket closed")
		}
		s.done <- err
		close(s.closed)
		close(s.events)
	})
}

// Close cancels the loops and closes the ws connection. It is safe to
// call more than once.
func (s *Socket) Close() error {
	s.shutdown(nil)
	return s.conn.Close(1000, "")
}
