package tools

import (
	"context"

	"github.com/overspecific/blorb/internal/llm"
)

// SubagentEventKind mirrors the engine event kinds relevant to display.
type SubagentEventKind string

const (
	SubagentText          SubagentEventKind = "text"
	SubagentThinking      SubagentEventKind = "thinking"
	SubagentTextDelta     SubagentEventKind = "text_delta"
	SubagentThinkingDelta SubagentEventKind = "thinking_delta"
	SubagentToolCall      SubagentEventKind = "tool_call"
	SubagentToolCallDelta SubagentEventKind = "tool_call_delta"
	SubagentToolResult    SubagentEventKind = "tool_result"
	// SubagentUsage carries the token usage of one completed LLM call
	// by the named agent.
	SubagentUsage SubagentEventKind = "usage"
)

// SubagentEvent is one observable moment of a subagent run. Agent is
// the name of the agent that produced it and Depth its nesting level
// below the caller (1 for a directly invoked subagent).
type SubagentEvent struct {
	Agent  string
	Depth  int
	Kind   SubagentEventKind
	Text   string
	Name   string
	Args   string
	Index  int
	Output string
	Failed bool
	// Usage is set on SubagentUsage events: the token usage of one
	// completed LLM call by the named agent.
	Usage llm.Usage
}

// SubagentUsageRecord is one LLM call's usage within a subagent run.
type SubagentUsageRecord struct {
	Agent string
	Model string
	Usage llm.Usage
}

// SubagentResult is the outcome of a subagent run, mirroring ToolResult:
// Err marks a subagent-level failure (e.g. it exhausted its max_turns),
// which is still a valid tool result for the parent model.
type SubagentResult struct {
	Output string
	Err    bool
	// Usage itemises the subagent's own LLM calls, one record per call
	// in call order; a subagent whose provider reports no usage has
	// zero-usage records. Empty when the subagent run failed before
	// completing its first call. The parent's own usage from this
	// exchange is accounted by its next LLM call's prompt tokens, which
	// include the tool call and result messages.
	Usage []SubagentUsageRecord
}

// SubagentRunner executes a named agent from the same config as a
// subagent: it runs the agent to completion for one user message —
// resolving all of its tool calls, like a chat turn — and returns its
// concatenated assistant text. Implementations emit events with Agent
// set to the running agent's name and Depth 0 (relative to themselves).
// The real implementation lives in the engine package; tests use fakes.
type SubagentRunner interface {
	RunSubagent(ctx context.Context, agentName, userMessage string, onEvent func(SubagentEvent) error) (SubagentResult, error)
}
