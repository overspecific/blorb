package run

import (
	"encoding/json"
	"io"

	"github.com/overspecific/blorb/internal/engine"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/tools"
	"github.com/overspecific/blorb/internal/usage"
)

// The ndjson format streams the run's full event stream to stdout as one
// JSON object per line, written as each event arrives — the whole stream,
// not just assistant messages. Every line is a flat typed event
// discriminated by its "type" field; consumers must ignore unknown types
// (forward compatibility). The vocabulary:
//
//	text_delta      {type, text}                    — assistant text fragment
//	thinking_delta  {type, thinking}                — reasoning fragment
//	tool_call_delta {type, index, name?, arguments} — tool call fragment; name on the first fragment for an index, arguments concatenated
//	text            {type, text}                    — whole assistant message (non-streaming)
//	thinking        {type, thinking}                — whole reasoning (non-streaming)
//	tool_call       {type, name, arguments}         — whole tool call
//	tool_result     {type, name, output, failed}    — tool result; output is always the full body
//	usage           {type, agent, model, usage, stats} — one completed LLM call's token usage and call stats
//	subagent_*      — the same vocabulary prefixed subagent_, with agent and depth fields added
//	done            {type, text?, usage, stats?, rates?, agents} — terminal on success; text is the final assistant text
//	error           {type, error}                   — terminal on failure; no done follows
//
// The stream contract:
//
//   - Events are emitted once the turn starts. A run that fails before
//     the turn (unknown format, client build, registry build, tracer
//     start) emits no lines at all; the error goes to stderr via main's
//     reporting.
//   - Success ends with done, carrying the final text (absent when empty)
//     and the usage totals with the per-agent split, plus the summed call
//     stats and — when any time was measured — the derived rates.
//   - Failure (the run failed, turn error or post-turn tracer failure)
//     ends with error carrying the run error's message and no done; the
//     exit codes are unchanged (main maps them).
//   - Platform termination mid-turn exits 0, so the stream ends with done
//     carrying no text and the usage totals; the human-readable reason
//     goes to the diagnostics writer (stderr).
//
// Streaming is inherently delta-driven: with Stream on, delta events go
// out line-by-line as they arrive; with --no-stream the whole-message
// types carry the turn instead. The terminal event is emitted regardless
// of stream mode.

// ndjsonEvent is one line of the ndjson stream. All events share the same
// flat shape with omitempty on optional fields; type discriminates.
type ndjsonEvent struct {
	Type string `json:"type"`
	// Text carries assistant text (text, text_delta).
	Text string `json:"text,omitempty"`
	// Thinking carries reasoning (thinking, thinking_delta).
	Thinking string `json:"thinking,omitempty"`
	// Name carries the tool name (tool_call, tool_call_delta,
	// tool_result); on tool_call_delta it may repeat or be empty
	// mid-stream — consumers key by index.
	Name string `json:"name,omitempty"`
	// Index is the tool call the delta fragment belongs to (always
	// present on tool_call_delta, including 0).
	Index *int `json:"index,omitempty"`
	// Arguments is the (fragment of the) JSON-encoded arguments string.
	Arguments string `json:"arguments,omitempty"`
	// Output is the full tool result body (tool_result); no summary,
	// regardless of ToolOutput.
	Output string `json:"output,omitempty"`
	// Failed marks a failed tool result (tool_result).
	Failed bool `json:"failed,omitempty"`
	// Agent attributes the event (usage, subagent_*).
	Agent string `json:"agent,omitempty"`
	// Model is the agent's configured provider model (usage,
	// subagent_usage).
	Model string `json:"model,omitempty"`
	// Depth is the subagent nesting level (always present on
	// subagent_*, including 0); the parent's own events carry none.
	Depth *int `json:"depth,omitempty"`
	// Usage is the token usage (usage, subagent_usage, done).
	Usage *llm.Usage `json:"usage,omitempty"`
	// Stats is the stats of one call (usage, subagent_usage) and the
	// summed stats (done). Nil omits it; per-call events always carry
	// it (zero included — "the client did not measure").
	Stats *llm.CallStats `json:"stats,omitempty"`
	// Rates is the derived throughput (done), omitted when no time was
	// measured. Zero fields mean no time was measured.
	Rates *ndjsonRates `json:"rates,omitempty"`
	// Agents is the per-agent usage split (done), mirroring
	// usage.Account.AgentTotals.
	Agents []ndjsonAgentUsage `json:"agents,omitempty"`
	// Error is the run error's message (error).
	Error string `json:"error,omitempty"`
}

// ndjsonAgentUsage is one agent's summed usage in done.agents.
type ndjsonAgentUsage struct {
	Agent string    `json:"agent"`
	Usage llm.Usage `json:"usage"`
	// Stats is the agent's summed call stats; always present, zero
	// included — "measured nothing".
	Stats llm.CallStats `json:"stats"`
}

// ndjsonRates is the derived throughput on done, computed from the
// summed stats. Zero fields mean no time was measured.
type ndjsonRates struct {
	TokensPerSec float64 `json:"tokens_per_sec"`
	BytesPerSec  float64 `json:"bytes_per_sec"`
}

// ndjsonSink writes the ndjson event stream: printEvent and onSubagent
// marshal one line per event as it arrives, and finish emits the terminal
// done/error event after the turn. A marshal error (the only failure
// mode: a struct-to-JSON bug) returns from the callback, aborting the
// turn per the standard callback contract.
type ndjsonSink struct {
	enc     *json.Encoder
	account *usage.Account
}

// newNDJSONSink builds the sink. SetEscapeHTML(false) keeps tool output
// containing <, >, and & readable in the JSON.
func newNDJSONSink(w io.Writer, account *usage.Account) *ndjsonSink {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &ndjsonSink{enc: enc, account: account}
}

// emit writes one line.
func (s *ndjsonSink) emit(ev ndjsonEvent) error {
	return s.enc.Encode(ev)
}

// printEvent maps one engine event to its line.
func (s *ndjsonSink) printEvent(ev engine.Event) error {
	switch ev.Kind {
	case engine.EventAssistantText:
		return s.emit(ndjsonEvent{Type: "text", Text: ev.Text})
	case engine.EventAssistantThinking:
		return s.emit(ndjsonEvent{Type: "thinking", Thinking: ev.Text})
	case engine.EventAssistantTextDelta:
		return s.emit(ndjsonEvent{Type: "text_delta", Text: ev.Text})
	case engine.EventAssistantThinkingDelta:
		return s.emit(ndjsonEvent{Type: "thinking_delta", Thinking: ev.Text})
	case engine.EventToolCall:
		return s.emit(ndjsonEvent{Type: "tool_call", Name: ev.Name, Arguments: ev.Args})
	case engine.EventToolCallDelta:
		return s.emit(ndjsonEvent{Type: "tool_call_delta", Name: ev.Name, Index: &ev.Index, Arguments: ev.Args})
	case engine.EventToolResult:
		return s.emit(ndjsonEvent{Type: "tool_result", Name: ev.Name, Output: ev.Output, Failed: ev.Failed})
	case engine.EventUsage:
		usage := ev.Usage
		stats := ev.Stats
		return s.emit(ndjsonEvent{Type: "usage", Agent: ev.AgentName, Model: ev.Model, Usage: &usage, Stats: &stats})
	default:
		return nil
	}
}

// onSubagent maps one subagent event to its subagent_-prefixed line,
// carrying the producing agent's name and nesting depth.
func (s *ndjsonSink) onSubagent(ev tools.SubagentEvent) error {
	var e ndjsonEvent
	switch ev.Kind {
	case tools.SubagentText:
		e = ndjsonEvent{Type: "subagent_text", Text: ev.Text}
	case tools.SubagentThinking:
		e = ndjsonEvent{Type: "subagent_thinking", Thinking: ev.Text}
	case tools.SubagentTextDelta:
		e = ndjsonEvent{Type: "subagent_text_delta", Text: ev.Text}
	case tools.SubagentThinkingDelta:
		e = ndjsonEvent{Type: "subagent_thinking_delta", Thinking: ev.Text}
	case tools.SubagentToolCall:
		e = ndjsonEvent{Type: "subagent_tool_call", Name: ev.Name, Arguments: ev.Args}
	case tools.SubagentToolCallDelta:
		e = ndjsonEvent{Type: "subagent_tool_call_delta", Name: ev.Name, Index: &ev.Index, Arguments: ev.Args}
	case tools.SubagentToolResult:
		e = ndjsonEvent{Type: "subagent_tool_result", Name: ev.Name, Output: ev.Output, Failed: ev.Failed}
	case tools.SubagentUsage:
		stats := ev.Stats
		e = ndjsonEvent{Type: "subagent_usage", Model: ev.Model, Usage: &ev.Usage, Stats: &stats}
	default:
		return nil
	}
	e.Agent = ev.Agent
	e.Depth = &ev.Depth
	return s.emit(e)
}

// finish emits the terminal event: error on a failed run (no done),
// otherwise done with the final text (absent on platform termination,
// which exits 0 with an empty final) and the usage totals. runErr is the
// run's settled outcome error — after the outcome mapping and session
// finish — so the terminal event is the stream's actual last word.
// Called iff RunTurn was entered.
func (s *ndjsonSink) finish(final string, runErr error) error {
	if runErr != nil {
		return s.emit(ndjsonEvent{Type: "error", Error: runErr.Error()})
	}
	total := s.account.Total()
	done := ndjsonEvent{Type: "done", Text: final, Usage: &total}
	stats := s.account.TotalStats()
	done.Stats = &stats
	if stats.Elapsed > 0 {
		done.Rates = &ndjsonRates{
			TokensPerSec: s.account.TokensPerSec(),
			BytesPerSec:  s.account.BytesPerSec(),
		}
	}
	for _, at := range s.account.AgentTotals() {
		done.Agents = append(done.Agents, ndjsonAgentUsage{Agent: at.Agent, Usage: at.Usage, Stats: at.Stats})
	}
	return s.emit(done)
}
