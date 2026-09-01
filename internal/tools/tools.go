// Package tools implements execution of tools declared in blorb.json,
// plus the registry mapping tool names to their definitions. Tool
// implementations are dispatched on the entry's type: "command" tools run
// as subprocesses, and other tool types live in their own packages.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/logging"
)

// DefaultTimeout bounds each tool execution.
const DefaultTimeout = 30 * time.Second

// maxOutputLen bounds captured stdout and stderr so a runaway tool cannot
// exhaust memory. Truncated output is marked as such.
const maxOutputLen = 1 << 20

// maxStderrLen bounds captured stderr included in failure output.
const maxStderrLen = 4 << 10

// tool is the internal interface every tool implementation satisfies. The
// registry applies the per-call timeout and sink; each implementation owns
// its execution and its wire-log result record. close releases per-instance
// resources (the builtins' sandbox roots); command tools release nothing.
type tool interface {
	name() string
	definition() llm.Tool
	run(ctx context.Context, args json.RawMessage, sink logging.Sink) (ToolResult, error)
	close()
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
	tools map[string]tool
	// order lists the tool names in registration order, preserving the
	// caller's listing (the agent's tool order) for Definitions.
	order   []string
	timeout time.Duration
	sink    logging.Sink
	baseDir string
}

// Option customizes a Registry.
type Option func(*Registry)

// WithTimeout overrides the per-call execution timeout.
func WithTimeout(d time.Duration) Option {
	return func(r *Registry) { r.timeout = d }
}

// WithSink sets the wire-log sink for tool calls and results. A nil sink
// (the default) disables logging.
func WithSink(s logging.Sink) Option {
	return func(r *Registry) { r.sink = s }
}

// WithConfigDir sets the directory that config-relative paths (builtin
// base_dir) resolve against. Callers pass config.Dir(); empty means the
// process working directory.
func WithConfigDir(dir string) Option {
	return func(r *Registry) { r.baseDir = dir }
}

// NewRegistry converts config tool entries into a registry, revalidating
// each entry per its type. Configuration validation errors surface here.
func NewRegistry(entries []config.ToolEntry, opts ...Option) (*Registry, error) {
	r := &Registry{
		tools:   make(map[string]tool, len(entries)),
		timeout: DefaultTimeout,
	}
	for _, opt := range opts {
		opt(r)
	}
	for _, e := range entries {
		if e.Name == "" {
			return nil, fmt.Errorf("tool with empty name")
		}
		if !config.NamePattern.MatchString(e.Name) {
			return nil, fmt.Errorf("tool name %q must match %s", e.Name, config.NamePattern)
		}
		if e.Description == "" {
			return nil, fmt.Errorf("tool %q: description is required", e.Name)
		}
		var t tool
		var err error
		switch e.Type {
		case config.ToolTypeCommand:
			t, err = newCommandTool(e)
		case config.ToolTypeBuiltin:
			t, err = newBuiltinTool(e, r.baseDir)
		default:
			err = fmt.Errorf("tool %q: unknown tool type %q (supported: %s)",
				e.Name, e.Type, strings.Join(config.SupportedToolTypes(), ", "))
		}
		if err != nil {
			return nil, err
		}
		if _, dup := r.tools[t.name()]; dup {
			return nil, fmt.Errorf("duplicate tool name %q", t.name())
		}
		r.tools[t.name()] = t
		r.order = append(r.order, t.name())
	}
	return r, nil
}

// Close releases per-instance resources held by the tools: the file
// builtins' open sandbox roots. A registry is built once at startup and
// held for the process or session lifetime; chat closes it at teardown so
// the roots are not left open until process exit. Safe to call more than
// once.
func (r *Registry) Close() {
	for _, t := range r.tools {
		t.close()
	}
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
// types, in registration order: the caller's (the agent's) tool listing.
func (r *Registry) Definitions() []llm.Tool {
	defs := make([]llm.Tool, 0, len(r.order))
	for _, name := range r.order {
		defs = append(defs, r.tools[name].definition())
	}
	return defs
}

// Run executes the named tool with the given JSON args. Infrastructure
// failures (unknown tool, bad args, timeout, spawn failure) return an
// error; tool-reported failures (non-zero exit, tool-reported errors) come
// back as ToolResult{Err: true} with no error.
//
// Each implementation writes its own KindToolResult wire-log record at each
// outcome point; the registry writes the shared KindToolRequest record just
// before execution starts. Nothing is logged for unknown tools or invalid
// args: nothing ran, so there is no wire traffic to record. Sink write
// errors are ignored — logging is best-effort.
func (r *Registry) Run(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
	t, ok := r.tools[name]
	if !ok {
		return ToolResult{}, fmt.Errorf("unknown tool %q (known tools: %s)", name, strings.Join(r.Names(), ", "))
	}

	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage("{}")
	}
	if !json.Valid(args) {
		return ToolResult{}, fmt.Errorf("tool %q: args are not valid JSON", name)
	}

	// Logged only once execution is possible, immediately before the
	// implementation runs. The write error is ignored — logging is
	// best-effort.
	if r.sink != nil {
		r.sink.Write(logging.Record{
			Time:   time.Now(),
			Kind:   logging.KindToolRequest,
			Method: "RUN",
			URL:    name,
			Body:   append([]byte(nil), args...),
		})
	}

	runCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	return t.run(runCtx, args, r.sink)
}

// writeResultRecord logs one KindToolResult record with the given body.
// Called directly at each outcome point (not deferred) so the timestamp
// reflects when the outcome was known. Write errors are ignored — logging
// is best-effort.
func writeResultRecord(sink logging.Sink, name, body string) {
	if sink == nil {
		return
	}
	sink.Write(logging.Record{
		Time:   time.Now(),
		Kind:   logging.KindToolResult,
		Method: "RUN",
		URL:    name,
		Body:   []byte(body),
	})
}

// trimSingleTrailingNewline trims one trailing newline (\n or \r\n).
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
