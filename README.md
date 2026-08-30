> **A little human note:** this is very much in the early slop zone - definitely a work-in-progress and I haven't spent much time going over things; I'm kind of just sketching out the shape. Your mileage may very much vary, it might set things on fire etc etc. 
> _-- Simon_

# Blorb
![Blorb](assets/blorb.jpg)

A single-binary tool for making AI agents.

Blorb lets you define an agent in a `blorb.json` file and run it in a simple interactive chat interface. It's built for experimentation: try out different system prompts and tool setups with minimal ceremony, using a plain JSON config and the standard library.

Tools are plain executables declared in the config, or built-ins implemented inside Blorb itself. When the model calls a tool, Blorb runs it and pipes the model's JSON arguments to it (for command tools, via stdin); the tool's output goes back to the model. Anything on your machine that can read stdin and write stdout can be a tool.

## Features

- Agent definition via a single `blorb.json` file
- Interactive chat REPL with multi-turn tool calling
- Streamed assistant responses over SSE (text, reasoning, and tool calls as they arrive); `--no-stream` disables it
- Full conversation and tool logging — every LLM request/response and tool call/result is written to a per-session subdirectory of `.logs` next to `blorb.json`, one timestamped file per wire interaction, so sorting the filenames replays the turn in order and sessions never interleave
- Tools as local subprocesses or built-ins (read/grep), with JSON-schema argument declarations for commands
- Any OpenAI-compatible chat completions endpoint as the LLM backend
- Per-tool 30s timeout, process-group cleanup, and stderr capture
- Single dependency-free Go binary

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

4. Run the example agent. It talks to whatever OpenAI-compatible chat completions endpoint you point it at, so adjust `base_url` and `model` in [examples/simple/blorb.json](examples/simple/blorb.json) to match yours (OpenAI, Lemonade, LM Studio, vLLM, Ollama, ...):

   ```sh
   ./blorb chat --config examples/simple/blorb.json
   ```

   Then try prompts like `tell me the time` or `echo "blorb is fun"`.

The `bin/` scripts add mise's shims to `PATH` if present, so you don't need to activate mise yourself to run them.

## Usage

```sh
blorb <command> [flags]
```

Commands:

- `chat` — chat with an agent defined in `blorb.json`
- `version` — print the version
- `help` — print help

### Chat

```sh
# uses ./blorb.json by default
./blorb chat

# or point at an explicit config
./blorb chat --config examples/simple/blorb.json
```

Flags: `-c | --config <path>`, `--no-stream` (disable streamed responses), `-h | --help`. Version: `blorb --version` or `blorb version`.

Type `exit` (or hit Ctrl-D) to quit. Ctrl-C interrupts an in-flight turn; Ctrl-C while idle exits the session.

## Configuration

Agents are defined in a `blorb.json` file:

```json
{
  "name": "simple",
  "system_prompt": "You are Simple, a cheerful demo agent. Keep your answers short.",
  "provider": {
    "type": "openai",
    "model": "Gemma-4-E4B-it-GGUF",
    "base_url": "http://localhost:13305/v1",
    "api_key_env": "MY_API_KEY"
  },
  "max_turns": 10,
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



### Top-level fields


| Field           | Required | Description                                    |
| --------------- | -------- | ---------------------------------------------- |
| `name`          | yes      | Agent name, shown in the chat banner.          |
| `system_prompt` | yes      | The system prompt for the agent.               |
| `provider`      | yes      | LLM backend config (see below).                |
| `max_turns`     | no       | Max model turns per user message (default 10). |
| `tools`         | no       | List of tool declarations (see below).         |
| `logging`       | no       | Wire logging config (see below).               |
| `prefactor`     | no       | Prefactor tracing config (see below).          |




### Provider

The `provider` object selects the LLM backend. The `type` discriminator determines which fields are recognized; this is the extension point for future backend types.

Currently supported: `openai` — any OpenAI-compatible chat completions API (OpenAI, Lemonade, LM Studio, vLLM, Ollama, ...).


| Field         | Required | Description                                                                                         |
| ------------- | -------- | --------------------------------------------------------------------------------------------------- |
| `type`        | yes      | Must be `openai`.                                                                                   |
| `model`       | yes      | Model name passed to the API.                                                                       |
| `base_url`    | yes      | Base URL of the chat completions endpoint, e.g. `http://localhost:13305/v1`. Must be http or https. |
| `api_key_env` | no       | Name of the environment variable containing the API key, if the endpoint needs one.                 |




### Tools

Each tool has a required `type` field selecting one of two kinds:

**`command` tools** run an executable as a subprocess:

- `name` — identifier the model uses to call it; must match `^[a-zA-Z0-9_-]+$` and be unique within the config.
- `description` — tells the model what the tool does and when to use it.
- `command` — the argv to run, e.g. `["python", "-u", "my_tool.py"]`.
- `args_schema` — optional JSON Schema object describing the arguments; passed through to the API. When omitted, an empty schema is used. Command tools only: builtins define their own model-facing schema.

When the model calls a tool, Blorb runs the command with the model's JSON arguments piped to its stdin. Anything the tool writes to stdout (success) or stderr (failure) is returned to the model as the tool result. Each tool execution has a 30 second timeout, and on timeout the whole process group is killed.

**`builtin` tools** are implemented inside Blorb itself and need no executable. The `builtin` field selects the implementation — currently `read` and `grep` — and the `config` object configures it:

```json
{
  "type": "builtin",
  "name": "read",
  "description": "Look inside project files.",
  "builtin": "read",
  "config": { "base_dir": "./src" }
}
```

Both builtins are file tools sharing one setting: `base_dir`, a required path to a directory the tool is sandboxed to. Relative paths in `blorb.json` resolve against the config file's directory (the same rule as `logging.path`), so `"base_dir": "knowledgebase"` anchors next to wherever the config lives. The model's path arguments resolve inside that directory tree, and only that tree is accessible — attempts to escape (including through symlinks) fail; symlinks are followed only when they resolve back inside `base_dir`. `read` takes `{"path": ...}` and returns a file's contents; `grep` takes `{"pattern": ...}` and an optional `{"path": ...}` (default: the base directory) and returns matches as `path:line:text`, skipping `.git` directories and binary files. Each tool execution has a 30 second timeout like command tools.

### Logging

Blorb writes full conversation and tool logging by default. Every LLM request/response and every tool call/result is written as one timestamped file into a log directory next to the `blorb.json` file, so a plain lexical sort of the filenames reconstructs the exact sequence of a turn.

```json
{
  "logging": { "path": ".logs", "enabled": true }
}
```

| Field      | Default | Description                                                                        |
| ---------- | ------- | ---------------------------------------------------------------------------------- |
| `path`     | `.logs` | Log directory name, resolved relative to the config file. A single directory name — no separators. |
| `enabled`  | `true`  | Set to `false` to turn logging off.                                                |

Each chat session writes into its own subdirectory of the log dir, named `<timestamp>-<uuid>` (e.g. `20260830T142533-123456789-a1b2c3d4e5f6...`), so conversations never interleave and the session directories themselves sort chronologically.

Files are named `<timestamp>-<kind>.txt`, e.g. `20260830T142533-123456789-llm-request.txt`. The kinds are `llm-request`, `llm-response`, `tool-request`, and `tool-result`. Because the timestamp comes first with nanosecond precision, sorting the filenames within a session's subdirectory replays the turn in order.

Log files capture full request/response bodies and headers, including any API key sent to the provider, and the full content of tool calls and results. Keep them out of version control and treat them as sensitive. Log writes are best-effort: a logging failure never fails the agent run.

### Prefactor tracing

Blorb can trace every agent run to [Prefactor](https://prefactor.ai), an agent-activity auditing platform. Add a `prefactor` block to enable it; when present, chat sessions are recorded as Prefactor agent instances, and each user message, assistant message, LLM API call, and tool call becomes a span within the instance.

```json
{
  "prefactor": {
    "api_token_env": "PREFACTOR_API_TOKEN",
    "api_url": "https://app.prefactorai.com/api/v1",
    "agent_id": "...",
    "environment_id": "..."
  }
}
```

| Field            | Default                             | Description                                                                                          |
| ---------------- | ----------------------------------- | ---------------------------------------------------------------------------------------------------- |
| `api_token_env`  | `PREFACTOR_API_TOKEN`               | Name of the environment variable containing the Prefactor API token. The variable must be set and non-empty. |
| `api_url`        | `https://app.prefactorai.com/api/v1` | API base URL. Must be http or https with a host when set.                                            |
| `agent_id`       | _(empty)_                           | Prefactor agent to register instances under. Optional; may be empty for deployment-scoped tokens.    |
| `environment_id` | _(empty)_                           | Prefactor environment to register instances under. Optional.                                         |

**What maps to what.** A chat session is one Prefactor agent instance: registered on startup, finished when the session ends (`complete` whenever the chat exits normally — exit/quit, EOF, or SIGINT at the prompt, since quitting the chat is a finished chat; `failed` on any error exit). Within the session, each user message opens a `blorb:agent_turn` span that stays open while the agent works, with these child spans:

- `blorb:user_message` — the user's input.
- `blorb:llm` — one LLM API call, with the model, full message history, response content, reasoning, tool calls, and token usage.
- `blorb:tool:<name>` — one tool execution, with the JSON arguments and the tool output (and whether it failed).
- `blorb:assistant_message` — a completed assistant reply (whole-message mode only; streamed replies are covered by the `blorb:llm` span).

The activity schema is derived from your config — including each tool's `args_schema` — and registered with Prefactor on startup, so spans are well-typed in the platform.

**Hard dependency.** When tracing is configured, a Prefactor failure fails the run rather than continuing untraced: a startup failure refuses to start the session, and a failure mid-turn ends the session with the error reported.

**Termination.** Stopping a run from the Prefactor web app sends a terminate signal, which blorb honours cooperatively: the in-flight turn fails, the session ends, and the termination reason is printed. The instance is not finished a second time locally — the platform marks terminated instances itself.

`PREFACTOR_API_TOKEN` (or the configured `api_token_env`) must be exported before running:

```
export PREFACTOR_API_TOKEN="pf_..."
blorb chat
```

## Example

See [examples/simple](examples/simple) for a minimal agent with `echo` and `current_time` command tools and `read`/`grep` builtins (pointed at the example's `knowledgebase/` directory), including notes on pointing the provider at different OpenAI-compatible servers.



## Development

- `mise.toml` — pins the toolchain (`mise install` to set it up)
- `bin/qc` — format, style check, and run tests
- `bin/build` — build the `blorb` binary

Layout:

- `internal/config` — the `blorb.json` schema, loading, and validation
- `internal/engine` — the agent loop: model calls, tool execution, turn limits
- `internal/chat` — the interactive chat REPL
- `internal/tools` — tool registry and subprocess execution
- `internal/tools/builtin` — built-in tools (`read`, `grep`)
- `internal/llm` — provider-neutral LLM types
- `internal/llm/openai` — OpenAI-compatible client, with SSE streaming support
- `internal/logging` — wire logging for LLM and tool interactions



## License

[MIT](LICENSE)