package band_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/overspecific/blorb/internal/band"
	"github.com/overspecific/blorb/internal/logging"
)

// connect connects a Socket to the fake and waits for its handshake.
func connect(t *testing.T, srv *fakeBandWSServer, opts ...band.SocketOption) *band.Socket {
	t.Helper()
	s, err := band.Connect(context.Background(), "ws://"+srv.addr+"/socket", "test-key", opts...)
	if err != nil {
		t.Fatalf("Connect error = %v, want nil", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestJoinSendsWellFormedEnvelope(t *testing.T) {
	srv := newFakeBandWSServer(t)
	s := connect(t, srv)

	if err := s.Join(context.Background(), "chat_room:room-1"); err != nil {
		t.Fatalf("Join error = %v, want nil", err)
	}

	envs := srv.awaitSeen(t, 1)
	env := envs[0]
	if got := bandUnquote(env[0]); got == "" {
		t.Errorf("join_ref = %q, want a non-empty string", got)
	}
	if got := bandUnquote(env[1]); got == "" {
		t.Errorf("ref = %q, want a non-empty string", got)
	}
	if got := bandUnquote(env[2]); got != "chat_room:room-1" {
		t.Errorf("topic = %q, want %q", got, "chat_room:room-1")
	}
	if got := bandUnquote(env[3]); got != "phx_join" {
		t.Errorf("event = %q, want %q", got, "phx_join")
	}
	if string(env[4]) != "{}" {
		t.Errorf("payload = %s, want {}", env[4])
	}
}

func TestJoinRejectedSurfacesReason(t *testing.T) {
	srv := newFakeBandWSServer(t)
	s := connect(t, srv)
	srv.waitHandshake(t)

	srv.replyOverride = func(env []json.RawMessage) ([]byte, bool) {
		if bandUnquote(env[3]) == "phx_join" {
			return phxReplyRaw(string(env[1]), `{"status":"error","response":{"reason":"nope"}}`), true
		}
		return nil, false
	}

	err := s.Join(context.Background(), "chat_room:room-1")
	if err == nil {
		t.Fatal("Join error = nil, want the rejection reason")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("Join error = %v, want it to carry the reason", err)
	}
}

func TestHeartbeatsArrive(t *testing.T) {
	srv := newFakeBandWSServer(t)
	connect(t, srv, band.WithHeartbeatInterval(10*time.Millisecond))
	srv.waitHandshake(t)

	srv.awaitSeen(t, 2)

	// Heartbeats go to the phoenix topic with the heartbeat event.
	found := false
	for _, env := range srv.seenEnvelopes() {
		if bandUnquote(env[2]) == "phoenix" && bandUnquote(env[3]) == "heartbeat" {
			found = true
		}
	}
	if !found {
		t.Error("no heartbeat envelope seen; heartbeats are not flowing")
	}
}

func TestServerEventDelivered(t *testing.T) {
	srv := newFakeBandWSServer(t)
	s := connect(t, srv)

	payload := `{"id":"m-1","content":"hi"}`
	srv.push(string(pushEvent("chat_room:room-1", "message_created", payload)))

	select {
	case ev := <-s.Events():
		if ev.Topic != "chat_room:room-1" || ev.Event != "message_created" {
			t.Errorf("Event = (%q, %q), want (chat_room:room-1, message_created)", ev.Topic, ev.Event)
		}
		if string(ev.Payload) != payload {
			t.Errorf("Event payload = %s, want %s", ev.Payload, payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the pushed event")
	}
}

func TestPhxCloseClosesEventsAndSetsDone(t *testing.T) {
	srv := newFakeBandWSServer(t)
	s := connect(t, srv)
	srv.waitHandshake(t)

	srv.push(`[null,null,"chat_room:room-1","phx_close",{}]`)

	select {
	case err := <-s.Done():
		if err == nil {
			t.Error("Done delivered nil, want the reason")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Done")
	}
	if _, ok := <-s.Events(); ok {
		t.Error("Events still receives after phx_close; want the channel closed")
	}
}

func TestDroppedConnectionCloses(t *testing.T) {
	srv := newFakeBandWSServer(t)
	s := connect(t, srv)
	srv.waitHandshake(t)

	srv.kill()

	select {
	case err := <-s.Done():
		if err == nil {
			t.Error("Done delivered nil after a dropped connection, want the read error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Done")
	}
}

func TestSimultaneousJoinAndEvent(t *testing.T) {
	srv := newFakeBandWSServer(t)
	s := connect(t, srv)
	srv.waitHandshake(t)

	// A pushed event queues while the join round-trips; the join must
	// complete, not deadlock against the event push.
	srv.push(string(pushEvent("chat_room:room-1", "message_created", `{"id":"m-1"}`)))
	if err := s.Join(context.Background(), "chat_room:room-1"); err != nil {
		t.Fatalf("Join error = %v, want nil", err)
	}
	select {
	case ev := <-s.Events():
		if ev.Event != "message_created" {
			t.Errorf("Event = %q, want message_created", ev.Event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the pushed event")
	}
}

func TestSinkRecordsBothDirections(t *testing.T) {
	srv := newFakeBandWSServer(t)
	sink := logging.NewBuffered(32)
	s := connect(t, srv, band.WithSocketSink(sink))

	if err := s.Join(context.Background(), "chat_room:room-1"); err != nil {
		t.Fatalf("Join error = %v, want nil", err)
	}
	srv.awaitSeen(t, 1)

	records := sink.Records()
	var out, in int
	for _, r := range records {
		switch r.Kind {
		case logging.Kind("band-request"):
			out++
			if r.Method != "websocket" {
				t.Errorf("outgoing record method = %q, want websocket", r.Method)
			}
			if r.URL != "chat_room:room-1" {
				t.Errorf("outgoing record URL = %q, want the topic", r.URL)
			}
		case logging.Kind("band-response"):
			in++
		}
	}
	if out == 0 || in == 0 {
		t.Errorf("sink records: %d outgoing, %d incoming, want both nonzero", out, in)
	}
}
