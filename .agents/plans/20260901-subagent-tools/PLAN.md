# Subagent tools

Add a new tool type `"subagent"` that lets an agent use another agent defined in the same `blorb.json` as a tool. A tool entry of type `subagent` names a target agent; when the parent model calls the tool, blorb runs that agent to completion for one turn — resolving all of its tool calls, exactly like a chat turn — and returns the concatenated assistant text as the tool output. The default input schema is a single required `prompt` string, which becomes the subagent's user message; when the entry declares its own `args_schema`, the raw JSON arguments are presented as JSON in the user message (the subagent's system prompt is assumed to account for this). Config validation rejects agent references that don't resolve and any delegation cycle. The subagent run is exempt from the 30s per-tool timeout (it is bounded by the subagent's own `max_turns` and context cancellation), and its events — including streamed assistant text — are forwarded to the chat interface, rendered indented and labeled with the subagent's name, so the user watches the subagent work live.

Design in brief:

- `config` (Commit 1): `ToolTypeSubagent`, the `agent` field on `ToolEntry`, reference and cycle validation.
- `tools` (Commit 2): `subagentTool` plus the `SubagentRunner` interface, `SubagentEvent`/`SubagentResult` types (defined in `tools` because `engine` imports `tools`, not vice versa), registry wiring, and timeout exemption.
- `engine` (Commit 3): the real runner — `engine.NewSubagentRunner` builds a per-invocation registry and engine for the target agent and adapts `engine.Event`s to `tools.SubagentEvent`s, recursively wiring itself in so subagents can use subagents.
- `chat` (Commit 4): client factory wiring and the streaming display of subagent activity.
- Docs and the shipped example (Commit 5).

Event forwarding convention: the runner emits `SubagentEvent`s with `Agent` set to the running agent's name and `Depth` 0; every `subagentTool` that forwards events on behalf of a deeper run increments `Depth` by one and leaves `Agent` untouched. The top-level chat callback therefore sees the deepest producing agent's name and its absolute nesting depth.

Out of scope: Prefactor spans for nested activity (nested LLM calls are not wrapped in the tracing client; only the parent's spans are recorded — a documented limitation), and the future one-shot CLI / HTTP run modes.

## Todo

- [x] Commit 1: config — subagent tool type, reference and cycle validation
- [x] Commit 2: tools — subagentTool, SubagentRunner interface, registry wiring, timeout exemption
- [x] Commit 3: engine — the real SubagentRunner
- [x] Commit 4: chat — wiring and streaming subagent display
- [ ] Commit 5: docs and example

---

## Commit 1: config — subagent tool type, reference and cycle validation

> Add the `"subagent"` tool type to the config schema in `internal/config/config.go`:
>
> 1. Add `ToolTypeSubagent ToolType = "subagent"` alongside `ToolTypeCommand` and `ToolTypeBuiltin`, and include it in `SupportedToolTypes()` (keep the list sorted alphabetically: builtin, command, subagent).
> 2. Add a field to `ToolEntry` under a `// Fields for type "subagent".` comment:
>    ```go
>    // Agent names the target agent this tool delegates to; it must be
>    // a defined agent in the same config.
>    Agent string `json:"agent,omitempty"`
>    ```
> 3. In `ToolEntry.validate` add a `case ToolTypeSubagent:` branch: `agent` is required and must match `NamePattern`; `args_schema`, when set, must be valid JSON (same check as command tools); `command`, `builtin`, and `config` must be empty, rejected with messages matching the existing style (`"command is not valid for subagent tools"`, etc.).
> 4. In `Config.Validate`, after the per-agent validation loop (so all agent names are known), check every tool entry of type `subagent`: its `Agent` must name a defined agent, else error `tool %q: agent %q is not a defined agent`.
> 5. Cycle detection, also in `Config.Validate`: build the delegation graph — for each agent, for each tool name it lists, if that entry has type `subagent` there is an edge from the agent to the entry's target agent. Detect any cycle (including self-reference: an agent granted a tool that targets itself) with a DFS and report the path, e.g. `agent cycle detected: "a" -> "b" -> "a"`. A cycle anywhere in the config is rejected, since delegation depth is otherwise unbounded.
>
> Tests in `internal/config/config_test.go` plus testdata fixtures following the existing one-file-per-error-mode convention: a valid config with a subagent tool (asserted loadable via `Load`), missing `agent` field, `agent` not matching `NamePattern`, `agent` naming an undefined agent, `command`/`builtin`/`config` set on a subagent tool, invalid `args_schema` JSON, a two-agent cycle (`a` delegates to `b`, `b` back to `a`), and a self-cycle. Also assert the error message substrings the way existing validation tests do.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 2: tools — subagentTool, SubagentRunner interface, registry wiring, timeout exemption

> Create `internal/tools/subagent.go` implementing the subagent tool type, and wire it into the registry in `internal/tools/tools.go`.
>
> New exported types in `internal/tools/agent.go`:
>
> ```go
> // SubagentEventKind mirrors the engine event kinds relevant to display.
> type SubagentEventKind string
>
> const (
> 	SubagentText          SubagentEventKind = "text"
> 	SubagentThinking      SubagentEventKind = "thinking"
> 	SubagentTextDelta     SubagentEventKind = "text_delta"
> 	SubagentThinkingDelta SubagentEventKind = "thinking_delta"
> 	SubsubagentToolCall      SubagentEventKind = "tool_call"
> 	SubsubagentToolCallDelta SubagentEventKind = "tool_call_delta"
> 	SubsubagentToolResult    SubagentEventKind = "tool_result"
> )
>
> // SubagentEvent is one observable moment of a subagent run. Agent is
> // the name of the agent that produced it and Depth its nesting level
> // below the caller (1 for a directly invoked subagent).
> type SubagentEvent struct {
> 	Agent  string
> 	Depth  int
> 	Kind   SubagentEventKind
> 	Text   string
> 	Name   string
> 	Args   string
> 	Index  int
> 	Output string
> 	Failed bool
> }
>
> // SubagentResult is the outcome of a subagent run, mirroring ToolResult:
> // Err marks a subagent-level failure (e.g. it exhausted its max_turns),
> // which is still a valid tool result for the parent model.
> type SubagentResult struct {
> 	Output string
> 	Err    bool
> }
>
> // SubagentRunner executes a named agent from the same config as a
> // subagent: it runs the agent to completion for one user message —
> // resolving all of its tool calls, like a chat turn — and returns its
> // concatenated assistant text. Implementations emit events with Agent
> // set to the running agent's name and Depth 0 (relative to themselves).
> // The real implementation lives in the engine package; tests use fakes.
> type SubagentRunner interface {
> 	RunSubagent(ctx context.Context, agentName, userMessage string, onEvent func(SubagentEvent) error) (SubagentResult, error)
> }
> ```
>
> The default args schema, used when the entry declares none:
>
> ```go
> const defaultAgentArgsSchema = `{"type":"object","properties":{"prompt":{"type":"string","description":"The message to send to the agent."}},"required":["prompt"],"additionalProperties":false}`
> ```
>
> `subagentTool` holds `toolName`, `description`, `agentName` (the target), `argsSchema json.RawMessage` (empty means the default schema), `runner SubagentRunner`, and `events func(SubagentEvent) error` (may be nil). `newSubagentTool(e config.ToolEntry, runner SubagentRunner, events func(SubagentEvent) error) (tool, error)` revalidates the entry per the `NewRegistry` convention (non-empty `agent` matching `NamePattern`) and errors on a nil runner: `tool %q: subagent tools require a subagent runner (pass tools.WithSubagentRunner)`.
>
> `definition()` returns `llm.Tool` with the entry's schema, or the default schema when none is set.
>
> `run(ctx, args, sink)`:
>
> 1. Build the user message. With a custom `args_schema`: the raw JSON `args` verbatim. With the default schema: unmarshal into `struct { Prompt string \`json:"prompt"\` }`; a decode failure or empty prompt is a model-facing failure — `ToolResult{Err: true, Output: "tool %q: arguments must include a non-empty \"prompt\" string"}`, with a `writeResultRecord`, and no Go error.
> 2. Forward events: wrap the `events` callback into the `onEvent` passed to the runner — it increments `Depth` by one and passes the event through unchanged (it must not overwrite `Agent`, which names the deepest producing agent). A nil `events` callback becomes a no-op.
> 3. Call `runner.RunSubagent(ctx, agentName, userMessage, forward)`. A returned Go error is infrastructure failure: `writeResultRecord(sink, name, "error: "+err.Error())` and return it. Otherwise return `ToolResult{Output: trimSingleTrailingNewline(res.Output), Err: res.Err}`, writing the result record at each outcome point, following `command.go`'s conventions.
>
> Registry changes in `internal/tools/tools.go`:
>
> - New fields `subagentRunner SubagentRunner` and `subagentEvents func(SubagentEvent) error`; new options `WithSubagentRunner(r SubagentRunner)` and `WithSubagentEvents(cb func(SubagentEvent) error)`.
> - Dispatch `case config.ToolTypeSubagent:` in `NewRegistry` to `newSubagentTool(e, r.subagentRunner, r.subagentEvents)`.
> - Timeout exemption: a subagent tool's run is bounded by the subagent's `max_turns` and context cancellation, not the 30s per-tool timeout. Add an unexported marker interface `type longRunning interface { longRunning() bool }`, implement it on `subagentTool` returning true, and in `Registry.Run` skip the `context.WithTimeout` derivation (but still `defer cancel()` safely) for tools that are long-running.
>
> Tests in a new `internal/tools/subagent_test.go` (external test package, `t.Parallel()`, table-driven where sensible), using a fake `SubagentRunner` that records the agent name and user message and returns canned results/events: definition carries the default schema when none is set and the custom schema verbatim when set; default-schema prompt extraction (including whitespace-only prompt rejected); custom-schema args passed through as the exact JSON string; `SubagentResult` mapping (output trimming, `Err` propagation); Go error propagation; event forwarding bumps `Depth` and preserves `Agent`; nil events callback is tolerated; `NewRegistry` errors on a subagent entry without a runner; timeout exemption — a registry built with `WithTimeout(100*time.Millisecond)` whose fake runner blocks ~300ms completes successfully for the subagent tool. Also cover the `writeResultRecord` output via a `logging` sink fake or session if the existing tools tests have a pattern for it — follow whatever they do.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 3: engine — the real SubagentRunner

> Create `internal/engine/subagent.go` with the production `SubagentRunner` (the `engine` package already imports `tools`, so there is no cycle):
>
> ```go
> // SubagentRunnerConfig configures a SubagentRunner.
> type SubagentRunnerConfig struct {
> 	// Config is the whole loaded config: subagents, their tool grants,
> 	// and their providers are resolved from it.
> 	Config config.Config
> 	// NewClient builds the LLM client for a subagent's provider.
> 	NewClient func(agent config.Agent) (llm.Client, error)
> 	// Stream enables streaming in subagent engines; clients that do not
> 	// implement llm.StreamingClient fall back to whole messages.
> 	Stream bool
> 	// Sink receives the nested engines' wire logs.
> 	Sink logging.Sink
> }
>
> func NewSubagentRunner(cfg SubagentRunnerConfig) *SubagentRunner
> ```
>
> `RunSubagent` implements `tools.SubagentRunner`:
>
> 1. Resolve the target agent with `cfg.Config.Agent(agentName)`; missing is a Go error (`subagent %q is not defined in the config`) — it cannot happen for validated configs but guards programmatic ones.
> 2. Build the subagent's registry per invocation: `tools.NewRegistry(cfg.Config.AgentTools(agent), tools.WithSink(cfg.Sink), tools.WithConfigDir(cfg.Config.Dir()), tools.WithSubagentRunner(r), tools.WithSubagentEvents(onEvent))` — the runner passes its own `onEvent` straight through as the nested event callback (a pure relay: each `subagentTool` in the nested registry already adds its own `Depth` increment as events bubble up). `defer registry.Close()` so nested builtin sandboxes are released. A construction error is a Go error.
> 3. Build the subagent's client via `cfg.NewClient(agent)` (Go error on failure) and its engine: `engine.New(EngineConfig{Client: client, Tools: registry, SystemPrompt: agent.SystemPrompt, MaxTurns: agent.MaxTurnsOrDefault(), Stream: cfg.Stream})`.
> 4. Run `eng.RunTurn(ctx, userMessage, convert)` where `convert` maps `engine.Event` to `tools.SubagentEvent` — `EventAssistantText`→`SubagentText`, `EventAssistantThinking`→`SubagentThinking`, the delta kinds likewise, `EventToolCall`→`SubsubagentToolCall`, `EventToolCallDelta`→`SubsubagentToolCallDelta`, `EventToolResult`→`SubsubagentToolResult` — setting `Agent: agentName`, `Depth: 0`, and copying `Text`/`Name`/`Args`/`Index`/`Output`/`Failed` as applicable. A `convert` error aborts the subagent turn and is returned as a Go error. A nil `onEvent` becomes a no-op.
> 5. Map the outcome: if `ctx.Err() != nil`, return a Go error wrapping it (`subagent %q: %w`) — cancellation (e.g. SIGINT) is infrastructure. Any other `RunTurn` error — `ErrTooManyTurns`, provider truncation, API failure — is a subagent-level failure: `SubagentResult{Output: err.Error(), Err: true}` with no Go error, so the parent model sees and can react to it. On success return `SubagentResult{Output: final, Err: false}`.
>
> Tests in a new `internal/engine/subagent_test.go` reusing the existing fakes (`fakeClient`, `streamingClient`, `textResp`, `toolCallResp`, `call`) plus a client factory that dispatches on agent name: a no-tools subagent returns its final text and its request carries the subagent's system prompt and the user message; a subagent with a real command tool resolves its tool call before finishing and its concatenated assistant text (content alongside the tool call plus the final message) becomes the output; a two-level nesting test — parent engine with a subagent tool wired to the runner, subagent B itself holding a subagent tool to C, with distinct fake clients — asserting the top-level callback sees B's events at Depth 1 and C's at Depth 2, with `Agent` naming the deepest agent; a subagent that exhausts `max_turns` (set `max_turns: 1`, fake always returns a tool call) yields `SubagentResult{Err: true}` and no Go error, and the parent's history records a failed tool result; context cancellation returns a Go error; a failing `NewClient` returns a Go error. One streaming test asserting delta events arrive as `SubagentTextDelta` etc. when the factory returns a streaming client and `Stream` is true.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 4: chat — wiring and streaming subagent display

> Wire the subagent runner into the chat session and render subagent activity, including streamed output, in `internal/chat/chat.go`.
>
> Wiring in `Run`:
>
> 1. Generalize the client factory so it works for any agent, not just the session's: a helper on `Options` (e.g. `newClientFor(agent config.Agent, sink logging.Sink)`) that returns `opts.NewClient(agent)` when injected, else `NewClientWithGetenv(agent, getenv, sink)` — and reimplement the existing `newClient(sink)` in terms of it.
> 2. Construct the runner after the sink and client: `engine.NewSubagentRunner(engine.SubagentRunnerConfig{Config: opts.Config, NewClient: factory, Stream: opts.Stream && streaming, Sink: sink})` (the same `streaming` capability check the session's engine uses), and pass `tools.WithSubagentRunner(runner)` to the session's `tools.NewRegistry`.
> 3. Event pipe: the registry's event callback is set at construction but the per-turn printer is created inside `runTurn`. Add a small session-scoped holder:
>    ```go
>    type subagentPipe struct {
>        mu sync.Mutex
>        cb func(tools.SubagentEvent) error
>    }
>    ```
>    with `set(cb)` and `emit(ev) error` (no-op when unset), pass `tools.WithSubagentEvents(pipe.emit)` to the registry, and in `runTurn` set the pipe to the turn's subagent printer after creating it and clear it via `defer` before returning.
>
> Display: extend `chatEvents` to return a third value, `onSubagent func(tools.SubagentEvent) error`, alongside the existing `onEvent` and `flush`. It renders through the *same* writer and shares the `endLine`/`writeDelta`/`partialLine` machinery so subagent output interleaves cleanly with the parent's blocks (the parent's `>>> Result: Tool: ask_x` heading must terminate a partial subagent line, and vice versa). Rendering rules: indent headings and whole-message blocks by two spaces per `Depth` and label them with the producing agent — `  [researcher] >>> Assistant:`, `  [researcher] >>> Assistant (thinking):`, `  [researcher] >>> Tool: read`, `  [researcher] >>> Result:|Error: Tool: read` — mirroring the parent's heading conventions; whole messages print their text indented; delta fragments print inline (unindented) after their heading, labeled by it, using `writeDelta`. Track heading-once state per `(depth, agent)` for the delta streams (a map, cleared alongside `startRound`'s resets when a tool result ends a round). Events outside a turn cannot occur; the pipe drops them if ever unset.
>
> Tests in `internal/chat/chat_test.go`: a two-agent test config (parent granted a subagent tool targeting the second agent; the second agent granted a trivial command tool so a nested round trip is exercised) with `opts.NewClient` dispatching on agent name to separate fakes — parent fake: one tool call then final text; subagent fake: a tool-call round then final text. Assert the stdout contains, in order: `>>> Tool: ask_researcher`, the indented `[researcher]` assistant/tool blocks, then `>>> Result: Tool: ask_researcher` whose body is the subagent's concatenated final text; and assert the parent's request carried the subagent tool definition with the default prompt schema. A streaming variant using the streaming fakes for both agents, asserting the subagent's text deltas appear inline under its labeled heading while the parent's own deltas still render. A `--no-stream`-style non-streaming variant asserting whole-message subagent blocks. Update `newTestOptions`/`testConfig` helpers as needed rather than duplicating them.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 5: docs and example

> Document subagent tools and ship one in the example.
>
> 1. `README.md`: update the tools intro ("Tools are plain executables declared in the config, or built-ins...") and the feature bullet list to mention subagents. In the Tools section, after the `command` and `builtin` blocks, add a `**subagent tools**` block: the required `agent` field names a defined agent in the same config (a config error if it doesn't, as is any delegation cycle); `args_schema` is optional — by default the tool takes a single required `prompt` string that becomes the subagent's user message, and with a custom schema the JSON arguments are presented as JSON to the subagent (its system prompt is expected to account for that). Describe the execution semantics: the subagent runs to completion — all of its tool calls resolved, the same as a chat turn — and the concatenated assistant messages become the tool output; subagent tools are exempt from the 30s per-tool timeout and are bounded by the subagent's `max_turns`; subagents can themselves use subagent tools (acyclically); and the chat interface shows the subagent's activity live, indented and labeled with its name. Note the limitation that nested LLM calls are not traced to Prefactor (only the parent's spans are).
> 2. `examples/simple/blorb.json`: add a subagent tool — `{"type": "subagent", "name": "ask_scholar", "description": "...", "agent": "scholar"}` — granted to `simple` only (never to `scholar`, which would be a cycle), and update the example's README/notes if it lists the tools. The Prefactor example test loads and validates the shipped example configs, so `bin/qc` must still pass with the changed file.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.
