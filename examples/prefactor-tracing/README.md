# Prefactor tracing example agent

A simplified version of the [simple](../simple) example focused on Prefactor tracing: a single named agent (`tracey`, the config's `default_agent`) with just the `read` and `grep` builtins, pointed at that example's `knowledgebase/` directory (a field guide to biscuits of the world), with a `prefactor` block.

## Setup

The model config points at a local [Lemonade](https://lemonade-server.com) server (`http://localhost:13305/v1`) with the `Gemma-4-E4B-it-GGUF` model. Adjust `base_url` and `model_name` in the top-level `models` list in `blorb.json` to match whatever OpenAI-compatible endpoint you want to use (OpenAI, Lemonade, LM Studio, vLLM, Ollama, ...). If your endpoint needs an API key, add an `api_key_env` entry naming the environment variable that holds it.

## Prefactor tracing

`blorb.json` ships with a minimal `prefactor` block:

```json
"prefactor": { "api_token_env": "PREFACTOR_API_TOKEN" }
```

Export the token (or point `api_token_env` at whatever variable holds yours):

```sh
export PREFACTOR_API_TOKEN="pf_..."
```

With the block present, each chat session registers a Prefactor agent instance and records every user message, LLM call, and tool call as a span; a `blorb run` invocation records one instance containing a single turn, with the same span model. See the [Prefactor tracing section](../../README.md#prefactor-tracing) in the main README for the full field reference including `api_url`, `agent_id`, and `environment_id`. To run without tracing, remove the block — blorb then behaves exactly as before. Note that a Prefactor failure with the block present fails the run rather than continuing untraced.

## Run

From the repo root:

```sh
bin/build
./blorb chat --config examples/prefactor-tracing/blorb.json
./blorb run --config examples/prefactor-tracing/blorb.json "what's a jammie dodger?"   # one traced instance, one turn
```
Then try prompts like:

```
what's a jammie dodger?
which biscuits survive a long dunking?
```

Type `exit` (or hit Ctrl-D) to quit. Ctrl-C interrupts an in-flight turn; Ctrl-C while idle exits.

## Logs

Running the example produces a `.logs` directory next to `blorb.json`. Each chat session gets its own `<timestamp>-<uuid>` subdirectory containing one file per LLM request/response and per tool call/result, so conversations never interleave. Each filename starts with a nanosecond timestamp, so sorting the filenames within a session replays the conversation in order. Logging can be disabled by adding `"logging": { "enabled": false }` to `blorb.json`.