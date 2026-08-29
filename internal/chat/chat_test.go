package chat_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/overspecific/goblorb/internal/chat"
	"github.com/overspecific/goblorb/internal/config"
	"github.com/overspecific/goblorb/internal/llm"
)

// fakeClient returns canned responses in order and records requests.
type fakeClient struct {
	responses []llm.Response
}

func (f *fakeClient) Chat(_ context.Context, _ llm.Request) (*llm.Response, error) {
	if len(f.responses) == 0 {
		return nil, errors.New("fakeClient: no more canned responses")
	}
	resp := f.responses[0]
	f.responses = f.responses[1:]
	return &resp, nil
}

type optsFunc func(*chat.Options)

func newTestOptions(cfg config.Config, input string, responses ...llm.Response) (chat.Options, *strings.Builder, *strings.Builder) {
	var stdout, stderr strings.Builder
	o := chat.Options{
		Config:  cfg,
		Version: "test",
		Stdin:   strings.NewReader(input),
		Stdout:  &stdout,
		Stderr:  &stderr,
		NewClient: func(config.Config) (llm.Client, error) {
			return &fakeClient{responses: responses}, nil
		},
	}
	return o, &stdout, &stderr
}

func minimalConfig() config.Config {
	return config.Config{
		Name:         "tester",
		SystemPrompt: "You are helpful.",
		Provider: config.Provider{
			Type:    config.ProviderTypeOpenAI,
			Model:   "gpt-test",
			BaseURL: "http://localhost:1",
		},
	}
}

func TestRunPlainReplySession(t *testing.T) {
	t.Parallel()

	cfg := minimalConfig()
	o, stdout, stderr := newTestOptions(cfg, "hello there\nexit\n",
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "hi!"), FinishReason: llm.FinishStop},
	)

	err := chat.Run(context.Background(), o)
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "hi!") {
		t.Errorf("stdout = %q, want the assistant reply", out)
	}
	errOut := stderr.String()
	if !strings.Contains(errOut, "tester") || !strings.Contains(errOut, "gpt-test") {
		t.Errorf("stderr = %q, want a banner naming agent and model", errOut)
	}
	if strings.Contains(out, "blorb") {
		t.Errorf("stdout = %q, want no banner on stdout", out)
	}
}

func TestRunExitCommand(t *testing.T) {
	t.Parallel()

	o, stdout, _ := newTestOptions(minimalConfig(), "hi\nquit\n",
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "reply"), FinishReason: llm.FinishStop},
	)

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if !strings.Contains(stdout.String(), "reply") {
		t.Errorf("stdout = %q, want the reply before quitting", stdout.String())
	}
}

func TestRunEOF(t *testing.T) {
	t.Parallel()

	o, _, _ := newTestOptions(minimalConfig(), "")

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
}

func TestRunEmptyLinesIgnored(t *testing.T) {
	t.Parallel()

	// Whitespace lines produce no API calls; the fake has no responses, so
	// any turn would fail the run.
	o, _, _ := newTestOptions(minimalConfig(), "   \n\t\nexit\n")

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
}

func TestRunToolEventsOnStderr(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	File := filepath.Join(dir, "marker.txt")
	if err := os.WriteFile(File, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := minimalConfig()
	cfg.Tools = []config.ToolEntry{{
		Name:        "write_marker",
		Description: "Creates a marker file.",
		Command:     []string{"touch", File},
	}}

	toolCallResp := llm.Response{
		Message: llm.Message{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{{
				ID: "call_1", Type: "function", FunctionName: "write_marker", FunctionArgs: "{}",
			}},
		},
		FinishReason: llm.FinishToolCalls,
	}

	o, stdout, stderr := newTestOptions(cfg, "make the marker\nexit\n",
		toolCallResp,
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "made it"), FinishReason: llm.FinishStop},
	)

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	errOut := stderr.String()
	if !strings.Contains(errOut, "→ write_marker") {
		t.Errorf("stderr = %q, want a tool call line", errOut)
	}
	if !strings.Contains(errOut, "← write_marker") {
		t.Errorf("stderr = %q, want a tool result line", errOut)
	}
	if strings.Contains(stdout.String(), "write_marker") {
		t.Errorf("stdout = %q, want tool activity only on stderr", stdout.String())
	}
	if !strings.Contains(stdout.String(), "made it") {
		t.Errorf("stdout = %q, want the final text", stdout.String())
	}
}

func TestRunStartupErrors(t *testing.T) {
	t.Parallel()

	t.Run("client construction failure", func(t *testing.T) {
		t.Parallel()

		o, _, _ := newTestOptions(minimalConfig(), "hello\n")
		o.NewClient = func(config.Config) (llm.Client, error) {
			return nil, errors.New("no provider available")
		}

		err := chat.Run(context.Background(), o)
		if err == nil || !strings.Contains(err.Error(), "no provider available") {
			t.Errorf("error = %v, want the client construction error", err)
		}
	})

	t.Run("tool registry failure", func(t *testing.T) {
		t.Parallel()

		cfg := minimalConfig()
		cfg.Tools = []config.ToolEntry{{
			Name: "bad", Description: "Bad tool.", Command: []string{},
		}}

		o, _, _ := newTestOptions(cfg, "hello\n")

		err := chat.Run(context.Background(), o)
		if err == nil || !strings.Contains(err.Error(), "build tools") {
			t.Errorf("error = %v, want a build-tools error", err)
		}
	})
}

func TestRunTurnErrorKeepsSessionAlive(t *testing.T) {
	t.Parallel()

	// The fake has no responses: the first turn fails, the next exits.
	o, _, stderr := newTestOptions(minimalConfig(), "hello\nexit\n")

	err := chat.Run(context.Background(), o)
	if err != nil {
		t.Fatalf("Run error = %v, want nil (turn errors must not end the session)", err)
	}
	errOut := stderr.String()
	if !strings.Contains(errOut, "error:") || !strings.Contains(errOut, "no more canned responses") {
		t.Errorf("stderr = %q, want the turn error printed", errOut)
	}
	if strings.Contains(errOut, "(interrupted)") {
		t.Errorf("stderr = %q, want no (interrupted) marker for a plain failure", errOut)
	}
}

func TestRunSigintDuringTurnInterruptsTurnOnly(t *testing.T) {
	t.Parallel()

	sigs := make(chan os.Signal, 1)
	cfg := minimalConfig()

	// A client that blocks until its context is cancelled, i.e. an
	// in-flight request cut short by the interrupt.
	client := &blockingClient{started: make(chan struct{})}
	var stdout, stderr strings.Builder
	o := chat.Options{
		Config:  cfg,
		Version: "test",
		Stdin:   strings.NewReader("start a long turn\nexit\n"),
		Stdout:  &stdout,
		Stderr:  &stderr,
		NewClient: func(config.Config) (llm.Client, error) {
			return client, nil
		},
		SigintChan: sigs,
	}

	runErr := make(chan error, 1)
	go func() { runErr <- chat.Run(context.Background(), o) }()

	// Wait until the turn is in flight, then interrupt it.
	awaitTurnStarted(t, client)
	sigs <- syscall.SIGINT

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after interrupt")
	}
	if !strings.Contains(stderr.String(), "(interrupted)") {
		t.Errorf("stderr = %q, want the interrupted marker", stderr.String())
	}
	if client.cancelledWith() == context.Background() || client.cancelledWith() == nil {
		t.Error("turn context was not cancelled by the interrupt")
	}
}

func TestRunSigintWhileIdleExits(t *testing.T) {
	t.Parallel()

	sigs := make(chan os.Signal, 1)
	o, _, _ := newTestOptions(minimalConfig(), "")
	o.SigintChan = sigs

	runErr := make(chan error, 1)
	go func() { runErr <- chat.Run(context.Background(), o) }()

	// Give Run a moment to reach the read loop, then signal.
	time.Sleep(50 * time.Millisecond)
	sigs <- syscall.SIGINT

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after idle interrupt")
	}
}

// blockingClient blocks in Chat until its context is cancelled.
type blockingClient struct {
	started    chan struct{}
	mu         sync.Mutex
	cancelled  context.Context
	turnedOnce bool
}

func (b *blockingClient) Chat(ctx context.Context, _ llm.Request) (*llm.Response, error) {
	b.mu.Lock()
	if !b.turnedOnce {
		b.turnedOnce = true
		close(b.started)
	}
	b.mu.Unlock()

	<-ctx.Done()
	b.mu.Lock()
	b.cancelled = ctx
	b.mu.Unlock()
	return nil, ctx.Err()
}

func (b *blockingClient) cancelledWith() context.Context {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cancelled
}

func awaitTurnStarted(t *testing.T, b *blockingClient) {
	t.Helper()
	select {
	case <-b.started:
	case <-time.After(2 * time.Second):
		t.Fatal("turn never started")
	}
}

func TestNewClient(t *testing.T) {
	t.Run("openai with key env set", func(t *testing.T) {
		t.Setenv("BLORB_TEST_KEY", "key-value")

		cfg := minimalConfig()
		cfg.Provider.APIKeyEnv = "BLORB_TEST_KEY"

		client, err := chat.NewClient(cfg)
		if err != nil || client == nil {
			t.Fatalf("NewClient error = %v, want a client", err)
		}
	})

	t.Run("openai with missing key env", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Provider.APIKeyEnv = "BLORB_TEST_MISSING_KEY"

		_, err := chat.NewClient(cfg)
		if err == nil || !strings.Contains(err.Error(), "BLORB_TEST_MISSING_KEY") {
			t.Errorf("error = %v, want a missing-env error naming the variable", err)
		}
	})

	t.Run("unknown provider type", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Provider.Type = "telepathy"

		_, err := chat.NewClient(cfg)
		if err == nil || !strings.Contains(err.Error(), "telepathy") {
			t.Errorf("error = %v, want an unsupported-type error", err)
		}
	})
}

func TestConfigPathResolution(t *testing.T) {
	t.Run("default path missing", func(t *testing.T) {
		dir := t.TempDir()
		_, err := config.Load(filepath.Join(dir, "blorb.json"))
		if err == nil || !errors.Is(err, os.ErrNotExist) {
			t.Errorf("error = %v, want a not-exist error", err)
		}
	})
}
