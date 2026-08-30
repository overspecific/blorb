# Tool types: command and builtin

Tools in blorb.json currently must all be local commands. This plan introduces a required `type` field on each tool entry that governs which other fields are present, with two types: `"command"` (the existing subprocess behavior, unchanged) and `"builtin"` (a new mechanism identified by a separate `builtin` field). Each builtin owns its complete shape: its model-facing args schema *and* its per-entry settings schema, carried in a `config` object on the entry that the selected builtin alone defines and validates — a future "web fetch" builtin would have an entirely different `config` (allowed hosts, timeouts) without any framework change. It just happens that the two initial builtins, `read` and `grep`, are both file tools and share a settings shape: a required `base_dir` sandboxing them to one directory tree, enforced with `os.OpenRoot` (syscall-level containment, no TOCTOU window), with symlinks followed only when they resolve back inside the tree. The refactor follows the existing `Provider.Type` discriminated-union pattern in `internal/config`, introduces a tool interface in `internal/tools` so the registry is polymorphic, and keeps `Registry.Run`/`Definitions`/`Names` signatures stable so the engine, chat, and prefactor packages are unaffected. Since `type` is newly required, all existing fixtures, tests, the README, and `examples/simple/blorb.json` are updated in the same clean break.

## Todo

- [x] Commit 1: required `type` field on tool entries (config layer)
- [x] Commit 2: tool interface and type-tagged registry (tools layer)
- [x] Commit 3: builtin tools package with `read` and `grep`
- [x] Commit 4: wire `builtin` type through config and registry
- [ ] Commit 5: README and examples

---

## Commit 1: required `type` field on tool entries (config layer)

> Add a required, case-sensitive `type` field to tool entries in `internal/config/config.go`, mirroring the existing `Provider.Type` discriminated-union pattern:
>
> - Define `ToolTypeCommand ToolType = "command"` and `ToolTypeBuiltin ToolType = "builtin"` (a `string` type, like `ProviderType`), plus a `SupportedToolTypes() []string` helper sorted alphabetically.
> - Extend `ToolEntry` with `Type ToolType \`json:"type"\`` placed before `Name`. Only `command` tools carry `Command`/`ArgsSchema` semantics for now; `builtin` tools carry `Builtin` and `Config` (added in a later stage - do not add the fields yet).
> - Update `ToolEntry.validate()` (internal/config/config.go:281) to switch on `t.Type`:
>   - empty type → error `"type is required (one of: command, builtin)"` (mentioning `strings.Join(SupportedToolTypes(), ", ")`)
>   - `command` → existing name/description/command validation plus the new type check
>   - `builtin` → name/description validation only for now (per-type validation lands when the `builtin`/`config` fields are wired in a later stage; add a TODO-free placeholder test asserting name/description still validated)
>   - anything else → error `unknown tool type %q (supported: command, builtin)`
>   - the shared name/description checks stay in `validate()` before the switch so they are not duplicated per case
> - Config validation of name/description must run for every type: name required and matching `ToolNamePattern`; description required.
> - Update all tool entries in test fixtures and tests to include `"type": "command"`:
>   - `internal/config/testdata/*.json` (valid.json and every fixture containing tools, e.g. tool_missing_command.json, tool_bad_name.json, tool_missing_name.json, tool_missing_description.json, duplicate_tool_names.json, and any others - grep them all)
>   - `internal/config/config_test.go` literals
>   - `internal/tools/tools_test.go` `entry` helper (make it take the type or add a `commandEntry` helper)
>   - `internal/engine/engine_test.go` `toolRegistry` helper
>   - `internal/prefactor/schema_test.go` `testRegistry` helper
> - New config tests, following existing fixture conventions:
>   - `internal/config/testdata/tool_missing_type.json` → error contains `type is required`
>   - `internal/config/testdata/tool_unknown_type.json` (e.g. `"type": "webhook"`) → error contains `unknown tool type`
>   - wire them into the existing table in `config_test.go` the same way `tool_missing_command.json` is
> - Existing behavior otherwise unchanged; `args_schema` validation still only applies where present (it is only meaningful for `command` today, but do not reject it for `builtin` yet - that lands with the `builtin` field).
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 2: tool interface and type-tagged registry (tools layer)

> Restructure `internal/tools` so the registry is polymorphic, without changing behavior for command tools or any public signature the engine/chat/prefactor depend on:
>
> - Introduce an unexported `tool` interface (lowercase, since `Tool` the struct becomes an implementation detail):
>
>   ```go
>   type tool interface {
>       name() string
>       definition() llm.Tool
>       run(ctx context.Context, args json.RawMessage, sink logging.Sink) (ToolResult, error)
>   }
>   ```
>
>   Ownership split: `Registry.Run` applies the per-call `context.WithTimeout` and then calls `tool.run` with the derived context and the sink; each implementation owns its own execution and its `KindToolRequest`/`KindToolResult` wire-log records (same payloads, same order as today). The error contract is preserved: infrastructure failures (timeout, spawn failure, bad tool name) return a Go error from `Registry.Run`; tool-reported failures (non-zero exit) return `ToolResult{Err: true, Output: ...}` with a nil error.
> - Move the current subprocess execution into a `commandTool` struct implementing the interface, carrying over unchanged: `exec.CommandContext` with process-group kill on timeout, args on stdin, the 1 MiB `limitedBuffer` cap, 4 KiB stderr truncation, trailing-newline trim, per-call `context.WithTimeout`, and the existing wire-log records with identical payloads.
> - `Registry` becomes `map[string]tool`; `NewRegistry` switches on `config.ToolType` to construct the right implementation (only `command` exists in this stage; `builtin` returns an error `builtin tools are not yet supported` - unreachable via config until stage 4, but keeps the switch total). Keep revalidating entries in `NewRegistry` as today, switching per type.
> - `ToolResult`, `Registry.Run`, `Registry.Names`, `Registry.Definitions`, `WithTimeout`, `WithSink` keep their exact signatures. The exported `Tool` struct is removed (grep for external uses first; per the survey none exist outside the package's own tests, but verify).
> - Tests: all existing `internal/tools` tests keep passing with only fixture updates (entries now carry `Type: config.ToolTypeCommand`). Coverage must stay comprehensive: timeout, output-cap, stderr-truncation, bad-name, no-entry errors all still exercised against the `commandTool` path. If any test reached into `tools.Tool` directly, convert it to go through `NewRegistry`/`Run`.
> - `internal/engine`, `internal/chat`, `internal/prefactor` must not change at all in this stage and their tests must stay green - that is the proof the seam held.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 3: builtin tools package with `read` and `grep`

> Create a new package `internal/tools/builtin` containing the two builtin implementations, pure stdlib, no new dependencies. **Core design: each builtin owns everything about itself — its model-facing args schema, its entry `config` settings schema, and its execution. The framework (config and tools packages) never learns builtin-specific field names; adding a "web fetch" builtin later means adding a file to this package and nothing else.** read and grep being similar is their business: they share an unexported file-sandbox helper within the package because both are file tools, not because the framework knows they are.
>
> - Package API (imported by both `internal/config` for validation and `internal/tools` for construction; this package imports neither — verify with `go list -deps` that no cycle arises):
>   - `Supported() []string` — implementation names, sorted
>   - `Lookup(name string) (Builtin, bool)`
>   - `type Builtin struct` with `Name`, `Description string`, `ArgsSchema json.RawMessage` (model-facing), plus unexported function fields for settings parsing and execution
>   - `type Options interface{}` — an opaque marker; each builtin parses its `config` object into its own concrete settings type (read/grep: `fileOptions{BaseDir string}`)
>   - `ParseConfig(name string, raw json.RawMessage) (Options, error)` — dispatches to the builtin's settings parser; `raw` may be nil/empty for builtins that take no settings (not the case today). Each parser decodes with `DisallowUnknownFields` so unknown fields in a builtin's `config` are rejected the same way unknown top-level fields are, and applies per-builtin semantic checks
>   - `(Builtin) Run(ctx context.Context, opts Options, args json.RawMessage) (Result, error)` — executes with previously-parsed settings; `Result` is a local `{Output string; Err bool}` mirroring `tools.ToolResult` (converted by `internal/tools`; keeps this package dependency-free of the tools package — prefer this over importing it)
>   - a `config schema` per builtin is not exposed yet (settings validation is Go-side); expose a settings JSON schema from the package only when a consumer needs it
> - **read/grep shared settings (their choice, not the framework's):** `config` object `{"base_dir": string}`, required, must exist and be a directory at parse time (`base_dir %q is not a directory`). Unknown fields rejected. Resolution to a canonical absolute path happens at registry construction (stage 4), not here.
> - **File sandboxing (shared unexported helper, security-critical — build this first, test it hardest):**
>   - The model-supplied `path` argument is resolved against the sandbox's `*os.Root` and every open/read/stat goes through it. Containment is enforced by Root at the syscall level, not by string-checking paths — there is no TOCTOU window, and the pre-resolution joined path is never used for I/O.
>   - **Mechanism: `os.OpenRoot`/`fs.Root` (Go 1.24+), decided — no spike needed.** Open the base with `os.OpenRoot` once per tool construction and do all reads/walks through the `*os.Root` / `Root.FS()`. This forbids escaping paths at the syscall level and neutralizes TOCTOU entirely — strictly better than EvalSymlinks-based checking. Verified against the Go 1.27 stdlib: `Root.FS()` implements `fs.ReadDirFS`/`fs.ReadFileFS`/`fs.StatFS`/`fs.ReadLinkFS`, so `fs.WalkDir` walks the tree; `Root.Lstat` identifies symlinks without following them. Observed, pinned behavior: `..` traversal and inside-to-outside symlinks are rejected by Root itself (`path escapes from parent` / `invalid argument`); inside-to-inside file symlinks are followed transparently; dangling symlinks surface as `ErrNotExist`. **Error mapping policy:** Root operation failures that are `errors.Is(err, fs.ErrNotExist)` or `errors.Is(err, fs.ErrPermission)` pass through with their normal message (legitimate outcomes the model should see); every other Root failure (escape, invalid name — note there is no exported sentinel for "escapes", it is an unexported `os` error) maps to the uniform sandbox error `error: path is outside the allowed directory`. The uniform message is used for all escape attempts — inside-to-outside links and plain `../` traversal alike — so the model can't distinguish probe outcomes.
>   - Relative `path` args work naturally (resolved against the root); absolute `path` args: `Root`/`fs.FS` semantics reject absolute paths outright (fs paths are slash-relative, no leading `/`), and Root rejects them too — treat that as an escape (uniform sandbox error). A path *inside* the base written absolutely is therefore rejected; that is a documented consequence of the chosen mechanism, acceptable because models can trivially use the relative form. The base directory itself is addressable as `.`.
>   - During a grep walk: `fs.WalkDir` over `Root.FS()` does not follow directory symlinks (entries surface as `fs.ModeSymlink` and aren't descended into — verified) — keep it that way, it is the secure default; grep simply never enters linked directories. File symlinks inside the tree are followed when reading (inside-to-inside works, inside-to-outside fails the open and — per the error-mapping policy — that failure during a walk means *skip the file silently*, treating it like any other unreadable entry, rather than failing the whole grep). `.git` directories and binary-looking files are skipped (a simple heuristic: skip files whose first 1 KiB contains a NUL byte).
> - `read` tool: takes `{"path": string}` (required). Opens the resolved path within the sandbox. Output is the file content with a single trailing newline trimmed (mirroring the command tool's trailing-newline trim). Errors:
>   - path missing/not a string → tool-reported failure: `Result{Err: true, Output: "error: path is required and must be a string"}` (return nil error - this is the tool reporting failure, like a non-zero exit)
>   - open/read failure (missing file, permission, is-a-directory) → tool-reported failure: `Result{Err: true, Output: "error: " + err.Error()}`, nil error
>   - path resolving outside the base (via `../`, absolute elsewhere, or symlink) → the sandbox error above, nil error
>   - impose the same 1 MiB output cap as command tools: if the file exceeds it, return a tool-reported failure mentioning the cap and the file size
> - `grep` tool: takes `{"pattern": string (required), "path": string (optional, default ".")}` — the default means the base directory itself. `path` is sandboxed exactly like read's. Uses `regexp.Compile` on the pattern:
>   - invalid pattern → tool-reported failure with the compile error
>   - the `path` being a file means grep just that file; being a directory means walk it with `fs.WalkDir` over the sandbox's root FS. During a walk: directory symlinks are not descended into (`fs.WalkDir` doesn't follow them — verified — keep it that way, it is the secure default); skip `.git` directories and binary-looking files (a simple heuristic: skip files whose first 1 KiB contains a NUL byte). File symlinks *inside* the tree are read through the sandbox: inside-to-inside links are followed transparently, inside-to-outside links fail the open and — per the error-mapping policy — are skipped silently during a walk (treated like any other unreadable entry, the rest of the grep still succeeds); a `path` argument that is itself an escaping symlink fails the whole grep with the uniform sandbox error
>   - for each matching line, output `{path relative to the base}:{line number}:{line text}` separated by `\n`, trailing newline trimmed; no matches → empty output, not an error
>   - same 1 MiB combined-output cap: on exceeding it, stop and append a final line `output truncated at 1 MiB` and treat it as success (grep's cap is about result volume, not a tool failure - document this choice in a comment-free way via the test)
>   - unreadable file/directory → skip it silently (best-effort, like grep's exit-code-2-on-stderr behavior, but we surface nothing)
> - Args schemas: hand-write minimal JSON schemas (`{"type":"object","properties":{...},"required":[...]}`) for both, stored on the `Builtin` values, passed through as `ArgsSchema` so they reach `llm.Tool.Parameters`. The model-facing `path` descriptions should say paths are relative to the tool's configured directory.
> - Description copy for each: read = "Read a file at the given path and return its contents."; grep = "Search files for lines matching a regular expression pattern, returning matches as path:line:text." (config always requires an entry description, so these are documentation/defaults only.)
> - Tests in `internal/tools/builtin/builtin_test.go` (external test package `builtin_test`, stdlib only, `t.Parallel()`, table-style `t.Run`):
>   - `ParseConfig` tests per builtin: valid settings; missing `base_dir`; `base_dir` wrong type; `base_dir` not a directory; unknown field in `config`; empty `config` object; `ParseConfig` for an unknown builtin name
>   - happy paths for read and grep (using `t.TempDir()` fixtures, including a file with NUL bytes that grep skips, a nested directory, a `.git` dir that is skipped), missing path, bad type for path, missing file, oversized read file, invalid regex, grep on a single file path, grep with no matches, grep output truncation. Verify line-number and path formatting exactly (paths relative to base, forward slashes via `filepath.ToSlash`)
>   - **Sandbox tests get their own dedicated test function** (e.g. `TestSandbox`, not buried in happy-path tables), with `t.TempDir()` as base and an outside `t.TempDir()` sibling:
>     - read with `../sibling/file` → sandbox error
>     - read with an absolute path to a file outside the base → sandbox error
>     - read with an absolute path *inside* the base → also the sandbox error — a documented consequence of `os.Root`'s slash-relative path semantics; models should use the relative form
>     - symlink inside the base pointing to a file inside the base → read succeeds
>     - symlink inside the base pointing outside → sandbox error, same message as the `../` case (assert equality of the two outputs, not just substring)
>     - dangling symlink → `ErrNotExist` passes through with its normal message (per the error-mapping policy; verified Root surfaces it that way)
>     - grep with `path` walking a tree containing a symlink-to-outside file → the target file's contents never appear in output, grep still succeeds on the rest
>     - grep with `path` itself being a symlink pointing outside → sandbox error
>     - grep walking a tree containing a directory symlink → the linked directory's contents never appear (not descended), grep still succeeds
>     - grep walking a tree containing an inside-to-inside file symlink → its target's matching lines do appear (followed transparently)
> - No config or registry changes in this stage; the package is unused-but-complete (staticcheck's unused check is satisfied by the tests exercising it).
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 4: wire `builtin` type through config and registry

> Connect the builtin package end-to-end:
>
> - `internal/config/config.go`: add `Builtin string \`json:"builtin,omitempty"\`` and `Config json.RawMessage \`json:"config,omitempty"\`` to `ToolEntry`. The raw `config` object is opaque to the config package — its fields belong to the selected builtin, and `json.RawMessage` deliberately bypasses top-level `DisallowUnknownFields` (per-builtin unknown-field rejection happens inside each builtin's parser). Extend the `builtin` case of `ToolEntry.validate()`:
>   - `Builtin` required, must be one of `builtin.Supported()`
>   - settings validation delegates to the builtin: call `builtin.ParseConfig(t.Builtin, t.Config)` and wrap any error (config imports `internal/tools/builtin`; the builtin package imports neither config nor tools, so no cycle — verify with `go list -deps`)
>   - `Command` must be empty for builtin entries, and `Builtin`/`Config` must be empty for command entries - error messages like `command is not valid for builtin tools` and the mirror `builtin is not valid for command tools` (Config.Validate already prefixes `tool %q`)
>   - `args_schema` on a builtin entry is rejected (the model-facing schema comes from the builtin itself): error `args_schema is not valid for builtin tools`
> - `internal/tools/tools.go`: `NewRegistry` gains a `builtinTool` implementation of the `tool` interface wrapping `builtin.Lookup`:
>   - `name()` and `definition()` use the entry's `Name`/`Description` (the user-facing tool name and description from config) and the builtin's `ArgsSchema`
>   - construction calls `builtin.ParseConfig` again (defensively revalidating, and yielding the `Options` the tool runs with). For read/grep, construction opens the sandbox: the builtin package resolves `base_dir` to an absolute path and calls `os.OpenRoot` on it, failing construction if it doesn't resolve or isn't a directory — the open `*os.Root` is held by the tool instance for its lifetime. This lives in the builtin package (e.g. an `Open`/`Prepare` step on `Options`, or the root opened inside `ParseConfig`; keep it builtin-owned either way since sandboxes are a file-tool concept, invisible to the tools package). The timeout/execution wrapper stays in the tools package
>   - `run` delegates to the builtin's execution with the parsed `Options`, wrapped in the same `context.WithTimeout`, and emits the same `KindToolRequest`/`KindToolResult` wire-log records as command tools (same payloads, same order) so wire logging shows builtins identically. `builtin.Result` converts to `tools.ToolResult` here
>   - validation in `NewRegistry` for builtin entries re-checks type/builtin-name known/description present/`ParseConfig` succeeding, mirroring the config checks defensively
> - Config tests: new fixtures `tool_builtin_valid.json` (with a `config` object using an existing dir such as `"."` or `"testdata"` — fixtures run from the `internal/config` package dir), `tool_builtin_unknown.json` (e.g. `"builtin": "nope"`), `tool_builtin_missing_config.json` (read/grep require `base_dir` — error surfaces from `ParseConfig`), `tool_builtin_bad_config.json` (nonexistent `base_dir`), `tool_builtin_unknown_config_field.json`, `tool_builtin_with_command.json`, `tool_command_with_builtin.json`, `tool_command_with_config.json`, `tool_builtin_with_args_schema.json`, wired into the existing table.
> - Tools tests: registry construction from builtin entries (valid, unknown builtin name, missing builtin name, invalid settings, unresolvable base), `Run` dispatching to read/grep via the registry (read a temp file, grep a temp tree) asserting output matches the builtin package tests — including one end-to-end sandbox rejection through the registry (a `../` read returning the sandbox error as a tool result, not a Go error), `Definitions` uses the entry's name/description and the builtin's args schema, wire-log records emitted for a builtin run (assert via a capturing `logging.Sink` if one exists in tests today, otherwise a tiny stub implementing the Sink interface), timeout enforcement on a builtin (hard to trigger on read/grep directly - if not feasible cheaply, rely on the wrapper being shared with commandTool's timeout test path or construct a builtinTool against a stub; keep it honest).
> - Engine-level: one test in `internal/engine` exercising a turn where the model calls a builtin `read` tool through a `fakeClient` canned response, asserting the tool result content returns to the model - proves the whole chain. Base directory for the test registry: `t.TempDir()`.
> - Fixture/test updates: add `"type": "builtin", "builtin": "read", "config": {...}` style entries where useful; existing command fixtures unchanged since stage 1.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 5: README and examples

> Documentation pass:
>
> - `README.md` tools section (around lines 94-108 example and 146-155 description): update the JSON example to include `"type": "command"`, describe the `type` field (required, one of `command`/`builtin`), document the `builtin` type: the `builtin` field selects the implementation (`read` or `grep`), and each builtin defines its own `config` object — document read/grep's `base_dir` and the sandbox it enforces (only that directory tree is accessible; symlinks are followed only when they resolve back inside `base_dir`, and attempts to escape — including via symlink — fail). Note that `args_schema` is command-only (builtins define their own model-facing schema), and update "Each tool is an executable invoked as a subprocess" to describe both kinds. Match existing README tone and structure; no manual line wrapping.
> - `examples/simple/blorb.json`: add `"type": "command"` to the two existing tools, and add a `"read"` and a `"grep"` builtin entry (giving them distinct user-facing names like `"read"`/`"grep"` with sensible descriptions, and `"config": {"base_dir": "."}` so the example agent can inspect its own directory) so the example demonstrates both types.
> - `internal/config/testdata/valid.json`: add a builtin entry alongside the command ones so the canonical valid fixture covers both types (update its assertions in `config_test.go` if it asserts exact tool entries).
> - Skim `AGENTS.md` - only touch it if it describes tools as commands (per the survey it does not appear to; leave it alone if so).
> - If the README documents project layout (lines 232-240), add `internal/tools/builtin` to it.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.