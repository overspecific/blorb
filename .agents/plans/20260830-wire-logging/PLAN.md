# Full conversation and tool logging

Adds wire logging under a `.logs` directory adjacent to the `blorb.json` file: one file per LLM request and one per LLM response (method, URL, full headers, and body), and one file per tool call request and one per tool result (tool name, args, and output). Each conversation gets its own session subdirectory named with a timestamp and a UUID, so sessions never interleave. Filenames are timestamp-first with nanosecond precision so a plain lexical sort of the directory reconstructs the true sequence of a turn across all kinds. Nothing is streamed to disk: each file is written once, when the whole request/response is available. Because the LLM wire log needs exact headers and bodies (which only the provider client sees) and tool args/output need exact tool I/O (which only the registry sees), logging lives at those two layers and is threaded through from the top rather than reconstructed from engine events. Logging is on by default, best-effort (a failure to write a log never fails a turn), and can be disabled or relocated via a new `logging` section in `blorb.json`.

## Todo

- [x] Commit 1: Logging package (`internal/logging`)
- [x] Commit 2: OpenAI client request/response logging
- [x] Commit 3: Tool registry call/result logging
- [x] Commit 4: Thread the logger through config, chat, and CLI
- [x] Commit 5: Documentation and gitignore
- [x] Commit 6: Log the assembled response for streamed turns
- [x] Commit 7: Per-conversation log directories
- [x] Commit 8: Fix reasoning loss in streamed-response log records

---

## Commit 1: Logging package (`internal/logging`)

> Create `internal/logging` (package `logging`), the reusable foundation. It is provider- and tool-agnostic: no imports of config, engine, llm, tools, or net/http.
>
> - Add a `Kind string` type with constants `KindLLMRequest = "llm-request"`, `KindLLMResponse = "llm-response"`, `KindToolRequest = "tool-request"`, `KindToolResult = "tool-result"`. These double as filename and header labels.
> - Add a `Record` struct: `Time time.Time` (set by the caller at capture time — when the interaction happened, not when it is written), `Kind Kind`, `Method string`, `URL string`, `Headers map[string][]string`, `Body []byte`. Document that callers must set `Time` and `Kind`; the sink handles uniqueness and ordering.
> - Add a `Sink` interface: `Write(r Record) error`. Document the best-effort contract: producers call `Write` for every record and treat any error as informational only — per AGENTS.md, logging is the one place errors are not propagated.
> - Add `NewNop() Sink` returning a do-nothing sink, and `New(dir string) (Sink, error)` returning a file sink that validates `dir` exists and is a directory (`os.Stat`), returning a descriptive error otherwise. Creating the directory is deliberately not the sink's job: the caller (Commit 4) creates `.logs` and passes the path in.
> - The file sink writes each record as exactly one file named `<timestamp>-<kind>.txt`, e.g. `20260830T142533-123456789-llm-request.txt`. The timestamp is `r.Time.UTC()` formatted with the layout `20060102T150405-000000000` — six date digits, six time digits, then nine fixed fractional digits, separated by a dash; the fixed width keeps the string both unambiguous and correctly sortable. The timestamp comes *first* so that a lexical sort of filenames across all kinds is the true chronological sequence — a kind-first prefix would sort by kind instead. File mode `0o600` (logs may contain headers, API keys, and conversation content). Content format:
>
>   ```
>   # 2006-01-02T15:04:05.999999999+00:00 kind=llm-request
>   POST http://localhost:13305/v1/chat/completions
>   Content-Type: application/json
>   Authorization: Bearer sk-123
>
>   {"model":"gpt-4o-mini",...}
>   ```
>
>   Line 1 is `# ` + `r.Time` in RFC3339Nano + ` kind=<Kind>`. Line 2 is `<Method> <URL>` (for tool records: method `RUN` and the tool name as URL, e.g. `RUN echo`). Then one `<Key>: <values joined by ", ">` line per header, keys sorted alphabetically. Then a blank line. Then `Body` verbatim — never re-encoded, never pretty-printed; exact wire bytes are the point. When `Body` is empty, omit the blank line and write nothing after the header block.
> - Uniqueness and ordering: the file sink holds a `sync.Mutex` and the last-written `time.Time`. On each `Write`, under the mutex: if `r.Time` is zero use `time.Now()`; if the (UTC) time is not strictly after the last-written time, bump it to last+1ns; then write the file with that time and record it as the new last. This guarantees unique, strictly increasing filenames even when two records share a nanosecond, and makes the sink safe for concurrent producers.
> - Add `NewBuffered(size int) *Buffered`: an in-memory `Sink` retaining the last `size` records (all records when `size <= 0`), with a `Records() []Record` method returning a copy in write order. Commit 2 and 3 tests use this as their capturing sink. Keep the API small: no `Multi`, no config parsing, no network.
>
> Tests in `internal/logging/logging_test.go` (package `logging_test`):
> - Round-trip: writing a record with headers and a body to a sink from `New(t.TempDir())` produces exactly one file whose name fully matches `^\d{8}T\d{6}-\d{9}-llm-request\.txt$`, and whose content has the `# ` timestamp line with `kind=llm-request`, the `POST <url>` line, sorted `Key: Value` header lines, a blank line, then the exact body bytes.
> - Multi-value headers render comma-joined; header keys sort alphabetically; an empty body writes no blank separator line.
> - `New` rejects a nonexistent directory and a path that is a regular file, with descriptive errors; `NewNop()` writes nothing (verify a temp dir stays empty).
> - Ordering: write two records whose `Time` is explicitly the same nanosecond, plus a third with an earlier `Time`; assert three distinct files exist and that sorting filenames yields them in write order (the earlier-Time third record written last must be bumped above the first two).
> - A record with zero `Time` still produces a valid, now-stamped filename.
> - `NewBuffered` retains only the last `size` records and `Records()` returns them in write order (and is a copy: mutating the returned slice does not affect the sink).
>
> No other packages change. Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 2: OpenAI client request/response logging

> In `internal/llm/openai/openai.go`, add optional wire logging to the OpenAI client. The client is the only place with the exact request body, headers, URL, status, and response body, so the log is taken here, not reconstructed from `llm.Request`. Import `github.com/overspecific/blorb/internal/logging`.
>
> - Add `Sink logging.Sink` to `Config` (documented: when non-nil, one `KindLLMRequest` record per call and one `KindLLMResponse` record per response are written; nil disables logging. Write errors are ignored — logging is best-effort).
> - In `post` (shared by `Chat` and `ChatStream`), after building `httpReq` and before `httpClient().Do`: if the sink is non-nil, write `Record{Time: time.Now(), Kind: KindLLMRequest, Method: http.MethodPost, URL: endpoint, Headers: clone of httpReq.Header, Body: payload}`. `endpoint` is the joined `<base>/chat/completions` URL; clone the header map (`textproto.MIMEHeader` clone or manual copy) since the HTTP client may mutate it; `payload` is the marshaled request bytes (pass a copy — the record owns its body).
> - Add a `responseHeaders(resp *http.Response) map[string][]string` helper: a copy of `resp.Header` plus a synthetic `Status` key carrying `fmt.Sprint(resp.StatusCode)` (the status is not a header but belongs in the log's header block).
> - In `Chat`: after `respBody` is fully read and before any status or decode early return, if the sink is non-nil write `Record{Time: time.Now(), Kind: KindLLMResponse, Method: http.MethodPost, URL: endpoint, Headers: responseHeaders(httpResp), Body: respBody}`. Placing the write before the status check means non-2xx responses are logged with their body too. `Chat` currently builds `endpoint` inside `post`; the response record needs the same URL, so return or reconstruct it (e.g. have `post` return the final URL alongside the response, or factor a `chatCompletionsURL()` helper used by both the record building and `post`).
> - In `ChatStream`: the body is consumed as SSE, not buffered whole. After the non-2xx check, wrap `httpResp.Body` in a small `loggingBody` type (an `io.ReadCloser` that copies every successful `Read` chunk into an internal `bytes.Buffer` and delegates `Close`), cap the accumulation at `maxBodyLen` (stop accumulating, append a `...(truncated)` marker, keep reading), and register a deferred record write as the *last* defer in `ChatStream` so every return path — stream completed, scanner error, `onDelta` abort, context cancellation — logs the bytes received so far with `responseHeaders(httpResp)` and the accumulated body. The non-2xx path already reads the body with `io.ReadAll` before any wrapping: log there directly with that body and return, as today.
> - Keep the record-building logic in one place per side (a `logRequest`/`logResponse` pair of unexported helpers, or inline nil-guarded calls in `post`/`Chat`/`ChatStream`) so `Chat` and `ChatStream` cannot drift. Do not log at delta granularity — whole-response files only; the streamed record is written once, when the stream path finishes.
>
> Tests in `internal/llm/openai/openai_test.go`:
> - Extend the `newTestClient` helper to take a sink (existing tests pass nil and stay unchanged in behavior).
> - A successful `Chat` with a `logging.NewBuffered(0)` sink produces exactly two records: first `KindLLMRequest` with method POST, the full `<server>/chat/completions` URL, a `Content-Type: application/json` header, and a body containing the marshaled wire request (assert `model` and a message appear); then `KindLLMResponse` with the synthetic `Status: 200` header and the exact response body bytes. Assert request `Time` is before response `Time`.
> - A `Chat` against a 401 server still logs the response record with `Status: 401` and the server's error body.
> - A `ChatStream` against a server sending several SSE chunks logs one `KindLLMRequest` (body contains `"stream":true`) and one `KindLLMResponse` whose body is the raw SSE bytes (`data: ...` lines), written after the stream completes, with request `Time` before response `Time`.
> - A `ChatStream` aborted mid-stream by an `onDelta` error still logs a response record containing the bytes received up to the abort.
> - A `ChatStream` non-2xx response logs the response record with the error body and synthetic status.
> - With a sink whose `Write` returns an error, `Chat` and `ChatStream` still succeed and return the same results as with no sink.
>
> No other packages change (do not wire the sink into config yet). Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 3: Tool registry call/result logging

> In `internal/tools/tools.go`, add optional per-call logging to the registry, mirroring the OpenAI client's approach. Import `github.com/overspecific/blorb/internal/logging`.
>
> - Add `WithSink(s logging.Sink) Option` to the existing `Option` list; `nil` (the default) disables logging.
> - In `Run`, log two records per invocation, but only once execution is actually possible — right after the unknown-tool and invalid-args checks pass (i.e. immediately before `cmd.Start`). Nothing is logged for unknown tools or invalid args: no process ran, so there is no wire traffic to record. Document this decision in the `Run` comment.
>   - Request, before `cmd.Start`: `Record{Time: time.Now(), Kind: KindToolRequest, Method: "RUN", URL: name, Body: args}` — the post-normalization, validated args bytes, exactly what the tool receives on stdin. No headers.
>   - Result, written once per `Run` on every path after the request record — success, non-zero exit, timeout, context cancellation, `Start` failure, and the non-exit `Wait` failure: body is the `ToolResult.Output` string on the success and exit-failure paths (exactly what the model receives, including the `Tool %s failed with exit code %d.\nStderr: ...` formatting), or `error: <err.Error()>` on infrastructure-error paths. Use a small unexported `writeResult` helper (taking sink, name, body) called at each return point after the request record; do not use `defer`, so the timestamp reflects when the outcome was known and the helper cannot fire twice.
> - Sink writes are best-effort: a `Write` error is ignored and never changes `Run`'s return values.
>
> Tests in `internal/tools/tools_test.go`:
> - A `Run` against a real shell tool (the existing `entry("echo_args", ...)` pattern) with a `logging.NewBuffered(0)` sink logs a `KindToolRequest` record with `URL` = the tool name and body exactly the args JSON passed in, and a `KindToolResult` record with body equal to the `ToolResult.Output` returned to the caller. Assert request `Time` is before result `Time`.
> - A non-zero-exit tool (the existing `failer` pattern) logs a result record whose body matches the returned `ToolResult.Output` (contains `failed with exit code` and the stderr capture).
> - A tool that times out (the existing timeout pattern) logs a result record with `error: ... timed out ...` in the body, and `Run` still returns the timeout error.
> - `Run` with an unknown tool name or invalid args writes no records at all.
> - A sink whose `Write` returns an error does not affect `Run`'s return values on the success path.
> - Options: `WithSink(nil)` behaves as no sink; `WithSink` composes with `WithTimeout`.
>
> No other packages change (do not thread the sink from config yet). Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 4: Thread the logger through config, chat, and CLI

> Connect the pieces so a chat session writes real log files to `.logs` next to the config file. The engine is deliberately untouched: the client logs LLM wire traffic and the registry logs tool traffic, so no `EngineConfig` change is needed — verify this while building (the engine's event flow already carries everything; adding a sink there would be redundant).
>
> - In `internal/config/config.go`: add a `LogConfig` struct with `Path string` (`json:"path,omitempty"`) and `Enabled *bool` (`json:"enabled,omitempty"`, nil means true, so `"enabled": false` disables logging without removing the path). Add `Logging LogConfig` (`json:"logging"`) to `Config`. In `Validate`, when `Path` is set, reject anything that is not a single clean path component — `filepath.Base(Path) != Path`, or `Path` is `.` or `..` — since the directory is resolved relative to the config file's directory and `../` traversal would surprise users. Add accessors: `(c *Config) LoggingEnabled() bool` (false only when `Enabled` is non-nil and false) and `(c *Config) LogDir() string` (`.logs` when `Path` is empty).
> - In `internal/chat/chat.go`: add `ConfigPath string` to `Options` (documented: path to the `blorb.json` that produced `Config`; when empty, the session runs without file logging). In `Run`, before building the client and registry: when `ConfigPath` is set and `opts.Config.LoggingEnabled()`, resolve `dir := filepath.Join(filepath.Dir(opts.ConfigPath), opts.Config.LogDir())`, `os.MkdirAll(dir, 0o755)` (failure is a startup error: `fmt.Errorf("create log dir: %w", err)`), then `sink, err := logging.New(dir)` (its validation error also fails startup). Otherwise use `sink = logging.NewNop()` — always a non-nil sink, so downstream code has one path.
> - Still in `internal/chat/chat.go`: extend `NewClientWithGetenv` and `NewClient` to take a `sink logging.Sink` parameter (`(cfg config.Config, getenv func(string) string, sink logging.Sink) (llm.Client, error)`), passing it into `openai.Config.Sink` (nil sink = off). Update `Options.newClient` to pass the resolved sink. In `Run`, pass `tools.WithSink(sink)` to `tools.NewRegistry`. The `Options.NewClient` override keeps its existing signature `func(config.Config) (llm.Client, error)`; overrides ignore the sink (fakes don't log), which the new tests account for.
> - In `main.go`: in `chatCommand`'s `Action`, pass the already-read config path through: `chat.Options{..., ConfigPath: cmd.String("config")}`. No new flags: logging is on by default and configured via `blorb.json`.
> - New tests:
>   - `internal/config/config_test.go`: absent `logging` section parses with `LoggingEnabled()` true and `LogDir()` == `.logs`; `"enabled": false` parses with `LoggingEnabled()` false; `"path": "mylogs"` round-trips and `LogDir()` returns it; a path with a separator (`a/b`) and `..` each fail `Validate`.
>   - `internal/chat/chat_test.go`, end-to-end with the *real* openai client (fakes don't log): start an `httptest.Server` serving two canned chat completions (first with a tool call, then a final text), write a minimal valid `blorb.json` into a temp dir with `provider.base_url` pointing at the test server and one real shell tool (e.g. `echo`), set `Options.ConfigPath` to that file, run the session over stdin (`do it\nexit\n`), then `filepath.Glob` the temp dir's `.logs`: assert exactly the expected six files exist, and that their names, sorted lexically, have kind suffixes in the order `llm-request, llm-response, tool-request, tool-result, llm-request, llm-response` — the true sequence of a one-tool-round turn. Assert the tool request file contains the tool name and args, and an LLM request file contains the user message text.
>   - Also in `internal/chat/chat_test.go`: the same setup with `"logging": {"enabled": false}` produces no `.logs` directory (`os.Stat` reports `IsNotExist`); with `"logging": {"path": "mylogs"}` the files land under `<tmp>/mylogs/` instead.
>   - `internal/chat/chat_test.go`: `NewClientWithGetenv` with a nil sink returns a working client.
>   - `main_test.go`: assert the chat command still defaults `--config` to `config.DefaultPath` (existing test) and no new flags were added to `chatCommand`.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

> No other packages change. Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 6: Log the assembled response for streamed turns

> Commit 2 made `ChatStream`'s `KindLLMResponse` record capture the raw SSE event-source stream (`data: ...` lines) via a `loggingBody` reader wrapper, while `Chat` logs the final JSON response body. That is inconsistent: an `llm-response` file should show the response the model produced, in the same shape regardless of which path produced it. Change the streaming path to log the assembled final response instead.
>
> - In `internal/llm/openai/openai.go`, delete the `loggingBody` wrapper and the capture/defer write in `ChatStream` (openai.go:298-323, 377-391): the raw byte capture has no consumer once the assembled response is logged.
> - Add a `marshalWireResponse(resp *llm.Response) ([]byte, error)` helper that renders the completed neutral response back into the OpenAI wire response envelope — `{"id":...,"choices":[{"message":{...},"finish_reason":...}],"usage":{...}}` — reusing the existing neutral→wire message conversion (`neutralMessage`'s inverse: build a `wireMessage` from the response message the same way `wireMessages` does for history). Streaming and non-streaming `llm-response` files then carry directly comparable bodies.
> - Move the response log write into `readStream` (or a closure passed from `ChatStream`), since that is where the accumulator's assembled response exists:
>   - Stream completes normally → log the assembled response marshaled by `marshalWireResponse`, with the usual headers (`responseHeaders(httpResp)` including the synthetic `Status` entry).
>   - Stream aborts or errors after data arrived (`onDelta` error, scanner error, context cancellation) → log the *partial* assembled response from the accumulator — what the model had produced so far, still useful for debugging — and then return the error as today. `readStream` currently returns before building `acc.response()` on these paths; build it for the log write regardless.
>   - Stream fails with no data at all (the existing "no data in stream" error) → log a body of `error: <err>`, mirroring the tools' error-result convention, then return the error.
>   - Non-2xx response → unchanged: the whole HTTP error body is already logged before any wrapping.
> - `readStream` needs the response headers and endpoint URL for the record; pass them in (or pass a small log-options struct) from `ChatStream` rather than re-deriving. Keep the write best-effort: a `marshalWireResponse` or sink error on the happy path is ignored and never changes the return values; if marshaling fails, log `error: <marshal error>` so every streamed request still has a response record.
> - Timestamps: the response record's `Time` is taken when the stream path finishes (as now), so request-before-response ordering still holds on every path.
>
> Tests in `internal/llm/openai/openai_test.go`:
> - Update `TestChatStreamLogsRawSSE` (now misnamed — rename to `TestChatStreamLogsAssembledResponse`): the response record's body is the assembled JSON — `content` is the concatenated `"Hello"`, `finish_reason` is `"stop"`, the `id` round-trips, and it contains no `data:` lines and no `[DONE]` terminator. Its shape matches a non-streaming response record (same envelope structure).
> - Update `TestChatStreamLogsAbortedStream`: the response record's body is the partial assembled response — it contains `"first"` and `"second"` (the content received before the abort), not `"never seen"`, and not raw SSE lines.
> - Add `TestChatStreamLogsNoDataError`: a 200 response with an empty SSE body logs a response record whose body is `error: ...` mentioning no data, and `ChatStream` still returns the error.
> - Unchanged and still passing: `TestChatStreamLogsNon2xx`, `TestChatLogsRequestAndResponse`, `TestLoggingSinkErrorsAreIgnored` (the stream half of it), and the end-to-end chat tests in `internal/chat/chat_test.go` (they assert file counts and the user message in the request log, not response body shape).
> - Also verify no test still references `data:` lines or `[DONE]` in a *response* record assertion.
>
> No other packages change. Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 7: Per-conversation log directories

> Currently all log files from every session land directly in `.logs`, so sessions overwrite each other's timeline. Give each conversation (one `chat.Run` session) its own subdirectory of the log dir, named `<timestamp>-<uuid>` (e.g. `20260830T142533-a1b2c3d4e5f6...`), created when the session starts. Timestamp-first with the same `20060102T150405` layout as the log filenames, so the session directories themselves sort chronologically; the UUID (crypto-random) disambiguates sessions started in the same second.
>
> - In `internal/logging/logging.go`, add `NewSession(dir string) (string, error)`: creates a fresh session subdirectory under `dir` and returns its name. The name is `timeDirName(time.Now())` (reuse the existing helper — the `20060102T150405-<9 digits>` timestamp form, truncated to drop the fractional suffix or kept whole; pick `20060102T150405-123456789` in full for consistency with log filenames, dash included) + `-` + a hex UUID from `crypto/rand` (16 random bytes, `hex.EncodeToString` → 32 chars; no dependency on `google/uuid`). Validate `dir` exactly like `New` does (exists, is a directory) — factor that validation into a small unexported helper shared by `New` and `NewSession`. `os.MkdirAll` the subdirectory with `0o755` (an error is returned; callers treat it as startup-fatal, not best-effort — this is session setup, before any turn runs). Also add `Sink` construction convenience: `NewSession` returning the *sink* as well (`(logging.Sink, string, error)`: the subdirectory name and a file sink for it) saves callers the second step; keep the pieces composable though — the file sink itself is unchanged.
> - In `internal/chat/chat.go` `resolveSink`: after `os.MkdirAll(dir, 0o755)` of the log dir, call `logging.NewSession(dir)` to create the session subdirectory and build the file sink on *that* path, so every record from one session lands in `<config dir>/<logging.path>/<timestamp>-<uuid>/`. Startup error wrapping stays the same (`create log dir: ...`, `open log dir: ...` shapes). Disabled/`ConfigPath`-empty behavior is unchanged (nop sink, nothing created).
> - The client and registry take no changes: they receive the sink and are oblivious to which directory it writes into.
> - Tests in `internal/logging/logging_test.go`: `NewSession` on a valid dir creates exactly one subdirectory whose name matches `^\d{8}T\d{6}-\d{9}-[0-9a-f]{32}$` and returns a usable file sink (write a record through it; the file appears inside the subdirectory); two `NewSession` calls on the same dir yield two distinct names (the UUID differs even when the timestamp collides); invalid dirs (nonexistent, regular file) return errors, and no partial directory is left behind; the name sorts chronologically when two sessions are created in order (allow the timestamp-bump caveat: the monotonic guarantee is per-sink, not across `NewSession` calls, so only assert distinctness and the shape, plus ordering when the nanoseconds provably differ — simplest to assert the first session's name sorts before the second's only if construction used distinct `time.Now()` values; keep the ordering assertion lenient or drop it in favour of shape+distinctness).
> - Tests in `internal/chat/chat_test.go`: update the logging end-to-end tests to glob under the session subdirectory — after a run, `.logs` contains exactly one entry, a directory matching the `<timestamp>-<uuid>` pattern, and the six log files (from Commit 6's world: request/response pairs for two rounds plus the tool pair) live inside it with the same sorted-kind-sequence assertions as before. `TestRunLoggingDisabledWritesNothing` and `TestRunCustomLogPath` update their expectations the same way (`mylogs/<session>/` for the custom path). Also add: two sequential `chat.Run` sessions against the same config produce two distinct session subdirectories, with each session's files landing in its own.
> - README/example docs: update the Logging section (and the `examples/simple` paragraph) to say each session's logs go into its own `<timestamp>-<uuid>` subdirectory of `.logs`, so conversations never interleave.
>
> No engine changes. Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 8: Fix reasoning loss in streamed-response log records

> Two defects surfaced in the Commit 6/7 review, both in `internal/llm/openai/openai.go`'s streaming log path:
>
> 1. Major bug: `marshalWireResponse` builds the logged message via `wireMessages`, which deliberately drops `Reasoning` for messages without tool calls (the re-send-to-server rule: a final answer's reasoning is stale by the next request). That rule is about *requests*; an `llm-response` log must be full-fidelity — what the model actually produced. As it stands, a streamed final answer's chain-of-thought is silently missing from the log, corrupting the record of what the model said.
> 2. Minor, related cleanup: `readStream`'s `logResponse` closure takes a `resp *llm.Response` parameter that is never used — only `body` flows into the record, and the no-data path passes `nil`. Dead API surface that invites confusion.
>
> Changes, both confined to `internal/llm/openai/openai.go`:
>
> - In `marshalWireResponse`, stop routing through `wireMessages`: build the `wireMessage` for the response message directly, keeping `Reasoning` unconditionally (set `Reasoning: resp.Message.Reasoning` along with Role, Content, ToolCallID, and the ToolCalls conversion — mirroring `wireMessages`' conversion loop but without its reasoning-dropping branch). Add a short comment that the log record is full-fidelity and deliberately differs from `wireMessages`' request-side reasoning rule. Factor the tool-call conversion loop shared by both (the `for _, tc := range m.ToolCalls` body) into a small unexported helper if it keeps the duplication honest — otherwise leave `wireMessages` untouched.
> - Drop the unused `resp` parameter from `logResponse` (it becomes `func(body []byte)`), and update its three call sites; the `fail` helper no longer needs to pass the response alongside the body.
>
> Tests in `internal/llm/openai/openai_test.go`:
> - Extend `TestChatStreamLogsAssembledResponse`'s server chunks with a `reasoning_content` fragment, and assert the logged response body carries the assembled reasoning in `choices[0].message.reasoning_content` — pinning that the log is full-fidelity even for a final answer (no tool calls) where the *request* path would drop it.
> - The aborted-stream test (`TestChatStreamLogsAbortedStream`) already asserts the partial assembled JSON; no change needed beyond confirming it still passes.
> - No other tests change; the request-side reasoning behavior (resending reasoning only with tool-call rounds) is already pinned by existing tests and must remain intact.
>
> No other packages change. Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 5: Documentation and gitignore

> - In `README.md`:
>   - Add a Features bullet: full conversation and tool logging — every LLM request/response and tool call/result is written to `.logs` next to `blorb.json`, one timestamped file per wire interaction, so sorting the filenames replays the turn in order.
>   - Add a `logging` sub-section under Configuration: an example fragment `"logging": {"path": ".logs", "enabled": true}`, the defaults (enabled, `.logs` next to the config file), `"enabled": false` to disable, `path` restricted to a single path component (no separators), the file naming scheme (`<timestamp>-<kind>.txt`, e.g. `20260830T142533-123456789-llm-request.txt`, with kinds `llm-request`, `llm-response`, `tool-request`, `tool-result`), and that sorting filenames reconstructs the exact sequence of a turn.
>   - Note that logs contain full request/response bodies and headers, including any API key sent to the provider, and that log writes are best-effort: a logging failure never fails the agent run.
>   - Add `internal/logging` to the Development layout list (wire logging for LLM and tool interactions).
> - In `examples/simple/README.md`: add a short paragraph noting that running the example produces a `.logs` directory next to `blorb.json` with one file per LLM request/response and per tool call/result, and that it can be disabled with `"logging": {"enabled": false}`.
> - In `.gitignore`: add `.logs/` so local runs of the example or a root-level config don't dirty the tree.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.