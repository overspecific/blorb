# Prefactor integration

Blorb optionally records every trace to Prefactor (https://prefactor.ai), an agent-activity auditing platform. A new `prefactor` block in blorb.json enables it; the API token is the only required setting. When enabled, a chat session maps to a Prefactor agent instance: each user message, assistant message, LLM call, and tool call becomes a span, and the instance is finished (complete/failed/cancelled) when the session ends. We implement the small HTTP API surface directly in Go (no new dependencies) and register a proper activity schema (`blorb:user_message`, `blorb:assistant_message`, `blorb:llm`, `blorb:tool:<name>`) so traces are well-typed in the platform. Tracing is a hard dependency: when configured, a Prefactor failure fails the run rather than running untraced, and the platform's terminate signal is honoured cooperatively (the session winds down when asked to stop).

Wire facts (from docs.prefactor.ai, cross-checked against the official SDKs' payloads):

- Base URL `https://app.prefactorai.com/api/v1`, auth header `Authorization: Bearer <token>`.
- `POST /agent_instance/register` with `{agent_id?, environment_id?, agent_version, agent_schema_version, idempotency_key?}`; the new instance ID is `details.id` in the response.
- `POST /agent_instance/{id}/start` with `{timestamp?, idempotency_key?}`.
- `POST /agent_spans` with `{details: {agent_instance_id, schema_name, status, payload, result_payload?, parent_span_id?, started_at?, finished_at?}, idempotency_key?}`; new span ID is `details.id`. Response carries `control: {terminate, reason}` — terminate:true means someone on your team stopped the run from the Prefactor web app; blorb honours this cooperatively (see Commit 4). The finish endpoints carry the same control field.
- `POST /agent_spans/{id}/finish` with `{status, result_payload?, timestamp?, idempotency_key?}`; HTTP 409 + code `invalid_action` means already finished, treat as success.
- `POST /agent_instance/{id}/finish` with `{status, timestamp?, idempotency_key?}`.
- Activity schema: `agent_schema_version: {external_identifier, span_type_schemas: [{name, params_schema, result_schema?, template?, description?}]}` (or the `span_schemas`/`span_result_schemas` map form).

## Todo

- [x] Commit 1: prefactor config schema
- [x] Commit 2: prefactor HTTP client
- [x] Commit 3: activity schemas for blorb spans
- [ ] Commit 4: tracer — session as instance, turns/messages as spans
- [ ] Commit 5: wire tracer into chat
- [ ] Commit 6: docs and example

---

## Commit 1: prefactor config schema

Add an optional `prefactor` object to the blorb.json schema in internal/config/config.go:

```json
"prefactor": {
  "api_token_env": "PREFACTOR_API_TOKEN",
  "api_url": "https://app.prefactorai.com/api/v1",
  "agent_id": "...",
  "environment_id": "..."
}
```

- New type `PrefactorConfig` with fields: `APITokenEnv *string` (`api_token_env`, pointer following the `api_key_env` convention — absent defaults to `PREFACTOR_API_TOKEN`, explicit empty is invalid), `APIURL string` (`api_url`, optional, must be http/https with a host when set, default applied by a getter), `AgentID string` (`agent_id`, optional, may be empty for deployment-scoped tokens), `EnvironmentID string` (`environment_id`, optional). Add `Prefactor *PrefactorConfig `json:"prefactor,omitempty"`` to Config.
- Add const `DefaultPrefactorAPIURL = "https://app.prefactorai.com/api/v1"` and `DefaultPrefactorTokenEnv = "PREFACTOR_API_TOKEN"`.
- Add `PrefactorEnabled() bool` on Config (non-nil block = enabled).
- Add validate methods: `PrefactorConfig.validate` — api_url scheme/host when set, api_token_env non-empty when present.
- Tests in internal/config/config_test.go: absent block loads and validates; full block loads with all fields; unknown field in the prefactor block rejected (DisallowUnknownFields already covers it, add an explicit case); explicit empty api_token_env rejected; bad api_url rejected; defaults applied by getters (APITokenEnvOrDefault, APIURLOrDefault).

Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 2: prefactor HTTP client

Create package internal/prefactor with a minimal, dependency-free HTTP client for the five operations we need. Files: internal/prefactor/client.go, internal/prefactor/types.go, internal/prefactor/client_test.go.

- `types.go` — request/response types mirroring the wire shapes: `RegisterInstanceRequest{AgentID, EnvironmentID, AgentVersion, AgentSchemaVersion, IdempotencyKey}`, `AgentVersion{Name, ExternalIdentifier, Description, RuntimeEnvironment}`, `AgentSchemaVersion{ExternalIdentifier, SpanTypeSchemas []SpanTypeSchema}`, `SpanTypeSchema{Name, ParamsSchema, ResultSchema, Title, Description, Template}`, `RegisterInstanceResponse{Status, Details InstanceDetails}`, `CreateSpanRequest{Details SpanDetailsForCreate, IdempotencyKey}`, `SpanDetailsForCreate{AgentInstanceID, SchemaName, Status, Payload, ResultPayload, ParentSpanID, StartedAt, FinishedAt, SensitiveEncoding}`, `SpanResponse{Status, Control Control, Details SpanDetails}`, `Control{Terminate bool, Reason string}`, `FinishSpanRequest{Status, ResultPayload, Timestamp, IdempotencyKey}`, `StartInstanceRequest{Timestamp, IdempotencyKey}`, `FinishInstanceRequest{Status, Timestamp, IdempotencyKey}`. Time fields as time.Time with RFC3339 marshalling. Envelope error type `APIError{StatusCode int, Code, Message string}` implementing `error`.
- `client.go` — `Client` struct with `Config{BaseURL, Token, HTTPClient, Retries}`; constructor `New(cfg Config)`. Methods, all taking context and honouring cancellation:
  - `RegisterInstance(ctx, RegisterInstanceRequest) (RegisterInstanceResponse, error)`
  - `StartInstance(ctx, instanceID string, StartInstanceRequest) (InstanceDetails, error)`
  - `CreateSpan(ctx, CreateSpanRequest) (SpanResponse, error)`
  - `FinishSpan(ctx, spanID string, FinishSpanRequest) (SpanResponse, error)` — treat 409/`invalid_action` as success (return the zero-value response).
  - `FinishInstance(ctx, instanceID string, FinishInstanceRequest) (InstanceDetails, error)`
  - One shared `do(ctx, method, path, body, out) error`: builds the request, sets `Authorization: Bearer` and `Content-Type: application/json`, sends, decodes the `{"status": "success", ...}` envelope or converts non-200s to `*APIError`, retries on 429/503 (up to Retries, default 2, with linear backoff honouring the `Retry-After` header / `retry_after_ms` when present) and on transport errors.
- `client_test.go` — spin up `httptest.Server` per test: assert auth header, request path, JSON body shape for each method; decode responses into the typed structs and assert fields (instance ID from `details.id`, terminate flag from `control`); 429-then-success retry test; 503-then-success retry test; 409 invalid_action on FinishSpan returns success; non-200 error mapping to APIError fields; malformed JSON error.

Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 3: activity schemas for blorb spans

Define the activity schema we register so Prefactor types our spans. File: internal/prefactor/schema.go (+ schema_test.go).

- Span types (all with permissive `additionalProperties: true` so payloads never get rejected, following Prefactor's own default-schema pattern):
  - `blorb:agent_turn` — one user→assistant exchange (the parent span of everything in a turn). params: `{user_message}`; result: `{assistant_text, status}`. Templates: `"{{user_message}}"` / `"{{assistant_text}}"`.
  - `blorb:user_message` — params `{content}`.
  - `blorb:assistant_message` — params `{content, reasoning?}`; result `{content}`.
  - `blorb:llm` — one LLM API call. params `{model, messages}` (messages as an array of `{role, content}` objects, tool calls included); result `{content, reasoning, tool_calls, usage: {prompt_tokens, completion_tokens, total_tokens}, finish_reason, status}`.
  - `blorb:tool:<name>` — one per configured tool, generated from the tool registry: params schema derived from the tool's `args_schema` (falling back to an open object), result `{output, failed}`. Template `"{{output}}"`.
- `DefaultAgentSchemaVersion(cfg config.Config) AgentSchemaVersion`: builds the full schema version from the agent's name and configured tools. `external_identifier` a stable hash of the schema content (e.g. SHA-256 prefix) so unchanged configs resolve to the same registered schema version; name it `blorb-schema-<hash8>`.
- Schemas expressed as Go values marshalled from `map[string]any` / typed structs — no JSON string literals.
- Tests: golden-marshal the full schema version to JSON and compare against a testdata file; assert every configured tool produces a `blorb:tool:<name>` entry whose params schema matches the configured `args_schema`; assert the external identifier is stable across calls and differs when a tool's args_schema changes.

Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 4: tracer — session as instance, turns/messages as spans

Create the tracing layer that maps an agent run onto Prefactor's model. Files: internal/prefactor/tracer.go, internal/prefactor/tracer_test.go.

- `TracerConfig{Client *Client, AgentID string, EnvironmentID string, AgentName string, AgentVersion string, Now func() time.Time}` — span IDs are server-assigned, not client-generated: PFIDs are ULIDs carrying an account-specific partition (the API provides `POST /pfid/generate` for exactly this), so locally-invented IDs would fail validation. Every create call is synchronous, so the tracer always has the server-returned `details.id` before a span needs finishing — span handles store the ID from the create response.
- Error model: tracing is a hard dependency. Every tracer method returns an error and every error is propagated — a session does not continue untraced. Errors carry context (`fmt.Errorf("prefactor: start turn: %w", err)` style) so the chat layer can report what failed. There is no Logger and no disabled state.
- Termination: Prefactor's control signal (`control.terminate` on span create/finish responses) requests the run stop. The tracer surfaces this as a sentinel error, `ErrTerminated` (wrapping the platform's reason), returned from whichever tracer call observed it and subsequently from every tracer call until the session ends — the chat layer treats it like a termination request from outside (see Commit 5). `errors.Is` is the intended check.
- `Tracer` struct with the session lifecycle:
  - `StartSession(ctx) error` — registers the instance (with the schema version from Commit 3 and an agent version name from the blorb name + version), starts it, stores the instance ID. Returns an error if either call fails; a control.terminate in the responses is surfaced as ErrTerminated (unlikely at startup, but checked uniformly).
  - `StartTurn(ctx, userMessage string) (*Turn, error)` — creates the `blorb:agent_turn` span (active) and a `blorb:user_message` child span (complete immediately). Returns a `Turn` handle.
  - On `Turn`, methods called by the chat layer (next commit):
    - `LLMCall(ctx, req llm.Request) (*LLMSpan, error)` — starts an active `blorb:llm` child span.
    - `LLMSpan.Complete(resp llm.Response) error` / `LLMSpan.Fail(err) error` — finish the span with the response (usage, tool calls) or the error.
    - `ToolCall(ctx, name string, args string) (*ToolSpan, error)` — starts an active `blorb:tool:<name>` span (args is the raw JSON arguments string from the event).
    - `ToolSpan.Complete(output string, failed bool) error` / `ToolSpan.Fail(err) error`.
    - `AssistantMessage(ctx, content, reasoning string) error` — records a completed `blorb:assistant_message` span.
  - `Turn.Complete(finalText string) error` / `Turn.Fail(err) error` / `Turn.Cancel() error` — finish the turn span (`complete`, `failed`, or `cancelled`).
  - `FinishSession(ctx, status string) error` — finishes the instance (`complete`, `failed`, or `cancelled`).
- Spans are created active-then-finished (two calls) rather than single-shot so long operations (LLM calls, tools) show as in-progress in the Prefactor UI while they run.
- Concurrency-safe: turns are sequential in blorb, but a session outlives many turns; guard session state with a mutex.
- Tests with an httptest server: full session — assert register/start called; run a turn with one LLM call (complete) and one tool call (complete) and one failed tool call; assert the spans arrive with correct `schema_name`, `status`, and `parent_span_id` (all of user_message, llm, tool, and assistant_message spans parented to the turn span), and instance finish status. Error paths: register failure returns the error from StartSession; a span-create failure returns the error from the method that hit it; Fail paths set `failed` status with the error text in the result payload. Termination: a control.terminate response on span create makes the in-flight call return ErrTerminated (check with errors.Is, including the wrapped reason), and subsequent tracer calls keep returning ErrTerminated.

Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 5: wire tracer into chat

Connect the tracer to the engine via the seams the engine already exposes: the `llm.Client` interface (wrappable) and the event callback. No engine changes are needed — note `EngineConfig.Tools` is the concrete `*tools.Registry`, so a registry wrapper is impossible; tool spans are driven purely from events instead.

- internal/chat/tracewrap.go (+ tests in internal/chat/tracewrap_test.go):
  - `tracingClient` — wraps an `llm.Client` (and `llm.StreamingClient` when the inner one implements it): calls `turn.LLMCall` before `Chat`/`ChatStream`, completes/fails the span after, and returns any tracer error from the Chat call itself. For streaming, the span wraps the whole stream; the assembled response (the streaming client already returns the complete `llm.Response`) completes the span, deltas are not individually traced.
  - Assistant messages and tool calls: derive from engine events in the existing `onEvent` callback in chat.Run. When tracing is enabled, compose the printing callback with a tracing callback that maps:
    - `EventToolCall` → `turn.ToolCall` (start an active tool span; the engine emits this immediately before running the tool, in both streaming and whole-message modes);
    - `EventToolResult` → `ToolSpan.Complete`/`Fail` (the engine emits this immediately after the tool finishes; pair with the pending span by tool name — tools run sequentially within a turn);
    - `EventAssistantText` → `turn.AssistantMessage` (whole-message events only, not deltas).
    A tracer error inside the event callback aborts the turn via the existing onEvent-error path (the engine already propagates callback errors as turn failures), after which the turn is failed and the session ends.
- chat.go Options: add `Tracer *prefactor.Tracer` (nil = tracing off). In `Run`, when `Tracer != nil`:
  - `StartSession` before the REPL loop — an error fails startup ("chat: prefactor: ...") unless it is `ErrTerminated`, in which case the session exits immediately, printing the termination reason.
  - Wrap the client in `tracingClient`; compose the event callback per turn.
  - After `RunTurn`: `turn.Complete` on success, `turn.Cancel` when the turn context was cancelled by SIGINT, `turn.Fail` otherwise. If a tracer call returns `ErrTerminated`, print the termination reason and exit the session (the instance is already terminated server-side; do not call FinishSession — the platform marks terminated instances itself and a second finish would 409).
  - `FinishSession` on every other session exit path with the matching status: complete on graceful exit (exit/quit/EOF), cancelled on SIGINT exit, failed on any error exit (including startup errors after the session started and Prefactor errors that end the session). FinishSession errors are reported like other fatal errors.
  - The turn starts with the user's input line and ends when `RunTurn` returns, including multi-round turns (one turn span per user message, with the LLM/tool spans as children).
- Construction: in main.go's chat command, build the tracer when `cfg.Prefactor != nil` — resolve the token from `api_token_env` (erroring like provider keys when the env var is set but empty), construct the client with the resolved base URL, and pass it to chat.Options. Construction errors are exit-worthy like any other config problem.
- Tests: with a fake engine/client/registry (existing chat test fakes), drive a scripted session against an httptest Prefactor server — one turn with streaming on (assert one llm span, correct parent chain), one turn with a tool call, one interrupted turn (turn cancelled, session cancelled). Termination: a control.terminate mid-turn ends the session with the reason printed. Fatal errors: a Prefactor 500 during a turn ends the session with the error surfaced. Tracing disabled path unchanged. main_test.go: config with a prefactor block missing its env var exits with an error.

Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 6: docs and example

- README.md: add a "Prefactor tracing" section under the existing config documentation — the `prefactor` block reference table (all fields, defaults, which are required), the span model (what maps to what: session → instance, turn/message/llm/tool → spans), the environment variables needed (`PREFACTOR_API_TOKEN` or the configured `api_token_env`), a note that tracing is a hard dependency (a Prefactor failure fails the run rather than continuing untraced), and the terminate behaviour (stopping a run from the Prefactor web app ends the blorb session).
- examples/: check the existing example structure; if examples carry a blorb.json, add a commented-out `prefactor` block (JSON has no comments, so document it in the example's README or a sibling file) showing a minimal enabled configuration with only `api_token_env` set.
- Review pass: read the diff end to end — check every tracer error is handled or propagated (never silently dropped), check the terminate path end to end (control response → ErrTerminated → session exit), check that no span is recorded when tracing is disabled, and run the full `bin/qc`.

Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.