// Package tools implements execution of tools declared in blorb.json as
// subprocesses, plus the registry mapping tool names to their definitions.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/llm"
)

// DefaultTimeout bounds each tool execution.
const DefaultTimeout = 30 * time.Second

// maxOutputLen bounds captured stdout and stderr so a runaway tool cannot
// exhaust memory. Truncated output is marked as such.
const maxOutputLen = 1 << 20

// maxStderrLen bounds captured stderr included in failure output.
const maxStderrLen = 4 << 10

// Tool is an executable tool with its API-facing definition.
type Tool struct {
	Name        string
	Description string
	// ArgsSchema is the JSON schema describing the arguments, passed
	// through to the API. When nil, "{}" is used.
	ArgsSchema json.RawMessage
	Command    []string
}

// ToolResult is the outcome of running a tool. Err marks tool-level failure
// (non-zero exit); that is still a valid result for the model and does not
// surface as a Go error.
type ToolResult struct {
	Output string
	Err    bool
}

// Registry holds the configured tools.
type Registry struct {
	tools   map[string]Tool
	timeout time.Duration
}

// Option customizes a Registry.
type Option func(*Registry)

// WithTimeout overrides the per-call execution timeout.
func WithTimeout(d time.Duration) Option {
	return func(r *Registry) { r.timeout = d }
}

// NewRegistry converts config tool entries into a registry, revalidating
// names. Configuration validation errors surface here.
func NewRegistry(entries []config.ToolEntry, opts ...Option) (*Registry, error) {
	r := &Registry{
		tools:   make(map[string]Tool, len(entries)),
		timeout: DefaultTimeout,
	}
	for _, opt := range opts {
		opt(r)
	}
	for _, e := range entries {
		if e.Name == "" {
			return nil, fmt.Errorf("tool with empty name")
		}
		if !config.ToolNamePattern.MatchString(e.Name) {
			return nil, fmt.Errorf("tool name %q must match %s", e.Name, config.ToolNamePattern)
		}
		if e.Description == "" {
			return nil, fmt.Errorf("tool %q: description is required", e.Name)
		}
		if len(e.Command) == 0 {
			return nil, fmt.Errorf("tool %q: command is required", e.Name)
		}
		for _, cmd := range e.Command {
			if cmd == "" {
				return nil, fmt.Errorf("tool %q: command must not contain empty strings", e.Name)
			}
		}
		if _, dup := r.tools[e.Name]; dup {
			return nil, fmt.Errorf("duplicate tool name %q", e.Name)
		}
		r.tools[e.Name] = Tool{
			Name:        e.Name,
			Description: e.Description,
			ArgsSchema:  e.ArgsSchema,
			Command:     e.Command,
		}
	}
	return r, nil
}

// Names returns the registered tool names, sorted for stable output.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Definitions produces the API-facing tool definitions using the neutral llm
// types.
func (r *Registry) Definitions() []llm.Tool {
	defs := make([]llm.Tool, 0, len(r.tools))
	for _, name := range r.Names() {
		t := r.tools[name]
		schema := t.ArgsSchema
		if len(schema) == 0 {
			schema = json.RawMessage("{}")
		}
		defs = append(defs, llm.Tool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  schema,
		})
	}
	return defs
}

// Run executes the named tool with the given JSON args (the args become the
// tool process's stdin). Infrastructure failures (unknown tool, bad args,
// timeout, unusable stdin) return an error; tool-level failures (non-zero
// exit) come back as ToolResult{Err: true} with no error.
func (r *Registry) Run(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
	tool, ok := r.tools[name]
	if !ok {
		return ToolResult{}, fmt.Errorf("unknown tool %q (known tools: %s)", name, strings.Join(r.Names(), ", "))
	}

	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage("{}")
	}
	if !json.Valid(args) {
		return ToolResult{}, fmt.Errorf("tool %q: args are not valid JSON", name)
	}

	runCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, tool.Command[0], tool.Command[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	cmd.Stdin = bytes.NewReader(args)
	var stdout, stderr limitedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return ToolResult{}, fmt.Errorf("tool %q: start %s: %w", name, tool.Command[0], err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- cmd.Wait()
	}()

	var runErr error
	select {
	case runErr = <-errCh:
	case <-runCtx.Done():
		killProcessGroup(cmd.Process)
		<-errCh // reap the child; the outcome no longer matters
		if ctx.Err() != nil {
			return ToolResult{}, fmt.Errorf("tool %q: %w", name, ctx.Err())
		}
		return ToolResult{}, fmt.Errorf("tool %q: timed out after %s", name, r.timeout)
	}

	if runErr == nil {
		return ToolResult{Output: trimSingleTrailingNewline(stdout.String())}, nil
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return ToolResult{
			Output: fmt.Sprintf("Tool %s failed with exit code %d.\nStderr: %s",
				name, exitErr.ExitCode(), truncateStderr(stderr.String())),
			Err: true,
		}, nil
	}
	return ToolResult{}, fmt.Errorf("tool %q: %w", name, runErr)
}

// killProcessGroup kills the tool and anything it spawned. The process was
// started with Setpgid, so its pid is its process group id.
func killProcessGroup(p *os.Process) {
	if p == nil {
		return
	}
	_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
	_ = p.Kill()
}

func trimSingleTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\r\n") {
		return s[:len(s)-2]
	}
	if strings.HasSuffix(s, "\n") {
		return s[:len(s)-1]
	}
	return s
}

func truncateStderr(s string) string {
	if len(s) > maxStderrLen {
		return truncateRunes(s, maxStderrLen) + "...(truncated)"
	}
	return s
}

// limitedBuffer is an io.Writer that retains at most maxOutputLen bytes and
// reports whether it dropped anything beyond that.
type limitedBuffer struct {
	buf     bytes.Buffer
	dropped bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := maxOutputLen - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
			b.dropped = true
		} else {
			b.buf.Write(p)
		}
	} else {
		b.dropped = true
	}
	return len(p), nil // always claim success so the child never sees an error
}

func (b *limitedBuffer) String() string {
	s := b.buf.String()
	if b.dropped {
		s += "\n...(output truncated at 1 MiB)"
	}
	return s
}

// truncateRunes cuts s to at most max bytes without splitting a UTF-8 rune.
func truncateRunes(s string, max int) string {
	for max < len(s) && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}
