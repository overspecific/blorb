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

// Message is one entry in a conversation history.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is a request from the model to invoke a tool.
type ToolCall struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	FunctionName string `json:"name"`
	// FunctionArgs is the JSON-encoded arguments string, as it appears on the
	// wire. Use DecodedArgs instead of manipulating it directly.
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
// malformed (including assistant messages with tool calls missing ids).
type Client interface {
	Chat(ctx context.Context, req Request) (*Response, error)
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
