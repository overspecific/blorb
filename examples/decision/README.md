# Decision example agent

A config that uses a decision model as a tool. It declares a `decider` - a fixed set of typed questions for a System One model (Jev) - and a `decider` tool that lets the `triage` agent evaluate a support ticket against it. The agent gets a typed decision back (which department, whether a refund was requested) and answers from it.

## Setup

The decision model talks to a System One-compatible endpoint. Any endpoint that speaks the protocol works:

- TypeSafe itself: `https://api.typesafe.ai/v1`
- OpenRouter's System One surface: `https://openrouter.ai/api/v1`
- jevmodel.org: `https://jevmodel.org/v1`
- apimodels, Venice, and other vendors that share the protocol

Point the `typesafe` provider's `base_url` at your vendor (a URL ending in the version segment works for all of them) and set the API key environment variable named by its `api_key_env`:

```sh
export TYPESAFE_API_KEY="..."
```

A decision model sits on an `openai-compatible` provider and is marked with `"model_type": "decision"`. Its `model_name` is optional: when omitted, the request leaves the model field out and the server applies its deployment default. It does not accept the chat-model knobs (`reasoning_effort`, `tool_choice`, `logprobs`, and the Ollama-only settings).

The `chat` model the agent talks to is a plain chat completions model on its own provider; point it at whatever chat endpoint you have and export `OPENAI_API_KEY` (or edit the provider to match your server).

## The decider

A decider fixes the typed questions every call asks; only the state varies per call. This example asks two:

```json
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
        "sales": "New purchases and upgrades",
        "other": "No listed department fits"
      }
    },
    "refund_requested": {
      "type": "noul",
      "instructions": "Does the customer explicitly request a refund?",
      "criteria": {
        "true": "Explicitly asks for a refund",
        "false": "Does not ask for a refund"
      }
    }
  }
}
```

`department` is a `choice` question: `criteria` is a map of option name to description, and the answer selects one option with a probability per option. `refund_requested` is a `noul` question: a calibration whose answer is the probability of yes. A `score` question (an ordered rubric) is the third type; see [Deciders](../../docs/configuration.md#deciders).

## The tool

The `triage_ticket` tool references the decider by name. By default it takes a single `state` string, so the agent calls it with the ticket text:

```json
{
  "type": "decider",
  "name": "triage_ticket",
  "description": "Evaluate a support ticket and return its department and whether a refund was requested.",
  "decider": "triage"
}
```

The tool makes one decision API call with the decider's questions and returns the answers as a JSON object, keyed by question name. In chat, the decision prints as a labeled, indented block before the tool result, so you watch it land:

```text
>>> Tool: triage_ticket
{"state":"I was charged twice for one order. Please refund the duplicate."}

[triage] >>> Decision:
  {"department":{"type":"choice","choice":"billing","probabilities":{...},"confidence":0.88},"refund_requested":{"type":"noul","noul":0.95}}
>>> Result: Tool: triage_ticket
  {"department":...}
```

## Run

From the repo root:

```sh
bin/build

# chat with the triage agent; it decides via the decider tool
./blorb chat --config examples/decision/blorb.json

# or run the decider directly, the way `run --agent` invokes an agent
./blorb decide --config examples/decision/blorb.json --decider triage \
  "I was charged twice for one order. Please refund the duplicate before Friday."
```

`blorb decide` prints the answers JSON to stdout and exits: the decider counterpart of `blorb run`. The `[state]` argument shares `run`'s prompt syntax (literal, `@@` escape, `@file`, `-` for stdin).

## Logs

Running the example produces a `.logs` directory next to `blorb.json`, one file per LLM request/response and per tool call/result. The decision call's request/response land there too, like any other LLM interaction. Logging can be disabled by adding `"logging": { "enabled": false }` to `blorb.json`.
