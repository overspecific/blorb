package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/engine"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/tools"
)

// fakeDeciderClient records decision requests and returns canned responses.
type fakeDeciderClient struct {
	responses []*llm.DecisionResponse
	err       error
	requests  []llm.DecisionRequest
}

func (f *fakeDeciderClient) Decide(_ context.Context, req llm.DecisionRequest) (*llm.DecisionResponse, error) {
	f.requests = append(f.requests, req)
	if len(f.responses) > 0 {
		resp := f.responses[0]
		f.responses = f.responses[1:]
		return resp, nil
	}
	if f.err != nil {
		return nil, f.err
	}
	return nil, errors.New("fakeDeciderClient: no more canned responses")
}

// deciderConfig builds an in-memory config with a decision model and one
// decider named "triage", plus an agent "worker" that may be granted tools.
func deciderConfig(t *testing.T) config.Config {
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
			Name:         "worker",
			SystemPrompt: "You work.",
			Model:        "m",
			MaxTurns:     10,
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
				"escalate": {
					Type:         config.QuestionTypeNoul,
					Instructions: json.RawMessage(`"Escalate?"`),
				},
			},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("deciderConfig invalid: %v", err)
	}
	return cfg
}

func newDeciderFactory(client llm.DeciderClient) func(config.Config, config.Decider) (llm.DeciderClient, error) {
	return func(config.Config, config.Decider) (llm.DeciderClient, error) {
		return client, nil
	}
}

func TestDeciderRunnerUsesConfig(t *testing.T) {
	t.Parallel()

	cfg := deciderConfig(t)
	fake := &fakeDeciderClient{responses: []*llm.DecisionResponse{{
		Model: "jev-1.13.0",
		Answers: map[string]llm.DecisionAnswer{
			"priority": {Type: "choice", Choice: "high"},
			"escalate": {Type: "noul", Noul: ptr(0.95)},
		},
		Usage: llm.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12},
		Stats: llm.CallStats{Elapsed: time.Second},
	}}}

	runner := engine.NewDeciderRunner(engine.DeciderRunnerConfig{
		Config:            cfg,
		NewDecisionClient: newDeciderFactory(fake),
	})

	res, err := runner.RunDecider(context.Background(), "triage", json.RawMessage(`"the ticket"`))
	if err != nil {
		t.Fatalf("RunDecider error = %v, want nil", err)
	}
	if res.Err {
		t.Errorf("res.Err = true, want false")
	}
	if res.Output != `{"escalate":{"type":"noul","noul":0.95},"priority":{"type":"choice","choice":"high"}}` {
		t.Errorf("res.Output = %q, want compact answers JSON", res.Output)
	}

	if len(fake.requests) != 1 {
		t.Fatalf("decision calls = %d, want 1", len(fake.requests))
	}
	req := fake.requests[0]
	if req.Model != "jev-latest" {
		t.Errorf("request model = %q, want jev-latest", req.Model)
	}
	if string(req.State) != `"the ticket"` {
		t.Errorf("request state = %s, want the state verbatim", req.State)
	}
	if len(req.Questions) != 2 {
		t.Fatalf("request questions = %d, want 2", len(req.Questions))
	}
	prio := req.Questions["priority"]
	if prio.Type != "choice" || string(prio.Instructions) != `"How urgent?"` || string(prio.Criteria) != `{"low":"Low","high":"High"}` {
		t.Errorf("priority question = %+v, want the config question verbatim", prio)
	}

	if len(res.Usage) != 1 {
		t.Fatalf("res.Usage = %d records, want 1", len(res.Usage))
	}
	if res.Usage[0].Agent != "triage" || res.Usage[0].Model != "jev-1.13.0" {
		t.Errorf("usage record = %+v, want the decider name and resolved model", res.Usage[0])
	}
	if res.Usage[0].Usage != (llm.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}) {
		t.Errorf("usage = %+v, want the response usage", res.Usage[0].Usage)
	}
}

func TestDeciderRunnerFallsBackToConfiguredModelName(t *testing.T) {
	t.Parallel()

	cfg := deciderConfig(t)
	fake := &fakeDeciderClient{responses: []*llm.DecisionResponse{{Answers: map[string]llm.DecisionAnswer{}}}}
	runner := engine.NewDeciderRunner(engine.DeciderRunnerConfig{
		Config:            cfg,
		NewDecisionClient: newDeciderFactory(fake),
	})
	res, err := runner.RunDecider(context.Background(), "triage", json.RawMessage(`"x"`))
	if err != nil {
		t.Fatalf("RunDecider error = %v, want nil", err)
	}
	if res.Usage[0].Model != "jev-latest" {
		t.Errorf("usage model = %q, want the configured model name when the server reports none", res.Usage[0].Model)
	}
}

func TestDeciderRunnerClientErrorIsDeciderFailure(t *testing.T) {
	t.Parallel()

	cfg := deciderConfig(t)
	fake := &fakeDeciderClient{err: errors.New("server rejected the request")}
	runner := engine.NewDeciderRunner(engine.DeciderRunnerConfig{
		Config:            cfg,
		NewDecisionClient: newDeciderFactory(fake),
	})

	res, err := runner.RunDecider(context.Background(), "triage", json.RawMessage(`"x"`))
	if err != nil {
		t.Fatalf("RunDecider error = %v, want nil (a decider failure is a result)", err)
	}
	if !res.Err || !strings.Contains(res.Output, "server rejected") {
		t.Errorf("res = %+v, want an Err result carrying the client error", res)
	}
}

func TestDeciderRunnerCancellationIsInfrastructure(t *testing.T) {
	t.Parallel()

	cfg := deciderConfig(t)
	fake := &fakeDeciderClient{err: errors.New("whatever")}
	runner := engine.NewDeciderRunner(engine.DeciderRunnerConfig{
		Config:            cfg,
		NewDecisionClient: newDeciderFactory(fake),
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := runner.RunDecider(ctx, "triage", json.RawMessage(`"x"`))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestDeciderRunnerGoErrors(t *testing.T) {
	t.Parallel()

	t.Run("undefined decider", func(t *testing.T) {
		t.Parallel()
		runner := engine.NewDeciderRunner(engine.DeciderRunnerConfig{Config: deciderConfig(t)})
		_, err := runner.RunDecider(context.Background(), "ghost", json.RawMessage(`"x"`))
		if err == nil || !strings.Contains(err.Error(), `decider "ghost" is not defined`) {
			t.Errorf("error = %v, want an undefined-decider error", err)
		}
	})

	t.Run("non-decision model", func(t *testing.T) {
		t.Parallel()
		cfg := deciderConfig(t)
		cfg.Deciders[0].Model = "m"
		runner := engine.NewDeciderRunner(engine.DeciderRunnerConfig{Config: cfg})
		_, err := runner.RunDecider(context.Background(), "triage", json.RawMessage(`"x"`))
		if err == nil || !strings.Contains(err.Error(), "is not a decision model") {
			t.Errorf("error = %v, want a non-decision-model error", err)
		}
	})

	t.Run("nil client factory", func(t *testing.T) {
		t.Parallel()
		runner := engine.NewDeciderRunner(engine.DeciderRunnerConfig{Config: deciderConfig(t)})
		_, err := runner.RunDecider(context.Background(), "triage", json.RawMessage(`"x"`))
		if err == nil || !strings.Contains(err.Error(), "no decision client factory") {
			t.Errorf("error = %v, want a nil-factory error", err)
		}
	})

	t.Run("failing client factory", func(t *testing.T) {
		t.Parallel()
		runner := engine.NewDeciderRunner(engine.DeciderRunnerConfig{
			Config: deciderConfig(t),
			NewDecisionClient: func(config.Config, config.Decider) (llm.DeciderClient, error) {
				return nil, errors.New("factory exploded")
			},
		})
		_, err := runner.RunDecider(context.Background(), "triage", json.RawMessage(`"x"`))
		if err == nil || !strings.Contains(err.Error(), "factory exploded") {
			t.Errorf("error = %v, want the factory error", err)
		}
	})
}

// TestDeciderRunnerNestedDepth wires a decider runner into a subagent
// runner: a parent agent holds both a decider tool and a subagent tool, and
// the decider's events must reach the top-level callback at the expected
// depth (directly invoked deciders at 1, deciders called by the subagent at
// 2).
func TestDeciderRunnerNestedDepth(t *testing.T) {
	t.Parallel()

	cfg := deciderConfig(t)
	cfg.Agents = append(cfg.Agents, config.Agent{
		Name:         "parent",
		SystemPrompt: "Parent.",
		Model:        "m",
		MaxTurns:     10,
	})
	cfg.Tools = append(cfg.Tools,
		config.ToolEntry{Type: config.ToolTypeSubagent, Name: "ask_worker", Description: "Delegate.", Agent: "worker"},
		config.ToolEntry{Type: config.ToolTypeDecider, Name: "triage_direct", Description: "Triage.", Decider: "triage"},
		config.ToolEntry{Type: config.ToolTypeDecider, Name: "triage_nested", Description: "Triage.", Decider: "triage"},
	)
	parent := mustAgent(t, cfg, "parent")
	parent.Tools = []string{"ask_worker", "triage_direct"}
	for i := range cfg.Agents {
		if cfg.Agents[i].Name == "parent" {
			cfg.Agents[i] = parent
		}
	}
	worker := mustAgent(t, cfg, "worker")
	worker.Tools = []string{"triage_nested"}
	for i := range cfg.Agents {
		if cfg.Agents[i].Name == "worker" {
			cfg.Agents[i] = worker
		}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate error = %v, want nil", err)
	}

	fakeDecider := &fakeDeciderClient{responses: []*llm.DecisionResponse{
		{Model: "jev-1.13.0", Answers: map[string]llm.DecisionAnswer{"priority": {Type: "choice", Choice: "high"}}},
		{Model: "jev-1.13.0", Answers: map[string]llm.DecisionAnswer{"priority": {Type: "choice", Choice: "low"}}},
	}}
	deciderRunner := engine.NewDeciderRunner(engine.DeciderRunnerConfig{
		Config:            cfg,
		NewDecisionClient: newDeciderFactory(fakeDecider),
	})

	clientWorker := &fakeClient{responses: []llm.Response{
		toolCallResp(call("c1", "triage_nested", `{"state":"nested ticket"}`)),
		textResp("worker final"),
	}}
	clientParent := &fakeClient{responses: []llm.Response{
		toolCallResp(call("p1", "triage_direct", `{"state":"direct ticket"}`)),
		toolCallResp(call("p2", "ask_worker", `{"prompt":"go"}`)),
		textResp("parent final"),
	}}
	factory := &clientFactory{clients: map[string]llm.Client{
		"parent": clientParent,
		"worker": clientWorker,
	}}
	subRunner := engine.NewSubagentRunner(engine.SubagentRunnerConfig{
		Config:        cfg,
		NewClient:     factory.newClient,
		DeciderRunner: deciderRunner,
	})

	var seen []tools.SubagentEvent
	registry, err := tools.NewRegistry(
		cfg.AgentTools(mustAgent(t, cfg, "parent")),
		tools.WithSubagentRunner(subRunner),
		tools.WithDeciderRunner(deciderRunner),
		tools.WithSubagentEvents(func(ev tools.SubagentEvent) error {
			seen = append(seen, ev)
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}
	defer registry.Close()

	parentEng := engine.New(engine.EngineConfig{Client: clientParent, Tools: registry})
	final, err := parentEng.RunTurn(context.Background(), "go", func(engine.Event) error { return nil })
	if err != nil {
		t.Fatalf("RunTurn error = %v, want nil", err)
	}
	if final != "parent final" {
		t.Errorf("final = %q, want parent final", final)
	}

	depths := map[string][]int{}
	for _, ev := range seen {
		if ev.Kind == tools.SubagentDecision {
			depths[ev.Agent] = append(depths[ev.Agent], ev.Depth)
		}
	}
	if len(depths["triage"]) != 2 {
		t.Fatalf("decision events = %v, want two events for the triage decider", depths)
	}
	if depths["triage"][0] != 1 {
		t.Errorf("direct decision depth = %d, want 1", depths["triage"][0])
	}
	if depths["triage"][1] != 2 {
		t.Errorf("nested decision depth = %d, want 2", depths["triage"][1])
	}
}

func ptr[T any](v T) *T { return &v }

// TestDeciderRunnerJudgeCanCallDecider covers a judge agent granted a
// decider tool: the judge runner must wire the decider runner into the
// judge's registry.
func TestDeciderRunnerJudgeCanCallDecider(t *testing.T) {
	t.Parallel()

	cfg := deciderConfig(t)
	cfg.Agents = append(cfg.Agents, config.Agent{
		Name:         "judge",
		SystemPrompt: "You judge.",
		Model:        "m",
		MaxTurns:     10,
	})
	main := mustAgent(t, cfg, "worker")
	main.Judges = []config.Judge{{Agent: "judge"}}
	for i := range cfg.Agents {
		if cfg.Agents[i].Name == "worker" {
			cfg.Agents[i] = main
		}
	}
	cfg.Tools = append(cfg.Tools, config.ToolEntry{
		Type: config.ToolTypeDecider, Name: "triage_tool", Description: "Triage.", Decider: "triage",
	})
	judge := mustAgent(t, cfg, "judge")
	judge.Tools = []string{"triage_tool"}
	for i := range cfg.Agents {
		if cfg.Agents[i].Name == "judge" {
			cfg.Agents[i] = judge
		}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate error = %v, want nil", err)
	}

	fakeDecider := &fakeDeciderClient{responses: []*llm.DecisionResponse{{
		Model:   "jev-1.13.0",
		Answers: map[string]llm.DecisionAnswer{"priority": {Type: "choice", Choice: "high"}},
	}}}
	deciderRunner := engine.NewDeciderRunner(engine.DeciderRunnerConfig{
		Config:            cfg,
		NewDecisionClient: newDeciderFactory(fakeDecider),
	})

	clientJudge := &fakeClient{responses: []llm.Response{
		toolCallResp(call("j1", "triage_tool", `{"state":"judge state"}`)),
		textResp("verdict: fine"),
	}}
	factory := &clientFactory{clients: map[string]llm.Client{"judge": clientJudge}}
	judgeRunner := engine.NewJudgeRunner(engine.JudgeRunnerConfig{
		Config:        cfg,
		NewClient:     factory.newClient,
		DeciderRunner: deciderRunner,
	})

	main = mustAgent(t, cfg, "worker")
	outcomes, err := judgeRunner.RunJudges(context.Background(), main, config.JudgeWhenEnd, "the transcript", nil)
	if err != nil {
		t.Fatalf("RunJudges error = %v, want nil", err)
	}
	if len(outcomes) != 1 || outcomes[0].Output != "verdict: fine" {
		t.Fatalf("outcomes = %+v, want the judge verdict", outcomes)
	}
	if len(fakeDecider.requests) != 1 {
		t.Fatalf("decision calls = %d, want 1 (the judge used the decider tool)", len(fakeDecider.requests))
	}
	if string(fakeDecider.requests[0].State) != `"judge state"` {
		t.Errorf("decision state = %s, want the judge's tool-call state", fakeDecider.requests[0].State)
	}
}
