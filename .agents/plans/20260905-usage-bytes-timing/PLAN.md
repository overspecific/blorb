# Usage stats: output bytes and timings

Extend the usage accounting built in the 20260902-usage-stats plan with per-call output bytes and elapsed time, so the existing footers and ndjson events can report throughput (tokens/sec, bytes/sec) alongside token counts. The plumbing pattern is identical to the token plan: the data is already in hand — clients assemble the complete response, and the calls are already on the clock — but drop it. This plan adds an `llm.CallStats` type (output bytes — broken down into content, reasoning, and tool calls — and elapsed duration) to the provider-neutral vocabulary, carries it on `llm.Response` so it flows through the existing `EventUsage` / `SubagentUsage` event paths unchanged, extends `usage.Record` and `Account` to accumulate it, and extends the footers and ndjson events to display and expose it, including derived tokens/sec and bytes/sec.

Scope decisions:

- Stats are captured by the provider clients (`internal/llm/openai`, `internal/llm/ollama`) — the only network touchpoints — and ride the existing response/event/account plumbing. No new event kinds; `EventUsage` and `SubagentUsage` are per-call and already flow everywhere tokens do.
- Bytes are model output bytes, broken down by what produced them: content, reasoning, and tool calls (each tool call's function name plus its JSON-encoded arguments). This is a generation lens, not a network one: wire and framing overhead (SSE `data:` prefixes, JSON structure, server-generated ids) and the request payload (dominated by resent conversation history) are deliberately not measured. The breakdown is carried, not just the total, so displays can attribute output to text, thinking, or tool use without re-measuring.
- Timing is end-to-end call duration: from just before the request is sent (in `post`) to when the response body has been fully received (when `Chat` has read the body / when `ChatStream`'s `readStream` completes). It includes server thinking time and all network transfer — the number a user perceives as "how long did the model take" — but excludes local request building and response parsing.
- Thinking/reasoning is included in every measure: providers count reasoning tokens inside `completion_tokens` (OpenAI's `completion_tokens_details.reasoning_tokens` is a subset; Ollama's `eval_count` includes thinking), reasoning is model output and so counts in output bytes (as its own breakdown component), and `Elapsed` spans thinking time. Consequence: for reasoning models, tokens/sec is diluted by think time — the rate is "tokens delivered per wall-clock second", which is the honest end-to-end number. Splitting think-time from generation-time (e.g. time-to-first-delta) is deliberately out of scope for this plan; a later plan can add it off the same `CallStats` seam.
- Derived rates (tokens/sec, bytes/sec) are computed at display/serialization time from summed totals; tokens/sec uses completion tokens and bytes/sec uses total output bytes — both generation throughput. Both are omitted (or zero) when the summed duration is zero (a fake client with no timing).
- `llm.Usage` (tokens) is unchanged; `CallStats` is a sibling so the token JSON shape and all existing tests stay intact.
- The prefactor `blorb:llm` span payload gains the new fields (its `Complete` takes the whole `llm.Response`, so the plumbing is a one-place payload extension).
- No new flags or config: always-on, like the token footers.

## Todo

- [x] Commit 1: `llm.CallStats` and `llm.OutputBytes` types
- [x] Commit 2: OpenAI client captures stats
- [x] Commit 3: Ollama client captures stats
- [x] Commit 4: engine and subagent propagation
- [x] Commit 5: `internal/usage` accumulates stats
- [x] Commit 6: footer rendering with rates
- [x] Commit 7: ndjson events carry stats
- [x] Commit 8: prefactor span payload
- [x] Commit 9: docs

---

## Commit 1: `llm.CallStats` and `llm.OutputBytes` types

> Add the provider-neutral call-stats vocabulary to `internal/llm`, and carry it on `llm.Response`. Nothing populates it yet; this commit defines the shape everything else builds on — including the output-bytes helper both provider clients will call, so its counting rules are defined and pinned once, not per client.
>
> In `internal/llm/llm.go`:
>
> - Add below `Usage`:
>
> ```go
> // OutputBytes holds the byte sizes of the model's output, split by
> // what produced it: assistant text, extracted reasoning, and tool
> // calls. Total is derivable via Total.
> type OutputBytes struct {
> 	Content   int `json:"content_bytes"`
> 	Reasoning int `json:"reasoning_bytes"`
> 	ToolCalls int `json:"tool_call_bytes"`
> }
>
> // Total returns the summed byte count across all components.
> func (o OutputBytes) Total() int {
> 	return o.Content + o.Reasoning + o.ToolCalls
> }
>
> // CallStats holds the observable stats of one LLM call, measured by
> // the provider client that made it. Output is the size of the
> // model's output, split by component (see Message.OutputBytes).
> // Elapsed is the end-to-end call duration: from just before the
> // request is sent to when the response body has been fully
> // received. All fields are zero when the client could not observe
> // them (e.g. a fake client in tests).
> type CallStats struct {
> 	Output  OutputBytes  `json:"output"`
> 	Elapsed time.Duration `json:"elapsed_ns"`
> }
> ```
>
>   (`time` is a new import for the package.) The JSON tag `elapsed_ns` keeps the serialized form unambiguous: the value marshals as Go's integer nanosecond count, and consumers unmarshalling into `time.Duration` recover the duration directly. The serialized shape is `stats:{"output":{"content_bytes":...,"reasoning_bytes":...,"tool_call_bytes":...},"elapsed_ns":...}`.
> - Add a method on `Message` (the counting definition lives with the type it measures):
>
> ```go
> // OutputBytes returns the size of the model's output in this
> // message, split into components: Content and Reasoning byte
> // lengths, plus each tool call's function name and JSON-encoded
> // arguments — the fields the model produced. Server-generated tool
> // call ids are excluded from ToolCalls: they are not the model's
> // output. Byte lengths are len() of the Go strings — UTF-8 bytes,
> // matching what the user perceives as output size.
> func (m Message) OutputBytes() OutputBytes
> ```
>
>   Implementation: `Content: len(m.Content)`, `Reasoning: len(m.Reasoning)`, `ToolCalls: sum of len(tc.FunctionName) + len(tc.FunctionArgs)` over `m.ToolCalls`.
> - Add to `Response`:
>
> ```go
> 	// Stats holds the stats of the call that produced this response,
> 	// as measured by the provider client. Zero for fake clients and
> 	// any client that does not measure.
> 	Stats CallStats `json:"stats"`
> ```
>
>   No `omitempty`: a struct-typed field never omits in encoding/json regardless, and the field is always meaningful, zero included.
>
> Tests: if `internal/llm` has no test file, create `internal/llm/llm_test.go` (package `llm_test`, `t.Parallel()`); otherwise extend it. Table-driven `OutputBytes` cases: empty message → all-zero `OutputBytes` and `Total() == 0`; content only → `Content` set, others zero; content + reasoning → both set; a message with two tool calls → `ToolCalls` is the sum of both calls' name+args lengths, with a non-empty `ID` on one call proving ids are excluded. Round-trip a `Response` with non-zero `CallStats` through `encoding/json` and assert the nested marshalled shape — `stats.output.content_bytes`, `stats.output.reasoning_bytes`, `stats.output.tool_call_bytes`, `stats.elapsed_ns` — and that unmarshalling recovers the values. Also assert a zero-`Stats` `Response` marshals with `"stats":{"output":{"content_bytes":0,"reasoning_bytes":0,"tool_call_bytes":0},"elapsed_ns":0}` (the always-present behaviour the tag choice pins).
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 2: OpenAI client captures stats

> The OpenAI-compatible client measures each call. Both `Chat` and `ChatStream` share `post`, which owns sending — so the clock starts there; the caller owns reading the response body, so the clock stops there. Output bytes come from the assembled response via commit 1's helper, so there is nothing to intercept on the read path.
>
> In `internal/llm/openai/openai.go`:
>
> - Change `post`'s signature to also return the clock's start: `func (c *Client) post(ctx context.Context, body wireRequest) (*http.Response, string, time.Time, error)`. In `post` (openai.go:526), measure `startedAt := time.Now()` right before `http.NewRequestWithContext` builds the request (after the payload is marshalled — local marshalling is not part of the call) and return it alongside the existing returns on every path (zero value on the marshal/join failures before the measurement starts).
> - In `Chat` (openai.go:106): after `io.ReadAll` succeeds (line 123), stop the clock — `elapsed := time.Since(startedAt)` — before parsing, which is local work, not part of the call. Attach at the response construction (line 158):
>
> ```go
> 	Stats: llm.CallStats{Output: resp.Message.OutputBytes(), Elapsed: elapsed},
> ```
>
>   (compute `Output` from the just-built `resp.Message`; inline the helper call in the literal so it reads the parsed message). The non-2xx error path (line 144) returns no response, so no stats escape — correct, matching the usage behaviour (failed calls report nothing).
> - In `ChatStream` (openai.go:362): the non-2xx path reads the body whole (line 382) — stats are lost with the error return; fine. Pass `startedAt` into `readStream`, which stops the clock when the stream completes: on the happy return — after the validation following `acc.response()` (openai.go:482-488) — set
>
> ```go
> 	resp.Stats = llm.CallStats{Output: resp.Message.OutputBytes(), Elapsed: time.Since(startedAt)}
> ```
>
>   before returning. Error paths return `nil, err` (the assembled partial response is only logged), so stats are dropped there, matching `Chat`. The `defer` in `ChatStream` drains the body after `readStream` returns; the clock has already stopped — harmless.
> - Wire-log records are unchanged: they already log full bodies.
>
> Tests (`internal/llm/openai/openai_test.go`, extending the existing `httptest.Server` fixtures):
>
> - `Chat` happy path: assert `resp.Stats.Output` equals `resp.Message.OutputBytes()` recomputed in the test from the fixture's expected content/reasoning/tool calls (pin the component numbers themselves, e.g. known content plus a tool call with known name and args → the summed lengths per component), and `resp.Stats.Elapsed > 0`.
> - `ChatStream` happy path (multi-chunk SSE fixture): the assembled response's stats carry the same recomputed `Output` breakdown (deltas concatenate to the same message, so streamed and whole-message paths must agree — this is the cross-path consistency pin) and `Elapsed > 0`.
> - Error paths (non-2xx, failed stream) are already covered by existing tests; no new stats assertions needed since no response escapes.
> - Keep timings off critical assertions: assert `Elapsed > 0`, never an exact value.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 3: Ollama client captures stats

> Mirror commit 2 for the Ollama client. The structure is the same (shared `post`, whole-body `Chat`, NDJSON-streaming `ChatStream`), so this is the same change in `internal/llm/ollama/client.go`, with no per-package helpers needed — commit 1's `Message.OutputBytes` is shared via `llm`.
>
> - `post` (client.go:466): same signature change (`(*http.Response, string, time.Time, error)`) and same capture point — `startedAt := time.Now()` right before the request is built and sent.
> - `Chat` (client.go:102): after `io.ReadAll` (line 117) succeeds, stop the clock (`elapsed := time.Since(startedAt)`) before decoding; after `neutralResponse` builds `resp` (line 152), set `resp.Stats = llm.CallStats{Output: resp.Message.OutputBytes(), Elapsed: elapsed}` before returning.
> - `ChatStream` (client.go:331): pass `startedAt` into `readStream` (client.go:383); on the happy return (client.go:459) set `resp.Stats = llm.CallStats{Output: resp.Message.OutputBytes(), Elapsed: time.Since(startedAt)}`. Error paths drop stats, as in commit 2.
> - `translate.go` is untouched (wire building is upstream of measurement).
>
> Tests (`internal/llm/ollama/client_test.go`, mirroring commit 2's): `Chat` happy path asserts the `Output` breakdown equals the recomputed expectation from the fixture's content/reasoning/tool calls and `Elapsed > 0`; `ChatStream` (multi-line NDJSON fixture) asserts the same on the assembled response; error paths need no new assertions.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 4: engine and subagent propagation

> `EventUsage` and `SubagentUsage` are one-per-call events that already carry usage everywhere; they gain the stats so accounting never touches another path. The engine reads stats straight off the response it already holds.
>
> In `internal/engine/engine.go`:
>
> - Add a `Stats llm.CallStats` field to `Event` (after `Model`), documented: "Stats is the stats of the completed call; zero for all kinds except EventUsage." Extend the `Event` struct doc comment's "set only for EventUsage" sentence to include Stats.
> - In `emitUsage` (engine.go:272): carry `resp.Stats` onto the event — `var stats llm.CallStats; if resp != nil { stats = resp.Stats }`, mirroring the usage handling.
>
> In `internal/tools/agent.go`:
>
> - Add `Stats llm.CallStats` to `SubagentEvent` (after `Model`), documented like the engine's field: set on `SubagentUsage` events.
> - Add `Stats llm.CallStats` to `SubagentUsageRecord` (after `Usage`), so the result path carries stats too.
>
> In `internal/engine/subagent.go`:
>
> - In `convert` (subagent.go:136): copy `Stats: ev.Stats` into the `tools.SubagentEvent` (the struct literal at line 138 already copies every field; add this one).
> - In `RunSubagent`'s `collect` (subagent.go:91): copy `Stats: ev.Stats` into each collected `tools.SubagentUsageRecord`.
>
> No behavioural change anywhere: stats ride the same paths as usage and render nowhere yet. Failed calls still emit no usage event, so their (partial) stats are dropped — consistent with token accounting.
>
> Tests:
>
> - `internal/engine/engine_test.go`: extend the existing `EventUsage` tests' fake responses (the `usageResp`-style helper) with non-zero `Stats` on one of them and assert the emitted `EventUsage` carries them exactly; the streamed variant asserts the same for a streamed response's stats. One new focused test is enough — the ordering/once-per-call semantics are already pinned by the existing usage tests and don't change.
> - `internal/engine/subagent_test.go`: in the existing usage-collection tests, give the fake responses stats and assert the `SubagentUsage` events and `SubagentResult.Usage` records carry them (the `want` slices at subagent_test.go:597 and :646 gain `Stats` fields).
> - `internal/tools/subagent_test.go`: the relay test's expected records (subagent_test.go:446) gain `Stats`.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 5: `internal/usage` accumulates stats

> The account grows the same numbers it grew for tokens, plus derived-rate helpers and a summed-stats accessor. Sums, not averages: rates are computed from summed bytes and summed duration, which is the honest aggregate (a mean of per-call rates would over-weight fast calls).
>
> In `internal/usage/usage.go`:
>
> - Add a `Stats llm.CallStats` field to `Record` (after `Usage`), documented: "Stats holds the call's stats; zero when the provider client did not measure."
> - Add a `Stats llm.CallStats` field to `AgentTotal` (after `Usage`).
> - `AgentTotals` (usage.go:52) and `Total` (usage.go:75): also sum the stats — `Output.Content`, `Output.Reasoning`, `Output.ToolCalls`, and `Elapsed` — component-wise per agent and across all records respectively. `Total`'s signature is unchanged (it still returns `llm.Usage`); add alongside it:
>
> ```go
> // TotalStats returns the summed call stats across all records.
> func (a *Account) TotalStats() llm.CallStats
> ```
>
> - Add derived-rate helpers in a new file `internal/usage/rates.go`:
>
> ```go
> // TokensPerSec returns completion tokens generated per second
> // across all records, derived from the summed elapsed time.
> // Completion tokens, not total: generation speed is the rate the
> // user cares about. Zero when no time was measured (the summed
> // elapsed is zero).
> func (a *Account) TokensPerSec() float64
>
> // BytesPerSec returns output bytes produced per second across all
> // records, using the summed total output bytes. Zero when no time
> // was measured.
> func (a *Account) BytesPerSec() float64
> ```
>
>   Both divide by the summed elapsed converted to seconds as a float, guarding the zero denominator.
> - The chat/run wraps construct `usage.Record` in four places (`internal/chat/usage.go:31`, `:47`; `internal/run/usage.go:17`, `:33`): add `Stats: ev.Stats` next to the existing `Usage: ev.Usage` in all four. (This is plumbing the account needs to ever see stats; it belongs with this commit since the account is inert without it. It touches chat and run but changes no behaviour — no test output changes.)
>
> Tests:
>
> - `internal/usage/usage_test.go`: extend the multi-record table cases to carry stats and assert `AgentTotals` and `Total` sum every component per agent and overall; zero-stats records preserved (sum to zero, don't drop); `TotalStats` sums across all records and is zero for an empty account.
> - New `internal/usage/rates_test.go` (package `usage_test`, table-driven): empty account → both rates zero; records with output bytes/tokens and elapsed → correct rates (use exact float equality where inputs divide cleanly, e.g. 100 tokens over 2s → 50); zero total elapsed with non-zero bytes/tokens → both rates zero (the guard); mixed records sum before dividing (100 tokens over 1s + 100 over 3s → 200/4s = 50/sec, not a mean of 100 and 33.3).
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 6: footer rendering with rates

> The footers grow a second line with output bytes (with the content/reasoning/tools split), time, and rates. Kept on its own line so the token line's exact shape (pinned by many tests) is untouched.
>
> In `internal/usage/format.go`:
>
> - `FormatTurn` becomes `"\n---\ntokens: " + formatTotals(a) + formatStats(a)` (and `FormatSession` likewise), where:
>
> ```go
> // formatStats renders the stats line: elapsed time, output bytes
> // with their content/reasoning/tools split, and the derived rates.
> // The line is empty — renders nothing — when no call measured any
> // output or time (the summed stats are all zero).
> func formatStats(a *Account) string
> ```
>
> - The rendered shape:
>
> ```
> (blank line)
> ---
> tokens: 123 prompt, 456 completion, 579 total (main: 100/300/400, worker: 23/156/179)
> stats: 4s, 9.2KB output (6KB text, 2KB reasoning, 1.2KB tools), 20.0 tok/s, 2.3KB/s
> ```
>
>   Rendering rules: the total output figure and elapsed always render when the line is shown (zeros included); the parenthesised split renders only its non-zero components, in text/reasoning/tools order, each labelled singularly ("text", "reasoning", "tools") — a plain text model does not carry "0B reasoning" noise, while a reasoning model shows where its bytes went. Elapsed uses Go's `Duration.String()` (`4s`, `12.345678s` — the idiomatic compact form; do not hand-roll a decimal trim). Bytes use a small `humanBytes(n int) string` helper (in `format.go`; it is rendering): `B` below 1024, then `KB`, `MB`, `GB`, dividing by 1024, one decimal place, trailing `.0` dropped (`512B`, `4KB`, `4.5KB`, `1MB`). The bytes/sec rate renders through `humanBytes` with the unit `/s` (`2.3KB/s`) — integer truncation in the rare sub-1B/s case is display-grade precision, fine; tokens/sec renders as `%.1f` with the unit `tok/s`.
> - The stats line is omitted entirely when the account's summed total output bytes and elapsed are both zero — a provider that measured nothing shows only the token line, exactly as today (this keeps every existing footer test passing for fake clients with zero stats, and the new tests pin the new line's shape).
> - Per-agent split on the stats line: no — the token line's split covers attribution; the stats line is session-wide throughput.
> - `FormatSession` gains the same stats line (same `formatStats`), so the session footer shows cumulative time/bytes/rates.
>
> Tests (`internal/usage/format_test.go`, new table cases):
>
> - zero-stats account (fake clients) renders exactly the old shape — no second line (regression pin)
> - account with stats renders the stats line with the exact expected string: records summing to 4s elapsed, 6144 content bytes, 2048 reasoning bytes, 1228 tool-call bytes (total 9420) with 80 completion tokens → `stats: 4s, 9.2KB output (6KB text, 2KB reasoning, 1.2KB tools), 20.0 tok/s, 2.3KB/s`
> - a stats line where reasoning and tools are zero omits the split: content-only records → `stats: 4s, 4KB output, ...` with no parenthesised part
> - a stats line where only one component is non-zero shows just it (e.g. reasoning-only → `(4KB reasoning)`)
> - the session variant carries the same line after its token line
> - `humanBytes` unit cases: 0 → "0B", 512 → "512B", 4096 → "4KB", 4608 → "4.5KB", 1048576 → "1MB", 1073741824 → "1GB"
>
> Chat and run integration: no code changes needed in `internal/chat` or `internal/run` — they already print `FormatTurn`/`FormatSession` verbatim, and the commit 5 wraps already feed stats into the accounts. Extend one chat footer test and one run footer test to give their fake responses non-zero stats and assert the stats line appears; leave the zero-stats fixtures asserting the old shape.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 7: ndjson events carry stats

> The machine-readable surface mirrors the human one: per-call events carry stats, and the terminal `done` event carries totals plus rates. No new "type" values are introduced — existing types gain fields, which consumers already tolerate by the ignore-unknown contract. The ndjson `stats` object is the `llm.CallStats` JSON shape from commit 1: `{"output":{"content_bytes":...,"reasoning_bytes":...,"tool_call_bytes":...},"elapsed_ns":...}` — the breakdown is machine-readable in full, and consumers can derive the total with the same sum the footer does.
>
> In `internal/run/ndjson.go`:
>
> - Add to `ndjsonEvent`:
>
> ```go
> 	// Stats is the stats of one call (usage, subagent_usage) and the
> 	// summed stats (done).
> 	Stats *llm.CallStats `json:"stats,omitempty"`
> ```
>
>   (`llm` is already imported.) Being a pointer, `omitempty` genuinely omits it when nil.
> - Add `Stats llm.CallStats `json:"stats"`` to `ndjsonAgentUsage` (non-pointer: per-agent entries always carry it, zero included — the split is explicit data, and a zero is meaningful: "measured nothing").
> - Add the derived-throughput object for `done`:
>
> ```go
> // ndjsonRates is the derived throughput on done, computed from the
> // summed stats. Zero fields mean no time was measured.
> type ndjsonRates struct {
> 	TokensPerSec float64 `json:"tokens_per_sec"`
> 	BytesPerSec  float64 `json:"bytes_per_sec"`
> }
> ```
>
>   and `Rates *ndjsonRates `json:"rates,omitempty"`` on `ndjsonEvent`, set on `done` only, omitted when the summed elapsed is zero (the same guard as the footer). Compute via the `Account` helpers from commit 5.
> - `printEvent`'s `EventUsage` case (ndjson.go:136): attach the call's stats unconditionally — the per-call `usage` event always carries them, zero included (zero is "client did not measure", which is information): `stats := ev.Stats; return s.emit(ndjsonEvent{Type: "usage", Agent: ev.AgentName, Model: ev.Model, Usage: &ev.Usage, Stats: &stats})`.
> - `onSubagent`'s `SubagentUsage` case (ndjson.go:163): same — `stats := ev.Stats` and `Stats: &stats`.
> - `finish` (ndjson.go:179): `done.Usage` unchanged; add `done.Stats` pointing at `s.account.TotalStats()` (commit 5), and `done.Rates` when the summed elapsed is non-zero; each `done.Agents` entry carries its per-agent summed stats via `AgentTotals`' new `Stats` field (extend the loop at ndjson.go:185).
> - Update the vocabulary doc comment at the top of the file: the `usage`, `subagent_usage`, and `done` entries gain `stats`, and `done` gains `rates`.
>
> Tests (`internal/run/run_test.go`, in the ndjson section, lines ~2066-2760):
>
> - a `usage` event with stats: the emitted line's `stats` object matches the fake response's full breakdown (`output.content_bytes` et al. and `elapsed_ns`)
> - a `subagent_usage` line likewise
> - the `done` line: `stats` equals the summed stats, `agents[].stats` equals per-agent sums, `rates.tokens_per_sec`/`rates.bytes_per_sec` correct for the fixture's numbers
> - a run whose fake client measured nothing: `usage` lines carry zero stats, `done` omits `rates`
> - add a test comment noting the forward-compatibility shape: existing types gained fields, no new types.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 8: prefactor span payload

> The `blorb:llm` span's result payload already serializes usage; it gains the call stats. `LLMSpan.Complete` takes the whole `llm.Response` (tracer.go:366), so the stats arrive with it — this is a payload extension only.
>
> In `internal/prefactor/tracer.go`:
>
> - In `llmResultPayload` (tracer.go:380), add a `"stats"` entry to the returned map, mirroring the nested `CallStats` JSON shape:
>
> ```go
> 	"stats": map[string]any{
> 		"output_bytes": map[string]any{
> 			"content":   resp.Stats.Output.Content,
> 			"reasoning": resp.Stats.Output.Reasoning,
> 			"tool_calls": resp.Stats.Output.ToolCalls,
> 			"total":      resp.Stats.Output.Total(),
> 		},
> 		"elapsed_ms": resp.Stats.Elapsed.Milliseconds(),
> 	}
> ```
>
>   The breakdown rides alongside its total so telemetry consumers see where bytes went without summing. Milliseconds, not nanoseconds, for `elapsed_ms`: the span payload is human-facing telemetry (its other fields are strings and maps), and `elapsed_ms` is the convention for span durations; sub-millisecond calls show 0, which is acceptable for telemetry — add that one-line comment on the map entry. The ndjson surface keeps nanosecond precision (`llm.CallStats`' JSON); the tracer payload is a lossy display.
> - No changes to span creation (`LLMCall`), the client, or `chat`'s tracewrap — they pass the response through untouched.
>
> Tests (`internal/prefactor/tracer_test.go`): extend `TestTracerLLMSpanPayloads` (tracer_test.go:370) — give the completed response non-zero `Stats` and assert the captured result payload's `stats` map carries the nested `output_bytes` breakdown (with the correct `total`) and `elapsed_ms` as the millisecond-rounded value; a zero-stats response carries zeros.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 9: docs

> Document the extended stats in the README, alongside the token-usage documentation added by the 20260902-usage-stats plan.
>
> - `README.md`:
>   - Features list: extend the token usage bullet to mention per-call stats (output bytes — the model's own content, reasoning, and tool calls — and duration) and derived throughput rates in the turn footers and ndjson events.
>   - Usage section, chat subsection: note the stats line under the token footer (elapsed, output with its content/reasoning/tools split, tok/s, KB/s) and that the session footer carries cumulative stats.
>   - One-shot runs subsection: note the footer's stats line; for the ndjson format, document the `stats` field on `usage`/`subagent_usage`/`done` (with its `output` breakdown) and the `rates` object on `done` (consumers can compute their own rates from the raw fields; `rates` is a convenience).
>   - If wire logs are documented anywhere, no changes: the logs already carry full bodies.
> - No config or flag changes to document: everything is always-on.
>
> Files: `README.md`.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.