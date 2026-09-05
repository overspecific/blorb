package ollama

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/overspecific/blorb/internal/llm"
)

// wireRequest builds the Ollama wire request from a neutral request.
// defaultModel applies when the request carries no model; reasoningEffort
// maps onto the think field (see thinkValue); format and keepAlive pass
// through verbatim from the client config (empty/nil omits the fields);
// logprobs/topLogprobs come from the client config and are only sent when
// logprobs is true.
//
// Thinking is deliberately NOT sent back to the server: a final answer's
// reasoning is stale by the next request, and resending it pollutes the
// prompt. It is decoded from responses only.
func wireRequest(req llm.Request, defaultModel, reasoningEffort string, format json.RawMessage, keepAlive string, logprobs bool, topLogprobs int) (chatRequest, error) {
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
		Model:      model,
		Messages:   msgs,
		Think:      thinkValue(reasoningEffort),
		Tools:      wireTools(req.Tools),
		Options:    wireSamplingOptions(req.Sampling),
		Format:     format,
		KeepAlive:  keepAlive,
		ToolChoice: wireToolChoice(req.ToolChoice),
		Logprobs:   logprobs,
		// top_logprobs is only meaningful with logprobs: omit it
		// otherwise so the wire never carries a dangling count.
		TopLogprobs: topLogprobsIf(logprobs, topLogprobs),
	}, nil
}

// topLogprobsIf returns n when logprobs is true, 0 (omitted on the wire)
// otherwise.
func topLogprobsIf(logprobs bool, n int) int {
	if !logprobs {
		return 0
	}
	return n
}

// wireToolChoice maps the neutral tool choice onto the wire: auto (and
// nil) omits the field entirely; none and required serialize as the bare
// string; force serializes as the OpenAI object shape
// {"type":"function","function":{"name":...}}, which Ollama accepts
// identically, so both clients build it the same way.
func wireToolChoice(tc *llm.ToolChoice) any {
	if tc == nil || tc.Mode == llm.ToolChoiceAuto {
		return nil
	}
	switch tc.Mode {
	case llm.ToolChoiceForce:
		return wireForcedToolChoice{Type: "function", Function: wireForcedToolFn{Name: tc.ForceTool}}
	default:
		return string(tc.Mode)
	}
}

// wireForcedToolChoice is the OpenAI forced-tool object shape, which
// Ollama accepts identically, so both clients build it the same way.
type wireForcedToolChoice struct {
	Type     string           `json:"type"`
	Function wireForcedToolFn `json:"function"`
}

type wireForcedToolFn struct {
	Name string `json:"name"`
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
