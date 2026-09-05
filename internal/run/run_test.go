package run_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/overspecific/blorb/internal/chat"
	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/engine"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/prefactor"
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

// fragmentingClient wraps a streaming client and splits each tool call's
// arguments delta into two fragments, so consumers exercise client-side
// assembly of arguments fragments.
type fragmentingClient struct {
	inner struct{ runStreamingFakeClient }
}

func (f *fragmentingClient) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return f.inner.Chat(ctx, req)
}

func (f *fragmentingClient) ChatStream(ctx context.Context, req llm.Request, onDelta func(llm.Delta) error) (*llm.Response, error) {
	return f.inner.ChatStream(ctx, req, func(d llm.Delta) error {
		if tc := d.ToolCall; tc != nil && len(tc.Arguments) > 1 {
			half := len(tc.Arguments) / 2
			if err := onDelta(llm.Delta{ToolCall: &llm.ToolCallDelta{
				Index: tc.Index, ID: tc.ID, Type: tc.Type, Name: tc.Name, Arguments: tc.Arguments[:half],
			}}); err != nil {
				return err
			}
			return onDelta(llm.Delta{ToolCall: &llm.ToolCallDelta{
				Index: tc.Index, Arguments: tc.Arguments[half:],
			}})
		}
		return onDelta(d)
	})
}

// runTestModel returns the canonical top-level model entry the run test
// agents reference.
func runTestModel() config.Model {
	return config.Model{
		Name:      "gpt-test",
		Type:      config.ModelTypeOpenAI,
		ModelName: "gpt-test",
		BaseURL:   "http://localhost:1",
	}
}

// runTestAgent returns the canonical agent definition used by run tests.
func runTestAgent() config.Agent {
	return config.Agent{
		Name:         "tester",
		SystemPrompt: "You are helpful.",
		Model:        runTestModel().Name,
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
	return config.Config{Models: []config.Model{runTestModel()}, Agents: []config.Agent{agent}, Tools: tools}
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

// runFailOnWriter wraps a writer and fails every write containing
// marker; other writes pass through. Test fixture for failing a specific
// line in a stream (e.g. the ndjson terminal event's write).
type runFailOnWriter struct {
	w      io.Writer
	marker string
}

func (s *runFailOnWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), s.marker) {
		return 0, errors.New("write refused")
	}
	return s.w.Write(p)
}

func TestRunPlainReply(t *testing.T) {
	t.Parallel()

	cfg := runTestConfig(runTestAgent())
	var stdout runSyncBuffer
	final, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
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
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
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
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
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
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
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
		Models: []config.Model{runTestModel()},
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
		NewClient: func(_ config.Config, agent config.Agent) (llm.Client, error) {
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
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
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
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
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
			NewClient: func(config.Config, config.Agent) (llm.Client, error) {
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
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
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
		"models": [{"name": "gpt-test", "type": "openai-compatible", "model_name": "gpt-test", "base_url": "http://localhost:1"}],
		"agents": [{
			"name": "logger",
			"system_prompt": "You are helpful.",
			"model": "gpt-test",
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
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
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
		NewClient: func(config.Config, config.Agent) (llm.Client, error) { return &runFakeClient{}, nil },
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
	cfg := runTestConfig(agent)
	m := runTestModel()
	m.APIKeyEnv = ptr("INJECTED_MISSING_VAR")
	cfg.Models = []config.Model{m}

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

// --- Prefactor tracing tests (mirror chat's tracewrap_test fake server) ---

// runPFServer is a minimal Prefactor API over httptest, recording the
// calls it receives.
type runPFServer struct {
	mu    sync.Mutex
	calls []runPFCall
	next  int

	// spanTerminate, when true, the first span create responds with
	// control.terminate and the given reason.
	spanTerminate   bool
	terminateReason string
	terminated      bool
	// terminateAfter, when non-negative, responds to the span create
	// after that many successful span creates with control.terminate and
	// terminateReason, letting a test fire the terminate mid-turn (after
	// StartTurn's two span creates). Negative disables count-based
	// termination.
	terminateAfter  int
	spanCreateCount int
	// registerTerminate, when true, the instance register call responds
	// with control.terminate and the given reason (firing the
	// StartSession terminate branch).
	registerTerminate bool
	// spanFailStatus, when non-zero, is served for span creates.
	spanFailStatus int
	// registerFailStatus, when non-zero, is served for the instance
	// register call.
	registerFailStatus int
	// finishFailPrefix, when non-empty, makes /finish requests to paths
	// with this prefix fail with finishFailStatus.
	finishFailPrefix string
	finishFailStatus int
	// spanFinishFailStatus, when non-zero, is served for span finishes
	// whose result_payload carries spanFinishFailKey (empty key: all
	// span finishes). This lets a test fail the turn-span finish (its
	// payload carries assistant_text) while the llm-span finishes still
	// succeed.
	spanFinishFailStatus int
	spanFinishFailKey    string
}

type runPFCall struct {
	Path string
	Body map[string]any
}

func newRunPFServer(t *testing.T) (*runPFServer, *httptest.Server) {
	t.Helper()
	f := &runPFServer{next: 1, terminateAfter: -1}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *runPFServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.calls = append(f.calls, runPFCall{Path: r.URL.Path, Body: body})
	terminate := f.spanTerminate && !f.terminated && r.URL.Path == "/agent_spans"
	countTerminate := false
	if f.terminateAfter >= 0 && r.URL.Path == "/agent_spans" {
		f.spanCreateCount++
		countTerminate = !f.terminated && f.spanCreateCount > f.terminateAfter
		if countTerminate {
			f.terminated = true
		}
	}
	if terminate {
		f.terminated = true
	}
	spanFail := f.spanFailStatus != 0 && r.URL.Path == "/agent_spans"
	registerFail := f.registerFailStatus != 0 && r.URL.Path == "/agent_instance/register"
	spanFinishFail := f.spanFinishFailStatus != 0 &&
		strings.HasPrefix(r.URL.Path, "/agent_spans/") && strings.HasSuffix(r.URL.Path, "/finish") &&
		(f.spanFinishFailKey == "" || spanFinishHasKey(body, f.spanFinishFailKey))
	finishFail := f.finishFailPrefix != "" &&
		strings.HasPrefix(r.URL.Path, f.finishFailPrefix) && strings.HasSuffix(r.URL.Path, "/finish")
	f.mu.Unlock()

	if registerFail {
		writeRunJSON(w, f.registerFailStatus, map[string]any{
			"error": map[string]any{"code": "boom", "message": "register refused"},
		})
		return
	}
	if countTerminate {
		writeRunJSON(w, http.StatusOK, map[string]any{
			"status":  "success",
			"control": map[string]any{"terminate": true, "reason": f.terminateReason},
			"details": map[string]any{"id": "span-term"},
		})
		return
	}
	if spanFinishFail {
		writeRunJSON(w, f.spanFinishFailStatus, map[string]any{
			"error": map[string]any{"code": "boom", "message": "span finish refused"},
		})
		return
	}
	if f.registerTerminate {
		f.mu.Lock()
		first := !f.terminated
		f.terminated = true
		f.mu.Unlock()
		if first {
			writeRunJSON(w, http.StatusOK, map[string]any{
				"status":  "success",
				"control": map[string]any{"terminate": true, "reason": f.terminateReason},
				"details": map[string]any{"id": "inst-1"},
			})
			return
		}
	}
	if spanFail {
		writeRunJSON(w, f.spanFailStatus, map[string]any{
			"error": map[string]any{"code": "boom", "message": "nope"},
		})
		return
	}
	if finishFail {
		writeRunJSON(w, f.finishFailStatus, map[string]any{
			"error": map[string]any{"code": "boom", "message": "finish refused"},
		})
		return
	}
	if terminate {
		writeRunJSON(w, http.StatusOK, map[string]any{
			"status":  "success",
			"control": map[string]any{"terminate": true, "reason": f.terminateReason},
			"details": map[string]any{"id": "span-term"},
		})
		return
	}
	switch {
	case r.URL.Path == "/agent_instance/register":
		writeRunJSON(w, http.StatusOK, map[string]any{
			"status":  "success",
			"details": map[string]any{"id": "inst-1"},
		})
	case r.URL.Path == "/agent_spans":
		writeRunJSON(w, http.StatusOK, map[string]any{
			"status":  "success",
			"control": map[string]any{"terminate": false},
			"details": map[string]any{"id": "span"},
		})
	case strings.HasPrefix(r.URL.Path, "/agent_instance/") && strings.HasSuffix(r.URL.Path, "/start"),
		strings.HasPrefix(r.URL.Path, "/agent_instance/") && strings.HasSuffix(r.URL.Path, "/finish"),
		strings.HasPrefix(r.URL.Path, "/agent_spans/") && strings.HasSuffix(r.URL.Path, "/finish"):
		writeRunJSON(w, http.StatusOK, map[string]any{
			"status":  "success",
			"control": map[string]any{"terminate": false},
			"details": map[string]any{"id": "x"},
		})
	default:
		writeRunJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"code": "not_found", "message": r.URL.Path},
		})
	}
}

func writeRunJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// spanCreates returns the recorded span-create calls.
func (f *runPFServer) spanCreates() []runPFCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []runPFCall
	for _, c := range f.calls {
		if c.Path == "/agent_spans" {
			out = append(out, c)
		}
	}
	return out
}

// finishCalls returns recorded /finish calls.
func (f *runPFServer) finishCalls() []runPFCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []runPFCall
	for _, c := range f.calls {
		if strings.HasSuffix(c.Path, "/finish") {
			out = append(out, c)
		}
	}
	return out
}

func (f *runPFServer) setSpanTerminate(reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spanTerminate = true
	f.terminateReason = reason
}

// setRegisterTerminate makes the instance register call respond with
// control.terminate and the given reason.
func (f *runPFServer) setRegisterTerminate(reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registerTerminate = true
	f.terminateReason = reason
}

// setTerminateAfterSpanCreates responds to the first span create after n
// successful ones with control.terminate and the given reason (e.g. n = 2
// fires it on the llm-span create, mid-turn).
func (f *runPFServer) setTerminateAfterSpanCreates(n int, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminateAfter = n
	f.terminateReason = reason
}

// setSpanFail makes span creates fail with the given status.
func (f *runPFServer) setSpanFail(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spanFailStatus = status
}

// setRegisterFail makes the instance register call fail with the given
// status.
func (f *runPFServer) setRegisterFail(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registerFailStatus = status
}

// setFinishFail makes /finish requests under pathPrefix (e.g.
// "/agent_instance/") fail with the given status.
func (f *runPFServer) setFinishFail(pathPrefix string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finishFailPrefix = pathPrefix
	f.finishFailStatus = status
}

// setSpanFinishFail makes span finishes fail with the given status. When
// key is non-empty, only finishes whose result_payload carries key fail
// (e.g. "assistant_text" for the turn-span finish); empty fails all.
func (f *runPFServer) setSpanFinishFail(status int, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spanFinishFailStatus = status
	f.spanFinishFailKey = key
}

// spanFinishHasKey reports whether a finish request's result_payload
// carries the given key.
func spanFinishHasKey(body map[string]any, key string) bool {
	rp, ok := body["result_payload"].(map[string]any)
	if !ok {
		return false
	}
	_, ok = rp[key]
	return ok
}

// pfSpanSchema extracts a span's schema_name.
func pfSpanSchema(c runPFCall) string {
	d, _ := c.Body["details"].(map[string]any)
	s, _ := d["schema_name"].(string)
	return s
}

// runTracedOptions builds options wired to trace against srv.
func runTracedOptions(cfg config.Config, srv *httptest.Server, stdout *runSyncBuffer, responses ...llm.Response) run.Options {
	client := prefactor.New(prefactor.Config{BaseURL: srv.URL, Token: "t"})
	return run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: stdout,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
			return &runFakeClient{responses: responses}, nil
		},
		Tracer: prefactor.NewTracer(prefactor.TracerConfig{
			Client:    client,
			AgentName: cfg.Agents[0].Name,
		}),
	}
}

func TestRunTracedSuccess(t *testing.T) {
	t.Parallel()

	f, srv := newRunPFServer(t)
	cfg := runTestConfig(runTestAgent())
	var stdout runSyncBuffer
	opts := runTracedOptions(cfg, srv, &stdout,
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "the final words"), FinishReason: llm.FinishStop},
	)

	final, err := run.Run(context.Background(), opts, "hello")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if final != "the final words" {
		t.Errorf("final = %q, want %q", final, "the final words")
	}

	// Spans: turn and user_message (carried by StartTurn) plus one llm
	// span for the round; the whole-message path also records
	// assistant_message.
	var schemas []string
	for _, c := range f.spanCreates() {
		schemas = append(schemas, pfSpanSchema(c))
	}
	for _, want := range []string{"blorb:agent_turn", "blorb:user_message", "blorb:llm"} {
		found := false
		for _, s := range schemas {
			if s == want {
				found = true
			}
		}
		if !found {
			t.Errorf("span schemas = %v, want %q present", schemas, want)
		}
	}

	// One user-turn span completed with the final text.
	var turnFinish map[string]any
	for _, c := range f.finishCalls() {
		if !strings.HasPrefix(c.Path, "/agent_spans/") {
			continue
		}
		if rp, ok := c.Body["result_payload"].(map[string]any); ok && rp["assistant_text"] == "the final words" {
			turnFinish = c.Body
		}
	}
	if turnFinish == nil || turnFinish["status"] != "complete" {
		t.Errorf("turn finish = %v, want complete with the final text", turnFinish)
	}

	// Instance finished complete.
	var instanceFinish map[string]any
	for _, c := range f.finishCalls() {
		if strings.HasPrefix(c.Path, "/agent_instance/") {
			instanceFinish = c.Body
		}
	}
	if instanceFinish == nil || instanceFinish["status"] != "complete" {
		t.Errorf("instance finish = %v, want status complete", instanceFinish)
	}
}

func TestRunTracedProviderFailure(t *testing.T) {
	t.Parallel()

	f, srv := newRunPFServer(t)
	cfg := runTestConfig(runTestAgent())
	var stdout runSyncBuffer
	opts := runTracedOptions(cfg, srv, &stdout) // no responses: the turn fails

	_, err := run.Run(context.Background(), opts, "hello")
	if err == nil {
		t.Fatal("Run succeeded, want the provider failure to surface")
	}
	if !strings.HasPrefix(err.Error(), "run: ") {
		t.Errorf("error = %v, want the run: prefix", err)
	}

	var instanceFinish map[string]any
	for _, c := range f.finishCalls() {
		if strings.HasPrefix(c.Path, "/agent_instance/") {
			instanceFinish = c.Body
		}
	}
	if instanceFinish == nil || instanceFinish["status"] != "failed" {
		t.Errorf("instance finish = %v, want status failed", instanceFinish)
	}
}

func TestRunTracedTerminateMidRun(t *testing.T) {
	t.Parallel()

	f, srv := newRunPFServer(t)
	f.setSpanTerminate("operator stopped")
	cfg := runTestConfig(runTestAgent())
	var stdout runSyncBuffer
	opts := runTracedOptions(cfg, srv, &stdout,
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "hi!"), FinishReason: llm.FinishStop},
	)

	final, err := run.Run(context.Background(), opts, "hello")
	if err != nil {
		t.Fatalf("Run error = %v, want nil (terminate exits cleanly)", err)
	}
	if final != "" {
		t.Errorf("final = %q, want empty", final)
	}
	out := stdout.String()
	if !strings.Contains(out, "stopped by platform") || !strings.Contains(out, "operator stopped") {
		t.Errorf("stdout = %q, want the termination reason", out)
	}

	// No instance finish: the platform has already terminated it.
	for _, c := range f.finishCalls() {
		if strings.HasPrefix(c.Path, "/agent_instance/") {
			t.Errorf("unexpected instance finish %v after termination", c.Body)
		}
	}
}

func TestRunTracedCancelledContext(t *testing.T) {
	t.Parallel()

	f, srv := newRunPFServer(t)
	cfg := runTestConfig(runTestAgent())
	client := &runBlockingClient{started: make(chan struct{})}
	var stdout runSyncBuffer
	opts := runTracedOptions(cfg, srv, &stdout)
	opts.NewClient = func(config.Config, config.Agent) (llm.Client, error) { return client, nil }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := run.Run(ctx, opts, "hello")
		done <- err
	}()

	<-client.started
	cancel()

	if err := <-done; err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}

	// The deliberate interrupt semantics: unlike chat, an interrupted run
	// is a failed instance.
	var instanceFinish map[string]any
	for _, c := range f.finishCalls() {
		if strings.HasPrefix(c.Path, "/agent_instance/") {
			instanceFinish = c.Body
		}
	}
	if instanceFinish == nil || instanceFinish["status"] != "failed" {
		t.Errorf("instance finish = %v, want status failed (an interrupted run is a failed run)", instanceFinish)
	}
}

func TestRunTracingDisabledNoSpans(t *testing.T) {
	t.Parallel()

	// Point a tracer at the fake server but never attach it: no span may
	// be recorded. The fake would record any stray traffic; assert there
	// was none by checking the run succeeded (span assertions would fail
	// in the traced tests above if wiring leaked).
	f, srv := newRunPFServer(t)
	cfg := runTestConfig(runTestAgent())
	var stdout runSyncBuffer
	opts := runTracedOptions(cfg, srv, &stdout,
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "hi!"), FinishReason: llm.FinishStop},
	)
	opts.Tracer = nil

	_, err := run.Run(context.Background(), opts, "hello")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if got := len(f.spanCreates()) + len(f.finishCalls()); got != 0 {
		t.Errorf("recorded %d tracer calls with tracing disabled, want 0", got)
	}
}

// TestRunTracedStartTurnSpanFailure covers the StartTurn failure path: a
// span-create 500 fails the run (hard dependency), the already-started
// instance finishes failed, and the error carries the run: prefix.
func TestRunTracedStartTurnSpanFailure(t *testing.T) {
	t.Parallel()

	f, srv := newRunPFServer(t)
	f.setSpanFail(http.StatusInternalServerError)
	cfg := runTestConfig(runTestAgent())
	var stdout runSyncBuffer
	opts := runTracedOptions(cfg, srv, &stdout,
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "hi!"), FinishReason: llm.FinishStop},
	)

	_, err := run.Run(context.Background(), opts, "hello")
	if err == nil {
		t.Fatal("Run succeeded, want the StartTurn span failure to surface")
	}
	if !strings.HasPrefix(err.Error(), "run: ") {
		t.Errorf("error = %v, want the run: prefix", err)
	}
	if !strings.Contains(err.Error(), "prefactor") {
		t.Errorf("error = %v, want it to mention prefactor", err)
	}

	// The session started (register + start succeeded), so the instance
	// must finish failed.
	var instanceFinish map[string]any
	for _, c := range f.finishCalls() {
		if strings.HasPrefix(c.Path, "/agent_instance/") {
			instanceFinish = c.Body
		}
	}
	if instanceFinish == nil || instanceFinish["status"] != "failed" {
		t.Errorf("instance finish = %v, want status failed", instanceFinish)
	}
}

// TestRunTracedStartSessionFailure covers the StartSession failure path:
// a register 500 fails the run before any turn runs, and no instance
// finish is attempted (the instance was never started).
func TestRunTracedStartSessionFailure(t *testing.T) {
	t.Parallel()

	f, srv := newRunPFServer(t)
	f.setRegisterFail(http.StatusInternalServerError)
	cfg := runTestConfig(runTestAgent())
	var stdout runSyncBuffer
	opts := runTracedOptions(cfg, srv, &stdout,
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "hi!"), FinishReason: llm.FinishStop},
	)

	_, err := run.Run(context.Background(), opts, "hello")
	if err == nil {
		t.Fatal("Run succeeded, want the StartSession failure to surface")
	}
	if !strings.HasPrefix(err.Error(), "run: ") {
		t.Errorf("error = %v, want the run: prefix", err)
	}
	if !strings.Contains(err.Error(), "prefactor") {
		t.Errorf("error = %v, want it to mention prefactor", err)
	}

	// No instance finish: the instance never started.
	for _, c := range f.finishCalls() {
		if strings.HasPrefix(c.Path, "/agent_instance/") {
			t.Errorf("unexpected instance finish %v after a failed StartSession", c.Body)
		}
	}
}

// TestRunTracedStartSessionTerminate covers the StartSession terminate
// branch: a control.terminate on register exits cleanly with the reason
// printed, no turn runs, and no instance finish is recorded.
func TestRunTracedStartSessionTerminate(t *testing.T) {
	t.Parallel()

	f, srv := newRunPFServer(t)
	f.setRegisterTerminate("operator stopped")
	cfg := runTestConfig(runTestAgent())
	var stdout runSyncBuffer
	opts := runTracedOptions(cfg, srv, &stdout,
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "hi!"), FinishReason: llm.FinishStop},
	)

	final, err := run.Run(context.Background(), opts, "hello")
	if err != nil {
		t.Fatalf("Run error = %v, want nil (terminate exits cleanly)", err)
	}
	if final != "" {
		t.Errorf("final = %q, want empty", final)
	}
	out := stdout.String()
	if !strings.Contains(out, "stopped by platform") || !strings.Contains(out, "operator stopped") {
		t.Errorf("stdout = %q, want the termination reason", out)
	}

	// No instance finish: the platform has already terminated it.
	for _, c := range f.finishCalls() {
		if strings.HasPrefix(c.Path, "/agent_instance/") {
			t.Errorf("unexpected instance finish %v after termination", c.Body)
		}
	}
}

// TestRunTracedFinishFailureOnSuccess covers the FinishSession error on an
// otherwise-successful run: the finish error surfaces with the run:
// prefix.
func TestRunTracedFinishFailureOnSuccess(t *testing.T) {
	t.Parallel()

	f, srv := newRunPFServer(t)
	f.setFinishFail("/agent_instance/", http.StatusInternalServerError)
	cfg := runTestConfig(runTestAgent())
	var stdout runSyncBuffer
	opts := runTracedOptions(cfg, srv, &stdout,
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "hi!"), FinishReason: llm.FinishStop},
	)

	_, err := run.Run(context.Background(), opts, "hello")
	if err == nil {
		t.Fatal("Run succeeded, want the FinishSession failure to surface")
	}
	if !strings.HasPrefix(err.Error(), "run: ") {
		t.Errorf("error = %v, want the run: prefix", err)
	}
	if !strings.Contains(err.Error(), "finish") {
		t.Errorf("error = %v, want the finish failure", err)
	}
}

// TestRunTracedFinishFailureOnFailedRun covers the error-priority rule:
// when the run already failed, a FinishSession error loses to the original
// run error.
func TestRunTracedFinishFailureOnFailedRun(t *testing.T) {
	t.Parallel()

	f, srv := newRunPFServer(t)
	f.setFinishFail("/agent_instance/", http.StatusInternalServerError)
	cfg := runTestConfig(runTestAgent())
	var stdout runSyncBuffer
	opts := runTracedOptions(cfg, srv, &stdout) // no responses: the turn fails

	_, err := run.Run(context.Background(), opts, "hello")
	if err == nil {
		t.Fatal("Run succeeded, want the provider failure to surface")
	}
	// The original run error wins, not the finish error.
	if !strings.Contains(err.Error(), "no more canned responses") {
		t.Errorf("error = %v, want the original provider failure", err)
	}
	if strings.Contains(err.Error(), "finish instance") {
		t.Errorf("error = %v, want the finish failure suppressed by the run error", err)
	}
}

// TestRunTracedTurnCompleteFailure covers the turn.Complete error branch:
// a 500 on the turn-span finish (its payload carries assistant_text, so
// the llm-span finishes still succeed) fails an otherwise-successful run.
func TestRunTracedTurnCompleteFailure(t *testing.T) {
	t.Parallel()

	f, srv := newRunPFServer(t)
	f.setSpanFinishFail(http.StatusInternalServerError, "assistant_text")
	cfg := runTestConfig(runTestAgent())
	var stdout runSyncBuffer
	opts := runTracedOptions(cfg, srv, &stdout,
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "hi!"), FinishReason: llm.FinishStop},
	)

	_, err := run.Run(context.Background(), opts, "hello")
	if err == nil {
		t.Fatal("Run succeeded, want the turn.Complete failure to surface")
	}
	if !strings.HasPrefix(err.Error(), "run: ") {
		t.Errorf("error = %v, want the run: prefix", err)
	}
}

// TestRunTracedTurnFailFailure covers the turn.Fail error branch: with all
// span finishes failing, the llm-span failure surfaces from the Chat call
// and the subsequent turn.Fail failure surfaces too.
func TestRunTracedTurnFailFailure(t *testing.T) {
	t.Parallel()

	f, srv := newRunPFServer(t)
	f.setSpanFinishFail(http.StatusInternalServerError, "")
	cfg := runTestConfig(runTestAgent())
	var stdout runSyncBuffer
	opts := runTracedOptions(cfg, srv, &stdout) // no responses: the turn fails

	_, err := run.Run(context.Background(), opts, "hello")
	if err == nil {
		t.Fatal("Run succeeded, want an error")
	}
	// The turn failed and the span finish failed too: the tracer error
	// surfaces with the run: prefix.
	if !strings.HasPrefix(err.Error(), "run: ") {
		t.Errorf("error = %v, want the run: prefix", err)
	}
}

// TestRunTracedTerminateMidTurn covers the outcome-mapping terminate
// branch: a control.terminate on the llm-span create (after StartTurn's
// two span creates) fails the in-flight turn; the run exits cleanly with
// the reason printed, and no instance finish is recorded.
func TestRunTracedTerminateMidTurn(t *testing.T) {
	t.Parallel()

	f, srv := newRunPFServer(t)
	// StartTurn creates two spans (agent_turn + user_message); fire the
	// terminate on the next create (the llm span).
	f.setTerminateAfterSpanCreates(2, "operator stopped mid-turn")
	cfg := runTestConfig(runTestAgent())
	var stdout runSyncBuffer
	opts := runTracedOptions(cfg, srv, &stdout,
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "hi!"), FinishReason: llm.FinishStop},
	)

	final, err := run.Run(context.Background(), opts, "hello")
	if err != nil {
		t.Fatalf("Run error = %v, want nil (terminate exits cleanly)", err)
	}
	if final != "" {
		t.Errorf("final = %q, want empty", final)
	}
	out := stdout.String()
	if !strings.Contains(out, "stopped by platform") || !strings.Contains(out, "operator stopped mid-turn") {
		t.Errorf("stdout = %q, want the termination reason", out)
	}

	// No instance finish: the platform has already terminated it.
	for _, c := range f.finishCalls() {
		if strings.HasPrefix(c.Path, "/agent_instance/") {
			t.Errorf("unexpected instance finish %v after termination", c.Body)
		}
	}
}

// TestRunTracedTerminateMidTurnSpanFinishFailure pins the corner where the
// terminate signal wins over a tracer failure: with the turn-span finish
// also failing (all span finishes fail), the run still exits cleanly on
// the terminate — the platform's request to stop cannot be overridden.
func TestRunTracedTerminateMidTurnSpanFinishFailure(t *testing.T) {
	t.Parallel()

	f, srv := newRunPFServer(t)
	f.setTerminateAfterSpanCreates(2, "operator stopped mid-turn")
	f.setSpanFinishFail(http.StatusInternalServerError, "")
	cfg := runTestConfig(runTestAgent())
	var stdout runSyncBuffer
	opts := runTracedOptions(cfg, srv, &stdout,
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "hi!"), FinishReason: llm.FinishStop},
	)

	final, err := run.Run(context.Background(), opts, "hello")
	if err != nil {
		t.Fatalf("Run error = %v, want nil (the terminate signal wins)", err)
	}
	if final != "" {
		t.Errorf("final = %q, want empty", final)
	}
	out := stdout.String()
	if !strings.Contains(out, "stopped by platform") || !strings.Contains(out, "operator stopped mid-turn") {
		t.Errorf("stdout = %q, want the termination reason", out)
	}
}

// TestResolveSinkMkdirFailure covers the ResolveSink error path: a config
// path whose parent is a regular file makes the log-dir MkdirAll fail,
// surfacing the create-log-dir error.
func TestResolveSinkMkdirFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// A regular file that will be used as a directory: MkdirAll under it
	// fails with ENOTDIR.
	notADir := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := config.Config{} // logging defaults to enabled
	_, err := chat.ResolveSink(filepath.Join(notADir, "blorb.json"), cfg)
	if err == nil || !strings.Contains(err.Error(), "create log dir") {
		t.Errorf("error = %v, want a create-log-dir failure", err)
	}
}

func TestRunUsageFooterHappyPath(t *testing.T) {
	t.Parallel()

	cfg := runTestConfig(runTestAgent())
	var stdout runSyncBuffer
	final, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
			return &runFakeClient{responses: []llm.Response{
				{
					Message:      llm.NewTextMessage(llm.RoleAssistant, "the answer"),
					FinishReason: llm.FinishStop,
					Usage:        llm.Usage{PromptTokens: 12, CompletionTokens: 34, TotalTokens: 46},
					// The client measured stats: the footer carries a
					// stats line after the token line.
					Stats: llm.CallStats{
						Output:  llm.OutputBytes{Content: 2048, Reasoning: 1024},
						Elapsed: 2 * time.Second,
					},
				},
			}}, nil
		},
	}, "hello there")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if final != "the answer" {
		t.Errorf("final = %q, want it unaffected by the footer", final)
	}

	out := stdout.String()
	wantFooter := "tokens: 12 prompt, 34 completion, 46 total\nstats: 2s, 3KB output (2KB text, 1KB reasoning), 17.0 tok/s, 1.5KB/s\n"
	if !strings.HasSuffix(out, wantFooter) {
		t.Errorf("stdout = %q, want it to end with the usage footer and stats line", out)
	}
	if strings.Contains(out, "session tokens:") {
		t.Errorf("stdout = %q, want no session line (run has no session totals)", out)
	}
}

func TestRunUsageFooterToolRoundSummed(t *testing.T) {
	t.Parallel()

	cfg := runToolConfig(t)
	responses := runToolResponses("echoer")
	responses[0].Usage = llm.Usage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10}
	responses[1].Usage = llm.Usage{PromptTokens: 30, CompletionTokens: 4, TotalTokens: 34}

	var stdout runSyncBuffer
	_, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
			return &runFakeClient{responses: responses}, nil
		},
	}, "run it")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	out := stdout.String()
	// One footer with the turn's summed usage, not one per call.
	if n := strings.Count(out, "tokens: 38 prompt, 6 completion, 44 total"); n != 1 {
		t.Errorf("stdout = %q, want exactly one summed footer, got %d", out, n)
	}
	if strings.Contains(out, "tokens: 8 prompt") {
		t.Errorf("stdout = %q, want no per-call footer", out)
	}
}

func TestRunUsageFooterSubagentSplit(t *testing.T) {
	t.Parallel()

	cfg := runSubagentConfig(t)
	parent := &runFakeClient{responses: []llm.Response{
		{
			Message: llm.Message{
				Role:      llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{ID: "call_p", Type: "function", FunctionName: "ask_worker", FunctionArgs: `{"prompt":"do it"}`}},
			},
			FinishReason: llm.FinishToolCalls,
			Usage:        llm.Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110},
		},
		{Message: llm.NewTextMessage(llm.RoleAssistant, "parent done"), FinishReason: llm.FinishStop,
			Usage: llm.Usage{PromptTokens: 130, CompletionTokens: 20, TotalTokens: 150}},
	}}
	worker := &runFakeClient{responses: []llm.Response{
		{
			Message: llm.Message{
				Role:      llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{ID: "call_w", Type: "function", FunctionName: "worker_tool", FunctionArgs: "{}"}},
			},
			FinishReason: llm.FinishToolCalls,
			Usage:        llm.Usage{PromptTokens: 11, CompletionTokens: 2, TotalTokens: 13},
		},
		{Message: llm.NewTextMessage(llm.RoleAssistant, "worker result text"), FinishReason: llm.FinishStop,
			Usage: llm.Usage{PromptTokens: 15, CompletionTokens: 3, TotalTokens: 18}},
	}}

	var stdout runSyncBuffer
	_, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		NewClient: func(_ config.Config, agent config.Agent) (llm.Client, error) {
			if agent.Name == "worker" {
				return worker, nil
			}
			return parent, nil
		},
	}, "go")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	out := stdout.String()
	want := "tokens: 256 prompt, 35 completion, 291 total (parent: 230/30/260, worker: 26/5/31)"
	if !strings.Contains(out, want) {
		t.Errorf("stdout = %q, want the per-agent footer split %q", out, want)
	}
}

func TestRunUsageFooterOnFailingRun(t *testing.T) {
	t.Parallel()

	cfg := runTestConfig(runTestAgent(), config.ToolEntry{
		Type:        config.ToolTypeCommand,
		Name:        "echoer",
		Description: "Echoes.",
		Command:     []string{"true"},
	})
	responses := []llm.Response{
		{
			Message: llm.Message{
				Role:      llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{ID: "call_1", Type: "function", FunctionName: "echoer", FunctionArgs: "{}"}},
			},
			FinishReason: llm.FinishToolCalls,
			Usage:        llm.Usage{PromptTokens: 5, CompletionTokens: 6, TotalTokens: 11},
		},
		// The second call never arrives: the fake runs dry and the turn
		// fails mid-loop after one usage-bearing call.
	}
	var stdout runSyncBuffer
	_, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
			// One usage-bearing response, then the fake runs dry and the
			// turn fails mid-loop.
			return &runFakeClient{responses: responses}, nil
		},
	}, "hello")
	if err == nil {
		t.Fatal("Run succeeded, want a turn error")
	}

	out := stdout.String()
	if !strings.Contains(out, "tokens: 5 prompt, 6 completion, 11 total") {
		t.Errorf("stdout = %q, want the partial footer for the call that completed", out)
	}
}

func TestRunNoUsageNoFooterWhenCancelledBeforeAnyCall(t *testing.T) {
	t.Parallel()

	cfg := runTestConfig(runTestAgent())
	var stdout runSyncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := run.Run(ctx, run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
			return &runBlockingClient{started: make(chan struct{})}, nil
		},
	}, "hello")
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if out := stdout.String(); strings.Contains(out, "tokens:") {
		t.Errorf("stdout = %q, want no footer for a run cancelled before any call", out)
	}
}

// --- plain format tests ---

// TestRunFormatPlainWholeMessageToolRound pins the plain stdout contract:
// exactly the concatenation of the assistant text events with no
// separator (deliberately unlike Run's return value, which the engine
// joins with a newline between rounds), and all diagnostics on stderr.
func TestRunFormatPlainWholeMessageToolRound(t *testing.T) {
	t.Parallel()

	cfg := runToolConfig(t)
	// The first round emits text alongside its tool call; the second
	// round's text is the final answer.
	responses := runToolResponses("echoer")
	responses[0].Message.Content = "calling the tool now"
	responses[0].Usage = llm.Usage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10}
	responses[1].Usage = llm.Usage{PromptTokens: 30, CompletionTokens: 4, TotalTokens: 34}

	var stdout, stderr runSyncBuffer
	final, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		Stderr: &stderr,
		Format: run.FormatPlain,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
			return &runFakeClient{responses: responses}, nil
		},
	}, "run it")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	// Run's return joins the rounds with a newline; plain's stdout
	// splices them exactly as they came, no separator.
	if final != "calling the tool now\nmade it" {
		t.Errorf("final = %q, want the engine-joined text", final)
	}
	if got, want := stdout.String(), "calling the tool nowmade it"; got != want {
		t.Errorf("stdout = %q, want exactly %q (agent text spliced, no separator)", got, want)
	}

	diag := stderr.String()
	if !strings.Contains(diag, ">>> Assistant:") {
		t.Errorf("stderr = %q, want the assistant headings", diag)
	}
	if !strings.Contains(diag, ">>> Tool: echoer") || !strings.Contains(diag, ">>> Result: Tool: echoer") {
		t.Errorf("stderr = %q, want the tool activity blocks", diag)
	}
	if !strings.Contains(diag, "tokens: 38 prompt, 6 completion, 44 total") {
		t.Errorf("stderr = %q, want the usage footer", diag)
	}
	// The spliced stdout text must not appear verbatim on stderr: the
	// diagnostic stream renders the same words under headings, never as
	// one continuous splice.
	if strings.Contains(diag, "calling the tool nowmade it") {
		t.Errorf("stderr = %q, want no spliced agent text on the diagnostics stream", diag)
	}
}

// TestRunFormatPlainStreamed pins that plain's stdout receives the text
// fragments as they arrive, while stderr renders the chat-style streamed
// output.
func TestRunFormatPlainStreamed(t *testing.T) {
	t.Parallel()

	cfg := runTestConfig(runTestAgent())
	var stdout, stderr runSyncBuffer
	final, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		Stderr: &stderr,
		Format: run.FormatPlain,
		Stream: true,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
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
	if got, want := stdout.String(), "streamed!"; got != want {
		t.Errorf("stdout = %q, want exactly %q (fragments written through)", got, want)
	}
	diag := stderr.String()
	if !strings.Contains(diag, ">>> Assistant:\nstreamed!\n") {
		t.Errorf("stderr = %q, want the chat-style streamed rendering", diag)
	}
}

// TestRunFormatPlainThinkingOnStderrOnly pins that thinking is diagnostic:
// it renders on stderr and never reaches plain's stdout.
func TestRunFormatPlainThinkingOnStderrOnly(t *testing.T) {
	t.Parallel()

	cfg := runTestConfig(runTestAgent())
	var stdout, stderr runSyncBuffer
	_, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		Stderr: &stderr,
		Format: run.FormatPlain,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
			return &runFakeClient{responses: []llm.Response{
				{
					Message: llm.Message{
						Role:      llm.RoleAssistant,
						Reasoning: "let me think",
						Content:   "the answer",
					},
					FinishReason: llm.FinishStop,
				},
			}}, nil
		},
	}, "hello")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if got, want := stdout.String(), "the answer"; got != want {
		t.Errorf("stdout = %q, want exactly %q (no thinking)", got, want)
	}
	diag := stderr.String()
	if !strings.Contains(diag, ">>> Assistant (thinking):\nlet me think") {
		t.Errorf("stderr = %q, want the thinking block", diag)
	}
}

// TestRunFormatPlainSubagentOnStderr pins that subagent activity
// (including its assistant text) stays on stderr; stdout carries only the
// parent's text.
func TestRunFormatPlainSubagentOnStderr(t *testing.T) {
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
		{Message: llm.NewTextMessage(llm.RoleAssistant, "worker result text"), FinishReason: llm.FinishStop},
	}}

	var stdout, stderr runSyncBuffer
	final, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		Stderr: &stderr,
		Format: run.FormatPlain,
		NewClient: func(_ config.Config, agent config.Agent) (llm.Client, error) {
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
	if got, want := stdout.String(), "parent done"; got != want {
		t.Errorf("stdout = %q, want exactly %q (only the parent's text)", got, want)
	}
	diag := stderr.String()
	if !strings.Contains(diag, "[worker] >>> Assistant:") || !strings.Contains(diag, "worker result text") {
		t.Errorf("stderr = %q, want the worker's activity", diag)
	}
}

// TestRunFormatPlainToolOutputSuppression pins that plain's stderr tool
// blocks honor ToolOutput exactly like chat: summary without the flag,
// full body with it, failures always in full.
func TestRunFormatPlainToolOutputSuppression(t *testing.T) {
	t.Run("suppressed by default", func(t *testing.T) {
		t.Parallel()

		cfg := runToolConfig(t)
		var stdout, stderr runSyncBuffer
		_, err := run.Run(context.Background(), run.Options{
			Config: cfg,
			Agent:  cfg.Agents[0],
			Stdout: &stdout,
			Stderr: &stderr,
			Format: run.FormatPlain,
			NewClient: func(config.Config, config.Agent) (llm.Client, error) {
				return &runFakeClient{responses: runToolResponses("echoer")}, nil
			},
		}, "run it")
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		diag := stderr.String()
		if !strings.Contains(diag, "7 characters, 2 lines") {
			t.Errorf("stderr = %q, want the count summary", diag)
		}
		if strings.Contains(diag, "\none\n") {
			t.Errorf("stderr = %q, want no raw tool output body", diag)
		}
	})

	t.Run("full body with ToolOutput", func(t *testing.T) {
		t.Parallel()

		cfg := runToolConfig(t)
		var stdout, stderr runSyncBuffer
		_, err := run.Run(context.Background(), run.Options{
			Config:     cfg,
			Agent:      cfg.Agents[0],
			Stdout:     &stdout,
			Stderr:     &stderr,
			Format:     run.FormatPlain,
			ToolOutput: true,
			NewClient: func(config.Config, config.Agent) (llm.Client, error) {
				return &runFakeClient{responses: runToolResponses("echoer")}, nil
			},
		}, "run it")
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if diag := stderr.String(); !strings.Contains(diag, ">>> Result: Tool: echoer\none\ntwo") {
			t.Errorf("stderr = %q, want the full tool output body", diag)
		}
	})

	t.Run("failed result always full", func(t *testing.T) {
		t.Parallel()

		cfg := runTestConfig(runTestAgent(), config.ToolEntry{
			Type:        config.ToolTypeCommand,
			Name:        "failer",
			Description: "Fails.",
			Command:     []string{"sh", "-c", "echo boom >&2; exit 3"},
		})
		var stdout, stderr runSyncBuffer
		_, err := run.Run(context.Background(), run.Options{
			Config: cfg,
			Agent:  cfg.Agents[0],
			Stdout: &stdout,
			Stderr: &stderr,
			Format: run.FormatPlain,
			NewClient: func(config.Config, config.Agent) (llm.Client, error) {
				return &runFakeClient{responses: runToolResponses("failer")}, nil
			},
		}, "run it")
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		diag := stderr.String()
		if !strings.Contains(diag, ">>> Error: Tool: failer") {
			t.Errorf("stderr = %q, want the error result heading", diag)
		}
		if !strings.Contains(diag, "failed with exit code 3") {
			t.Errorf("stderr = %q, want the full failed body", diag)
		}
	})
}

// TestRunFormatPlainFooterOnStderr pins that the usage footer goes to the
// injected Stderr, not stdout, in plain format.
func TestRunFormatPlainFooterOnStderr(t *testing.T) {
	t.Parallel()

	cfg := runTestConfig(runTestAgent())
	var stdout, stderr runSyncBuffer
	_, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		Stderr: &stderr,
		Format: run.FormatPlain,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
			return &runFakeClient{responses: []llm.Response{
				{
					Message:      llm.NewTextMessage(llm.RoleAssistant, "the answer"),
					FinishReason: llm.FinishStop,
					Usage:        llm.Usage{PromptTokens: 12, CompletionTokens: 34, TotalTokens: 46},
				},
			}}, nil
		},
	}, "hello")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if out := stdout.String(); strings.Contains(out, "tokens:") {
		t.Errorf("stdout = %q, want no footer", out)
	}
	if diag := stderr.String(); !strings.HasSuffix(diag, "tokens: 12 prompt, 34 completion, 46 total\n") {
		t.Errorf("stderr = %q, want it to end with the usage footer", diag)
	}
}

// TestRunFormatPlainTerminateOnStderr pins that the "stopped by platform"
// notice goes to Stderr in plain format.
func TestRunFormatPlainTerminateOnStderr(t *testing.T) {
	t.Parallel()

	f, srv := newRunPFServer(t)
	f.setSpanTerminate("operator stopped")
	cfg := runTestConfig(runTestAgent())
	var stdout, stderr runSyncBuffer
	opts := runTracedOptions(cfg, srv, &stdout,
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "hi!"), FinishReason: llm.FinishStop},
	)
	opts.Stderr = &stderr
	opts.Format = run.FormatPlain

	final, err := run.Run(context.Background(), opts, "hello")
	if err != nil {
		t.Fatalf("Run error = %v, want nil (terminate exits cleanly)", err)
	}
	if final != "" {
		t.Errorf("final = %q, want empty", final)
	}
	if out := stdout.String(); strings.Contains(out, "stopped by platform") {
		t.Errorf("stdout = %q, want no termination notice", out)
	}
	if diag := stderr.String(); !strings.Contains(diag, "stopped by platform") || !strings.Contains(diag, "operator stopped") {
		t.Errorf("stderr = %q, want the termination reason", diag)
	}
}

// TestRunFormatPlainFailedRunStdoutKeepsRoundText pins that a provider
// failure mid-loop leaves the completed round's text on stdout, the error
// is returned (not printed by run), and stdout carries no error text.
func TestRunFormatPlainFailedRunStdoutKeepsRoundText(t *testing.T) {
	t.Parallel()

	cfg := runToolConfig(t)
	responses := runToolResponses("echoer")
	responses[0].Message.Content = "calling the tool now"

	var stdout, stderr runSyncBuffer
	_, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		Stderr: &stderr,
		Format: run.FormatPlain,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
			// Two responses only: the turn fails on the third call.
			return &runFakeClient{responses: responses[:1]}, nil
		},
	}, "run it")
	if err == nil {
		t.Fatal("Run succeeded, want a turn error")
	}
	if !strings.HasPrefix(err.Error(), "run: ") {
		t.Errorf("error = %v, want the run: prefix", err)
	}
	if got, want := stdout.String(), "calling the tool now"; got != want {
		t.Errorf("stdout = %q, want exactly the completed round's text %q", got, want)
	}
	if out := stdout.String(); strings.Contains(out, "no more canned responses") || strings.Contains(out, "error") {
		t.Errorf("stdout = %q, want no error text on stdout", out)
	}
}

// TestRunFormatPlainNilStderrFallsBackToStdout pins the single-stream
// fallback: with Stderr unset, plain's diagnostics land on Stdout.
func TestRunFormatPlainNilStderrFallsBackToStdout(t *testing.T) {
	t.Parallel()

	cfg := runTestConfig(runTestAgent())
	var stdout runSyncBuffer
	_, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		Format: run.FormatPlain,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
			return &runFakeClient{responses: []llm.Response{
				{
					Message:      llm.NewTextMessage(llm.RoleAssistant, "the answer"),
					FinishReason: llm.FinishStop,
					Usage:        llm.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
				},
			}}, nil
		},
	}, "hello")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	out := stdout.String()
	if !strings.Contains(out, ">>> Assistant:") {
		t.Errorf("stdout = %q, want the headings on the fallback stream", out)
	}
	if !strings.Contains(out, "tokens: 1 prompt, 2 completion, 3 total") {
		t.Errorf("stdout = %q, want the footer on the fallback stream", out)
	}
	if !strings.Contains(out, "the answer") {
		t.Errorf("stdout = %q, want the agent text", out)
	}
}

// TestRunFormatChatIgnoresStderr pins that chat keeps the footer and
// termination notices on stdout even when Stderr is set.
func TestRunFormatChatIgnoresStderr(t *testing.T) {
	t.Parallel()

	cfg := runTestConfig(runTestAgent())
	var stdout, stderr runSyncBuffer
	_, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		Stderr: &stderr,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
			return &runFakeClient{responses: []llm.Response{
				{
					Message:      llm.NewTextMessage(llm.RoleAssistant, "the answer"),
					FinishReason: llm.FinishStop,
					Usage:        llm.Usage{PromptTokens: 12, CompletionTokens: 34, TotalTokens: 46},
				},
			}}, nil
		},
	}, "hello")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	out := stdout.String()
	if !strings.HasSuffix(out, "tokens: 12 prompt, 34 completion, 46 total\n") {
		t.Errorf("stdout = %q, want the footer still on stdout for chat", out)
	}
	if diag := stderr.String(); diag != "" {
		t.Errorf("stderr = %q, want chat to ignore Stderr entirely", diag)
	}
}

// TestRunFormatUnknownFailsBeforeLLMCall pins that an unknown format
// fails the run with the supported-list error and makes no LLM call.
func TestRunFormatUnknownFailsBeforeLLMCall(t *testing.T) {
	t.Parallel()

	cfg := runTestConfig(runTestAgent())
	client := &runFakeClient{}
	var stdout, stderr runSyncBuffer
	_, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		Stderr: &stderr,
		Format: "yaml",
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
			return client, nil
		},
	}, "hello")
	if err == nil {
		t.Fatal("Run succeeded, want an unknown-format error")
	}
	if !strings.Contains(err.Error(), `unknown format "yaml"`) || !strings.Contains(err.Error(), "chat, plain, ndjson") {
		t.Errorf("error = %v, want the value and the supported formats", err)
	}
	if len(client.requests) != 0 {
		t.Errorf("LLM calls = %d, want 0 (format validated before any call)", len(client.requests))
	}
}

// --- ndjson format tests ---

// ndjsonLine is one parsed line of an ndjson stream: the type plus the
// per-type fields the tests care about.
type ndjsonLine struct {
	Type      string     `json:"type"`
	Text      string     `json:"text"`
	Thinking  string     `json:"thinking"`
	Name      string     `json:"name"`
	Index     *int       `json:"index"`
	Arguments string     `json:"arguments"`
	Output    string     `json:"output"`
	Failed    bool       `json:"failed"`
	Agent     string     `json:"agent"`
	Model     string     `json:"model"`
	Depth     *int       `json:"depth"`
	Usage     *llm.Usage `json:"usage"`
	Agents    []struct {
		Agent string    `json:"agent"`
		Usage llm.Usage `json:"usage"`
	} `json:"agents"`
	Error string `json:"error"`
}

// parseNDJSONLines parses every line of out into ndjsonLine values; it
// fails the test on any invalid line.
func parseNDJSONLines(t *testing.T, out string) []ndjsonLine {
	t.Helper()
	var lines []ndjsonLine
	dec := json.NewDecoder(strings.NewReader(out))
	for dec.More() {
		var l ndjsonLine
		if err := dec.Decode(&l); err != nil {
			t.Fatalf("parsing ndjson line: %v\nstream:\n%s", err, out)
		}
		lines = append(lines, l)
	}
	return lines
}

// ndjsonTypes returns the type of each line.
func ndjsonTypes(lines []ndjsonLine) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Type
	}
	return out
}

// TestRunFormatNDJSONNonStreamedToolRound pins the exact line sequence of
// a non-streamed run with a tool round, the done event's totals, and the
// per-agent split.
func TestRunFormatNDJSONNonStreamedToolRound(t *testing.T) {
	t.Parallel()

	cfg := runToolConfig(t)
	responses := runToolResponses("echoer")
	// The first round emits text alongside its tool call.
	responses[0].Message.Content = "calling the tool now"
	responses[0].Usage = llm.Usage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10}
	responses[1].Usage = llm.Usage{PromptTokens: 30, CompletionTokens: 4, TotalTokens: 34}

	var stdout, stderr runSyncBuffer
	final, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		Stderr: &stderr,
		Format: run.FormatNDJSON,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
			return &runFakeClient{responses: responses}, nil
		},
	}, "run it")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	lines := parseNDJSONLines(t, stdout.String())
	want := []string{"usage", "text", "tool_call", "tool_result", "usage", "text", "done"}
	if got := ndjsonTypes(lines); !slicesEqual(got, want) {
		t.Errorf("line types = %v, want %v", got, want)
	}
	if lines[1].Text != "calling the tool now" {
		t.Errorf("lines[1] = %+v, want the first round's text event", lines[1])
	}
	if lines[2].Name != "echoer" || lines[2].Arguments != "{}" {
		t.Errorf("lines[2] = %+v, want the tool_call event", lines[2])
	}
	if lines[3].Output != "one\ntwo" || lines[3].Failed {
		t.Errorf("lines[3] = %+v, want the full tool result body", lines[3])
	}
	if lines[5].Text != "made it" {
		t.Errorf("lines[5] = %+v, want the final text event", lines[5])
	}

	done := lines[6]
	if done.Text != final {
		t.Errorf("done.text = %q, want the run's return value %q", done.Text, final)
	}
	if done.Usage == nil || done.Usage.TotalTokens != 44 {
		t.Errorf("done.usage = %+v, want total 44", done.Usage)
	}
	if len(done.Agents) != 1 || done.Agents[0].Agent != "tester" || done.Agents[0].Usage.TotalTokens != 44 {
		t.Errorf("done.agents = %+v, want the tester split with total 44", done.Agents)
	}

	if diag := stderr.String(); diag != "" {
		t.Errorf("stderr = %q, want ndjson diagnostics silent for this run", diag)
	}
}

// TestRunFormatNDJSONStreamed pins that streamed text fragments go out
// line-by-line, followed by usage then done (the engine's documented
// ordering).
func TestRunFormatNDJSONStreamed(t *testing.T) {
	t.Parallel()

	cfg := runTestConfig(runTestAgent())
	var stdout runSyncBuffer
	_, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		Format: run.FormatNDJSON,
		Stream: true,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
			return &runStreamingFakeClient{responses: []llm.Response{
				{
					Message:      llm.NewTextMessage(llm.RoleAssistant, "streamed!"),
					FinishReason: llm.FinishStop,
					Usage:        llm.Usage{PromptTokens: 5, CompletionTokens: 6, TotalTokens: 11},
				},
			}}, nil
		},
	}, "hello")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	lines := parseNDJSONLines(t, stdout.String())
	if got := ndjsonTypes(lines); !slicesEqual(got, []string{"text_delta", "usage", "done"}) {
		t.Fatalf("line types = %v, want [text_delta usage done]", got)
	}
	if lines[0].Text != "streamed!" {
		t.Errorf("text_delta = %+v, want the response content", lines[0])
	}
	if lines[2].Usage == nil || lines[2].Usage.TotalTokens != 11 {
		t.Errorf("done.usage = %+v, want total 11", lines[2].Usage)
	}
}

// TestRunFormatNDJSONThinking pins that thinking arrives in the thinking
// field, separate from text, in both stream modes.
func TestRunFormatNDJSONThinking(t *testing.T) {
	t.Run("non-streamed", func(t *testing.T) {
		t.Parallel()

		cfg := runTestConfig(runTestAgent())
		var stdout runSyncBuffer
		_, err := run.Run(context.Background(), run.Options{
			Config: cfg,
			Agent:  cfg.Agents[0],
			Stdout: &stdout,
			Format: run.FormatNDJSON,
			NewClient: func(config.Config, config.Agent) (llm.Client, error) {
				return &runFakeClient{responses: []llm.Response{
					{Message: llm.Message{
						Role:      llm.RoleAssistant,
						Reasoning: "pondering",
						Content:   "the answer",
					}, FinishReason: llm.FinishStop},
				}}, nil
			},
		}, "hello")
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		lines := parseNDJSONLines(t, stdout.String())
		if got := ndjsonTypes(lines); !slicesEqual(got, []string{"usage", "thinking", "text", "done"}) {
			t.Fatalf("line types = %v, want [usage thinking text done]", got)
		}
		if lines[1].Thinking != "pondering" || lines[1].Text != "" {
			t.Errorf("thinking event = %+v, want the reasoning in the thinking field", lines[1])
		}
	})

	t.Run("streamed", func(t *testing.T) {
		t.Parallel()

		cfg := runTestConfig(runTestAgent())
		var stdout runSyncBuffer
		_, err := run.Run(context.Background(), run.Options{
			Config: cfg,
			Agent:  cfg.Agents[0],
			Stdout: &stdout,
			Format: run.FormatNDJSON,
			Stream: true,
			NewClient: func(config.Config, config.Agent) (llm.Client, error) {
				return &runStreamingFakeClient{responses: []llm.Response{
					{Message: llm.Message{
						Role:      llm.RoleAssistant,
						Reasoning: "pondering",
						Content:   "the answer",
					}, FinishReason: llm.FinishStop},
				}}, nil
			},
		}, "hello")
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		lines := parseNDJSONLines(t, stdout.String())
		if got := ndjsonTypes(lines); !slicesEqual(got, []string{"thinking_delta", "text_delta", "usage", "done"}) {
			t.Fatalf("line types = %v, want [thinking_delta text_delta usage done]", got)
		}
		if lines[0].Thinking != "pondering" {
			t.Errorf("thinking_delta = %+v, want the reasoning fragment", lines[0])
		}
	})
}

// TestRunFormatNDJSONStreamedToolCall pins the tool_call_delta contract:
// indexed fragments, the name on the first, arguments concatenating.
func TestRunFormatNDJSONStreamedToolCall(t *testing.T) {
	t.Parallel()

	cfg := runToolConfig(t)
	streamingResponses := []llm.Response{
		{
			Message: llm.Message{
				Role:      llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{ID: "call_1", Type: "function", FunctionName: "echoer", FunctionArgs: `{"x":1}`}},
			},
			FinishReason: llm.FinishToolCalls,
			Usage:        llm.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
		},
		{Message: llm.NewTextMessage(llm.RoleAssistant, "done"), FinishReason: llm.FinishStop,
			Usage: llm.Usage{PromptTokens: 4, CompletionTokens: 5, TotalTokens: 9}},
	}
	// fragmentingClient splits each tool call's args into two fragments so
	// the delta stream exercises client-side assembly.
	fragmenting := struct{ runStreamingFakeClient }{runStreamingFakeClient{responses: streamingResponses}}
	client := fragmentingClient{fragmenting}

	var stdout runSyncBuffer
	_, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		Format: run.FormatNDJSON,
		Stream: true,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
			return &client, nil
		},
	}, "run it")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	lines := parseNDJSONLines(t, stdout.String())
	var deltas []ndjsonLine
	for _, l := range lines {
		if l.Type == "tool_call_delta" {
			deltas = append(deltas, l)
		}
	}
	if len(deltas) != 2 {
		t.Fatalf("tool_call_delta count = %d, want 2 (name+args, then args)", len(deltas))
	}
	if deltas[0].Index == nil || *deltas[0].Index != 0 {
		t.Errorf("first delta index = %v, want 0", deltas[0].Index)
	}
	if deltas[0].Name != "echoer" {
		t.Errorf("first delta name = %q, want echoer", deltas[0].Name)
	}
	var args string
	for _, d := range deltas {
		args += d.Arguments
	}
	if args != `{"x":1}` {
		t.Errorf("concatenated arguments = %q, want the full args", args)
	}
}

// TestRunFormatNDJSONToolResultAlwaysFull pins that ndjson tool results
// carry the full body regardless of ToolOutput, and carry failed=true on
// a failed result.
func TestRunFormatNDJSONToolResultAlwaysFull(t *testing.T) {
	t.Run("full body with ToolOutput false", func(t *testing.T) {
		t.Parallel()

		cfg := runToolConfig(t)
		var stdout runSyncBuffer
		_, err := run.Run(context.Background(), run.Options{
			Config: cfg,
			Agent:  cfg.Agents[0],
			Stdout: &stdout,
			Format: run.FormatNDJSON,
			NewClient: func(config.Config, config.Agent) (llm.Client, error) {
				return &runFakeClient{responses: runToolResponses("echoer")}, nil
			},
		}, "run it")
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		for _, l := range parseNDJSONLines(t, stdout.String()) {
			if l.Type == "tool_result" {
				if l.Output != "one\ntwo" {
					t.Errorf("tool_result output = %q, want the full body", l.Output)
				}
				if l.Failed {
					t.Errorf("tool_result failed = true, want false")
				}
				return
			}
		}
		t.Fatalf("no tool_result line in stream:\n%s", stdout.String())
	})

	t.Run("failed result", func(t *testing.T) {
		t.Parallel()

		cfg := runTestConfig(runTestAgent(), config.ToolEntry{
			Type:        config.ToolTypeCommand,
			Name:        "failer",
			Description: "Fails.",
			Command:     []string{"sh", "-c", "echo boom >&2; exit 3"},
		})
		var stdout runSyncBuffer
		_, err := run.Run(context.Background(), run.Options{
			Config: cfg,
			Agent:  cfg.Agents[0],
			Stdout: &stdout,
			Format: run.FormatNDJSON,
			NewClient: func(config.Config, config.Agent) (llm.Client, error) {
				return &runFakeClient{responses: runToolResponses("failer")}, nil
			},
		}, "run it")
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		for _, l := range parseNDJSONLines(t, stdout.String()) {
			if l.Type == "tool_result" {
				if !l.Failed {
					t.Errorf("tool_result failed = false, want true")
				}
				if !strings.Contains(l.Output, "failed with exit code 3") {
					t.Errorf("tool_result output = %q, want the full failed body", l.Output)
				}
				return
			}
		}
		t.Fatalf("no tool_result line in stream:\n%s", stdout.String())
	})
}

// TestRunFormatNDJSONSubagentEvents pins the subagent_* contract: agent
// and depth fields, nested usage attribution, and both agents in
// done.agents.
func TestRunFormatNDJSONSubagent(t *testing.T) {
	t.Parallel()

	cfg := runSubagentConfig(t)
	parent := &runFakeClient{responses: []llm.Response{
		{
			Message: llm.Message{
				Role:      llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{ID: "call_p", Type: "function", FunctionName: "ask_worker", FunctionArgs: `{"prompt":"do it"}`}},
			},
			FinishReason: llm.FinishToolCalls,
			Usage:        llm.Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110},
		},
		{Message: llm.NewTextMessage(llm.RoleAssistant, "parent done"), FinishReason: llm.FinishStop,
			Usage: llm.Usage{PromptTokens: 130, CompletionTokens: 20, TotalTokens: 150}},
	}}
	worker := &runFakeClient{responses: []llm.Response{
		{Message: llm.NewTextMessage(llm.RoleAssistant, "worker result text"), FinishReason: llm.FinishStop,
			Usage: llm.Usage{PromptTokens: 15, CompletionTokens: 3, TotalTokens: 18}},
	}}

	var stdout runSyncBuffer
	_, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		Format: run.FormatNDJSON,
		NewClient: func(_ config.Config, agent config.Agent) (llm.Client, error) {
			if agent.Name == "worker" {
				return worker, nil
			}
			return parent, nil
		},
	}, "go")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	lines := parseNDJSONLines(t, stdout.String())
	var sawText, sawUsage bool
	for _, l := range lines {
		if !strings.HasPrefix(l.Type, "subagent_") {
			continue
		}
		if l.Agent != "worker" {
			t.Errorf("%s agent = %q, want worker", l.Type, l.Agent)
		}
		if l.Depth == nil || *l.Depth != 1 {
			t.Errorf("%s depth = %v, want 1", l.Type, l.Depth)
		}
		if l.Type == "subagent_text" && l.Text == "worker result text" {
			sawText = true
		}
		if l.Type == "subagent_usage" {
			sawUsage = true
			if l.Usage == nil || l.Usage.TotalTokens != 18 {
				t.Errorf("subagent_usage = %+v, want the nested call's usage", l.Usage)
			}
		}
	}
	if !sawText || !sawUsage {
		t.Errorf("stream missing subagent_text or subagent_usage:\n%s", stdout.String())
	}

	// The parent's own events: usage carries the agent attribution (the
	// engine sets AgentName on usage events); all other parent events
	// carry no agent/depth.
	for _, l := range lines {
		if strings.HasPrefix(l.Type, "subagent_") || l.Type == "usage" {
			continue
		}
		if l.Agent != "" || l.Depth != nil {
			t.Errorf("parent event %s carries agent=%q depth=%v, want none", l.Type, l.Agent, l.Depth)
		}
	}

	done := lines[len(lines)-1]
	if done.Type != "done" {
		t.Fatalf("last line type = %q, want done", done.Type)
	}
	if done.Text != "parent done" {
		t.Errorf("done.text = %q, want %q", done.Text, "parent done")
	}
	if len(done.Agents) != 2 {
		t.Fatalf("done.agents = %+v, want both agents", done.Agents)
	}
	if done.Agents[0].Agent != "parent" || done.Agents[0].Usage.TotalTokens != 260 {
		t.Errorf("done.agents[0] = %+v, want parent total 260", done.Agents[0])
	}
	if done.Agents[1].Agent != "worker" || done.Agents[1].Usage.TotalTokens != 18 {
		t.Errorf("done.agents[1] = %+v, want worker total 18", done.Agents[1])
	}
	if done.Usage == nil || done.Usage.TotalTokens != 278 {
		t.Errorf("done.usage = %+v, want total 278", done.Usage)
	}
}

// TestRunFormatNDJSONFailedRun pins that a provider failure after a
// completed round ends the stream with error, no done.
func TestRunFormatNDJSONFailedRun(t *testing.T) {
	t.Parallel()

	cfg := runToolConfig(t)
	responses := runToolResponses("echoer")

	var stdout runSyncBuffer
	_, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		Format: run.FormatNDJSON,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
			// The first round completes, then the fake runs dry.
			return &runFakeClient{responses: responses[:1]}, nil
		},
	}, "run it")
	if err == nil {
		t.Fatal("Run succeeded, want a turn error")
	}

	lines := parseNDJSONLines(t, stdout.String())
	if got := ndjsonTypes(lines); !slicesEqual(got, []string{"usage", "tool_call", "tool_result", "error"}) {
		t.Fatalf("line types = %v, want the completed round's events then error", got)
	}
	last := lines[len(lines)-1]
	if last.Error == "" || !strings.Contains(last.Error, "no more canned responses") {
		t.Errorf("error event = %+v, want the run error's message", last)
	}
}

// TestRunFormatNDJSONCancelledSingleErrorLine pins that a run cancelled
// before any call still ends the stream with a single error line (the
// turn was entered).
func TestRunFormatNDJSONCancelledSingleErrorLine(t *testing.T) {
	t.Parallel()

	cfg := runTestConfig(runTestAgent())
	var stdout runSyncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := run.Run(ctx, run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		Format: run.FormatNDJSON,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
			return &runBlockingClient{started: make(chan struct{})}, nil
		},
	}, "hello")
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}

	lines := parseNDJSONLines(t, stdout.String())
	if got := ndjsonTypes(lines); !slicesEqual(got, []string{"error"}) {
		t.Fatalf("line types = %v, want exactly [error]", got)
	}
	if !strings.Contains(lines[0].Error, "context canceled") {
		t.Errorf("error event = %+v, want the cancellation message", lines[0])
	}
}

// TestRunFormatNDJSONNoLinesWhenTurnNeverStarted pins that a run failing
// before the turn (registry build failure) emits no ndjson lines at all.
func TestRunFormatNDJSONNoLinesWhenTurnNeverStarted(t *testing.T) {
	t.Parallel()

	cfg := runTestConfig(runTestAgent(), config.ToolEntry{
		Type: config.ToolTypeCommand,
		Name: "bad", Description: "Bad tool.", Command: []string{},
	})

	var stdout runSyncBuffer
	_, err := run.Run(context.Background(), run.Options{
		Config:    cfg,
		Agent:     cfg.Agents[0],
		Stdout:    &stdout,
		Format:    run.FormatNDJSON,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) { return &runFakeClient{}, nil },
	}, "hello")
	if err == nil || !strings.Contains(err.Error(), "build tools") {
		t.Fatalf("error = %v, want a build-tools error", err)
	}
	if out := stdout.String(); out != "" {
		t.Errorf("stdout = %q, want no ndjson lines", out)
	}
}

// TestRunFormatNDJSONTracedTerminateMidRun pins the platform-termination
// contract: done with no text, and the reason on stderr.
func TestRunFormatNDJSONTracedTerminateMidRun(t *testing.T) {
	t.Parallel()

	f, srv := newRunPFServer(t)
	// StartTurn creates two spans (agent_turn + user_message); fire the
	// terminate on the next create (the llm span), so the turn was
	// entered and the stream carries its terminal done event.
	f.setTerminateAfterSpanCreates(2, "operator stopped")
	cfg := runTestConfig(runTestAgent())
	var stdout, stderr runSyncBuffer
	opts := runTracedOptions(cfg, srv, &stdout,
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "hi!"), FinishReason: llm.FinishStop},
	)
	opts.Stderr = &stderr
	opts.Format = run.FormatNDJSON

	final, err := run.Run(context.Background(), opts, "hello")
	if err != nil {
		t.Fatalf("Run error = %v, want nil (terminate exits cleanly)", err)
	}
	if final != "" {
		t.Errorf("final = %q, want empty", final)
	}

	lines := parseNDJSONLines(t, stdout.String())
	if got := ndjsonTypes(lines); len(got) == 0 || got[len(got)-1] != "done" {
		t.Fatalf("line types = %v, want the stream to end with done", got)
	}
	done := lines[len(lines)-1]
	if done.Text != "" {
		t.Errorf("done.text = %q, want absent (terminated mid-run)", done.Text)
	}
	if diag := stderr.String(); !strings.Contains(diag, "stopped by platform") || !strings.Contains(diag, "operator stopped") {
		t.Errorf("stderr = %q, want the termination reason", diag)
	}
	if out := stdout.String(); strings.Contains(out, "stopped by platform") {
		t.Errorf("stdout = %q, want no human termination notice on the event stream", out)
	}
}

// TestRunFormatNDJSONFooterSuppressed pins that stdout carries no
// "tokens:" line; the totals live only in done.
func TestRunFormatNDJSONFooterSuppressed(t *testing.T) {
	t.Parallel()

	cfg := runTestConfig(runTestAgent())
	var stdout runSyncBuffer
	_, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		Format: run.FormatNDJSON,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
			return &runFakeClient{responses: []llm.Response{
				{
					Message:      llm.NewTextMessage(llm.RoleAssistant, "the answer"),
					FinishReason: llm.FinishStop,
					Usage:        llm.Usage{PromptTokens: 12, CompletionTokens: 34, TotalTokens: 46},
				},
			}}, nil
		},
	}, "hello")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if out := stdout.String(); strings.Contains(out, "tokens:") {
		t.Errorf("stdout = %q, want no human footer", out)
	}
}

// TestRunFormatNDJSONNoHTMLEscaping pins that tool output containing <, >,
// and & round-trips unescaped in the JSON line.
func TestRunFormatNDJSONNoHTMLEscaping(t *testing.T) {
	t.Parallel()

	cfg := runTestConfig(runTestAgent(), config.ToolEntry{
		Type:        config.ToolTypeCommand,
		Name:        "htmler",
		Description: "Emits HTML-ish output.",
		Command:     []string{"printf", "a < b & c > d"},
	})

	var stdout runSyncBuffer
	_, err := run.Run(context.Background(), run.Options{
		Config: cfg,
		Agent:  cfg.Agents[0],
		Stdout: &stdout,
		Format: run.FormatNDJSON,
		NewClient: func(config.Config, config.Agent) (llm.Client, error) {
			return &runFakeClient{responses: runToolResponses("htmler")}, nil
		},
	}, "run it")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	out := stdout.String()
	if strings.Contains(out, "\\u003c") || strings.Contains(out, "\\u003e") || strings.Contains(out, "\\u0026") {
		t.Errorf("stdout = %q, want no HTML-escaped sequences", out)
	}
	var found bool
	for _, l := range parseNDJSONLines(t, out) {
		if l.Type == "tool_result" {
			found = true
			if l.Output != "a < b & c > d" {
				t.Errorf("tool_result output = %q, want %q", l.Output, "a < b & c > d")
			}
		}
	}
	if !found {
		t.Fatalf("no tool_result line in stream:\n%s", out)
	}
}

// TestRunFormatNDJSONTracedTurnCompleteFailureStillFinishesSession pins
// that the ndjson epilogue never strands a traced session: even when a
// post-turn tracer failure fails the run, the session is still finished
// (failed status) and the stream ends with an error event — done is never
// the last word on a non-zero exit.
func TestRunFormatNDJSONTracedTurnCompleteFailureStillFinishesSession(t *testing.T) {
	t.Parallel()

	f, srv := newRunPFServer(t)
	// The turn completes, but the turn-span finish (assistant_text
	// payload) fails, failing the run after RunTurn returned nil.
	f.setSpanFinishFail(http.StatusInternalServerError, "assistant_text")
	cfg := runTestConfig(runTestAgent())
	var stdout runSyncBuffer
	opts := runTracedOptions(cfg, srv, &stdout,
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "hi!"), FinishReason: llm.FinishStop},
	)
	opts.Format = run.FormatNDJSON

	_, err := run.Run(context.Background(), opts, "hello")
	if err == nil {
		t.Fatal("Run succeeded, want the turn.Complete failure to surface")
	}

	// The session is finished as failed despite the post-turn failure.
	var finished bool
	for _, c := range f.finishCalls() {
		if strings.HasPrefix(c.Path, "/agent_instance/") {
			finished = true
			if c.Body["status"] != prefactor.InstanceFailed {
				t.Errorf("instance finish status = %v, want %q", c.Body["status"], prefactor.InstanceFailed)
			}
		}
	}
	if !finished {
		t.Error("no instance finish recorded, want the session finished even on the post-turn failure")
	}

	// The stream ends with error, not done: the outcome mapping and
	// session finish run before the terminal event.
	lines := parseNDJSONLines(t, stdout.String())
	if got := ndjsonTypes(lines); got[len(got)-1] != "error" {
		t.Fatalf("line types = %v, want the stream to end with error", got)
	}
	last := lines[len(lines)-1]
	if last.Error == "" {
		t.Errorf("error event = %+v, want the run error's message", last)
	}
}

// TestRunFormatNDJSONFinishFailureStillFinishesSession pins the ndjson
// sink's own failure handling: when emitting the terminal event fails
// (marshal/write bug), the run surfaces the error, and the traced session
// is still finished — not stranded. The instance records the agent's
// work, which completed (the turn ran to completion; only the CLI's
// output write failed), so it finishes complete; the run still exits
// non-zero via the returned error.
func TestRunFormatNDJSONFinishFailureStillFinishesSession(t *testing.T) {
	t.Parallel()

	f, srv := newRunPFServer(t)
	cfg := runTestConfig(runTestAgent())
	var stdout runSyncBuffer
	opts := runTracedOptions(cfg, srv, &stdout,
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "hi!"), FinishReason: llm.FinishStop},
	)
	opts.Format = run.FormatNDJSON
	// The stdout writer fails only when the terminal event is emitted
	// (after the per-event lines), so the turn itself runs to completion.
	opts.Stdout = &runFailOnWriter{w: &stdout, marker: `"type":"done"`}

	_, err := run.Run(context.Background(), opts, "hello")
	if err == nil {
		t.Fatal("Run succeeded, want the ndjson finish failure to surface")
	}

	// The session is finished, not stranded: the instance records the
	// agent's completed turn.
	var finished bool
	for _, c := range f.finishCalls() {
		if strings.HasPrefix(c.Path, "/agent_instance/") {
			finished = true
			if c.Body["status"] != prefactor.InstanceComplete {
				t.Errorf("instance finish status = %v, want %q (the agent's turn completed)", c.Body["status"], prefactor.InstanceComplete)
			}
		}
	}
	if !finished {
		t.Error("no instance finish recorded, want the session finished even when the ndjson finish fails")
	}
}

// slicesEqual reports whether two string slices are equal (the run tests
// target a pre-slices stdlib Go; kept local to avoid a helper import).
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
