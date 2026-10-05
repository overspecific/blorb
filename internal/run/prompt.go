// Package run implements the single-run mode: one prompt in, one agent turn out.
package run

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// ResolvePrompt turns a command's text argument into the user-facing text:
// a literal argument, a file reference (@path), or stdin (-). A prompt
// starting with @ is a file reference (@- reads stdin); - reads stdin; a
// prompt starting with @@ is a literal prompt whose remaining text is used
// verbatim, so a literal beginning with a single @ is expressible as @@. An
// empty prompt argument is an error, not stdin. cmdName prefixes the
// errors ("run", "decide").
func ResolvePrompt(cmdName, arg string, stdin io.Reader) (string, error) {
	switch {
	case arg == "":
		return "", fmt.Errorf("%s: no prompt given (pass a prompt argument, @file, or - for stdin)", cmdName)
	case arg == "-":
		return readStdinPrompt(cmdName, stdin)
	case strings.HasPrefix(arg, "@@"):
		return arg[1:], nil
	case strings.HasPrefix(arg, "@"):
		return resolveFilePrompt(cmdName, arg[1:], stdin)
	default:
		return arg, nil
	}
}

// readStdinPrompt reads the whole prompt from stdin, trimmed; an empty
// result is an error.
func readStdinPrompt(cmdName string, stdin io.Reader) (string, error) {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return "", fmt.Errorf("%s: read prompt from stdin: %w", cmdName, err)
	}
	prompt := strings.TrimSpace(string(data))
	if prompt == "" {
		return "", fmt.Errorf("%s: empty prompt from stdin", cmdName)
	}
	return prompt, nil
}

// resolveFilePrompt resolves the path part of an @file reference: "-" reads
// stdin like a bare -; anything else is a file path read from disk and
// trimmed.
func resolveFilePrompt(cmdName, path string, stdin io.Reader) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%s: @file reference has no path", cmdName)
	}
	if path == "-" {
		return readStdinPrompt(cmdName, stdin)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: read prompt file: %w", cmdName, err)
	}
	prompt := strings.TrimSpace(string(data))
	if prompt == "" {
		return "", fmt.Errorf("%s: empty prompt from @file", cmdName)
	}
	return prompt, nil
}
