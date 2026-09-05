// Package usage accounts for token usage across a turn or session:
// each LLM call is itemised and attributed to the agent that made it,
// and totals roll up per agent and overall.
package usage

import (
	"sort"

	"github.com/overspecific/blorb/internal/llm"
)

// Record describes one LLM call's token usage. Agent names the agent
// whose engine made the call (the session agent or a subagent); Model
// is the configured provider model, which may be empty when unknown.
// Calls are identified only by their position in the account; providers
// do not report a stable call id.
type Record struct {
	Agent string
	Model string
	Usage llm.Usage
	// Stats holds the call's stats; zero when the provider client did
	// not measure.
	Stats llm.CallStats
}

// AgentTotal is one agent's summed usage.
type AgentTotal struct {
	Agent string
	Usage llm.Usage
	// Stats is the agent's summed call stats.
	Stats llm.CallStats
}

// Account accumulates usage records for one turn or one session.
// Methods are not safe for concurrent use; a session's turns run
// sequentially.
type Account struct {
	records []Record
}

// Add appends one call's record. A call whose usage is all-zero (some
// servers report nothing) is still recorded: it is a real call whose
// usage is unknown, and dropping it would under-count calls.
func (a *Account) Add(rec Record) {
	a.records = append(a.records, rec)
}

// Records returns a defensive copy of the accumulated records, in
// call order.
func (a *Account) Records() []Record {
	out := make([]Record, len(a.records))
	copy(out, a.records)
	return out
}

// AgentTotals returns per-agent summed usage sorted by agent name.
func (a *Account) AgentTotals() []AgentTotal {
	byAgent := make(map[string]llm.Usage)
	statsByAgent := make(map[string]llm.CallStats)
	order := make([]string, 0, len(a.records))
	for _, rec := range a.records {
		if _, seen := byAgent[rec.Agent]; !seen {
			order = append(order, rec.Agent)
		}
		u := byAgent[rec.Agent]
		u.PromptTokens += rec.Usage.PromptTokens
		u.CompletionTokens += rec.Usage.CompletionTokens
		u.TotalTokens += rec.Usage.TotalTokens
		byAgent[rec.Agent] = u

		s := statsByAgent[rec.Agent]
		s.Output.Content += rec.Stats.Output.Content
		s.Output.Reasoning += rec.Stats.Output.Reasoning
		s.Output.ToolCalls += rec.Stats.Output.ToolCalls
		s.Elapsed += rec.Stats.Elapsed
		statsByAgent[rec.Agent] = s
	}

	sort.Strings(order)
	totals := make([]AgentTotal, 0, len(order))
	for _, agent := range order {
		totals = append(totals, AgentTotal{Agent: agent, Usage: byAgent[agent], Stats: statsByAgent[agent]})
	}
	return totals
}

// Total returns the summed usage across all records.
func (a *Account) Total() llm.Usage {
	var total llm.Usage
	for _, rec := range a.records {
		total.PromptTokens += rec.Usage.PromptTokens
		total.CompletionTokens += rec.Usage.CompletionTokens
		total.TotalTokens += rec.Usage.TotalTokens
	}
	return total
}

// TotalStats returns the summed call stats across all records.
func (a *Account) TotalStats() llm.CallStats {
	var total llm.CallStats
	for _, rec := range a.records {
		total.Output.Content += rec.Stats.Output.Content
		total.Output.Reasoning += rec.Stats.Output.Reasoning
		total.Output.ToolCalls += rec.Stats.Output.ToolCalls
		total.Elapsed += rec.Stats.Elapsed
	}
	return total
}
