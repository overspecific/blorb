package band_test

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/overspecific/blorb/internal/ws"
)

const testGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// fakeBandWSServer is a scripted Phoenix Channels server over a plain TCP
// listener: it answers the ws upgrade, replies ok to joins and
// heartbeats, and pushes whatever envelopes the test enqueues.
type fakeBandWSServer struct {
	t    *testing.T
	addr string
	ln   net.Listener

	handshook chan struct{}

	mu   sync.Mutex
	conn net.Conn

	// script holds raw envelope bytes pushed after the handshake.
	scriptMu sync.Mutex
	script   [][]byte
	// notify fires (non-blocking) whenever script gains an element.
	notify chan struct{}

	seenMu sync.Mutex
	seen   [][]json.RawMessage

	// replyOverride lets a test answer joins itself; when nil the
	// default ok reply applies.
	replyOverride func(env []json.RawMessage) ([]byte, bool)
}

func newFakeBandWSServer(t *testing.T) *fakeBandWSServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &fakeBandWSServer{
		t:         t,
		addr:      ln.Addr().String(),
		ln:        ln,
		handshook: make(chan struct{}),
		notify:    make(chan struct{}, 8),
	}
	go s.serve()
	return s
}

func (s *fakeBandWSServer) serve() {
	conn, err := s.ln.Accept()
	if err != nil {
		return
	}

	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		conn.Close()
		return
	}
	accept := acceptKey(req.Header.Get("Sec-WebSocket-Key"))
	fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
	conn.SetDeadline(time.Time{})

	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()
	close(s.handshook)

	done := make(chan struct{})
	defer func() { close(done); conn.Close() }()

	// A sender goroutine drains the script so pushes flow without
	// waiting for an inbound frame to trigger the reader loop. Writes
	// serialize against the read loop's own replies through writeMu; a
	// single net.Conn tolerates concurrent read+write, only concurrent
	// write+write needs the mutex.
	var writeMu sync.Mutex
	go func() {
		for {
			select {
			case <-s.notify:
			case <-done:
				return
			}
			for {
				raw := s.popScript()
				if raw == nil {
					break
				}
				writeMu.Lock()
				err := srvWriteFrame(conn, ws.Frame{FIN: true, Opcode: ws.OpcodeText, Payload: raw})
				writeMu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()

	for {
		f, err := srvReadFrame(conn)
		if err != nil {
			return
		}
		s.recordSeen(f.Payload)

		var env []json.RawMessage
		if err := json.Unmarshal(f.Payload, &env); err != nil || len(env) < 5 {
			return
		}
		event := bandUnquote(env[3])

		if s.replyOverride != nil {
			if raw, handled := s.replyOverride(env); handled {
				if raw != nil {
					if err := srvWriteFrame(conn, ws.Frame{FIN: true, Opcode: ws.OpcodeText, Payload: raw}); err != nil {
						return
					}
				}
				continue
			}
		}

		switch event {
		case "phx_join", "phx_leave", "heartbeat":
			writeMu.Lock()
			err := srvWriteFrame(conn, ws.Frame{FIN: true, Opcode: ws.OpcodeText,
				Payload: phxReplyRaw(string(env[1]), `{"status":"ok","response":{}}`)})
			writeMu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func (s *fakeBandWSServer) popScript() []byte {
	s.scriptMu.Lock()
	defer s.scriptMu.Unlock()
	if len(s.script) == 0 {
		return nil
	}
	raw := s.script[0]
	s.script = s.script[1:]
	return raw
}

// waitHandshake blocks until the server completed its upgrade handshake.
func (s *fakeBandWSServer) waitHandshake(t *testing.T) {
	t.Helper()
	select {
	case <-s.handshook:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the test server handshake")
	}
}

// push enqueues one raw envelope for delivery to the client.
func (s *fakeBandWSServer) push(raw string) {
	s.scriptMu.Lock()
	s.script = append(s.script, []byte(raw))
	s.scriptMu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// awaitSeen waits until the server has read at least n envelopes and
// returns them.
func (s *fakeBandWSServer) awaitSeen(t *testing.T, n int) [][]json.RawMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got := s.seenEnvelopes()
		if len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server saw %d envelopes, want at least %d", len(s.seenEnvelopes()), n)
	return nil
}

func (s *fakeBandWSServer) seenEnvelopes() [][]json.RawMessage {
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	out := make([][]json.RawMessage, len(s.seen))
	copy(out, s.seen)
	return out
}

func (s *fakeBandWSServer) recordSeen(payload []byte) {
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	var env []json.RawMessage
	if err := json.Unmarshal(payload, &env); err == nil {
		s.seen = append(s.seen, env)
	}
}

// kill drops the TCP connection abruptly.
func (s *fakeBandWSServer) kill() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		s.conn.Close()
	}
}

// phxReplyRaw builds raw reply envelope bytes: null join_ref, the given
// ref, the phoenix topic, phx_reply, and the payload.
func phxReplyRaw(ref, payload string) []byte {
	return []byte(`[null,` + ref + `,"phoenix","phx_reply",` + payload + `]`)
}

// pushEvent builds a server-initiated event envelope (null refs).
func pushEvent(topic, event, payload string) []byte {
	return []byte(`[null,null,"` + topic + `","` + event + `",` + payload + `]`)
}

// bandUnquote decodes a JSON string element.
func bandUnquote(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// acceptKey computes the RFC 6455 Sec-WebSocket-Accept value.
func acceptKey(clientKey string) string {
	h := sha1.New()
	h.Write([]byte(clientKey + testGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// --- minimal unmasked server-side framing (mirrors internal/ws's test
// server; the codec itself is internal to ws) ---

func srvReadFrame(conn net.Conn) (ws.Frame, error) {
	var head [2]byte
	if _, err := ioReadFull(conn, head[:]); err != nil {
		return ws.Frame{}, err
	}
	fin := head[0]&0x80 != 0
	op := ws.Opcode(head[0] & 0x0f)
	masked := head[1]&0x80 != 0
	length := int64(head[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := ioReadFull(conn, ext[:]); err != nil {
			return ws.Frame{}, err
		}
		length = int64(ext[0])<<8 | int64(ext[1])
	case 127:
		var ext [8]byte
		if _, err := ioReadFull(conn, ext[:]); err != nil {
			return ws.Frame{}, err
		}
		for _, b := range ext {
			length = length<<8 | int64(b)
		}
	}
	var key [4]byte
	if masked {
		if _, err := ioReadFull(conn, key[:]); err != nil {
			return ws.Frame{}, err
		}
	}
	payload := make([]byte, length)
	if _, err := ioReadFull(conn, payload); err != nil {
		return ws.Frame{}, err
	}
	for i := range payload {
		payload[i] ^= key[i%4]
	}
	return ws.Frame{FIN: fin, Opcode: op, Payload: payload}, nil
}

func srvWriteFrame(conn net.Conn, f ws.Frame) error {
	length := len(f.Payload)
	var head [10]byte
	head[0] = byte(f.Opcode)
	if f.FIN {
		head[0] |= 0x80
	}
	n := 2
	switch {
	case length <= 125:
		head[1] = byte(length)
	case length <= 0xFFFF:
		head[1] = 126
		head[2] = byte(length >> 8)
		head[3] = byte(length)
		n = 4
	default:
		head[1] = 127
		for i := 0; i < 8; i++ {
			head[2+i] = byte(length >> (8 * (7 - i)))
		}
		n = 10
	}
	if _, err := conn.Write(head[:n]); err != nil {
		return err
	}
	_, err := conn.Write(f.Payload)
	return err
}

func ioReadFull(r net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
