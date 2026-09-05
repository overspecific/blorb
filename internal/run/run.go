// Package run implements the single-run mode: one prompt in, one agent turn out.
package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/overspecific/blorb/internal/chat"
	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/engine"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/logging"
	"github.com/overspecific/blorb/internal/prefactor"
	"github.com/overspecific/blorb/internal/tools"
	"github.com/overspecific/blorb/internal/usage"
)

// Options configure a single run.
type Options struct {
	Config config.Config
	// Agent is the resolved agent definition the run executes: its system
	// prompt, named model, and max turns drive the engine and client.
	Agent  config.Agent
	Stdout io.Writer
	// Stderr receives diagnostics for the plain and ndjson formats:
	// progress, tool activity, and the usage footer. Nil falls back
	// to Stdout, preserving the single-stream behavior. The chat
	// format ignores it and always uses Stdout.
	Stderr io.Writer
	// NewClient overrides the LLM client construction. Tests only; nil
	// builds the real client from the agent's named model. It mirrors
	// chat.Options.NewClient.
	NewClient func(cfg config.Config, agent config.Agent) (llm.Client, error)
	// Getenv overrides the environment lookup used to resolve the
	// model's api_key_env; os.Getenv when nil. Tests only.
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
	// Tracer, when non-nil, records the run to Prefactor: one instance
	// containing one turn span. Tracing is a hard dependency: a tracer
	// error fails the run. Nil disables tracing.
	Tracer *prefactor.Tracer
	// Format selects the run's output format: "chat" (the zero
	// value and default), "plain", or "ndjson". An unknown value
	// fails the run before any LLM call is made.
	Format string
}

// Run executes exactly one agent turn for prompt and returns the final
// assistant text. It builds the client, registry, and engine exactly like
// a chat session would, runs the turn, and releases resources.
func Run(ctx context.Context, opts Options, prompt string) (string, error) {
	if err := opts.validateFormat(); err != nil {
		return "", err
	}

	sink, err := chat.ResolveSink(opts.ConfigPath, opts.Config)
	if err != nil {
		return "", err
	}

	client, err := opts.newClient(sink)
	if err != nil {
		return "", err
	}

	// The resolved top-level model entry backs the tracing wrapper and
	// engine below; the client is built from it. Its provider carries
	// the server-wide generation defaults the engine sends on every
	// request.
	model, ok := opts.Config.Model(opts.Agent.Model)
	if !ok {
		return "", fmt.Errorf("agent %q: model %q is not a defined model", opts.Agent.Name, opts.Agent.Model)
	}
	provider, ok := opts.Config.Provider(model.Provider)
	if !ok {
		return "", fmt.Errorf("model %q: provider %q is not a defined provider", model.Name, model.Provider)
	}

	// The engine suppresses whole-message events when it streams; only
	// claim streaming when the real client can stream (mirrors chat's
	// capability check).
	streaming := false
	if _, ok := client.(llm.StreamingClient); ok {
		streaming = true
	}

	// Usage accounting: run deliberately mirrors chat's unexported
	// wrappers rather than sharing them (same reasoning as newClient and
	// isTerminated). One run is one turn, so the turn footer is the run's
	// whole summary; there is no session line.
	account := &usage.Account{}
	printEvent, onSubagent, flush, finishNDJSON := opts.events(account)
	turnEvent := usageWrap(printEvent, account)
	onSubagent = runUsageWrap(onSubagent, account)

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

	// With a tracer there is no holder (one turn, one wrapper), so the
	// engine is constructed below, after the client is wrapped: sink →
	// client → streaming check (raw client) → registry (needs the
	// streaming flag) → StartSession (needs the registry schema) →
	// StartTurn → wrap client → engine.New → RunTurn.
	var turn *prefactor.Turn
	if opts.Tracer != nil {
		schema := prefactor.DefaultAgentSchemaVersion(opts.Agent.Name, registry)
		if err := opts.Tracer.StartSession(ctx, schema); err != nil {
			if isTerminated(err) {
				fmt.Fprintf(opts.diagnostics(), "stopped by platform: %v\n", terminationReason(err))
				return "", nil
			}
			return "", fmt.Errorf("run: %w", err)
		}

		turn, err = opts.Tracer.StartTurn(ctx, prompt)
		if err != nil {
			if isTerminated(err) {
				fmt.Fprintf(opts.diagnostics(), "stopped by platform: %v\n", terminationReason(err))
				return "", nil
			}
			return "", finishTracedRun(opts, fmt.Errorf("run: %w", err))
		}
		// The streaming check above ran on the raw client; re-asserting on
		// the wrapper would always succeed because tracingClient
		// implements ChatStream unconditionally.
		client = chat.NewTracingClient(client, turn, model.ModelName)
	}

	// The engine is built here (not before the tracing block) so the
	// wrapped client is in place when tracing; see the construction-order
	// comment above.
	eng := engine.New(engine.EngineConfig{
		Client:       client,
		Tools:        registry,
		SystemPrompt: opts.Agent.SystemPrompt,
		MaxTurns:     opts.Agent.MaxTurnsOrDefault(),
		Stream:       opts.Stream && streaming,
		AgentName:    opts.Agent.Name,
		Model:        model.ModelName,
		Sampling:     provider.SamplingParams(),
	})

	// With tracing, engine events map onto the turn's spans before
	// printing; onSubagent stays the registry callback as without tracing.
	if turn != nil {
		turnEvent = chat.TraceEvent(turn, turnEvent)
	}

	final, runErr := eng.RunTurn(ctx, prompt, turnEvent)
	flush()

	// The footer prints for the calls that completed even on the error
	// paths (interrupt, provider failure mid-loop): partial usage, then
	// the outcome. A run cancelled before any call has no records and
	// prints no footer. ndjson suppresses it: the done/error event
	// carries the usage, and a human footer would be noise for a machine
	// consumer.
	if len(account.Records()) > 0 && opts.Format != FormatNDJSON {
		fmt.Fprintf(opts.diagnostics(), "%s\n", usage.FormatTurn(account))
	}

	err = mapTurnOutcome(turn, final, runErr, ctx)
	if err == nil {
		err = finishTracedRun(opts, nil)
	} else {
		err = finishTracedRun(opts, err)
	}

	// The ndjson terminal event is the stream's last word, so it is
	// emitted only after the outcome mapping and session finish have
	// settled the run's result: done means the run exits 0 (platform
	// termination arrives here as err nil with empty final, so it ends
	// the stream with a textless done per the contract), error that it
	// does not — carrying the final run error, which may be a
	// post-turn failure rather than the raw turn error. Emitted iff
	// RunTurn was entered. An emit failure (the sink's only failure
	// mode: a marshal/write bug) becomes the run's error; any earlier
	// error still wins, so it is not masked.
	if finishNDJSON != nil {
		if emitErr := finishNDJSON(final, err); emitErr != nil && err == nil {
			err = emitErr
		}
	}

	if err != nil {
		return "", err
	}
	return final, nil
}

// mapTurnOutcome finishes the turn span to match the turn's outcome and
// returns the run's outcome error (nil on success). A run is intended to
// run to completion, so a user interrupt is a failed run, not a graceful
// outcome: a cancelled ctx arrives as an ordinary turn error and fails the
// turn, deliberately unlike chat.
func mapTurnOutcome(turn *prefactor.Turn, final string, runErr error, ctx context.Context) error {
	if turn == nil {
		if runErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("run: %w", ctx.Err())
		}
		return fmt.Errorf("run: %w", runErr)
	}

	switch {
	case runErr != nil && isTerminated(runErr):
		if err := turn.Fail(runErr); err != nil && !isTerminated(err) {
			// The terminate signal still wins; a span-finish failure
			// cannot override the platform's request to stop.
			_ = err
		}
		return errTerminated{reason: terminationReason(runErr)}
	case runErr != nil:
		if err := turn.Fail(runErr); err != nil {
			return fmt.Errorf("run: %w", err)
		}
		if ctx.Err() != nil {
			return fmt.Errorf("run: %w", ctx.Err())
		}
		return fmt.Errorf("run: %w", runErr)
	default:
		if err := turn.Complete(final); err != nil {
			return fmt.Errorf("run: %w", err)
		}
		return nil
	}
}

// errTerminated marks the platform-termination outcome: the reason has
// been (or will be) reported to the user and the run exits 0.
type errTerminated struct{ reason string }

func (e errTerminated) Error() string { return "terminated: " + e.reason }

// finishTracedRun finishes the Prefactor instance to match the run
// outcome, then returns the run's outcome error (nil on success). On
// platform termination the instance is already marked server-side and a
// second finish would 409, so FinishSession is skipped.
func finishTracedRun(opts Options, runErr error) error {
	if opts.Tracer == nil {
		return runErr
	}
	if term, ok := runErr.(errTerminated); ok {
		fmt.Fprintf(opts.diagnostics(), "stopped by platform: %s\n", term.reason)
		return nil
	}
	status := prefactor.InstanceComplete
	if runErr != nil {
		status = prefactor.InstanceFailed
	}
	// The instance finish uses a background context, like the span
	// finishes and chat's session finish: when the run is cancelled the
	// run ctx is already dead, and a clean exit must still be able to
	// record its terminal state.
	if err := opts.Tracer.FinishSession(context.Background(), status); err != nil {
		if runErr != nil {
			return runErr
		}
		return fmt.Errorf("run: %w", err)
	}
	return runErr
}

// newClient builds the LLM client for the run's agent: the injected
// NewClient factory when set, else the real model path with the injected
// getenv.
func (o Options) newClient(sink logging.Sink) (llm.Client, error) {
	if o.NewClient != nil {
		return o.NewClient(o.Config, o.Agent)
	}
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	return chat.NewClientWithGetenv(o.Config, o.Agent, getenv, sink)
}

// subagentRunner builds the engine-backed runner for the run's config,
// mirroring chat's runner: per-invocation registries and engines for any
// agent in the config. streaming is the run client's capability check
// result, shared with the run engine.
func (o Options) subagentRunner(sink logging.Sink, streaming bool) *engine.SubagentRunner {
	return engine.NewSubagentRunner(engine.SubagentRunnerConfig{
		Config: o.Config,
		NewClient: func(cfg config.Config, agent config.Agent) (llm.Client, error) {
			if o.NewClient != nil {
				return o.NewClient(cfg, agent)
			}
			getenv := o.Getenv
			if getenv == nil {
				getenv = os.Getenv
			}
			return chat.NewClientWithGetenv(cfg, agent, getenv, sink)
		},
		Stream: o.Stream && streaming,
		Sink:   sink,
	})
}

// isTerminated reports whether err is the tracer's termination sentinel.
// Both helpers only use exported prefactor types; chat keeps its copies
// unexported, so run replicates the two small functions (parallel
// structure, like newClient).
func isTerminated(err error) bool {
	return errors.Is(err, prefactor.ErrTerminated)
}

// terminationReason extracts the platform's reason from a termination
// error for display.
func terminationReason(err error) string {
	var te *prefactor.TerminatedError
	if errors.As(err, &te) {
		return te.Reason
	}
	return ""
}
