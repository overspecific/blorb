# Simple example agent

A minimal agent with two tools: `echo` (echoes text back) and `current_time` (returns the current date and time).

## Setup

The provider config points at a local Ollama server (`http://localhost:11434/v1`) with the `gpt-4o-mini` model. Adjust `base_url` and `model` in `blorb.json` to match whatever OpenAI-compatible endpoint you want to use (OpenAI, LM Studio, vLLM, Ollama, ...). If your endpoint needs an API key, add an `api_key_env` entry naming the environment variable that holds it.

## Run

From the repo root:

```sh
bin/build
./blorb chat -config examples/simple/blorb.json
```

Then try prompts like:

```
tell me the time
echo "blorb is fun"
```

Type `exit` (or hit Ctrl-D) to quit, Ctrl-C to interrupt an in-flight turn.