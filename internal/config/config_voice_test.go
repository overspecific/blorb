package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/overspecific/blorb/internal/config"
)

// validVoice returns the canonical valid voice block for programmatic
// configs.
func validVoice() *config.VoiceConfig {
	return &config.VoiceConfig{APIKeyEnv: "ASSEMBLYAI_API_KEY"}
}

func TestLoadWithVoice(t *testing.T) {
	cfg, err := loadTestdata(t, "with_voice.json")
	if err != nil {
		t.Fatalf("Load(with_voice.json) error = %v, want nil", err)
	}
	agent := cfg.Agents[0]
	if !agent.VoiceEnabled() {
		t.Fatal("VoiceEnabled() = false, want true")
	}
	v := agent.Voice
	if v == nil {
		t.Fatal("Voice = nil, want non-nil")
	}
	if got := v.APIKeyEnv; got != "ASSEMBLYAI_API_KEY" {
		t.Errorf("APIKeyEnv = %q, want %q", got, "ASSEMBLYAI_API_KEY")
	}
	if got := v.Greeting; got != "Hello there." {
		t.Errorf("Greeting = %q, want %q", got, "Hello there.")
	}
	if got := v.Voice; got != "james" {
		t.Errorf("Voice = %q, want %q", got, "james")
	}
	if v.Volume == nil || *v.Volume != 80 {
		t.Errorf("Volume = %v, want 80", v.Volume)
	}
	if got := v.InputCommandOrDefault(); !slicesEqual(got, []string{"arecord", "-q"}) {
		t.Errorf("InputCommandOrDefault() = %v, want [arecord -q]", got)
	}
	if got := v.OutputCommandOrDefault(); !slicesEqual(got, []string{"aplay", "-q"}) {
		t.Errorf("OutputCommandOrDefault() = %v, want [aplay -q]", got)
	}
	if got := v.WSURLOrDefault(); got != "wss://voice.example.com/ws" {
		t.Errorf("WSURLOrDefault() = %q, want %q", got, "wss://voice.example.com/ws")
	}
}

func TestVoiceAbsent(t *testing.T) {
	cfg, err := loadTestdata(t, "valid.json")
	if err != nil {
		t.Fatalf("Load(valid.json) error = %v, want nil", err)
	}
	if cfg.Agents[0].VoiceEnabled() {
		t.Error("VoiceEnabled() = true, want false when the voice block is absent")
	}
}

func TestVoiceDefaults(t *testing.T) {
	agent := validAgent()
	agent.Voice = &config.VoiceConfig{APIKeyEnv: "ASSEMBLYAI_API_KEY"}
	cfg := config.Config{
		Providers: []config.Provider{validProvider()},
		Models:    []config.Model{validModel()},
		Agents:    []config.Agent{agent},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate error = %v, want nil", err)
	}
	if !cfg.Agents[0].VoiceEnabled() {
		t.Error("VoiceEnabled() = false, want true with a voice block present")
	}
	v := cfg.Agents[0].Voice
	if got := v.InputCommandOrDefault(); !slicesEqual(got, config.DefaultVoiceInputCommand) {
		t.Errorf("InputCommandOrDefault() = %v, want %v", got, config.DefaultVoiceInputCommand)
	}
	if got := v.OutputCommandOrDefault(); !slicesEqual(got, config.DefaultVoiceOutputCommand) {
		t.Errorf("OutputCommandOrDefault() = %v, want %v", got, config.DefaultVoiceOutputCommand)
	}
	if got := v.WSURLOrDefault(); got != config.DefaultVoiceWSURL {
		t.Errorf("WSURLOrDefault() = %q, want %q", got, config.DefaultVoiceWSURL)
	}
}

func TestVoiceExplicitZeroVolume(t *testing.T) {
	agent := validAgent()
	zero := 0
	agent.Voice = &config.VoiceConfig{APIKeyEnv: "ASSEMBLYAI_API_KEY", Volume: &zero}
	cfg := config.Config{
		Providers: []config.Provider{validProvider()},
		Models:    []config.Model{validModel()},
		Agents:    []config.Agent{agent},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate error = %v, want nil for an explicit volume 0", err)
	}
	if v := cfg.Agents[0].Voice; v.Volume == nil || *v.Volume != 0 {
		t.Errorf("Volume = %v, want an explicit 0", v.Volume)
	}
}

func TestVoiceRoundTripAllFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blorb.json")
	data := `{
	  "providers": [{"name": "remote", "type": "openai-compatible", "base_url": "https://api.example.com/v1"}],
	  "models": [{"name": "m", "provider": "remote", "model_name": "m"}],
	  "agents": [{
	    "name": "helper",
	    "system_prompt": "Be helpful.",
	    "max_turns": 1,
	    "model": "m",
	    "voice": {
	      "api_key_env": "KEY_ENV",
	      "greeting": "Hi.",
	      "voice": "james",
	      "volume": 0,
	      "input_command": ["capture"],
	      "output_command": ["play"],
	      "ws_url": "ws://ws.example.com/voice"
	    }
	  }]
	}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load error = %v, want nil", err)
	}
	v := cfg.Agents[0].Voice
	if got, want := v.APIKeyEnv, "KEY_ENV"; got != want {
		t.Errorf("APIKeyEnv = %q, want %q", got, want)
	}
	if got, want := v.Greeting, "Hi."; got != want {
		t.Errorf("Greeting = %q, want %q", got, want)
	}
	if got, want := v.Voice, "james"; got != want {
		t.Errorf("Voice = %q, want %q", got, want)
	}
	if v.Volume == nil || *v.Volume != 0 {
		t.Errorf("Volume = %v, want 0", v.Volume)
	}
	if got, want := v.InputCommandOrDefault(), []string{"capture"}; !slicesEqual(got, want) {
		t.Errorf("InputCommandOrDefault() = %v, want %v", got, want)
	}
	if got, want := v.OutputCommandOrDefault(), []string{"play"}; !slicesEqual(got, want) {
		t.Errorf("OutputCommandOrDefault() = %v, want %v", got, want)
	}
	if got, want := v.WSURLOrDefault(), "ws://ws.example.com/voice"; got != want {
		t.Errorf("WSURLOrDefault() = %q, want %q", got, want)
	}
}

func TestVoiceInvalid(t *testing.T) {
	tests := []struct {
		name  string
		voice *config.VoiceConfig
		want  string
	}{
		{
			name:  "missing api_key_env",
			voice: &config.VoiceConfig{},
			want:  "api_key_env is required",
		},
		{
			name: "volume above range",
			voice: func() *config.VoiceConfig {
				v := validVoice()
				n := 101
				v.Volume = &n
				return v
			}(),
			want: "volume 101 must be between 0 and 100",
		},
		{
			name: "volume below range",
			voice: func() *config.VoiceConfig {
				v := validVoice()
				n := -1
				v.Volume = &n
				return v
			}(),
			want: "volume -1 must be between 0 and 100",
		},
		{
			name: "ws_url with http scheme",
			voice: func() *config.VoiceConfig {
				v := validVoice()
				v.WSURL = "http://example.com"
				return v
			}(),
			want: `ws_url "http://example.com" must use ws or wss scheme`,
		},
		{
			name: "ws_url without host",
			voice: func() *config.VoiceConfig {
				v := validVoice()
				v.WSURL = "wss:///socket"
				return v
			}(),
			want: `ws_url "wss:///socket" must include a host`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := validAgent()
			agent.Voice = tt.voice
			cfg := config.Config{
				Providers: []config.Provider{validProvider()},
				Models:    []config.Model{validModel()},
				Agents:    []config.Agent{agent},
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

// slicesEqual reports whether two string slices are equal, treating nil and
// empty as equal.
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
