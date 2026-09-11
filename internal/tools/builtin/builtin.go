// Package builtin implements the tools that ship inside blorb itself,
// selected with "type": "builtin" in blorb.json. Each builtin owns its
// complete shape: its model-facing args schema, its per-entry settings
// schema (parsed from the entry's config object), and its execution. The
// config and tools packages dispatch on the builtin name but never learn
// builtin-specific field names, so adding a builtin means adding to
// Supported/Lookup/ParseConfig here and nothing else.
//
// A builtin toolset bundle is the second builtin shape: instead of one
// builtin, it names a stable group of member builtins plus one shared
// settings object. The bundle owns the member list (ToolsetMembers) and
// validates the shared settings shape (ParseToolsetConfig); callers use
// the members' own names as granted leaf names and re-parse the shared
// settings per member. Adding a bundle means adding to
// SupportedToolsets/ToolsetMembers/ParseToolsetConfig here and nothing
// else.
//
// Builtins record tool-reported failures (bad arguments, unreadable files)
// as Result{Err: true} with a nil Go error, the same contract as a
// command tool's non-zero exit. A Go error from Run means the call could
// not be performed at all.
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Options is the opaque parsed settings for a builtin, produced by
// ParseConfig and consumed by Run. Each builtin defines its own concrete
// settings type; callers treat the value as opaque.
type Options interface{}

// ParseOptions customizes settings parsing. BaseDir is the directory that
// builtin-relative paths (base_dir and friends) resolve against; empty
// means the process working directory.
type ParseOptions struct {
	BaseDir string
}

// Result is the outcome of a builtin run, mirroring tools.ToolResult: Err
// marks a tool-reported failure, which is still a valid result for the
// model and does not surface as a Go error.
type Result struct {
	Output string
	Err    bool
}

// Builtin describes one builtin implementation: its identity, its
// model-facing args schema, and its unexported parse/execute functions.
type Builtin struct {
	// Name is the builtin identifier used in the config's builtin field.
	Name string
	// Description documents the builtin; config always supplies the
	// user-facing description.
	Description string
	// ArgsSchema is the model-facing JSON schema for the tool's arguments.
	ArgsSchema json.RawMessage

	parseConfig func(raw json.RawMessage, po ParseOptions) (Options, error)
	run         func(ctx context.Context, opts Options, args json.RawMessage) (Result, error)
}

// Supported lists the builtin implementation names, sorted.
func Supported() []string {
	return []string{"grep", "read"}
}

// Lookup returns the named builtin implementation.
func Lookup(name string) (Builtin, bool) {
	switch name {
	case "read":
		return readBuiltin, true
	case "grep":
		return grepBuiltin, true
	default:
		return Builtin{}, false
	}
}

// ParseConfig validates and parses the config object for the named builtin,
// producing the opaque Options that its Run consumes. raw may be nil or
// empty (not the case for the current builtins, which require base_dir).
// Settings here are strict: unknown fields are rejected, and each builtin
// applies its own semantic checks. Relative paths in settings resolve
// against po.BaseDir (the config file's directory), or the process working
// directory when po.BaseDir is empty.
func ParseConfig(name string, raw json.RawMessage, po ParseOptions) (Options, error) {
	b, ok := Lookup(name)
	if !ok {
		return nil, fmt.Errorf("unknown builtin %q (supported: %s)", name, strings.Join(Supported(), ", "))
	}
	return b.parseConfig(raw, po)
}

// Run executes the builtin with previously-parsed settings. For file
// builtins, opts must be a prepared value from Prepare: an unprepared
// options value has no sandbox and the run fails.
func (b Builtin) Run(ctx context.Context, opts Options, args json.RawMessage) (Result, error) {
	return b.run(ctx, opts, args)
}

// SupportedToolsets lists the builtin toolset bundle names, sorted.
func SupportedToolsets() []string {
	return []string{"file"}
}

// ToolsetMembers returns the named toolset bundle's member implementations
// in their stable granted order, and whether the bundle exists. The
// members' Name is the granted leaf name, Description the default member
// description, and ArgsSchema the member schema.
func ToolsetMembers(name string) ([]Builtin, bool) {
	switch name {
	case "file":
		return []Builtin{readBuiltin, grepBuiltin}, true
	default:
		return nil, false
	}
}

// ParseToolsetConfig validates the shared settings object for the named
// toolset bundle. Like ParseConfig it is strict: unknown fields are
// rejected, and relative paths in settings resolve against po.BaseDir.
// The parsed value is not retained; callers re-parse the raw object per
// member at tool construction. It returns an error for an unknown bundle
// name.
func ParseToolsetConfig(name string, raw json.RawMessage, po ParseOptions) error {
	switch name {
	case "file":
		_, err := parseFileConfig(raw, po)
		return err
	default:
		return fmt.Errorf("unknown builtin toolset %q (supported: %s)", name, strings.Join(SupportedToolsets(), ", "))
	}
}
