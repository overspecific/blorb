package builtin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
)

// maxReadLen caps a read's output at the same 1 MiB as command tools.
const maxReadLen = 1 << 20

// maxReadLineLen caps a single line for sliced reads; a line longer than
// the cap is a failure rather than a silent truncation. bufio.Scanner
// needs the room for its buffer.
const maxReadLineLen = maxReadLen

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
    },
    "offset": {
      "type": "integer",
      "description": "Line number to start from, 1-based (default: 1), matching the line numbers grep reports"
    },
    "limit": {
      "type": "integer",
      "description": "Maximum number of lines to return; 0 means no limit (default: 0)"
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
	if err := json.Unmarshal(bytes.TrimSpace(bytesOr(args, []byte("null"))), &a); err != nil || a.Path == nil {
		return "", false
	}
	return *a.Path, true
}

// sliceArgs extracts the optional "offset" and "limit" arguments: offset is
// a 1-based first line, limit a maximum line count. Both zero when absent
// (or zero); a wrong type or an out-of-range value is a tool-reported
// failure. Only integer JSON numbers are accepted.
func sliceArgs(args json.RawMessage) (offset, limit int, res Result, ok bool) {
	var a struct {
		Offset any `json:"offset"`
		Limit  any `json:"limit"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(bytesOr(args, []byte("null"))), &a); err != nil {
		return 0, 0, Result{}, true // malformed args surface as no slicing; path parsing reports them
	}
	if a.Offset != nil {
		n, isInt := a.Offset.(float64)
		if !isInt || n != float64(int(n)) || n < 1 {
			return 0, 0, Result{Output: "error: offset must be a positive integer", Err: true}, false
		}
		offset = int(n)
	}
	if a.Limit != nil {
		n, isNum := a.Limit.(float64)
		if !isNum || n != float64(int(n)) || n < 0 {
			return 0, 0, Result{Output: "error: limit must be a non-negative integer", Err: true}, false
		}
		limit = int(n)
	}
	return offset, limit, Result{}, true
}

func runRead(ctx context.Context, opts Options, args json.RawMessage) (Result, error) {
	fc := opts.(fileConfig)
	sb := fc.sb
	path, ok := pathArg(args)
	if !ok {
		return Result{Output: "error: path is required and must be a string", Err: true}, nil
	}
	offset, limit, res, ok := sliceArgs(args)
	if !ok {
		return res, nil
	}

	info, err := sb.root.Stat(path)
	if err != nil {
		return Result{Output: "error: " + mapErr(err).Error(), Err: true}, nil
	}
	if info.IsDir() {
		res, err := sb.list(path)
		if err != nil {
			return Result{}, mapErr(err)
		}
		if offset == 0 && limit == 0 {
			return res, nil
		}
		res.Output = sliceLines(res.Output, offset, limit)
		return res, nil
	}

	f, err := sb.root.Open(path)
	if err != nil {
		return Result{Output: "error: " + mapErr(err).Error(), Err: true}, nil
	}
	defer f.Close()

	// Without offset or limit the whole file is read under the size cap.
	if offset == 0 && limit == 0 {
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

	// With offset or limit the file is streamed line by line, so slices
	// read big files without hitting the whole-file read cap. The cap
	// still governs the output volume: exceeding it is the same
	// oversized-file failure as above.
	if offset == 0 {
		offset = 1
	}
	var b strings.Builder
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 0, 64*1024), maxReadLineLen)
	line := 0
	for scan.Scan() {
		line++
		if line < offset {
			continue
		}
		if limit > 0 && line >= offset+limit {
			break
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.Write(scan.Bytes())
		if b.Len() > maxReadLen {
			return Result{
				Output: fmt.Sprintf("error: output exceeds the %d byte read cap", maxReadLen),
				Err:    true,
			}, nil
		}
	}
	if err := scan.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return Result{
				Output: fmt.Sprintf("error: line exceeds the %d byte read cap", maxReadLineLen),
				Err:    true,
			}, nil
		}
		return Result{Output: "error: " + mapErr(err).Error(), Err: true}, nil
	}
	return Result{Output: b.String()}, nil
}

// sliceLines returns the offset..offset+limit-1 1-based lines of the
// newline-joined output (no trailing newline), defaulting to all lines
// from offset when limit is 0. A trailing empty line is dropped first, so a
// final newline does not count as a line of its own.
func sliceLines(out string, offset, limit int) string {
	out = trimSingleTrailingNewline(out)
	if out == "" {
		return ""
	}
	lines := strings.Split(out, "\n")
	if offset < 1 {
		offset = 1
	}
	start := offset - 1
	if start > len(lines) {
		start = len(lines)
	}
	end := len(lines)
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	return strings.Join(lines[start:end], "\n")
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
