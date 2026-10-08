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

	"github.com/overspecific/blorb/internal/llm"
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

// ToolTypeToolset selects an entry that refers to another top-level
// toolset by name. It is valid only inside a toolset's tools list. The
// referred toolset's own name supplies the granted prefix.
const ToolTypeToolset ToolType = "toolset"

// ToolTypeDecider selects a tool that evaluates one of the config's
// deciders: the tool call supplies only the state, and blorb makes one
// decision-model call with the decider's fixed questions and returns the
// typed answers.
const ToolTypeDecider ToolType = "decider"

// ToolTypeBand selects a tool that calls the Band platform. It is never
// valid in blorb.json: band tools are constructed programmatically by
// the band command and injected into the registry.
const ToolTypeBand ToolType = "band"

// ToolsetTypeSimple selects a plain toolset: a group of inline tool
// declarations plus references to other toolsets. It is the default when
// a toolset's type field is absent.
const ToolsetTypeSimple = "simple"

// ToolsetTypeBuiltin selects a builtin toolset: a named bundle shipped
// inside blorb, chosen by the builtin field and configured by one shared
// settings object.
const ToolsetTypeBuiltin = "builtin"

// JudgeWhenEnd is the judge timing that runs after the judged agent
// finishes: after the single turn in run mode, at session end in chat
// mode. Currently the only supported timing.
const JudgeWhenEnd = "end"

// supportedJudgeWhens lists the supported judge timings, sorted
// alphabetically.
func supportedJudgeWhens() []string {
	return []string{JudgeWhenEnd}
}

// NamePattern is the strict pattern agent and tool names must match so
// they are valid function names for the API and safe to exec.
var NamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// QuestionNamePattern is the pattern decider question names must match:
// the decision wire format restricts question names to identifiers.
var QuestionNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

const (
	// DefaultPath is used when no config path flag is given.
	DefaultPath = "./blorb.json"

	// DefaultMaxTurns is used when max_turns is unset.
	DefaultMaxTurns = 10

	// DefaultLogDir is used when logging.path is unset: logs land in a
	// .logs directory next to the config file.
	DefaultLogDir = ".logs"

	// ProviderTypeOpenAI selects an OpenAI-compatible chat completions API.
	ProviderTypeOpenAI = "openai-compatible"

	// ProviderTypeOllama selects Ollama's native /api/chat API: a local
	// Ollama server or Ollama cloud.
	ProviderTypeOllama = "ollama"

	// ModelTypeLLM is the default model_type: a chat completions model
	// the agent engine drives in a message loop.
	ModelTypeLLM = "llm"

	// ModelTypeDecision selects a decision model: a System One model
	// (Jev is the first example) that evaluates a state against typed
	// questions and returns typed answers with probabilities. Only
	// deciders may reference a decision model.
	ModelTypeDecision = "decision"

	// QuestionTypeChoice asks a question whose answer space is a set of
	// named options; the answer selects one and reports a probability per
	// option.
	QuestionTypeChoice = "choice"

	// QuestionTypeNoul asks a calibration question whose answer is the
	// probability of yes.
	QuestionTypeNoul = "noul"

	// QuestionTypeScore asks a question whose answer space is an ordered
	// set of levels; the answer selects a level and reports a probability
	// per level.
	QuestionTypeScore = "score"

	// DefaultPrefactorAPIURL is the Prefactor API base URL used when
	// api_url is unset in the prefactor config block.
	DefaultPrefactorAPIURL = "https://app.prefactorai.com/api/v1"

	// DefaultPrefactorTokenEnv is the environment variable the Prefactor
	// API token is read from when api_token_env is unset.
	DefaultPrefactorTokenEnv = "PREFACTOR_API_TOKEN"

	// DefaultBandRESTURL is the Band Agent API base URL used when
	// rest_url is unset in the band config block.
	DefaultBandRESTURL = "https://api.band.ai"

	// DefaultBandWSURL is the Band subscriptions WebSocket URL used when
	// ws_url is unset in the band config block.
	DefaultBandWSURL = "wss://app.band.ai/api/v1/socket/websocket"

	// DefaultVoiceWSURL is the AssemblyAI Voice Agent WebSocket endpoint
	// used when ws_url is unset in the voice config block.
	DefaultVoiceWSURL = "wss://agents.assemblyai.com/v1/ws"
)

// DefaultVoiceInputCommand is the microphone capture command used when
// input_command is unset in the voice config block: arecord emitting 24 kHz
// 16-bit mono PCM on stdout.
var DefaultVoiceInputCommand = []string{"arecord", "-q", "-f", "cd", "-r", "24000", "-c", "1"}

// DefaultVoiceOutputCommand is the speaker playback command used when
// output_command is unset in the voice config block: aplay consuming 24 kHz
// 16-bit mono PCM on stdin.
var DefaultVoiceOutputCommand = []string{"aplay", "-q", "-r", "24000", "-f", "s16_le", "-c", "1"}

// Config is the top-level blorb.json schema. It declares the shared
// provider, model, and tool vocabularies once and a set of named agents
// that reference them by name.
type Config struct {
	// Providers is the required, non-empty list of named LLM server
	// connections. Models reference them by name; the connection
	// declarations live here, once.
	Providers []Provider `json:"providers"`
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
	Tools []ToolEntry `json:"tools,omitempty"`
	// Toolsets is the shared toolset vocabulary: named groups of tools
	// that agents grant whole. A toolset's members are renamed with the
	// toolset name as a prefix when granted; see AgentTools.
	Toolsets []Toolset `json:"toolsets,omitempty"`
	// Deciders is the shared decider vocabulary: named decision-model
	// evaluations fixed to a set of typed questions. A decider tool
	// references one by name; the declarations live here, once.
	Deciders  []Decider        `json:"deciders,omitempty"`
	Logging   LogConfig        `json:"logging"`
	Prefactor *PrefactorConfig `json:"prefactor,omitempty"`

	// dir is the directory of the loaded config file, the anchor for all
	// config-relative paths (logging.path, builtin base_dir). Empty for a
	// programmatically-built Config that never went through Load, which
	// makes relative paths resolve against the process working directory.
	dir string
}

// Decider is one named decider definition inside a config. It names a
// decision model and fixes the typed questions every call asks; only
// the state varies per call. Deciders are reachable only through
// decider tools; they are not agents and have no tools, turns, or
// judges of their own.
type Decider struct {
	// Name identifies the decider within the config. It must match
	// NamePattern and be unique among deciders.
	Name string `json:"name"`
	// Model names the decision model this decider evaluates with. It must
	// be a defined decision model in the same config.
	Model string `json:"model"`
	// Questions fixes the typed questions every call asks, keyed by
	// question name. There must be at least one.
	Questions map[string]Question `json:"questions"`
}

// Question is one typed question a decider asks. Type is one of
// choice, noul, or score. Instructions is the question itself: a
// string for short questions, or an object or array putting the
// question in one field and the data that guides it in the others;
// it is passed to the server verbatim. Criteria defines the answer
// space: an object of two or more option descriptions for choice, an
// ordered array of two to ten level descriptions for score, and
// optional true/false label descriptions (an object) for noul.
type Question struct {
	Type         string          `json:"type"`
	Instructions json.RawMessage `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria,omitempty"`
}

// Agent is one named agent definition inside a config. It owns the
// agent-scoped settings and lists, by name, the tools it may use and the
// top-level model it talks to; the model and tool declarations themselves
// live once at the top level and are shared.
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
	// Tools lists what this agent may use, by name: top-level tools,
	// toolsets (granting every member), and individual toolset members
	// by their granted prefixed name. Absent or empty means the agent
	// has no tools. Every name must resolve, and no granted name may
	// repeat. Listing order is the agent's.
	Tools []string `json:"tools,omitempty"`
	// Judges lists the agents that judge this agent's run: once the
	// agent finishes, each end-timing judge is invoked with a
	// rendering of the run's transcript as its first user message.
	// Validation guarantees every named agent is defined, every When
	// is a supported timing, and no judge chain (judges, possibly
	// combined with subagent edges) forms a cycle.
	Judges []Judge `json:"judges,omitempty"`
	// Band is the optional Band platform connection for this agent. A
	// present block lets the band command serve this agent as a remote
	// agent on Band; see BandConfig.
	Band *BandConfig `json:"band,omitempty"`
	// Voice is the optional voice session configuration for this agent. A
	// present block lets the voice command serve this agent through
	// AssemblyAI's Voice Agent API; see VoiceConfig.
	Voice *VoiceConfig `json:"voice,omitempty"`
}

// Judge names one agent that judges this agent's run, and when it
// runs. When is JudgeWhenEnd when unset (the JSON field is optional).
type Judge struct {
	// Agent names the judge; it must be a defined agent in the
	// same config.
	Agent string `json:"agent"`
	// When selects when the judge runs; empty means
	// JudgeWhenEnd. Validation rejects unknown values.
	When string `json:"when,omitempty"`
}

// WhenOrDefault returns When, or JudgeWhenEnd when unset (empty).
func (j Judge) WhenOrDefault() string {
	if j.When == "" {
		return JudgeWhenEnd
	}
	return j.When
}

// JudgesWhen returns the judge entries whose timing is when, in
// the agent's listed order. The "end" trigger sites use it to
// select their judges; a future timing's trigger site does the
// same with its own value.
func (a Agent) JudgesWhen(when string) []Judge {
	var out []Judge
	for _, j := range a.Judges {
		if j.WhenOrDefault() == when {
			out = append(out, j)
		}
	}
	return out
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

// BandEnabled reports whether the agent is configured to run on Band: a
// present band block enables it.
func (a Agent) BandEnabled() bool {
	return a.Band != nil
}

// BandConfig is the optional band object in an agent definition, connecting
// blorb to the Band platform as a remote agent. The block's presence enables
// the band command for that agent. The agent's Band id is not configured: it
// comes from the agent API key at startup (GET /me).
type BandConfig struct {
	// APIKeyEnv names the environment variable holding the Band agent
	// API key. It is required: the agent API key is the one thing a
	// Band connection cannot do without, and there is no sensible
	// default for where it lives.
	APIKeyEnv string `json:"api_key_env"`
	// RESTURL is the Band Agent API base URL. Optional; when empty
	// DefaultBandRESTURL applies (see RESTURLOrDefault).
	RESTURL string `json:"rest_url,omitempty"`
	// WSURL is the Band subscriptions WebSocket URL. Optional; when
	// empty DefaultBandWSURL applies (see WSURLOrDefault).
	WSURL string `json:"ws_url,omitempty"`
}

// RESTURLOrDefault returns the configured rest_url, or
// DefaultBandRESTURL when unset.
func (b *BandConfig) RESTURLOrDefault() string {
	if b.RESTURL == "" {
		return DefaultBandRESTURL
	}
	return b.RESTURL
}

// WSURLOrDefault returns the configured ws_url, or DefaultBandWSURL
// when unset.
func (b *BandConfig) WSURLOrDefault() string {
	if b.WSURL == "" {
		return DefaultBandWSURL
	}
	return b.WSURL
}

// validate checks the band block: api_key_env is required and non-empty,
// rest_url must be http/https with a host when set, and ws_url must be
// ws/wss with a host when set.
func (b *BandConfig) validate() error {
	if b.APIKeyEnv == "" {
		return fmt.Errorf("api_key_env is required")
	}
	if b.RESTURL != "" {
		u, err := url.Parse(b.RESTURL)
		if err != nil {
			return fmt.Errorf("rest_url %q: %w", b.RESTURL, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("rest_url %q must use http or https scheme", b.RESTURL)
		}
		if u.Host == "" {
			return fmt.Errorf("rest_url %q must include a host", b.RESTURL)
		}
	}
	if b.WSURL != "" {
		u, err := url.Parse(b.WSURL)
		if err != nil {
			return fmt.Errorf("ws_url %q: %w", b.WSURL, err)
		}
		if u.Scheme != "ws" && u.Scheme != "wss" {
			return fmt.Errorf("ws_url %q must use ws or wss scheme", b.WSURL)
		}
		if u.Host == "" {
			return fmt.Errorf("ws_url %q must include a host", b.WSURL)
		}
	}
	if b.APIKeyEnv == "" {
		return fmt.Errorf("api_key_env is required")
	}
	return nil
}

// VoiceEnabled reports whether the agent is configured for a voice session:
// a present voice block enables it.
func (a Agent) VoiceEnabled() bool {
	return a.Voice != nil
}

// VoiceConfig is the optional voice object in an agent definition, running
// the agent through AssemblyAI's Voice Agent API. The block's presence
// enables the voice command for that agent. AssemblyAI's managed model runs
// the conversation loop server-side, so the agent's model entry is not used.
type VoiceConfig struct {
	// APIKeyEnv names the environment variable holding the AssemblyAI API
	// key. It is required: there is no sensible default for where it
	// lives.
	APIKeyEnv string `json:"api_key_env"`
	// Greeting is what the agent says on connect, spoken verbatim without
	// the LLM. Optional.
	Greeting string `json:"greeting,omitempty"`
	// Voice is the AssemblyAI voice id. Optional; empty means the API
	// default voice.
	Voice string `json:"voice,omitempty"`
	// Volume is the output volume from 0 to 100. It is a pointer so an
	// explicit 0 (silence) is distinguishable from an absent field, which
	// means the voice's native level.
	Volume *int `json:"volume,omitempty"`
	// InputCommand is the microphone capture command. Optional; when
	// empty DefaultVoiceInputCommand applies.
	InputCommand []string `json:"input_command,omitempty"`
	// OutputCommand is the speaker playback command. Optional; when empty
	// DefaultVoiceOutputCommand applies.
	OutputCommand []string `json:"output_command,omitempty"`
	// WSURL is the Voice Agent WebSocket endpoint. Optional; when empty
	// DefaultVoiceWSURL applies.
	WSURL string `json:"ws_url,omitempty"`
}

// InputCommandOrDefault returns the configured input_command, or
// DefaultVoiceInputCommand when unset.
func (v *VoiceConfig) InputCommandOrDefault() []string {
	if len(v.InputCommand) == 0 {
		return append([]string(nil), DefaultVoiceInputCommand...)
	}
	return append([]string(nil), v.InputCommand...)
}

// OutputCommandOrDefault returns the configured output_command, or
// DefaultVoiceOutputCommand when unset.
func (v *VoiceConfig) OutputCommandOrDefault() []string {
	if len(v.OutputCommand) == 0 {
		return append([]string(nil), DefaultVoiceOutputCommand...)
	}
	return append([]string(nil), v.OutputCommand...)
}

// WSURLOrDefault returns the configured ws_url, or DefaultVoiceWSURL when
// unset.
func (v *VoiceConfig) WSURLOrDefault() string {
	if v.WSURL == "" {
		return DefaultVoiceWSURL
	}
	return v.WSURL
}

// validate checks the voice block: api_key_env is required and non-empty,
// volume when set must be within 0-100, and ws_url must be ws/wss with a
// host when set.
func (v *VoiceConfig) validate() error {
	if v.APIKeyEnv == "" {
		return fmt.Errorf("api_key_env is required")
	}
	if v.Volume != nil && (*v.Volume < 0 || *v.Volume > 100) {
		return fmt.Errorf("volume %d must be between 0 and 100", *v.Volume)
	}
	if v.WSURL != "" {
		u, err := url.Parse(v.WSURL)
		if err != nil {
			return fmt.Errorf("ws_url %q: %w", v.WSURL, err)
		}
		if u.Scheme != "ws" && u.Scheme != "wss" {
			return fmt.Errorf("ws_url %q must use ws or wss scheme", v.WSURL)
		}
		if u.Host == "" {
			return fmt.Errorf("ws_url %q must include a host", v.WSURL)
		}
	}
	return nil
}

// Provider is one named LLM server connection in blorb.json: the
// connection-level facts (type, base_url, api_key_env) one or more models
// share. Type is the discriminator that selects the wire protocol and
// determines which other provider fields are recognized; per-type parsing
// and validation lives here rather than being flattened onto Config so
// future provider types can add their own fields. Models reference
// providers by name; the declarations live once at the top level and are
// shared.
type Provider struct {
	// Name identifies the provider within the config: what models put in
	// their provider field. It must be unique within the config.
	Name string `json:"name"`

	// Type selects the wire protocol: "openai-compatible" for
	// OpenAI-compatible chat completions APIs (base_url is the API root
	// with /chat/completions appended) or "ollama" for Ollama's native
	// /api/chat API (base_url is the bare Ollama server root with
	// /api/chat appended).
	Type string `json:"type"`
	// BaseURL is the server root: /chat/completions or /api/chat is
	// appended per the type above.
	BaseURL string `json:"base_url"`
	// APIKeyEnv names the environment variable holding the API key. It is
	// a pointer so an explicit "api_key_env": "" is distinguishable from
	// an absent field: empty is a config error, absent means no key.
	APIKeyEnv *string `json:"api_key_env,omitempty"`

	// Sampling holds the server-wide generation defaults applied to every
	// model on this provider. Fields left unset mean the server default
	// applies; they are pointers so explicit zeros ("temperature": 0)
	// survive. These are connection facts, not model facts: one server
	// serves one generation configuration. See Sampling.
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"top_p,omitempty"`
	Seed             *int64   `json:"seed,omitempty"`
	Stop             []string `json:"stop,omitempty"`
	MaxTokens        *int     `json:"max_tokens,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
}

// APIKeyEnvOrDefault returns the configured api_key_env, or "" when unset.
func (p *Provider) APIKeyEnvOrDefault() string {
	if p.APIKeyEnv == nil {
		return ""
	}
	return *p.APIKeyEnv
}

// SamplingParams converts the provider's server-wide generation defaults
// into the neutral sampling vocabulary the engine sends on every request.
func (p *Provider) SamplingParams() llm.SamplingParams {
	return llm.SamplingParams{
		Temperature:      p.Temperature,
		TopP:             p.TopP,
		Seed:             p.Seed,
		Stop:             p.Stop,
		MaxTokens:        p.MaxTokens,
		FrequencyPenalty: p.FrequencyPenalty,
		PresencePenalty:  p.PresencePenalty,
	}
}

// Model is one named LLM backend declaration in blorb.json: a provider
// connection (by name) plus the model-level facts that distinguish this
// model within it. Agents reference models by name; the declarations live
// once at the top level and are shared.
type Model struct {
	// Name identifies the model within the config: what agents put in
	// their model field. It is free-form to the config author —
	// it does not feed the API or the agent identity — and must be
	// unique within the config.
	Name string `json:"name"`

	// Provider names the top-level provider entry carrying the connection
	// (type, base_url, api_key_env) this model talks to. It must be a
	// defined provider in the same config (see Config.Provider).
	Provider string `json:"provider"`
	// ModelName is the model identifier sent to the server: for ollama
	// the Ollama tag, for openai-compatible the server's model id. It is
	// required for llm models; a decision model may leave it empty, in
	// which case the wire request omits the model field and the server
	// applies its default.
	ModelName string `json:"model_name,omitempty"`
	// ModelType selects what the model is: ModelTypeLLM (an absent field's
	// meaning) for a chat completions model the agent engine drives in a
	// message loop, or ModelTypeDecision for a decision model that
	// evaluates a state against typed questions and returns typed
	// answers. Only deciders may reference a decision model.
	ModelType string `json:"model_type,omitempty"`
	// Format is Ollama's structured-output setting, valid only on models
	// whose provider's type is ollama: either the JSON string "json" or a
	// JSON schema object. Empty means free-form output.
	Format json.RawMessage `json:"format,omitempty"`
	// KeepAlive is Ollama's how-long-the-model-stays-loaded setting,
	// valid only on models whose provider's type is ollama. Blorb passes
	// the string through verbatim — which duration forms a given server
	// accepts is the server's business, matching the reasoning_effort
	// rule. Empty means the server default applies.
	KeepAlive string `json:"keep_alive,omitempty"`
	// ToolChoice is how the model is steered around tools: "auto" (the
	// default and the absent field's meaning), "none" (calls forbidden
	// for a turn, tools still advertised — keeps the request prefix
	// byte-identical so provider prompt caches keep hitting), "required"
	// (the server forces some tool call), or "force" (the model must
	// call forced_tool). Models on both provider types support the knob.
	ToolChoice string `json:"tool_choice,omitempty"`
	// ForcedTool names the tool the model must call when tool_choice is
	// "force". Required in force mode and an error otherwise. Must match
	// NamePattern.
	ForcedTool string `json:"forced_tool,omitempty"`
	// Logprobs asks the server to report per-token log probabilities of
	// the response's content tokens, surfaced through the run command's
	// --logprobs flag. Models on both provider types support the knob.
	Logprobs bool `json:"logprobs,omitempty"`
	// TopLogprobs is how many top alternative tokens to report per
	// position, in [0, 20], and settable only when logprobs is true. It
	// is a pointer so an explicit "top_logprobs": 0 is distinguishable
	// from an absent field.
	TopLogprobs *int `json:"top_logprobs,omitempty"`
	// ReasoningEffort is the optional thinking effort the backend is
	// asked for. Empty means the server default applies. Accepted values
	// are the union of the OpenAI and Ollama scales; see
	// validateReasoningEffort. Thinking is about the model, not the
	// connection, so it is declared here and not on the provider.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// reasoningEfforts is the accepted set of reasoning_effort values: the
// union of OpenAI's and Ollama's scales. Which values a given model
// actually accepts is the server's business; validation only rejects
// obvious typos.
var reasoningEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// validateReasoningEffort rejects any non-empty reasoning_effort outside
// the accepted value set, naming the allowed values.
func validateReasoningEffort(effort string) error {
	if effort == "" {
		return nil
	}
	if !slices.Contains(reasoningEfforts, effort) {
		return fmt.Errorf("reasoning_effort %q must be one of: %s", effort, strings.Join(reasoningEfforts, ", "))
	}
	return nil
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

	// Fields for type "decider".
	// Decider names the decider this tool evaluates; it must be a
	// defined decider in the same config.
	Decider string `json:"decider,omitempty"`

	// Fields for type "toolset": a reference to another top-level
	// toolset. Valid only inside a toolset's tools list; the granted
	// prefix comes from the referenced toolset's own name.
	Toolset string `json:"toolset,omitempty"`

	// Fields for type "band": the platform tool this entry invokes,
	// one of the band package's fixed names (band_send_message and
	// friends). Band tools are built by the band command, never
	// declared in blorb.json, so validation rejects the type at load.
	Band string `json:"band,omitempty"`
}

// Toolset is a named group of tool declarations an agent can grant by
// naming the toolset in its tools list. Its members are copied and renamed
// with a hyphen-joined prefix at grant time (see Config.AgentTools).
// Absent or empty tools is a valid empty toolset, which grants nothing.
//
// Two kinds exist, selected by Type: simple (the default when the field is
// absent) groups inline tool declarations and references to other
// toolsets; builtin selects a named bundle shipped inside blorb, has no
// per-member declarations, and is configured by one shared Config object.
// Validation normalizes an absent Type to simple, so a loaded toolset
// always carries an explicit kind.
type Toolset struct {
	// Name identifies the toolset within the config and becomes the
	// prefix of every granted member name. It must match NamePattern
	// and be unique among toolsets.
	Name string `json:"name"`
	// Type selects the kind: ToolsetTypeSimple (the absent field's
	// meaning) or ToolsetTypeBuiltin. Validation rejects unknown values.
	Type string `json:"type,omitempty"`
	// Tools lists the tool declarations a simple toolset groups: inline
	// tool entries plus entries that refer to other top-level toolsets
	// by name (type "toolset"). Nesting is allowed; granted prefixes
	// compose along the path. Valid only for simple toolsets.
	Tools []ToolEntry `json:"tools,omitempty"`
	// Builtin names the builtin bundle a builtin toolset wraps, from
	// builtin.SupportedToolsets. Valid only for builtin toolsets.
	Builtin string `json:"builtin,omitempty"`
	// Config is the builtin toolset's shared settings object. Its shape
	// is defined and validated by the selected bundle. It is a
	// RawMessage so it bypasses top-level unknown-field rejection, the
	// same as ToolEntry.Config. Valid only for builtin toolsets.
	Config json.RawMessage `json:"config,omitempty"`
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

// SupportedProviderTypes lists the provider types this build recognizes,
// sorted alphabetically.
func SupportedProviderTypes() []string {
	return []string{ProviderTypeOllama, ProviderTypeOpenAI}
}

// SupportedModelTypes lists the model_type values this build recognizes,
// sorted alphabetically.
func SupportedModelTypes() []string {
	return []string{ModelTypeDecision, ModelTypeLLM}
}

// SupportedQuestionTypes lists the question types this build recognizes,
// sorted alphabetically.
func SupportedQuestionTypes() []string {
	return []string{QuestionTypeChoice, QuestionTypeNoul, QuestionTypeScore}
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

// Provider returns the named provider definition and whether it exists in
// the config. Models reference providers by name; callers resolve the
// named entry before building an LLM client.
func (c Config) Provider(name string) (Provider, bool) {
	for _, p := range c.Providers {
		if p.Name == name {
			return p, true
		}
	}
	return Provider{}, false
}

// Toolset returns the named toolset definition and whether it exists in
// the config.
func (c Config) Toolset(name string) (Toolset, bool) {
	for _, ts := range c.Toolsets {
		if ts.Name == name {
			return ts, true
		}
	}
	return Toolset{}, false
}

// Decider returns the named decider definition and whether it exists in
// the config.
func (c Config) Decider(name string) (Decider, bool) {
	for _, d := range c.Deciders {
		if d.Name == name {
			return d, true
		}
	}
	return Decider{}, false
}

// DefaultAgentName returns the configured default agent name and whether
// one is set.
func (c Config) DefaultAgentName() (string, bool) {
	if c.DefaultAgent == "" {
		return "", false
	}
	return c.DefaultAgent, true
}

// AgentTools returns the tool entries the agent may use, in the agent's
// listed order (not the top-level declaration order), so tool listing
// order is the agent author's. Toolset references expand at their position
// in the list and in the toolset's declared member order, with each
// granted member name prefixed by the toolset path (kb-read, dev-clock-read,
// files-read). An empty result is valid: a no-tools agent, or one granting
// only an empty toolset. Validation guarantees every listed name resolves
// and no granted name repeats, so the result only comes up short for a
// programmatically built config that never went through Validate.
func (c Config) AgentTools(a Agent) []ToolEntry {
	index, err := c.toolReferenceIndex()
	if err != nil {
		return nil
	}
	out, err := c.agentGrantedTools(a, index)
	if err != nil {
		return nil
	}
	return out
}

// expandToolset returns the granted entries of one toolset, with every leaf
// name prefixed by prefix (the toolset path from the grant root) joined by
// hyphens. A simple toolset's inline leaves are copied with rewritten
// names; a toolset-reference entry appends the referenced toolset's name to
// the path and recurses. A builtin toolset synthesizes its members from the
// bundle: the member's own name is the leaf name, its description comes
// from the bundle, and the toolset's shared raw config is copied per
// member. seen holds the toolset path currently being expanded, so an
// unvalidated programmatic cycle grants nothing instead of hanging.
func (c Config) expandToolset(ts Toolset, prefix []string, seen map[string]bool) ([]ToolEntry, error) {
	if seen[ts.Name] {
		return nil, nil
	}
	seen[ts.Name] = true
	defer delete(seen, ts.Name)

	if ts.Type == ToolsetTypeBuiltin {
		members, ok := builtin.ToolsetMembers(ts.Builtin)
		if !ok {
			return nil, fmt.Errorf("toolset %q: unknown builtin toolset %q", ts.Name, ts.Builtin)
		}
		out := make([]ToolEntry, 0, len(members))
		for _, m := range members {
			out = append(out, ToolEntry{
				Type:        ToolTypeBuiltin,
				Name:        grantedName(prefix, m.Name),
				Description: m.Description,
				Builtin:     m.Name,
				Config:      append([]byte(nil), ts.Config...),
			})
		}
		return out, nil
	}

	var out []ToolEntry
	for _, e := range ts.Tools {
		if e.Type == ToolTypeToolset {
			ref, ok := c.Toolset(e.Toolset)
			if !ok {
				continue
			}
			child, err := c.expandToolset(ref, append(append([]string(nil), prefix...), ref.Name), seen)
			if err != nil {
				return nil, err
			}
			out = append(out, child...)
			continue
		}
		leaf := e
		leaf.Name = grantedName(prefix, e.Name)
		out = append(out, leaf)
	}
	return out, nil
}

// grantedName joins a grant path and a leaf name with the hyphen separator
// the LLM API requires.
func grantedName(prefix []string, leaf string) string {
	return strings.Join(append(append([]string(nil), prefix...), leaf), "-")
}

// toolReferenceIndex builds the agent reference space: each top-level tool
// name maps to its entry, each toolset name maps to its full expansion, and
// every granted member name maps to its entry. A duplicate key anywhere is
// an error, so tools, toolsets, and granted member names share one unique
// namespace. This also catches hyphen-composition collisions: toolset "a-b"
// containing "c" and toolset "a" referring to toolset "b" each grant
// "a-b-c", and both paths land on the same key.
func (c Config) toolReferenceIndex() (map[string][]ToolEntry, error) {
	index := make(map[string][]ToolEntry)
	add := func(name string, entries ...ToolEntry) error {
		if _, ok := index[name]; ok {
			return fmt.Errorf("duplicate name %q (tools, toolsets, and toolset members share one reference space)", name)
		}
		index[name] = entries
		return nil
	}
	for _, t := range c.Tools {
		if err := add(t.Name, t); err != nil {
			return nil, err
		}
	}
	for _, ts := range c.Toolsets {
		expanded, err := c.expandToolset(ts, []string{ts.Name}, map[string]bool{})
		if err != nil {
			return nil, err
		}
		if err := add(ts.Name, expanded...); err != nil {
			return nil, err
		}
		for _, e := range expanded {
			if err := add(e.Name, e); err != nil {
				return nil, err
			}
		}
	}
	return index, nil
}

// agentGrantedTools expands an agent's tools list against the reference
// index into the granted entries, in the agent's listed order (a toolset
// reference expanding at its position in the toolset's declared member
// order). Any granted name appearing twice is an error: an exact repeat, a
// toolset granted alongside one of its members, and any other overlap.
func (c Config) agentGrantedTools(a Agent, index map[string][]ToolEntry) ([]ToolEntry, error) {
	var out []ToolEntry
	seen := make(map[string]struct{})
	for _, name := range a.Tools {
		for _, e := range index[name] {
			if _, ok := seen[e.Name]; ok {
				return nil, fmt.Errorf("agent %q: duplicate tool %q", a.Name, e.Name)
			}
			seen[e.Name] = struct{}{}
			out = append(out, e)
		}
	}
	return out, nil
}

// Validate checks all required fields and value constraints. Providers
// validate first: models depend on them, and the model checks need the
// providers in scope.
func (c *Config) Validate() error {
	if c.Providers == nil {
		return fmt.Errorf("providers is required")
	}
	if len(c.Providers) == 0 {
		return fmt.Errorf("providers must not be empty")
	}
	if err := validateUniqueProviderNames(c.Providers); err != nil {
		return err
	}
	for _, p := range c.Providers {
		if err := p.validate(); err != nil {
			return fmt.Errorf("provider %q: %w", p.Name, err)
		}
	}
	if c.Models == nil {
		return fmt.Errorf("models is required")
	}
	if len(c.Models) == 0 {
		return fmt.Errorf("models must not be empty")
	}
	for _, m := range c.Models {
		if err := m.validate(c.Providers); err != nil {
			return fmt.Errorf("model %q: %w", m.Name, err)
		}
	}
	if err := validateUniqueModelNames(c.Models); err != nil {
		return err
	}
	for _, d := range c.Deciders {
		if err := d.validate(c.Models); err != nil {
			return fmt.Errorf("decider %q: %w", d.Name, err)
		}
	}
	if err := validateUniqueDeciderNames(c.Deciders); err != nil {
		return err
	}
	if c.Agents == nil {
		return fmt.Errorf("agents is required")
	}
	if len(c.Agents) == 0 {
		return fmt.Errorf("agents must not be empty")
	}
	for _, t := range c.Tools {
		if err := t.validate(c.dir, false); err != nil {
			return fmt.Errorf("tool %q: %w", t.Name, err)
		}
	}
	if err := validateUniqueToolNames(c.Tools); err != nil {
		return err
	}
	for i := range c.Toolsets {
		if err := c.Toolsets[i].validate(c.dir); err != nil {
			return err
		}
	}
	if err := validateUniqueToolsetNames(c.Toolsets); err != nil {
		return err
	}
	if err := c.validateToolsetRefs(); err != nil {
		return err
	}
	if err := c.validateToolsetCycles(); err != nil {
		return err
	}
	// The reference index is built once, after every toolset is validated,
	// so a collision across the whole reference space surfaces before any
	// agent error.
	index, err := c.toolReferenceIndex()
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(c.Agents))
	for _, a := range c.Agents {
		if err := a.validate(c.Models, index); err != nil {
			return err
		}
		if _, err := c.agentGrantedTools(a, index); err != nil {
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
	if err := c.validateDeciderRefs(); err != nil {
		return err
	}
	if err := c.validateJudgeRefs(); err != nil {
		return err
	}
	if err := c.validateAgentCycles(index); err != nil {
		return err
	}
	return nil
}

// validateSubagentRefs checks that every subagent tool entry names a
// defined agent, at the top level and inside every toolset. Runs after
// per-agent validation so all agent names are known.
func (c *Config) validateSubagentRefs() error {
	for _, t := range c.Tools {
		if t.Type != ToolTypeSubagent {
			continue
		}
		if _, ok := c.Agent(t.Agent); !ok {
			return fmt.Errorf("tool %q: agent %q is not a defined agent", t.Name, t.Agent)
		}
	}
	for _, ts := range c.Toolsets {
		for _, t := range ts.Tools {
			if t.Type != ToolTypeSubagent {
				continue
			}
			if _, ok := c.Agent(t.Agent); !ok {
				return fmt.Errorf("toolset %q: agent %q is not a defined agent", ts.Name, t.Agent)
			}
		}
	}
	return nil
}

// validateDeciderRefs checks that every decider tool entry names a defined
// decider, at the top level and inside every toolset. No cycle detection is
// needed: deciders hold no tools and nothing recurses through them.
func (c *Config) validateDeciderRefs() error {
	for _, t := range c.Tools {
		if t.Type != ToolTypeDecider {
			continue
		}
		if _, ok := c.Decider(t.Decider); !ok {
			return fmt.Errorf("tool %q: decider %q is not a defined decider", t.Name, t.Decider)
		}
	}
	for _, ts := range c.Toolsets {
		for _, t := range ts.Tools {
			if t.Type != ToolTypeDecider {
				continue
			}
			if _, ok := c.Decider(t.Decider); !ok {
				return fmt.Errorf("toolset %q: decider %q is not a defined decider", ts.Name, t.Decider)
			}
		}
	}
	return nil
}

// validateJudgeRefs checks that every agent's judges name defined
// agents. Runs after per-agent validation so all agent names are known.
func (c *Config) validateJudgeRefs() error {
	for _, a := range c.Agents {
		for _, j := range a.Judges {
			if _, ok := c.Agent(j.Agent); !ok {
				return fmt.Errorf("agent %q: judge %q is not a defined agent", a.Name, j.Agent)
			}
		}
	}
	return nil
}

// validateAgentCycles builds the agent delegation graph - an edge from
// an agent to each subagent tool target it is granted (expanded through
// toolsets), plus an edge from an agent to each of its judges (all
// timings: judges recurse agent-to-agent regardless of when they run) -
// and rejects any cycle, including self-reference. A cycle anywhere is
// fatal: both delegation kinds recurse agent-to-agent, so depth would
// otherwise be unbounded. index is the already-built reference space.
func (c *Config) validateAgentCycles(index map[string][]ToolEntry) error {
	edges := make(map[string][]string, len(c.Agents))
	for _, a := range c.Agents {
		granted, err := c.agentGrantedTools(a, index)
		if err != nil {
			return err
		}
		for _, t := range granted {
			if t.Type == ToolTypeSubagent {
				edges[a.Name] = append(edges[a.Name], t.Agent)
			}
		}
		for _, j := range a.Judges {
			edges[a.Name] = append(edges[a.Name], j.Agent)
		}
	}

	return detectCycle(agentNames(c.Agents), edges, "agent")
}

// validateToolsetCycles rejects any cycle among toolset references,
// including self-reference. Cycles are fatal because expansion at grant
// time would otherwise recurse forever. References are known to resolve:
// validateToolsetRefs runs first.
func (c *Config) validateToolsetCycles() error {
	edges := make(map[string][]string, len(c.Toolsets))
	for _, ts := range c.Toolsets {
		for _, t := range ts.Tools {
			if t.Type == ToolTypeToolset {
				edges[ts.Name] = append(edges[ts.Name], t.Toolset)
			}
		}
	}

	nodes := make([]string, len(c.Toolsets))
	for i, ts := range c.Toolsets {
		nodes[i] = ts.Name
	}
	return detectCycle(nodes, edges, "toolset")
}

// agentNames returns the names of the agents in declaration order.
func agentNames(agents []Agent) []string {
	names := make([]string, len(agents))
	for i, a := range agents {
		names[i] = a.Name
	}
	return names
}

// detectCycle walks the directed graph given by edges from the nodes in
// order and returns an error naming the first cycle found, including a
// self-reference. label prefixes the message ("agent", "toolset"). A node
// absent from edges is a leaf.
func detectCycle(nodes []string, edges map[string][]string, label string) error {
	const (
		white = 0 // unvisited
		gray  = 1 // on the current DFS path
		black = 2 // fully explored
	)
	color := make(map[string]int, len(nodes))
	var stack []string

	var visit func(node string) error
	visit = func(node string) error {
		color[node] = gray
		stack = append(stack, node)
		for _, next := range edges[node] {
			switch color[next] {
			case white:
				if err := visit(next); err != nil {
					return err
				}
			case gray:
				// Found a cycle: report the path from its first
				// occurrence on the current stack.
				start := 0
				for i, n := range stack {
					if n == next {
						start = i
						break
					}
				}
				path := append(append([]string(nil), stack[start:]...), next)
				quoted := make([]string, len(path))
				for i, n := range path {
					quoted[i] = fmt.Sprintf("%q", n)
				}
				return fmt.Errorf("%s cycle detected: %s", label, strings.Join(quoted, " -> "))
			}
		}
		stack = stack[:len(stack)-1]
		color[node] = black
		return nil
	}

	for _, n := range nodes {
		if color[n] == white {
			if err := visit(n); err != nil {
				return err
			}
		}
	}
	return nil
}

// validate checks one toolset definition: its name, its kind, and its kind's
// fields. dir anchors builtin base_dir resolution; see ToolEntry.validate.
// An absent type is normalized in place to simple.
func (t *Toolset) validate(dir string) error {
	if t.Name == "" {
		return fmt.Errorf("toolset name is required")
	}
	if !NamePattern.MatchString(t.Name) {
		return fmt.Errorf("toolset name %q must match %s", t.Name, NamePattern)
	}
	if t.Type == "" {
		t.Type = ToolsetTypeSimple
	}
	switch t.Type {
	case ToolsetTypeSimple:
		if t.Builtin != "" {
			return fmt.Errorf("toolset %q: builtin is not valid for simple toolsets", t.Name)
		}
		if len(t.Config) > 0 {
			return fmt.Errorf("toolset %q: config is not valid for simple toolsets", t.Name)
		}
		for i := range t.Tools {
			if err := t.Tools[i].validate(dir, true); err != nil {
				return fmt.Errorf("toolset %q: %w", t.Name, err)
			}
		}
	case ToolsetTypeBuiltin:
		if t.Builtin == "" {
			return fmt.Errorf("toolset %q: builtin is required (one of: %s)", t.Name, strings.Join(builtin.SupportedToolsets(), ", "))
		}
		if !slices.Contains(builtin.SupportedToolsets(), t.Builtin) {
			return fmt.Errorf("toolset %q: unknown builtin toolset %q (supported: %s)", t.Name, t.Builtin, strings.Join(builtin.SupportedToolsets(), ", "))
		}
		if err := builtin.ParseToolsetConfig(t.Builtin, t.Config, builtin.ParseOptions{BaseDir: dir}); err != nil {
			return fmt.Errorf("toolset %q: %w", t.Name, err)
		}
		if len(t.Tools) > 0 {
			return fmt.Errorf("toolset %q: tools is not valid for builtin toolsets", t.Name)
		}
	default:
		return fmt.Errorf("toolset %q: unknown toolset type %q (supported: builtin, simple)", t.Name, t.Type)
	}
	return nil
}

// validateToolsetRefs checks that every toolset-reference entry names a
// defined toolset. Runs after toolset name validation.
func (c *Config) validateToolsetRefs() error {
	for _, ts := range c.Toolsets {
		for _, t := range ts.Tools {
			if t.Type != ToolTypeToolset {
				continue
			}
			if _, ok := c.Toolset(t.Toolset); !ok {
				return fmt.Errorf("toolset %q: unknown toolset %q", ts.Name, t.Toolset)
			}
		}
	}
	return nil
}

// validate checks one agent definition: its name, settings, its band block
// when present, and that the model it names is defined and every tool,
// toolset, or toolset member it lists resolves against the reference index.
// models is the already-validated top-level model list the agent references
// by name; index is the reference space built from the top-level tools and
// toolsets.
func (a *Agent) validate(models []Model, index map[string][]ToolEntry) error {
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
	if idx := slices.IndexFunc(models, func(m Model) bool { return m.Name == a.Model }); models[idx].ResolvedModelType() != ModelTypeLLM {
		return fmt.Errorf("agent %q: model %q is a decision model; agents require an llm model", a.Name, a.Model)
	}
	if a.MaxTurns < 1 {
		return fmt.Errorf("agent %q: max_turns must be at least 1 (got %d)", a.Name, a.MaxTurns)
	}
	for _, name := range a.Tools {
		if _, ok := index[name]; !ok {
			return fmt.Errorf("agent %q: unknown tool, toolset, or toolset member %q", a.Name, name)
		}
	}
	seenJudges := make(map[string]struct{}, len(a.Judges))
	for _, j := range a.Judges {
		if j.Agent == "" {
			return fmt.Errorf("agent %q: judge name is required", a.Name)
		}
		if _, ok := seenJudges[j.Agent]; ok {
			return fmt.Errorf("agent %q: duplicate judge %q", a.Name, j.Agent)
		}
		seenJudges[j.Agent] = struct{}{}
		when := j.WhenOrDefault()
		if !slices.Contains(supportedJudgeWhens(), when) {
			return fmt.Errorf("agent %q: judge %q: unknown when %q (supported: %s)", a.Name, j.Agent, j.When, strings.Join(supportedJudgeWhens(), ", "))
		}
	}
	if a.Band != nil {
		if err := a.Band.validate(); err != nil {
			return fmt.Errorf("agent %q: band: %w", a.Name, err)
		}
	}
	if a.Voice != nil {
		if err := a.Voice.validate(); err != nil {
			return fmt.Errorf("agent %q: voice: %w", a.Name, err)
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

// validate checks one provider definition: its name, type, and the
// connection fields the type requires.
func (p *Provider) validate() error {
	if p.Name == "" {
		return fmt.Errorf("name is required")
	}
	if !NamePattern.MatchString(p.Name) {
		return fmt.Errorf("provider name %q must match %s", p.Name, NamePattern)
	}
	switch p.Type {
	case ProviderTypeOpenAI, ProviderTypeOllama:
		if err := validateEndpointProvider(p); err != nil {
			return err
		}
		return validateSampling(p)
	default:
		return fmt.Errorf("unknown type %q (supported: %v)", p.Type, SupportedProviderTypes())
	}
}

// validateSampling checks the provider's server-wide generation defaults:
// temperature >= 0, top_p in (0, 1], seed >= 0, max_tokens >= 1, penalties
// in [-2, 2], and every stop entry non-empty.
func validateSampling(p *Provider) error {
	if p.Temperature != nil && *p.Temperature < 0 {
		return fmt.Errorf("temperature %v must be at least 0", *p.Temperature)
	}
	if p.TopP != nil && (*p.TopP <= 0 || *p.TopP > 1) {
		return fmt.Errorf("top_p %v must be in (0, 1]", *p.TopP)
	}
	if p.Seed != nil && *p.Seed < 0 {
		return fmt.Errorf("seed %d must be at least 0", *p.Seed)
	}
	if p.MaxTokens != nil && *p.MaxTokens < 1 {
		return fmt.Errorf("max_tokens %d must be at least 1", *p.MaxTokens)
	}
	if p.FrequencyPenalty != nil && (*p.FrequencyPenalty < -2 || *p.FrequencyPenalty > 2) {
		return fmt.Errorf("frequency_penalty %v must be in [-2, 2]", *p.FrequencyPenalty)
	}
	if p.PresencePenalty != nil && (*p.PresencePenalty < -2 || *p.PresencePenalty > 2) {
		return fmt.Errorf("presence_penalty %v must be in [-2, 2]", *p.PresencePenalty)
	}
	for _, s := range p.Stop {
		if s == "" {
			return fmt.Errorf("stop entries must not be empty")
		}
	}
	return nil
}

// validateEndpointProvider checks the fields shared by the endpoint-backed
// provider types (openai-compatible, ollama): an http(s) base_url with a
// host and the api_key_env pointer rule. It does not check the name or type.
func validateEndpointProvider(p *Provider) error {
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
	return nil
}

// validate checks one model definition: its name, that the provider it
// names is defined, its model_name, the reasoning_effort value set, and
// the provider-type-gated knobs (format). providers is the
// already-validated top-level list the model references by name.
func (m *Model) validate(providers []Provider) error {
	if m.Name == "" {
		return fmt.Errorf("name is required")
	}
	if m.Provider == "" {
		return fmt.Errorf("provider is required")
	}
	idx := slices.IndexFunc(providers, func(p Provider) bool { return p.Name == m.Provider })
	if idx < 0 {
		return fmt.Errorf("provider %q is not a defined provider", m.Provider)
	}
	if !slices.Contains(SupportedModelTypes(), m.ResolvedModelType()) {
		return fmt.Errorf("model_type %q must be one of: %s", m.ModelType, strings.Join(SupportedModelTypes(), ", "))
	}
	if m.ResolvedModelType() == ModelTypeDecision {
		return m.validateDecision(providers[idx])
	}
	if m.ModelName == "" {
		return fmt.Errorf("model_name is required")
	}
	if err := validateReasoningEffort(m.ReasoningEffort); err != nil {
		return err
	}
	if err := validateFormat(m.Format, providers[idx].Type); err != nil {
		return err
	}
	if err := validateKeepAlive(m.KeepAlive, providers[idx].Type); err != nil {
		return err
	}
	if err := validateToolChoice(m); err != nil {
		return err
	}
	return validateLogprobs(m)
}

// validateDecision checks a decision model's fields: the llm-only knobs
// are rejected outright, model_name is optional, and the provider must be
// openai-compatible (a decision model needs a plain base_url plus bearer
// key; the ollama provider type has no decision surface).
func (m *Model) validateDecision(provider Provider) error {
	if provider.Type != ProviderTypeOpenAI {
		return fmt.Errorf("model_type %q requires an openai-compatible provider", ModelTypeDecision)
	}
	if m.ReasoningEffort != "" {
		return fmt.Errorf("reasoning_effort is not valid for decision models")
	}
	if len(m.Format) > 0 {
		return fmt.Errorf("format is not valid for decision models")
	}
	if m.KeepAlive != "" {
		return fmt.Errorf("keep_alive is not valid for decision models")
	}
	if m.ToolChoice != "" || m.ForcedTool != "" {
		return fmt.Errorf("tool_choice is not valid for decision models")
	}
	if m.Logprobs {
		return fmt.Errorf("logprobs is not valid for decision models")
	}
	if m.TopLogprobs != nil {
		return fmt.Errorf("top_logprobs is not valid for decision models")
	}
	return nil
}

// validate checks one decider definition: its name, that the model it
// names is a defined decision model, and that its questions are
// well-formed. models is the already-validated top-level list the decider
// references by name.
func (d *Decider) validate(models []Model) error {
	if d.Name == "" {
		return fmt.Errorf("name is required")
	}
	if !NamePattern.MatchString(d.Name) {
		return fmt.Errorf("name %q must match %s", d.Name, NamePattern)
	}
	if d.Model == "" {
		return fmt.Errorf("model is required")
	}
	idx := slices.IndexFunc(models, func(m Model) bool { return m.Name == d.Model })
	if idx < 0 {
		return fmt.Errorf("model %q is not a defined model", d.Model)
	}
	if models[idx].ResolvedModelType() != ModelTypeDecision {
		return fmt.Errorf("model %q is not a decision model", d.Model)
	}
	if len(d.Questions) == 0 {
		return fmt.Errorf("questions must not be empty")
	}
	for name, q := range d.Questions {
		if err := validateQuestionName(name); err != nil {
			return fmt.Errorf("question %q: %w", name, err)
		}
		if err := q.validate(); err != nil {
			return fmt.Errorf("question %q: %w", name, err)
		}
	}
	return nil
}

// validateQuestionName checks a decider question's name: the wire format
// restricts question names to identifiers, and the server caps them at 64
// characters.
func validateQuestionName(name string) error {
	if !QuestionNamePattern.MatchString(name) {
		return fmt.Errorf("name %q must match %s", name, QuestionNamePattern)
	}
	if len(name) > 64 {
		return fmt.Errorf("name %q must be at most 64 characters", name)
	}
	return nil
}

// validate checks one question: its type, instructions, and type-specific
// criteria. The question name is validated separately (it is the map key,
// not a field); see validateQuestionName.
func (q *Question) validate() error {
	if q.Type == "" {
		return fmt.Errorf("type is required (one of: %s)", strings.Join(SupportedQuestionTypes(), ", "))
	}
	if !slices.Contains(SupportedQuestionTypes(), q.Type) {
		return fmt.Errorf("unknown type %q (supported: %s)", q.Type, strings.Join(SupportedQuestionTypes(), ", "))
	}
	if len(q.Instructions) == 0 {
		return fmt.Errorf("instructions is required")
	}
	if !json.Valid(q.Instructions) {
		return fmt.Errorf("instructions must be valid JSON")
	}
	switch q.Type {
	case QuestionTypeChoice:
		return validateChoiceCriteria(q.Criteria)
	case QuestionTypeScore:
		return validateScoreCriteria(q.Criteria)
	case QuestionTypeNoul:
		if len(q.Criteria) == 0 {
			return nil
		}
		var asAny any
		if err := json.Unmarshal(q.Criteria, &asAny); err != nil {
			return fmt.Errorf("criteria must be valid JSON: %w", err)
		}
		if _, ok := asAny.(map[string]any); !ok {
			return fmt.Errorf("criteria must be a JSON object")
		}
	}
	return nil
}

// validateChoiceCriteria checks a choice question's criteria: required, a
// JSON object with at least two option descriptions.
func validateChoiceCriteria(criteria json.RawMessage) error {
	if len(criteria) == 0 {
		return fmt.Errorf("criteria is required for choice questions")
	}
	var asAny any
	if err := json.Unmarshal(criteria, &asAny); err != nil {
		return fmt.Errorf("criteria must be valid JSON: %w", err)
	}
	obj, ok := asAny.(map[string]any)
	if !ok {
		return fmt.Errorf("criteria must be a JSON object")
	}
	if len(obj) < 2 {
		return fmt.Errorf("choice criteria must have at least two options")
	}
	return nil
}

// validateScoreCriteria checks a score question's criteria: required, a
// JSON array of two to ten level descriptions.
func validateScoreCriteria(criteria json.RawMessage) error {
	if len(criteria) == 0 {
		return fmt.Errorf("criteria is required for score questions")
	}
	var asAny any
	if err := json.Unmarshal(criteria, &asAny); err != nil {
		return fmt.Errorf("criteria must be valid JSON: %w", err)
	}
	arr, ok := asAny.([]any)
	if !ok {
		return fmt.Errorf("criteria must be a JSON array")
	}
	if len(arr) < 2 {
		return fmt.Errorf("score criteria must have at least two levels")
	}
	if len(arr) > 10 {
		return fmt.Errorf("score criteria must have at most ten levels")
	}
	return nil
}

// validateLogprobs checks the logprobs/top_logprobs pair: top_logprobs in
// [0, 20] (OpenAI's cap), and settable only when logprobs is true. Models
// on both provider types support the knob.
func validateLogprobs(m *Model) error {
	if m.TopLogprobs == nil {
		return nil
	}
	if !m.Logprobs {
		return fmt.Errorf("top_logprobs is settable only when logprobs is true")
	}
	if *m.TopLogprobs < 0 || *m.TopLogprobs > 20 {
		return fmt.Errorf("top_logprobs %d must be in [0, 20]", *m.TopLogprobs)
	}
	return nil
}

// validateToolChoice checks the tool_choice/forced_tool pair: tool_choice
// must be one of the four modes; forced_tool is required in force mode and
// an error otherwise, and must match NamePattern. Models on both provider
// types support the knob.
func validateToolChoice(m *Model) error {
	switch m.ToolChoice {
	case "", string(llm.ToolChoiceAuto):
		if m.ForcedTool != "" {
			return fmt.Errorf("forced_tool is only valid when tool_choice is \"force\"")
		}
	case string(llm.ToolChoiceNone), string(llm.ToolChoiceRequired):
		if m.ForcedTool != "" {
			return fmt.Errorf("forced_tool is only valid when tool_choice is \"force\"")
		}
	case string(llm.ToolChoiceForce):
		if m.ForcedTool == "" {
			return fmt.Errorf("forced_tool is required when tool_choice is \"force\"")
		}
		if !NamePattern.MatchString(m.ForcedTool) {
			return fmt.Errorf("forced_tool %q must match %s", m.ForcedTool, NamePattern)
		}
	default:
		return fmt.Errorf("tool_choice %q must be one of: %s", m.ToolChoice, strings.Join(SupportedToolChoiceModes(), ", "))
	}
	return nil
}

// ResolvedToolChoice resolves the model's tool_choice/forced_tool pair into the
// neutral vocabulary: nil for auto (the absent field's meaning), the mode
// (with the forced tool named) otherwise. Absent and explicit "auto" are
// the same thing.
func (m *Model) ResolvedToolChoice() *llm.ToolChoice {
	if m.ToolChoice == "" || m.ToolChoice == string(llm.ToolChoiceAuto) {
		return nil
	}
	return &llm.ToolChoice{Mode: llm.ToolChoiceMode(m.ToolChoice), ForceTool: m.ForcedTool}
}

// TopLogprobsOrDefault returns the configured top_logprobs, or 0 when unset
// (the wire field is omitted and the server reports only the chosen token).
func (m *Model) TopLogprobsOrDefault() int {
	if m.TopLogprobs == nil {
		return 0
	}
	return *m.TopLogprobs
}

// ResolvedModelType returns the model's model_type, or ModelTypeLLM when
// unset.
func (m *Model) ResolvedModelType() string {
	if m.ModelType == "" {
		return ModelTypeLLM
	}
	return m.ModelType
}

// SupportedToolChoiceModes lists the tool_choice modes, sorted
// alphabetically.
func SupportedToolChoiceModes() []string {
	return []string{"auto", "force", "none", "required"}
}

// validateKeepAlive checks the ollama-only keep_alive knob: the string
// passes through verbatim, so only the provider-type gate applies
// (Ollama-only means rejected, not ignored).
func validateKeepAlive(keepAlive, providerType string) error {
	if keepAlive == "" {
		return nil
	}
	if providerType != ProviderTypeOllama {
		return fmt.Errorf("keep_alive is not valid for models on %s providers", providerType)
	}
	return nil
}

// validateFormat checks the ollama-only format knob: the value must be
// either the JSON string "json" or a JSON schema object, and it is
// rejected outright on models whose provider is not ollama (Ollama-only
// means rejected, not ignored).
func validateFormat(format json.RawMessage, providerType string) error {
	if len(format) == 0 {
		return nil
	}
	if providerType != ProviderTypeOllama {
		return fmt.Errorf("format is not valid for models on %s providers", providerType)
	}
	var asAny any
	if err := json.Unmarshal(format, &asAny); err != nil {
		return fmt.Errorf("format must be valid JSON: %w", err)
	}
	switch v := asAny.(type) {
	case string:
		if v != "json" {
			return fmt.Errorf("format must be \"json\" or a JSON schema object (got the string %q)", v)
		}
	case map[string]any:
		return nil
	default:
		return fmt.Errorf("format must be \"json\" or a JSON schema object (got %v)", v)
	}
	return nil
}

// SupportedToolTypes lists the tool types this build recognizes at the top
// level, sorted alphabetically. The toolset-reference type is valid only
// inside a toolset; see supportedToolEntryTypes.
func SupportedToolTypes() []string {
	return []string{string(ToolTypeBuiltin), string(ToolTypeCommand), string(ToolTypeDecider), string(ToolTypeSubagent)}
}

// supportedToolEntryTypes lists the tool types valid in the given location,
// sorted alphabetically: the registry types everywhere, plus the
// toolset-reference type inside a toolset's tools list.
func supportedToolEntryTypes(inToolset bool) []string {
	if inToolset {
		return []string{string(ToolTypeBuiltin), string(ToolTypeCommand), string(ToolTypeDecider), string(ToolTypeSubagent), string(ToolTypeToolset)}
	}
	return SupportedToolTypes()
}

// validate checks one tool entry. inToolset distinguishes a top-level tools
// entry from one declared inside a toolset: the toolset-reference type is
// valid only in the latter. dir anchors builtin base_dir resolution.
func (t *ToolEntry) validate(dir string, inToolset bool) error {
	if t.Type == "" {
		return fmt.Errorf("type is required (one of: %s)", strings.Join(supportedToolEntryTypes(inToolset), ", "))
	}
	if t.Type == ToolTypeToolset {
		return t.validateToolsetRef(inToolset)
	}
	// The band type is deliberately absent from SupportedToolTypes: band
	// tools exist only inside the band command's runtime. Name the fact
	// so a user writing one by hand is told where it belongs.
	if t.Type == ToolTypeBand {
		return fmt.Errorf("band tools are wired by the band command")
	}
	if !slices.Contains(SupportedToolTypes(), string(t.Type)) {
		return fmt.Errorf("unknown tool type %q (supported: %s)", t.Type, strings.Join(supportedToolEntryTypes(inToolset), ", "))
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
	if t.Band != "" {
		return fmt.Errorf("band is not valid for %s tools", t.Type)
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
	case ToolTypeDecider:
		if t.Decider == "" {
			return fmt.Errorf("decider is required")
		}
		if !NamePattern.MatchString(t.Decider) {
			return fmt.Errorf("decider %q must match %s", t.Decider, NamePattern)
		}
		if len(t.ArgsSchema) > 0 && !json.Valid(t.ArgsSchema) {
			return fmt.Errorf("args_schema must be valid JSON")
		}
		if len(t.Command) > 0 {
			return fmt.Errorf("command is not valid for decider tools")
		}
		if t.Builtin != "" {
			return fmt.Errorf("builtin is not valid for decider tools")
		}
		if len(t.Config) > 0 {
			return fmt.Errorf("config is not valid for decider tools")
		}
		if t.Agent != "" {
			return fmt.Errorf("agent is not valid for decider tools")
		}
	}
	return nil
}

// validateToolsetRef checks a toolset-reference entry: valid only inside a
// toolset, it names the referenced toolset and carries no other fields.
func (t *ToolEntry) validateToolsetRef(inToolset bool) error {
	if !inToolset {
		return fmt.Errorf("toolset entries are valid only inside a toolset")
	}
	if t.Toolset == "" {
		return fmt.Errorf("toolset is required")
	}
	if !NamePattern.MatchString(t.Toolset) {
		return fmt.Errorf("toolset %q must match %s", t.Toolset, NamePattern)
	}
	if t.Name != "" {
		return fmt.Errorf("name is not valid for toolset entries")
	}
	if t.Description != "" {
		return fmt.Errorf("description is not valid for toolset entries")
	}
	if len(t.Command) > 0 {
		return fmt.Errorf("command is not valid for toolset entries")
	}
	if len(t.ArgsSchema) > 0 {
		return fmt.Errorf("args_schema is not valid for toolset entries")
	}
	if t.Builtin != "" {
		return fmt.Errorf("builtin is not valid for toolset entries")
	}
	if len(t.Config) > 0 {
		return fmt.Errorf("config is not valid for toolset entries")
	}
	if t.Agent != "" {
		return fmt.Errorf("agent is not valid for toolset entries")
	}
	if t.Band != "" {
		return fmt.Errorf("band is not valid for toolset entries")
	}
	return nil
}

func validateUniqueToolsetNames(toolsets []Toolset) error {
	seen := make(map[string]struct{}, len(toolsets))
	for _, ts := range toolsets {
		if _, ok := seen[ts.Name]; ok {
			return fmt.Errorf("duplicate toolset name %q", ts.Name)
		}
		seen[ts.Name] = struct{}{}
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

func validateUniqueDeciderNames(deciders []Decider) error {
	seen := make(map[string]struct{}, len(deciders))
	for _, d := range deciders {
		if _, ok := seen[d.Name]; ok {
			return fmt.Errorf("duplicate decider name %q", d.Name)
		}
		seen[d.Name] = struct{}{}
	}
	return nil
}

func validateUniqueProviderNames(providers []Provider) error {
	seen := make(map[string]struct{}, len(providers))
	for _, p := range providers {
		if _, ok := seen[p.Name]; ok {
			return fmt.Errorf("duplicate provider name %q", p.Name)
		}
		seen[p.Name] = struct{}{}
	}
	return nil
}
