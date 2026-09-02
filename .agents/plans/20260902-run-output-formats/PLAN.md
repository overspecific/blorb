# `run` output formats

Give `blorb run` proper output formats via a `--format` flag (`chat`, `plain`, `ndjson`) so it composes in shell pipelines, per the roadmap vision. `chat` stays the default and byte-identical to today — everything on stdout, footer and "stopped by platform" lines included. `plain` puts just the agent's output on stdout with everything else (headings, tool activity, streaming, footers) on stderr — the agent output is the concatenation of all the turn's assistant text events with no added padding or newline, so mid-sentence tool-call prose splices back together exactly as it came (note this deliberately differs from `Run`'s return value, which the engine joins with a newline between rounds). `ndjson` streams the full event stream as flat typed events, one JSON object per line (`text_delta`, `thinking_delta`, `tool_call_delta`, `text`, `thinking`, `tool_call`, `tool_result`, `usage`, `subagent_*` mirrors, `done`, `error`), following the Ollama native API pattern: delta-per-line, thinking as a separate field, `done` marker carrying totals. Streaming applies across all three via the existing `--no-stream` flag (deltas live vs. whole-message events only), and `--tool-output` is respected by `chat` and `plain` result blocks while ndjson always carries full tool result bodies (machine consumption). The engine already emits everything through the in-turn `onEvent` callback plus the registry's subagent callback, so both new formats are pure event-sink work in `internal/run` — no engine changes.

Design decisions (from the vision and the user):

- ndjson schema: flat typed events — every line is `{"type":"text_delta","agent":"main","text":"..."}` with shared types for tool calls (indexed, args as a string, assembled client-side), subagent events (agent + depth fields), and a final `{"type":"done",...}` carrying usage totals and the final text. Failures end the stream with `{"type":"error","error":"..."}` and no done event; the process still exits non-zero.
- plain + `--no-stream`: whole blocks on stderr (chat-style headings, no live deltas) — the same information as streamed, just not incremental.
- ndjson result bodies always carry the full output; `--tool-output` is meaningless there.
- Writer model per format: chat ignores `Stderr` entirely (single stream, today's behavior); plain and ndjson treat stderr as the diagnostics channel (footer, "stopped by platform" lines).

Structure (each stage a commit):

1. `plain` format, with the plumbing it needs: `run.Options` grows `Stderr` (nil falls back to Stdout) and `Format` (validated in `Run` before any LLM call), a format dispatch builds the event callbacks, and the footer/"stopped by platform" prints become format-aware (stdout for chat, stderr for plain). Chat is untouched.
2. `ndjson` format: the flat typed event stream on stdout, `done`/`error` terminal events, footer suppressed.
3. `--format` flag in `main.go`, wiring `os.Stderr`.
4. README docs.

---

## Todo

- [ ] Commit 1: `plain` format and the Stderr/Format plumbing
- [ ] Commit 2: `ndjson` format
- [ ] Commit 3: `--format` flag
- [ ] Commit 4: docs

---

## Commit 1: `plain` format and the Stderr/Format plumbing

> The `plain` format: agent text only on stdout, everything else on stderr. The stderr stream keeps chat's exact rendering (headings, tool blocks, streamed fragments — the output of `chat.Events(errWriter, toolOutput)` unchanged, just pointed at stderr), while stdout receives exactly the agent's output. This commit also introduces the plumbing plain needs, in the same commit because it exists only to serve a format: the `Stderr` and `Format` options, format validation, and the format-aware footer/terminate targets. The chat format is byte-identical to today by construction: it never touches `Stderr`.

> `internal/run/run.go`:

> - Add to `Options`:

> ```go
> 	// Stderr receives diagnostics for the plain and ndjson formats:
> 	// progress, tool activity, and the usage footer. Nil falls back
> 	// to Stdout, preserving the single-stream behavior. The chat
> 	// format ignores it and always uses Stdout.
> 	Stderr io.Writer

> 	// Format selects the run's output format: "chat" (the zero
> 	// value and default), "plain", or "ndjson". An unknown value
> 	// fails the run before any LLM call is made.
> 	Format string
> ```

> - Add an unexported helper and format constants:

> ```go
> // stderrOr returns opts.Stderr, or opts.Stdout when unset (the
> // single-stream fallback).
> func (o Options) stderrOr() io.Writer
> ```

>   with `FormatChat = "chat"`, `FormatPlain = "plain"`, `FormatNDJSON = "ndjson"` constants (ndjson lands next commit; declare the constant now so validation already names all three — or declare it in commit 2, either is fine, pick one and keep the error message listing the supported set consistent with what exists).
> - Validate `Format` at the top of `Run`, before the sink/client are built: an unknown value returns an error naming the value and the supported formats, e.g. `run: unknown format "yaml" (supported: chat, plain, ndjson)`. No LLM call happens.
> - Replace the event-callback wiring `printEvent, onSubagent, flush := chat.Events(opts.Stdout, opts.ToolOutput)` (run.go:73) with a format dispatch. New file `internal/run/formats.go`:

> ```go
> // events builds the run's event callbacks for the output format.
> // chat renders the chat-style stream on stdout; plain renders the
> // same chat-style stream on stderr and tees only assistant text to
> // stdout.
> func (o Options) events() (printEvent func(engine.Event) error, onSubagent func(tools.SubagentEvent) error, flush func(), err error)
> ```

>   - `chat`: `chat.Events(opts.Stdout, opts.ToolOutput)` — unchanged.
>   - `plain`: `chat.Events(o.stderrOr(), o.ToolOutput)` for the diagnostic stream, plus a stdout tee: the engine callback forwards every event to the stderr printer unchanged and additionally writes `ev.Text` to `o.Stdout` for exactly `EventAssistantText` and `EventAssistantTextDelta` (thinking is diagnostic; tool events are diagnostic; subagent text is not the run's output — the subagent callback forwards to the stderr printer unchanged). stdout writes are raw fragments with no line bookkeeping, so nothing flushes; `flush` is the stderr printer's partial-line flush. Because the engine suppresses whole-message events when streaming, the two kinds never double-write.
>   - Keep `chat.Events` as the single owner of chat-style rendering — plain reuses it verbatim on stderr rather than duplicating heading logic.
> - The usage wrappers (`usageWrap`/`runUsageWrap`, run.go:80-81) stay outside the dispatch: they wrap whatever callbacks the format produced, so accounting is identical across formats. The tracing wrap (`chat.TraceEvent`) likewise composes after, unchanged.
> - Make the footer and terminate prints format-aware. Today: the footer prints at run.go:152 and "stopped by platform" at run.go:105, run.go:114, and in `finishTracedRun` (run.go:222), all to `opts.Stdout`. Introduce a small target helper, e.g. `func (o Options) diagnostics() io.Writer` returning `o.Stdout` for chat and `o.stderrOr()` for the other formats, and print the footer and terminate lines to it. Chat keeps stdout (byte-identical, and existing tests stay green); plain gets them on stderr where they belong.

> Tests (`internal/run/run_test.go`, new `TestRunFormatPlain*` cases using the existing fake-client fixtures — note `TestRunPlainReply` already exists for the default chat-format run, hence the `Format` in the new names):

> - plain, whole-message (non-streamed) run with a tool round: stdout is exactly the concatenation of the assistant text events — the pre-tool text then the final text, spliced with no separator (deliberately unlike `Run`'s return value, which the engine joins with a newline; pin the difference in a comment) — with no headings, no tool blocks, no footer, no trailing newline added
> - plain, streamed run: stdout receives the text fragments as they arrive (delta events write through); stderr receives the chat-style streamed rendering (headings + fragments)
> - plain, thinking events: thinking renders on stderr only; stdout has only content text
> - plain, subagent run (existing subagent fixtures): all subagent activity (including the subagent's assistant text) renders on stderr; stdout has only the parent's assistant text
> - plain, `--tool-output`-style suppression respected on the stderr stream (a successful tool result shows the count summary without ToolOutput; the full body with it; failed results always in full) — pins that plain's stderr blocks honor `opts.ToolOutput` exactly like chat
> - plain, footer: the "tokens:" footer prints to the injected Stderr, not Stdout
> - plain, terminate (existing traced-terminate fixtures): "stopped by platform" prints to Stderr
> - plain, failed run (provider error mid-loop after a completed tool round): the completed round's text is on stdout; the error is returned, not printed by run (main's cli.Exit handles reporting) — stdout carries no error text
> - plain with nil Stderr: everything lands on the single Stdout stream (the fallback shape)
> - chat format with Stderr set: footer and terminate lines still on Stdout (pins that chat ignores Stderr)
> - unknown `Format` value: `Run` returns an error naming the value and the supported formats, and makes no LLM call (the fake client records zero requests)
> - the existing chat-format run tests pass unchanged (byte-identical pinning)

> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 2: `ndjson` format

> The `ndjson` format: one JSON object per line on stdout, written as each event arrives — the full event stream, not just assistant messages. Flat typed events, designed after the Ollama native API pattern (delta-per-line chunks, thinking as a separate field, streamed tool calls accumulated client-side, `done` marker on the final line). The process exit codes are unchanged (main.go maps them, no format-specific logic).

> New file `internal/run/ndjson.go` (the encoder is self-contained; `formats.go`'s dispatch grows an ndjson branch):

> - The event vocabulary maps the engine/subagent kinds to types:

> ```
> text_delta      {type, text}                       — assistant text fragment
> thinking_delta  {type, thinking}                   — reasoning fragment
> tool_call_delta {type, index, name?, arguments}    — tool call fragment (name on the first fragment for an index; arguments concatenated)
> text            {type, text}                       — whole assistant message (non-streaming)
> thinking        {type, thinking}                    — whole reasoning (non-streaming)
> tool_call       {type, name, arguments}             — whole tool call
> tool_result     {type, name, output, failed}        — tool result; output is always the full body (no summary, regardless of ToolOutput)
> usage           {type, agent, model, usage:{prompt_tokens,completion_tokens,total_tokens}}
> subagent_*      — the same vocabulary prefixed subagent_ with agent and depth fields added: subagent_text_delta, subagent_thinking_delta, subagent_tool_call_delta, subagent_text, subagent_thinking, subagent_tool_call, subagent_tool_result, subagent_usage
> done            {type, text?, usage:{prompt_tokens,completion_tokens,total_tokens}, agents:[{agent, usage:{...}}]}
> error           {type, error}
> ```

>   Every line's `type` field discriminates; consumers ignore unknown types (forward compatibility — document this). `name` on tool_call_delta carries the engine's delta `Name` (may repeat or be empty mid-stream; consumers key by `index`). The `done` event's `text` matches `Run`'s return value (the engine-joined text, `omitempty` so it is absent when empty) and `usage` is the account total with the per-agent split mirroring `usage.Account.AgentTotals()`; `error`'s `error` is the run error's message. All events share the same flat one-object-per-line shape with `omitempty` on optional fields. Do not emit comment or heartbeat lines.
> - Implementation shape: the ndjson sink is two event callbacks plus a `finish(final string, runErr error)` the `Run` epilogue calls after `RunTurn`:
>   - `printEvent`: marshal one line per engine event (switch on `ev.Kind` to the type above; `EventUsage` → `usage` with the event's agent/model). Use `json.Encoder` for the newline handling and call `SetEscapeHTML(false)` so tool output containing `<`, `>`, `&` stays readable in the JSON.
>   - `onSubagent`: the same mapping for `tools.SubagentEvent` kinds, carrying the event's agent name and depth.
>   - A marshal error (the only failure mode: a struct-to-JSON bug) returns the error from the callback, aborting the turn — the standard callback contract.
> - Terminal events and the exact stream contract — state this in the ndjson.go doc comment and pin it in tests:
>   - The stream emits events once the turn starts. `finish` is called iff `RunTurn` was entered: a run that fails before the turn (unknown format, client build, registry build, tracer start) emits no ndjson lines at all — the error goes to stderr via main's existing reporting.
>   - Success: `done` with the final text and totals.
>   - Failure (`RunTurn` returned an error): `error` with the run error's message, no done; exit code unchanged (1 or 130 via main).
>   - Platform termination mid-turn: the run exits 0, so `error` is wrong — emit `done` with no `text` and the usage totals, and the existing "stopped by platform" line goes to the format-aware diagnostics writer (stderr), carrying the human-readable reason.
> - Streaming: ndjson is inherently delta-driven — with `Stream` on, the engine emits delta events and they go out line-by-line as they arrive; with `--no-stream`, the engine emits whole-message events and the stream carries the whole-message types (`text`, `thinking`, `tool_call`). No ndjson-specific stream logic; the flag flows through `Options.Stream` as today. The `done`/`error` event is emitted regardless of stream mode.
> - The usage footer is suppressed for ndjson: the `done`/`error` event carries the usage, and a human footer would be noise for a machine consumer. In `Run`'s footer print, skip it when the format is ndjson. Keep the footer for chat and plain.
> - Extend commit 1's `events()` dispatch with the ndjson branch and extend the epilogue to call `finish` (the chat/plain finisher is nil/unused — keep those paths untouched).

> Tests (`internal/run/run_test.go`, `TestRunFormatNDJSON*`; parse each output line with `encoding/json` into a `map[string]any` or a small struct with `type` plus per-type fields — the existing run tests already import `encoding/json`):

> - non-streamed run with a tool round: line sequence is `usage`, `text` (first round), `tool_call`, `tool_result`, `usage`, `text` (final), `done` — each line valid JSON, each `type` correct; the `done` event's `text` equals the run's return value and its `usage.total_tokens` equals the summed canned usage; `agents` lists each agent with its totals
> - streamed run: `text_delta` fragments arrive whose concatenation equals the response content, followed by `usage` then `done` (usage after deltas — pin the engine's documented ordering)
> - thinking: `thinking`/`thinking_delta` events carry the reasoning in the `thinking` field, separate from `text`
> - streamed tool call: `tool_call_delta` events with `index`, the first carrying `name`, `arguments` fragments concatenating to the full args
> - tool result: `output` is the full body even with `ToolOutput` false (pins the always-full decision); `failed` true on a failed result
> - subagent run: `subagent_*` events carry `agent` (the subagent's name) and `depth` (1 from the registry's forwarding — the parent's own events carry no depth); `subagent_usage` lines attribute the nested calls; `done.agents` includes both agents
> - failed run (provider error after a completed round): the stream ends with an `error` event, no `done`; the lines before it are the events of the calls that ran
> - cancelled run before any call: `RunTurn` was entered, so the stream is a single `error` line
> - run failing before the turn (registry build failure fixture): no ndjson lines at all; the error is returned (main reports it)
> - traced terminate mid-run: the stream ends with `done` and no `text`; the "stopped by platform" line lands on the injected Stderr, not stdout
> - footer suppression: stdout contains no "tokens:" line; the totals live only in `done`
> - no HTML escaping: a tool result containing `<>&` round-trips unescaped in the JSON line

> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 3: `--format` flag

> Wire the format to the CLI: a `--format` string flag on `run` only (chat is not gaining formats). Default `chat`.

> `main.go`:

> - Add to `runCommand`'s flags:

> ```go
> 			&cli.StringFlag{
> 				Name:  "format",
> 				Usage: "Output format: chat (default), plain, or ndjson",
> 				Value: run.FormatChat,
> 			},
> ```

> - Pass `Format: cmd.String("format")` into `run.Options` in the action, and add `Stderr: os.Stderr` — the plain/ndjson formats get real stderr; the chat format ignores it (commit 1 pinned that). Validation of the value happens in `run.Run` (commit 1's resolution) — an unknown value fails the run with the supported-list error and exits 1 through the existing error path; no CLI-level validation needed (single source of truth in `run`).
> - The chat-format default keeps today's behavior byte-identical: no flag, no change (footer stays on stdout for chat; only plain moves it).

> Tests (`main_test.go`, extending `runRunCommand` — it currently captures stdout only via an `os.Stdout` pipe swap; extend it to also swap and capture `os.Stderr`, returning both, and update its call sites, or add a sibling helper if the churn is preferable):

> - `--format` flag exists with default "chat": a run without the flag produces today's output shape (existing tests already pin this; add one explicit default-value check on the flag)
> - `--format plain`: stdout has only agent text; stderr carries headings/tool blocks and the footer
> - `--format ndjson`: stdout lines parse as the event stream, ending in `done`
> - `--format bogus`: exit error naming the value and the supported formats
> - flag count: there is no run flag-count pin today (only chat's, main_test.go:165 `TestChatCommandHasNoNewFlags`); add the run equivalent — a check that run has exactly 5 flags (config, no-stream, tool-output, agent, format), so the set stays deliberate
> - chat command unaffected: the existing chat flag-count test (main_test.go:165) keeps passing — chat does not grow a format flag (no new assertion needed; the existing pin guards it)

> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 4: docs

> Document the `--format` flag and the three formats in the README.

> `README.md`:

> - Features list (around line 17, the one-shot run bullet): mention output formats — plain (agent text on stdout, progress on stderr) and ndjson (streaming JSON events) alongside the chat-style default.
> - Usage section, One-shot runs subsection (around line 115): replace "The output format is currently identical to chat (including the `>>>` headings); additional output formats are planned" with the real description: a `--format` flag taking `chat` (default, identical to chat output, everything on stdout), `plain` (agent text only on stdout; headings, tool activity, streaming, and the token footer on stderr — composable in pipelines), and `ndjson` (one JSON event per line on stdout, streaming as it happens; tool calls, results, usage, and subagent activity included; ends with a `done` event carrying the final text and usage totals, or an `error` event on failure; a run that fails before the turn emits no events). Add the flags line update: run's flags gain `--format <chat|plain|ndjson>`.
> - A short ndjson event-type reference: a fenced block listing the event types and their fields (the vocabulary from commit 2), plus a one-line `jq` example such as `./blorb run --format ndjson "..." | jq -r 'select(.type=="text_delta") | .text'` to show the scripting use case.
> - Note the streaming interaction: `--no-stream` works across all formats (whole-message events instead of deltas; plain keeps whole blocks on stderr), and `--tool-output` applies to chat and plain result blocks while ndjson always carries full bodies.
> - Exit codes unchanged; state that ndjson's `done`/`error` event is the machine-readable outcome, not a changed exit contract.
> - `examples/simple/README.md`: no changes (the example describes commands, not output shapes; leave it — matching the usage-stats plan's decision).

> Files: `README.md`.

> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.