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
- Token usage stats: every turn ends with a token footer (prompt/completion/total), chat prints session totals at exit, and subagent usage is itemised by agent in the footer's per-agent split. When the provider client can measure, the footer also shows a stats line — elapsed time, output bytes (split into text, reasoning, and tool-call bytes), and derived throughput (tok/s, KB/s)
- Full wire logging: every LLM request/response and tool call/result is written to a timestamped file per session, so a plain sort of the filenames replays a turn in order (see [Logging](#logging))
- Tools as local subprocesses, built-ins (`read`, `grep`), or subagents — one agent delegating to another defined in the same config, with JSON Schema argument declarations
- Any OpenAI-compatible chat completions endpoint as the LLM backend
- Optional tracing of every run to [Prefactor](https://prefactor.ai) (see [Prefactor tracing](#prefactor-tracing))
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

4. Run the example agent. It talks to whatever OpenAI-compatible chat completions endpoint you point it at, so adjust `base_url` and `model_name` in the `models` list in [examples/simple/blorb.json](examples/simple/blorb.json) to match yours (OpenAI, Lemonade, LM Studio, vLLM, Ollama, ...):

   ```sh
   ./blorb chat --config examples/simple/blorb.json
   ```

   Then try prompts like `what's a jammie dodger?` or `which biscuits survive a long dunking?`.

The `bin/` scripts add mise's shims to `PATH` if present, so you don't need to activate mise yourself to run them.

## Usage

```sh
blorb <command> [flags]
```

Commands:

- `chat` — chat with an agent defined in `blorb.json`
- `run` — run one agent turn and exit
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

Each turn ends with a token usage footer, e.g. `tokens: 123 prompt, 456 completion, 579 total` — with a per-agent split when subagents ran (see [Subagent tools](#tools)). When the session ends, chat prints the cumulative totals on a `session tokens: ...` line. A zero count means the provider did not report usage.

When the provider client measures, the footer carries a second line with the call stats, e.g. `stats: 4s, 9.2KB output (6KB text, 2KB reasoning, 1.2KB tools), 20.0 tok/s, 2.3KB/s` — elapsed time, output bytes with the text/reasoning/tool-call split (components that are zero are omitted from the parenthesised split), and derived throughput. The session footer shows cumulative stats the same way. The line is omitted entirely when nothing was measured. Note that for reasoning models the rates are end-to-end — thinking time is included in both the elapsed span and the output bytes — so they read as "delivered per wall-clock second".

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

`run` takes the same flags as `chat` (`-c | --config <path>`, `--agent <name>`, `--no-stream`, `--tool-output`) plus `--format <chat|plain|ndjson>`.

**`chat`** (the default) is identical to chat output — everything on stdout, `>>>` headings, streamed fragments, and the per-turn token footer with its stats line.

**`plain`** puts just the agent's output on stdout — no headings, no decorations, no trailing newline — so it composes in pipelines (`./blorb run --format plain "..." | jq`). Everything else goes to stderr: the chat-style progress (headings, tool activity, streamed fragments) and the token footer. The agent's output is the assistant's text events spliced exactly as they arrived, with nothing added between them.

**`ndjson`** streams the run's full event stream to stdout as one JSON object per line, as it happens: assistant text, reasoning, tool calls and results, token usage, and subagent activity. Each line is a flat object discriminated by its `type` field; ignore unknown types for forward compatibility. The stream ends with a `done` event carrying the final text and usage totals, or an `error` event on failure. A run that fails before the turn starts (bad config, unknown format) emits no events.

Event types (subagent activity uses the same vocabulary prefixed `subagent_`, with `agent` and `depth` fields added):

```
text_delta      {type, text}                       assistant text fragment (streaming)
thinking_delta  {type, thinking}                   reasoning fragment (streaming)
tool_call_delta {type, index, name?, arguments}    tool call fragment; assemble by index, concatenating arguments
text            {type, text}                       whole assistant message (with --no-stream)
thinking        {type, thinking}                   whole reasoning (with --no-stream)
tool_call       {type, name, arguments}            whole tool call (with --no-stream)
tool_result     {type, name, output, failed}       tool result; output is always the full body
usage           {type, agent, model, usage, stats} one LLM call's token usage and call stats
done            {type, text?, usage, stats?, rates?, agents}  terminal on success
error           {type, error}                      terminal on failure
```

The `stats` object on `usage`, `subagent_usage`, and `done` carries the measured output bytes — `{"output":{"content_bytes":...,"reasoning_bytes":...,"tool_call_bytes":...},"elapsed_ns":...}`; derive the total by summing the three components. `done.agents[].stats` is each agent's summed stats. `done.rates` (`{"tokens_per_sec":...,"bytes_per_sec":...}`) is a convenience derived from the summed stats — consumers can compute their own rates from the raw fields — and is omitted when no time was measured.

For example, to print just the assistant's text as it streams:

```sh
./blorb run --format ndjson "..." | jq -r 'select(.type=="text_delta") | .text'
```

Streaming applies across all formats: `--no-stream` switches to whole-message events (in ndjson, the `text`/`thinking`/`tool_call` types instead of deltas; in plain, whole blocks on stderr instead of live fragments). `--tool-output` applies to the chat and plain result blocks; ndjson always carries full tool result bodies. The exit codes are unchanged — ndjson's `done`/`error` event is the machine-readable outcome, not a changed exit contract. A run that fails before the turn emits no ndjson events; the error goes to stderr.

Exit codes: `0` on a completed turn, `1` on any error, `130` on Ctrl-C (SIGINT).

## Configuration

A `blorb.json` defines the shared model and tool vocabularies and the agents that use them:

```json
{
  "default_agent": "simple",
  "models": [
    {
      "name": "local-gemma",
      "type": "openai-compatible",
      "model": "Gemma-4-E4B-it-GGUF",
      "base_url": "http://localhost:13305/v1",
      "api_key_env": "MY_API_KEY"
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
      "model": "local-gemma",
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

With this config, `./blorb chat` runs `simple` (the `default_agent`), `./blorb chat --agent quiet` runs the quiet one, and `./blorb chat --agent nope` fails naming the defined agents. Both agents share the `echo` tool; only `simple` also uses the `read` builtin.

An `ollama` model entry looks like this instead:

```json
{
  "name": "local-llama",
  "type": "ollama",
  "model_name": "llama3.1:latest",
  "base_url": "http://localhost:11434",
  "reasoning_effort": "medium"
}
```

### Top-level fields

| Field           | Required | Description                                                                        |
| --------------- | -------- | ------------------------------------------------------------------------------------ |
| `models`        | yes      | The named LLM backend declarations (see below).                                       |
| `agents`        | yes      | The agent definitions (see below).                                                   |
| `default_agent` | no       | Name of the agent commands use when none is given; must name a defined agent.        |
| `tools`         | no       | Top-level tool declarations, shared across agents (see below).                       |
| `logging`       | no       | Wire logging config (see below).                                                     |
| `prefactor`     | no       | Prefactor tracing config (see below).                                                |

### Models

Each model entry declares one named LLM backend. Agents reference it by name in their `model` field; the declarations live once at the top level and are shared.

The `type` discriminator determines which fields are recognized; this is the extension point for future backend types.

Currently supported: `openai-compatible` — any OpenAI-compatible chat completions API (OpenAI, Lemonade, LM Studio, vLLM, ...); and `ollama` — Ollama's native `/api/chat` API, for a local Ollama server or Ollama cloud.

| Field            | Required | Description                                                                                                           |
| ---------------- | -------- | --------------------------------------------------------------------------------------------------------------------- |
| `name`           | yes      | Identifier agents reference; unique within the config, and free-form to the config author.                             |
| `type`           | yes      | Must be `openai-compatible` or `ollama`.                                                                               |
| `model_name`     | yes      | Model name passed to the API (for ollama, the Ollama tag, e.g. `llama3.1:latest`).                                     |
| `base_url`       | yes      | For `openai-compatible`, the API root with `/chat/completions` appended, e.g. `http://localhost:13305/v1`. For `ollama`, the bare Ollama server root with `/api/chat` appended, e.g. `http://localhost:11434`. Must be http or https. |
| `api_key_env`    | no       | Name of the environment variable containing the API key, if the endpoint needs one. Optional for `ollama`: local Ollama needs no key, Ollama cloud does.                 |
| `reasoning_effort` | no     | The thinking effort the backend is asked for: one of `none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`. Empty (the default) means the server default applies. The value is passed through verbatim on both paths — which values a given model accepts is the server's business — except that `none` becomes `think: false` on the ollama path, since Ollama rejects the string `none`. Thinking output surfaces as reasoning, streamed live for both types (`reasoning_content` over SSE for openai-compatible, `thinking` over Ollama's NDJSON for ollama). |

### Agents

Each agent definition carries its own settings and the names of the top-level model and tools it uses:

| Field           | Required | Description                                                                                    |
| --------------- | -------- | ----------------------------------------------------------------------------------------------- |
| `name`          | yes      | Unique within the config; must match `^[a-zA-Z0-9_-]+$`. Shown in the chat banner, registered with Prefactor, and what the `--agent` flag of `blorb chat` takes. |
| `system_prompt` | yes      | The agent's system prompt.                                                                       |
| `model`         | yes      | Name of the top-level model entry this agent talks to.                                          |
| `max_turns`     | yes      | Max model turns per user message; must be at least 1.                                            |
| `tools`         | no       | The *names* of the top-level tools this agent may use. Absent or empty means no tools.           |

Tools are shared vocabulary: they are declared once at the top level, and each agent lists, by name, the ones it may use. An agent listing an unknown tool is a config error, and listing the same tool twice within one agent is an error too. The listed order is the agent's — that is the order the tools are presented to the model. Agent names must match `^[a-zA-Z0-9_-]+$` and be unique within the config.

Models work the same way: an agent's `model` must name a defined top-level model entry.

`default_agent` is optional; when set it must name a defined agent, and when absent `blorb chat` requires an explicit `--agent`.

### Provider

Removed in favor of the top-level `models` list (see [Models](#models)).

### Tools

Each tool has a required `type` field selecting one of three kinds:

**`command` tools** run an executable as a subprocess:

- `name` — identifier the model uses to call it; must match `^[a-zA-Z0-9_-]+$` and be unique within the config
- `description` — tells the model what the tool does and when to use it
- `command` — the argv to run, e.g. `["python", "-u", "my_tool.py"]`
- `args_schema` — optional JSON Schema object describing the arguments; passed through to the API. When omitted, an empty schema is used. (Command tools only; builtins define their own schema.)

When the model calls the tool, Blorb runs the command with the JSON arguments piped to its stdin. The tool's stdout (or stderr, on failure) is returned to the model as the tool result. Each execution has a 30 second timeout; on timeout the whole process group is killed.

**`builtin` tools** are implemented inside Blorb and need no executable. The `builtin` field selects the implementation — currently `read` and `grep` — and the `config` object configures it:

```json
{
  "type": "builtin",
  "name": "read",
  "description": "Look inside project files.",
  "builtin": "read",
  "config": { "base_dir": "./src" }
}
```

Both builtins are file tools sandboxed to a required `base_dir` directory:

- Relative `base_dir` paths in `blorb.json` resolve against the config file's directory (the same rule as `logging.path`), so `"base_dir": "knowledgebase"` anchors next to wherever the config lives.
- The model's path arguments resolve inside `base_dir`, and only that tree is accessible — attempts to escape it, including through symlinks, fail. Symlinks are followed only when they resolve back inside `base_dir`.
- Each execution has the same 30 second timeout as command tools.

The two builtins:

- `read` takes `{"path": ...}` and returns a file's contents (or, for a directory path, the recursive list of files in it). Two optional arguments slice the result by line, with the same 1-based line numbers grep reports: `offset` is the first line to return (default: 1) and `limit` is the maximum number of lines (default: no limit). With a `limit`, reads stream line by line, so slices of files larger than the 1 MiB whole-file cap still work.
- `grep` takes `{"pattern": ...}` and an optional `{"path": ...}` (default: the base directory). It returns matches as `path:line:text`, skipping `.git` directories and binary files. Matching is case-insensitive by default; the model can pass `"case_sensitive": true` in the tool-call arguments for exact-case matching.

**`subagent` tools** delegate to another agent defined in the same config:

```json
{
  "type": "subagent",
  "name": "ask_scholar",
  "description": "Delegate biscuit research questions to the scholar agent.",
  "agent": "scholar"
}
```

- `agent` — required; the name of a defined agent in the same config. Naming an agent that does not exist is a config error, as is any delegation cycle (an agent chain that loops back to itself, including an agent granted a subagent tool targeting itself).
- `args_schema` — optional JSON Schema object. By default the tool takes a single required `prompt` string, which becomes the subagent's user message. With a custom schema, the raw JSON arguments are presented to the subagent as JSON in the user message; the subagent's system prompt is expected to account for that.

Execution semantics:

- The subagent runs to completion — all of its tool calls resolved, exactly like a chat turn — and its concatenated assistant messages become the tool output returned to the calling model.
- Subagent tools are exempt from the 30s per-tool timeout: the run is bounded by the subagent's own `max_turns` (and context cancellation).
- Subagents can themselves use subagent tools (acyclically), so deep delegation is possible.
- The chat interface shows the subagent's activity live — its assistant messages and tool calls — indented and labeled with the subagent's name, so you watch it work.
- Subagent LLM calls are attributed to the subagent in the token footer's per-agent split (and in chat's session totals), so a turn's usage shows how much each agent spent.
- Limitation: nested LLM calls inside subagents are not traced to Prefactor; only the parent agent's spans are recorded.

### Logging

Blorb logs every LLM request/response and tool call/result by default: one timestamped file per wire interaction, written into a per-session subdirectory of a log directory next to `blorb.json`.

```json
{
  "logging": { "path": ".logs", "enabled": true }
}
```

| Field      | Default | Description                                                                        |
| ---------- | ------- | ---------------------------------------------------------------------------------- |
| `path`     | `.logs` | Log directory name, resolved relative to the config file. A single directory name — no separators. |
| `enabled`  | `true`  | Set to `false` to turn logging off.                                                |

Each chat session gets its own subdirectory named `<timestamp>-<uuid>`, so conversations never interleave and sessions sort chronologically. Files are named `<timestamp>-<kind>.txt`, e.g. `20260830T142533-123456789-llm-request.txt`, where the kind is one of `llm-request`, `llm-response`, `tool-request`, or `tool-result`. The nanosecond-precision timestamp prefix means a plain lexical sort of the filenames replays the turn in order.

Log files capture full request/response bodies and headers — including any API key sent to the provider — and the full content of tool calls and results. Keep them out of version control and treat them as sensitive. Log writes are best-effort: a logging failure never fails the agent run.

### Prefactor tracing

Blorb can trace every agent run to [Prefactor](https://prefactor.ai), an agent-activity auditing platform. Add a `prefactor` block to enable it; when present, chat sessions are recorded as Prefactor agent instances, and each user message, assistant message, LLM API call, and tool call becomes a span within the instance. A `blorb run` invocation records one instance with one turn — the same span model as a one-turn chat session.

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

**What maps to what.** A chat session is one Prefactor agent instance, registered on startup and finished when the session ends: `complete` on a normal exit (`exit`/quit, EOF, or Ctrl-C at the prompt — quitting the chat is a finished chat), `failed` on any error exit. Within the session, each user message opens a `blorb:agent_turn` span that stays open while the agent works, with these child spans:

- `blorb:user_message` — the user's input.
- `blorb:llm` — one LLM API call, with the model, full message history, response content, reasoning, tool calls, and token usage.
- `blorb:tool:<name>` — one tool execution, with the JSON arguments and the tool output (and whether it failed).
- `blorb:assistant_message` — a completed assistant reply (whole-message mode only; streamed replies are covered by the `blorb:llm` span).

The activity schema is derived from your config — including each tool's `args_schema` — and registered with Prefactor on startup, so spans are well-typed in the platform.

**Hard dependency.** When tracing is configured, a Prefactor failure fails the run rather than continuing untraced: a startup failure refuses to start the session, and a failure mid-turn ends the session with the error reported.

**Termination.** Stopping a run from the Prefactor web app sends a terminate signal, which Blorb honours cooperatively: the in-flight turn fails, the session ends, and the termination reason is printed. The platform marks terminated instances itself, so the instance isn't finished a second time locally.

`PREFACTOR_API_TOKEN` (or the configured `api_token_env`) must be exported before running:

```sh
export PREFACTOR_API_TOKEN="pf_..."
blorb chat
```

## Examples

See [examples/simple](examples/simple) for a two-agent config sharing one tool set: `echo` and `current_time` command tools and `read`/`grep` builtins (pointed at the example's `knowledgebase/` directory), with `scholar` granted only the knowledgebase builtins, including notes on pointing the model at different OpenAI-compatible servers.

See [examples/prefactor-tracing](examples/prefactor-tracing) for a single-agent variant with just the `read`/`grep` builtins (sharing simple's `knowledgebase/`) and Prefactor tracing enabled.

See [examples/ollama-cloud](examples/ollama-cloud) for a single-agent variant pointed at Ollama cloud (native `ollama` model type, API key via `api_key_env`, `reasoning_effort` on a thinking model).

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
- `internal/llm/ollama` — native Ollama client (`/api/chat`), non-streaming and NDJSON streaming
- `internal/logging` — wire logging for LLM and tool interactions
- `internal/prefactor` — Prefactor tracing client and tracer

## License

[MIT](LICENSE)