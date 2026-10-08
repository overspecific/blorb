# AssemblyAI voice agent input

Add a `blorb voice` command: a voice session where the user talks into a
microphone, AssemblyAI's Voice Agent API runs the conversation loop
server-side (STT, LLM, TTS), blorb's tools run locally, and the agent's
reply plays out loud on the speakers. The console streams a live
transcript of the whole conversation while it happens: partial user
speech as it is recognised, the agent's words in step with the audio,
and tool activity as it runs.

The Voice Agent API is a full-duplex WebSocket at
`wss://agents.assemblyai.com/v1/ws`: one `session.update` names the
system prompt, greeting, voice and tool declarations; mic audio streams
up as base64 PCM16 in `input.audio`; agent audio, transcripts and tool
calls stream down. Client-side tools are declared in `session.tools` and
invoked with `tool.call`, which blorb answers with `tool.result` after
running the tool through its existing tools registry. AssemblyAI's
managed conversational model is the LLM; blorb's own engine does not
participate, so the agent's `model` entry is not used and no LLM client
is built. Tool execution, wire logging and config-driven tool
declarations are the parts that reuse existing blorb machinery.

Not in scope: the Streaming STT product, TTS-only output, wake words,
memory files, Prefactor tracing and judge runs (both are engine
features; the engine is not involved). A later stage can add
`session.resume` reconnect handling.

Design decisions worth knowing before reading the stages:

- The agent's config in `blorb.json` gains an optional `voice` block:
  `api_key_env`, `greeting`, `voice` (the AssemblyAI voice id),
  `volume`, and optional `input`/`output` audio commands. The block's
  presence enables the `voice` command for that agent, exactly as the
  `band` block enables `band`.
- Audio capture and playback shell out to subprocesses (`arecord`,
  `aplay`) configured in `blorb.json`, so no audio library dependency is
  added. Tests inject the commands.
- The half-duplex echo gate from the voice-agent work (ignore the mic
  while agent audio is still queued to play) is deliberately omitted:
  the API's `interrupt_response` handles barge-in, and a gate would
  fight it. Headphones remain the answer for a laptop mic and speaker
  in the same room.
- `internal/voice` is one package holding the client (protocol, socket
  lifecycle) and the command frontend, mirroring how `internal/band`
  is organised (a socket client plus run/room frontends) and reusing
  `internal/ws` for the connection itself.

Protocol facts the client must honor (verified live by the
voice-agent workshop client and confirmed against the current API
docs):

- Auth is `Authorization: Bearer <key>` on the WebSocket handshake.
- Config goes in `session.update` nested under `session`; the server
  echoes it back under `config` in `session.ready`. Sending `config`
  instead of `session` silently yields an agent with an empty prompt.
- Inbound agent audio is `reply.audio` with the audio in the `data`
  field. User audio goes up as `input.audio` with the audio in the
  `audio` field. The fields are not symmetric.
- `transcript.user.delta` text is the full transcript so far for the
  item, superseding the previous delta for the same `item_id`; it is
  not an append. `transcript.agent.delta` carries the next word in
  `delta` and is an append.
- `tool.result`'s `result` field must be a JSON-encoded string, not a
  nested object, and `tool.call`'s `arguments` arrives as an already
  parsed object.
- Results are sent when `reply.done` is the latest received event, not
  immediately on `tool.call`; on `reply.done` with `status:
  "interrupted"` pending results are discarded. `reply.started` and
  `input.speech.started` mark a turn in flight, holding results.
- `input.audio` must not be streamed faster than real time
  (`audio_rate_violation`); mic audio read from a live subprocess
  capture is inherently real time.
- `session.end` is sent on intentional hang-up so billing stops at
  once; a bare socket close leaves a billable 30-second grace window.
  The server answers with `session.ended`, then closes.

## Todo

- [x] Commit 1: ws dial headers
- [x] Commit 2: voice config schema
- [x] Commit 3: audio subprocesses
- [x] Commit 4: voice client protocol
- [ ] Commit 5: voice session loop
- [ ] Commit 6: voice command
- [ ] Commit 7: example agent and docs

---

## Commit 1: ws dial headers

> `ws.Dial` sends a fixed set of handshake headers. The Voice Agent API
> authenticates with `Authorization: Bearer <key>`, and the Streaming
> STT API (a possible later addition) uses a bare `Authorization: <key>`
> header, so `internal/ws` needs a way to add headers to the opening
> handshake.
>
> Add a `WithHeader(key, value string) DialOption` to
> `internal/ws/client.go`: it appends one header line to the request in
> `dialSettings` (add a `headers [][2]string` field, applied after the
> fixed headers). Reject an empty key with a panic-free approach
> consistent with how the package treats misuse: `Dial` returns an
> error for a header with an empty key.
>
> Files: `internal/ws/client.go` for the option; extend the existing
> tests in `internal/ws/client_test.go` (and the fake server in
> `internal/ws/server_test.go` if it needs to assert request headers,
> keeping the existing test-server style). Tests: a dial with
> `WithHeader("Authorization", "Bearer k")` reaches the server with
> that header (the test server captures request headers, or a new test
> server variant does); a dial with an empty key name returns an error
> from `Dial`.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan
> file, except to check off your item in the Todo list at the top when
> done.

## Commit 2: voice config schema

> The agent definition in `blorb.json` needs an optional `voice` block,
> mirroring how `band` is an optional block on an agent. Its presence
> lets the `voice` command serve that agent.
>
> In `internal/config/config.go` add a `VoiceConfig` struct with JSON
> fields: `api_key_env` (required string: the environment variable
> holding the AssemblyAI API key), `greeting` (optional string: what
> the agent says on connect, spoken verbatim without the LLM), `voice`
> (optional string: AssemblyAI voice id, empty means the API default),
> `volume` (optional number 0-100, empty means the voice's native
> level), `input_command` (optional string array: the audio capture
> command; empty means a default of `["arecord", "-q", "-f", "cd",
> "-r", "24000", "-c", "1"]`, which emits 24 kHz 16-bit mono PCM on
> stdout), `output_command` (optional string array: playback command;
> empty defaults to `["aplay", "-q", "-r", "24000", "-f", "s16_le",
> "-c", "1"]`, which plays raw 24 kHz 16-bit mono from stdin), and
> `ws_url` (optional string: the WebSocket endpoint, empty defaults
> to `wss://agents.assemblyai.com/v1/ws`; useful for tests and the EU
> host).
>
> Add a `Voice *VoiceConfig` field to `Agent` (json `voice,omitempty`).
> Add a `validate` method on `VoiceConfig`: `api_key_env` required and
> non-empty; `volume` when set must be within 0-100; when volume is
> represented as a pointer, an explicit 0 must survive validation (it
> is a valid silence level). Follow the validation style of
> `BandConfig.validate` (internal/config/config.go:418) and call it from
> the agent validation path the same way band's is called. Add accessor
> helpers with the package's `*OrDefault` naming pattern:
> `InputCommandOrDefault`, `OutputCommandOrDefault`, `WSURLOrDefault`.
>
> Tests in `internal/config/config_test.go` following the existing
> band-config test cases: a valid voice block loads; missing
> `api_key_env` fails validation with its name in the message;
> out-of-range volume fails; explicit `"volume": 0` validates;
> defaults come back from the accessors when fields are absent.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan
> file, except to check off your item in the Todo list at the top when
> done.

## Commit 3: audio subprocesses

> Mic capture and speaker playback run as subprocesses configured by
> the voice block. This stage builds the two small executors, with no
> AssemblyAI involvement yet, so they can be tested on their own.
>
> Create `internal/voice/audio.go` with two constructors:
> `startCapture(ctx, command []string) (*audioReader, error)` runs the
> command and returns a reader for its stdout (the command must produce
> raw 24 kHz 16-bit little-endian mono PCM; the defaults from the
> config do), and `startPlayback(ctx, command []string) (*audioWriter,
> error)` runs the command and returns a writer for its stdin
> (consuming the same format). Both types implement `io.Closer`, which
> kills the process and waits for it, and both surface non-zero exit as
> an error from Close. A process that exits while the session runs
> surfaces as an error on the next read or write.
>
> Follow the package's existing subprocess style: look at how
> `internal/tools` executes command tools (command construction,
> process groups, error text) and match its conventions. If the tools
> package sets `Setpgid` or kills the process group, do the same so a
> hung `arecord` dies with the session.
>
> Tests in `internal/voice/audio_test.go`: capture with
> `["sh", "-c", "printf 'abc'"]` yields those bytes and a clean close;
> a capture command that fails to start (bad binary path) errors;
> playback with `["cat"]` receives what was written to it (the test
> writes, closes, and reads the child's output through a pipe of the
> test's own, or uses `["sh", "-c", "cat > file"]` against a temp file
> the test then reads); a playback command that exits non-zero reports
> the exit status from Close; closing twice is safe.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan
> file, except to check off your item in the Todo list at the top when
> done.

## Commit 4: voice client protocol

> This stage builds the AssemblyAI Voice Agent WebSocket client: dial,
> send `session.update`, stream audio both directions, decode every
> server event, and manage tool-call timing. No audio subprocesses and
> no engine yet; the client takes an `io.Reader` of mic PCM and an
> `io.Writer` for agent PCM, both injected, plus a callback for each
> decoded event.
>
> Create `internal/voice/protocol.go` with the wire types, one struct
> per message, JSON-tagged: client-sent `sessionUpdate` (nested
> `session` carrying `system_prompt`, `greeting`, `tools` (built from
> `[]llm.Tool`, which already has name, description, parameters
> JSON-schema fields), `input.format`, `output.voice`, `output.format`,
> `output.volume`), `inputAudio` (`audio` string), `toolResult`
> (`call_id`, `result` string, `is_error` bool), `sessionEnd`; and
> server-sent events decoded into a tagged union or a single struct with
> an omitempty-everything shape: `session.ready` (`session_id`),
> `session.updated`, `session.ended` (`session_duration_seconds`,
> `audio_duration_seconds`), `session.error` (`code`, `message`),
> `input.speech.started`, `input.speech.stopped`,
> `transcript.user.delta` (`item_id`, `text`), `transcript.user`
> (`text`), `reply.started` (`reply_id`), `reply.audio` (`data`),
> `transcript.agent.delta` (`delta`), `transcript.agent` (`text`,
> `interrupted`), `reply.done` (`reply_id`, `status`), `tool.call`
> (`call_id`, `name`, `arguments` json.RawMessage).
>
> Create `internal/voice/client.go` with a `Client` shaped like
> `band.Socket`: `Connect(ctx, cfg ClientConfig) (*Client, error)`
> dials via `ws.Dial` with `WithHeader("Authorization", "Bearer "+
> key)`, sends the `session.update`, waits for `session.ready`
> (erroring clearly on `session.error` before ready), and starts a read
> loop goroutine. `ClientConfig` carries the WS URL, API key, system
> prompt, greeting, voice, volume, tool definitions, a `logging.Sink`
> for best-effort wire logging (reuse the `band.Socket.logWire`
> pattern), and a `runTool func(ctx, name string, args json.RawMessage)
> (output string, err bool)` callback. The sink receives both
> directions as `voice-request` / `voice-response` records with the
> JSON body.
>
> The event callback model: `Client.Events() <-chan Event` for the
> transcript and lifecycle events the console renderer consumes
> (`Event` carries the decoded server event), plus the client itself
> owns reply.audio (decoding base64 PCM and writing it to a playback
> writer, see below) and tool.call (running the callback). The
> constructor takes the playback writer and mic reader: mic streaming is
> a goroutine reading the reader in chunks of 2400 bytes (50 ms at 24
> kHz 16-bit mono; chunks must not exceed 1000 ms or stream faster than
> real time), base64-encoding each chunk into an `input.audio` message;
> the goroutine stops on ctx done, reader error, or socket death. On
> `reply.audio` the client decodes `data` and writes to the playback
> writer, best-effort: a playback write error ends the session with
> that error (a dead speaker cannot be worked around).
>
> Tool timing state machine, per the client-side tools contract: keep
> `lastEvent` (updated on `reply.started`, `input.speech.started`,
> `reply.done`) and a `pending` list of completed-but-unsent results
> keyed by `call_id`. On `tool.call` run the tool (via the callback,
> with a context that dies with the session), append the result to
> `pending`, then flush if `lastEvent` is `reply.done`. On `reply.done`:
> if status is `interrupted`, clear `pending`; else flush. The result
> string is JSON-encoded (`json.Marshal` of a wrapper object or of a
> string value; the API wants a JSON string either way), `is_error`
> set from the callback's failed flag. A `tool.call` for a tool name
> the registry does not know still gets a `tool.result` with an error
> JSON string, so the server-side conversation recovers.
>
> `End()` sends `session.end`, waits for `session.ended` (bounded, for
> example 5 seconds), then closes the socket; `Close()` closes the
> socket without the handshake (the failure path). The client surfaces
> a terminal error on a channel like `band.Socket.Done()`.
>
> Tests in `internal/voice/client_test.go` using a fake Voice Agent
> server modeled directly on
> `internal/band/socket_fake_test.go` (plain TCP listener, ws upgrade,
> scripted messages): connect sends `session.update` with the system
> prompt, greeting, voice, and tool declarations, all under `session`;
> mic bytes stream up as base64 `input.audio` in order; a scripted
> `reply.audio` decodes onto the playback writer; `transcript.user` and
> `transcript.agent.delta` events arrive on the events channel with
> fields intact; a scripted `tool.call` runs the callback and, after a
> scripted `reply.done`, sends `tool.result` with a JSON-string result
> and matching `call_id`; `reply.done` with status `interrupted`
> discards pending results; `session.error` before `session.ready`
> fails `Connect` with the code in the message; `End()` sends
> `session.end` and returns after the scripted `session.ended`. Also
> unit-test the pure encode/decode helpers where useful (the user-delta
> supersede rule lives in the renderer, not the client, but a decode
> round trip on each event type belongs here).
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan
> file, except to check off your item in the Todo list at the top when
> done.

## Commit 5: voice session loop

> This stage wires the client, audio subprocesses, tools registry and
> console renderer into the session loop the command runs, still inside
> `internal/voice`.
>
> Create `internal/voice/session.go` with `Run(ctx, opts Options)
> error`. `Options` carries: the resolved `config.Agent`, the
> `config.Config`, the resolved API key (resolved by the caller, the
> command, via the getenv pattern the other commands use), the
> `logging.Sink` (resolved by the caller through the same
> `chat.ResolveSink` path the band command uses), `Stdout io.Writer`,
> a `Version` string for a banner, and `NoMic bool` (skip microphone
> capture; the session is agent-talk-only, for testing the output
> path). The function:
>
> 1. Builds the tools registry from the agent's granted tools the same
>    way `band.NewRoom` does (internal/band/room.go:154-233 is the
>    reference: `tools.NewRegistry(config.AgentTools(agent), sink and
>    config-dir options)`). Subagent and decider tools cannot run in a
>    voice session: a subagent tool runs a nested engine turn and a
>    decider makes decision-model calls, and neither LLM path exists
>    here. Validate at startup and error out listing the offending tool
>    names, so a config that grants them to a voice agent fails loudly
>    rather than silently running a reduced agent; the definitions sent
>    in `session.tools` are then exactly the registry's. (The
>    registry's own `Run` stays the execution path for everything it
>    does carry; wire its per-tool timeout through to the client
>    callback so a hung tool surfaces as a failed result rather than a
>    stuck session.)
> 2. Starts the capture and playback subprocesses from the voice
>    block's commands.
> 3. Connects the voice client with the agent's system prompt, greeting
>    and voice, and the tool definitions.
> 4. Prints a banner (blorb version, agent name, voice session) and a
>    hint that Ctrl-C hangs up.
> 5. Renders the event stream to stdout live: partial user speech from
>    `transcript.user.delta` printed on one updating line (the delta is
>    the full-so-far text, so each delta rewrites the line; carriage
>    returns or ANSI clearing are both acceptable, but the line must
>    settle to the final text and end with a newline on
>    `transcript.user`), the agent's words from
>    `transcript.agent.delta` appended word by word in step with the
>    audio (with a newline at `transcript.agent`), and tool activity:
>    on `tool.call` print the tool name and arguments, and the
>    result when the client callback completes it (success summary
>    versus error, mirroring how `chat.Events` gates result bodies is
>    not needed here - print the result body; voice sessions are
>    single-agent and the console is the only observer).
>
> Interruptions: on `reply.done` with status `interrupted`, print a
> short marker after the agent's partial line. On `session.ended`,
> print the session duration the event carries. On a terminal client
> error, print it and return a non-nil error.
>
> SIGINT: first Ctrl-C sends `session.end` and waits for the session
> to wind down (the client's `End`), second Ctrl-C force-closes
> (the client's `Close`). Follow the signal handling pattern in
> `internal/chat/chat.go` (chat.Run's sigint ladder), but simpler: no
> turn cancellation exists here because turns are server-side.
>
> Tests in `internal/voice/session_test.go` with an injected fake
> client: the client package's `Connect` is behind a function field on
> `Options` (like `chat.Options.NewClient`), so the session tests script
> events without a socket. Script: events in, assert stdout matches
> expected rendering (use the golden-style comparisons the chat tests
> use; look at how `chat_test.go` asserts rendered output). Cover: a
> user turn (deltas then final) and an agent reply (deltas then final)
> render as specified; an interrupted reply prints the marker; a tool
> call renders call and result; `session.ended` prints the duration; a
> client terminal error fails the session; SIGINT (injected) ends the
> session through `End`; `NoMic` runs without capture startup.
> Registry exclusion: an agent whose granted
> tools include a subagent tool fails startup with the subagent named.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan
> file, except to check off your item in the Todo list at the top when
> done.

## Commit 6: voice command

> Add the `blorb voice` subcommand to `main.go`, following the band
> command's structure (main.go:398-476 is the reference for config
> load, agent resolve, api key resolve, sink resolve, and error
> exits).
>
> Flags: the shared `--config`/`-c`, `--agent` (agent name, defaults
> to the config's `default_agent`), and `--no-mic` (bool: run without
> microphone capture; mic streaming is skipped, so the session is
> agent-talk-only until the flag is off - useful for testing the
> output path with a text-only user). The command resolves the
> voice block's `api_key_env` against the environment with the
> same error message pattern band uses (`api_key_env %q is set but
> the environment variable is empty`), errors if the selected agent
> has no `voice` block (`agent %q has no voice section`, pointing at
> the example config), resolves the sink via `chat.ResolveSink`, and
> calls `voice.Run`. SIGINT handling stays inside `voice.Run`.
>
> Tests in `main_voice_test.go` following `main_band_test.go`'s
> structure: a run against a config whose agent has a voice block
> invokes the session (the test injects a fake voice `Run` through a
> package-level function variable if that is the established pattern
> in `main_band_test.go`; match it); a run without a voice block errors
> with the agent name in the message; a missing environment variable
> for `api_key_env` errors; `--no-mic` passes through to the session
> options.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan
> file, except to check off your item in the Todo list at the top when
> done.

## Commit 7: example agent and docs

> Document the command and provide a runnable example.
>
> Create `examples/voice/blorb.json`: one agent with a short
> system prompt, one command tool (for example an `echo`-style or
> `date` command tool, reusing the shape of the simple example's
> tools), and a voice block (`api_key_env`: `ASSEMBLYAI_API_KEY`, a
> `greeting`, a `voice` id, the default audio commands, and a comment
> or README note that `ASSEMBLYAI_API_KEY` must be exported). Follow
> the structure of `examples/simple/blorb.json`. Add a short
> `examples/voice/README.md` if the other example directories have
> one; check `examples/band` and match its conventions.
>
> Update `docs/cli.md` with a `blorb voice` section (usage, flags,
> what the session does, Ctrl-C behavior, the `--no-mic` flag) and
> `docs/configuration.md` with the `voice` block reference (fields,
> defaults, validation rules, and a note that a voice agent does not
> use its `model` because the Voice Agent API runs the conversation
> server-side). Match the structure and tone of the existing band
> sections in both files. The intended reader is a developer using
> blorb with no knowledge of the AssemblyAI API: the docs should
> explain that speaking drives the conversation, tools run locally,
> and the LLM is AssemblyAI's, configured through them, not blorb.
>
> Update the module layout list in `AGENTS.md` with
> `internal/voice - the blorb voice frontend: AssemblyAI Voice Agent
> socket client, audio subprocess adapters, and the voiced session
> loop`, matching the entry style of the band line.
>
> Tests: no new unit tests are required for docs and the example
> config; run the config validation against the example by adding the
> example directory to whatever mechanism `examples/simple` uses to
> keep example configs valid (check for a test that loads example
> configs, and if one exists, the new example must pass it).
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan
> file, except to check off your item in the Todo list at the top when
> done.
