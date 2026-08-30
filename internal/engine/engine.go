// Package engine owns a conversation and drives agent turns against an
// llm.Client implementation and a tools.Registry.
package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
	// EventAssistantTextDelta carries an incremental fragment of
	// assistant text, emitted only when streaming. The concatenation of
	// all EventAssistantTextDelta events in a round equals the round's
	// assistant text.
	EventAssistantTextDelta
	// EventAssistantThinkingDelta carries an incremental fragment of
	// extracted chain-of-thought, emitted only when streaming. The
	// concatenation of all EventAssistantThinkingDelta events in a round
	// equals the round's reasoning, and they interleave with text deltas
	// in arrival order.
	EventAssistantThinkingDelta
	// EventToolCallDelta carries a fragment of a streamed tool call:
	// Name when the fragment includes it, and Args as an arguments
	// fragment. Emitted only when streaming; fragments assemble into the
	// round's tool calls.
	EventToolCallDelta
)

// Event is emitted as a turn progresses. Text is set for
// EventAssistantText and EventAssistantThinking; Name and Args for
// EventToolCall; Name, Output and Failed for EventToolResult. The delta
// kinds carry fragments: EventAssistantTextDelta and
// EventAssistantThinkingDelta set Text, and EventToolCallDelta sets Name,
// Args, and Index (the 0-based index of the tool call the fragment belongs
// to, from llm.ToolCallDelta.Index).
type Event struct {
	Kind   EventKind
	Text   string
	Name   string
	Args   string
	Index  int
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
	// Stream enables incremental streaming: when true and the client
	// implements llm.StreamingClient, assistant text, reasoning, and
	// tool calls are emitted incrementally as delta events; otherwise
	// the whole-message path is used.
	Stream bool
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
// The returned text is all assistant text produced during the turn:
// content emitted alongside tool calls plus the final message, in order.
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

	var text strings.Builder
	for {
		resp, streamed, err := e.call(ctx, onEvent)
		if err != nil {
			return "", err
		}

		// When the response was streamed the fragments were already
		// emitted as they arrived; only the whole-message path emits
		// them here.
		if !streamed {
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
		}

		if len(resp.Message.ToolCalls) == 0 {
			// A length-truncated response is an incomplete answer, not a
			// final one; surface it as a failed turn so the user knows the
			// text was cut off, after emitting what did arrive.
			if resp.FinishReason == llm.FinishLength {
				e.history = append(e.history, resp.Message)
				return "", fmt.Errorf("response was truncated by the provider (finish_reason %s)", llm.FinishLength)
			}
			e.history = append(e.history, resp.Message)
			return text.String() + resp.Message.Content, nil
		}

		text.WriteString(resp.Message.Content)
		if text.Len() > 0 && resp.Message.Content != "" {
			text.WriteString("\n")
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
// MaxTurns. When streaming is enabled and the client implements
// llm.StreamingClient, the streaming path is used and each delta is emitted
// via onEvent as it arrives; the returned streamed flag reports which path
// was taken so callers can avoid double-emitting whole-message events.
func (e *Engine) call(ctx context.Context, onEvent func(Event) error) (resp *llm.Response, streamed bool, err error) {
	if e.currentCalls >= e.maxTurns {
		msg := e.lastAssistantMessage()
		if msg != nil {
			return nil, false, fmt.Errorf("%w: last assistant message: %s", ErrTooManyTurns, msg.Content)
		}
		return nil, false, fmt.Errorf("%w: made %d API calls without a final text", ErrTooManyTurns, e.maxTurns)
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

	if e.cfg.Stream {
		if sc, ok := e.cfg.Client.(llm.StreamingClient); ok {
			resp, err := e.streamCall(ctx, sc, req, onEvent)
			return resp, err == nil && resp != nil, err
		}
	}
	resp, err = e.cfg.Client.Chat(ctx, req)
	return resp, false, err
}

// streamCall performs one streaming API call, emitting each fragment as an
// event in the order it arrives.
func (e *Engine) streamCall(ctx context.Context, sc llm.StreamingClient, req llm.Request, onEvent func(Event) error) (*llm.Response, error) {
	return sc.ChatStream(ctx, req, func(d llm.Delta) error {
		if d.Reasoning != "" {
			if err := onEvent(Event{Kind: EventAssistantThinkingDelta, Text: d.Reasoning}); err != nil {
				return err
			}
		}
		if d.Content != "" {
			if err := onEvent(Event{Kind: EventAssistantTextDelta, Text: d.Content}); err != nil {
				return err
			}
		}
		if d.ToolCall != nil {
			if err := onEvent(Event{Kind: EventToolCallDelta, Name: d.ToolCall.Name, Args: d.ToolCall.Arguments, Index: d.ToolCall.Index}); err != nil {
				return err
			}
		}
		return nil
	})
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
