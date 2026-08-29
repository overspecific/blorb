package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/tools"
)

func requireExecTools(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("exec-based tool tests require a POSIX shell")
	}
}

func entry(name, description string, command ...string) config.ToolEntry {
	return config.ToolEntry{Name: name, Description: description, Command: command}
}

func TestNewRegistry(t *testing.T) {
	t.Parallel()

	t.Run("converts entries", func(t *testing.T) {
		t.Parallel()

		r, err := tools.NewRegistry([]config.ToolEntry{
			entry("alpha", "First tool.", "echo", "hi"),
			entry("beta", "Second tool.", "cat"),
		})
		if err != nil {
			t.Fatalf("NewRegistry error = %v, want nil", err)
		}
		if got := r.Names(); len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
			t.Errorf("Names() = %v, want [alpha beta]", got)
		}
	})

	t.Run("rejects empty name", func(t *testing.T) {
		t.Parallel()
		if _, err := tools.NewRegistry([]config.ToolEntry{entry("", "No name.", "echo")}); err == nil {
			t.Error("NewRegistry succeeded, want error")
		}
	})

	t.Run("rejects invalid name", func(t *testing.T) {
		t.Parallel()
		if _, err := tools.NewRegistry([]config.ToolEntry{entry("has space", "", "echo")}); err == nil {
			t.Error("NewRegistry succeeded, want error")
		}
	})

	t.Run("rejects duplicate names", func(t *testing.T) {
		t.Parallel()
		_, err := tools.NewRegistry([]config.ToolEntry{
			entry("dupe", "One.", "echo"),
			entry("dupe", "Two.", "echo"),
		})
		if err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Errorf("error = %v, want duplicate name error", err)
		}
	})

	t.Run("rejects empty command", func(t *testing.T) {
		t.Parallel()
		if _, err := tools.NewRegistry([]config.ToolEntry{entry("t", "No command.")}); err == nil {
			t.Error("NewRegistry succeeded, want error")
		}
	})

	t.Run("rejects empty command element", func(t *testing.T) {
		t.Parallel()
		_, err := tools.NewRegistry([]config.ToolEntry{entry("t", "Empty element.", "echo", "")})
		if err == nil || !strings.Contains(err.Error(), "empty strings") {
			t.Errorf("error = %v, want empty command element error", err)
		}
	})

	t.Run("rejects empty description", func(t *testing.T) {
		t.Parallel()
		if _, err := tools.NewRegistry([]config.ToolEntry{entry("t", "", "echo")}); err == nil {
			t.Error("NewRegistry succeeded, want error")
		}
	})
}

func TestDefinitions(t *testing.T) {
	t.Parallel()

	r, err := tools.NewRegistry([]config.ToolEntry{
		{Name: "a_tool", Description: "With schema.", Command: []string{"echo"}, ArgsSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"number"}}}`)},
		entry("b_tool", "Without schema.", "echo"),
	})
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	defs := r.Definitions()
	if len(defs) != 2 {
		t.Fatalf("len(Definitions()) = %d, want 2", len(defs))
	}
	if defs[0].Name != "a_tool" || defs[0].Description != "With schema." {
		t.Errorf("defs[0] = %+v, want a_tool", defs[0])
	}
	if string(defs[0].Parameters) != `{"type":"object","properties":{"x":{"type":"number"}}}` {
		t.Errorf("defs[0].Parameters = %s, want the configured schema", defs[0].Parameters)
	}
	if string(defs[1].Parameters) != "{}" {
		t.Errorf("defs[1].Parameters = %s, want {} for empty ArgsSchema", defs[1].Parameters)
	}

	// Definitions must be usable as llm.Tool wire types.
	data, err := json.Marshal(defs[1])
	if err != nil {
		t.Fatalf("Marshal error = %v, want nil", err)
	}
	var back llm.Tool
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal error = %v, want nil", err)
	}
	if back.Name != "b_tool" {
		t.Errorf("round-tripped Name = %q, want %q", back.Name, "b_tool")
	}
}

func TestRunSuccess(t *testing.T) {
	requireExecTools(t)
	t.Parallel()

	r, err := tools.NewRegistry([]config.ToolEntry{
		entry("echo_args", "Echo stdin back.", "sh", "-c", `printf '%s' "$(cat)" && echo`),
	})
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	res, err := r.Run(context.Background(), "echo_args", json.RawMessage(`{"path":"."}`))
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if res.Err {
		t.Errorf("res.Err = true, want false")
	}
	if res.Output != `{"path":"."}` {
		t.Errorf("res.Output = %q, want %q", res.Output, `{"path":"."}`)
	}
}

func TestRunTrimsSingleTrailingNewline(t *testing.T) {
	requireExecTools(t)
	t.Parallel()

	r, err := tools.NewRegistry([]config.ToolEntry{
		entry("hello", "Prints hello.", "echo", "hello"),
	})
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	res, err := r.Run(context.Background(), "hello", nil)
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if res.Output != "hello" {
		t.Errorf("res.Output = %q, want %q", res.Output, "hello")
	}
}

func TestRunNonZeroExit(t *testing.T) {
	requireExecTools(t)
	t.Parallel()

	r, err := tools.NewRegistry([]config.ToolEntry{
		entry("failer", "Always fails.", "sh", "-c", "echo something went wrong >&2; exit 3"),
	})
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	res, err := r.Run(context.Background(), "failer", nil)
	if err != nil {
		t.Fatalf("Run error = %v, want nil (tool failure is a result, not an error)", err)
	}
	if !res.Err {
		t.Error("res.Err = false, want true")
	}
	if !strings.Contains(res.Output, "exit code 3") {
		t.Errorf("res.Output = %q, want exit code mention", res.Output)
	}
	if !strings.Contains(res.Output, "something went wrong") {
		t.Errorf("res.Output = %q, want captured stderr", res.Output)
	}
}

func TestRunTimeout(t *testing.T) {
	requireExecTools(t)
	t.Parallel()

	r, err := tools.NewRegistry([]config.ToolEntry{
		entry("sleeper", "Sleeps forever.", "sleep", "30"),
	}, tools.WithTimeout(300*time.Millisecond))
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	start := time.Now()
	_, err = r.Run(context.Background(), "sleeper", nil)
	if err == nil {
		t.Fatal("Run succeeded, want timeout error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Run took %s, want sub-second kill", elapsed)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want timeout mention", err)
	}
}

func TestRunTimeoutKillsChild(t *testing.T) {
	requireExecTools(t)
	t.Parallel()

	r, err := tools.NewRegistry([]config.ToolEntry{
		// The tool spawns a child that outlives it unless the whole group
		// is killed.
		entry("spawner", "Spawns a child sleeper.", "sh", "-c", `sleep 30 & echo "$!" > /tmp/blorb_test_child.pid; wait`),
	}, tools.WithTimeout(300*time.Millisecond))
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	if _, err := r.Run(context.Background(), "spawner", nil); err == nil {
		t.Fatal("Run succeeded, want timeout error")
	}

	// The spawned child must be gone (killed alongside the tool process).
	check := exec.Command("sh", "-c", `while kill -0 "$(cat /tmp/blorb_test_child.pid 2>/dev/null)" 2>/dev/null; do sleep 0.05; done; echo gone`)
	out, err := check.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "gone" {
		t.Errorf("child process still alive after timeout: out=%q err=%v", out, err)
	}
	_ = os.Remove("/tmp/blorb_test_child.pid")
}

func TestRunUnknownTool(t *testing.T) {
	t.Parallel()

	r, err := tools.NewRegistry([]config.ToolEntry{entry("known", "The only tool.", "echo")})
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	_, err = r.Run(context.Background(), "mystery", nil)
	if err == nil {
		t.Fatal("Run succeeded, want error")
	}
	if !strings.Contains(err.Error(), "unknown tool") || !strings.Contains(err.Error(), "known") {
		t.Errorf("error = %q, want unknown-tool error listing known tools", err)
	}
}

func TestRunInvalidArgs(t *testing.T) {
	t.Parallel()

	r, err := tools.NewRegistry([]config.ToolEntry{entry("t", "Any tool.", "echo")})
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	_, err = r.Run(context.Background(), "t", json.RawMessage(`not json`))
	if err == nil {
		t.Fatal("Run succeeded, want error")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("error = %q, want invalid-JSON mention", err)
	}
}

func TestRunWithContextCancellation(t *testing.T) {
	requireExecTools(t)
	t.Parallel()

	r, err := tools.NewRegistry([]config.ToolEntry{
		entry("sleeper", "Sleeps forever.", "sleep", "30"),
	})
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err = r.Run(ctx, "sleeper", nil)
	if err == nil {
		t.Fatal("Run succeeded, want error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "sleeper") {
		t.Errorf("error = %q, want it to name the tool", err)
	}
}

func TestRunOutputCapTruncates(t *testing.T) {
	requireExecTools(t)
	t.Parallel()

	r, err := tools.NewRegistry([]config.ToolEntry{
		// Write well beyond the 1 MiB output cap.
		entry("spewer", "Writes lots of output.", "sh", "-c", `head -c $((2 * 1024 * 1024)) /dev/zero | tr '\0' 'x'`),
	})
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	res, err := r.Run(context.Background(), "spewer", nil)
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if res.Err {
		t.Error("res.Err = true, want false")
	}
	if !strings.Contains(res.Output, "output truncated") {
		t.Errorf("res.Output marker missing truncation notice (len=%d)", len(res.Output))
	}
	if len(res.Output) > (1<<20)+64 {
		t.Errorf("res.Output length = %d, want at most ~1 MiB plus notice", len(res.Output))
	}
}
