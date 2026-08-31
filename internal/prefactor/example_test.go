package prefactor

import (
	"os"
	"path/filepath"
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
	if cfg.Agents[0].Name != "prefactor-tracing" {
		t.Errorf("Agents[0].Name = %q, want prefactor-tracing", cfg.Agents[0].Name)
	}
	if cfg.DefaultAgent != "prefactor-tracing" {
		t.Errorf("DefaultAgent = %q, want prefactor-tracing", cfg.DefaultAgent)
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
// prefactor block, so it runs without any tracing token.
func TestSimpleExampleTracingDisabled(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "simple", "blorb.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("example not present: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load(examples/simple/blorb.json) error = %v, want nil", err)
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1 (single-agent example)", len(cfg.Agents))
	}
	if cfg.Agents[0].Name != "simple" {
		t.Errorf("Agents[0].Name = %q, want simple", cfg.Agents[0].Name)
	}
	if cfg.DefaultAgent != "simple" {
		t.Errorf("DefaultAgent = %q, want simple", cfg.DefaultAgent)
	}
	if cfg.PrefactorEnabled() {
		t.Error("PrefactorEnabled() = true, want false — the simple example should not require a tracing token")
	}
}
