package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
)

// maxGrepOutputLen caps the combined grep output. Exceeding it stops the
// search and appends a truncation notice; grep treats the cap as result
// volume (a successful truncated result), not a tool failure.
const maxGrepOutputLen = 1 << 20

// grepBuiltin searches files for lines matching a regular expression.
var grepBuiltin = Builtin{
	Name:        "grep",
	Description: "Search files for lines matching a regular expression pattern, returning matches as path:line:text.",
	ArgsSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {
      "type": "string",
      "description": "Regular expression to search for"
    },
    "path": {
      "type": "string",
      "description": "File or directory to search, relative to the tool's configured directory (default: the directory itself)"
    },
    "case_sensitive": {
      "type": "boolean",
      "description": "Match case exactly (default: false, matches case-insensitively)"
    }
  },
  "required": ["pattern"]
}`),
	parseConfig: parseFileConfig,
	run:         runGrep,
}

func runGrep(ctx context.Context, opts Options, args json.RawMessage) (Result, error) {
	fc := opts.(fileConfig)
	sb := fc.sb
	var a struct {
		Pattern       *string `json:"pattern"`
		Path          *string `json:"path"`
		CaseSensitive *bool   `json:"case_sensitive"`
	}
	if err := json.Unmarshal(bytesOr(args, []byte("null")), &a); err != nil || a.Pattern == nil {
		return Result{Output: "error: pattern is required and must be a string", Err: true}, nil
	}
	// Case-insensitive by default; case_sensitive opts into exact case.
	if a.CaseSensitive == nil || !*a.CaseSensitive {
		*a.Pattern = "(?i)" + *a.Pattern
	}
	re, err := regexp.Compile(*a.Pattern)
	if err != nil {
		return Result{Output: "error: " + err.Error(), Err: true}, nil
	}
	path := "."
	if a.Path != nil && *a.Path != "" {
		path = *a.Path
	}

	out, err := sb.grep(ctx, re, path)
	if err != nil {
		return Result{Output: "error: " + err.Error(), Err: true}, nil
	}
	return Result{Output: out}, nil
}

// grep searches the file or directory at path for re, returning
// base-relative-path:line-number:line-text lines joined by \n. Unreadable
// entries are skipped silently; .git directories and binary-looking files
// are not searched; directory symlinks are not descended into.
func (s *sandbox) grep(ctx context.Context, re *regexp.Regexp, path string) (string, error) {
	// Validate the path argument itself through the sandbox so an escaping
	// path (../, absolute, or symlink out) fails the whole grep with the
	// uniform sandbox error before any walking starts.
	info, err := s.root.Stat(path)
	if err != nil {
		return "", mapErr(err)
	}

	var b strings.Builder
	if !info.IsDir() {
		s.scanFile(re, path, &b)
		return trimSingleTrailingNewline(b.String()), nil
	}

	root := cleanRel(path)
	err = fs.WalkDir(s.root.FS(), root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped silently
		}
		if d.IsDir() {
			// Keep .git out of the search, but allow an explicit path
			// argument pointing at it.
			if d.Name() == ".git" && p != root {
				return fs.SkipDir
			}
			return nil
		}
		s.scanFile(re, p, &b)
		return nil
	})
	if err != nil {
		return "", mapErr(err)
	}
	return trimSingleTrailingNewline(b.String()), nil
}

// scanFile searches one file and appends its matching lines. Unreadable and
// binary files contribute nothing.
func (s *sandbox) scanFile(re *regexp.Regexp, path string, b *strings.Builder) {
	if b.Len() > maxGrepOutputLen {
		return
	}
	f, err := s.root.Open(path)
	if err != nil {
		return // unreadable entries are skipped silently
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxReadLen+1))
	if err != nil || len(data) > maxReadLen || isBinary(data) {
		return
	}

	rel := relName(path)
	for i, line := range bytes.Split(data, []byte{'\n'}) {
		if !re.Match(line) {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(b, "%s:%d:%s", rel, i+1, bytes.TrimSuffix(line, []byte("\r")))
		if b.Len() > maxGrepOutputLen {
			b.WriteString("\noutput truncated at 1 MiB")
			return
		}
	}
}

// cleanRel normalizes a path argument for use as an fs.WalkDir root.
func cleanRel(p string) string {
	return strings.TrimPrefix(filepath.ToSlash(p), "./")
}

// isBinary reports whether the data looks binary: a NUL byte in the first
// kilobyte.
func isBinary(data []byte) bool {
	head := data
	if len(head) > 1024 {
		head = head[:1024]
	}
	return bytes.IndexByte(head, 0) >= 0
}

func bytesOr(raw json.RawMessage, fallback []byte) []byte {
	if len(raw) == 0 {
		return fallback
	}
	return raw
}
