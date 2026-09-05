package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/run"
)

func TestRunVersionCommand(t *testing.T) {
	cmd := rootCommand()
	cmd.Version = "1.2.3"
	out := &bytes.Buffer{}
	cmd.Writer = out

	if err := cmd.Run(context.Background(), []string{"blorb", "version"}); err != nil {
		t.Fatalf("run version: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "1.2.3" {
		t.Errorf("version output = %q, want %q", got, "1.2.3")
	}
}

func TestRunVersionFlag(t *testing.T) {
	cmd := rootCommand()
	cmd.Version = "1.2.3"
	out := &bytes.Buffer{}
	cmd.Writer = out

	if err := cmd.Run(context.Background(), []string{"blorb", "--version"}); err != nil {
		t.Fatalf("run --version: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "1.2.3") {
		t.Errorf("version output %q missing %q", got, "1.2.3")
	}
}

func TestRunHelpCommand(t *testing.T) {
	cmd := rootCommand()
	out := &bytes.Buffer{}
	cmd.Writer = out

	if err := cmd.Run(context.Background(), []string{"blorb", "help"}); err != nil {
		t.Fatalf("run help: %v", err)
	}
	for _, want := range []string{"chat", "version"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help output missing %q; got:\n%s", want, out.String())
		}
	}
}

// capturingExitHandler returns an ExitErrHandler that prints errors to w
// like cli's default handler but does not exit the process.
func capturingExitHandler(w io.Writer) cli.ExitErrHandlerFunc {
	return func(ctx context.Context, cmd *cli.Command, err error) {
		if err != nil {
			fmt.Fprintln(w, err)
		}
	}
}

func TestRunUnknownCommand(t *testing.T) {
	cmd := rootCommand()
	errOut := &bytes.Buffer{}
	cmd.ErrWriter = errOut
	cmd.ExitErrHandler = capturingExitHandler(errOut)

	if err := cmd.Run(context.Background(), []string{"blorb", "teleport"}); err == nil {
		t.Error("run(teleport) succeeded, want an error")
	}
	if got := errOut.String(); !strings.Contains(got, "teleport") {
		t.Errorf("error output %q missing %q", got, "teleport")
	}
}

func TestChatBadFlag(t *testing.T) {
	cmd := rootCommand()
	cmd.ErrWriter = &bytes.Buffer{}

	if err := cmd.Run(context.Background(), []string{"blorb", "chat", "--nope"}); err == nil {
		t.Error("chat --nope succeeded, want an error")
	}
}

func TestChatMissingConfig(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "blorb.json")

	errOut := &bytes.Buffer{}
	cmd := rootCommand()
	cmd.ErrWriter = errOut
	cmd.ExitErrHandler = capturingExitHandler(errOut)

	if err := cmd.Run(context.Background(), []string{"blorb", "chat", "-c", missing}); err == nil {
		t.Errorf("chat -c %s succeeded, want an error", missing)
	}
	if got := errOut.String(); !strings.Contains(got, missing) {
		t.Errorf("error output %q missing path %q", got, missing)
	}
}

func TestChatDefaultConfigPathFlag(t *testing.T) {
	cmd := chatCommand()
	flag, ok := cmd.Flags[0].(*cli.StringFlag)
	if !ok {
		t.Fatalf("first flag is %T, want *cli.StringFlag", cmd.Flags[0])
	}
	if flag.Value != config.DefaultPath {
		t.Errorf("config default = %q, want %q", flag.Value, config.DefaultPath)
	}
	if flag.Name != "config" || len(flag.Aliases) != 1 || flag.Aliases[0] != "c" {
		t.Errorf("unexpected flag name/aliases: %q %v", flag.Name, flag.Aliases)
	}
}

func TestChatNoStreamFlag(t *testing.T) {
	cmd := chatCommand()

	var noStream *cli.BoolFlag
	for _, f := range cmd.Flags {
		if bf, ok := f.(*cli.BoolFlag); ok && f.Names()[0] == "no-stream" {
			noStream = bf
			break
		}
	}
	if noStream == nil {
		t.Fatalf("chat has no no-stream flag; flags = %+v", cmd.Flags)
	}
	if noStream.Value {
		t.Errorf("no-stream default = true, want false (streaming on by default)")
	}
}

func TestChatToolOutputFlag(t *testing.T) {
	cmd := chatCommand()

	var toolOutput *cli.BoolFlag
	for _, f := range cmd.Flags {
		if bf, ok := f.(*cli.BoolFlag); ok && f.Names()[0] == "tool-output" {
			toolOutput = bf
			break
		}
	}
	if toolOutput == nil {
		t.Fatalf("chat has no tool-output flag; flags = %+v", cmd.Flags)
	}
	if toolOutput.Value {
		t.Errorf("tool-output default = true, want false (tool output off by default)")
	}
}

func TestChatCommandHasNoNewFlags(t *testing.T) {
	cmd := chatCommand()
	if len(cmd.Flags) != 4 {
		t.Errorf("chat has %d flags, want 4 (config, no-stream, tool-output, agent): %+v", len(cmd.Flags), cmd.Flags)
	}
	for _, f := range cmd.Flags {
		if f.Names()[0] == "logging" {
			t.Error("chat has a logging flag; logging must be configured via blorb.json only")
		}
	}
	// The agent is selected via a flag, not a positional argument.
	if len(cmd.Arguments) != 0 {
		t.Errorf("chat has %d positional arguments, want 0: %+v", len(cmd.Arguments), cmd.Arguments)
	}
}

// cannedReplyJSON is the canned non-streaming chat completions reply.
const cannedReplyJSON = `{"id":"r","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`

// respondCanned replies to a chat-completions request with the canned
// reply: an SSE stream when the request asks for one (stream:true), else
// plain JSON.
func respondCanned(w http.ResponseWriter, body []byte) {
	var req struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		sseData(w,
			`{"id":"r","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`{"id":"r","choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`,
			`{"id":"r","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(cannedReplyJSON))
}

// newFakeProviderServer returns an OpenAI-compatible chat completions
// server that always answers with the canned reply, so agent-selection
// tests run real sessions cheaply and observe the banner.
func newFakeProviderServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		respondCanned(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// sseData writes each payload as an SSE data line followed by a blank
// line, then [DONE].
func sseData(w io.Writer, payloads ...string) {
	for _, p := range payloads {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", p)
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

// writeAgentConfig writes a config with the given agent names, an optional
// default_agent (empty means none), and one top-level model entry per
// agent (named after the agent, wire model model-<name>) pointing at
// baseURL. Returns the config path.
func writeAgentConfig(t *testing.T, dir string, names []string, defaultAgent string, baseURL string) string {
	t.Helper()

	models := make([]map[string]any, len(names))
	agents := make([]map[string]any, len(names))
	for i, name := range names {
		models[i] = map[string]any{
			"name":       name,
			"provider":   "local",
			"model_name": "model-" + name,
		}
		agents[i] = map[string]any{
			"name":          name,
			"system_prompt": fmt.Sprintf("You are %s.", name),
			"model":         name,
			"max_turns":     1,
		}
	}
	base := map[string]any{
		"providers": []map[string]any{{
			"name":     "local",
			"type":     "openai-compatible",
			"base_url": baseURL,
		}},
		"models": models,
		"agents": agents,
	}
	if defaultAgent != "" {
		base["default_agent"] = defaultAgent
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

// runChatCommand runs `blorb chat -c cfgPath [args...]` over pipe-backed
// stdio and returns the captured stdout plus the session error. Errors
// handed to the ExitErrHandler (cli.Exit) are returned with the cli
// formatting stripped.
func runChatCommand(t *testing.T, cfgPath string, args ...string) (string, error) {
	t.Helper()

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	if _, err := io.WriteString(stdinW, "hi\nexit\n"); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	if err := stdinW.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	oldStdin, oldStdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = stdinR, stdoutW
	defer func() { os.Stdin, os.Stdout = oldStdin, oldStdout }()

	cmd := rootCommand()
	cmd.Writer = io.Discard
	cmd.ErrWriter = io.Discard
	errOut := &bytes.Buffer{}
	cmd.ExitErrHandler = capturingExitHandler(errOut)

	argv := append([]string{"blorb", "chat", "-c", cfgPath}, args...)
	runErr := cmd.Run(context.Background(), argv)

	if err := stdoutW.Close(); err != nil {
		t.Fatalf("close stdout: %v", err)
	}
	var out bytes.Buffer
	if _, err := io.Copy(&out, stdoutR); err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if err := stdoutR.Close(); err != nil {
		t.Fatalf("close stdout: %v", err)
	}

	// cli.Exit errors are reported through the ExitErrHandler; that is the
	// user-visible failure, so return it as the run error.
	if errOut.Len() > 0 {
		return out.String(), errors.New(errOut.String())
	}
	return out.String(), runErr
}

// runRunCommand runs `blorb run -c cfgPath [args...]` over pipe-backed
// stdio with the given stdin payload, and returns the captured stdout and
// stderr plus the session error. Errors handed to the ExitErrHandler
// (cli.Exit) are returned with the cli formatting stripped.
func runRunCommand(t *testing.T, cfgPath string, stdin string, args ...string) (string, string, error) {
	t.Helper()

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	if _, err := io.WriteString(stdinW, stdin); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	if err := stdinW.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	oldStdin, oldStdout, oldStderr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = stdinR, stdoutW, stderrW
	defer func() { os.Stdin, os.Stdout, os.Stderr = oldStdin, oldStdout, oldStderr }()

	cmd := rootCommand()
	cmd.Writer = io.Discard
	cmd.ErrWriter = io.Discard
	errOut := &bytes.Buffer{}
	cmd.ExitErrHandler = capturingExitHandler(errOut)

	argv := append([]string{"blorb", "run", "-c", cfgPath}, args...)
	runErr := cmd.Run(context.Background(), argv)

	if err := stdoutW.Close(); err != nil {
		t.Fatalf("close stdout: %v", err)
	}
	if err := stderrW.Close(); err != nil {
		t.Fatalf("close stderr: %v", err)
	}
	var out, errBuf bytes.Buffer
	if _, err := io.Copy(&out, stdoutR); err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if err := stdoutR.Close(); err != nil {
		t.Fatalf("close stdout: %v", err)
	}
	if _, err := io.Copy(&errBuf, stderrR); err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	if err := stderrR.Close(); err != nil {
		t.Fatalf("close stderr: %v", err)
	}

	if errOut.Len() > 0 {
		return out.String(), errBuf.String(), errors.New(errOut.String())
	}
	return out.String(), errBuf.String(), runErr
}

// TestRunOneTurnCommand is the happy path: a literal prompt runs one turn
// over the fake provider and exits 0.
func TestRunOneTurnCommand(t *testing.T) {
	srv := newFakeProviderServer(t)
	cfgPath := writeAgentConfig(t, t.TempDir(), []string{"helper"}, "helper", srv.URL)

	out, _, err := runRunCommand(t, cfgPath, "", "say hi")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if !strings.Contains(out, ">>> Assistant:") || !strings.Contains(out, "hi") {
		t.Errorf("stdout = %q, want the chat-style rendered reply", out)
	}
	if strings.Contains(out, ">>> User:") {
		t.Errorf("stdout = %q, want no >>> User heading", out)
	}
}

// TestRunPromptFromFileCommand verifies the @file prompt form: the
// provider receives the file's trimmed content as the user message.
func TestRunPromptFromFileCommand(t *testing.T) {
	var gotContent string
	srv := requestCapturingProvider(t, &gotContent)

	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(promptFile, []byte("hello file\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfgPath := writeAgentConfig(t, dir, []string{"helper"}, "helper", srv.URL)

	if _, _, err := runRunCommand(t, cfgPath, "", "@"+promptFile); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if !strings.Contains(gotContent, `"content":"hello file"`) {
		t.Errorf("provider request = %q, want the file's content as the user message", gotContent)
	}
}

// TestRunPromptFromStdinCommands verifies both stdin forms: bare `-` and
// `@-`.
func TestRunPromptFromStdinCommand(t *testing.T) {
	t.Run("dash reads stdin", func(t *testing.T) {
		var gotContent string
		srv := requestCapturingProvider(t, &gotContent)
		cfgPath := writeAgentConfig(t, t.TempDir(), []string{"helper"}, "helper", srv.URL)

		if _, _, err := runRunCommand(t, cfgPath, "piped prompt\n", "-"); err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if !strings.Contains(gotContent, `"content":"piped prompt"`) {
			t.Errorf("provider request = %q, want the piped prompt", gotContent)
		}
	})

	t.Run("at-dash reads stdin", func(t *testing.T) {
		var gotContent string
		srv := requestCapturingProvider(t, &gotContent)
		cfgPath := writeAgentConfig(t, t.TempDir(), []string{"helper"}, "helper", srv.URL)

		if _, _, err := runRunCommand(t, cfgPath, "piped prompt\n", "@-"); err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if !strings.Contains(gotContent, `"content":"piped prompt"`) {
			t.Errorf("provider request = %q, want the piped prompt", gotContent)
		}
	})
}

// requestCapturingProvider returns a fake provider server that records the
// last request body into *got and answers with the canned reply.
func requestCapturingProvider(t *testing.T, got *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*got = string(body)
		respondCanned(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRunDoubleAtEscapeCommand verifies the @@ escape strips one @ and
// passes the remainder verbatim.
func TestRunDoubleAtEscapeCommand(t *testing.T) {
	var gotContent string
	srv := requestCapturingProvider(t, &gotContent)
	cfgPath := writeAgentConfig(t, t.TempDir(), []string{"helper"}, "helper", srv.URL)

	if _, _, err := runRunCommand(t, cfgPath, "", "@@ @at-start"); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if !strings.Contains(gotContent, `"content":"@ @at-start"`) {
		t.Errorf("provider request = %q, want the escaped literal @ @at-start", gotContent)
	}
}

// TestRunNoPromptArgIsUsageError pins that an omitted prompt fails loudly
// instead of appearing to hang reading stdin.
func TestRunNoPromptArgIsUsageError(t *testing.T) {
	srv := newFakeProviderServer(t)
	cfgPath := writeAgentConfig(t, t.TempDir(), []string{"helper"}, "helper", srv.URL)

	_, _, err := runRunCommand(t, cfgPath, "")
	if err == nil {
		t.Fatal("run with no prompt succeeded, want a usage error")
	}
	if !strings.Contains(err.Error(), "run: no prompt given") {
		t.Errorf("error = %v, want the no-prompt usage error", err)
	}
}

// TestRunExtraArgsIsUsageError pins that extra positional args fail loudly
// and never reach the provider.
func TestRunExtraArgsIsUsageError(t *testing.T) {
	var gotContent string
	srv := requestCapturingProvider(t, &gotContent)
	cfgPath := writeAgentConfig(t, t.TempDir(), []string{"helper"}, "helper", srv.URL)

	_, _, err := runRunCommand(t, cfgPath, "", "a", "b")
	if err == nil {
		t.Fatal("run with extra args succeeded, want a usage error")
	}
	if !strings.Contains(err.Error(), "run: unexpected arguments after the prompt") || !strings.Contains(err.Error(), "b") {
		t.Errorf("error = %v, want the extra-args usage error naming the extras", err)
	}
	if gotContent != "" {
		t.Errorf("provider request = %q, want no provider call", gotContent)
	}
}

// TestRunMissingPromptFileCommand verifies a bad @file reference surfaces
// the file read failure.
func TestRunMissingPromptFileCommand(t *testing.T) {
	srv := newFakeProviderServer(t)
	cfgPath := writeAgentConfig(t, t.TempDir(), []string{"helper"}, "helper", srv.URL)

	_, _, err := runRunCommand(t, cfgPath, "", "@/no/such/prompt.txt")
	if err == nil || !strings.Contains(err.Error(), "run: read prompt file") {
		t.Errorf("error = %v, want the file read failure", err)
	}
}

// TestRunNoStreamCommand verifies the --no-stream flag reaches the
// provider: the request body must not ask for streaming (the default path
// sends "stream":true).
func TestRunNoStreamCommand(t *testing.T) {
	var gotContent string
	srv := requestCapturingProvider(t, &gotContent)
	cfgPath := writeAgentConfig(t, t.TempDir(), []string{"helper"}, "helper", srv.URL)

	out, _, err := runRunCommand(t, cfgPath, "", "--no-stream", "say hi")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if !strings.Contains(out, ">>> Assistant:") || !strings.Contains(out, "hi") {
		t.Errorf("stdout = %q, want the whole-message rendered reply", out)
	}
	var req map[string]any
	if err := json.Unmarshal([]byte(gotContent), &req); err != nil {
		t.Fatalf("unmarshal request: %v (body %q)", err, gotContent)
	}
	// The wire request omits "stream" when false; the default streamed
	// path sends "stream":true (pinned by the request-asserting tests).
	if _, ok := req["stream"]; ok {
		t.Errorf("request stream = %v, want it omitted with --no-stream (body %q)", req["stream"], gotContent)
	}
}

// TestRunAgentFlagCommand verifies --agent selects the agent for the run:
// writeAgentConfig gives each agent model-<name>, so the captured request
// body proves alpha ran over the default beta.
func TestRunAgentFlagCommand(t *testing.T) {
	var gotContent string
	srv := requestCapturingProvider(t, &gotContent)
	cfgPath := writeAgentConfig(t, t.TempDir(), []string{"alpha", "beta"}, "beta", srv.URL)

	out, _, err := runRunCommand(t, cfgPath, "", "--agent", "alpha", "--no-stream", "hello")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if !strings.Contains(out, ">>> Assistant:") {
		t.Errorf("stdout = %q, want the rendered reply", out)
	}
	var req map[string]any
	if err := json.Unmarshal([]byte(gotContent), &req); err != nil {
		t.Fatalf("unmarshal request: %v (body %q)", err, gotContent)
	}
	if req["model"] != "model-alpha" {
		t.Errorf("request model = %v, want model-alpha (--agent alpha over the default beta)", req["model"])
	}
}

// TestRunToolOutputFlagCommand verifies --tool-output renders the full
// tool result body in run mode.
func TestRunToolOutputFlagCommand(t *testing.T) {
	var calls int
	toolCallJSON := `{"id":"call_1","type":"function","function":{"name":"echo","arguments":"{}"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = w.Write([]byte(fmt.Sprintf(`{"id":"resp-1","choices":[{"message":{"role":"assistant","content":"","tool_calls":[%s]},"finish_reason":"tool_calls"}]}`, toolCallJSON)))
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp-2","choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	data, err := json.Marshal(map[string]any{
		"default_agent": "helper",
		"providers": []map[string]any{{
			"name":     "local",
			"type":     "openai-compatible",
			"base_url": srv.URL,
		}},
		"models": []map[string]any{{
			"name":       "m",
			"provider":   "local",
			"model_name": "m",
		}},
		"agents": []map[string]any{{
			"name":          "helper",
			"system_prompt": "You are helpful.",
			"model":         "m",
			"max_turns":     3,
			"tools":         []string{"echo"},
		}},
		"tools": []map[string]any{{
			"type":        "command",
			"name":        "echo",
			"description": "Echoes.",
			"command":     []string{"echo", "tool body here"},
		}},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	cfgPath := filepath.Join(dir, "blorb.json")
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	out, _, runErr := runRunCommand(t, cfgPath, "", "--no-stream", "--tool-output", "run the tool")
	if runErr != nil {
		t.Fatalf("Run error = %v, want nil", runErr)
	}
	if !strings.Contains(out, ">>> Result: Tool: echo") || !strings.Contains(out, "tool body here") {
		t.Errorf("stdout = %q, want the full tool output body", out)
	}
}

// TestRunChatCommandStillRegisteredAfterRun guards the subcommand list
// shape after inserting run between chat and version.
func TestRunChatCommandStillRegisteredAfterRun(t *testing.T) {
	cmd := rootCommand()
	var names []string
	for _, sub := range cmd.Commands {
		names = append(names, sub.Name)
	}
	joined := strings.Join(names, ",")
	for _, want := range []string{"chat", "run", "version"} {
		if !strings.Contains(joined, want) {
			t.Errorf("subcommands = %q, want %q present", joined, want)
		}
	}
}

func TestChatAgentArgument(t *testing.T) {
	srv := newFakeProviderServer(t)

	t.Run("no arg uses default_agent", func(t *testing.T) {
		cfgPath := writeAgentConfig(t, t.TempDir(), []string{"alpha", "beta"}, "beta", srv.URL)

		out, err := runChatCommand(t, cfgPath)
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if !strings.Contains(out, "beta") || !strings.Contains(out, "model-beta") {
			t.Errorf("stdout = %q, want the banner naming the default agent and its model", out)
		}
	})

	t.Run("--agent overrides the default", func(t *testing.T) {
		cfgPath := writeAgentConfig(t, t.TempDir(), []string{"alpha", "beta"}, "beta", srv.URL)

		out, err := runChatCommand(t, cfgPath, "--agent", "alpha")
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if !strings.Contains(out, "alpha") || !strings.Contains(out, "model-alpha") {
			t.Errorf("stdout = %q, want the banner naming agent alpha and its model", out)
		}
	})

	t.Run("unknown agent fails", func(t *testing.T) {
		cfgPath := writeAgentConfig(t, t.TempDir(), []string{"alpha", "beta"}, "beta", srv.URL)

		_, err := runChatCommand(t, cfgPath, "--agent", "gamma")
		if err == nil || !strings.Contains(err.Error(), `agent "gamma" is not defined in the config`) {
			t.Errorf("error = %v, want the not-defined error mentioning gamma", err)
		}
	})

	t.Run("no default and no flag lists available agents", func(t *testing.T) {
		cfgPath := writeAgentConfig(t, t.TempDir(), []string{"alpha", "beta"}, "", srv.URL)

		_, err := runChatCommand(t, cfgPath)
		if err == nil {
			t.Fatal("chat with no agent and no default succeeded, want an error")
		}
		if !strings.Contains(err.Error(), "no agent given and no default_agent configured") {
			t.Errorf("error = %v, want the available-agents error", err)
		}
		if !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "beta") {
			t.Errorf("error = %q, want both agent names listed", err)
		}
	})

	t.Run("no default but explicit flag", func(t *testing.T) {
		cfgPath := writeAgentConfig(t, t.TempDir(), []string{"alpha", "beta"}, "", srv.URL)

		out, err := runChatCommand(t, cfgPath, "--agent", "alpha")
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if !strings.Contains(out, "alpha") || !strings.Contains(out, "model-alpha") {
			t.Errorf("stdout = %q, want the banner naming agent alpha", out)
		}
	})
}

func TestChatPrefactorMissingEnvVarExits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blorb.json")
	cfgJSON := `{
		"default_agent": "helper",
		"providers": [{"name": "local", "type": "openai-compatible", "base_url": "http://localhost:1"}],
		"models": [{"name": "m", "provider": "local", "model_name": "m"}],
		"agents": [
			{
				"name": "helper",
				"system_prompt": "You are helpful.",
				"model": "m",
				"max_turns": 1
			}
		],
		"prefactor": {"api_token_env": "BLORB_TEST_PF_TOKEN"}
	}`
	if err := os.WriteFile(path, []byte(cfgJSON), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Ensure the env var is unset (empty).
	t.Setenv("BLORB_TEST_PF_TOKEN", "")

	errOut := &bytes.Buffer{}
	cmd := rootCommand()
	cmd.ErrWriter = errOut
	cmd.ExitErrHandler = capturingExitHandler(errOut)

	err := cmd.Run(context.Background(), []string{"blorb", "chat", "-c", path})
	if err == nil {
		t.Error("chat with a prefactor block but a missing token env var succeeded, want an error")
	}
	if got := errOut.String(); !strings.Contains(got, "prefactor") || !strings.Contains(got, "BLORB_TEST_PF_TOKEN") {
		t.Errorf("error output %q missing the prefactor env-var mention", got)
	}
}

// TestRunPrefactorMissingEnvVarExits mirrors the chat case: a config with
// a prefactor block but a missing token env var exits 1 with the
// env-var error.
func TestRunPrefactorMissingEnvVarExits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blorb.json")
	cfgJSON := `{
		"default_agent": "helper",
		"providers": [{"name": "local", "type": "openai-compatible", "base_url": "http://localhost:1"}],
		"models": [{"name": "m", "provider": "local", "model_name": "m"}],
		"agents": [
			{
				"name": "helper",
				"system_prompt": "You are helpful.",
				"model": "m",
				"max_turns": 1
			}
		],
		"prefactor": {"api_token_env": "BLORB_TEST_PF_RUN_TOKEN"}
	}`
	if err := os.WriteFile(path, []byte(cfgJSON), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Setenv("BLORB_TEST_PF_RUN_TOKEN", "")

	errOut := &bytes.Buffer{}
	cmd := rootCommand()
	cmd.ErrWriter = errOut
	cmd.ExitErrHandler = capturingExitHandler(errOut)

	err := cmd.Run(context.Background(), []string{"blorb", "run", "-c", path, "hi"})
	if err == nil {
		t.Error("run with a prefactor block but a missing token env var succeeded, want an error")
	}
	if got := errOut.String(); !strings.Contains(got, "prefactor") || !strings.Contains(got, "BLORB_TEST_PF_RUN_TOKEN") {
		t.Errorf("error output %q missing the prefactor env-var mention", got)
	}
}

// --- models command tests ---

// runModelsCommand runs the models subcommand against cfgPath and returns
// its stdout plus the outcome error. The models command's output is written
// through cmd.Writer, so this harness captures that rather than os.Stdout.
func runModelsCommand(t *testing.T, cfgPath string, env map[string]string) (string, error) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}

	cmd := rootCommand()
	out := &bytes.Buffer{}
	cmd.Writer = out
	errOut := &bytes.Buffer{}
	cmd.ExitErrHandler = capturingExitHandler(errOut)

	runErr := cmd.Run(context.Background(), []string{"blorb", "models", "-c", cfgPath})
	if errOut.Len() > 0 {
		return out.String(), errors.New(errOut.String())
	}
	return out.String(), runErr
}

// writeModelsConfig writes an openai-compatible provider pointing at
// baseURL with the named models; withOffline adds a second, unreachable
// ollama provider. Returns the config path.
func writeModelsConfig(t *testing.T, baseURL string, withOffline bool, modelNames ...string) string {
	t.Helper()

	models := make([]map[string]any, len(modelNames))
	for i, name := range modelNames {
		models[i] = map[string]any{
			"name":       name,
			"provider":   "local",
			"model_name": "model-" + name,
		}
	}
	providers := []map[string]any{
		{"name": "local", "type": "openai-compatible", "base_url": baseURL},
	}
	if withOffline {
		providers = append(providers, map[string]any{
			"name": "offline", "type": "ollama", "base_url": "http://localhost:1",
		})
	}
	base := map[string]any{
		"providers": providers,
		"models":    models,
		"agents": []map[string]any{{
			"name":          "helper",
			"system_prompt": "You are helpful.",
			"model":         modelNames[0],
			"max_turns":     1,
		}},
	}

	data, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "blorb.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// newFakeModelsServer serves an OpenAI /models list containing the given
// model ids.
func newFakeModelsServer(t *testing.T, ids ...string) *httptest.Server {
	t.Helper()
	entries := make([]string, len(ids))
	for i, id := range ids {
		entries[i] = fmt.Sprintf(`{"id":%q,"created":1700000000,"owned_by":"org"}`, id)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"object":"list","data":[%s]}`, strings.Join(entries, ","))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestModelsCommandAllInstalled(t *testing.T) {
	srv := newFakeModelsServer(t, "model-helper", "model-extra")
	cfgPath := writeModelsConfig(t, srv.URL, false, "helper")

	out, err := runModelsCommand(t, cfgPath, nil)
	if err != nil {
		t.Fatalf("models command error = %v, want nil", err)
	}
	if !strings.Contains(out, "provider local (openai-compatible, "+srv.URL+")") {
		t.Errorf("output %q missing the provider block heading", out)
	}
	if !strings.Contains(out, "model-helper (installed)") {
		t.Errorf("output %q missing the installed marking", out)
	}
	if !strings.Contains(out, "model-extra\n") {
		t.Errorf("output %q missing the unconfigured server model", out)
	}
	if !strings.Contains(out, "provider local (openai-compatible, "+srv.URL+")") && strings.Count(out, "provider ") != 1 {
		t.Errorf("output %q should carry exactly one provider block", out)
	}
}

func TestModelsCommandTypoedModelExitsNonZero(t *testing.T) {
	// The server lists only model-helper; the config also declares a
	// misspelled model the server does not have.
	srv := newFakeModelsServer(t, "model-helper")
	cfgPath := writeModelsConfig(t, srv.URL, false, "helper", "helper-mistyped")

	out, err := runModelsCommand(t, cfgPath, nil)
	if err == nil {
		t.Fatalf("models command succeeded, want exit 1 (typo detection is the point)\noutput:\n%s", out)
	}
	if !strings.Contains(out, "model-helper (installed)") {
		t.Errorf("output %q missing the installed marking", out)
	}
	if !strings.Contains(out, "model-helper-mistyped (NOT INSTALLED)") {
		t.Errorf("output %q missing the NOT INSTALLED marking", out)
	}
}

func TestModelsCommandListingFailureReportsAndContinues(t *testing.T) {
	// The first provider's server errors on /models; the second is the
	// offline ollama provider, which fails to connect. Both failures are
	// reported and the command still exits non-zero.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server says no", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	cfgPath := writeModelsConfig(t, srv.URL, true, "helper")

	out, err := runModelsCommand(t, cfgPath, nil)
	if err == nil {
		t.Fatalf("models command succeeded, want exit 1 (listing failed)\noutput:\n%s", out)
	}
	if !strings.Contains(out, "server says no") {
		t.Errorf("output %q missing the listing error", out)
	}
	if !strings.Contains(out, "provider offline (ollama") {
		t.Errorf("output %q missing the second provider's block", out)
	}
}

func TestModelsCommandMissingAPIKeyEnv(t *testing.T) {
	srv := newFakeModelsServer(t, "model-helper")
	cfgPath := writeModelsConfig(t, srv.URL, false, "helper")

	// Add an api_key_env to the local provider, unset in the environment.
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	updated := strings.Replace(string(data),
		`"base_url":`,
		`"api_key_env": "BLORB_TEST_MODELS_KEY", "base_url":`, 1)
	if err := os.WriteFile(cfgPath, []byte(updated), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	out, err := runModelsCommand(t, cfgPath, map[string]string{"BLORB_TEST_MODELS_KEY": ""})
	if err == nil {
		t.Fatalf("models command succeeded, want exit 1 (missing env)\noutput:\n%s", out)
	}
	if !strings.Contains(out, "BLORB_TEST_MODELS_KEY") {
		t.Errorf("output %q missing the missing-env error", out)
	}
}

// --- run --format flag tests ---

// TestRunFormatFlagDefaults pins that the run command has a --format flag
// defaulting to chat.
func TestRunFormatFlagDefaults(t *testing.T) {
	cmd := runCommand()

	var format *cli.StringFlag
	for _, f := range cmd.Flags {
		if sf, ok := f.(*cli.StringFlag); ok && f.Names()[0] == "format" {
			format = sf
			break
		}
	}
	if format == nil {
		t.Fatalf("run has no format flag; flags = %+v", cmd.Flags)
	}
	if format.Value != run.FormatChat {
		t.Errorf("format default = %q, want %q", format.Value, run.FormatChat)
	}
}

// TestRunCommandHasNoNewFlags pins run's flag set: exactly 5 flags
// (config, no-stream, tool-output, agent, format), so the set stays
// deliberate.
func TestRunCommandHasNoNewFlags(t *testing.T) {
	cmd := runCommand()
	if len(cmd.Flags) != 5 {
		t.Errorf("run has %d flags, want 5 (config, no-stream, tool-output, agent, format): %+v", len(cmd.Flags), cmd.Flags)
	}
	var names []string
	for _, f := range cmd.Flags {
		names = append(names, f.Names()[0])
	}
	joined := strings.Join(names, ",")
	for _, want := range []string{"config", "no-stream", "tool-output", "agent", "format"} {
		if !strings.Contains(joined, want) {
			t.Errorf("run flags = %q, want %q present", joined, want)
		}
	}
}

// TestRunFormatPlainCommand pins the CLI-level plain contract: stdout
// carries only agent text; stderr carries headings, tool blocks, and the
// footer.
func TestRunFormatPlainCommand(t *testing.T) {
	srv := newFakeProviderServer(t)
	cfgPath := writeAgentConfig(t, t.TempDir(), []string{"helper"}, "helper", srv.URL)

	out, stderr, err := runRunCommand(t, cfgPath, "", "--format", "plain", "say hi")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if out != "hi" {
		t.Errorf("stdout = %q, want exactly the agent text", out)
	}
	if !strings.Contains(stderr, ">>> Assistant:") {
		t.Errorf("stderr = %q, want the heading on stderr", stderr)
	}
}

// TestRunFormatNDJSONCommand pins the CLI-level ndjson contract: stdout
// lines parse as the event stream ending in done.
func TestRunFormatNDJSONCommand(t *testing.T) {
	srv := newFakeProviderServer(t)
	cfgPath := writeAgentConfig(t, t.TempDir(), []string{"helper"}, "helper", srv.URL)

	out, _, err := runRunCommand(t, cfgPath, "", "--format", "ndjson", "say hi")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	var last map[string]any
	dec := json.NewDecoder(strings.NewReader(out))
	for dec.More() {
		var line map[string]any
		if err := dec.Decode(&line); err != nil {
			t.Fatalf("parsing ndjson line: %v\nstream:\n%s", err, out)
		}
		last = line
	}
	if last == nil || last["type"] != "done" {
		t.Errorf("last line = %v, want the done event", last)
	}
}

// TestRunFormatUnknownCommand pins that an unknown --format value fails
// the run naming the value and the supported formats.
func TestRunFormatUnknownCommand(t *testing.T) {
	srv := newFakeProviderServer(t)
	cfgPath := writeAgentConfig(t, t.TempDir(), []string{"helper"}, "helper", srv.URL)

	_, _, err := runRunCommand(t, cfgPath, "", "--format", "yaml", "say hi")
	if err == nil {
		t.Fatal("run with an unknown format succeeded, want an error")
	}
	if !strings.Contains(err.Error(), `unknown format "yaml"`) || !strings.Contains(err.Error(), "chat, plain, ndjson") {
		t.Errorf("error = %v, want the value and the supported formats", err)
	}
}
