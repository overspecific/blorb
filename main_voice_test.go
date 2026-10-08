package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/voice"
)

// writeVoiceConfig writes a blorb.json with an agent pointing at the fake voice
// endpoint, or no voice block when voice is nil.
func writeVoiceConfig(t *testing.T, wsURL string, voice map[string]any) string {
	t.Helper()
	agent := map[string]any{
		"name": "helper", "system_prompt": "You are helpful.", "model": "m", "max_turns": 1,
	}
	if voice != nil {
		agent["voice"] = voice
	}
	cfg := map[string]any{
		"providers":     []map[string]any{{"name": "local", "type": "openai-compatible", "base_url": "http://localhost:1"}},
		"models":        []map[string]any{{"name": "m", "provider": "local", "model_name": "m"}},
		"agents":        []map[string]any{agent},
		"default_agent": "helper",
	}
	if voice != nil {
		if _, ok := voice["ws_url"]; !ok {
			voice["ws_url"] = wsURL
		}
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "blorb.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// stubVoiceRun replaces voiceRun for the duration of one test, capturing the
// options it received.
func stubVoiceRun(t *testing.T, capture *voice.Options, err error) {
	t.Helper()
	orig := voiceRun
	voiceRun = func(_ context.Context, opts voice.Options) error {
		*capture = opts
		return err
	}
	t.Cleanup(func() { voiceRun = orig })
}

func TestVoiceCommandInvokesSession(t *testing.T) {
	path := writeVoiceConfig(t, "ws://voice.example/ws", map[string]any{
		"api_key_env": "ASSEMBLYAI_API_KEY",
		"greeting":    "hi",
	})
	t.Setenv("ASSEMBLYAI_API_KEY", "secret")

	var captured voice.Options
	stubVoiceRun(t, &captured, nil)

	cmd := rootCommand()
	cmd.Writer = io.Discard
	cmd.ErrWriter = io.Discard
	cmd.ExitErrHandler = capturingExitHandler(io.Discard)
	if err := cmd.Run(context.Background(), []string{"blorb", "voice", "-c", path}); err != nil {
		t.Fatalf("voice command error = %v, want nil", err)
	}
	if captured.Agent.Name != "helper" {
		t.Errorf("Agent.Name = %q, want helper", captured.Agent.Name)
	}
	if captured.APIKey != "secret" {
		t.Errorf("APIKey = %q, want secret", captured.APIKey)
	}
	if captured.NoMic {
		t.Error("NoMic = true, want false without the flag")
	}
}

func TestVoiceCommandNoMicFlag(t *testing.T) {
	path := writeVoiceConfig(t, "ws://voice.example/ws", map[string]any{"api_key_env": "ASSEMBLYAI_API_KEY"})
	t.Setenv("ASSEMBLYAI_API_KEY", "secret")

	var captured voice.Options
	stubVoiceRun(t, &captured, nil)

	cmd := rootCommand()
	cmd.Writer = io.Discard
	cmd.ErrWriter = io.Discard
	cmd.ExitErrHandler = capturingExitHandler(io.Discard)
	if err := cmd.Run(context.Background(), []string{"blorb", "voice", "-c", path, "--no-mic"}); err != nil {
		t.Fatalf("voice command error = %v, want nil", err)
	}
	if !captured.NoMic {
		t.Error("NoMic = false, want true with --no-mic")
	}
}

func TestVoiceCommandMissingSection(t *testing.T) {
	path := writeVoiceConfig(t, "", nil)

	errOut := &bytes.Buffer{}
	cmd := rootCommand()
	cmd.Writer = io.Discard
	cmd.ErrWriter = errOut
	cmd.ExitErrHandler = capturingExitHandler(errOut)
	if err := cmd.Run(context.Background(), []string{"blorb", "voice", "-c", path}); err == nil {
		t.Fatal("voice without a voice section succeeded, want an error")
	}
	if !strings.Contains(errOut.String(), "has no voice section") {
		t.Errorf("error = %q, want the missing-voice-section message", errOut.String())
	}
	if !strings.Contains(errOut.String(), "helper") {
		t.Errorf("error = %q, want the agent name", errOut.String())
	}
}

func TestVoiceCommandMissingAPIKey(t *testing.T) {
	path := writeVoiceConfig(t, "ws://voice.example/ws", map[string]any{"api_key_env": "ASSEMBLYAI_API_KEY"})
	t.Setenv("ASSEMBLYAI_API_KEY", "")

	errOut := &bytes.Buffer{}
	cmd := rootCommand()
	cmd.Writer = io.Discard
	cmd.ErrWriter = errOut
	cmd.ExitErrHandler = capturingExitHandler(errOut)
	if err := cmd.Run(context.Background(), []string{"blorb", "voice", "-c", path}); err == nil {
		t.Fatal("voice without an API key succeeded, want an error")
	}
	if !strings.Contains(errOut.String(), "ASSEMBLYAI_API_KEY") {
		t.Errorf("error = %q, want the api_key_env name", errOut.String())
	}
}

func TestVoiceExampleConfigValidates(t *testing.T) {
	if _, err := config.Load("examples/voice/blorb.json"); err != nil {
		t.Fatalf("Load(examples/voice/blorb.json) error = %v, want nil", err)
	}
}
