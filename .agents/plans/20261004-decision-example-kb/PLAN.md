# Decision example: knowledgebase-grounded triage

Rework `examples/decision` so the decision model does the routing that a generative model is bad at, using the biscuit knowledgebase from the `simple` example as the material. The example becomes a small two-agent system: a `triage` agent that greps the shared `kb` toolset for a ticket's biscuit and then calls one `triage_ticket` decider that answers several narrowing questions at once (which biscuit is at stake, which region owns it, whether the complaint is answerable from one region file, and the recommended action). It also gets a `search` subagent to fall back on, and demonstrates a custom `args_schema` so the decider's state is structured rather than a flat string.

The knowledgebase itself does not move. The decision config references `../simple/knowledgebase` by relative path, the same way `examples/prefactor-tracing` and `examples/ollama-cloud` already do, so the biscuit material stays in one place and the new example is a cross-reference rather than a copy. `internal/prefactor/example_test.go` already loads this config under `bin/qc`, so that test's expectations are updated in the same stage as the config change.

## Todo

- [x] Commit 1: config - the knowledgebase-grounded triage example and its test
- [ ] Commit 2: docs - the top-level README pointer

---

## Commit 1: config - the knowledgebase-grounded triage example and its test

> Replace the single-agent ticket-triage config with the knowledgebase-grounded two-agent system described here. All edits are to `examples/decision/blorb.json`, the existing `examples/decision/README.md`, and `internal/prefactor/example_test.go`; the knowledgebase stays where it is.
>
> The example keeps its existing provider and model blocks unchanged: a `local` openai-compatible provider at `http://localhost:13305/v1` with the `small` chat model (Gemma-4-E4B-it-GGUF), and a `typesafe` openai-compatible provider at `https://api.typesafe.ai/v1` with the `jev` decision model (`model_type: "decision"`). Keep `default_agent` as `"triage"`. Do not add or remove providers or models.
>
> Add one builtin toolset and two tools:
>
> - A toolset named `kb`, `"type": "builtin"`, `"builtin": "file"`, `"config": {"base_dir": "../simple/knowledgebase"}`, copying the block verbatim from `examples/ollama-cloud/blorb.json` (the relative `base_dir` resolves against the config file's directory, so the two `..` segments land on `examples/simple/knowledgebase`).
> - A `search` subagent tool, `"type": "subagent"`, `"agent": "search"`, with a description in the existing example voice: delegate the digging to an expert searcher with the `kb-read` and `kb-grep` tools who tries alternative patterns until something turns up; prefer it to grepping by hand when the thing might be spelled or phrased in more than one way. Copy the wording from `examples/simple/blorb.json`'s `search` tool if it fits (do not grant it to the `search` agent itself).
> - The existing `triage_ticket` decider tool, unchanged, referencing the `triage` decider.
>
> Add a second agent named `search`, `"model": "small"`, `"max_turns": 10`, `"tools": ["kb"]`, with a system prompt in the voice of `examples/simple`'s search agent: an expert searcher with `kb-read` and `kb-grep`, relentless, exploiting regex alternation, character classes, optional parts and case-insensitivity, trying the other number and synonyms on an empty pattern, reporting only the most relevant `path:line:text` matches and not paraphrasing them. It has no delegations of its own.
>
> Rewrite the `triage` agent: `"model": "small"`, `"max_turns": 10`, `"tools": ["kb-read", "kb-grep", "search", "triage_ticket"]` (grant `kb` and the single `kb-read` member to show both grant forms; the plain `kb` already supplies `kb-grep`). Its system prompt explains that it triages complaints about biscuits by consulting the knowledgebase first: it never decides blind. It greps the `kb` for the biscuit or the issue, reads the one region file the match points at when it needs the full entry, delegates to `search` when a first pattern comes up empty, and only then calls `triage_ticket`. It must pass the original complaint text plus the knowledgebase excerpt it found as the state, call `triage_ticket` as its final action of the turn, then answer from the decision: name the biscuit and its home region, say whether the outcome is contested, and give the recommended action, keeping the answer short.
>
> Replace the `triage` decider's questions with one set of narrowing questions evaluated in the same request (the decision model answers all of them at once against the same state):
>
> - `biscuit` - `choice` over a sample of the knowledgebase's biscuits across regions (Rich Tea, Digestive, Jammie Dodger, Ginger Nut, Hobnob, Tim Tam, Stroopwafel, Other), instructions asking which named biscuit the complaint is about; `Other` covers anything not listed.
> - `region` - `choice` over the knowledgebase's regions, one option per region file plus `unknown` (united_kingdom, france, italy, germany, netherlands, spain, scandinavia, north_america, australia_new_zealand, middle_east, india, unknown), instructions asking which region file owns that biscuit; `unknown` when the excerpt does not make it clear.
> - `answerable_from_one_file` - `noul` asking whether the excerpt (state) already holds enough to answer, so the agent can decide whether to delegate to `search`; use the `criteria` true/false labels.
> - `action` - `score` over the ordered dispositions (answer from the knowledgebase as-is, dig further with the search agent, refer to a human).
>
> Set the decider tool's `args_schema` to a custom object with `complaint` (string) and `excerpt` (string) properties, both required, so the raw JSON arguments become the state rather than the default single `state` field. This is the example's demonstration of a structured decider state.
>
> Update `examples/decision/README.md` to match: retitle it around the knowledgebase-grounded triage; explain that the decision model narrows the routing (which biscuit, which region, whether the excerpt answers it, what to do) while the agent does the retrieval, and that several questions are asked at once in one decision call; describe the two agents, the shared `../simple/knowledgebase`, and the custom `args_schema`; keep the Setup section's provider guidance (the chat model is local, the decision model is hosted) and the Logs section. Show a worked example of the tool call with a structured argument and the `[triage] >>> Decision:` block, and keep the existing `blorb decide` and `--state-json` invocation guidance, updating the example state to a structured ticket object.
>
> Update `internal/prefactor/example_test.go`'s `TestDecisionExampleConfigValid` to the new shape in the same commit, since `bin/qc` loads the shipped config and the test's old expectations would otherwise fail the gate: `DefaultAgent` is `triage`; the `triage` agent's resolved tools (via `cfg.AgentTools`) are `kb-read`, `kb-grep`, `search`, `triage_ticket` in that declared order; the `search` agent's tools are `["kb"]`; the `triage_ticket` tool is a `ToolTypeDecider` naming the `triage` decider and carries a custom `args_schema` requiring `complaint` and `excerpt`; the `jev` model resolves to `config.ModelTypeDecision`; the `triage` decider names `jev` and carries all four questions with the expected types (`biscuit` choice, `region` choice, `answerable_from_excerpt` noul, `action` score); and `cfg.PrefactorEnabled()` is false. Keep the existing `mustExampleAgent` helper and follow the assertion style of `TestSimpleExampleTracingDisabled` in the same file (compare `[]string` with `fmt.Sprint`).
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 2: docs - the top-level README pointer

> Expand the `examples/decision` entry in `README.md` (currently one line in the Examples section) to describe the new example: a decision model that narrows a ticket's biscuit, region, answerability and action in one call, with the agent doing knowledgebase retrieval through the shared `kb` toolset and the `search` subagent, and a structured decision state through a custom `args_schema`. Keep it to a sentence or two, in the register of the neighbouring example entries.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.
