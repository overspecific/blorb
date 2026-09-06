We're building Blorb, a single-binary tool for making AI agents. It lets a user define an agent via a `blorb.json` file and then run them in a variety of ways - in a simple chat interface, one turn at a time via the CLI, or via HTTP. It lets users experiment with different tool setups and system prompts. Tools can be setup via the configuration file, and can be local scripts, subagents and more.

We use mise to setup and manage the development environment.

It is built in Go, with as few dependencies as possible. Everything has unit tests, and we aim for comprehensive test coverage. We are currently using Go 1.27.0 and we use the latest features where we can; we like generics. Errors are always checked and handled - the only exception being best-effort logging where it wouldn't make sense to stop operation due to an IO failure.

We work in git, and we only commit or push when the user explicitly asks.

We check our work using `bin/qc` - this formats, checks style and runs tests. We make sure this is passing before continuing to the next step.

NOTE: the word is "overspecific" - make sure you get it correct in paths.

We build high-quality, well-architected, correct code. We don't take shortcuts; if work needs to be done, we do it. We build test-first (TDD) where we can. After finishing a chunk of work, we go the extra mile and conciously do a review pass before handing it over - code, plans, everything. If something needs fixing we fix it.

## Module layout

- `internal/config` — the `blorb.json` schema, loading, and validation
- `internal/engine` — the agent loop: model calls, tool execution, turn limits
- `internal/chat` — the interactive chat REPL
- `internal/run` — the one-shot run command
- `internal/tools` — tool registry and subprocess execution
- `internal/tools/builtin` — built-in tools (`read`, `grep`)
- `internal/llm` — provider-neutral LLM types
- `internal/llm/openai` — OpenAI-compatible client, with SSE streaming support
- `internal/llm/ollama` — native Ollama client (`/api/chat`), non-streaming and NDJSON streaming
- `internal/logging` — wire logging for LLM and tool interactions
- `internal/prefactor` — Prefactor tracing client and tracer
- `internal/usage` — usage stats (tokens, bytes, timing)

## Executing plans

Plans live in `.agents/plans/{yyyymmdd}-{plan_name}/PLAN.md` (see the planning skill for how they are written). When executing one:

- Work through the stages in order, one stage per commit, commit message matching the stage name.
- Stop after each commit and let the user review before starting the next stage.
- The exception to "only commit when the user explicitly asks" above: executing a requested plan commits per stage as part of the request.
- Each stage prompt says "do not commit" - that is addressed to the per-stage agent within the plan's own workflow, not to you; you are the one committing after verifying with `bin/qc`.
- Check off each stage in the plan's Todo list in the same commit as the stage's work. Do not make a separate commit that only checks off an item. If the stage itself excludes plan edits, wait: check it off in the next commit that touches the plan, or with the final check-off commit if no later stage edits the plan.

## Markdown files

When writing Markdown, we do not manually wrap.

## English

We use Plain English when talking to the user, writing comments or UI copy:

- We avoid metaphor, simile, analogies and cliches; stick to the facts
- We avoid jargon or corporate-speak
- We don't sell an idea; we just present it neutrally
- We don't use title case ever unless it is a name; we use sentence case
- We use no emojis, em dashes, smart quotes, or decorative Unicode symbols
- We use plain hyphens and straight quotes only
- Natural language characters (accented letters, CJK, etc.) are fine when the content requires them

We use the `good-english` skill when writing longer documents or chunks of user copy, not for short everyday messages.
