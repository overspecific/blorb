# Voice example agent

A minimal voice agent: one agent (`assistant`, the config's `default_agent`) with a `voice` block, ready to run as a spoken conversation through AssemblyAI's Voice Agent API.

## Setup

Create an AssemblyAI account and copy an API key, which the command reads from the environment.

`blorb.json` ships with the one connection setting a voice block needs:

```json
"voice": {
  "api_key_env": "ASSEMBLYAI_API_KEY"
}
```

The `voice` block requires `api_key_env`, the environment variable holding the API key. Everything else is optional: `greeting` is what the agent says on connect, `voice` picks a voice, `volume` sets the playback level, and `input_command`/`output_command` override the microphone and speaker commands (`arecord` and `aplay` by default). The endpoint defaults to AssemblyAI's hosted service.

The model config in `blorb.json` is only a placeholder: a voice session talks to AssemblyAI's own conversational model, so the agent's `model` entry is not used. The tools are the part that runs locally.

## Run

Export the API key, then start the session from the repo root:

```sh
bin/build
export ASSEMBLYAI_API_KEY="your-api-key"
./blorb voice --config examples/voice/blorb.json
```

Speak into your microphone and the agent answers out loud. It needs `arecord` and `aplay` (from ALSA) on the machine, and a microphone and speaker. The console prints a live transcript while you talk: your words as they are recognised, the agent's words as it speaks, and any tool calls it makes. Tools run on this machine, so the agent answers time questions with the local `clock` tool.

Headphones avoid the microphone picking up the speaker, which the agent hears as you talking over it. Stop the session with Ctrl-C: the first interrupt hangs up cleanly, a second exits at once. Pass `--no-mic` to run without a microphone, so you can test the output path without one.
