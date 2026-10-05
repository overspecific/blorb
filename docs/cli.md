# CLI reference

The `blorb run` one-shot mode, for scripting against Blorb - see the [README](../README.md) for a getting-started overview.

`blorb run` executes exactly one agent turn and exits - the scripting counterpart to `chat`. The prompt argument is required and exactly one is accepted: omitting it or passing extra arguments is a usage error (a scripting tool must not appear to hang when its arguments are forgotten, so stdin is only read when explicitly requested with `-` or `@-`).

`run` takes the same flags as `chat` (`-c | --config <path>`, `--agent <name>`, `--no-stream`, `--tool-output`) plus `--format <chat|plain|ndjson>` and `--logprobs`.

**`chat`** (the default) is identical to chat output - everything on stdout, `>>>` headings, streamed fragments, and the per-turn usage footer.

**`plain`** and **`ndjson`** are the machine-parsing formats, designed for scripts and pipelines - see the [output formats reference](formats.md) for their full details: the ndjson event types, the `stats` object, logprobs output, and streaming behavior.

The agent's configured judges, if any, run after the turn and print their judgements: in `chat` and `plain` as `>>> Judge: <name>` blocks (on stderr for `plain`, since it is not the run's output), and in `ndjson` as `judge_*` events before the terminal `done`/`error`. See [Judges](configuration.md#judges).

`--logprobs` asks the server for per-token log probabilities (overriding `logprobs` in the model config) and prints one line per token after the response body in the chat and plain formats. It requires `--no-stream`; a run with `--logprobs` and streaming on fails before any LLM call. See [formats](formats.md#plain-logprobs) for the block's shape.

Exit codes: `0` on a completed turn, `1` on any error, `130` on Ctrl-C (SIGINT).

## `blorb decide`

`blorb decide` is the decider counterpart of `run`: it evaluates one decider against one state and prints the answers as JSON, exiting. It directly invokes a decider the way `run --agent` invokes an agent.

```sh
./blorb decide --decider triage "The invoice failed twice and the customer is threatening to cancel."
```

It takes `-c | --config <path>` and a required `--decider <name>` flag. There is no default decider: deciders are named things you invoke deliberately, and `default_agent` names an agent. Naming a decider that is not defined exits `1` with an error naming the available deciders.

The positional `[state]` argument uses the same syntax as `run`'s prompt: a literal string (`@@` escapes a leading `@`), `@file` to read from a file, or `-` to read from stdin. Exactly one state argument is accepted; stdin is only read when explicitly requested with `-` or `@-`.

stdout is the answers JSON, one object keyed by question name. There is no usage footer: the JSON body is the output a script consumes (usage still lands in the wire logs). A decider-level failure - the server rejected the request - is written to stderr and exits `1`; there is no parent model to react to it, so the failure is yours to see.

Exit codes: `0` on a completed decision, `1` on any error, `130` on Ctrl-C (SIGINT), matching `run`.
