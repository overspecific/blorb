// Package chat implements the interactive REPL run mode.
package chat

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/engine"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/llm/openai"
	"github.com/overspecific/blorb/internal/logging"
	"github.com/overspecific/blorb/internal/prefactor"
	"github.com/overspecific/blorb/internal/tools"
)

// subagentPipe relays subagent activity events to the current turn's
// display callback. The registry's event callback is fixed at construction
// but the printer is created per turn, so the pipe is the stable
// indirection: runTurn points it at the turn's printer and clears it after.
type subagentPipe struct {
	mu sync.Mutex
	cb func(tools.SubagentEvent) error
}

// set points the pipe at cb (nil clears it).
func (p *subagentPipe) set(cb func(tools.SubagentEvent) error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cb = cb
}

// emit forwards one event to the current callback. Events outside a turn
// cannot occur; with no callback set they are dropped.
func (p *subagentPipe) emit(ev tools.SubagentEvent) error {
	p.mu.Lock()
	cb := p.cb
	p.mu.Unlock()
	if cb == nil {
		return nil
	}
	return cb(ev)
}

// Options configure a chat session.
type Options struct {
	Config config.Config
	// Agent is the resolved agent definition the session runs: its
	// system prompt, provider, and max turns drive the engine and client,
	// and its name names the banner and the Prefactor agent.
	Agent      config.Agent
	Version    string
	Stdin      io.Reader
	Stdout     io.Writer
	NewClient  func(agent config.Agent) (llm.Client, error)
	SigintChan <-chan os.Signal
	// Getenv overrides the environment lookup used to resolve
	// provider.api_key_env; os.Getenv when nil. Tests only.
	Getenv func(string) string
	// Stream enables incremental rendering of assistant responses: when
	// true and the client supports it, text, reasoning, and tool call
	// fragments are printed as they arrive. Disabled by --no-stream.
	Stream bool
	// ToolOutput enables printing the full body of parent tool results in
	// the chat. When false (the default), result blocks show a character
	// and line count of the output instead of the output itself. The full
	// body always renders for failed results and for subagent activity,
	// which is never suppressed.
	ToolOutput bool
	// ConfigPath is the path to the blorb.json that produced Config. When
	// empty, the session runs without file logging. When set and logging
	// is enabled in the config, the session writes wire logs into the
	// resolved log directory next to the config file.
	ConfigPath string
	// Tracer, when non-nil, records every turn to Prefactor. Tracing is a
	// hard dependency: a tracer error fails the turn or the session. Nil
	// disables tracing.
	Tracer *prefactor.Tracer
}

// sessionResult tells Run how the REPL loop ended, so the tracer can
// finish the instance with the matching terminal state.
type sessionResult int

const (
	// sessionGraceful: exit/quit/EOF or ctx done with no failure.
	sessionGraceful sessionResult = iota
	// sessionCompleteInterrupted: a signal landed as a turn finished.
	sessionGracefulInterrupted
	// sessionFailed: a fatal error ended the session.
	sessionFailure
	// sessionTerminated: the platform requested termination.
	sessionTerminate
)

// Run drives one interactive chat session. It returns nil on graceful exit
// (exit/quit, EOF, or SIGINT when no turn is in flight) and an error on
// startup failures. Turn failures are printed to stdout and keep the
// session alive. SIGINT during a turn cancels just that turn.
func Run(ctx context.Context, opts Options) error {
	sink, err := resolveSink(opts)
	if err != nil {
		return err
	}

	client, err := opts.newClient(sink)
	if err != nil {
		return err
	}

	// The engine suppresses whole-message events when it streams; only
	// claim streaming when the real (inner) client can stream — the
	// holder implements ChatStream unconditionally, so asserting on it
	// would always succeed and silently swallow replies from
	// non-streaming clients. The per-turn tracing wrapper preserves the
	// inner capability (its ChatStream falls back to Chat), so checking
	// once here is enough.
	streaming := false
	if _, ok := client.(llm.StreamingClient); ok {
		streaming = true
	}

	// The subagent runner builds per-invocation registries and engines for
	// any agent in the config; the session's factory generalizes to any
	// agent so subagents get their own clients. Streaming is shared with
	// the session engine's capability check.
	pipe := &subagentPipe{}
	registry, err := tools.NewRegistry(opts.Config.AgentTools(opts.Agent),
		tools.WithSink(sink), tools.WithConfigDir(opts.Config.Dir()),
		tools.WithSubagentRunner(opts.subagentRunner(sink, streaming)),
		tools.WithSubagentEvents(pipe.emit))
	if err != nil {
		return fmt.Errorf("build tools: %w", err)
	}
	// Builtins hold per-instance resources (open sandbox roots) for their
	// lifetime; release them when the session ends.
	defer registry.Close()

	// The holder lets per-turn tracing wrappers come and go without
	// engine changes; the engine sees a stable llm.Client.
	holder := &clientHolder{inner: client}

	eng := engine.New(engine.EngineConfig{
		Client:       holder,
		Tools:        registry,
		SystemPrompt: opts.Agent.SystemPrompt,
		MaxTurns:     opts.Agent.MaxTurnsOrDefault(),
		Stream:       opts.Stream && streaming,
	})

	// Session-level tracing: register and start the Prefactor instance
	// before the REPL loop. A terminate at startup exits immediately.
	if opts.Tracer != nil {
		schema := prefactor.DefaultAgentSchemaVersion(opts.Agent.Name, registry)
		if err := opts.Tracer.StartSession(ctx, schema); err != nil {
			if isTerminated(err) {
				fmt.Fprintf(opts.Stdout, "stopped by platform: %v\n", terminationReason(err))
				return nil
			}
			return fmt.Errorf("chat: %w", err)
		}
	}

	var (
		mu          sync.Mutex
		cancelTurn  context.CancelFunc
		interrupted bool
		sigExit     bool
	)
	setTurn := func(cancel context.CancelFunc) {
		mu.Lock()
		defer mu.Unlock()
		cancelTurn = cancel
	}
	// takeInterrupted returns and clears the interrupted flag so one
	// interruption only affects the turn it landed on.
	takeInterrupted := func() bool {
		mu.Lock()
		defer mu.Unlock()
		was := interrupted
		interrupted = false
		return was
	}

	exit := make(chan struct{})
	var once sync.Once
	requestExit := func() {
		mu.Lock()
		sigExit = true
		mu.Unlock()
		once.Do(func() { close(exit) })
	}

	// done is closed when Run returns, so the forwarding and reader
	// goroutines below stop instead of leaking.
	done := make(chan struct{})
	defer close(done)

	sigint := make(chan os.Signal, 1)
	if opts.SigintChan != nil {
		// Forward from the injected test channel.
		go func() {
			for {
				select {
				case sig, ok := <-opts.SigintChan:
					if !ok {
						return
					}
					select {
					case sigint <- sig:
					case <-done:
						return
					}
				case <-done:
					return
				}
			}
		}()
	} else {
		signal.Notify(sigint, syscall.SIGINT)
		defer signal.Stop(sigint)
	}

	go func() {
		for {
			select {
			case <-done:
				return
			case <-sigint:
				mu.Lock()
				cancel := cancelTurn
				if cancel != nil {
					interrupted = true
				}
				cancelTurn = nil
				mu.Unlock()
				if cancel != nil {
					// A turn is in flight: cancel just that turn.
					cancel()
				} else {
					// Nothing in flight: exit the session.
					requestExit()
				}
			}
		}
	}()

	fmt.Fprintf(opts.Stdout, "blorb %s (%s, %s)\n", opts.Version, opts.Agent.Name, opts.Agent.Provider.Model)

	input := make(chan inputResult, 1)
	go readLines(opts.Stdin, input, done)

	result, runErr := func() (sessionResult, error) {
		needHeading := true
		for {
			if needHeading {
				fmt.Fprint(opts.Stdout, "\n>>> User:\n")
				needHeading = false
			}
			select {
			case <-ctx.Done():
				return sessionGraceful, nil
			case <-exit:
				mu.Lock()
				wasSig := sigExit
				mu.Unlock()
				if wasSig {
					return sessionGracefulInterrupted, nil
				}
				return sessionGraceful, nil
			case r, ok := <-input:
				if !ok {
					return sessionGraceful, nil
				}
				if r.err != nil {
					return sessionFailure, fmt.Errorf("read input: %w", r.err)
				}
				if r.line == "" {
					needHeading = true
					continue
				}
				switch strings.ToLower(r.line) {
				case "exit", "quit":
					return sessionGraceful, nil
				}
				if ctx.Err() != nil {
					return sessionGraceful, nil
				}

				res, err := runTurn(ctx, opts, eng, holder, pipe, takeInterrupted, setTurn, r.line)
				needHeading = true
				if res == sessionTerminate || res == sessionFailure {
					return res, err
				}
				if res == sessionGracefulInterrupted {
					// A signal landed just as the turn finished: exit
					// cleanly, as before tracing existed.
					return sessionGracefulInterrupted, nil
				}
				// Recoverable turn outcome (err already printed).
			}
		}
	}()

	// Finish the Prefactor instance to match the session outcome. On
	// platform termination the instance is already marked server-side and
	// a second finish would 409, so it is skipped.
	if opts.Tracer != nil {
		if result == sessionTerminate {
			fmt.Fprintf(opts.Stdout, "stopped by platform: %v\n", terminationReason(runErr))
			return nil
		}
		status := prefactor.InstanceComplete
		switch result {
		case sessionFailure:
			status = prefactor.InstanceFailed
		}
		// The instance finish uses a background context, like the span
		// finishes: when the session exits via root-ctx cancellation the
		// session ctx is already dead, and a clean exit must still be
		// able to record its terminal state.
		if err := opts.Tracer.FinishSession(context.Background(), status); err != nil {
			if runErr != nil {
				return runErr
			}
			return fmt.Errorf("chat: %w", err)
		}
	}
	return runErr
}

// runTurn executes one user turn, driving the tracer when configured.

// Returns the session outcome: sessionGraceful keeps the loop alive
// (recoverable turn errors are printed here); fatal outcomes abort.
func runTurn(
	ctx context.Context,
	opts Options,
	eng *engine.Engine,
	holder *clientHolder,
	pipe *subagentPipe,
	takeInterrupted func() bool,
	setTurn func(context.CancelFunc),
	line string,
) (sessionResult, error) {
	turnCtx, cancel := context.WithCancel(ctx)
	setTurn(cancel)
	defer func() {
		setTurn(nil)
		cancel()
	}()

	printEvent, onSubagent, flush := chatEvents(opts.Stdout, opts.ToolOutput)
	pipe.set(onSubagent)
	defer pipe.set(nil)

	if opts.Tracer == nil {
		_, runErr := eng.RunTurn(turnCtx, line, printEvent)
		flush()
		return classifyTurnOutcome(runErr, turnCtx, takeInterrupted(), opts.Stdout)
	}

	turn, err := opts.Tracer.StartTurn(turnCtx, line)
	if err != nil {
		return tracerFailure(err)
	}

	holder.inner = newTracingClient(holder.inner, turn, opts.Agent.Provider.Model)
	defer func() { holder.inner = unwrapTracingClient(holder.inner) }()

	finalText, runErr := eng.RunTurn(turnCtx, line, traceEvent(turn, printEvent))
	flush()

	interrupted := takeInterrupted()

	// Finish the turn span to match the turn's actual outcome: a signal
	// landing just as a successful turn completes does not cancel
	// anything — the turn is done, and the session exits cleanly.
	switch {
	case runErr != nil && isTerminated(runErr):
		if err := turn.Fail(runErr); err != nil && !isTerminated(err) {
			// The terminate signal still wins.
			_ = err
		}
		return sessionTerminate, runErr
	case runErr != nil && (interrupted || turnCtx.Err() != nil):
		if err := turn.Cancel(); err != nil {
			return tracerFailure(err)
		}
	case runErr != nil:
		if err := turn.Fail(runErr); err != nil {
			return tracerFailure(err)
		}
	default:
		if err := turn.Complete(finalText); err != nil {
			return tracerFailure(err)
		}
	}

	return classifyTurnOutcome(runErr, turnCtx, interrupted, opts.Stdout)
}

// tracerFailure maps a tracer error to the session outcome: terminate wins,
// everything else ends the session as failed.
func tracerFailure(err error) (sessionResult, error) {
	if isTerminated(err) {
		return sessionTerminate, err
	}
	return sessionFailure, fmt.Errorf("chat: %w", err)
}

// classifyTurnOutcome maps a turn error (already reflected in the tracer)
// to the session outcome, printing recoverable turn errors.
func classifyTurnOutcome(runErr error, turnCtx context.Context, interrupted bool, out io.Writer) (sessionResult, error) {
	if runErr == nil {
		// A signal landed just as the turn finished: exit cleanly.
		if interrupted {
			return sessionGracefulInterrupted, nil
		}
		return sessionGraceful, nil
	}
	if interrupted && turnCtx.Err() != nil {
		fmt.Fprintln(out, "(interrupted)")
		return sessionGraceful, nil
	}
	fmt.Fprintf(out, "error: %v\n", runErr)
	return sessionGraceful, nil
}

type inputResult struct {
	line string
	err  error
}

// readLines pumps trimmed lines of input until EOF, error, or done is
// closed (the session ending with stdin still open). The scanner accepts
// lines up to 1 MiB so long pasted input arrives as one turn.
func readLines(r io.Reader, out chan<- inputResult, done <-chan struct{}) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for {
		select {
		case <-done:
			return
		default:
		}
		if !scanner.Scan() {
			break
		}
		// Non-blocking send: if Run has exited, drop the line instead of
		// blocking forever.
		select {
		case out <- inputResult{line: strings.TrimSpace(scanner.Text())}:
		case <-done:
			return
		}
	}
	if err := scanner.Err(); err != nil {
		select {
		case out <- inputResult{err: err}:
		case <-done:
		}
	}
	close(out)
}

// chatEvents returns the parent event callback, the subagent event
// callback, and a flush function. All output goes to stdout: assistant
// text under a ">>> Assistant:" heading, tool activity as heading blocks,
// and streamed fragments written inline. Subagent activity renders through
// the same writer with headings labeled by the producing agent and
// indented by two spaces per nesting depth, so it interleaves with the
// parent's blocks. The flush function terminates any partial streamed line
// after the turn. Streamed fragments are written without a trailing
// newline; whole-message events write complete blocks. toolOutput gates
// the body of parent tool results: when false, a successful result block
// shows a character and line count of the output instead of the output
// itself. Failed results and subagent activity always render in full.
func chatEvents(out io.Writer, toolOutput bool) (func(engine.Event) error, func(tools.SubagentEvent) error, func()) {
	var (
		printedHeading   bool
		printedThinking  bool
		streamedToolCall bool
		// toolHeadings tracks which tool calls (by delta index) already
		// printed a heading, so two calls to the same tool in one round
		// each get their own heading block.
		toolHeadings = map[int]bool{}
		// partialLine tracks whether a streamed fragment left the output
		// mid-line, so the next block can terminate the line first and
		// keep blank-line separators consistent.
		partialLine bool
	)

	endLine := func() {
		if partialLine {
			fmt.Fprint(out, "\n")
			partialLine = false
		}
	}

	writeDelta := func(fragment string) {
		fmt.Fprint(out, fragment)
		partialLine = !strings.HasSuffix(fragment, "\n")
	}

	// heading writes a block heading, terminating any partial line first
	// and prefixing a blank line so blocks are consistently separated.
	// Subagent headings pass their indentation through text.
	heading := func(text string) {
		endLine()
		fmt.Fprintf(out, "\n%s\n", text)
	}

	// subagentState is the per-subagent-stream heading bookkeeping: for
	// each (depth, agent), whether the given delta heading already printed.
	type streamKey struct {
		depth int
		agent string
	}
	subHeadings := map[streamKey]subStreamHeadings{}

	// startRound resets per-round heading state: a tool result ends the
	// current round, so the next response's blocks start fresh. The
	// subagent stream state resets too: a later invocation of the same
	// subagent in the same turn must print its headings again.
	// streamedToolCall is deliberately not reset: a streamed round already
	// rendered every subsequent whole-message tool call inline, so
	// suppressing whole-message tool blocks stays correct for the rest of
	// the turn.
	startRound := func() {
		printedHeading = false
		printedThinking = false
		toolHeadings = map[int]bool{}
		subHeadings = map[streamKey]subStreamHeadings{}
	}

	onEvent := func(ev engine.Event) error {
		switch ev.Kind {
		case engine.EventAssistantThinking:
			heading(">>> Assistant (thinking):")
			fmt.Fprintln(out, ev.Text)
		case engine.EventAssistantText:
			heading(">>> Assistant:")
			fmt.Fprintln(out, ev.Text)
		case engine.EventAssistantThinkingDelta:
			if !printedThinking {
				heading(">>> Assistant (thinking):")
				printedThinking = true
			}
			writeDelta(ev.Text)
		case engine.EventAssistantTextDelta:
			if !printedHeading {
				heading(">>> Assistant:")
				printedHeading = true
			}
			writeDelta(ev.Text)
		case engine.EventToolCallDelta:
			if ev.Name != "" && !toolHeadings[ev.Index] {
				toolHeadings[ev.Index] = true
				streamedToolCall = true
				heading(">>> Tool: " + ev.Name)
			}
			fmt.Fprint(out, ev.Args)
			partialLine = !strings.HasSuffix(ev.Args, "\n")
		case engine.EventToolCall:
			// In streaming mode the fragments already rendered this call;
			// skip the whole-message block to avoid printing it twice.
			if !streamedToolCall {
				heading(">>> Tool: " + ev.Name)
				fmt.Fprintln(out, ev.Args)
			}
		case engine.EventToolResult:
			marker := "Result:"
			if ev.Failed {
				marker = "Error:"
			}
			heading(">>> " + marker + " Tool: " + ev.Name)
			// A failed result always shows its body so the user can diagnose
			// the failure; a successful one shows a count summary unless tool
			// output is enabled.
			if ev.Failed || toolOutput {
				fmt.Fprintln(out, ev.Output)
			} else {
				fmt.Fprintln(out, outputSummary(ev.Output))
			}
			// A tool result marks the end of an assistant round: the next
			// response starts a fresh set of heading blocks.
			startRound()
		}
		return nil
	}

	// indent is two spaces per nesting depth.
	indent := func(depth int) string {
		return strings.Repeat("  ", depth)
	}

	onSubagent := func(ev tools.SubagentEvent) error {
		key := streamKey{depth: ev.Depth, agent: ev.Agent}
		st, ok := subHeadings[key]
		if !ok {
			st = subStreamHeadings{toolHeadings: map[int]bool{}}
		}
		label := fmt.Sprintf("%s[%s] ", indent(ev.Depth), ev.Agent)

		switch ev.Kind {
		case tools.SubagentThinking:
			heading(label + ">>> Assistant (thinking):")
			fmt.Fprintln(out, indent(ev.Depth)+ev.Text)
		case tools.SubagentText:
			heading(label + ">>> Assistant:")
			fmt.Fprintln(out, indent(ev.Depth)+ev.Text)
		case tools.SubagentThinkingDelta:
			if !st.printedThinking {
				heading(label + ">>> Assistant (thinking):")
				st.printedThinking = true
			}
			writeDelta(ev.Text)
		case tools.SubagentTextDelta:
			if !st.printedHeading {
				heading(label + ">>> Assistant:")
				st.printedHeading = true
			}
			writeDelta(ev.Text)
		case tools.SubagentToolCallDelta:
			if ev.Name != "" && !st.toolHeadings[ev.Index] {
				st.toolHeadings[ev.Index] = true
				st.streamedToolCall = true
				heading(label + ">>> Tool: " + ev.Name)
			}
			fmt.Fprint(out, ev.Args)
			partialLine = !strings.HasSuffix(ev.Args, "\n")
		case tools.SubagentToolCall:
			// In streaming mode the fragments already rendered this call;
			// skip the whole-message block to avoid printing it twice.
			if !st.streamedToolCall {
				heading(label + ">>> Tool: " + ev.Name)
				fmt.Fprintln(out, indent(ev.Depth)+ev.Args)
			}
		case tools.SubagentToolResult:
			marker := "Result:"
			if ev.Failed {
				marker = "Error:"
			}
			heading(label + ">>> " + marker + " Tool: " + ev.Name)
			// Subagent results always render in full: suppression applies
			// only to the parent's own tool results.
			fmt.Fprintln(out, indent(ev.Depth)+ev.Output)
			// A subagent tool result ends the subagent's round, mirroring
			// the parent's startRound. The result event carries the calling
			// agent's identity, not the called agent's, so the whole map is
			// reset: any deeper subagent stream that just finished must
			// print its headings again on a later invocation.
			subHeadings = map[streamKey]subStreamHeadings{}
			st = subStreamHeadings{toolHeadings: map[int]bool{}}
		}
		subHeadings[key] = st
		return nil
	}

	flush := endLine

	return onEvent, onSubagent, flush
}

// outputSummary describes a tool result body when the body itself is not
// printed: the character count and the line count (empty output is
// 0 characters, 0 lines; a trailing newline does not count as an extra
// line, so "a\nb" and "a\nb\n" are both 2 lines).
func outputSummary(output string) string {
	charCount := utf8.RuneCountInString(output)
	lineCount := 0
	if output != "" {
		lineCount = strings.Count(output, "\n") + 1
		if strings.HasSuffix(output, "\n") {
			lineCount--
		}
	}
	return fmt.Sprintf("%d characters, %d %s", charCount, lineCount, pluralize("line", lineCount))
}

// pluralize returns the singular or plural form of word for n.
func pluralize(word string, n int) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// subStreamHeadings tracks which delta headings a subagent stream already
// printed, mirroring the parent's per-round state.
type subStreamHeadings struct {
	printedHeading   bool
	printedThinking  bool
	streamedToolCall bool
	toolHeadings     map[int]bool
}

// resolveSink builds the wire-log sink for a session. File logging is on
// only when ConfigPath is set and the config enables logging; otherwise a
// no-op sink is returned so downstream code always has a non-nil sink.
// When enabled, the session gets its own `<timestamp>-<uuid>` subdirectory
// of the log dir, so conversations never interleave.
func resolveSink(opts Options) (logging.Sink, error) {
	if opts.ConfigPath == "" || !opts.Config.LoggingEnabled() {
		return logging.NewNop(), nil
	}

	dir := filepath.Join(filepath.Dir(opts.ConfigPath), opts.Config.LogDir())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	sink, _, err := logging.NewSession(dir)
	if err != nil {
		return nil, fmt.Errorf("open log dir: %w", err)
	}
	return sink, nil
}

// NewClient builds the LLM client described by an agent's provider,
// switching on provider.type. When a second provider lands this graduates
// to a registry map. getenv is the environment lookup, injectable for
// tests; pass os.Getenv in production. sink receives LLM wire logs; nil
// disables them.
func NewClientWithGetenv(agent config.Agent, getenv func(string) string, sink logging.Sink) (llm.Client, error) {
	switch agent.Provider.Type {
	case config.ProviderTypeOpenAI:
		apiKey := ""
		if envName := agent.Provider.APIKeyEnvOrDefault(); envName != "" {
			apiKey = getenv(envName)
			if apiKey == "" {
				return nil, fmt.Errorf("api_key_env %q is set but the environment variable is empty", envName)
			}
		}
		return openai.New(openai.Config{
			BaseURL: agent.Provider.BaseURL,
			Model:   agent.Provider.Model,
			APIKey:  apiKey,
			Sink:    sink,
		})
	default:
		return nil, fmt.Errorf("provider type %q is not supported (supported: %v)", agent.Provider.Type, config.SupportedProviderTypes())
	}
}

// NewClient builds the LLM client described by an agent's provider using
// os.Getenv.
func NewClient(agent config.Agent) (llm.Client, error) {
	return NewClientWithGetenv(agent, os.Getenv, logging.NewNop())
}

func (o Options) newClient(sink logging.Sink) (llm.Client, error) {
	return o.newClientFor(o.Agent, sink)
}

// newClientFor builds the LLM client for any agent: the injected NewClient
// factory when set, else the real provider path with the injected getenv.
func (o Options) newClientFor(agent config.Agent, sink logging.Sink) (llm.Client, error) {
	if o.NewClient != nil {
		return o.NewClient(agent)
	}
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	return NewClientWithGetenv(agent, getenv, sink)
}

// subagentRunner builds the engine-backed runner for the session's config.
// streaming is the session's capability check result (whether the session
// client itself implements llm.StreamingClient), shared with the session's
// engine; subagent clients that cannot stream fall back to whole messages.
func (o Options) subagentRunner(sink logging.Sink, streaming bool) *engine.SubagentRunner {
	return engine.NewSubagentRunner(engine.SubagentRunnerConfig{
		Config: o.Config,
		NewClient: func(agent config.Agent) (llm.Client, error) {
			return o.newClientFor(agent, sink)
		},
		Stream: o.Stream && streaming,
		Sink:   sink,
	})
}
