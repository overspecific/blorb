package usage_test

import (
	"strings"
	"testing"
	"time"

	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/usage"
)

func TestFormatTurn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		records []usage.Record
		want    string
	}{
		{
			name: "empty account renders zeros",
			want: "\n---\ntokens: 0 prompt, 0 completion, 0 total",
		},
		{
			name: "single agent renders without split",
			records: []usage.Record{
				{Agent: "main", Usage: llm.Usage{PromptTokens: 123, CompletionTokens: 456, TotalTokens: 579}},
			},
			want: "\n---\ntokens: 123 prompt, 456 completion, 579 total",
		},
		{
			name: "two agents render a name-ordered split",
			records: []usage.Record{
				{Agent: "worker", Usage: llm.Usage{PromptTokens: 23, CompletionTokens: 156, TotalTokens: 179}},
				{Agent: "main", Usage: llm.Usage{PromptTokens: 100, CompletionTokens: 300, TotalTokens: 400}},
			},
			want: "\n---\ntokens: 123 prompt, 456 completion, 579 total (main: 100/300/400, worker: 23/156/179)",
		},
		{
			name: "zero-usage records still render",
			records: []usage.Record{
				{Agent: "main", Usage: llm.Usage{}},
			},
			want: "\n---\ntokens: 0 prompt, 0 completion, 0 total",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a := &usage.Account{}
			for _, rec := range tt.records {
				a.Add(rec)
			}
			if got := usage.FormatTurn(a); got != tt.want {
				t.Errorf("FormatTurn() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatSession(t *testing.T) {
	t.Parallel()

	a := &usage.Account{}
	a.Add(usage.Record{Agent: "main", Usage: llm.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}})
	a.Add(usage.Record{Agent: "worker", Usage: llm.Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30}})

	want := "\n---\nsession tokens: 11 prompt, 22 completion, 33 total (main: 1/2/3, worker: 10/20/30)"
	if got := usage.FormatSession(a); got != want {
		t.Errorf("FormatSession() = %q, want %q", got, want)
	}
}

func TestFormatSessionPrefixDiffersFromTurn(t *testing.T) {
	t.Parallel()

	a := &usage.Account{}
	a.Add(usage.Record{Agent: "main", Usage: llm.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}})

	turn := usage.FormatTurn(a)
	session := usage.FormatSession(a)
	if turn == session {
		t.Errorf("FormatTurn and FormatSession rendered identically: %q", turn)
	}
	if !strings.HasPrefix(session, "\n---\nsession tokens: ") {
		t.Errorf("FormatSession = %q, want the session prefix after the delimiter", session)
	}
}

func TestFormatTurnDelimited(t *testing.T) {
	t.Parallel()

	a := &usage.Account{}
	a.Add(usage.Record{Agent: "main", Usage: llm.Usage{PromptTokens: 364, CompletionTokens: 182, TotalTokens: 546}})

	want := "\n---\ntokens: 364 prompt, 182 completion, 546 total"
	if got := usage.FormatTurn(a); got != want {
		t.Errorf("FormatTurn() = %q, want %q", got, want)
	}
}

func TestFormatSessionDelimited(t *testing.T) {
	t.Parallel()

	a := &usage.Account{}
	a.Add(usage.Record{Agent: "main", Usage: llm.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}})

	want := "\n---\nsession tokens: 1 prompt, 1 completion, 2 total"
	if got := usage.FormatSession(a); got != want {
		t.Errorf("FormatSession() = %q, want %q", got, want)
	}
}

func TestFormatStatsLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		records []usage.Record
		want    string
	}{
		{
			name: "zero stats render no stats line",
			records: []usage.Record{
				{Agent: "main", Usage: llm.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}},
			},
			want: "\n---\ntokens: 1 prompt, 2 completion, 3 total",
		},
		{
			name: "full breakdown with rates",
			records: []usage.Record{
				{
					Agent: "main",
					Usage: llm.Usage{CompletionTokens: 80, TotalTokens: 80},
					Stats: llm.CallStats{
						Output:  llm.OutputBytes{Content: 6144, Reasoning: 2048, ToolCalls: 1228},
						Elapsed: 4 * time.Second,
					},
				},
			},
			want: "\n---\ntokens: 0 prompt, 80 completion, 80 total\nstats: 4s, 9.2KB output (6KB text, 2KB reasoning, 1.2KB tools), 20.0 tok/s, 2.3KB/s",
		},
		{
			name: "content-only omits the split",
			records: []usage.Record{
				{
					Agent: "main",
					Usage: llm.Usage{CompletionTokens: 80, TotalTokens: 80},
					Stats: llm.CallStats{
						Output:  llm.OutputBytes{Content: 4096},
						Elapsed: 4 * time.Second,
					},
				},
			},
			want: "\n---\ntokens: 0 prompt, 80 completion, 80 total\nstats: 4s, 4KB output, 20.0 tok/s, 1KB/s",
		},
		{
			name: "reasoning-only split shows just reasoning",
			records: []usage.Record{
				{
					Agent: "main",
					Usage: llm.Usage{CompletionTokens: 40, TotalTokens: 40},
					Stats: llm.CallStats{
						Output:  llm.OutputBytes{Reasoning: 4096},
						Elapsed: 4 * time.Second,
					},
				},
			},
			want: "\n---\ntokens: 0 prompt, 40 completion, 40 total\nstats: 4s, 4KB output (4KB reasoning), 10.0 tok/s, 1KB/s",
		},
		{
			name: "zero elapsed renders no rates",
			records: []usage.Record{
				{
					Agent: "main",
					Usage: llm.Usage{CompletionTokens: 80, TotalTokens: 80},
					Stats: llm.CallStats{Output: llm.OutputBytes{Content: 4096}},
				},
			},
			want: "\n---\ntokens: 0 prompt, 80 completion, 80 total\nstats: 0s, 4KB output",
		},
		{
			name: "sub-second elapsed renders one decimal",
			records: []usage.Record{
				{
					Agent: "main",
					Usage: llm.Usage{CompletionTokens: 40, TotalTokens: 40},
					Stats: llm.CallStats{
						Output:  llm.OutputBytes{Content: 2048},
						Elapsed: 12345 * time.Millisecond,
					},
				},
			},
			want: "\n---\ntokens: 0 prompt, 40 completion, 40 total\nstats: 12.3s, 2KB output, 3.2 tok/s, 165B/s",
		},
		{
			name: "records sum before rates compute",
			records: []usage.Record{
				{
					Agent: "main",
					Usage: llm.Usage{CompletionTokens: 40, TotalTokens: 40},
					Stats: llm.CallStats{Output: llm.OutputBytes{Content: 2048}, Elapsed: time.Second},
				},
				{
					Agent: "worker",
					Usage: llm.Usage{CompletionTokens: 40, TotalTokens: 40},
					Stats: llm.CallStats{Output: llm.OutputBytes{Content: 2048}, Elapsed: 3 * time.Second},
				},
			},
			want: "\n---\ntokens: 0 prompt, 80 completion, 80 total (main: 0/40/40, worker: 0/40/40)\nstats: 4s, 4KB output, 20.0 tok/s, 1KB/s",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a := &usage.Account{}
			for _, rec := range tt.records {
				a.Add(rec)
			}
			if got := usage.FormatTurn(a); got != tt.want {
				t.Errorf("FormatTurn() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatSessionCarriesStatsLine(t *testing.T) {
	t.Parallel()

	a := &usage.Account{}
	a.Add(usage.Record{
		Agent: "main",
		Usage: llm.Usage{PromptTokens: 1, CompletionTokens: 40, TotalTokens: 41},
		Stats: llm.CallStats{
			Output:  llm.OutputBytes{Content: 4096},
			Elapsed: 4 * time.Second,
		},
	})

	want := "\n---\nsession tokens: 1 prompt, 40 completion, 41 total\nstats: 4s, 4KB output, 10.0 tok/s, 1KB/s"
	if got := usage.FormatSession(a); got != want {
		t.Errorf("FormatSession() = %q, want %q", got, want)
	}
}
