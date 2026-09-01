package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/overspecific/blorb/internal/chat"
	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/prefactor"
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
			{
				Name:   "version",
				Usage:  "Print the version",
				Action: func(ctx context.Context, cmd *cli.Command) error { return printlnVersion(cmd.Root()) },
			},
		},
	}
}

// chatCommand builds the chat subcommand.
func chatCommand() *cli.Command {
	return &cli.Command{
		Name:      "chat",
		Usage:     "Chat with an agent defined in blorb.json",
		UsageText: "blorb chat [command options] [agent]",
		Arguments: []cli.Argument{
			&cli.StringArg{
				Name:      "agent",
				UsageText: "Name of the agent to chat with; defaults to the config's default_agent",
			},
		},
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
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			cfg, err := config.Load(cmd.String("config"))
			if err != nil {
				return cli.Exit(fmt.Sprintf("chat: %v", err), 1)
			}

			agent, err := resolveAgent(cfg, cmd.StringArg("agent"))
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
