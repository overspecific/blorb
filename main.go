package main

import (
	"context"
	"fmt"
	"os"

	"github.com/urfave/cli/v3"

	"github.com/overspecific/blorb/internal/chat"
	"github.com/overspecific/blorb/internal/config"
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
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			cfg, err := config.Load(cmd.String("config"))
			if err != nil {
				return cli.Exit(fmt.Sprintf("chat: %v", err), 1)
			}

			err = chat.Run(ctx, chat.Options{
				Config:  cfg,
				Version: cmd.Root().Version,
				Stdin:   os.Stdin,
				Stdout:  os.Stdout,
				Stream:  !cmd.Bool("no-stream"),
			})
			if err != nil {
				return cli.Exit(fmt.Sprintf("chat: %v", err), 1)
			}
			return nil
		},
	}
}

func printlnVersion(cmd *cli.Command) error {
	fmt.Fprintln(cmd.Root().Writer, cmd.Root().Version)
	return nil
}
