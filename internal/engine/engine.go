// Package engine owns a conversation and drives agent turns against an
// llm.Client implementation and a tools.Registry.
package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/tools"
)

// ErrTooManyTurns is returned when MaxTurns is exhausted without a final
// assistant text. The wrapped message, when present, is the last assistant
// message so callers can present partial output.
var ErrTooManyTurns = errors.New("too many turns")

// EventKind identifies what happened during a turn.
type EventKind int

const (
	// EventAssistantText carries assistant text content.
	EventAssistantText EventKind = iota
	// EventToolCall precedes a tool execution and carries name and args.
	EventToolCall
	// EventToolResult carries the outcome of a tool execution.
	EventToolResult
	// EventAssistantThinking carries extracted chain-of-thought
	// (reasoning_content) from an assistant message. It precedes the
	// matching EventAssistantText, and a single message may contain both
	// thinking and content.
	EventAssistantThinking
)

// Event is emitted as a turn progresses. Text is set for
// EventAssistantText and EventAssistantThinking; Name and Args for
// EventToolCall; Name, Output and Failed for EventToolResult.
type Event struct {
	Kind   EventKind
	Text   string
	Name   string
	Args   string
	Output string
	Failed bool
}

// EngineConfig configures an Engine.
type EngineConfig struct {
	// Client is the provider-neutral LLM client. The engine deliberately
	// depends only on llm.Client so new providers are additive packages.
	Client llm.Client
	Tools  *tools.Registry
	// SystemPrompt seeds the conversation as the first message.
	SystemPrompt string
	// MaxTurns bounds API calls per RunTurn; zero means
	// config.DefaultMaxTurns.
	MaxTurns int
}

// Engine owns conversation state and drives turns.
type Engine struct {
	cfg          EngineConfig
	history      []llm.Message
	maxTurns     int
	currentCalls int
}

// New constructs an Engine.
func New(cfg EngineConfig) *Engine {
	maxTurns := cfg.MaxTurns
	if maxTurns == 0 {
		maxTurns = config.DefaultMaxTurns
	}
	return &Engine{cfg: cfg, maxTurns: maxTurns}
}

// RunTurn appends the user message and loops API calls and tool executions
// until the model produces a final text. Events are emitted via onEvent as
// the turn progresses; an onEvent error aborts the turn with that error.
func (e *Engine) RunTurn(ctx context.Context, userMessage string, onEvent func(Event) error) (final string, err error) {
	e.currentCalls = 0
	historyLen := len(e.history)
	e.history = append(e.history, llm.NewTextMessage(llm.RoleUser, userMessage))

	// An aborted turn can leave the user message unanswered, or assistant
	// tool calls without matching tool results (e.g. interruption mid-run).
	// Repair history so the next request stays protocol-valid.
	defer func() {
		if err != nil {
			e.history = e.history[:historyLen]
			e.repairUnansweredToolCalls()
		}
	}()

	if onEvent == nil {
		onEvent = func(Event) error { return nil }
	}

	for {
		resp, err := e.call(ctx)
		if err != nil {
			return "", err
		}

		if resp.Message.Reasoning != "" {
			if err := onEvent(Event{Kind: EventAssistantThinking, Text: resp.Message.Reasoning}); err != nil {
				return "", err
			}
		}

		if resp.Message.Content != "" {
			if err := onEvent(Event{Kind: EventAssistantText, Text: resp.Message.Content}); err != nil {
				return "", err
			}
		}

		if len(resp.Message.ToolCalls) == 0 {
			e.history = append(e.history, resp.Message)
			return resp.Message.Content, nil
		}

		e.history = append(e.history, resp.Message)
		if err := e.runToolCalls(ctx, resp.Message.ToolCalls, onEvent); err != nil {
			return "", err
		}
	}
}

// History returns a defensive copy of the conversation history.
func (e *Engine) History() []llm.Message {
	out := make([]llm.Message, len(e.history))
	copy(out, e.history)
	return out
}

// SeedHistoryForTest replaces the conversation history. Tests only.
func (e *Engine) SeedHistoryForTest(msgs []llm.Message) {
	e.history = append([]llm.Message(nil), msgs...)
}

// RepairUnansweredToolCallsForTest exposes the history repair pass. Tests only.
func (e *Engine) RepairUnansweredToolCallsForTest() {
	e.repairUnansweredToolCalls()
}

// call performs one API call, seeding the system prompt and enforcing
// MaxTurns.
func (e *Engine) call(ctx context.Context) (*llm.Response, error) {
	if e.currentCalls >= e.maxTurns {
		msg := e.lastAssistantMessage()
		if msg != nil {
			return nil, fmt.Errorf("%w: last assistant message: %s", ErrTooManyTurns, msg.Content)
		}
		return nil, fmt.Errorf("%w: made %d API calls without a final text", ErrTooManyTurns, e.maxTurns)
	}
	e.currentCalls++

	messages := make([]llm.Message, 0, len(e.history)+1)
	if e.cfg.SystemPrompt != "" {
		messages = append(messages, llm.NewTextMessage(llm.RoleSystem, e.cfg.SystemPrompt))
	}
	messages = append(messages, e.history...)

	req := llm.Request{Messages: messages}
	if e.cfg.Tools != nil {
		req.Tools = e.cfg.Tools.Definitions()
	}
	return e.cfg.Client.Chat(ctx, req)
}

// runToolCalls executes each tool call, emits events, and appends the
// tool-result messages to history. An infrastructure failure from the
// registry still produces an error tool message so the conversation stays
// protocol-valid.
func (e *Engine) runToolCalls(ctx context.Context, calls []llm.ToolCall, onEvent func(Event) error) error {
	for _, tc := range calls {
		args, err := tc.DecodedArgs()
		if err != nil {
			// Malformed arguments are a model failure, not an
			// infrastructure one: feed the error back as a failed tool
			// result so the model can recover, and keep history
			// protocol-valid.
			e.history = append(e.history,
				llm.NewToolResultMessage(tc.ID, err.Error(), true))
			if err := onEvent(Event{Kind: EventToolResult, Name: tc.FunctionName, Output: err.Error(), Failed: true}); err != nil {
				return err
			}
			continue
		}

		if err := onEvent(Event{Kind: EventToolCall, Name: tc.FunctionName, Args: tc.FunctionArgs}); err != nil {
			return err
		}

		var (
			res    tools.ToolResult
			runErr error
		)
		if e.cfg.Tools == nil {
			runErr = fmt.Errorf("no tools are configured")
		} else {
			res, runErr = e.cfg.Tools.Run(ctx, tc.FunctionName, args)
		}

		if runErr != nil {
			e.history = append(e.history,
				llm.NewToolResultMessage(tc.ID, runErr.Error(), true))
			if err := onEvent(Event{Kind: EventToolResult, Name: tc.FunctionName, Output: runErr.Error(), Failed: true}); err != nil {
				return err
			}
			continue
		}

		e.history = append(e.history,
			llm.NewToolResultMessage(tc.ID, res.Output, res.Err))
		if err := onEvent(Event{Kind: EventToolResult, Name: tc.FunctionName, Output: res.Output, Failed: res.Err}); err != nil {
			return err
		}
	}
	return nil
}

// repairUnansweredToolCalls appends synthetic error tool results for any
// tool call that has no matching tool-result message in history, i.e. one
// orphaned by a turn abort (interruption, API failure mid-tool-loop).
func (e *Engine) repairUnansweredToolCalls() {
	answered := make(map[string]bool)
	for _, m := range e.history {
		if m.Role == llm.RoleTool {
			answered[m.ToolCallID] = true
		}
	}
	for i := len(e.history) - 1; i >= 0; i-- {
		m := e.history[i]
		if m.Role != llm.RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.ID != "" && !answered[tc.ID] {
				e.history = append(e.history,
					llm.NewToolResultMessage(tc.ID, "tool call was interrupted before it ran", true))
			}
		}
	}
}

func (e *Engine) lastAssistantMessage() *llm.Message {
	for i := len(e.history) - 1; i >= 0; i-- {
		if e.history[i].Role == llm.RoleAssistant {
			msg := e.history[i]
			return &msg
		}
	}
	return nil
}
