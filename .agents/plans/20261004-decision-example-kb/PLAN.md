# Decision example: a decider router over the biscuit knowledgebase

Rework `examples/decision` so the decision model does the routing that a generative model is bad at, using the biscuit knowledgebase from the `simple` example as the material. The example is a small two-agent system: a `scholar` agent that answers biscuit questions, and a `route_question` decider it calls once per question to plan the route - which region file to read and whether it needs to read at all - answering several narrowing questions at once (which region, what kind of question, whether the excerpt already suffices, and whether to answer or retrieve). It also gets a `search` subagent to fall back on, and demonstrates a custom `args_schema` so the decider's state is structured rather than a flat string.

The knowledgebase itself does not move. The decision config references `../simple/knowledgebase` by relative path, the same way `examples/prefactor-tracing` and `examples/ollama-cloud` already do, so the biscuit material stays in one place and the new example is a cross-reference rather than a copy. `internal/prefactor/example_test.go` already loads this config under `bin/qc`, so that test's expectations are updated in the same stage as the config change.

## Todo

- [x] Commit 1: config - the knowledgebase-grounded triage example and its test
- [x] Commit 2: docs - the top-level README pointer
- [x] Commit 3: reframe the example as a biscuit scholar whose decider gates any question
- [x] Commit 4: make the decider's region answer the actual knowledgebase filename

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
> - `answerable_from_excerpt` - `noul` asking whether the excerpt (state) already holds enough to answer, so the agent can decide whether to delegate to `search`; use the `criteria` true/false labels.
> - `action` - `score` over the ordered dispositions (answer from the knowledgebase as-is, dig further with the search agent, refer to a human).
>
> Set the decider tool's `args_schema` to a custom object with `complaint` (string) and `excerpt` (string) properties, both required, so the raw JSON arguments become the state rather than the default single `state` field. This is the example's demonstration of a structured decider state.
>
> Update `examples/decision/README.md` to match: retitle it around the knowledgebase-grounded triage; explain that the decision model narrows the routing (which biscuit, which region, whether the excerpt answers it, what to do) while the agent does the retrieval, and that several questions are asked at once in one decision call; describe the two agents, the shared `../simple/knowledgebase`, and the custom `args_schema`; keep the Setup section's provider guidance (the chat model is local, the decision model is hosted) and the Logs section. Show a worked example of the tool call with a structured argument and the `[triage] >>> Decision:` block, and keep the existing `blorb decide` and `--state-json` invocation guidance, updating the example state to a structured ticket object.
>
> Update `internal/prefactor/example_test.go`'s `TestDecisionExampleConfigValid` to the new shape in the same commit, since `bin/qc` loads the shipped config and the test's old expectations would otherwise fail the gate: `DefaultAgent` is `triage`; the `triage` agent's resolved tools (via `cfg.AgentTools`) are `kb-read`, `kb-grep`, `search`, `triage_ticket` in that declared order; the `search` agent's tools are `["kb"]`; the `triage_ticket` tool is a `ToolTypeDecider` naming the `triage` decider and carries a custom `args_schema` requiring `complaint` and `excerpt`; the `jev` model resolves to `config.ModelTypeDecision`; the `triage` decider names `jev` and carries all four questions with the expected types (`biscuit` choice, `region` choice, `answerable_from_excerpt` noul, `action` score); and `cfg.PrefactorEnabled()` is false. Keep the existing `mustExampleAgent` helper and follow the assertion style of `TestSimpleExampleTracingDisabled` in the same file (compare `[]string` with `fmt.Sprint`).
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 3: reframe the example as a biscuit scholar whose decider gates any question

> The shipped example reads as a support desk for complaints, which is not what it was meant to be: it should be a biscuit scholar like `examples/simple`, using the decision model as the routing and confidence gate that runs on any biscuit question, not only on complaints. Reframe it. Edits are to `examples/decision/blorb.json`, `examples/decision/README.md`, `internal/prefactor/example_test.go` and the `examples/decision` entry in `README.md`.
>
> In `examples/decision/blorb.json`, rename the default agent from `triage` to `scholar`, and set `default_agent` to `scholar`. Keep the `search` agent as it is. The `scholar` agent's system prompt becomes a biscuit scholar (voice of `examples/simple`'s scholar) with one important rule: for any question about biscuits, it first calls the decider to plan, then answers. Specifically: call `route_question` with the user's question and any knowledgebase excerpt it already has; the decision picks the region file to read and says whether that excerpt is enough. When the decision says the excerpt is not enough, grep the chosen region file (or the whole knowledgebase) with `kb-grep`, reading with `kb-read` when it needs the full entry, and delegate to the `search` agent when a pattern comes up empty. Then answer from what it read, naming the file it drew from. Keep answers short. The scholar keeps `"model": "small"`, `"max_turns": 10`, `"tools": ["kb", "search", "route_question"]`.
>
> Rename the `triage` decider to `route_question` (the `decider` field and the `triage_ticket` tool's `decider` reference move with it). Rework the questions so they fit any biscuit question rather than a complaint:
>
> - `region` - `choice` over the knowledgebase's regions, one option per region file plus `unknown` (united_kingdom, france, italy, germany, netherlands, spain, scandinavia, north_america, australia_new_zealand, middle_east, india, unknown); instructions ask which region file is most likely to hold the answer; `unknown` when the question is not region-specific.
> - `question_kind` - `choice` over the kinds of question the knowledgebase answers (origin, ingredients, dunking, comparison, other), so the agent knows whether to read a region file or `dunking.md`.
> - `answerable_from_excerpt` - `noul`, unchanged in spirit: whether the excerpt in the state already holds enough to answer, so the agent can choose to answer from it or to retrieve.
> - `action` - `score` over the ordered dispositions (answer from the excerpt as-is, retrieve from the chosen region file, dig further with the search agent). Drop the "refer to a human" level, which belonged to the support framing.
>
> Rename the decider tool `triage_ticket` to `route_question`. Change its `args_schema` properties from `complaint`/`excerpt` to `question` (the user's biscuit question, verbatim) and `excerpt` (any knowledgebase excerpt already gathered, empty when none), both required, so the state stays structured. Update the tool description to match.
>
> Update `examples/decision/README.md`: retitle it around a decision-model router over the biscuit knowledgebase; describe the `scholar` and `search` agents, the shared `../simple/knowledgebase`, the one decision call that picks the region and gates retrieval, and the structured state. Use a normal biscuit question in the worked example (e.g. `which biscuits survive a long dunking?` or `tell me about french biscuits`), not a complaint, and show the `[route_question] >>> Decision:` block. Keep the Setup section's provider guidance, the `blorb decide` and `--state-json` guidance (updating the example state to a question object), and the Logs section.
>
> Update `internal/prefactor/example_test.go`'s `TestDecisionExampleConfigValid` to match: `DefaultAgent` is `scholar`; the `scholar` agent's tools are `kb-read`, `kb-grep`, `search`, `route_question`; the `search` agent's tools are `kb-read`, `kb-grep`; `route_question` is a `ToolTypeDecider` naming the `route_question` decider and carries a custom `args_schema` requiring `question` and `excerpt`; the `route_question` decider names `jev` and carries the four questions with the expected types (`region` choice, `question_kind` choice, `answerable_from_excerpt` noul, `action` score). Keep the existing `mustExampleAgent` helper and the `TestSimpleExampleTracingDisabled` assertion style. Update any prose in the test's doc comment that still says triage.
>
> Update the `examples/decision` entry in the top-level `README.md` to describe the router framing (a scholar that routes a biscuit question through one decision call, then retrieves through the shared knowledgebase) rather than ticket triage.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 4: make the decider's region answer the actual knowledgebase filename

> The route the decider picks is not directly usable: `region` answers with option keys like `france`, but the knowledgebase files are `france.md`, so the scholar greps `path: france` and fails with `statat france: no such file or directory`. Fix that and the prompt, so the decision output can be used verbatim. Edits are to `examples/decision/blorb.json`, `examples/decision/README.md`, and (only if needed) `internal/prefactor/example_test.go`.
>
> In `examples/decision/blorb.json`:
>
> - Rename the `region` choice options to the actual filenames, so the selected value is the file to read: `united-kingdom.md`, `france.md`, `italy.md`, `germany.md`, `netherlands.md`, `spain.md`, `scandinavia.md`, `north-america.md`, `australia-new-zealand.md`, `middle-east.md`, `india.md`, and `unknown`. Note the hyphens in the filenames where the current keys use underscores (`united_kingdom` -> `united-kingdom.md`), and that choice criteria keys are free-form strings, not constrained to identifiers (only the question names are). Update the `region` instructions to say the answer is the knowledgebase filename to read, and that `unknown` means the question is not region-specific so the whole knowledgebase should be searched.
> - In the `scholar` system prompt, state the knowledgebase files explicitly so the model never invents one: the files are one per region, named like `france.md` and `united-kingdom.md`, plus `dunking.md` and a `README.md` that lists them. Say the decision's `region` answer is the filename to read (or `unknown` for the whole tree), to pass it straight to `kb-grep`/`kb-read` as the `path` argument, and to include the `.md` suffix. When the region is `unknown`, search the whole knowledgebase. Keep the rest of the prompt's intent.
> - Update the `route_question` tool description if it implied a region name rather than a filename.
>
> In `examples/decision/README.md`: update the `region` criteria sample and the prose to say the options are the knowledgebase filenames (including `.md`), so the decision's answer is used directly as the read path; update the worked decision block's `region` value to `united-kingdom.md` (or `france.md`) and the `[route_question] >>> Decision:` output accordingly; and fix the sample state in the `--state-json` invocation if it names a region.
>
> In `internal/prefactor/example_test.go`: the `region` question stays a `choice`; no assertion currently checks its criteria keys, so the test likely needs no change - read it and adjust only if it does.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.
