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

// defaultAgentArgsSchema is the args schema for a subagent tool whose
// entry declares none: a single required prompt string that becomes the
// subagent's user message.
const defaultAgentArgsSchema = `{"type":"object","properties":{"prompt":{"type":"string","description":"The message to send to the agent."}},"required":["prompt"],"additionalProperties":false}`

// subagentTool delegates to another agent defined in the same config: a
// call runs that agent for one turn and returns its final assistant text.
// The run is bounded by the subagent's max_turns and context cancellation,
// not the registry's per-tool timeout.
type subagentTool struct {
	toolName    string
	description string
	agentName   string
	argsSchema  json.RawMessage
	runner      SubagentRunner
	events      func(SubagentEvent) error
}

// newSubagentTool validates a subagent tool entry and builds its
// implementation.
func newSubagentTool(e config.ToolEntry, runner SubagentRunner, events func(SubagentEvent) error) (tool, error) {
	if e.Agent == "" {
		return nil, fmt.Errorf("tool %q: agent is required", e.Name)
	}
	if !config.NamePattern.MatchString(e.Agent) {
		return nil, fmt.Errorf("tool %q: agent %q must match %s", e.Name, e.Agent, config.NamePattern)
	}
	if runner == nil {
		return nil, fmt.Errorf("tool %q: subagent tools require a subagent runner (pass tools.WithSubagentRunner)", e.Name)
	}
	return &subagentTool{
		toolName:    e.Name,
		description: e.Description,
		agentName:   e.Agent,
		argsSchema:  e.ArgsSchema,
		runner:      runner,
		events:      events,
	}, nil
}

func (t *subagentTool) name() string { return t.toolName }

// close releases per-instance resources; a subagent tool holds none.
func (t *subagentTool) close() {}

func (t *subagentTool) definition() llm.Tool {
	schema := t.argsSchema
	if len(schema) == 0 {
		schema = json.RawMessage(defaultAgentArgsSchema)
	}
	return llm.Tool{
		Name:        t.toolName,
		Description: t.description,
		Parameters:  schema,
	}
}

// longRunning marks the tool exempt from the registry's per-call timeout:
// a subagent run is bounded by the subagent's max_turns and ctx instead.
func (t *subagentTool) longRunning() {}

// run executes the subagent for one turn. Tool-level failures (bad
// arguments, subagent-level failure such as exhausted max_turns) return
// ToolResult{Err: true} with no error; infrastructure failures (a runner
// error) return an error.
func (t *subagentTool) run(ctx context.Context, args json.RawMessage, sink logging.Sink) (ToolResult, error) {
	userMessage, res, err := t.userMessage(args)
	if err != nil || res.Err {
		writeResultRecord(sink, t.toolName, res.Output)
		return res, err
	}

	// Relay the runner's events to the session callback, bumping Depth by
	// one: the runner reports Depth 0 relative to itself, and this tool
	// call sits one level deeper than its caller. Agent is left untouched
	// — it already names the deepest producing agent.
	forward := func(ev SubagentEvent) error {
		if t.events == nil {
			return nil
		}
		ev.Depth++
		return t.events(ev)
	}

	res2, err := t.runner.RunSubagent(ctx, t.agentName, userMessage, forward)
	if err != nil {
		err = fmt.Errorf("tool %q: %w", t.toolName, err)
		writeResultRecord(sink, t.toolName, "error: "+err.Error())
		return ToolResult{}, err
	}

	res = ToolResult{Output: trimSingleTrailingNewline(res2.Output), Err: res2.Err}
	writeResultRecord(sink, t.toolName, res.Output)
	return res, nil
}

// userMessage builds the user message sent to the subagent from the tool
// call's arguments. With the default schema the prompt field is extracted;
// a custom args_schema passes the raw JSON through verbatim, the
// subagent's system prompt being expected to account for that. A bad
// prompt is a model-facing failure: ToolResult{Err: true}, no Go error.
func (t *subagentTool) userMessage(args json.RawMessage) (string, ToolResult, error) {
	if len(t.argsSchema) > 0 {
		return string(args), ToolResult{}, nil
	}

	var parsed struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(args, &parsed); err != nil || strings.TrimSpace(parsed.Prompt) == "" {
		failure := fmt.Sprintf("tool %q: arguments must include a non-empty \"prompt\" string", t.toolName)
		return "", ToolResult{Output: failure, Err: true}, nil
	}
	return parsed.Prompt, ToolResult{}, nil
}
