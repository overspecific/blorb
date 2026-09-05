package usage_test

import (
	"testing"
	"time"

	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/usage"
)

func TestRates(t *testing.T) {
	t.Parallel()

	t.Run("empty account", func(t *testing.T) {
		t.Parallel()

		a := &usage.Account{}
		if got := a.TokensPerSec(); got != 0 {
			t.Errorf("TokensPerSec() = %v, want 0", got)
		}
		if got := a.BytesPerSec(); got != 0 {
			t.Errorf("BytesPerSec() = %v, want 0", got)
		}
	})

	t.Run("clean division", func(t *testing.T) {
		t.Parallel()

		a := &usage.Account{}
		a.Add(usage.Record{
			Agent: "main",
			Usage: llm.Usage{CompletionTokens: 100},
			Stats: llm.CallStats{
				Output:  llm.OutputBytes{Content: 400, Reasoning: 100},
				Elapsed: 2 * time.Second,
			},
		})
		if got := a.TokensPerSec(); got != 50 {
			t.Errorf("TokensPerSec() = %v, want 50", got)
		}
		if got := a.BytesPerSec(); got != 250 {
			t.Errorf("BytesPerSec() = %v, want 250", got)
		}
	})

	t.Run("zero elapsed guards", func(t *testing.T) {
		t.Parallel()

		a := &usage.Account{}
		a.Add(usage.Record{
			Agent: "main",
			Usage: llm.Usage{CompletionTokens: 100},
			Stats: llm.CallStats{Output: llm.OutputBytes{Content: 400}},
		})
		if got := a.TokensPerSec(); got != 0 {
			t.Errorf("TokensPerSec() = %v, want 0 (no time measured)", got)
		}
		if got := a.BytesPerSec(); got != 0 {
			t.Errorf("BytesPerSec() = %v, want 0 (no time measured)", got)
		}
	})

	t.Run("records sum before dividing", func(t *testing.T) {
		t.Parallel()

		// 100 tokens over 1s plus 100 over 3s is 200/4s = 50/sec —
		// the summed rate, not a mean of per-call rates (100 and
		// 33.3 would average 66.7 and over-weight the fast call).
		a := &usage.Account{}
		a.Add(usage.Record{
			Agent: "main",
			Usage: llm.Usage{CompletionTokens: 100},
			Stats: llm.CallStats{Output: llm.OutputBytes{Content: 100}, Elapsed: time.Second},
		})
		a.Add(usage.Record{
			Agent: "worker",
			Usage: llm.Usage{CompletionTokens: 100},
			Stats: llm.CallStats{Output: llm.OutputBytes{Content: 300}, Elapsed: 3 * time.Second},
		})
		if got := a.TokensPerSec(); got != 50 {
			t.Errorf("TokensPerSec() = %v, want 50", got)
		}
		if got := a.BytesPerSec(); got != 100 {
			t.Errorf("BytesPerSec() = %v, want 100", got)
		}
	})
}
