package band_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/overspecific/blorb/internal/band"
	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/logging"
)

// roomTestConfig builds the canonical single-agent config the room
// tests run, returning both the config and the resolved agent.
func roomTestConfig() (config.Config, config.Agent) {
	agent := config.Agent{
		Name:         "helper",
		SystemPrompt: "You are helpful.",
		Model:        "m",
		MaxTurns:     3,
	}
	cfg := config.Config{
		Providers: []config.Provider{{
			Name:    "local",
			Type:    config.ProviderTypeOpenAI,
			BaseURL: "http://localhost:1",
		}},
		Models: []config.Model{{Name: "m", Provider: "local", ModelName: "m"}},
		Agents: []config.Agent{agent},
	}
	return cfg, agent
}

// roomLLM is a canned llm.Client: one response per turn, requests
// recorded.
type roomLLM struct {
	mu        sync.Mutex
	responses []llm.Response
	requests  []llm.Request
}

func (f *roomLLM) Chat(_ context.Context, req llm.Request) (*llm.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if len(f.responses) == 0 {
		return nil, errors.New("roomLLM: no more canned responses")
	}
	resp := f.responses[0]
	f.responses = f.responses[1:]
	return &resp, nil
}

// roomTextResp builds one plain-text assistant response.
func roomTextResp(content string) llm.Response {
	return llm.Response{
		ID:           "resp",
		Message:      llm.NewTextMessage(llm.RoleAssistant, content),
		FinishReason: llm.FinishStop,
	}
}

// roomToolCallResp builds one response invoking a tool; args is the
// JSON arguments body as a string.
func roomToolCallResp(name, args string) llm.Response {
	return llm.Response{
		ID: "resp",
		Message: llm.Message{
			Role:      llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function", FunctionName: name, FunctionArgs: args}},
		},
		FinishReason: llm.FinishToolCalls,
	}
}

// bandRestFake records what the room runtime does to the platform:
// mark calls, message sends, and posted events. Context seeding is
// answered with a scripted page.
type bandRestFake struct {
	srv *httptest.Server

	mu      sync.Mutex
	markLog []string
	sent    []bandSent
	events  []bandPostedEvent

	// eventsStatus, when non-zero, is the HTTP status the events
	// endpoint answers, to exercise the best-effort diagnostic path.
	eventsStatus int
}

type bandSent struct {
	Content  string
	Mentions []band.Mention
}

type bandPostedEvent struct {
	Content     string
	MessageType string
	Metadata    string
}

// bandHandler answers the endpoints a Handle flow hits: processing and
// processed marks, message sends, room events, and the context seeding
// endpoint (contextPage is the inner data value, possibly "[]").
func bandHandler(t *testing.T, f *bandRestFake, contextPage string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/processing"):
			f.noteMark("processing")
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/processed"):
			f.noteMark("processed")
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/failed"):
			f.noteMark("failed: " + string(body))
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/messages"):
			var wire struct {
				Message struct {
					Content  string         `json:"content"`
					Mentions []band.Mention `json:"mentions"`
				} `json:"message"`
			}
			_ = json.Unmarshal(body, &wire)
			f.mu.Lock()
			f.sent = append(f.sent, bandSent{Content: wire.Message.Content, Mentions: wire.Message.Mentions})
			f.mu.Unlock()
			writeData(w, http.StatusOK, `{"message":{"id":"sent-1"}}`)
		case strings.HasSuffix(r.URL.Path, "/events"):
			var wire struct {
				Event struct {
					Content     string          `json:"content"`
					MessageType string          `json:"message_type"`
					Metadata    json.RawMessage `json:"metadata"`
				} `json:"event"`
			}
			_ = json.Unmarshal(body, &wire)
			f.mu.Lock()
			f.events = append(f.events, bandPostedEvent{
				Content:     wire.Event.Content,
				MessageType: wire.Event.MessageType,
				Metadata:    string(wire.Event.Metadata),
			})
			status := f.eventsStatus
			f.mu.Unlock()
			if status != 0 {
				writeError(w, status, "EVENT_REJECTED", "no events now", "req-1")
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/context"):
			writeData(w, http.StatusOK, contextPage)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (f *bandRestFake) noteMark(kind string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markLog = append(f.markLog, kind)
}

// newBandRestFake builds the fake for the given context page.
func newBandRestFake(t *testing.T, contextPage string) *bandRestFake {
	t.Helper()
	f := &bandRestFake{}
	f.srv = httptest.NewServer(bandHandler(t, f, contextPage))
	t.Cleanup(f.srv.Close)
	return f
}

// sentMessages returns the messages the room sent, in order.
func (f *bandRestFake) sentMessages() []bandSent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bandSent(nil), f.sent...)
}

// postedEvents returns the events the room posted, in order.
func (f *bandRestFake) postedEvents() []bandPostedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bandPostedEvent(nil), f.events...)
}

// marks returns the mark kinds in order: "processing", "processed",
// or "failed: <body>".
func (f *bandRestFake) marks() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.markLog...)
}

// newRoom builds a Room against the fake platform with the canned LLM
// responses.
func newRoom(t *testing.T, f *bandRestFake, llmResp []llm.Response) (*band.Room, *roomLLM) {
	t.Helper()
	return newRoomFull(t, f, llmResp, nil, nil, false)
}

// newRoomWithDiag builds a Room like newRoom, sending best-effort
// diagnostics to diag.
func newRoomWithDiag(t *testing.T, f *bandRestFake, llmResp []llm.Response, diag io.Writer) (*band.Room, *roomLLM) {
	t.Helper()
	return newRoomFull(t, f, llmResp, diag, nil, false)
}

// newRoomWithOutput builds a Room like newRoom, printing agent turn
// output to stdout.
func newRoomWithOutput(t *testing.T, f *bandRestFake, llmResp []llm.Response, stdout io.Writer) (*band.Room, *roomLLM) {
	t.Helper()
	return newRoomFull(t, f, llmResp, nil, stdout, false)
}

// newRoomFull builds a Room against the fake platform with the given
// sinks and canned LLM responses.
func newRoomFull(t *testing.T, f *bandRestFake, llmResp []llm.Response, diag, stdout io.Writer, toolOutput bool) (*band.Room, *roomLLM) {
	t.Helper()
	return newRoomStreaming(t, f, llmResp, diag, stdout, toolOutput, false)
}

// newRoomStreaming builds a Room like newRoomFull with streaming
// optionally enabled; the client is streaming-capable only when stream
// is true, so the capability check is exercised rather than bypassed.
func newRoomStreaming(t *testing.T, f *bandRestFake, llmResp []llm.Response, diag, stdout io.Writer, toolOutput, stream bool) (*band.Room, *roomLLM) {
	t.Helper()
	cfg, agent := roomTestConfig()
	client := band.NewClient(f.srv.URL, "k", logging.NewNop())
	llmFake := &roomLLM{responses: llmResp}
	var llmClient llm.Client = llmFake
	if stream {
		llmClient = &roomStreamLLM{inner: llmFake}
	}
	room, err := band.NewRoom(band.RoomOptions{
		Config:      cfg,
		Agent:       agent,
		Client:      client,
		RoomID:      "room-1",
		AgentID:     "agent-1",
		Diagnostics: diag,
		Stdout:      stdout,
		ToolOutput:  toolOutput,
		Stream:      stream,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
			return llmClient, nil
		},
	})
	if err != nil {
		t.Fatalf("NewRoom error = %v, want nil", err)
	}
	t.Cleanup(room.Close)
	return room, llmFake
}

// roomStreamLLM makes the canned roomLLM streaming-capable: it emits the
// response's text as one delta before returning the whole response.
type roomStreamLLM struct {
	inner *roomLLM
}

func (f *roomStreamLLM) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return f.inner.Chat(ctx, req)
}

func (f *roomStreamLLM) ChatStream(ctx context.Context, req llm.Request, onDelta func(llm.Delta) error) (*llm.Response, error) {
	resp, err := f.inner.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Message.Content != "" {
		if err := onDelta(llm.Delta{Content: resp.Message.Content}); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// mentionMsg builds one incoming mention from the named sender.
func mentionMsg(senderID, senderName, content string) band.ChatMessage {
	return band.ChatMessage{
		ID:          "m-1",
		Content:     content,
		SenderID:    senderID,
		SenderName:  senderName,
		SenderType:  "User",
		MessageType: "text",
	}
}

// handleMsg runs one Handle with the room's canonical message.
func handleMsg(t *testing.T, room *band.Room, msg band.ChatMessage) error {
	t.Helper()
	return room.Handle(context.Background(), msg)
}
