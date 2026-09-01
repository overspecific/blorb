// Package run implements the single-run mode: one prompt in, one agent turn out.
package run

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// ResolvePrompt turns the run command's prompt argument into the user
// message for the single turn: a literal argument, a file reference
// (@path), or stdin (-). A prompt starting with @ is a file reference
// (@- reads stdin); - reads stdin; a prompt starting with @@ is a literal
// prompt whose remaining text is used verbatim, so a literal beginning
// with a single @ is expressible as @@. An empty prompt argument is an
// error, not stdin.
func ResolvePrompt(arg string, stdin io.Reader) (string, error) {
	switch {
	case arg == "":
		return "", fmt.Errorf("run: no prompt given (pass a prompt argument, @file, or - for stdin)")
	case arg == "-":
		return readStdinPrompt(stdin)
	case strings.HasPrefix(arg, "@@"):
		return arg[1:], nil
	case strings.HasPrefix(arg, "@"):
		return resolveFilePrompt(arg[1:], stdin)
	default:
		return arg, nil
	}
}

// readStdinPrompt reads the whole prompt from stdin, trimmed; an empty
// result is an error.
func readStdinPrompt(stdin io.Reader) (string, error) {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return "", fmt.Errorf("run: read prompt from stdin: %w", err)
	}
	prompt := strings.TrimSpace(string(data))
	if prompt == "" {
		return "", fmt.Errorf("run: empty prompt from stdin")
	}
	return prompt, nil
}

// resolveFilePrompt resolves the path part of an @file reference: "-" reads
// stdin like a bare -; anything else is a file path read from disk and
// trimmed.
func resolveFilePrompt(path string, stdin io.Reader) (string, error) {
	if path == "" {
		return "", fmt.Errorf("run: @file reference has no path")
	}
	if path == "-" {
		return readStdinPrompt(stdin)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("run: read prompt file: %w", err)
	}
	prompt := strings.TrimSpace(string(data))
	if prompt == "" {
		return "", fmt.Errorf("run: empty prompt from @file")
	}
	return prompt, nil
}
