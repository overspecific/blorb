package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/overspecific/goblorb/internal/chat"
	"github.com/overspecific/goblorb/internal/config"
)

// version is set at build time via -ldflags "-X main.version=..." (see bin/build).
var version = "dev"

const defaultConfigPath = "./blorb.json"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) < 1 {
		usage()
		return 2
	}

	switch args[0] {
	case "version":
		fmt.Println(version)
		return 0
	case "help", "-h", "--help":
		usage()
		return 0
	case "chat":
		return cmdChat(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "blorb: unknown command %q\n\n", args[0])
		usage()
		return 2
	}
}

func cmdChat(args []string) int {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "path to blorb.json")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "blorb chat: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *showVersion {
		fmt.Println(version)
		return 0
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "blorb chat: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT)
	defer stop()

	err = chat.Run(ctx, chat.Options{
		Config:  cfg,
		Version: version,
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "blorb chat: %v\n", err)
		return 1
	}
	return 0
}

func usage() {
	fmt.Fprint(os.Stderr, `blorb - a single-binary tool for making AI agents

Usage:

  blorb <command> [flags]

Commands:

  chat      Chat with an agent defined in blorb.json
  version   Print the version
  help      Print this help
`)
}
