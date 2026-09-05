package usage_test

import (
	"testing"
	"time"

	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/usage"
)

func TestEmptyAccount(t *testing.T) {
	t.Parallel()

	a := &usage.Account{}

	if got := a.Records(); len(got) != 0 {
		t.Errorf("Records() = %v, want empty", got)
	}
	if got := a.AgentTotals(); len(got) != 0 {
		t.Errorf("AgentTotals() = %v, want empty", got)
	}
	if got := a.Total(); got != (llm.Usage{}) {
		t.Errorf("Total() = %v, want zero", got)
	}
}

func TestSingleRecord(t *testing.T) {
	t.Parallel()

	want := usage.Record{
		Agent: "main",
		Model: "gpt-test",
		Usage: llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
	a := &usage.Account{}
	a.Add(want)

	got := a.Records()
	if len(got) != 1 || got[0] != want {
		t.Errorf("Records() = %v, want [%v]", got, want)
	}
	if got := a.Total(); got != want.Usage {
		t.Errorf("Total() = %v, want %v", got, want.Usage)
	}
}

func TestAgentTotalsSumAndSort(t *testing.T) {
	t.Parallel()

	a := &usage.Account{}
	a.Add(usage.Record{
		Agent: "main",
		Usage: llm.Usage{PromptTokens: 100, CompletionTokens: 30, TotalTokens: 130},
	})
	a.Add(usage.Record{
		Agent: "worker",
		Usage: llm.Usage{PromptTokens: 23, CompletionTokens: 56, TotalTokens: 79},
	})
	a.Add(usage.Record{
		Agent: "main",
		Usage: llm.Usage{PromptTokens: 10, CompletionTokens: 10, TotalTokens: 20},
	})

	totals := a.AgentTotals()
	want := []usage.AgentTotal{
		{Agent: "main", Usage: llm.Usage{PromptTokens: 110, CompletionTokens: 40, TotalTokens: 150}},
		{Agent: "worker", Usage: llm.Usage{PromptTokens: 23, CompletionTokens: 56, TotalTokens: 79}},
	}
	if len(totals) != len(want) {
		t.Fatalf("AgentTotals() = %v, want %v", totals, want)
	}
	for i, w := range want {
		if totals[i] != w {
			t.Errorf("AgentTotals()[%d] = %v, want %v", i, totals[i], w)
		}
	}

	if got, wantTotal := a.Total(), (llm.Usage{PromptTokens: 133, CompletionTokens: 96, TotalTokens: 229}); got != wantTotal {
		t.Errorf("Total() = %v, want %v", got, wantTotal)
	}
}

func TestStatsSum(t *testing.T) {
	t.Parallel()

	mainStats := llm.CallStats{
		Output:  llm.OutputBytes{Content: 100, Reasoning: 50, ToolCalls: 25},
		Elapsed: 3 * time.Second,
	}
	workerStats := llm.CallStats{
		Output:  llm.OutputBytes{Content: 10, Reasoning: 5, ToolCalls: 2},
		Elapsed: time.Second,
	}
	a := &usage.Account{}
	a.Add(usage.Record{Agent: "main", Usage: llm.Usage{PromptTokens: 100, CompletionTokens: 30, TotalTokens: 130}, Stats: mainStats})
	a.Add(usage.Record{Agent: "worker", Usage: llm.Usage{PromptTokens: 23, CompletionTokens: 56, TotalTokens: 79}, Stats: workerStats})
	a.Add(usage.Record{Agent: "main", Usage: llm.Usage{PromptTokens: 10, CompletionTokens: 10, TotalTokens: 20}, Stats: mainStats})

	totals := a.AgentTotals()
	want := []usage.AgentTotal{
		{
			Agent: "main",
			Usage: llm.Usage{PromptTokens: 110, CompletionTokens: 40, TotalTokens: 150},
			Stats: llm.CallStats{
				Output:  llm.OutputBytes{Content: 200, Reasoning: 100, ToolCalls: 50},
				Elapsed: 6 * time.Second,
			},
		},
		{
			Agent: "worker",
			Usage: llm.Usage{PromptTokens: 23, CompletionTokens: 56, TotalTokens: 79},
			Stats: workerStats,
		},
	}
	if len(totals) != len(want) {
		t.Fatalf("AgentTotals() = %v, want %v", totals, want)
	}
	for i, w := range want {
		if totals[i] != w {
			t.Errorf("AgentTotals()[%d] = %+v, want %+v", i, totals[i], w)
		}
	}

	wantTotalStats := llm.CallStats{
		Output:  llm.OutputBytes{Content: 210, Reasoning: 105, ToolCalls: 52},
		Elapsed: 7 * time.Second,
	}
	if got := a.TotalStats(); got != wantTotalStats {
		t.Errorf("TotalStats() = %+v, want %+v", got, wantTotalStats)
	}
}

func TestZeroStatsPreservedAndEmptyAccountTotalStats(t *testing.T) {
	t.Parallel()

	// Zero-stats records sum to zero, they are not dropped.
	a := &usage.Account{}
	a.Add(usage.Record{Agent: "main", Usage: llm.Usage{TotalTokens: 1}})
	a.Add(usage.Record{Agent: "worker", Usage: llm.Usage{TotalTokens: 2}})

	if got := a.TotalStats(); got != (llm.CallStats{}) {
		t.Errorf("TotalStats() = %+v, want zero", got)
	}
	totals := a.AgentTotals()
	for i, w := range []llm.CallStats{{}, {}} {
		if totals[i].Stats != w {
			t.Errorf("AgentTotals()[%d].Stats = %+v, want zero", i, totals[i].Stats)
		}
	}

	// An empty account's TotalStats is zero, not an error.
	if got := (&usage.Account{}).TotalStats(); got != (llm.CallStats{}) {
		t.Errorf("empty TotalStats() = %+v, want zero", got)
	}
}

func TestRecordsReturnsCopy(t *testing.T) {
	t.Parallel()

	a := &usage.Account{}
	a.Add(usage.Record{Agent: "main", Usage: llm.Usage{TotalTokens: 1}})

	got := a.Records()
	got[0].Agent = "mutated"
	got = append(got, usage.Record{Agent: "extra"})
	if len(got) != 2 {
		t.Errorf("mutated copy = %v, want 2 records", got)
	}

	again := a.Records()
	if len(again) != 1 || again[0].Agent != "main" {
		t.Errorf("Records() after mutation = %v, want original single record", again)
	}
}

func TestZeroUsageRecordPreserved(t *testing.T) {
	t.Parallel()

	a := &usage.Account{}
	a.Add(usage.Record{Agent: "main", Usage: llm.Usage{}})
	a.Add(usage.Record{
		Agent: "worker",
		Usage: llm.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	})

	records := a.Records()
	if len(records) != 2 {
		t.Errorf("Records() = %v, want 2 records including the zero-usage one", records)
	}

	totals := a.AgentTotals()
	want := []usage.AgentTotal{
		{Agent: "main", Usage: llm.Usage{}},
		{Agent: "worker", Usage: llm.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}},
	}
	if len(totals) != len(want) {
		t.Fatalf("AgentTotals() = %v, want %v", totals, want)
	}
	for i, w := range want {
		if totals[i] != w {
			t.Errorf("AgentTotals()[%d] = %v, want %v", i, totals[i], w)
		}
	}
}
