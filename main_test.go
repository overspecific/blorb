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

func TestChatCommandHasNoNewFlags(t *testing.T) {
	cmd := chatCommand()
	if len(cmd.Flags) != 3 {
		t.Errorf("chat has %d flags, want 3 (config, no-stream, agent): %+v", len(cmd.Flags), cmd.Flags)
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

// newFakeProviderServer returns an OpenAI-compatible chat completions
// server that always answers with a canned reply, so agent-selection
// tests run real sessions cheaply and observe the banner.
func newFakeProviderServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// writeAgentConfig writes a config with the given agent names, an optional
// default_agent (empty means none), and each agent's provider pointing at
// baseURL with model-<name>. Returns the config path.
func writeAgentConfig(t *testing.T, dir string, names []string, defaultAgent string, baseURL string) string {
	t.Helper()

	agents := make([]map[string]any, len(names))
	for i, name := range names {
		agents[i] = map[string]any{
			"name":          name,
			"system_prompt": fmt.Sprintf("You are %s.", name),
			"provider": map[string]any{
				"type":     "openai",
				"model":    "model-" + name,
				"base_url": baseURL,
			},
			"max_turns": 1,
		}
	}
	base := map[string]any{
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
		"agents": [
			{
				"name": "helper",
				"system_prompt": "You are helpful.",
				"provider": {"type": "openai", "model": "m", "base_url": "http://localhost:1"},
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
