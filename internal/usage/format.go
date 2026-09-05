package usage

import (
	"fmt"
	"strings"
	"time"
)

// FormatTurn renders a per-turn footer block: a blank line, a
// horizontal-rule delimiter, then the turn's total usage, with per-agent
// split when the account holds more than one agent, then the stats line
// (formatStats) when any call measured output or time. The shape is:
//
//	(blank line)
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
	return "\n---\ntokens: " + formatTotals(a) + formatStats(a)
}

// FormatSession renders the session totals block, same shape as
// FormatTurn but prefixed "session tokens:" (chat prints it at exit).
func FormatSession(a *Account) string {
	return "\n---\nsession tokens: " + formatTotals(a) + formatStats(a)
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

// formatStats renders the stats line: elapsed time, output bytes
// with their content/reasoning/tools split, and the derived rates.
// The line is empty — renders nothing — when no call measured any
// output or time (the summed stats are all zero).
func formatStats(a *Account) string {
	stats := a.TotalStats()
	if stats.Output.Total() == 0 && stats.Elapsed == 0 {
		return ""
	}

	parts := []string{humanDuration(stats.Elapsed)}

	out := fmt.Sprintf("%s output", humanBytes(stats.Output.Total()))
	// The parenthesised split attributes output to reasoning or tools;
	// content-only output is all text, so there is nothing to split
	// out. Within the paren, only non-zero components render.
	if stats.Output.Reasoning > 0 || stats.Output.ToolCalls > 0 {
		var split []string
		if stats.Output.Content > 0 {
			split = append(split, fmt.Sprintf("%s text", humanBytes(stats.Output.Content)))
		}
		if stats.Output.Reasoning > 0 {
			split = append(split, fmt.Sprintf("%s reasoning", humanBytes(stats.Output.Reasoning)))
		}
		if stats.Output.ToolCalls > 0 {
			split = append(split, fmt.Sprintf("%s tools", humanBytes(stats.Output.ToolCalls)))
		}
		if len(split) > 0 {
			out += " (" + strings.Join(split, ", ") + ")"
		}
	}
	parts = append(parts, out)

	if stats.Elapsed > 0 {
		parts = append(parts,
			fmt.Sprintf("%.1f tok/s", a.TokensPerSec()),
			fmt.Sprintf("%s/s", humanBytes(int(a.BytesPerSec()))))
	}

	return "\nstats: " + strings.Join(parts, ", ")
}

// humanBytes renders a byte count compactly: B below 1024, then KB, MB,
// and GB, dividing by 1024 with one decimal place and a trailing .0
// dropped (512B, 4KB, 4.5KB, 1MB). Values at or beyond 1024GB cap at a
// GB figure.
func humanBytes(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%dB", n)
	}
	value := float64(n)
	units := []string{"KB", "MB", "GB"}
	for i, unit := range units {
		value /= 1024
		if value < 1024 || i == len(units)-1 {
			s := fmt.Sprintf("%.1f", value)
			s = strings.TrimSuffix(s, ".0")
			return s + unit
		}
	}
	return fmt.Sprintf("%dB", n)
}

// humanDuration renders a duration compactly in seconds with one decimal
// place and a trailing .0 dropped (4s, 12.3s, 90s) — display-grade
// precision, unlike Duration.String()'s nanosecond form.
func humanDuration(d time.Duration) string {
	s := fmt.Sprintf("%.1f", d.Seconds())
	return strings.TrimSuffix(s, ".0") + "s"
}
