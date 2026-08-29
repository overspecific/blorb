// Package llm defines the provider-neutral vocabulary shared by the engine,
// tools, chat, and all client implementations. Types here are wire-agnostic;
// provider packages convert to and from their own wire formats. Nothing in
// this package talks to the network.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
)

// Role identifies who produced a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCallType is the only tool call type in common use.
const ToolCallType = "function"

// Message is one entry in a conversation history. Its JSON tags exist for
// internal and test serialization only; they are deliberately NOT the
// OpenAI wire shape. Wire conversion lives in the provider packages (see
// internal/llm/openai).
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
	// Reasoning holds chain-of-thought the server extracted into its own
	// field (e.g. reasoning_content). It is preserved in history and
	// re-sent with assistant messages so reasoning models keep their
	// context across tool call rounds. Not shown as final answer text.
	Reasoning  string     `json:"reasoning,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is a request from the model to invoke a tool.
type ToolCall struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	// FunctionName is the model-chosen tool name. The JSON tags on this
	// struct are the neutral/test shape, not the OpenAI wire shape, which
	// nests these fields under "function" (see internal/llm/openai).
	FunctionName string `json:"name"`
	// FunctionArgs is the JSON-encoded arguments string. Use DecodedArgs
	// instead of manipulating it directly.
	FunctionArgs string `json:"arguments"`
}

// DecodedArgs returns the tool call arguments as raw JSON, unquoting the
// wire-encoded string form.
func (tc ToolCall) DecodedArgs() (json.RawMessage, error) {
	if tc.FunctionArgs == "" {
		return json.RawMessage("{}"), nil
	}
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(tc.FunctionArgs), &raw); err != nil {
		return nil, fmt.Errorf("decode tool call args for %q: %w", tc.FunctionName, err)
	}
	return raw, nil
}

// Tool describes a function the model may call.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Parameters is a JSON schema object describing the arguments.
	Parameters json.RawMessage `json:"parameters"`
}

// Request is a provider-neutral chat completion request.
type Request struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []Tool    `json:"tools,omitempty"`
}

// Response is a provider-neutral chat completion response.
type Response struct {
	ID           string  `json:"id"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
	Usage        Usage   `json:"usage"`
}

// Usage holds token counts reported by the provider.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Finish reason values in common use across OpenAI-compatible servers. Values
// not listed here are passed through unchanged so exotic servers do not break
// parsing.
const (
	FinishStop      = "stop"
	FinishToolCalls = "tool_calls"
	FinishLength    = "length"
)

// Client is the seam every LLM provider implements. The engine consumes only
// this interface; adding a provider means adding a package, not touching the
// engine.
//
// Contract: implementations send the full message history on every call,
// honour context cancellation, and return an error when a response is
// malformed (including assistant messages with tool calls missing ids or
// function names).
type Client interface {
	Chat(ctx context.Context, req Request) (*Response, error)
}

// Delta is one incremental piece of a streaming response. Content is a
// fragment of assistant text; Reasoning is a fragment of extracted
// chain-of-thought; ToolCall is non-nil when the delta carries a fragment of
// a tool call. A single Delta may carry one or more of these at once.
//
// Deltas arrive in order. The concatenation of all Content fragments equals
// the final message's content, and the concatenation of all Reasoning
// fragments equals the final message's reasoning; the tool-call fragments
// (keyed by ToolCallDelta.Index) assemble into the final message's tool
// calls.
type Delta struct {
	Content   string
	Reasoning string
	ToolCall  *ToolCallDelta
}

// ToolCallDelta is a fragment of a tool call within a streaming response.
// Index identifies which tool call in the message the fragment belongs to
// (0-based, matching the OpenAI tool_calls[].index). ID, Type, and Name are
// typically complete in the first fragment for a given index, while
// Arguments arrives as one or more fragments to be concatenated in order.
type ToolCallDelta struct {
	Index     int
	ID        string
	Type      string
	Name      string
	Arguments string
}

// StreamingClient is the seam for clients that can stream responses
// incrementally in addition to the whole-message Chat path.
//
// Contract: ChatStream sends the full message history, honours context
// cancellation, and calls onDelta for each incremental piece of content,
// reasoning, or tool call as it arrives (in order; see Delta). It returns
// the complete Response, including any tool calls, once the stream finishes.
// An onDelta error aborts the stream and is returned. Clients that do not
// implement this interface are used via Client.Chat instead.
type StreamingClient interface {
	Client
	ChatStream(ctx context.Context, req Request, onDelta func(Delta) error) (*Response, error)
}

// NewTextMessage builds a message with a string content body.
func NewTextMessage(role Role, content string) Message {
	return Message{Role: role, Content: content}
}

// NewToolResultMessage builds a tool-result message. When failed is true the
// output is prefixed so the model understands the tool errored.
func NewToolResultMessage(toolCallID, output string, failed bool) Message {
	content := output
	if failed {
		content = "Tool call failed: " + output
	}
	return Message{Role: RoleTool, Content: content, ToolCallID: toolCallID}
}
