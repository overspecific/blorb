package ollama

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/overspecific/blorb/internal/llm"
)

// wireRequest builds the Ollama wire request from a neutral request.
// defaultModel applies when the request carries no model; reasoningEffort
// maps onto the think field (see thinkValue).
//
// Thinking is deliberately NOT sent back to the server: a final answer's
// reasoning is stale by the next request, and resending it pollutes the
// prompt. It is decoded from responses only.
func wireRequest(req llm.Request, defaultModel, reasoningEffort string) (chatRequest, error) {
	model := req.Model
	if model == "" {
		model = defaultModel
	}

	msgs := make([]wireMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		calls, err := wireToolCalls(m.ToolCalls)
		if err != nil {
			return chatRequest{}, fmt.Errorf("message %q: %w", m.Role, err)
		}
		msgs = append(msgs, wireMessage{
			Role:       string(m.Role),
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			ToolCalls:  calls,
		})
	}

	return chatRequest{
		Model:    model,
		Messages: msgs,
		Think:    thinkValue(reasoningEffort),
		Tools:    wireTools(req.Tools),
	}, nil
}

// wireTools converts neutral tool definitions into Ollama's
// OpenAI-style function envelope.
func wireTools(tools []llm.Tool) []wireTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]wireTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, wireTool{
			Type: llm.ToolCallType,
			Function: wireFnResponse{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	return out
}

// randomHex returns 16 random hex characters, for synthesized tool call
// ids ("call_" + randomHex). An error falls back to a fixed placeholder:
// the engine needs a non-empty id more than it needs entropy.
func randomHex() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}
