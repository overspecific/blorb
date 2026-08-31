package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
	if len(cmd.Flags) != 2 {
		t.Errorf("chat has %d flags, want 2 (config, no-stream): %+v", len(cmd.Flags), cmd.Flags)
	}
	for _, f := range cmd.Flags {
		if f.Names()[0] == "logging" {
			t.Error("chat has a logging flag; logging must be configured via blorb.json only")
		}
	}
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
