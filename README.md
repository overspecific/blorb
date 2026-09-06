> **A little human note:** this is very much in the early slop zone - definitely a work-in-progress and I haven't spent much time going over things; I'm kind of just sketching out the shape. Your mileage may very much vary, it might set things on fire etc etc. 
> _-- Simon_

# Blorb
![Blorb](assets/blorb.jpg)

A single-binary tool for making AI agents.

Blorb lets you define agents in a `blorb.json` file and chat with any of them. It's built for experimentation: try different system prompts and tool setups with minimal ceremony, using a plain JSON config — a single config can hold several named agents sharing one tool set, and you pick which one to run per invocation.

Tools are plain executables declared in the config, built-ins implemented inside Blorb, or other agents in the same config (subagents). When the model calls a tool, Blorb runs it, pipes the JSON arguments to its stdin (for command tools), and returns the output to the model. Anything that can read stdin and write stdout can be a tool.

## Features

- One `blorb.json` defines any number of named agents, each with its own system prompt, named model, and tool grants
- Interactive chat REPL with multi-turn tool calling, running a chosen agent per invocation
- One-shot `run` mode: one prompt in, one agent turn out, for scripting — the prompt comes from a quoted argument, a file, or stdin, with `--format plain` (agent text only on stdout, progress on stderr) and `--format ndjson` (streaming JSON events) alongside the chat-style default
- Streamed assistant responses over SSE (text, reasoning, and tool calls as they arrive); `--no-stream` disables it
- Tool results summarize by default in chat (a character and line count); `--tool-output` shows the full output, and failed results and subagent output always show in full
- Usage stats: every turn ends with a usage footer — one line per agent (tokens plus, when the client measures, elapsed time, output bytes with a text/reasoning/tool-call split, and derived throughput) and a `total:` line; chat prints the same session totals at exit
- Full wire logging: every LLM request/response and tool call/result is written to a timestamped file per session, so a plain sort of the filenames replays a turn in order (see [Logging](docs/configuration.md#logging))
- Tools as local subprocesses, built-ins (`read`, `grep`), or subagents — one agent delegating to another defined in the same config, with JSON Schema argument declarations
- OpenAI-compatible chat completions endpoints and Ollama servers (local or cloud) as LLM backends, with provider-level sampling defaults, structured output, tool-choice control, and per-token logprobs
- `blorb models` — per provider, what the server has installed, flagging configured models that are missing
- Optional tracing of every run to [Prefactor](https://prefactor.ai) (see [Prefactor tracing](docs/configuration.md#prefactor-tracing))
- Per-tool 30s timeout, process-group cleanup, and stderr capture
- A single Go binary

## Getting started

1. Install [mise](https://mise.jdx.dev). Blorb uses mise to set up and manage the development environment, including the Go toolchain (pinned in `mise.toml`).

2. From the repo root, install the toolchain and run the checks:

   ```sh
   mise install
   bin/qc
   ```

3. Build the binary:

   ```sh
   bin/build
   ```

   This produces the `blorb` binary in the repo root.

4. Run the example agent. It talks to whatever OpenAI-compatible chat completions endpoint you point it at, so adjust the provider's `base_url` and the model's `model_name` in [examples/simple/blorb.json](examples/simple/blorb.json) to match yours (OpenAI, Lemonade, LM Studio, vLLM, Ollama, ...):

   ```sh
   ./blorb chat --config examples/simple/blorb.json
   ```

   Then try prompts like `what's a jammie dodger?` or `which biscuits survive a long dunking?`. `./blorb models --config examples/simple/blorb.json` lists what the server has installed.

The `bin/` scripts add mise's shims to `PATH` if present, so you don't need to activate mise yourself to run them.

## Usage

```sh
blorb <command> [flags]
```

Commands:

- `chat` — chat with an agent defined in `blorb.json`
- `run` — run one agent turn and exit
- `models` — list the models each provider's server has installed
- `version` — print the version
- `help` — print help

```sh
# chat with the config's default agent (./blorb.json by default)
./blorb chat

# or chat with an explicitly named agent
./blorb chat --agent alpha

# or point at an explicit config
./blorb chat --config examples/simple/blorb.json

# flags can appear in any order
./blorb chat --config examples/simple/blorb.json --agent simple
```

The agent is resolved in order: the `--agent` flag when given, else the config's `default_agent`; with neither, the command fails and lists the defined agents. Naming an agent that is not in the config is an error.

Flags: `-c | --config <path>`, `--agent <name>`, `--no-stream` (disable streamed responses), `--tool-output` (show the full output of tool results — without it, result blocks show a character and line count instead; failed results and subagent output always show in full), `-h | --help`. Version: `blorb --version` or `blorb version`.

Type `exit` (or hit Ctrl-D) to quit. Ctrl-C interrupts an in-flight turn; Ctrl-C while idle exits the session.

Each turn ends with a usage footer: one line per agent that made a call (multiple invocations of the same agent sum into its one line), then a `total:` line — or just the agent's line when no subagents ran, since it is already the total. With measured stats, each line also carries elapsed time, output bytes with the text/reasoning/tool-call split, and derived throughput:

```
---
main: 123 prompt, 456 completion, 579 total, 4s, 8.2KB output (5KB text, 2KB reasoning, 1.2KB tools), 114.0 tok/s, 2.1KB/s
worker: 23 prompt, 156 completion, 179 total, 2s, 1.1KB output, 78.0 tok/s, 563B/s
total: 146 prompt, 612 completion, 758 total, 6s, 9.3KB output, 102.0 tok/s, 1.6KB/s
```

When the session ends, chat prints the same block with a `session ` prefix on each line. A zero token count means the provider did not report usage; a line's stats part is omitted when nothing was measured. Note that for reasoning models the rates are end-to-end — thinking time is included in both the elapsed span and the output bytes — so they read as "delivered per wall-clock second".

### One-shot runs

`blorb run` executes exactly one agent turn and exits — the scripting counterpart to `chat`:

```sh
# a literal prompt
./blorb run "Summarize ./notes.md"

# the prompt from a file
./blorb run @prompt.txt

# the prompt from stdin
echo "What changed in the last commit?" | ./blorb run -

# stdin via the @ form (equivalent to -)
git diff | ./blorb run @-

# a prompt that should start with a literal @: escape it with @@
./blorb run "@@ literal @ prompt"
```

The prompt argument is required and exactly one is accepted: omitting it or passing extra arguments is a usage error (a scripting tool must not appear to hang when its arguments are forgotten, so stdin is only read when explicitly requested with `-` or `@-`).

`run` takes the same flags as `chat` (`-c | --config <path>`, `--agent <name>`, `--no-stream`, `--tool-output`) plus `--format <chat|plain|ndjson>` and `--logprobs`. `chat` (the default) is chat output; `plain` puts just the agent's output on stdout (progress and the usage footer on stderr) so it composes in pipelines; `ndjson` streams the run's full event stream to stdout as one JSON object per line — assistant text, reasoning, tool calls and results, token usage, and subagent activity — ending with a `done` or `error` event.

Exit codes: `0` on a completed turn, `1` on any error, `130` on Ctrl-C (SIGINT).

See the [CLI reference](docs/cli.md) for the format details: the ndjson event types, the `stats` object, streaming behavior, and logprobs output.

### Listing installed models

`blorb models` enumerates, per provider in the config, what that provider's server has installed — a quick check that every configured `model_name` is actually served, flagging configured models the server does not have. See [Listing installed models](docs/configuration.md#listing-installed-models) for the full reference and sample output.

## Configuration

A `blorb.json` defines the shared provider, model, and tool vocabularies and the agents that use them:

```json
{
  "default_agent": "simple",
  "providers": [
    {
      "name": "local",
      "type": "openai-compatible",
      "base_url": "http://localhost:13305/v1",
      "api_key_env": "MY_API_KEY",
      "temperature": 0.7
    }
  ],
  "models": [
    {
      "name": "local-gemma",
      "provider": "local",
      "model_name": "Gemma-4-E4B-it-GGUF"
    },
    {
      "name": "local-gemma-strict",
      "provider": "local",
      "model_name": "Gemma-4-E4B-it-GGUF",
      "reasoning_effort": "medium"
    }
  ],
  "agents": [
    {
      "name": "simple",
      "system_prompt": "You are Simple, a cheerful demo agent. Keep your answers short.",
      "model": "local-gemma",
      "max_turns": 10,
      "tools": ["echo", "read"]
    },
    {
      "name": "quiet",
      "system_prompt": "You answer in one short sentence.",
      "model": "local-gemma-strict",
      "tools": ["echo"]
    }
  ],
  "tools": [
    {
      "type": "command",
      "name": "echo",
      "description": "Echoes back whatever text you pass in.",
      "command": ["echo"],
      "args_schema": {
        "type": "object",
        "properties": {
          "message": { "type": "string" }
        },
        "required": ["message"]
      }
    },
    {
      "type": "builtin",
      "name": "read",
      "description": "Read a file at the given path and return its contents.",
      "builtin": "read",
      "config": { "base_dir": "." }
    }
  ]
}
```

With this config, `./blorb chat` runs `simple` (the `default_agent`), `./blorb chat --agent quiet` runs the quiet one, and `./blorb chat --agent nope` fails naming the defined agents. Both agents share the `echo` tool; only `simple` also uses the `read` builtin. Both models share one provider — one server declaration, two model entries.

See [docs/configuration.md](docs/configuration.md) for the complete field reference: providers (and their sampling fields), models (tool choice, logprobs, structured output), agents, tools (command, builtin, subagent), wire logging, and Prefactor tracing.

## Examples

See [examples/simple](examples/simple) for a two-agent config sharing one tool set: `echo` and `current_time` command tools and `read`/`grep` builtins (pointed at the example's `knowledgebase/` directory), with `scholar` granted only the knowledgebase builtins, including notes on pointing the model at different OpenAI-compatible servers.

See [examples/prefactor-tracing](examples/prefactor-tracing) for a single-agent variant with just the `read`/`grep` builtins (sharing simple's `knowledgebase/`) and Prefactor tracing enabled.

See [examples/ollama-cloud](examples/ollama-cloud) for a single-agent variant pointed at Ollama cloud (native `ollama` model type, API key via `api_key_env`, `reasoning_effort` on a thinking model).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup, building, and testing.

## License

[MIT](LICENSE)