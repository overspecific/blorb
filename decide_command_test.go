package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDeciderConfig writes a config with one decision model, one decider
// ("triage"), and one agent ("helper"), all pointing at baseURL. It returns
// the config path.
func writeDeciderConfig(t *testing.T, dir, baseURL string) string {
	t.Helper()

	base := map[string]any{
		"providers": []map[string]any{{
			"name":     "local",
			"type":     "openai-compatible",
			"base_url": baseURL,
		}},
		"models": []map[string]any{
			{"name": "m", "provider": "local", "model_name": "m"},
			{"name": "jev", "provider": "local", "model_name": "jev-latest", "model_type": "decision"},
		},
		"agents": []map[string]any{{
			"name": "helper", "system_prompt": "You are helpful.", "model": "m", "max_turns": 1,
		}},
		"deciders": []map[string]any{{
			"name":  "triage",
			"model": "jev",
			"questions": map[string]any{
				"priority": map[string]any{
					"type":         "choice",
					"instructions": "How urgent?",
					"criteria":     map[string]any{"low": "Low", "high": "High"},
				},
			},
		}},
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

// newFakeDecisionServer returns a System One-compatible server answering
// with a fixed choice answer.
func newFakeDecisionServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/systemone") {
			t.Errorf("request path = %q, want /systemone", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "jev-1.13.0",
			"answers": {"priority": {"type":"choice","choice":"high"}},
			"usage": {"input_tokens": 10, "output_tokens": 2}
		}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runDecideCommand runs `blorb decide -c cfgPath [args...]` over
// pipe-backed stdio and returns stdout, stderr, and the session error.
func runDecideCommand(t *testing.T, cfgPath, stdin string, args ...string) (string, string, error) {
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

	argv := append([]string{"blorb", "decide", "-c", cfgPath}, args...)
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
		return out.String(), errBuf.String(), fmt.Errorf("%s", errOut.String())
	}
	return out.String(), errBuf.String(), runErr
}

// TestDecideCommandHappyPath resolves the decider and prints its answers
// JSON.
func TestDecideCommandHappyPath(t *testing.T) {
	srv := newFakeDecisionServer(t)
	cfgPath := writeDeciderConfig(t, t.TempDir(), srv.URL)

	out, _, err := runDecideCommand(t, cfgPath, "", "--decider", "triage", "the ticket")
	if err != nil {
		t.Fatalf("decide error = %v, want nil", err)
	}
	if want := `{"priority":{"type":"choice","choice":"high"}}` + "\n"; out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
}

// TestDecideCommandUnknownDecider exits 1 with the available names.
func TestDecideCommandUnknownDecider(t *testing.T) {
	srv := newFakeDecisionServer(t)
	cfgPath := writeDeciderConfig(t, t.TempDir(), srv.URL)

	_, _, err := runDecideCommand(t, cfgPath, "", "--decider", "ghost", "the ticket")
	if err == nil {
		t.Fatal("decide with an unknown decider succeeded, want an error")
	}
	if !strings.Contains(err.Error(), `decider "ghost" is not defined`) || !strings.Contains(err.Error(), "triage") {
		t.Errorf("error = %v, want the unknown-decider error naming the available deciders", err)
	}
}

// TestDecideCommandMissingStateIsUsageError pins the "decide: " prefix on
// the no-state usage error.
func TestDecideCommandMissingStateIsUsageError(t *testing.T) {
	srv := newFakeDecisionServer(t)
	cfgPath := writeDeciderConfig(t, t.TempDir(), srv.URL)

	_, _, err := runDecideCommand(t, cfgPath, "", "--decider", "triage")
	if err == nil {
		t.Fatal("decide with no state succeeded, want a usage error")
	}
	if !strings.Contains(err.Error(), "decide: no prompt given") {
		t.Errorf("error = %v, want the decide-prefixed no-state usage error", err)
	}
}

// TestDecideCommandStateFromStdin exercises the - state form.
func TestDecideCommandStateFromStdin(t *testing.T) {
	srv := newFakeDecisionServer(t)
	cfgPath := writeDeciderConfig(t, t.TempDir(), srv.URL)

	out, _, err := runDecideCommand(t, cfgPath, "piped state\n", "--decider", "triage", "-")
	if err != nil {
		t.Fatalf("decide error = %v, want nil", err)
	}
	if !strings.Contains(out, `"priority":{"type":"choice","choice":"high"}`) {
		t.Errorf("stdout = %q, want the answers JSON", out)
	}
}

// TestDecideCommandMissingDeciderFlag pins that --decider is required.
func TestDecideCommandMissingDeciderFlag(t *testing.T) {
	srv := newFakeDecisionServer(t)
	cfgPath := writeDeciderConfig(t, t.TempDir(), srv.URL)

	_, _, err := runDecideCommand(t, cfgPath, "", "the ticket")
	if err == nil {
		t.Fatal("decide without --decider succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "decider") {
		t.Errorf("error = %v, want the required-flag error", err)
	}
}
