# Code Review — Blorb

Findings per file, ordered by severity: Bug (correctness), Minor (quality), Nit (style).

## main.go

- Nit: `usage()` and `chatUsage()` print to stderr; help output conventionally goes to stdout.
- Minor: the root package has no tests; `run`, `cmdChat`, and the usage paths are untested.

## internal/config/config.go

- Bug: `MaxTurnsOrDefault` treats an explicit `max_turns: 0` as "unset", but `Validate` only rejects negative values, so `max_turns: 0` silently becomes 10. Either reject 0 in `Validate` or document that 0 means default.
- Bug: `Validate` error message says "max_turns must be at least 1" but the check is `< 0`; the message and the check disagree.
- Minor: `Load`'s trailing-JSON check (`dec.Decode(&struct{}{})`) only catches a second JSON value, not trailing garbage like `{} xyz`; a second decode returning a non-EOF error is ignored.
- Minor: `api_key_env` set to an empty string is silently treated as unset (the chat package skips it); an explicit empty value is indistinguishable from absent without a pointer field.
- Minor: `ArgsSchema` is not validated as JSON at load time; invalid schema JSON is only caught when the API rejects it.
- Nit: `validateUniqueToolNames` uses `map[string]bool`; `map[string]struct{}` is idiomatic.

## internal/llm/llm.go

- Nit: `ToolCall.Type` is stored but always forced to `ToolCallType` on the wire in `openai.wireMessages`; the field is dead weight in the neutral type.
- Nit: `Request.Model` is never set by the engine (the openai client falls back to its config model); the field exists but is unused by the only caller.

## internal/llm/openai/openai.go

- Minor: `Chat` reads the whole response body with `io.ReadAll` before checking the status code; a huge error body is fully buffered (the truncation only applies to the error message, not the read).
- Minor: no timeout on the HTTP client by default; a hung server blocks a turn until the user interrupts. `http.DefaultClient` has no timeout.
- Minor: `wireMessages` drops `Reasoning` for assistant messages without tool calls, but the neutral `Message.Reasoning` is still kept in engine history; the round-trip asymmetry is documented but easy to trip over.
- Minor: `neutralMessage` forces `Type: llm.ToolCallType` on every decoded tool call, discarding whatever the server sent; harmless today, but it hides server data.
- Minor: `Chat` ignores `FinishReason`; a `length`-truncated response is treated as a complete answer.
- Nit: `hasToolCallWithoutID`/`hasToolCallWithoutName` could be one pass; trivial.
- Nit: `truncate` slices by bytes, which can split a UTF-8 rune mid-sequence in the error message.

## internal/tools/tools.go

- Minor: on parent-context cancellation, `Run` returns `ctx.Err()` with no "timed out" text, while the tool's own timeout returns a distinct error; the distinction is lost in the error message. (The process group IS killed in both cases: `killProcessGroup` runs in the `runCtx.Done()` branch regardless of which deadline fired.)
- Minor: `Names()` uses a hand-rolled bubble sort; `sort.Strings` is clearer.
- Minor: `NewRegistry` revalidates names but not `Description` or `Command` contents (empty strings in command); config.Validate covers this, but the registry is a public API and can be constructed with bad data.
- Minor: stdout is buffered without a cap; a tool spewing gigabytes would OOM the process.
- Nit: `maxStderrLen` truncation also splits UTF-8 mid-rune.

## internal/engine/engine.go

- Minor: `RunTurn` appends the user message to history before the turn; if the turn fails (e.g. API error), the user message stays in history, so the next turn re-sends a user message that never got an answer. The repair pass only fixes tool calls, not the orphaned user message.
- Minor: `engine.DefaultMaxTurns` duplicates `config.DefaultMaxTurns` (both 10); two sources of truth.
- Minor: `lastAssistantMessage` returns a pointer into `e.history`; the caller only reads it, but a defensive copy would be safer.
- Minor: `RunTurn` returns only the final message's content; partial text from earlier rounds (content alongside tool calls) is emitted as events but lost to callers that don't use events.
- Nit: `repairUnansweredToolCalls` iterates history backwards but appends forwards; the order of synthetic results for multiple unanswered calls in one message is reversed relative to the call order (protocol-valid either way, but surprising).

## internal/chat/chat.go

- Bug: `interrupted` is set by the signal handler but never cleared; after one interrupted turn, the next successful turn also exits the session (the `wasInterrupted()` check at chat.go:150-156 fires again). The flag must be consumed/reset when read.
- Minor: `Run` leaks the `readLines` goroutine on exit: when the session ends (exit command, EOF, SIGINT), the `input` channel is never drained and `readLines` blocks forever on `out <- inputResult{...}` if the reader is still open (e.g. a pipe that stays open). In practice the process exits right after, so impact is low.
- Minor: the SIGINT forwarding goroutine (`for sig := range opts.SigintChan`) also leaks when `Run` returns; it blocks on the channel forever. Same low impact.
- Minor: `NewClient` reads `os.Getenv` directly; the error for a missing key is only surfaced at chat startup, not config load.
- Nit: `readLines` uses `bufio.Scanner` with the default 64KB line limit; long pasted input is silently split into multiple turns.

## internal/cli/cli.go

- Bug: `-c` followed by another flag (e.g. `-c -V`) consumes `-V` as the config path; standard flag parsers reject this. `-c --config x` similarly consumes `--config` as the path.
- Minor: `--config` with a value that starts with `-` is accepted as a path (e.g. `--config -V`); same issue as above.
- Nit: `shortValue` has a redundant first case: `attached == "=" || strings.HasPrefix(attached, "=") && len(attached) == 1` — the second operand is dead (a string equal to "=" already has prefix "=" and length 1). Harmless but confusing.
- Nit: `ParseChatFlags` returns `flags` (partially mutated) on error; callers ignore it, but returning a partial result is a smell.

## Tests

- `internal/engine/engine_test.go`: `toolRegistry` declares a `calls` slice that is never written to; `maybeTwoCalls` is a pointless wrapper; `requireShell` is a no-op (the tools tests have the real skip). Dead test scaffolding.
- `internal/chat/chat_test.go`: `TestConfigPathResolution` tests `config.Load` from the chat package; it belongs in the config package.
- `internal/chat/chat_test.go`: `TestRunSigintWhileIdleExits` uses a 50ms sleep; flaky on slow CI.
- `internal/tools/tools_test.go`: `TestRunTimeoutKillsChild` writes to a fixed `/tmp/blorb_test_child.pid` path; parallel runs of the test suite collide.
- No tests for `main.go` (usage, version, chat flag wiring).
- No test for `repairUnansweredToolCalls` ordering (the reversed-order nit above).
- No test for the `-c -V` flag-consumption bug in cli.

## Cross-cutting

- `config.DefaultMaxTurns` and `engine.DefaultMaxTurns` both define 10; the engine should use the config constant or the config should own the default.
- `blorb` binary exists in the repo root but is correctly ignored via `.gitignore` (`/blorb`) and not tracked; no action needed.
- `mise.toml` pins Go 1.27.0; `go.mod` says `go 1.27.0` — consistent.

## Verification

`bin/qc` passes (gofmt, go vet, go build, go test all green) as of this review.
