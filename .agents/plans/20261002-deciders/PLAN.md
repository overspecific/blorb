# Deciders: decision models like Jev as agent tools

Execution: work through the stages in order, one commit per stage, without pausing unless the user asks to stop after each commit for review. The exception: if a stage cannot be done the way this plan is written, pause and ask the user what to do before continuing.

Add support for decision models (System One models, in TypeSafe's vocabulary; Jev is the first example) alongside chat LLMs. A decision model does not generate text: you send it a state and a set of typed questions (choice, noul, score) and it returns typed answers with probabilities and confidence values. Blorb gains three things: a `model_type` field on models (`"llm"` is the default, `"decision"` is new), a top-level `deciders` section where a decider is defined by naming a decision model and fixing its questions, and a `"decider"` tool type that lets an agent use a decider. A decider tool call supplies only the state (a string with the default schema, or the raw arguments as JSON with a custom one); blorb makes one decision API call with the decider's questions and returns the answers as a JSON object for the parent model to read. A decider is much simpler than an agent: no loop, no turns, no tools, no history - one request in, one typed response out - so it has a small runner rather than an engine. Two conveniences round it out: the console shows the decision itself - the answers, labeled with the decider's name and indented like subagent activity - not just the surrounding tool call, and a new `blorb decide` command runs a decider directly, the decider counterpart of `blorb run`.

Design in brief:

- `config` (Commits 1 and 2): the `model_type` field with validation gating the llm-only knobs, the `deciders` section with question validation, and the `"decider"` tool type with reference validation. The existing provider-type constants (`ModelTypeOpenAI`, `ModelTypeOllama`) are renamed to `ProviderType*` so the model_type vocabulary gets the clear name.
- `llm` (Commit 3): neutral decision request/response types and a `DeciderClient` interface, plus an HTTP client for the System One wire protocol (`POST {base_url}/systemone`, the shape TypeSafe, OpenRouter, Venice and other vendors share).
- `tools` (Commit 4): `deciderTool`, the `DeciderRunner` interface and `DeciderResult` (in `tools` because `engine` imports `tools`, not vice versa), the `SubagentDecision` event kind, and registry wiring.
- `engine` (Commit 5): the real runner, plus wiring the runner into the subagent and judge registries so subagents and judges can also use decider tools.
- Frontends (Commit 6): chat, run and band construct and pass the runner; chat gains the real decision client factory; the decision renders in the console through the existing subagent-activity channel.
- The `blorb decide` command (Commit 7): a new `internal/decide` package and CLI subcommand - one state in, answers JSON out.
- Docs and a new example (Commit 8).

Decisions made with the user: the `model_type` value is `"decision"`; no new provider type - a decision model sits on an `openai-compatible` provider (bearer key plus base_url) and its `model_type` selects the decision client; the client appends a fixed `/systemone` path to the provider's `base_url` (matching how the openai client appends `/chat/completions`), which works for TypeSafe, OpenRouter's `/api/v1/systemone` surface, jevmodel.org, apimodels and Venice (which serves the same handler at `/systemone`); decider questions live in the decider config, not the tool entry - the tool call supplies only the state.

Usage and events: a decision call spends input tokens, so it is accounted like a nested LLM call. The runner returns one `SubagentUsageRecord` (agent = the decider's name, model = the resolved model name); the tool puts it in `ToolResult.Usage` and emits it as a `SubagentUsage` event through the registry's existing subagent-event callback with `Depth` 1. This reuses the whole accounting chain in chat, run and band with no new event family. The decision itself rides the same channel: the tool emits a `SubagentDecision` event carrying the answers JSON, which chat renders as a `[decider] >>> Decision:` block (indented like subagent activity, always in full), run's ndjson emits as a `subagent_decision` line, and band inherits through the shared `chat.Events` renderer. A decider run stays under the registry's per-tool timeout (decision calls are sub-second; a hung server fails like any other tool).

Out of scope: Prefactor tracing of decision calls (they are not wrapped in the tracing client; same limitation as nested subagent calls), output formats for `blorb decide` (its stdout is already JSON; a `--format` flag can come later), and any client-side enforcement of vendor limits the server already enforces (question counts, state size).

## Todo

- [x] Commit 1: config - the model_type field and the provider-type rename
- [x] Commit 2: config - deciders and the decider tool type
- [x] Commit 3: llm - decision types and the System One client
- [ ] Commit 4: tools - deciderTool, SubagentDecision event, registry wiring
- [ ] Commit 5: engine - the real DeciderRunner, wired into subagent and judge registries
- [ ] Commit 6: chat, run and band wiring, decision display
- [ ] Commit 7: the decide command
- [ ] Commit 8: docs and example

---

## Commit 1: config - the model_type field and the provider-type rename

> The `type` on a provider and the new `model_type` on a model are different vocabularies, and the current constant names would collide. Rename the provider-type constants first, then add `model_type`.
>
> 1. Rename `config.ModelTypeOpenAI` to `config.ProviderTypeOpenAI` and `config.ModelTypeOllama` to `config.ProviderTypeOllama`, and rename `SupportedModelTypes()` to `SupportedProviderTypes()`. Update every reference: `internal/config/config.go` (constants, `Provider.validate`, helpers), `internal/chat/chat.go` (the `clientFactories` map keys and the unknown-type error), and all test files that name them. This is a mechanical rename; `bin/qc` must pass with no behavior change.
> 2. Add the model_type vocabulary to `internal/config/config.go`:
>
>    ```go
>    // ModelTypeLLM is the default model_type: a chat completions model
>    // the agent engine drives in a message loop.
>    ModelTypeLLM = "llm"
>
>    // ModelTypeDecision selects a decision model: a System One model
>    // (Jev is the first example) that evaluates a state against typed
>    // questions and returns typed answers with probabilities. Only
>    // deciders may reference a decision model.
>    ModelTypeDecision = "decision"
>    ```
>
>    Add `Model.ModelType string \`json:"model_type,omitempty"\`` to the `Model` struct with a doc comment explaining both values and that empty means llm, and add:
>
>    ```go
>    // ResolvedModelType returns the model's model_type, or ModelTypeLLM
>    // when unset.
>    func (m *Model) ResolvedModelType() string
>
>    // SupportedModelTypes lists the model_type values this build
>    // recognizes, sorted alphabetically.
>    func SupportedModelTypes() []string
>    ```
>
> 3. Extend `Model.validate` (it already resolves the provider, so the provider type is in scope):
>    - Reject any `model_type` outside `""`, `llm` and `decision` with the existing style: `model_type %q must be one of: %s` naming `SupportedModelTypes()`.
>    - For decision models, reject every llm-only knob with messages of the form `%s is not valid for decision models` for `reasoning_effort`, `format`, `keep_alive`, `tool_choice` (which also covers `forced_tool`, since the existing pair rule rejects a forced tool without force mode), and `logprobs`/`top_logprobs` (check `m.Logprobs` is false and `m.TopLogprobs` is nil).
>    - For decision models, `model_name` becomes optional: the wire request omits the model field and the server applies its default. Keep `model_name` required for llm models.
>    - For decision models, require the provider's type to be `ProviderTypeOpenAI`: a decision model needs a plain base_url plus bearer key, and the ollama provider type has no decision surface. Error: `model_type "decision" requires an openai-compatible provider`.
> 4. Extend `Agent.validate` to reject a decision model: resolve the named model in the models list (it already checks the model is defined) and error `model %q is a decision model; agents require an llm model` when its `ResolvedModelType()` is not `ModelTypeLLM`.
>
> Tests in `internal/config/config_test.go` plus testdata fixtures following the one-file-per-error-mode convention: a valid config with a decision model (with and without `model_name`) loading through `Load`; unknown `model_type` value; a decision model with each of the rejected knobs set; a decision model on an `ollama` provider; an llm model with no `model_name` still erroring; and an agent whose model is a decision model. Assert error message substrings the way existing validation tests do.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 2: config - deciders and the decider tool type

> Add the `deciders` section and the `"decider"` tool type to `internal/config/config.go`.
>
> 1. New types:
>
>    ```go
>    // Decider is one named decider definition inside a config. It names a
>    // decision model and fixes the typed questions every call asks; only
>    // the state varies per call. Deciders are reachable only through
>    // decider tools; they are not agents and have no tools, turns, or
>    // judges of their own.
>    type Decider struct {
>        Name string `json:"name"`
>        Model string `json:"model"`
>        Questions map[string]Question `json:"questions"`
>    }
>
>    // Question is one typed question a decider asks. Type is one of
>    // choice, noul, or score. Instructions is the question itself: a
>    // string for short questions, or an object or array putting the
>    // question in one field and the data that guides it in the others;
>    // it is passed to the server verbatim. Criteria defines the answer
>    // space: an object of two or more option descriptions for choice, an
>    // ordered array of two to ten level descriptions for score, and
>    // optional true/false label descriptions (an object) for noul.
>    type Question struct {
>        Type string `json:"type"`
>        Instructions json.RawMessage `json:"instructions"`
>        Criteria json.RawMessage `json:"criteria,omitempty"`
>    }
>    ```
>
>    Constants `QuestionTypeChoice = "choice"`, `QuestionTypeNoul = "noul"`, `QuestionTypeScore = "score"`, a `SupportedQuestionTypes()` helper, and `QuestionNamePattern = ^[a-zA-Z0-9_]+$` (the wire format restricts question names to identifiers; the server also caps them at 64 characters, so check that too).
> 2. Add `Deciders []Decider \`json:"deciders,omitempty"\`` to `Config` and a lookup method `func (c Config) Decider(name string) (Decider, bool)` mirroring `c.Agent`.
> 3. `Decider.validate(models []Model)`: name required and matching `NamePattern`; model required, defined, and a decision model (else `model %q is not a decision model`); questions required with at least one entry. Per question, errors name the decider and question: name must match `QuestionNamePattern` and be at most 64 characters; type must be in `SupportedQuestionTypes()`; instructions must be present and valid JSON; criteria by type: required for choice and score, and for choice it must be a JSON object with at least two entries, for score a JSON array of two to ten entries, and for noul either absent or a JSON object.
> 4. In `Config.Validate`, after the models loop and before the agents loop (deciders reference models; agents are unaffected): validate each decider with a `decider %q: %w` wrapper, then check unique decider names (`duplicate decider name %q`, matching the existing duplicate-name style).
> 5. Add `ToolTypeDecider ToolType = "decider"` and include it in `SupportedToolTypes()` (keep the list sorted: builtin, command, decider, subagent). Add a field to `ToolEntry` under a `// Fields for type "decider".` comment:
>
>    ```go
>    // Decider names the decider this tool evaluates; it must be a
>    // defined decider in the same config.
>    Decider string `json:"decider,omitempty"`
>    ```
>
>    In `ToolEntry.validate`, add a `case ToolTypeDecider:` branch: `decider` is required and must match `NamePattern`; `args_schema` is allowed (a decider tool may define its own state schema) and must be valid JSON; `command`, `builtin`, `config`, and `agent` must be empty, rejected with the existing per-type style (`command is not valid for decider tools`, etc.). `supportedToolEntryTypes` picks the new type up automatically through `SupportedToolTypes()`. Update the sorted-list assertions in `internal/config/config_test.go` that hard-code `SupportedToolTypes()` and `supportedToolEntryTypes` (the alphabetical-order test near config_test.go:928 and any per-location variant) to include `"decider"` in the expected lists.
> 6. Add `validateDeciderRefs` mirroring `validateSubagentRefs`: every decider tool entry at the top level and inside every toolset must name a defined decider, else `tool %q: decider %q is not a defined decider` (and the `toolset %q:` prefix variant). Call it from `Config.Validate` next to `validateSubagentRefs`. No cycle detection is needed: deciders hold no tools and nothing recurses through them.
>
> Tests plus testdata fixtures: a valid config with a decider and a decider tool (asserted loadable via `Load`, including a custom `args_schema` and a noul question with criteria); decider with missing or non-matching name; missing model; model naming an undefined model; model naming an llm model; no questions; question name with a bad character, and one over 64 characters; unknown question type; missing instructions; instructions that is invalid JSON; choice without criteria, with criteria that is not an object, and with only one option; score without criteria, with criteria that is not an array, with one level, and with eleven levels; noul with criteria that is not an object; duplicate decider names; decider tool with missing `decider`; decider tool naming an undefined decider (top level and inside a toolset); decider tool with `command`/`builtin`/`config`/`agent` set; decider tool with invalid `args_schema` JSON.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 3: llm - decision types and the System One client

> Add the neutral decision types and the HTTP client, mirroring how `llm.Client` and the openai/ollama packages split the seam.
>
> 1. New file `internal/llm/decision.go` (package `llm`), holding the provider-neutral decision vocabulary:
>
>    ```go
>    // DecisionRequest is one decision call: a state to evaluate and the
>    // typed questions to ask about it. All questions are evaluated in
>    // parallel against the same state; no question sees another's
>    // answer. State and questions are passed to the server verbatim.
>    type DecisionRequest struct {
>        Model string `json:"model,omitempty"`
>        State json.RawMessage `json:"state"`
>        Questions map[string]DecisionQuestion `json:"questions"`
>    }
>
>    // DecisionQuestion is one typed question. Type is choice, noul, or
>    // score; see the config package for the criteria rules.
>    type DecisionQuestion struct {
>        Type string `json:"type"`
>        Instructions json.RawMessage `json:"instructions"`
>        Criteria json.RawMessage `json:"criteria,omitempty"`
>    }
>
>    // DecisionAnswer is one question's typed answer. Which fields are
>    // set depends on the question's type: choice sets Choice,
>    // Probabilities and Confidence; score sets Score, Probabilities and
>    // Confidence; noul sets Noul (the calibrated probability of yes).
>    // The pointer fields keep a meaningful zero (a score of 0, a noul
>    // of 0) distinguishable from an absent field.
>    type DecisionAnswer struct {
>        Type string `json:"type"`
>        Choice string `json:"choice,omitempty"`
>        Probabilities map[string]float64 `json:"probabilities,omitempty"`
>        Confidence *float64 `json:"confidence,omitempty"`
>        Noul *float64 `json:"noul,omitempty"`
>        Score *float64 `json:"score,omitempty"`
>    }
>
>    // DecisionResponse is the outcome of one decision call: the server's
>    // resolved model name, one answer per question (keyed by the
>    // request's question names), and the reported usage.
>    type DecisionResponse struct {
>        Model string
>        Answers map[string]DecisionAnswer
>        Usage Usage
>        Stats CallStats
>    }
>
>    // DeciderClient is the provider-neutral decision client seam. The
>    // engine depends only on this interface; the real client lives in
>    // internal/llm/decision and tests use fakes.
>    type DeciderClient interface {
>        Decide(ctx context.Context, req DecisionRequest) (*DecisionResponse, error)
>    }
>    ```
>
>    `Usage` maps the wire's `input_tokens`/`output_tokens` to `PromptTokens`/`CompletionTokens` with `TotalTokens` the sum (decision output tokens are free on every vendor shipping this protocol, but the counts are still reported and recorded).
> 2. New package `internal/llm/decision` (file `decision.go`), following `internal/llm/openai/openai.go`'s conventions (default `http.Client` with a generous timeout, `New(Config)` validating `BaseURL`, best-effort sink writes, elapsed measured from just before the request is sent to when the response body has been fully read):
>
>    ```go
>    // Config configures a Client.
>    type Config struct {
>        // BaseURL is the API root; /systemone is appended.
>        BaseURL string
>        // Model is the model id sent in the request body; empty omits
>        // the field and the server applies its default.
>        Model string
>        // APIKey is sent as a Bearer token; empty sends no header.
>        APIKey string
>        // HTTPClient overrides the transport; nil uses a shared default.
>        HTTPClient *http.Client
>        // Sink receives wire logs; nil disables them.
>        Sink logging.Sink
>    }
>
>    func New(cfg Config) (*Client, error)
>    func (c *Client) Decide(ctx context.Context, req llm.DecisionRequest) (*llm.DecisionResponse, error)
>    ```
>
>    Behavior: `POST {BaseURL}/systemone` via `url.JoinPath` with the trailing slash trimmed (same as the openai client's path join); body marshaled from the request with `State` and each question's `Instructions`/`Criteria` passed through as raw JSON; `Authorization: Bearer` only when the key is non-empty; a non-2xx response parsed as `{"type": ..., "message": ...}` when possible and returned as an error carrying status, type and message (a body that does not parse falls back to a status-line error); a 200 response parsed into `llm.DecisionResponse` with usage mapped and `Stats.Elapsed` measured (`Stats.Output` stays zero: a decision response is not text and has no meaningful byte split). Write `logging.KindLLMRequest` before the call and `logging.KindLLMResponse` after it, mirroring the openai client's records (URL, method, body, status).
>
>    This endpoint shape is the one TypeSafe's SDK uses and that OpenRouter (`/api/v1/systemone`), jevmodel.org (`/v1/systemone`), apimodels (`/v1/systemone`) and Venice (same handler at `/systemone`) all accept; a `base_url` ending in the version segment works for each.
>
> Tests in `internal/llm/decision/decision_test.go` (external test package, `t.Parallel()`, table-driven where sensible) against `httptest` servers: the request body carries the model field when set and omits it when empty, the state bytes verbatim, and the questions verbatim; the endpoint is the base URL plus `/systemone` with a trailing slash trimmed; the Bearer header is set when a key is configured and absent otherwise; answers of all three types parse with the right pointer fields set and absent fields nil; usage maps input/output/total; a 422 with a type and message produces an error naming both; a 500 with a non-JSON body produces a status error; the sink receives a request record and a response record; `New` rejects an empty `BaseURL`; a cancelled context returns an error.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 4: tools - deciderTool, SubagentDecision event, registry wiring

> Create `internal/tools/decider.go` implementing the decider tool type, and wire it into the registry in `internal/tools/tools.go`.
>
> 1. New exported types in `internal/tools/agent.go`, next to the subagent family:
>
>    ```go
>    // DeciderResult is the outcome of a decider run, mirroring
>    // ToolResult: Err marks a decider-level failure (for example the
>    // server rejected the request), which is still a valid tool result
>    // for the parent model.
>    type DeciderResult struct {
>        Output string
>        Err bool
>        // Usage holds the decision call's own usage, one record; empty
>        // when the call failed before reaching the server.
>        Usage []SubagentUsageRecord
>    }
>
>    // DeciderRunner evaluates a named decider from the same config
>    // against a state: one decision-model call with the decider's
>    // configured questions, returning the answers as JSON. The real
>    // implementation lives in the engine package; tests use fakes.
>    type DeciderRunner interface {
>        RunDecider(ctx context.Context, deciderName string, state json.RawMessage) (DeciderResult, error)
>    }
>    ```
>
>    Add one constant to the existing `SubagentEventKind` set in the same file:
>
>    ```go
>    // SubagentDecision carries a decider's answers, riding the same
>    // display and accounting channel as subagent activity: Output holds
>    // the answers JSON, Agent the decider's name, and Depth the nesting
>    // level below the calling agent (1 for a directly invoked decider).
>    SubagentDecision SubagentEventKind = "decision"
>    ```
>
>    The `SubagentEvent` struct needs no change: `Agent`, `Depth` and `Output` already carry what the decision event holds.
> 2. The default args schema, used when the entry declares none, mirrors the subagent tool's single-string convention - the tool call supplies the state as plain text:
>
>    ```go
>    const defaultDeciderArgsSchema = `{"type":"object","properties":{"state":{"type":"string","description":"The situation to evaluate, described in plain text."}},"required":["state"],"additionalProperties":false}`
>    ```
>
>    `deciderTool` holds `toolName`, `description`, `deciderName`, `argsSchema json.RawMessage` (empty means the default schema), `runner DeciderRunner`, and `events func(SubagentEvent) error` (may be nil). `newDeciderTool(e config.ToolEntry, runner DeciderRunner, events func(SubagentEvent) error) (tool, error)` revalidates per the `NewRegistry` convention: non-empty `decider` matching `NamePattern`, and a nil runner errors `tool %q: decider tools require a decider runner (pass tools.WithDeciderRunner)`.
> 3. `definition()` returns `llm.Tool` with the entry's schema or the default. The tool description should tell the parent model what the tool decides; the config author writes it.
> 4. `run(ctx, args, sink)`:
>    - Build the state. With a custom `args_schema`: the raw JSON `args` verbatim (the model's arguments are the state). With the default schema: unmarshal into `struct { State string \`json:"state"\` }`; a decode failure or empty state is a model-facing failure - `ToolResult{Err: true, Output: "tool %q: arguments must include a non-empty \"state\" string"}` with a `writeResultRecord`, no Go error.
>    - Call `runner.RunDecider(ctx, deciderName, state)`. A returned Go error is infrastructure failure: `writeResultRecord(sink, name, "error: "+err.Error())` and return it wrapped `tool %q: %w`.
>    - Otherwise emit the usage: for each record in `res.Usage`, if `events` is set, emit `SubagentEvent{Kind: SubagentUsage, Agent: rec.Agent, Depth: 1, Model: rec.Model, Usage: rec.Usage, Stats: rec.Stats}`. Depth 1 is the same level a directly invoked subagent reports: the decider call sits one level below the calling agent, and any subagent tool between the session and this tool adds its own increment as the event bubbles out.
>    - Emit the decision itself: if `events` is set, emit `SubagentEvent{Kind: SubagentDecision, Agent: t.deciderName, Depth: 1, Output: res.Output}` after the usage event. It arrives before the parent's own `>>> Result:` block, so the user watches the decision land the same way they watch a subagent work. Failed decisions emit no decision event: the parent's `>>> Error:` block (which always shows its body) carries the failure.
>    - Return `ToolResult{Output: trimSingleTrailingNewline(res.Output), Err: res.Err, Usage: res.Usage}`, writing the result record at each outcome point per the `command.go` conventions.
> 5. Registry changes in `internal/tools/tools.go`: a `deciderRunner DeciderRunner` field, a `WithDeciderRunner(r DeciderRunner)` option, and a dispatch `case config.ToolTypeDecider:` in `NewRegistry` passing `newDeciderTool(e, r.deciderRunner, r.subagentEvents)` - the decider's usage events ride the same `WithSubagentEvents` callback as subagent activity. The decider tool is not long-running: it stays under the registry's per-call timeout like command tools.
>
> Tests in a new `internal/tools/decider_test.go` (external test package, `t.Parallel()`, table-driven where sensible) with a fake `DeciderRunner` recording the decider name and state bytes: the definition carries the default schema when none is set and a custom schema verbatim when set; default-schema state extraction (including an empty state rejected as a model-facing failure); custom-schema args passed through as the exact raw JSON; result mapping (output trimming, `Err` propagation); the usage records land in `ToolResult.Usage` and as `SubagentUsage` events with Depth 1 when an events callback is set; a successful run emits one `SubagentDecision` event (Agent = the decider name, Depth 1, Output = the answers JSON) after the usage event, a failed run emits none, and a nil callback is tolerated for both; a Go error propagates as infrastructure failure; `NewRegistry` errors on a decider entry without a runner; the registry timeout applies (a fake runner blocking past a `WithTimeout` fails with a deadline error).
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 5: engine - the real DeciderRunner, wired into subagent and judge registries

> Create `internal/engine/decider.go` with the production runner, and make the subagent and judge runners wire it into the registries they build, so a subagent or judge granted a decider tool works too.
>
> 1. The runner:
>
>    ```go
>    // DeciderRunnerConfig configures a DeciderRunner.
>    type DeciderRunnerConfig struct {
>        // Config is the whole loaded config: deciders and their models
>        // are resolved from it.
>        Config config.Config
>        // NewDecisionClient builds the decision client for a decider's
>        // named model.
>        NewDecisionClient func(cfg config.Config, decider config.Decider) (llm.DeciderClient, error)
>    }
>    ```
>
>    `RunDecider(ctx, deciderName, state)`:
>    - Resolve the decider with `cfg.Config.Decider(deciderName)`; missing is a Go error (`decider %q is not defined in the config`) - it cannot happen for validated configs but guards programmatic ones.
>    - Resolve the decider's model; a model that is not a decision model is a Go error (the same guard validation already enforces).
>    - Build the client via `cfg.NewDecisionClient` (nil factory or factory error is a Go error, wrapped like the subagent runner's).
>    - Build `llm.DecisionRequest{Model: model.ModelName, State: state, Questions: ...}` converting the decider's `config.Question` map to `llm.DecisionQuestion` (a field-for-field copy; the JSON tags match) and call `Decide`.
>    - Map the outcome: `ctx.Err()` is a Go error (cancellation is infrastructure, mirroring the subagent rule); any other client error is a decider-level failure the parent model sees and can react to - `DeciderResult{Output: err.Error(), Err: true}` with no Go error. On success marshal `resp.Answers` to compact JSON and return `DeciderResult{Output: string(buf), Err: false, Usage: []SubagentUsageRecord{{Agent: deciderName, Model: resp.Model, Usage: resp.Usage, Stats: resp.Stats}}}`. Prefer the server's resolved model name from the response when non-empty, else the configured `model.ModelName`, so usage records name a real model when the config leaves `model_name` unset.
> 2. Nested wiring. Subagents and judges build their own registries, so they need the runner too:
>    - Add `DeciderRunner tools.DeciderRunner` to both `SubagentRunnerConfig` and `JudgeRunnerConfig` (they share a field layout, so `NewJudgeRunner`'s internal `SubagentRunnerConfig(cfg)` conversion carries it across unchanged), with a doc comment noting it is optional: a registry only errors when the nested agent is actually granted a decider tool and no runner is configured.
>    - In `SubagentRunner.RunSubagent` and `JudgeRunner.runJudge`, pass `tools.WithDeciderRunner(r.cfg.DeciderRunner)` when building the nested registry (a nil value is fine: `newDeciderTool` errors only if a decider entry exists).
>
> Tests in a new `internal/engine/decider_test.go` (external test package, `t.Parallel()`, reusing the existing fakes plus a fake `DeciderClient` recording requests and returning canned responses): the request carries the decider's questions verbatim (type, instructions, criteria raw JSON), the state bytes verbatim, and the configured model name; answers marshal to compact JSON keyed by question name; usage records carry the decider name and the resolved model; a client error yields `DeciderResult{Err: true}` and no Go error; a cancelled context yields a Go error; an undefined decider, a non-decision model, and a failing client factory each yield Go errors. Add a nesting test mirroring the subagent one: a parent agent granted a decider tool whose runner is wired into a subagent runner, asserting the decider's usage event reaches the top-level callback at the expected depth; and a test that a judge agent granted a decider tool can call it (following the existing judge test patterns).
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 6: chat, run and band wiring, decision display

> Wire the decider runner into all three frontends, add the real decision client factory to chat, and render the decision in the console.
>
> 1. In `internal/chat/chat.go`:
>    - Add `NewDecisionClientWithGetenv(cfg config.Config, decider config.Decider, getenv func(string) string, sink logging.Sink) (llm.DeciderClient, error)`: resolve `decider.Model` (undefined model is an error), guard that it is a decision model, resolve its provider and guard that it is openai-compatible, resolve the API key with the existing `resolveAPIKey`, and build `decision.New(decision.Config{BaseURL: provider.BaseURL, Model: model.ModelName, APIKey: apiKey, Sink: sink})`.
>    - Guard the agent path: `NewClientWithGetenv` errors when the agent's model is a decision model (`agent %q: model %q is a decision model`), so a misconfigured runtime fails clearly instead of building a chat client against a decision server.
>    - Add `Options.NewDecisionClient func(cfg config.Config, decider config.Decider) (llm.DeciderClient, error)` (tests only, mirroring `Options.NewClient`), a `deciderRunner(sink)` helper mirroring `subagentRunner`, and in `Run` pass `tools.WithDeciderRunner(...)` to the session registry, plus the `DeciderRunner` field on the subagent and judge runner configs so nested runs get the same runner.
>    - Display, in the `onSubagent` callback of `Events`: add a `case tools.SubagentDecision:` branch rendering `heading(label + ">>> Decision:")` followed by `fmt.Fprintln(out, indent(ev.Depth)+ev.Output)` - the same labeled, indented convention as the subagent's assistant and tool blocks. The decision body always prints in full: it is what the user asked to see, and it is small (one JSON object), so no `toolOutput` gating. It arrives before the parent's own `>>> Result: Tool:` block; if a partial line is open, the existing `heading` machinery terminates it like any other subagent heading.
> 2. In `internal/run/run.go`: add `Options.NewDecisionClient` (tests only), a `deciderRunner(sink)` method mirroring `subagentRunner`, pass `WithDeciderRunner` to the run's registry, and set the `DeciderRunner` field on both runner configs the run builds.
> 3. In `internal/run/ndjson.go`: add a `case tools.SubagentDecision:` branch to `onSubagent` emitting `ndjsonEvent{Type: "subagent_decision", Agent: ev.Agent, Depth: &ev.Depth, Output: ev.Output}`, mirroring the other `subagent_*` lines.
> 4. In `internal/band/room.go`: add `RoomOptions.NewDecisionClient` (tests only), build the runner in `NewRoom` through the same injected-or-real closure pattern as `clientFactory`, pass `WithDeciderRunner` to the room registry, and set the field on the subagent and judge runner configs. No display work: the room's per-turn printer is `chat.Events`, so the decision block renders through the shared callback.
>
> Tests per frontend, using injected fake decision clients: in `internal/chat/chat_test.go`, a config whose parent agent is granted a decider tool, the parent fake client calls it and then finishes - assert stdout shows, in order, `>>> Tool:` for the decider tool, the indented `[decider-name] >>> Decision:` block with the answers JSON, and `>>> Result: Tool:` whose body is the same JSON; with `--tool-output` off the decision block still prints in full (it is not a parent tool result, so suppression does not apply); and the turn's usage accounts the decision call attributed to the decider's name (following the existing subagent test structure). In `internal/run/run_test.go`, the same shape for the plain format (asserting the decision block on stderr via the diagnostic printer, mirroring where subagent activity goes, plus the final text on stdout), and for the ndjson format asserting the `subagent_decision` line with agent, depth and output between the `tool_call` and `tool_result` lines. In `internal/band`, a test following the existing room patterns exercising a decider tool through the room's registry with a fake decision client, asserting the decision block appears in the room's turn output, and that `NewRoom` rejects nothing when no decider tool is granted.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 7: the decide command

> Add a new `decide` subcommand: the decider counterpart of `run`. It takes one state argument and prints the decider's answers as JSON - directly invoking a decider, like `--agent` does for agents.
>
> 1. New package `internal/decide` with `decide.go`, mirroring `internal/run`'s structure (a `Run` function with an `Options` struct):
>
>    ```go
>    // Options configures one decision.
>    type Options struct {
>        Config     config.Config
>        Decider    config.Decider
>        Stdout     io.Writer
>        Stderr     io.Writer
>        ConfigPath string
>        // NewDecisionClient overrides decision client construction.
>        // Tests only; nil uses the real path.
>        NewDecisionClient func(cfg config.Config, decider config.Decider) (llm.DeciderClient, error)
>    }
>
>    // Run evaluates the named decider against state and writes the
>    // answers JSON to opts.Stdout.
>    func Run(ctx context.Context, opts Options, state string) error
>    ```
>
>    Behavior: resolve the sink with `chat.ResolveSink(opts.ConfigPath, opts.Config)` (wire logging lands in the same `.logs` files as every other mode); build the decision client through the injected factory or `chat.NewDecisionClientWithGetenv` with `os.Getenv`; build the runner with `engine.NewDeciderRunner(engine.DeciderRunnerConfig{Config: opts.Config, NewDecisionClient: ...})`; call `RunDecider` with the state as a JSON string (`json.RawMessage(strconv.Quote(state))`); write the answers to stdout followed by a newline. A `DeciderResult` with `Err` true writes the failure to stderr and returns the message as the error (a direct invocation has no parent model to react, so the failure is the user's to see); a Go error returns as-is. Print no usage footer: one call, and the JSON body is the output a script consumes (usage still lands in the wire logs).
> 2. In `main.go`: add `decideCommand()` to `rootCommand`'s `Commands`, and a `resolveDecider(cfg, name)` helper mirroring `resolveAgent` (no decider: `decider %q is not defined in the config (available: %s)` with the sorted decider names). The command takes `-c | --config`, a required `--decider <name>` flag (no defaulting: deciders are named things you invoke deliberately, and `default_agent` names an agent), and one positional `[state]` argument resolved with the same rules as `run`'s prompt (`run.ResolvePrompt`: literal string, `@@` escape for a leading `@`, `@file`, `-` for stdin) - a state and a prompt are both "one blob of text the command consumes". First generalize the prefix: `ResolvePrompt` and its helpers hard-code "run: " in their error messages, so change them to take the command name as a parameter (update the existing `prompt_test.go` assertions accordingly, keeping the "run: " strings for the run command), and pass "decide" from the new command. Exit codes follow `run`: 0 on a completed decision, 1 on any error, 130 on SIGINT via `signal.NotifyContext` and the `errors.Is(err, context.Canceled)` check.
>
> Tests: in `internal/decide/decide_test.go` (external test package, `t.Parallel()`), with a fake decision client - a happy path asserting stdout is the answers JSON (plus a trailing newline) and the request carried the state verbatim and the decider's questions; a decider-level failure (`Err` true) writes the failure to stderr and returns an error; a Go error propagates; an empty decision client factory errors. In `main_test.go`, following the existing CLI harness tests: `decide` with a valid config and injected fake resolves the decider and prints the JSON; `--decider` naming an undefined decider exits 1 with the error naming the available deciders; a missing state argument is a usage error carrying the "decide: " prefix; state via stdin (`-`) works; `SIGINT` maps to 130 following the run command's test conventions. Update the existing prompt resolution tests for the parameterized prefix.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 8: docs and example

> Document the new fields, command and display, and ship a worked example.
>
> 1. `docs/configuration.md`:
>    - Models section: add the `model_type` row (`llm` the default, `decision` for decision models), note that `model_name` is optional for decision models (the server default applies when omitted), that the llm-only knobs (`reasoning_effort`, `format`, `keep_alive`, `tool_choice`/`forced_tool`, `logprobs`/`top_logprobs`) are rejected on decision models, and that a decision model requires an `openai-compatible` provider (the client appends `/systemone` to its `base_url`, and sends the bearer key from `api_key_env`).
>    - Agents section: note that an agent's `model` must be an `llm` model.
>    - New "Deciders" section after Agents: what a decision model is (typed answers with probabilities instead of generated text; Jev as the example), the decider schema (`name`, `model`, `questions`), the three question types with their criteria rules, the identifier rule for question names, and the call semantics (every question evaluated in parallel against the same state in one request; vendor limits such as question counts and state size are the server's to enforce).
>    - Tools section: add the `decider` tool type - the `decider` field names a defined decider; the default args schema takes a single `state` string and a custom `args_schema` makes the raw arguments the state; the output is the answers as a JSON object keyed by question name, with each answer carrying its type, the selected value, and the probabilities and confidence the server reported; the run is one HTTP call under the per-tool timeout; a failed call comes back as a normal failed tool result the parent model can react to; and the chat display shows the decision as a labeled, indented block before the tool result. Note that decider calls are accounted in usage output as calls by the decider's name, and that they are not traced to Prefactor (the same limitation as nested subagent calls).
> 2. `docs/cli.md`: document `blorb decide` - the required `--decider` flag, the `[state]` argument sharing `run`'s prompt syntax (literal, `@@` escape, `@file`, `-` for stdin), stdout being the answers JSON, and the exit codes (0, 1, 130 matching `run`).
> 3. `README.md`: add a feature bullet for decision models and deciders, a short paragraph in the Configuration section pointing at the new docs, and a line in the Usage section showing a `blorb decide` invocation.
> 4. `docs/formats.md`: note in the ndjson section that `subagent_usage` lines also carry decision calls (with `agent` naming the decider) and document the `subagent_decision` event line (agent, depth, output carrying the answers JSON).
> 5. New `examples/decision/` with `blorb.json` and `README.md`: a provider pointing at a System One-compatible endpoint (`base_url` with the version segment, `api_key_env`), a decision model, one decider with a small question set (one choice and one noul, following the ticket-triage shape in the protocol docs), one agent granted the decider tool, and a README explaining the setup (any TypeSafe-shaped endpoint works: TypeSafe itself, OpenRouter's systemone surface, or another vendor), what the tool output looks like, and how to run the decider directly with `blorb decide`. Follow the existing example READMEs' structure. Following the existing convention, add a test loading and validating the shipped example - mirror `internal/prefactor/example_test.go`'s skip-if-absent pattern in a test file beside the example's consumer (an `internal/config` example test loading `examples/decision/blorb.json`, or extend the existing example test file if that is the established home), so `bin/qc` proves the shipped config is valid.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.
