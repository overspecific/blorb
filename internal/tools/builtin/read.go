package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// maxReadLen caps a read's output at the same 1 MiB as command tools.
const maxReadLen = 1 << 20

// readBuiltin reads a single file inside the sandbox.
var readBuiltin = Builtin{
	Name:        "read",
	Description: "Read a file at the given path and return its contents.",
	ArgsSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Path of the file to read, relative to the tool's configured directory"
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
	path, ok := pathArg(args)
	if !ok {
		return Result{Output: "error: path is required and must be a string", Err: true}, nil
	}

	sb, err := openSandbox(fc.resolved)
	if err != nil {
		return Result{}, err
	}
	defer sb.close()

	info, err := sb.root.Stat(path)
	if err != nil {
		return Result{Output: "error: " + mapErr(err).Error(), Err: true}, nil
	}
	if info.IsDir() {
		return Result{Output: "error: read " + path + ": is a directory", Err: true}, nil
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
			Output: fmt.Sprintf("error: file is at least %d bytes, exceeding the %d byte read cap", len(data), maxReadLen),
			Err:    true,
		}, nil
	}
	return Result{Output: trimSingleTrailingNewline(string(data))}, nil
}
