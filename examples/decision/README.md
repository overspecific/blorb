# Decision-model router over the biscuit knowledgebase

A config that puts a decision model to work on a job a generative model is bad at: picking a route through a body of text before the agent reads it. It reuses the biscuit knowledgebase from the [simple](../simple) example and adds a `scholar` agent that answers biscuit questions, and a `route_question` decider - a fixed set of typed questions for a System One model (Jev) - that the scholar calls once per question to decide which knowledgebase file to read and whether it needs to read at all.

The chat model is the same local server the [simple](../simple) example uses. The decision model is hosted, because it speaks a different protocol.

## Setup

The chat agent talks to a local OpenAI-compatible server. The model config points at a local [Lemonade](https://lemonade-server.com) server (`http://localhost:13305/v1`) with the `Gemma-4-E4B-it-GGUF` model; adjust `base_url` and `model_name` in the top-level `models` list in `blorb.json` to match whatever OpenAI-compatible endpoint you want to use (OpenAI, Lemonade, LM Studio, vLLM, Ollama, ...). If your endpoint needs an API key, add an `api_key_env` entry naming the environment variable that holds it.

The decision model is a System One model (Jev is the first example): it evaluates a state against typed questions and returns typed answers with probabilities instead of generated text. Point the `typesafe` provider's `base_url` at a vendor that speaks the protocol - TypeSafe itself (`https://api.typesafe.ai/v1`), OpenRouter (`https://openrouter.ai/api/v1`), jevmodel.org (`https://jevmodel.org/v1`), apimodels, Venice - and export the key its `api_key_env` names:

```sh
export TYPESAFE_API_KEY="..."
```

A decision model sits on an `openai-compatible` provider and is marked with `"model_type": "decision"`. Its `model_name` is optional: when omitted, the request leaves the model field out and the server applies its deployment default. It does not accept the chat-model knobs (`reasoning_effort`, `tool_choice`, `logprobs`, and the Ollama-only settings). A decision model cannot be an agent's model; only a decider references it.

## The agents

`scholar` is the default agent. It is granted the `kb` toolset from the [simple](../simple) example, pointing at that example's knowledgebase with a relative `base_dir` (`../simple/knowledgebase`, resolved against `blorb.json`'s directory). The grant is the whole `kb` toolset, which is `kb-read` and `kb-grep`. It also gets the `search` subagent tool and the `route_question` decider tool. Its system prompt tells it the knowledgebase is one file per region (`france.md`, `united-kingdom.md`, ...), plus `dunking.md` and a `README.md`. For any biscuit question it calls `route_question` first, then reads the file the decision names: the region files are short, so it reads the whole file rather than inventing a grep pattern. It greps only for a term across the whole knowledgebase, and delegates to `search` when a pattern comes up empty. The decision is the plan: which file to read, and whether the excerpt it already has is enough.

`search` is an expert searcher given the same `kb` toolset and no delegations of its own. When a grep pattern comes up empty it tries alternatives before reporting back: other spellings, synonyms, singular and plural, broader terms. Its output is grep's format (`path:line:text`). The scholar delegates to it when a pattern comes up empty.

## The decider

A decider fixes the typed questions every call asks; only the state varies per call. This example asks four at once - the decision model evaluates all of them against the same state in a single request, so the scholar needs one call to plan its route:

```json
{
  "name": "route_question",
  "model": "jev",
  "questions": {
    "region": {
      "type": "choice",
      "instructions": "Which knowledgebase file is most likely to hold the answer? Answer with the filename to read. Choose unknown when the question is not region-specific, in which case search the whole knowledgebase.",
      "criteria": {
        "united-kingdom.md": "The British Isles",
        "france.md": "France",
        "...": "...",
        "unknown": "Not region-specific; search the whole knowledgebase"
      }
    },
    "question_kind": {
      "type": "choice",
      "instructions": "What kind of question is this, so the agent knows what to read?",
      "criteria": {
        "origin": "Where and when a biscuit comes from",
        "ingredients": "What a biscuit is made of",
        "dunking": "How a biscuit behaves in a hot drink",
        "comparison": "How biscuits compare to one another",
        "other": "None of the listed kinds fits"
      }
    },
    "answerable_from_excerpt": {
      "type": "noul",
      "instructions": "Does the excerpt in the state already hold enough to answer the question, or does the knowledgebase need to be read?",
      "criteria": { "true": "The excerpt is enough to answer", "false": "The knowledgebase needs to be read" }
    },
    "action": {
      "type": "score",
      "instructions": "What should your agent do next with this question?",
      "criteria": [
        "Answer from the excerpt as it is",
        "Retrieve from the chosen knowledgebase file",
        "Dig further with the search agent"
      ]
    }
  }
}
```

`region` and `question_kind` are `choice` questions: `criteria` is a map of option name to description, and each answer selects one option with a probability per option. `answerable_from_excerpt` is a `noul`: a calibration whose answer is the probability of yes. `action` is a `score`: an ordered rubric. The `region` options are the actual knowledgebase filenames (`france.md`, `united-kingdom.md`, `dunking.md`, ...), so the decision's answer is used directly as the path the scholar reads; `unknown` means the question is not region-specific and the scholar searches the whole knowledgebase.

## The tool

The `route_question` tool references the decider by name. A decider tool takes a single `state` string by default; this one gives the tool a custom `args_schema`, so the raw JSON arguments are the state - a structured question and excerpt rather than a blob of text:

```json
{
  "type": "decider",
  "name": "route_question",
  "description": "Plan how to answer a biscuit question: which knowledgebase file is most likely to hold the answer, what kind of question it is, whether the excerpt already suffices, and whether to retrieve or answer.",
  "decider": "route_question",
  "args_schema": {
    "type": "object",
    "properties": {
      "question": { "type": "string", "description": "The user's biscuit question, verbatim" },
      "excerpt": { "type": "string", "description": "Any knowledgebase excerpt already gathered, or an empty string" }
    },
    "required": ["question", "excerpt"],
    "additionalProperties": false
  }
}
```

The tool makes one decision API call with the decider's four questions and the structured state, and returns the answers as a JSON object, keyed by question name. In chat, the decision prints as a labeled, indented block before the tool result, so you watch it land:

```text
>>> Tool: route_question
{"question":"which biscuits survive a long dunking?","excerpt":""}

[route_question] >>> Decision:
  {"region":{"type":"choice","choice":"united-kingdom.md","probabilities":{...},"confidence":0.74},"question_kind":{"type":"choice","choice":"dunking",...},"answerable_from_excerpt":{"type":"noul","noul":0.05},"action":{"type":"score","score":1,...}}
>>> Result: Tool: route_question
  {"region":...,"question_kind":...,"answerable_from_excerpt":...,"action":...}
```

## Run

From the repo root:

```sh
bin/build

# chat with the scholar; it routes each question through the decider, then reads
./blorb chat --config examples/decision/blorb.json

# or run the decider directly, the way `run --agent` invokes an agent
./blorb decide --config examples/decision/blorb.json --decider route_question \
  "which biscuits survive a long dunking?"
```

Then try prompts like `tell me about french biscuits`, `which biscuits survive a long dunking?`, or `who eats something like a jammie dodger?` and watch the decision pick a region before the scholar reads.

`blorb decide` prints the answers JSON to stdout and exits: the decider counterpart of `blorb run`. The `[state]` argument shares `run`'s prompt syntax (literal, `@@` escape, `@file`, `-` for stdin); by default it is sent as a JSON string, and `--state-json` sends a structured object or array verbatim:

```sh
./blorb decide --config examples/decision/blorb.json --decider route_question --state-json \
  '{"question":"which biscuits survive a long dunking?","excerpt":""}'
```

The command does not need the chat model or the local server, only the decision provider.

## Logs

Running the example produces a `.logs` directory next to `blorb.json`, one file per LLM request/response and per tool call/result. The decision call's request/response land there too, like any other LLM interaction. Logging can be disabled by adding `"logging": { "enabled": false }` to `blorb.json`.
