# CLI reference

The `blorb run` one-shot mode, for scripting against Blorb — see the [README](../README.md) for a getting-started overview.

`blorb run` executes exactly one agent turn and exits — the scripting counterpart to `chat`. The prompt argument is required and exactly one is accepted: omitting it or passing extra arguments is a usage error (a scripting tool must not appear to hang when its arguments are forgotten, so stdin is only read when explicitly requested with `-` or `@-`).

`run` takes the same flags as `chat` (`-c | --config <path>`, `--agent <name>`, `--no-stream`, `--tool-output`) plus `--format <chat|plain|ndjson>` and `--logprobs`.

**`chat`** (the default) is identical to chat output — everything on stdout, `>>>` headings, streamed fragments, and the per-turn usage footer.

**`plain`** and **`ndjson`** are the machine-parsing formats, designed for scripts and pipelines — see the [output formats reference](formats.md) for their full details: the ndjson event types, the `stats` object, logprobs output, and streaming behavior.

`--logprobs` asks the server for per-token log probabilities (overriding `logprobs` in the model config) and prints one line per token after the response body in the chat and plain formats. It requires `--no-stream`; a run with `--logprobs` and streaming on fails before any LLM call. See [formats](formats.md#plain-logprobs) for the block's shape.

Exit codes: `0` on a completed turn, `1` on any error, `130` on Ctrl-C (SIGINT).
