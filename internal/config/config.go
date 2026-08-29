// Package config defines the blorb.json schema and loads it from disk.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
)

// ToolNamePattern is the strict pattern tool names must match so they are
// valid function names for the API and safe to exec.
var ToolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

const (
	// DefaultMaxTurns is used when max_turns is unset.
	DefaultMaxTurns = 10

	// ProviderTypeOpenAI selects an OpenAI-compatible chat completions API.
	ProviderTypeOpenAI = "openai"
)

// Config is the top-level blorb.json schema.
type Config struct {
	Name         string      `json:"name"`
	SystemPrompt string      `json:"system_prompt"`
	Provider     Provider    `json:"provider"`
	MaxTurns     int         `json:"max_turns,omitempty"`
	Tools        []ToolEntry `json:"tools,omitempty"`
}

// Provider is the provider object, the extension point for multiple LLM
// backends. The Type discriminator determines which other fields are
// recognized; per-type parsing and validation lives here rather than being
// flattened onto Config so future provider types can add their own fields.
type Provider struct {
	Type string `json:"type"`

	// Fields for type "openai".
	Model     string `json:"model,omitempty"`
	BaseURL   string `json:"base_url,omitempty"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
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
	if err := dec.Decode(&struct{}{}); err == nil {
		return Config{}, fmt.Errorf("parse config %s: unexpected trailing JSON values", path)
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
