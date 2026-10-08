package voice

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

// fakeVoiceServer is a scripted Voice Agent API server over a plain TCP
// listener: it answers the ws upgrade, replies session.ready to a valid
// session.update, and lets the test push server events and inspect client
// messages.
type fakeVoiceServer struct {
	t    *testing.T
	addr string
	ln   net.Listener

	handshook chan struct{}

	// failReady replies to session.update with session.error instead of
	// session.ready.
	failReady bool
	failCode  string
	failMsg   string

	mu       sync.Mutex
	conn     net.Conn
	authHead string
	seen     []map[string]json.RawMessage
	writeMu  sync.Mutex

	handshakeOnce sync.Once
}

func newFakeVoiceServer(t *testing.T) *fakeVoiceServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &fakeVoiceServer{
		t:         t,
		addr:      ln.Addr().String(),
		ln:        ln,
		handshook: make(chan struct{}),
	}
	go s.serve()
	return s
}

func (s *fakeVoiceServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.serveConn(conn)
	}
}

func (s *fakeVoiceServer) serveConn(conn net.Conn) {
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		conn.Close()
		return
	}
	accept := voiceAcceptKey(req.Header.Get("Sec-WebSocket-Key"))
	fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
	conn.SetDeadline(time.Time{})

	s.mu.Lock()
	s.conn = conn
	s.authHead = req.Header.Get("Authorization")
	s.mu.Unlock()
	s.handshakeOnce.Do(func() { close(s.handshook) })

	defer conn.Close()
	for {
		f, err := voiceSrvReadFrame(conn)
		if err != nil {
			return
		}
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(f.Payload, &msg); err != nil {
			continue
		}
		s.record(msg)
		if voiceType(msg) == typeSessionUpdate {
			if s.failReady {
				s.pushEvent(`{"type":"session.error","code":"` + s.failCode + `","message":"` + s.failMsg + `"}`)
			} else {
				s.pushEvent(`{"type":"session.ready","session_id":"sess-1"}`)
			}
		}
	}
}

func (s *fakeVoiceServer) record(msg map[string]json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, msg)
}

// pushEvent writes one server event frame to the client.
func (s *fakeVoiceServer) pushEvent(payload string) {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = voiceSrvWriteFrame(conn, ws.Frame{FIN: true, Opcode: ws.OpcodeText, Payload: []byte(payload)})
}

// awaitMessage waits until a client message of the given type is seen and
// returns it.
func (s *fakeVoiceServer) awaitMessage(t *testing.T, typ string) map[string]json.RawMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, msg := range s.messages() {
			if voiceType(msg) == typ {
				return msg
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no client message of type %q seen", typ)
	return nil
}

// awaitMessages waits until at least n messages of the given type are seen
// (including the one awaitMessage may already have returned) and returns
// them in order.
func (s *fakeVoiceServer) awaitMessages(t *testing.T, typ string, n int) []map[string]json.RawMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var out []map[string]json.RawMessage
		for _, msg := range s.messages() {
			if voiceType(msg) == typ {
				out = append(out, msg)
			}
		}
		if len(out) >= n {
			return out
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("saw fewer than %d client messages of type %q", n, typ)
	return nil
}

// awaitNoMessage asserts that no client message of the given type arrives
// within a short window.
func (s *fakeVoiceServer) awaitNoMessage(t *testing.T, typ string) {
	t.Helper()
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		for _, msg := range s.messages() {
			if voiceType(msg) == typ {
				t.Fatalf("unexpected client message of type %q", typ)
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *fakeVoiceServer) messages() []map[string]json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]json.RawMessage, len(s.seen))
	copy(out, s.seen)
	return out
}

func (s *fakeVoiceServer) authHeader() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authHead
}

// voiceType decodes the "type" field of a raw client message.
func voiceType(msg map[string]json.RawMessage) string {
	var t string
	_ = json.Unmarshal(msg["type"], &t)
	return t
}

// nested decodes a nested object field of a raw client message.
func nested(t *testing.T, msg map[string]json.RawMessage, field string) map[string]json.RawMessage {
	t.Helper()
	var out map[string]json.RawMessage
	if err := json.Unmarshal(msg[field], &out); err != nil {
		t.Fatalf("decode %q: %v", field, err)
	}
	return out
}

func fieldString(t *testing.T, m map[string]json.RawMessage, field string) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(m[field], &s); err != nil {
		t.Fatalf("decode %q as string: %v", field, err)
	}
	return s
}

// voiceAcceptKey computes the RFC 6455 Sec-WebSocket-Accept value.
func voiceAcceptKey(clientKey string) string {
	h := sha1.New()
	h.Write([]byte(clientKey + testGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// --- minimal unmasked server-side framing (mirrors internal/ws's test
// server) ---

func voiceSrvReadFrame(conn net.Conn) (ws.Frame, error) {
	var head [2]byte
	if _, err := voiceReadFull(conn, head[:]); err != nil {
		return ws.Frame{}, err
	}
	fin := head[0]&0x80 != 0
	op := ws.Opcode(head[0] & 0x0f)
	masked := head[1]&0x80 != 0
	length := int64(head[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := voiceReadFull(conn, ext[:]); err != nil {
			return ws.Frame{}, err
		}
		length = int64(ext[0])<<8 | int64(ext[1])
	case 127:
		var ext [8]byte
		if _, err := voiceReadFull(conn, ext[:]); err != nil {
			return ws.Frame{}, err
		}
		for _, b := range ext {
			length = length<<8 | int64(b)
		}
	}
	var key [4]byte
	if masked {
		if _, err := voiceReadFull(conn, key[:]); err != nil {
			return ws.Frame{}, err
		}
	}
	payload := make([]byte, length)
	if _, err := voiceReadFull(conn, payload); err != nil {
		return ws.Frame{}, err
	}
	for i := range payload {
		payload[i] ^= key[i%4]
	}
	return ws.Frame{FIN: fin, Opcode: op, Payload: payload}, nil
}

func voiceSrvWriteFrame(conn net.Conn, f ws.Frame) error {
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

func voiceReadFull(r net.Conn, buf []byte) (int, error) {
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
