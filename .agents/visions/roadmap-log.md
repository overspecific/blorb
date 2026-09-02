# Roadmap - conversation log

## Conversation persistence - 2026-09-02

Topic: SQLite conversation database, resumable conversations, and resumable subagents. User wants all three added to the roadmap. Grounded in the code: history is an in-memory `[]llm.Message` in the engine (discarded at exit), and each subagent tool call spins up a fresh engine for one turn and discards it — both genuine gaps. Decisions from clarifying questions: one shared SQLite DB holds all conversations (chat, run, subagents) with subagent rows linked to their parent; resume is durable across process restarts; UX is CLI flags (chat gets `--resume` by conversation ID with probably a picker, run can resume an existing conversation for another turn); resumable subagents work by the subagent tool returning a conversation handle/ID to the parent, which can call it again to continue — requiring the runner interface and tool args schema to grow a handle notion. Noted that the system prompt lives outside history today, so stored conversations need config identity (agent name/model) for faithful resume. This is blorb's first storage dependency — pure-Go driver (no CGO) is the lean to keep the single binary; driver choice, DB location, run's resume flag shape, handle shape, and picker scope captured as open questions. New "Conversation persistence" section added to the vision doc.

## Conversation persistence - 2026-09-02

Topic: subagent resumability must not depend on SQLite. User clarified: resuming a subagent within a parent conversation doesn't require the database — in-memory handles within the process are enough for the base implementation, and even when SQLite is enabled, in-process subagent resume should use live state rather than the DB. The database only matters for restoring subagent conversations after a restart (a resumed parent picking up its subagent conversations). Vision doc updated to state this independence explicitly.

## Session start - 2026-09-02

Topic: creating the vision document. Created blank `roadmap.md` alongside this log, ready to start discussing what comes next in blorb.

## Token usage stats - 2026-09-02

Topic: itemised subagent accounting. User refined the subagent point: a subagent invocation has two usage sides — the subagent's own LLM calls, and the tool call/response exchange on the parent agent (which grows the parent's prompt on the next round); both cost tokens and both should show. Ideally every agent invocation is itemised: each LLM call (parent or nested) listed with its own counts, attributed to the agent that made it, rolled up into turn and session totals. Vision doc updated.

## Token usage stats - 2026-09-02

Topic: token stats in chat and run output. User wants token stats surfaced in both chat and run, all formats. Verified the plumbing mostly exists: `llm.Usage` is parsed on every response including streamed (the OpenAI client requests a final usage chunk), but the engine drops it. Decisions: both per-turn footers and cumulative session totals (chat shows both); subagent usage must be accounted properly — attributed to the subagent and rolled up, needing engine work since events and `tools.SubagentResult` don't carry usage today; per format — chat gets a human footer, `plain` puts stats on stderr, ndjson carries stats as an event with the final `done` event carrying totals. New "Token usage stats" section added to the vision doc.

## `run` output formats - 2026-09-02

Topic: streaming option across formats. User decided every output format should have a streaming option, not just ndjson — controlled by the existing `--no-stream` flag: streamed means delta events as they arrive (progress shown live), `--no-stream` means whole-message events only, matching the choice chat already makes. Per-format details of `--no-stream` behavior left as an open question. Vision doc updated.

## `run` output formats - 2026-09-02

Topic: ndjson event stream shape. User asked how the native Ollama API streams; researched it. Ollama streams NDJSON with one small JSON object per line — token-sized text fragments as `message.content` chunks, `thinking` as a separate field, streamed tool calls accumulated client-side, and a `done` marker (with stats) on the final line. User agreed blorb's ndjson format should follow this pattern. Noted in the vision doc.

## `run` output formats - 2026-09-02

Topic: ndjson streaming feasibility. Confirmed by reading the engine that ndjson can stream for real — `engine.RunTurn` emits all events (including streamed deltas) through an in-turn callback, so the formatter writes one JSON line per event as it arrives; no plumbing work needed. Noted in the vision doc.

## `run` output formats - 2026-09-02

Topic: ndjson format. Settled that the ndjson stream shows everything — including tool call and tool result events — not just assistant messages; one JSON object per line, streaming as it happens. Exact event schema still TBD.

## `run` output formats - 2026-09-02

Topic: `run` output formats. Renamed the third format from `json` to `ndjson` for clarity.

## `run` output formats - 2026-09-02

Topic: `run` output formats. User's first priority for the roadmap. Format details settled: a `--format` flag with `chat` (the current chat-style output) remaining the default; `plain` puts just the agent's output on stdout with everything else (streaming, tool activity, headings) on stderr — the only difference from chat is the stdout/stderr split. The agent output is the concatenation of all the turn's assistant messages with no added padding or newline, since agents sometimes call tools mid-sentence and the prose should splice back together exactly. `json` (NDJSON more or less) is the next format to come after `plain`; details TBD. HTTP serving mode deferred behind this.

## `run` output formats - 2026-09-02

Topic: `run` output formats. User's first priority for the roadmap. The obvious first format: agent output only — the agent's final output goes to stdout, everything else (streaming, tool activity, headings) goes to stderr. Rationale: streaming/progress goes to stderr so stdout stays clean for piping into other shell tools. The HTTP serving mode is still planned but deferred behind this in priority.

## Skeleton agreed - 2026-09-02

Topic: document structure. The user confirmed the skeleton: new run modes (HTTP serving, `run` output formats), ecosystem (more builtins such as web fetch, more provider types), depth on existing features (nested subagent Prefactor tracing), and an open area for undecided candidates (concurrency/sessions, context management, config tooling). Skeleton written to `roadmap.md` with open questions captured; depths still to be filled in through discussion.