package usage

import (
	"fmt"
	"strings"
	"time"

	"github.com/overspecific/blorb/internal/llm"
)

// FormatTurn renders a per-turn footer block: a blank line, a
// horizontal-rule delimiter, then one usage line per agent that made a
// call (a name-ordered split; an agent's multiple calls sum into its one
// line), then a "total:" line across all agents. When a single agent
// contributed, its line and the total carry the same numbers, so the
// block is just that one line — the total line is not repeated. The
// shape, with subagents:
//
//	(blank line)
//	---
//	main: 123 prompt, 456 completion, 579 total, 4s, 9.2KB output, 20.0 tok/s, 2.3KB/s
//	worker: 23 prompt, 156 completion, 179 total, 2s, 1.1KB output, 11.5 tok/s, 550B/s
//	total: 146 prompt, 612 completion, 758 total, 6s, 10.3KB output, 25.0 tok/s, 2.1KB/s
//
// and a single agent renders only its line:
//
//	---
//	main: 123 prompt, 456 completion, 579 total, 4s, 9.2KB output, 20.0 tok/s, 2.3KB/s
//
// Each line's stats part (elapsed time, output bytes with their
// text/reasoning/tools split, and the derived rates) renders when that
// line's summed stats measured anything; lines whose client measured
// nothing show tokens only. A zero token total still renders: it means
// the provider did not report usage, not that no call happened.
func FormatTurn(a *Account) string {
	return "\n---\n" + formatLines(a, "")
}

// FormatSession renders the session totals block, same shape as
// FormatTurn but with a "session " prefix on each line label (chat prints
// it at exit).
func FormatSession(a *Account) string {
	return "\n---\n" + formatLines(a, "session ")
}

// formatLines renders the per-agent lines plus the total line. Labels are
// prefixed with prefix ("session " or ""). The total line is omitted when
// a single agent contributed — its line is already the total.
func formatLines(a *Account, prefix string) string {
	totals := a.AgentTotals()
	lines := make([]string, 0, len(totals)+1)
	for _, at := range totals {
		lines = append(lines, prefix+at.Agent+": "+agentLine(a, at))
	}
	if len(totals) > 1 {
		lines = append(lines, prefix+"total: "+agentLine(a, AgentTotal{Usage: a.Total(), Stats: a.TotalStats()}))
	}
	return strings.Join(lines, "\n")
}

// agentLine renders one agent's "N prompt, N completion, N total" body
// plus its stats part (formatStatsPart) when the agent's calls measured
// any output or time.
func agentLine(a *Account, at AgentTotal) string {
	line := fmt.Sprintf("%d prompt, %d completion, %d total",
		at.Usage.PromptTokens, at.Usage.CompletionTokens, at.Usage.TotalTokens)
	if stats := formatStatsPart(at.Stats, at.Usage.CompletionTokens); stats != "" {
		line += ", " + stats
	}
	return line
}

// formatStatsPart renders the stats part of one agent line: elapsed
// time, output bytes with their content/reasoning/tools split, and the
// derived rates. completionTokens is the agent's summed completion
// count, feeding tokens/sec. It is empty — renders nothing — when no
// call measured any output or time (the summed stats are all zero).
func formatStatsPart(stats llm.CallStats, completionTokens int) string {
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
			fmt.Sprintf("%.1f tok/s", float64(completionTokens)/stats.Elapsed.Seconds()),
			fmt.Sprintf("%s/s", humanBytes(int(float64(stats.Output.Total())/stats.Elapsed.Seconds()))))
	}

	return strings.Join(parts, ", ")
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
