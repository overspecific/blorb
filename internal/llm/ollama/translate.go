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
		Options:  wireSamplingOptions(req.Sampling),
	}, nil
}

// wireSamplingOptions maps the neutral sampling parameters onto Ollama's
// nested options object, with MaxTokens as num_predict. A request with no
// sampling overrides at all returns nil, leaving options off the wire.
func wireSamplingOptions(s llm.SamplingParams) *wireOptions {
	if !s.Any() {
		return nil
	}
	return &wireOptions{
		Temperature:      s.Temperature,
		TopP:             s.TopP,
		Seed:             s.Seed,
		Stop:             s.Stop,
		NumPredict:       s.MaxTokens,
		FrequencyPenalty: s.FrequencyPenalty,
		PresencePenalty:  s.PresencePenalty,
	}
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
// ids ("call_" + randomHex). crypto/rand.Read is documented to never fail
// on Go 1.24+, so a rand error is impossible here; panicking beats a
// placeholder, whose identical ids would silently corrupt the tool-call
// history matching the ids exist for.
func randomHex() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("ollama: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
