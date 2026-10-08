package ws

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// keyGUID is the RFC 6455 magic GUID used in the Sec-WebSocket-Accept
// computation.
const keyGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Default dial settings.
const (
	DefaultReadLimit = 1 << 20 // 1 MiB
	DefaultReadWait  = 60 * time.Second
)

// CloseError is returned by ReadMessage after a close frame arrives from
// the peer. Code and Reason carry the close's payload.
type CloseError struct {
	Code   uint16
	Reason string
}

func (e *CloseError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("websocket closed by peer (code %d)", e.Code)
	}
	return fmt.Sprintf("websocket closed by peer (code %d): %s", e.Code, e.Reason)
}

// DialOption customizes a Dial. The zero value keeps defaults.
type DialOption func(*dialSettings)

type dialSettings struct {
	readWait  time.Duration
	readLimit int64
	tlsConfig *tls.Config
	headers   [][2]string
}

// WithReadWait sets how long a single frame read may take before the
// connection is treated as dead. The deadline is refreshed on every
// inbound frame.
func WithReadWait(d time.Duration) DialOption {
	return func(s *dialSettings) { s.readWait = d }
}

// WithReadLimit bounds the payload size of a single inbound frame,
// protecting against a peer announcing a huge length.
func WithReadLimit(n int64) DialOption {
	return func(s *dialSettings) { s.readLimit = n }
}

// WithTLSConfig sets the TLS configuration used for wss:// dials. Tests
// use it to accept self-signed server certificates.
func WithTLSConfig(c *tls.Config) DialOption {
	return func(s *dialSettings) { s.tlsConfig = c }
}

// WithHeader appends one header line to the opening handshake. Headers are
// applied after the fixed handshake headers, in the order the options are
// given. Dial returns an error if key is empty.
func WithHeader(key, value string) DialOption {
	return func(s *dialSettings) { s.headers = append(s.headers, [2]string{key, value}) }
}

// Conn is a live client-side WebSocket connection. One goroutine reads at
// a time; writers serialize among themselves.
type Conn struct {
	raw net.Conn

	// writeMu serializes writers: RFC 6455 interleaves whole frames, not
	// halves of them.
	writeMu sync.Mutex

	readWait  time.Duration
	readLimit int64

	closeOnce sync.Once
	closeErr  error
}

// Dial opens a WebSocket client connection to rawURL, which must use the
// ws or wss scheme. It performs the RFC 6455 opening handshake by hand and
// verifies the server's Sec-WebSocket-Accept before returning.
func Dial(ctx context.Context, rawURL string, opts ...DialOption) (*Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse websocket url: %w", err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, fmt.Errorf("websocket url %q must use ws or wss scheme", rawURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("websocket url %q must include a host", rawURL)
	}

	settings := dialSettings{readWait: DefaultReadWait, readLimit: DefaultReadLimit}
	for _, opt := range opts {
		opt(&settings)
	}
	for _, h := range settings.headers {
		if h[0] == "" {
			return nil, errors.New("websocket handshake header name must not be empty")
		}
	}

	requestURI := u.RequestURI()
	if requestURI == "" {
		requestURI = "/"
	}

	// Key is 16 random bytes, base64 (RFC 6455 section 4.1).
	var keyRaw [16]byte
	if _, err := rand.Read(keyRaw[:]); err != nil {
		return nil, fmt.Errorf("generate websocket key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyRaw[:])

	var req strings.Builder
	req.WriteString("GET " + requestURI + " HTTP/1.1\r\n")
	req.WriteString("Host: " + u.Host + "\r\n")
	req.WriteString("Upgrade: websocket\r\n")
	req.WriteString("Connection: Upgrade\r\n")
	req.WriteString("Sec-WebSocket-Key: " + key + "\r\n")
	req.WriteString("Sec-WebSocket-Version: 13\r\n")
	for _, h := range settings.headers {
		req.WriteString(h[0] + ": " + h[1] + "\r\n")
	}
	req.WriteString("\r\n")

	hostPort := u.Host
	if !strings.Contains(hostPort, ":") {
		hostPort += ":80"
		if u.Scheme == "wss" {
			hostPort = u.Host + ":443"
		}
	}

	var raw net.Conn
	if u.Scheme == "wss" {
		dialer := &net.Dialer{}
		raw, err = dialer.DialContext(ctx, "tcp", hostPort)
		if err != nil {
			return nil, fmt.Errorf("dial wss %s: %w", hostPort, err)
		}
		tlsConfig := settings.tlsConfig
		if tlsConfig == nil {
			tlsConfig = &tls.Config{}
		}
		if tlsConfig.ServerName == "" {
			tlsConfig = tlsConfig.Clone()
			tlsConfig.ServerName = u.Hostname()
		}
		tlsConn := tls.Client(raw, tlsConfig)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, fmt.Errorf("tls handshake with %s: %w", hostPort, err)
		}
		raw = tlsConn
	} else {
		dialer := &net.Dialer{}
		raw, err = dialer.DialContext(ctx, "tcp", hostPort)
		if err != nil {
			return nil, fmt.Errorf("dial ws %s: %w", hostPort, err)
		}
	}

	// The handshake honors ctx cancellation through the connection
	// deadline, and through a watcher that interrupts a blocked read
	// when ctx is cancelled without a deadline.
	if deadline, ok := ctx.Deadline(); ok {
		raw.SetDeadline(deadline)
	}
	stopWatch := context.AfterFunc(ctx, func() {
		_ = raw.SetDeadline(time.Now())
	})
	defer stopWatch()
	if _, err := raw.Write([]byte(req.String())); err != nil {
		raw.Close()
		return nil, fmt.Errorf("write websocket handshake: %w", err)
	}

	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("read websocket handshake response: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		detail := readSnippet(br)
		if challenge := resp.Header.Get("Sec-WebSocket-Error"); challenge != "" {
			detail = challenge
		}
		raw.Close()
		return nil, fmt.Errorf("websocket handshake rejected: status %d%s", resp.StatusCode, detailSuffix(detail))
	}

	wantAccept := secAccept(key)
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != wantAccept {
		raw.Close()
		return nil, fmt.Errorf("websocket handshake: bad Sec-WebSocket-Accept %q, want %q", got, wantAccept)
	}

	raw.SetDeadline(time.Time{})
	return &Conn{raw: raw, readWait: settings.readWait, readLimit: settings.readLimit}, nil
}

// readSnippet reads a bounded amount of what remains after the response
// headers, for error messages.
func readSnippet(br *bufio.Reader) string {
	snippet := make([]byte, 256)
	n, _ := br.Read(snippet)
	return strings.TrimSpace(string(snippet[:n]))
}

func detailSuffix(detail string) string {
	if strings.TrimSpace(detail) == "" {
		return ""
	}
	return ": " + strings.TrimSpace(detail)
}

// secAccept computes the expected Sec-WebSocket-Accept value for a client
// key (RFC 6455 section 1.3).
func secAccept(clientKey string) string {
	h := sha1.New()
	h.Write([]byte(clientKey + keyGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// ReadMessage reads the next complete data message, reassembling
// fragments, answering pings, and translating close frames into a
// *CloseError (after which the connection is dead for reads). The read
// deadline refreshes per frame: ctx may also carry a deadline, and
// whichever expires sooner ends the read.
func (c *Conn) ReadMessage(ctx context.Context) (Opcode, []byte, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	if err := setReadDeadline(ctx, c.raw, c.readWait); err != nil {
		return 0, nil, err
	}

	var (
		opcode   Opcode
		payload  []byte
		fragment bool
	)
	for {
		// The deadline refreshes on every frame: a peer trickling
		// pings keeps the connection alive, silence kills it.
		if err := setReadDeadline(ctx, c.raw, c.readWait); err != nil {
			return 0, nil, err
		}
		f, err := readFrame(c.raw, c.readLimit)
		if err != nil {
			return 0, nil, err
		}

		switch f.Opcode {
		case OpcodePing:
			if err := c.writeControl(OpcodePong, f.Payload); err != nil {
				return 0, nil, fmt.Errorf("reply to ping: %w", err)
			}
		case OpcodePong:
			// A reply to our own ping; nothing to do.
		case OpcodeClose:
			code, reason := parseClosePayload(f.Payload)
			c.raw.Close()
			return 0, nil, &CloseError{Code: code, Reason: reason}
		case OpcodeText, OpcodeBinary:
			if !fragment {
				if f.FIN {
					return f.Opcode, f.Payload, nil
				}
				opcode = f.Opcode
				payload = append(payload, f.Payload...)
				fragment = true
				continue
			}
			// A data frame mid-message that is not a continuation is a
			// protocol violation.
			return 0, nil, fmt.Errorf("new data frame 0x%x mid-fragmented-message", f.Opcode)
		case OpcodeContinuation:
			if !fragment {
				return 0, nil, errors.New("websocket continuation frame without a started message")
			}
			payload = append(payload, f.Payload...)
			if f.FIN {
				return opcode, payload, nil
			}
		default:
			return 0, nil, fmt.Errorf("unknown websocket opcode 0x%x", f.Opcode)
		}
	}
}

// writeControl writes one control frame under the write lock.
func (c *Conn) writeControl(op Opcode, payload []byte) error {
	return c.writeFrameLocked(Frame{FIN: true, Opcode: op, Payload: payload})
}

func (c *Conn) writeFrameLocked(f Frame) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeFrame(c.raw, f, true)
}

// WriteText writes one text message, masked.
func (c *Conn) WriteText(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(c.readWait)
	}
	if err := c.raw.SetWriteDeadline(deadline); err != nil {
		return err
	}
	err := c.writeFrameLocked(Frame{FIN: true, Opcode: OpcodeText, Payload: data})
	c.raw.SetWriteDeadline(time.Time{})
	return err
}

// SetReadLimit bounds the payload size of inbound frames from now on.
func (c *Conn) SetReadLimit(n int64) {
	c.readLimit = n
}

// Close sends a close frame and shuts the underlying connection down. A
// second Close is a no-op returning the first Close's result. It does not
// wait for the peer's close: a connection may have a concurrent reader
// (the Socket's read loop), and two readers competing on one conn would
// race on the read deadline, so the closing handshake's drain is skipped.
func (c *Conn) Close(code uint16, reason string) error {
	c.closeOnce.Do(func() {
		payload := makeClosePayload(code, reason)
		c.raw.SetWriteDeadline(time.Now().Add(5 * time.Second))
		err := c.writeFrameLocked(Frame{FIN: true, Opcode: OpcodeClose, Payload: payload})
		c.closeErr = errors.Join(err, c.raw.Close())
	})
	return c.closeErr
}

// setReadDeadline applies a fresh read deadline derived from ctx and the
// configured per-read wait: whichever comes sooner.
func setReadDeadline(ctx context.Context, raw net.Conn, readWait time.Duration) error {
	deadline := time.Now().Add(readWait)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	return raw.SetReadDeadline(deadline)
}

// makeClosePayload serializes a status code and reason per RFC 6455
// section 5.5.1.
func makeClosePayload(code uint16, reason string) []byte {
	payload := make([]byte, 2+len(reason))
	payload[0] = byte(code >> 8)
	payload[1] = byte(code)
	copy(payload[2:], reason)
	return payload
}

// parseClosePayload decodes a close frame payload: an optional 2-byte
// code, then an optional reason.
func parseClosePayload(payload []byte) (uint16, string) {
	if len(payload) < 2 {
		return 1005, "" // no status code present
	}
	code := uint16(payload[0])<<8 | uint16(payload[1])
	return code, string(payload[2:])
}
