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
	"slices"
	"strings"

	"github.com/overspecific/blorb/internal/tools/builtin"
)

// ToolType selects the kind of a tool entry; the discriminator determines
// which other fields on ToolEntry are recognized.
type ToolType string

// ToolTypeCommand selects a subprocess tool invoked via its command.
const ToolTypeCommand ToolType = "command"

// ToolTypeBuiltin selects a tool implemented inside blorb itself, chosen by
// the builtin field.
const ToolTypeBuiltin ToolType = "builtin"

// ToolTypeSubagent selects a tool that delegates to another agent defined
// in the same config: calling it runs that agent for one turn and returns
// its final assistant text.
const ToolTypeSubagent ToolType = "subagent"

// NamePattern is the strict pattern agent and tool names must match so
// they are valid function names for the API and safe to exec.
var NamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

const (
	// DefaultPath is used when no config path flag is given.
	DefaultPath = "./blorb.json"

	// DefaultMaxTurns is used when max_turns is unset.
	DefaultMaxTurns = 10

	// DefaultLogDir is used when logging.path is unset: logs land in a
	// .logs directory next to the config file.
	DefaultLogDir = ".logs"

	// ModelTypeOpenAI selects an OpenAI-compatible chat completions API.
	ModelTypeOpenAI = "openai-compatible"

	// DefaultPrefactorAPIURL is the Prefactor API base URL used when
	// api_url is unset in the prefactor config block.
	DefaultPrefactorAPIURL = "https://app.prefactorai.com/api/v1"

	// DefaultPrefactorTokenEnv is the environment variable the Prefactor
	// API token is read from when api_token_env is unset.
	DefaultPrefactorTokenEnv = "PREFACTOR_API_TOKEN"
)

// Config is the top-level blorb.json schema. It declares the shared
// model and tool vocabularies once and a set of named agents that
// reference them by name.
type Config struct {
	// Models is the required, non-empty list of named LLM backends.
	// Agents reference them by name; the declarations live here, once.
	Models []Model `json:"models"`
	// Agents is the required, non-empty list of agent definitions. Agent
	// names carry identity: commands resolve an agent by name, the chat
	// banner and Prefactor registrations use it.
	Agents []Agent `json:"agents"`
	// DefaultAgent optionally names the agent commands use when none is
	// given. When set it must name a defined agent.
	DefaultAgent string `json:"default_agent,omitempty"`
	// Tools is the shared tool vocabulary. Agents grant themselves tools
	// by name; the declarations live here, once.
	Tools     []ToolEntry      `json:"tools,omitempty"`
	Logging   LogConfig        `json:"logging"`
	Prefactor *PrefactorConfig `json:"prefactor,omitempty"`

	// dir is the directory of the loaded config file, the anchor for all
	// config-relative paths (logging.path, builtin base_dir). Empty for a
	// programmatically-built Config that never went through Load, which
	// makes relative paths resolve against the process working directory.
	dir string
}

// Agent is one named agent definition inside a config. It owns the
// agent-scoped settings and lists, by name, the top-level tools it may
// use and the top-level model it talks to; the model and tool
// definitions themselves live once at the top level and are shared.
type Agent struct {
	// Name identifies the agent within the config. It feeds the chat
	// banner, the chat command's agent argument, and Prefactor's agent
	// name, and must match NamePattern.
	Name string `json:"name"`
	// SystemPrompt is the agent's system prompt.
	SystemPrompt string `json:"system_prompt"`
	// Model names the top-level model entry this agent talks to. It must
	// be a defined model in the same config (see Config.AgentModel).
	Model string `json:"model"`
	// MaxTurns bounds the agent's per-turn tool round trips; 0 means
	// DefaultMaxTurns (see MaxTurnsOrDefault).
	MaxTurns int `json:"max_turns,omitempty"`
	// Tools lists the names of the top-level tools this agent may use.
	// Absent or empty means the agent has no tools. Every name must
	// exist in the top-level tools; listing order is the agent's.
	Tools []string `json:"tools,omitempty"`
}

// Dir returns the directory of the file this Config was loaded from, or
// "" for a programmatically-built Config. Config-relative paths resolve
// against it.
func (c *Config) Dir() string {
	return c.dir
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

// Model is one named LLM backend declaration in blorb.json. The Type
// discriminator determines which other fields are recognized; per-type
// parsing and validation lives here rather than being flattened onto
// Config so future model types can add their own fields. Agents
// reference models by name; the declarations live once at the top level
// and are shared.
type Model struct {
	// Name identifies the model within the config: what agents put in
	// their model field. It is free-form to the config author —
	// it does not feed the API or the agent identity — and must be
	// unique within the config.
	Name string `json:"name"`

	// Fields for type "openai-compatible".
	Type      string `json:"type"`
	ModelName string `json:"model_name,omitempty"`
	BaseURL   string `json:"base_url,omitempty"`
	// APIKeyEnv names the environment variable holding the API key. It is
	// a pointer so an explicit "api_key_env": "" is distinguishable from
	// an absent field: empty is a config error, absent means no key.
	APIKeyEnv *string `json:"api_key_env,omitempty"`
}

// APIKeyEnvOrDefault returns the configured api_key_env, or "" when unset.
func (m *Model) APIKeyEnvOrDefault() string {
	if m.APIKeyEnv == nil {
		return ""
	}
	return *m.APIKeyEnv
}

// ToolEntry is a tool declaration in blorb.json. The Type discriminator
// determines which other fields are recognized; per-type parsing and
// validation lives here rather than being flattened onto Config so future
// tool types can add their own fields.
type ToolEntry struct {
	Type ToolType `json:"type"`

	Name        string `json:"name"`
	Description string `json:"description"`

	// Fields for type "command".
	Command    []string        `json:"command,omitempty"`
	ArgsSchema json.RawMessage `json:"args_schema,omitempty"`

	// Fields for type "builtin". Builtin selects the implementation from
	// the builtin package; Config is the opaque settings object whose
	// shape the selected builtin alone defines and validates. Config is a
	// RawMessage so it bypasses top-level unknown-field rejection:
	// per-builtin unknown-field checks happen inside each builtin's
	// parser.
	Builtin string          `json:"builtin,omitempty"`
	Config  json.RawMessage `json:"config,omitempty"`

	// Fields for type "subagent".
	// Agent names the target agent this tool delegates to; it must be
	// a defined agent in the same config.
	Agent string `json:"agent,omitempty"`
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
	cfg.dir = filepath.Dir(path)
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

// SupportedModelTypes lists the model types this build recognizes.
func SupportedModelTypes() []string {
	return []string{ModelTypeOpenAI}
}

// MaxTurnsOrDefault returns MaxTurns, or DefaultMaxTurns when unset (zero).
func (a Agent) MaxTurnsOrDefault() int {
	if a.MaxTurns == 0 {
		return DefaultMaxTurns
	}
	return a.MaxTurns
}

// Agent returns the named agent definition and whether it exists in the
// config. Callers resolve an agent by name before handing one to a command.
func (c Config) Agent(name string) (Agent, bool) {
	for _, a := range c.Agents {
		if a.Name == name {
			return a, true
		}
	}
	return Agent{}, false
}

// Model returns the named model definition and whether it exists in the
// config. Agents reference models by name; callers resolve the named
// entry before building an LLM client.
func (c Config) Model(name string) (Model, bool) {
	for _, m := range c.Models {
		if m.Name == name {
			return m, true
		}
	}
	return Model{}, false
}

// DefaultAgentName returns the configured default agent name and whether
// one is set.
func (c Config) DefaultAgentName() (string, bool) {
	if c.DefaultAgent == "" {
		return "", false
	}
	return c.DefaultAgent, true
}

// AgentTools returns the top-level tool entries the agent may use, in the
// agent's listed order (not the top-level declaration order), so tool
// listing order is the agent author's. An empty result is valid: a
// no-tools agent. Validation guarantees every listed name exists in the
// top-level tools, so the result only comes up short for a programmatically
// built config that never went through Validate.
func (c Config) AgentTools(a Agent) []ToolEntry {
	out := make([]ToolEntry, 0, len(a.Tools))
	for _, name := range a.Tools {
		for _, t := range c.Tools {
			if t.Name == name {
				out = append(out, t)
				break
			}
		}
	}
	return out
}

// Validate checks all required fields and value constraints.
func (c *Config) Validate() error {
	if c.Models == nil {
		return fmt.Errorf("models is required")
	}
	if len(c.Models) == 0 {
		return fmt.Errorf("models must not be empty")
	}
	for _, m := range c.Models {
		if err := m.validate(); err != nil {
			return fmt.Errorf("model %q: %w", m.Name, err)
		}
	}
	if err := validateUniqueModelNames(c.Models); err != nil {
		return err
	}
	if c.Agents == nil {
		return fmt.Errorf("agents is required")
	}
	if len(c.Agents) == 0 {
		return fmt.Errorf("agents must not be empty")
	}
	for _, t := range c.Tools {
		if err := t.validate(c.dir); err != nil {
			return fmt.Errorf("tool %q: %w", t.Name, err)
		}
	}
	if err := validateUniqueToolNames(c.Tools); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(c.Agents))
	for _, a := range c.Agents {
		if err := a.validate(c.Models, c.Tools); err != nil {
			return err
		}
		if _, ok := seen[a.Name]; ok {
			return fmt.Errorf("duplicate agent name %q", a.Name)
		}
		seen[a.Name] = struct{}{}
	}
	if err := c.Logging.validate(); err != nil {
		return fmt.Errorf("logging: %w", err)
	}
	if c.Prefactor != nil {
		if err := c.Prefactor.validate(); err != nil {
			return fmt.Errorf("prefactor: %w", err)
		}
	}
	// Last so a per-agent error surfaces first.
	if c.DefaultAgent != "" && !slices.ContainsFunc(c.Agents, func(a Agent) bool { return a.Name == c.DefaultAgent }) {
		return fmt.Errorf("default_agent %q is not a defined agent", c.DefaultAgent)
	}
	// Subagent references and cycles come after per-agent validation, so
	// every agent name is known by the time they run.
	if err := c.validateSubagentRefs(); err != nil {
		return err
	}
	if err := c.validateSubagentCycles(); err != nil {
		return err
	}
	return nil
}

// validateSubagentRefs checks that every subagent tool entry names a
// defined agent. Runs after per-agent validation so all agent names are
// known.
func (c *Config) validateSubagentRefs() error {
	for _, t := range c.Tools {
		if t.Type != ToolTypeSubagent {
			continue
		}
		if _, ok := c.Agent(t.Agent); !ok {
			return fmt.Errorf("tool %q: agent %q is not a defined agent", t.Name, t.Agent)
		}
	}
	return nil
}

// validateSubagentCycles builds the agent delegation graph — an edge from
// an agent to each subagent tool target it is granted — and rejects any
// cycle, including self-reference. A cycle anywhere is fatal: delegation
// depth would otherwise be unbounded.
func (c *Config) validateSubagentCycles() error {
	// target[toolName] is the agent a subagent tool delegates to.
	target := make(map[string]string, len(c.Tools))
	for _, t := range c.Tools {
		if t.Type == ToolTypeSubagent {
			target[t.Name] = t.Agent
		}
	}

	edges := make(map[string][]string, len(c.Agents))
	for _, a := range c.Agents {
		for _, name := range a.Tools {
			if to, ok := target[name]; ok {
				edges[a.Name] = append(edges[a.Name], to)
			}
		}
	}

	const (
		white = 0 // unvisited
		gray  = 1 // on the current DFS path
		black = 2 // fully explored
	)
	color := make(map[string]int, len(c.Agents))
	var stack []string

	var visit func(agent string) error
	visit = func(agent string) error {
		color[agent] = gray
		stack = append(stack, agent)
		for _, next := range edges[agent] {
			switch color[next] {
			case white:
				if err := visit(next); err != nil {
					return err
				}
			case gray:
				// Found a cycle: report the path from its first
				// occurrence on the current stack.
				start := 0
				for i, a := range stack {
					if a == next {
						start = i
						break
					}
				}
				path := append(append([]string(nil), stack[start:]...), next)
				quoted := make([]string, len(path))
				for i, a := range path {
					quoted[i] = fmt.Sprintf("%q", a)
				}
				return fmt.Errorf("agent cycle detected: %s", strings.Join(quoted, " -> "))
			}
		}
		stack = stack[:len(stack)-1]
		color[agent] = black
		return nil
	}

	for _, a := range c.Agents {
		if color[a.Name] == white {
			if err := visit(a.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

// validate checks one agent definition: its name, settings, and that the
// model it names and every tool it lists exist in the config's top-level
// declarations. models and tools are the already-validated top-level
// entries the agent references by name.
func (a *Agent) validate(models []Model, tools []ToolEntry) error {
	if a.Name == "" {
		return fmt.Errorf("agent name is required")
	}
	if !NamePattern.MatchString(a.Name) {
		return fmt.Errorf("agent name %q must match %s", a.Name, NamePattern)
	}
	if a.SystemPrompt == "" {
		return fmt.Errorf("agent %q: system_prompt is required", a.Name)
	}
	if a.Model == "" {
		return fmt.Errorf("agent %q: model is required", a.Name)
	}
	if !slices.ContainsFunc(models, func(m Model) bool { return m.Name == a.Model }) {
		return fmt.Errorf("agent %q: model %q is not a defined model", a.Name, a.Model)
	}
	if a.MaxTurns < 1 {
		return fmt.Errorf("agent %q: max_turns must be at least 1 (got %d)", a.Name, a.MaxTurns)
	}
	seen := make(map[string]struct{}, len(a.Tools))
	for _, name := range a.Tools {
		if _, ok := seen[name]; ok {
			return fmt.Errorf("agent %q: duplicate tool %q", a.Name, name)
		}
		seen[name] = struct{}{}
		if !slices.ContainsFunc(tools, func(t ToolEntry) bool { return t.Name == name }) {
			return fmt.Errorf("agent %q: unknown tool %q", a.Name, name)
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

func (m *Model) validate() error {
	if m.Name == "" {
		return fmt.Errorf("name is required")
	}
	switch m.Type {
	case ModelTypeOpenAI:
		if m.ModelName == "" {
			return fmt.Errorf("model_name is required")
		}
		u, err := url.Parse(m.BaseURL)
		if err != nil {
			return fmt.Errorf("base_url %q: %w", m.BaseURL, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("base_url %q must use http or https scheme", m.BaseURL)
		}
		if u.Host == "" {
			return fmt.Errorf("base_url %q must include a host", m.BaseURL)
		}
		if m.APIKeyEnv != nil && *m.APIKeyEnv == "" {
			return fmt.Errorf("api_key_env must not be empty when set")
		}
	default:
		return fmt.Errorf("unknown type %q (supported: %v)", m.Type, SupportedModelTypes())
	}
	return nil
}

// SupportedToolTypes lists the tool types this build recognizes, sorted
// alphabetically.
func SupportedToolTypes() []string {
	return []string{string(ToolTypeBuiltin), string(ToolTypeCommand), string(ToolTypeSubagent)}
}

func (t *ToolEntry) validate(dir string) error {
	if t.Type == "" {
		return fmt.Errorf("type is required (one of: %s)", strings.Join(SupportedToolTypes(), ", "))
	}
	if t.Name == "" {
		return fmt.Errorf("name is required")
	}
	if !NamePattern.MatchString(t.Name) {
		return fmt.Errorf("name %q must match %s", t.Name, NamePattern)
	}
	if t.Description == "" {
		return fmt.Errorf("description is required")
	}
	switch t.Type {
	case ToolTypeCommand:
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
		if t.Builtin != "" {
			return fmt.Errorf("builtin is not valid for command tools")
		}
		if len(t.Config) > 0 {
			return fmt.Errorf("config is not valid for command tools")
		}
	case ToolTypeBuiltin:
		if t.Builtin == "" {
			return fmt.Errorf("builtin is required (one of: %s)", strings.Join(builtin.Supported(), ", "))
		}
		if !slices.Contains(builtin.Supported(), t.Builtin) {
			return fmt.Errorf("unknown builtin %q (supported: %s)", t.Builtin, strings.Join(builtin.Supported(), ", "))
		}
		if len(t.Command) > 0 {
			return fmt.Errorf("command is not valid for builtin tools")
		}
		if len(t.ArgsSchema) > 0 {
			return fmt.Errorf("args_schema is not valid for builtin tools")
		}
		if _, err := builtin.ParseConfig(t.Builtin, t.Config, builtin.ParseOptions{BaseDir: dir}); err != nil {
			return err
		}
	case ToolTypeSubagent:
		if t.Agent == "" {
			return fmt.Errorf("agent is required")
		}
		if !NamePattern.MatchString(t.Agent) {
			return fmt.Errorf("agent %q must match %s", t.Agent, NamePattern)
		}
		if len(t.ArgsSchema) > 0 && !json.Valid(t.ArgsSchema) {
			return fmt.Errorf("args_schema must be valid JSON")
		}
		if len(t.Command) > 0 {
			return fmt.Errorf("command is not valid for subagent tools")
		}
		if t.Builtin != "" {
			return fmt.Errorf("builtin is not valid for subagent tools")
		}
		if len(t.Config) > 0 {
			return fmt.Errorf("config is not valid for subagent tools")
		}
	default:
		return fmt.Errorf("unknown tool type %q (supported: %s)", t.Type, strings.Join(SupportedToolTypes(), ", "))
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

func validateUniqueModelNames(models []Model) error {
	seen := make(map[string]struct{}, len(models))
	for _, m := range models {
		if _, ok := seen[m.Name]; ok {
			return fmt.Errorf("duplicate model name %q", m.Name)
		}
		seen[m.Name] = struct{}{}
	}
	return nil
}
