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

// End records the hang-up and finishes the session cleanly.
func (f *fakeClient) End() error {
	f.mu.Lock()
	f.endCalled = true
	f.mu.Unlock()
	f.finish(nil)
	return nil
}

func (f *fakeClient) Close() error {
	f.mu.Lock()
	f.closeCalls++
	f.mu.Unlock()
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
	if opts.NewClient == nil {
		opts.NewClient = func(_ context.Context, c ClientConfig) (SessionClient, error) {
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
	if !strings.Contains(out, "User: hello\n") {
		t.Errorf("missing final user line in output:\n%s", out)
	}
	if !strings.Contains(out, "Agent: Hi there\n") {
		t.Errorf("missing agent line in output:\n%s", out)
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
	if out := buf.String(); !strings.Contains(out, "[interrupted]") {
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
	if out := buf.String(); !strings.Contains(out, "Agent: whole reply\n") {
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
	if !strings.Contains(out, "Agent: I was say [interrupted]\n") {
		t.Errorf("missing interrupted final line in output:\n%s", out)
	}
}

func TestSessionToolCallRenders(t *testing.T) {
	fake, buf, done := startSession(t, voiceConfig(echoTool()), Options{})
	fake.waitConnected(t)
	fake.mu.Lock()
	runTool := fake.cfg.RunTool
	fake.mu.Unlock()
	if runTool == nil {
		t.Fatal("ClientConfig.RunTool = nil, want a runner")
	}

	output, isErr := runTool(context.Background(), "echoer", json.RawMessage(`{}`))
	if isErr {
		t.Errorf("runTool isErr = true, want false; output %q", output)
	}
	buf.waitFor(t, ">>> Tool: echoer")
	buf.waitFor(t, ">>> Result: hi")

	fake.push(Event{Type: typeSessionEnded})
	fake.finish(nil)
	if err := <-done; err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
}

func TestSessionUnknownToolRendersError(t *testing.T) {
	fake, buf, done := startSession(t, voiceConfig(echoTool()), Options{})
	fake.waitConnected(t)
	fake.mu.Lock()
	runTool := fake.cfg.RunTool
	fake.mu.Unlock()

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
		NewClient: func(_ context.Context, _ ClientConfig) (SessionClient, error) {
			return fake2, nil
		},
	})
	if err == nil {
		t.Fatal("Run error = nil, want the capture startup failure")
	}
}

func TestSessionSubagentToolFails(t *testing.T) {
	cfg := voiceConfig(echoTool())
	cfg.Tools = append(cfg.Tools, config.ToolEntry{
		Type:        config.ToolTypeSubagent,
		Name:        "delegate",
		Description: "Delegate to another agent",
		Agent:       "assistant",
	})
	cfg.Agents[0].Tools = []string{"echoer", "delegate"}

	err := Run(context.Background(), Options{
		Agent:     cfg.Agents[0],
		Config:    cfg,
		Stdout:    &syncBuffer{},
		NoMic:     true,
		NewClient: func(_ context.Context, _ ClientConfig) (SessionClient, error) { return newFakeClient(), nil },
	})
	if err == nil {
		t.Fatal("Run error = nil, want a subagent exclusion error")
	}
	if !strings.Contains(err.Error(), "delegate") {
		t.Errorf("Run error = %v, want it to name the subagent tool", err)
	}
}

func floatPtr(f float64) *float64 { return &f }
