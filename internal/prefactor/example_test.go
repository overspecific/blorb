package prefactor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/overspecific/blorb/internal/config"
)

// TestPrefactorTracingExampleConfigValid ensures the shipped example
// blorb.json loads, validates, and carries the prefactor block as advertised
// in its README. Living in package prefactor keeps it out of the shipped
// binaries while still running under bin/qc.
func TestPrefactorTracingExampleConfigValid(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "prefactor-tracing", "blorb.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("example not present: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load(examples/prefactor-tracing/blorb.json) error = %v, want nil", err)
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1 (single-agent example)", len(cfg.Agents))
	}
	tracey, ok := cfg.Agent("tracey")
	if !ok {
		t.Fatalf("Agent(tracey) missing; agents = %v", cfg.Agents)
	}
	if tracey.Name != "tracey" {
		t.Errorf("Agent(tracey).Name = %q, want tracey", tracey.Name)
	}
	if cfg.DefaultAgent != "tracey" {
		t.Errorf("DefaultAgent = %q, want tracey", cfg.DefaultAgent)
	}
	if !cfg.PrefactorEnabled() {
		t.Error("PrefactorEnabled() = false, want true — the example documents a prefactor block")
	}
	if got := cfg.Prefactor.APITokenEnvOrDefault(); got != "PREFACTOR_API_TOKEN" {
		t.Errorf("APITokenEnvOrDefault() = %q, want PREFACTOR_API_TOKEN", got)
	}
	if got := cfg.Prefactor.APIURLOrDefault(); got != config.DefaultPrefactorAPIURL {
		t.Errorf("APIURLOrDefault() = %q, want the default", got)
	}
}

// TestSimpleExampleTracingDisabled ensures the simple example stays free of a
// prefactor block, so it runs without any tracing token, and shows the
// shared-tools story: two agents granted overlapping subsets of one shared
// tool set, with simple as the default.
func TestSimpleExampleTracingDisabled(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "simple", "blorb.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("example not present: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load(examples/simple/blorb.json) error = %v, want nil", err)
	}
	simple, ok := cfg.Agent("simple")
	if !ok {
		t.Fatalf("Agent(simple) missing; agents = %v", cfg.Agents)
	}
	scholar, ok := cfg.Agent("scholar")
	if !ok {
		t.Fatalf("Agent(scholar) missing; agents = %v", cfg.Agents)
	}
	if cfg.DefaultAgent != "simple" {
		t.Errorf("DefaultAgent = %q, want simple — ./blorb chat must run exactly as before", cfg.DefaultAgent)
	}
	// simple gets the utilities and the subagent delegations (no direct
	// knowledgebase or clock access); the specialists own their domains.
	for _, name := range []string{"echo", "ask_scholar", "ask_horologist"} {
		if !slices.Contains(simple.Tools, name) {
			t.Errorf("agent simple tools = %v, want it to include %q", simple.Tools, name)
		}
	}
	for _, name := range []string{"read", "grep", "current_time", "search"} {
		if slices.Contains(simple.Tools, name) {
			t.Errorf("agent simple tools = %v, want it to exclude %q (specialists own those domains)", simple.Tools, name)
		}
	}
	if got, want := scholar.Tools, []string{"kb", "search"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("scholar.Tools = %v, want %v (the scholar delegates its digging to the search agent)", got, want)
	}
	// Granting the kb builtin toolset expands to its prefixed members, in
	// the toolset's declared order, before the explicitly listed search.
	if got, want := toolNames(cfg.AgentTools(scholar)), []string{"kb-read", "kb-grep", "search"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("AgentTools(scholar) = %v, want %v", got, want)
	}
	search, ok := cfg.Agent("search")
	if !ok {
		t.Fatalf("Agent(search) missing; agents = %v", cfg.Agents)
	}
	if got, want := search.Tools, []string{"kb"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("search.Tools = %v, want %v (the search agent needs the kb tools, nothing else)", got, want)
	}
	if got, want := toolNames(cfg.AgentTools(search)), []string{"kb-read", "kb-grep"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("AgentTools(search) = %v, want %v", got, want)
	}
	horologist, ok := cfg.Agent("horologist")
	if !ok {
		t.Fatalf("Agent(horologist) missing; agents = %v", cfg.Agents)
	}
	if got, want := horologist.Tools, []string{"current_time", "calendar", "days_until"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("horologist.Tools = %v, want %v (the horologist needs no echo or knowledgebase)", got, want)
	}
	if cfg.PrefactorEnabled() {
		t.Error("PrefactorEnabled() = true, want false — the simple example should not require a tracing token")
	}
}

// TestPlaudExampleConfigValid ensures the shipped Plaud example blorb.json
// loads, validates, and grants the plaud agent exactly the two Plaud CLI
// command tools it documents plus the summarize subagent tool delegating to
// the summarizer agent.
func TestPlaudExampleConfigValid(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "plaud", "blorb.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("example not present: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load(examples/plaud/blorb.json) error = %v, want nil", err)
	}
	plaud, ok := cfg.Agent("plaud")
	if !ok {
		t.Fatalf("Agent(plaud) missing; agents = %v", cfg.Agents)
	}
	if cfg.DefaultAgent != "plaud" {
		t.Errorf("DefaultAgent = %q, want plaud", cfg.DefaultAgent)
	}
	if got, want := plaud.Tools, []string{"files", "transcript", "summarize"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("plaud.Tools = %v, want %v (the agent owns both Plaud CLI tools and the summarize subagent)", got, want)
	}
	// Both CLI entries are command tools wrapping the plaud CLI; the third
	// delegates to the summarizer agent.
	for _, entry := range cfg.AgentTools(plaud) {
		switch entry.Name {
		case "files", "transcript":
			if entry.Type != config.ToolTypeCommand {
				t.Errorf("tool %q type = %q, want %q", entry.Name, entry.Type, config.ToolTypeCommand)
			}
		case "summarize":
			if entry.Type != config.ToolTypeSubagent {
				t.Errorf("tool %q type = %q, want %q", entry.Name, entry.Type, config.ToolTypeSubagent)
			}
			if entry.Agent != "summarizer" {
				t.Errorf("tool summarize agent = %q, want summarizer", entry.Agent)
			}
			// The custom schema hands the subagent a file_id instead of
			// the default prompt string.
			var schema struct {
				Required []string `json:"required"`
			}
			if err := json.Unmarshal(entry.ArgsSchema, &schema); err != nil {
				t.Fatalf("summarize args_schema unmarshal error = %v, want nil", err)
			}
			if fmt.Sprint(schema.Required) != fmt.Sprint([]string{"file_id"}) {
				t.Errorf("summarize args_schema required = %v, want [file_id]", schema.Required)
			}
		}
	}
	// The summarizer is granted just the transcript tool: it receives the
	// file ID in its user message, so it never lists files itself.
	summarizer, ok := cfg.Agent("summarizer")
	if !ok {
		t.Fatal("Agent(summarizer) missing; the summarize subagent tool targets it")
	}
	if got, want := summarizer.Tools, []string{"transcript"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("summarizer.Tools = %v, want %v", got, want)
	}
	// Its documented output is a bare JSON object, not markdown.
	if !strings.Contains(summarizer.SystemPrompt, "JSON object") {
		t.Error("summarizer system prompt does not instruct JSON object output")
	}
	// Its dates and times are pinned to fixed ISO 8601 forms.
	if !strings.Contains(summarizer.SystemPrompt, "YYYY-MM-DD") {
		t.Error("summarizer system prompt does not pin ISO 8601 date format YYYY-MM-DD")
	}
	if cfg.PrefactorEnabled() {
		t.Error("PrefactorEnabled() = true, want false — the plaud example should not require a tracing token")
	}
}

// toolNames returns the granted entry names in order.
func toolNames(entries []config.ToolEntry) []string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	return names
}

// TestDecisionExampleConfigValid ensures the shipped decision example
// blorb.json loads, validates, and carries the decider and decider tool it
// documents.
func TestDecisionExampleConfigValid(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "decision", "blorb.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("example not present: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load(examples/decision/blorb.json) error = %v, want nil", err)
	}
	if cfg.DefaultAgent != "triage" {
		t.Errorf("DefaultAgent = %q, want triage", cfg.DefaultAgent)
	}
	decider, ok := cfg.Decider("triage")
	if !ok {
		t.Fatalf("Decider(triage) missing; deciders = %v", cfg.Deciders)
	}
	if decider.Model != "jev" {
		t.Errorf("decider model = %q, want jev", decider.Model)
	}
	jev, ok := cfg.Model("jev")
	if !ok {
		t.Fatal("Model(jev) missing")
	}
	if jev.ResolvedModelType() != config.ModelTypeDecision {
		t.Errorf("jev model_type = %q, want decision", jev.ResolvedModelType())
	}
	if len(decider.Questions) != 2 {
		t.Fatalf("decider questions = %d, want 2", len(decider.Questions))
	}
	if got := decider.Questions["department"].Type; got != config.QuestionTypeChoice {
		t.Errorf("department question type = %q, want choice", got)
	}
	if got := decider.Questions["refund_requested"].Type; got != config.QuestionTypeNoul {
		t.Errorf("refund_requested question type = %q, want noul", got)
	}
	// The agent is granted the decider tool.
	if got, want := toolNames(cfg.AgentTools(mustExampleAgent(t, cfg, "triage"))), []string{"triage_ticket"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("triage tools = %v, want %v", got, want)
	}
	for _, entry := range cfg.AgentTools(mustExampleAgent(t, cfg, "triage")) {
		if entry.Type != config.ToolTypeDecider {
			t.Errorf("tool %q type = %q, want decider", entry.Name, entry.Type)
		}
		if entry.Decider != "triage" {
			t.Errorf("tool %q decider = %q, want triage", entry.Name, entry.Decider)
		}
	}
	if cfg.PrefactorEnabled() {
		t.Error("PrefactorEnabled() = true, want false — the decision example should not require a tracing token")
	}
}

// mustExampleAgent resolves a named agent or fails the test.
func mustExampleAgent(t *testing.T, cfg config.Config, name string) config.Agent {
	t.Helper()
	a, ok := cfg.Agent(name)
	if !ok {
		t.Fatalf("Agent(%q) missing", name)
	}
	return a
}
