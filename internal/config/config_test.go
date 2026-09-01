package config_test

import (
	"encoding/json"
	"errors"
	"fmt"
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

	if len(cfg.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(cfg.Agents))
	}
	agent := cfg.Agents[0]
	if agent.Name != "helper" {
		t.Errorf("Agents[0].Name = %q, want %q", agent.Name, "helper")
	}
	if agent.SystemPrompt != "You are helpful." {
		t.Errorf("Agents[0].SystemPrompt = %q, want %q", agent.SystemPrompt, "You are helpful.")
	}
	if agent.Provider.Type != config.ProviderTypeOpenAI {
		t.Errorf("Agents[0].Provider.Type = %q, want %q", agent.Provider.Type, config.ProviderTypeOpenAI)
	}
	if agent.Provider.Model != "gpt-4o-mini" {
		t.Errorf("Agents[0].Provider.Model = %q, want %q", agent.Provider.Model, "gpt-4o-mini")
	}
	if agent.Provider.BaseURL != "https://api.example.com/v1" {
		t.Errorf("Agents[0].Provider.BaseURL = %q, want %q", agent.Provider.BaseURL, "https://api.example.com/v1")
	}
	if agent.MaxTurns != 5 {
		t.Errorf("Agents[0].MaxTurns = %d, want 5", agent.MaxTurns)
	}
	if got, want := agent.MaxTurnsOrDefault(), 5; got != want {
		t.Errorf("Agents[0].MaxTurnsOrDefault() = %d, want %d", got, want)
	}
	if got, want := agent.Tools, []string{"list_files", "greet", "read_fixture"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Agents[0].Tools = %v, want %v", got, want)
	}
	// No default_agent in the fixture: the field is optional and Load
	// succeeds with it empty.
	if cfg.DefaultAgent != "" {
		t.Errorf("DefaultAgent = %q, want empty when unset", cfg.DefaultAgent)
	}
	if len(cfg.Tools) != 3 {
		t.Fatalf("len(Tools) = %d, want 3", len(cfg.Tools))
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

func TestLoadAgentsMulti(t *testing.T) {
	// The canonical multi-agent fixture: two agents sharing one tool, only
	// one of which also uses a second tool, and one with an empty tools
	// list. Pins the parsed shape and per-agent defaults.
	cfg, err := loadTestdata(t, "agents_multi_valid.json")
	if err != nil {
		t.Fatalf("Load(agents_multi_valid.json) error = %v, want nil", err)
	}

	if len(cfg.Agents) != 2 {
		t.Fatalf("len(Agents) = %d, want 2", len(cfg.Agents))
	}
	main := cfg.Agents[0]
	if main.Name != "main" {
		t.Errorf("Agents[0].Name = %q, want %q", main.Name, "main")
	}
	if main.SystemPrompt != "You are helpful." {
		t.Errorf("Agents[0].SystemPrompt = %q, want %q", main.SystemPrompt, "You are helpful.")
	}
	if main.MaxTurns != 3 {
		t.Errorf("Agents[0].MaxTurns = %d, want 3", main.MaxTurns)
	}
	if got, want := main.MaxTurnsOrDefault(), 3; got != want {
		t.Errorf("main MaxTurnsOrDefault() = %d, want %d", got, want)
	}
	if got, want := main.Tools, []string{"echo", "read_fixture"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("main Tools = %v, want %v", got, want)
	}

	quiet := cfg.Agents[1]
	if quiet.Name != "quiet" {
		t.Errorf("Agents[1].Name = %q, want %q", quiet.Name, "quiet")
	}
	if len(quiet.Tools) != 0 {
		t.Errorf("quiet Tools = %v, want empty", quiet.Tools)
	}
	// Configs require max_turns >= 1, so a loaded agent always carries an
	// explicit value; zero-value defaulting is covered programmatically
	// in TestMaxTurnsOrDefaultProgrammatic.
	if got, want := quiet.MaxTurnsOrDefault(), 1; got != want {
		t.Errorf("quiet MaxTurnsOrDefault() = %d, want %d", got, want)
	}
	if cfg.DefaultAgent != "main" {
		t.Errorf("DefaultAgent = %q, want %q", cfg.DefaultAgent, "main")
	}
	if len(cfg.Tools) != 2 {
		t.Fatalf("len(Tools) = %d, want 2", len(cfg.Tools))
	}
}

func TestLoadMaxTurnsDefault(t *testing.T) {
	cfg, err := loadTestdata(t, "with_api_key_env.json")
	if err != nil {
		t.Fatalf("Load error = %v, want nil", err)
	}
	agent := cfg.Agents[0]
	if agent.Provider.APIKeyEnvOrDefault() != "MY_API_KEY" {
		t.Errorf("APIKeyEnv = %q, want %q", agent.Provider.APIKeyEnvOrDefault(), "MY_API_KEY")
	}
	if agent.MaxTurns != 5 {
		t.Errorf("MaxTurns = %d, want 5", agent.MaxTurns)
	}
	if got, want := agent.MaxTurnsOrDefault(), 5; got != want {
		t.Errorf("MaxTurnsOrDefault() = %d, want %d", got, want)
	}
}

func TestMaxTurnsOrDefaultProgrammatic(t *testing.T) {
	agent := config.Agent{}
	if got := agent.MaxTurnsOrDefault(); got != config.DefaultMaxTurns {
		t.Errorf("MaxTurnsOrDefault() = %d, want %d for zero value", got, config.DefaultMaxTurns)
	}

	agent.MaxTurns = 7
	if got := agent.MaxTurnsOrDefault(); got != 7 {
		t.Errorf("MaxTurnsOrDefault() = %d, want 7", got)
	}
}

func TestAgentLookup(t *testing.T) {
	cfg := config.Config{
		Agents:       []config.Agent{{Name: "alpha"}, {Name: "beta"}},
		DefaultAgent: "b",
	}

	t.Run("present", func(t *testing.T) {
		a, ok := cfg.Agent("alpha")
		if !ok || a.Name != "alpha" {
			t.Errorf("Agent(alpha) = (%+v, %v), want the alpha agent", a, ok)
		}
	})

	t.Run("absent", func(t *testing.T) {
		a, ok := cfg.Agent("missing")
		if ok {
			t.Errorf("Agent(missing) = (%+v, %v), want not ok", a, ok)
		}
		if a.Name != "" {
			t.Errorf("Agent(missing).Name = %q, want empty for the zero Agent", a.Name)
		}
	})

	t.Run("default set", func(t *testing.T) {
		name, ok := cfg.DefaultAgentName()
		if !ok || name != "b" {
			t.Errorf("DefaultAgentName() = (%q, %v), want (b, true)", name, ok)
		}
	})

	t.Run("default unset", func(t *testing.T) {
		name, ok := config.Config{Agents: []config.Agent{{Name: "alpha"}}}.DefaultAgentName()
		if ok {
			t.Errorf("DefaultAgentName() = (%q, true), want false when default_agent is unset", name)
		}
	})
}

func TestAgentTools(t *testing.T) {
	cfg := config.Config{
		Tools: []config.ToolEntry{
			{Name: "first"},
			{Name: "second"},
			{Name: "third"},
		},
	}

	t.Run("listed order wins over declaration order", func(t *testing.T) {
		// The agent lists its tools out of top-level declaration order;
		// AgentTools must follow the agent's list order.
		agent := config.Agent{Name: "a", Tools: []string{"third", "first"}}
		got := cfg.AgentTools(agent)
		if len(got) != 2 {
			t.Fatalf("got %d tools, want 2: %+v", len(got), got)
		}
		if got[0].Name != "third" || got[1].Name != "first" {
			t.Errorf("AgentTools = [%s, %s], want [third, first] (agent's listed order)", got[0].Name, got[1].Name)
		}
	})

	t.Run("no-tools agent gets an empty slice", func(t *testing.T) {
		agent := config.Agent{Name: "a"}
		if got := cfg.AgentTools(agent); len(got) != 0 {
			t.Errorf("AgentTools = %v, want empty for a no-tools agent", got)
		}
	})

	t.Run("all tools granted misses nothing", func(t *testing.T) {
		agent := config.Agent{Name: "a", Tools: []string{"first", "second", "third"}}
		got := cfg.AgentTools(agent)
		if len(got) != 3 {
			t.Fatalf("got %d tools, want 3", len(got))
		}
		for i, want := range []string{"first", "second", "third"} {
			if got[i].Name != want {
				t.Errorf("tool %d = %q, want %q", i, got[i].Name, want)
			}
		}
	})

	t.Run("unlisted top-level tools are excluded", func(t *testing.T) {
		agent := config.Agent{Name: "a", Tools: []string{"second"}}
		got := cfg.AgentTools(agent)
		if len(got) != 1 || got[0].Name != "second" {
			t.Errorf("AgentTools = %v, want only second", got)
		}
	})
}

// TestValidateRejectsBadArgsSchema covers args_schema validation. It cannot
// be exercised through Load: json.RawMessage only captures values that are
// already valid JSON, so invalid schema JSON fails the outer parse first.
func TestValidateRejectsBadArgsSchema(t *testing.T) {
	cfg := config.Config{
		Agents: []config.Agent{validAgent()},
		Tools: []config.ToolEntry{{
			Type:        config.ToolTypeCommand,
			Name:        "t",
			Description: "Broken schema.",
			Command:     []string{"echo"},
			ArgsSchema:  json.RawMessage(`{oops`),
		}},
	}

	err := cfg.Validate()
	if err == nil || !contains(err.Error(), "args_schema must be valid JSON") {
		t.Errorf("Validate error = %v, want an args_schema error", err)
	}
}

func TestValidateAcceptsMissingAPIKeyEnv(t *testing.T) {
	cfg := config.Config{Agents: []config.Agent{validAgent()}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate error = %v, want nil when api_key_env is absent", err)
	}
}

// validAgent returns the canonical single valid agent for programmatic
// configs.
func validAgent() config.Agent {
	return config.Agent{
		Name:         "helper",
		SystemPrompt: "You are helpful.",
		Provider: config.Provider{
			Type:    config.ProviderTypeOpenAI,
			Model:   "m",
			BaseURL: "http://localhost:1",
		},
		MaxTurns: 1,
	}
}

func TestLoadWithPrefactor(t *testing.T) {
	cfg, err := loadTestdata(t, "with_prefactor.json")
	if err != nil {
		t.Fatalf("Load(with_prefactor.json) error = %v, want nil", err)
	}
	if !cfg.PrefactorEnabled() {
		t.Fatal("PrefactorEnabled() = false, want true")
	}
	pf := cfg.Prefactor
	if pf == nil {
		t.Fatal("Prefactor = nil, want non-nil")
	}
	if got := pf.APITokenEnvOrDefault(); got != "MY_PREFACTOR_TOKEN" {
		t.Errorf("APITokenEnvOrDefault() = %q, want %q", got, "MY_PREFACTOR_TOKEN")
	}
	if got := pf.APIURLOrDefault(); got != "https://prefactor.example.com/api/v1" {
		t.Errorf("APIURLOrDefault() = %q, want %q", got, "https://prefactor.example.com/api/v1")
	}
	if pf.AgentID != "agent-123" {
		t.Errorf("AgentID = %q, want %q", pf.AgentID, "agent-123")
	}
	if pf.EnvironmentID != "env-456" {
		t.Errorf("EnvironmentID = %q, want %q", pf.EnvironmentID, "env-456")
	}
}

func TestPrefactorAbsent(t *testing.T) {
	cfg, err := loadTestdata(t, "valid.json")
	if err != nil {
		t.Fatalf("Load(valid.json) error = %v, want nil", err)
	}
	if cfg.PrefactorEnabled() {
		t.Error("PrefactorEnabled() = true, want false when the prefactor block is absent")
	}
}

func TestPrefactorDefaults(t *testing.T) {
	cfg := config.Config{
		Agents:    []config.Agent{validAgent()},
		Prefactor: &config.PrefactorConfig{},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate error = %v, want nil", err)
	}
	if !cfg.PrefactorEnabled() {
		t.Error("PrefactorEnabled() = false, want true with an empty prefactor block")
	}
	if got := cfg.Prefactor.APITokenEnvOrDefault(); got != config.DefaultPrefactorTokenEnv {
		t.Errorf("APITokenEnvOrDefault() = %q, want %q", got, config.DefaultPrefactorTokenEnv)
	}
	if got := cfg.Prefactor.APIURLOrDefault(); got != config.DefaultPrefactorAPIURL {
		t.Errorf("APIURLOrDefault() = %q, want %q", got, config.DefaultPrefactorAPIURL)
	}
}

func TestValidateBuiltinEntry(t *testing.T) {
	// Builtin entries carry the shared name and description checks, plus
	// per-type validation of the builtin field, its settings object, and
	// the command-only fields they must not carry.
	base := config.Config{
		Agents: []config.Agent{validAgent()},
		Tools: []config.ToolEntry{{
			Type:        config.ToolTypeBuiltin,
			Name:        "t",
			Description: "A builtin tool.",
			Builtin:     "read",
			Config:      json.RawMessage(`{"base_dir":"."}`),
		}},
	}
	if err := base.Validate(); err != nil {
		t.Errorf("Validate error = %v, want nil for a minimal builtin entry", err)
	}

	case_ := base
	case_.Tools = append([]config.ToolEntry(nil), base.Tools...)

	noName := case_
	noName.Tools[0].Name = ""
	if err := noName.Validate(); err == nil || !contains(err.Error(), "name is required") {
		t.Errorf("Validate error = %v, want a name error", err)
	}

	case_ = base
	case_.Tools = append([]config.ToolEntry(nil), base.Tools...)
	case_.Tools[0].Name = "bad name.dotted"
	if err := case_.Validate(); err == nil || !contains(err.Error(), "must match") {
		t.Errorf("Validate error = %v, want a name pattern error", err)
	}

	case_ = base
	case_.Tools = append([]config.ToolEntry(nil), base.Tools...)
	case_.Tools[0].Description = ""
	if err := case_.Validate(); err == nil || !contains(err.Error(), "description is required") {
		t.Errorf("Validate error = %v, want a description error", err)
	}
}

func TestLoadRejects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		file     string
		errParts []string
	}{
		{"agents_missing.json", []string{"agents is required"}},
		{"agents_empty.json", []string{"agents must not be empty"}},
		{"agent_missing_name.json", []string{"agent name is required"}},
		{"agent_bad_name.json", []string{`agent name "has space" must match`}},
		{"duplicate_agent_names.json", []string{"duplicate agent name"}},
		{"agent_missing_system_prompt.json", []string{`agent "helper": system_prompt is required`}},
		{"agent_invalid_provider.json", []string{"unknown type", "anthropic", "openai"}},
		{"agent_bad_max_turns.json", []string{`agent "helper": max_turns must be at least 1 (got 0)`}},
		{"agent_unknown_tool.json", []string{`agent "helper": unknown tool "nope"`}},
		{"agent_duplicate_tool.json", []string{`agent "helper": duplicate tool "echo"`}},
		{"agent_empty_tool.json", []string{`agent "helper": unknown tool ""`}},
		{"agent_unknown_field.json", []string{"no_such_field"}},
		{"default_agent_unknown.json", []string{`default_agent "ghost" is not a defined agent`}},
		{"missing_provider_model.json", []string{"model is required"}},
		{"missing_provider_base_url.json", []string{"base_url"}},
		{"bad_base_url_scheme.json", []string{"ftp", "http or https"}},
		{"tool_missing_name.json", []string{"name is required"}},
		{"tool_missing_description.json", []string{"description is required"}},
		{"tool_missing_type.json", []string{"type is required"}},
		{"tool_unknown_type.json", []string{"unknown tool type"}},
		{"tool_missing_command.json", []string{"command is required"}},
		{"tool_bad_name.json", []string{"must match"}},
		{"tool_builtin_unknown.json", []string{"unknown builtin", "nope"}},
		{"tool_builtin_missing_builtin.json", []string{"builtin is required"}},
		{"tool_builtin_missing_config.json", []string{"base_dir is required"}},
		{"tool_builtin_bad_config.json", []string{"base_dir"}},
		{"tool_builtin_unknown_config_field.json", []string{"extra"}},
		{"tool_builtin_with_command.json", []string{"command is not valid for builtin tools"}},
		{"tool_builtin_with_args_schema.json", []string{"args_schema is not valid for builtin tools"}},
		{"tool_command_with_builtin.json", []string{"builtin is not valid for command tools"}},
		{"tool_command_with_config.json", []string{"config is not valid for command tools"}},
		{"duplicate_tool_names.json", []string{"duplicate tool name"}},
		{"unknown_top_level_field.json", []string{"unknown_field"}},
		{"empty_api_key_env.json", []string{"api_key_env must not be empty when set"}},
		{"prefactor_unknown_field.json", []string{"no_such_field"}},
		{"prefactor_empty_token_env.json", []string{"api_token_env must not be empty when set"}},
		{"prefactor_bad_api_url_scheme.json", []string{"ftp", "http or https"}},
		{"prefactor_api_url_no_host.json", []string{"api_url", "host"}},
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

func TestLoadBuiltinValid(t *testing.T) {
	cfg, err := loadTestdata(t, "tool_builtin_valid.json")
	if err != nil {
		t.Fatalf("Load(tool_builtin_valid.json) error = %v, want nil", err)
	}
	if len(cfg.Tools) != 1 {
		t.Fatalf("len(Tools) = %d, want 1", len(cfg.Tools))
	}
	tool := cfg.Tools[0]
	if tool.Type != config.ToolTypeBuiltin {
		t.Errorf("Type = %q, want builtin", tool.Type)
	}
	if tool.Builtin != "read" {
		t.Errorf("Builtin = %q, want read", tool.Builtin)
	}
	if len(tool.Config) == 0 || !json.Valid(tool.Config) {
		t.Errorf("Config = %s, want the raw config object", tool.Config)
	}
	if len(tool.Command) != 0 {
		t.Errorf("Command = %v, want empty for a builtin entry", tool.Command)
	}
	// base_dir "." resolves relative to the config file, so the fixture's
	// builtin read with base_dir "." anchors at testdata/ — proven by the
	// fixture loading at all (the parse stats the directory).
}

func TestConfigDir(t *testing.T) {
	cfg, err := loadTestdata(t, "valid.json")
	if err != nil {
		t.Fatalf("Load error = %v, want nil", err)
	}
	if got, want := cfg.Dir(), "testdata"; !strings.HasSuffix(got, want) {
		t.Errorf("Dir() = %q, want it to end with %q", got, want)
	}

	// A programmatically-built Config has no directory: config-relative
	// paths fall back to the process working directory.
	prog := config.Config{Agents: []config.Agent{validAgent()}}
	if got := prog.Dir(); got != "" {
		t.Errorf("Dir() = %q, want empty for a built Config", got)
	}
}

func TestLoadBuiltinBaseDirRelativeToConfig(t *testing.T) {
	t.Parallel()

	// base_dir resolves against the config file's directory, not the
	// process working directory (matching logging.path's rule).
	t.Run("resolves relative paths", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		cfgDir := filepath.Join(dir, "cfg")
		if err := os.MkdirAll(filepath.Join(cfgDir, "kb"), 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(cfgDir, "blorb.json")
		if err := os.WriteFile(path, []byte(`{
			"agents": [{
				"name": "helper",
				"system_prompt": "s",
				"provider": {"type": "openai", "model": "m", "base_url": "http://localhost:1"},
				"max_turns": 1
			}],
			"tools": [{
				"type": "builtin",
				"name": "read",
				"description": "Read a file.",
				"builtin": "read",
				"config": {"base_dir": "kb"}
			}]
		}`), 0o644); err != nil {
			t.Fatal(err)
		}

		// Loading from a different working directory must still resolve
		// kb against the config's directory.
		if _, err := config.Load(path); err != nil {
			t.Errorf("Load error = %v, want nil (base_dir resolves against the config file)", err)
		}
	})

	t.Run("missing dir relative to config fails", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "blorb.json")
		if err := os.WriteFile(path, []byte(`{
			"agents": [{
				"name": "helper",
				"system_prompt": "s",
				"provider": {"type": "openai", "model": "m", "base_url": "http://localhost:1"},
				"max_turns": 1
			}],
			"tools": [{
				"type": "builtin",
				"name": "read",
				"description": "Read a file.",
				"builtin": "read",
				"config": {"base_dir": "does-not-exist"}
			}]
		}`), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := config.Load(path)
		if err == nil || !strings.Contains(err.Error(), "does-not-exist") {
			t.Errorf("error = %v, want a base_dir resolution error", err)
		}
	})
}

func TestLoadErrors(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		_, err := loadTestdata(t, "does_not_exist.json")
		if err == nil || !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Load(does_not_exist.json) error = %v, want os.ErrNotExist", err)
		}
	})

	t.Run("default path missing", func(t *testing.T) {
		dir := t.TempDir()
		_, err := config.Load(filepath.Join(dir, "blorb.json"))
		if err == nil || !errors.Is(err, os.ErrNotExist) {
			t.Errorf("error = %v, want a not-exist error", err)
		}
	})

	t.Run("trailing values", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "extra.json")
		if err := os.WriteFile(path, []byte(`{"agents":[]} {}`), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		_, err := config.Load(path)
		if err == nil {
			t.Fatal("Load succeeded, want error")
		}
	})

	t.Run("trailing garbage", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "garbage.json")
		if err := os.WriteFile(path, []byte(`{"agents":[{"name":"x","system_prompt":"s","provider":{"type":"openai","model":"m","base_url":"http://x"}}]} xyz`), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		_, err := config.Load(path)
		if err == nil {
			t.Fatal("Load succeeded, want error")
		}
		if !contains(err.Error(), "trailing") {
			t.Errorf("error = %q, want a trailing-data mention", err)
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

// loggingConfig builds a valid Config with the given logging settings.
func loggingConfig(t *testing.T, logging json.RawMessage) (config.Config, error) {
	t.Helper()
	var full string
	if len(logging) > 0 {
		full = fmt.Sprintf(`{
			"agents": [
				{
					"name": "helper",
					"system_prompt": "You are helpful.",
					"provider": {"type": "openai", "model": "m", "base_url": "http://localhost:1"},
					"max_turns": 1
				}
			],
			"logging": %s
		}`, logging)
	} else {
		full = `{
			"agents": [
				{
					"name": "helper",
					"system_prompt": "You are helpful.",
					"provider": {"type": "openai", "model": "m", "base_url": "http://localhost:1"},
					"max_turns": 1
				}
			]
		}`
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "blorb.json")
	if err := os.WriteFile(path, []byte(full), 0o600); err != nil {
		return config.Config{}, err
	}
	return config.Load(path)
}

func TestLoggingDefaults(t *testing.T) {
	cfg, err := loggingConfig(t, nil)
	if err != nil {
		t.Fatalf("Load error = %v, want nil", err)
	}
	if !cfg.LoggingEnabled() {
		t.Error("LoggingEnabled() = false, want true (default enabled)")
	}
	if got := cfg.LogDir(); got != ".logs" {
		t.Errorf("LogDir() = %q, want .logs", got)
	}
}

func TestLoggingExplicitlyDisabled(t *testing.T) {
	cfg, err := loggingConfig(t, json.RawMessage(`{"enabled": false}`))
	if err != nil {
		t.Fatalf("Load error = %v, want nil", err)
	}
	if cfg.LoggingEnabled() {
		t.Error("LoggingEnabled() = true, want false")
	}
	// The path is independent of enabled.
	if got := cfg.LogDir(); got != ".logs" {
		t.Errorf("LogDir() = %q, want .logs", got)
	}
}

func TestLoggingCustomPath(t *testing.T) {
	cfg, err := loggingConfig(t, json.RawMessage(`{"path": "mylogs"}`))
	if err != nil {
		t.Fatalf("Load error = %v, want nil", err)
	}
	if !cfg.LoggingEnabled() {
		t.Error("LoggingEnabled() = false, want true")
	}
	if got := cfg.LogDir(); got != "mylogs" {
		t.Errorf("LogDir() = %q, want mylogs", got)
	}
}

func TestValidateRejectsBadLoggingPath(t *testing.T) {
	for _, path := range []string{"a/b", "..", ".", "../escape"} {
		_, err := loggingConfig(t, json.RawMessage(fmt.Sprintf(`{"path": %q}`, path)))
		if err == nil {
			t.Errorf("path %q: Load succeeded, want error", path)
			continue
		}
		if !strings.Contains(err.Error(), "logging") {
			t.Errorf("path %q: error = %v, want it to mention logging", path, err)
		}
	}
}
