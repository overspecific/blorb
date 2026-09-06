# Contributing to Blorb

Blorb is a single Go binary for making AI agents. Thanks for wanting to help — this file covers everything you need to get a development environment running and contribute changes.

## Development environment

Blorb uses [mise](https://mise.jdx.dev) to set up and manage the development environment, including the Go toolchain (pinned in `mise.toml`):

```sh
mise install
```

The `bin/` scripts add mise's shims to `PATH` if present, so you don't need to activate mise yourself to run them.

## Building and testing

- `bin/build` — build the `blorb` binary in the repo root
- `bin/qc` — format, style check, and run tests

Run `bin/qc` before handing anything over; make sure it is passing before continuing to the next step of your work.

## Conventions

- As few dependencies as possible.
- Everything has unit tests, and we aim for comprehensive test coverage. We build test-first (TDD) where we can.
- We are currently using Go 1.27.0 and we use the latest features where we can; we like generics.
- Errors are always checked and handled - the only exception being best-effort logging where it wouldn't make sense to stop operation due to an IO failure.
- We build high-quality, well-architected, correct code. We don't take shortcuts; if work needs to be done, we do it. After finishing a chunk of work, go the extra mile and do a review pass before handing it over.