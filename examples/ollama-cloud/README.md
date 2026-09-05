# Ollama cloud example agent

A simplified version of the [simple](../simple) example pointed at [Ollama cloud](https://ollama.com) instead of a local server: a single named agent (`cloudy`, the config's `default_agent`) with just the `read` and `grep` builtins, sandboxed to that example's `knowledgebase/` directory (a field guide to biscuits of the world). It demonstrates blorb's native `ollama` model type, including `reasoning_effort` on a thinking model — gemma4 does its reasoning out loud, and it shows up in the chat under `>>> Assistant (thinking):`.

## Setup

Create an [Ollama account](https://ollama.com) and grab an API key, then export it:

```sh
export OLLAMA_API_KEY="..."
```

The model entry in `blorb.json` reads:

```json
{
  "name": "cloud-gemma4",
  "type": "ollama",
  "base_url": "https://ollama.com",
  "model_name": "gemma4:e4b",
  "api_key_env": "OLLAMA_API_KEY",
  "reasoning_effort": "medium"
}
```

For `type: ollama`, `base_url` is the bare server root — `https://ollama.com` for the cloud, or `http://localhost:11434` for a local Ollama (which needs no `api_key_env` at all). `model_name` is the Ollama model tag; browse https://ollama.com/library for what's available, and pick a thinking-capable model (gemma4, qwen3, deepseek-r1, ...) if you want to see the reasoning output. `reasoning_effort` is optional: `none` disables thinking, other values (`low`, `medium`, `high`, `max`) ask for more or less of it, and removing the line uses the server default. Remove `api_key_env` entirely for a local server; for the cloud it is required — an empty environment variable is a config error.

## Run

From the repo root:

```sh
bin/build
./blorb chat --config examples/ollama-cloud/blorb.json
./blorb run --config examples/ollama-cloud/blorb.json "which biscuits survive a long dunking?"   # one turn, then exit
```

Then try prompts like:

```
what's a jammie dodger?
which biscuits survive a long dunking?
```

Type `exit` (or hit Ctrl-D) to quit. Ctrl-C interrupts an in-flight turn; Ctrl-C while idle exits. Streaming is on by default — gemma4's thinking streams live before the answer; `--no-stream` turns that off.

## Logs

Running the example produces a `.logs` directory next to `blorb.json`, one file per LLM request/response and per tool call/result — the requests show the `think` field blorb sends from `reasoning_effort`, and the responses show the `thinking` field decoded back out. Logging can be disabled by adding `"logging": { "enabled": false }` to `blorb.json`.