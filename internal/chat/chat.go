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

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/engine"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/llm/openai"
	"github.com/overspecific/blorb/internal/logging"
	"github.com/overspecific/blorb/internal/tools"
)

// Options configure a chat session.
type Options struct {
	Config     config.Config
	Version    string
	Stdin      io.Reader
	Stdout     io.Writer
	NewClient  func(cfg config.Config) (llm.Client, error)
	SigintChan <-chan os.Signal
	// Getenv overrides the environment lookup used to resolve
	// provider.api_key_env; os.Getenv when nil. Tests only.
	Getenv func(string) string
	// Stream enables incremental rendering of assistant responses: when
	// true and the client supports it, text, reasoning, and tool call
	// fragments are printed as they arrive. Disabled by --no-stream.
	Stream bool
	// ConfigPath is the path to the blorb.json that produced Config. When
	// empty, the session runs without file logging. When set and logging
	// is enabled in the config, the session writes wire logs into the
	// resolved log directory next to the config file.
	ConfigPath string
}

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

	registry, err := tools.NewRegistry(opts.Config.Tools, tools.WithSink(sink))
	if err != nil {
		return fmt.Errorf("build tools: %w", err)
	}

	eng := engine.New(engine.EngineConfig{
		Client:       client,
		Tools:        registry,
		SystemPrompt: opts.Config.SystemPrompt,
		MaxTurns:     opts.Config.MaxTurnsOrDefault(),
		Stream:       opts.Stream,
	})

	var (
		mu          sync.Mutex
		cancelTurn  context.CancelFunc
		interrupted bool
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
	requestExit := func() { once.Do(func() { close(exit) }) }

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

	fmt.Fprintf(opts.Stdout, "blorb %s (%s, %s)\n", opts.Version, opts.Config.Name, opts.Config.Provider.Model)

	input := make(chan inputResult, 1)
	go readLines(opts.Stdin, input, done)

	needHeading := true
	for {
		if needHeading {
			fmt.Fprint(opts.Stdout, "\n>>> User:\n")
			needHeading = false
		}
		select {
		case <-ctx.Done():
			return nil
		case <-exit:
			return nil
		case r, ok := <-input:
			if !ok {
				return nil
			}
			if r.err != nil {
				return fmt.Errorf("read input: %w", r.err)
			}
			if r.line == "" {
				needHeading = true
				continue
			}
			switch strings.ToLower(r.line) {
			case "exit", "quit":
				return nil
			}
			if ctx.Err() != nil {
				return nil
			}

			turnCtx, cancel := context.WithCancel(ctx)
			setTurn(cancel)

			onEvent, flush := chatEvents(opts.Stdout)
			_, runErr := eng.RunTurn(turnCtx, r.line, onEvent)
			flush()

			setTurn(nil)
			cancel()
			needHeading = true
			interrupted := takeInterrupted()

			if runErr == nil {
				// A signal landed just as the turn finished: exit cleanly.
				if interrupted {
					return nil
				}
				continue
			}
			if interrupted && turnCtx.Err() != nil {
				fmt.Fprintln(opts.Stdout, "(interrupted)")
				continue
			}
			fmt.Fprintf(opts.Stdout, "error: %v\n", runErr)
		}
	}
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

// chatEvents returns the event callback plus a flush function. All output
// goes to stdout: assistant text under a ">>> Assistant:" heading, tool
// activity as heading blocks, and streamed fragments written inline. The
// flush function terminates any partial streamed line after the turn.
// Streamed fragments are written without a trailing newline; whole-message
// events write complete blocks.
func chatEvents(out io.Writer) (func(engine.Event) error, func()) {
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
	heading := func(text string) {
		endLine()
		fmt.Fprintf(out, "\n%s\n", text)
	}

	// startRound resets per-round heading state: a tool result ends the
	// current round, so the next response's blocks start fresh.
	// streamedToolCall is deliberately not reset: a streamed round already
	// rendered every subsequent whole-message tool call inline, so
	// suppressing whole-message tool blocks stays correct for the rest of
	// the turn.
	startRound := func() {
		printedHeading = false
		printedThinking = false
		toolHeadings = map[int]bool{}
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
			fmt.Fprintln(out, ev.Output)
			// A tool result marks the end of an assistant round: the next
			// response starts a fresh set of heading blocks.
			startRound()
		}
		return nil
	}

	flush := endLine

	return onEvent, flush
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

// NewClient builds the LLM client described by a config, switching on
// provider.type. When a second provider lands this graduates to a registry
// map. getenv is the environment lookup, injectable for tests; pass
// os.Getenv in production. sink receives LLM wire logs; nil disables them.
func NewClientWithGetenv(cfg config.Config, getenv func(string) string, sink logging.Sink) (llm.Client, error) {
	switch cfg.Provider.Type {
	case config.ProviderTypeOpenAI:
		apiKey := ""
		if envName := cfg.Provider.APIKeyEnvOrDefault(); envName != "" {
			apiKey = getenv(envName)
			if apiKey == "" {
				return nil, fmt.Errorf("api_key_env %q is set but the environment variable is empty", envName)
			}
		}
		return openai.New(openai.Config{
			BaseURL: cfg.Provider.BaseURL,
			Model:   cfg.Provider.Model,
			APIKey:  apiKey,
			Sink:    sink,
		})
	default:
		return nil, fmt.Errorf("provider type %q is not supported (supported: %v)", cfg.Provider.Type, config.SupportedProviderTypes())
	}
}

// NewClient builds the LLM client described by a config using os.Getenv.
func NewClient(cfg config.Config) (llm.Client, error) {
	return NewClientWithGetenv(cfg, os.Getenv, logging.NewNop())
}

func (o Options) newClient(sink logging.Sink) (llm.Client, error) {
	if o.NewClient != nil {
		return o.NewClient(o.Config)
	}
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	return NewClientWithGetenv(o.Config, getenv, sink)
}
