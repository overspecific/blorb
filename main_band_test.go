package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/ws"
	"github.com/urfave/cli/v3"
)

// bandAgentID is the agent UUID the band e2e fakes serve.
const bandAgentID = "22222222-2222-2222-2222-222222222222"

// bandE2E holds the two Band fakes plus the LLM backend and the
// observations the assertions read.
type bandE2E struct {
	rest *httptest.Server
	llm  *httptest.Server
	ws   *bandWSFake

	mu      sync.Mutex
	sent    []bandE2ESend
	marks   []string
	llmReqs int
}

type bandE2ESend struct {
	Content  string        `json:"content"`
	Mentions []bandMention `json:"mentions"`
}

type bandMention struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// newBandE2E builds the fakes: a Band REST server, a Band subscriptions
// WebSocket server, and an OpenAI-compatible LLM backend whose turns are
// a band_send_message tool call then a stop.
func newBandE2E(t *testing.T) *bandE2E {
	t.Helper()
	e := &bandE2E{}

	// REST fake.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/me", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"data":{"id":%q,"name":"blorb"}}`, bandAgentID)
	})
	mux.HandleFunc("/api/v1/agent/chats", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"data":[{"id":"room-1"}]}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/api/v1/agent/chats/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/messages/next"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(path, "/context"):
			io.WriteString(w, `{"data":[]}`)
		case strings.HasSuffix(path, "/processing"):
			e.noteMark("processing")
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(path, "/processed"):
			e.noteMark("processed")
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(path, "/failed"):
			e.noteMark("failed")
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(path, "/messages") && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			var wire struct {
				Message struct {
					Content  string        `json:"content"`
					Mentions []bandMention `json:"mentions"`
				} `json:"message"`
			}
			_ = json.Unmarshal(body, &wire)
			e.mu.Lock()
			e.sent = append(e.sent, bandE2ESend{Content: wire.Message.Content, Mentions: wire.Message.Mentions})
			e.mu.Unlock()
			io.WriteString(w, `{"data":{"message":{"id":"out-1"}}}`)
		case strings.HasSuffix(path, "/events"):
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	e.rest = httptest.NewServer(mux)
	t.Cleanup(e.rest.Close)

	// LLM fake: streams when the request asks for it (the band command
	// streams by default), else answers plain JSON. The first request is
	// a band_send_message tool call, the second a stop.
	llmMux := http.NewServeMux()
	llmMux.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)
		e.mu.Lock()
		e.llmReqs++
		n := e.llmReqs
		e.mu.Unlock()
		if n%2 == 1 {
			if req.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				sseData(w,
					`{"id":"r1","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
					`{"id":"r1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"band_send_message","arguments":"{\"content\":\"the reply\","}}]},"finish_reason":null}]}`,
					`{"id":"r1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"mentions\":[{\"id\":\"u-1\",\"name\":\"User One\"}]}"}}]},"finish_reason":null}]}`,
					`{"id":"r1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				)
				return
			}
			io.WriteString(w, `{"id":"r1","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"band_send_message","arguments":"{\"content\":\"the reply\",\"mentions\":[{\"id\":\"u-1\",\"name\":\"User One\"}]}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			sseData(w,
				`{"id":"r2","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
				`{"id":"r2","choices":[{"delta":{},"finish_reason":"stop"}]}`,
			)
			return
		}
		io.WriteString(w, `{"id":"r2","choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`)
	})
	e.llm = httptest.NewServer(llmMux)
	t.Cleanup(e.llm.Close)

	// WebSocket fake.
	e.ws = newBandWSFake(t)
	return e
}

func (e *bandE2E) noteMark(kind string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.marks = append(e.marks, kind)
}

func (e *bandE2E) sentMessages() []bandE2ESend {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]bandE2ESend(nil), e.sent...)
}

func (e *bandE2E) markKinds() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.marks...)
}

// bandWSFake is a minimal Band subscriptions WebSocket server: it
// answers joins and heartbeats and lets tests push server events.
type bandWSFake struct {
	addr      string
	ln        net.Listener
	handshake chan struct{}

	mu      sync.Mutex
	writeMu sync.Mutex
	conn    net.Conn
}

func newBandWSFake(t *testing.T) *bandWSFake {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &bandWSFake{addr: ln.Addr().String(), ln: ln, handshake: make(chan struct{})}
	go s.serve()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *bandWSFake) serve() {
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
	accept := bandAcceptKey(req.Header.Get("Sec-WebSocket-Key"))
	fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()
	close(s.handshake)

	for {
		f, err := bandReadFrame(conn)
		if err != nil {
			return
		}
		var env []json.RawMessage
		if err := json.Unmarshal(f.Payload, &env); err != nil || len(env) < 5 {
			return
		}
		event := bandUnquote(env[3])
		switch event {
		case "phx_join", "phx_leave", "heartbeat":
			s.writeMu.Lock()
			bandWriteFrame(conn, ws.Frame{FIN: true, Opcode: ws.OpcodeText,
				Payload: []byte(`[null,` + string(env[1]) + `,"phoenix","phx_reply",{"status":"ok","response":{}}]`)})
			s.writeMu.Unlock()
		}
	}
}

func (s *bandWSFake) waitHandshake(t *testing.T) {
	t.Helper()
	select {
	case <-s.handshake:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the band ws handshake")
	}
}

// pushMessageCreated pushes one live mention on the room's topic.
func (s *bandWSFake) pushMessageCreated(roomID, id, content string) {
	payload, _ := json.Marshal(map[string]any{
		"id": id, "content": content, "sender_id": "u-1", "sender_name": "User One",
		"sender_type": "User", "message_type": "text",
	})
	raw, _ := json.Marshal([]any{nil, nil, "chat_room:" + roomID, "message_created", json.RawMessage(payload)})
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	bandWriteFrame(conn, ws.Frame{FIN: true, Opcode: ws.OpcodeText, Payload: raw})
}

// --- minimal frame and handshake helpers (the ws codec is internal) ---

func bandReadFrame(conn net.Conn) (ws.Frame, error) {
	var head [2]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return ws.Frame{}, err
	}
	fin := head[0]&0x80 != 0
	op := ws.Opcode(head[0] & 0x0f)
	masked := head[1]&0x80 != 0
	length := int64(head[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(conn, ext[:]); err != nil {
			return ws.Frame{}, err
		}
		length = int64(ext[0])<<8 | int64(ext[1])
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(conn, ext[:]); err != nil {
			return ws.Frame{}, err
		}
		for _, b := range ext {
			length = length<<8 | int64(b)
		}
	}
	var key [4]byte
	if masked {
		if _, err := io.ReadFull(conn, key[:]); err != nil {
			return ws.Frame{}, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return ws.Frame{}, err
	}
	for i := range payload {
		payload[i] ^= key[i%4]
	}
	return ws.Frame{FIN: fin, Opcode: op, Payload: payload}, nil
}

func bandWriteFrame(conn net.Conn, f ws.Frame) {
	length := len(f.Payload)
	head := []byte{byte(f.Opcode)}
	if f.FIN {
		head[0] |= 0x80
	}
	switch {
	case length <= 125:
		head = append(head, byte(length))
	case length <= 0xFFFF:
		head = append(head, 126, byte(length>>8), byte(length))
	default:
		head = append(head, 127, 0, 0, 0, 0, byte(length>>24), byte(length>>16), byte(length>>8), byte(length))
	}
	_, _ = conn.Write(append(head, f.Payload...))
}

func bandAcceptKey(key string) string {
	h := sha1.New()
	h.Write([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func bandUnquote(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

// writeBandConfig writes a blorb.json with an agent band section pointing at
// the fakes.
func writeBandConfig(t *testing.T, llmBaseURL, restURL, wsURL string) string {
	t.Helper()
	cfg := map[string]any{
		"providers": []map[string]any{{
			"name": "local", "type": "openai-compatible", "base_url": llmBaseURL,
		}},
		"models": []map[string]any{{"name": "m", "provider": "local", "model_name": "m"}},
		"agents": []map[string]any{{
			"name": "helper", "system_prompt": "You are helpful.", "model": "m", "max_turns": 3,
			"band": map[string]any{
				"api_key_env": "BAND_API_KEY",
				"rest_url":    restURL,
				"ws_url":      wsURL,
			},
		}},
		"default_agent": "helper",
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "blorb.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestBandCommandMissingSection(t *testing.T) {
	// An agent without a band block is a clear startup error.
	dir := t.TempDir()
	cfg := map[string]any{
		"providers":     []map[string]any{{"name": "local", "type": "openai-compatible", "base_url": "http://localhost:1"}},
		"models":        []map[string]any{{"name": "m", "provider": "local", "model_name": "m"}},
		"agents":        []map[string]any{{"name": "helper", "system_prompt": "hi", "model": "m", "max_turns": 1}},
		"default_agent": "helper",
	}
	data, _ := json.Marshal(cfg)
	path := filepath.Join(dir, "blorb.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	errOut := &bytes.Buffer{}
	cmd := rootCommand()
	cmd.ErrWriter = errOut
	cmd.ExitErrHandler = capturingExitHandler(errOut)
	if err := cmd.Run(context.Background(), []string{"blorb", "band", "-c", path}); err == nil {
		t.Fatal("band without a band section succeeded, want an error")
	}
	if !strings.Contains(errOut.String(), "has no band section") {
		t.Errorf("error = %q, want the missing-band-section message", errOut.String())
	}
}

func TestBandCommandMissingAPIKey(t *testing.T) {
	path := writeBandConfig(t, "http://localhost:1", "http://localhost:2", "ws://localhost:3/socket")
	t.Setenv("BAND_API_KEY", "")

	errOut := &bytes.Buffer{}
	cmd := rootCommand()
	cmd.ErrWriter = errOut
	cmd.ExitErrHandler = capturingExitHandler(errOut)
	if err := cmd.Run(context.Background(), []string{"blorb", "band", "-c", path}); err == nil {
		t.Fatal("band without an API key succeeded, want an error")
	}
	if !strings.Contains(errOut.String(), "BAND_API_KEY") {
		t.Errorf("error = %q, want the api_key_env name", errOut.String())
	}
}

func TestBandCommandEndToEnd(t *testing.T) {
	e := newBandE2E(t)
	t.Setenv("BAND_API_KEY", "test-key")
	path := writeBandConfig(t, e.llm.URL, e.rest.URL, "ws://"+e.ws.addr+"/socket")

	// Run the command with a cancelable context standing in for SIGINT.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	oldStderr := os.Stderr
	os.Stderr = stderrW
	defer func() { os.Stderr = oldStderr }()

	done := make(chan error, 1)
	go func() {
		cmd := rootCommand()
		cmd.Writer = io.Discard
		cmd.ErrWriter = io.Discard
		cmd.ExitErrHandler = capturingExitHandler(io.Discard)
		done <- cmd.Run(ctx, []string{"blorb", "band", "-c", path})
	}()

	e.ws.waitHandshake(t)
	// Push one mention; the room replies through band_send_message and
	// the message is marked processed. The push repeats until the runner
	// has joined the room (an early push is dropped: no room state yet);
	// the repeated id deduplicates, so it is processed at most once.
	waitForE2E(t, func() bool {
		e.ws.pushMessageCreated("room-1", "m-e2e", "hello")
		return len(e.sentMessages()) == 1 && len(e.markKinds()) >= 2
	})
	sent := e.sentMessages()[0]
	if sent.Content != "the reply" {
		t.Errorf("sent content = %q, want the tool-path reply", sent.Content)
	}
	if len(sent.Mentions) != 1 || sent.Mentions[0].ID != "u-1" {
		t.Errorf("sent mentions = %+v, want u-1", sent.Mentions)
	}
	marks := e.markKinds()
	if marks[0] != "processing" || marks[len(marks)-1] != "processed" {
		t.Errorf("marks = %v, want processing then processed", marks)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("band command error = %v, want nil on shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the band command to stop")
	}

	stderrW.Close()
	var footer bytes.Buffer
	if _, err := io.Copy(&footer, stderrR); err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	if !strings.Contains(footer.String(), "session helper") {
		t.Errorf("stderr = %q, want the usage footer", footer.String())
	}
}

func TestBandExampleConfigValidates(t *testing.T) {
	if _, err := config.Load("examples/band/blorb.json"); err != nil {
		t.Fatalf("Load(examples/band/blorb.json) error = %v, want nil", err)
	}
}

func TestBandNoStreamFlag(t *testing.T) {
	cmd := bandCommand()

	var noStream *cli.BoolFlag
	for _, f := range cmd.Flags {
		if bf, ok := f.(*cli.BoolFlag); ok && f.Names()[0] == "no-stream" {
			noStream = bf
			break
		}
	}
	if noStream == nil {
		t.Fatalf("band has no no-stream flag; flags = %+v", cmd.Flags)
	}
	if noStream.Value {
		t.Errorf("no-stream default = true, want false (streaming on by default)")
	}
}

func waitForE2E(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the e2e condition")
}
