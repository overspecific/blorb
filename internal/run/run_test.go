package run_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
		NewClient: func(config.Agent) (llm.Client, error) {
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
	opts.NewClient = func(config.Agent) (llm.Client, error) { return client, nil }

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
		NewClient: func(config.Agent) (llm.Client, error) {
			return &runFakeClient{responses: []llm.Response{
				{
					Message:      llm.NewTextMessage(llm.RoleAssistant, "the answer"),
					FinishReason: llm.FinishStop,
					Usage:        llm.Usage{PromptTokens: 12, CompletionTokens: 34, TotalTokens: 46},
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
	if !strings.HasSuffix(out, "tokens: 12 prompt, 34 completion, 46 total\n") {
		t.Errorf("stdout = %q, want it to end with the usage footer", out)
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
		NewClient: func(config.Agent) (llm.Client, error) {
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
		NewClient: func(config.Agent) (llm.Client, error) {
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
		NewClient: func(config.Agent) (llm.Client, error) {
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
		NewClient: func(config.Agent) (llm.Client, error) {
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
		NewClient: func(config.Agent) (llm.Client, error) {
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
			NewClient: func(config.Agent) (llm.Client, error) {
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
			NewClient: func(config.Agent) (llm.Client, error) {
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
			NewClient: func(config.Agent) (llm.Client, error) {
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
		NewClient: func(config.Agent) (llm.Client, error) {
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
		NewClient: func(config.Agent) (llm.Client, error) {
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
		NewClient: func(config.Agent) (llm.Client, error) {
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
		NewClient: func(config.Agent) (llm.Client, error) {
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
		NewClient: func(config.Agent) (llm.Client, error) {
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
