package usage_test

import (
	"strings"
	"testing"

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
