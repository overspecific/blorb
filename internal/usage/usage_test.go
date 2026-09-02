package usage_test

import (
	"testing"

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
