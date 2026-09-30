# Band example agent

A minimal [Band](https://band.ai) setup: one agent (`roommate`, the config's `default_agent`) with a `band` block, ready to connect to the platform as a remote agent.

## Setup

Register an external agent on Band and copy its agent API key, which the command reads from the environment.

`blorb.json` ships with a placeholder connection:

```json
"band": {
  "api_key_env": "BAND_API_KEY"
}
```

The `band` block requires `api_key_env`, the environment variable holding the API key. The endpoints default to Band's hosted Agent API and subscriptions socket, so `rest_url` and `ws_url` only need setting for a self-hosted deployment. The agent's Band id is not configured; the command reads it from the API key at startup.

The model config points at a local [Lemonade](https://lemonade-server.com) server (`http://localhost:13305/v1`) with the `Gemma-4-E4B-it-GGUF` model. Adjust `base_url` and `model_name` in the top-level `models` list in `blorb.json` to match whatever OpenAI-compatible endpoint you want to use (OpenAI, Lemonade, LM Studio, vLLM, Ollama, ...). If your endpoint needs an API key, add an `api_key_env` entry naming the environment variable that holds it.

## Run

Export the API key, then start the frontend from the repo root:

```sh
bin/build
export BAND_API_KEY="the-agent-api-key"
./blorb band --config examples/band/blorb.json
```

The command validates the key, joins the agent's rooms, and answers messages that mention the agent. Mention the agent in a room it is part of and it replies; each reply is sent by calling the `band_send_message` tool with an @mention of the participant being addressed. As the agent works, each room's activity is printed to standard output - assistant text, tool calls and results, and subagent activity - so you can watch what it is doing; assistant responses stream as they arrive, so pass `--no-stream` to wait for each whole response, and add `--tool-output` to show full tool result bodies. Stop it with Ctrl-C: the first interrupt stops gracefully after the in-flight message, a second exits immediately. On shutdown it prints the session usage footer to stderr.

One `blorb band` process serves one Band agent. Running it twice with the same agent evicts the older connection, because the platform allows only the latest connection per agent.
