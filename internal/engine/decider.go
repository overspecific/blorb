package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/tools"
)

// DeciderRunnerConfig configures a DeciderRunner.
type DeciderRunnerConfig struct {
	// Config is the whole loaded config: deciders and their models
	// are resolved from it.
	Config config.Config
	// NewDecisionClient builds the decision client for a decider's
	// named model.
	NewDecisionClient func(cfg config.Config, decider config.Decider) (llm.DeciderClient, error)
}

// DeciderRunner evaluates named deciders from one config. It implements
// tools.DeciderRunner.
type DeciderRunner struct {
	cfg DeciderRunnerConfig
}

// NewDeciderRunner constructs a DeciderRunner.
func NewDeciderRunner(cfg DeciderRunnerConfig) *DeciderRunner {
	return &DeciderRunner{cfg: cfg}
}

// RunDecider evaluates the named decider against state: one decision-model
// call with the decider's configured questions, returning the answers as
// JSON. A client error is a decider-level failure the parent model sees; a
// cancelled context is infrastructure and surfaces as an error. See
// tools.DeciderRunner.
func (r *DeciderRunner) RunDecider(ctx context.Context, deciderName string, state json.RawMessage) (tools.DeciderResult, error) {
	decider, ok := r.cfg.Config.Decider(deciderName)
	if !ok {
		return tools.DeciderResult{}, fmt.Errorf("decider %q is not defined in the config", deciderName)
	}

	model, ok := r.cfg.Config.Model(decider.Model)
	if !ok {
		return tools.DeciderResult{}, fmt.Errorf("decider %q: model %q is not a defined model", deciderName, decider.Model)
	}
	if model.ResolvedModelType() != config.ModelTypeDecision {
		return tools.DeciderResult{}, fmt.Errorf("decider %q: model %q is not a decision model", deciderName, decider.Model)
	}

	client, err := r.newClient(decider)
	if err != nil {
		return tools.DeciderResult{}, err
	}

	resp, err := client.Decide(ctx, llm.DecisionRequest{
		Model:     model.ModelName,
		State:     state,
		Questions: decisionQuestions(decider.Questions),
	})
	if ctx.Err() != nil {
		// Cancellation (e.g. SIGINT) is infrastructure, not a decider
		// outcome: surface it as an error so the caller can treat the
		// whole turn as interrupted.
		return tools.DeciderResult{}, fmt.Errorf("decider %q: %w", deciderName, ctx.Err())
	}
	if err != nil {
		// Any other failure is a decider-level failure the parent model
		// sees and can react to, like a tool-reported failure.
		return tools.DeciderResult{Output: err.Error(), Err: true}, nil
	}

	buf, err := json.Marshal(resp.Answers)
	if err != nil {
		return tools.DeciderResult{}, fmt.Errorf("decider %q: marshal answers: %w", deciderName, err)
	}

	// Prefer the server's resolved model name: it names a real model even
	// when the config leaves model_name unset.
	modelName := resp.Model
	if modelName == "" {
		modelName = model.ModelName
	}
	return tools.DeciderResult{
		Output: string(buf),
		Err:    false,
		Usage: []tools.SubagentUsageRecord{{
			Agent: deciderName,
			Model: modelName,
			Usage: resp.Usage,
			Stats: resp.Stats,
		}},
	}, nil
}

func (r *DeciderRunner) newClient(decider config.Decider) (llm.DeciderClient, error) {
	if r.cfg.NewDecisionClient == nil {
		return nil, fmt.Errorf("decider %q: no decision client factory configured", decider.Name)
	}
	client, err := r.cfg.NewDecisionClient(r.cfg.Config, decider)
	if err != nil {
		return nil, fmt.Errorf("build decider %q client: %w", decider.Name, err)
	}
	return client, nil
}

// decisionQuestions converts a decider's config questions to the neutral
// decision vocabulary, a field-for-field copy (the JSON tags match).
func decisionQuestions(questions map[string]config.Question) map[string]llm.DecisionQuestion {
	out := make(map[string]llm.DecisionQuestion, len(questions))
	for name, q := range questions {
		out[name] = llm.DecisionQuestion{
			Type:         q.Type,
			Instructions: q.Instructions,
			Criteria:     q.Criteria,
		}
	}
	return out
}
