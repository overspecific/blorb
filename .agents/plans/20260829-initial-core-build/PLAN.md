# Initial core build

Builds the core of Blorb end to end: reading `blorb.json`, calling an LLM through a provider-neutral client seam (first implementation: OpenAI-compatible chat completions over plain HTTP + JSON, no SDKs), executing tools declared in config as subprocesses, and driving the agent turn loop in a terminal chat interface. The design deliberately separates provider-neutral types and the `llm.Client` interface from any provider implementation so new LLM clients (Anthropic, Bedrock, Ollama, …) are additive packages. CLI one-shot mode and HTTP serving are deliberately left for a later plan. Everything uses the standard library only, Go 1.27, with comprehensive unit tests.

## Todo

- [ ] Commit 1: Skeleton + `blorb.json` config loading
- [ ] Commit 2: Provider-neutral LLM types and `Client` interface
- [ ] Commit 3: OpenAI-compatible client implementation
- [ ] Commit 4: Tool registry and runner
- [ ] Commit 5: Agent engine turn loop
- [ ] Commit 6: Chat mode and main wiring

---

## Commit 1: Skeleton + `blorb.json` config loading

> Create the Go module (`github.com/overspecific/goblorb`, Go 1.27) and the core configuration types.
>
> Create `main.go` with a `version` variable (wired for `-X main.version` from `bin/build`) that prints usage for now: no subcommand prints help to stderr and exits 2; an explicit `version` subcommand prints the version. This keeps `bin/build` working from commit 1.
>
> Create package `config` (directory `internal/config`) defining the `blorb.json` schema. Top level fields: `name` (required, non-empty), `system_prompt` (required, non-empty), and `provider` (required object). The provider object is the extension point for multiple LLM backends: `type` is required (a discriminator; this build recognizes only `"openai"`, and validation must reject unknown types with a message listing supported types) and the remaining fields are interpreted per type — for `"openai"`: required `model`, required `base_url` (must be a valid URL with http or https scheme), optional `api_key_env` (name of an environment variable holding the API key; optional, empty means no auth header). Future provider types will carry their own fields under the same `provider` object, so keep provider parsing in its own struct with a type-aware `Validate` rather than flattening fields to the top level. Plus optional integer `max_turns` (default 10, minimum 1). Field `tools` is an optional array of objects with required non-empty `name`, required non-empty `description`, and required `command` (a non-empty array of strings to exec); `args_schema` optional free-form JSON schema text. Tool names must be unique. Parse tool names with a strict regex `^[a-zA-Z0-9_-]+$` so they are valid function names for the API and safe to exec.
>
> Implement `config.Load(path string) (Config, error)` which reads and parses the file with clean, wrapped errors (include the file path and, for JSON errors, the decode message). Implement `(*Config).Validate() error` performing all validation above, dispatching to per-provider validation by `provider.type`. Use `json` struct tags with `snake_case` names and strict decoding (reject unknown fields via a `Decoder.DisallowUnknownFields` based path) so typos in `blorb.json` fail loudly.
>
> Tests in `internal/config/config_test.go`: table-driven golden tests using testdata files under `internal/config/testdata/` covering a valid config with two tools, missing required fields (each of name, system_prompt, provider type, provider model, provider base_url, tool name/description/command), duplicate tool names, bad tool name regex, bad base_url scheme, unknown provider type, unknown top-level field, and the max_turns default and minimum behaviour. Verify testdata coverage so every branch is exercised.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 2: Provider-neutral LLM types and `Client` interface

> Create package `llm` (directory `internal/llm`) holding the provider-neutral vocabulary shared by the engine, tools, chat, and all future client implementations. Nothing here talks to the network.
>
> Define the conversation types: `Role` constants (`RoleSystem`, `RoleUser`, `RoleAssistant`, `RoleTool`); `Message` with `Role`, `Content` string, optional `ToolCalls []ToolCall`, and `ToolCallID` string for tool-result messages; `ToolCall` with `ID`, `Type` (constant `"function"`), `FunctionName`, and `FunctionArgs` holding the JSON-encoded arguments string. For OpenAI the wire shape is nearly identical, so give these types `json` tags matching that shape but keep them structs this package owns; providers whose wire format differs (e.g. Anthropic) convert in their own package rather than leaking wire types here. Add a `ToolCall.DecodedArgs() (json.RawMessage, error)` helper so callers never manipulate the encoded form. Define `Tool` (name, description, JSON-schema parameters), `Request` (model, messages, tools optional), `Response` (id, message, `FinishReason`, `Usage` with token counts), and a `FinishReason` string type with the common OpenAI-compatible values (`stop`, `tool_calls`, `length`, plus `""` and unknown values passed through unchanged so exotic servers do not break parsing). Constructors worth adding: `NewToolResultMessage(toolCallID, output string, failed bool)` and `NewTextMessage(role, content string)` to keep message assembly consistent across call sites.
>
> Define the seam every client implements: `type Client interface { Chat(ctx context.Context, req Request) (*Response, error) }`. Document the contract on the interface: implementations must send the full message history each call, honour context cancellation, and surface missing tool_call ids as errors. This is the single interface the engine consumes; adding a provider means adding a package, not touching the engine.
>
> Tests in `internal/llm/llm_test.go`: table-driven marshaling tests for `Message` across all roles (including assistant-with-tool-calls and tool-result shapes), `DecodedArgs` success and error paths, constructor helpers producing correctly tagged JSON, and `FinishReason` round-trips including unknown values. No other packages change.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 3: OpenAI-compatible client implementation

> Create package `llmopenai` (directory `internal/llm/openai`) implementing `llm.Client` for OpenAI-compatible `/chat/completions` servers over plain `net/http` + JSON. It imports `internal/llm` for the request/response types; define only the wire-specific bits here (the response envelope).
>
> Implement `openai.New(cfg Config) *Client` where `Config` carries `BaseURL`, `Model` (default per request), `APIKey` (sets `Authorization: Bearer` when non-empty), and an optional `HTTPClient *http.Client` for tests. Implement `(*Client).Chat(ctx context.Context, req llm.Request) (*llm.Response, error)`: marshal, POST to `<base_url>/chat/completions` (join paths carefully so a base URL with or without trailing slash works, and reject non-http(s) schemes), set `Content-Type: application/json`, and on non-2xx return a descriptive error including the status code and a truncated response body. On context cancellation return the context error. Decode the envelope (`id`, `choices` — take index 0 — each with `message` and `finish_reason`, plus `usage`) into `llm.Response`, erroring if the response has no choices or a tool_call message is missing ids. Decode errors are wrapped with the body prefix.
>
> Tests in `internal/llm/openai/openai_test.go` using `httptest.NewServer`: success round-trip verifying the exact request JSON including a tools array and tool-result messages; base URL with both trailing and non-trailing slash; 401 error surface; 500 with HTML body surfacing status plus truncated body; context cancellation mid-request; API key header presence; invalid JSON response; empty-choices and missing-tool-call-id error paths. No other packages change.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 4: Tool registry and runner

> Create package `tools` (directory `internal/tools`) implementing execution of tools declared in config (`config.Config.Tools` -> `config.Tool`: name, description, command, args_schema).
>
> Define `type Tool struct` with `Name`, `Description string`, `ArgsSchema string` (free-form JSON schema text passed through to the API; default `"{}"` when empty), and the exec command. Define `type ToolResult struct` with `Output string` and `Err bool` marking tool-level failure. Implement `NewRegistry(cfg []config.Tool) (*Registry, error)` which converts config entries (revalidating names) and `(*Registry).Definitions() []llm.Tool` producing API tool definitions (name, description, parameters) using the neutral `llm` types.
>
> Implement `(*Registry).Run(ctx context.Context, name string, args json.RawMessage) (ToolResult, error)`: look up the tool (unknown name is an error wrapping the set of known names for debugging), validate `args` is a JSON object (`{}` it when nil/empty), then exec the command array with the args JSON as the process's stdin. Wire stdin/stdout/stderr via pipes; the tool's stdout is the result `Output` (trimmed of a single trailing newline); stderr is captured for error reporting only. Enforce a per-call timeout of 30 seconds (constant, later configurable) via context: on expiry kill the process group (use `syscall.SysProcAttr{Setpgid: true}` and kill `-pid`) and return a timeout error. A non-zero exit produces `ToolResult{Err: true}` whose Output tells the model the tool failed, including exit code and captured stderr (truncated to 4KiB); `Run` returns no error in that case since a failed tool is still a valid tool message for the model. Only infrastructure failures (unknown tool, bad args, timeout, unusable stdin) return errors.
>
> Tests in `internal/tools/tools_test.go`: registry conversion including empty ArgsSchema defaulting, duplicate and invalid names rejected; Run success with stdin JSON echoed back by a shell script; non-zero exit producing `ToolResult{Err: true}` with stderr captured; timeout killing a `sleep` script (use a short custom timeout via an exported `WithTimeout` option on Run or Registry to keep the test sub-second); nil args coerced to `{}`; invalid JSON args rejected; unknown tool name error listing known tools; verify no zombie processes remain after timeout by checking the child is gone. Mark tests that exec shell scripts to skip on `runtime.GOOS == "windows"` (we still only support exec-style commands).
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 5: Agent engine turn loop

> Create package `engine` (directory `internal/engine`) that owns a conversation and drives turns against an `llm.Client` implementation and `tools.Registry`.
>
> Define `type Engine struct` constructed via `New(e EngineConfig)` with: `Client llm.Client` (the provider-neutral seam from commit 2 — the engine must not import any specific provider package, keeping new clients purely additive), `Tools *tools.Registry`, `SystemPrompt`, and `MaxTurns` (default 10 when zero). The model travels inside `llm.Request`, whose default is supplied by the client's own configuration, so the engine stays model-agnostic. State: ordered `[]llm.Message` conversation history; only the engine mutates it.
>
> Implement `(*Engine).RunTurn(ctx context.Context, userMessage string, onEvent func(Event) error) (finalText string, err error)`. A turn appends the user message, then loops up to `MaxTurns` API calls: send history (seeding the system prompt as the first system message when set), and examine the assistant reply. If it contains tool calls, append the assistant message to history, run each tool through the registry, emit an `EventToolCall` (name + args) then an `EventToolResult` (name, output, err) per call, append tool-result messages built with `llm.NewToolResultMessage` (with `tool_call_id`; on registry infrastructure failure append an error tool message so the conversation stays protocol-valid), and continue the loop. If the reply is a plain message, append it to history, emit `EventAssistantText`, and finish the turn with its content as the final text. If the reply has both content and tool calls, emit content then execution proceeds. When `MaxTurns` is exhausted without a final text, return a descriptive `ErrTooManyTurns` wrapping the last assistant message (if any) so callers can present partial output. Cancellation propagates; `onEvent` errors abort the turn with that error (this is how the UI can later implement stop).
>
> Define `type Event struct` with a `Kind` enum (`EventAssistantText`, `EventToolCall`, `EventToolResult`) and the relevant fields; document that events are emitted before the corresponding history mutation takes effect for results. Also implement `(*Engine).History() []llm.Message` returning a defensive copy for testing and future persistence.
>
> Tests in `internal/engine/engine_test.go` using a scripted fake `llm.Client` (table of canned responses asserting request contents): plain reply round-trip with system prompt seeded as message 0; single tool call resulting in exactly one registry execution and a tool-result follow-up with matching `tool_call_id` and correctly encoded arguments; multi-turn where the model calls two tools in one assistant message; tool failure (registry infra error) producing an error tool message and conversation continuing to a final text; `ErrTooManyTurns` firing when a fake always returns tool calls, including partial-text handling; event ordering and payload assertions; `onEvent` returning an error aborting the turn; context cancellation; `History()` defensive-copy check. No changes to other packages.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 6: Chat mode and main wiring

> Wire everything into `main.go` behind a `chat` subcommand, the first run mode.
>
> CLI shape: `blorb chat [-config path]` loads the config (default path `./blorb.json`), builds the configured client via a provider factory — a small `newLLMClient(cfg config.Config) (llm.Client, error)` that switches on `provider.type`, resolves `api_key_env` from the environment (error cleanly if the env var is set in config but absent), and constructs the matching client (today only `openai.New`); a switch is enough until a second provider exists, at which point it graduates to a registry map. Also builds `tools.Registry`. Runs the engine in a REPL: print a small banner with agent name and model (to stderr so stdout stays clean), read lines from stdin; empty input continues, `exit`, `quit`, or EOF ends the session. For each user line run one engine turn with an `onEvent` callback that writes assistant text to stdout as it arrives and tool activity as indented stderr lines (e.g. `→ tool_name(args…)` then `← result`). Exit code 0 on graceful exit, non-zero on config/startup errors with a clean single-line error message (no stack traces). Support Ctrl-C (SIGINT): on first signal cancel the in-flight turn and show a fresh prompt; a second signal exits immediately. Also accept the `-version` flag and keep the `version` subcommand from commit 1.
>
> Structure `main.go` as a thin entrypoint delegating to package `chat` (directory `internal/chat`) containing `chat.Run(ctx, Options) error` with the REPL logic so it is testable. Terminal output uses plain fmt to stdout/stderr; no readline or TUI libraries. Long lines and wrapped output are acceptable for v1.
>
> Tests in `internal/chat/chat_test.go`: drive `Run` with an in-memory `io.Reader`/`io.Writer` pair and a scripted fake `llm.Client` (not a real client) — asserting: a session with one user line and a plain assistant reply produces the banner, the reply on stdout, and exit nil; `exit` command terminates; EOF terminates; empty lines are ignored; tool events appear on stderr in order. No network in tests. Unit test the provider factory too: unknown provider type error text, and missing-`api_key_env` error text. Additionally unit test the flag/config resolution helper (default path, missing file error text); asserting `go run . version` semantics at test level is unnecessary.
>
> End the REPL loop cleanly on context cancellation without error output.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.