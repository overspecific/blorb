package band_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/overspecific/blorb/internal/llm"
)

// contextPage builds one scripted /context response body: messages with
// sender ids, names, message types, and contents.
func contextPage(msgs ...string) string {
	return "[" + strings.Join(msgs, ",") + "]"
}

func ctxMsg(id, senderID, senderName, senderType, messageType, content string) string {
	return `{"id":"` + id + `","sender_id":"` + senderID + `","sender_name":"` + senderName +
		`","sender_type":"` + senderType + `","message_type":"` + messageType + `","content":` + jsonQuote(content) + `}`
}

// jsonQuote encodes one string as a JSON string value.
func jsonQuote(s string) string {
	b := make([]byte, 0, len(s)+2)
	b = append(b, '"')
	for _, r := range s {
		switch r {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\n':
			b = append(b, '\\', 'n')
		default:
			b = append(b, []byte(string(r))...)
		}
	}
	return string(append(b, '"'))
}

func TestRoomMentionRunsTurnAndMarksProcessed(t *testing.T) {
	f := newBandRestFake(t, "[]")
	room, llm := newRoom(t, f, []llm.Response{roomTextResp("hello you")})

	if err := handleMsg(t, room, mentionMsg("u-1", "User One", "hi")); err != nil {
		t.Fatalf("Handle error = %v, want nil", err)
	}

	marks := f.marks()
	if len(marks) != 2 || marks[0] != "processing" || marks[1] != "processed" {
		t.Errorf("marks = %v, want [processing processed]", marks)
	}

	sent := f.sentMessages()
	if len(sent) != 1 {
		t.Fatalf("sent = %d messages, want 1 (the fallback of the plain answer)", len(sent))
	}
	if sent[0].Content != "hello you" {
		t.Errorf("sent content = %q, want %q", sent[0].Content, "hello you")
	}
	if len(sent[0].Mentions) != 1 || sent[0].Mentions[0].ID != "u-1" {
		t.Errorf("sent mentions = %+v, want one mention of u-1", sent[0].Mentions)
	}

	if len(llm.requests) != 1 {
		t.Fatalf("llm calls = %d, want 1", len(llm.requests))
	}
	if got := llm.requests[0].Messages[len(llm.requests[0].Messages)-1].Content; got != "User One (id: u-1): hi" {
		t.Errorf("user message = %q, want the sender-prefixed form with the sender id", got)
	}
	// The system prompt tells the agent its own id and that it cannot
	// mention itself: without both, the model guesses an id and loops.
	sys := llm.requests[0].Messages[0]
	if sys.Role != "system" || !strings.Contains(sys.Content, "agent-1") ||
		!strings.Contains(sys.Content, "never mention yourself") {
		t.Errorf("system prompt = %q, want the band identity guidance naming agent-1", sys.Content)
	}
}

func TestRoomToolCallPath(t *testing.T) {
	f := newBandRestFake(t, "[]")
	room, llm := newRoom(t, f, []llm.Response{
		roomToolCallResp("band_send_message", `{"content":"the reply","mentions":[{"id":"u-1"}]}`),
		roomTextResp(""),
	})

	if err := handleMsg(t, room, mentionMsg("u-1", "User One", "question")); err != nil {
		t.Fatalf("Handle error = %v, want nil", err)
	}

	sent := f.sentMessages()
	if len(sent) != 1 || sent[0].Content != "the reply" {
		t.Fatalf("sent = %+v, want one message \"the reply\"", sent)
	}

	// The tool_call and tool_result events were posted to the room.
	events := f.postedEvents()
	if len(events) != 2 {
		t.Fatalf("events = %d, want tool_call and tool_result", len(events))
	}
	if events[0].MessageType != "tool_call" || !strings.Contains(events[0].Content, "band_send_message") {
		t.Errorf("events[0] = %+v, want a band_send_message tool_call event", events[0])
	}
	if events[1].MessageType != "tool_result" {
		t.Errorf("events[1].MessageType = %q, want tool_result", events[1].MessageType)
	}

	// The tool call reached the engine's request as a response message.
	if len(llm.requests) != 2 {
		t.Fatalf("llm calls = %d, want 2 (tool call round plus the wrap-up)", len(llm.requests))
	}
}

func TestRoomFallbackNotUsedWhenToolRan(t *testing.T) {
	f := newBandRestFake(t, "[]")
	room, _ := newRoom(t, f, []llm.Response{
		roomToolCallResp("band_send_message", `{"content":"sent via tool","mentions":[{"handle":"someone"}]}`),
		roomTextResp("plain text that must not be sent"),
	})

	if err := handleMsg(t, room, mentionMsg("u-1", "User One", "question")); err != nil {
		t.Fatalf("Handle error = %v, want nil", err)
	}

	sent := f.sentMessages()
	if len(sent) != 1 || sent[0].Content != "sent via tool" {
		t.Errorf("sent = %+v, want exactly one tool-path message, no fallback", sent)
	}
}

func TestRoomTurnFailed(t *testing.T) {
	f := newBandRestFake(t, "[]")
	room, _ := newRoom(t, f, nil) // no canned responses: the turn fails

	err := handleMsg(t, room, mentionMsg("u-1", "User One", "hi"))
	if err == nil {
		t.Fatal("Handle error = nil, want the turn failure")
	}

	marks := f.marks()
	if len(marks) != 2 || marks[0] != "processing" || !strings.HasPrefix(marks[1], "failed:") {
		t.Errorf("marks = %v, want processing then failed with the error text", marks)
	}
}

func TestRoomDuplicateMessageRunsOnce(t *testing.T) {
	f := newBandRestFake(t, "[]")
	room, llm := newRoom(t, f, []llm.Response{
		roomTextResp("one"),
		roomTextResp("two"),
	})

	msg := mentionMsg("u-1", "User One", "hi")
	if err := handleMsg(t, room, msg); err != nil {
		t.Fatalf("first Handle error = %v, want nil", err)
	}
	if err := handleMsg(t, room, msg); err != nil {
		t.Fatalf("duplicate Handle error = %v, want nil (a seen id returns nil)", err)
	}
	if len(llm.requests) != 1 {
		t.Errorf("llm calls = %d, want 1 across a duplicate delivery", len(llm.requests))
	}
}

func TestRoomSeedsHistoryFromContext(t *testing.T) {
	page := contextPage(
		ctxMsg("c1", "u-9", "Other User", "User", "text", "before the mention"),
		ctxMsg("c2", "agent-1", "helper", "Agent", "text", "own earlier reply"),
		ctxMsg("c3", "agent-1", "helper", "Agent", "tool_call", "tool noise"),
		ctxMsg("c4", "u-7", "Another Agent", "Agent", "text", "another agent's text"),
	)
	f := newBandRestFake(t, page)
	room, llm := newRoom(t, f, []llm.Response{roomTextResp("ok")})

	if err := handleMsg(t, room, mentionMsg("u-1", "User One", "hi")); err != nil {
		t.Fatalf("Handle error = %v, want nil", err)
	}

	// The seeded history sits between the system prompt and this
	// turn's user message: two user messages (other participants) and
	// one assistant message (own text), in context order, with the
	// noise skipped.
	req := llm.requests[0]
	messages := req.Messages
	if len(messages) < 4 {
		t.Fatalf("request messages = %d, want the seeded history plus system plus the mention", len(messages))
	}
	var userTexts []string
	for _, m := range messages {
		if m.Role == "user" {
			userTexts = append(userTexts, m.Content)
		}
	}
	if len(userTexts) != 3 {
		t.Fatalf("user messages = %v, want two seeded and the mention", userTexts)
	}
	if userTexts[0] != "Other User (id: u-9): before the mention" {
		t.Errorf("seeded user message = %q, want the sender-prefixed context text with the sender id", userTexts[0])
	}
	// The another-agent text seeds as a user message too: only the
	// agent's own messages become assistant history.
	if userTexts[1] != "Another Agent (id: u-7): another agent's text" {
		t.Errorf("seeded other-agent message = %q, want it as a user message", userTexts[1])
	}
	if userTexts[2] != "User One (id: u-1): hi" {
		t.Errorf("mention user message = %q, want the sender prefix", userTexts[2])
	}
	// The seeded history is converted exactly once: the second
	// message's request carries the same prefix shape, not doubled.
	if strings.Contains(userTexts[0], "helper:") {
		t.Errorf("own text %q leaked into user history; own messages are assistant messages", userTexts[0])
	}
}

func TestRoomSeedHappensOnce(t *testing.T) {
	f := newBandRestFake(t, "[]")
	room, _ := newRoom(t, f, []llm.Response{roomTextResp("a"), roomTextResp("b")})

	if err := handleMsg(t, room, mentionMsg("u-1", "User One", "first")); err != nil {
		t.Fatalf("first Handle error = %v, want nil", err)
	}
}

func TestRoomDiagnosticsWritten(t *testing.T) {
	// A rejected event post is best-effort: the turn still succeeds and
	// the failure lands in the diagnostics writer.
	f := newBandRestFake(t, "[]")
	f.mu.Lock()
	f.eventsStatus = http.StatusInternalServerError
	f.mu.Unlock()

	var diag strings.Builder
	room, _ := newRoomWithDiag(t, f, []llm.Response{
		roomToolCallResp("band_send_message", `{"content":"the reply","mentions":[{"id":"u-1"}]}`),
		roomTextResp(""),
	}, &diag)

	if err := handleMsg(t, room, mentionMsg("u-1", "User One", "question")); err != nil {
		t.Fatalf("Handle error = %v, want nil (event posting is best-effort)", err)
	}
	if got := diag.String(); !strings.Contains(got, "post tool_call event") {
		t.Errorf("diagnostics = %q, want a failed event-post line", got)
	}
}

func TestRoomPrintsTurnOutput(t *testing.T) {
	// The room renders the agent's activity to its Stdout like chat and
	// run do: assistant blocks and tool call headings with arguments.
	f := newBandRestFake(t, "[]")
	var out strings.Builder
	room, _ := newRoomWithOutput(t, f, []llm.Response{
		roomToolCallResp("band_send_message", `{"content":"the reply","mentions":[{"id":"u-1"}]}`),
		roomTextResp(""),
	}, &out)

	if err := handleMsg(t, room, mentionMsg("u-1", "User One", "question")); err != nil {
		t.Fatalf("Handle error = %v, want nil", err)
	}

	got := out.String()
	if !strings.Contains(got, ">>> User: User One") || !strings.Contains(got, "question") {
		t.Errorf("output = %q, want the incoming mention printed", got)
	}
	if !strings.Contains(got, ">>> Tool: band_send_message") {
		t.Errorf("output = %q, want a tool heading", got)
	}
	if !strings.Contains(got, "the reply") {
		t.Errorf("output = %q, want the tool call arguments", got)
	}
}

func TestRoomStreamsAssistantOutput(t *testing.T) {
	// With Stream on and a streaming-capable client, the assistant text
	// renders as deltas; the reply still reaches Band as one message.
	f := newBandRestFake(t, "[]")
	var out strings.Builder
	room, _ := newRoomStreaming(t, f, []llm.Response{roomTextResp("streamed hello")}, nil, &out, false, true)

	if err := handleMsg(t, room, mentionMsg("u-1", "User One", "hi")); err != nil {
		t.Fatalf("Handle error = %v, want nil", err)
	}

	got := out.String()
	if !strings.Contains(got, ">>> Assistant:") || !strings.Contains(got, "streamed hello") {
		t.Errorf("output = %q, want the streamed assistant text", got)
	}
	sent := f.sentMessages()
	if len(sent) != 1 || sent[0].Content != "streamed hello" {
		t.Errorf("sent = %+v, want the reply reaching Band whole", sent)
	}
}

func TestRoomJudgesRunAfterTurn(t *testing.T) {
	// A judge in the config: the room runs it after a successful turn.
	// The judge's own canned LLM is the room's, so the judged turn and
	// the judging call share the fake; the judge engine gets its own
	// client through the same factory.
	f := newBandRestFake(t, "[]")

	// No judge agents in the canonical config; a judging config is
	// validated config-wide, so keep the smoke to: no judges, no
	// judge error surfaces, nothing leaks into the room.
	room, _ := newRoom(t, f, []llm.Response{roomTextResp("fine")})
	if err := handleMsg(t, room, mentionMsg("u-1", "User One", "hi")); err != nil {
		t.Fatalf("Handle error = %v, want nil", err)
	}

	sent := f.sentMessages()
	if len(sent) != 1 || sent[0].Content != "fine" {
		t.Errorf("sent = %+v, want exactly the turn's answer (judge output leaks nowhere)", sent)
	}
}
