# Single-run mode

A new `blorb run` subcommand executes exactly one agent turn and exits — the scripting counterpart to the interactive `blorb chat`. The user prompt comes from a single quoted argument, from a file given as `@path`, or from stdin when the prompt arg is `-`; omitting the argument (or passing more than one) is a usage error. Everything the run needs already exists: `config.Load` → agent resolution → `chat.NewClientWithGetenv` → `tools.NewRegistry` → `engine.New` → `eng.RunTurn` (the same chain `internal/chat` wires for a session, and the same one `engine.SubagentRunner` uses for a one-turn run). The work is a new `internal/run` package that builds that chain once, resolves the prompt, runs one turn with chat-style event printing (reusing `chat.chatEvents` via a small export), and maps the outcome to an exit code. Prefactor tracing maps a run to one instance with one turn, mirroring the chat session shape, and the same tracing hard-dependency rules apply. `main.go` grows the subcommand with the same flags as `chat` (`-c`, `--agent`, `--no-stream`, `--tool-output`) — the user plans to add more options later, so the option plumbing is structured for extension.

## Todo

- [x] Commit 1: prompt resolution (arg / `@file` / stdin `-`)
- [x] Commit 2: internal/run engine chain and one-turn execution
- [x] Commit 3: `blorb run` subcommand wiring
- [x] Commit 4: Prefactor tracing for run
- [x] Commit 5: docs

Review follow-ups (from the post-completion review pass):

- [ ] Follow-up 1: tracer-failure tests for `internal/run`
- [ ] Follow-up 2: strengthen `--agent` and `--no-stream` CLI tests
- [ ] Follow-up 3: dedupe canned provider response bodies in `main_test.go`
- [ ] Follow-up 4: drop duplicated prompt-forms comment in `runCommand`

---

## Commit 1: prompt resolution (arg / `@file` / stdin `-`)

> Create package `internal/run` with a new file `internal/run/prompt.go` implementing prompt resolution for the single-run mode. The function signature is:
>
> ```go
> // ResolvePrompt turns the run command's prompt argument into the user
> // message for the single turn: a literal argument, a file reference
> // (@path), or stdin (-).
> func ResolvePrompt(arg string, stdin io.Reader) (string, error)
> ```
>
> Behavior:
>
> - `arg == ""`: an error: `"run: no prompt given (pass a prompt argument, @file, or - for stdin)"` — the CLI requires exactly one prompt argument, so an empty argument never means stdin.
> - `arg == "-"`: read all of `stdin` (use `io.ReadAll`), trim leading and trailing whitespace (`strings.TrimSpace`, matching how chat trims input lines), and return it. An empty result after trimming is an error: `"run: empty prompt from stdin"`.
> - `strings.HasPrefix(arg, "@@")`: an escaped literal — return `arg[1:]` verbatim (untrimmed), so `@@hello` yields `@hello`. This must be checked before the general `@` prefix.
> - `strings.HasPrefix(arg, "@")`: resolve the path as follows — `@-` reads all of `stdin` exactly like the bare `-` case above (gh's convention; an empty result after trimming is the same `"run: empty prompt from stdin"` error); `@<path>` reads that file with `os.ReadFile`, trims with `strings.TrimSpace`, and returns it. An empty file after trimming is an error: `"run: empty prompt from @file"`. A `@` with an empty path (`"@"`) is an error: `"run: @file reference has no path"`. A file read error wraps the os error: `"run: read prompt file: %w"`.
> - Anything else: return the arg verbatim (no trimming — the user quoted it on the command line, so it is already exact). Document the reserved prefixes in the doc comment: "a prompt starting with @ is a file reference (@- reads stdin); - reads stdin; a prompt starting with @@ is a literal prompt whose remaining text is used verbatim, so a literal beginning with a single @ is expressible as @@. An empty prompt argument is an error, not stdin."
>
> Files: `internal/run/prompt.go` (new), `internal/run/prompt_test.go` (new).
>
> Tests (table-driven, package `run_test`, matching the repo's external-test-package and table-test conventions; the package doc comment on prompt.go reads `// Package run implements the single-run mode: one prompt in, one agent turn out.`):
>
> - literal arg returned verbatim, including leading/trailing spaces (`"  hello  "` stays `"  hello  "`)
> - `@@escape`: `"@@hi"` → `"@hi"`, and `"@@"` → `"@"` (both untrimmed — `"@@ spaced "` keeps its spaces)
> - `@file` reads and trims surrounding whitespace (`"hello\n"` → `"hello"`), including a file with only whitespace → error
> - `@file` on a missing file → error containing the wrapped os error
> - `@` alone → error "run: @file reference has no path"
> - `@-` reads stdin like bare `-` (`"piped\n"` → `"piped"`), and `@-` with empty stdin → the stdin empty-prompt error
> - `-` with a string reader containing `"hi there\n"` → `"hi there"`
> - `-` with empty stdin → error "run: empty prompt from stdin"
> - `-` with only-whitespace stdin → error
> - arg `"-"` is treated as stdin even though `-` is also a plausible literal — this is the specified contract
> - empty arg (`""`) → error "run: no prompt given (pass a prompt argument, @file, or - for stdin)"
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at top when done.

## Commit 2: internal/run engine chain and one-turn execution

> Add the one-turn execution core to `internal/run`. New file `internal/run/run.go`:
>
> ```go
> // Options configure a single run.
> type Options struct {
>     Config     config.Config
>     Agent      config.Agent
>     Stdout     io.Writer
>     NewClient  func(agent config.Agent) (llm.Client, error) // tests only; mirrors chat.Options
>     Getenv     func(string) string                          // tests only
>     Stream     bool
>     ToolOutput bool
>     ConfigPath string // logging, same semantics as chat.Options
> }
>
> // Run executes exactly one agent turn for prompt and returns the final
> // assistant text. It builds the client, registry, and engine exactly like
> // a chat session would, runs the turn, and releases resources.
> func Run(ctx context.Context, opts Options, prompt string) (string, error)
> ```
>
> Implementation, mirroring `internal/chat/chat.go` `Run` startup (chat.go:109-171) minus the REPL:
>
> - Build the sink with the same logic as `chat.resolveSink` (chat.go:713-727). That function is unexported in package chat — move the sink-resolution into a shared helper rather than duplicating it: add `func ResolveSink(configPath string, cfg config.Config) (logging.Sink, error)` to a new file `internal/chat/sink.go`, have `chat.resolveSink` delegate to it, and call `chat.ResolveSink(opts.ConfigPath, opts.Config)` from run. Its tests move/extend in chat's existing test file (`chat_test.go` has sink tests — keep them passing, adjusting call sites only).
> - Build the client via `opts.newClient(sink)` — implement the identical `newClient`/`newClientFor` pattern from chat (chat.go:761-776) in run; do not export chat's (run's Options are separate on purpose: the run command will gain its own options later, and sharing the unexported helper shape keeps the two packages parallel).
> - Streaming capability check: `_, ok := client.(llm.StreamingClient)` (chat.go:127-130) — same holder-free logic since run has exactly one turn and no per-turn client swapping.
> - Registry: `tools.NewRegistry(opts.Config.AgentTools(opts.Agent), tools.WithSink(sink), tools.WithConfigDir(opts.Config.Dir()), tools.WithSubagentRunner(runner), tools.WithSubagentEvents(onSubagent))` where `runner` is built exactly like `chat.Options.subagentRunner` (chat.go:782-791) — replicate that small function in run (same reasoning as newClient: parallel structure, not shared code). `defer registry.Close()`.
> - Engine: `engine.New(engine.EngineConfig{Client: client, Tools: registry, SystemPrompt: opts.Agent.SystemPrompt, MaxTurns: opts.Agent.MaxTurnsOrDefault(), Stream: opts.Stream && streaming})`.
> - Event printing: export chat's printer for reuse. Change `chat.chatEvents` (chat.go:488) to `chat.Events` with the identical signature and behavior, updating chat's internal call site (chat.go:362) and any chat tests referencing `chatEvents`. Run calls `printEvent, onSubagent, flush := chat.Events(opts.Stdout, opts.ToolOutput)`, passes `printEvent` to `eng.RunTurn`, and must wire `onSubagent` the way chat does: run has no `subagentPipe` (no per-turn printer swap needed with one turn), so pass `onSubagent` directly as the registry's `WithSubagentEvents` callback — construct the registry *after* obtaining the callbacks. Call `flush()` after `RunTurn` returns.
> - Do not add a `Tracer` field to Options in this commit; it is added in the tracing commit along with its behavior, so the field never exists in a half-wired state.
> - Run the turn: `final, err := eng.RunTurn(ctx, prompt, printEvent)`; on success return `final, nil`. On error return `"", fmt.Errorf("run: %w", err)` — except context cancellation: when `ctx.Err() != nil` and the error is not nil, return the wrapped ctx error (`fmt.Errorf("run: %w", ctx.Err())`) so the CLI can distinguish interruption; a nil error with a cancelled ctx still returns the final text (the turn completed).
> - SIGINT handling: none in this commit — the CLI's root context cancellation (Ctrl-C) kills the turn via ctx, which is the specified behavior for one-shot runs.
>
> Files: `internal/run/run.go` (new), `internal/run/run_test.go` (new), `internal/chat/sink.go` (new, moved resolveSink), `internal/chat/chat.go` (call-site updates), `internal/chat/chat_test.go` (sink test call sites).
>
> Tests (`internal/run/run_test.go`, package `run_test`, using the fake-client patterns from `internal/chat/chat_test.go` — a fake `llm.Client` that returns a canned response, and one that emits tool-call events):
>
> - happy path: fake client returns "the answer"; assert returned text and that stdout contains `">>> Assistant:"` and `"the answer"` (chat-style rendering, no `">>> User:"` heading — assert its absence)
> - streaming path: fake `llm.StreamingClient` emits text deltas; assert stdout contains the streamed text and the single Assistant heading
> - tool path: fake client emits a tool-call event then a result event; assert stdout contains `">>> Tool:"` and `">>> Result:"` headings; with `ToolOutput: false` the result body is summarized (assert the body text is absent), with `ToolOutput: true` it is present
> - subagent path: registry wired with `WithSubagentEvents` — drive a fake whose tool is a subagent-shaped event via the onSubagent callback; assert the indented subagent heading renders (mirror the assertions in chat_test.go's subagent rendering tests, simplified)
> - sink: with `ConfigPath` set and a temp config with `log_dir`, assert the session log dir is created (mirror chat's existing sink test)
> - error path: fake client returns an error; assert the returned error wraps it and the turn fails (exit-code mapping is the CLI commit's concern)
> - cancelled ctx before RunTurn: fake client that sleeps until ctx done; assert the error wraps `context.Canceled`
> - `registry.Close` released: fake builtin tool is not required here; instead assert via the sink/log-dir path that Run returns cleanly (Close is best-effort) — a direct assertion that Close ran is not observable through the public API, so cover it by using a real command tool with a temp dir and asserting no error; the deferred Close correctness is covered by chat's existing tests for the same pattern
> - chat package: `bin/qc` must pass with the `chatEvents` → `chat.Events` rename and the sink move — all existing chat tests green, unchanged except call sites
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at top when done.

## Commit 3: `blorb run` subcommand wiring

> Add the `run` subcommand to `main.go`, mirroring `chatCommand` (main.go:47-107):
>
> ```go
> // runCommand builds the run subcommand: one prompt, one agent turn, exit.
> func runCommand() *cli.Command
> ```
>
> - Name `run`, Usage: `Run one agent turn and exit`.
> - Flags, identical set to chat: `-c/--config` (default `config.DefaultPath`), `--no-stream`, `--tool-output`, `--agent`.
> - Action: `config.Load` → `resolveAgent` (main.go:113, reused as-is) → same error wrapping as chat (`"run: %v"` prefix instead of `"chat: %v"`). Prefactor is not wired in this commit (tracing commit adds it) — do not call `buildPrefactorTracer`.
> - Prompt argument: exactly one positional arg, retrieved via `cmd.Args().First()`. `blorb run "do a thing"` passes the literal; `blorb run @prompt.txt` reads the file; `blorb run -` reads stdin; `blorb run @-` also reads stdin; `blorb run "@@..."` escapes to a literal starting with `@`. Document in the flag help/usage text: "Prompt: a literal string (start it with @@ to begin with a literal @), @file, or - for stdin".
>   - No positional arg is a usage error: exit code 1 with `run: no prompt given (pass a prompt argument, @file, or - for stdin)` — the prompt is never implicitly read from stdin, because a scripting tool must not appear to hang when its arguments are forgotten. (Reading stdin is always explicit: `-` or `@-`.) No dedicated CLI check is needed: `cmd.Args().First()` returns `""` with no positional args, and `ResolvePrompt("")` returns exactly this error, so the action can delegate.
>   - More than one positional arg is a usage error: exit code 1 with `run: unexpected arguments after the prompt: <joined extra args>` — check `cmd.Args().Len() > 1` after resolving the first arg. A scripting tool must fail loudly on argument mistakes, not silently drop half the prompt.
> - Wire `run.Run(ctx, run.Options{Config: cfg, Agent: agent, Stdout: os.Stdout, Stream: !cmd.Bool("no-stream"), ToolOutput: cmd.Bool("tool-output"), ConfigPath: cmd.String("config")}, prompt)` — Getenv stays nil so it defaults to os.Getenv, exactly as the chat action does. On success print nothing extra — the final text is already printed by the event printer (chat-style); on error return `cli.Exit(fmt.Sprintf("run: %v", err), 1)`.
> - Exit codes: 0 on a completed turn, 1 on any error. For SIGINT to produce exit code 130, the action must first install signal→context propagation: in `runCommand`'s action (not `main()`, which must stay simple), create `sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt)` with `defer stop()`, pass `sigCtx` to `run.Run`, and when the returned error satisfies `errors.Is(err, context.Canceled)`, return `cli.Exit("run: interrupted", 130)`. (`Run` returns the wrapped ctx error per commit 2's contract.) Do not change `main()`'s `context.Background()` in this commit.
> - Register in `rootCommand()` (main.go:31-44) between chat and version.
>
> Files: `main.go`, `main_test.go`.
>
> Tests (in `main_test.go`, using the existing helpers `newFakeProviderServer` (main_test.go:184) and a new `runRunCommand` helper modeled on `runChatCommand` (main_test.go:235) but taking an explicit stdin payload and argv args; keep the pipe-backed-stdio approach and `capturingExitHandler`):
>
> - `blorb run -c cfg "say hi"` with the fake provider → exit 0, stdout contains `">>> Assistant:"` and the canned reply, and no `">>> User:"`
> - `blorb run -c cfg @promptfile` where promptfile contains `hello file` → the fake provider server asserts the received user message content equals `hello file` (capture the request body in the handler like chat's request-asserting tests do)
> - `blorb run -c cfg -` with stdin `piped prompt\n` → provider receives `piped prompt`; same assertion for `blorb run -c cfg @-`
> - `blorb run -c cfg "@@ @at-start"` → provider receives `@ @at-start` (the `@@` escape is stripped, remainder verbatim)
> - `blorb run -c cfg` with no prompt arg → exit 1, error output mentions `run: no prompt given`
> - `blorb run -c cfg "a" "b"` (extra positional args) → exit 1, error output mentions the extra args; the provider is never called
> - `blorb run -c cfg @missing` → exit 1, error output mentions the file read failure
> - SIGINT exit code 130 is intentionally untested: driving a real signal through the in-process CLI harness is flaky on some platforms, and the mapping is three lines verified by inspection (`signal.NotifyContext` → ctx cancel → `errors.Is(err, context.Canceled)`). Note this in a comment on the exit-130 branch.
> - `--no-stream` still works end to end (fake non-streaming client path)
> - `--tool-output` flag accepted and passed through (fake tool-emitting server asserting the full body renders; mirror chat's tool-output test if one exists, else assert the flag does not error and the run completes)
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at top when done.

## Commit 4: Prefactor tracing for run

> Wire Prefactor tracing into the run mode, mirroring chat's session shape: one run = one Prefactor instance containing one turn span with the same span types (`blorb:user_message` is carried by the turn itself, as in chat).
>
> `internal/run/run.go` changes:
>
> - Add `Tracer *prefactor.Tracer` to `Options` (nil = tracing off, same contract as chat.Options.Tracer).
> - In `Run`, when `opts.Tracer != nil`, follow chat's per-turn flow (chat.go:372-409) for the single turn:
>   - `StartSession(ctx, prefactor.DefaultAgentSchemaVersion(opts.Agent.Name, registry))` before running the turn — on `prefactor.ErrTerminated`, print `stopped by platform: <reason>` to stdout and return `( "", nil)` (exit 0, matching chat's cooperative-termination behavior); any other error returns `fmt.Errorf("run: %w", err)`.
>   - Wrap the client with chat's tracing wrapper for the turn. chat's `newTracingClient`/`traceEvent` (tracewrap.go:52, tracewrap.go:156) are unexported and tightly bound to chat's `clientHolder`/per-turn flow. Export the two composable pieces from chat: rename `newTracingClient` → `chat.NewTracingClient` with return type `llm.Client` (the concrete `*tracingClient` type stays unexported so the exported constructor does not expose an unexported type — its `ChatStream` passthrough already preserves streaming capability through the interface), and `traceEvent` → `chat.TraceEvent` (same signatures; update chat's internal call sites and tests). Run then installs the wrapper directly on its client before engine construction (no holder needed — one turn, one wrapper): `client = chat.NewTracingClient(client, turn, opts.Agent.Provider.Model)` when tracing, built after `StartTurn`. The streaming capability check in run (commit 2) must happen on the raw client *before* wrapping — re-asserting on the wrapper would always succeed because `tracingClient` implements `ChatStream` unconditionally (the same trap chat's holder comment documents at chat.go:120-130), silently enabling streaming for non-streaming clients.
>   - Run also needs `isTerminated` and `terminationReason` (tracewrap.go:116-128), which chat keeps unexported; both only use exported `prefactor` types, so replicate the two small functions in `internal/run` (parallel structure, same reasoning as newClient).
>   - Construction order matters because there is no holder: sink → client → streaming check (raw client) → registry (needs the streaming flag) → `StartSession` (needs the registry schema) → `StartTurn` → wrap the client → `engine.New` (needs the wrapped client) → `RunTurn` → outcome mapping → `FinishSession`. Commit 2 builds the engine before the turn because it has no tracer; this commit moves the engine construction below the turn-start block so the wrapped client is in place — spell this resequencing out in the code with a comment.
>   - `turn, err := opts.Tracer.StartTurn(ctx, prompt)`; on error: `run: %w` (the tracer failure path is the same hard-dependency rule).
>   - Event callback: compose `chat.TraceEvent(turn, printEvent)` and pass it to `eng.RunTurn`; keep `onSubagent` as the registry callback as in commit 2.
>   - Outcome mapping, mirroring chat's switch (chat.go:388-407) but differing from chat on interruption, deliberately: a run is intended to run to completion, so a user interrupt is a failed run, not a graceful outcome. A cancelled ctx reaches this switch as an ordinary turn error (the engine propagates it from the in-flight LLM call) and falls into the generic error branch:
>     - turn error and `errors.Is(err, prefactor.ErrTerminated)`: `turn.Fail(err)`, then return the terminate handling (print `stopped by platform: <reason>`, return `"", nil`).
>     - turn error otherwise (including ctx cancellation): `turn.Fail(err)`; return `"", fmt.Errorf("run: %w", err)`.
>     - success: `turn.Complete(final)`; return final.
>   - Session finish, mirroring chat (chat.go:317-337): background context like chat's (chat.go:331), and *skipped entirely on the terminate path* — the instance is already marked server-side and a second finish returns `ErrTerminated` (a 409), which would fail an otherwise-clean exit. Otherwise the status is one of `prefactor.InstanceComplete` (success) or `prefactor.InstanceFailed` (any error path, including ctx cancellation per the interrupt decision). A `FinishSession` error on an otherwise-successful run returns `fmt.Errorf("run: %w", err)`; when the run already failed, return the original run error.
>
> Files: `internal/run/run.go`, `internal/run/run_test.go`, `internal/chat/tracewrap.go` (rename exports), `internal/chat/tracewrap_test.go` (call sites), `main.go` (build the tracer like chat: `buildPrefactorTracer` is already agent-parameterized (main.go:141) — call it in runCommand's action when `cfg.PrefactorEnabled()` and pass it in Options).
>
> Tests:
>
> - `internal/run/run_test.go`, tracing cases using the `fakePF`-style httptest Prefactor fake (mirror `internal/chat/tracewrap_test.go:183`'s setup): a successful run records register/start/finish with `InstanceComplete`, one user-turn span completed with the final text, and one `blorb:llm` span; a provider-failure run records `InstanceFailed` and returns the error; a terminate mid-run returns `("", nil)` after printing the reason, with *no* finish request recorded (the terminate path skips `FinishSession`); a ctx-cancelled run records `InstanceFailed` (the deliberate interrupt semantics — unlike chat, an interrupted run is a failed instance).
> - `main_test.go`: config with a prefactor block but a missing token env var → `blorb run` exits 1 with the prefactor env-var error (mirror main_test.go:363-382's chat case).
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at top when done.

## Commit 5: docs

> Document the run mode end to end.
>
> - `README.md`:
>   - Usage section (after the chat examples around line 78): a `blorb run` block with the prompt forms — `./blorb run "Summarize ./notes.md"` (literal), `./blorb run @prompt.txt` (file), `echo "..." | ./blorb run -` (stdin), `./blorb run @-` (stdin via the @ form), and `./blorb run "@@ literal @ prompt"` (the `@@` escape for prompts starting with @). State that the prompt argument is required and exactly one is accepted — omitting it or passing extras is a usage error; stdin must be requested explicitly with `-` or `@-`. Then the flags it shares with chat (`-c`, `--agent`, `--no-stream`, `--tool-output`). State the exit-code contract: 0 on a completed turn, 1 on error, 130 on SIGINT. Note that the output format is currently identical to chat (including the `>>>` headings); additional output formats are planned.
>   - Features list (line 13): add the one-shot run bullet.
>   - Prefactor tracing section (line 261): note that `blorb run` records one instance with one turn, the same span model as a one-turn chat session.
>   - If the README's chat-flag documentation lives in a table, mirror the run flags in the same style.
> - `examples/simple/README.md` (Run section around line 22): add one `run` example line consistent with its chat examples.
> - `examples/prefactor-tracing/README.md`: if it documents chat-only usage, add the run equivalent.
> - No code changes; `bin/qc` must still pass (docs-only diff, but the prefactor example tests load the shipped configs, so do not touch the JSON files).
>
> Files: `README.md`, `examples/simple/README.md`, `examples/prefactor-tracing/README.md` (if applicable).
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at top when done.

## Follow-up 1: tracer-failure tests for `internal/run`

> `internal/run` has the repo's lowest coverage (75%) and the uncovered branches are exactly the tracer *failure* paths in `internal/run/run.go`:
>
> - sink error (run.go:56)
> - `StartSession`/`StartTurn` non-terminate errors (run.go:99, 108) and the `StartSession` terminate branch (run.go:95-98)
> - `turn.Fail`/`turn.Complete` error branches (run.go:167-182)
> - `FinishSession` failure and its error-priority logic (run.go:214-217)
>
> The `finishTracedRun` priority rule — when the run already failed, a `FinishSession` error loses to the original run error — is entirely untested. Chat covers the equivalent ground with `f.setSpanFail(http.StatusInternalServerError)` (see `TestRunTracedFatalError`, `internal/chat/tracewrap_test.go:713`); run has no such test.
>
> Work, in `internal/run/run_test.go`:
>
> - Extend the existing `runPFServer` with a `setSpanFail(code int)` mode (fail span creates with the given status; mirror chat's `fakePFServer.setSpanFail`, tracewrap_test.go) and a way to fail instance finishes (e.g. `setFinishFail(path prefix, code)` failing `/agent_instance/*/finish` with a 500).
> - New tests:
>   - `StartTurn` span failure (setSpanFail 500): Run returns an error mentioning `run:`, the instance is finished `failed`, and `turn.Fail`'s error branch on the already-started session is exercised via the StartTurn error path (run.go:108).
>   - `StartSession` span/instance failure: Run returns the wrapped `run:` error before any turn runs (run.go:99).
>   - `FinishSession` failure on an otherwise-successful run (finish endpoint 500): Run returns `run: <finish error>` (run.go:217).
>   - `FinishSession` failure on an already-failed run: the original run error wins, not the finish error (run.go:214-216).
>   - If cheap to add with the same fake (e.g. terminate-on-register or terminate-on-first-span-create), cover the `StartSession` terminate branch (run.go:95-98): prints `stopped by platform: <reason>`, returns `("", nil)`, no instance finish recorded.
> - Sink error path (run.go:56): a `ResolveSink` failure is awkward to trigger through `run.Run` (it needs an unwritable log dir); cover it with a direct `chat.ResolveSink` error test only if chat doesn't already have one — otherwise leave it, and note that here.
>
> Verify with `bin/qc` (expect `internal/run` coverage to rise meaningfully; mapTurnOutcome and finishTracedRun should be near-fully covered). Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at top when done.

## Follow-up 2: strengthen `--agent` and `--no-stream` CLI tests

> Two `main_test.go` run-command tests don't actually verify what they claim:
>
> - `TestRunAgentFlagCommand` (main_test.go:536) admits it cannot observe agent selection. It can: `writeAgentConfig` gives each agent `model-<name>`, so `requestCapturingProvider` + asserting `"model":"model-alpha"` in the captured request body proves the `--agent` flag selected alpha over the default beta.
> - `TestRunNoStreamCommand` (main_test.go:522) never asserts `--no-stream` reached the provider. With the captured request body, assert the JSON contains `"stream":false` (the default path sends `"stream":true`; the existing request-asserting tests already pin the streamed content).
>
> Update both tests to capture and assert on the request body (replacing the "not observable here" comments). Optionally fold `--agent` + `--no-stream` assertions into one test each; keep the existing names.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at top when done.

## Follow-up 3: dedupe canned provider response bodies in `main_test.go`

> `newFakeProviderServer` (main_test.go:185) and `requestCapturingProvider` (main_test.go:431) duplicate the same canned JSON and SSE payloads (the three SSE deltas and the non-streaming reply). Extract a shared helper, e.g.:
>
> ```go
> // respondCanned replies to a chat-completions request with the canned
> // reply: an SSE stream when the request asks for one, else plain JSON.
> func respondCanned(w http.ResponseWriter, body []byte)
> ```
>
> Both servers then read the body (the capturing one records it), unmarshal the `stream` field, and delegate to the helper. The payloads must stay byte-identical — the existing tests assert on `"content":"hi"` and the SSE shape.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at top when done.

## Follow-up 4: drop duplicated prompt-forms comment in `runCommand`

> The comment "Prompt: a literal string (start it with @@ to begin with a literal @), @file, or - for stdin" appears twice in `runCommand` in `main.go` — above the struct literal (main.go:114-115) and inside the Action (main.go:122-123). The `Description` field already carries the text for users. Keep one comment (the one above the struct literal, next to `ArgsUsage`), delete the one inside the Action.
>
> Verify with `bin/qc` (no behavior change; docs-only diff within the function). Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at top when done.