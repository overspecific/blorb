# Contributing to Blorb

Blorb is a tool for making AI agents, built in Go as a single binary. Thanks for wanting to help - this file covers everything you need to get a development environment running and contribute changes.

## Development environment

Blorb uses [mise](https://mise.jdx.dev) to set up and manage the development environment, including the Go toolchain (pinned in `mise.toml`):

```sh
mise install
```

The `bin/` scripts add mise's shims to `PATH` if present, so you don't need to activate mise yourself to run them.

## Building and testing

- `bin/build` - build the `blorb` binary in the repo root
- `bin/qc` - format, style check, and run tests

Run `bin/qc` before handing anything over; make sure it is passing before continuing to the next step of your work.

## Finding your way around

Everything lives under `internal/`, one package per concern. A turn flows roughly: a front-end command drives the engine, the engine talks to the model through the neutral LLM layer, and executes any tool calls the model makes.

- `main.go` - CLI entry point: argument parsing and command dispatch (`chat`, `run`, `models`)
- `internal/config` - the `blorb.json` schema, loading, and validation
- `internal/engine` - the agent loop: model calls, tool execution, turn limits; subagents run through the same loop
- `internal/chat` / `internal/run` - the two front-ends driving the engine: the interactive chat REPL and the one-shot run command
- `internal/llm` - the provider-neutral LLM types; the engine speaks only these. `internal/llm/openai` (SSE streaming) and `internal/llm/ollama` (`/api/chat`, NDJSON streaming) implement the clients behind them
- `internal/tools` - tool registry and subprocess execution; `internal/tools/builtin` holds the built-in `read` and `grep` tools
- `internal/logging` - wire logging for LLM and tool interactions
- `internal/prefactor` - Prefactor tracing client and tracer
- `internal/usage` - usage stats (tokens, bytes, timing)

## Conventions

- As few dependencies as possible.
- Everything has unit tests, and we aim for comprehensive test coverage. We build test-first (TDD) where we can.
- We are currently using Go 1.27.0 and we use the latest features where we can; we like generics.
- Errors are always checked and handled - the only exception being best-effort logging where it wouldn't make sense to stop operation due to an IO failure.
