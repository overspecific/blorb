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
	// and their providers are resolved from it.
	Config config.Config
	// NewClient builds the LLM client for a subagent's provider.
	NewClient func(agent config.Agent) (llm.Client, error)
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
	})

	final, err := eng.RunTurn(ctx, userMessage, r.convert(agentName, onEvent))
	if ctx.Err() != nil {
		// Cancellation (e.g. SIGINT) is infrastructure, not a subagent
		// outcome: surface it as an error so the caller can treat the
		// whole turn as interrupted.
		return tools.SubagentResult{}, fmt.Errorf("subagent %q: %w", agentName, ctx.Err())
	}
	if err != nil {
		// Any other failure — ErrTooManyTurns, provider truncation, API
		// errors — is a subagent-level failure the parent model sees and
		// can react to, like a tool-reported failure.
		return tools.SubagentResult{Output: err.Error(), Err: true}, nil
	}
	return tools.SubagentResult{Output: final, Err: false}, nil
}

func (r *SubagentRunner) newClient(agent config.Agent) (llm.Client, error) {
	if r.cfg.NewClient == nil {
		return nil, fmt.Errorf("subagent %q: no client factory configured", agent.Name)
	}
	client, err := r.cfg.NewClient(agent)
	if err != nil {
		return nil, fmt.Errorf("build subagent %q client: %w", agent.Name, err)
	}
	return client, nil
}

// convert adapts engine events to subagent events, tagging each with the
// producing agent's name and Depth 0 (the nested subagentTool adds its own
// increment as the event bubbles out). A callback error aborts the turn.
func (r *SubagentRunner) convert(agentName string, onEvent func(tools.SubagentEvent) error) func(Event) error {
	if onEvent == nil {
		return nil
	}
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
			out.Kind = tools.SubsubagentToolCall
		case EventToolCallDelta:
			out.Kind = tools.SubsubagentToolCallDelta
		case EventToolResult:
			out.Kind = tools.SubsubagentToolResult
		default:
			return fmt.Errorf("subagent %q: unhandled event kind %d", agentName, ev.Kind)
		}
		return onEvent(out)
	}
}
