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
	"strings"
	"syscall"
	"time"

	"github.com/overspecific/goblorb/internal/config"
	"github.com/overspecific/goblorb/internal/llm"
)

// DefaultTimeout bounds each tool execution.
const DefaultTimeout = 30 * time.Second

// maxStderrLen bounds captured stderr included in failure output.
const maxStderrLen = 4 << 10

// Tool is an executable tool with its API-facing definition.
type Tool struct {
	Name        string
	Description string
	// ArgsSchema is free-form JSON schema text passed through to the API.
	// When empty, "{}" is used.
	ArgsSchema string
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
		if len(e.Command) == 0 {
			return nil, fmt.Errorf("tool %q: command is required", e.Name)
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
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	return names
}

// Definitions produces the API-facing tool definitions using the neutral llm
// types.
func (r *Registry) Definitions() []llm.Tool {
	defs := make([]llm.Tool, 0, len(r.tools))
	for _, name := range r.Names() {
		t := r.tools[name]
		schema := t.ArgsSchema
		if schema == "" {
			schema = "{}"
		}
		defs = append(defs, llm.Tool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  json.RawMessage(schema),
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
	var stdout, stderr bytes.Buffer
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
		runErr = <-errCh
		if ctx.Err() != nil {
			return ToolResult{}, ctx.Err()
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
		return s[:maxStderrLen] + "...(truncated)"
	}
	return s
}
