// Package decide implements the decide mode: one state in, one decider's
// answers out, the decider counterpart of the run command.
package decide

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/overspecific/blorb/internal/chat"
	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/engine"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/logging"
)

// Options configures one decision.
type Options struct {
	Config     config.Config
	Decider    config.Decider
	Stdout     io.Writer
	Stderr     io.Writer
	ConfigPath string
	// NewDecisionClient overrides decision client construction.
	// Tests only; nil uses the real path.
	NewDecisionClient func(cfg config.Config, decider config.Decider) (llm.DeciderClient, error)
}

// Run evaluates the named decider against state and writes the answers
// JSON to opts.Stdout.
func Run(ctx context.Context, opts Options, state string) error {
	sink, err := chat.ResolveSink(opts.ConfigPath, opts.Config)
	if err != nil {
		return err
	}

	runner := engine.NewDeciderRunner(engine.DeciderRunnerConfig{
		Config:            opts.Config,
		NewDecisionClient: opts.newDecisionClientFor(sink),
	})

	res, err := runner.RunDecider(ctx, opts.Decider.Name, json.RawMessage(strconv.Quote(state)))
	if err != nil {
		return err
	}
	if res.Err {
		if opts.Stderr != nil {
			fmt.Fprintln(opts.Stderr, res.Output)
		}
		return errors.New(res.Output)
	}
	if opts.Stdout != nil {
		fmt.Fprintln(opts.Stdout, res.Output)
	}
	return nil
}

// newDecisionClientFor returns the decision client factory: the injected
// NewDecisionClient when set, else the real path with os.Getenv.
func (o Options) newDecisionClientFor(sink logging.Sink) func(cfg config.Config, decider config.Decider) (llm.DeciderClient, error) {
	return func(cfg config.Config, decider config.Decider) (llm.DeciderClient, error) {
		if o.NewDecisionClient != nil {
			return o.NewDecisionClient(cfg, decider)
		}
		return chat.NewDecisionClientWithGetenv(cfg, decider, os.Getenv, sink)
	}
}
