// Package run implements the single-run mode: one prompt in, one agent turn out.
package run

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/overspecific/blorb/internal/chat"
	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/engine"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/logging"
	"github.com/overspecific/blorb/internal/tools"
)

// Options configure a single run.
type Options struct {
	Config config.Config
	// Agent is the resolved agent definition the run executes: its system
	// prompt, provider, and max turns drive the engine and client.
	Agent  config.Agent
	Stdout io.Writer
	// NewClient overrides the LLM client construction. Tests only; nil
	// builds the real client from the agent's provider. It mirrors
	// chat.Options.NewClient.
	NewClient func(agent config.Agent) (llm.Client, error)
	// Getenv overrides the environment lookup used to resolve
	// provider.api_key_env; os.Getenv when nil. Tests only.
	Getenv func(string) string
	// Stream enables incremental rendering of assistant responses: when
	// true and the client supports it, text, reasoning, and tool call
	// fragments are printed as they arrive.
	Stream bool
	// ToolOutput enables printing the full body of parent tool results,
	// mirroring chat.Options.ToolOutput.
	ToolOutput bool
	// ConfigPath is the path to the blorb.json that produced Config, with
	// the same file-logging semantics as chat.Options.ConfigPath.
	ConfigPath string
}

// Run executes exactly one agent turn for prompt and returns the final
// assistant text. It builds the client, registry, and engine exactly like
// a chat session would, runs the turn, and releases resources.
func Run(ctx context.Context, opts Options, prompt string) (string, error) {
	sink, err := chat.ResolveSink(opts.ConfigPath, opts.Config)
	if err != nil {
		return "", err
	}

	client, err := opts.newClient(sink)
	if err != nil {
		return "", err
	}

	// The engine suppresses whole-message events when it streams; only
	// claim streaming when the real client can stream (mirrors chat's
	// capability check).
	streaming := false
	if _, ok := client.(llm.StreamingClient); ok {
		streaming = true
	}

	printEvent, onSubagent, flush := chat.Events(opts.Stdout, opts.ToolOutput)

	// One turn, no per-turn printer swap: onSubagent is wired directly.
	registry, err := tools.NewRegistry(opts.Config.AgentTools(opts.Agent),
		tools.WithSink(sink), tools.WithConfigDir(opts.Config.Dir()),
		tools.WithSubagentRunner(opts.subagentRunner(sink, streaming)),
		tools.WithSubagentEvents(onSubagent))
	if err != nil {
		return "", fmt.Errorf("build tools: %w", err)
	}
	// Builtins hold per-instance resources (open sandbox roots) for their
	// lifetime; release them when the run ends.
	defer registry.Close()

	eng := engine.New(engine.EngineConfig{
		Client:       client,
		Tools:        registry,
		SystemPrompt: opts.Agent.SystemPrompt,
		MaxTurns:     opts.Agent.MaxTurnsOrDefault(),
		Stream:       opts.Stream && streaming,
	})

	final, runErr := eng.RunTurn(ctx, prompt, printEvent)
	flush()

	switch {
	case runErr == nil:
		return final, nil
	case ctx.Err() != nil:
		// The turn was cut short by cancellation (e.g. SIGINT mapped to
		// the context): surface the ctx error so the CLI can distinguish
		// interruption from a plain failure.
		return "", fmt.Errorf("run: %w", ctx.Err())
	default:
		return "", fmt.Errorf("run: %w", runErr)
	}
}

// newClient builds the LLM client for the run's agent: the injected
// NewClient factory when set, else the real provider path with the
// injected getenv.
func (o Options) newClient(sink logging.Sink) (llm.Client, error) {
	if o.NewClient != nil {
		return o.NewClient(o.Agent)
	}
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	return chat.NewClientWithGetenv(o.Agent, getenv, sink)
}

// subagentRunner builds the engine-backed runner for the run's config,
// mirroring chat's runner: per-invocation registries and engines for any
// agent in the config. streaming is the run client's capability check
// result, shared with the run engine.
func (o Options) subagentRunner(sink logging.Sink, streaming bool) *engine.SubagentRunner {
	return engine.NewSubagentRunner(engine.SubagentRunnerConfig{
		Config: o.Config,
		NewClient: func(agent config.Agent) (llm.Client, error) {
			if o.NewClient != nil {
				return o.NewClient(agent)
			}
			getenv := o.Getenv
			if getenv == nil {
				getenv = os.Getenv
			}
			return chat.NewClientWithGetenv(agent, getenv, sink)
		},
		Stream: o.Stream && streaming,
		Sink:   sink,
	})
}
