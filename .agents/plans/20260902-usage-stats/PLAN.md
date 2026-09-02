# Token usage stats

Surface token usage (`llm.Usage`: prompt/completion/total) in chat and run output. The plumbing already exists — every response carries `llm.Usage` (the OpenAI client parses it on plain responses and requests a final usage chunk when streaming, which it accumulates) — but the engine drops it on the floor: engine events and `tools.SubagentResult` carry no usage, so nothing reaches the display and nested subagent usage never escapes the nested engine. This plan propagates usage end to end:

- The engine gains an `EventUsage` event (one per LLM call, parent and nested alike through subagent events), attributed to the calling agent and rolling up into turn and session totals via a shared `internal/usage` accounting package.
- `tools.SubagentResult` gains a `Usage` slice so a subagent run's own calls are attributed in the parent's accounting.
- Chat grows a per-turn footer line after each turn and a session-totals line at exit; run prints the per-turn footer. The planned `plain` and `ndjson` run formats (a separate roadmap item) consume the same events later — stats on stderr for plain, stats as an event for ndjson.

Scope decisions (from the vision and user): chat-format output only for this plan (the per-format bullet's plain/ndjson behavior lands with the run-formats plan); cumulative chat totals are shown at exit only, no interactive live command; every LLM call is itemised in the account (one entry per call, attributed to the agent that made it) while the displayed footer is a compact per-turn roll-up — turn totals, split by agent, with subagents itemised.

## Todo

- [x] Commit 1: `internal/usage` accounting package
- [x] Commit 2: engine `EventUsage` event
- [x] Commit 3: subagent usage propagation (`SubagentEvent` + `SubagentResult`)
- [x] Commit 4: footer rendering helpers in `internal/usage`
- [x] Commit 5: chat per-turn footer and session totals
- [x] Commit 6: run per-turn footer
- [ ] Commit 7: docs

---

## Commit 1: `internal/usage` accounting package

> Create a new package `internal/usage` in `internal/usage/usage.go` that owns usage accounting: itemised per-call records, per-agent roll-ups, and turn/session totals. It is deliberately independent of the engine and display (later commits feed it and render from it), so it can be built and tested standalone.
>
> ```go
> // Package usage accounts for token usage across a turn or session:
> // each LLM call is itemised and attributed to the agent that made it,
> // and totals roll up per agent and overall.
> package usage
>
> import "github.com/overspecific/blorb/internal/llm"
>
> // Record describes one LLM call's token usage. Agent names the agent
> // whose engine made the call (the session agent or a subagent). Calls
> // are identified only by their position in the account; providers do
> // not report a stable call id.
> type Record struct {
> 	Agent string
> 	Model string
> 	Usage llm.Usage
> }
>
> // Account accumulates usage records for one turn or one session.
> // Methods are not safe for concurrent use; a session's turns run
> // sequentially.
> type Account struct {
> 	records []Record
> }
>
> // Add appends one call's record.
> func (a *Account) Add(rec Record)
>
> // Records returns a defensive copy of the accumulated records, in
> // call order.
> func (a *Account) Records() []Record
>
> // AgentTotals returns per-agent summed usage, sorted by agent name
> // for stable display.
> func (a *Account) AgentTotals() map[string]llm.Usage
>
> // Total returns the summed usage across all records.
> func (a *Account) Total() llm.Usage
> ```
>
> `AgentTotals` returning a map plus a documented sort order is awkward for callers that need deterministic iteration — instead make it return a slice:
>
> ```go
> // AgentTotal is one agent's summed usage.
> type AgentTotal struct {
> 	Agent string
> 	Usage llm.Usage
> }
>
> // AgentTotals returns per-agent summed usage sorted by agent name.
> func (a *Account) AgentTotals() []AgentTotal
> ```
>
> `Model` on `Record` is plumbed now (the engine knows the configured model per agent via its client construction, and the account is where per-model reporting will live), but this plan's display does not use it — that keeps the account complete from day one instead of growing the event kinds twice. The engine fills it in commit 2 from the agent's provider config where it is readily available, or leaves it empty where it is not; document that it may be empty when unknown.
>
> Note: a call with all-zero usage (some servers report nothing) is still recorded — it is a real call whose usage is unknown, and dropping it would under-count calls; the display layer decides how to render zeros.
>
> Files: `internal/usage/usage.go` (new), `internal/usage/usage_test.go` (new).
>
> Tests (`internal/usage/usage_test.go`, package `usage_test`, table-driven per repo conventions):
>
> - empty account: `Records` nil/empty, `AgentTotals` empty, `Total` zero
> - single record: `Records` returns it in order, `Total` matches its usage
> - multiple records across agents (e.g. "main" x2, "worker" x1): `AgentTotals` sums per agent and is sorted by name; `Total` sums across all
> - `Records` returns a copy: mutating the returned slice does not affect a second call's result
> - zero-usage record is preserved in `Records` and `AgentTotals` (sums to zero for that agent, agent still listed)
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 2: engine `EventUsage` event

> The engine observes every response's `llm.Usage` — including streamed responses, where `ChatStream` returns the assembled response with accumulated usage — but drops it. Emit it.
>
> In `internal/engine/engine.go`:
>
> - Add a new kind at the end of the `EventKind` const block:
>
> ```go
> 	// EventUsage carries the token usage of one completed LLM call,
> 	// emitted once per call after its response is fully received
>  // (whole-message or streamed). Usage is the provider-reported
>  // counts; a provider that reports none yields a zero Usage.
> 	EventUsage
> ```
>
> - Add a `Usage llm.Usage` field to the `Event` struct (zero for all other kinds; extend the struct doc comment to say so). Do not add the usage inline to existing kinds — a dedicated event keeps call-level attribution clean (one event per call, exactly once) and leaves delta kinds untouched.
> - Add an `AgentName string` field to `EngineConfig`: the display name of the agent this engine runs (the session agent or a subagent), used to attribute `EventUsage`. It is optional — empty means the caller does not care and the events simply carry an empty name.
> - In `call` (engine.go:200), after a successful response and before returning it (both the streamed and whole-message paths — i.e. at each successful return point of `call`), emit `onEvent(Event{Kind: EventUsage, Usage: resp.Usage})`. Emission must happen inside `call` so it fires exactly once per API call: `streamCall` returns the assembled response including accumulated streamed usage, so emitting in `call` after `streamCall` returns covers streaming with the same code path. An `onEvent` error aborts the turn like any other event error (return the error). Emission order: `EventUsage` is the first event for its call, before any thinking/text/delta events of that response — but it is emitted after the *previous* call's events, preserving strict call order. Note the streamed-path subtlety: `call` returns `streamed=true` after `streamCall` completes, so a usage event emitted at that point arrives after all of that call's deltas, not before them. Emit at the single successful-return point in `call` (after the `if e.cfg.Stream` block, where both paths converge — check the control flow: the streaming block returns directly, so emit before each return or restructure to a single return; prefer the minimal change: emit at both successful return points) and write a comment pinning the ordering: "EventUsage is emitted once per completed LLM call; for streamed calls it arrives after the call's deltas, since the provider reports usage in the stream's final chunk."
> - `Model` attribution for commit 1's `Record`: add `Model string` to `EngineConfig` too (the agent's configured provider model). `call` sets it on the `EventUsage` event via a new `Model string` field on `Event`. Where callers construct engines today — `internal/chat/chat.go` and `internal/engine/subagent.go` and `internal/run/run.go` — fill both new fields from the agent definition (`opts.Agent.Name` / `agent.Name`, `opts.Agent.Provider.Model` / `agent.Provider.Model`). This commit only plumbs; nothing displays yet.
> - Tracing interaction: `chat.TraceEvent` (internal/chat/tracewrap.go:158) switches on event kinds and ignores unknown kinds by design — verify `EventUsage` falls through the switch harmlessly (it does: the switch has no default error branch; add a brief comment there noting the usage kind is intentionally not traced).
>
> Tests (`internal/engine/engine_test.go`, extending the existing fake-client patterns):
>
> - a response with non-zero usage (extend the `textResp` helper or add `usageResp(content string, usage llm.Usage)`) emits exactly one `EventUsage` event with the response's usage, positioned after the call's text/thinking events (whole-message path)
> - a streamed response (existing `streamingClient` fake, which returns the canned response from `ChatStream`) with usage set emits exactly one `EventUsage`, after all of that call's delta events
> - a multi-call turn (tool call then final text, two responses with different usages) emits two `EventUsage` events in call order
> - an `onEvent` error on `EventUsage` aborts the turn with that error (the engine's standard callback contract)
> - a failing client call emits no `EventUsage`
> - chat and run (construction-site) tests still pass unchanged — the new `EngineConfig` fields are optional
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 3: subagent usage propagation

> Subagent usage must escape the nested engine. Two paths, both fixed in this commit: the event path (live accounting of nested calls as they happen, used by the display) and the result path (the `SubagentResult` returned to the parent, used by the engine's own subagent calls when the session callback is absent).
>
> `internal/tools/agent.go`:
>
> - Add to `SubagentEvent`:
>
> ```go
> 	// Usage is set on SubagentUsage events: the token usage of one
> 	// completed LLM call by the named agent.
> 	Usage llm.Usage
> ```
>
>   (`llm` is already imported in the tools package via tools.go's `llm.Tool`.)
> - Add a new kind and extend the kind doc:
>
> ```go
> 	SubagentUsage SubagentEventKind = "usage"
> ```
>
> - Add to `SubagentResult`:
>
> ```go
> 	// Usage itemises the subagent's own LLM calls, one record per
> 	// call in call order; a subagent whose provider reports no usage
> 	// has zero-usage records. Empty when the subagent run failed
> 	// before completing its first call.
> 	Usage []SubagentUsageRecord
>
> 	// SubagentUsageRecord is one LLM call's usage within a
> 	// subagent run.
> 	SubagentUsageRecord struct {
> 		Agent string
> 		Model string
> 		Usage llm.Usage
> 	}
> ```
>
>   (Top-level declarations in agent.go, not methods.) The parent side of the invocation — the parent's own prompt growth from carrying the tool call/response — is already accounted: the parent's *next* LLM call's `EventUsage` includes the grown prompt tokens, so no separate record is needed; add a comment on `SubagentResult.Usage` saying exactly that: "The parent's own usage from this exchange is accounted by its next LLM call's prompt tokens, which include the tool call and result messages."
>
> `internal/engine/subagent.go`:
>
> - In `convert` (subagent.go:108), map `EventUsage` to a `SubagentEvent` with `Kind: tools.SubagentUsage`, `Agent: agentName`, `Usage: ev.Usage` (plus whatever model field the engine event carries, from commit 2). The `default` error branch stays for genuinely unknown kinds.
> - In `RunSubagent` (subagent.go:45), collect the nested run's usage records: wrap the `onEvent` callback (or the `convert` output) to also append `SubagentUsageRecord{Agent: agentName, Model: ev.Model, Usage: ev.Usage}` per usage event into a slice; on the success return (subagent.go:91), set `SubagentResult.Usage` from that slice. On the two failure returns that represent infrastructure/cancellation (subagent.go:83, the ctx error path) return no usage — but on the subagent-level-failure return (subagent.go:89, `Output: err.Error(), Err: true`) still return the collected usage: those calls really happened and their tokens were really spent; the parent's accounting should not lose them.
>   - The `onEvent == nil` case: `convert` returns nil and the nested engine's callback is nil, meaning `RunTurn` gets a nil callback and emits nothing. That would silently lose usage for the result path. Fix: always pass a collecting callback to the nested engine (a non-nil `convert`-like function even when `onEvent` is nil), so `SubagentResult.Usage` is populated regardless of whether the caller wants events. Adjust `convert`'s nil handling accordingly: when `onEvent` is nil it still converts and the caller collects, it just does not forward.
>
> Tests:
>
> - `internal/engine/subagent_test.go`: a subagent whose fake client returns two responses (tool call then final) with distinct usages → the parent-facing `onEvent` receives `SubagentUsage` events in call order with `Agent` set to the subagent's name and `Depth` 0 from `RunSubagent` (the subagentTool's `forward` in tools/subagent.go:90 is what bumps Depth for session-level display — the direct `RunSubagent` test observes Depth 0); and `SubagentResult.Usage` (call `RunSubagent` directly, no onEvent) lists both calls' records in order — this covers the nil-callback collection fix
> - a subagent that exhausts `max_turns` (existing `loopResp` pattern, subagent_test.go:298) → the result still carries the usage records of the calls that ran, with `Err: true`
> - `internal/tools/subagent_test.go`: the subagentTool relays `SubagentUsage` events through its `forward` callback with Depth incremented (extend the existing relay test) and copies `SubagentResult.Usage` records into its own `ToolResult` path unchanged (the tool result output text is unaffected)
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 4: footer rendering helpers in `internal/usage`

> The display lives with each mode (chat/run), but the string formats are shared and unit-testable. Put the formatting in the accounting package, next to its data.
>
> New file `internal/usage/format.go`:
>
> ```go
> // FormatTurn renders a per-turn footer line: the turn's total usage
> // with per-agent split when the account holds more than one agent.
> // The shape is:
> //   "tokens: 123 prompt, 456 completion, 579 total"
> // and with multiple agents, one parenthesised item per agent:
> //   "tokens: 123 prompt, 456 completion, 579 total (main: 100/300/400, worker: 23/156/179)"
> // where each agent item is "name: prompt/completion/total".
> func FormatTurn(a *Account) string
>
> // FormatSession renders the session totals line, same shape as
> // FormatTurn but prefixed "session tokens:" (chat prints it at exit):
> //   "session tokens: 123 prompt, 456 completion, 579 total (main: ...)"
> func FormatSession(a *Account) string
> ```
>
> - A single-agent account renders without the parenthesised split (the common case: no subagents ran).
> - A zero-usage account (provider reported nothing) still renders — "tokens: 0 prompt, 0 completion, 0 total" — because the call count, not the token count, is what the user can rely on; add a doc note that zero means the provider did not report usage.
> - Both functions are deterministic: `AgentTotals` already sorts by agent name.
> - Do not pluralise "token(s)" per count; keep the fixed "tokens:" prefix and bare number words ("123 prompt, 456 completion, 579 total"), matching the concise style of chat's existing footers like "(interrupted)".
>
> Files: `internal/usage/format.go` (new), `internal/usage/format_test.go` (new).
>
> Tests (table-driven, `usage_test` package):
>
> - empty account → "tokens: 0 prompt, 0 completion, 0 total" (and the session variant's prefix)
> - single-agent account → no parenthesised split
> - two-agent account → split present, agents in name order, each "name: prompt/completion/total"
> - `FormatSession` prefix differs from `FormatTurn`
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 5: chat per-turn footer and session totals

> Wire accounting into the chat session: a footer after each turn, cumulative session totals at exit (the scope decision: exit only, no interactive command).
>
> `internal/chat/chat.go`:
>
> - In `Run` (chat.go:108), create `sessAccount := &usage.Account{}` before the REPL loop. Session-level `AgentName`/`Model` plumbing already went into the engine in commit 2.
> - In `runTurn` (chat.go:344): create `turnAccount := &usage.Account{}`; wrap the event callback passed to `eng.RunTurn` so that `EventUsage` events are recorded into `turnAccount` (with the event's agent/model fields) — the wrap composes with the existing `printEvent`/`TraceEvent` chain: `usageWrap(printOrTrace func(engine.Event) error)` forwards every event unchanged and additionally records on `EventUsage`. Subagent usage arrives through the *subagent event* path, not `printEvent` — wrap the pipe's turn callback (`onSubagent`, set via `pipe.set` at chat.go:362) the same way: forward every `tools.SubagentEvent` unchanged, record `SubagentUsage` events into `turnAccount` with the event's Agent/Model/Usage. Both wraps live in a new file `internal/chat/usage.go` with a doc comment explaining the two event paths (parent events from the engine, subagent events from the registry pipe) and that a usage event records the call that already completed.
> - After `eng.RunTurn` returns in `runTurn` (both the traced and untraced paths — record before the outcome switch, after `flush()`), print the footer when the turn made at least one LLM call (`len(turnAccount.Records()) > 0`): `fmt.Fprintf(opts.Stdout, "%s\n", usage.FormatTurn(turnAccount))`, then `sessAccount.Add` every record from `turnAccount`. A turn that errored before any call (e.g. interruption at turn start) prints no footer; a turn that errored after calls (mid-loop API failure) still prints one for the calls that completed — the tokens were spent. This means `runTurn` needs the session account: pass it in as a parameter (runTurn already takes opts; add `sessAccount *usage.Account` to its signature) rather than reaching for a closure over a local.
>   - Ordering with the interrupt path: the footer prints *before* `classifyTurnOutcome` prints "(interrupted)" / "error: ...". A partial turn shows its partial usage then the outcome line; keep that order and pin it in a test.
> - Session totals at exit: after the REPL loop and before the tracer finish (chat.go:316), print `usage.FormatSession(sessAccount)` when `len(sessAccount.Records()) > 0` — a session where nothing ran prints no line. This is best-effort output on the way out; print unconditionally of the session's success/failure classification (both exits spent tokens). Place it before the `FinishSession` call so it prints even when tracing finishes (and fails) afterwards.
> - Traced chat: `TraceEvent` (tracewrap.go:158) forwards unknown kinds by falling through its switch — `EventUsage` passes through untouched to `printEvent`, which ignores kinds it does not render (chat's `Events` switch). No tracing of usage spans; add a one-line comment in `TraceEvent`'s switch.
>
> Tests (`internal/chat/chat_test.go`, using the existing `fakeClient` whose canned responses gain usage values — extend `textResp`-style helpers with usage, as engine's tests did):
>
> - a two-turn session with distinct per-turn usages: each turn's output ends with its own footer line ("tokens: ..."), the footers differ, and the exit output contains a "session tokens: ..." line equal to the sum
> - a turn with a tool round (two LLM calls) prints one footer with the turn's summed usage — not one per call
> - a turn using a subagent (existing subagent test fixtures) prints a footer whose parenthesised split names the parent and the subagent with the right counts (subagent records attributed via the subagent event wrap)
> - an interrupted turn (existing SIGINT-during-turn fixtures) that completed calls first prints its partial footer, then "(interrupted)"
> - a session with no completed calls prints no footer and no session line
> - zero-usage responses (provider reports nothing) still print the "tokens: 0 prompt, ..." footer
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 6: run per-turn footer

> The single-turn run mode gets the same per-turn footer. Run has exactly one turn and exits, so the per-turn footer *is* the run's summary; no separate session line (chat.go:256-style banner lines and run's exit contract stay untouched — the footer is one more stdout line).
>
> `internal/run/run.go`:
>
> - In `Run` (run.go:53), create `account := &usage.Account{}` next to the `printEvent` setup (run.go:72). Wrap `printEvent` with the same recording wrapper as chat's — chat's `usageWrap` from commit 5 is chat's unexported helper; replicate the small wrapper in run (parallel structure, the same reasoning as `newClient`/`isTerminated`: run deliberately mirrors chat's helpers rather than sharing unexported internals). Wrap `onSubagent` identically for subagent usage events.
> - After `eng.RunTurn` returns and after `flush()` (run.go:135), print the footer when at least one record was collected: `fmt.Fprintf(opts.Stdout, "%s\n", usage.FormatTurn(account))`. On the error paths (interrupted run, provider failure mid-loop) the footer still prints for the calls that completed, before the run returns its error — matching chat's ordering decision (partial usage, then the outcome). The terminate path (platform stop before any call) has no records and prints no footer.
> - `Stderr` is not introduced in this plan (the `plain` format's split is the run-formats plan); the footer goes to `opts.Stdout` like all current run output.
>
> Tests (`internal/run/run_test.go`, extending the existing canned-response fakes with usage values):
>
> - happy path with usage: stdout ends with the footer line after the assistant text; the returned text is unaffected
> - tool-round run (two calls): one footer with the summed usage
> - subagent run (existing subagent fixture): footer split names parent and subagent
> - failing run mid-loop (existing error fixtures): partial footer printed, then the error returned
> - cancelled run before any call: no footer
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 7: docs

> Document token usage in the README.
>
> - `README.md`:
>   - Features list (around line 13): add a bullet for token usage stats (per-turn footers in chat and run, session totals at exit, subagent usage itemised by agent).
>   - Usage section, chat subsection (around line 70): note that each turn ends with a token footer and that exit prints the session total.
>   - Usage section, One-shot runs subsection (around line 89): note the token footer after the run's output.
>   - Where the subagent section describes nested activity display (find the section describing `ask_scholar`-style delegation output), add that subagent LLM calls are attributed to the subagent in the footer's per-agent split.
> - No new flags, no config changes, no exit-code changes: everything is always-on. Keep the docs consistent with that (there is no `--usage` flag to document).
> - `examples/simple/README.md`: no changes needed — the footer is always-on and the example's "Run" section describes commands, not output shapes; leave it.
>
> Files: `README.md`.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.