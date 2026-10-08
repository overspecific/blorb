package voice

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/llm"
)

// syncBuffer is a goroutine-safe bytes.Buffer for capturing session output.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor polls until the buffer contains want or the deadline passes.
func (b *syncBuffer) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(b.String(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("output never contained %q; got:\n%s", want, b.String())
}

// fakeClient is an injected SessionClient the session tests script directly.
type fakeClient struct {
	events chan Event
	done   chan error
	once   sync.Once

	connectedOnce sync.Once
	connected     chan struct{}

	mu         sync.Mutex
	endCalled  bool
	closeCalls int
	cfg        ClientConfig

	// endBlock, when non-nil, makes End wait for it before finishing,
	// modelling the real client's wait for the server's session.ended.
	endBlock chan struct{}
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		events:    make(chan Event, 16),
		done:      make(chan error, 1),
		connected: make(chan struct{}),
	}
}

func (f *fakeClient) Events() <-chan Event { return f.events }
func (f *fakeClient) Done() <-chan error   { return f.done }

// markConnected records the ClientConfig Run handed the client and signals
// waitConnected.
func (f *fakeClient) markConnected(cfg ClientConfig) {
	f.mu.Lock()
	f.cfg = cfg
	f.mu.Unlock()
	f.connectedOnce.Do(func() { close(f.connected) })
}

// waitConnected blocks until Run has connected the fake.
func (f *fakeClient) waitConnected(t *testing.T) {
	t.Helper()
	select {
	case <-f.connected:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the session to connect the client")
	}
}

// End records the hang-up and finishes the session cleanly, waiting on
// endBlock first when set.
func (f *fakeClient) End() error {
	f.mu.Lock()
	f.endCalled = true
	block := f.endBlock
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-f.done:
		}
	}
	f.finish(nil)
	return nil
}

func (f *fakeClient) Close() error {
	f.mu.Lock()
	f.closeCalls++
	block := f.endBlock
	f.mu.Unlock()
	// A force-close unblocks a waiting End.
	if block != nil {
		select {
		case <-block:
		default:
			close(block)
		}
	}
	f.finish(nil)
	return nil
}

func (f *fakeClient) push(ev Event) { f.events <- ev }

// finish closes the events channel and delivers the terminal error.
func (f *fakeClient) finish(err error) {
	f.once.Do(func() {
		close(f.events)
		if err == nil {
			err = ErrSessionClosed
		}
		f.done <- err
	})
}

// voiceConfig returns a config with one agent carrying a command tool and a
// voice block whose playback command is harmless in tests.
func voiceConfig(tools ...config.ToolEntry) config.Config {
	return config.Config{
		Providers: []config.Provider{{Name: "p", Type: config.ProviderTypeOpenAI, BaseURL: "https://example.com/v1"}},
		Models:    []config.Model{{Name: "m", Provider: "p", ModelName: "m"}},
		Tools:     tools,
		Agents: []config.Agent{{
			Name:         "assistant",
			SystemPrompt: "Be brief.",
			Model:        "m",
			MaxTurns:     3,
			Tools:        []string{"echoer"},
			Voice: &config.VoiceConfig{
				APIKeyEnv:     "ASSEMBLYAI_API_KEY",
				Greeting:      "Hello.",
				OutputCommand: []string{"cat"},
			},
		}},
	}
}

func echoTool() config.ToolEntry {
	return config.ToolEntry{
		Type:        config.ToolTypeCommand,
		Name:        "echoer",
		Description: "Echo a fixed string",
		Command:     []string{"echo", "hi"},
	}
}

// startSession runs a session against a fake client in a goroutine and
// returns the fake and captured output.
func startSession(t *testing.T, cfg config.Config, opts Options) (*fakeClient, *syncBuffer, chan error) {
	t.Helper()
	fake := newFakeClient()
	buf := &syncBuffer{}
	if opts.Stdout == nil {
		opts.Stdout = buf
	}
	if opts.NewSessionClient == nil {
		opts.NewSessionClient = func(_ context.Context, c ClientConfig) (SessionClient, error) {
			fake.markConnected(c)
			return fake, nil
		}
	}
	opts.Config = cfg
	if opts.Agent.Name == "" {
		opts.Agent = cfg.Agents[0]
	}
	opts.NoMic = true

	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), opts) }()
	return fake, buf, done
}

func TestSessionRendersUserAndAgent(t *testing.T) {
	fake, buf, done := startSession(t, voiceConfig(echoTool()), Options{Version: "test"})
	fake.waitConnected(t)

	fake.push(Event{Type: typeUserDelta, ItemID: "i1", Text: "hel"})
	fake.push(Event{Type: typeUserDelta, ItemID: "i1", Text: "hello"})
	fake.push(Event{Type: typeUserFinal, Text: "hello"})
	fake.push(Event{Type: typeAgentDelta, Delta: "Hi"})
	fake.push(Event{Type: typeAgentDelta, Delta: " there"})
	fake.push(Event{Type: typeAgentFinal, Text: "Hi there"})
	fake.push(Event{Type: typeSessionEnded, SessionDurationSeconds: floatPtr(3.0)})
	fake.finish(nil)

	if err := <-done; err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	out := buf.String()
	if !strings.Contains(out, "blorb test (assistant, voice session)") {
		t.Errorf("missing banner in output:\n%s", out)
	}
	if !strings.Contains(out, ">>> User:") || !strings.Contains(out, "\r\x1b[Khello\n") {
		t.Errorf("missing final user block in output:\n%s", out)
	}
	if !strings.Contains(out, ">>> Assistant:\nHi there\n") {
		t.Errorf("missing agent block in output:\n%s", out)
	}
	if !strings.Contains(out, "Session ended (3.0s).") {
		t.Errorf("missing session duration in output:\n%s", out)
	}
}

func TestSessionInterruptedMarker(t *testing.T) {
	fake, buf, done := startSession(t, voiceConfig(echoTool()), Options{})
	fake.waitConnected(t)

	fake.push(Event{Type: typeAgentDelta, Delta: "I was say"})
	fake.push(Event{Type: typeReplyDone, Status: "interrupted"})
	fake.push(Event{Type: typeSessionEnded})
	fake.finish(nil)

	if err := <-done; err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if out := buf.String(); !strings.Contains(out, ">>> Assistant:") || !strings.Contains(out, "[interrupted]") {
		t.Errorf("missing interrupt marker in output:\n%s", out)
	}
}

func TestSessionWholeAgentReply(t *testing.T) {
	// A reply delivered with no deltas (the whole text arrives at once)
	// still renders on one Agent line.
	fake, buf, done := startSession(t, voiceConfig(echoTool()), Options{})
	fake.waitConnected(t)

	fake.push(Event{Type: typeAgentFinal, Text: "whole reply"})
	fake.push(Event{Type: typeReplyDone, Status: "completed"})
	fake.push(Event{Type: typeSessionEnded})
	fake.finish(nil)

	if err := <-done; err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if out := buf.String(); !strings.Contains(out, ">>> Assistant:\nwhole reply\n") {
		t.Errorf("missing whole agent reply in output:\n%s", out)
	}
}

func TestSessionInterruptedFinalKeepsLineForMarker(t *testing.T) {
	// transcript.agent (interrupted) carries the trimmed text and arrives
	// before reply.done; the marker must land on the same line.
	fake, buf, done := startSession(t, voiceConfig(echoTool()), Options{})
	fake.waitConnected(t)

	fake.push(Event{Type: typeAgentFinal, Text: "I was say", Interrupted: true})
	fake.push(Event{Type: typeReplyDone, Status: "interrupted"})
	fake.push(Event{Type: typeSessionEnded})
	fake.finish(nil)

	if err := <-done; err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	out := buf.String()
	if !strings.Contains(out, ">>> Assistant:\nI was say [interrupted]\n") {
		t.Errorf("missing interrupted final line in output:\n%s", out)
	}
}

func TestSessionAgentDeltasJoinWords(t *testing.T) {
	// The server's agent deltas are inconsistently spaced: replies usually
	// carry a trailing space ("I ", "am "), but the greeting's words arrive
	// bare ("Hi!", "I", "am"). Both must render as readable words.
	fake, buf, done := startSession(t, voiceConfig(echoTool()), Options{})
	fake.waitConnected(t)

	for _, w := range []string{"Hi!", "I", "am", "ready", "when", "you", "are."} {
		fake.push(Event{Type: typeAgentDelta, Delta: w})
	}
	fake.push(Event{Type: typeAgentFinal, Text: "Hi! I am ready when you are."})
	fake.push(Event{Type: typeReplyDone, Status: "completed"})
	fake.push(Event{Type: typeSessionEnded})
	fake.finish(nil)

	if err := <-done; err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if out := buf.String(); !strings.Contains(out, ">>> Assistant:\nHi! I am ready when you are.\n") {
		t.Errorf("bare word deltas did not join with spaces:\n%s", out)
	}
}

func TestSessionAgentDeltasKeepServerSpacing(t *testing.T) {
	// Space-terminated deltas must not gain an extra space.
	fake, buf, done := startSession(t, voiceConfig(echoTool()), Options{})
	fake.waitConnected(t)

	for _, w := range []string{"I ", "am ", "not ", "sure."} {
		fake.push(Event{Type: typeAgentDelta, Delta: w})
	}
	fake.push(Event{Type: typeAgentFinal, Text: "I am not sure."})
	fake.push(Event{Type: typeReplyDone, Status: "completed"})
	fake.push(Event{Type: typeSessionEnded})
	fake.finish(nil)

	if err := <-done; err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	out := buf.String()
	if !strings.Contains(out, ">>> Assistant:\nI am not sure.\n") {
		t.Errorf("space-terminated deltas rendered wrong:\n%s", out)
	}
	if strings.Contains(out, "not  sure") || strings.Contains(out, "sure. \n") {
		t.Errorf("extra space in agent line:\n%s", out)
	}
}

func TestSessionToolCallRenders(t *testing.T) {
	fake, buf, done := startSession(t, voiceConfig(echoTool()), Options{})
	fake.waitConnected(t)
	runTool := fake.cfg.RunTool
	if runTool == nil {
		t.Fatal("ClientConfig.RunTool = nil, want a runner")
	}

	output, isErr := runTool(context.Background(), "echoer", json.RawMessage(`{}`))
	if isErr {
		t.Errorf("runTool isErr = true, want false; output %q", output)
	}
	buf.waitFor(t, ">>> Tool: echoer")
	buf.waitFor(t, ">>> Result: Tool: echoer\nhi")

	fake.push(Event{Type: typeSessionEnded})
	fake.finish(nil)
	if err := <-done; err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
}

func TestSessionUnknownToolRendersError(t *testing.T) {
	fake, buf, done := startSession(t, voiceConfig(echoTool()), Options{})
	fake.waitConnected(t)
	runTool := fake.cfg.RunTool

	output, isErr := runTool(context.Background(), "nope", json.RawMessage(`{}`))
	if !isErr {
		t.Errorf("runTool isErr = false, want true for an unknown tool")
	}
	if !strings.Contains(output, "unknown tool") {
		t.Errorf("output = %q, want it to name the unknown tool", output)
	}
	buf.waitFor(t, ">>> Error:")

	fake.push(Event{Type: typeSessionEnded})
	fake.finish(nil)
	<-done
}

func TestSessionClientErrorFails(t *testing.T) {
	fake, _, done := startSession(t, voiceConfig(echoTool()), Options{})

	fake.finish(errors.New("socket exploded"))
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "socket exploded") {
		t.Errorf("Run error = %v, want the client's terminal error", err)
	}
}

func TestSessionSigintEnds(t *testing.T) {
	sig := make(chan os.Signal, 1)
	fake, _, done := startSession(t, voiceConfig(echoTool()), Options{SigintChan: sig})
	fake.waitConnected(t)

	sig <- os.Interrupt

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SIGINT to end the session")
	}
	fake.mu.Lock()
	called := fake.endCalled
	fake.mu.Unlock()
	if !called {
		t.Error("End was not called on the first SIGINT")
	}
}

func TestSessionSigintDoubleTaps(t *testing.T) {
	// The real client's End blocks until the server answers session.ended.
	// A second Ctrl-C while End waits must force-close rather than being
	// ignored behind the blocked End call.
	sig := make(chan os.Signal, 1)
	fake, buf, done := startSession(t, voiceConfig(echoTool()), Options{SigintChan: sig})
	fake.waitConnected(t)

	fake.mu.Lock()
	fake.endBlock = make(chan struct{})
	fake.mu.Unlock()

	sig <- os.Interrupt
	buf.waitFor(t, "Hanging up")

	// End is now blocked; the second signal must force-close.
	sig <- os.Interrupt

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second Ctrl-C did not force-close while End was blocked")
	}
	fake.mu.Lock()
	closes := fake.closeCalls
	fake.mu.Unlock()
	if closes == 0 {
		t.Error("Close was not called on the second SIGINT")
	}
}

func TestSessionNoMicSkipsCapture(t *testing.T) {
	cfg := voiceConfig(echoTool())
	cfg.Agents[0].Voice.InputCommand = []string{"/nonexistent/blorb-capture"}

	// NoMic skips capture startup, so the bogus command never runs.
	fake, _, done := startSession(t, cfg, Options{NoMic: true})
	fake.waitConnected(t)
	fake.finish(nil)
	if err := <-done; err != nil {
		t.Fatalf("Run error = %v, want nil with NoMic", err)
	}

	// Without NoMic the bogus capture command fails startup.
	fake2 := newFakeClient()
	err := Run(context.Background(), Options{
		Agent:  cfg.Agents[0],
		Config: cfg,
		Stdout: &syncBuffer{},
		NoMic:  false,
		NewSessionClient: func(_ context.Context, _ ClientConfig) (SessionClient, error) {
			return fake2, nil
		},
	})
	if err == nil {
		t.Fatal("Run error = nil, want the capture startup failure")
	}
}

func TestSessionDeciderToolFails(t *testing.T) {
	cfg := voiceConfig(echoTool())
	cfg.Deciders = []config.Decider{{
		Name:      "triage",
		Model:     "m",
		Questions: map[string]config.Question{"q": {Type: config.QuestionTypeNoul}},
	}}
	cfg.Tools = append(cfg.Tools, config.ToolEntry{
		Type:        config.ToolTypeDecider,
		Name:        "classify",
		Description: "Classify the state",
		Decider:     "triage",
	})
	cfg.Agents[0].Tools = []string{"echoer", "classify"}

	err := Run(context.Background(), Options{
		Agent:            cfg.Agents[0],
		Config:           cfg,
		Stdout:           &syncBuffer{},
		NoMic:            true,
		NewSessionClient: func(_ context.Context, _ ClientConfig) (SessionClient, error) { return newFakeClient(), nil },
	})
	if err == nil {
		t.Fatal("Run error = nil, want a decider exclusion error")
	}
	if !strings.Contains(err.Error(), "classify") {
		t.Errorf("Run error = %v, want it to name the decider tool", err)
	}
}

func TestSessionSubagentToolRunsAndRenders(t *testing.T) {
	// A granted subagent tool runs the target agent through the engine with
	// the injected LLM client, and its activity renders with the chat
	// subagent style (labeled [agent] and indented).
	cfg := voiceConfig(echoTool())
	cfg.Tools = append(cfg.Tools, config.ToolEntry{
		Type:        config.ToolTypeSubagent,
		Name:        "delegate",
		Description: "Delegate to another agent",
		Agent:       "scholar",
	})
	cfg.Agents = append(cfg.Agents, config.Agent{
		Name:         "scholar",
		SystemPrompt: "You are a scholar.",
		Model:        "m",
		MaxTurns:     2,
	})
	cfg.Agents[0].Tools = []string{"echoer", "delegate"}

	scholarLLM := &cannedClient{responses: []llm.Response{
		{
			ID:           "r1",
			Message:      llm.NewTextMessage(llm.RoleAssistant, "biscuits are lovely"),
			FinishReason: llm.FinishStop,
		},
	}}

	fake := newFakeClient()
	buf := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Options{
			Agent:  cfg.Agents[0],
			Config: cfg,
			Stdout: buf,
			NoMic:  true,
			NewSessionClient: func(_ context.Context, c ClientConfig) (SessionClient, error) {
				fake.markConnected(c)
				return fake, nil
			},
			NewLLMClient: func(_ config.Config, _ config.Agent) (llm.Client, error) {
				return scholarLLM, nil
			},
		})
	}()
	fake.waitConnected(t)

	output, isErr := fake.cfg.RunTool(context.Background(), "delegate", json.RawMessage(`{"prompt":"biscuits"}`))
	if isErr {
		t.Errorf("subagent tool isErr = true, want false; output %q", output)
	}
	if !strings.Contains(output, "biscuits are lovely") {
		t.Errorf("subagent output = %q, want the scholar's reply", output)
	}
	buf.waitFor(t, "[scholar] >>> Assistant:")
	buf.waitFor(t, "biscuits are lovely")

	fake.push(Event{Type: typeSessionEnded})
	fake.finish(nil)
	if err := <-done; err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
}

func floatPtr(f float64) *float64 { return &f }

// cannedClient is an llm.Client returning one canned response per call.
type cannedClient struct {
	mu        sync.Mutex
	responses []llm.Response
}

func (c *cannedClient) Chat(_ context.Context, _ llm.Request) (*llm.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.responses) == 0 {
		return nil, errors.New("cannedClient: no more responses")
	}
	resp := c.responses[0]
	c.responses = c.responses[1:]
	return &resp, nil
}
