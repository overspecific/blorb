package decide_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/decide"
	"github.com/overspecific/blorb/internal/llm"
)

// fakeDecisionClient records requests and returns canned responses.
type fakeDecisionClient struct {
	requests []llm.DecisionRequest
	response *llm.DecisionResponse
	err      error
}

func (f *fakeDecisionClient) Decide(_ context.Context, req llm.DecisionRequest) (*llm.DecisionResponse, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		return nil, f.err
	}
	if f.response != nil {
		return f.response, nil
	}
	return &llm.DecisionResponse{Answers: map[string]llm.DecisionAnswer{}}, nil
}

func decideConfig(t *testing.T) (config.Config, config.Decider) {
	t.Helper()

	cfg := config.Config{
		Providers: []config.Provider{{
			Name:    "local",
			Type:    config.ProviderTypeOpenAI,
			BaseURL: "http://localhost:1",
		}},
		Models: []config.Model{
			{Name: "m", Provider: "local", ModelName: "m"},
			{Name: "jev", Provider: "local", ModelName: "jev-latest", ModelType: config.ModelTypeDecision},
		},
		Agents: []config.Agent{{
			Name: "helper", SystemPrompt: "You are helpful.", Model: "m", MaxTurns: 1,
		}},
		Deciders: []config.Decider{{
			Name:  "triage",
			Model: "jev",
			Questions: map[string]config.Question{
				"priority": {
					Type:         config.QuestionTypeChoice,
					Instructions: json.RawMessage(`"How urgent?"`),
					Criteria:     json.RawMessage(`{"low":"Low","high":"High"}`),
				},
			},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("decideConfig invalid: %v", err)
	}
	d, ok := cfg.Decider("triage")
	if !ok {
		t.Fatal("triage decider missing")
	}
	return cfg, d
}

func TestRunHappyPath(t *testing.T) {
	t.Parallel()

	cfg, decider := decideConfig(t)
	fake := &fakeDecisionClient{response: &llm.DecisionResponse{
		Model:   "jev-1.13.0",
		Answers: map[string]llm.DecisionAnswer{"priority": {Type: "choice", Choice: "high"}},
	}}

	var stdout, stderr strings.Builder
	err := decide.Run(context.Background(), decide.Options{
		Config:  cfg,
		Decider: decider,
		Stdout:  &stdout,
		Stderr:  &stderr,
		NewDecisionClient: func(config.Config, config.Decider) (llm.DeciderClient, error) {
			return fake, nil
		},
	}, "the ticket")
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	if got, want := stdout.String(), `{"priority":{"type":"choice","choice":"high"}}`+"\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
	if len(fake.requests) != 1 {
		t.Fatalf("decision calls = %d, want 1", len(fake.requests))
	}
	if string(fake.requests[0].State) != `"the ticket"` {
		t.Errorf("state = %s, want the state as a JSON string", fake.requests[0].State)
	}
	if len(fake.requests[0].Questions) != 1 {
		t.Errorf("questions = %d, want the decider's one question", len(fake.requests[0].Questions))
	}
}

func TestRunDeciderFailure(t *testing.T) {
	t.Parallel()

	cfg, decider := decideConfig(t)
	fake := &fakeDecisionClient{err: errors.New("server rejected the request")}

	var stdout, stderr strings.Builder
	err := decide.Run(context.Background(), decide.Options{
		Config:  cfg,
		Decider: decider,
		Stdout:  &stdout,
		Stderr:  &stderr,
		NewDecisionClient: func(config.Config, config.Decider) (llm.DeciderClient, error) {
			return fake, nil
		},
	}, "the ticket")
	if err == nil || !strings.Contains(err.Error(), "server rejected") {
		t.Errorf("Run error = %v, want the decider failure", err)
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "server rejected") {
		t.Errorf("stderr = %q, want the failure body", stderr.String())
	}
}

func TestRunGoErrorPropagates(t *testing.T) {
	t.Parallel()

	cfg, decider := decideConfig(t)
	err := decide.Run(context.Background(), decide.Options{
		Config:  cfg,
		Decider: decider,
		Stdout:  &strings.Builder{},
		Stderr:  &strings.Builder{},
		NewDecisionClient: func(config.Config, config.Decider) (llm.DeciderClient, error) {
			return nil, errors.New("factory exploded")
		},
	}, "the ticket")
	if err == nil || !strings.Contains(err.Error(), "factory exploded") {
		t.Errorf("Run error = %v, want the factory error", err)
	}
}

func TestRunRealPathModelError(t *testing.T) {
	t.Parallel()

	cfg, _ := decideConfig(t)
	// Point the decider at an undefined model to force the real path
	// (nil NewDecisionClient) to fail during resolution.
	cfg.Deciders = []config.Decider{{Name: "triage", Model: "ghost"}}
	err := decide.Run(context.Background(), decide.Options{
		Config:  cfg,
		Decider: cfg.Deciders[0],
		Stdout:  &strings.Builder{},
		Stderr:  &strings.Builder{},
	}, "the ticket")
	if err == nil || !strings.Contains(err.Error(), "not a defined model") {
		t.Errorf("Run error = %v, want a model-resolution error", err)
	}
}
