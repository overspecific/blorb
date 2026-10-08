# Voice example agent

The [simple](../simple) example, run as a voice session: agents and tools unchanged, plus a `voice` block on the main agent so it can be spoken to through AssemblyAI's Voice Agent API.

## Setup

Create an AssemblyAI account and copy an API key, which the command reads from the environment.

`blorb.json` ships with the connection setting a voice block needs:

```json
"voice": {
  "api_key_env": "ASSEMBLYAI_API_KEY",
  "greeting": "Hi! I am Simple. Ask me about biscuits, or the time.",
  "voice": "james"
}
```

The `voice` block requires `api_key_env`, the environment variable holding the API key. Everything else is optional: `greeting` is what the agent says on connect, `voice` picks a voice, `volume` sets the playback level, `echo_gate` mutes the microphone while the agent speaks (set in this example), and `input_command`/`output_command` override the microphone and speaker commands (`arecord` and `aplay` by default). The endpoint defaults to AssemblyAI's hosted service.

AssemblyAI's own model runs the conversation with `simple`, so `simple`'s `model` entry is not used for the spoken turns. It is still used for the agents `simple` delegates to: `ask_scholar`, `search`, and `ask_horologist` are subagent tools that run a normal agent turn on this machine, through the model in `blorb.json`. The model config points at a local [Lemonade](https://lemonade-server.com) server (`http://localhost:13305/v1`) with the `Gemma-4-E4B-it-GGUF` model, shared with [simple](../simple); adjust `base_url` and `model_name` to match whatever OpenAI-compatible endpoint you want.

## Run

Export the API key, then start the session from the repo root:

```sh
bin/build
export ASSEMBLYAI_API_KEY="your-api-key"
./blorb voice --config examples/voice/blorb.json
```

Speak into your microphone and the agent answers out loud. It needs `arecord` and `aplay` (from ALSA) on the machine, and a microphone and speaker. The console prints a live transcript while you talk: your words as they are recognised, the agent's words as it speaks, and tool activity as it runs, in the same `>>>` blocks the chat UI uses. When `simple` delegates to a subagent, the subagent's own work renders labeled `[scholar]`, `[search]` or `[horologist]` and indented, exactly as it does in `blorb chat`.

Ask about biscuits and `simple` calls `ask_scholar`, which greps the knowledgebase under `../simple/knowledgebase`; ask the time and it calls `ask_horologist`. The command tools (`echo`, `current_time`, `calendar`, `days_until`) and the `kb` builtin toolset run on this machine. `echo` and `calendar` need `jq` and `python3`.

This example sets `echo_gate: true`, which mutes the microphone while the agent is speaking so that a laptop speaker near the microphone does not feed the agent's own voice back to it. The trade-off is that you cannot talk over the agent while it speaks. With headphones you can drop the setting and barge-in works. Stop the session with Ctrl-C: the first interrupt hangs up cleanly, a second exits at once. Pass `--no-mic` to run without a microphone, so you can test the output path without one.
