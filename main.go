package main

import (
	"fmt"
	"os"
)

// version is set at build time via -ldflags "-X main.version=..." (see bin/build).
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "blorb: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
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
