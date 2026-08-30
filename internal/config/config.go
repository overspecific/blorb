// Package config defines the blorb.json schema and loads it from disk.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
)

// ToolNamePattern is the strict pattern tool names must match so they are
// valid function names for the API and safe to exec.
var ToolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

const (
	// DefaultPath is used when no config path flag is given.
	DefaultPath = "./blorb.json"

	// DefaultMaxTurns is used when max_turns is unset.
	DefaultMaxTurns = 10

	// DefaultLogDir is used when logging.path is unset: logs land in a
	// .logs directory next to the config file.
	DefaultLogDir = ".logs"

	// ProviderTypeOpenAI selects an OpenAI-compatible chat completions API.
	ProviderTypeOpenAI = "openai"

	// DefaultPrefactorAPIURL is the Prefactor API base URL used when
	// api_url is unset in the prefactor config block.
	DefaultPrefactorAPIURL = "https://app.prefactorai.com/api/v1"

	// DefaultPrefactorTokenEnv is the environment variable the Prefactor
	// API token is read from when api_token_env is unset.
	DefaultPrefactorTokenEnv = "PREFACTOR_API_TOKEN"
)

// Config is the top-level blorb.json schema.
type Config struct {
	Name         string           `json:"name"`
	SystemPrompt string           `json:"system_prompt"`
	Provider     Provider         `json:"provider"`
	MaxTurns     int              `json:"max_turns,omitempty"`
	Tools        []ToolEntry      `json:"tools,omitempty"`
	Logging      LogConfig        `json:"logging"`
	Prefactor    *PrefactorConfig `json:"prefactor,omitempty"`
}

// PrefactorEnabled reports whether Prefactor tracing is configured: a
// present prefactor block enables it.
func (c *Config) PrefactorEnabled() bool {
	return c.Prefactor != nil
}

// LogConfig is the logging object in blorb.json.
type LogConfig struct {
	// Path is the log directory name, resolved relative to the config
	// file's directory. When empty, .logs is used. Restricted to a single
	// path component so ../ traversal cannot escape the config directory.
	Path string `json:"path,omitempty"`
	// Enabled is a pointer so an explicit "enabled": false is
	// distinguishable from an absent field: nil means enabled.
	Enabled *bool `json:"enabled,omitempty"`
}

// LoggingEnabled reports whether wire logging is on: true unless
// "enabled": false was set explicitly.
func (c *Config) LoggingEnabled() bool {
	return c.Logging.Enabled == nil || *c.Logging.Enabled
}

// LogDir returns the configured logging path, or DefaultLogDir when unset.
func (c *Config) LogDir() string {
	if c.Logging.Path == "" {
		return DefaultLogDir
	}
	return c.Logging.Path
}

// PrefactorConfig is the optional prefactor object in blorb.json, enabling
// tracing of agent activity to the Prefactor platform.
type PrefactorConfig struct {
	// APITokenEnv names the environment variable holding the Prefactor
	// API token. It is a pointer following the api_key_env convention:
	// absent defaults to DefaultPrefactorTokenEnv, explicit empty is a
	// config error.
	APITokenEnv *string `json:"api_token_env,omitempty"`
	// APIURL is the Prefactor API base URL. Optional; when empty
	// DefaultPrefactorAPIURL applies (see APIURLOrDefault).
	APIURL string `json:"api_url,omitempty"`
	// AgentID is the Prefactor agent to register instances under.
	// Optional; may be empty for deployment-scoped tokens.
	AgentID string `json:"agent_id,omitempty"`
	// EnvironmentID is the Prefactor environment to register instances
	// under. Optional.
	EnvironmentID string `json:"environment_id,omitempty"`
}

// APITokenEnvOrDefault returns the configured api_token_env, or
// DefaultPrefactorTokenEnv when unset.
func (p *PrefactorConfig) APITokenEnvOrDefault() string {
	if p.APITokenEnv == nil {
		return DefaultPrefactorTokenEnv
	}
	return *p.APITokenEnv
}

// APIURLOrDefault returns the configured api_url, or
// DefaultPrefactorAPIURL when unset.
func (p *PrefactorConfig) APIURLOrDefault() string {
	if p.APIURL == "" {
		return DefaultPrefactorAPIURL
	}
	return p.APIURL
}

// validate checks the prefactor block: api_url must be http/https with a
// host when set, and an explicitly-set api_token_env must be non-empty.
func (p *PrefactorConfig) validate() error {
	if p.APIURL != "" {
		u, err := url.Parse(p.APIURL)
		if err != nil {
			return fmt.Errorf("api_url %q: %w", p.APIURL, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("api_url %q must use http or https scheme", p.APIURL)
		}
		if u.Host == "" {
			return fmt.Errorf("api_url %q must include a host", p.APIURL)
		}
	}
	if p.APITokenEnv != nil && *p.APITokenEnv == "" {
		return fmt.Errorf("api_token_env must not be empty when set")
	}
	return nil
}

// Provider is the provider object, the extension point for multiple LLM
// backends. The Type discriminator determines which other fields are
// recognized; per-type parsing and validation lives here rather than being
// flattened onto Config so future provider types can add their own fields.
type Provider struct {
	Type string `json:"type"`

	// Fields for type "openai".
	Model   string `json:"model,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
	// APIKeyEnv names the environment variable holding the API key. It is
	// a pointer so an explicit "api_key_env": "" is distinguishable from
	// an absent field: empty is a config error, absent means no key.
	APIKeyEnv *string `json:"api_key_env,omitempty"`
}

// APIKeyEnvOrDefault returns the configured api_key_env, or "" when unset.
func (p *Provider) APIKeyEnvOrDefault() string {
	if p.APIKeyEnv == nil {
		return ""
	}
	return *p.APIKeyEnv
}

// ToolEntry is a tool declaration in blorb.json.
type ToolEntry struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Command     []string        `json:"command"`
	ArgsSchema  json.RawMessage `json:"args_schema,omitempty"`
}

// Load reads and parses the blorb.json file at path, then validates it.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("parse config %s: unexpected trailing data after JSON value", path)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

// SupportedProviderTypes lists the provider types this build recognizes.
func SupportedProviderTypes() []string {
	return []string{ProviderTypeOpenAI}
}

// MaxTurnsOrDefault returns MaxTurns, or DefaultMaxTurns when unset (zero).
func (c *Config) MaxTurnsOrDefault() int {
	if c.MaxTurns == 0 {
		return DefaultMaxTurns
	}
	return c.MaxTurns
}

// Validate checks all required fields and value constraints.
func (c *Config) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("name is required")
	}
	if c.SystemPrompt == "" {
		return fmt.Errorf("system_prompt is required")
	}
	if err := c.Provider.validate(); err != nil {
		return fmt.Errorf("provider: %w", err)
	}
	if c.MaxTurns < 1 {
		return fmt.Errorf("max_turns must be at least 1 (got %d)", c.MaxTurns)
	}
	for _, t := range c.Tools {
		if err := t.validate(); err != nil {
			return fmt.Errorf("tool %q: %w", t.Name, err)
		}
	}
	if err := validateUniqueToolNames(c.Tools); err != nil {
		return err
	}
	if err := c.Logging.validate(); err != nil {
		return fmt.Errorf("logging: %w", err)
	}
	if c.Prefactor != nil {
		if err := c.Prefactor.validate(); err != nil {
			return fmt.Errorf("prefactor: %w", err)
		}
	}
	return nil
}

// validate rejects a logging.path that is not a single clean path
// component: the directory is resolved relative to the config file's
// directory, and separators or . / .. would traverse elsewhere.
func (l *LogConfig) validate() error {
	if l.Path == "" {
		return nil
	}
	if filepath.Base(l.Path) != l.Path || l.Path == "." || l.Path == ".." {
		return fmt.Errorf("path %q must be a single directory name (no separators)", l.Path)
	}
	return nil
}

func (p *Provider) validate() error {
	switch p.Type {
	case ProviderTypeOpenAI:
		if p.Model == "" {
			return fmt.Errorf("model is required")
		}
		u, err := url.Parse(p.BaseURL)
		if err != nil {
			return fmt.Errorf("base_url %q: %w", p.BaseURL, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("base_url %q must use http or https scheme", p.BaseURL)
		}
		if u.Host == "" {
			return fmt.Errorf("base_url %q must include a host", p.BaseURL)
		}
		if p.APIKeyEnv != nil && *p.APIKeyEnv == "" {
			return fmt.Errorf("api_key_env must not be empty when set")
		}
	default:
		return fmt.Errorf("unknown type %q (supported: %v)", p.Type, SupportedProviderTypes())
	}
	return nil
}

func (t *ToolEntry) validate() error {
	if t.Name == "" {
		return fmt.Errorf("name is required")
	}
	if !ToolNamePattern.MatchString(t.Name) {
		return fmt.Errorf("name %q must match %s", t.Name, ToolNamePattern)
	}
	if t.Description == "" {
		return fmt.Errorf("description is required")
	}
	if len(t.Command) == 0 {
		return fmt.Errorf("command is required")
	}
	for _, cmd := range t.Command {
		if cmd == "" {
			return fmt.Errorf("command must not contain empty strings")
		}
	}
	if len(t.ArgsSchema) > 0 && !json.Valid(t.ArgsSchema) {
		return fmt.Errorf("args_schema must be valid JSON")
	}
	return nil
}

func validateUniqueToolNames(tools []ToolEntry) error {
	seen := make(map[string]struct{}, len(tools))
	for _, t := range tools {
		if _, ok := seen[t.Name]; ok {
			return fmt.Errorf("duplicate tool name %q", t.Name)
		}
		seen[t.Name] = struct{}{}
	}
	return nil
}
