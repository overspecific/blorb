# Configuration

A `blorb.json` defines the shared provider, model, and tool vocabularies and the agents that use them. This is the full field reference - see the [README](../README.md) for a getting-started overview:

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

With this config, `./blorb chat` runs `simple` (the `default_agent`), `./blorb chat --agent quiet` runs the quiet one, and `./blorb chat --agent nope` fails naming the defined agents. Both agents share the `echo` tool; only `simple` also uses the `read` builtin. Both models share one provider - one server declaration, two model entries.

An `ollama` provider, with a model on it, looks like this instead:

```json
{
  "providers": [
    {
      "name": "ollama-cloud",
      "type": "ollama",
      "base_url": "https://ollama.com",
      "api_key_env": "OLLAMA_API_KEY"
    }
  ],
  "models": [
    {
      "name": "local-llama",
      "provider": "ollama-cloud",
      "model_name": "llama3.1:latest",
      "reasoning_effort": "medium"
    }
  ]
}
```

## Top-level fields

| Field           | Required | Description                                                                        |
| --------------- | -------- | ------------------------------------------------------------------------------------ |
| `providers`     | yes      | The named LLM server connections, shared by models (see below).                       |
| `models`        | yes      | The named LLM backend declarations (see below).                                       |
| `agents`        | yes      | The agent definitions (see below).                                                   |
| `default_agent` | no       | Name of the agent commands use when none is given; must name a defined agent.        |
| `tools`         | no       | Top-level tool declarations, shared across agents (see below).                       |
| `toolsets`      | no       | Named groups of tools, granted to agents by name (see below).                        |
| `deciders`      | no       | Named decision-model evaluations with fixed typed questions (see below).             |
| `logging`       | no       | Wire logging config (see below).                                                     |
| `prefactor`     | no       | Prefactor tracing config (see below).                                                |

## Providers

Each provider entry declares one named LLM server connection: the facts every model on that server shares. Models reference it by name in their `provider` field; the declarations live once at the top level and are shared - several models can point at one server.

The `type` discriminator determines which fields are recognized; this is the extension point for future connection types.

Currently supported: `openai-compatible` - any OpenAI-compatible chat completions API (OpenAI, Lemonade, LM Studio, vLLM, ...); and `ollama` - Ollama's native `/api/chat` API, for a local Ollama server or Ollama cloud.

| Field              | Required | Description                                                                                                           |
| ------------------ | -------- | --------------------------------------------------------------------------------------------------------------------- |
| `name`             | yes      | Identifier models reference; unique within the config, must match `^[a-zA-Z0-9_-]+$`.                                  |
| `type`             | yes      | Must be `openai-compatible` or `ollama`.                                                                               |
| `base_url`         | yes      | For `openai-compatible`, the API root with `/chat/completions` appended, e.g. `http://localhost:13305/v1`. For `ollama`, the bare Ollama server root with `/api/chat` appended, e.g. `http://localhost:11434`. Must be http or https with a host. |
| `api_key_env`      | no       | Name of the environment variable containing the API key, if the endpoint needs one. Optional for `ollama`: local Ollama needs no key, Ollama cloud does.                 |
| `temperature`      | no       | Server-wide generation defaults (all seven below): empty means the server default applies. See the sampling table.     |
| `top_p`            | no       | See the sampling table below.                                                                                        |
| `seed`             | no       | See the sampling table below.                                                                                        |
| `stop`             | no       | See the sampling table below.                                                                                        |
| `max_tokens`       | no       | See the sampling table below.                                                                                        |
| `frequency_penalty`| no       | See the sampling table below.                                                                                        |
| `presence_penalty` | no       | See the sampling table below.                                                                                        |

**Sampling fields** - the seven optional generation knobs are server-wide defaults applied to every model on the provider (every request the engine sends, on the initial call and after every tool round). Empty/absent means the server default applies; the numeric fields are pointers, so an explicit `"temperature": 0` is honored, not ignored.

| Field               | Range        | Wire mapping                                                                                     |
| ------------------- | ------------ | ------------------------------------------------------------------------------------------------ |
| `temperature`       | >= 0         | openai-compatible: top-level `temperature`. ollama: `options.temperature`.                        |
| `top_p`             | (0, 1]       | openai-compatible: top-level `top_p`. ollama: `options.top_p`.                                    |
| `seed`              | >= 0         | openai-compatible: top-level `seed`. ollama: `options.seed`.                                      |
| `stop`              | non-empty entries | openai-compatible: top-level `stop`. ollama: `options.stop`.                                 |
| `max_tokens`        | >= 1         | openai-compatible: top-level `max_tokens` (not `max_completion_tokens`, for llama-server/vLLM-class compatibility). ollama: `options.num_predict`. |
| `frequency_penalty` | [-2, 2]      | openai-compatible: top-level `frequency_penalty`. ollama: `options.frequency_penalty`.            |
| `presence_penalty`  | [-2, 2]      | openai-compatible: top-level `presence_penalty`. ollama: `options.presence_penalty`.              |

## Models

Each model entry declares one named LLM backend: a provider connection (by name) plus the model-level facts that distinguish this model within it. Agents reference the model by name in their `model` field; the declarations live once at the top level and are shared.

| Field            | Required | Description                                                                                                           |
| ---------------- | -------- | --------------------------------------------------------------------------------------------------------------------- |
| `name`           | yes      | Identifier agents reference; unique within the config, and free-form to the config author.                             |
| `provider`       | yes      | Name of a defined top-level provider entry carrying the connection.                                                    |
| `model_name`     | yes for `llm`, no for `decision` | Model name passed to the API (for ollama, the Ollama tag, e.g. `llama3.1:latest`). Optional for decision models: when omitted the wire request omits the model field and the server applies its default. |
| `model_type`     | no       | `llm` (the default) for a chat completions model, or `decision` for a decision model (see [Deciders](#deciders)). A decision model requires an `openai-compatible` provider and rejects every llm-only knob. |
| `reasoning_effort` | no     | The thinking effort the backend is asked for: one of `none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`. Empty (the default) means the server default applies. The value is passed through verbatim on both paths - which values a given model accepts is the server's business - except that `none` becomes `think: false` on the ollama path, since Ollama rejects the string `none`. Thinking output surfaces as reasoning, streamed live for both types (`reasoning_content` over SSE for openai-compatible, `thinking` over Ollama's NDJSON for ollama). Not valid for decision models. |
| `format`         | no       | **Ollama-only** (a model on an `openai-compatible` provider is rejected): Ollama's structured-output setting, either the JSON string `"json"` or a JSON schema object. Anything else - arrays, scalars, invalid JSON - is a config error. Not valid for decision models. |
| `keep_alive`     | no       | **Ollama-only** (rejected on `openai-compatible`): how long the model stays loaded after the request, passed through verbatim (e.g. `"5m"`); which duration forms a given server accepts is the server's business. Not valid for decision models. |
| `tool_choice`    | no       | How the model is steered around tools: `auto` (the default), `none`, `required`, or `force`. See below. Not valid for decision models. |
| `forced_tool`    | only with `tool_choice: "force"` | The tool the model must call in force mode; an error anywhere else. Must match `^[a-zA-Z0-9_-]+$`. Not valid for decision models. |
| `logprobs`       | no       | Ask the server for per-token log probabilities of the response's content tokens. Models on both provider types. Not valid for decision models. |
| `top_logprobs`   | no       | How many top alternative tokens to report per position, in [0, 20]; settable only when `logprobs` is true - an explicit `"top_logprobs": 0` without `logprobs` is a config error. Not valid for decision models. |

**Decision models.** A `model_type` of `"decision"` declares a decision model: a System One model (Jev is the first example) that evaluates a state against typed questions and returns typed answers with probabilities instead of generated text. A decision model:

- requires an `openai-compatible` provider - the connection is a plain `base_url` plus bearer key from `api_key_env`, and the decision client appends `/systemone` to the provider's `base_url` (the openai client appends `/chat/completions`);
- may leave `model_name` empty, in which case the wire request omits the model field and the server applies its deployment default;
- rejects every llm-only knob (`reasoning_effort`, `format`, `keep_alive`, `tool_choice`/`forced_tool`, `logprobs`/`top_logprobs`), since none applies to a decision call;
- can be referenced only by a decider, never by an agent.

A `base_url` ending in a version segment works for the vendors that share this protocol: TypeSafe (`https://api.typesafe.ai/v1`), OpenRouter (`https://openrouter.ai/api/v1`), jevmodel.org (`https://jevmodel.org/v1`), apimodels, and Venice.

**tool_choice.** The four modes:

- `auto` (the default, and the absent field's meaning): the model decides freely.
- `none`: tool calls are forbidden for the turn, while the tool definitions stay advertised. The request prefix stays byte-identical to the conversation so far, so provider prompt caches (OpenAI prefix caching, vLLM, llama.cpp warm KV) keep hitting - omitting the tool definitions would bust the cache. It forbids calls for a turn without paying the context reprocessing cost.
- `required`: the server is asked to force some tool call; which one is the server's choice, and blorb does not police the result.
- `force`: the model must call `forced_tool`. The engine checks at construction that the forced tool is among the agent's granted tools, and if the model replies with text instead of the call, the turn fails with a clear error naming the tool and what came back instead. Once the forced tool has run, follow-up requests drop the `tool_choice` field so the model is free to produce the final answer.

On the wire, `none` and `required` serialize as the bare string and `force` as the OpenAI object shape `{"type":"function","function":{"name":...}}` - which Ollama accepts identically. `auto` omits the field.

**logprobs.** A non-streaming feature: with `logprobs: true` the server reports one entry per content token, decoded into the neutral response and surfaced through `blorb run` (the `--logprobs` flag enables them for a run even when the config leaves them off, alongside the config setting). In `run --format ndjson`, the `text` and `done` events carry a `logprobs` array. In `run --format chat` and `run --format plain --logprobs`, one line prints after the response body per token - the token, its logprob, and the top alternative when present:

```text
Hi there
  "Hi" logprob=-0.2500 (top: "Hi" -0.2500)
  " there" logprob=-0.1000
```

Streamed responses do not decode logprob data, so `blorb run --logprobs` requires `--no-stream`: a run with the flag and streaming on fails before any LLM call. See the [output formats reference](formats.md) for the `run` flags.

A response that generated content tokens must carry the logprobs the request asked for; if a server accepts the request and returns none - Ollama Cloud silently drops the flags (ollama/ollama#13638), on both its native and OpenAI-compatible endpoints - the run fails with a "server did not return logprobs" error naming the body. Use a local Ollama server or an OpenAI-compatible server that implements the field.

## Agents

Each agent definition carries its own settings and the names of the top-level model and tools it uses:

| Field           | Required | Description                                                                                    |
| --------------- | -------- | ----------------------------------------------------------------------------------------------- |
| `name`          | yes      | Unique within the config; must match `^[a-zA-Z0-9_-]+$`. Shown in the chat banner, registered with Prefactor, and what the `--agent` flag of `blorb chat` takes. |
| `system_prompt` | yes      | The agent's system prompt.                                                                       |
| `model`         | yes      | Name of the top-level model entry this agent talks to.                                          |
| `max_turns`     | yes      | Max model turns per user message; must be at least 1.                                            |
| `tools`         | no       | The names of the top-level tools, toolsets, and toolset members this agent may use. Absent or empty means no tools. |
| `judges`        | no       | The judge entries for this agent's runs: each names an agent that receives the run's transcript as its first user message, plus an optional `when` selecting the moment it runs (default `"end"`). See the [Judges](#judges) section below. |

Tools are shared vocabulary: they are declared once (as top-level `tools` or inside a `toolsets` group), and each agent lists, by name, the ones it may use. An agent's `tools` list may name a top-level tool, a whole toolset, or one toolset member by its granted name. Naming something that does not exist is a config error, and granting the same tool twice - directly or through a toolset - is an error too. The listed order is the agent's: that is the order the tools are presented to the model, with a toolset's members expanding at the toolset's position. See [Toolsets](#toolsets) below. Agent names must match `^[a-zA-Z0-9_-]+$` and be unique within the config.

Models work the same way: an agent's `model` must name a defined top-level model entry, and that model must be an `llm` model - naming a decision model is a config error (`agents require an llm model`). Decision models are reachable only through deciders.

`default_agent` is optional; when set it must name a defined agent, and when absent `blorb chat` requires an explicit `--agent`.

### Voice sessions

An agent with a `voice` block can run as a spoken conversation through the `blorb voice` command. The block's presence enables the command for that agent, the same way a `band` block enables `blorb band`:

```json
"voice": {
  "api_key_env": "ASSEMBLYAI_API_KEY",
  "greeting": "Hi! I am ready when you are.",
  "voice": "james"
}
```

| Field            | Required | Description                                                                                          |
| ---------------- | -------- | ------------------------------------------------------------------------------------------------------ |
| `api_key_env`    | yes      | Name of the environment variable holding the AssemblyAI API key.                                     |
| `greeting`       | no       | What the agent says on connect, spoken verbatim without the model.                                   |
| `voice`          | no       | The AssemblyAI voice id. Empty means the API's default voice.                                          |
| `volume`         | no       | Playback volume from 0 to 100. Empty means the voice's native level; an explicit 0 is silence.        |
| `input_command`  | no       | The microphone capture command as an argument array. Empty defaults to `["arecord", "-q", "-f", "cd", "-r", "24000", "-c", "1"]`, which emits 24 kHz 16-bit mono PCM on stdout. |
| `output_command` | no       | The speaker playback command as an argument array. Empty defaults to `["aplay", "-q", "-r", "24000", "-f", "s16_le", "-c", "1"]`, which plays the same format from stdin. |
| `ws_url`         | no       | The Voice Agent WebSocket endpoint. Empty defaults to `wss://agents.assemblyai.com/v1/ws`; set it for a test server or a regional host. |
| `echo_gate`      | no       | When true, mutes the microphone while the agent is speaking, so a speaker near the microphone cannot feed the agent's own voice back to it. The trade-off is that talking over the agent (barge-in) is disabled while it speaks. Leave it off with headphones. Default false. |

Validation requires `api_key_env`, rejects a `volume` outside 0 to 100, and requires `ws_url` to be ws/wss with a host when set.

A voice agent's spoken turns do not use its `model`. The Voice Agent API runs the conversation loop server-side - speech recognition, AssemblyAI's managed model, and speech synthesis - so blorb builds no LLM client for the spoken turns. The agent's `tools` still run locally, through the same registry the other commands use: `command` and `builtin` tools work as in `chat`, and `subagent` tools run a normal agent turn through the target agent's configured model on this machine, rendering that activity in the transcript. Because a voice session builds no decision-model path, `decider` tools cannot run, and `blorb voice` fails at startup if the agent is granted one.

A laptop microphone and speaker in the same room feed the agent's speech back to it: it hears itself, transcribes that as the user talking, and interrupts itself. Two ways out. `echo_gate: true` in the `voice` block closes the microphone while the agent's audio is still playing, at the cost of barge-in; it is the right choice for a laptop speaker and microphone. Headphones leave the microphone open and keep barge-in working, so the gate is not needed.

## Deciders

A decider is a named decision-model evaluation: it names a decision model and fixes the typed questions every call asks, so only the state varies per call. A decider produces no text - you send it a state and typed questions, and it returns typed answers with probabilities and confidence values. It is reachable only through a `decider` tool and the `blorb decide` command; deciders are not agents and have no tools, turns, or judges of their own.

```json
{
  "deciders": [
    {
      "name": "triage",
      "model": "jev",
      "questions": {
        "department": {
          "type": "choice",
          "instructions": "Which team should handle this ticket?",
          "criteria": {
            "billing": "Payments, invoices, and refunds",
            "technical": "Bugs, outages, integrations",
            "sales": "New purchases and upgrades"
          }
        },
        "urgency": {
          "type": "score",
          "instructions": "How urgent is the request?",
          "criteria": ["No time constraint", "Can wait this week", "Needs attention within a day"]
        },
        "refund_requested": {
          "type": "noul",
          "instructions": "Does the customer explicitly request a refund?"
        }
      }
    }
  ]
}
```

| Field       | Required | Description                                                                                          |
| ----------- | -------- | ---------------------------------------------------------------------------------------------------- |
| `name`      | yes      | Identifier the `decider` tool and `blorb decide --decider` reference; unique within the config, must match `^[a-zA-Z0-9_-]+$`. |
| `model`     | yes      | Name of a defined `decision` model in the same config. Naming a non-decision model is a config error. |
| `questions` | yes      | A map of question name to typed question; at least one entry.                                         |

Each question has a `type` and `instructions`, plus type-specific `criteria`:

| Field          | Required | Description                                                                                          |
| -------------- | -------- | ---------------------------------------------------------------------------------------------------- |
| `type`         | yes      | One of `choice`, `noul`, or `score`.                                                                  |
| `instructions` | yes      | The question itself: a string for short questions, or a JSON object or array putting the question in one field and the data that guides it in the others. Passed to the server verbatim. |
| `criteria`     | by type  | Defines the answer space. See below.                                                                  |

The three question types:

- **`choice`** selects one of a set of named options. `criteria` is required and is a JSON object of option name to description, with at least two entries:

  ```json
  {
    "type": "choice",
    "instructions": "Which team should handle this?",
    "criteria": { "billing": "Payments and refunds", "support": "Technical help" }
  }
  ```

- **`score`** rates the state against an ordered set of levels. `criteria` is required and is a JSON array of two to ten descriptions, ordered from lowest to highest:

  ```json
  {
    "type": "score",
    "instructions": "How urgent is this?",
    "criteria": ["Can wait", "Needs attention today", "Immediate outage"]
  }
  ```

- **`noul`** asks a calibration question whose answer is the probability of yes. `criteria` is optional; when present it is a JSON object of `true` and `false` descriptions:

  ```json
  {
    "type": "noul",
    "instructions": "Does the customer request a refund?",
    "criteria": { "true": "Explicitly asks for a refund", "false": "Does not" }
  }
  ```

Question names must be identifiers matching `^[a-zA-Z0-9_]+$` and at most 64 characters; they stay in your code and are not sent to the model.

Call semantics: every question is evaluated in parallel against the same state in one request; no question sees another's answer. Which state forms the server accepts - a string, object, or array - and vendor limits such as question counts and state size are the server's to enforce.

The answers come back as an object keyed by question name, one answer per question, each carrying its `type`, the selected value, and the probabilities and confidence the server reported. The `choice` answer carries `choice`, `probabilities`, and `confidence`; `score` carries `score`, `probabilities`, `confidence`, and a `legend`; `noul` carries `noul` (the probability of yes) and no confidence.

## Tools

Each tool has a required `type` field selecting one of four kinds:

The top-level `tools` list takes only these four types. A `"type": "toolset"` entry is not valid there: it is a reference to another toolset, allowed only inside a toolset's `tools` list. See [Toolsets](#toolsets).

**`command` tools** run an executable as a subprocess:

- `name` - identifier the model uses to call it; must match `^[a-zA-Z0-9_-]+$` and be unique within the config
- `description` - tells the model what the tool does and when to use it
- `command` - the argv to run, e.g. `["python", "-u", "my_tool.py"]`
- `args_schema` - optional JSON Schema object describing the arguments; passed through to the API. When omitted, an empty schema is used. (Command tools only; builtins define their own schema.)

When the model calls the tool, Blorb runs the command with the JSON arguments piped to its stdin. The tool's stdout (or stderr, on failure) is returned to the model as the tool result. Each execution has a 30 second timeout; on timeout the whole process group is killed.

**`builtin` tools** are implemented inside Blorb and need no executable. The `builtin` field selects the implementation - currently `read` and `grep` - and the `config` object configures it:

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
- The model's path arguments resolve inside `base_dir`, and only that tree is accessible - attempts to escape it, including through symlinks, fail. Symlinks are followed only when they resolve back inside `base_dir`.
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

- `agent` - required; the name of a defined agent in the same config. Naming an agent that does not exist is a config error, as is any delegation cycle (an agent chain that loops back to itself, including an agent granted a subagent tool targeting itself).
- `args_schema` - optional JSON Schema object. By default the tool takes a single required `prompt` string, which becomes the subagent's user message. With a custom schema, the raw JSON arguments are presented to the subagent as JSON in the user message; the subagent's system prompt is expected to account for that.

Execution semantics:

- The subagent runs to completion - all of its tool calls resolved, exactly like a chat turn - and its concatenated assistant messages become the tool output returned to the calling model.
- Subagent tools are exempt from the 30s per-tool timeout: the run is bounded by the subagent's own `max_turns` (and context cancellation).
- Subagents can themselves use subagent tools (acyclically), so deep delegation is possible.
- The chat interface shows the subagent's activity live - its assistant messages and tool calls - indented and labeled with the subagent's name, so you watch it work.
- Subagent LLM calls are attributed to the subagent with their own footer line (and in chat's session totals), so a turn's usage shows how much each agent spent.
- Limitation: nested LLM calls inside subagents are not traced to Prefactor; only the parent agent's spans are recorded.

**`decider` tools** evaluate a named decider defined in the same config:

```json
{
  "type": "decider",
  "name": "triage_ticket",
  "description": "Decide the priority and escalation of a support ticket.",
  "decider": "triage"
}
```

- `decider` - required; the name of a defined decider in the same config. Naming a decider that does not exist is a config error. A decider holds no tools and nothing recurses through it, so no cycle check is needed.
- `args_schema` - optional JSON Schema object. By default the tool takes a single required `state` string, the situation to evaluate. With a custom schema, the raw JSON arguments are the state (the decider's questions are fixed in the decider config, so the tool call supplies only the state).

Execution semantics:

- The tool makes one decision API call with the decider's configured questions and returns the typed answers as a JSON object, keyed by question name. Each answer carries its `type`, the selected value (`choice` / `score` / `noul`), and the probabilities and confidence the server reported.
- The call is one HTTP request under the registry's 30 second per-tool timeout (decision calls are sub-second; a hung server fails like any other tool).
- A failed call - the server rejected the request - comes back as a normal failed tool result the parent model can read and react to, not as a Go error.
- In chat (and the run and band frontends), the decision prints as a labeled, indented block before the parent's own tool result, so you watch the decision land the same way you watch a subagent work:

  ```text
  >>> Tool: triage_ticket
  {"state":"..."}

  [triage] >>> Decision:
    {"department":{"type":"choice","choice":"billing",...},"refund_requested":{"type":"noul","noul":0.95}}
  >>> Result: Tool: triage_ticket
  ```

- Decision calls are accounted in usage output as calls by the decider's name (the decider is not an agent, but its token usage is reported and attributed).
- Limitation: decision calls are not traced to Prefactor, the same limitation as nested subagent calls.

## Toolsets

A toolset is a named group of tools declared once at the top level and granted to an agent by naming the toolset in the agent's `tools` list. It is useful when several agents share the same bundle of tools, or when a tool needs the same configuration repeated across agents. Two kinds exist, selected by the toolset's `type` field.

**Simple toolsets** (the default when `type` is absent) hold inline tool declarations plus references to other toolsets:

```json
{
  "toolsets": [
    {
      "name": "kb",
      "tools": [
        {
          "type": "command",
          "name": "read",
          "description": "Read a knowledgebase file.",
          "command": ["cat"]
        },
        {
          "type": "builtin",
          "name": "grep",
          "description": "Search the knowledgebase.",
          "builtin": "grep",
          "config": { "base_dir": "knowledgebase" }
        }
      ]
    },
    {
      "name": "dev",
      "tools": [{ "type": "toolset", "toolset": "kb" }]
    }
  ]
}
```

A simple toolset entry is an ordinary `command`, `builtin`, or `subagent` declaration, or a `"type": "toolset"` reference to another top-level toolset. Referencing a toolset nests it; prefixes compose along the path.

**Builtin toolsets** wrap a bundle shipped inside Blorb. Instead of per-member declarations, they name the `builtin` bundle and carry one shared `config` object. Currently the only bundle is `file`, which bundles the `read` and `grep` builtins and takes the same `base_dir` setting they do:

```json
{
  "toolsets": [
    {
      "name": "kb",
      "type": "builtin",
      "builtin": "file",
      "config": { "base_dir": "knowledgebase" }
    }
  ]
}
```

The settings are written once; each member parses them independently. Members carry the builtins' own descriptions, so there are none to write.

### Granting and naming

Name a toolset in an agent's `tools` list and every member is granted:

```json
{
  "name": "scholar",
  "system_prompt": "...",
  "model": "small",
  "tools": ["kb", "search"]
}
```

Name a single member by its granted name to pull just that one:

```json
{
  "tools": ["kb-read", "search"]
}
```

A granted tool is renamed with its toolset as a prefix, joined by a single hyphen: toolset `kb` containing `read` grants `kb-read`, and a builtin toolset `kb` of kind `file` grants `kb-read` and `kb-grep`. Nested toolsets compose: toolset `dev` referring to toolset `clock`, which contains `read`, grants `dev-clock-read`. Granting `dev` and `clock` side by side is fine and yields `dev-clock-read` and `clock-read` - distinct names.

The hyphen is the separator between the toolset path and the leaf name. Leaf names are still the config author's: underscores inside a name are fine and read distinctly from the separating hyphens.

### Validation

- A toolset reference naming an unknown toolset is an error, as is a reference cycle (including a toolset referring to itself).
- A toolset with no `tools` (or an empty list) is valid and grants nothing.
- Any redundancy in one agent's `tools` list is an error, whether an exact repeat or a toolset granted alongside one of its own members.
- Every name an agent can write must be unique across the config: top-level tool names, toolset names, and granted member names share one reference space. This also catches composition collisions, for example a toolset `a-b` containing `c` and a toolset `a` referring to a toolset `b` containing `c` both grant `a-b-c`.

### A note on function names

A granted name goes to the LLM API verbatim as the function name. OpenAI and OpenAI-compatible servers that copy its validation accept only `^[a-zA-Z0-9_-]{1,64}$`, so a composed name inherits that 64-character cap. Blorb does not enforce the cap (it is OpenAI's limit, not a universal one), but keep composed names short if you target such a server.

## Judges

A judge is an ordinary agent in the same config, attached to another agent through that agent's `judges` field. When the judged agent's run finishes, each judge receives the run's transcript as its first user message, runs to completion like any agent (its own tool calls resolved), and its final reply - the judgement - prints to the screen. Nothing else consumes the judgement.

```json
{
  "agents": [
    {
      "name": "main",
      "system_prompt": "You are helpful.",
      "model": "small",
      "judges": [{ "agent": "reviewer" }]
    },
    {
      "name": "reviewer",
      "system_prompt": "You are a reviewer. You are given the transcript of another agent's run. Judge whether it answered the user's question well and report your verdict.",
      "model": "small"
    }
  ]
}
```

The `judges` field is a list of judge entries, one object per judge:

| Field   | Required | Description                                                                 |
| ------- | -------- | ---------------------------------------------------------------------------- |
| `agent` | yes      | Name of a defined agent in the same config. Naming an unknown agent is a config error, as is naming the same judge twice on one agent. |
| `when`  | no       | When the judge runs. `"end"` is the default and currently the only supported value; other timings may be supported in future releases.   |

### When judges run

With `when: "end"` (the default), a judge runs after the judged agent finishes:

- `blorb run`: after the single turn, before the usage footer.
- `blorb chat`: once at session end, on the full session transcript, before the session usage totals.

Judges run even when the judged turn failed - max turns, provider error, API error. The transcript then shows whatever survived the failure, so the judge can review that too.

### What a judge receives

A judge's own `system_prompt` carries the judging instructions. Blorb supplies the rest: the judge's first user message is the judged run's entire transcript - every user, assistant, and tool message in order, including the agent's reasoning, its tool calls with their arguments, and every tool result. The transcript renders as labeled blocks, one per message, with every body line indented two spaces:

```text
[user]
  What is a jammie dodger?
[tool call]
  name: read
  arguments: {"path": "digestives.md"}
[tool result]
  id: abc123
  A jammie dodger is a British biscuit ...
[assistant]
  A jammie dodger is a domed biscuit ...
```

The labels are `[user]`, `[assistant]`, `[assistant thinking]` (the agent's reasoning), `[tool call]`, and `[tool result]`. Blorb explains this format to the judge in the same message, so a judge's system prompt only needs the judging instructions - no format description required.

The transcript arrives inside a fenced block: an unpredictable id is generated per judge run, the opening and closing lines both carry it, and the message tells the judge to treat everything inside the fence as data to review. Content produced during the judged run - tool output, file contents - cannot predict the id, so it cannot forge the transcript's structure.

### Judges see untrusted content

The transcript contains everything the judged agent read and produced, including file contents and tool output - content an attacker may control. The fence and the indentation stop that content from forging the transcript's structure, but not from giving the judge instructions: a judge can be misled by what it reads. Judge output is informational only - it is printed and never fed back into the agent or the run - which bounds the damage but does not make judges trustworthy on hostile input.

### What a judge can do

A judge is a full agent: it can use tools like any other, including subagents. A judge can re-check the judged agent's work with the same knowledgebase tools rather than trusting the transcript.

### Judge chains

A judge can itself have judges. When the judge's run completes, its own judges run by the same rule, against its transcript - the judged transcript embedded inside it as indented content, inside a fresh fence with its own id. Cycles are a config error: a judge chain that loops back to itself fails validation, so the recursion is always finite.

### Usage and limitations

Judge LLM calls are attributed to the judge in the usage footer and the chat session totals, and they appear in the ndjson `done` event's per-agent split. In `blorb run` and at chat session end, the judge's thinking and tool activity stream as it works, labeled `[<judge>]` and indented like subagent activity, and the judgement then prints as a `>>> Judge: <name>` block per judge, in the order the `judges` field lists them. In `--format ndjson`, judge events stream as `judge_*` types before the terminal event (see [formats](formats.md#ndjson)).

Limitations:

- Judge LLM calls are not traced to Prefactor, the same limitation as nested subagent calls.
- In chat, a judge delegating to a subagent shows the judge's tool activity but not the subagent's own live text.

## Logging

Blorb logs every LLM request/response and tool call/result by default: one timestamped file per wire interaction, written into a per-session subdirectory of a log directory next to `blorb.json`.

```json
{
  "logging": { "path": ".logs", "enabled": true }
}
```

| Field      | Default | Description                                                                        |
| ---------- | ------- | ---------------------------------------------------------------------------------- |
| `path`     | `.logs` | Log directory name, resolved relative to the config file. A single directory name - no separators. |
| `enabled`  | `true`  | Set to `false` to turn logging off.                                                |

Each chat session gets its own subdirectory named `<timestamp>-<uuid>`, so conversations never interleave and sessions sort chronologically. Files are named `<timestamp>-<kind>.txt`, e.g. `20260830T142533-123456789-llm-request.txt`, where the kind is one of `llm-request`, `llm-response`, `tool-request`, or `tool-result`. The nanosecond-precision timestamp prefix means a plain lexical sort of the filenames replays the turn in order.

Log files capture full request/response bodies and headers - including any API key sent to the provider - and the full content of tool calls and results. Keep them out of version control and treat them as sensitive. Log writes are best-effort: a logging failure never fails the agent run.

## Prefactor tracing

Blorb can trace every agent run to [Prefactor](https://prefactor.ai), an agent-activity auditing platform. Add a `prefactor` block to enable it; when present, chat sessions are recorded as Prefactor agent instances, and each user message, assistant message, LLM API call, and tool call becomes a span within the instance. A `blorb run` invocation records one instance with one turn - the same span model as a one-turn chat session.

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

**What maps to what.** A chat session is one Prefactor agent instance, registered on startup and finished when the session ends: `complete` on a normal exit (`exit`/quit, EOF, or Ctrl-C at the prompt - quitting the chat is a finished chat), `failed` on any error exit. Within the session, each user message opens a `blorb:agent_turn` span that stays open while the agent works, with these child spans:

- `blorb:user_message` - the user's input.
- `blorb:llm` - one LLM API call, with the model, full message history, response content, reasoning, tool calls, and token usage.
- `blorb:tool:<name>` - one tool execution, with the JSON arguments and the tool output (and whether it failed).
- `blorb:assistant_message` - a completed assistant reply (whole-message mode only; streamed replies are covered by the `blorb:llm` span).

The activity schema is derived from your config - including each tool's `args_schema` - and registered with Prefactor on startup, so spans are well-typed in the platform.

**Hard dependency.** When tracing is configured, a Prefactor failure fails the run rather than continuing untraced: a startup failure refuses to start the session, and a failure mid-turn ends the session with the error reported.

**Termination.** Stopping a run from the Prefactor web app sends a terminate signal, which Blorb honours cooperatively: the in-flight turn fails, the session ends, and the termination reason is printed. The platform marks terminated instances itself, so the instance isn't finished a second time locally.

`PREFACTOR_API_TOKEN` (or the configured `api_token_env`) must be exported before running:

```sh
export PREFACTOR_API_TOKEN="pf_..."
blorb chat
```

## Listing installed models

`blorb models` enumerates, per provider in the config, what that provider's server has installed - a quick check that every configured `model_name` is actually served:

```sh
./blorb models --config examples/simple/blorb.json
```

```text
provider local (openai-compatible, http://localhost:13305/v1)
  Gemma-4-E4B-it-GGUF (used by small)
  Qwen3-32B-GGUF
  gemma4:31b
  mxbai-embed-large
provider local-llama (ollama, http://localhost:11434)
  llama3.1:latest (used by local-llama)
  mistral:7b (NOT INSTALLED; configured as local-mistral)
```

Each provider prints one block with its name, type, and base_url, followed by the server's installed models (from `GET /models` on openai-compatible servers, `/api/tags` on Ollama). A model the config uses is marked `used by <config model name>`, naming the model entry (several entries may share one `model_name`); unconfigured server models print plainly. A configured `model_name` the server does not have prints as `NOT INSTALLED`, naming the config entry that points at it - that is the typo you are looking for. API keys resolve through the same path as a real session, so a missing key or a listing failure is reported per provider and does not stop the others.

Exit codes: `0` when every provider that could be listed shows all its configured models installed; `1` when any listing failed or any model is missing - typo detection is the point.
