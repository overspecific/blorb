package ws_test

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/overspecific/blorb/internal/ws"
)

const testGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// testServer is a minimal in-process WebSocket server used only by tests.
// It answers the opening handshake, then speaks frames both directions.
type testServer struct {
	addr string
	ln   net.Listener

	tlsWrap bool

	// failAccept makes the server answer the handshake with the given
	// HTTP status instead of upgrading.
	failAccept int
	// badAccept makes the server compute a wrong Sec-WebSocket-Accept.
	badAccept bool

	handshook chan struct{}

	mu   sync.Mutex
	conn net.Conn
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	return &testServer{addr: ln.Addr().String(), ln: ln, handshook: make(chan struct{}, 1)}
}

// serve accepts one connection, performs the server side of the opening
// handshake, and leaves the connection live for the test to script. The
// accept completes asynchronously; call sites use the helpers, which wait
// for the handshake.
func (s *testServer) serve(t *testing.T) {
	t.Helper()
	go func() {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		if s.tlsWrap {
			conn = tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{*localhostCert()}})
		}

		br := bufio.NewReader(conn)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		if s.failAccept != 0 {
			fmt.Fprintf(conn, "HTTP/1.1 %d Nope\r\nContent-Length: 4\r\n\r\nnope", s.failAccept)
			return
		}
		accept := acceptKey(req.Header.Get("Sec-WebSocket-Key"))
		if s.badAccept {
			accept = "bogus"
		}
		fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)

		// The test scripts bound the connection deadlines; drop the
		// accept-time guard so slow tests never flake here.
		conn.SetDeadline(time.Time{})

		s.mu.Lock()
		s.conn = conn
		s.mu.Unlock()
		close(s.handshook)

		// Block so the deferred close runs when the test finishes with
		// the connection, not when serve returns.
		<-time.After(30 * time.Second)
	}()
}

func (s *testServer) waitHandshake(t *testing.T) {
	t.Helper()
	select {
	case <-s.handshook:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the test server handshake")
	}
}

func (s *testServer) connection(t *testing.T) net.Conn {
	s.waitHandshake(t)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn
}

// awaitText reads one text frame and returns its payload.
func (s *testServer) awaitText(t *testing.T) []byte {
	t.Helper()
	f := s.readFrame(t)
	if f.Opcode != ws.OpcodeText {
		t.Fatalf("server read opcode 0x%x, want text", f.Opcode)
	}
	return f.Payload
}

// awaitOpcode reads frames until one with the wanted opcode arrives,
// returning it.
func (s *testServer) awaitOpcode(t *testing.T, want ws.Opcode) ws.Frame {
	t.Helper()
	for {
		f := s.readFrame(t)
		if f.Opcode == want {
			return f
		}
	}
}

func (s *testServer) readFrame(t *testing.T) ws.Frame {
	t.Helper()
	f, err := srvReadFrame(s.connection(t))
	if err != nil {
		t.Fatalf("server readFrame: %v", err)
	}
	return f
}

// sendText writes one unmasked text frame to the client.
func (s *testServer) sendText(t *testing.T, payload string) {
	t.Helper()
	if err := srvWriteFrame(s.connection(t), ws.Frame{FIN: true, Opcode: ws.OpcodeText, Payload: []byte(payload)}); err != nil {
		t.Fatalf("server sendText: %v", err)
	}
}

// sendFragmented writes a text message as final=false then a continuation
// with FIN set.
func (s *testServer) sendFragmented(t *testing.T, a, b string) {
	t.Helper()
	if err := srvWriteFrame(s.connection(t), ws.Frame{FIN: false, Opcode: ws.OpcodeText, Payload: []byte(a)}); err != nil {
		t.Fatalf("server sendFragmented 1: %v", err)
	}
	if err := srvWriteFrame(s.connection(t), ws.Frame{FIN: true, Opcode: ws.OpcodeContinuation, Payload: []byte(b)}); err != nil {
		t.Fatalf("server sendFragmented 2: %v", err)
	}
}

// sendPing sends an unmasked ping carrying payload.
func (s *testServer) sendPing(t *testing.T, payload []byte) {
	t.Helper()
	if err := srvWriteFrame(s.connection(t), ws.Frame{FIN: true, Opcode: ws.OpcodePing, Payload: payload}); err != nil {
		t.Fatalf("server sendPing: %v", err)
	}
}

// awaitClose reads frames until a close frame arrives and returns its
// code and reason. It reports errors instead of failing the test so it is
// safe to call from a helper goroutine. It does not mirror the close: the
// client does not wait for the peer's close (see ws.Conn.Close).
func (s *testServer) awaitClose(t *testing.T) (uint16, string, error) {
	t.Helper()
	raw := s.connection(t)
	for {
		f, err := srvReadFrame(raw)
		if err != nil {
			return 0, "", err
		}
		if f.Opcode != ws.OpcodeClose {
			continue
		}
		var code uint16
		var reason string
		if len(f.Payload) >= 2 {
			code = uint16(f.Payload[0])<<8 | uint16(f.Payload[1])
			reason = string(f.Payload[2:])
		}
		return code, reason, nil
	}
}

// kill drops the connection abruptly.
func (s *testServer) kill(t *testing.T) {
	t.Helper()
	s.connection(t).Close()
}

// acceptKey computes the RFC 6455 Sec-WebSocket-Accept value.
func acceptKey(clientKey string) string {
	h := sha1.New()
	h.Write([]byte(clientKey + testGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// --- minimal frame handling for the test server ---

// srvReadFrame decodes one client frame (clients always mask).
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

// srvWriteFrame encodes one unmasked server frame.
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

// --- TLS test certificate ---

var localhostCertValue = func() *tls.Certificate {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}()

func localhostCert() *tls.Certificate { return localhostCertValue }
