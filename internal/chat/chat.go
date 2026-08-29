// Package chat implements the interactive REPL run mode.
package chat

import (
	"bufio"
	"context"
	"errors"
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
// (quit/exit, EOF, or a single SIGINT), and an error on startup or turn
// failures.
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

	// interruptMu guards currentTurn, which holds the context cancel func
	// for the in-flight (or next) turn. A signal cancels it; a second signal
	// while nothing is running exits immediately.
	var interruptMu sync.Mutex
	currentTurn := context.CancelFunc(func() {})

	handleSignal := make(chan os.Signal, 1)
	if opts.SigintChan != nil {
		// Tests inject a signal channel; receive-only clients still work.
		sigIn := opts.SigintChan
		fwd := make(chan os.Signal, 1)
		go func() {
			for sig := range sigIn {
				fwd <- sig
			}
		}()
		handleSignal = fwd
	} else {
		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, syscall.SIGINT)
		defer signal.Stop(sigint)
		handleSignal = sigint
	}
	go func() {
		for range handleSignal {
			interruptMu.Lock()
			cancel := currentTurn
			interruptMu.Unlock()
			if cancel != nil {
				cancel()
			}
		}
	}()

	fmt.Fprintf(opts.Stderr, "blorb %s (%s, %s)\n", opts.Version, opts.Config.Name, opts.Config.Provider.Model)

	scanner := bufio.NewScanner(opts.Stdin)
	for {
		line, err := readLine(scanner)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read input: %w", err)
		}
		if line == "" {
			continue
		}
		// A second SIGINT while nothing is in flight exits immediately.
		if ctx.Err() != nil {
			return nil
		}

		turnCtx, cancelTurn := context.WithCancel(ctx)
		interruptMu.Lock()
		currentTurn = cancelTurn
		interruptMu.Unlock()

		_, runErr := eng.RunTurn(turnCtx, line, chatEvents(opts.Stdout, opts.Stderr))

		interruptMu.Lock()
		currentTurn = nil
		interruptMu.Unlock()
		cancelTurn()

		if runErr == nil {
			continue
		}
		if turnCtx.Err() != nil && ctx.Err() == nil && !errors.Is(runErr, engine.ErrTooManyTurns) {
			// Interrupted by signal; fall through to a fresh prompt.
			fmt.Fprintln(opts.Stderr, "(interrupted)")
			continue
		}
		return runErr
	}
}

// chatEvents returns the event callback: assistant text to stdout, tool
// activity as indented stderr lines.
func chatEvents(stdout, stderr io.Writer) func(engine.Event) error {
	return func(ev engine.Event) error {
		switch ev.Kind {
		case engine.EventAssistantText:
			fmt.Fprintln(stdout, ev.Text)
		case engine.EventToolCall:
			fmt.Fprintf(stderr, "  → %s(%s)\n", ev.Name, ev.Args)
		case engine.EventToolResult:
			marker := "←"
			if ev.Failed {
				marker = "✗"
			}
			fmt.Fprintf(stderr, "  %s %s: %s\n", marker, ev.Name, ev.Output)
		}
		return nil
	}
}

// readLine reads one trimmed line of input.
func readLine(scanner *bufio.Scanner) (string, error) {
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", err
		}
		return "", io.EOF
	}
	return strings.TrimSpace(scanner.Text()), nil
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
