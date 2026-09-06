# Output formats

The machine-parsing reference for `blorb run --format plain` and `blorb run --format ndjson` - the formats designed for scripts and pipelines. See the [CLI reference](cli.md) for the `run` command itself and the chat format.

## plain

**`plain`** puts just the agent's output on stdout - no headings, no decorations, no trailing newline - so it composes in pipelines (`./blorb run --format plain "..." | jq`). Everything else goes to stderr: the chat-style progress (headings, tool activity, streamed fragments) and the usage footer. The agent's output is the assistant's text events spliced exactly as they arrived, with nothing added between them.

### plain logprobs

With `--format plain --logprobs`, one line prints after the response body per token - the token, its logprob, and the top alternative when present:

```text
Hi there
  "Hi" logprob=-0.2500 (top: "Hi" -0.2500)
  " there" logprob=-0.1000
```

A non-streaming feature: `--logprobs` both asks the server for the data (overriding `logprobs` in the model config - see [Models](configuration.md#models)) and prints the block, and it requires `--no-stream` - a `blorb run --logprobs` without it fails before any LLM call, because streamed responses do not decode logprob data. A server that accepts the request but returns none errors the run instead of printing an empty block (Ollama Cloud does exactly this; see [Models](configuration.md#models)).

## ndjson

**`ndjson`** streams the run's full event stream to stdout as one JSON object per line, as it happens: assistant text, reasoning, tool calls and results, token usage, and subagent activity. Each line is a flat object discriminated by its `type` field; ignore unknown types for forward compatibility. The stream ends with a `done` event carrying the final text and usage totals, or an `error` event on failure. A run that fails before the turn starts (bad config, unknown format) emits no events.

### ndjson event types

Subagent activity uses the same vocabulary prefixed `subagent_`, with `agent` and `depth` fields added:

```text
text_delta      {type, text}                       assistant text fragment (streaming)
thinking_delta  {type, thinking}                   reasoning fragment (streaming)
tool_call_delta {type, index, name?, arguments}    tool call fragment; assemble by index, concatenating arguments
text            {type, text, logprobs?}            whole assistant message (with --no-stream); logprobs when the model reported them
thinking        {type, thinking}                   whole reasoning (with --no-stream)
tool_call       {type, name, arguments}            whole tool call (with --no-stream)
tool_result     {type, name, output, failed}       tool result; output is always the full body
usage           {type, agent, model, usage, stats} one LLM call's token usage and call stats
done            {type, text?, logprobs?, usage, stats, rates?, agents}  terminal on success
error           {type, error}                      terminal on failure
```

### The stats object

The `stats` object on `usage`, `subagent_usage`, and `done` always carries the measured output bytes: `{"output":{"content_bytes":...,"reasoning_bytes":...,"tool_call_bytes":...},"elapsed_ns":...}`. Derive the total by summing the three components. `done.agents[].stats` is each agent's summed stats. `done.rates` (`{"tokens_per_sec":...,"bytes_per_sec":...}`) is a convenience derived from the summed stats - consumers can compute their own rates from the raw fields - and is omitted when no time was measured.

With `--logprobs` (or `logprobs: true` in the model config; a non-streaming feature - see [plain logprobs](#plain-logprobs)), the `text` and `done` events carry a `logprobs` array - one entry per content token.

For example, to print just the assistant's text as it streams:

```sh
./blorb run --format ndjson "..." | jq -r 'select(.type=="text_delta") | .text'
```

## Streaming and tool output

Streaming applies across all formats: `--no-stream` switches to whole-message events (in ndjson, the `text`/`thinking`/`tool_call` types instead of deltas; in plain, whole blocks on stderr instead of live fragments). `--tool-output` applies to the chat and plain result blocks; ndjson always carries full tool result bodies.

Exit codes: `0` on a completed turn, `1` on any error, `130` on Ctrl-C (SIGINT). The exit codes are unchanged across formats - ndjson's `done`/`error` event is the machine-readable outcome, not a changed exit contract. A run that fails before the turn emits no ndjson events; the error goes to stderr.
