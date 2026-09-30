# Plaud example agent

A two-agent config that exposes the [Plaud CLI](https://plaud.ai) (`plaud`) as agent tools: the agents can list the user's Plaud recordings and fetch their transcripts, then answer questions about them. It shows a `command` tool wrapping a real CLI that reads its tool arguments from stdin as JSON - the `sh -c` scripts parse them out with `jq` and pass them on as flags - and a `subagent` tool delegating summary work to a second agent.

The main agent (`plaud`, the config's `default_agent`) is granted three tools, all declared at the top level:

- `files` - lists the user's Plaud recordings with their IDs, names, dates, and durations, one page at a time (`page` and `page_size` are optional; the CLI defaults to page 1, 20 per page, and the page size must be between 10 and 100).
- `transcript` - fetches a recording's transcript by `file_id` (the ID format from `files`, e.g. `of_28946b31bb1f863c94efade3844e303b`). The optional `block` picks a variant: `transaction` (the raw transcript, the default), `transaction_polish` (the AI-cleaned transcript), `outline`, or `mark_memo` (moments flagged with the device's highlight button). Not every recording has every variant; the error message lists what is available.
- `summarize` - a subagent tool delegating to the `summarizer` agent (below). It takes the recording's `file_id` (the ID format from `files`, e.g. `of_28946b31bb1f863c94efade3844e303b`) - `plaud` must list the files first to get the ID, same as for `transcript`.

The second agent, `summarizer`, is granted just the `transcript` tool. The `summarize` tool declares a custom `args_schema` with a required `file_id`, so instead of the default `prompt` string, the raw JSON arguments (e.g. `{"file_id": "of_..."}`) become the summarizer's user message - its system prompt accounts for that. The summarizer fetches the transcript (preferring `transaction_polish`, falling back to `transaction`) and returns a JSON object and nothing else - no markdown or code fences:

```json
{
  "recording": "Monday standup",
  "summary": "I met with the team for our Monday standup. We went over ...",
  "action_items": [
    { "task": "Send the weekly report", "due": "2026-10-02", "category": "professional", "estimated_duration": "PT30M" }
  ]
}
```

The `summary` is written in first person from the perspective of whoever made the recording - `I` and `we`, not "the user" - so it reads like their own notes. `due` is null when the transcript states no date that can be pinned to a calendar date; otherwise every date and time is ISO 8601, zero-padded, in exactly two forms - a date alone as `YYYY-MM-DD`, or a date with a time of day as `YYYY-MM-DDTHH:MM:SS` followed by the timezone exactly as the transcript states it (`Z` for UTC, or `+HH:MM`/`-HH:MM`; no offset when the transcript states none). Month names, `10/02/2026`, and other forms never appear, and relative dates like `by Friday` are resolved against the recording's date when the transcript shows it. Tasks belong to the recording's maker - there is no `owner` field. `category` classifies the task by its nature - exactly `personal` or `professional`. `estimated_duration` is the summarizer's estimate of how long the task will take, as an ISO 8601 duration in exactly one form - `PT` then hours `H` and minutes `M`, zero components omitted, rounded to whole minutes, between `PT1M` and `PT8H` (e.g. `PT30M`, `PT2H`, `PT1H15M`). `action_items` is empty when no tasks were agreed. The JSON becomes the tool result `plaud` sees, so it can present the summary in whatever form fits the conversation. The summarizer's activity - assistant messages and tool calls - shows up indented and labeled with its name in the chat, and its LLM calls get their own line in the usage footer.

## Setup

Two accounts to set up: the Plaud CLI and Crusoe.

Authenticate with the Plaud CLI first:

```sh
plaud login
```

Then create a [Crusoe](https://www.crusoe.ai) account and grab an inference API key, and export it:

```sh
export CRUSOE_API_KEY="..."
```

The model config points at Crusoe's inference API (`https://api.inference.crusoecloud.com/v1`, `openai-compatible` provider type) with the `zai-org/GLM-5.3-Flash` model, granted to both agents as `flash`. Swap `model_name` for any other model the endpoint serves (list them with `GET /v1/models`), or point `base_url` at a different OpenAI-compatible endpoint, a local Ollama (change `type` to `ollama`, `http://localhost:11434`, no `api_key_env` needed), or back at Ollama cloud.

## Run

From the repo root:

```sh
bin/build
./blorb chat --config examples/plaud/blorb.json
./blorb run --config examples/plaud/blorb.json "summarize my most recent recording"   # one turn, then exit
```

Then try prompts like:

```text
list my recordings
what did I record most recently?
summarize the Welcome to Plaud.ai recording
give me the outline of my latest meeting
what moments did I highlight in my most recent recording?
give me a summary and action items for my most recent recording
```

Type `exit` (or hit Ctrl-D) to quit. Ctrl-C interrupts an in-flight turn; Ctrl-C while idle exits. Streaming is on by default - GLM-5.3-Flash is a thinking model, so its reasoning streams live ahead of each answer (also indented and labeled when the summarizer subagent runs); `--no-stream` turns that off.

## Logs

Running the example produces a `.logs` directory next to `blorb.json`. Each chat session gets its own `<timestamp>-<uuid>` subdirectory containing one file per LLM request/response and per tool call/result, so conversations never interleave. Logging can be disabled by adding `"logging": { "enabled": false }` to `blorb.json`.
