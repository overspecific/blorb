# Simple example agent

A config with two named agents sharing one tool set:

- `simple` (the default) — a cheerful demo agent with all four tools: `echo` (echoes text back), `current_time` (returns the current date and time), and the `read` and `grep` builtins (sandboxed to the `knowledgebase/` directory, a field guide to biscuits of the world split into one file per region plus a dunking guide, with a README listing the files).
- `scholar` — a biscuit scholar granted only the `read` and `grep` builtins; no echo, no clock, just the knowledgebase.

Both agents share the tool declarations: the four tools are declared once at the top level and granted to each agent by name.

## Setup

The provider config points at a local [Lemonade](https://lemonade-server.com) server (`http://localhost:13305/v1`) with the `Gemma-4-E4B-it-GGUF` model. Adjust `base_url` and `model` inside each agent in `blorb.json` to match whatever OpenAI-compatible endpoint you want to use (OpenAI, Lemonade, LM Studio, vLLM, Ollama, ...). If your endpoint needs an API key, add an `api_key_env` entry naming the environment variable that holds it.

## Run

From the repo root:

```sh
bin/build
./blorb chat --config examples/simple/blorb.json                      # the default agent: simple
./blorb chat --config examples/simple/blorb.json --agent scholar    # the knowledgebase-only agent
```

Then try prompts like:

```
tell me the time
echo "blorb is fun"
what's a jammie dodger?
which biscuits survive a long dunking?
```

Type `exit` (or hit Ctrl-D) to quit. Ctrl-C interrupts an in-flight turn; Ctrl-C while idle exits.

## Logs

Running the example produces a `.logs` directory next to `blorb.json`. Each chat session gets its own `<timestamp>-<uuid>` subdirectory containing one file per LLM request/response and per tool call/result, so conversations never interleave. Each filename starts with a nanosecond timestamp, so sorting the filenames within a session replays the conversation in order. Logging can be disabled by adding `"logging": { "enabled": false }` to `blorb.json`.
