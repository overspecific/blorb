package chat_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
		Agent:   cfg.Agents[0],
		Version: "test",
		Stdin:   strings.NewReader(input),
		Stdout:  &stdout,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
			return &fakeClient{responses: responses}, nil
		},
	}
	return o, &stdout
}

func newStreamingTestOptions(cfg config.Config, input string, responses ...llm.Response) (chat.Options, *syncBuffer) {
	var stdout syncBuffer
	o := chat.Options{
		Config:  cfg,
		Agent:   cfg.Agents[0],
		Version: "test",
		Stdin:   strings.NewReader(input),
		Stdout:  &stdout,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
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

// testAgent returns the canonical agent definition used by tests: an
// OpenAI-provider agent named tester with no tools.
func testAgent() config.Agent {
	return config.Agent{
		Name:         "tester",
		SystemPrompt: "You are helpful.",
		Provider: config.Provider{
			Type:    config.ProviderTypeOpenAI,
			Model:   "gpt-test",
			BaseURL: "http://localhost:1",
		},
	}
}

// testConfig wraps agent in a config carrying the given top-level tool
// declarations and grants the agent all of them.
func testConfig(agent config.Agent, tools ...config.ToolEntry) config.Config {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	agent.Tools = names
	return config.Config{Agents: []config.Agent{agent}, Tools: tools}
}

func TestRunPlainReplySession(t *testing.T) {
	t.Parallel()

	cfg := testConfig(testAgent())
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

// TestRunScopesToolsToTheAgent verifies the registry only carries the
// agent's granted tools: a top-level tool the agent does not list never
// reaches the model, and granted tools arrive in the agent's listed order
// (not the top-level declaration order).
func TestRunScopesToolsToTheAgent(t *testing.T) {
	t.Parallel()

	tools := []config.ToolEntry{
		{Type: config.ToolTypeCommand, Name: "shared", Description: "Shared tool.", Command: []string{"true"}},
		{Type: config.ToolTypeCommand, Name: "alpha", Description: "Alpha.", Command: []string{"true"}},
		{Type: config.ToolTypeCommand, Name: "beta", Description: "Beta tool.", Command: []string{"true"}},
	}
	agent := testAgent()
	agent.Tools = []string{"beta", "alpha"}
	cfg := config.Config{Agents: []config.Agent{agent}, Tools: tools}

	fc := &fakeClient{responses: []llm.Response{
		{Message: llm.NewTextMessage(llm.RoleAssistant, "ok"), FinishReason: llm.FinishStop},
	}}
	var stdout strings.Builder
	o := chat.Options{
		Config:  cfg,
		Agent:   agent,
		Version: "test",
		Stdin:   strings.NewReader("hi\nexit\n"),
		Stdout:  &stdout,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
			return fc, nil
		},
	}

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if len(fc.requests) != 1 {
		t.Fatalf("API calls = %d, want 1", len(fc.requests))
	}
	var names []string
	for _, tl := range fc.requests[0].Tools {
		names = append(names, tl.Name)
	}
	if got, want := fmt.Sprint(names), fmt.Sprint([]string{"beta", "alpha"}); got != want {
		t.Errorf("request tools = %s, want %s (agent's listed order; the shared tool must not appear)", got, want)
	}
}

// TestRunNoToolsAgentSendsNoTools verifies a no-tools agent's requests
// carry no tool definitions at all.
func TestRunNoToolsAgentSendsNoTools(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		Agents: []config.Agent{testAgent()},
		Tools: []config.ToolEntry{{
			Type:        config.ToolTypeCommand,
			Name:        "shared",
			Description: "Shared tool.",
			Command:     []string{"true"},
		}},
	}

	fc := &fakeClient{responses: []llm.Response{
		{Message: llm.NewTextMessage(llm.RoleAssistant, "ok"), FinishReason: llm.FinishStop},
	}}
	var stdout strings.Builder
	o := chat.Options{
		Config:  cfg,
		Agent:   cfg.Agents[0],
		Version: "test",
		Stdin:   strings.NewReader("hi\nexit\n"),
		Stdout:  &stdout,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
			return fc, nil
		},
	}

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if len(fc.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(fc.requests))
	}
	if len(fc.requests[0].Tools) != 0 {
		t.Errorf("request tools = %v, want none (no-tools agent)", fc.requests[0].Tools)
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
	o, stdout := newTestOptions(testConfig(testAgent()), "why?\nexit\n", thinkingResp)

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

	o, stdout := newStreamingTestOptions(testConfig(testAgent()), "hello\nexit\n",
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
	o, stdout := newStreamingTestOptions(testConfig(testAgent()), "why?\nexit\n", thinkingResp)
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

func TestRunStreamedSameToolTwiceInOneRound(t *testing.T) {
	t.Parallel()

	cfg := testConfig(testAgent(), config.ToolEntry{
		Type:        config.ToolTypeCommand,
		Name:        "touch1",
		Description: "Creates a marker file.",
		// Both calls echo their marker argument; the command never runs
		// the real touch tool, it just needs to succeed.
		Command: []string{"true"},
	})

	// Both tool calls use the same tool name; only the index differs.
	sameName := func(args string) llm.ToolCall {
		return llm.ToolCall{ID: "call_" + args, Type: "function", FunctionName: "touch1", FunctionArgs: args}
	}
	toolCallResp := llm.Response{
		Message: llm.Message{
			Role:      llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{sameName("{}"), sameName(`{"two":true}`)},
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
	// Each call to the same tool in one round gets its own heading.
	if got := strings.Count(out, ">>> Tool: touch1"); got != 2 {
		t.Errorf("stdout = %q, want two >>> Tool: touch1 headings (one per tool call), got %d", out, got)
	}
}

func TestRunStreamedToolCallDeltas(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	marker := filepath.Join(dir, "marker.txt")

	cfg := testConfig(testAgent(), config.ToolEntry{
		Type:        config.ToolTypeCommand,
		Name:        "touch",
		Description: "Creates a marker file.",
		Command:     []string{"touch", marker},
	})

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

	cfg := testConfig(testAgent(), config.ToolEntry{
		Type:        config.ToolTypeCommand,
		Name:        "touch",
		Description: "Creates a marker file.",
		Command:     []string{"touch", marker},
	})

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

	o, stdout := newStreamingTestOptions(testConfig(testAgent()), "hello\nexit\n",
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
	o, stdout := newStreamingTestOptions(testConfig(testAgent()), "hello\nexit\n")
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

	o, stdout := newTestOptions(testConfig(testAgent()), "hi\nquit\n",
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

	o, _ := newTestOptions(testConfig(testAgent()), "")

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
}

func TestRunEmptyLinesIgnored(t *testing.T) {
	t.Parallel()

	// Whitespace lines produce no API calls; the fake has no responses, so
	// any turn would fail the run.
	o, _ := newTestOptions(testConfig(testAgent()), "   \n\t\nexit\n")

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

	cfg := testConfig(testAgent(), config.ToolEntry{
		Type:        config.ToolTypeCommand,
		Name:        "write_marker",
		Description: "Creates a marker file.",
		Command:     []string{"touch", File},
	})

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

		o, _ := newTestOptions(testConfig(testAgent()), "hello\n")
		o.NewClient = func(config.Config, config.Agent) (llm.Client, error) {
			return nil, errors.New("no provider available")
		}

		err := chat.Run(context.Background(), o)
		if err == nil || !strings.Contains(err.Error(), "no provider available") {
			t.Errorf("error = %v, want the client construction error", err)
		}
	})

	t.Run("tool registry failure", func(t *testing.T) {
		t.Parallel()

		cfg := testConfig(testAgent(), config.ToolEntry{
			Type: config.ToolTypeCommand,
			Name: "bad", Description: "Bad tool.", Command: []string{},
		})

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
	o, stdout := newTestOptions(testConfig(testAgent()), "hello\nexit\n")

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
	cfg := testConfig(testAgent())

	// A client that blocks until its context is cancelled, i.e. an
	// in-flight request cut short by the interrupt.
	client := &blockingClient{started: make(chan struct{})}
	var stdout strings.Builder
	o := chat.Options{
		Config:  cfg,
		Agent:   cfg.Agents[0],
		Version: "test",
		Stdin:   strings.NewReader("start a long turn\nexit\n"),
		Stdout:  &stdout,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
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
	o, stdout := newTestOptions(testConfig(testAgent()), "")
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
	cfg := testConfig(testAgent())

	// First turn blocks until interrupted; second turn succeeds.
	client := &blockingClient{started: make(chan struct{})}
	var stdout strings.Builder
	o := chat.Options{
		Config:  cfg,
		Agent:   cfg.Agents[0],
		Version: "test",
		Stdin:   strings.NewReader("start a long turn\ntry again\nexit\n"),
		Stdout:  &stdout,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
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

		agent := testAgent()
		agent.Provider.APIKeyEnv = ptr("BLORB_TEST_KEY")

		client, err := chat.NewClient(testConfig(agent), agent)
		if err != nil || client == nil {
			t.Fatalf("NewClient error = %v, want a client", err)
		}
	})

	t.Run("openai with missing key env", func(t *testing.T) {
		agent := testAgent()
		agent.Provider.APIKeyEnv = ptr("BLORB_TEST_MISSING_KEY")

		_, err := chat.NewClient(testConfig(agent), agent)
		if err == nil || !strings.Contains(err.Error(), "BLORB_TEST_MISSING_KEY") {
			t.Errorf("error = %v, want a missing-env error naming the variable", err)
		}
	})

	t.Run("unknown provider type", func(t *testing.T) {
		agent := testAgent()
		agent.Provider.Type = "telepathy"

		_, err := chat.NewClient(testConfig(agent), agent)
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

	agent := testAgent()
	agent.Provider.APIKeyEnv = ptr("INJECTED_MISSING_VAR")
	cfg := testConfig(agent)

	// No NewClient override and an injected Getenv that resolves nothing:
	// Run must fail startup with the missing-env error, proving the
	// injected lookup was consulted.
	o := chat.Options{
		Config:  cfg,
		Agent:   cfg.Agents[0],
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
	agent := testAgent()
	agent.Provider.APIKeyEnv = ptr("INJECTED_ENV_VAR")
	cfg := testConfig(agent)

	var stdout strings.Builder
	o := chat.Options{
		Config:  cfg,
		Agent:   cfg.Agents[0],
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
	cfg := testConfig(testAgent())

	fc := &fakeClient{responses: []llm.Response{
		{Message: llm.NewTextMessage(llm.RoleAssistant, "ok"), FinishReason: llm.FinishStop},
	}}
	var stdout strings.Builder
	o := chat.Options{
		Config:  cfg,
		Agent:   cfg.Agents[0],
		Version: "test",
		Stdin:   strings.NewReader(long + "\nexit\n"),
		Stdout:  &stdout,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
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

// writeBlorbConfig writes a minimal valid config with the given raw JSON
// merged at the top level (as a raw map) and returns its path. The
// provider override is special-cased: it applies to the agent's provider,
// since provider settings live inside each agent definition.
func writeBlorbConfig(t *testing.T, dir string, extra map[string]any) string {
	t.Helper()

	agent := map[string]any{
		"name":          "logger",
		"system_prompt": "You are helpful.",
		"max_turns":     5,
		"provider": map[string]any{
			"type":     "openai",
			"model":    "gpt-test",
			"base_url": "placeholder",
		},
		"tools": []string{"echo"},
	}
	base := map[string]any{
		"default_agent": "logger",
		"agents":        []map[string]any{agent},
		"tools": []map[string]any{{
			"type":        "command",
			"name":        "echo",
			"description": "Echo stdin back.",
			"command":     []string{"sh", "-c", `printf '%s' "$(cat)" && echo`},
		}},
	}
	for k, v := range extra {
		if k == "provider" {
			agent["provider"] = v
			continue
		}
		base[k] = v
	}

	data, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	path := filepath.Join(dir, "blorb.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// runLoggedSession runs a real-client chat session against a canned server
// and returns the session subdirectory's .txt contents sorted lexically.
func runLoggedSession(t *testing.T, cfgPath, stdin string) []string {
	t.Helper()
	return runLoggedSessionWithDir(t, cfgPath, filepath.Join(filepath.Dir(cfgPath), ".logs"), stdin)
}

// logSessionDirs lists the subdirectories of logDir sorted lexically.
func logSessionDirs(t *testing.T, logDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", logDir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, filepath.Join(logDir, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

// runLoggedSessionWithDir runs a real-client chat session, verifies that
// logDir contains exactly one session subdirectory, and returns the .txt
// files inside that subdirectory sorted lexically.
func runLoggedSessionWithDir(t *testing.T, cfgPath, logDir, stdin string) []string {
	t.Helper()

	var stdout strings.Builder
	o, err := optionsFromConfigPath(t, cfgPath, stdin, &stdout)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	return sessionLogFiles(t, logDir)
}

// sessionLogFiles finds the single session subdirectory in logDir and
// returns its .txt files sorted lexically. The session dir must match the
// <timestamp>-<uuid> name.
func sessionLogFiles(t *testing.T, logDir string) []string {
	t.Helper()

	sessions := logSessionDirs(t, logDir)
	if len(sessions) != 1 {
		t.Fatalf("got %d session dirs in %s, want exactly 1: %v", len(sessions), logDir, sessions)
	}
	sessionRe := regexp.MustCompile(`^\d{8}T\d{6}-\d{9}-[0-9a-f]{32}$`)
	if !sessionRe.MatchString(filepath.Base(sessions[0])) {
		t.Fatalf("session dir name %q does not match <timestamp>-<uuid>", filepath.Base(sessions[0]))
	}

	names, err := sortLogNames(sessions[0])
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	return names
}

// optionsFromConfigPath builds chat.Options from a config file on disk,
// using the real (non-fake) client path so wire logging actually engages.
func optionsFromConfigPath(t *testing.T, cfgPath, stdin string, stdout io.Writer) (chat.Options, error) {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return chat.Options{}, err
	}
	return chat.Options{
		Config:     cfg,
		Agent:      cfg.Agents[0],
		Version:    "test",
		Stdin:      strings.NewReader(stdin),
		Stdout:     stdout,
		ConfigPath: cfgPath,
	}, nil
}

// sortLogNames lists the .txt files in dir sorted lexically.
func sortLogNames(dir string) ([]string, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*.txt"))
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// TestRunWritesLogFilesForToolRound is the end-to-end wire logging test: a
// full one-tool-round turn over the real OpenAI client must produce exactly
// six log files whose lexically sorted names follow the true sequence of
// the turn.
func TestRunWritesLogFilesForToolRound(t *testing.T) {
	t.Parallel()

	var calls int
	toolCallJSON := `{"id":"call_1","type":"function","function":{"name":"echo","arguments":"{\"text\":\"ping\"}"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = w.Write([]byte(fmt.Sprintf(`{"id":"resp-1","choices":[{"message":{"role":"assistant","content":"","tool_calls":[%s]},"finish_reason":"tool_calls"}]}`, toolCallJSON)))
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp-2","choices":[{"message":{"role":"assistant","content":"all done"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfgPath := writeBlorbConfig(t, dir, map[string]any{
		"provider": map[string]any{
			"type":     "openai",
			"model":    "gpt-test",
			"base_url": srv.URL,
		},
	})

	names := runLoggedSession(t, cfgPath, "do it\nexit\n")
	if len(names) != 6 {
		t.Fatalf("got %d log files, want 6: %v", len(names), names)
	}

	wantKinds := []string{
		"llm-request",
		"llm-response",
		"tool-request",
		"tool-result",
		"llm-request",
		"llm-response",
	}
	for i, want := range wantKinds {
		got := filepath.Base(names[i])
		if !strings.HasSuffix(got, "-"+want+".txt") {
			t.Errorf("sorted log %d: got %q, want suffix -%s.txt", i, got, want)
		}
	}

	// The tool request file names the tool and carries the args.
	toolReqData, err := os.ReadFile(names[2])
	if err != nil {
		t.Fatalf("read tool request log: %v", err)
	}
	if !strings.Contains(string(toolReqData), "echo") || !strings.Contains(string(toolReqData), "ping") {
		t.Errorf("tool request log = %q, want the tool name and args", toolReqData)
	}

	// An LLM request file carries the user message text.
	llmReqData, err := os.ReadFile(names[0])
	if err != nil {
		t.Fatalf("read llm request log: %v", err)
	}
	if !strings.Contains(string(llmReqData), "do it") {
		t.Errorf("llm request log = %q, want the user message text", llmReqData)
	}
}

func TestRunLoggingDisabledWritesNothing(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfgPath := writeBlorbConfig(t, dir, map[string]any{
		"provider": map[string]any{
			"type":     "openai",
			"model":    "gpt-test",
			"base_url": srv.URL,
		},
		"logging": map[string]any{"enabled": false},
	})

	var stdout strings.Builder
	o, err := optionsFromConfigPath(t, cfgPath, "do it\nexit\n", &stdout)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	if _, err := os.Stat(filepath.Join(dir, ".logs")); !os.IsNotExist(err) {
		t.Errorf("stat .logs error = %v, want IsNotExist", err)
	}
}

func TestRunCustomLogPath(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfgPath := writeBlorbConfig(t, dir, map[string]any{
		"provider": map[string]any{
			"type":     "openai",
			"model":    "gpt-test",
			"base_url": srv.URL,
		},
		"logging": map[string]any{"path": "mylogs"},
	})

	names := runLoggedSessionWithDir(t, cfgPath, filepath.Join(dir, "mylogs"), "do it\nexit\n")
	if len(names) == 0 {
		t.Fatal("got 0 log files, want at least one request/response pair")
	}
	for _, name := range names {
		if base := filepath.Base(filepath.Dir(filepath.Dir(name))); base != "mylogs" {
			t.Errorf("log file %q outside mylogs/<session>/", name)
		}
	}
}

// runSession runs a real-client chat session without asserting anything
// about the log directory shape.
func runSession(t *testing.T, cfgPath, stdin string) {
	t.Helper()
	var stdout strings.Builder
	o, err := optionsFromConfigPath(t, cfgPath, stdin, &stdout)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
}

// TestRunSequentialSessionsGetDistinctLogDirs is the per-conversation
// isolation test: two chat sessions against the same config each get their
// own session subdirectory, and each session's files land in its own.
func TestRunSequentialSessionsGetDistinctLogDirs(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfgPath := writeBlorbConfig(t, dir, map[string]any{
		"provider": map[string]any{
			"type":     "openai",
			"model":    "gpt-test",
			"base_url": srv.URL,
		},
	})

	runSession(t, cfgPath, "first\nexit\n")
	runSession(t, cfgPath, "second\nexit\n")

	sessions := logSessionDirs(t, filepath.Join(dir, ".logs"))
	if len(sessions) != 2 {
		t.Fatalf("got %d session dirs, want 2: %v", len(sessions), sessions)
	}
	for _, s := range sessions {
		names, err := sortLogNames(s)
		if err != nil {
			t.Fatalf("list logs in %s: %v", s, err)
		}
		if len(names) != 2 {
			t.Errorf("session %s has %d files, want 2 (one request/response pair)", filepath.Base(s), len(names))
		}
	}
}

func TestNewClientWithGetenvNilSink(t *testing.T) {
	t.Setenv("BLORB_TEST_KEY", "key-value")

	agent := testAgent()
	agent.Provider.APIKeyEnv = ptr("BLORB_TEST_KEY")

	client, err := chat.NewClientWithGetenv(testConfig(agent), agent, os.Getenv, nil)
	if err != nil || client == nil {
		t.Fatalf("NewClientWithGetenv error = %v, want a client (nil sink = logging off)", err)
	}
}
