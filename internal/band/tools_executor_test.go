package band_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/overspecific/blorb/internal/band"
	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/logging"
	"github.com/overspecific/blorb/internal/tools"
)

// toolFake is an httptest server serving the Agent API endpoints the
// seven band tools hit, recording every request for byte-checks.
type toolFake struct {
	srv *httptest.Server

	mu   sync.Mutex
	reqs []toolRequest
}

type toolRequest struct {
	Method string
	Path   string
	APIKey string
	Body   []byte
}

func newToolFake(t *testing.T, handler http.HandlerFunc) *toolFake {
	t.Helper()
	f := &toolFake{}
	wrapped := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, toolRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			APIKey: r.Header.Get("X-API-Key"),
			Body:   body,
		})
		f.mu.Unlock()
		handler(w, r)
	}
	f.srv = httptest.NewServer(http.HandlerFunc(wrapped))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *toolFake) seen() []toolRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]toolRequest, len(f.reqs))
	copy(out, f.reqs)
	return out
}

// executor builds an executor bound to room-1 against the fake.
func (f *toolFake) executor(t *testing.T, agentID string, onSend func()) tools.BandExecutor {
	t.Helper()
	client := band.NewClient(f.srv.URL, "k", logging.NewNop())
	return band.NewToolExecutor(client, "room-1", agentID, onSend)
}

// run invokes a tool by name with JSON args through the standard
// executor surface.
func runTool(t *testing.T, exec tools.BandExecutor, name, args string) (tools.ToolResult, error) {
	t.Helper()
	return exec.RunBandTool(context.Background(), name, []byte(args), logging.NewNop())
}

// decode decodes a tool result's output JSON.
func decode(t *testing.T, res tools.ToolResult, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(res.Output), v); err != nil {
		t.Fatalf("decode output %q: %v", res.Output, err)
	}
}

func TestExecutorSendMessage(t *testing.T) {
	f := newToolFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chats/room-1/messages") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeData(w, http.StatusOK, `{"message":{"id":"sent-1"}}`)
	})

	var fired int
	exec := f.executor(t, "agent-1", func() { fired++ })
	res, err := runTool(t, exec, band.ToolSendMessage,
		`{"content":"hi","mentions":[{"id":"u-1"}]}`)
	if err != nil {
		t.Fatalf("RunBandTool error = %v, want nil", err)
	}
	var out struct {
		Sent bool `json:"sent"`
	}
	decode(t, res, &out)
	if !out.Sent {
		t.Errorf("output = %s, want {\"sent\":true}", res.Output)
	}
	if fired != 1 {
		t.Errorf("onSendMessage fired %d times after a successful send, want 1", fired)
	}

	req := f.seen()[0]
	if !strings.Contains(string(req.Body), `"content":"hi"`) {
		t.Errorf("body = %s, want the content", req.Body)
	}
	if !strings.Contains(string(req.Body), `"mentions":[{"id":"u-1"}]`) {
		t.Errorf("body = %s, want the mention", req.Body)
	}
}

func TestExecutorSendMessageValidation(t *testing.T) {
	f := newToolFake(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a rejected send must not reach the platform")
	})
	exec := f.executor(t, "agent-1", nil)

	t.Run("missing content", func(t *testing.T) {
		res, err := runTool(t, exec, band.ToolSendMessage, `{"mentions":[{"id":"u"}]}`)
		if err != nil {
			t.Fatalf("RunBandTool error = %v, want nil (a failed result is not an error)", err)
		}
		if !res.Err || !strings.Contains(res.Output, "non-empty") {
			t.Errorf("result = %+v, want Err with a non-empty-content message", res)
		}
	})

	t.Run("empty mentions", func(t *testing.T) {
		res, err := runTool(t, exec, band.ToolSendMessage, `{"content":"hi"}`)
		if err != nil {
			t.Fatalf("RunBandTool error = %v, want nil", err)
		}
		if !res.Err || !strings.Contains(res.Output, "mention") {
			t.Errorf("result = %+v, want Err naming the mentions", res)
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		res, err := runTool(t, exec, band.ToolSendMessage, `{`)
		if err != nil {
			t.Fatalf("RunBandTool error = %v, want nil", err)
		}
		if !res.Err {
			t.Errorf("result = %+v, want Err", res)
		}
	})

	t.Run("name-only mention", func(t *testing.T) {
		res, err := runTool(t, exec, band.ToolSendMessage, `{"content":"hi","mentions":[{"name":"Simon Russell"}]}`)
		if err != nil {
			t.Fatalf("RunBandTool error = %v, want nil", err)
		}
		if !res.Err || !strings.Contains(res.Output, `"id" or "handle"`) {
			t.Errorf("result = %+v, want Err telling the model a name alone does not identify anyone", res)
		}
	})

	t.Run("self mention", func(t *testing.T) {
		res, err := runTool(t, exec, band.ToolSendMessage, `{"content":"hi","mentions":[{"id":"agent-1"}]}`)
		if err != nil {
			t.Fatalf("RunBandTool error = %v, want nil", err)
		}
		if !res.Err || !strings.Contains(res.Output, "yourself") {
			t.Errorf("result = %+v, want Err saying the agent cannot mention itself", res)
		}
	})
}

func TestExecutorSendEvent(t *testing.T) {
	f := newToolFake(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chats/room-1/events") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	exec := f.executor(t, "agent-1", nil)
	res, err := runTool(t, exec, band.ToolSendEvent,
		`{"content":"called read","message_type":"thought"}`)
	if err != nil {
		t.Fatalf("RunBandTool error = %v, want nil", err)
	}
	if res.Err {
		t.Errorf("result = %+v, want success", res)
	}

	reqs := f.seen()
	if len(reqs) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(reqs))
	}
	if !strings.Contains(string(reqs[0].Body), `"message_type":"thought"`) ||
		!strings.Contains(string(reqs[0].Body), `"content":"called read"`) {
		t.Errorf("body = %s, want the message type and content", reqs[0].Body)
	}
}

func TestExecutorSendEventValidation(t *testing.T) {
	f := newToolFake(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a rejected event must not reach the platform")
	})
	exec := f.executor(t, "agent-1", nil)

	res, err := runTool(t, exec, band.ToolSendEvent, `{"content":"x","message_type":"tool_call"}`)
	if err != nil {
		t.Fatalf("RunBandTool error = %v, want nil", err)
	}
	if !res.Err || !strings.Contains(res.Output, "one of thought, error, task") {
		t.Errorf("result = %+v, want Err naming the allowed message types", res)
	}
}

func TestExecutorGetParticipants(t *testing.T) {
	f := newToolFake(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chats/room-1/participants") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeData(w, http.StatusOK, `[`+
			`{"id":"agent-1","role":"member","status":"active","type":"Agent","name":"Helper"},`+
			`{"id":"u-1","role":"owner","status":"active","type":"User","name":"U"}]`)
	})
	exec := f.executor(t, "agent-1", nil)
	res, err := runTool(t, exec, band.ToolGetParticipants, `{}`)
	if err != nil {
		t.Fatalf("RunBandTool error = %v, want nil", err)
	}
	var participants []struct {
		ID     string `json:"id"`
		IsSelf bool   `json:"is_self"`
	}
	decode(t, res, &participants)
	if len(participants) != 2 {
		t.Fatalf("participants = %+v, want two entries", participants)
	}
	if participants[0].ID != "agent-1" || !participants[0].IsSelf {
		t.Errorf("participants[0] = %+v, want agent-1 marked is_self", participants[0])
	}
	if participants[1].ID != "u-1" || participants[1].IsSelf {
		t.Errorf("participants[1] = %+v, want u-1 not marked is_self", participants[1])
	}
}

func TestExecutorAddParticipant(t *testing.T) {
	f := newToolFake(t, func(w http.ResponseWriter, r *http.Request) {
		writeData(w, http.StatusCreated, `{"id":"u-2","role":"admin","status":"active","type":"User"}`)
	})
	exec := f.executor(t, "agent-1", nil)
	res, err := runTool(t, exec, band.ToolAddParticipant, `{"participant_id":"u-2"}`)
	if err != nil {
		t.Fatalf("RunBandTool error = %v, want nil", err)
	}
	var p band.ChatParticipant
	decode(t, res, &p)
	if p.ID != "u-2" {
		t.Errorf("participant = %+v, want u-2", p)
	}
	if strings.Contains(string(f.seen()[0].Body), `"role"`) {
		t.Errorf("body = %s, want role omitted when unset", f.seen()[0].Body)
	}
}

func TestExecutorRemoveParticipant(t *testing.T) {
	f := newToolFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		writeData(w, http.StatusOK, `{"id":"u-2","status":"removed"}`)
	})
	exec := f.executor(t, "agent-1", nil)
	if _, err := runTool(t, exec, band.ToolRemoveParticipant, `{"participant_id":"u-2"}`); err != nil {
		t.Fatalf("RunBandTool error = %v, want nil", err)
	}
	if !strings.Contains(f.seen()[0].Path, "/participants/u-2") {
		t.Errorf("path = %q, want the participant id", f.seen()[0].Path)
	}
}

func TestExecutorLookupPeers(t *testing.T) {
	f := newToolFake(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/peers") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeData(w, http.StatusOK, `[{"id":"p-1","name":"Peer","handle":"h","type":"Agent"}]`)
	})
	exec := f.executor(t, "agent-1", nil)
	res, err := runTool(t, exec, band.ToolLookupPeers, `{"page":1}`)
	if err != nil {
		t.Fatalf("RunBandTool error = %v, want nil", err)
	}
	var peers []band.Peer
	decode(t, res, &peers)
	if len(peers) != 1 || peers[0].ID != "p-1" {
		t.Errorf("peers = %+v, want one p-1", peers)
	}
}

func TestExecutorCreateChatroom(t *testing.T) {
	f := newToolFake(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chats") {
			writeData(w, http.StatusOK, `{"id":"new-1","title":"T"}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/participants") {
			// The auto-add check happens below, over the recorded
			// requests: the recorder drains bodies before handlers run.
			writeData(w, http.StatusCreated, `{"id":"agent-1","role":"owner","status":"active","type":"Agent"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	exec := f.executor(t, "agent-1", nil)
	res, err := runTool(t, exec, band.ToolCreateChatroom, `{"title":"T"}`)
	if err != nil {
		t.Fatalf("RunBandTool error = %v, want nil", err)
	}
	var room band.ChatRoom
	decode(t, res, &room)
	if room.ID != "new-1" {
		t.Errorf("room = %+v, want new-1", room)
	}

	// Request 0 is the create; request 1 is the auto-add.
	reqs := f.seen()
	if len(reqs) != 2 {
		t.Fatalf("server saw %d requests, want 2 (create plus auto-add)", len(reqs))
	}
	var parsed struct {
		Participant struct {
			ParticipantID string `json:"participant_id"`
			Role          string `json:"role"`
		} `json:"participant"`
	}
	if err := json.Unmarshal(reqs[1].Body, &parsed); err != nil {
		t.Fatalf("decode auto-add body %s: %v", reqs[1].Body, err)
	}
	if parsed.Participant.ParticipantID != "agent-1" || parsed.Participant.Role != "owner" {
		t.Errorf("auto-add body = %s, want agent-1 as owner", reqs[1].Body)
	}
}

func TestExecutorPlatformFailureSurfacesAsErr(t *testing.T) {
	f := newToolFake(t, func(w http.ResponseWriter, r *http.Request) {
		writeError(w, 422, "VALIDATION", "mentions must resolve", "req-7")
	})
	exec := f.executor(t, "agent-1", nil)
	res, err := runTool(t, exec, band.ToolSendMessage, `{"content":"hi","mentions":[{"id":"u"}]}`)
	if err != nil {
		t.Fatalf("RunBandTool error = %v, want nil (the platform failure is a result)", err)
	}
	if !res.Err {
		t.Errorf("result Err = false, want true")
	}
	if !strings.Contains(res.Output, "mentions must resolve") {
		t.Errorf("result output = %q, want the server's message", res.Output)
	}
}

func TestExecutorOnSendMessageFiresOnlyOnSend(t *testing.T) {
	fires := 0
	f := newToolFake(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeData(w, http.StatusOK, `{"message":{"id":"s1"}}`)
	})
	exec := f.executor(t, "agent-1", func() { fires++ })

	if _, err := runTool(t, exec, band.ToolSendMessage, `{"content":"x","mentions":[{"id":"u"}]}`); err != nil {
		t.Fatalf("SendMessage error = %v, want nil", err)
	}
	if fires != 1 {
		t.Errorf("fires = %d after a send, want 1", fires)
	}

	if _, err := runTool(t, exec, band.ToolSendEvent, `{"content":"x","message_type":"task"}`); err != nil {
		t.Fatalf("SendEvent error = %v, want nil", err)
	}
	if fires != 1 {
		t.Errorf("fires = %d after an event, want 1 (events do not fire the callback)", fires)
	}
}

func TestToolEntries(t *testing.T) {
	entries := band.ToolEntries()
	if len(entries) != 7 {
		t.Fatalf("len(ToolEntries()) = %d, want 7", len(entries))
	}

	wantOrder := []string{
		band.ToolSendMessage,
		band.ToolSendEvent,
		band.ToolGetParticipants,
		band.ToolAddParticipant,
		band.ToolRemoveParticipant,
		band.ToolLookupPeers,
		band.ToolCreateChatroom,
	}
	for i, want := range wantOrder {
		e := entries[i]
		if e.Name != want {
			t.Errorf("entries[%d].Name = %q, want %q", i, e.Name, want)
		}
		if e.Type != config.ToolTypeBand {
			t.Errorf("entries[%d].Type = %q, want band", i, e.Type)
		}
		if e.Description == "" {
			t.Errorf("entries[%d].Description is empty; every tool needs one", i)
		}
		if !bandNameKnown(want) {
			t.Errorf("tool %q is not a known band tool", want)
		}
		var schema map[string]any
		if err := json.Unmarshal(e.ArgsSchema, &schema); err != nil {
			t.Errorf("entries[%d] ArgsSchema %s is not valid JSON: %v", i, e.ArgsSchema, err)
		}
	}
}

// bandNameKnown checks the names the executor dispatches on cover the
// entry set.
func bandNameKnown(name string) bool {
	switch name {
	case band.ToolSendMessage, band.ToolSendEvent, band.ToolGetParticipants,
		band.ToolAddParticipant, band.ToolRemoveParticipant,
		band.ToolLookupPeers, band.ToolCreateChatroom:
		return true
	}
	return false
}

func TestSendMessageSchemaRequiresMentionIdentifier(t *testing.T) {
	var entry config.ToolEntry
	for _, e := range band.ToolEntries() {
		if e.Name == band.ToolSendMessage {
			entry = e
		}
	}
	var schema struct {
		Properties struct {
			Mentions struct {
				MinItems int `json:"minItems"`
				Items    struct {
					AnyOf []struct {
						Required []string `json:"required"`
					} `json:"anyOf"`
				} `json:"items"`
			} `json:"mentions"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(entry.ArgsSchema, &schema); err != nil {
		t.Fatalf("decode send_message schema: %v", err)
	}
	if schema.Properties.Mentions.MinItems != 1 {
		t.Errorf("mentions minItems = %d, want 1", schema.Properties.Mentions.MinItems)
	}
	var sawID, sawHandle bool
	for _, alt := range schema.Properties.Mentions.Items.AnyOf {
		for _, required := range alt.Required {
			switch required {
			case "id":
				sawID = true
			case "handle":
				sawHandle = true
			}
		}
	}
	if !sawID || !sawHandle {
		t.Errorf("mentions anyOf = %+v, want id and handle alternatives", schema.Properties.Mentions.Items.AnyOf)
	}
}
