package main

import (
	"context"
	"fmt"
	"os"

	"github.com/overspecific/blorb/internal/chat"
	"github.com/overspecific/blorb/internal/cli"
	"github.com/overspecific/blorb/internal/config"
)

// version is set at build time via -ldflags "-X main.version=..." (see bin/build).
var version = "dev"

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
	case "-V", "--version":
		fmt.Println(version)
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
	flags, err := cli.ParseChatFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "blorb chat: %v\n\n", err)
		fmt.Fprintf(os.Stderr, "Usage: blorb chat [-c | --config <path>] [-h | --help] [-V | --version]\n")
		return 2
	}
	if flags.ShowHelp {
		chatUsage()
		return 0
	}
	if flags.ShowVersion {
		fmt.Println(version)
		return 0
	}

	cfg, err := config.Load(flags.ConfigPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "blorb chat: %v\n", err)
		return 1
	}

	err = chat.Run(context.Background(), chat.Options{
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

func chatUsage() {
	fmt.Fprint(os.Stderr, `Usage: blorb chat [flags]

Flags:

  -c, --config <path>   Path to blorb.json (default ./blorb.json)
  -h, --help            Print this help
  -V, --version         Print the version
`)
}
