# Multiple agents per blorb.json

Today a blorb.json describes exactly one agent: `system_prompt`, `provider`, and `max_turns` sit at the top level, and every tool in `tools` is available to it. This plan restructures the schema so a config describes *named agents*: a top-level `agents` array holds the agent definitions, each carrying its own `name` field, and an optional top-level `default_agent` names the agent commands use when none is given. Each agent owns `system_prompt`, `provider`, and `max_turns` — and instead of declaring tools, lists the names of the top-level tools it can use. Tools are shared vocabulary defined once at the top level and granted per agent, so several agents can share a tool without redefining it. Resolution is strict: an agent listing an unknown tool name is a config error, and `default_agent` must name an existing agent. The top-level `name` field is removed — agent names carry identity, flowing into the chat banner and the Prefactor agent name. This is a clean break in the tool-types tradition: old flat configs stop validating, and all fixtures, tests, the README, and both examples migrate in the same change. Because `Config` loses `SystemPrompt`/`Provider`/`MaxTurns`, everything consuming them — `internal/chat` (engine construction, client factory, banner), `internal/prefactor` example tests, `main.go` — switches to resolving a single `config.Agent` up front and passing it alongside the config. The chat command gains an optional positional `agent` argument: explicit name wins, else `default_agent`, else a clear error listing the available names.

## Todo

- [x] Commit 1: config schema — `agents`, `default_agent`, per-agent tool lists
- [x] Commit 2: per-agent tool access — `AgentTools` resolution and helper API
- [x] Commit 3: chat `agent` positional argument
- [ ] Commit 4: README, examples, and fixture polish

---

## Commit 1: config schema — `agents`, `default_agent`, per-agent tool lists

> Restructure the blorb.json schema in `internal/config/config.go`. New shape:
>
> ```json
> {
>   "default_agent": "main",
>   "tools": [ ...unchanged tool entries... ],
>   "agents": [
>     {
>       "name": "main",
>       "system_prompt": "...",
>       "provider": { ... },
>       "max_turns": 10,
>       "tools": ["echo", "read"]
>     }
>   ],
>   "logging": { ... },
>   "prefactor": { ... }
> }
> ```
>
> - Top level `Config` becomes: `Tools []ToolEntry`, `Agents []Agent` (`json:"agents"`), `DefaultAgent string` (`json:"default_agent,omitempty"`), `Logging`, `Prefactor`, `dir`. Remove `Name`, `SystemPrompt`, `Provider`, `MaxTurns` from `Config` — their accessors move to `Agent` or die (see below).
> - New `Agent` struct: `Name string` (`json:"name"`), `SystemPrompt string`, `Provider Provider`, `MaxTurns int`, `Tools []string` (`json:"tools,omitempty"` — the tool names this agent may use, absent meaning no tools). An `Agent` holds no `dir` and needs none: it only *references* tools by name; builtin `base_dir` validation stays on the top-level `ToolEntry.validate(c.dir)` path, unchanged.
> - New `Agent` methods replacing the `Config` ones: `(a Agent) MaxTurnsOrDefault() int` (same default logic). The `dir` accessor stays on `Config`.
> - Validation (rewrite `Config.Validate`):
>   - `agents` required and non-empty: `agents is required` / `agents must not be empty`
>   - per agent: `name` required and must match `ToolNamePattern` (the same `^[a-zA-Z0-9_-]+$` character class — rename the var to `NamePattern` and alias `ToolNamePattern = NamePattern` if that reads better, or keep one name and document it covers both); empty name → `agent name is required`, bad name → `agent name %q must match ...`
>   - agent names must be unique within the config: `duplicate agent name %q` (the map gave this for free; the array must check it explicitly — iterate in order so the first definition wins the error position)
>   - per agent: `system_prompt` required, `provider` validated as today, `max_turns >= 1` — all error messages prefixed `agent %q: `
>   - per agent: every entry in the agent's `tools` array must be non-empty and match `NamePattern`, and must exist in the top-level `Tools` by name: error `agent %q: unknown tool %q` (and `agent %q: unknown tool %q` for an empty entry). Duplicates within one agent's list are rejected: `agent %q: duplicate tool %q` (top-level uniqueness stays `duplicate tool name %q`)
>   - `default_agent`, when non-empty, must exist: error `default_agent %q is not a defined agent` (keep this check last so a per-agent error surfaces first)
>   - a `default_agent` naming an agent is optional; nothing requires one
>   - top-level `Tools` validation is unchanged (still `tool %q: %w` wrapping and `validateUniqueToolNames`)
>   - an agent with an empty `tools` array is valid (a no-tools agent is a legitimate setup)
> - Unchanged struct details: `Provider`, `ToolEntry`, `LogConfig`, `PrefactorConfig`, `Load` mechanics (strict decode, trailing-data check, `cfg.dir`, `Validate`). Strict `DisallowUnknownFields` decoding recursively rejects unknown fields inside each agent object for free — keep it that way.
> - Tests (`internal/config/config_test.go` + testdata): this is the big migration. Update every fixture and inline literal to the new shape, then add new cases:
>   - `agents_missing.json` → `agents is required`
>   - `agents_empty.json` (`"agents": []`) → `agents must not be empty`
>   - `agent_missing_name.json` → `agent name is required`
>   - `agent_bad_name.json` (e.g. `"name": "has space"`) → the name-pattern error
>   - `duplicate_agent_names.json` → `duplicate agent name`
>   - `agent_missing_system_prompt.json`, `agent_invalid_provider.json`, `agent_bad_max_turns.json` → errors prefixed `agent "..."...`
>   - `agent_unknown_tool.json` → `unknown tool`
>   - `agent_duplicate_tool.json` → `duplicate tool`
>   - `default_agent_unknown.json` → `is not a defined agent`
>   - `agents_multi_valid.json` — the canonical new-shape fixture: two agents sharing one top-level tool, only one of which uses a second tool; one with `"tools": []`; assert the parsed `Agents` slice contents (including `Name` fields), `DefaultAgent`, and per-agent `MaxTurnsOrDefault()` defaulting
>   - a valid config with no `default_agent` — assert `DefaultAgent == ""` and that `Load` succeeds (proving the field is optional)
>   - keep exercising every pre-existing branch (logging validation, prefactor validation, provider types, tool-type validation) — just carried over into an agent
> - Update the consumers of the removed fields so the tree compiles, `bin/qc` passes, and no consumer reads agent fields off `Config` anymore. Everything that breaks lives in `internal/chat` (chat.go reads `Config.SystemPrompt`, `Config.Provider`, `Config.MaxTurns` at chat.go:106-112, chat.go:211, chat.go:327, chat.go:558-591, and `chat_test.go` builds `config.Config` literals throughout), `main.go` (banner-adjacent resolution and `buildPrefactorTracer`'s `cfg.Name`), and `internal/prefactor/example_test.go` (`cfg.Name`). Make the full stage-2 design change now rather than a shim — a throwaway intermediate shape would be more work than the real thing:
>   - `chat.Options` gains `Agent config.Agent`; `Run` builds the client factory inputs, engine config (`SystemPrompt`, `MaxTurnsOrDefault`), tracing model, and banner from `opts.Agent` (the banner uses `opts.Agent.Name` — with the name inside the definition there is no separate `AgentName` field to drift out of sync). In this commit the registry is still built from all of `opts.Config.Tools` — per-agent tool scoping deliberately lands in the next commit (the intermediate state mirrors the tool-types plan's staged rollout; call it out in nothing but this plan).
>   - `NewClientWithGetenv`/`NewClient` take `(cfg config.Config, agent config.Agent, ...)` reading `agent.Provider`; the injected `NewClient` in `Options` follows suit.
>   - `main.go`'s chat Action resolves the agent after `Load`: this commit has no positional argument (that lands in its own commit), so resolve `cfg.DefaultAgent`; when empty, fail `chat: no agent given and no default_agent configured (available: main, helper)` (names sorted); then scan `cfg.Agents` for a matching `Name` and fail `chat: agent %q is not defined in the config` when absent. `buildPrefactorTracer` takes the resolved agent and uses `agent.Name` for `TracerConfig.AgentName`. A raw slice scan is fine here; the helper API lands next commit.
>   - `chat_test.go` migrates via a small `testAgent()`/`testConfig()` helper pair used by every test that builds a `Config` or calls `Run`; `internal/prefactor/example_test.go` migrates its example loads to the new shape with single-agent configs.
> - Because `internal/prefactor/example_test.go` loads the *shipped* example configs, both `examples/simple/blorb.json` and `examples/prefactor-tracing/blorb.json` must migrate to the new shape in this commit too — a minimal mechanical conversion (one agent per config, the agent named as the old top-level `name`, the old `name`/`system_prompt`/`provider`/`max_turns`/`tools` moving into it, `default_agent` set to it) — or `bin/qc` cannot pass. The examples' READMEs and prose polish stay in the final commit.
> - `bin/qc` must pass with no skipped or weakened assertions.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 2: per-agent tool access — `AgentTools` resolution and helper API

> Stage 1 moved the agent-scoped settings; this stage enforces the per-agent tool list. The pattern: `config` resolves, callers select.
>
> - Add to `internal/config`:
>   - `(c Config) Agent(name string) (Agent, bool)` — slice scan by `Name`
>   - `(c Config) DefaultAgentName() (string, bool)` — `DefaultAgent` and its presence
>   - `(c Config) AgentTools(a Agent) []ToolEntry` — the top-level `ToolEntry` values whose `Name` is in `a.Tools`, in the agent's listed order (not the top-level declaration order), so tool listing order is the agent author's. An empty result is valid (a no-tools agent).
> - `internal/chat`: `Run` builds the registry from `config.AgentTools(opts.Config, opts.Agent)` instead of all of `opts.Config.Tools` (internal/chat/chat.go:81). The registry keeps the same sink/config-dir wiring; `WithConfigDir(opts.Config.Dir())` is unchanged (base_dir resolution stays anchored to the config file, which is where the user's tools live).
> - `main.go`: replace the raw slice scans from stage 1 with `cfg.Agent(name)` and `cfg.DefaultAgentName()`, keeping the same error messages.
> - Tests:
>   - `internal/config`: `AgentTools` returns exactly the agent's listed tools in list order (an agent listing tools out of top-level declaration order pins the ordering choice), returns an empty slice for a no-tools agent, and misses nothing when all top-level tools are granted.
>   - `internal/chat`: a session given agent A's tools gets only those tools in the registry — assert via a fake client capturing `llm.Request.Tools` that a shared tool absent from the agent's list never reaches the model, and that granted tools arrive in the agent's listed order. `Agent`/`DefaultAgentName` lookup helpers get direct config tests (present, absent, default set, default unset).
>   - `main.go` behavior is covered by its stage-1 tests plus stage 3's argument tests; no new main tests here.
> - Grep sweep: `Config.SystemPrompt|Config.Provider|Config.MaxTurns|cfg.Name` must have no remaining hits outside `config` itself and the `Agent` methods.
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 3: chat `agent` positional argument

> `blorb chat [agent [flags]]` — an optional positional argument naming the agent.
>
> - `main.go` `chatCommand`: add `Arguments: []cli.Argument{&cli.StringArg{Name: "agent"}}` to the command struct (urfave/cli v3's positional-argument mechanism; an argument with `Min` unset/zero is optional — a bare `blorb chat` must stay valid, and `AnyArguments`-style multi-arg is *not* what we want). The value is read via `cmd.StringArg("agent")`, empty when absent (verified against the v3 API: `ArgumentBase.Parse` consumes at most one token and leaves `Value` untouched when none). Document it in `Usage`/`UsageText`: `blorb chat [command options] [agent]`.
> - Resolution order in the `Action`: positional `agent` if present; else `cfg.DefaultAgentName()`; else fail with the stage-1 message `chat: no agent given and no default_agent configured (available: main, helper)`. Then `cfg.Agent(name)`, failing `chat: agent %q is not defined in the config` when missing. `DefaultAgentName` remains the API for the default leg.
> - `main_test.go`: update `TestChatCommandHasNoNewFlags` (the positional arg is not a flag — keep asserting exactly the two flags); add tests using a fake-server strategy so runs are cheap and fully observable: point `provider.base_url` at an `httptest.Server` returning a canned chat-completions response, run the session over a `bytes.Buffer` stdin (`"hi\nexit\n"`), and assert the banner shows the resolved agent name and model:
>   - temp config with agents `{alpha, beta}`, `default_agent: "beta"`: no arg → banner contains `beta`
>   - arg `alpha` → banner contains `alpha`
>   - arg `gamma` → exits with the not-defined error mentioning `gamma`
>   - config with no `default_agent`, no arg → exits with the available-agents error, asserting both `alpha` and `beta` appear in the message
>   - config with no `default_agent`, arg `alpha` → banner contains `alpha`
>   - a second `main_test.go` case may share one fake server across subtests; keep the table style of the existing tests
> - `README.md` usage section is *not* touched here (stage 4 owns docs); but keep any README usage examples compiling in docs-tests if such a mechanism exists (grep for one; none is expected).
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.

## Commit 4: README, examples, and fixture polish

> Documentation pass for the new schema. Match existing README tone and structure; no manual line wrapping.
>
> - `README.md`:
>   - Overview/features: "define *agents*" plural where it currently says one agent; mention running a chosen agent per invocation.
>   - The configuration section: new canonical JSON example with `agents` (two named agents sharing a top-level tool), `default_agent`, and per-agent `tools` arrays. Update the top-level fields table: remove `name`, `system_prompt`, `provider`, `max_turns` rows; add `agents` (required), `default_agent` (no), `tools` (no, top-level declarations). New "Agents" subsection between Provider and Tools: the per-agent fields table (`name`, `system_prompt`, `provider`, `max_turns`, `tools` — the *names* of top-level tools the agent may use; listed order is the agent's; an agent listing an unknown tool is a config error; an empty list is a no-tools agent), a note that agent names must match the name pattern, be unique, and are what the chat command takes.
>   - Chat usage: `blorb chat [agent]` examples — explicit agent, default via `default_agent`, and the no-default error behavior.
> - `examples/simple/blorb.json` and `examples/prefactor-tracing/blorb.json`: both were mechanically migrated in commit 1 — verify their shapes read well as *documentation* and polish (e.g. make `examples/simple` a two-agent config showing the shared-tools story if that reads better, keeping `default_agent: "simple"` so `./blorb chat --config examples/simple/blorb.json` runs exactly as before; the prefactor example stays single-agent with its agent named e.g. `tracey` and `default_agent` set). Any content changes must keep `internal/prefactor/example_test.go` passing. Update `examples/simple/README.md` and `examples/prefactor-tracing/README.md` wherever they name flat config fields.
> - `internal/config/testdata/valid.json`: make it the canonical fixture (two agents, shared tools, `default_agent`) if stage 1's `agents_multi_valid.json` already covers that, fold them together and keep exactly one canonical valid fixture, with `config_test.go` assertions updated.
> - `internal/prefactor/example_test.go`: final polish so the assertions read naturally against the finished examples (assert the agent's `Name` and `DefaultAgent` explicitly rather than whatever commit 1's mechanical migration left, prefactor block still present/absent respectively).
> - Review pass: read the finished README sections and both examples against the shipped code (banner format, `agent` usage line, error messages) and fix any drift. Confirm `bin/qc` passes and the whole tree has no stale references to the old flat shape (grep `default_agent`, `"agents"`, and old field names in docs/examples one last time).
>
> Verify with `bin/qc`. Do not commit. Do not create or modify any plan file, except to check off your item in the Todo list at the top when done.