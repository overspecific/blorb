package builtin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// fileOptions is the shared settings shape for the file builtins: a
// required base_dir sandboxing the tool to one directory tree.
type fileOptions struct {
	BaseDir string `json:"base_dir"`
}

// fileConfig is the decoded shape with the resolved absolute base
// directory added.
type fileConfig struct {
	fileOptions
	resolved string
}

// parseFileConfig validates the shared file-tool settings object:
// base_dir is required and must exist and be a directory. Unknown fields
// are rejected. A relative base_dir resolves against po.BaseDir (the
// config file's directory), or the process working directory when empty.
func parseFileConfig(raw json.RawMessage, po ParseOptions) (Options, error) {
	var opts fileOptions
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&opts); err != nil {
		if isEOFErr(err) {
			return nil, fmt.Errorf("base_dir is required")
		}
		return nil, err
	}
	if opts.BaseDir == "" {
		return nil, fmt.Errorf("base_dir is required")
	}
	base := opts.BaseDir
	if !filepath.IsAbs(base) && po.BaseDir != "" {
		base = filepath.Join(po.BaseDir, base)
	}
	abs, err := filepath.Abs(base)
	if err != nil {
		return nil, fmt.Errorf("base_dir %q: %w", opts.BaseDir, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("base_dir %q: %w", opts.BaseDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("base_dir %q is not a directory", opts.BaseDir)
	}
	return fileConfig{fileOptions: opts, resolved: abs}, nil
}

func isEOFErr(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// sandbox pairs an open os.Root with its base directory. All file access
// for a builtin instance goes through it.
type sandbox struct {
	root *os.Root
	base string
}

// openSandbox opens the base directory as a Root. It fails only if the
// directory cannot be resolved or opened; ParseConfig has already verified
// it exists as a directory.
func openSandbox(base string) (*sandbox, error) {
	abs, err := filepath.Abs(base)
	if err != nil {
		return nil, fmt.Errorf("base_dir %q: %w", base, err)
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("base_dir %q: %w", base, err)
	}
	return &sandbox{root: root, base: abs}, nil
}

// close releases the Root. Safe on a nil sandbox.
func (s *sandbox) close() {
	if s == nil {
		return
	}
	_ = s.root.Close()
}

// mapErr applies the sandbox error-mapping policy: not-exist and permission
// failures pass through with their normal message (legitimate outcomes the
// model should see); every other Root failure is an escape or invalid-name
// attempt and maps to the uniform sandbox error so probe outcomes are
// indistinguishable.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return err
	}
	return errors.New("path is outside the allowed directory")
}

// relName renders a sandbox-relative path as a display path with forward
// slashes; the base itself is ".".
func relName(p string) string {
	p = strings.TrimPrefix(p, "./")
	if p == "" {
		return "."
	}
	return filepath.ToSlash(p)
}

// trimSingleTrailingNewline trims one trailing \n or \r\n, mirroring the
// command tool's output trim.
func trimSingleTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\r\n") {
		return s[:len(s)-2]
	}
	if strings.HasSuffix(s, "\n") {
		return s[:len(s)-1]
	}
	return s
}
