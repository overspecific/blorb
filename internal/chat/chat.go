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

	"github.com/overspecific/goblorb/internal/config"
	"github.com/overspecific/goblorb/internal/engine"
	"github.com/overspecific/goblorb/internal/llm"
	"github.com/overspecific/goblorb/internal/llm/openai"
	"github.com/overspecific/goblorb/internal/tools"
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
	wasInterrupted := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return interrupted
	}

	exit := make(chan struct{})
	var once sync.Once
	requestExit := func() { once.Do(func() { close(exit) }) }

	sigint := make(chan os.Signal, 1)
	if opts.SigintChan != nil {
		// Forward from the injected test channel.
		go func() {
			for sig := range opts.SigintChan {
				sigint <- sig
			}
		}()
	} else {
		signal.Notify(sigint, syscall.SIGINT)
		defer signal.Stop(sigint)
	}

	go func() {
		for range sigint {
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
	}()

	fmt.Fprintf(opts.Stderr, "blorb %s (%s, %s)\n", opts.Version, opts.Config.Name, opts.Config.Provider.Model)

	input := make(chan inputResult, 1)
	go readLines(opts.Stdin, input)

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

			_, runErr := eng.RunTurn(turnCtx, r.line, chatEvents(opts.Stdout, opts.Stderr))

			setTurn(nil)
			cancel()
			needHeading = true
			interrupted := wasInterrupted()

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

// readLines pumps trimmed lines of input until EOF or error.
func readLines(r io.Reader, out chan<- inputResult) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		out <- inputResult{line: strings.TrimSpace(scanner.Text())}
	}
	if err := scanner.Err(); err != nil {
		out <- inputResult{err: err}
	}
	close(out)
}

// chatEvents returns the event callback: assistant text to stdout under a
// ">>> Assistant:" heading, tool activity as heading blocks on stderr.
func chatEvents(stdout, stderr io.Writer) func(engine.Event) error {
	printedHeading := false
	return func(ev engine.Event) error {
		switch ev.Kind {
		case engine.EventAssistantText:
			if !printedHeading {
				fmt.Fprintln(stdout, "\n>>> Assistant:")
				printedHeading = true
			}
			fmt.Fprintln(stdout, ev.Text)
		case engine.EventToolCall:
			fmt.Fprintf(stderr, "\n>>> Tool: %s\n", ev.Name)
			fmt.Fprintln(stderr, ev.Args)
		case engine.EventToolResult:
			marker := "Result:"
			if ev.Failed {
				marker = "Error:"
			}
			fmt.Fprintf(stderr, "\n>>> %s Tool: %s\n%s\n", marker, ev.Name, ev.Output)
		}
		return nil
	}
}

// NewClient builds the LLM client described by a config, switching on
// provider.type. When a second provider lands this graduates to a registry
// map.
func NewClient(cfg config.Config) (llm.Client, error) {
	switch cfg.Provider.Type {
	case config.ProviderTypeOpenAI:
		apiKey := ""
		if cfg.Provider.APIKeyEnv != "" {
			apiKey = os.Getenv(cfg.Provider.APIKeyEnv)
			if apiKey == "" {
				return nil, fmt.Errorf("api_key_env %q is set but the environment variable is empty", cfg.Provider.APIKeyEnv)
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

func (o Options) newClient() (llm.Client, error) {
	if o.NewClient != nil {
		return o.NewClient(o.Config)
	}
	return NewClient(o.Config)
}
