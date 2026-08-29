// Package chat implements the interactive REPL run mode.
package chat

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/engine"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/llm/openai"
	"github.com/overspecific/blorb/internal/tools"
)

// Options configure a chat session.
type Options struct {
	Config     config.Config
	Version    string
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	NewClient  func(cfg config.Config) (llm.Client, error)
	SigintChan <-chan os.Signal
	// Getenv overrides the environment lookup used to resolve
	// provider.api_key_env; os.Getenv when nil. Tests only.
	Getenv func(string) string
	// Stream enables incremental rendering of assistant responses: when
	// true and the client supports it, text, reasoning, and tool call
	// fragments are printed as they arrive. Disabled by --no-stream.
	Stream bool
}

// Run drives one interactive chat session. It returns nil on graceful exit
// (exit/quit, EOF, or SIGINT when no turn is in flight) and an error on
// startup failures. Turn failures are printed to stderr and keep the
// session alive. SIGINT during a turn cancels just that turn.
func Run(ctx context.Context, opts Options) error {
	client, err := opts.newClient()
	if err != nil {
		return err
	}

	registry, err := tools.NewRegistry(opts.Config.Tools)
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

	fmt.Fprintf(opts.Stderr, "blorb %s (%s, %s)\n", opts.Version, opts.Config.Name, opts.Config.Provider.Model)

	input := make(chan inputResult, 1)
	go readLines(opts.Stdin, input, done)

	needHeading := true
	for {
		if needHeading {
			fmt.Fprint(opts.Stderr, "\n>>> User:\n")
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

			onEvent, flush := chatEvents(opts.Stdout, opts.Stderr)
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
				fmt.Fprintln(opts.Stderr, "(interrupted)")
				continue
			}
			fmt.Fprintf(opts.Stderr, "error: %v\n", runErr)
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

// chatEvents returns the event callback plus a flush function. The callback
// renders assistant text under a ">>> Assistant:" heading and tool activity
// as heading blocks on stderr; the flush function writes the trailing
// newline after streamed delta output. Delta events (streaming) write
// fragments without trailing newlines; the whole-message events write
// complete blocks.
func chatEvents(stdout, stderr io.Writer) (func(engine.Event) error, func()) {
	var (
		printedHeading   bool
		printedThinking  bool
		streamedToolCall bool
		toolHeadings     = map[string]bool{}
		wroteToStdout    bool
	)

	// flushStdout terminates any streamed partial line on stdout before
	// stderr activity begins, so the two streams stay readable.
	flushStdout := func() {
		if wroteToStdout {
			fmt.Fprint(stdout, "\n")
			wroteToStdout = false
		}
	}

	onEvent := func(ev engine.Event) error {
		switch ev.Kind {
		case engine.EventAssistantThinking:
			fmt.Fprintf(stdout, "\n>>> Assistant (thinking):\n%s\n", ev.Text)
		case engine.EventAssistantText:
			if !printedHeading {
				fmt.Fprintln(stdout, "\n>>> Assistant:")
				printedHeading = true
			}
			fmt.Fprintln(stdout, ev.Text)
		case engine.EventAssistantThinkingDelta:
			if !printedThinking {
				fmt.Fprint(stdout, "\n>>> Assistant (thinking):\n")
				printedThinking = true
			}
			fmt.Fprint(stdout, ev.Text)
			wroteToStdout = true
		case engine.EventAssistantTextDelta:
			if !printedHeading {
				fmt.Fprintln(stdout, "\n>>> Assistant:")
				printedHeading = true
			}
			fmt.Fprint(stdout, ev.Text)
			wroteToStdout = true
		case engine.EventToolCallDelta:
			if !toolHeadings[ev.Name] {
				toolHeadings[ev.Name] = true
				flushStdout()
				fmt.Fprintf(stderr, "\n>>> Tool: %s\n", ev.Name)
			}
			fmt.Fprint(stderr, ev.Args)
			streamedToolCall = true
		case engine.EventToolCall:
			// The streamed fragments already rendered this call; skip the
			// whole-message block to avoid printing it twice.
			if !streamedToolCall {
				fmt.Fprintf(stderr, "\n>>> Tool: %s\n", ev.Name)
				fmt.Fprintln(stderr, ev.Args)
			}
		case engine.EventToolResult:
			// A partial streamed line on stdout must be terminated before
			// writing to stderr.
			flushStdout()
			marker := "Result:"
			if ev.Failed {
				marker = "Error:"
			}
			fmt.Fprintf(stderr, "\n>>> %s Tool: %s\n%s\n", marker, ev.Name, ev.Output)
		}
		return nil
	}

	flush := func() {
		flushStdout()
	}

	return onEvent, flush
}

// NewClient builds the LLM client described by a config, switching on
// provider.type. When a second provider lands this graduates to a registry
// map. getenv is the environment lookup, injectable for tests; pass
// os.Getenv in production.
func NewClientWithGetenv(cfg config.Config, getenv func(string) string) (llm.Client, error) {
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
		})
	default:
		return nil, fmt.Errorf("provider type %q is not supported (supported: %v)", cfg.Provider.Type, config.SupportedProviderTypes())
	}
}

// NewClient builds the LLM client described by a config using os.Getenv.
func NewClient(cfg config.Config) (llm.Client, error) {
	return NewClientWithGetenv(cfg, os.Getenv)
}

func (o Options) newClient() (llm.Client, error) {
	if o.NewClient != nil {
		return o.NewClient(o.Config)
	}
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	return NewClientWithGetenv(o.Config, getenv)
}
