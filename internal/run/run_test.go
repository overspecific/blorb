package run_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/overspecific/blorb/internal/chat"
	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/engine"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/run"
)

// runFakeClient returns canned responses in order and records requests
// (mirrors chat_test's fakeClient).
type runFakeClient struct {
	responses []llm.Response
	requests  []llm.Request
}

func (f *runFakeClient) Chat(_ context.Context, req llm.Request) (*llm.Response, error) {
	f.requests = append(f.requests, req)
	if len(f.responses) == 0 {
		return nil, errors.New("runFakeClient: no more canned responses")
	}
	resp := f.responses[0]
	f.responses = f.responses[1:]
	return &resp, nil
}

// runStreamingFakeClient breaks each canned response into a single delta
// per field before returning the complete response.
type runStreamingFakeClient struct {
	responses []llm.Response
}

func (f *runStreamingFakeClient) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return (&runFakeClient{responses: f.responses}).Chat(ctx, req)
}

func (f *runStreamingFakeClient) ChatStream(_ context.Context, req llm.Request, onDelta func(llm.Delta) error) (*llm.Response, error) {
	if len(f.responses) == 0 {
		return nil, errors.New("runStreamingFakeClient: no more canned responses")
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

// runTestAgent returns the canonical agent definition used by run tests.
func runTestAgent() config.Agent {
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

// runTestConfig wraps agent in a config carrying the given tool
// declarations and grants the agent all of them.
func runTestConfig(agent config.Agent, tools ...config.ToolEntry) config.Config {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	agent.Tools = names
	return config.Config{Agents: []config.Agent{agent}, Tools: tools}
}

// syncBuffer is a mutex-guarded strings.Builder.
type runSyncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *runSyncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *runSyncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestRunPlainReply(t *testing.T) {
	t.Parallel()

	cfg := runTestConfig(runTestAgent())
	var stdout runSyncBuffer
	final, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		NewClient: func(config.Agent) (llm.Client, error) {
			return &runFakeClient{responses: []llm.Response{
				{Message: llm.NewTextMessage(llm.RoleAssistant, "the answer"), FinishReason: llm.FinishStop},
			}}, nil
		},
	}, "hello there")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if final != "the answer" {
		t.Errorf("final = %q, want %q", final, "the answer")
	}
	out := stdout.String()
	if !strings.Contains(out, ">>> Assistant:") || !strings.Contains(out, "the answer") {
		t.Errorf("stdout = %q, want the chat-style rendered reply", out)
	}
	if strings.Contains(out, ">>> User:") {
		t.Errorf("stdout = %q, want no >>> User heading (the prompt is never echoed)", out)
	}
}

func TestRunStreamedReply(t *testing.T) {
	t.Parallel()

	cfg := runTestConfig(runTestAgent())
	var stdout runSyncBuffer
	final, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		Stream: true,
		NewClient: func(config.Agent) (llm.Client, error) {
			return &runStreamingFakeClient{responses: []llm.Response{
				{Message: llm.NewTextMessage(llm.RoleAssistant, "streamed!"), FinishReason: llm.FinishStop},
			}}, nil
		},
	}, "hello")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if final != "streamed!" {
		t.Errorf("final = %q, want %q", final, "streamed!")
	}
	out := stdout.String()
	if !strings.Contains(out, ">>> Assistant:\nstreamed!\n") {
		t.Errorf("stdout = %q, want the streamed reply under one heading", out)
	}
	if strings.Count(out, ">>> Assistant:") != 1 {
		t.Errorf("stdout = %q, want exactly one Assistant heading", out)
	}
}

// runToolResponses builds the canned responses for a one-tool-round turn.
func runToolResponses(toolName string) []llm.Response {
	return []llm.Response{
		{
			Message: llm.Message{
				Role:      llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{ID: "call_1", Type: "function", FunctionName: toolName, FunctionArgs: "{}"}},
			},
			FinishReason: llm.FinishToolCalls,
		},
		{Message: llm.NewTextMessage(llm.RoleAssistant, "made it"), FinishReason: llm.FinishStop},
	}
}

func runToolConfig(t *testing.T) config.Config {
	t.Helper()
	return runTestConfig(runTestAgent(), config.ToolEntry{
		Type:        config.ToolTypeCommand,
		Name:        "echoer",
		Description: "Echoes.",
		Command:     []string{"printf", "one\ntwo\n"},
	})
}

func TestRunToolOutputSuppressedByDefault(t *testing.T) {
	t.Parallel()

	cfg := runToolConfig(t)
	var stdout runSyncBuffer
	_, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		NewClient: func(config.Agent) (llm.Client, error) {
			return &runFakeClient{responses: runToolResponses("echoer")}, nil
		},
	}, "run it")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	out := stdout.String()
	if !strings.Contains(out, ">>> Tool: echoer") {
		t.Errorf("stdout = %q, want a tool call heading", out)
	}
	if !strings.Contains(out, ">>> Result: Tool: echoer") {
		t.Errorf("stdout = %q, want a tool result heading", out)
	}
	// "one\ntwo\n" is 7 characters on 2 lines.
	if !strings.Contains(out, "7 characters, 2 lines") {
		t.Errorf("stdout = %q, want the count summary", out)
	}
	if strings.Contains(out, "\none\n") {
		t.Errorf("stdout = %q, want no raw tool output body", out)
	}
}

func TestRunToolOutputFlagShowsOutput(t *testing.T) {
	t.Parallel()

	cfg := runToolConfig(t)
	var stdout runSyncBuffer
	_, err := run.Run(context.Background(), run.Options{
		Config:     cfg,
		Agent:      cfg.Agents[0],
		Stdout:     &stdout,
		ToolOutput: true,
		NewClient: func(config.Agent) (llm.Client, error) {
			return &runFakeClient{responses: runToolResponses("echoer")}, nil
		},
	}, "run it")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	out := stdout.String()
	if !strings.Contains(out, ">>> Result: Tool: echoer\none\ntwo") {
		t.Errorf("stdout = %q, want the full tool output body", out)
	}
	if strings.Contains(out, "characters") {
		t.Errorf("stdout = %q, want no count summary", out)
	}
}

// runSubagentConfig builds a two-agent config: parent granted a subagent
// tool targeting worker, mirroring chat_test's shape.
func runSubagentConfig(t *testing.T) config.Config {
	t.Helper()

	parent := runTestAgent()
	parent.Name = "parent"
	parent.MaxTurns = 3
	worker := runTestAgent()
	worker.Name = "worker"
	worker.MaxTurns = 3

	cfg := config.Config{
		Agents: []config.Agent{parent, worker},
		Tools: []config.ToolEntry{
			{Type: config.ToolTypeSubagent, Name: "ask_worker", Description: "Ask the worker.", Agent: "worker"},
			{Type: config.ToolTypeCommand, Name: "worker_tool", Description: "Worker tool.", Command: []string{"true"}},
		},
	}
	cfg.Agents[0].Tools = []string{"ask_worker"}
	cfg.Agents[1].Tools = []string{"worker_tool"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	return cfg
}

func TestRunSubagentRendersActivity(t *testing.T) {
	t.Parallel()

	cfg := runSubagentConfig(t)
	parent := &runFakeClient{responses: []llm.Response{
		{
			Message: llm.Message{
				Role:      llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{ID: "call_p", Type: "function", FunctionName: "ask_worker", FunctionArgs: `{"prompt":"do it"}`}},
			},
			FinishReason: llm.FinishToolCalls,
		},
		{Message: llm.NewTextMessage(llm.RoleAssistant, "parent done"), FinishReason: llm.FinishStop},
	}}
	worker := &runFakeClient{responses: []llm.Response{
		{
			Message: llm.Message{
				Role:      llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{ID: "call_w", Type: "function", FunctionName: "worker_tool", FunctionArgs: "{}"}},
			},
			FinishReason: llm.FinishToolCalls,
		},
		{Message: llm.NewTextMessage(llm.RoleAssistant, "worker result text"), FinishReason: llm.FinishStop},
	}}

	var stdout runSyncBuffer
	final, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		NewClient: func(agent config.Agent) (llm.Client, error) {
			if agent.Name == "worker" {
				return worker, nil
			}
			return parent, nil
		},
	}, "go")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if final != "parent done" {
		t.Errorf("final = %q, want %q", final, "parent done")
	}

	out := stdout.String()
	// The worker's live activity renders indented and labeled via the
	// registry's WithSubagentEvents callback.
	if !strings.Contains(out, "[worker] >>> Assistant:") || !strings.Contains(out, "worker result text") {
		t.Errorf("stdout = %q, want the worker's indented labeled activity", out)
	}
	if !strings.Contains(out, ">>> Result: Tool: ask_worker") {
		t.Errorf("stdout = %q, want the parent's result heading", out)
	}
}

func TestRunClientError(t *testing.T) {
	t.Parallel()

	cfg := runTestConfig(runTestAgent())
	_, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &runSyncBuffer{},
		NewClient: func(config.Agent) (llm.Client, error) {
			return nil, errors.New("no provider available")
		},
	}, "hello")
	if err == nil || !strings.Contains(err.Error(), "no provider available") {
		t.Errorf("error = %v, want the client construction error", err)
	}
}

func TestRunTurnErrorWrapped(t *testing.T) {
	t.Parallel()

	cfg := runTestConfig(runTestAgent())
	_, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &runSyncBuffer{},
		NewClient: func(config.Agent) (llm.Client, error) {
			return &runFakeClient{}, nil // no responses: turn fails
		},
	}, "hello")
	if err == nil {
		t.Fatal("Run succeeded, want a turn error")
	}
	if !strings.HasPrefix(err.Error(), "run: ") {
		t.Errorf("error = %v, want the run: prefix", err)
	}
	if !strings.Contains(err.Error(), "no more canned responses") {
		t.Errorf("error = %v, want the underlying turn error", err)
	}
}

// blockingClient blocks in Chat until its context is cancelled.
type runBlockingClient struct {
	started chan struct{}
	once    sync.Once
}

func (b *runBlockingClient) Chat(ctx context.Context, _ llm.Request) (*llm.Response, error) {
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestRunCancelledContext(t *testing.T) {
	t.Parallel()

	cfg := runTestConfig(runTestAgent())
	client := &runBlockingClient{started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := run.Run(ctx, run.Options{
			Config: cfg,
			Agent:  cfg.Agents[0],
			Stdout: &runSyncBuffer{},
			NewClient: func(config.Agent) (llm.Client, error) {
				return client, nil
			},
		}, "hello")
		done <- err
	}()

	select {
	case <-client.started:
	case <-ctx.Done():
		t.Fatal("client never started")
	}
	cancel()

	if err := <-done; err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
}

// TestRunRegistryClosedCleanly covers the deferred registry.Close path: a
// real command tool with a temp dir runs through a full tool round and Run
// returns cleanly.
func TestRunRegistryClosedCleanly(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	marker := filepath.Join(dir, "marker.txt")
	cfg := runTestConfig(runTestAgent(), config.ToolEntry{
		Type:        config.ToolTypeCommand,
		Name:        "touch",
		Description: "Creates a marker file.",
		Command:     []string{"touch", marker},
	})

	var stdout runSyncBuffer
	_, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		NewClient: func(config.Agent) (llm.Client, error) {
			return &runFakeClient{responses: runToolResponses("touch")}, nil
		},
	}, "make the marker")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("marker stat error = %v, want the tool to have run", err)
	}
}

// TestRunWritesSessionLogDir mirrors chat's sink test: a config with file
// logging enabled creates the session log dir next to the config file.
func TestRunWritesSessionLogDir(t *testing.T) {
	t.Parallel()

	srvLogDirResponses := []llm.Response{
		{Message: llm.NewTextMessage(llm.RoleAssistant, "logged"), FinishReason: llm.FinishStop},
	}
	// The real client path needs a provider server; point base_url at a
	// dead port is fine only when NewClient is injected, so inject a fake
	// while still exercising the sink/config-path machinery.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "blorb.json")
	cfgJSON := `{
		"default_agent": "logger",
		"agents": [{
			"name": "logger",
			"system_prompt": "You are helpful.",
			"provider": {"type": "openai", "model": "gpt-test", "base_url": "http://localhost:1"},
			"max_turns": 1
		}],
		"tools": [],
		"logging": {"enabled": true}
	}`
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var stdout runSyncBuffer
	_, err = run.Run(context.Background(), run.Options{
		Config:     cfg,
		Agent:      cfg.Agents[0],
		Stdout:     &stdout,
		ConfigPath: cfgPath,
		NewClient: func(config.Agent) (llm.Client, error) {
			return &runFakeClient{responses: srvLogDirResponses}, nil
		},
	}, "hello")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	logDir := filepath.Join(dir, ".logs")
	entries, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v (want the session log dir created)", logDir, err)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		t.Errorf("log dir entries = %v, want exactly one session subdirectory", entries)
	}
}

// TestRunRegistryBuildFailure verifies a tool registry failure surfaces as
// a build-tools error, mirroring chat.
func TestRunRegistryBuildFailure(t *testing.T) {
	t.Parallel()

	cfg := runTestConfig(runTestAgent(), config.ToolEntry{
		Type: config.ToolTypeCommand,
		Name: "bad", Description: "Bad tool.", Command: []string{},
	})

	_, err := run.Run(context.Background(), run.Options{
		Config:    cfg,
		Agent:     cfg.Agents[0],
		Stdout:    &runSyncBuffer{},
		NewClient: func(config.Agent) (llm.Client, error) { return &runFakeClient{}, nil },
	}, "hello")
	if err == nil || !strings.Contains(err.Error(), "build tools") {
		t.Errorf("error = %v, want a build-tools error", err)
	}
}

// TestRunGetenvInjected verifies the injected env lookup is consulted when
// no NewClient override is set.
func TestRunGetenvInjected(t *testing.T) {
	t.Parallel()

	agent := runTestAgent()
	agent.Provider.APIKeyEnv = ptr("INJECTED_MISSING_VAR")
	cfg := runTestConfig(agent)

	_, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &runSyncBuffer{},
		Getenv: func(string) string { return "" },
	}, "hello")
	if err == nil || !strings.Contains(err.Error(), "INJECTED_MISSING_VAR") {
		t.Errorf("error = %v, want a missing-env error naming the variable", err)
	}
}

func ptr(s string) *string {
	return &s
}

// TestRunEventsExportMatchesChat pins that chat.Events (shared with run)
// still produces chat's exact rendering contract.
func TestRunEventsExportMatchesChat(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	printEvent, _, flush := chat.Events(&b, false)
	resp := llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "hi!"), FinishReason: llm.FinishStop}
	if err := printEvent(engine.Event{Kind: engine.EventAssistantText, Text: resp.Message.Content}); err != nil {
		t.Fatalf("printEvent: %v", err)
	}
	flush()
	if !strings.Contains(b.String(), ">>> Assistant:\nhi!") {
		t.Errorf("output = %q, want chat's assistant heading rendering", b.String())
	}
}
