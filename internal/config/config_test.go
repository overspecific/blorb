package config_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/overspecific/blorb/internal/config"
)

func loadTestdata(t *testing.T, name string) (config.Config, error) {
	t.Helper()
	return config.Load(filepath.Join("testdata", name))
}

func TestLoadValid(t *testing.T) {
	cfg, err := loadTestdata(t, "valid.json")
	if err != nil {
		t.Fatalf("Load(valid.json) error = %v, want nil", err)
	}

	if cfg.Name != "helper" {
		t.Errorf("Name = %q, want %q", cfg.Name, "helper")
	}
	if cfg.SystemPrompt != "You are helpful." {
		t.Errorf("SystemPrompt = %q, want %q", cfg.SystemPrompt, "You are helpful.")
	}
	if cfg.Provider.Type != config.ProviderTypeOpenAI {
		t.Errorf("Provider.Type = %q, want %q", cfg.Provider.Type, config.ProviderTypeOpenAI)
	}
	if cfg.Provider.Model != "gpt-4o-mini" {
		t.Errorf("Provider.Model = %q, want %q", cfg.Provider.Model, "gpt-4o-mini")
	}
	if cfg.Provider.BaseURL != "https://api.example.com/v1" {
		t.Errorf("Provider.BaseURL = %q, want %q", cfg.Provider.BaseURL, "https://api.example.com/v1")
	}
	if cfg.MaxTurns != 5 {
		t.Errorf("MaxTurns = %d, want 5", cfg.MaxTurns)
	}
	if got, want := cfg.MaxTurnsOrDefault(), 5; got != want {
		t.Errorf("MaxTurnsOrDefault() = %d, want %d", got, want)
	}
	if len(cfg.Tools) != 2 {
		t.Fatalf("len(Tools) = %d, want 2", len(cfg.Tools))
	}

	first := cfg.Tools[0]
	if first.Name != "list_files" {
		t.Errorf("Tools[0].Name = %q, want %q", first.Name, "list_files")
	}
	if first.Description != "List files in a directory." {
		t.Errorf("Tools[0].Description = %q, want %q", first.Description, "List files in a directory.")
	}
	if len(first.Command) != 3 || first.Command[0] != "ls" || first.Command[1] != "-la" || first.Command[2] != "." {
		t.Errorf("Tools[0].Command = %v, want [ls -la .]", first.Command)
	}
	if len(first.ArgsSchema) != 0 {
		t.Errorf("Tools[0].ArgsSchema = %s, want empty", first.ArgsSchema)
	}
	if len(cfg.Tools[1].ArgsSchema) == 0 || !json.Valid(cfg.Tools[1].ArgsSchema) {
		t.Fatalf("Tools[1].ArgsSchema = %s, want valid JSON", cfg.Tools[1].ArgsSchema)
	}
	var schema map[string]any
	if err := json.Unmarshal(cfg.Tools[1].ArgsSchema, &schema); err != nil {
		t.Fatalf("unmarshal ArgsSchema: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("Tools[1].ArgsSchema type = %v, want object", schema["type"])
	}
}

func TestLoadMaxTurnsDefault(t *testing.T) {
	cfg, err := loadTestdata(t, "with_api_key_env.json")
	if err != nil {
		t.Fatalf("Load error = %v, want nil", err)
	}
	if cfg.Provider.APIKeyEnv != "MY_API_KEY" {
		t.Errorf("APIKeyEnv = %q, want %q", cfg.Provider.APIKeyEnv, "MY_API_KEY")
	}
	if cfg.MaxTurns != 5 {
		t.Errorf("MaxTurns = %d, want 5", cfg.MaxTurns)
	}
	if got, want := cfg.MaxTurnsOrDefault(), 5; got != want {
		t.Errorf("MaxTurnsOrDefault() = %d, want %d", got, want)
	}
}

func TestMaxTurnsOrDefaultProgrammatic(t *testing.T) {
	cfg := config.Config{}
	if got := cfg.MaxTurnsOrDefault(); got != config.DefaultMaxTurns {
		t.Errorf("MaxTurnsOrDefault() = %d, want %d for zero value", got, config.DefaultMaxTurns)
	}

	cfg.MaxTurns = 7
	if got := cfg.MaxTurnsOrDefault(); got != 7 {
		t.Errorf("MaxTurnsOrDefault() = %d, want 7", got)
	}
}

func TestLoadRejects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		file     string
		errParts []string
	}{
		{"missing_name.json", []string{"name is required"}},
		{"missing_system_prompt.json", []string{"system_prompt is required"}},
		{"missing_provider_model.json", []string{"model is required"}},
		{"missing_provider_base_url.json", []string{"base_url"}},
		{"unknown_provider_type.json", []string{"unknown type", "anthropic", "openai"}},
		{"bad_base_url_scheme.json", []string{"ftp", "http or https"}},
		{"tool_missing_name.json", []string{"name is required"}},
		{"tool_missing_description.json", []string{"description is required"}},
		{"tool_missing_command.json", []string{"command is required"}},
		{"tool_bad_name.json", []string{"must match"}},
		{"duplicate_tool_names.json", []string{"duplicate tool name"}},
		{"unknown_top_level_field.json", []string{"unknown_field"}},
		{"negative_max_turns.json", []string{"max_turns"}},
		{"zero_max_turns.json", []string{"max_turns must be at least 1 (got 0)"}},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			_, err := loadTestdata(t, tc.file)
			if err == nil {
				t.Fatalf("Load(%s) succeeded, want error", tc.file)
			}
			for _, part := range tc.errParts {
				if !contains(err.Error(), part) {
					t.Errorf("Load(%s) error = %q, want it to contain %q", tc.file, err.Error(), part)
				}
			}
		})
	}
}

func TestLoadErrors(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		_, err := loadTestdata(t, "does_not_exist.json")
		if err == nil || !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Load(does_not_exist.json) error = %v, want os.ErrNotExist", err)
		}
	})

	t.Run("trailing values", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "extra.json")
		if err := os.WriteFile(path, []byte(`{"name":"x"} {}`), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		_, err := config.Load(path)
		if err == nil {
			t.Fatal("Load succeeded, want error")
		}
	})

	t.Run("invalid json", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "invalid.json")
		if err := os.WriteFile(path, []byte(`{not json`), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		_, err := config.Load(path)
		if err == nil {
			t.Fatal("Load succeeded, want error")
		}
	})
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
