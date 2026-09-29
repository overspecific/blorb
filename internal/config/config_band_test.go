package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/overspecific/blorb/internal/config"
)

// validBand returns the canonical valid band block for programmatic
// configs.
func validBand() *config.BandConfig {
	return &config.BandConfig{AgentID: "agent-uuid"}
}

func TestLoadWithBand(t *testing.T) {
	cfg, err := loadTestdata(t, "with_band.json")
	if err != nil {
		t.Fatalf("Load(with_band.json) error = %v, want nil", err)
	}
	if !cfg.BandEnabled() {
		t.Fatal("BandEnabled() = false, want true")
	}
	b := cfg.Band
	if b == nil {
		t.Fatal("Band = nil, want non-nil")
	}
	if b.AgentID != "agent-123" {
		t.Errorf("AgentID = %q, want %q", b.AgentID, "agent-123")
	}
	if got := b.APIKeyEnvOrDefault(); got != "MY_BAND_KEY" {
		t.Errorf("APIKeyEnvOrDefault() = %q, want %q", got, "MY_BAND_KEY")
	}
	if got := b.RESTURLOrDefault(); got != "https://band.example.com" {
		t.Errorf("RESTURLOrDefault() = %q, want %q", got, "https://band.example.com")
	}
	if got := b.WSURLOrDefault(); got != "wss://band.example.com/socket" {
		t.Errorf("WSURLOrDefault() = %q, want %q", got, "wss://band.example.com/socket")
	}
}

func TestBandAbsent(t *testing.T) {
	cfg, err := loadTestdata(t, "valid.json")
	if err != nil {
		t.Fatalf("Load(valid.json) error = %v, want nil", err)
	}
	if cfg.BandEnabled() {
		t.Error("BandEnabled() = true, want false when the band block is absent")
	}
}

func TestBandDefaults(t *testing.T) {
	cfg := config.Config{
		Providers: []config.Provider{validProvider()},
		Models:    []config.Model{validModel()},
		Agents:    []config.Agent{validAgent()},
		Band:      &config.BandConfig{AgentID: "agent-uuid"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate error = %v, want nil", err)
	}
	if !cfg.BandEnabled() {
		t.Error("BandEnabled() = false, want true with a band block present")
	}
	if got := cfg.Band.APIKeyEnvOrDefault(); got != config.DefaultBandAPIKeyEnv {
		t.Errorf("APIKeyEnvOrDefault() = %q, want %q", got, config.DefaultBandAPIKeyEnv)
	}
	if got := cfg.Band.RESTURLOrDefault(); got != config.DefaultBandRESTURL {
		t.Errorf("RESTURLOrDefault() = %q, want %q", got, config.DefaultBandRESTURL)
	}
	if got := cfg.Band.WSURLOrDefault(); got != config.DefaultBandWSURL {
		t.Errorf("WSURLOrDefault() = %q, want %q", got, config.DefaultBandWSURL)
	}
}

func TestBandRoundTripAllFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blorb.json")
	data := `{
	  "providers": [{"name": "remote", "type": "openai-compatible", "base_url": "https://api.example.com/v1"}],
	  "models": [{"name": "m", "provider": "remote", "model_name": "m"}],
	  "agents": [{"name": "helper", "system_prompt": "Be helpful.", "max_turns": 1, "model": "m"}],
	  "band": {
	    "agent_id": "uuid-1",
	    "api_key_env": "KEY_ENV",
	    "rest_url": "https://rest.example.com",
	    "ws_url": "ws://ws.example.com/socket"
	  }
	}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load error = %v, want nil", err)
	}
	if got, want := cfg.Band.AgentID, "uuid-1"; got != want {
		t.Errorf("AgentID = %q, want %q", got, want)
	}
	if got, want := cfg.Band.APIKeyEnvOrDefault(), "KEY_ENV"; got != want {
		t.Errorf("APIKeyEnvOrDefault() = %q, want %q", got, want)
	}
	if got, want := cfg.Band.RESTURLOrDefault(), "https://rest.example.com"; got != want {
		t.Errorf("RESTURLOrDefault() = %q, want %q", got, want)
	}
	if got, want := cfg.Band.WSURLOrDefault(), "ws://ws.example.com/socket"; got != want {
		t.Errorf("WSURLOrDefault() = %q, want %q", got, want)
	}
}

func TestBandInvalid(t *testing.T) {
	tests := []struct {
		name string
		band *config.BandConfig
		want string
	}{
		{
			name: "missing agent_id",
			band: &config.BandConfig{},
			want: "agent_id is required",
		},
		{
			name: "empty api_key_env",
			band: func() *config.BandConfig {
				b := validBand()
				empty := ""
				b.APIKeyEnv = &empty
				return b
			}(),
			want: "api_key_env must not be empty when set",
		},
		{
			name: "rest_url with ws scheme",
			band: func() *config.BandConfig {
				b := validBand()
				b.RESTURL = "ws://example.com"
				return b
			}(),
			want: `rest_url "ws://example.com" must use http or https scheme`,
		},
		{
			name: "ws_url with http scheme",
			band: func() *config.BandConfig {
				b := validBand()
				b.WSURL = "http://example.com"
				return b
			}(),
			want: `ws_url "http://example.com" must use ws or wss scheme`,
		},
		{
			name: "rest_url without host",
			band: func() *config.BandConfig {
				b := validBand()
				b.RESTURL = "https:///api"
				return b
			}(),
			want: `rest_url "https:///api" must include a host`,
		},
		{
			name: "ws_url without host",
			band: func() *config.BandConfig {
				b := validBand()
				b.WSURL = "wss:///socket"
				return b
			}(),
			want: `ws_url "wss:///socket" must include a host`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Config{
				Providers: []config.Provider{validProvider()},
				Models:    []config.Model{validModel()},
				Agents:    []config.Agent{validAgent()},
				Band:      tt.band,
			}
			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate error = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}
