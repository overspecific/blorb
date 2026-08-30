# Simple example agent

A minimal agent with two tools: `echo` (echoes text back) and `current_time` (returns the current date and time).

## Setup

The provider config points at a local [Lemonade](https://lemonade-server.com) server (`http://localhost:13305/v1`) with the `Gemma-4-E4B-it-GGUF` model. Adjust `base_url` and `model` in `blorb.json` to match whatever OpenAI-compatible endpoint you want to use (OpenAI, Lemonade, LM Studio, vLLM, Ollama, ...). If your endpoint needs an API key, add an `api_key_env` entry naming the environment variable that holds it.

## Run

From the repo root:

```sh
bin/build
./blorb chat --config examples/simple/blorb.json
```

Then try prompts like:

```
tell me the time
echo "blorb is fun"
```

Type `exit` (or hit Ctrl-D) to quit. Ctrl-C interrupts an in-flight turn; Ctrl-C while idle exits.

## Logs

Running the example produces a `.logs` directory next to `blorb.json` with one file per LLM request/response and per tool call/result. Each filename starts with a nanosecond timestamp, so sorting the filenames replays the conversation in order. Logging can be disabled by adding `"logging": { "enabled": false }` to `blorb.json`.