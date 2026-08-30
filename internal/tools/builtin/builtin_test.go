package builtin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/overspecific/blorb/internal/tools/builtin"
)

// opts parses config settings for the named builtin, failing the test on
// error.
func opts(t *testing.T, name, raw string) builtin.Options {
	t.Helper()
	o, err := builtin.ParseConfig(name, json.RawMessage(raw), builtin.ParseOptions{})
	if err != nil {
		t.Fatalf("ParseConfig(%s) error = %v, want nil", name, err)
	}
	return o
}

func TestSupported(t *testing.T) {
	t.Parallel()

	got := builtin.Supported()
	if len(got) != 2 || got[0] != "grep" || got[1] != "read" {
		t.Errorf("Supported() = %v, want [grep read]", got)
	}
	for _, name := range got {
		if _, ok := builtin.Lookup(name); !ok {
			t.Errorf("Lookup(%q) not found", name)
		}
	}
	if _, ok := builtin.Lookup("nope"); ok {
		t.Error("Lookup(nope) succeeded, want false")
	}
}

func TestLookup(t *testing.T) {
	t.Parallel()

	rd, ok := builtin.Lookup("read")
	if !ok {
		t.Fatal("Lookup(read) not found")
	}
	if rd.Name != "read" {
		t.Errorf("Name = %q, want read", rd.Name)
	}
	if len(rd.ArgsSchema) == 0 || !json.Valid(rd.ArgsSchema) {
		t.Errorf("read ArgsSchema = %s, want valid JSON", rd.ArgsSchema)
	}
	var readSchema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(rd.ArgsSchema, &readSchema); err != nil {
		t.Fatalf("unmarshal read schema: %v", err)
	}
	if _, ok := readSchema.Properties["path"]; !ok {
		t.Errorf("read args schema missing path property: %s", rd.ArgsSchema)
	}
	if len(readSchema.Required) != 1 || readSchema.Required[0] != "path" {
		t.Errorf("read args schema required = %v, want [path]", readSchema.Required)
	}

	g, ok := builtin.Lookup("grep")
	if !ok {
		t.Fatal("Lookup(grep) not found")
	}
	if len(g.ArgsSchema) == 0 || !json.Valid(g.ArgsSchema) {
		t.Errorf("grep ArgsSchema = %s, want valid JSON", g.ArgsSchema)
	}
	for _, prop := range []string{"pattern", "path"} {
		if !strings.Contains(string(g.ArgsSchema), `"`+prop+`"`) {
			t.Errorf("grep args schema missing %q property", prop)
		}
	}
}

func TestParseConfig(t *testing.T) {
	t.Parallel()

	t.Run("read valid", func(t *testing.T) {
		t.Parallel()
		if _, err := builtin.ParseConfig("read", json.RawMessage(`{"base_dir":"."}`), builtin.ParseOptions{}); err != nil {
			t.Errorf("ParseConfig error = %v, want nil", err)
		}
	})

	t.Run("grep valid", func(t *testing.T) {
		t.Parallel()
		if _, err := builtin.ParseConfig("grep", json.RawMessage(`{"base_dir":"."}`), builtin.ParseOptions{}); err != nil {
			t.Errorf("ParseConfig error = %v, want nil", err)
		}
	})

	t.Run("missing base_dir", func(t *testing.T) {
		t.Parallel()
		_, err := builtin.ParseConfig("read", nil, builtin.ParseOptions{})
		if err == nil || !strings.Contains(err.Error(), "base_dir is required") {
			t.Errorf("error = %v, want a base_dir required error", err)
		}
	})

	t.Run("empty config object", func(t *testing.T) {
		t.Parallel()
		_, err := builtin.ParseConfig("read", json.RawMessage(`{}`), builtin.ParseOptions{})
		if err == nil || !strings.Contains(err.Error(), "base_dir is required") {
			t.Errorf("error = %v, want a base_dir is required error", err)
		}
	})

	t.Run("base_dir wrong type", func(t *testing.T) {
		t.Parallel()
		_, err := builtin.ParseConfig("read", json.RawMessage(`{"base_dir":42}`), builtin.ParseOptions{})
		if err == nil {
			t.Error("ParseConfig succeeded, want error")
		}
	})

	t.Run("unknown field rejected", func(t *testing.T) {
		t.Parallel()
		_, err := builtin.ParseConfig("read", json.RawMessage(`{"base_dir":".","extra":1}`), builtin.ParseOptions{})
		if err == nil {
			t.Error("ParseConfig succeeded, want unknown-field error")
		}
	})

	t.Run("unknown builtin name", func(t *testing.T) {
		t.Parallel()
		_, err := builtin.ParseConfig("nope", json.RawMessage(`{}`), builtin.ParseOptions{})
		if err == nil || !strings.Contains(err.Error(), "unknown builtin") {
			t.Errorf("error = %v, want an unknown-builtin error", err)
		}
	})
}

func TestParseConfigBaseDirMustBeDirectory(t *testing.T) {
	t.Parallel()

	t.Run("not a directory", func(t *testing.T) {
		t.Parallel()
		file := filepath.Join(t.TempDir(), "file.txt")
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := builtin.ParseConfig("read", json.RawMessage(`{"base_dir":"`+file+`"}`), builtin.ParseOptions{})
		if err == nil || !strings.Contains(err.Error(), "is not a directory") {
			t.Errorf("error = %v, want a not-a-directory error", err)
		}
	})

	t.Run("does not exist", func(t *testing.T) {
		t.Parallel()
		_, err := builtin.ParseConfig("grep", json.RawMessage(`{"base_dir":"`+filepath.Join(t.TempDir(), "missing")+`"}`), builtin.ParseOptions{})
		if err == nil {
			t.Error("ParseConfig succeeded, want error")
		}
	})
}

// grebFixture builds a base directory containing:
//
//	alpha.txt        two lines, one with "needle"
//	nested/deep.txt  "needle here"
//	nested/inside.md no match
//	bin.dat          contains NUL bytes (grep skips)
//	.git/config      "needle here" (grep skips)
func grebFixture(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	files := map[string]string{
		"alpha.txt":        "first line\nsecond needle line\n",
		"nested/deep.txt":  "needle here\n",
		"nested/inside.md": "no match\n",
		"bin.dat":          "ok\x00binary\nneedle here\n",
		".git/config":      "needle here\n",
	}
	for p, content := range files {
		full := filepath.Join(base, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return base
}

func TestRead(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "f.txt"), []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := opts(t, "read", `{"base_dir":"`+base+`"}`)

	t.Run("happy path", func(t *testing.T) {
		t.Parallel()
		b, ok := builtin.Lookup("read")
		if !ok {
			t.Fatal("Lookup(read) not found")
		}
		res, err := b.Run(context.Background(), o, json.RawMessage(`{"path":"f.txt"}`))
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if res.Err {
			t.Errorf("res.Err = true, want false; output %q", res.Output)
		}
		if res.Output != "line1\nline2" {
			t.Errorf("res.Output = %q, want trailing newline trimmed", res.Output)
		}
	})

	t.Run("nested path", func(t *testing.T) {
		t.Parallel()
		nested := t.TempDir()
		if err := os.MkdirAll(filepath.Join(nested, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(nested, "sub", "x.txt"), []byte("content\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		no := opts(t, "read", `{"base_dir":"`+nested+`"}`)
		b, _ := builtin.Lookup("read")
		res, err := b.Run(context.Background(), no, json.RawMessage(`{"path":"sub/x.txt"}`))
		if err != nil || res.Err {
			t.Fatalf("Run = %+v, %v; want clean read", res, err)
		}
		if res.Output != "content" {
			t.Errorf("res.Output = %q, want content", res.Output)
		}
	})

	t.Run("missing path", func(t *testing.T) {
		t.Parallel()
		b, _ := builtin.Lookup("read")
		res, err := b.Run(context.Background(), o, nil)
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if !res.Err || res.Output != "error: path is required and must be a string" {
			t.Errorf("res = %+v, want path-required failure", res)
		}
	})

	t.Run("path wrong type", func(t *testing.T) {
		t.Parallel()
		b, _ := builtin.Lookup("read")
		res, err := b.Run(context.Background(), o, json.RawMessage(`{"path":42}`))
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if !res.Err || res.Output != "error: path is required and must be a string" {
			t.Errorf("res = %+v, want path-required failure", res)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		b, _ := builtin.Lookup("read")
		res, err := b.Run(context.Background(), o, json.RawMessage(`{"path":"gone.txt"}`))
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if !res.Err {
			t.Errorf("res.Err = false, want true; output %q", res.Output)
		}
		if !strings.Contains(res.Output, "no such file") {
			t.Errorf("res.Output = %q, want a not-found mention", res.Output)
		}
	})

	t.Run("directory", func(t *testing.T) {
		t.Parallel()
		b, _ := builtin.Lookup("read")
		res, err := b.Run(context.Background(), o, json.RawMessage(`{"path":"."}`))
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if !res.Err || !strings.Contains(res.Output, "is a directory") {
			t.Errorf("res = %+v, want an is-a-directory failure", res)
		}
	})

	t.Run("oversized file", func(t *testing.T) {
		t.Parallel()
		big := t.TempDir()
		if err := os.WriteFile(filepath.Join(big, "big.txt"), make([]byte, (1<<20)+10), 0o644); err != nil {
			t.Fatal(err)
		}
		no := opts(t, "read", `{"base_dir":"`+big+`"}`)
		b, _ := builtin.Lookup("read")
		res, err := b.Run(context.Background(), no, json.RawMessage(`{"path":"big.txt"}`))
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if !res.Err {
			t.Error("res.Err = false, want true for an oversized file")
		}
		if !strings.Contains(res.Output, "exceeding") || !strings.Contains(res.Output, "1048576") {
			t.Errorf("res.Output = %q, want cap and size mention", res.Output)
		}
	})
}

func TestGrep(t *testing.T) {
	t.Parallel()

	base := grebFixture(t)
	o := opts(t, "grep", `{"base_dir":"`+base+`"}`)
	g, _ := builtin.Lookup("grep")

	t.Run("walk finds matches with relative paths", func(t *testing.T) {
		t.Parallel()
		res, err := g.Run(context.Background(), o, json.RawMessage(`{"pattern":"needle"}`))
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if res.Err {
			t.Errorf("res.Err = true, want false; output %q", res.Output)
		}
		want := "alpha.txt:2:second needle line\nnested/deep.txt:1:needle here"
		if res.Output != want {
			t.Errorf("res.Output = %q, want %q", res.Output, want)
		}
	})

	t.Run("single file path", func(t *testing.T) {
		t.Parallel()
		res, err := g.Run(context.Background(), o, json.RawMessage(`{"pattern":"needle","path":"alpha.txt"}`))
		if err != nil || res.Err {
			t.Fatalf("Run = %+v, %v; want clean result", res, err)
		}
		if res.Output != "alpha.txt:2:second needle line" {
			t.Errorf("res.Output = %q, want single-file match", res.Output)
		}
	})

	t.Run("no matches is empty success", func(t *testing.T) {
		t.Parallel()
		res, err := g.Run(context.Background(), o, json.RawMessage(`{"pattern":"zzz-nowhere"}`))
		if err != nil || res.Err {
			t.Fatalf("Run = %+v, %v; want clean result", res, err)
		}
		if res.Output != "" {
			t.Errorf("res.Output = %q, want empty", res.Output)
		}
	})

	t.Run("invalid regex", func(t *testing.T) {
		t.Parallel()
		res, err := g.Run(context.Background(), o, json.RawMessage(`{"pattern":"[unclosed"}`))
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if !res.Err || !strings.Contains(res.Output, "missing closing ]") {
			t.Errorf("res = %+v, want compile-error failure", res)
		}
	})

	t.Run("missing pattern", func(t *testing.T) {
		t.Parallel()
		res, err := g.Run(context.Background(), o, json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if !res.Err || res.Output != "error: pattern is required and must be a string" {
			t.Errorf("res = %+v, want pattern-required failure", res)
		}
	})

	t.Run("skips .git and binary files", func(t *testing.T) {
		t.Parallel()
		res, err := g.Run(context.Background(), o, json.RawMessage(`{"pattern":"needle","path":"."}`))
		if err != nil || res.Err {
			t.Fatalf("Run = %+v, %v; want clean result", res, err)
		}
		if strings.Contains(res.Output, ".git") {
			t.Errorf("res.Output contains .git: %q", res.Output)
		}
		if strings.Contains(res.Output, "bin.dat") {
			t.Errorf("res.Output contains binary file matches: %q", res.Output)
		}
	})

	t.Run("output truncation ends with notice and stays success", func(t *testing.T) {
		t.Parallel()
		big := t.TempDir()
		// Many matching lines across files so the combined output passes
		// the 1 MiB cap.
		for i := 0; i < 12; i++ {
			name := filepath.Join(big, fmt.Sprintf("big%02d.txt", i))
			if err := os.WriteFile(name, bytes.Repeat([]byte("match me\n"), 15000), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		no := opts(t, "grep", `{"base_dir":"`+big+`"}`)
		g, _ := builtin.Lookup("grep")
		res, err := g.Run(context.Background(), no, json.RawMessage(`{"pattern":"match"}`))
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if res.Err {
			t.Errorf("res.Err = true, want false (cap is volume, not failure); output len %d", len(res.Output))
		}
		if !strings.HasSuffix(res.Output, "output truncated at 1 MiB") {
			t.Errorf("res.Output = %q..., want truncation notice at end", tail(res.Output))
		}
		if len(res.Output) > (1<<20)+64 {
			t.Errorf("res.Output length = %d, want at most ~1 MiB plus notice", len(res.Output))
		}
	})
}

func TestSandbox(t *testing.T) {
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

	o := opts(t, "read", `{"base_dir":"`+base+`"}`)
	rd, _ := builtin.Lookup("read")

	// sandboxErr runs a read with the given path argument and requires it
	// to fail with exactly the uniform sandbox error.
	sandboxErr := func(t *testing.T, path string) string {
		t.Helper()
		res, err := rd.Run(context.Background(), o, json.RawMessage(`{"path":`+quote(path)+`}`))
		if err != nil {
			t.Fatalf("Run(%s) error = %v, want a tool result", path, err)
		}
		if !res.Err {
			t.Fatalf("Run(%s) res.Err = false, want true; output %q", path, res.Output)
		}
		return res.Output
	}

	var dotdotOutput string

	t.Run("dotdot traversal", func(t *testing.T) {
		t.Parallel()
		dotdotOutput = sandboxErr(t, "../secret.txt")
		if strings.Contains(dotdotOutput, "secret") {
			t.Errorf("res.Output leaked content: %q", dotdotOutput)
		}
	})

	t.Run("absolute path outside", func(t *testing.T) {
		t.Parallel()
		got := sandboxErr(t, outside)
		if got != "error: path is outside the allowed directory" {
			t.Errorf("res.Output = %q, want the uniform sandbox error", got)
		}
	})

	t.Run("absolute path inside base also rejected", func(t *testing.T) {
		t.Parallel()
		got := sandboxErr(t, filepath.Join(base, "inside.txt"))
		if got != "error: path is outside the allowed directory" {
			t.Errorf("res.Output = %q, want the uniform sandbox error", got)
		}
	})

	t.Run("symlink to inside file succeeds", func(t *testing.T) {
		t.Parallel()
		// Root only follows symlinks with relative targets; an absolute
		// target would be rejected as an escape even pointing inside.
		if err := os.Symlink("inside.txt", filepath.Join(base, "link_in.txt")); err != nil {
			t.Fatal(err)
		}
		res, err := rd.Run(context.Background(), o, json.RawMessage(`{"path":"link_in.txt"}`))
		if err != nil || res.Err {
			t.Fatalf("Run = %+v, %v; want clean symlink read", res, err)
		}
		if res.Output != "inside" {
			t.Errorf("res.Output = %q, want inside", res.Output)
		}
	})

	t.Run("symlink to outside matches dotdot error exactly", func(t *testing.T) {
		t.Parallel()
		if err := os.Symlink(outside, filepath.Join(base, "link_out.txt")); err != nil {
			t.Fatal(err)
		}
		res, err := rd.Run(context.Background(), o, json.RawMessage(`{"path":"link_out.txt"}`))
		if err != nil {
			t.Fatalf("Run error = %v, want a tool result", err)
		}
		if !res.Err {
			t.Fatalf("res.Err = false, want true; output %q", res.Output)
		}
		want := sandboxErr(t, "../secret.txt")
		if res.Output != want {
			t.Errorf("symlink error = %q, want exact same message as dotdot case %q", res.Output, want)
		}
	})

	t.Run("dangling symlink is a normal not-exist error", func(t *testing.T) {
		t.Parallel()
		// A relative dangling symlink surfaces as fs.ErrNotExist (the
		// error-mapping policy passes it through); an absolute dangling
		// target would be rejected as an escape instead.
		if err := os.Symlink("gone.txt", filepath.Join(base, "dangle.txt")); err != nil {
			t.Fatal(err)
		}
		res, err := rd.Run(context.Background(), o, json.RawMessage(`{"path":"dangle.txt"}`))
		if err != nil {
			t.Fatalf("Run error = %v, want a tool result", err)
		}
		if !res.Err || !strings.Contains(res.Output, "no such file") {
			t.Errorf("res = %+v, want a normal not-exist failure", res)
		}
	})

	t.Run("grep walk skips dir symlinks and outside file links", func(t *testing.T) {
		t.Parallel()
		tree := t.TempDir()
		if err := os.WriteFile(filepath.Join(tree, "a.txt"), []byte("findme\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Sibling of the base, used as the outside world.
		secret := filepath.Join(filepath.Dir(tree), "grep-secret.txt")
		if err := os.WriteFile(secret, []byte("findme secret\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Directory symlink pointing at the sibling temp dir.
		if err := os.Symlink(filepath.Dir(tree), filepath.Join(tree, "linked")); err != nil {
			t.Fatal(err)
		}
		// File symlink pointing to a file outside the tree (absolute
		// target, so Root rejects it) - grep must skip this silently.
		if err := os.Symlink(secret, filepath.Join(tree, "outsidelink.txt")); err != nil {
			t.Fatal(err)
		}
		// Inside-to-inside file symlink - followed transparently.
		if err := os.Symlink("a.txt", filepath.Join(tree, "inlink.txt")); err != nil {
			t.Fatal(err)
		}

		gs := opts(t, "grep", `{"base_dir":"`+tree+`"}`)
		g, _ := builtin.Lookup("grep")
		res, err := g.Run(context.Background(), gs, json.RawMessage(`{"pattern":"findme"}`))
		if err != nil {
			t.Fatalf("Run error = %v, want a tool result", err)
		}
		if res.Err {
			t.Fatalf("res.Err = true; output %q", res.Output)
		}
		if strings.Contains(res.Output, "secret") {
			t.Errorf("res.Output leaked outside file: %q", res.Output)
		}
		if !strings.Contains(res.Output, "a.txt:1:findme") {
			t.Errorf("res.Output = %q, want a.txt match", res.Output)
		}
		if !strings.Contains(res.Output, "inlink") {
			t.Errorf("res.Output = %q, want inside file symlink followed", res.Output)
		}
	})

	t.Run("grep path argument that escapes fails whole grep", func(t *testing.T) {
		t.Parallel()
		g2, _ := builtin.Lookup("grep")
		res, err := g2.Run(context.Background(), o, json.RawMessage(`{"pattern":"x","path":"../secret.txt"}`))
		if err != nil {
			t.Fatalf("Run error = %v, want a tool result", err)
		}
		if !res.Err || res.Output != "error: path is outside the allowed directory" {
			t.Errorf("res = %+v, want the uniform sandbox error", res)
		}
	})
}

func quote(s string) string {
	data, _ := json.Marshal(s)
	return string(data)
}

// tail returns the last 64 characters of s for readable failure output.
func tail(s string) string {
	if len(s) > 64 {
		return "..." + s[len(s)-64:]
	}
	return s
}
