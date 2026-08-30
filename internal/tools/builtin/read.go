package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"strings"
)

// maxReadLen caps a read's output at the same 1 MiB as command tools.
const maxReadLen = 1 << 20

// readBuiltin reads a file or lists a directory inside the sandbox. Reading
// a directory returns its files (recursively, one relative path per line,
// forward slashes); directories, symlinks to directories, and .git are not
// listed.
var readBuiltin = Builtin{
	Name:        "read",
	Description: "Read a file at the given path and return its contents, or, if the path is a directory, return the list of files in it (recursively).",
	ArgsSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Path of the file to read, or directory to list, relative to the tool's configured directory"
    }
  },
  "required": ["path"]
}`),
	parseConfig: parseFileConfig,
	run:         runRead,
}

// pathArg extracts the required string "path" argument from the model's
// args.
func pathArg(args json.RawMessage) (string, bool) {
	var a struct {
		Path *string `json:"path"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(args), &a); err != nil || a.Path == nil {
		return "", false
	}
	return *a.Path, true
}

func runRead(ctx context.Context, opts Options, args json.RawMessage) (Result, error) {
	fc := opts.(fileConfig)
	sb := fc.sb
	path, ok := pathArg(args)
	if !ok {
		return Result{Output: "error: path is required and must be a string", Err: true}, nil
	}

	info, err := sb.root.Stat(path)
	if err != nil {
		return Result{Output: "error: " + mapErr(err).Error(), Err: true}, nil
	}
	if info.IsDir() {
		return sb.list(path)
	}

	f, err := sb.root.Open(path)
	if err != nil {
		return Result{Output: "error: " + mapErr(err).Error(), Err: true}, nil
	}
	defer f.Close()

	// Read one byte past the cap so an oversized file is detected without
	// buffering the whole thing.
	data, err := io.ReadAll(io.LimitReader(f, maxReadLen+1))
	if err != nil {
		return Result{Output: "error: " + mapErr(err).Error(), Err: true}, nil
	}
	if len(data) > maxReadLen {
		return Result{
			Output: fmt.Sprintf("error: file exceeds the %d byte read cap", maxReadLen),
			Err:    true,
		}, nil
	}
	return Result{Output: trimSingleTrailingNewline(string(data))}, nil
}

// list returns the files under the directory at path as base-relative
// paths, one per line (forward slashes, walk order). Directories, symlinks
// to directories, and .git trees are not listed; unreadable entries are
// skipped silently - mirroring grep's walk policy.
func (s *sandbox) list(path string) (Result, error) {
	root := cleanRel(path)
	var b strings.Builder
	err := fs.WalkDir(s.root.FS(), root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped silently
		}
		if d.IsDir() {
			if d.Name() == ".git" && p != root {
				return fs.SkipDir
			}
			return nil
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(relName(p))
		return nil
	})
	if err != nil {
		return Result{}, mapErr(err)
	}
	return Result{Output: b.String()}, nil
}
