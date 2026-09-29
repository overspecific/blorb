package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/logging"
)

// bandTool invokes a Band platform tool by name: the seven fixed
// band_* tools the band package defines. The executor (wired by the
// band command) carries the actual platform client.
type bandTool struct {
	toolName    string
	description string
	selector    string
	schema      json.RawMessage
	executor    BandExecutor
}

// newBandTool validates a programmatically-built band entry and builds
// its implementation.
func newBandTool(e config.ToolEntry, executor BandExecutor) (tool, error) {
	if e.Band == "" {
		return nil, fmt.Errorf("tool %q: band is required", e.Name)
	}
	if executor == nil {
		return nil, fmt.Errorf("tool %q: band tools require a band executor (pass tools.WithBandExecutor)", e.Name)
	}
	return &bandTool{
		toolName:    e.Name,
		description: e.Description,
		selector:    e.Band,
		schema:      e.ArgsSchema,
		executor:    executor,
	}, nil
}

func (t *bandTool) name() string { return t.toolName }

// close releases per-instance resources; a band tool holds none.
func (t *bandTool) close() {}

func (t *bandTool) definition() llm.Tool {
	return llm.Tool{
		Name:        t.toolName,
		Description: t.description,
		Parameters:  t.schema,
	}
}

// run delegates to the band executor, which owns the platform client,
// the room binding, and the result shape (failed platform calls come
// back as ToolResult{Err: true} so the LLM can see and retry).
func (t *bandTool) run(ctx context.Context, args json.RawMessage, sink logging.Sink) (ToolResult, error) {
	res, err := t.executor.RunBandTool(ctx, t.selector, args, sink)
	if err != nil {
		err = fmt.Errorf("tool %q: %w", t.toolName, err)
		writeResultRecord(sink, t.toolName, "error: "+err.Error())
		return ToolResult{}, err
	}
	writeResultRecord(sink, t.toolName, res.Output)
	return res, nil
}

// BandExecutor executes one named Band platform tool. Mirroring
// SubagentRunner: the real implementation lives in the band package
// (bound to a client and room); tests use fakes.
type BandExecutor interface {
	RunBandTool(ctx context.Context, name string, args json.RawMessage, sink logging.Sink) (ToolResult, error)
}

// WithBandExecutor sets the executor band tools dispatch through.
// Required for registries containing band tools.
func WithBandExecutor(e BandExecutor) Option {
	return func(reg *Registry) { reg.bandExecutor = e }
}
