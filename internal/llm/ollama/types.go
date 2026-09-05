// Package ollama implements llm.Client for Ollama's native /api/chat API
// over plain net/http + JSON, non-streaming and streaming (NDJSON).
package ollama

import (
	"encoding/json"

	"github.com/overspecific/blorb/internal/llm"
)

// chatRequest is the Ollama /api/chat request envelope. It carries none of
// Ollama's options/format/tool_choice knobs: the neutral llm.Request has no
// such fields.
//
// Think is `any` rather than bool: it carries either false (disabling
// thinking; Ollama rejects the string "none") or a level string passed
// through from the model's reasoning_effort. Because an interface is only
// empty when nil, omitempty still serializes a held false, so
// "think": false reaches the wire.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
	Stream   bool          `json:"stream,omitempty"`
	Think    any           `json:"think,omitempty"`
	Tools    []wireTool    `json:"tools,omitempty"`
}

// wireMessage is the Ollama message shape. Content has no omitempty: empty
// content is sent as "", which Ollama accepts. Thinking is decode-only —
// reasoning is deliberately never re-sent to the server, matching the
// openai client's rule that a final answer's reasoning is stale by the
// next request (see translate.wireRequest).
type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	Thinking   string         `json:"thinking,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
}

// wireToolCall is the Ollama tool call shape. Function.Arguments is a
// json.RawMessage rather than a string: Ollama sends the arguments as an
// embedded JSON object, not a string-encoded one.
type wireToolCall struct {
	ID       string     `json:"id,omitempty"`
	Function wireFnCall `json:"function"`
}

type wireFnCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// wireTool is the Ollama tool definition shape, which wraps the function
// fields in the OpenAI-style nested envelope tagged with type "function".
type wireTool struct {
	Type     string         `json:"type"`
	Function wireFnResponse `json:"function"`
}

type wireFnResponse struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// chatResponse is one Ollama /api/chat response object — the whole body
// when non-streaming, one NDJSON line when streaming. The final line (the
// only one, when non-streaming) carries done: true, done_reason, and the
// token counts.
type chatResponse struct {
	Model      string      `json:"model"`
	CreatedAt  string      `json:"created_at"`
	Message    wireMessage `json:"message"`
	Done       bool        `json:"done"`
	DoneReason string      `json:"done_reason"`
	// PromptEvalCount and EvalCount are the prompt and completion token
	// counts, respectively.
	PromptEvalCount int `json:"prompt_eval_count"`
	EvalCount       int `json:"eval_count"`
}

// thinkValue maps the model's reasoning effort onto Ollama's think field.
// Empty omits it (server default); "none" becomes the boolean false
// because Ollama rejects the string "none"; every other value passes
// through verbatim as the level string — which levels a given model
// accepts is the server's business.
func thinkValue(reasoningEffort string) any {
	switch reasoningEffort {
	case "":
		return nil
	case "none":
		return false
	default:
		return reasoningEffort
	}
}

// wireToolCalls converts a neutral tool call into the wire shape, parsing
// the string-encoded FunctionArgs into the embedded object form Ollama
// expects. Empty arguments become {}; invalid arguments error out.
func wireToolCalls(calls []llm.ToolCall) ([]wireToolCall, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	out := make([]wireToolCall, 0, len(calls))
	for _, tc := range calls {
		args := json.RawMessage(tc.FunctionArgs)
		if len(args) == 0 {
			args = json.RawMessage("{}")
		} else if !json.Valid(args) {
			return nil, &invalidArgsError{call: tc}
		}
		out = append(out, wireToolCall{
			ID: tc.ID,
			Function: wireFnCall{
				Name:      tc.FunctionName,
				Arguments: args,
			},
		})
	}
	return out, nil
}

type invalidArgsError struct {
	call llm.ToolCall
}

func (e *invalidArgsError) Error() string {
	return "tool call " + e.call.FunctionName + ": arguments are not valid JSON"
}

// neutralResponse converts a (possibly final) Ollama response chunk into
// the provider-neutral response.
func neutralResponse(chunk *chatResponse) *llm.Response {
	msg := llm.Message{
		Role:    llm.RoleAssistant,
		Content: chunk.Message.Content,
		// Ollama puts the model's thinking in its own field; it maps to
		// the neutral Reasoning.
		Reasoning: chunk.Message.Thinking,
	}
	for _, wtc := range chunk.Message.ToolCalls {
		callType := llm.ToolCallType
		args := string(wtc.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		id := wtc.ID
		if id == "" {
			// Ollama does not send tool call ids, and the engine needs
			// non-empty ids for history matching and repair.
			id = "call_" + randomHex()
		}
		msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{
			ID:           id,
			Type:         callType,
			FunctionName: wtc.Function.Name,
			FunctionArgs: args,
		})
	}
	resp := &llm.Response{
		Message:      msg,
		FinishReason: mapFinishReason(chunk.DoneReason),
		Usage: llm.Usage{
			PromptTokens:     chunk.PromptEvalCount,
			CompletionTokens: chunk.EvalCount,
			TotalTokens:      chunk.PromptEvalCount + chunk.EvalCount,
		},
	}
	return resp
}

// mapFinishReason translates Ollama's done_reason into the neutral finish
// reasons; unknown values map to stop, the common case.
func mapFinishReason(doneReason string) string {
	switch doneReason {
	case "length":
		return llm.FinishLength
	case "tool_calls":
		return llm.FinishToolCalls
	default:
		return llm.FinishStop
	}
}
