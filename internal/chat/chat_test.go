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
	"github.com/overspecific/blorb/internal/llm/ollama"
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
// agent named tester referencing the test model, with no tools.
func testAgent() config.Agent {
	return config.Agent{
		Name:         "tester",
		SystemPrompt: "You are helpful.",
		Model:        testModel().Name,
	}
}

// testModel returns the canonical top-level model entry the test agents
// reference: an OpenAI-compatible model named gpt-test on the test
// provider, whose connection testProvider returns.
func testProvider() config.Provider {
	return config.Provider{
		Name:    "local",
		Type:    config.ModelTypeOpenAI,
		BaseURL: "http://localhost:1",
	}
}

func testModel() config.Model {
	return config.Model{
		Name:      "gpt-test",
		Provider:  "local",
		ModelName: "gpt-test",
	}
}

// testConfig wraps agent in a config carrying its named model and the
// given top-level tool declarations, granting the agent all of them.
func testConfig(agent config.Agent, tools ...config.ToolEntry) config.Config {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	agent.Tools = names
	return config.Config{
		Providers: []config.Provider{testProvider()},
		Models:    []config.Model{testModel()},
		Agents:    []config.Agent{agent},
		Tools:     tools,
	}
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

// TestRunProviderSamplingReachesEngine pins the plumbing: the agent's
// model's provider sampling defaults reach every llm.Request the session
// engine sends.
func TestRunProviderSamplingReachesEngine(t *testing.T) {
	t.Parallel()

	temp := 0.0 // explicit zero must survive the plumbing
	fc := &fakeClient{responses: []llm.Response{
		{Message: llm.NewTextMessage(llm.RoleAssistant, "hi"), FinishReason: llm.FinishStop},
	}}

	cfg := testConfig(testAgent())
	p := testProvider()
	p.Temperature = &temp
	cfg.Providers = []config.Provider{p}

	var stdout strings.Builder
	o := chat.Options{
		Config:  cfg,
		Agent:   cfg.Agents[0],
		Version: "test",
		Stdin:   strings.NewReader("hello\nexit\n"),
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
	req := fc.requests[0]
	if req.Sampling.Temperature == nil || *req.Sampling.Temperature != 0 {
		t.Errorf("request Sampling.Temperature = %v, want the provider's explicit 0", req.Sampling.Temperature)
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
	cfg := config.Config{Providers: []config.Provider{testProvider()}, Models: []config.Model{testModel()}, Agents: []config.Agent{agent}, Tools: tools}

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

	cfg := testConfig(testAgent())
	cfg.Tools = append(cfg.Tools, config.ToolEntry{
		Type:        config.ToolTypeCommand,
		Name:        "shared",
		Description: "Shared tool.",
		Command:     []string{"true"},
	})

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

// TestRunToolOutputSuppressedByDefault pins the --tool-output off state:
// the tool call heading and arguments still render, but the result block
// shows a char/line count instead of the output body.
func TestRunToolOutputSuppressedByDefault(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	File := filepath.Join(dir, "marker.txt")
	if err := os.WriteFile(File, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := testConfig(testAgent(), config.ToolEntry{
		Type:        config.ToolTypeCommand,
		Name:        "echoer",
		Description: "Echoes.",
		Command:     []string{"printf", "one\ntwo\nthree\nfour\n"},
	})

	toolCallResp := llm.Response{
		Message: llm.Message{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{{
				ID: "call_1", Type: "function", FunctionName: "echoer", FunctionArgs: "{}",
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
	if !strings.Contains(out, ">>> Tool: echoer") {
		t.Errorf("stdout = %q, want a tool call heading", out)
	}
	if !strings.Contains(out, ">>> Result: Tool: echoer") {
		t.Errorf("stdout = %q, want a tool result heading", out)
	}
	// The output body is replaced by the count summary.
	if !strings.Contains(out, "18 characters, 4 lines") {
		t.Errorf("stdout = %q, want a 18 characters, 4 lines count line", out)
	}
	if strings.Contains(out, "\none\n") {
		t.Errorf("stdout = %q, want no tool output body", out)
	}
}

// TestRunToolOutputFlagShowsOutput pins the --tool-output on state: the
// result block shows the full output body, not the count summary.
func TestRunToolOutputFlagShowsOutput(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	File := filepath.Join(dir, "marker.txt")
	if err := os.WriteFile(File, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := testConfig(testAgent(), config.ToolEntry{
		Type:        config.ToolTypeCommand,
		Name:        "echoer",
		Description: "Echoes.",
		Command:     []string{"printf", "one\ntwo\nthree\nfour\n"},
	})

	toolCallResp := llm.Response{
		Message: llm.Message{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{{
				ID: "call_1", Type: "function", FunctionName: "echoer", FunctionArgs: "{}",
			}},
		},
		FinishReason: llm.FinishToolCalls,
	}

	o, stdout := newTestOptions(cfg, "make the marker\nexit\n",
		toolCallResp,
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "made it"), FinishReason: llm.FinishStop},
	)
	o.ToolOutput = true

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	out := stdout.String()
	if !strings.Contains(out, ">>> Result: Tool: echoer\none\ntwo\nthree\nfour") {
		t.Errorf("stdout = %q, want the full tool output body", out)
	}
	if strings.Contains(out, "characters") {
		t.Errorf("stdout = %q, want no count summary", out)
	}
}

// TestRunFailedToolResultAlwaysShowsOutput pins the failure carve-out:
// even with tool output suppressed, a failed tool result shows its full
// output so the user can diagnose the failure.
func TestRunFailedToolResultAlwaysShowsOutput(t *testing.T) {
	t.Parallel()

	cfg := testConfig(testAgent(), config.ToolEntry{
		Type:        config.ToolTypeCommand,
		Name:        "failer",
		Description: "Fails.",
		Command:     []string{"sh", "-c", "echo boom >&2; exit 3"},
	})

	toolCallResp := llm.Response{
		Message: llm.Message{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{{
				ID: "call_1", Type: "function", FunctionName: "failer", FunctionArgs: "{}",
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
	if !strings.Contains(out, ">>> Error: Tool: failer") {
		t.Errorf("stdout = %q, want a failed tool result heading", out)
	}
	if !strings.Contains(out, "boom") {
		t.Errorf("stdout = %q, want the full failure output", out)
	}
}

// TestRunToolOutputStreamingPinsCountLine pins the streaming path: with
// tool output suppressed, the streamed arguments leave the stream mid-line
// and the count-summary result block still terminates the partial line with
// its blank-line separator.
func TestRunToolOutputStreamingPinsCountLine(t *testing.T) {
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
	if strings.Count(out, "\n\n>>> Result: Tool: touch") != 1 {
		t.Errorf("stdout = %q, want a blank line between the arguments and the result heading", out)
	}
	if strings.Count(out, "0 characters, 0 lines") != 1 {
		t.Errorf("stdout = %q, want exactly one count line for the empty output", out)
	}
}

// TestRunSubagentResultShowsCountsForParent pins the subagent carve-out's
// parent side: with tool output suppressed, the parent's ask_worker result
// block shows a count summary instead of the subagent's final text.
func TestRunSubagentResultShowsCountsForParent(t *testing.T) {
	t.Parallel()

	cfg := subagentTestConfig(t)
	parent, worker := subagentClients("worker result text")
	var stdout strings.Builder
	o := chat.Options{
		Config:  cfg,
		Agent:   cfg.Agents[0],
		Version: "test",
		Stdin:   strings.NewReader("go\nexit\n"),
		Stdout:  &stdout,
		NewClient: func(_ config.Config, agent config.Agent) (llm.Client, error) {
			if agent.Name == "worker" {
				return worker, nil
			}
			return parent, nil
		},
	}

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	out := stdout.String()
	// The worker's activity always renders, suppression does not apply.
	if !strings.Contains(out, "[worker] >>> Assistant:\n  worker result text") {
		t.Errorf("stdout = %q, want the worker's live activity", out)
	}
	resultAt := strings.Index(out, ">>> Result: Tool: ask_worker")
	if resultAt < 0 {
		t.Fatalf("stdout = %q, want the parent's result heading", out)
	}
	// The parent's result body is the count summary, not the subagent text.
	// "worker result text" is 18 characters on one line (the tool layer
	// trims the trailing newline before the result event).
	resultTail := out[resultAt:]
	if !strings.Contains(resultTail, "18 characters, 1 line") {
		t.Errorf("result tail = %q, want a count summary", resultTail)
	}
	if strings.Contains(resultTail, "worker result text") {
		t.Errorf("result tail = %q, want no raw output", resultTail)
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
	cfg := testConfig(testAgent())

	t.Run("openai with key env set", func(t *testing.T) {
		t.Setenv("BLORB_TEST_KEY", "key-value")

		p := testProvider()
		p.APIKeyEnv = ptr("BLORB_TEST_KEY")
		cfg.Providers = []config.Provider{p}

		client, err := chat.NewClient(cfg, cfg.Agents[0])
		if err != nil || client == nil {
			t.Fatalf("NewClient error = %v, want a client", err)
		}
	})

	t.Run("openai with missing key env", func(t *testing.T) {
		p := testProvider()
		p.APIKeyEnv = ptr("BLORB_TEST_MISSING_KEY")
		cfg.Providers = []config.Provider{p}

		_, err := chat.NewClient(cfg, cfg.Agents[0])
		if err == nil || !strings.Contains(err.Error(), "BLORB_TEST_MISSING_KEY") {
			t.Errorf("error = %v, want a missing-env error naming the variable", err)
		}
	})

	t.Run("unknown provider type", func(t *testing.T) {
		p := testProvider()
		p.Type = "telepathy"
		cfg.Providers = []config.Provider{p}

		_, err := chat.NewClient(cfg, cfg.Agents[0])
		if err == nil || !strings.Contains(err.Error(), "telepathy") {
			t.Errorf("error = %v, want an unsupported-type error", err)
		}
	})

	t.Run("undefined model", func(t *testing.T) {
		agent := testAgent()
		agent.Model = "ghost"
		broken := config.Config{Providers: []config.Provider{testProvider()}, Models: []config.Model{testModel()}, Agents: []config.Agent{agent}}

		_, err := chat.NewClient(broken, agent)
		if err == nil || !strings.Contains(err.Error(), "ghost") {
			t.Errorf("error = %v, want an undefined-model error", err)
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
	p := testProvider()
	p.APIKeyEnv = ptr("INJECTED_MISSING_VAR")
	cfg := testConfig(agent)
	cfg.Providers = []config.Provider{p}

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
	p := testProvider()
	p.APIKeyEnv = ptr("INJECTED_ENV_VAR")
	cfg := testConfig(agent)
	cfg.Providers = []config.Provider{p}

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
// "provider" override is special-cased: it replaces the shared provider
// entry, since connection settings live in the top-level providers list.
func writeBlorbConfig(t *testing.T, dir string, extra map[string]any) string {
	t.Helper()

	provider := map[string]any{
		"name":     "local",
		"type":     "openai-compatible",
		"base_url": "placeholder",
	}
	model := map[string]any{
		"name":       "gpt-test",
		"provider":   "local",
		"model_name": "gpt-test",
	}
	agent := map[string]any{
		"name":          "logger",
		"system_prompt": "You are helpful.",
		"max_turns":     5,
		"model":         "gpt-test",
		"tools":         []string{"echo"},
	}
	base := map[string]any{
		"default_agent": "logger",
		"providers":     []map[string]any{provider},
		"models":        []map[string]any{model},
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
			base["providers"] = []map[string]any{v.(map[string]any)}
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
			"name":     "local",
			"type":     "openai-compatible",
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
			"name":     "local",
			"type":     "openai-compatible",
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
			"name":     "local",
			"type":     "openai-compatible",
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
			"name":     "local",
			"type":     "openai-compatible",
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

	cfg := testConfig(testAgent())
	p := testProvider()
	p.APIKeyEnv = ptr("BLORB_TEST_KEY")
	cfg.Providers = []config.Provider{p}

	client, err := chat.NewClientWithGetenv(cfg, cfg.Agents[0], os.Getenv, nil)
	if err != nil || client == nil {
		t.Fatalf("NewClientWithGetenv error = %v, want a client (nil sink = logging off)", err)
	}
}

// TestNewClientPlumbsReasoningEffort verifies a model entry carrying
// reasoning_effort builds a client that sends the effort on the wire: the
// httptest round trip goes through NewClientWithGetenv and the real openai
// client, covering the plumbing end to end.
func TestNewClientPlumbsReasoningEffort(t *testing.T) {
	t.Parallel()

	var gotEffort any
	var sawEffort bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var gotReq map[string]any
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Errorf("unmarshal request: %v", err)
		}
		gotEffort, sawEffort = gotReq["reasoning_effort"]
		_, _ = w.Write([]byte(`{"id":"r","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	p := testProvider()
	p.BaseURL = srv.URL
	m := testModel()
	m.ReasoningEffort = "high"
	cfg := config.Config{
		Providers: []config.Provider{p},
		Models:    []config.Model{m},
		Agents:    []config.Agent{testAgent()},
	}

	client, err := chat.NewClientWithGetenv(cfg, cfg.Agents[0], os.Getenv, nil)
	if err != nil || client == nil {
		t.Fatalf("NewClientWithGetenv error = %v, want a client", err)
	}
	if _, err := client.Chat(context.Background(), llm.Request{}); err != nil {
		t.Fatalf("Chat error = %v, want nil", err)
	}
	if !sawEffort || gotEffort != "high" {
		t.Errorf("wire reasoning_effort = %v (present: %v), want %q", gotEffort, sawEffort, "high")
	}
}

// ollamaTestProvider returns the canonical ollama provider for tests.
func ollamaTestProvider() config.Provider {
	return config.Provider{
		Name:    "local",
		Type:    config.ModelTypeOllama,
		BaseURL: "http://localhost:11434",
	}
}

// ollamaTestModel returns the canonical ollama model entry for tests: it
// names ollamaTestProvider's provider.
func ollamaTestModel() config.Model {
	return config.Model{
		Name:      "local-llama",
		Provider:  "local",
		ModelName: "llama3.1:latest",
	}
}

// TestNewClientOllama covers the ollama entry of the model-type registry:
// an ollama model entry builds an *ollama.Client that talks to /api/chat,
// the api_key_env pointer rule applies (missing env errors, absent env
// builds a key-less client), and reasoning_effort reaches the wire as the
// mapped think field.
func TestNewClientOllama(t *testing.T) {
	t.Parallel()

	agent := testAgent()
	agent.Model = "local-llama"

	t.Run("builds an ollama client and hits /api/chat", func(t *testing.T) {
		t.Parallel()

		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			_, _ = w.Write([]byte(`{"model":"llama3.1:latest","message":{"role":"assistant","content":"hi"},"done":true,"done_reason":"stop"}`))
		}))
		defer srv.Close()

		p := ollamaTestProvider()
		p.BaseURL = srv.URL
		cfg := config.Config{Providers: []config.Provider{p}, Models: []config.Model{ollamaTestModel()}, Agents: []config.Agent{agent}}

		client, err := chat.NewClientWithGetenv(cfg, cfg.Agents[0], os.Getenv, nil)
		if err != nil || client == nil {
			t.Fatalf("NewClientWithGetenv error = %v, want a client", err)
		}
		if _, ok := client.(*ollama.Client); !ok {
			t.Fatalf("client = %T, want *ollama.Client", client)
		}
		if _, err := client.Chat(context.Background(), llm.Request{}); err != nil {
			t.Fatalf("Chat error = %v, want nil", err)
		}
		if gotPath != "/api/chat" {
			t.Errorf("request path = %q, want %q", gotPath, "/api/chat")
		}
	})

	t.Run("api_key_env with empty env errors", func(t *testing.T) {
		t.Parallel()

		p := ollamaTestProvider()
		p.APIKeyEnv = ptr("BLORB_TEST_MISSING_OLLAMA_KEY")
		cfg := config.Config{Providers: []config.Provider{p}, Models: []config.Model{ollamaTestModel()}, Agents: []config.Agent{agent}}

		_, err := chat.NewClientWithGetenv(cfg, cfg.Agents[0], os.Getenv, nil)
		if err == nil || !strings.Contains(err.Error(), "BLORB_TEST_MISSING_OLLAMA_KEY") {
			t.Errorf("error = %v, want a missing-env error naming the variable", err)
		}
	})

	t.Run("api_key_env absent builds a client", func(t *testing.T) {
		t.Parallel()

		cfg := config.Config{Providers: []config.Provider{ollamaTestProvider()}, Models: []config.Model{ollamaTestModel()}, Agents: []config.Agent{agent}}

		client, err := chat.NewClientWithGetenv(cfg, cfg.Agents[0], os.Getenv, nil)
		if err != nil || client == nil {
			t.Fatalf("NewClientWithGetenv error = %v, want a client", err)
		}
	})

	t.Run("reasoning_effort maps to think on the wire", func(t *testing.T) {
		t.Parallel()

		var gotThink any
		var sawThink bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req map[string]any
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("unmarshal request: %v", err)
			}
			gotThink, sawThink = req["think"]
			_, _ = w.Write([]byte(`{"model":"llama3.1:latest","message":{"role":"assistant","content":"hi"},"done":true,"done_reason":"stop"}`))
		}))
		defer srv.Close()

		m := ollamaTestModel()
		m.ReasoningEffort = "none"
		p := ollamaTestProvider()
		p.BaseURL = srv.URL
		cfg := config.Config{Providers: []config.Provider{p}, Models: []config.Model{m}, Agents: []config.Agent{agent}}

		client, err := chat.NewClientWithGetenv(cfg, cfg.Agents[0], os.Getenv, nil)
		if err != nil || client == nil {
			t.Fatalf("NewClientWithGetenv error = %v, want a client", err)
		}
		if _, err := client.Chat(context.Background(), llm.Request{}); err != nil {
			t.Fatalf("Chat error = %v, want nil", err)
		}
		if !sawThink || gotThink != false {
			t.Errorf("wire think = %v (present: %v), want false (none maps to the boolean)", gotThink, sawThink)
		}
	})
}

// TestNewClientOllamaFormat pins that an ollama model entry carrying
// format builds a client that serializes it onto the wire, and that an
// ollama model without format omits it.
func TestNewClientOllamaFormat(t *testing.T) {
	t.Parallel()

	agent := testAgent()
	agent.Model = "local-llama"

	t.Run("format reaches the wire", func(t *testing.T) {
		t.Parallel()

		var gotFormat any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req map[string]any
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("unmarshal request: %v", err)
			}
			gotFormat = req["format"]
			_, _ = w.Write([]byte(`{"model":"llama3.1:latest","message":{"role":"assistant","content":"hi"},"done":true,"done_reason":"stop"}`))
		}))
		defer srv.Close()

		p := ollamaTestProvider()
		p.BaseURL = srv.URL
		m := ollamaTestModel()
		m.Format = json.RawMessage(`"json"`)
		cfg := config.Config{Providers: []config.Provider{p}, Models: []config.Model{m}, Agents: []config.Agent{agent}}

		client, err := chat.NewClientWithGetenv(cfg, cfg.Agents[0], os.Getenv, nil)
		if err != nil {
			t.Fatalf("NewClientWithGetenv error = %v, want nil", err)
		}
		if _, err := client.Chat(context.Background(), llm.Request{}); err != nil {
			t.Fatalf("Chat error = %v, want nil", err)
		}
		if gotFormat != "json" {
			t.Errorf("wire format = %v, want %q", gotFormat, "json")
		}
	})

	t.Run("unset format omits the field", func(t *testing.T) {
		t.Parallel()

		var sawFormat bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req map[string]any
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("unmarshal request: %v", err)
			}
			_, sawFormat = req["format"]
			_, _ = w.Write([]byte(`{"model":"llama3.1:latest","message":{"role":"assistant","content":"hi"},"done":true,"done_reason":"stop"}`))
		}))
		defer srv.Close()

		p := ollamaTestProvider()
		p.BaseURL = srv.URL
		cfg := config.Config{Providers: []config.Provider{p}, Models: []config.Model{ollamaTestModel()}, Agents: []config.Agent{agent}}

		client, err := chat.NewClientWithGetenv(cfg, cfg.Agents[0], os.Getenv, nil)
		if err != nil {
			t.Fatalf("NewClientWithGetenv error = %v, want nil", err)
		}
		if _, err := client.Chat(context.Background(), llm.Request{}); err != nil {
			t.Fatalf("Chat error = %v, want nil", err)
		}
		if sawFormat {
			t.Errorf("wire request carries format, want the field omitted")
		}
	})
}

// TestNewClientOllamaKeepAlive pins that an ollama model entry carrying
// keep_alive builds a client that sends it on the wire, and that an entry
// without it omits the field.
func TestNewClientOllamaKeepAlive(t *testing.T) {
	t.Parallel()

	agent := testAgent()
	agent.Model = "local-llama"

	run := func(keepAlive string, check func(t *testing.T, wire map[string]any)) *httptest.Server {
		var wireReq map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &wireReq); err != nil {
				t.Errorf("unmarshal request: %v", err)
			}
			_, _ = w.Write([]byte(`{"model":"llama3.1:latest","message":{"role":"assistant","content":"hi"},"done":true,"done_reason":"stop"}`))
		}))

		p := ollamaTestProvider()
		p.BaseURL = srv.URL
		m := ollamaTestModel()
		m.KeepAlive = keepAlive
		cfg := config.Config{Providers: []config.Provider{p}, Models: []config.Model{m}, Agents: []config.Agent{agent}}

		client, err := chat.NewClientWithGetenv(cfg, cfg.Agents[0], os.Getenv, nil)
		if err != nil {
			t.Fatalf("NewClientWithGetenv error = %v, want nil", err)
		}
		if _, err := client.Chat(context.Background(), llm.Request{}); err != nil {
			t.Fatalf("Chat error = %v, want nil", err)
		}
		check(t, wireReq)
		return srv
	}

	t.Run("keep_alive reaches the wire", func(t *testing.T) {
		t.Parallel()
		srv := run("5m", func(t *testing.T, wire map[string]any) {
			if got := wire["keep_alive"]; got != "5m" {
				t.Errorf("wire keep_alive = %v, want %q", got, "5m")
			}
		})
		defer srv.Close()
	})

	t.Run("unset keep_alive omits the field", func(t *testing.T) {
		t.Parallel()
		srv := run("", func(t *testing.T, wire map[string]any) {
			if _, present := wire["keep_alive"]; present {
				t.Errorf("wire request carries keep_alive = %v, want the field omitted", wire["keep_alive"])
			}
		})
		defer srv.Close()
	})
}

// subagentTestConfig builds a two-agent config: parent granted a subagent
// tool targeting worker, and worker granted a trivial command tool so a
// nested tool round trip is exercised.
func subagentTestConfig(t *testing.T) config.Config {
	t.Helper()

	worker := testAgent()
	worker.Name = "worker"
	worker.SystemPrompt = "You are a worker."
	worker.MaxTurns = 3
	parent := testAgent()
	parent.Name = "parent"
	parent.SystemPrompt = "You are the parent."
	parent.MaxTurns = 3

	cfg := config.Config{
		Providers: []config.Provider{testProvider()},
		Models:    []config.Model{testModel()},
		Agents:    []config.Agent{parent, worker},
		Tools: []config.ToolEntry{
			{
				Type:        config.ToolTypeSubagent,
				Name:        "ask_worker",
				Description: "Ask the worker.",
				Agent:       "worker",
			},
			{
				Type:        config.ToolTypeCommand,
				Name:        "worker_tool",
				Description: "The worker's tool.",
				Command:     []string{"true"},
			},
		},
	}
	cfg.Agents[0].Tools = []string{"ask_worker"}
	cfg.Agents[1].Tools = []string{"worker_tool"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("subagentTestConfig invalid: %v", err)
	}
	return cfg
}

// subagentClients builds the parent and worker fakes: the parent makes one
// subagent tool call then finishes; the worker runs one tool round then
// finishes with the final text.
func subagentClients(finalText string) (*fakeClient, *fakeClient) {
	parent := &fakeClient{responses: []llm.Response{
		{
			Message: llm.Message{
				Role:      llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{ID: "call_p", Type: "function", FunctionName: "ask_worker", FunctionArgs: `{"prompt":"do it"}`}},
			},
			FinishReason: llm.FinishToolCalls,
		},
		{
			Message:      llm.NewTextMessage(llm.RoleAssistant, "parent done"),
			FinishReason: llm.FinishStop,
		},
	}}
	worker := &fakeClient{responses: []llm.Response{
		{
			Message: llm.Message{
				Role:      llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{ID: "call_w", Type: "function", FunctionName: "worker_tool", FunctionArgs: "{}"}},
			},
			FinishReason: llm.FinishToolCalls,
		},
		{
			Message:      llm.NewTextMessage(llm.RoleAssistant, finalText),
			FinishReason: llm.FinishStop,
		},
	}}
	return parent, worker
}

// TestRunSubagentToolRendersActivity is the end-to-end chat test: the
// parent delegates to the worker, whose nested activity renders indented
// and labeled on stdout, and whose final text becomes the parent's tool
// result. Tool output is enabled: the test asserts the parent's result
// body, which the default summary line replaces.
func TestRunSubagentToolRendersActivity(t *testing.T) {
	t.Parallel()

	cfg := subagentTestConfig(t)
	parent, worker := subagentClients("worker result text")
	var stdout strings.Builder
	o := chat.Options{
		Config:     cfg,
		Agent:      cfg.Agents[0],
		Version:    "test",
		Stdin:      strings.NewReader("go\nexit\n"),
		Stdout:     &stdout,
		ToolOutput: true,
		NewClient: func(_ config.Config, agent config.Agent) (llm.Client, error) {
			if agent.Name == "worker" {
				return worker, nil
			}
			return parent, nil
		},
	}

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	out := stdout.String()
	// Order: the parent's tool call heading, the indented worker activity,
	// then the parent's tool result carrying the worker's final text.
	toolCallAt := strings.Index(out, ">>> Tool: ask_worker")
	workerAssistantAt := strings.Index(out, "[worker] >>> Assistant:")
	workerToolAt := strings.Index(out, "[worker] >>> Tool: worker_tool")
	resultAt := strings.Index(out, ">>> Result: Tool: ask_worker")
	if toolCallAt < 0 || workerAssistantAt < 0 || workerToolAt < 0 || resultAt < 0 {
		t.Fatalf("stdout = %q, want the subagent call, activity, and result headings", out)
	}
	if !(toolCallAt < workerToolAt && workerToolAt < workerAssistantAt && workerAssistantAt < resultAt) {
		t.Errorf("stdout = %q, want tool call, worker activity, then result in order", out)
	}

	// The worker's whole-message blocks carry its final text, indented.
	if !strings.Contains(out, "[worker] >>> Assistant:\n  worker result text") {
		t.Errorf("stdout = %q, want the worker's final text under an indented labeled heading", out)
	}
	// The parent's tool result body is the subagent's final text.
	resultTail := out[resultAt:]
	if !strings.Contains(resultTail, "worker result text") {
		t.Errorf("result tail = %q, want the subagent output as the tool result", resultTail)
	}

	// The parent's request carried the subagent tool definition with the
	// default prompt schema.
	var def *llm.Tool
	for i := range parent.requests[0].Tools {
		if parent.requests[0].Tools[i].Name == "ask_worker" {
			def = &parent.requests[0].Tools[i]
		}
	}
	if def == nil {
		t.Fatal("parent request carried no ask_worker definition")
	}
	if !strings.Contains(string(def.Parameters), `"prompt"`) {
		t.Errorf("definition parameters = %s, want the default prompt schema", def.Parameters)
	}
	// The worker got the prompt from the parent's tool call as its message.
	wMsgs := worker.requests[0].Messages
	if wMsgs[len(wMsgs)-1].Role != llm.RoleUser || wMsgs[len(wMsgs)-1].Content != "do it" {
		t.Errorf("worker user message = %+v, want the prompt from the tool call", wMsgs[len(wMsgs)-1])
	}
}

// TestRunSubagentStreamedDeltas verifies the streaming variant: the
// subagent's deltas render inline under its labeled heading while the
// parent's own deltas still render.
func TestRunSubagentStreamedDeltas(t *testing.T) {
	t.Parallel()

	cfg := subagentTestConfig(t)

	parent := &streamingFakeClient{responses: []llm.Response{
		{
			Message: llm.Message{
				Role:      llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{ID: "call_p", Type: "function", FunctionName: "ask_worker", FunctionArgs: `{"prompt":"do it"}`}},
			},
			FinishReason: llm.FinishToolCalls,
		},
		{
			Message:      llm.NewTextMessage(llm.RoleAssistant, "parent done"),
			FinishReason: llm.FinishStop,
		},
	}}
	worker := &streamingFakeClient{responses: []llm.Response{
		{
			Message: llm.Message{
				Role:      llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{ID: "call_w", Type: "function", FunctionName: "worker_tool", FunctionArgs: "{}"}},
			},
			FinishReason: llm.FinishToolCalls,
		},
		{
			Message:      llm.NewTextMessage(llm.RoleAssistant, "worker streamed answer"),
			FinishReason: llm.FinishStop,
		},
	}}

	var stdout syncBuffer
	o := chat.Options{
		Config:  cfg,
		Agent:   cfg.Agents[0],
		Version: "test",
		Stdin:   strings.NewReader("go\nexit\n"),
		Stdout:  &stdout,
		NewClient: func(_ config.Config, agent config.Agent) (llm.Client, error) {
			if agent.Name == "worker" {
				return worker, nil
			}
			return parent, nil
		},
		Stream: true,
	}

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	out := stdout.String()
	// The subagent's text deltas render inline under its labeled heading.
	if !strings.Contains(out, "[worker] >>> Assistant:\nworker streamed answer") {
		t.Errorf("stdout = %q, want the worker's streamed text under its labeled heading", out)
	}
	// The parent's own deltas still render under its unlabeled heading.
	if !strings.Contains(out, ">>> Assistant:\nparent done\n") {
		t.Errorf("stdout = %q, want the parent's streamed text under its own heading", out)
	}
	// The worker's tool heading is labeled and indented.
	if !strings.Contains(out, "  [worker] >>> Tool: worker_tool") {
		t.Errorf("stdout = %q, want an indented labeled worker tool heading", out)
	}
	// The parent's result heading terminates the worker's partial line.
	if !strings.Contains(out, ">>> Result: Tool: ask_worker") {
		t.Errorf("stdout = %q, want the parent's result heading", out)
	}
}

// TestRunSubagentStreamedTwiceInOneTurn pins the round-boundary reset of
// the per-(depth, agent) subagent heading state: the same subagent invoked
// twice in one streamed turn must get its labeled heading each time, since
// the parent's tool result ends the round.
func TestRunSubagentStreamedTwiceInOneTurn(t *testing.T) {
	t.Parallel()

	cfg := subagentTestConfig(t)

	subCall := llm.ToolCall{ID: "call_p", Type: "function", FunctionName: "ask_worker", FunctionArgs: `{"prompt":"do it"}`}
	parent := &streamingFakeClient{responses: []llm.Response{
		{
			Message:      llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{subCall}},
			FinishReason: llm.FinishToolCalls,
		},
		{
			Message:      llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{subCall}},
			FinishReason: llm.FinishToolCalls,
		},
		{
			Message:      llm.NewTextMessage(llm.RoleAssistant, "parent done"),
			FinishReason: llm.FinishStop,
		},
	}}
	worker := &streamingFakeClient{responses: []llm.Response{
		{Message: llm.NewTextMessage(llm.RoleAssistant, "first answer"), FinishReason: llm.FinishStop},
		{Message: llm.NewTextMessage(llm.RoleAssistant, "second answer"), FinishReason: llm.FinishStop},
	}}

	var stdout syncBuffer
	o := chat.Options{
		Config:  cfg,
		Agent:   cfg.Agents[0],
		Version: "test",
		Stdin:   strings.NewReader("go\nexit\n"),
		Stdout:  &stdout,
		NewClient: func(_ config.Config, agent config.Agent) (llm.Client, error) {
			if agent.Name == "worker" {
				return worker, nil
			}
			return parent, nil
		},
		Stream: true,
	}

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	out := stdout.String()
	if got := strings.Count(out, "[worker] >>> Assistant:"); got != 2 {
		t.Errorf("stdout = %q, want %d labeled worker assistant headings (one per invocation), got %d", out, 2, got)
	}
	if !strings.Contains(out, "[worker] >>> Assistant:\nfirst answer") {
		t.Errorf("stdout = %q, want the first invocation's text under its heading", out)
	}
	if !strings.Contains(out, "[worker] >>> Assistant:\nsecond answer") {
		t.Errorf("stdout = %q, want the second invocation's text under its heading", out)
	}
}

// TestRunSubagentNestedStreamedTwiceInOneTurn pins the reset of the whole
// subagent heading map on a subagent's own tool result: a three-level chain
// (parent -> b -> c) where b calls c twice in one turn must label c's
// second invocation too. The tool-result event carries the calling agent's
// identity (b), not the called agent's (c), so resetting only b's state
// would orphan c's and drop its second heading.
func TestRunSubagentNestedStreamedTwiceInOneTurn(t *testing.T) {
	t.Parallel()

	parent := testAgent()
	parent.Name = "parent"
	parent.SystemPrompt = "parent"
	parent.MaxTurns = 3
	b := testAgent()
	b.Name = "b"
	b.SystemPrompt = "b"
	b.MaxTurns = 3
	c := testAgent()
	c.Name = "c"
	c.SystemPrompt = "c"
	c.MaxTurns = 3

	cfg := config.Config{
		Providers: []config.Provider{testProvider()},
		Models:    []config.Model{testModel()},
		Agents:    []config.Agent{parent, b, c},
		Tools: []config.ToolEntry{
			{Type: config.ToolTypeSubagent, Name: "ask_b", Description: "ask b", Agent: "b"},
			{Type: config.ToolTypeSubagent, Name: "ask_c", Description: "ask c", Agent: "c"},
		},
	}
	cfg.Agents[0].Tools = []string{"ask_b"}
	cfg.Agents[1].Tools = []string{"ask_c"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}

	askC := llm.ToolCall{ID: "call_c", Type: "function", FunctionName: "ask_c", FunctionArgs: `{"prompt":"go"}`}
	askB := llm.ToolCall{ID: "call_b", Type: "function", FunctionName: "ask_b", FunctionArgs: `{"prompt":"go"}`}

	parentClient := &streamingFakeClient{responses: []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{askB}}, FinishReason: llm.FinishToolCalls},
		{Message: llm.NewTextMessage(llm.RoleAssistant, "parent done"), FinishReason: llm.FinishStop},
	}}
	bClient := &streamingFakeClient{responses: []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{askC}}, FinishReason: llm.FinishToolCalls},
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{askC}}, FinishReason: llm.FinishToolCalls},
		{Message: llm.NewTextMessage(llm.RoleAssistant, "b done"), FinishReason: llm.FinishStop},
	}}
	cClient := &streamingFakeClient{responses: []llm.Response{
		{Message: llm.NewTextMessage(llm.RoleAssistant, "c first"), FinishReason: llm.FinishStop},
		{Message: llm.NewTextMessage(llm.RoleAssistant, "c second"), FinishReason: llm.FinishStop},
	}}

	var stdout syncBuffer
	o := chat.Options{
		Config:  cfg,
		Agent:   cfg.Agents[0],
		Version: "test",
		Stdin:   strings.NewReader("go\nexit\n"),
		Stdout:  &stdout,
		NewClient: func(_ config.Config, agent config.Agent) (llm.Client, error) {
			switch agent.Name {
			case "b":
				return bClient, nil
			case "c":
				return cClient, nil
			}
			return parentClient, nil
		},
		Stream: true,
	}

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	out := stdout.String()
	if got := strings.Count(out, "[c] >>> Assistant:"); got != 2 {
		t.Errorf("stdout = %q, want %d labeled c assistant headings (one per invocation), got %d", out, 2, got)
	}
	if !strings.Contains(out, "[c] >>> Assistant:\nc first") {
		t.Errorf("stdout = %q, want c's first text under its heading", out)
	}
	if !strings.Contains(out, "[c] >>> Assistant:\nc second") {
		t.Errorf("stdout = %q, want c's second text under its heading", out)
	}
}

// TestRunSubagentNonStreamingWholeBlocks pins the --no-stream variant:
// whole-message subagent blocks render indented and labeled.
func TestRunSubagentNonStreamingWholeBlocks(t *testing.T) {
	t.Parallel()

	cfg := subagentTestConfig(t)
	parent, worker := subagentClients("worker whole text")
	var stdout strings.Builder
	o := chat.Options{
		Config:  cfg,
		Agent:   cfg.Agents[0],
		Version: "test",
		Stdin:   strings.NewReader("go\nexit\n"),
		Stdout:  &stdout,
		NewClient: func(_ config.Config, agent config.Agent) (llm.Client, error) {
			if agent.Name == "worker" {
				return worker, nil
			}
			return parent, nil
		},
		Stream: false,
	}

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "  [worker] >>> Tool: worker_tool\n") {
		t.Errorf("stdout = %q, want a whole-message indented worker tool block", out)
	}
	if !strings.Contains(out, "  [worker] >>> Assistant:\n  worker whole text") || !strings.Contains(out, "worker whole text") {
		// The worker's final text appears under its labeled heading.
		if !strings.Contains(out, "[worker] >>> Assistant:") || !strings.Contains(out, "worker whole text") {
			t.Errorf("stdout = %q, want the worker's whole-message assistant block", out)
		}
	}
}

func TestRunUsageFooterPerTurnAndSession(t *testing.T) {
	t.Parallel()

	turnOne := llm.Response{
		Message:      llm.NewTextMessage(llm.RoleAssistant, "first"),
		FinishReason: llm.FinishStop,
		Usage:        llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
	turnTwo := llm.Response{
		Message:      llm.NewTextMessage(llm.RoleAssistant, "second"),
		FinishReason: llm.FinishStop,
		Usage:        llm.Usage{PromptTokens: 20, CompletionTokens: 7, TotalTokens: 27},
		// The second turn's client measured stats: its footer carries a
		// stats line, the first turn's (unmeasured) does not.
		Stats: llm.CallStats{
			Output:  llm.OutputBytes{Content: 4096},
			Elapsed: 4 * time.Second,
		},
	}
	o, stdout := newTestOptions(testConfig(testAgent()), "one\ntwo\nexit\n", turnOne, turnTwo)

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	out := stdout.String()
	firstFooter := "tester: 10 prompt, 5 completion, 15 total"
	secondFooter := "tester: 20 prompt, 7 completion, 27 total"
	if !strings.Contains(out, firstFooter) {
		t.Errorf("stdout = %q, want the first turn's footer %q", out, firstFooter)
	}
	if !strings.Contains(out, secondFooter) {
		t.Errorf("stdout = %q, want the second turn's footer %q", out, secondFooter)
	}
	session := "session tester: 30 prompt, 12 completion, 42 total"
	if !strings.Contains(out, session) {
		t.Errorf("stdout = %q, want the session totals line %q", out, session)
	}
	statsLine := "4s, 4KB output, "
	if n := strings.Count(out, statsLine); n != 2 {
		t.Errorf("stdout = %q, want the stats line twice (second turn + session), got %d", out, n)
	}
	// Turn rate: 7 completion tokens over 4s; session rate: 12 over 4s.
	if !strings.Contains(out, "1.8 tok/s") || !strings.Contains(out, "3.0 tok/s") {
		t.Errorf("stdout = %q, want both the turn rate 1.8 tok/s and the session rate 3.0 tok/s", out)
	}
	if strings.Contains(out, "tester: 10 prompt, 5 completion, 15 total, ") {
		t.Errorf("stdout = %q, want no stats part on the unmeasured first turn's footer", out)
	}
	if strings.LastIndex(out, firstFooter) > strings.Index(out, secondFooter) {
		t.Errorf("stdout = %q, want the first turn's footer before the second's", out)
	}
}

func TestRunUsageFooterOncePerToolRoundTurn(t *testing.T) {
	t.Parallel()

	cfg := testConfig(testAgent(), config.ToolEntry{
		Type:        config.ToolTypeCommand,
		Name:        "done_tool",
		Description: "Does nothing.",
		Command:     []string{"true"},
	})
	call := llm.Response{
		Message: llm.Message{
			Role:      llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{{ID: "call_1", Type: "function", FunctionName: "done_tool", FunctionArgs: "{}"}},
		},
		FinishReason: llm.FinishToolCalls,
		Usage:        llm.Usage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10},
	}
	final := llm.Response{
		Message:      llm.NewTextMessage(llm.RoleAssistant, "done"),
		FinishReason: llm.FinishStop,
		Usage:        llm.Usage{PromptTokens: 30, CompletionTokens: 4, TotalTokens: 34},
	}
	o, stdout := newTestOptions(cfg, "go\nexit\n", call, final)

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	out := stdout.String()
	// One turn footer with the turn's summed usage, not one per call; the
	// session line reuses the same totals, prefixed "session ".
	if n := strings.Count(out, "\ntester: 38 prompt, 6 completion, 44 total"); n != 1 {
		t.Errorf("stdout = %q, want exactly one summed turn footer, got %d", out, n)
	}
	if !strings.Contains(out, "session tester: 38 prompt, 6 completion, 44 total") {
		t.Errorf("stdout = %q, want the session totals line", out)
	}
	if strings.Contains(out, "tester: 8 prompt") {
		t.Errorf("stdout = %q, want no per-call footer", out)
	}
}

func TestRunUsageFooterWithSubagentSplit(t *testing.T) {
	t.Parallel()

	cfg := subagentTestConfig(t)
	parent, worker := subagentClients("worker result text")
	// The worker runs two LLM calls (tool round + final); the parent two.
	parent.responses[0].Usage = llm.Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110}
	parent.responses[1].Usage = llm.Usage{PromptTokens: 130, CompletionTokens: 20, TotalTokens: 150}
	worker.responses[0].Usage = llm.Usage{PromptTokens: 11, CompletionTokens: 2, TotalTokens: 13}
	worker.responses[1].Usage = llm.Usage{PromptTokens: 15, CompletionTokens: 3, TotalTokens: 18}

	var stdout syncBuffer
	o := chat.Options{
		Config:  cfg,
		Agent:   cfg.Agents[0],
		Version: "test",
		Stdin:   strings.NewReader("go\nexit\n"),
		Stdout:  &stdout,
		NewClient: func(_ config.Config, agent config.Agent) (llm.Client, error) {
			if agent.Name == "worker" {
				return worker, nil
			}
			return parent, nil
		},
	}

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	out := stdout.String()
	// The footer splits by agent: parent 230/30/260, worker 26/5/31.
	want := "parent: 230 prompt, 30 completion, 260 total\nworker: 26 prompt, 5 completion, 31 total\ntotal: 256 prompt, 35 completion, 291 total"
	if !strings.Contains(out, want) {
		t.Errorf("stdout = %q, want the per-agent footer split %q", out, want)
	}
}

func TestRunUsageFooterForInterruptedPartialTurn(t *testing.T) {
	t.Parallel()

	sigs := make(chan os.Signal, 1)
	cfg := subagentTestConfig(t)
	_, worker := subagentClients("worker result text")
	first := llm.Response{
		Message: llm.Message{
			Role:      llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{{ID: "call_p", Type: "function", FunctionName: "ask_worker", FunctionArgs: `{"prompt":"do it"}`}},
		},
		FinishReason: llm.FinishToolCalls,
		Usage:        llm.Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110},
	}

	// The parent's second call blocks until cancelled: the turn is cut
	// short after one usage-bearing response.
	client := &usageBlockingClient{first: first, started: make(chan struct{})}
	var stdout strings.Builder
	o := chat.Options{
		Config:     cfg,
		Agent:      cfg.Agents[0],
		Version:    "test",
		Stdin:      strings.NewReader("go\n"),
		Stdout:     &stdout,
		SigintChan: sigs,
		NewClient: func(_ config.Config, agent config.Agent) (llm.Client, error) {
			if agent.Name == "worker" {
				return worker, nil
			}
			return client, nil
		},
	}

	runErr := make(chan error, 1)
	go func() { runErr <- chat.Run(context.Background(), o) }()

	// Wait until the parent's second call is in flight, then interrupt.
	select {
	case <-client.started:
	case <-time.After(2 * time.Second):
		t.Fatal("second parent call never started")
	}
	sigs <- syscall.SIGINT

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after interrupt")
	}

	out := stdout.String()
	footerAt := strings.Index(out, "parent: 100 prompt, 10 completion, 110 total")
	interruptedAt := strings.Index(out, "(interrupted)")
	if footerAt < 0 || interruptedAt < 0 {
		t.Fatalf("stdout = %q, want the partial footer and the interrupted marker", out)
	}
	if footerAt > interruptedAt {
		t.Errorf("stdout = %q, want the footer before the (interrupted) line", out)
	}
}

// usageBlockingClient serves one canned response, then blocks on the next
// call until its context is cancelled, like an in-flight request cut short
// by SIGINT.
type usageBlockingClient struct {
	mu      sync.Mutex
	calls   int
	first   llm.Response
	started chan struct{}
}

func (c *usageBlockingClient) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	c.mu.Lock()
	c.calls++
	first := c.calls == 1
	c.mu.Unlock()
	if first {
		return &c.first, nil
	}
	close(c.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestRunNoUsageNoFooter(t *testing.T) {
	t.Parallel()

	// The fake has no responses: no call completes, so no footer and no
	// session line print.
	o, stdout := newTestOptions(testConfig(testAgent()), "hello\nexit\n")

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	out := stdout.String()
	if strings.Contains(out, "tokens:") {
		t.Errorf("stdout = %q, want no usage footer", out)
	}
	if strings.Contains(out, "session tokens:") {
		t.Errorf("stdout = %q, want no session totals line", out)
	}
}

func TestRunZeroUsageStillPrintsFooter(t *testing.T) {
	t.Parallel()

	zero := llm.Response{
		Message:      llm.NewTextMessage(llm.RoleAssistant, "answer"),
		FinishReason: llm.FinishStop,
		Usage:        llm.Usage{},
	}
	o, stdout := newTestOptions(testConfig(testAgent()), "hi\nexit\n", zero)

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "tester: 0 prompt, 0 completion, 0 total") {
		t.Errorf("stdout = %q, want the zero-usage footer", out)
	}
	if !strings.Contains(out, "session tester: 0 prompt, 0 completion, 0 total") {
		t.Errorf("stdout = %q, want the zero-usage session line", out)
	}
}
