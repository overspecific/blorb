# Knowledgebase-grounded decision example

A config that puts a decision model to work on a job a generative model is bad at: narrowing a complaint before the agent acts. It reuses the biscuit knowledgebase from the [simple](../simple) example and adds a `triage` agent that looks a complaint up, a `search` subagent that digs for it when a first pattern comes up empty, and a `decider` - a fixed set of typed questions for a System One model (Jev) - that the agent calls once to decide which biscuit and region are at stake, whether the excerpt it gathered already answers the complaint, and what to do next.

The chat model is the same local server the [simple](../simple) example uses. The decision model is hosted, because it speaks a different protocol.

## Setup

The chat agent talks to a local OpenAI-compatible server. The model config points at a local [Lemonade](https://lemonade-server.com) server (`http://localhost:13305/v1`) with the `Gemma-4-E4B-it-GGUF` model; adjust `base_url` and `model_name` in the top-level `models` list in `blorb.json` to match whatever OpenAI-compatible endpoint you want to use (OpenAI, Lemonade, LM Studio, vLLM, Ollama, ...). If your endpoint needs an API key, add an `api_key_env` entry naming the environment variable that holds it.

The decision model is a System One model (Jev is the first example): it evaluates a state against typed questions and returns typed answers with probabilities instead of generated text. Point the `typesafe` provider's `base_url` at a vendor that speaks the protocol - TypeSafe itself (`https://api.typesafe.ai/v1`), OpenRouter (`https://openrouter.ai/api/v1`), jevmodel.org (`https://jevmodel.org/v1`), apimodels, Venice - and export the key its `api_key_env` names:

```sh
export TYPESAFE_API_KEY="..."
```

A decision model sits on an `openai-compatible` provider and is marked with `"model_type": "decision"`. Its `model_name` is optional: when omitted, the request leaves the model field out and the server applies its deployment default. It does not accept the chat-model knobs (`reasoning_effort`, `tool_choice`, `logprobs`, and the Ollama-only settings). A decision model cannot be an agent's model; only a decider references it.

## The agents

`triage` is the default agent. It is granted the `kb` toolset from the [simple](../simple) example, pointing at that example's knowledgebase with a relative `base_dir` (`../simple/knowledgebase`, resolved against `blorb.json`'s directory). The grant shows both forms: the whole `kb` toolset, which is `kb-read` and `kb-grep`, and the single `kb-read` member by name. It also gets the `search` subagent tool and the `triage_ticket` decider tool. Its system prompt tells it to never decide blind: grep the knowledgebase for the biscuit and the issue, read the region file the match points at when it needs the full entry, and delegate to `search` when a pattern comes up empty. Only then does it call the decider.

`search` is an expert searcher given the same `kb` toolset and no delegations of its own. When a grep pattern comes up empty it tries alternatives before reporting back: other spellings, synonyms, singular and plural, broader terms. Its output is grep's format (`path:line:text`).

## The decider

A decider fixes the typed questions every call asks; only the state varies per call. This example asks four at once - the decision model evaluates all of them against the same state in a single request, so the agent needs one call to narrow the complaint:

```json
{
  "name": "triage",
  "model": "jev",
  "questions": {
    "biscuit": {
      "type": "choice",
      "instructions": "Which named biscuit is the complaint about? Choose Other when no listed biscuit fits.",
      "criteria": {
        "rich_tea": "Rich Tea, the plain British dunker",
        "digestive": "Digestive, including the chocolate variant",
        "...": "..."
      }
    },
    "region": {
      "type": "choice",
      "instructions": "Which region file of the knowledgebase owns that biscuit? Choose unknown when the excerpt does not make it clear.",
      "criteria": { "united_kingdom": "The British Isles", "...": "..." }
    },
    "answerable_from_excerpt": {
      "type": "noul",
      "instructions": "Does the excerpt in the state already hold enough to answer the complaint, or does it need more digging?",
      "criteria": { "true": "The excerpt is enough to answer", "false": "More digging is needed" }
    },
    "action": {
      "type": "score",
      "instructions": "What should the agent do with this complaint?",
      "criteria": [
        "Answer from the knowledgebase as it is",
        "Dig further with the search agent",
        "Refer the complaint to a human"
      ]
    }
  }
}
```

`biscuit` and `region` are `choice` questions: `criteria` is a map of option name to description, and each answer selects one option with a probability per option. `answerable_from_excerpt` is a `noul`: a calibration whose answer is the probability of yes. `action` is a `score`: an ordered rubric. The `region` options name the knowledgebase's region files, so the decision itself tells the agent which file to read.

## The tool

The `triage_ticket` tool references the decider by name. A decider tool takes a single `state` string by default; this one gives the tool a custom `args_schema`, so the raw JSON arguments are the state - a structured ticket rather than a blob of text:

```json
{
  "type": "decider",
  "name": "triage_ticket",
  "description": "Evaluate a biscuit complaint against a knowledgebase excerpt and return which biscuit and region are at stake, whether the excerpt answers it, and the recommended action.",
  "decider": "triage",
  "args_schema": {
    "type": "object",
    "properties": {
      "complaint": { "type": "string", "description": "The original complaint text, verbatim" },
      "excerpt": { "type": "string", "description": "The knowledgebase excerpt gathered about the biscuit and issue" }
    },
    "required": ["complaint", "excerpt"],
    "additionalProperties": false
  }
}
```

The tool makes one decision API call with the decider's four questions and the structured state, and returns the answers as a JSON object, keyed by question name. In chat, the decision prints as a labeled, indented block before the tool result, so you watch it land:

```text
>>> Tool: triage_ticket
{"complaint":"My chocolate digestive collapsed on the second dunk. I want compensation.","excerpt":"united-kingdom.md:11: ... chocolate digestives are a gamble (the chocolate acts as a partial barrier ...)"}

[triage] >>> Decision:
  {"biscuit":{"type":"choice","choice":"digestive","probabilities":{...},"confidence":0.91},"region":{"type":"choice","choice":"united_kingdom",...},"answerable_from_excerpt":{"type":"noul","noul":0.86},"action":{"type":"score","score":0,...}}
>>> Result: Tool: triage_ticket
  {"biscuit":...,"region":...,"answerable_from_excerpt":...,"action":...}
```

## Run

From the repo root:

```sh
bin/build

# chat with the triage agent; it retrieves from the knowledgebase, then decides
./blorb chat --config examples/decision/blorb.json

# or run the decider directly, the way `run --agent` invokes an agent
./blorb decide --config examples/decision/blorb.json --decider triage \
  "My chocolate digestive collapsed on the second dunk."
```

`blorb decide` prints the answers JSON to stdout and exits: the decider counterpart of `blorb run`. The `[state]` argument shares `run`'s prompt syntax (literal, `@@` escape, `@file`, `-` for stdin); by default it is sent as a JSON string, and `--state-json` sends a structured object or array verbatim:

```sh
./blorb decide --config examples/decision/blorb.json --decider triage --state-json \
  '{"complaint":"My chocolate digestive collapsed on the second dunk.","excerpt":"chocolate digestives are a gamble; the chocolate is a partial barrier."}'
```

The command does not need the chat model or the local server, only the decision provider.

## Logs

Running the example produces a `.logs` directory next to `blorb.json`, one file per LLM request/response and per tool call/result. The decision call's request/response land there too, like any other LLM interaction. Logging can be disabled by adding `"logging": { "enabled": false }` to `blorb.json`.
