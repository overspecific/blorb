# Roadmap

_This document is in progress - it will be updated as we discuss. See the [conversation log](roadmap-log.md) for how it developed._

## New run modes

Chat and `run` exist today; the missing third mode is serving agents over HTTP.

### `run` output formats

First priority. `run` currently reuses the chat-style output; it should get proper output formats via a `--format` flag so it composes in shell pipelines. Every format should have a streaming option, controlled by the existing `--no-stream` flag:

- `--format chat` (default) — the current chat-style output, unchanged.
- `--format plain` — just the agent's output to stdout, everything else (streaming events, tool activity, headings) to stderr. The agent output is the concatenation of all the turn's assistant messages, with no padding or newline added between them — agents sometimes call tools halfway through a sentence, and their prose should be spliced back together exactly as it came.
- `--format ndjson` — next after `plain`: NDJSON, for programmatic consumption. One JSON object per line, streaming the full event stream as it happens — you see everything, including tool call and tool result events, not just assistant messages. This streams for real: the engine already emits events through an in-turn callback (including streamed deltas), so the formatter writes a line per event as it arrives. The event stream follows the Ollama native API pattern: delta-per-line chunks (token-sized text fragments, thinking as a separate field, streamed tool calls accumulated client-side), with a `done` marker on the final line. Exact event schema still to be worked out.

Streaming applies across all three: streamed means delta events as they arrive (ndjson delta-per-line; plain and chat showing progress live on stderr/stdout), while `--no-stream` means whole-message events only — the same choice chat already makes today. Per-format details of what `--no-stream` looks like still to be worked out.

### Token usage stats

Token stats should be surfaced in the output of both chat and `run` (all formats). The plumbing mostly exists: `llm.Usage` (prompt/completion/total) is parsed on every response — including streamed responses, where the OpenAI client already requests a final usage chunk and accumulates it — but it is not surfaced to the user today, and the engine drops it on the floor.

- **Per turn** — a stats footer after each agent turn (per-turn counts), and also **cumulative session totals** in chat, shown at exit (and live on request?).
- **Subagents count, itemised** — usage from nested LLM calls inside subagent runs must be accounted properly. There are two sides to a subagent invocation: the subagent's own LLM calls, and the tool call/response exchange on the parent agent (which grows the parent's prompt on the next round). Both cost tokens and both should show. Ideally every agent invocation is itemised and accounted for: each LLM call (parent or nested) listed with its own prompt/completion/total counts, attributed to the agent that made it, rolled up into turn totals and session totals. This needs engine work: the event stream and `tools.SubagentResult` don't carry usage today, so usage has to be propagated out of nested engines.
- **Per format** — chat shows a human footer line; `plain` puts stats on stderr so stdout stays clean; `ndjson` carries stats as an event (like Ollama's `done` line carrying eval counts); the final `done` event would carry the totals.

## Conversation persistence

Today a conversation lives entirely in the engine's in-memory `[]llm.Message` history and is discarded when the process exits; subagent conversations are even more ephemeral (a fresh engine per tool call, one turn, thrown away). Both become durable:

### SQLite conversation database

A single SQLite database holds every conversation — chat sessions, `run` turns, and subagent conversations — with IDs linking each subagent conversation to its parent. This is blorb's first storage layer and first storage dependency, which matters given the "as few dependencies as possible" rule: a pure-Go driver (no CGO) is the natural fit to keep the single-binary property; exact driver choice is an open question.

- **What's stored** — the full message history of each conversation (every role, tool calls and tool results included), enough to faithfully reconstruct an engine's state and continue the conversation. The system prompt lives outside the history today (prepended per request), so the conversation record needs to carry the config identity (agent name/model) alongside the messages for a faithful resume.
- **Durable across runs** — resume works across process restarts, not just within a session. The database is the source of truth once a turn is committed.
- **Subagents in the same DB** — a subagent conversation is stored as its own conversation row, related to the parent conversation, so nesting is queryable.

### Resumable conversations

- **chat** — a `--resume` flag: takes a conversation ID (and probably a picker listing past conversations when no ID is given); the engine is rebuilt from the stored history and the REPL continues where the last session left off.
- **run** — today it's strictly one turn with a discarded engine; with the database, `run` can resume an existing conversation and run another turn into it. The exact flag shape is an open question.

### Resumable subagents

Semi-related but not quite the same feature: today every subagent tool call starts a brand-new stateless conversation. Instead, the subagent tool returns a conversation handle/ID to the parent, and the parent can call the subagent again referencing that ID — the subagent continues where it left off, with its prior history reloaded. This needs work at the tool boundary: the runner interface and tool args schema have no notion of a conversation handle today, so both must grow one (e.g. an optional ID alongside the prompt).

Important: this is independent of SQLite. Subagent resumability within a parent conversation only needs the subagent's history held somewhere the parent's session can reach — in-memory handles within the process work fine and are the base implementation. It must not depend on the database being enabled: even when SQLite persistence is on, resuming a subagent in-process uses live state, and the DB only comes into play for restoring subagent conversations after a restart (a resumed parent picking its subagent conversations back up — which composes with resumable conversations, since subagent conversations land in the same database linked to their parent).

## Ecosystem

- More built-in tools (e.g. a web fetch builtin, with its own `config` shape — allowed hosts, timeouts)
- More provider types beyond the single `openai` type

## Depth on existing features

- Prefactor tracing for nested LLM calls inside subagents (currently only the parent agent's spans are recorded — a documented limitation)

## Open areas

Not yet decided: candidates floated include concurrency/sessions, context management, and config tooling. Nothing committed here yet.

## Open questions

- Priority/ordering across these areas is undecided.
- Which open-area candidates (if any) belong in the roadmap?
- What does HTTP serving actually look like — one endpoint per agent, streaming, auth?
- Which output formats does `run` need, and for what scripting use cases?
- Which SQLite driver — pure-Go (e.g. modernc.org/sqlite, keeps the no-CGO single binary) vs CGO-based (mattn/go-sqlite3)? Given the dependency rule, pure Go is the lean.
- Database location/management — default path, and is it per-project (alongside blorb.json) or per-user?
- What does the `run` resume flag look like, and does resuming `run` compose with the `--format` work?
- Exact shape of the subagent conversation handle — how the parent references it in tool args, and what happens when an ID references a missing/foreign conversation.
- Does the picker for `chat --resume` list conversations for the current agent only, or all?