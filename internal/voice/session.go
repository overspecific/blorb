package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/overspecific/blorb/internal/chat"
	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/engine"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/logging"
	"github.com/overspecific/blorb/internal/tools"
)

// SessionClient is the connected session's view of the voice client, so the
// session loop can run against a fake in tests.
type SessionClient interface {
	Events() <-chan Event
	Done() <-chan error
	End() error
	Close() error
}

// Options configure a voice session.
type Options struct {
	// Agent is the resolved agent definition the session serves.
	Agent config.Agent
	// Config is the loaded configuration the agent's tools resolve against.
	Config config.Config
	// APIKey authenticates the Voice Agent WebSocket handshake; the caller
	// resolves api_key_env against the environment.
	APIKey string
	// Sink receives best-effort wire records.
	Sink logging.Sink
	// Stdout receives the transcript and status lines.
	Stdout io.Writer
	// Version is the blorb version named in the banner.
	Version string
	// NoMic skips microphone capture: the session is agent-talk-only.
	NoMic bool
	// SigintChan injects the interrupt signal in tests; nil installs the
	// real SIGINT handler.
	SigintChan <-chan os.Signal
	// NewSessionClient connects the voice client. Tests inject a fake; nil
	// dials the real AssemblyAI endpoint.
	NewSessionClient func(ctx context.Context, cfg ClientConfig) (SessionClient, error)
	// NewLLMClient builds the LLM client for an agent's named model, used
	// to run subagent tools. Tests inject a fake; nil builds the real
	// client from the config.
	NewLLMClient func(cfg config.Config, agent config.Agent) (llm.Client, error)
}

// Run drives one voice session: it wires the agent's tools, the audio
// subprocesses and the voice client together, renders the transcript to
// stdout, and ends the session on Ctrl-C or a terminal client error.
func Run(ctx context.Context, opts Options) error {
	con := &console{w: opts.Stdout}
	sink := opts.Sink
	if sink == nil {
		sink = logging.NewNop()
	}

	voiceCfg := opts.Agent.Voice
	if voiceCfg == nil {
		return fmt.Errorf("agent %q has no voice section", opts.Agent.Name)
	}

	newLLMClient := opts.NewLLMClient
	if newLLMClient == nil {
		newLLMClient = func(cfg config.Config, agent config.Agent) (llm.Client, error) {
			return chat.NewClientWithGetenv(cfg, agent, os.Getenv, sink)
		}
	}

	registry, defs, err := buildVoiceTools(opts.Config, opts.Agent, sink, newLLMClient, con)
	if err != nil {
		return err
	}
	defer registry.Close()

	newSession := opts.NewSessionClient
	if newSession == nil {
		newSession = func(ctx context.Context, cfg ClientConfig) (SessionClient, error) {
			return Connect(ctx, cfg)
		}
	}

	// Tool execution runs on the client's read loop, which must answer
	// tool.call; its output, including subagent activity, serializes with
	// the transcript through the console lock. A hung tool surfaces as a
	// failed result after the registry's per-call timeout.
	runTool := func(toolCtx context.Context, name string, args json.RawMessage) (string, bool) {
		return con.runTool(toolCtx, registry, name, args)
	}

	clientCfg := ClientConfig{
		URL:          voiceCfg.WSURLOrDefault(),
		APIKey:       opts.APIKey,
		SystemPrompt: opts.Agent.SystemPrompt,
		Greeting:     voiceCfg.Greeting,
		Voice:        voiceCfg.Voice,
		Volume:       voiceCfg.Volume,
		Tools:        defs,
		Playback:     io.Discard,
		RunTool:      runTool,
		Sink:         sink,
	}

	var capture *audioReader
	var playback *audioWriter
	if !opts.NoMic {
		capture, err = startCapture(ctx, voiceCfg.InputCommandOrDefault())
		if err != nil {
			return err
		}
		defer capture.Close()
		clientCfg.Mic = capture
	}
	playback, err = startPlayback(ctx, voiceCfg.OutputCommandOrDefault())
	if err != nil {
		return err
	}
	defer playback.Close()
	clientCfg.Playback = playback

	client, err := newSession(ctx, clientCfg)
	if err != nil {
		return err
	}

	con.printf("blorb %s (%s, voice session)\n", opts.Version, opts.Agent.Name)
	con.printf("Speak to talk; Ctrl-C hangs up.\n")

	return sessionLoop(ctx, opts, client, con)
}

// sessionLoop renders events until the session ends, the context is
// cancelled, or the client fails.
//
// The client closes Events only after all buffered events have been
// delivered, so the loop watches Events alone for the session's end: reading
// Done in the same select could abandon events still in the channel. Done is
// consulted once Events closes.
func sessionLoop(ctx context.Context, opts Options, client SessionClient, con *console) error {
	sigint, stop := sigintChannel(opts.SigintChan)
	defer stop()

	r := &renderer{}
	ended := false
	ending := false

	for {
		select {
		case <-ctx.Done():
			return endSession(client, ended)
		case <-sigint:
			if !ending {
				// First Ctrl-C: hang up gracefully. The client's End waits
				// for the server's session.ended, which can take a moment,
				// so run it off the loop: a second Ctrl-C must still be
				// seen (to force-close) and events must keep rendering.
				ending = true
				con.printf("\nHanging up; Ctrl-C again to quit now.\n")
				go func() { _ = client.End() }()
				continue
			}
			// Second Ctrl-C: force-close.
			_ = client.Close()
			return nil
		case ev, ok := <-client.Events():
			if !ok {
				return finish(con, ended, <-client.Done())
			}
			if ev.Type == typeSessionEnded {
				ended = true
			}
			con.render(r, ev)
		}
	}
}

// endSession hangs up gracefully on context cancellation, then reports the
// client's terminal error unless the session already ended cleanly.
func endSession(client SessionClient, ended bool) error {
	_ = client.End()
	err := <-client.Done()
	if ended || isCleanClose(err) {
		return nil
	}
	return err
}

// finish prints a terminal client error and returns it, or nil when the
// session ended cleanly.
func finish(con *console, ended bool, err error) error {
	if ended || err == nil || isCleanClose(err) {
		return nil
	}
	con.printf("voice: %v\n", err)
	return err
}

// isCleanClose reports whether the client's terminal error is the deliberate
// close sentinel rather than a real failure.
func isCleanClose(err error) bool {
	return err == nil || errors.Is(err, ErrSessionClosed)
}

// buildVoiceTools builds the tools registry from the agent's granted tools and
// returns its API-facing definitions. Decider tools cannot run in a voice
// session: a decider makes decision-model calls through a path the voice
// command does not build, so a config that grants one fails at startup rather
// than running a reduced agent. Subagent tools do run: they recurse into the
// engine with the agent's configured local model.
func buildVoiceTools(cfg config.Config, agent config.Agent, sink logging.Sink, newLLMClient func(config.Config, config.Agent) (llm.Client, error), con *console) (*tools.Registry, []llm.Tool, error) {
	granted := cfg.AgentTools(agent)

	var deciders []string
	for _, e := range granted {
		if e.Type == config.ToolTypeDecider {
			deciders = append(deciders, e.Name)
		}
	}
	if len(deciders) > 0 {
		return nil, nil, fmt.Errorf("agent %q: voice sessions cannot run decider tools: %s",
			agent.Name, strings.Join(deciders, ", "))
	}

	// Subagent engines render through the same chat-style subagent printer
	// the band room uses. The printer writes to the console's raw writer;
	// runTool holds the console lock for the whole tool call, so subagent
	// activity serializes with the transcript without re-entering the lock.
	_, onSubagent, _ := chat.Events(con.rawWriter(), true)

	subagents := engine.NewSubagentRunner(engine.SubagentRunnerConfig{
		Config:    cfg,
		NewClient: newLLMClient,
		// Stream enables incremental subagent activity when the client
		// supports it; the engine falls back to whole messages for clients
		// that do not, so this is safe unconditionally.
		Stream: true,
		Sink:   sink,
	})

	registry, err := tools.NewRegistry(
		granted,
		tools.WithSink(sink),
		tools.WithConfigDir(cfg.Dir()),
		tools.WithSubagentRunner(subagents),
		tools.WithSubagentEvents(onSubagent),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("build tools: %w", err)
	}
	return registry, registry.Definitions(), nil
}

// compactJSON returns raw JSON as a compact single line, or the input
// unchanged when it is not valid JSON.
func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

// console serializes all session output - the transcript from the session
// loop and tool and subagent activity from the client's read loop - behind one
// mutex, so blocks never interleave.
type console struct {
	mu sync.Mutex
	w  io.Writer
}

// rawWriter returns the underlying writer without the lock. It is for
// callbacks that are already invoked under the console lock (the subagent
// printer during runTool), so they must not take the lock again.
func (c *console) rawWriter() io.Writer {
	if c.w == nil {
		return io.Discard
	}
	return c.w
}

// printf writes formatted output under the lock.
func (c *console) printf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.rawWriter(), format, args...)
}

// render renders one event under the console lock, so a transcript write
// never interleaves with tool or subagent activity.
func (c *console) render(r *renderer, ev Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r.render(c.rawWriter(), ev)
}

// runTool runs one tool through the registry and renders its call and result
// as heading blocks, matching the chat UI. It holds the console lock for the
// whole call, so subagent activity serializes with the transcript. A
// tool-reported failure or an infrastructure error is returned to the server
// flagged as an error, so the conversation recovers.
func (c *console) runTool(ctx context.Context, registry *tools.Registry, name string, args json.RawMessage) (output string, isError bool) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.rawWriter()

	fmt.Fprintf(w, "\n>>> Tool: %s\n%s\n", name, compactJSON(args))

	res, err := registry.Run(ctx, name, args)
	if err != nil {
		fmt.Fprintf(w, "\n>>> Error: Tool: %s\n%v\n", name, err)
		return err.Error(), true
	}
	if res.Err {
		fmt.Fprintf(w, "\n>>> Error: Tool: %s\n%s\n", name, res.Output)
		return res.Output, true
	}
	fmt.Fprintf(w, "\n>>> Result: Tool: %s\n%s\n", name, res.Output)
	return res.Output, false
}

// renderer turns the decoded server events into the chat UI's blocks:
// ">>> User:", ">>> Assistant:" and lifecycle notes, separated by blank
// lines. The user's partial speech rewrites its line as the transcript grows;
// the agent's words append as they arrive.
type renderer struct {
	// userOpen and agentOpen track whether a block heading was printed and
	// awaits its terminating newline.
	userOpen  bool
	agentOpen bool
	// agentWrote is whether any text has been written to the current agent
	// block, so a final transcript is not printed twice.
	agentWrote bool
	// agentLastByte is the final byte written to the agent block, used to
	// join word deltas with a space. The server's deltas are inconsistently
	// spaced: replies usually carry a trailing space ("I ", "am "), but the
	// greeting's words arrive bare ("Hi!", "I", "am"), so a raw concatenation
	// runs together.
	agentLastByte byte
	// partialLine is whether the last write left the output mid-line.
	partialLine bool
}

// endLine terminates a partial line so the next block separates cleanly.
func (r *renderer) endLine(w io.Writer) {
	if r.partialLine {
		fmt.Fprint(w, "\n")
		r.partialLine = false
	}
}

// heading writes a block heading after a blank line, matching chat.
func (r *renderer) heading(w io.Writer, text string) {
	r.endLine(w)
	fmt.Fprintf(w, "\n%s\n", text)
}

// writeAgentText appends text to the current agent block, inserting a single
// space when neither the preceding output nor the incoming text already
// carries one. This joins word deltas into readable text whether the server
// sends them bare or space-terminated.
func (r *renderer) writeAgentText(w io.Writer, text string) {
	if text == "" {
		return
	}
	if r.agentWrote && !isSpace(r.agentLastByte) && !isSpace(text[0]) {
		fmt.Fprint(w, " ")
	}
	fmt.Fprint(w, text)
	r.agentWrote = true
	r.agentLastByte = text[len(text)-1]
	r.partialLine = !strings.HasSuffix(text, "\n")
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

func (r *renderer) render(w io.Writer, ev Event) {
	switch ev.Type {
	case typeUserDelta:
		// The delta is the full transcript so far: rewrite its line.
		if !r.userOpen {
			r.heading(w, ">>> User:")
			r.userOpen = true
		}
		fmt.Fprintf(w, "\r\x1b[K%s", ev.Text)
	case typeUserFinal:
		if !r.userOpen {
			r.heading(w, ">>> User:")
			r.userOpen = true
		}
		fmt.Fprintf(w, "\r\x1b[K%s\n", ev.Text)
		r.userOpen = false
	case typeAgentDelta:
		if !r.agentOpen {
			r.heading(w, ">>> Assistant:")
			r.agentOpen = true
		}
		r.writeAgentText(w, ev.Delta)
	case typeAgentFinal:
		if !r.agentOpen {
			r.heading(w, ">>> Assistant:")
			r.agentOpen = true
		}
		if !r.agentWrote && ev.Text != "" {
			fmt.Fprint(w, ev.Text)
			r.agentWrote = true
		}
		r.agentLastByte = 0
		// An interrupted reply is closed by its reply.done, which appends
		// the marker; a completed one ends here.
		if !ev.Interrupted {
			fmt.Fprint(w, "\n")
			r.agentOpen = false
			r.agentWrote = false
		}
	case typeReplyDone:
		if r.agentOpen {
			if ev.Status == "interrupted" {
				fmt.Fprint(w, " [interrupted]")
			}
			fmt.Fprint(w, "\n")
			r.agentOpen = false
			r.agentWrote = false
			r.agentLastByte = 0
		}
	case typeSessionEnded:
		r.renderEnded(w, ev)
	case typeSessionError:
		r.heading(w, ">>> Voice error")
		fmt.Fprintf(w, "%s: %s\n", ev.Code, ev.Message)
	}
}

// renderEnded prints the session's duration when the event carries one.
func (r *renderer) renderEnded(w io.Writer, ev Event) {
	r.endLine(w)
	if ev.SessionDurationSeconds != nil {
		fmt.Fprintf(w, "\nSession ended (%.1fs).\n", *ev.SessionDurationSeconds)
		return
	}
	fmt.Fprintln(w, "\nSession ended.")
}

// sigintChannel returns a channel of interrupt signals. A non-nil injected
// channel is forwarded as-is; otherwise the real SIGINT handler is installed
// and stop removes it.
func sigintChannel(injected <-chan os.Signal) (<-chan os.Signal, func()) {
	if injected != nil {
		return injected, func() {}
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT)
	return ch, func() { signal.Stop(ch) }
}
