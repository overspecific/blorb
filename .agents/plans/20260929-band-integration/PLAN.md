# Band integration (blorb as a remote agent on band.ai)

Band (band.ai) is a multi-agent chat platform. A blorb agent can join it as a remote agent: blorb connects out to Band with an agent API key, receives @mention messages from chat rooms, runs them through the normal blorb engine, and replies by calling Band platform tools - the LLM answers a room by calling `band_send_message`, the same design Band's own SDK uses. This plan adds a new long-running frontend, `blorb band`, plus the plumbing it needs: a `band` section in blorb.json, a hand-rolled WebSocket client (RFC 6455, no new dependency), a Phoenix Channels protocol layer, a REST client for Band's Agent API, a new `band` tool type wired in by the band command (not declared by users in blorb.json), a per-room runtime with in-memory history seeded from Band's context endpoint, and a runner that handles startup sync, live event delivery, and reconnect.

Band has two APIs, both needed: the Request API (REST, client to platform: send messages, manage participants, mark messages processed) and the Subscriptions API (WebSocket, platform to client: push delivery of @mentions). Protocol references used throughout: <https://docs.band.ai/integrations/custom-integration.md>, <https://docs.band.ai/websocket/overview.md>, <https://docs.band.ai/api/agent-api.md>.

Out of scope for v1: contacts (contact request events are ignored), room tasks (beta), memories (locked feature), file upload/download, message attachments, delegation, and the `room_participants` channel (participants are fetched on demand instead). One connection per agent id exists (the platform's last-connection-wins policy), so one `blorb band` process serves one Band agent.

## Todo

- [x] Commit 1: config band section
- [x] Commit 2: internal/ws frame codec
- [x] Commit 3: internal/ws client dial and lifecycle
- [x] Commit 4: internal/band Phoenix socket
- [x] Commit 5: internal/band REST client core
- [x] Commit 6: internal/band REST client collaboration endpoints
- [x] Commit 7: band tool type and executor wiring
- [ ] Commit 8: internal/band room runtime
- [ ] Commit 9: internal/band runner
- [ ] Commit 10: blorb band command, docs, example

---

## Commit 1: config band section

Add an optional top-level `band` object to blorb.json in `internal/config/config.go`, following the existing `prefactor` block's pattern exactly:

```json
"band": {
  "agent_id": "uuid-from-band",
  "api_key_env": "BAND_API_KEY",
  "rest_url": "https://api.band.ai",
  "ws_url": "wss://app.band.ai/api/v1/socket/websocket"
}
```

New type `BandConfig` with fields: `AgentID string` (json `agent_id`, required when the block is present, non-empty), `APIKeyEnv *string` (json `api_key_env`, pointer per the api_key_env convention: absent defaults to a new `DefaultBandAPIKeyEnv` constant `"BAND_API_KEY"`, explicit empty is a config error), `RESTURL string` (json `rest_url`, optional, default `DefaultBandRESTURL` = `"https://api.band.ai"`), `WSURL string` (json `ws_url`, optional, default `DefaultBandWSURL` = `"wss://app.band.ai/api/v1/socket/websocket"`). Add `Band *BandConfig` to `Config` with json tag `band,omitempty`, plus accessor methods `APIKeyEnvOrDefault`, `RESTURLOrDefault`, `WSURLOrDefault` mirroring the prefactor ones, and a `BandEnabled()` helper reporting whether the block is present. Validation in `Config.Validate` next to the prefactor check: agent_id required and non-empty; rest_url when set must parse as http/https with a host; ws_url when set must parse as ws/wss with a host. Reject unknown fields as usual (the top-level DisallowUnknownFields already covers this).

Tests in `internal/config/config_test.go` (or a new `config_band_test.go` if the file is large): valid minimal block defaults (absent api_key_env -> BAND_API_KEY, absent URLs -> defaults); missing agent_id rejected; explicit empty api_key_env rejected; bad schemes rejected (rest_url with ws scheme, ws_url with http scheme); absent block validates fine; a block with all fields set round-trips through Load with correct values. Follow the existing test naming and table style.

> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 2: internal/ws frame codec

Create a new package `internal/ws` implementing the RFC 6455 frame layer, no dependencies beyond the standard library. Files: `internal/ws/frame.go` and `internal/ws/frame_internal_test.go` (internal test package, so the codec's unexported functions can be exercised directly).

`frame.go` defines `Opcode` constants (continuation, text, binary, close, ping, pong) and a `Frame` struct: `FIN bool`, `Opcode Opcode`, `Payload []byte`. Two codec functions over `io.Reader`/`io.Writer`:

- `readFrame(r io.Reader, maxLen int64) (Frame, error)`: parses the two-byte fixed header, the 7-bit / 16-bit / 64-bit extended lengths, and the mask bit. A masked frame is unmasked with the 4-byte key. Enforces: RSV bits must be zero (fail with a clear error naming the bit), control frames (close, ping, pong) must have FIN set and payload at most 125 bytes, `maxLen` bounds the payload size (an over-size frame is an error before any allocation or read of the payload), and the read is a single io.Reader loop honoring short reads.
- `writeFrame(w io.Writer, f Frame, masked bool) error`: serializes the header (FIN, opcode, mask bit, correct length form), generates a random 4-byte mask key when `masked` (crypto/rand), masks the payload, and writes it all. Client code always passes `masked=true`; the server side of tests passes `false`.

Payload lengths use `encoding/binary`; keep the writer and reader allocation-light (a header scratch buffer, no per-frame allocations beyond the payload slice).

Tests: round-trip frames of every length form (0, 125, 126, 65535, 65536 payload bytes) through a `bytes.Buffer` in both masked and unmasked modes; server-sent frames (unmasked) decode; invalid inputs are errors: nonzero RSV, fragmented control frame, control frame over 125 bytes, payload over maxLen, truncated reader (header cut off mid-read, length announced but payload missing). Also verify the mask key differs between two written frames (randomness sanity) and that masking actually transforms the payload (write masked, read raw bytes, confirm they differ from the original, then decode and confirm they match).

> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 3: internal/ws client dial and lifecycle

Add `internal/ws/client.go` plus `internal/ws/server_test.go` and `internal/ws/client_test.go` (external test package `ws_test`).

`client.go` implements the client half of the opening handshake and a message-oriented connection:

- `Dial(ctx context.Context, rawURL string, opts ...DialOption) (*Conn, error)`: parse the URL (scheme `ws` or `wss`, host required; `wss` uses `tls.Dial` with the config's ServerName, `ws` uses `net.Dialer` with the ctx). Send the HTTP upgrade request by hand: method GET, the URL's path plus query, `Host`, `Upgrade: websocket`, `Connection: Upgrade`, `Sec-WebSocket-Key` (16 crypto/random bytes, base64), `Sec-WebSocket-Version: 13`. Read the response with `http.ReadResponse`; anything but 101 is an error carrying the status code and the response's `Sec-WebSocket-Error` or body snippet if present. Verify `Sec-WebSocket-Accept` equals base64(sha1(key + the RFC 6455 GUID)); mismatch is an error. Take over the raw connection.
- `Conn` exposes: `ReadMessage(ctx context.Context) (opcode Opcode, data []byte, err error)`, `WriteText(ctx context.Context, data []byte) error`, `Close(code uint16, reason string) error`, and `SetReadLimit(int64)`. Read is the interesting part: it loops on `readFrame` with a read deadline (configurable via a `DialOption`, default 60s, refreshed on every frame), reassembles fragmented data messages (continuation frames until FIN; interleaved control frames are handled, not buffered), automatically answers any ping with a pong, and converts a close frame into a Go error (`CloseError` carrying code and reason) after which the connection is dead for reads. Binary frames from the server are returned as-is; the caller treats them as unexpected. Writes go through a mutex (one writer at a time), always masked, text opcode. `Close` performs the closing handshake: send a close frame, drain reads briefly for the peer's close, then close the underlying conn; a second Close is a no-op. A `DialOption` also sets the initial read limit (default 1 MiB), which guards against a peer announcing a huge frame.
- Writes and reads honor ctx cancellation through deadline setting on the underlying conn.

`server_test.go` builds a minimal in-process WebSocket server for tests only: `net.Listen` on 127.0.0.1:0, accept, read the upgrade request with `bufio`, verify Sec-WebSocket-Version and compute/return Sec-WebSocket-Accept, answer 101, then speak frames using the codec from commit 2 with `masked=false` on writes and accepting masked client frames on reads. Expose helpers to script a session: await a text frame, send a text frame, send a ping, send fragmented text, send a close, and kill the conn abruptly.

`client_test.go` covers, over both `ws://` (plain listener) and `wss://` (listener wrapped in `tls.Server` with a self-signed cert and an InsecureSkipVerify client tls option added for the test via a DialOption): successful handshake round-trip with a text exchange; server that answers 401 or a bad Accept key fails Dial with a clear error; ping from server is auto-answered (server asserts it received a pong); fragmented server text arrives as one message; close handshake: client Close, server sees the close frame and mirrors it, both sides return cleanly; server-initiated close surfaces as CloseError with the code and reason; abrupt server hangup mid-read returns an error; read deadline expiry returns an error (short deadline option); a server frame over the read limit errors without allocating it.

> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 4: internal/band Phoenix socket

Create package `internal/band` with `socket.go`, `types.go`, and `socket_test.go` (external test package `band_test`).

`types.go` holds the wire vocabulary shared by later stages: `ChatMessage` (ID, Content, SenderID, SenderName, SenderType, MessageType, InsertedAt, Metadata with Mentions), `Mention` (ID, Handle, Name), `ChatRoom` (ID, Title, TaskID, InsertedAt, UpdatedAt), `ChatParticipant` (ID, Type, Role, Status, Handle, Name), and a `Peer` type for lookup results (id, name, handle, type - check <https://docs.band.ai/api/agent-api/agent-api-peers/list-agent-peers.md> for the exact shape).

`socket.go` implements the Phoenix Channels protocol on top of `*ws.Conn`:

- The envelope is a JSON array `[join_ref, ref, topic, event, payload]`; refs are strings, server-initiated events carry `null` join_ref and ref. Define `Event` (Topic, Event name, Payload `json.RawMessage`).
- `Connect(ctx, wsURL, apiKey string) (*Socket, error)` dials `wsURL` with the query parameters `api_key` and `vsn=2.0.0` added (use `url.Values` encoding; the caller passes the base URL from config). If the ws handshake is rejected (HTTP 401), return an error saying the Band agent API key was rejected.
- `Socket` runs one reader goroutine and owns a write mutex over the ws conn. API: `Join(ctx, topic string) error` sends `[joinRef, ref, topic, "phx_join", {}]` and waits for the matching `phx_reply` (status `ok` / error with reason) with a timeout; `Leave(ctx, topic string) error` similarly; `Send(event)` for outgoing pushes if needed later (heartbeats and joins use it); `Events() <-chan Event` delivering server-initiated events (null refs) and non-matching replies are dropped; heartbeat: a ticker goroutine sends `[null, ref, "phoenix", "heartbeat", {}]` every 30 seconds and treats the heartbeat reply as liveness (refreshing the ws read deadline is already implicit in any inbound frame). `Close()` cancels the goroutines and closes the ws conn. On a `phx_error` or `phx_close` message, or a dead ws conn, the events channel closes and a `Done() <-chan error` delivers the terminal error, so the runner (commit 9) can reconnect.
- Ref counters: join_ref is unique per joined channel (an incrementing integer formatted as a string), message ref increments per message sent. Both must be strings or null per the protocol.
- A `WithSocketSink`-style option (constructor arg) takes a `logging.Sink`: every outgoing and incoming JSON envelope is written as a `logging.Record` with kind `"band-request"` (outgoing) or `"band-response"` (incoming), method `"websocket"`, URL the topic, body the JSON. Best-effort per the logging package's contract.

`socket_test.go` uses a fake Band WebSocket server built from the test server in commit 3, scripted per test: join succeeds (assert the client sent a well-formed phx_join, reply ok); join rejected surfaces the reason; heartbeat frames arrive on the server every interval (use a short injected heartbeat interval - make the interval a field settable for tests); server-initiated `message_created` event with null refs is delivered on Events with the payload intact; a `phx_close` from the server closes Events and sets Done; a dropped TCP connection does the same; simultaneous join + inbound event works (no deadlock); sink records are written for both directions.

> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 5: internal/band REST client core

Add `internal/band/client.go` and `internal/band/client_test.go` (external test package).

`Client` wraps the Agent API: constructor `NewClient(restURL, apiKey string, sink logging.Sink) *Client` using an `http.Client` with a sane timeout (30s). Every request sets `X-API-Key`. A private `do(ctx, method, path, reqBody, respBody any) error` helper handles JSON encode/decode, the `{"data": ...}` envelope the API wraps responses in, and error decoding: non-2xx responses parse `{"error": {"code", "message", "request_id"}}` into a `*APIError` (fields Code, Message, RequestID, plus the HTTP status), which is returned as the error. The helper writes a `logging.Record` pair (kind `"band-request"` with method, full URL, body; `"band-response"` with status line and body), best-effort. 204 responses decode as empty.

Endpoints (all under `/api/v1/agent`), with wire details from the docs pages (append `.md` to any page URL for clean Markdown):

- `Me(ctx) (AgentProfile, error)` - GET `/me`, returns the agent's own profile (id, name, description). Used to validate the key at startup.
- `ListChats(ctx) ([]ChatRoom, error)` - GET `/chats`, following the page/page_size pagination metadata until exhausted.
- `CreateChat(ctx, title, taskID string) (ChatRoom, error)` - POST `/chats` with `{"chat": {"title": ..., "task_id": ...}}`, both optional; see <https://docs.band.ai/api/agent-api/agent-api-chats/create-agent-chat.md>.
- `NextMessage(ctx, chatID string) (*ChatMessage, error)` - GET `/chats/{id}/messages/next`; 204 returns `nil, nil`.
- `MarkProcessing(ctx, chatID, messageID string) error` - POST `/chats/{id}/messages/{id}/processing`.
- `MarkProcessed(ctx, chatID, messageID string) error` - POST `.../processed`.
- `MarkFailed(ctx, chatID, messageID, errorText string) error` - POST `.../failed` with `{"error": errorText}`.
- `SendMessage(ctx, chatID, content string, mentions []Mention) (MessageSent, error)` - POST `/chats/{id}/messages` with `{"message": {"content": ..., "mentions": [...]}}`; see <https://docs.band.ai/api/agent-api/agent-api-messages/create-agent-chat-message.md> for the mention entry shape (id or handle, optional name and kind).
- `SendEvent(ctx, chatID, content, messageType string, metadata json.RawMessage) error` - POST `/chats/{id}/events` with `{"event": {"content": ..., "message_type": ..., "metadata": ...}}`; message_type is one of tool_call, tool_result, thought, error, task, attention; see <https://docs.band.ai/api/agent-api/agent-api-events/create-agent-chat-event.md>.
- `Context(ctx, chatID string) ([]ChatMessage, error)` - GET `/chats/{id}/context`, cursor pagination (limit 100, follow `metadata.next_cursor` while `has_more`), results concatenated oldest-first.

Tests against `httptest.NewServer` fakes, one handler per endpoint plus table cases: success shapes decode (including 204 -> nil for NextMessage, and cursor pagination stitching two pages in order); 401/403/404/422 produce `*APIError` with code, message, and status intact; request bodies are byte-checked (mentions serialized correctly, failed body is `{"error": ...}`); the X-API-Key header is sent; sink request and response records are written; CreateChat omits empty fields rather than sending empty strings.

> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 6: internal/band REST client collaboration endpoints

Extend `internal/band/client.go` and `client_test.go` with the room collaboration endpoints:

- `ListParticipants(ctx, chatID string) ([]ChatParticipant, error)` - GET `/chats/{id}/participants`, paginated.
- `AddParticipant(ctx, chatID, participantID, role string) (ChatParticipant, error)` - POST `/chats/{id}/participants` with `{"participant": {"participant_id": ..., "role": ...}}`, role optional (default member); see <https://docs.band.ai/api/agent-api/agent-api-participants/add-agent-chat-participant.md>.
- `RemoveParticipant(ctx, chatID, participantID string) error` - DELETE from `/chats/{id}/participants`; follow the exact method and body at <https://docs.band.ai/api/agent-api/agent-api-participants/remove-agent-chat-participant.md>.
- `LookupPeers(ctx) ([]Peer, error)` - GET `/peers`, paginated; shape from <https://docs.band.ai/api/agent-api/agent-api-peers/list-agent-peers.md>.

Same conventions as commit 5: envelope, APIError, sink records, pagination exhaustion. Tests: each endpoint's success decode, body byte-check (role omitted when empty), error mapping, and pagination. Keep the existing tests passing untouched.

> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 7: band tool type and executor wiring

Wire the seven Band platform tools into the existing tool system, following the subagent tool pattern (a config entry type whose implementation is injected into the registry through an option).

Config changes in `internal/config/config.go`: new constant `ToolTypeBand ToolType = "band"` and a `Band string` field on `ToolEntry` (json `band,omitempty`) naming the platform tool. Tool entries of type `band` are valid only when constructed programmatically by the band command: `supportedToolEntryTypes` (used by config validation) must NOT include `band`, so a user writing `{"type": "band"}` in blorb.json gets "unknown tool type" - the error message for type `band` should name this explicitly ("band tools are wired by the band command"). The other types' validation cases reject a set `Band` field (like they reject `Builtin` on command tools). `NewRegistry`'s dispatch (in `internal/tools/tools.go`) gets a `case config.ToolTypeBand` handled before the default unknown-type error, since registry entries can be programmatically built.

Registry changes in `internal/tools`: new interface `BandExecutor` mirroring `SubagentRunner`: `RunBandTool(ctx context.Context, name string, args json.RawMessage, sink logging.Sink) (ToolResult, error)`; new option `WithBandExecutor(BandExecutor)`; a `bandTool` implementation (own file `internal/tools/band_tool.go`) holding the entry name, its `Band` selector, and the executor, with `run` delegating to the executor and normal registry timeout/output rules applying. Constructing a band tool without an executor set is a registry-build error naming the missing wiring.

Band tool definitions in a new `internal/band/tools.go`: `ToolEntries() []config.ToolEntry` returning the seven entries in a fixed order, each with a short plain-English description and a JSON-schema `ArgsSchema`. Names and argument shapes (mirroring the Python SDK's tool set):

- `band_send_message`: `{content: string (required), mentions: [{id?, handle?, name?}] (required, at least one)}`
- `band_send_event`: `{content: string (required), message_type: one of thought, error, task (required), metadata?: object}`
- `band_get_participants`: `{}`
- `band_add_participant`: `{participant_id: string (required), role?: one of owner, admin, member}`
- `band_remove_participant`: `{participant_id: string (required)}`
- `band_lookup_peers`: `{page?: integer, page_size?: integer}`
- `band_create_chatroom`: `{title?: string, task_id?: string}`

Also in `tools.go`: the executor type `toolExecutor` bound to a `*Client` and a room ID (plus a `onSendMessage func()` callback used by the room runtime in commit 8 to know whether the LLM sent a message this turn), implementing `tools.BandExecutor`. It dispatches on the tool name, decodes args (missing required fields are a failed ToolResult with a clear message, not a Go error - the LLM should see and fix the problem), calls the matching client method, and returns the result as JSON text. `band_send_message` and `band_send_event` act on the bound room; `band_lookup_peers` and `band_create_chatroom` are room-independent (a created room is reported back; the agent is auto-added as owner). The executor for room-bound tools must be constructed per room.

Tests: in `internal/tools`, a registry built with a fake `BandExecutor` executes a band entry through the normal `Run` path (name and args passed through, output and Err surfaced, timeout enforced with a sleeping fake). In `internal/config`, `{"type": "band"}` in blorb.json is rejected, and `Band` set on a command entry is rejected. In `internal/band`, `ToolEntries` returns seven entries passing `NamePattern` with non-empty descriptions and valid JSON schemas; the executor's tests, against httptest fakes from commits 5-6, cover each tool's happy path, args validation failure (missing content, empty mentions, unknown message_type), a 422 from the API surfacing as a failed ToolResult with the server's message so the LLM can retry, and the `onSendMessage` callback firing exactly on successful sends.

> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 8: internal/band room runtime

Add `internal/band/room.go` and `internal/band/room_test.go`: the per-room state machine that turns an incoming mention into a processed message.

`Room` holds, for one chat room: the resolved blorb agent (`config.Agent`), the config, an `*engine.Engine` (built exactly like `run.Run` builds one: client via an injected `NewClient` factory, per-room `tools.Registry` from `cfg.AgentTools(agent)` with the seven band entries from `ToolEntries()` prepended and a room-bound executor wired via `WithBandExecutor`, sink and subagent runner wired like run's), the `*Client`, the room id, this Band agent's own id, a seen-message-id set (at-least-once delivery means duplicates are possible; keep the last 1024 ids per room), usage accounting hooks (the runner passes an `onEvent func(engine.Event) error` in, like chat and run do), and a session-bootstrapped flag.

`Room.Handle(ctx, msg ChatMessage)` implements the processing workflow:

1. Deduplicate by message id; a seen id returns nil immediately.
2. `MarkProcessing`; a 404 skips the message (it moved or was deleted), other errors fail the message.
3. On first handled message for this room in the process lifetime, seed the engine history from `Client.Context`: messages from other participants with message type `text` become `llm` user messages formatted `"<sender name>: <content>"` (sender name falling back to sender id), the agent's own `text` messages become assistant messages, and all non-text messages (tool_call, tool_result, thought, and so on) are skipped - they duplicate what the engine history will hold or are another participant's activity noise.
4. Run one engine turn with the incoming message as the user message (same sender-prefixed format). The turn's event callback posts `tool_call` and `tool_result` events to the room via `Client.SendEvent` (content: tool name and a short summary; metadata: the call arguments / result status), best-effort - a failure to post an event is logged, never fails the turn. Usage events flow to the runner's accounting.
5. Fallback send: if the turn completed, final text is non-empty, and no `band_send_message` executed during the turn (the executor callback tracked it), send the final text as a message mentioning the original sender (mention entry with the sender's id and name; the server prepends the @mention if missing). This is the emergency path Band's docs allow, so a model that answers in plain text still reaches the room.
6. On success `MarkProcessed`; if the turn failed (engine error, too many turns, provider failure), `MarkFailed` with the error text - a MarkFailed failure is logged and the original error returned. A cancelled ctx (shutdown) skips both marks so the platform re-serves the message next start.
7. After the outcome settles, run the agent's end-timing judges on the turn transcript (`llm.FormatTranscript(eng.History())`), mirroring run's judge phase: judge failures are logged, never fatal; a cancelled ctx skips judges.

`Room.Close()` closes the registry (releasing builtin sandbox roots). The system prompt is the agent's own plus a short appended band preamble (a `bandPreamble()` function: you are one participant in a multi-agent chat room; you reply by calling band_send_message with @mentions of the participants you are addressing; find participants with band_get_participants or band_lookup_peers; plain text answers are delivered as a fallback only). `Room` is safe for use by a single goroutine (the runner serializes per room, commit 9).

Tests, using the existing fakes (fake LLM client with canned responses, httptest Band REST fake, subagent and judge runners wired like run's tests): a mention arrives, MarkProcessing is called, one turn runs, the LLM calls `band_send_message`, MarkProcessed is called, and the history format is right; the no-tool-call fallback path sends the final text mentioning the sender; the turn fails -> MarkFailed with the error text; a duplicate message id does not run twice; session bootstrap converts a scripted `/context` page (other user text, own text, own tool_call event, another agent's text) into the expected `[]llm.Message`; tool_call/tool_result events are posted during the turn; judges run after a successful turn and their output does not leak into the room.

> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 9: internal/band runner

Add `internal/band/run.go` and `internal/band/run_test.go`: the long-running frontend loop.

`Options`: `Config config.Config`, `Agent config.Agent`, `Stderr io.Writer`, `NewClient` and `Getenv` injection seams (mirroring run/chat, tests only), `ConfigPath string` (sink resolution via `chat.ResolveSink`, like run/chat), the resolved Band settings (agent id, API key, rest and ws URLs), and a `HeartbeatInterval`/`ReconnectBase`/`ReconnectMax` set of knobs with defaults (30s, 1s, 30s) overridable for tests.

`Run(ctx context.Context, opts Options) error`:

1. Build the REST client; call `Me` to validate the key up front with a clear error (a 401 means the agent API key or agent id is wrong - say so). Fetch the agent's own id (used by rooms to recognize own messages) and name.
2. Connect the socket (`Connect` with the ws URL). On connect, join `agent_rooms:{agent_id}`; call `ListChats`; for each room, create the `Room` state and join `chat_room:{room_id}`.
3. Startup sync per room: loop `NextMessage` until 204, feeding each into the room's processing (this drains messages that arrived while offline, including ones stuck in `processing` from a crash - the platform re-serves them).
4. Live loop: one goroutine per room reads a message channel; socket events are dispatched by topic: `message_created` on `chat_room:{id}` to that room's channel, `room_added` on `agent_rooms` (create state, join the chat_room channel, drain), `room_removed` (cancel the room goroutine, close the room, drop state). Unknown events and topics are ignored (logged at debug level via the sink records already written by the socket).
5. Heartbeat runs in the socket (commit 4). If the socket dies (Done fires, read error, heartbeat timeout): close it, reconnect with exponential backoff (base doubling to max, reset after a successful join round), and on reconnect re-run steps 2-3 (re-join everything, re-drain). The platform's last-connection-wins policy means a stale connection is evicted server-side, so reconnect is always safe. Backoff sleeps honor ctx.
6. Shutdown on ctx cancel: stop accepting new events, let in-flight room processing finish (bounded wait), close all rooms, return nil. A startup error (bad key, unreachable API) returns immediately with the error.

Tests with a combined fake: one `httptest` REST fake plus a scripted fake Band WebSocket server (from commit 3's helper). Cover: startup drain processes two queued messages then goes quiet; a pushed `message_created` flows through processing to a sent reply and MarkProcessed; `room_added` mid-run creates a room and it receives a later message; `room_removed` tears the room down; socket death triggers reconnect and re-drain (assert the second connection's joins and drain with a short reconnect base); ctx cancel finishes in-flight work and returns nil; an invalid key fails fast with the clear message.

> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 10: blorb band command, docs, example

Wire the frontend into the CLI in `main.go`: a `band` subcommand with the shared `--config` flag and the `--agent` argument resolved by the existing `resolveAgent`, mirroring the run and chat commands' flag handling. It loads the config, errors clearly when no `band` section is configured ("band section is required to run the band command; see examples/band"), resolves the API key from the configured environment variable via `os.Getenv`, builds the options (sink via `chat.ResolveSink`, prefactor tracer via the existing `buildPrefactorTracer` if configured, streaming detection like run), and calls `band.Run` with a SIGINT-mapped context (first SIGINT: graceful stop, finishing the in-flight message; a second SIGINT exits immediately, matching chat's interrupt ladder). On clean shutdown it prints the session usage footer (`usage.FormatSession`) to stderr. Exit code 0 on graceful stop, non-zero on startup failure.

End-to-end test in `main_test.go`, following the existing CLI test pattern: a fake LLM backend (httptest `/chat/completions`) plus the fake Band servers, the binary run with `BAND_API_KEY` set in its environment and a config with a `band` section pointing rest_url and ws_url at the fakes; push one `message_created`, assert the reply was sent and the message marked processed, then SIGINT and assert a clean exit and the usage footer.

Docs and example: update `README.md` (command list and a short band section in the style of the existing frontend sections), update the module layout list in `AGENTS.md` with `internal/band` and `internal/ws`, and add `examples/band/blorb.json` plus `examples/band/README.md` (a minimal working agent using the band section, modeled on the other examples' structure, with the README explaining: register an external agent on band.ai, copy the agent UUID and API key, set `BAND_API_KEY`, run `blorb band`). The example README follows the good-english rules; the example config validates.

> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.
