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