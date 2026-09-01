package prefactor

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	for _, name := range []string{"read", "grep", "current_time"} {
		if slices.Contains(simple.Tools, name) {
			t.Errorf("agent simple tools = %v, want it to exclude %q (specialists own those domains)", simple.Tools, name)
		}
	}
	if got, want := scholar.Tools, []string{"read", "grep"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("scholar.Tools = %v, want %v (the scholar needs no echo or clock)", got, want)
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
