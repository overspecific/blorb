package engine

import (
	"context"
	"fmt"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/logging"
	"github.com/overspecific/blorb/internal/tools"
)

// SubagentRunnerConfig configures a SubagentRunner.
type SubagentRunnerConfig struct {
	// Config is the whole loaded config: subagents, their tool grants,
	// and their models are resolved from it.
	Config config.Config
	// NewClient builds the LLM client for a subagent's named model.
	NewClient func(cfg config.Config, agent config.Agent) (llm.Client, error)
	// Stream enables streaming in subagent engines; clients that do not
	// implement llm.StreamingClient fall back to whole messages.
	Stream bool
	// Sink receives the nested engines' wire logs.
	Sink logging.Sink
}

// SubagentRunner executes named agents from one config as subagents. It
// implements tools.SubagentRunner.
type SubagentRunner struct {
	cfg SubagentRunnerConfig
}

// NewSubagentRunner constructs a SubagentRunner. The runner wires itself
// into every subagent registry it builds, so subagents can themselves use
// subagent tools (config validation rejects cycles).
func NewSubagentRunner(cfg SubagentRunnerConfig) *SubagentRunner {
	return &SubagentRunner{cfg: cfg}
}

// RunSubagent runs the named agent to completion for one user message —
// resolving all of its tool calls, exactly like a chat turn — and returns
// its concatenated assistant text. Events are reported with Agent set to
// the running agent's name and Depth 0; each subagentTool on the way out
// adds its own Depth increment. See tools.SubagentRunner.
func (r *SubagentRunner) RunSubagent(ctx context.Context, agentName, userMessage string, onEvent func(tools.SubagentEvent) error) (tools.SubagentResult, error) {
	agent, ok := r.cfg.Config.Agent(agentName)
	if !ok {
		return tools.SubagentResult{}, fmt.Errorf("subagent %q is not defined in the config", agentName)
	}

	registry, err := tools.NewRegistry(
		r.cfg.Config.AgentTools(agent),
		tools.WithSink(r.cfg.Sink),
		tools.WithConfigDir(r.cfg.Config.Dir()),
		tools.WithSubagentRunner(r),
		tools.WithSubagentEvents(onEvent),
	)
	if err != nil {
		return tools.SubagentResult{}, fmt.Errorf("build subagent %q tools: %w", agentName, err)
	}
	// Builtins hold per-instance resources (open sandbox roots); release
	// them when the subagent run ends.
	defer registry.Close()

	// The agent's named model resolves against the config and drives the
	// engine's model field (usage records, tracing) and sampling (the
	// model's provider's server-wide generation defaults) below.
	model, ok := r.cfg.Config.Model(agent.Model)
	if !ok {
		return tools.SubagentResult{}, fmt.Errorf("subagent %q: model %q is not a defined model", agentName, agent.Model)
	}
	provider, ok := r.cfg.Config.Provider(model.Provider)
	if !ok {
		return tools.SubagentResult{}, fmt.Errorf("subagent %q: model %q: provider %q is not a defined provider", agentName, agent.Model, model.Provider)
	}

	client, err := r.newClient(agent)
	if err != nil {
		return tools.SubagentResult{}, err
	}

	eng := New(EngineConfig{
		Client:       client,
		Tools:        registry,
		SystemPrompt: agent.SystemPrompt,
		MaxTurns:     agent.MaxTurnsOrDefault(),
		Stream:       r.cfg.Stream,
		AgentName:    agent.Name,
		Model:        model.ModelName,
		Sampling:     provider.SamplingParams(),
	})

	// Usage records are always collected — even when the caller wants no
	// events — so SubagentResult.Usage is populated regardless: the calls
	// happened and their tokens were spent.
	var usageRecords []tools.SubagentUsageRecord
	collect := func(ev Event) error {
		if ev.Kind == EventUsage {
			usageRecords = append(usageRecords, tools.SubagentUsageRecord{
				Agent: agentName,
				Model: ev.Model,
				Usage: ev.Usage,
				Stats: ev.Stats,
			})
		}
		return r.convert(agentName, onEvent)(ev)
	}

	final, err := eng.RunTurn(ctx, userMessage, collect)
	if ctx.Err() != nil {
		// Cancellation (e.g. SIGINT) is infrastructure, not a subagent
		// outcome: surface it as an error so the caller can treat the
		// whole turn as interrupted.
		return tools.SubagentResult{}, fmt.Errorf("subagent %q: %w", agentName, ctx.Err())
	}
	if err != nil {
		// Any other failure — ErrTooManyTurns, provider truncation, API
		// errors — is a subagent-level failure the parent model sees and
		// can react to, like a tool-reported failure. The calls that ran
		// still spent their tokens, so the collected usage travels with
		// the failure.
		return tools.SubagentResult{Output: err.Error(), Err: true, Usage: usageRecords}, nil
	}
	return tools.SubagentResult{Output: final, Err: false, Usage: usageRecords}, nil
}

func (r *SubagentRunner) newClient(agent config.Agent) (llm.Client, error) {
	if r.cfg.NewClient == nil {
		return nil, fmt.Errorf("subagent %q: no client factory configured", agent.Name)
	}
	client, err := r.cfg.NewClient(r.cfg.Config, agent)
	if err != nil {
		return nil, fmt.Errorf("build subagent %q client: %w", agent.Name, err)
	}
	return client, nil
}

// convert adapts engine events to subagent events, tagging each with the
// producing agent's name and Depth 0 (the nested subagentTool adds its own
// increment as the event bubbles out). A callback error aborts the turn.
// When onEvent is nil the conversion still happens — callers use it for
// usage collection — but nothing is forwarded.
func (r *SubagentRunner) convert(agentName string, onEvent func(tools.SubagentEvent) error) func(Event) error {
	return func(ev Event) error {
		out := tools.SubagentEvent{
			Agent:  agentName,
			Depth:  0,
			Text:   ev.Text,
			Name:   ev.Name,
			Args:   ev.Args,
			Index:  ev.Index,
			Output: ev.Output,
			Failed: ev.Failed,
			Model:  ev.Model,
			Usage:  ev.Usage,
			Stats:  ev.Stats,
		}
		switch ev.Kind {
		case EventAssistantText:
			out.Kind = tools.SubagentText
		case EventAssistantThinking:
			out.Kind = tools.SubagentThinking
		case EventAssistantTextDelta:
			out.Kind = tools.SubagentTextDelta
		case EventAssistantThinkingDelta:
			out.Kind = tools.SubagentThinkingDelta
		case EventToolCall:
			out.Kind = tools.SubagentToolCall
		case EventToolCallDelta:
			out.Kind = tools.SubagentToolCallDelta
		case EventToolResult:
			out.Kind = tools.SubagentToolResult
		case EventUsage:
			out.Kind = tools.SubagentUsage
		default:
			return fmt.Errorf("subagent %q: unhandled event kind %d", agentName, ev.Kind)
		}
		if onEvent == nil {
			return nil
		}
		return onEvent(out)
	}
}
