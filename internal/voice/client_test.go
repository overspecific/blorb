package voice

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/overspecific/blorb/internal/llm"
)

// connectVoice dials the fake and returns the connected client.
func connectVoice(t *testing.T, srv *fakeVoiceServer, cfg ClientConfig) *Client {
	t.Helper()
	cfg.URL = "ws://" + srv.addr + "/v1/ws"
	if cfg.APIKey == "" {
		cfg.APIKey = "test-key"
	}
	c, err := Connect(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Connect error = %v, want nil", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestConnectSendsSessionUpdate(t *testing.T) {
	srv := newFakeVoiceServer(t)
	volume := 70
	connectVoice(t, srv, ClientConfig{
		SystemPrompt: "Be brief.",
		Greeting:     "Hello.",
		Voice:        "james",
		Volume:       &volume,
		Tools: []llm.Tool{{
			Name:        "get_weather",
			Description: "Get the weather",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
	})

	if got := srv.authHeader(); got != "Bearer test-key" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer test-key")
	}

	msg := srv.awaitMessage(t, typeSessionUpdate)
	session := nested(t, msg, "session")
	if got := fieldString(t, session, "system_prompt"); got != "Be brief." {
		t.Errorf("system_prompt = %q, want %q", got, "Be brief.")
	}
	if got := fieldString(t, session, "greeting"); got != "Hello." {
		t.Errorf("greeting = %q, want %q", got, "Hello.")
	}

	var tools []toolDecl
	if err := json.Unmarshal(session["tools"], &tools); err != nil {
		t.Fatalf("decode tools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "get_weather" || tools[0].Type != "function" {
		t.Errorf("tools = %+v, want one function tool get_weather", tools)
	}
	if !strings.Contains(string(tools[0].Parameters), `"city"`) {
		t.Errorf("tool parameters = %s, want the city property", tools[0].Parameters)
	}

	output := nested(t, session, "output")
	if got := fieldString(t, output, "voice"); got != "james" {
		t.Errorf("output.voice = %q, want %q", got, "james")
	}
	var vol int
	if err := json.Unmarshal(output["volume"], &vol); err != nil || vol != 70 {
		t.Errorf("output.volume = %s, want 70", output["volume"])
	}
	input := nested(t, session, "input")
	format := nested(t, input, "format")
	if got := fieldString(t, format, "encoding"); got != "audio/pcm" {
		t.Errorf("input.format.encoding = %q, want %q", got, "audio/pcm")
	}
}

func TestMicStreamsInOrder(t *testing.T) {
	srv := newFakeVoiceServer(t)
	mic := bytes.NewReader([]byte("abcdef"))
	// A reader that ends after 6 bytes; each chunk will be whole because
	// io.ReadFull pads to what is available.
	connectVoice(t, srv, ClientConfig{Mic: mic})

	msgs := srv.awaitMessages(t, typeInputAudio, 1)
	var got []byte
	for _, m := range msgs {
		audio := fieldString(t, m, "audio")
		decoded, err := base64.StdEncoding.DecodeString(audio)
		if err != nil {
			t.Fatalf("decode input.audio: %v", err)
		}
		got = append(got, decoded...)
	}
	if string(got) != "abcdef" {
		t.Errorf("mic bytes = %q, want %q", got, "abcdef")
	}
}

func TestReplyAudioPlays(t *testing.T) {
	srv := newFakeVoiceServer(t)
	var mu sync.Mutex
	var played bytes.Buffer
	c := connectVoice(t, srv, ClientConfig{Playback: writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return played.Write(p)
	})})

	want := []byte("pcmbytes")
	srv.pushEvent(`{"type":"reply.audio","data":"` + base64.StdEncoding.EncodeToString(want) + `"}`)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := played.Len()
		mu.Unlock()
		if n >= len(want) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if !bytes.Equal(played.Bytes(), want) {
		t.Errorf("played = %q, want %q", played.Bytes(), want)
	}
	_ = c
}

func TestEchoGateHoldsBackMicWhileAgentSpeaks(t *testing.T) {
	// With the gate on, mic chunks captured while agent audio is playing are
	// drained but not sent. The test drives a reader that yields chunks
	// slowly: while reply.audio is queued the chunks are withheld, and once
	// the gate expires they flow again.
	srv := newFakeVoiceServer(t)
	mic := &pacedReader{fill: []byte("mic!"), interval: 10 * time.Millisecond}
	c := connectVoice(t, srv, ClientConfig{Mic: mic, EchoGate: true})

	// Queue a long chunk of agent audio: 24000 bytes is 500 ms of playback,
	// so the gate stays closed well past the next few mic reads.
	long := make([]byte, 24000)
	srv.pushEvent(`{"type":"reply.audio","data":"` + base64.StdEncoding.EncodeToString(long) + `"}`)

	// Wait until the gate has definitely engaged, then let several mic
	// chunks pass; they must be swallowed.
	waitUntil(t, 5*time.Second, func() bool { return c.gateUntil.Load() > time.Now().UnixNano() })
	time.Sleep(150 * time.Millisecond)

	// Let the gate expire, then wait for a mic chunk to arrive.
	waitUntil(t, 2*time.Second, func() bool { return time.Now().UnixNano() >= c.gateUntil.Load() })
	msg := srv.awaitMessage(t, typeInputAudio)
	decoded, err := base64.StdEncoding.DecodeString(fieldString(t, msg, "audio"))
	if err != nil {
		t.Fatalf("decode input.audio: %v", err)
	}
	if !bytes.HasPrefix(decoded, []byte("mic!")) {
		t.Errorf("mic chunk after gate = %q, want it to start with mic!", decoded)
	}
}

func TestEchoGateOffStreamsMicDuringReply(t *testing.T) {
	// With the gate off, mic audio flows even while the agent speaks.
	srv := newFakeVoiceServer(t)
	mic := bytes.NewReader([]byte("abcdef"))
	connectVoice(t, srv, ClientConfig{Mic: mic, EchoGate: false})

	long := make([]byte, 24000)
	srv.pushEvent(`{"type":"reply.audio","data":"` + base64.StdEncoding.EncodeToString(long) + `"}`)
	msgs := srv.awaitMessages(t, typeInputAudio, 1)
	var got []byte
	for _, m := range msgs {
		decoded, err := base64.StdEncoding.DecodeString(fieldString(t, m, "audio"))
		if err != nil {
			t.Fatalf("decode input.audio: %v", err)
		}
		got = append(got, decoded...)
	}
	if string(got) != "abcdef" {
		t.Errorf("mic bytes = %q, want abcdef", got)
	}
}

// pacedReader fills the read buffer with repeated copies of fill, sleeping
// interval between reads, forever.
type pacedReader struct {
	fill     []byte
	interval time.Duration
}

func (p *pacedReader) Read(b []byte) (int, error) {
	time.Sleep(p.interval)
	n := 0
	for n < len(b) {
		n += copy(b[n:], p.fill)
	}
	return n, nil
}

// waitUntil polls cond until it is true or the deadline passes.
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

func TestEventsForwarded(t *testing.T) {
	srv := newFakeVoiceServer(t)
	c := connectVoice(t, srv, ClientConfig{})

	srv.pushEvent(`{"type":"transcript.user.delta","item_id":"i1","text":"hel"}`)
	srv.pushEvent(`{"type":"transcript.user","text":"hello"}`)
	srv.pushEvent(`{"type":"transcript.agent.delta","delta":"Hi"}`)
	srv.pushEvent(`{"type":"transcript.agent","text":"Hi there"}`)

	want := []struct {
		typ  string
		text string
	}{
		{typeUserDelta, "hel"},
		{typeUserFinal, "hello"},
		{typeAgentDelta, ""}, // delta lives in Delta, checked separately
		{typeAgentFinal, "Hi there"},
	}
	got := collectEvents(t, c, 4)
	for i, w := range want {
		if got[i].Type != w.typ {
			t.Errorf("event[%d].Type = %q, want %q", i, got[i].Type, w.typ)
		}
	}
	if got[0].ItemID != "i1" || got[0].Text != "hel" {
		t.Errorf("user delta = %+v, want item i1 text hel", got[0])
	}
	if got[1].Text != "hello" {
		t.Errorf("user final text = %q, want %q", got[1].Text, "hello")
	}
	if got[2].Delta != "Hi" {
		t.Errorf("agent delta = %q, want %q", got[2].Delta, "Hi")
	}
	if got[3].Text != "Hi there" {
		t.Errorf("agent final text = %q, want %q", got[3].Text, "Hi there")
	}
}

func TestToolCallRunsAndResultsAfterReplyDone(t *testing.T) {
	srv := newFakeVoiceServer(t)

	ran := make(chan struct{}, 1)
	c := connectVoice(t, srv, ClientConfig{
		RunTool: func(ctx context.Context, name string, args json.RawMessage) (string, bool) {
			if name != "get_weather" {
				t.Errorf("tool name = %q, want get_weather", name)
			}
			ran <- struct{}{}
			return `{"temperature_c":18}`, false
		},
	})
	_ = c

	srv.pushEvent(`{"type":"tool.call","call_id":"c1","name":"get_weather","arguments":{"city":"Paris"}}`)
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("tool callback did not run")
	}
	// Nothing is sent until reply.done is the latest event.
	srv.awaitNoMessage(t, typeToolResult)

	srv.pushEvent(`{"type":"reply.done","reply_id":"r1","status":"completed"}`)

	msg := srv.awaitMessage(t, typeToolResult)
	if got := fieldString(t, msg, "call_id"); got != "c1" {
		t.Errorf("call_id = %q, want %q", got, "c1")
	}
	if got := fieldString(t, msg, "result"); got != `"{\"temperature_c\":18}"` {
		t.Errorf("result = %s, want a JSON-encoded string of the tool output", msg["result"])
	}
}

func TestToolCallWhenReplyDoneAlreadyLatest(t *testing.T) {
	// If reply.done already arrived, a tool.call sent afterwards flushes at
	// once, since lastEvent is reply.done.
	srv := newFakeVoiceServer(t)
	c := connectVoice(t, srv, ClientConfig{
		RunTool: func(ctx context.Context, name string, args json.RawMessage) (string, bool) {
			return "ok", false
		},
	})
	_ = c

	srv.pushEvent(`{"type":"reply.done","reply_id":"r1","status":"completed"}`)
	// Wait for the reply.done to be consumed before the tool.call.
	collectEvents(t, c, 1)

	srv.pushEvent(`{"type":"tool.call","call_id":"c2","name":"t","arguments":{}}`)
	msg := srv.awaitMessage(t, typeToolResult)
	if got := fieldString(t, msg, "call_id"); got != "c2" {
		t.Errorf("call_id = %q, want %q", got, "c2")
	}
}

func TestInterruptedReplyDiscardsPending(t *testing.T) {
	srv := newFakeVoiceServer(t)
	c := connectVoice(t, srv, ClientConfig{
		RunTool: func(ctx context.Context, name string, args json.RawMessage) (string, bool) {
			return "ok", false
		},
	})
	_ = c

	srv.pushEvent(`{"type":"tool.call","call_id":"c1","name":"t","arguments":{}}`)
	srv.awaitNoMessage(t, typeToolResult)

	srv.pushEvent(`{"type":"reply.done","reply_id":"r1","status":"interrupted"}`)

	// The pending result is discarded: a consumer sees the reply.done, then
	// nothing more is sent.
	collectEvents(t, c, 1)
	srv.awaitNoMessage(t, typeToolResult)
}

func TestToolErrorFlagged(t *testing.T) {
	srv := newFakeVoiceServer(t)
	c := connectVoice(t, srv, ClientConfig{
		RunTool: func(ctx context.Context, name string, args json.RawMessage) (string, bool) {
			return "tool failed", true
		},
	})
	_ = c

	srv.pushEvent(`{"type":"tool.call","call_id":"c1","name":"t","arguments":{}}`)
	srv.pushEvent(`{"type":"reply.done","reply_id":"r1","status":"completed"}`)

	msg := srv.awaitMessage(t, typeToolResult)
	var isErr bool
	if err := json.Unmarshal(msg["is_error"], &isErr); err != nil || !isErr {
		t.Errorf("is_error = %s, want true", msg["is_error"])
	}
}

func TestSessionErrorBeforeReadyFailsConnect(t *testing.T) {
	srv := newFakeVoiceServer(t)
	srv.failReady = true
	srv.failCode = "invalid_config"
	srv.failMsg = "bad voice"

	_, err := Connect(context.Background(), ClientConfig{
		URL:    "ws://" + srv.addr + "/v1/ws",
		APIKey: "k",
	})
	if err == nil {
		t.Fatal("Connect error = nil, want the session.error")
	}
	if !strings.Contains(err.Error(), "invalid_config") {
		t.Errorf("Connect error = %v, want it to name the code", err)
	}
}

func TestEndSendsSessionEnd(t *testing.T) {
	srv := newFakeVoiceServer(t)
	c := connectVoice(t, srv, ClientConfig{})

	go func() {
		// Reply to session.end only once it has been sent.
		srv.awaitMessage(t, typeSessionEnd)
		srv.pushEvent(`{"type":"session.ended","session_duration_seconds":3.5,"audio_duration_seconds":1.2}`)
	}()

	if err := c.End(); err != nil {
		t.Errorf("End error = %v, want nil", err)
	}
}

func TestEventDurationFields(t *testing.T) {
	srv := newFakeVoiceServer(t)
	c := connectVoice(t, srv, ClientConfig{})

	srv.pushEvent(`{"type":"session.ended","session_duration_seconds":3.5,"audio_duration_seconds":1.2}`)
	got := collectEvents(t, c, 1)[0]
	if got.SessionDurationSeconds == nil || *got.SessionDurationSeconds != 3.5 {
		t.Errorf("SessionDurationSeconds = %v, want 3.5", got.SessionDurationSeconds)
	}
	if got.AudioDurationSeconds == nil || *got.AudioDurationSeconds != 1.2 {
		t.Errorf("AudioDurationSeconds = %v, want 1.2", got.AudioDurationSeconds)
	}
}

func TestDecodeEventRoundTrip(t *testing.T) {
	raw := `{"type":"tool.call","call_id":"c1","name":"t","arguments":{"a":1}}`
	ev, err := decodeEvent([]byte(raw))
	if err != nil {
		t.Fatalf("decodeEvent error = %v, want nil", err)
	}
	if ev.Type != typeToolCall || ev.CallID != "c1" || ev.Name != "t" {
		t.Errorf("decoded = %+v, want the tool.call fields", ev)
	}
	if string(ev.Arguments) != `{"a":1}` {
		t.Errorf("arguments = %s, want the raw object", ev.Arguments)
	}

	if _, err := decodeEvent([]byte(`{}`)); err == nil {
		t.Error("decodeEvent error = nil for a missing type, want an error")
	}
}

// collectEvents reads n events from the client's Events channel.
func collectEvents(t *testing.T, c *Client, n int) []Event {
	t.Helper()
	var out []Event
	timeout := time.After(5 * time.Second)
	for len(out) < n {
		select {
		case ev, ok := <-c.Events():
			if !ok {
				t.Fatalf("Events closed after %d events, want %d", len(out), n)
			}
			out = append(out, ev)
		case <-timeout:
			t.Fatalf("timed out with %d of %d events", len(out), n)
		}
	}
	return out
}

// writerFunc adapts a function to io.Writer.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
