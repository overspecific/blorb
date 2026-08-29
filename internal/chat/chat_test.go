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

	"github.com/overspecific/blorb/internal/chat"
	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/llm"
)

// fakeClient returns canned responses in order and records requests.
type fakeClient struct {
	responses []llm.Response
	requests  []llm.Request
}

func (f *fakeClient) Chat(_ context.Context, req llm.Request) (*llm.Response, error) {
	f.requests = append(f.requests, req)
	if len(f.responses) == 0 {
		return nil, errors.New("fakeClient: no more canned responses")
	}
	resp := f.responses[0]
	f.responses = f.responses[1:]
	return &resp, nil
}

func newTestOptions(cfg config.Config, input string, responses ...llm.Response) (chat.Options, *syncBuffer) {
	var stdout syncBuffer
	o := chat.Options{
		Config:  cfg,
		Version: "test",
		Stdin:   strings.NewReader(input),
		Stdout:  &stdout,
		NewClient: func(config.Config) (llm.Client, error) {
			return &fakeClient{responses: responses}, nil
		},
	}
	return o, &stdout
}

func newStreamingTestOptions(cfg config.Config, input string, responses ...llm.Response) (chat.Options, *syncBuffer) {
	var stdout syncBuffer
	o := chat.Options{
		Config:  cfg,
		Version: "test",
		Stdin:   strings.NewReader(input),
		Stdout:  &stdout,
		NewClient: func(config.Config) (llm.Client, error) {
			return &streamingFakeClient{responses: responses}, nil
		},
	}
	return o, &stdout
}

// streamingFakeClient breaks each canned response into a single delta per
// field before returning the complete response, like a streaming provider.
type streamingFakeClient struct {
	responses []llm.Response
}

func (f *streamingFakeClient) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return (&fakeClient{responses: f.responses}).Chat(ctx, req)
}

func (f *streamingFakeClient) ChatStream(_ context.Context, req llm.Request, onDelta func(llm.Delta) error) (*llm.Response, error) {
	req.Messages = append(req.Messages, llm.NewTextMessage(llm.RoleUser, "stream"))
	if len(f.responses) == 0 {
		return nil, errors.New("streamingFakeClient: no more canned responses")
	}
	resp := f.responses[0]
	f.responses = f.responses[1:]
	msg := resp.Message
	if msg.Reasoning != "" {
		if err := onDelta(llm.Delta{Reasoning: msg.Reasoning}); err != nil {
			return nil, err
		}
	}
	if msg.Content != "" {
		if err := onDelta(llm.Delta{Content: msg.Content}); err != nil {
			return nil, err
		}
	}
	for i, tc := range msg.ToolCalls {
		if err := onDelta(llm.Delta{ToolCall: &llm.ToolCallDelta{
			Index: i, ID: tc.ID, Type: tc.Type, Name: tc.FunctionName, Arguments: tc.FunctionArgs,
		}}); err != nil {
			return nil, err
		}
	}
	return &resp, nil
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
	o, stdout := newTestOptions(cfg, "hello there\nexit\n",
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "hi!"), FinishReason: llm.FinishStop},
	)

	err := chat.Run(context.Background(), o)
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	out := stdout.String()
	if !strings.Contains(out, ">>> Assistant:\nhi!") {
		t.Errorf("stdout = %q, want the reply under a >>> Assistant heading", out)
	}
	if !strings.Contains(out, "tester") || !strings.Contains(out, "gpt-test") {
		t.Errorf("stdout = %q, want a banner naming agent and model", out)
	}
	// Heading prints before reading: initially, and again after the turn.
	if strings.Count(out, ">>> User:\n") != 2 {
		t.Errorf("stdout = %q, want a >>> User heading before each read (2 total)", out)
	}
}

func TestRunThinkingDisplayedOnStdout(t *testing.T) {
	t.Parallel()

	thinkingResp := llm.Response{
		Message: llm.Message{
			Role:      llm.RoleAssistant,
			Content:   "the answer",
			Reasoning: "pondering...",
		},
		FinishReason: llm.FinishStop,
	}
	o, stdout := newTestOptions(minimalConfig(), "why?\nexit\n", thinkingResp)

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	out := stdout.String()
	if !strings.Contains(out, ">>> Assistant (thinking):\npondering...") {
		t.Errorf("stdout = %q, want the reasoning under a >>> Assistant (thinking) heading", out)
	}
	if !strings.Contains(out, ">>> Assistant:\nthe answer") {
		t.Errorf("stdout = %q, want the text under a >>> Assistant heading", out)
	}
}

func TestRunStreamedAssistantTextOnStdout(t *testing.T) {
	t.Parallel()

	o, stdout := newStreamingTestOptions(minimalConfig(), "hello\nexit\n",
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "streamed!"), FinishReason: llm.FinishStop},
	)
	o.Stream = true

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	out := stdout.String()
	// Heading printed exactly once, deltas concatenated, trailing newline
	// flushed.
	if !strings.Contains(out, ">>> Assistant:\nstreamed!\n") {
		t.Errorf("stdout = %q, want the streamed reply under one heading with a trailing newline", out)
	}
	if strings.Count(out, ">>> Assistant:") != 1 {
		t.Errorf("stdout = %q, want exactly one >>> Assistant heading", out)
	}
}

func TestRunStreamedThinkingOnStdout(t *testing.T) {
	t.Parallel()

	thinkingResp := llm.Response{
		Message: llm.Message{
			Role:      llm.RoleAssistant,
			Content:   "the answer",
			Reasoning: "pondering...",
		},
		FinishReason: llm.FinishStop,
	}
	o, stdout := newStreamingTestOptions(minimalConfig(), "why?\nexit\n", thinkingResp)
	o.Stream = true

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	out := stdout.String()
	if !strings.Contains(out, ">>> Assistant (thinking):\npondering...") {
		t.Errorf("stdout = %q, want the reasoning deltas under a >>> Assistant (thinking) heading", out)
	}
	if strings.Count(out, ">>> Assistant (thinking):") != 1 {
		t.Errorf("stdout = %q, want exactly one thinking heading", out)
	}
	if !strings.Contains(out, ">>> Assistant:\nthe answer\n") {
		t.Errorf("stdout = %q, want the text deltas under a >>> Assistant heading", out)
	}
}

func TestRunStreamedToolCallDeltas(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	marker := filepath.Join(dir, "marker.txt")

	cfg := minimalConfig()
	cfg.Tools = []config.ToolEntry{{
		Name:        "touch",
		Description: "Creates a marker file.",
		Command:     []string{"touch", marker},
	}}

	toolCallResp := llm.Response{
		Message: llm.Message{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{{
				ID: "call_1", Type: "function", FunctionName: "touch", FunctionArgs: "{}",
			}},
		},
		FinishReason: llm.FinishToolCalls,
	}
	o, stdout := newStreamingTestOptions(cfg, "run it\nexit\n",
		toolCallResp,
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "done"), FinishReason: llm.FinishStop},
	)
	o.Stream = true

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	out := stdout.String()
	if !strings.Contains(out, ">>> Tool: touch") {
		t.Errorf("stdout = %q, want a tool heading for touch", out)
	}
	if strings.Count(out, ">>> Tool: touch") != 1 {
		t.Errorf("stdout = %q, want the streamed fragments to render the tool heading exactly once", out)
	}
	if !strings.Contains(out, "{}") {
		t.Errorf("stdout = %q, want the arguments fragment", out)
	}
	// The streamed arguments leave the stream mid-line; the following tool
	// result block must terminate the partial line first so its heading
	// starts on a fresh line, keeping the blank-line separators consistent.
	if strings.Count(out, "\n\n>>> Result: Tool: touch") != 1 {
		t.Errorf("stdout = %q, want a blank line between the arguments and the result heading", out)
	}
	// Tool results are rendered after the streamed turn.
	if !strings.Contains(out, ">>> Result: Tool: touch") {
		t.Errorf("stdout = %q, want the tool result heading", out)
	}
	if !strings.Contains(out, ">>> Assistant:\ndone\n") {
		t.Errorf("stdout = %q, want the final streamed text", out)
	}
}

// TestRunStreamedToolRoundsRenderPerRound is a regression test for a
// multi-round streamed turn: after a tool result, the next assistant round
// must print fresh thinking/text headings rather than streaming raw, and
// spacing between blocks must be consistent.
func TestRunStreamedToolRoundsRenderPerRoundHeadings(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	marker := filepath.Join(dir, "marker.txt")

	cfg := minimalConfig()
	cfg.Tools = []config.ToolEntry{{
		Name:        "touch",
		Description: "Creates a marker file.",
		Command:     []string{"touch", marker},
	}}

	toolCallResp := llm.Response{
		Message: llm.Message{
			Role:      llm.RoleAssistant,
			Reasoning: "round one thinking",
			Content:   "calling the tool",
			ToolCalls: []llm.ToolCall{{
				ID: "call_1", Type: "function", FunctionName: "touch", FunctionArgs: "{}",
			}},
		},
		FinishReason: llm.FinishToolCalls,
	}
	finalResp := llm.Response{
		Message: llm.Message{
			Role:      llm.RoleAssistant,
			Content:   "final answer",
			Reasoning: "round two thinking",
		},
		FinishReason: llm.FinishStop,
	}
	o, stdout := newStreamingTestOptions(cfg, "run it\nexit\n", toolCallResp, finalResp)
	o.Stream = true

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	out := stdout.String()
	// Both rounds get their own heading pairs.
	if strings.Count(out, ">>> Assistant (thinking):") != 2 {
		t.Errorf("stdout = %q, want one thinking heading per streamed round (2 total)", out)
	}
	if strings.Count(out, ">>> Assistant:\n") != 2 {
		t.Errorf("stdout = %q, want one Assistant heading per streamed round (2 total)", out)
	}
	if !strings.Contains(out, ">>> Assistant (thinking):\nround one thinking") {
		t.Errorf("stdout = %q, want round one reasoning under a heading", out)
	}
	if !strings.Contains(out, ">>> Assistant:\nfinal answer\n") {
		t.Errorf("stdout = %q, want the final round streamed under a heading", out)
	}
	// The tool result block renders between the two rounds on stdout.
	if strings.Count(out, ">>> Result: Tool: touch") != 1 {
		t.Errorf("stdout = %q, want the tool result block on stdout", out)
	}
}

func TestRunStreamOffUsesWholeMessagePath(t *testing.T) {
	t.Parallel()

	o, stdout := newStreamingTestOptions(minimalConfig(), "hello\nexit\n",
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "whole"), FinishReason: llm.FinishStop},
	)
	o.Stream = false

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	if !strings.Contains(stdout.String(), ">>> Assistant:\nwhole\n") {
		t.Errorf("stdout = %q, want the whole-message reply path used", stdout.String())
	}
}

func TestRunStreamedTurnErrorStillFlushes(t *testing.T) {
	t.Parallel()

	// A turn that fails after streaming some text must still flush the
	// trailing newline, and the error lands on stdout.
	o, stdout := newStreamingTestOptions(minimalConfig(), "hello\nexit\n")
	o.Stream = true

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "error:") {
		t.Errorf("stdout = %q, want a turn error", out)
	}
	if strings.Contains(out, ">>> Assistant:") {
		// Heading printed, then the stream failed: a trailing newline must
		// still have been flushed by the callback's flush function.
		if !strings.HasSuffix(out, "\n") {
			t.Errorf("stdout = %q, want a trailing flush newline", out)
		}
	}
}

func TestRunExitCommand(t *testing.T) {
	t.Parallel()

	o, stdout := newTestOptions(minimalConfig(), "hi\nquit\n",
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

	o, _ := newTestOptions(minimalConfig(), "")

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
}

func TestRunEmptyLinesIgnored(t *testing.T) {
	t.Parallel()

	// Whitespace lines produce no API calls; the fake has no responses, so
	// any turn would fail the run.
	o, _ := newTestOptions(minimalConfig(), "   \n\t\nexit\n")

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
}

func TestRunToolEventsOnStdout(t *testing.T) {
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

	o, stdout := newTestOptions(cfg, "make the marker\nexit\n",
		toolCallResp,
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "made it"), FinishReason: llm.FinishStop},
	)

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	out := stdout.String()
	if !strings.Contains(out, ">>> Tool: write_marker") {
		t.Errorf("stdout = %q, want a tool call heading", out)
	}
	if !strings.Contains(out, ">>> Result: Tool: write_marker") {
		t.Errorf("stdout = %q, want a tool result heading", out)
	}
	if !strings.Contains(out, ">>> Assistant:\nmade it") {
		t.Errorf("stdout = %q, want the final text under a >>> Assistant heading", out)
	}
}

func TestRunStartupErrors(t *testing.T) {
	t.Parallel()

	t.Run("client construction failure", func(t *testing.T) {
		t.Parallel()

		o, _ := newTestOptions(minimalConfig(), "hello\n")
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

		o, _ := newTestOptions(cfg, "hello\n")

		err := chat.Run(context.Background(), o)
		if err == nil || !strings.Contains(err.Error(), "build tools") {
			t.Errorf("error = %v, want a build-tools error", err)
		}
	})
}

func TestRunTurnErrorKeepsSessionAlive(t *testing.T) {
	t.Parallel()

	// The fake has no responses: the first turn fails, the next exits.
	o, stdout := newTestOptions(minimalConfig(), "hello\nexit\n")

	err := chat.Run(context.Background(), o)
	if err != nil {
		t.Fatalf("Run error = %v, want nil (turn errors must not end the session)", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "error:") || !strings.Contains(out, "no more canned responses") {
		t.Errorf("stdout = %q, want the turn error printed", out)
	}
	if strings.Contains(out, "(interrupted)") {
		t.Errorf("stdout = %q, want no (interrupted) marker for a plain failure", out)
	}
}

func TestRunSigintDuringTurnInterruptsTurnOnly(t *testing.T) {
	t.Parallel()

	sigs := make(chan os.Signal, 1)
	cfg := minimalConfig()

	// A client that blocks until its context is cancelled, i.e. an
	// in-flight request cut short by the interrupt.
	client := &blockingClient{started: make(chan struct{})}
	var stdout strings.Builder
	o := chat.Options{
		Config:  cfg,
		Version: "test",
		Stdin:   strings.NewReader("start a long turn\nexit\n"),
		Stdout:  &stdout,
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
	if !strings.Contains(stdout.String(), "(interrupted)") {
		t.Errorf("stdout = %q, want the interrupted marker", stdout.String())
	}
	if client.cancelledWith() == context.Background() || client.cancelledWith() == nil {
		t.Error("turn context was not cancelled by the interrupt")
	}
}

func TestRunSigintWhileIdleExits(t *testing.T) {
	t.Parallel()

	sigs := make(chan os.Signal, 1)
	o, stdout := newTestOptions(minimalConfig(), "")
	o.SigintChan = sigs

	runErr := make(chan error, 1)
	go func() { runErr <- chat.Run(context.Background(), o) }()

	// Wait for the banner, which Run prints right before entering the read
	// loop, instead of sleeping a fixed interval.
	awaitBanner(t, stdout)
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

// awaitBanner waits until the stdout buffer contains the banner text.
func awaitBanner(t *testing.T, stdout *syncBuffer) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if strings.Contains(stdout.String(), "blorb") {
			return
		}
		select {
		case <-deadline:
			t.Fatal("banner never appeared on stdout")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// syncBuffer is a mutex-guarded strings.Builder for tests that read a
// buffer while Run writes to it from another goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestRunInterruptedTurnThenSuccessKeepsSession is a regression test: the
// interrupted flag used to stick, so the next successful turn also exited.
func TestRunInterruptedTurnThenSuccessKeepsSession(t *testing.T) {
	t.Parallel()

	sigs := make(chan os.Signal, 1)
	cfg := minimalConfig()

	// First turn blocks until interrupted; second turn succeeds.
	client := &blockingClient{started: make(chan struct{})}
	var stdout strings.Builder
	o := chat.Options{
		Config:  cfg,
		Version: "test",
		Stdin:   strings.NewReader("start a long turn\ntry again\nexit\n"),
		Stdout:  &stdout,
		NewClient: func(config.Config) (llm.Client, error) {
			return &sequencedClient{first: client, rest: &fakeClient{
				responses: []llm.Response{
					{Message: llm.NewTextMessage(llm.RoleAssistant, "second try worked"), FinishReason: llm.FinishStop},
				},
			}}, nil
		},
		SigintChan: sigs,
	}

	runErr := make(chan error, 1)
	go func() { runErr <- chat.Run(context.Background(), o) }()

	awaitTurnStarted(t, client)
	sigs <- syscall.SIGINT

	// The first turn ends interrupted; the session must stay alive and run
	// the second turn successfully before "exit" ends it.
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return; session likely exited after the interrupted turn")
	}

	if !strings.Contains(stdout.String(), "(interrupted)") {
		t.Errorf("stdout = %q, want the interrupted marker", stdout.String())
	}
	if !strings.Contains(stdout.String(), "second try worked") {
		t.Errorf("stdout = %q, want the follow-up turn to run and reply", stdout.String())
	}
}

// sequencedClient routes the first Chat call to first and all later calls
// to rest.
type sequencedClient struct {
	first llm.Client
	rest  llm.Client
	once  sync.Once
}

func (s *sequencedClient) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	var c llm.Client
	s.once.Do(func() { c = s.first })
	if c == nil {
		c = s.rest
	}
	return c.Chat(ctx, req)
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
		cfg.Provider.APIKeyEnv = ptr("BLORB_TEST_KEY")

		client, err := chat.NewClient(cfg)
		if err != nil || client == nil {
			t.Fatalf("NewClient error = %v, want a client", err)
		}
	})

	t.Run("openai with missing key env", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Provider.APIKeyEnv = ptr("BLORB_TEST_MISSING_KEY")

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

func ptr(s string) *string {
	return &s
}

// TestRunUsesInjectedGetenv verifies Options.Getenv replaces the
// environment lookup when no NewClient override is set.
func TestRunUsesInjectedGetenv(t *testing.T) {
	t.Parallel()

	cfg := minimalConfig()
	cfg.Provider.APIKeyEnv = ptr("INJECTED_MISSING_VAR")

	// No NewClient override and an injected Getenv that resolves nothing:
	// Run must fail startup with the missing-env error, proving the
	// injected lookup was consulted.
	o := chat.Options{
		Config:  cfg,
		Version: "test",
		Stdin:   strings.NewReader("hi\n"),
		Stdout:  &strings.Builder{},
		Getenv:  func(string) string { return "" },
	}

	err := chat.Run(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "INJECTED_MISSING_VAR") {
		t.Errorf("error = %v, want a missing-env error naming the variable", err)
	}
}

// TestRunInjectedGetenvResolvesKey pins the positive path: the injected
// lookup supplies the key, so startup gets past env resolution (the turn
// fails with a connection error on stderr, not an env error).
func TestRunInjectedGetenvResolvesKey(t *testing.T) {
	t.Parallel()

	// The test base URL points at a dead port, so a client that gets past
	// env resolution fails on connect; the env error must not appear.
	cfg := minimalConfig()
	cfg.Provider.APIKeyEnv = ptr("INJECTED_ENV_VAR")

	var stdout strings.Builder
	o := chat.Options{
		Config:  cfg,
		Version: "test",
		Stdin:   strings.NewReader("hi\nexit\n"),
		Stdout:  &stdout,
		Getenv: func(key string) string {
			if key == "INJECTED_ENV_VAR" {
				return "injected-key"
			}
			return ""
		},
	}

	err := chat.Run(context.Background(), o)
	if err != nil {
		t.Fatalf("Run error = %v, want nil (turn errors keep the session alive)", err)
	}
	out := stdout.String()
	if strings.Contains(out, "api_key_env") {
		t.Errorf("stdout = %q, want no env resolution failure (key was injected)", out)
	}
	if !strings.Contains(out, "connection refused") {
		t.Errorf("stdout = %q, want a connect error proving the turn ran past env resolution", out)
	}
}

// TestRunLongLineDeliveredAsOneTurn verifies the scanner accepts lines well
// beyond 64 KiB without splitting them into multiple turns.
func TestRunLongLineDeliveredAsOneTurn(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", 200_000)
	cfg := minimalConfig()

	fc := &fakeClient{responses: []llm.Response{
		{Message: llm.NewTextMessage(llm.RoleAssistant, "ok"), FinishReason: llm.FinishStop},
	}}
	var stdout strings.Builder
	o := chat.Options{
		Config:  cfg,
		Version: "test",
		Stdin:   strings.NewReader(long + "\nexit\n"),
		Stdout:  &stdout,
		NewClient: func(config.Config) (llm.Client, error) {
			return fc, nil
		},
	}

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	// Only one user turn must have hit the API, carrying the whole line.
	if len(fc.requests) != 1 {
		t.Fatalf("API calls = %d, want 1 (long line split would call twice)", len(fc.requests))
	}
	last := fc.requests[0].Messages[len(fc.requests[0].Messages)-1]
	if last.Content != long {
		t.Errorf("user message length = %d, want %d (full line)", len(last.Content), len(long))
	}
}
