package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/overspecific/blorb/internal/band"
	"github.com/overspecific/blorb/internal/chat"
	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/decide"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/logging"
	"github.com/overspecific/blorb/internal/prefactor"
	"github.com/overspecific/blorb/internal/run"
	"github.com/overspecific/blorb/internal/usage"
)

// version is set at build time via -ldflags "-X main.version=..." (see bin/build).
var version = "dev"

func main() {
	cmd := rootCommand()
	cmd.Version = version
	if err := cmd.Run(context.Background(), os.Args); err != nil {
		// Usage errors and cli.ExitCoder errors have already been reported
		// by cli itself; anything reaching here is silent, so just exit.
		os.Exit(1)
	}
}

// rootCommand builds the top-level blorb command. It is exposed for tests.
func rootCommand() *cli.Command {
	return &cli.Command{
		Name:  "blorb",
		Usage: "a single-binary tool for making AI agents",
		Commands: []*cli.Command{
			chatCommand(),
			runCommand(),
			decideCommand(),
			bandCommand(),
			modelsCommand(),
			{
				Name:   "version",
				Usage:  "Print the version",
				Action: func(ctx context.Context, cmd *cli.Command) error { return printlnVersion(cmd.Root()) },
			},
		},
	}
}

// modelsCommand builds the models subcommand: for each provider in the
// config, list the models its server has installed and mark the provider's
// configured models installed or missing. Typo detection is the point: the
// command exits non-zero when any listing failed or any configured model
// is missing from its server.
func modelsCommand() *cli.Command {
	return &cli.Command{
		Name:  "models",
		Usage: "List the models each provider's server has installed",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Aliases: []string{"c"},
				Value:   config.DefaultPath,
				Usage:   "Path to blorb.json",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			cfg, err := config.Load(cmd.String("config"))
			if err != nil {
				return cli.Exit(fmt.Sprintf("models: %v", err), 1)
			}

			w := cmd.Root().Writer
			exitCode := 0
			for _, provider := range cfg.Providers {
				models := providerModels(cfg, provider.Name)

				fmt.Fprintf(w, "provider %s (%s, %s)\n", provider.Name, provider.Type, provider.BaseURL)

				client, err := chat.NewProviderClient(cfg, provider.Name, os.Getenv, logging.NewNop())
				if err != nil {
					fmt.Fprintf(w, "  error: %v\n", err)
					exitCode = 1
					continue
				}

				lister, ok := client.(llm.ModelLister)
				if !ok {
					fmt.Fprintf(w, "  error: provider type %q cannot list installed models\n", provider.Type)
					exitCode = 1
					continue
				}

				installed, err := lister.ListModels(ctx)
				if err != nil {
					fmt.Fprintf(w, "  error: %v\n", err)
					exitCode = 1
					continue
				}

				// usedBy maps each server model name to the config model
				// entries that name it, so the listing can show which
				// config models use what the server has.
				usedBy := make(map[string][]string, len(models))
				for _, m := range models {
					usedBy[m.ModelName] = append(usedBy[m.ModelName], m.Name)
				}

				installedSet := make(map[string]struct{}, len(installed))
				for _, mi := range installed {
					installedSet[mi.Name] = struct{}{}
					if users := usedBy[mi.Name]; len(users) > 0 {
						fmt.Fprintf(w, "  %s (used by %s)\n", mi.Name, strings.Join(users, ", "))
					} else {
						fmt.Fprintf(w, "  %s\n", mi.Name)
					}
				}
				// A configured model the server does not have is the typo
				// case: name both the wire model_name and the config
				// entry that points at it.
				for _, m := range models {
					if _, ok := installedSet[m.ModelName]; !ok {
						fmt.Fprintf(w, "  %s (NOT INSTALLED; configured as %s)\n", m.ModelName, m.Name)
						exitCode = 1
					}
				}
			}

			if exitCode != 0 {
				return cli.Exit("", 1)
			}
			return nil
		},
	}
}

// providerModels returns the config's model entries that name the given
// provider.
func providerModels(cfg config.Config, providerName string) []config.Model {
	var out []config.Model
	for _, m := range cfg.Models {
		if m.Provider == providerName {
			out = append(out, m)
		}
	}
	return out
}

// chatCommand builds the chat subcommand.
func chatCommand() *cli.Command {
	return &cli.Command{
		Name:  "chat",
		Usage: "Chat with an agent defined in blorb.json",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Aliases: []string{"c"},
				Value:   config.DefaultPath,
				Usage:   "Path to blorb.json",
			},
			&cli.BoolFlag{
				Name:  "no-stream",
				Usage: "Disable streaming of assistant responses",
			},
			&cli.BoolFlag{
				Name:  "tool-output",
				Usage: "Show the full output of tool results (subagent output is always shown)",
			},
			&cli.StringFlag{
				Name:  "agent",
				Usage: "Name of the agent to chat with; defaults to the config's default_agent",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			cfg, err := config.Load(cmd.String("config"))
			if err != nil {
				return cli.Exit(fmt.Sprintf("chat: %v", err), 1)
			}

			agent, err := resolveAgent(cfg, cmd.String("agent"))
			if err != nil {
				return cli.Exit(fmt.Sprintf("chat: %v", err), 1)
			}

			var tracer *prefactor.Tracer
			if cfg.PrefactorEnabled() {
				tracer, err = buildPrefactorTracer(ctx, cfg, agent)
				if err != nil {
					return cli.Exit(fmt.Sprintf("chat: %v", err), 1)
				}
			}

			err = chat.Run(ctx, chat.Options{
				Config:     cfg,
				Agent:      agent,
				Version:    cmd.Root().Version,
				Stdin:      os.Stdin,
				Stdout:     os.Stdout,
				Stream:     !cmd.Bool("no-stream"),
				ToolOutput: cmd.Bool("tool-output"),
				ConfigPath: cmd.String("config"),
				Tracer:     tracer,
			})
			if err != nil {
				return cli.Exit(fmt.Sprintf("chat: %v", err), 1)
			}
			return nil
		},
	}
}

// runCommand builds the run subcommand: one prompt, one agent turn, exit.
// The prompt is a positional argument: a literal string (start it with @@
// to begin with a literal @), @file, or - for stdin.
func runCommand() *cli.Command {
	return &cli.Command{
		Name:  "run",
		Usage: "Run one agent turn and exit",
		// Prompt: a literal string (start it with @@ to begin with a
		// literal @), @file, or - for stdin.
		ArgsUsage:   "[prompt]",
		Description: "Prompt: a literal string (start it with @@ to begin with a literal @), @file, or - for stdin. Exactly one prompt argument is accepted; stdin is read only when explicitly requested with - or @-.",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Aliases: []string{"c"},
				Value:   config.DefaultPath,
				Usage:   "Path to blorb.json",
			},
			&cli.BoolFlag{
				Name:  "no-stream",
				Usage: "Disable streaming of assistant responses",
			},
			&cli.BoolFlag{
				Name:  "tool-output",
				Usage: "Show the full output of tool results (subagent output is always shown)",
			},
			&cli.StringFlag{
				Name:  "agent",
				Usage: "Name of the agent to run; defaults to the config's default_agent",
			},
			&cli.StringFlag{
				Name:  "format",
				Usage: "Output format: chat (default), plain, or ndjson",
				Value: run.FormatChat,
			},
			&cli.BoolFlag{
				Name:  "logprobs",
				Usage: "Ask the server for per-token log probabilities and print one line per token after the response body (chat and plain formats); requires --no-stream",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			prompt, err := run.ResolvePrompt("run", cmd.Args().First(), os.Stdin)
			if err == nil && cmd.Args().Len() > 1 {
				err = fmt.Errorf("run: unexpected arguments after the prompt: %s", strings.Join(cmd.Args().Slice()[1:], " "))
			}
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}

			cfg, err := config.Load(cmd.String("config"))
			if err != nil {
				return cli.Exit(fmt.Sprintf("run: %v", err), 1)
			}

			agent, err := resolveAgent(cfg, cmd.String("agent"))
			if err != nil {
				return cli.Exit(fmt.Sprintf("run: %v", err), 1)
			}

			var tracer *prefactor.Tracer
			if cfg.PrefactorEnabled() {
				tracer, err = buildPrefactorTracer(ctx, cfg, agent)
				if err != nil {
					return cli.Exit(fmt.Sprintf("run: %v", err), 1)
				}
			}

			// SIGINT maps to exit code 130 via context propagation. The
			// signal-to-exit-code mapping itself is not tested (driving a
			// real signal through the in-process CLI harness is flaky);
			// it is three lines verified by inspection:
			// NotifyContext cancels sigCtx on SIGINT, run.Run wraps the
			// ctx error, and the errors.Is check below maps it to 130.
			sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt)
			defer stop()

			// The final text is already printed by the event printer;
			// nothing extra is output on success.
			_, err = run.Run(sigCtx, run.Options{
				Config:     cfg,
				Agent:      agent,
				Stdout:     os.Stdout,
				Stderr:     os.Stderr,
				Stream:     !cmd.Bool("no-stream"),
				ToolOutput: cmd.Bool("tool-output"),
				ConfigPath: cmd.String("config"),
				Tracer:     tracer,
				Format:     cmd.String("format"),
				Logprobs:   cmd.Bool("logprobs"),
			}, prompt)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return cli.Exit("run: interrupted", 130)
				}
				// No "run: " prefix here: run.Run's errors already carry
				// it (mapTurnOutcome and the tracer wrappers), and a
				// second prefix would double up.
				return cli.Exit(err.Error(), 1)
			}
			return nil
		},
	}
}

// decideCommand builds the decide subcommand: evaluate one decider against
// one state argument and print the answers JSON, the decider counterpart
// of run. The state is a positional argument sharing run's prompt syntax
// (literal, @@ escape, @file, - for stdin).
func decideCommand() *cli.Command {
	return &cli.Command{
		Name:      "decide",
		Usage:     "Evaluate a decider against a state and print the answers as JSON",
		ArgsUsage: "[state]",
		Description: "State: a literal string (start it with @@ to begin with a literal @), @file, or - for stdin. " +
			"Exactly one state argument is accepted; stdin is read only when explicitly requested with - or @-. " +
			"With --state-json the state is passed to the server as raw JSON rather than a JSON string.",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Aliases: []string{"c"},
				Value:   config.DefaultPath,
				Usage:   "Path to blorb.json",
			},
			&cli.StringFlag{
				Name:     "decider",
				Usage:    "Name of the decider to evaluate",
				Required: true,
			},
			&cli.BoolFlag{
				Name:  "state-json",
				Usage: "Treat the state argument as raw JSON (an object or array), passed to the server verbatim, instead of a plain string",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			state, err := run.ResolvePrompt("decide", cmd.Args().First(), os.Stdin)
			if err == nil && cmd.Args().Len() > 1 {
				err = fmt.Errorf("decide: unexpected arguments after the state: %s", strings.Join(cmd.Args().Slice()[1:], " "))
			}
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}

			cfg, err := config.Load(cmd.String("config"))
			if err != nil {
				return cli.Exit(fmt.Sprintf("decide: %v", err), 1)
			}

			decider, err := resolveDecider(cfg, cmd.String("decider"))
			if err != nil {
				return cli.Exit(fmt.Sprintf("decide: %v", err), 1)
			}

			// SIGINT maps to exit code 130 via context propagation,
			// mirroring run.
			sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt)
			defer stop()

			err = decide.Run(sigCtx, decide.Options{
				Config:     cfg,
				Decider:    decider,
				Stdout:     os.Stdout,
				Stderr:     os.Stderr,
				ConfigPath: cmd.String("config"),
				StateJSON:  cmd.Bool("state-json"),
			}, state)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return cli.Exit("decide: interrupted", 130)
				}
				return cli.Exit(err.Error(), 1)
			}
			return nil
		},
	}
}

// bandCommand builds the band subcommand: run the agent as a remote
// agent on the Band platform until interrupted.
func bandCommand() *cli.Command {
	return &cli.Command{
		Name:  "band",
		Usage: "Connect to the Band platform and answer room mentions",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Aliases: []string{"c"},
				Value:   config.DefaultPath,
				Usage:   "Path to blorb.json",
			},
			&cli.StringFlag{
				Name:  "agent",
				Usage: "Name of the agent to run; defaults to the config's default_agent",
			},
			&cli.BoolFlag{
				Name:  "no-stream",
				Usage: "Disable streaming of assistant responses",
			},
			&cli.BoolFlag{
				Name:  "tool-output",
				Usage: "Show the full output of tool results (subagent output is always shown)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			cfg, err := config.Load(cmd.String("config"))
			if err != nil {
				return cli.Exit(fmt.Sprintf("band: %v", err), 1)
			}

			agent, err := resolveAgent(cfg, cmd.String("agent"))
			if err != nil {
				return cli.Exit(fmt.Sprintf("band: %v", err), 1)
			}

			if !agent.BandEnabled() {
				return cli.Exit(fmt.Sprintf("band: agent %q has no band section; see examples/band", agent.Name), 1)
			}

			// The agent API key comes from the configured environment
			// variable; it must be set and non-empty.
			bandCfg := agent.Band
			envName := bandCfg.APIKeyEnv
			apiKey := os.Getenv(envName)
			if apiKey == "" {
				return cli.Exit(fmt.Sprintf("band: api_key_env %q is set but the environment variable is empty", envName), 1)
			}

			// Wire logging follows the same rules as chat and run.
			sink, err := chat.ResolveSink(cmd.String("config"), cfg)
			if err != nil {
				return cli.Exit(fmt.Sprintf("band: %v", err), 1)
			}

			// Prefactor tracing, when configured, records the whole
			// process as one agent instance.
			var tracer *prefactor.Tracer
			if cfg.PrefactorEnabled() {
				tracer, err = buildPrefactorTracer(ctx, cfg, agent)
				if err != nil {
					return cli.Exit(fmt.Sprintf("band: %v", err), 1)
				}
			}

			// SIGINT: the first cancels the context for a graceful stop
			// (finishing the in-flight message); a second exits
			// immediately, matching chat's interrupt ladder.
			sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt)
			defer stop()

			account, err := band.Run(sigCtx, band.Options{
				Config:     cfg,
				Agent:      agent,
				Stdout:     os.Stdout,
				Stderr:     os.Stderr,
				Stream:     !cmd.Bool("no-stream"),
				ToolOutput: cmd.Bool("tool-output"),
				ConfigPath: cmd.String("config"),
				Sink:       sink,
				APIKey:     apiKey,
				RESTURL:    bandCfg.RESTURLOrDefault(),
				WSURL:      bandCfg.WSURLOrDefault(),
				Tracer:     tracer,
			})
			if err != nil {
				return cli.Exit(fmt.Sprintf("band: %v", err), 1)
			}

			// The session footer prints for the work that completed, on
			// a clean shutdown too.
			if len(account.Records()) > 0 {
				fmt.Fprintf(os.Stderr, "%s\n", usage.FormatSession(account))
			}
			return nil
		},
	}
}

// resolveAgent picks the agent a chat session runs: the given name when
// non-empty, else the config's default_agent. It fails with the available
// agent names when nothing is chosen and with a not-defined error when the
// chosen name is not in the config.
func resolveAgent(cfg config.Config, name string) (config.Agent, error) {
	if name == "" {
		def, ok := cfg.DefaultAgentName()
		if !ok {
			return config.Agent{}, fmt.Errorf("no agent given and no default_agent configured (available: %s)", strings.Join(sortedAgentNames(cfg), ", "))
		}
		name = def
	}
	agent, ok := cfg.Agent(name)
	if !ok {
		return config.Agent{}, fmt.Errorf("agent %q is not defined in the config", name)
	}
	return agent, nil
}

// sortedAgentNames lists the config's agent names sorted alphabetically.
func sortedAgentNames(cfg config.Config) []string {
	names := make([]string, 0, len(cfg.Agents))
	for _, a := range cfg.Agents {
		names = append(names, a.Name)
	}
	sort.Strings(names)
	return names
}

// resolveDecider resolves the named decider: deciders are named things you
// invoke deliberately, so there is no defaulting. It fails with the
// available decider names when the chosen name is not in the config.
func resolveDecider(cfg config.Config, name string) (config.Decider, error) {
	decider, ok := cfg.Decider(name)
	if !ok {
		return config.Decider{}, fmt.Errorf("decider %q is not defined in the config (available: %s)", name, strings.Join(sortedDeciderNames(cfg), ", "))
	}
	return decider, nil
}

// sortedDeciderNames lists the config's decider names sorted
// alphabetically.
func sortedDeciderNames(cfg config.Config) []string {
	names := make([]string, 0, len(cfg.Deciders))
	for _, d := range cfg.Deciders {
		names = append(names, d.Name)
	}
	sort.Strings(names)
	return names
}

// buildPrefactorTracer constructs the Prefactor tracer for a chat session:
// the API token is resolved from the configured environment variable, which
// must be set and non-empty.
func buildPrefactorTracer(ctx context.Context, cfg config.Config, agent config.Agent) (*prefactor.Tracer, error) {
	pf := cfg.Prefactor
	envName := pf.APITokenEnvOrDefault()
	token := os.Getenv(envName)
	if token == "" {
		return nil, fmt.Errorf("prefactor: api_token_env %q is set but the environment variable is empty", envName)
	}
	client := prefactor.New(prefactor.Config{
		BaseURL: pf.APIURLOrDefault(),
		Token:   token,
	})
	return prefactor.NewTracer(prefactor.TracerConfig{
		Client:        client,
		AgentID:       pf.AgentID,
		EnvironmentID: pf.EnvironmentID,
		AgentName:     agent.Name,
	}), nil
}

func printlnVersion(cmd *cli.Command) error {
	fmt.Fprintln(cmd.Root().Writer, cmd.Root().Version)
	return nil
}
