package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/logging"
	"github.com/overspecific/blorb/internal/tools/builtin"
)

// builtinTool runs a tool implemented inside blorb, selected by the
// entry's builtin field. The entry's name and description are the
// user-facing model-facing identity; the builtin supplies the args schema
// and execution, configured with the settings parsed from the entry's
// config object.
type builtinTool struct {
	toolName    string
	description string
	b           builtin.Builtin
	opts        builtin.Options
}

// newBuiltinTool validates a builtin tool entry and constructs the tool,
// parsing the settings (which for file builtins also verifies base_dir
// resolves to an existing directory).
func newBuiltinTool(e config.ToolEntry) (tool, error) {
	if e.Builtin == "" {
		return nil, fmt.Errorf("tool %q: builtin is required", e.Name)
	}
	b, ok := builtin.Lookup(e.Builtin)
	if !ok {
		return nil, fmt.Errorf("tool %q: unknown builtin %q (supported: %s)",
			e.Name, e.Builtin, strings.Join(builtin.Supported(), ", "))
	}
	if e.Description == "" {
		return nil, fmt.Errorf("tool %q: description is required", e.Name)
	}
	opts, err := builtin.ParseConfig(e.Builtin, e.Config)
	if err != nil {
		return nil, fmt.Errorf("tool %q: %w", e.Name, err)
	}
	return &builtinTool{
		toolName:    e.Name,
		description: e.Description,
		b:           b,
		opts:        opts,
	}, nil
}

func (t *builtinTool) name() string { return t.toolName }

func (t *builtinTool) definition() llm.Tool {
	schema := t.b.ArgsSchema
	if len(schema) == 0 {
		schema = json.RawMessage("{}")
	}
	return llm.Tool{
		Name:        t.toolName,
		Description: t.description,
		Parameters:  schema,
	}
}

// run delegates to the builtin's execution. The ctx is already derived
// with the registry's per-call timeout. Tool-reported failures come back
// as ToolResult{Err: true}; a Go error means the call could not be
// performed.
func (t *builtinTool) run(ctx context.Context, args json.RawMessage, sink logging.Sink) (ToolResult, error) {
	res, err := t.b.Run(ctx, t.opts, args)
	if err != nil {
		err = fmt.Errorf("tool %q: %w", t.toolName, err)
		writeResultRecord(sink, t.toolName, "error: "+err.Error())
		return ToolResult{}, err
	}
	result := ToolResult{Output: res.Output, Err: res.Err}
	writeResultRecord(sink, t.toolName, result.Output)
	return result, nil
}
