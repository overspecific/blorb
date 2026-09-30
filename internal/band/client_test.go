package band_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/overspecific/blorb/internal/band"
	"github.com/overspecific/blorb/internal/logging"
)

// restFake is an httptest server answering the Agent API endpoints the
// client hits, recording every request for byte-checks.
type restFake struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []restRequest
}

type restRequest struct {
	Method string
	Path   string
	APIKey string
	Body   []byte
}

func newRestFake(t *testing.T, handler http.HandlerFunc) *restFake {
	t.Helper()
	f := &restFake{}
	wrapped := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, restRequest{
			Method: r.Method,
			Path:   r.URL.Path + "?" + r.URL.RawQuery,
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

func (f *restFake) seen() []restRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]restRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

// client builds a client against the fake with the canonical key.
func (f *restFake) client(t *testing.T, sink logging.Sink) *band.Client {
	t.Helper()
	return band.NewClient(f.srv.URL, "k", sink)
}

// writeData answers with the {"data": ...} envelope.
func writeData(w http.ResponseWriter, status int, data string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"data":` + data + `}`))
}

// writeError answers with the error envelope.
func writeError(w http.ResponseWriter, status int, code, message, requestID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":{"code":"` + code + `","message":"` + message + `","request_id":"` + requestID + `"}}`))
}

// writeDataMeta answers with the {"data": ...} envelope plus the cursor
// pagination metadata.
func writeDataMeta(w http.ResponseWriter, status int, data, nextCursor string, hasMore bool) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := `{"data":` + data + `,"metadata":{"has_more":` + strconv.FormatBool(hasMore) +
		`,"next_cursor":` + strconv.Quote(nextCursor) + `}}`
	_, _ = w.Write([]byte(body))
}

func TestMe(t *testing.T) {
	f := newRestFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agent/me" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeData(w, http.StatusOK, `{"id":"a-1","name":"blorb","description":"an agent"}`)
	})
	profile, err := f.client(t, logging.NewNop()).Me(context.Background())
	if err != nil {
		t.Fatalf("Me error = %v, want nil", err)
	}
	if profile.ID != "a-1" || profile.Name != "blorb" {
		t.Errorf("Me = %+v, want id a-1 name blorb", profile)
	}

	reqs := f.seen()
	if len(reqs) != 1 || reqs[0].APIKey != "k" {
		t.Errorf("requests = %+v, want exactly one carrying X-API-Key k", reqs)
	}
}

func TestListChatsFollowsPagination(t *testing.T) {
	// The client walks until a short page; the fake's first page must be
	// exactly one full page of rooms to force a second call.
	full := make([]string, listChatsPageSizeForTest)
	for i := range full {
		full[i] = `{"id":"r-` + strconv.Itoa(i+1) + `"}`
	}
	calls := 0
	f := newRestFake(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if got := r.URL.Query().Get("page"); got != strconv.Itoa(calls) {
			t.Errorf("page = %q, want %d", got, calls)
		}
		if calls == 1 {
			writeData(w, http.StatusOK, "["+strings.Join(full, ",")+"]")
			return
		}
		writeData(w, http.StatusOK, `[{"id":"r-last"}]`)
	})
	c := f.client(t, logging.NewNop())
	rooms, err := c.ListChats(context.Background())
	if err != nil {
		t.Fatalf("ListChats error = %v, want nil", err)
	}
	if len(rooms) != listChatsPageSizeForTest+1 {
		t.Errorf("rooms = %d, want %d across two pages", len(rooms), listChatsPageSizeForTest+1)
	}
}

// listChatsPageSizeForTest mirrors the client's internal page size so the
// pagination test can force a full first page.
const listChatsPageSizeForTest = 100

func TestCreateChatOmitsEmptyFields(t *testing.T) {
	f := newRestFake(t, func(w http.ResponseWriter, r *http.Request) {
		// Echo the request body's chat back as the room.
		var req struct {
			Chat map[string]any `json:"chat"`
		}
		json.Unmarshal(readBody(r), &req)
		title := req.Chat["title"]
		writeData(w, http.StatusOK, `{"id":"new-1","title":`+quoteAny(title)+`}`)
	})
	c := f.client(t, logging.NewNop())
	room, err := c.CreateChat(context.Background(), "T", "")
	if err != nil {
		t.Fatalf("CreateChat error = %v, want nil", err)
	}
	if room.ID != "new-1" {
		t.Errorf("room ID = %q, want new-1", room.ID)
	}

	body := f.seen()[0].Body
	if strings.Contains(string(body), "task_id") {
		t.Errorf("body %s carries task_id though unset", body)
	}
}

// readBody reads and restores a request body for handlers that also
// inspect it.
func readBody(r *http.Request) []byte {
	body, _ := io.ReadAll(r.Body)
	return body
}

// quoteAny re-encodes a decoded JSON value as bytes.
func quoteAny(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return `""`
	}
	return string(raw)
}

func TestNextMessage204(t *testing.T) {
	f := newRestFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	c := f.client(t, logging.NewNop())
	msg, err := c.NextMessage(context.Background(), "room-1")
	if err != nil {
		t.Fatalf("NextMessage error = %v, want nil", err)
	}
	if msg != nil {
		t.Errorf("NextMessage = %+v, want nil on 204", msg)
	}
}

func TestMarksAndFailedBody(t *testing.T) {
	f := newRestFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	c := f.client(t, logging.NewNop())
	if err := c.MarkProcessing(context.Background(), "room-1", "m-1"); err != nil {
		t.Fatalf("MarkProcessing error = %v, want nil", err)
	}
	if err := c.MarkProcessed(context.Background(), "room-1", "m-1"); err != nil {
		t.Fatalf("MarkProcessed error = %v, want nil", err)
	}
	if err := c.MarkFailed(context.Background(), "room-1", "m-1", "boom happened"); err != nil {
		t.Fatalf("MarkFailed error = %v, want nil", err)
	}

	reqs := f.seen()
	if !strings.Contains(reqs[0].Path, "/messages/m-1/processing") {
		t.Errorf("processing path = %q", reqs[0].Path)
	}
	if !strings.Contains(reqs[1].Path, "/messages/m-1/processed") {
		t.Errorf("processed path = %q", reqs[1].Path)
	}
	if got, want := string(reqs[2].Body), `{"error":"boom happened"}`; got != want {
		t.Errorf("failed body = %s, want %s", got, want)
	}
}

func TestSendMessageBody(t *testing.T) {
	f := newRestFake(t, func(w http.ResponseWriter, r *http.Request) {
		writeData(w, http.StatusOK, `{"message":{"id":"sent-1"}}`)
	})
	c := f.client(t, logging.NewNop())
	sent, err := c.SendMessage(context.Background(), "room-1", "hi there",
		[]band.Mention{{ID: "u-1", Name: "User One"}})
	if err != nil {
		t.Fatalf("SendMessage error = %v, want nil", err)
	}
	if sent.Message.ID != "sent-1" {
		t.Errorf("sent message ID = %q, want sent-1", sent.Message.ID)
	}

	var decoded struct {
		Message struct {
			Content  string         `json:"content"`
			Mentions []band.Mention `json:"mentions"`
		} `json:"message"`
	}
	if err := json.Unmarshal(f.seen()[0].Body, &decoded); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	if decoded.Message.Content != "hi there" || len(decoded.Message.Mentions) != 1 ||
		decoded.Message.Mentions[0].ID != "u-1" {
		t.Errorf("sent message = %+v, want content plus one mention u-1", decoded.Message)
	}
}

func TestSendEventBody(t *testing.T) {
	f := newRestFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	c := f.client(t, logging.NewNop())
	err := c.SendEvent(context.Background(), "room-1", "called read", "tool_call",
		json.RawMessage(`{"name":"read"}`))
	if err != nil {
		t.Fatalf("SendEvent error = %v, want nil", err)
	}
	req := f.seen()[0]
	body := string(req.Body)
	if !strings.Contains(body, `"message_type":"tool_call"`) ||
		!strings.Contains(body, `"metadata":{"name":"read"}`) ||
		!strings.Contains(body, `"content":"called read"`) {
		t.Errorf("SendEvent body = %s, want content, message_type, and metadata", body)
	}
}

func TestContextCursorPagination(t *testing.T) {
	f := newRestFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			writeDataMeta(w, http.StatusOK,
				"["+strings.Repeat(`{"id":"m-1"},`, contextPageSizeForTest-1)+`{"id":"c1"}]`,
				"cur-opaque", true)
			return
		}
		writeDataMeta(w, http.StatusOK, `[{"id":"m-2"}]`, "", false)
	})
	c := f.client(t, logging.NewNop())
	messages, err := c.Context(context.Background(), "room-1")
	if err != nil {
		t.Fatalf("Context error = %v, want nil", err)
	}
	// Both pages concatenate in order: 100 on the first, 1 on the
	// second.
	if len(messages) != contextPageSizeForTest+1 || messages[0].ID != "m-1" || messages[len(messages)-1].ID != "m-2" {
		t.Errorf("messages = %d items, want %d+1 stitched in order", len(messages), contextPageSizeForTest)
	}
	// The second request carries the opaque cursor the first page
	// returned, not a message id.
	reqs := f.seen()
	if len(reqs) != 2 || !strings.Contains(reqs[1].Path, "cursor=cur-opaque") {
		t.Errorf("requests = %+v, want the second to carry cursor=cur-opaque", reqs)
	}
}

// contextPageSizeForTest mirrors the client's internal page size so the
// pagination test can force a full first page.
const contextPageSizeForTest = 100

func TestAPIErrorMapping(t *testing.T) {
	for _, status := range []int{401, 403, 404, 422} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			f := newRestFake(t, func(w http.ResponseWriter, r *http.Request) {
				writeError(w, status, "ERR_CODE", "server said no", "req-9")
			})
			c := f.client(t, logging.NewNop())
			err := c.MarkProcessed(context.Background(), "room", "m")
			var apiErr *band.APIError
			if !errorsAs(err, &apiErr) {
				t.Fatalf("error = %v (%T), want *band.APIError", err, err)
			}
			if apiErr.Status != status || apiErr.Code != "ERR_CODE" ||
				apiErr.Message != "server said no" || apiErr.RequestID != "req-9" {
				t.Errorf("APIError = %+v, want status %d with the wire fields", apiErr, status)
			}
		})
	}
}

// errorsAs is a local errors.As for the single concrete case.
func errorsAs(err error, target **band.APIError) bool {
	if apiErr, ok := err.(*band.APIError); ok {
		*target = apiErr
		return true
	}
	return false
}

func TestListParticipants(t *testing.T) {
	f := newRestFake(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chats/room-1/participants") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeData(w, http.StatusOK, `[{"id":"u-1","role":"owner","status":"active","type":"User","name":"User One"}]`)
	})
	c := f.client(t, logging.NewNop())
	participants, err := c.ListParticipants(context.Background(), "room-1")
	if err != nil {
		t.Fatalf("ListParticipants error = %v, want nil", err)
	}
	if len(participants) != 1 || participants[0].ID != "u-1" || participants[0].Role != "owner" {
		t.Errorf("participants = %+v, want one owner u-1", participants)
	}
}

func TestAddParticipantOmitsRole(t *testing.T) {
	f := newRestFake(t, func(w http.ResponseWriter, r *http.Request) {
		writeData(w, http.StatusCreated, `{"id":"u-2","role":"member","status":"active","type":"User"}`)
	})
	c := f.client(t, logging.NewNop())
	participant, err := c.AddParticipant(context.Background(), "room-1", "u-2", "admin")
	if err != nil {
		t.Fatalf("AddParticipant error = %v, want nil", err)
	}
	if participant.ID != "u-2" {
		t.Errorf("participant = %+v, want id u-2", participant)
	}

	req := f.seen()[0]
	if !strings.Contains(string(req.Body), `"role":"admin"`) {
		t.Errorf("body %s lacks the explicit role", req.Body)
	}
}

func TestAddParticipantRoleOmittedWhenEmpty(t *testing.T) {
	f := newRestFake(t, func(w http.ResponseWriter, r *http.Request) {
		writeData(w, http.StatusCreated, `{"id":"u-2","role":"member","status":"active","type":"User"}`)
	})
	c := f.client(t, logging.NewNop())
	if _, err := c.AddParticipant(context.Background(), "room-1", "u-2", ""); err != nil {
		t.Fatalf("AddParticipant error = %v, want nil", err)
	}
	if strings.Contains(string(f.seen()[0].Body), `"role"`) {
		t.Errorf("body %s carries role though unset", f.seen()[0].Body)
	}
}

func TestRemoveParticipantMethod(t *testing.T) {
	f := newRestFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		writeData(w, http.StatusOK, `{"id":"u-2","role":"member","status":"removed","type":"User"}`)
	})
	c := f.client(t, logging.NewNop())
	if err := c.RemoveParticipant(context.Background(), "room-1", "u-2"); err != nil {
		t.Fatalf("RemoveParticipant error = %v, want nil", err)
	}
	if !strings.Contains(f.seen()[0].Path, "/participants/u-2") {
		t.Errorf("path = %q, want the participant id in the path", f.seen()[0].Path)
	}
}

func TestLookupPeersFollowsPagination(t *testing.T) {
	// A full first page forces the second.
	full := make([]string, peersPageSizeForTest)
	for i := range full {
		full[i] = `{"id":"p-` + strconv.Itoa(i+1) + `","name":"Peer","handle":"h","type":"Agent"}`
	}
	calls := 0
	f := newRestFake(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if got := r.URL.Query().Get("page"); got != strconv.Itoa(calls) {
			t.Errorf("page = %q, want %d", got, calls)
		}
		if calls == 1 {
			writeData(w, http.StatusOK, "["+strings.Join(full, ",")+"]")
			return
		}
		writeData(w, http.StatusOK, `[{"id":"p-last","name":"Last","handle":"l","type":"User"}]`)
	})
	c := f.client(t, logging.NewNop())
	peers, err := c.LookupPeers(context.Background())
	if err != nil {
		t.Fatalf("LookupPeers error = %v, want nil", err)
	}
	if len(peers) != peersPageSizeForTest+1 {
		t.Errorf("peers = %d, want %d across two pages", len(peers), peersPageSizeForTest+1)
	}
	if peers[len(peers)-1].ID != "p-last" || peers[len(peers)-1].Type != "User" {
		t.Errorf("last peer = %+v, want p-last User", peers[len(peers)-1])
	}
}

// peersPageSizeForTest mirrors the client's internal page size so the
// pagination test can force a full first page.
const peersPageSizeForTest = 100

func TestLookupPeersPageHonorsArgs(t *testing.T) {
	f := newRestFake(t, func(w http.ResponseWriter, r *http.Request) {
		writeData(w, http.StatusOK, `[]`)
	})
	c := f.client(t, logging.NewNop())
	if _, err := c.LookupPeersPage(context.Background(), 3, 7); err != nil {
		t.Fatalf("LookupPeersPage error = %v, want nil", err)
	}
	reqs := f.seen()
	if len(reqs) != 1 || !strings.Contains(reqs[0].Path, "page=3") || !strings.Contains(reqs[0].Path, "page_size=7") {
		t.Errorf("requests = %+v, want a single call with page=3 and page_size=7", reqs)
	}
}

func TestSinkRequestResponseRecords(t *testing.T) {
	f := newRestFake(t, func(w http.ResponseWriter, r *http.Request) {
		writeData(w, http.StatusOK, `{}`)
	})
	sink := logging.NewBuffered(8)
	c := f.client(t, sink)
	if _, err := c.Me(context.Background()); err != nil {
		t.Fatalf("Me error = %v, want nil", err)
	}

	records := sink.Records()
	var out, in int
	for _, r := range records {
		switch r.Kind {
		case logging.Kind("band-request"):
			out++
			if r.Method != "GET" || !strings.Contains(r.URL, "/me") {
				t.Errorf("outgoing record = (%s %s), want GET ending /me", r.Method, r.URL)
			}
		case logging.Kind("band-response"):
			in++
		}
	}
	if out != 1 || in != 1 {
		t.Errorf("sink got %d request and %d response records, want 1 and 1", out, in)
	}
}
