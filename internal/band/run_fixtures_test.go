package band_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/overspecific/blorb/internal/band"
	"github.com/overspecific/blorb/internal/chat"
	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/logging"
)

// runnerAgentID is this Band agent's own UUID the runner fakes serve.
const runnerAgentID = "11111111-1111-1111-1111-111111111111"

// runnerWSFake is a scripted Band subscriptions WebSocket server for
// runner tests: joins and heartbeats get ok replies, and tests push
// raw server-initiated envelopes that flow to the client.
type runnerWSFake struct {
	*fakeBandWSServer
}

func newRunnerWSFake(t *testing.T) *runnerWSFake {
	t.Helper()
	return &runnerWSFake{fakeBandWSServer: newFakeBandWSServer(t)}
}

// pushEvent enqueues one server-initiated envelope (null refs).
func (s *runnerWSFake) pushEvent(topic, event, payload string) {
	s.push(`[null,null,"` + topic + `","` + event + `",` + payload + `]`)
}

// pushMessageCreated enqueues one live mention on a room's topic; msg
// is the mention payload the room processes.
func (s *runnerWSFake) pushMessageCreated(roomID string, msg band.ChatMessage) {
	raw, err := json.Marshal([]any{nil, nil, "chat_room:" + roomID, "message_created", msg})
	if err != nil {
		panic(err)
	}
	s.push(string(raw))
}

// pushRoomAdded enqueues one room_added on the rooms topic.
func (s *runnerWSFake) pushRoomAdded(t *testing.T, roomID string) {
	t.Helper()
	s.pushEvent("agent_rooms:"+runnerAgentID, "room_added", `{"room":{"id":"`+roomID+`"}}`)
}

// pushRoomRemoved enqueues one room_removed on the rooms topic.
func (s *runnerWSFake) pushRoomRemoved(t *testing.T, roomID string) {
	t.Helper()
	s.pushEvent("agent_rooms:"+runnerAgentID, "room_removed", `{"room":{"id":"`+roomID+`"}}`)
}

// pushClose enqueues one phx_close ending the socket.
func (s *runnerWSFake) pushClose() {
	s.push(`[null,null,"phoenix","phx_close",{"reason":"server quitting"}]`)
}

// runnerRestFake is the REST half: it answers /me and /chats with
// fixtures, scripts per-room /messages/next drains, and records
// message sends and mark calls.
type runnerRestFake struct {
	srv *httptest.Server

	allMu sync.Mutex
	all   []string

	mu      sync.Mutex
	markLog []string
	sent    []bandSent

	// next scripts per-room /messages/next drain queues.
	next map[string][]string
}

// contextJSON renders one room's scripted /context body: the send
// drain bodies become the context messages (the room's own past
// activity, oldest first).
func (f *runnerRestFake) contextJSON(roomID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	bodies := f.next[roomID]
	out := "["
	for i, b := range bodies {
		if i > 0 {
			out += ","
		}
		// A drained mention is also the context message: same shape.
		out += b
	}
	return out + "]"
}

// newRunnerRestFakeUnauthorized serves /me with a 401 error envelope.
func newRunnerRestFakeUnauthorized(t *testing.T) *runnerRestFake {
	t.Helper()
	f := &runnerRestFake{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/me", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "bad key", "req-u")
	})
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.allMu.Lock()
		f.all = append(f.all, r.Method+" "+r.URL.Path)
		f.allMu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newRunnerRestFake(t *testing.T, roomsJSON string, nextBodies map[string][]string) *runnerRestFake {
	t.Helper()
	f := &runnerRestFake{next: nextBodies}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/me", func(w http.ResponseWriter, _ *http.Request) {
		writeData(w, http.StatusOK, `{"id":"`+runnerAgentID+`","name":"blorb agent"}`)
	})
	mux.HandleFunc("/api/v1/agent/chats", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agent/chats":
			writeData(w, http.StatusOK, roomsJSON)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages"):
			f.recordSend(t, w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	t.Logf("runnerRestFake: registering /chats/ subroutes")
	mux.HandleFunc("/api/v1/agent/chats/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/context"):
			writeData(w, http.StatusOK, f.contextJSON(roomIDOf(path)))
		case strings.HasSuffix(path, "/processing"):
			f.noteMark("processing " + roomIDOf(path))
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(path, "/processed"):
			f.noteMark("processed " + roomIDOf(path))
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(path, "/failed"):
			failedBody, _ := io.ReadAll(r.Body)
			f.noteMark("failed " + roomIDOf(path) + ": " + string(failedBody))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/messages"):
			f.recordSend(t, w, r)
		case strings.HasSuffix(path, "/messages/next"):
			f.serveNext(w, roomIDOf(path))
		case strings.HasSuffix(path, "/events"):
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.allMu.Lock()
		f.all = append(f.all, r.Method+" "+r.URL.Path)
		f.allMu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// roomIDOf extracts the chat id from a /api/v1/agent/chats/{id}/...
// path: whatever follows the prefix up to the next slash.
func roomIDOf(path string) string {
	const prefix = "/api/v1/agent/chats/"
	trimmed := strings.TrimPrefix(path, prefix)
	if i := strings.Index(trimmed, "/"); i >= 0 {
		return trimmed[:i]
	}
	return trimmed
}

// recordSend records one message send and answers ok.
func (f *runnerRestFake) recordSend(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	bodyBytes, _ := io.ReadAll(r.Body)
	bodyCopy := string(bodyBytes)
	t.Logf("recordSend: %s %s body=%s", r.Method, r.URL.Path, bodyCopy)
	var wire struct {
		Message struct {
			Content  string         `json:"content"`
			Mentions []band.Mention `json:"mentions"`
		} `json:"message"`
	}
	_ = json.Unmarshal(bodyBytes, &wire)
	f.mu.Lock()
	f.sent = append(f.sent, bandSent{Content: wire.Message.Content, Mentions: wire.Message.Mentions})
	f.mu.Unlock()
	writeData(w, http.StatusOK, `{"message":{"id":"out-1"}}`)
}

func (f *runnerRestFake) noteMark(kind string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markLog = append(f.markLog, kind)
}

// serveNext answers one /messages/next drain step with the scripted
// body, or 204 once the script is over.
func (f *runnerRestFake) serveNext(w http.ResponseWriter, roomID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	queue := f.next[roomID]
	if len(queue) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	f.next[roomID] = queue[1:]
	writeData(w, http.StatusOK, queue[0])
}

func (f *runnerRestFake) sentMessages() []bandSent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bandSent(nil), f.sent...)
}

func (f *runnerRestFake) marks() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.markLog...)
}

// roomListJSON builds a /chats page listing the given room ids.
func roomListJSON(ids ...string) string {
	out := "["
	for i, id := range ids {
		if i > 0 {
			out += ","
		}
		out += `{"id":"` + id + `"}`
	}
	return out + "]"
}

// drainBodies scripts a room's /messages/next: each listed mention is
// served once, then 204. mention contents are plain; senders are
// User One / u-1.
func drainBodies(contents ...string) []string {
	out := make([]string, len(contents))
	for i, c := range contents {
		out[i] = `{"id":"m-drain` + itoaRunner(i+1) + `","sender_id":"u-1","sender_name":"User One","sender_type":"User","message_type":"text","content":` + jsonQuoteRunner(c) + `}`
	}
	return out
}

// itoaRunner renders a small non-negative int in decimal.
func itoaRunner(n int) string {
	digits := ""
	if n == 0 {
		return "0"
	}
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

// jsonQuoteRunner encodes one string as a JSON string value.
func jsonQuoteRunner(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(raw)
}

// llmFactory is the client factory the runner fakes hand out.
type llmFactory func(config.Config, config.Agent) (llm.Client, error)

// llmFakeServer builds an httptest OpenAI-compatible chat completions
// backend serving one canned response per call, in order. The room
// engines reach it through the real openai client via the config's
// base_url pointing here.
type llmFakeServer struct {
	srv *httptest.Server

	mu   sync.Mutex
	reqs int
}

func newRunnerLLMFake(t *testing.T) (*llmFakeServer, llmFactory) {
	t.Helper()
	f := &llmFakeServer{}

	mux := http.NewServeMux()
	mux.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs++
		n := f.reqs
		f.mu.Unlock()

		// Every turn is scripted as a band_send_message tool call
		// followed by an empty final text: odd calls are the tool call,
		// even calls the wrap-up.
		if n%2 == 1 {
			_, _ = w.Write([]byte(`{"id":"r1","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"band_send_message","arguments":"{\"content\":\"the reply\",\"mentions\":[{\"id\":\"u-1\",\"name\":\"User One\"}]}"}}]},"finish_reason":"tool_calls"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"r2","choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`))
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)

	factory := func(cfg config.Config, agent config.Agent) (llm.Client, error) {
		// The config's provider base_url is pointed at the fake by
		// the test config; build the real client.
		getenv := func(string) string { return "test-key" }
		return chat.NewClientWithGetenv(cfg, agent, getenv, logging.NewNop())
	}
	return f, factory
}

// mentionFor builds the live/drain mention for a room.
func mentionFor(roomID, msgID, content string) band.ChatMessage {
	_ = roomID
	return band.ChatMessage{
		ID:          msgID,
		Content:     content,
		SenderID:    "u-1",
		SenderName:  "User One",
		SenderType:  "User",
		MessageType: "text",
	}
}
