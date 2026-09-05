// Package engine owns a conversation and drives agent turns against an
// llm.Client implementation and a tools.Registry.
package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	// EventUsage carries the token usage of one completed LLM call,
	// emitted once per call after its response is fully received
	// (whole-message or streamed). Usage is the provider-reported
	// counts; a provider that reports none yields a zero Usage.
	EventUsage
)

// Event is emitted as a turn progresses. Text is set for
// EventAssistantText and EventAssistantThinking; Name and Args for
// EventToolCall; Name, Output and Failed for EventToolResult. The delta
// kinds carry fragments: EventAssistantTextDelta and
// EventAssistantThinkingDelta set Text, and EventToolCallDelta sets Name,
// Args, and Index (the 0-based index of the tool call the fragment belongs
// to, from llm.ToolCallDelta.Index). Usage, AgentName, and Model are set
// only for EventUsage; they are zero/empty for all other kinds.
type Event struct {
	Kind   EventKind
	Text   string
	Name   string
	Args   string
	Index  int
	Output string
	Failed bool
	// Usage is the provider-reported token usage of the completed call.
	Usage llm.Usage
	// Stats is the stats of the completed call; zero for all kinds
	// except EventUsage.
	Stats llm.CallStats
	// Logprobs carries the response's per-token log probabilities on
	// EventAssistantText, when the client config asked for them and the
	// path was non-streaming; nil otherwise (streamed responses do not
	// decode logprobs).
	Logprobs []llm.Logprob
	// AgentName attributes the call to the agent whose engine made it.
	AgentName string
	// Model is the agent's configured provider model.
	Model string
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
	// AgentName is the display name of the agent this engine runs (the
	// session agent or a subagent), used to attribute EventUsage. It is
	// optional: empty means the caller does not care and the events
	// simply carry an empty name.
	AgentName string
	// Model is the agent's configured provider model, reported on
	// EventUsage. Optional; empty when unknown.
	Model string
	// Sampling carries the generation parameters copied into every
	// llm.Request the engine builds — on the initial call and on every
	// follow-up tool round. Zero value (no fields set) means the server
	// defaults apply everywhere.
	Sampling llm.SamplingParams
	// ToolChoice, when non-nil, is copied into every llm.Request the
	// engine builds. In force mode the engine also polices the result:
	// the forced tool must be among the registry's tools at construction
	// (error otherwise), and a response that calls no tools is a
	// repairable deviation. For the other modes the field is sent and
	// the result taken as it comes: required asks the server to force
	// some call, and the engine does not police which (or whether) the
	// model complied.
	ToolChoice *llm.ToolChoice
}

// Engine owns conversation state and drives turns.
type Engine struct {
	cfg             EngineConfig
	history         []llm.Message
	maxTurns        int
	currentCalls    int
	constructionErr error
}

// New constructs an Engine. A force-mode ToolChoice naming a tool the
// registry does not carry is recorded here and fails the first turn —
// construction cannot error, so the misconfiguration surfaces as a clear
// turn failure naming the tool.
func New(cfg EngineConfig) *Engine {
	maxTurns := cfg.MaxTurns
	if maxTurns == 0 {
		maxTurns = config.DefaultMaxTurns
	}
	return &Engine{cfg: cfg, maxTurns: maxTurns, constructionErr: validateForcedTool(cfg)}
}

// validateForcedTool checks force mode at construction: the forced tool
// must be among the registry's tools. The other modes need nothing.
func validateForcedTool(cfg EngineConfig) error {
	if cfg.ToolChoice == nil || cfg.ToolChoice.Mode != llm.ToolChoiceForce {
		return nil
	}
	name := cfg.ToolChoice.ForceTool
	if cfg.Tools != nil && slices.ContainsFunc(cfg.Tools.Definitions(), func(t llm.Tool) bool { return t.Name == name }) {
		return nil
	}
	return fmt.Errorf("forced tool %q is not among the agent's granted tools", name)
}

// RunTurn appends the user message and loops API calls and tool executions
// until the model produces a final text. Events are emitted via onEvent as
// the turn progresses; an onEvent error aborts the turn with that error.
// The returned text is all assistant text produced during the turn:
// content emitted alongside tool calls plus the final message, in order.
func (e *Engine) RunTurn(ctx context.Context, userMessage string, onEvent func(Event) error) (final string, err error) {
	if e.constructionErr != nil {
		return "", e.constructionErr
	}

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
	// forcedCalled tracks whether the forced tool has been called this
	// turn: force mode polices the model until the forced tool runs, and
	// the follow-up calls that execute it must be free to produce the
	// final answer.
	forcedCalled := false
	for {
		resp, streamed, err := e.call(ctx, onEvent)
		if err != nil {
			return "", err
		}

		if e.cfg.ToolChoice != nil && e.cfg.ToolChoice.Mode == llm.ToolChoiceForce && !forcedCalled {
			for _, tc := range resp.Message.ToolCalls {
				if tc.FunctionName == e.cfg.ToolChoice.ForceTool {
					forcedCalled = true
					break
				}
			}
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
				if err := onEvent(Event{Kind: EventAssistantText, Text: resp.Message.Content, Logprobs: resp.Logprobs}); err != nil {
					return "", err
				}
			}
		}

		if len(resp.Message.ToolCalls) == 0 {
			// In force mode, a response with no tool calls before the
			// forced tool ran is a deviation: the model was told to call
			// the forced tool and replied with text instead. The engine
			// has no protocol-valid repair for this — there is no tool
			// call to answer with a corrective tool result — so the turn
			// fails naming the forced tool and what came back instead.
			if e.cfg.ToolChoice != nil && e.cfg.ToolChoice.Mode == llm.ToolChoiceForce && !forcedCalled {
				e.history = append(e.history, resp.Message)
				return "", fmt.Errorf("forced tool %q was not called: the model replied with text instead: %s",
					e.cfg.ToolChoice.ForceTool, resp.Message.Content)
			}
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

	req := llm.Request{Messages: messages, Sampling: e.cfg.Sampling, ToolChoice: e.cfg.ToolChoice}
	if e.cfg.Tools != nil {
		req.Tools = e.cfg.Tools.Definitions()
	}

	if e.cfg.Stream {
		if sc, ok := e.cfg.Client.(llm.StreamingClient); ok {
			resp, err := e.streamCall(ctx, sc, req, onEvent)
			if err != nil {
				// streamed is meaningless on failure (the caller
				// discards resp), but false keeps the flag honest:
				// no complete streamed response was produced.
				return nil, false, err
			}
			// EventUsage is emitted once per completed LLM call; for
			// streamed calls it arrives after the call's deltas, since
			// the provider reports usage in the stream's final chunk.
			if err := e.emitUsage(resp, onEvent); err != nil {
				return nil, true, err
			}
			return resp, true, nil
		}
	}
	resp, err = e.cfg.Client.Chat(ctx, req)
	if err != nil {
		return nil, false, err
	}
	// EventUsage is emitted once per completed LLM call; in the
	// whole-message path it precedes the response's thinking/text events.
	if err := e.emitUsage(resp, onEvent); err != nil {
		return nil, false, err
	}
	return resp, false, nil
}

// emitUsage reports one completed LLM call's usage via EventUsage.
func (e *Engine) emitUsage(resp *llm.Response, onEvent func(Event) error) error {
	var usage llm.Usage
	var stats llm.CallStats
	if resp != nil {
		usage = resp.Usage
		stats = resp.Stats
	}
	return onEvent(Event{
		Kind:      EventUsage,
		Usage:     usage,
		Stats:     stats,
		AgentName: e.cfg.AgentName,
		Model:     e.cfg.Model,
	})
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
