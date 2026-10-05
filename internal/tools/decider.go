package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/logging"
)

// defaultDeciderArgsSchema is the args schema for a decider tool whose
// entry declares none: a single required state string to evaluate.
const defaultDeciderArgsSchema = `{"type":"object","properties":{"state":{"type":"string","description":"The situation to evaluate, described in plain text."}},"required":["state"],"additionalProperties":false}`

// deciderTool evaluates a named decider defined in the same config: a call
// supplies the state and returns the decider's typed answers as JSON. The
// call is one HTTP request under the registry's per-tool timeout (decision
// calls are sub-second; a hung server fails like any other tool).
type deciderTool struct {
	toolName    string
	description string
	deciderName string
	argsSchema  json.RawMessage
	runner      DeciderRunner
	events      func(SubagentEvent) error
}

// newDeciderTool validates a decider tool entry and builds its
// implementation.
func newDeciderTool(e config.ToolEntry, runner DeciderRunner, events func(SubagentEvent) error) (tool, error) {
	if e.Decider == "" {
		return nil, fmt.Errorf("tool %q: decider is required", e.Name)
	}
	if !config.NamePattern.MatchString(e.Decider) {
		return nil, fmt.Errorf("tool %q: decider %q must match %s", e.Name, e.Decider, config.NamePattern)
	}
	if runner == nil {
		return nil, fmt.Errorf("tool %q: decider tools require a decider runner (pass tools.WithDeciderRunner)", e.Name)
	}
	return &deciderTool{
		toolName:    e.Name,
		description: e.Description,
		deciderName: e.Decider,
		argsSchema:  e.ArgsSchema,
		runner:      runner,
		events:      events,
	}, nil
}

func (t *deciderTool) name() string { return t.toolName }

// close releases per-instance resources; a decider tool holds none.
func (t *deciderTool) close() {}

func (t *deciderTool) definition() llm.Tool {
	schema := t.argsSchema
	if len(schema) == 0 {
		schema = json.RawMessage(defaultDeciderArgsSchema)
	}
	return llm.Tool{
		Name:        t.toolName,
		Description: t.description,
		Parameters:  schema,
	}
}

// run evaluates the decider for the supplied state. Tool-level failures
// (bad arguments, a decider-level failure such as a rejected request)
// return ToolResult{Err: true} with no error; infrastructure failures (a
// runner error) return an error.
func (t *deciderTool) run(ctx context.Context, args json.RawMessage, sink logging.Sink) (ToolResult, error) {
	state, res, err := t.state(args)
	if err != nil || res.Err {
		writeResultRecord(sink, t.toolName, res.Output)
		return res, err
	}

	res2, err := t.runner.RunDecider(ctx, t.deciderName, state)
	if err != nil {
		err = fmt.Errorf("tool %q: %w", t.toolName, err)
		writeResultRecord(sink, t.toolName, "error: "+err.Error())
		return ToolResult{}, err
	}

	// The decider call sits one level below the calling agent: Depth 1 is
	// the same level a directly invoked subagent reports. Any subagent
	// tool between the session and this tool adds its own increment as
	// the event bubbles out.
	if t.events != nil {
		for _, rec := range res2.Usage {
			if err := t.events(SubagentEvent{
				Kind:  SubagentUsage,
				Agent: rec.Agent,
				Depth: 1,
				Model: rec.Model,
				Usage: rec.Usage,
				Stats: rec.Stats,
			}); err != nil {
				return ToolResult{}, err
			}
		}
		if !res2.Err {
			if err := t.events(SubagentEvent{
				Kind:   SubagentDecision,
				Agent:  t.deciderName,
				Depth:  1,
				Output: res2.Output,
			}); err != nil {
				return ToolResult{}, err
			}
		}
	}

	res = ToolResult{Output: trimSingleTrailingNewline(res2.Output), Err: res2.Err, Usage: res2.Usage}
	writeResultRecord(sink, t.toolName, res.Output)
	return res, nil
}

// state builds the state sent to the decider from the tool call's
// arguments. With a custom args_schema the raw JSON is the state; with the
// default schema the state field is extracted. A missing or empty state is
// a model-facing failure: ToolResult{Err: true}, no Go error.
func (t *deciderTool) state(args json.RawMessage) (json.RawMessage, ToolResult, error) {
	if len(t.argsSchema) > 0 {
		return args, ToolResult{}, nil
	}

	var parsed struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(args, &parsed); err != nil || strings.TrimSpace(parsed.State) == "" {
		failure := fmt.Sprintf("tool %q: arguments must include a non-empty \"state\" string", t.toolName)
		return nil, ToolResult{Output: failure, Err: true}, nil
	}
	encoded, err := json.Marshal(parsed.State)
	if err != nil {
		failure := fmt.Sprintf("tool %q: arguments must include a non-empty \"state\" string", t.toolName)
		return nil, ToolResult{Output: failure, Err: true}, nil
	}
	return encoded, ToolResult{}, nil
}
