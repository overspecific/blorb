# Fix review findings

Fix the correctness and quality issues found in the code review (REVIEW.md, alongside this file), in small coherent commits. Each commit leaves the tree building and `bin/qc` passing.

## Todo

- [x] Commit 1: Fix max_turns validation and default handling
- [x] Commit 2: Fix cli flag parsing edge cases
- [x] Commit 3: Fix engine history repair and turn bookkeeping
- [x] Commit 4: Fix tools output cap, cancellation error, and sorting
- [x] Commit 5: Fix chat interrupted-flag bug and goroutine leaks
- [x] Commit 6: Harden openai client (body read, timeout, tool call type)
- [x] Commit 7: Clean up test scaffolding and misplaced tests
- [x] Commit 8: Surface truncated (finish_reason length) responses
- [x] Commit 9: Add main.go tests
- [x] Commit 10: Harden config loading (trailing garbage, api_key_env, args_schema)
- [x] Commit 11: Harden chat input (scanner limit, injectable env lookup)
- [x] Commit 12: Return accumulated assistant text from RunTurn

---

## Commit 1: Fix max_turns validation and default handling

In `internal/config/config.go`:

- Reject `max_turns: 0` in `Validate` (error: "max_turns must be at least 1 (got 0)"), so an explicit zero can no longer silently become the default. Keep `MaxTurnsOrDefault` as-is for programmatic construction.
- Fix the existing error message so the check and message agree (the current message already says "at least 1" while the check is `< 0`; after rejecting 0 the message is correct).
- Change `validateUniqueToolNames` to use `map[string]struct{}` instead of `map[string]bool`.
- Add testdata `zero_max_turns.json` and a case in `TestLoadRejects` in `internal/config/config_test.go` expecting the new error.

Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 2: Fix cli flag parsing edge cases

In `internal/cli/cli.go`:

- Reject a config value that looks like a flag: when `-c` or `--config` takes its value from the next argument, error with `-c requires a path` / `--config requires a path` if that argument starts with `-` (and is not exactly `-`). Attached forms (`-cfoo`, `--config=foo`) are unaffected.
- Simplify the dead first case in `shortValue` (`attached == "=" || strings.HasPrefix(attached, "=") && len(attached) == 1` becomes just `attached == "="`).
- Return a zero-value `ChatFlags` (not the partially mutated one) on every error path.
- Add cases to `TestParseChatFlagsErrors` in `internal/cli/cli_test.go`: `-c -V`, `-c --config`, `--config -V`.

Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 3: Fix engine history repair and turn bookkeeping

In `internal/engine/engine.go`:

- In `RunTurn`, record the history length before appending the user message; on error, truncate history back to that length before running `repairUnansweredToolCalls`, so a failed turn does not leave an orphaned user message in history.
- In `repairUnansweredToolCalls`, append synthetic tool results in call order (iterate the assistant message's tool calls forwards, not backwards).
- Remove `engine.DefaultMaxTurns` and use `config.DefaultMaxTurns` in `New` (import `internal/config`), so there is one source of truth for the default.
- Make `lastAssistantMessage` return a copy of the message instead of a pointer into `e.history`.
- Update `internal/engine/engine_test.go`: add a test that a failed turn leaves history without the orphaned user message and that a subsequent turn works; add a test that two unanswered tool calls in one message are repaired in call order.

Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 4: Fix tools output cap, cancellation error, and sorting

In `internal/tools/tools.go`:

- Cap captured stdout (and stderr) with `io.LimitReader` (e.g. 1 MiB) so a runaway tool cannot OOM the process; note the truncation in the output.
- On parent-context cancellation, return a wrapped error that names the tool and keeps `errors.Is(err, context.Canceled)` working (e.g. `fmt.Errorf("tool %q: %w", name, ctx.Err())`), so the cancellation is distinguishable from a timeout in the message.
- Revalidate `Description` and `Command` contents in `NewRegistry` (non-empty description, non-empty command strings), matching `config.Validate`, since the registry is a public API.
- Replace the hand-rolled sort in `Names()` with `sort.Strings`.
- Make `truncateStderr` not split UTF-8 runes (truncate at a rune boundary).
- In `internal/tools/tools_test.go`, add a test that a tool writing more than the cap has its output truncated, and a test that `NewRegistry` rejects an empty command string.

Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 5: Fix chat interrupted-flag bug and goroutine leaks

In `internal/chat/chat.go`:

- Fix the `interrupted` flag bug: it is set by the signal handler but never cleared, so after one interrupted turn every subsequent successful turn exits the session. Consume/reset the flag when it is read (e.g. a `takeInterrupted()` that returns and clears it under the mutex), and use it in the post-turn check.
- Give `readLines` a done channel (or use a context) so `Run` can stop it on exit; close the done channel on every return path (exit command, EOF, SIGINT, ctx done, input error). This prevents the goroutine leaking when stdin stays open.
- Stop the SIGINT forwarding goroutine on exit: use a done channel closed on return, and select on it in the forwarding loop.
- Update `internal/chat/chat_test.go`: add a test that after an interrupted turn, a subsequent successful turn does NOT exit the session (regression test for the flag bug).

Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 6: Harden openai client (body read, timeout, tool call type)

In `internal/llm/openai/openai.go`:

- Read the response body with `io.LimitReader` capped at a sane bound (e.g. 16 MiB) so a huge error body is not fully buffered; keep the existing truncation for error messages.
- Add a default request timeout when `HTTPClient` is nil: use a client with a timeout (e.g. 5 minutes) instead of `http.DefaultClient`, so a hung server cannot block a turn forever. Document the choice in the `Config` comment.
- In `neutralMessage`, preserve the server-sent tool call type when non-empty, defaulting to `llm.ToolCallType`; stop discarding server data.
- Make `truncate` not split UTF-8 runes (truncate at a rune boundary).
- Add tests in `internal/llm/openai/openai_test.go`: a server that writes a body larger than the cap returns an error without hanging; a server that never responds returns an error within the timeout (use a short timeout via a custom `HTTPClient`); a tool call with a non-"function" type round-trips with that type preserved.

Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 7: Clean up test scaffolding and misplaced tests

- In `internal/engine/engine_test.go`: remove the unused `calls` slice from `toolRegistry`, remove the pointless `maybeTwoCalls` wrapper, and remove the no-op `requireShell` (replace its call sites with nothing; the tools tests already skip on Windows).
- In `internal/chat/chat_test.go`: move `TestConfigPathResolution` to `internal/config/config_test.go` (it tests `config.Load`).
- In `internal/tools/tools_test.go`: make `TestRunTimeoutKillsChild` use `t.TempDir()` for the pid file instead of the fixed `/tmp/blorb_test_child.pid` path.
- In `internal/chat/chat_test.go`: replace the 50ms sleep in `TestRunSigintWhileIdleExits` with a readiness signal (e.g. a channel closed by a hook, or poll for the banner on stderr with a timeout).

Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 8: Surface truncated (finish_reason length) responses

In `internal/engine/engine.go`:

- When a response arrives with `FinishReason == llm.FinishLength` and no tool calls, treat the turn as failed with a clear error (e.g. "response was truncated by the provider (finish_reason length)") instead of returning the partial text as a final answer. Emit the partial text as an `EventAssistantText` first so the user still sees it.
- Add a test in `internal/engine/engine_test.go` for a `length`-truncated response: events carry the partial text and `RunTurn` returns the truncation error.

Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 9: Add main.go tests

In `main_test.go` (new file, package main):

- Test `run` for: no args (exit 2, usage on stderr), `version`/`-V`/`--version` (prints version, exit 0), `help`/`-h`/`--help` (usage, exit 0), unknown command (exit 2, error on stderr).
- Test `cmdChat` for: bad flags (exit 2), `--help` (exit 0), `--version` (exit 0), missing config file (exit 1, error on stderr). Use a temp dir for the config path so no real blorb.json is needed.

Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 10: Harden config loading (trailing garbage, api_key_env, args_schema)

In `internal/config/config.go`:

- In `Load`, treat any non-EOF error from the trailing-value check as a parse error (currently only a successfully decoded second value is caught; trailing garbage like `{} xyz` is silently ignored).
- Reject an explicitly empty `api_key_env` in `Provider.validate` (error: "api_key_env must not be empty when set"), so an empty value is a config error rather than silently meaning "no key".
- Validate `ArgsSchema` as JSON in `ToolEntry.validate` when non-empty (error: "args_schema must be valid JSON").
- Add testdata files and cases in `internal/config/config_test.go`: `empty_api_key_env.json`, `bad_args_schema.json`, and a trailing-garbage file written in the test (like the existing trailing-values test).

Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 11: Harden chat input (scanner limit, injectable env lookup)

In `internal/chat/chat.go`:

- Raise the `bufio.Scanner` line limit in `readLines` (e.g. `scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)`) so long pasted input is not silently split into multiple turns.
- Add a `Getenv func(string) string` field to `Options` (defaulting to `os.Getenv`) and use it in `NewClient` so the env lookup is testable; keep the existing missing-key error.
- Update `internal/chat/chat_test.go`: add a test that a line longer than 64KB is delivered as a single turn, and a test that `NewClient` uses the injected `Getenv`.

Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 12: Return accumulated assistant text from RunTurn

In `internal/engine/engine.go`:

- Change `RunTurn` to accumulate assistant text across all rounds of the turn (content emitted alongside tool calls plus the final message) and return the accumulated text, so callers that do not use events do not lose partial text. Update the doc comment.
- Update `internal/engine/engine_test.go`: `TestRunTurnContentThenToolCalls` should expect the accumulated text ("let me check" + "final") as the return value; other tests that return a single text message are unaffected.

Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.
