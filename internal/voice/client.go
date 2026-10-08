package voice

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/logging"
	"github.com/overspecific/blorb/internal/ws"
)

// micChunkBytes is how much PCM one input.audio message carries: 2400 bytes
// is 50 ms at 24 kHz 16-bit mono, within the API's one-second and real-time
// limits.
const micChunkBytes = 2400

// endTimeout bounds how long End waits for the server's session.ended before
// closing the socket anyway.
const endTimeout = 5 * time.Second

// ErrSessionClosed is the terminal error a client reports when it closed
// deliberately (End, Close, or a clean session.ended) rather than failing.
var ErrSessionClosed = errors.New("voice session closed")

// ClientConfig is everything Connect needs to open and drive one voice
// session.
type ClientConfig struct {
	// URL is the Voice Agent WebSocket endpoint.
	URL string
	// APIKey authenticates the handshake.
	APIKey string
	// SystemPrompt, Greeting, Voice and Volume configure the session;
	// empty Voice and nil Volume mean the server defaults.
	SystemPrompt string
	Greeting     string
	Voice        string
	Volume       *int
	// Tools are the client-side tools declared in session.tools.
	Tools []llm.Tool

	// Mic is the source of 24 kHz 16-bit mono PCM to stream up. Nil means no
	// microphone: the session is agent-talk-only.
	Mic io.Reader
	// Playback receives the agent's PCM. Nil discards it.
	Playback io.Writer
	// EchoGate, when true, holds back mic audio while the agent is
	// speaking, so a speaker near the microphone cannot feed the agent's
	// own voice back to it. Barge-in is unavailable while it is set.
	EchoGate bool

	// RunTool runs one tool call. err reports whether the tool itself
	// failed (the result is still returned to the server, flagged).
	RunTool func(ctx context.Context, name string, args json.RawMessage) (output string, err bool)

	// Sink receives best-effort wire records. Nil means no logging.
	Sink logging.Sink
}

// Client is one live Voice Agent WebSocket session. One goroutine reads and
// owns reply.audio and tool.call; sends serialize on the connection. After a
// protocol error, dead socket, or clean session.ended the Events channel
// closes and Done carries the terminal error.
type Client struct {
	conn *ws.Conn

	events chan Event
	done   chan error
	closed chan struct{}
	ended  chan struct{}
	once   sync.Once

	ctx    context.Context
	cancel context.CancelFunc

	mic      io.Reader
	playback io.Writer
	runTool  func(ctx context.Context, name string, args json.RawMessage) (output string, err bool)
	sink     logging.Sink
	echoGate bool

	// gateUntil is the Unix-nanosecond time until which the echo gate holds
	// back mic audio; a past value means open. The read loop advances it,
	// the mic loop reads it. See advanceGate.
	gateUntil atomic.Int64

	// tool timing state, owned by the read loop.
	lastEvent string
	pending   []pendingResult
}

// pcmBytesPerSecond is the byte rate of the 24 kHz 16-bit mono PCM the
// session exchanges (24000 samples/s * 2 bytes/sample).
const pcmBytesPerSecond = 24000 * 2

// pendingResult is one completed tool call waiting to be sent.
type pendingResult struct {
	callID  string
	result  string
	isError bool
}

// Connect dials the Voice Agent endpoint, sends session.update, waits for
// session.ready, and starts the read and microphone loops. A session.error
// before ready is returned here with its code.
func Connect(ctx context.Context, cfg ClientConfig) (*Client, error) {
	sink := cfg.Sink
	if sink == nil {
		sink = logging.NewNop()
	}
	playback := cfg.Playback
	if playback == nil {
		playback = io.Discard
	}

	conn, err := ws.Dial(ctx, cfg.URL, ws.WithHeader("Authorization", "Bearer "+cfg.APIKey))
	if err != nil {
		return nil, fmt.Errorf("dial voice websocket: %w", err)
	}

	cctx, cancel := context.WithCancel(ctx)
	c := &Client{
		conn:     conn,
		events:   make(chan Event, 64),
		done:     make(chan error, 1),
		closed:   make(chan struct{}),
		ended:    make(chan struct{}),
		ctx:      cctx,
		cancel:   cancel,
		mic:      cfg.Mic,
		playback: playback,
		runTool:  cfg.RunTool,
		sink:     sink,
		echoGate: cfg.EchoGate,
	}

	update := sessionUpdate{
		Type: typeSessionUpdate,
		Session: sessionConfig{
			SystemPrompt: cfg.SystemPrompt,
			Greeting:     cfg.Greeting,
			Tools:        toolDecls(cfg.Tools),
			Input:        audioConfig{Format: audioFormat{Encoding: audioPCM}},
			Output: audioOutput{
				Voice:  cfg.Voice,
				Format: audioFormat{Encoding: audioPCM},
				Volume: cfg.Volume,
			},
		},
	}
	if err := c.send(update); err != nil {
		cancel()
		conn.Close(1000, "")
		return nil, err
	}

	if err := c.awaitReady(); err != nil {
		cancel()
		conn.Close(1000, "")
		return nil, err
	}

	go c.readLoop()
	if c.mic != nil {
		go c.micLoop()
	}
	return c, nil
}

// awaitReady reads until session.ready, returning early on session.error.
func (c *Client) awaitReady() error {
	for {
		_, data, err := c.conn.ReadMessage(c.ctx)
		if err != nil {
			return fmt.Errorf("read voice handshake: %w", err)
		}
		c.logWire("voice-response", data)
		ev, err := decodeEvent(data)
		if err != nil {
			return err
		}
		switch ev.Type {
		case typeSessionReady:
			return nil
		case typeSessionError:
			return fmt.Errorf("voice session error %s: %s", ev.Code, ev.Message)
		}
	}
}

// Events delivers the transcript and lifecycle events the console renderer
// consumes. reply.audio and tool.call are handled by the client and are not
// delivered here.
func (c *Client) Events() <-chan Event { return c.events }

// Done delivers the terminal error after the session ends, then the Events
// channel closes.
func (c *Client) Done() <-chan error { return c.done }

// send encodes and writes one client message, logging it.
func (c *Client) send(msg any) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encode voice message: %w", err)
	}
	c.logWire("voice-request", body)
	if err := c.conn.WriteText(c.ctx, body); err != nil {
		c.shutdown(fmt.Errorf("write voice message: %w", err))
		return err
	}
	return nil
}

// micLoop streams microphone PCM up as input.audio until the reader ends, the
// context is cancelled, or the socket dies. With the echo gate on, chunks
// captured while the agent's audio is still playing are drained but not sent,
// so the speaker output cannot feed back into the conversation.
func (c *Client) micLoop() {
	buf := make([]byte, micChunkBytes)
	for {
		n, err := io.ReadFull(c.mic, buf)
		if n > 0 && (!c.echoGate || time.Now().UnixNano() >= c.gateUntil.Load()) {
			if sendErr := c.send(inputAudio{
				Type:  typeInputAudio,
				Audio: base64.StdEncoding.EncodeToString(buf[:n]),
			}); sendErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// advanceGate extends the echo gate to cover a chunk of agent audio that was
// just queued for playback. Audio plays in real time, so the gate closes for
// the chunk's duration from the later of now and the current gate deadline;
// that accumulates the playback finish time even when chunks arrive in a
// burst ahead of real time.
func (c *Client) advanceGate(pcmLen int) {
	if !c.echoGate || pcmLen == 0 {
		return
	}
	dur := int64(float64(pcmLen) / pcmBytesPerSecond * float64(time.Second))
	now := time.Now().UnixNano()
	for {
		cur := c.gateUntil.Load()
		base := now
		if cur > base {
			base = cur
		}
		if c.gateUntil.CompareAndSwap(cur, base+dur) {
			return
		}
	}
}

// readLoop decodes server events until the connection dies. It owns the
// Events channel and reply.audio/tool.call handling.
func (c *Client) readLoop() {
	defer close(c.events)
	for {
		_, data, err := c.conn.ReadMessage(c.ctx)
		if err != nil {
			c.shutdown(fmt.Errorf("read voice event: %w", err))
			return
		}
		c.logWire("voice-response", data)

		ev, err := decodeEvent(data)
		if err != nil {
			c.shutdown(err)
			return
		}
		if err := c.handleEvent(ev); err != nil {
			c.shutdown(err)
			return
		}
	}
}

// handleEvent processes one decoded event: reply.audio and tool.call are the
// client's to act on, the rest are forwarded to Events.
func (c *Client) handleEvent(ev Event) error {
	switch ev.Type {
	case typeReplyAudio:
		pcm, err := base64.StdEncoding.DecodeString(ev.Data)
		if err != nil {
			return fmt.Errorf("decode reply audio: %w", err)
		}
		if _, err := c.playback.Write(pcm); err != nil {
			return fmt.Errorf("play reply audio: %w", err)
		}
		c.advanceGate(len(pcm))
		return nil
	case typeToolCall:
		return c.handleToolCall(ev)
	case typeReplyStarted, typeSpeechStarted:
		// A turn is in flight: a tool.call during it must wait.
		c.lastEvent = ev.Type
		return c.forward(ev)
	case typeReplyDone:
		c.lastEvent = ev.Type
		if ev.Status == "interrupted" {
			c.pending = nil
		} else if err := c.flushPending(); err != nil {
			return err
		}
		return c.forward(ev)
	case typeSessionEnded:
		// Enqueue the event before signalling End's waiter, so a
		// concurrent End cannot shut the socket down before the renderer
		// has seen the session totals.
		err := c.forward(ev)
		close(c.ended)
		return err
	default:
		return c.forward(ev)
	}
}

// handleToolCall runs the requested tool and records its result. The result is
// held until reply.done is the latest event, per the client-side tools
// contract.
func (c *Client) handleToolCall(ev Event) error {
	var output string
	var isError bool
	if c.runTool != nil {
		output, isError = c.runTool(c.ctx, ev.Name, ev.Arguments)
	} else {
		output = fmt.Sprintf("no tool runner is configured (tool %q)", ev.Name)
		isError = true
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return fmt.Errorf("encode tool result: %w", err)
	}
	c.pending = append(c.pending, pendingResult{callID: ev.CallID, result: string(encoded), isError: isError})
	if c.lastEvent == typeReplyDone {
		if err := c.flushPending(); err != nil {
			return err
		}
	}
	return nil
}

// flushPending sends every held tool result, oldest first, and clears the
// queue.
func (c *Client) flushPending() error {
	for _, p := range c.pending {
		if err := c.send(toolResult{
			Type:    typeToolResult,
			CallID:  p.callID,
			Result:  p.result,
			IsError: p.isError,
		}); err != nil {
			return err
		}
	}
	c.pending = nil
	return nil
}

// forward delivers one event to Events without blocking the read loop
// forever: the channel is bounded, and a consumer that has stopped watching
// means the session is over anyway.
func (c *Client) forward(ev Event) error {
	select {
	case c.events <- ev:
	case <-c.closed:
	}
	return nil
}

// End sends session.end, waits up to endTimeout for session.ended, then
// closes the socket. It is the intentional hang-up, stopping billing at once.
func (c *Client) End() error {
	select {
	case <-c.closed:
		return nil
	default:
	}
	if err := c.send(sessionEnd{Type: typeSessionEnd}); err != nil {
		return err
	}
	select {
	case <-c.ended:
	case <-c.closed:
	case <-time.After(endTimeout):
	}
	c.shutdown(nil)
	return c.conn.Close(1000, "")
}

// Close closes the socket without the session.end handshake. It is the
// failure path and is safe to call more than once.
func (c *Client) Close() error {
	c.shutdown(nil)
	return c.conn.Close(1000, "")
}

// logWire writes one best-effort record to the sink.
func (c *Client) logWire(kind string, body []byte) {
	_ = c.sink.Write(logging.Record{
		Time:   time.Now(),
		Kind:   logging.Kind(kind),
		Method: "websocket",
		URL:    "voice",
		Body:   body,
	})
}

// shutdown records the terminal error once and signals every waiter. A nil err
// means a deliberate close, not a failure.
func (c *Client) shutdown(err error) {
	c.once.Do(func() {
		if err == nil {
			err = ErrSessionClosed
		}
		c.cancel()
		c.done <- err
		close(c.closed)
	})
}
