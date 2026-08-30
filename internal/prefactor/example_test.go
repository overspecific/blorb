package prefactor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/overspecific/blorb/internal/config"
)

// TestExampleConfigValid ensures the shipped example blorb.json loads,
// validates, and carries the prefactor block as advertised in its README.
// Living in package prefactor keeps it out of the shipped binaries while
// still running under bin/qc.
func TestExampleConfigValid(t *testing.T) {
	exampleDir := filepath.Join("..", "..", "examples", "simple")
	if _, err := os.Stat(exampleDir); err != nil {
		t.Skipf("example not present: %v", err)
	}

	// The example config resolves its tools' base_dir against the working
	// directory; the example README documents running from the repo root.
	t.Chdir(filepath.Join("..", ".."))

	cfg, err := config.Load(filepath.Join("examples", "simple", "blorb.json"))
	if err != nil {
		t.Fatalf("Load(examples/simple/blorb.json) error = %v, want nil", err)
	}
	if cfg.Name != "simple" {
		t.Errorf("Name = %q, want simple", cfg.Name)
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
