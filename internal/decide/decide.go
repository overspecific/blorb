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
	// StateJSON treats the state argument as raw JSON instead of a plain
	// string: the bytes are passed to the server verbatim, so a structured
	// object or array state is expressible. Invalid JSON is an error.
	StateJSON bool
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

	raw, err := encodeState(state, opts.StateJSON)
	if err != nil {
		return err
	}

	runner := engine.NewDeciderRunner(engine.DeciderRunnerConfig{
		Config:            opts.Config,
		NewDecisionClient: opts.newDecisionClientFor(sink),
	})

	res, err := runner.RunDecider(ctx, opts.Decider.Name, raw)
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

// encodeState turns the state text into the wire state: as raw JSON passed
// through verbatim when asJSON is set, else as a JSON string. asJSON with
// invalid JSON is an error.
func encodeState(state string, asJSON bool) (json.RawMessage, error) {
	if !asJSON {
		return json.RawMessage(strconv.Quote(state)), nil
	}
	if !json.Valid([]byte(state)) {
		return nil, errors.New("state is not valid JSON")
	}
	return json.RawMessage(state), nil
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
