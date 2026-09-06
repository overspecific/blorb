# Split the README into user, reference, and development docs

The README has grown to ~530 lines and mixes four audiences: product pitch for users, a full configuration reference, a machine-parsing reference for `run --format ndjson/plain`, and development instructions including a module layout aimed at agents rather than humans. Split it into a lean user-facing README, a `docs/` reference set (`configuration.md`, `cli.md`), a `CONTRIBUTING.md`, and move agent-only material (module layout) into AGENTS.md. No prose is rewritten beyond what the split requires: the plan moves content, fixes the links the move breaks, and trims the README to what a user needs.

## Todo

- [x] Commit 1: Create CONTRIBUTING.md from the README development sections
- [x] Commit 2: Move the configuration reference to docs/configuration.md
- [ ] Commit 3: Move the `run` format reference to docs/cli.md
- [ ] Commit 4: Slim the README and move the module layout to AGENTS.md
- [ ] Commit 5: Review pass

---

## Commit 1: Create CONTRIBUTING.md from the README development sections

Create `CONTRIBUTING.md` at the repo root, aimed at human contributors (this file is the public face of contributing; the agent-specific rules in AGENTS.md are NOT duplicated into it). Give it:

- A short intro sentence ("Blorb is a single Go binary... contributions welcome" in the repo's voice).
- A **Development environment** section: install mise (`mise.toml` pins the Go toolchain, `mise install` to set it up); note that the `bin/` scripts add mise's shims to `PATH` so mise need not be activated manually.
- A **Building and testing** section: `bin/build` builds the `blorb` binary in the repo root; `bin/qc` formats, checks style, and runs tests — run it before handing anything over.
- A **Conventions** section carrying the human-relevant coding conventions from AGENTS.md, paraphrased for contributors: as few dependencies as possible; comprehensive unit-test coverage (TDD where possible); Go 1.27.0, using the latest language features (including generics); errors are always checked and handled — the only exception being best-effort logging where an IO failure should not stop operation. Keep the wording consistent with AGENTS.md's spirit; do not invent new conventions.

Do not move the module layout (that is commit 4's job, into AGENTS.md). The README keeps its Development section for now — it is removed in commit 4.

Do not modify any Go code; there are no tests to write. Check every relative link in CONTRIBUTING.md resolves (`mise.toml`, `bin/qc`, `bin/build` exist as named).

Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 2: Move the configuration reference to docs/configuration.md

Create `docs/configuration.md`, the full `blorb.json` reference, by moving — not rewriting — these sections from README.md:

- The whole `## Configuration` section: intro sentence, the full example config JSON, the walk-through paragraph after it, the ollama provider JSON example, and every subsection — `### Top-level fields`, `### Providers` (both tables), `### Models` (table plus the tool_choice and logprobs subsections), `### Agents`, `### Tools` (command, builtin, subagent — all bullets and JSON blocks), `### Logging`, `### Prefactor tracing`.
- The `### Listing installed models` section under Usage (the `blorb models` reference with the sample output block) — it is configuration-adjacent reference material; move it in as a top-level `## Listing installed models` section after the config content.

Give configuration.md a `# Configuration` H1, a one-line intro pointing back to the README ("The `blorb.json` file defines... — see the README for a getting-started overview"), and promote moved `###` subsections to `##` as needed for a clean hierarchy. Content is copied verbatim otherwise. Check the internal anchor links in the moved text still resolve within configuration.md (the moved text references `[Logging](#logging)`, `[Subagent tools](#tools)`, `[Models](#models)`-style anchors — adjust heading levels so they do, or fix the link targets).

In README.md, replace the moved `## Configuration` section with a short `## Configuration` stub: keep the full example config JSON and the walk-through paragraph (users need to see a config immediately), then point to `docs/configuration.md` for the complete field reference. Replace the moved `### Listing installed models` with a two-sentence `### Listing installed models` stub: what `blorb models` does (per provider, flags configured models missing from the server) plus a link to the full section in `docs/configuration.md`. Update the README Features bullet on wire logging and the Prefactor bullet to link to `docs/configuration.md#logging` / `#prefactor-tracing` instead of local anchors. Update `examples/prefactor-tracing/README.md` line 23 to point at `docs/configuration.md#prefactor-tracing` instead of `../../README.md#prefactor-tracing`.

Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 3: Move the `run` format reference to docs/cli.md

Create `docs/cli.md`, the reference for machine-facing output formats, by moving — not rewriting — the deep reference material out of README's `### One-shot runs` section. docs/cli.md gets:

- A `# CLI reference` H1 and a one-line intro ("The `blorb run` one-shot mode and its output formats, for scripting against Blorb").
- From the README: the `--format`/`--logprobs` flag paragraph, the **chat**/**plain**/**ndjson** format descriptions, the ndjson event-type table (all event types), the `stats` object paragraph, the `jq` example, the streaming/`--tool-output` paragraph, and the exit-codes sentence. Keep the short run-invocation examples (literal prompt, `@file`, stdin `-`, `@@` escape) in the README — they belong to the quick-start story — but move the "prompt argument is required and exactly one is accepted" semantics paragraph to docs/cli.md.

The README keeps a short `### One-shot runs` section: the invocation examples, one line per format (`plain` = agent text only on stdout for pipelines, `ndjson` = streaming JSON events for scripts), the exit-code sentence, and a link to `docs/cli.md` for the format and event reference. Move the **logprobs** reference material (the plain-format token-line example and the non-streaming caveat) to docs/cli.md, keeping in README only the flag mention; the long "logprobs" sub-bullet in the README Features list shrinks to name the feature. The `--logprobs` sentence in README's flags paragraph changes its `[Models](#models), **logprobs**` cross-reference to a link into `docs/configuration.md` (where the models table lives after commit 2).

Update the Usage section's usage-footer walk-through and the flags paragraph so they link to docs pages where they now reference moved detail. Check every internal link in both files resolves.

Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 4: Slim the README and move the module layout to AGENTS.md

Two halves:

**AGENTS.md**: append a `## Module layout` section carrying the bullet list from README's Development section verbatim (`internal/config` ... `internal/prefactor`, with each line's description). This is agent-facing material: agents navigating the tree need it, humans do not.

**README.md**: remove the `## Development` section entirely and replace it with a short `## Contributing` section: one sentence pointing to `CONTRIBUTING.md` for development setup and conventions. Do a final review of what remains in the README — it should now read as: note, title + image, one-paragraph pitch, Features, Getting started (now including the `blorb` install-from-source instructions `mise install` / `bin/build`, since a user without the repo checked out cannot use blorb otherwise — move the build steps from Getting started into a natural user flow and keep the dev-only pointers out), Usage, One-shot runs (stub), Configuration (stub), Examples, Contributing, License. Fix any stale cross-references left behind by the earlier moves. The README must not duplicate what CONTRIBUTING.md or the docs pages now carry.

Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 5: Review pass

Re-read all five documents end to end as a reviewer — README.md, CONTRIBUTING.md, docs/configuration.md, docs/cli.md, AGENTS.md — plus the three example READMEs, checking:

- No content was lost or accidentally duplicated between the moved files (grep for orphaned headings or double paragraphs; compare against `git show HEAD~4:README.md` for coverage).
- Every Markdown link in all five files resolves (relative paths exist; every `#anchor` matches a real heading in its file).
- No dangling references to sections that no longer exist where they are claimed to be.
- The split reads coherently: README for a user from zero to first successful chat turn; docs/ as reference; CONTRIBUTING.md for a would-be contributor; AGENTS.md for coding agents.
- `internal/prefactor/example_test.go`'s README-commented example still matches where the prefactor docs live (the comment references its README — make sure the example config content is still correct relative to docs/configuration.md).

Fix whatever the review finds in the same commit. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.