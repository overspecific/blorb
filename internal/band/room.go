package band

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/overspecific/blorb/internal/chat"
	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/engine"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/logging"
	"github.com/overspecific/blorb/internal/prefactor"
	"github.com/overspecific/blorb/internal/tools"
)

// seenLimit is how many message ids a room remembers. At-least-once
// delivery makes duplicates possible; the seen set keeps the last
// seenLimit ids so an old replay is dropped while memory stays bounded.
const seenLimit = 1024

// RoomOptions configure one Room.
type RoomOptions struct {
	Config config.Config
	// Agent is the resolved blorb agent the room runs.
	Agent config.Agent
	// Client is the Band Agent API client, shared across rooms; the
	// room-bound tool executor and every mark call use it.
	Client *Client
	// RoomID is the Band chat room this state machine serves.
	RoomID string
	// AgentID is this Band agent's own id, used to recognize its own
	// messages in seeded history and to join created rooms.
	AgentID string
	// Sink receives wire logs for the client, engine, and nested runs.
	// Nil means no logging.
	Sink logging.Sink
	// Diagnostics receives best-effort operational warnings (failed
	// event posts, judge failures). Nil discards them.
	Diagnostics io.Writer
	// OnEvent receives the turn's engine events (usage accounting).
	// Nil discards them; its errors fail the turn like any onEvent.
	OnEvent func(engine.Event) error
	// NewClient overrides LLM client construction. Tests only; nil
	// builds the real client from the config.
	NewClient func(cfg config.Config, agent config.Agent) (llm.Client, error)
	// Getenv overrides the environment lookup for api_key_env; nil
	// means os.Getenv. Tests only.
	Getenv func(string) string
}

// Room is the per-room state machine turning an incoming mention into a
// processed message: dedup, mark processing, one engine turn with the
// band tools around it, then mark processed or failed. Safe for use by
// a single goroutine; the runner serializes per room.
type Room struct {
	agent       config.Agent
	cfg         config.Config
	eng         *engine.Engine
	client      *Client
	roomID      string
	agentID     string
	registry    *tools.Registry
	diag        io.Writer
	onEvent     func(engine.Event) error
	judgeRunner *engine.JudgeRunner

	// baseClient is the room's unwrapped LLM client; holder delegates to
	// it except during a traced turn, when it wraps baseClient in a
	// tracing client. modelName is recorded on traced LLM spans.
	baseClient llm.Client
	holder     *clientHolder
	modelName  string

	// trace is the process-wide Prefactor session shared by every room;
	// tracer is its tracer. Both are nil when tracing is off.
	trace       *traceSession
	tracer      *prefactor.Tracer
	onTerminate func()

	// sentThisTurn reports whether band_send_message executed during
	// the turn in flight, tracked by the executor's callback. Handle
	// runs serialized, so a plain boolean is enough.
	sentThisTurn bool

	// bootstrapped marks that history was seeded from the context
	// endpoint already, once per process lifetime.
	bootstrapped bool

	seenMu sync.Mutex
	seen   map[string]struct{}
	seenQ  []string
}

// NewRoom builds one room's runtime: the LLM client (through the same
// factory run uses), the per-room registry with the band tools
// prepended and a room-bound executor wired, and the engine.
func NewRoom(opts RoomOptions) (*Room, error) {
	sink := opts.Sink
	if sink == nil {
		sink = logging.NewNop()
	}

	r := &Room{
		agent:   opts.Agent,
		cfg:     opts.Config,
		client:  opts.Client,
		roomID:  opts.RoomID,
		agentID: opts.AgentID,
		onEvent: opts.OnEvent,
		diag:    opts.Diagnostics,
		seen:    make(map[string]struct{}),
	}

	newClient := func(cfg config.Config, agent config.Agent) (llm.Client, error) {
		if opts.NewClient != nil {
			return opts.NewClient(cfg, agent)
		}
		getenv := opts.Getenv
		if getenv == nil {
			getenv = os.Getenv
		}
		return chat.NewClientWithGetenv(cfg, agent, getenv, sink)
	}

	llmClient, err := clientFactory(opts, sink)(opts.Config, opts.Agent)
	if err != nil {
		return nil, fmt.Errorf("build llm client: %w", err)
	}

	executor := NewToolExecutor(opts.Client, opts.RoomID, opts.AgentID, func() {
		r.sentThisTurn = true
	})

	registry, err := tools.NewRegistry(
		append(ToolEntries(), opts.Config.AgentTools(opts.Agent)...),
		tools.WithSink(sink),
		tools.WithConfigDir(opts.Config.Dir()),
		tools.WithBandExecutor(executor),
		tools.WithSubagentRunner(engine.NewSubagentRunner(engine.SubagentRunnerConfig{
			Config:    opts.Config,
			NewClient: newClient,
			Stream:    false,
			Sink:      sink,
		})),
	)
	if err != nil {
		return nil, fmt.Errorf("build tools: %w", err)
	}
	r.registry = registry

	model, ok := opts.Config.Model(opts.Agent.Model)
	if !ok {
		registry.Close()
		return nil, fmt.Errorf("agent %q: model %q is not a defined model", opts.Agent.Name, opts.Agent.Model)
	}
	provider, ok := opts.Config.Provider(model.Provider)
	if !ok {
		registry.Close()
		return nil, fmt.Errorf("model %q: provider %q is not a defined provider", model.Name, model.Provider)
	}

	r.judgeRunner = engine.NewJudgeRunner(engine.JudgeRunnerConfig{
		Config:    opts.Config,
		NewClient: clientFactory(opts, sink),
		Stream:    false,
		Sink:      sink,
	})

	// The engine holds the holder, not the raw client, so a traced turn
	// can install the tracing wrapper without rebuilding the engine.
	r.baseClient = llmClient
	r.holder = &clientHolder{inner: llmClient}
	r.modelName = model.ModelName

	r.eng = engine.New(engine.EngineConfig{
		Client: r.holder,
		Tools:  registry,
		// The system prompt is the agent's own plus the band preamble:
		// how to behave as one participant of a multi-agent room.
		SystemPrompt: opts.Agent.SystemPrompt + "\n\n" + bandPreamble(),
		MaxTurns:     opts.Agent.MaxTurnsOrDefault(),
		Stream:       false,
		AgentName:    opts.Agent.Name,
		Model:        model.ModelName,
		Sampling:     provider.SamplingParams(),
		ToolChoice:   model.ResolvedToolChoice(),
	})

	return r, nil
}

// setTracer installs the shared Prefactor session and the terminate
// callback the runner injects after NewRoom. A nil tracer disables
// tracing for the room.
func (r *Room) setTracer(session *traceSession, tracer *prefactor.Tracer, onTerminate func()) {
	r.trace = session
	r.tracer = tracer
	r.onTerminate = onTerminate
}

// clientFactory returns the LLM client factory shared by the room's
// engine, its subagents, and its judges: the injected NewClient when
// set, else the real path with the injected getenv.
func clientFactory(opts RoomOptions, sink logging.Sink) func(config.Config, config.Agent) (llm.Client, error) {
	return func(cfg config.Config, agent config.Agent) (llm.Client, error) {
		if opts.NewClient != nil {
			return opts.NewClient(cfg, agent)
		}
		getenv := opts.Getenv
		if getenv == nil {
			getenv = os.Getenv
		}
		return chat.NewClientWithGetenv(cfg, agent, getenv, sink)
	}
}

// bandPreamble is appended to the agent's system prompt: how to behave
// as one participant of a multi-agent chat room.
func bandPreamble() string {
	return strings.Join([]string{
		"You are one participant in a multi-agent chat room on the Band platform.",
		"You reply by calling the band_send_message tool, mentioning the participants you are addressing.",
		"Find room participants with band_get_participants and learn who else exists with band_lookup_peers.",
		"A plain text answer is delivered only as a fallback; prefer the tool.",
	}, " ")
}

// Close releases the room's resources: the registry's builtin sandbox
// roots. Safe to call more than once.
func (r *Room) Close() {
	if r.registry != nil {
		r.registry.Close()
		r.registry = nil
	}
}

// Handle processes one incoming mention end to end: deduplicate, mark
// processing, run one turn, post events, and mark processed or failed.
// A returned error means the message failed and the platform is told
// so; nil means it was handled and marked processed. A ctx cancelled
// mid-turn skips both marks so the platform re-serves the message next
// start.
func (r *Room) Handle(ctx context.Context, msg ChatMessage) error {
	if r.seenMessage(msg.ID) {
		return nil
	}

	if err := r.client.MarkProcessing(ctx, r.roomID, msg.ID); err != nil {
		var apiErr *APIError
		if asAPIError(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			// The message moved or was deleted before we claimed
			// it; the platform already re-routed it.
			return nil
		}
		return fmt.Errorf("mark processing: %w", err)
	}

	// One-time history seeding: the first handled message for this
	// room in the process lifetime converts Band's room context into
	// engine history so the model sees what came before.
	if !r.bootstrapped {
		if err := r.seedHistory(ctx); err != nil {
			return fmt.Errorf("seed history: %w", err)
		}
		r.bootstrapped = true
	}

	prompt := senderMessage(msg)
	events := r.turnEvents()

	// Tracing: one turn span per handled message under the shared
	// process session. The tracing client wrapper lasts only for the
	// turn; the holder restores the base client afterward.
	var turn *prefactor.Turn
	if r.tracer != nil {
		if err := r.trace.start(ctx, r.agent.Name, r.registry); err != nil {
			return r.failTracing(ctx, msg, err)
		}
		var startErr error
		turn, startErr = r.tracer.StartTurn(ctx, prompt)
		if startErr != nil {
			return r.failTracing(ctx, msg, startErr)
		}
		r.holder.inner = chat.NewTracingClient(r.baseClient, turn, r.modelName)
		events = chat.TraceEvent(turn, events)
	}

	r.sentThisTurn = false
	final, turnErr := r.eng.RunTurn(ctx, prompt, events)

	if turn != nil {
		r.holder.inner = r.baseClient
	}

	if ctx.Err() != nil {
		// Shutdown: cancel the turn span and skip both marks so the
		// platform re-serves the message next start.
		if turn != nil {
			r.cancelTurn(turn)
		}
		return ctx.Err()
	}

	if turn != nil {
		if err := r.finishTurn(turn, final, turnErr); err != nil {
			if errors.Is(err, prefactor.ErrTerminated) {
				turnErr = err
			} else if turnErr == nil {
				turnErr = err
			}
		}
	}

	if turnErr != nil {
		if errors.Is(turnErr, prefactor.ErrTerminated) {
			// The platform asked us to stop; the instance is marked
			// server-side, so the message is not failed here.
			r.signalTerminate()
			return turnErr
		}
		if err := r.client.MarkFailed(ctx, r.roomID, msg.ID, turnErr.Error()); err != nil {
			r.diagf("mark failed %s: %v", msg.ID, err)
		}
		r.runJudges(ctx, turnErr)
		return turnErr
	}

	// Fallback send: the turn completed with final text and no
	// band_send_message executed, so the model answered in plain text.
	// Mention the original sender; the server prepends the @mention if
	// missing.
	if final != "" && !r.sentThisTurn {
		if _, err := r.client.SendMessage(ctx, r.roomID, final, []Mention{{ID: msg.SenderID, Name: msg.SenderName}}); err != nil {
			r.diagf("fallback send: %v", err)
		}
	}

	if err := r.client.MarkProcessed(ctx, r.roomID, msg.ID); err != nil {
		return fmt.Errorf("mark processed: %w", err)
	}

	r.runJudges(ctx, nil)
	return nil
}

// failTracing maps a tracing failure before the turn starts to the
// message's outcome: a platform terminate stops the process without
// failing the message; anything else marks the message failed so the
// platform is told.
func (r *Room) failTracing(ctx context.Context, msg ChatMessage, err error) error {
	if errors.Is(err, prefactor.ErrTerminated) {
		r.signalTerminate()
		return err
	}
	wrapped := fmt.Errorf("prefactor: %w", err)
	if markErr := r.client.MarkFailed(ctx, r.roomID, msg.ID, wrapped.Error()); markErr != nil {
		r.diagf("mark failed %s: %v", msg.ID, markErr)
	}
	return wrapped
}

// signalTerminate tells the runner the platform asked the process to
// stop. Safe to call more than once.
func (r *Room) signalTerminate() {
	if r.onTerminate != nil {
		r.onTerminate()
	}
}

// finishTurn records the turn's outcome on its span.
func (r *Room) finishTurn(turn *prefactor.Turn, final string, turnErr error) error {
	if turnErr != nil {
		if errors.Is(turnErr, prefactor.ErrTerminated) {
			return turnErr
		}
		return turn.Fail(turnErr)
	}
	return turn.Complete(final)
}

// cancelTurn closes a turn span for a cancelled turn; a tracing failure
// here is logged, not fatal.
func (r *Room) cancelTurn(turn *prefactor.Turn) {
	if err := turn.Cancel(); err != nil && !errors.Is(err, prefactor.ErrTerminated) {
		r.diagf("cancel prefactor turn: %v", err)
	}
}

// seenMessage records a message id and reports whether it was seen
// already, evicting the oldest id past seenLimit.
func (r *Room) seenMessage(id string) bool {
	r.seenMu.Lock()
	defer r.seenMu.Unlock()
	if _, ok := r.seen[id]; ok {
		return true
	}
	r.seen[id] = struct{}{}
	r.seenQ = append(r.seenQ, id)
	if len(r.seenQ) > seenLimit {
		delete(r.seen, r.seenQ[0])
		r.seenQ = r.seenQ[1:]
	}
	return false
}

// hasSeen reports whether a message id was recorded already, without
// recording it.
func (r *Room) hasSeen(id string) bool {
	r.seenMu.Lock()
	defer r.seenMu.Unlock()
	_, ok := r.seen[id]
	return ok
}

// seedHistory converts Band's room context into engine history: other
// participants' text messages become user messages prefixed with the
// sender's name (falling back to the sender id), the agent's own text
// messages become assistant messages, and all non-text messages
// (tool_call, tool_result, thought, and so on) are skipped: they
// duplicate what the engine history holds or are another participant's
// activity noise.
func (r *Room) seedHistory(ctx context.Context) error {
	messages, err := r.client.Context(ctx, r.roomID)
	if err != nil {
		return err
	}
	var history []llm.Message
	for _, m := range messages {
		if m.MessageType != "text" {
			continue
		}
		if m.SenderID == r.agentID {
			history = append(history, llm.NewTextMessage(llm.RoleAssistant, m.Content))
			continue
		}
		history = append(history, llm.NewTextMessage(llm.RoleUser, senderPrefix(m)+m.Content))
	}
	r.eng.SeedHistory(history)
	return nil
}

// senderPrefix renders "<sender name>: " (the name falling back to the
// sender id) for a message from another participant.
func senderPrefix(m ChatMessage) string {
	name := m.SenderName
	if name == "" {
		name = m.SenderID
	}
	return name + ": "
}

// senderMessage renders an incoming mention as the engine's user
// message: the sender prefix plus the content.
func senderMessage(m ChatMessage) string {
	return senderPrefix(m) + m.Content
}

// turnEvents wraps the runner's onEvent with the room's event posting:
// tool_call and tool_result moments are posted to the room as Band
// events (best-effort: a failure is logged, never fails the turn);
// everything else flows through to the caller's accounting.
func (r *Room) turnEvents() func(engine.Event) error {
	return func(ev engine.Event) error {
		switch ev.Kind {
		case engine.EventToolCall:
			payload, err := json.Marshal(map[string]any{"name": ev.Name, "arguments": json.RawMessage(ev.Args)})
			if err != nil {
				payload = []byte(`{}`)
			}
			if eErr := r.client.SendEvent(context.Background(), r.roomID,
				fmt.Sprintf("calling tool %s", ev.Name), "tool_call", payload); eErr != nil {
				r.diagf("post tool_call event: %v", eErr)
			}
		case engine.EventToolResult:
			payload, err := json.Marshal(map[string]any{"name": ev.Name, "failed": ev.Failed})
			if err != nil {
				payload = []byte(`{}`)
			}
			if eErr := r.client.SendEvent(context.Background(), r.roomID,
				fmt.Sprintf("tool %s done", ev.Name), "tool_result", payload); eErr != nil {
				r.diagf("post tool_result event: %v", eErr)
			}
		}
		if r.onEvent != nil {
			return r.onEvent(ev)
		}
		return nil
	}
}

// runJudges runs the agent's end-timing judges on the turn transcript,
// mirroring run's judge phase: judge failures are logged, never fatal;
// a cancelled ctx skips them. turnErr, when non-nil, appends the
// failure note to the transcript so judges review what survived plus
// the failure.
func (r *Room) runJudges(ctx context.Context, turnErr error) {
	if ctx.Err() != nil || len(r.agent.JudgesWhen(config.JudgeWhenEnd)) == 0 {
		return
	}
	transcript := llm.FormatTranscript(r.eng.History())
	if turnErr != nil {
		transcript = turnErrorNote(transcript, turnErr)
	}
	// Judge events are discarded: the room has no turn printer. Judge
	// failures are logged, never fatal.
	_, err := r.judgeRunner.RunJudges(ctx, r.agent, config.JudgeWhenEnd, transcript, nil)
	if err != nil {
		r.diagf("judge error: %v", err)
	}
}

// turnErrorNote appends the run-error block to a transcript, in the
// same label-at-column-0, body-indented-two-spaces convention as
// llm.FormatTranscript blocks.
func turnErrorNote(transcript string, turnErr error) string {
	body := strings.ReplaceAll(turnErr.Error(), "\n", "\n  ")
	return transcript + "\n\n[run error]\n  " + body
}

// diagf writes one best-effort diagnostic line; a write failure is
// ignored by contract.
func (r *Room) diagf(format string, args ...any) {
	if r.diag == nil {
		return
	}
	fmt.Fprintf(r.diag, "band room %s: "+format+"\n", append([]any{r.roomID}, args...)...)
}
