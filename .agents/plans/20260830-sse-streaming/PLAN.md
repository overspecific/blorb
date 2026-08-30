# SSE streaming of LLM responses

Adds streaming of LLM responses over Server-Sent Events (SSE), from the OpenAI-compatible client through the engine to the chat UI. Streaming is additive: the existing non-streaming `Chat` path is unchanged, and the chat UI gains a `--no-stream` flag to disable it. When streaming is enabled and the client supports it, everything the OpenAI API streams is surfaced incrementally as it arrives, in order: assistant text, reasoning (chain-of-thought), and tool calls (name and arguments). Otherwise the existing whole-message path is used.

## Todo

- [x] Commit 1: Streaming types and interface
- [x] Commit 2: OpenAI streaming client
- [x] Commit 3: Engine streaming
- [x] Commit 4: Chat UI and CLI
- [x] Commit 5: Cap reasoning and tool-call argument accumulation
- [x] Commit 6: Carry tool-call index through delta events
- [x] Commit 7: Harden ChatStream non-2xx and empty-stream handling
- [x] Commit 8: Fix stale docs and update README

---

## Commit 1: Streaming types and interface

> In `internal/llm/llm.go`, add the provider-neutral streaming vocabulary:
>
> - `type Delta struct { Content string; Reasoning string; ToolCall *ToolCallDelta }` — one incremental piece of a streaming response. `Content` is a fragment of assistant text; `Reasoning` is a fragment of extracted chain-of-thought; `ToolCall` is non-nil when the delta carries a fragment of a tool call. Document that deltas arrive in order and that the concatenation of all `Content` fragments equals the final message's content, the concatenation of all `Reasoning` fragments equals the final message's reasoning, and the tool-call fragments (keyed by `Index`) assemble into the final message's tool calls.
> - `type ToolCallDelta struct { Index int; ID string; Type string; Name string; Arguments string }` — a fragment of a tool call. `Index` identifies which tool call in the message the fragment belongs to (0-based, matching the OpenAI `tool_calls[].index`); `ID`, `Type`, and `Name` are typically complete in the first fragment for a given index, while `Arguments` arrives as one or more fragments to be concatenated in order.
> - `type StreamingClient interface { Client; ChatStream(ctx context.Context, req Request, onDelta func(Delta) error) (*Response, error) }` — the seam for clients that can stream. Document the contract: send the full message history, honour context cancellation, call `onDelta` for each incremental piece of content, reasoning, or tool call as it arrives (in order), and return the complete `*Response` (including any tool calls) once the stream finishes. An `onDelta` error aborts the stream and is returned. Clients that do not implement this interface are used via `Client.Chat` instead.
>
> Add a test in `internal/llm/llm_test.go` with a small mock type implementing both `Chat` and `ChatStream`, asserting via a compile-time `var _ llm.StreamingClient = ...` check that it satisfies the interface. No other packages change.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 2: OpenAI streaming client

> In `internal/llm/openai/openai.go`, implement `ChatStream` on `Client` so it satisfies `llm.StreamingClient`:
>
> - Add `Stream bool` (`json:"stream,omitempty"`) and `StreamOptions *streamOptions` (`json:"stream_options,omitempty"`) to `wireRequest`, where `streamOptions` has `IncludeUsage bool` (`json:"include_usage"`). `Chat` leaves these unset; `ChatStream` sets `Stream: true` and `StreamOptions: &streamOptions{IncludeUsage: true}`.
> - Extract the request-building and POST logic shared by `Chat` and `ChatStream` into a helper (marshal body, join the `/chat/completions` path, build the `http.Request` with headers) so the two paths cannot drift. `ChatStream(ctx, req, onDelta)` then reads the response body as an SSE stream: scan line by line, and for each line beginning with `data:` parse the payload. A payload of `[DONE]` ends the stream; ignore other lines. On a non-2xx status, return the same descriptive error as `Chat` (status + truncated body).
> - Parse each chunk into a wire struct with `id`, `choices[0].delta` (`content`, `reasoning_content`, `tool_calls` with `index`/`id`/`type`/`function.name`/`function.arguments`), `choices[0].finish_reason`, and `usage`. Accumulate: append `content` and `reasoning_content` fragments; merge tool-call deltas by `index` (accumulating `id`, `type`, `name`, and `arguments` fragments in order); capture `id` from the first chunk, `finish_reason` from the last chunk that sets it, and `usage` from the final chunk.
> - Call `onDelta(Delta{Content: ..., Reasoning: ..., ToolCall: ...})` for each chunk that carries content, reasoning, or a tool-call fragment, in order. A tool-call fragment is emitted as a `ToolCallDelta` carrying the chunk's `index`, `id`, `type`, `name`, and `arguments` fragment. An `onDelta` error aborts the stream and is returned.
> - When the stream ends, build the complete `llm.Response` (role assistant, accumulated content/reasoning/tool calls, finish reason, usage) and apply the same tool-call validation as `Chat` (missing id or name is an error). Cap accumulated content at the existing `maxBodyLen` and return an error if exceeded, so a runaway stream cannot exhaust memory. Honour context cancellation.
>
> Tests in `internal/llm/openai/openai_test.go` using `httptest.NewServer`: a server streaming several content chunks yields deltas in order and a final response whose content is the concatenation; `reasoning_content` fragments accumulate into `Message.Reasoning`; a server streaming tool-call fragments (id/name/arguments split across chunks) yields a single complete `ToolCall` and emits `ToolCallDelta` fragments in order; the request body includes `"stream":true` and `"stream_options":{"include_usage":true}`; a non-2xx response surfaces the status; a `[DONE]`-less stream ending at EOF still returns the accumulated response; context cancellation mid-stream returns the context error. No other packages change.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 3: Engine streaming

> In `internal/engine/engine.go`, add streaming to the turn loop:
>
> - Add `Stream bool` to `EngineConfig` (documented: when true and the client implements `llm.StreamingClient`, assistant text, reasoning, and tool calls are emitted incrementally; otherwise the whole-message path is used).
> - Add `EventAssistantTextDelta`, `EventAssistantThinkingDelta`, and `EventToolCallDelta` to the `EventKind` enum. `EventAssistantTextDelta` and `EventAssistantThinkingDelta` carry incremental text/reasoning in `Event.Text`; `EventToolCallDelta` carries a tool-call fragment in `Event.Name` (the tool name, when present) and `Event.Args` (an arguments fragment). Document that they are emitted only when streaming, and that the concatenation of all `EventAssistantTextDelta` events in a round equals the round's assistant text (and likewise `EventAssistantThinkingDelta` for reasoning), while `EventToolCallDelta` fragments assemble into the round's tool calls.
> - Change `call` to accept the `onEvent` callback and return `(resp *llm.Response, streamed bool, err error)`. When `e.cfg.Stream` is set and the client implements `llm.StreamingClient`, use `ChatStream` (via a small `streamCall` helper) and return `streamed == true`; otherwise use `Chat` and return `streamed == false`. In the streaming path, emit each reasoning fragment as `EventAssistantThinkingDelta`, each content fragment as `EventAssistantTextDelta`, and each tool-call fragment as `EventToolCallDelta`, in the order they arrive.
> - In `RunTurn`, use the `streamed` flag: when true, skip the existing `EventAssistantThinking`/`EventAssistantText` emission for that response (the streaming path already emitted them); when false, emit them as today. Tool-call handling, history appending, and the accumulated `final` text are unchanged (they use the complete response).
>
> Tests in `internal/engine/engine_test.go`: extend the fake client with an optional `ChatStream` (or add a streaming fake) and assert that with `Stream: true` a streaming client produces `EventAssistantTextDelta` events whose concatenation equals the final text, and no `EventAssistantText`; that reasoning fragments are emitted as `EventAssistantThinkingDelta` events whose concatenation equals the reasoning, interleaved in arrival order with the text deltas; that tool-call fragments are emitted as `EventToolCallDelta` events and the turn still drives tool execution and returns the correct final text; that with `Stream: false` (or a non-streaming client) the existing whole-message events are emitted. No other packages change.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 4: Chat UI and CLI

> Wire streaming into the chat REPL and the CLI:
>
> - In `internal/chat/chat.go`, add `Stream bool` to `Options` and pass it through to `engine.EngineConfig.Stream`. Change `chatEvents` to return `(func(engine.Event) error, func())`: the callback handles `EventAssistantThinkingDelta` by printing the `>>> Assistant (thinking):` heading once and then writing each reasoning delta to stdout without a trailing newline, `EventAssistantTextDelta` by printing the `>>> Assistant:` heading once and then writing each text delta to stdout without a trailing newline, and `EventToolCallDelta` by printing the `>>> Tool: <name>` heading once per tool call and then writing each arguments fragment to stderr without a trailing newline; the returned flush function writes a trailing newline if any delta was written. Keep the existing block-event handling, and make tool events flush any pending newline before writing to stderr. In the turn loop, create the callback and flush function before `RunTurn`, and call the flush function after `RunTurn` returns regardless of error.
> - In `main.go`, add a `--no-stream` bool flag to `chatCommand` (usage: "Disable streaming of assistant responses") and pass `Stream: !cmd.Bool("no-stream")` to `chat.Options`, so streaming is on by default and disabled by the flag.
>
> Tests: in `internal/chat/chat_test.go`, drive `Run` with a streaming fake client and assert the assistant text appears on stdout (heading once, deltas concatenated, trailing newline flushed); assert reasoning deltas appear under a `>>> Assistant (thinking):` heading; assert tool-call deltas appear on stderr under a `>>> Tool:` heading; assert that with `Stream: false` the whole-message path is used; assert tool results still land on stderr after a streamed turn. In `main_test.go`, assert `chatCommand` has a `no-stream` bool flag defaulting to false.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 5: Cap reasoning and tool-call argument accumulation

> In `internal/llm/openai/openai.go`, the stream accumulator caps accumulated `content` at `maxContentLen` but leaves `reasoning` and tool-call `arguments` unbounded, so a runaway or buggy server streaming endless `reasoning_content` or argument fragments can exhaust memory. Apply the same cap to both:
>
> - In `streamAccumulator.addDelta`, before appending a `reasoning` fragment, check `a.reasoning.Len()+len(delta.Reasoning) > maxContentLen` and return the same "exceeds %d byte limit" error used for content.
> - When appending a tool-call `arguments` fragment (both the first-fragment construction and the `tc.Function.Arguments += wtd.Function.Arguments` merge), check the accumulated length against `maxContentLen` and return the same error. Track the running length per tool call (e.g. a `map[int]int` of accumulated argument bytes, or check `len(tc.Function.Arguments)` before appending).
>
> Tests in `internal/llm/openai/openai_test.go`: a server streaming reasoning fragments that exceed the cap returns a limit error; a server streaming tool-call argument fragments that exceed the cap returns a limit error. No other packages change.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 6: Carry tool-call index through delta events

> The engine's `streamCall` maps `llm.ToolCallDelta` to `Event{Name, Args}` and drops `Index`, so the chat UI keys its "already printed" map by tool name and collapses a second call to the same tool in one round (the whole-message path prints both). Carry the index through so each tool call renders its own heading:
>
> - In `internal/engine/engine.go`, add an `Index int` field to `Event` (documented: set for `EventToolCallDelta`, the 0-based tool-call index from `llm.ToolCallDelta.Index`). In `streamCall`, set `Index: d.ToolCall.Index` on the `EventToolCallDelta` event.
> - In `internal/chat/chat.go`, key the `toolHeadings` map by index instead of name: `toolHeadings[ev.Index]` (or a `map[int]bool`), so two calls to the same tool in one round each print a `>>> Tool: <name>` heading. Keep the heading text as `>>> Tool: <name>`.
>
> Tests: in `internal/engine/engine_test.go`, assert `EventToolCallDelta` events carry the correct `Index` for a multi-tool-call response. In `internal/chat/chat_test.go`, add a case where the model calls the same tool twice in one round and assert both `>>> Tool:` headings appear. No other packages change.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 7: Harden ChatStream non-2xx and empty-stream handling

> In `internal/llm/openai/openai.go`, make `ChatStream`'s error and edge-case handling consistent with `Chat`:
>
> - In the non-2xx branch, after `io.ReadAll(io.LimitReader(...))`, flag the `len == maxBodyLen` overflow the way `Chat` does (return "response exceeds %d byte limit") instead of silently truncating.
> - In `readStream`, after the scan loop, if no `data:` chunk was ever parsed (i.e. the accumulator saw zero deltas and no `[DONE]`), return a descriptive error (e.g. "decode response: no data in stream") rather than returning an empty `Response` that the engine would treat as a silent empty final answer. Track a `sawData bool` set on the first parsed chunk.
>
> Tests in `internal/llm/openai/openai_test.go`: a 200 response with an empty body (or only comments/blank lines) returns a "no data" error; a non-2xx response whose body exceeds the cap returns a limit error. No other packages change.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 8: Fix stale docs and update README

> - In `internal/engine/engine.go`, update the `Event` doc comment (currently "Text is set for EventAssistantText and EventAssistantThinking; Name and Args for EventToolCall; Name, Output and Failed for EventToolResult") to also cover the three delta kinds: `EventAssistantTextDelta`/`EventAssistantThinkingDelta` carry `Text`, and `EventToolCallDelta` carries `Name`, `Args`, and `Index`.
> - In `README.md`, document streaming: add a bullet to the Features list (streamed assistant responses over SSE, with `--no-stream` to disable), and update the Chat flags line to include `--no-stream`. Update the layout section's `internal/llm/openai` description to mention SSE streaming if appropriate.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.
