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

	"github.com/overspecific/blorb/internal/chat"
	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/logging"
	"github.com/overspecific/blorb/internal/prefactor"
	"github.com/overspecific/blorb/internal/run"
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

				installedSet := make(map[string]struct{}, len(installed))
				for _, mi := range installed {
					fmt.Fprintf(w, "  %s\n", mi.Name)
					installedSet[mi.Name] = struct{}{}
				}
				for _, m := range models {
					if _, ok := installedSet[m.ModelName]; ok {
						fmt.Fprintf(w, "  %s (installed)\n", m.ModelName)
					} else {
						fmt.Fprintf(w, "  %s (NOT INSTALLED)\n", m.ModelName)
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
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			prompt, err := run.ResolvePrompt(cmd.Args().First(), os.Stdin)
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
			}, prompt)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return cli.Exit("run: interrupted", 130)
				}
				return cli.Exit(fmt.Sprintf("run: %v", err), 1)
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
