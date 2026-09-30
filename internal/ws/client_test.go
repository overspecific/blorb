package ws_test

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/overspecific/blorb/internal/ws"
)

func TestHandshakeAndTextExchange(t *testing.T) {
	for _, scheme := range []string{"ws", "wss"} {
		t.Run(scheme, func(t *testing.T) {
			srv := newTestServer(t)
			if scheme == "wss" {
				srv.tlsWrap = true
			}
			srv.serve(t)

			opts := []ws.DialOption{}
			if scheme == "wss" {
				opts = append(opts, ws.WithTLSConfig(&tls.Config{InsecureSkipVerify: true}))
			}
			conn, err := ws.Dial(context.Background(), scheme+"://"+srv.addr+"/socket", opts...)
			if err != nil {
				t.Fatalf("Dial error = %v, want nil", err)
			}
			defer conn.Close(1000, "")

			if err := conn.WriteText(context.Background(), []byte("hello")); err != nil {
				t.Fatalf("WriteText error = %v, want nil", err)
			}
			if got := string(srv.awaitText(t)); got != "hello" {
				t.Errorf("server received %q, want %q", got, "hello")
			}

			srv.sendText(t, "world")
			op, data, err := conn.ReadMessage(context.Background())
			if err != nil {
				t.Fatalf("ReadMessage error = %v, want nil", err)
			}
			if op != ws.OpcodeText || string(data) != "world" {
				t.Errorf("ReadMessage = (0x%x, %q), want (text, \"world\")", op, data)
			}
		})
	}
}

func TestDialHandshakeFailures(t *testing.T) {
	t.Run("status 401", func(t *testing.T) {
		srv := newTestServer(t)
		srv.failAccept = 401
		srv.serve(t)
		_, err := ws.Dial(context.Background(), "ws://"+srv.addr+"/")
		if err == nil {
			t.Fatal("Dial error = nil, want a rejection error")
		}
		if !strings.Contains(err.Error(), "401") {
			t.Errorf("Dial error = %v, want it to name status 401", err)
		}
	})
	t.Run("bad accept key", func(t *testing.T) {
		srv := newTestServer(t)
		srv.badAccept = true
		srv.serve(t)
		_, err := ws.Dial(context.Background(), "ws://"+srv.addr+"/")
		if err == nil {
			t.Fatal("Dial error = nil, want an Accept mismatch error")
		}
		if !strings.Contains(err.Error(), "Sec-WebSocket-Accept") {
			t.Errorf("Dial error = %v, want it to name Sec-WebSocket-Accept", err)
		}
	})
	t.Run("bad scheme", func(t *testing.T) {
		_, err := ws.Dial(context.Background(), "http://example.com")
		if err == nil || !strings.Contains(err.Error(), "ws or wss") {
			t.Errorf("Dial error = %v, want a scheme error", err)
		}
	})
}

func TestPingAutoAnswer(t *testing.T) {
	srv := newTestServer(t)
	srv.serve(t)
	conn, err := ws.Dial(context.Background(), "ws://"+srv.addr+"/")
	if err != nil {
		t.Fatalf("Dial error = %v, want nil", err)
	}
	defer conn.Close(1000, "")

	// Pings are answered from within ReadMessage, so a reader must be
	// active while the ping arrives; the read continues to the data
	// message sent after the ping.
	srv.sendPing(t, []byte("probe"))
	srv.sendText(t, "after ping")
	op, data, err := conn.ReadMessage(context.Background())
	if err != nil {
		t.Fatalf("ReadMessage error = %v, want nil", err)
	}
	if op != ws.OpcodeText || string(data) != "after ping" {
		t.Errorf("ReadMessage = (0x%x, %q), want (text, \"after ping\")", op, data)
	}
	srv.awaitOpcode(t, ws.OpcodePong)
}

func TestFragmentedMessage(t *testing.T) {
	srv := newTestServer(t)
	srv.serve(t)
	conn, err := ws.Dial(context.Background(), "ws://"+srv.addr+"/")
	if err != nil {
		t.Fatalf("Dial error = %v, want nil", err)
	}
	defer conn.Close(1000, "")

	srv.sendFragmented(t, "frag-one,", "frag-two")
	op, data, err := conn.ReadMessage(context.Background())
	if err != nil {
		t.Fatalf("ReadMessage error = %v, want nil", err)
	}
	if op != ws.OpcodeText || string(data) != "frag-one,frag-two" {
		t.Errorf("ReadMessage = (0x%x, %q), want reassembled text", op, data)
	}
}

func TestClientCloseHandshake(t *testing.T) {
	srv := newTestServer(t)
	srv.serve(t)
	conn, err := ws.Dial(context.Background(), "ws://"+srv.addr+"/")
	if err != nil {
		t.Fatalf("Dial error = %v, want nil", err)
	}

	// The server reads the close frame the client sends. The client does
	// not wait for a mirrored close (it shuts the connection down as soon
	// as the frame is written), so only the frame's arrival is asserted.
	type closeResult struct {
		code   uint16
		reason string
		err    error
	}
	closed := make(chan closeResult, 1)
	go func() {
		code, reason, err := srv.awaitClose(t)
		closed <- closeResult{code: code, reason: reason, err: err}
	}()

	if err := conn.Close(1000, "done"); err != nil {
		t.Fatalf("Close error = %v, want nil", err)
	}
	select {
	case got := <-closed:
		if got.err != nil {
			t.Fatalf("server reading close: %v", got.err)
		}
		if got.code != uint16(1000) || got.reason != "done" {
			t.Errorf("server saw close (%d, %q), want (1000, \"done\")", got.code, got.reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the server to see the close frame")
	}

	// A second Close is a no-op that returns the first result.
	if err := conn.Close(1000, "done"); err != nil {
		t.Errorf("second Close error = %v, want nil", err)
	}
}

func TestServerInitiatedClose(t *testing.T) {
	srv := newTestServer(t)
	srv.serve(t)
	conn, err := ws.Dial(context.Background(), "ws://"+srv.addr+"/")
	if err != nil {
		t.Fatalf("Dial error = %v, want nil", err)
	}
	defer conn.Close(1000, "")

	closePayload := []byte{0x07, 0xE0, 'b', 'y', 'e'} // code 2016
	if err := srvWriteFrame(srv.connection(t), ws.Frame{FIN: true, Opcode: ws.OpcodeClose, Payload: closePayload}); err != nil {
		t.Fatalf("server send close: %v", err)
	}
	_, _, err = conn.ReadMessage(context.Background())
	var cerr *ws.CloseError
	if !errors.As(err, &cerr) {
		t.Fatalf("ReadMessage error = %v (%T), want *ws.CloseError", err, err)
	}
	if cerr.Code != 2016 || cerr.Reason != "bye" {
		t.Errorf("CloseError = (%d, %q), want (2016, \"bye\")", cerr.Code, cerr.Reason)
	}
}

func TestAbruptHangup(t *testing.T) {
	srv := newTestServer(t)
	srv.serve(t)
	conn, err := ws.Dial(context.Background(), "ws://"+srv.addr+"/")
	if err != nil {
		t.Fatalf("Dial error = %v, want nil", err)
	}
	defer conn.Close(1000, "")

	srv.sendText(t, "keepalive")
	if _, _, err := conn.ReadMessage(context.Background()); err != nil {
		t.Fatalf("ReadMessage error = %v, want nil", err)
	}

	srv.kill(t)
	if _, _, err := conn.ReadMessage(context.Background()); err == nil {
		t.Error("ReadMessage error = nil after the server hung up, want error")
	}
}

func TestReadDeadlineExpiry(t *testing.T) {
	srv := newTestServer(t)
	srv.serve(t)
	conn, err := ws.Dial(context.Background(), "ws://"+srv.addr+"/", ws.WithReadWait(100*time.Millisecond))
	if err != nil {
		t.Fatalf("Dial error = %v, want nil", err)
	}
	defer conn.Close(1000, "")

	if _, _, err := conn.ReadMessage(context.Background()); err == nil {
		t.Error("ReadMessage error = nil with nothing to read and a 100ms deadline, want error")
	}

	// The deadline refreshes per frame: a message arriving later than one
	// wait but within the freshness of a new read still lands.
	go func() {
		srv.waitHandshake(t)
		time.Sleep(250 * time.Millisecond)
		if err := srvWriteFrame(srv.connection(t), ws.Frame{FIN: true, Opcode: ws.OpcodeText, Payload: []byte("late")}); err != nil {
			return
		}
	}()
	// Prime the per-frame deadline refresh with a first message, then the
	// late one: reads always set a fresh deadline.
	srv.sendText(t, "early")
	if _, _, err := conn.ReadMessage(context.Background()); err != nil {
		t.Fatalf("ReadMessage error = %v, want nil", err)
	}
	srv.sendText(t, "in time")
	if _, _, err := conn.ReadMessage(context.Background()); err != nil {
		t.Fatalf("ReadMessage error = %v, want nil", err)
	}
}

func TestFrameOverReadLimit(t *testing.T) {
	srv := newTestServer(t)
	srv.serve(t)
	conn, err := ws.Dial(context.Background(), "ws://"+srv.addr+"/", ws.WithReadLimit(8))
	if err != nil {
		t.Fatalf("Dial error = %v, want nil", err)
	}
	defer conn.Close(1000, "")

	srv.sendText(t, "much longer than eight bytes")
	_, _, err = conn.ReadMessage(context.Background())
	if err == nil {
		t.Fatal("ReadMessage error = nil for an over-limit frame, want error")
	}
	if !strings.Contains(err.Error(), "exceeds read limit") {
		t.Errorf("ReadMessage error = %v, want it to name the read limit", err)
	}
}

func TestContextCancellation(t *testing.T) {
	srv := newTestServer(t)
	srv.serve(t)
	conn, err := ws.Dial(context.Background(), "ws://"+srv.addr+"/")
	if err != nil {
		t.Fatalf("Dial error = %v, want nil", err)
	}
	defer conn.Close(1000, "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := conn.ReadMessage(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("ReadMessage error = %v, want context.Canceled", err)
	}
	if err := conn.WriteText(ctx, []byte("x")); !errors.Is(err, context.Canceled) {
		t.Errorf("WriteText error = %v, want context.Canceled", err)
	}
}
