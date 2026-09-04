package engine_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/engine"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/tools"
)

// subagentConfig builds an in-memory config: agents (name -> prompt),
// delegations (tool name -> target agent), grants (agent name -> tool
// names, in order), and optional per-agent max_turns overrides.
func subagentConfig(t *testing.T, agents map[string]string, delegations map[string]string, grants map[string][]string, maxTurns map[string]int) config.Config {
	t.Helper()

	cfg := config.Config{
		Models: []config.Model{{
			Name:      "m",
			Type:      config.ModelTypeOpenAI,
			ModelName: "m",
			BaseURL:   "http://localhost:1",
		}},
		Agents: []config.Agent{},
		Tools:  []config.ToolEntry{},
	}
	for name, prompt := range agents {
		maxTurnsFor := maxTurns[name]
		if maxTurnsFor == 0 {
			maxTurnsFor = 10
		}
		cfg.Agents = append(cfg.Agents, config.Agent{
			Name:         name,
			SystemPrompt: prompt,
			Model:        "m",
			MaxTurns:     maxTurnsFor,
			Tools:        grants[name],
		})
	}
	for toolName, target := range delegations {
		cfg.Tools = append(cfg.Tools, config.ToolEntry{
			Type:        config.ToolTypeSubagent,
			Name:        toolName,
			Description: "Delegate to " + target + ".",
			Agent:       target,
		})
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("subagentConfig invalid: %v", err)
	}
	return cfg
}

// grantTool appends a tool entry to the config and grants it to the named
// agent, revalidating.
func grantTool(t *testing.T, cfg config.Config, entry config.ToolEntry, agentName string) config.Config {
	t.Helper()
	cfg.Tools = append(cfg.Tools, entry)
	agent := mustAgent(t, cfg, agentName)
	agent.Tools = append(agent.Tools, entry.Name)
	for i := range cfg.Agents {
		if cfg.Agents[i].Name == agentName {
			cfg.Agents[i] = agent
		}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("grantTool invalid: %v", err)
	}
	return cfg
}

func mustAgent(t *testing.T, cfg config.Config, name string) config.Agent {
	t.Helper()
	a, ok := cfg.Agent(name)
	if !ok {
		t.Fatalf("agent %q not found", name)
	}
	return a
}

// clientFactory dispatches on agent name: each agent gets its own fake.
type clientFactory struct {
	clients map[string]llm.Client
	err     error
}

func (f *clientFactory) newClient(_ config.Config, agent config.Agent) (llm.Client, error) {
	if f.err != nil {
		return nil, f.err
	}
	c, ok := f.clients[agent.Name]
	if !ok {
		return nil, fmt.Errorf("no fake client for agent %q", agent.Name)
	}
	return c, nil
}

func TestSubagentRunnerNoTools(t *testing.T) {
	t.Parallel()

	cfg := subagentConfig(t, map[string]string{"worker": "You work."}, nil, nil, nil)
	sub := &fakeClient{responses: []llm.Response{textResp("the answer")}}
	factory := &clientFactory{clients: map[string]llm.Client{"worker": sub}}

	runner := engine.NewSubagentRunner(engine.SubagentRunnerConfig{
		Config:    cfg,
		NewClient: factory.newClient,
	})

	var events []tools.SubagentEvent
	res, err := runner.RunSubagent(context.Background(), "worker", "do the thing", func(ev tools.SubagentEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("RunSubagent error = %v, want nil", err)
	}
	if res.Err || res.Output != "the answer" {
		t.Errorf("res = %+v, want {the answer, Err:false}", res)
	}

	// The subagent's request carries its system prompt and the user message.
	if len(sub.requests) != 1 {
		t.Fatalf("subagent API calls = %d, want 1", len(sub.requests))
	}
	msgs := sub.requests[0].Messages
	if len(msgs) != 2 {
		t.Fatalf("subagent request messages = %d, want 2", len(msgs))
	}
	if msgs[0].Role != llm.RoleSystem || msgs[0].Content != "You work." {
		t.Errorf("subagent system message = %+v, want the subagent's system prompt", msgs[0])
	}
	if msgs[1].Role != llm.RoleUser || msgs[1].Content != "do the thing" {
		t.Errorf("subagent user message = %+v, want %q", msgs[1], "do the thing")
	}

	if len(events) != 2 || events[0].Kind != tools.SubagentUsage ||
		events[1].Kind != tools.SubagentText ||
		events[1].Agent != "worker" || events[1].Depth != 0 || events[1].Text != "the answer" {
		t.Errorf("events = %+v, want a usage event then one SubagentText from worker at depth 0", events)
	}
}

func TestSubagentRunnerResolvesToolCalls(t *testing.T) {
	t.Parallel()

	cfg := subagentConfig(t, map[string]string{"worker": "You work."}, nil, nil, nil)
	// Give the worker a real tool to call.
	cfg = grantTool(t, cfg, config.ToolEntry{
		Type:        config.ToolTypeCommand,
		Name:        "echo",
		Description: "Echo stdin.",
		Command:     []string{"cat"},
	}, "worker")

	respWithBoth := llm.Response{
		ID: "resp",
		Message: llm.Message{
			Role:      llm.RoleAssistant,
			Content:   "working on it",
			ToolCalls: []llm.ToolCall{call("call_1", "echo", `{"msg":"hello"}`)},
		},
		FinishReason: llm.FinishToolCalls,
	}
	sub := &fakeClient{responses: []llm.Response{respWithBoth, textResp("final")}}
	factory := &clientFactory{clients: map[string]llm.Client{"worker": sub}}

	runner := engine.NewSubagentRunner(engine.SubagentRunnerConfig{
		Config:    cfg,
		NewClient: factory.newClient,
	})

	res, err := runner.RunSubagent(context.Background(), "worker", "go", nil)
	if err != nil {
		t.Fatalf("RunSubagent error = %v, want nil", err)
	}
	// The output is the concatenated assistant text: content alongside the
	// tool call plus the final message.
	if res.Err || res.Output != "working on it\nfinal" {
		t.Errorf("res = %+v, want the concatenated assistant text", res)
	}
	if len(sub.requests) != 2 {
		t.Fatalf("subagent API calls = %d, want 2 (tool call resolved)", len(sub.requests))
	}
}

func TestSubagentRunnerNestedDepths(t *testing.T) {
	t.Parallel()

	// The full stack: a parent engine holds a subagent tool to B, and B
	// itself holds a subagent tool to C. The top-level callback (wired via
	// WithSubagentEvents on the parent's registry) must see B's events at
	// Depth 1 and C's at Depth 2, with Agent naming the deepest producing
	// agent.
	cfg := subagentConfig(t,
		map[string]string{"parent": "Parent.", "b": "Agent B.", "c": "Agent C."},
		map[string]string{"ask_b": "b", "ask_c": "c"},
		map[string][]string{"parent": {"ask_b"}, "b": {"ask_c"}},
		nil,
	)

	clientC := &fakeClient{responses: []llm.Response{textResp("c final")}}
	clientB := &fakeClient{responses: []llm.Response{
		toolCallResp(call("call_b", "ask_c", `{"prompt":"dig deeper"}`)),
		textResp("b final"),
	}}
	clientParent := &fakeClient{responses: []llm.Response{
		toolCallResp(call("call_p", "ask_b", `{"prompt":"start"}`)),
		textResp("parent final"),
	}}

	factory := &clientFactory{clients: map[string]llm.Client{
		"parent": clientParent,
		"b":      clientB,
		"c":      clientC,
	}}
	runner := engine.NewSubagentRunner(engine.SubagentRunnerConfig{
		Config:    cfg,
		NewClient: factory.newClient,
	})

	var seen []tools.SubagentEvent
	registry, err := tools.NewRegistry(
		cfg.AgentTools(mustAgent(t, cfg, "parent")),
		tools.WithSubagentRunner(runner),
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
		t.Errorf("final = %q, want %q", final, "parent final")
	}

	depths := map[string][]int{}
	for _, ev := range seen {
		depths[ev.Agent] = append(depths[ev.Agent], ev.Depth)
	}
	if len(depths) != 2 {
		t.Fatalf("event agents = %v, want exactly b and c", depths)
	}
	for _, d := range depths["b"] {
		if d != 1 {
			t.Errorf("b event depth = %d, want 1", d)
		}
	}
	for _, d := range depths["c"] {
		if d != 2 {
			t.Errorf("c event depth = %d, want 2", d)
		}
	}

	// The prompt from the parent's tool call became B's user message, and
	// B's tool call prompt became C's.
	if got := lastUserContent(t, clientB); got != "start" {
		t.Errorf("b user message = %q, want %q", got, "start")
	}
	if got := lastUserContent(t, clientC); got != "dig deeper" {
		t.Errorf("c user message = %q, want %q", got, "dig deeper")
	}

	// Each request carries its own agent's system prompt.
	for _, tc := range []struct {
		client *fakeClient
		prompt string
	}{{clientB, "Agent B."}, {clientC, "Agent C."}} {
		msgs := tc.client.requests[0].Messages
		if msgs[0].Role != llm.RoleSystem || msgs[0].Content != tc.prompt {
			t.Errorf("system message = %+v, want %q", msgs[0], tc.prompt)
		}
	}
}

func lastUserContent(t *testing.T, fc *fakeClient) string {
	t.Helper()
	if len(fc.requests) == 0 {
		t.Fatal("no requests recorded")
	}
	msgs := fc.requests[len(fc.requests)-1].Messages
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleUser {
			return msgs[i].Content
		}
	}
	t.Fatal("no user message in the last request")
	return ""
}

func TestSubagentRunnerTooManyTurnsIsAResult(t *testing.T) {
	t.Parallel()

	// The subagent always calls a tool: with max_turns 1 it exhausts its
	// budget. That is a subagent-level failure, not a Go error.
	cfg := subagentConfig(t,
		map[string]string{"worker": "You work."},
		nil,
		nil,
		map[string]int{"worker": 1},
	)
	cfg = grantTool(t, cfg, config.ToolEntry{
		Type:        config.ToolTypeCommand,
		Name:        "echo",
		Description: "Echo stdin.",
		Command:     []string{"cat"},
	}, "worker")

	loopResp := toolCallResp(call("call_loop", "echo", "{}"))
	sub := &fakeClient{responses: []llm.Response{loopResp, loopResp}}
	factory := &clientFactory{clients: map[string]llm.Client{"worker": sub}}

	runner := engine.NewSubagentRunner(engine.SubagentRunnerConfig{
		Config:    cfg,
		NewClient: factory.newClient,
	})

	res, err := runner.RunSubagent(context.Background(), "worker", "go", nil)
	if err != nil {
		t.Fatalf("RunSubagent error = %v, want nil (exhaustion is a result)", err)
	}
	if !res.Err {
		t.Errorf("res.Err = false, want true")
	}
	if !strings.Contains(res.Output, "too many turns") {
		t.Errorf("res.Output = %q, want a too-many-turns mention", res.Output)
	}
}

func TestSubagentRunnerFailureRecordedInParentHistory(t *testing.T) {
	t.Parallel()

	// A parent delegates to a worker that exhausts its turns; the parent's
	// history must record a failed tool result so the model can react.
	cfg := subagentConfig(t,
		map[string]string{"parent": "Parent.", "worker": "Worker."},
		map[string]string{"ask_worker": "worker"},
		map[string][]string{"parent": {"ask_worker"}},
		map[string]int{"worker": 1},
	)

	loopResp := toolCallResp(call("call_loop", "echo", "{}"))
	workerClient := &fakeClient{responses: []llm.Response{loopResp, loopResp}}
	parentClient := &fakeClient{responses: []llm.Response{
		toolCallResp(call("call_p", "ask_worker", `{"prompt":"go"}`)),
		textResp("recovered"),
	}}

	factory := &clientFactory{clients: map[string]llm.Client{"parent": parentClient, "worker": workerClient}}
	runner := engine.NewSubagentRunner(engine.SubagentRunnerConfig{
		Config:    cfg,
		NewClient: factory.newClient,
	})

	registry, err := tools.NewRegistry(
		cfg.AgentTools(mustAgent(t, cfg, "parent")),
		tools.WithSubagentRunner(runner),
	)
	if err != nil {
		t.Fatalf("NewRegistry error = %v, want nil", err)
	}
	defer registry.Close()

	parentEng := engine.New(engine.EngineConfig{Client: parentClient, Tools: registry})
	final, err := parentEng.RunTurn(context.Background(), "go", func(engine.Event) error { return nil })
	if err != nil {
		t.Fatalf("RunTurn error = %v, want nil", err)
	}
	if final != "recovered" {
		t.Errorf("final = %q, want %q (the parent reacts to the failure)", final, "recovered")
	}

	// The tool result recorded in the parent's history carries the failure.
	h := parentEng.History()
	var toolMsg *llm.Message
	for i := range h {
		if h[i].Role == llm.RoleTool {
			toolMsg = &h[i]
		}
	}
	if toolMsg == nil {
		t.Fatal("no tool result in parent history")
	}
	if !strings.Contains(toolMsg.Content, "too many turns") {
		t.Errorf("tool result content = %q, want the subagent failure text", toolMsg.Content)
	}
}

func TestSubagentRunnerContextCancellation(t *testing.T) {
	t.Parallel()

	cfg := subagentConfig(t, map[string]string{"worker": "You work."}, nil, nil, nil)
	sub := &fakeClient{err: context.Canceled}
	factory := &clientFactory{clients: map[string]llm.Client{"worker": sub}}

	runner := engine.NewSubagentRunner(engine.SubagentRunnerConfig{
		Config:    cfg,
		NewClient: factory.newClient,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runner.RunSubagent(ctx, "worker", "go", nil)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestSubagentRunnerFailingClientFactory(t *testing.T) {
	t.Parallel()

	cfg := subagentConfig(t, map[string]string{"worker": "You work."}, nil, nil, nil)
	factory := &clientFactory{err: errors.New("no key")}

	runner := engine.NewSubagentRunner(engine.SubagentRunnerConfig{
		Config:    cfg,
		NewClient: factory.newClient,
	})

	_, err := runner.RunSubagent(context.Background(), "worker", "go", nil)
	if err == nil || !strings.Contains(err.Error(), "no key") {
		t.Errorf("error = %v, want the client factory error", err)
	}
}

func TestSubagentRunnerUnknownAgent(t *testing.T) {
	t.Parallel()

	cfg := subagentConfig(t, map[string]string{"worker": "You work."}, nil, nil, nil)
	factory := &clientFactory{}

	runner := engine.NewSubagentRunner(engine.SubagentRunnerConfig{
		Config:    cfg,
		NewClient: factory.newClient,
	})

	_, err := runner.RunSubagent(context.Background(), "ghost", "go", nil)
	if err == nil || !strings.Contains(err.Error(), `subagent "ghost" is not defined in the config`) {
		t.Errorf("error = %v, want an unknown-subagent error", err)
	}
}

func TestSubagentRunnerStreaming(t *testing.T) {
	t.Parallel()

	cfg := subagentConfig(t, map[string]string{"worker": "You work."}, nil, nil, nil)
	sub := &streamingClient{responses: []llm.Response{
		llm.Response{
			ID: "resp",
			Message: llm.Message{
				Role:      llm.RoleAssistant,
				Content:   "streamed answer",
				Reasoning: "hmm",
			},
			FinishReason: llm.FinishStop,
		},
	}}
	factory := &clientFactory{clients: map[string]llm.Client{"worker": sub}}

	runner := engine.NewSubagentRunner(engine.SubagentRunnerConfig{
		Config:    cfg,
		NewClient: factory.newClient,
		Stream:    true,
	})

	var events []tools.SubagentEvent
	res, err := runner.RunSubagent(context.Background(), "worker", "go", func(ev tools.SubagentEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("RunSubagent error = %v, want nil", err)
	}
	if res.Output != "streamed answer" {
		t.Errorf("res.Output = %q, want %q", res.Output, "streamed answer")
	}

	var text, thinking strings.Builder
	for _, ev := range events {
		switch ev.Kind {
		case tools.SubagentTextDelta:
			text.WriteString(ev.Text)
		case tools.SubagentThinkingDelta:
			thinking.WriteString(ev.Text)
		}
	}
	if text.String() != "streamed answer" {
		t.Errorf("text deltas = %q, want %q", text.String(), "streamed answer")
	}
	if thinking.String() != "hmm" {
		t.Errorf("thinking deltas = %q, want %q", thinking.String(), "hmm")
	}
	for _, ev := range events {
		if ev.Kind == tools.SubagentText || ev.Kind == tools.SubagentThinking {
			t.Errorf("whole-message event %v emitted during streaming", ev.Kind)
		}
	}
}

func TestSubagentRunnerUsagePropagation(t *testing.T) {
	t.Parallel()

	cfg := subagentConfig(t, map[string]string{"worker": "You work."}, nil, nil, nil)
	// Give the worker a real tool to call so the run makes two LLM calls.
	cfg = grantTool(t, cfg, config.ToolEntry{
		Type:        config.ToolTypeCommand,
		Name:        "echo",
		Description: "Echo stdin.",
		Command:     []string{"cat"},
	}, "worker")

	firstUsage := llm.Usage{PromptTokens: 11, CompletionTokens: 4, TotalTokens: 15}
	secondUsage := llm.Usage{PromptTokens: 60, CompletionTokens: 6, TotalTokens: 66}
	first := llm.Response{
		ID:           "resp",
		Message:      llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call("call_1", "echo", "{}")}},
		FinishReason: llm.FinishToolCalls,
		Usage:        firstUsage,
	}
	second := textResp("final")
	second.Usage = secondUsage
	sub := &fakeClient{responses: []llm.Response{first, second}}
	factory := &clientFactory{clients: map[string]llm.Client{"worker": sub}}

	runner := engine.NewSubagentRunner(engine.SubagentRunnerConfig{
		Config:    cfg,
		NewClient: factory.newClient,
	})

	var usageEvents []tools.SubagentEvent
	_, err := runner.RunSubagent(context.Background(), "worker", "go", func(ev tools.SubagentEvent) error {
		if ev.Kind == tools.SubagentUsage {
			usageEvents = append(usageEvents, ev)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RunSubagent error = %v, want nil", err)
	}

	if len(usageEvents) != 2 {
		t.Fatalf("usage events = %+v, want 2 in call order", usageEvents)
	}
	for i, want := range []llm.Usage{firstUsage, secondUsage} {
		if usageEvents[i].Usage != want {
			t.Errorf("usage event %d = %+v, want %+v", i, usageEvents[i].Usage, want)
		}
		if usageEvents[i].Agent != "worker" {
			t.Errorf("usage event %d Agent = %q, want %q", i, usageEvents[i].Agent, "worker")
		}
		if usageEvents[i].Depth != 0 {
			t.Errorf("usage event %d Depth = %d, want 0 from RunSubagent", i, usageEvents[i].Depth)
		}
	}
}

func TestSubagentRunnerUsageCollectedWithoutCallback(t *testing.T) {
	t.Parallel()

	cfg := subagentConfig(t, map[string]string{"worker": "You work."}, nil, nil, nil)
	first := llm.Response{
		ID:           "resp",
		Message:      llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call("call_1", "echo", "{}")}},
		FinishReason: llm.FinishToolCalls,
		Usage:        llm.Usage{PromptTokens: 11, CompletionTokens: 4, TotalTokens: 15},
	}
	second := textResp("final")
	second.Usage = llm.Usage{PromptTokens: 60, CompletionTokens: 6, TotalTokens: 66}
	sub := &fakeClient{responses: []llm.Response{first, second}}
	factory := &clientFactory{clients: map[string]llm.Client{"worker": sub}}

	runner := engine.NewSubagentRunner(engine.SubagentRunnerConfig{
		Config:    cfg,
		NewClient: factory.newClient,
	})

	// No onEvent: usage must still reach SubagentResult via collection.
	res, err := runner.RunSubagent(context.Background(), "worker", "go", nil)
	if err != nil {
		t.Fatalf("RunSubagent error = %v, want nil", err)
	}

	want := []tools.SubagentUsageRecord{
		{Agent: "worker", Model: "m", Usage: first.Usage},
		{Agent: "worker", Model: "m", Usage: second.Usage},
	}
	if len(res.Usage) != len(want) {
		t.Fatalf("res.Usage = %+v, want %+v", res.Usage, want)
	}
	for i, w := range want {
		if res.Usage[i] != w {
			t.Errorf("res.Usage[%d] = %+v, want %+v", i, res.Usage[i], w)
		}
	}
}

func TestSubagentRunnerFailureStillCarriesUsage(t *testing.T) {
	t.Parallel()

	// The subagent always calls a tool: with max_turns 1 it exhausts its
	// budget after one real LLM call, whose usage must survive.
	cfg := subagentConfig(t,
		map[string]string{"worker": "You work."},
		nil,
		nil,
		map[string]int{"worker": 1},
	)
	cfg = grantTool(t, cfg, config.ToolEntry{
		Type:        config.ToolTypeCommand,
		Name:        "echo",
		Description: "Echo stdin.",
		Command:     []string{"cat"},
	}, "worker")

	loopResp := toolCallResp(call("call_loop", "echo", "{}"))
	loopResp.Usage = llm.Usage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11}
	sub := &fakeClient{responses: []llm.Response{loopResp, loopResp}}
	factory := &clientFactory{clients: map[string]llm.Client{"worker": sub}}

	runner := engine.NewSubagentRunner(engine.SubagentRunnerConfig{
		Config:    cfg,
		NewClient: factory.newClient,
	})

	res, err := runner.RunSubagent(context.Background(), "worker", "go", nil)
	if err != nil {
		t.Fatalf("RunSubagent error = %v, want nil (exhaustion is a result)", err)
	}
	if !res.Err {
		t.Error("res.Err = false, want true")
	}
	want := []tools.SubagentUsageRecord{
		{Agent: "worker", Model: "m", Usage: loopResp.Usage},
	}
	if len(res.Usage) != 1 || res.Usage[0] != want[0] {
		t.Errorf("res.Usage = %+v, want %+v (calls that ran still spent tokens)", res.Usage, want)
	}
}
