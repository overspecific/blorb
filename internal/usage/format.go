package usage

import (
	"fmt"
	"strings"
)

// FormatTurn renders a per-turn footer block: a horizontal-rule delimiter
// followed by the turn's total usage, with per-agent split when the
// account holds more than one agent. The shape is:
//
//	---
//	tokens: 123 prompt, 456 completion, 579 total
//
// and with multiple agents, one parenthesised item per agent:
//
//	---
//	tokens: 123 prompt, 456 completion, 579 total (main: 100/300/400, worker: 23/156/179)
//
// where each agent item is "name: prompt/completion/total". A zero total
// still renders: it means the provider did not report usage, not that no
// call happened.
func FormatTurn(a *Account) string {
	return "---\ntokens: " + formatTotals(a)
}

// FormatSession renders the session totals block, same shape as
// FormatTurn but prefixed "session tokens:" (chat prints it at exit).
func FormatSession(a *Account) string {
	return "---\nsession tokens: " + formatTotals(a)
}

// formatTotals renders the shared "N prompt, N completion, N total" body,
// plus the per-agent split when more than one agent contributed.
func formatTotals(a *Account) string {
	total := a.Total()
	body := fmt.Sprintf("%d prompt, %d completion, %d total",
		total.PromptTokens, total.CompletionTokens, total.TotalTokens)

	agentTotals := a.AgentTotals()
	if len(agentTotals) <= 1 {
		return body
	}

	items := make([]string, 0, len(agentTotals))
	for _, at := range agentTotals {
		items = append(items, fmt.Sprintf("%s: %d/%d/%d",
			at.Agent, at.Usage.PromptTokens, at.Usage.CompletionTokens, at.Usage.TotalTokens))
	}
	return body + " (" + strings.Join(items, ", ") + ")"
}
