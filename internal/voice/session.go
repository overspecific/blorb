package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/overspecific/blorb/internal/config"
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
	// NewClient connects the voice client. Tests inject a fake; nil dials
	// the real AssemblyAI endpoint.
	NewClient func(ctx context.Context, cfg ClientConfig) (SessionClient, error)
}

// Run drives one voice session: it wires the agent's tools, the audio
// subprocesses and the voice client together, renders the transcript to
// stdout, and ends the session on Ctrl-C or a terminal client error.
func Run(ctx context.Context, opts Options) error {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	sink := opts.Sink
	if sink == nil {
		sink = logging.NewNop()
	}

	voiceCfg := opts.Agent.Voice
	if voiceCfg == nil {
		return fmt.Errorf("agent %q has no voice section", opts.Agent.Name)
	}

	registry, defs, err := buildVoiceTools(opts.Config, opts.Agent, sink)
	if err != nil {
		return err
	}
	defer registry.Close()

	newClient := opts.NewClient
	if newClient == nil {
		newClient = func(ctx context.Context, cfg ClientConfig) (SessionClient, error) {
			return Connect(ctx, cfg)
		}
	}

	// A voice session cannot run a local LLM turn, so its runTool callback
	// is the only execution path: a hung tool surfaces as a failed result
	// after the registry's per-call timeout.
	runTool := func(toolCtx context.Context, name string, args json.RawMessage) (string, bool) {
		return runVoiceTool(toolCtx, stdout, registry, name, args)
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

	client, err := newClient(ctx, clientCfg)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "blorb %s (%s, voice session)\n", opts.Version, opts.Agent.Name)
	fmt.Fprintln(stdout, "Speak to talk; Ctrl-C hangs up.")

	return sessionLoop(ctx, opts, client, stdout)
}

// sessionLoop renders events until the session ends, the context is
// cancelled, or the client fails.
//
// The client closes Events only after all buffered events have been
// delivered, so the loop watches Events alone for the session's end: reading
// Done in the same select could abandon events still in the channel. Done is
// consulted once Events closes.
func sessionLoop(ctx context.Context, opts Options, client SessionClient, stdout io.Writer) error {
	sigint, stop := sigintChannel(opts.SigintChan)
	defer stop()

	r := &renderer{out: stdout}
	ended := false
	ending := false

	for {
		select {
		case <-ctx.Done():
			return endSession(client, ended)
		case <-sigint:
			if !ending {
				// First Ctrl-C: hang up gracefully. The client's End
				// delivers the server's session.ended before the events
				// channel closes, so the loop keeps draining.
				ending = true
				if err := client.End(); err != nil {
					return err
				}
				continue
			}
			// Second Ctrl-C: force-close.
			_ = client.Close()
			return nil
		case ev, ok := <-client.Events():
			if !ok {
				return finish(client, r, ended, <-client.Done())
			}
			if ev.Type == typeSessionEnded {
				ended = true
			}
			r.render(ev)
		}
	}
}

// endSession hangs up gracefully on context cancellation, then reports the
// client's terminal error unless the session already ended cleanly.
func endSession(client SessionClient, ended bool) error {
	_ = client.End()
	if err := <-client.Done(); err != nil && !ended {
		return err
	}
	return nil
}

// finish prints a terminal client error and returns it, or nil when the
// session ended cleanly.
func finish(client SessionClient, r *renderer, ended bool, err error) error {
	if ended || err == nil || isCleanClose(err) {
		return nil
	}
	fmt.Fprintf(r.out, "voice: %v\n", err)
	return err
}

// isCleanClose reports whether the client's terminal error is the deliberate
// close sentinel rather than a real failure.
func isCleanClose(err error) bool {
	return err == nil || err.Error() == "voice session closed"
}

// buildVoiceTools builds the tools registry from the agent's granted tools and
// returns its API-facing definitions. Subagent and decider tools cannot run in
// a voice session: neither has the LLM path a local turn would need, so a
// config that grants one fails at startup rather than running a reduced agent.
func buildVoiceTools(cfg config.Config, agent config.Agent, sink logging.Sink) (*tools.Registry, []llm.Tool, error) {
	granted := cfg.AgentTools(agent)
	var unsupported []string
	for _, e := range granted {
		if e.Type == config.ToolTypeSubagent || e.Type == config.ToolTypeDecider {
			unsupported = append(unsupported, e.Name)
		}
	}
	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		return nil, nil, fmt.Errorf("agent %q: voice sessions cannot run %s tools: %s",
			agent.Name, "subagent and decider", strings.Join(unsupported, ", "))
	}

	registry, err := tools.NewRegistry(granted, tools.WithSink(sink), tools.WithConfigDir(cfg.Dir()))
	if err != nil {
		return nil, nil, fmt.Errorf("build tools: %w", err)
	}
	return registry, registry.Definitions(), nil
}

// runVoiceTool runs one tool through the registry and renders its call and
// result to stdout. A tool-reported failure or an infrastructure error is
// returned to the server flagged as an error, so the conversation recovers.
func runVoiceTool(ctx context.Context, stdout io.Writer, registry *tools.Registry, name string, args json.RawMessage) (string, bool) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	fmt.Fprintf(stdout, "\n>>> Tool: %s\n%s\n", name, compactJSON(args))

	res, err := registry.Run(ctx, name, args)
	if err != nil {
		fmt.Fprintf(stdout, ">>> Error: %v\n", err)
		return err.Error(), true
	}
	if res.Err {
		fmt.Fprintf(stdout, ">>> Error: %s\n", res.Output)
		return res.Output, true
	}
	fmt.Fprintf(stdout, ">>> Result: %s\n", res.Output)
	return res.Output, false
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

// renderer writes the live transcript: partial user speech on an updating
// line, the agent's words appended in step with the audio, and lifecycle
// notes.
type renderer struct {
	out io.Writer
	// agentOpen is true while an agent line has been started by a delta and
	// awaits its terminating newline.
	agentOpen bool
}

func (r *renderer) render(ev Event) {
	switch ev.Type {
	case typeUserDelta:
		// The delta is the full transcript so far: rewrite the line.
		fmt.Fprintf(r.out, "\r\x1b[KUser: %s", ev.Text)
	case typeUserFinal:
		fmt.Fprintf(r.out, "\r\x1b[KUser: %s\n", ev.Text)
	case typeAgentDelta:
		if !r.agentOpen {
			fmt.Fprint(r.out, "Agent: ")
			r.agentOpen = true
		}
		fmt.Fprint(r.out, ev.Delta)
	case typeAgentFinal:
		if r.agentOpen {
			fmt.Fprint(r.out, "\n")
		} else {
			// No deltas arrived (for example a barge-in that trimmed the
			// reply): print the final text whole.
			fmt.Fprintf(r.out, "Agent: %s\n", ev.Text)
		}
		r.agentOpen = false
	case typeReplyDone:
		if ev.Status == "interrupted" && r.agentOpen {
			fmt.Fprint(r.out, " [interrupted]\n")
			r.agentOpen = false
		}
	case typeSessionEnded:
		r.renderEnded(ev)
	case typeSessionError:
		fmt.Fprintf(r.out, "voice session error %s: %s\n", ev.Code, ev.Message)
	}
}

// renderEnded prints the session's duration when the event carries one.
func (r *renderer) renderEnded(ev Event) {
	if ev.SessionDurationSeconds != nil {
		fmt.Fprintf(r.out, "Session ended (%.1fs).\n", *ev.SessionDurationSeconds)
		return
	}
	fmt.Fprintln(r.out, "Session ended.")
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
