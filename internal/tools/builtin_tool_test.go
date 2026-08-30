package tools_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/logging"
	"github.com/overspecific/blorb/internal/tools"
)

// builtinEntry builds a builtin tool entry with the given settings.
func builtinEntry(name, description, b, cfg string) config.ToolEntry {
	return config.ToolEntry{
		Type:        config.ToolTypeBuiltin,
		Name:        name,
		Description: description,
		Builtin:     b,
		Config:      json.RawMessage(cfg),
	}
}

func TestBuiltinRegistryConstruction(t *testing.T) {
	t.Parallel()

	t.Run("valid entry", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()
		r, err := tools.NewRegistry([]config.ToolEntry{
			builtinEntry("read", "Read a file.", "read", `{"base_dir":"`+base+`"}`),
		})
		if err != nil {
			t.Fatalf("NewRegistry error = %v, want nil", err)
		}
		if got := r.Names(); len(got) != 1 || got[0] != "read" {
			t.Errorf("Names() = %v, want [read]", got)
		}
	})

	t.Run("unknown builtin name", func(t *testing.T) {
		t.Parallel()
		_, err := tools.NewRegistry([]config.ToolEntry{
			builtinEntry("read", "Read a file.", "nope", `{}`),
		})
		if err == nil || !strings.Contains(err.Error(), "unknown builtin") {
			t.Errorf("error = %v, want an unknown-builtin error", err)
		}
	})

	t.Run("missing builtin name", func(t *testing.T) {
		t.Parallel()
		e := builtinEntry("read", "Read a file.", "read", `{}`)
		e.Builtin = ""
		_, err := tools.NewRegistry([]config.ToolEntry{e})
		if err == nil || !strings.Contains(err.Error(), "builtin is required") {
			t.Errorf("error = %v, want a builtin-required error", err)
		}
	})

	t.Run("missing description", func(t *testing.T) {
		t.Parallel()
		e := builtinEntry("read", "Read a file.", "read", `{"base_dir":"."}`)
		e.Description = ""
		_, err := tools.NewRegistry([]config.ToolEntry{e})
		if err == nil || !strings.Contains(err.Error(), "description is required") {
			t.Errorf("error = %v, want a description error", err)
		}
	})

	t.Run("invalid settings", func(t *testing.T) {
		t.Parallel()
		_, err := tools.NewRegistry([]config.ToolEntry{
			builtinEntry("read", "Read a file.", "read", `{"base_dir":42}`),
		})
		if err == nil {
			t.Error("NewRegistry succeeded, want error")
		}
	})

	t.Run("unresolvable base_dir", func(t *testing.T) {
		t.Parallel()
		_, err := tools.NewRegistry([]config.ToolEntry{
			builtinEntry("read", "Read a file.", "read", `{"base_dir":"`+filepath.Join(t.TempDir(), "missing")+`"}`),
		})
		if err == nil {
			t.Error("NewRegistry succeeded, want error")
		}
	})

	t.Run("unknown config field rejected", func(t *testing.T) {
		t.Parallel()
		_, err := tools.NewRegistry([]config.ToolEntry{
			builtinEntry("read", "Read a file.", "read", `{"base_dir":".","extra":1}`),
		})
		if err == nil {
			t.Error("NewRegistry succeeded, want unknown-field error")
		}
	})
}

func TestBuiltinDefinitions(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	r, err := tools.NewRegistry([]config.ToolEntry{
		builtinEntry("my_read", "Reads things.", "read", `{"base_dir":"`+base+`"}`),
	})
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	defs := r.Definitions()
	if len(defs) != 1 {
		t.Fatalf("len(Definitions()) = %d, want 1", len(defs))
	}
	if defs[0].Name != "my_read" || defs[0].Description != "Reads things." {
		t.Errorf("defs[0] = %+v, want the entry's name and description", defs[0])
	}
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(defs[0].Parameters, &schema); err != nil {
		t.Fatalf("unmarshal Parameters: %v", err)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "path" {
		t.Errorf("Parameters = %s, want the builtin's args schema with required path", defs[0].Parameters)
	}
}

func TestBuiltinRunRead(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "f.txt"), []byte("hello builtin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := tools.NewRegistry([]config.ToolEntry{
		builtinEntry("read", "Read a file.", "read", `{"base_dir":"`+base+`"}`),
	})
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	res, err := r.Run(context.Background(), "read", json.RawMessage(`{"path":"f.txt"}`))
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if res.Err {
		t.Errorf("res.Err = true, want false; output %q", res.Output)
	}
	if res.Output != "hello builtin" {
		t.Errorf("res.Output = %q, want hello builtin (newline trimmed)", res.Output)
	}
}

func TestBuiltinRunGrep(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "f.txt"), []byte("one\ntwo special\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := tools.NewRegistry([]config.ToolEntry{
		builtinEntry("grep", "Search files.", "grep", `{"base_dir":"`+base+`"}`),
	})
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	res, err := r.Run(context.Background(), "grep", json.RawMessage(`{"pattern":"special"}`))
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if res.Err {
		t.Errorf("res.Err = true, want false; output %q", res.Output)
	}
	if res.Output != "f.txt:2:two special" {
		t.Errorf("res.Output = %q, want f.txt:2:two special", res.Output)
	}
}

func TestBuiltinRunSandboxRejectionIsToolResult(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	sibling := filepath.Join(filepath.Dir(base), "escape-target.txt")
	if err := os.WriteFile(sibling, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := tools.NewRegistry([]config.ToolEntry{
		builtinEntry("read", "Read a file.", "read", `{"base_dir":"`+base+`"}`),
	})
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	res, err := r.Run(context.Background(), "read", json.RawMessage(`{"path":"../escape-target.txt"}`))
	if err != nil {
		t.Fatalf("Run error = %v, want nil (sandbox rejection is a tool result)", err)
	}
	if !res.Err || res.Output != "error: path is outside the allowed directory" {
		t.Errorf("res = %+v, want the sandbox error as a tool result", res)
	}
}

func TestBuiltinRunLogsRequestAndResult(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "f.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sink := logging.NewBuffered(0)
	r, err := tools.NewRegistry([]config.ToolEntry{
		builtinEntry("read", "Read a file.", "read", `{"base_dir":"`+base+`"}`),
	}, tools.WithSink(sink))
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	args := json.RawMessage(`{"path":"f.txt"}`)
	res, err := r.Run(context.Background(), "read", args)
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	recs := sink.Records()
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2 (request + result)", len(recs))
	}
	if recs[0].Kind != logging.KindToolRequest || recs[1].Kind != logging.KindToolResult {
		t.Errorf("record kinds = %q, %q; want tool-request then tool-result", recs[0].Kind, recs[1].Kind)
	}
	if string(recs[0].Body) != string(args) {
		t.Errorf("request body = %q, want the args bytes", recs[0].Body)
	}
	if string(recs[1].Body) != res.Output {
		t.Errorf("result body = %q, want the ToolResult.Output %q", recs[1].Body, res.Output)
	}
}

func TestBuiltinRunLogsToolFailureResult(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	sink := logging.NewBuffered(0)
	r, err := tools.NewRegistry([]config.ToolEntry{
		builtinEntry("read", "Read a file.", "read", `{"base_dir":"`+base+`"}`),
	}, tools.WithSink(sink))
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	res, err := r.Run(context.Background(), "read", json.RawMessage(`{"path":"missing.txt"}`))
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if !res.Err {
		t.Errorf("res.Err = false, want true")
	}

	recs := sink.Records()
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	if string(recs[1].Body) != res.Output {
		t.Errorf("result body = %q, want the returned ToolResult.Output %q", recs[1].Body, res.Output)
	}
}

func TestBuiltinInvalidArgsReturnError(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	r, err := tools.NewRegistry([]config.ToolEntry{
		builtinEntry("read", "Read a file.", "read", `{"base_dir":"`+base+`"}`),
	})
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}

	if _, err := r.Run(context.Background(), "read", json.RawMessage(`not json`)); err == nil {
		t.Error("Run succeeded, want invalid-args error")
	}
	if _, err := r.Run(context.Background(), "nope", nil); err == nil {
		t.Error("Run with unknown tool succeeded, want error")
	}
}

func TestBuiltinTimeoutEnforcement(t *testing.T) {
	t.Parallel()

	// The registry derives the per-call context.WithTimeout; the grep
	// walk checks ctx.Err() per entry, so an already-canceled call
	// context (or an expired per-call deadline) aborts the walk and the
	// tool reports a context failure instead of running to completion.
	base := t.TempDir()
	for i := 0; i < 4; i++ {
		name := filepath.Join(base, fmt.Sprintf("big%d.txt", i))
		if err := os.WriteFile(name, bytes.Repeat([]byte("match me\n"), 2000), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r, err := tools.NewRegistry([]config.ToolEntry{
		builtinEntry("grep", "Search files.", "grep", `{"base_dir":"`+base+`"}`),
	}, tools.WithTimeout(time.Hour))
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}
	defer r.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := r.Run(ctx, "grep", json.RawMessage(`{"pattern":"match"}`))
	if err != nil {
		t.Fatalf("Run error = %v, want nil (a canceled walk is a tool result)", err)
	}
	if !res.Err || !strings.Contains(res.Output, "context canceled") {
		t.Errorf("res = %+v, want a context-canceled tool failure", res)
	}
}

func TestBuiltinBaseDirRelativeToConfigDir(t *testing.T) {
	t.Parallel()

	// WithConfigDir anchors relative base_dir values at the config file's
	// directory; registry construction verifies the directory exists, so a
	// resolution failure surfaces as a construction error.
	baseDir := t.TempDir()
	kb := filepath.Join(baseDir, "kb")
	if err := os.MkdirAll(kb, 0o755); err != nil {
		t.Fatal(err)
	}
	entry := builtinEntry("read", "Read a file.", "read", `{"base_dir":"kb"}`)

	if _, err := tools.NewRegistry([]config.ToolEntry{entry}, tools.WithConfigDir(baseDir)); err != nil {
		t.Errorf("NewRegistry error = %v, want nil (kb exists inside the config dir)", err)
	}
	if _, err := tools.NewRegistry([]config.ToolEntry{entry}, tools.WithConfigDir(filepath.Join(baseDir, "elsewhere"))); err == nil {
		t.Error("NewRegistry succeeded, want error (kb missing in the given config dir)")
	}
}

// TestBuiltinBaseDirSwapThroughRegistry pins the lifetime-root exploit at
// the registry boundary: if the sandbox were re-opened per run, renaming
// the base away and symlinking it to an outside directory after
// construction would let a read reach outside files. The root is held from
// construction, so the swap must surface as a sandbox failure.
func TestBuiltinBaseDirSwapThroughRegistry(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	sibling := t.TempDir()
	outside := filepath.Join(sibling, "secret.txt")
	if err := os.WriteFile(outside, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "inside.txt"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := tools.NewRegistry([]config.ToolEntry{
		builtinEntry("read", "Read a file.", "read", `{"base_dir":"`+base+`"}`),
	})
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}
	defer r.Close()

	if err := os.RemoveAll(base); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sibling, base); err != nil {
		t.Fatal(err)
	}

	res, err := r.Run(context.Background(), "read", json.RawMessage(`{"path":"secret.txt"}`))
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if !res.Err || strings.Contains(res.Output, "top secret") {
		t.Errorf("res = %+v, want the sandbox error as a tool result, never outside content", res)
	}
}

func TestBuiltinMixedRegistry(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "f.txt"), []byte("mixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := tools.NewRegistry([]config.ToolEntry{
		{Type: config.ToolTypeCommand, Name: "cmd", Description: "A command.", Command: []string{"echo", "hi"}},
		builtinEntry("read", "Read a file.", "read", `{"base_dir":"`+base+`"}`),
	})
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}
	if got := r.Names(); len(got) != 2 || got[0] != "cmd" || got[1] != "read" {
		t.Errorf("Names() = %v, want [cmd read]", got)
	}
}
