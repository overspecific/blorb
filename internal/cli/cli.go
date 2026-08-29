// Package cli implements command line flag parsing with POSIX-style
// syntax: single-dash short flags (-c) and double-dash long flags
// (--config). It depends on the standard library only.
package cli

import (
	"fmt"
	"strings"
)

// DefaultConfigPath is used when no config path flag is given.
const DefaultConfigPath = "./blorb.json"

// ChatFlags holds the parsed flags for the chat subcommand.
type ChatFlags struct {
	ConfigPath  string
	ShowVersion bool
	ShowHelp    bool
}

// ParseChatFlags parses chat subcommand arguments. Supported forms:
//
//	--config PATH     -c PATH   (value as the next argument)
//	--config=PATH     -c=PATH   (value attached with =)
//	                    -cPATH  (value attached directly)
//	--version         -V
//	--help            -h
//	--                          (terminates flag parsing)
//
// Short boolean flags may be combined (e.g. -hV). Any positional argument
// is an error; the chat subcommand takes none.
func ParseChatFlags(args []string) (ChatFlags, error) {
	flags := ChatFlags{ConfigPath: DefaultConfigPath}

	for i := 0; i < len(args); i++ {
		arg := args[i]

		switch {
		case arg == "--":
			if i+1 < len(args) {
				return flags, fmt.Errorf("unexpected argument %q", args[i+1])
			}
			return flags, nil

		case strings.HasPrefix(arg, "--"):
			name, value, hasValue := strings.Cut(arg[2:], "=")
			switch name {
			case "config":
				if hasValue {
					if value == "" {
						return flags, fmt.Errorf("--config requires a non-empty path")
					}
					flags.ConfigPath = value
					continue
				}
				i++
				if i >= len(args) {
					return flags, fmt.Errorf("--config requires a path")
				}
				flags.ConfigPath = args[i]
			case "version":
				flags.ShowVersion = true
			case "help":
				flags.ShowHelp = true
			default:
				return flags, fmt.Errorf("unknown flag --%s", name)
			}

		case strings.HasPrefix(arg, "-") && arg != "-":
			shorts := arg[1:]
			for j := 0; j < len(shorts); j++ {
				switch shorts[j] {
				case 'c':
					value, err := shortValue(shorts[j+1:], args, &i)
					if err != nil {
						return flags, err
					}
					flags.ConfigPath = value
					j = len(shorts)
				case 'V':
					flags.ShowVersion = true
				case 'h':
					flags.ShowHelp = true
				default:
					return flags, fmt.Errorf("unknown flag -%c", shorts[j])
				}
			}

		default:
			return flags, fmt.Errorf("unexpected argument %q", arg)
		}
	}
	return flags, nil
}

// shortValue extracts a short flag's value from the attached portion (which
// may be empty), an attached =value, or the next argument. next is advanced
// past a consumed next-argument value.
func shortValue(attached string, args []string, next *int) (string, error) {
	var value string
	switch {
	case attached == "=" || strings.HasPrefix(attached, "=") && len(attached) == 1:
		return "", fmt.Errorf("-c requires a path")
	case strings.HasPrefix(attached, "="):
		value = attached[1:]
	case attached != "":
		value = attached
	default:
		*next++
		if *next >= len(args) {
			return "", fmt.Errorf("-c requires a path")
		}
		value = args[*next]
	}
	if value == "" {
		return "", fmt.Errorf("-c requires a path")
	}
	return value, nil
}
