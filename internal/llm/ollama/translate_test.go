package ollama

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/overspecific/blorb/internal/llm"
)

// decodeRequest marshals a chatRequest and unmarshals it into a generic
// map, so tests assert on the exact wire JSON keys rather than the Go
// shape (omitempty behaviour in particular).
func decodeRequest(t *testing.T, req chatRequest) map[string]any {
	t.Helper()
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal chatRequest: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal chatRequest: %v", err)
	}
	return out
}

// TestWireRequestSamplingOptions pins the sampling mapping onto Ollama's
// nested options object: every parameter set produces the exact options
// JSON (including explicit zeros reaching the wire), and a request with no
// sampling overrides at all omits options entirely.
func TestWireRequestSamplingOptions(t *testing.T) {
	t.Parallel()

	temp := 0.0
	topP := 0.9
	seed := int64(42)
	maxTokens := 512
	freq := -1.5
	presence := 1.0

	t.Run("all params set produces the exact options object", func(t *testing.T) {
		t.Parallel()

		req := llm.Request{
			Messages: []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")},
			Sampling: llm.SamplingParams{
				Temperature:      &temp,
				TopP:             &topP,
				Seed:             &seed,
				Stop:             []string{"END"},
				MaxTokens:        &maxTokens,
				FrequencyPenalty: &freq,
				PresencePenalty:  &presence,
			},
		}
		wire, err := wireRequest(req, "fallback-model", "", nil, "", false, 0)
		if err != nil {
			t.Fatalf("wireRequest error = %v, want nil", err)
		}
		got := decodeRequest(t, wire)

		opts, ok := got["options"].(map[string]any)
		if !ok {
			t.Fatalf("wire request options = %#v, want the options object", got["options"])
		}
		want := map[string]any{
			"temperature":       0.0, // explicit zero must reach the wire
			"top_p":             0.9,
			"seed":              42.0,
			"stop":              []any{"END"},
			"num_predict":       512.0,
			"frequency_penalty": -1.5,
			"presence_penalty":  1.0,
		}
		optsJSON, err := json.Marshal(opts)
		if err != nil {
			t.Fatalf("marshal options: %v", err)
		}
		wantJSON, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("marshal want: %v", err)
		}
		if string(optsJSON) != string(wantJSON) {
			t.Errorf("options = %s, want exactly %s", optsJSON, wantJSON)
		}
	})

	t.Run("absent params omit options entirely", func(t *testing.T) {
		t.Parallel()

		req := llm.Request{
			Messages: []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")},
		}
		wire, err := wireRequest(req, "fallback-model", "", nil, "", false, 0)
		if err != nil {
			t.Fatalf("wireRequest error = %v, want nil", err)
		}
		got := decodeRequest(t, wire)
		if _, present := got["options"]; present {
			t.Errorf("wire request carries options = %#v, want the field omitted", got["options"])
		}
	})
}

func TestWireRequestMessages(t *testing.T) {
	t.Parallel()

	req := llm.Request{
		Model: "llama3.1:latest",
		Messages: []llm.Message{
			llm.NewTextMessage(llm.RoleSystem, "You are helpful."),
			llm.NewTextMessage(llm.RoleUser, "list the files"),
			{
				Role:       llm.RoleTool,
				Content:    "a.txt",
				ToolCallID: "call_9",
			},
			{
				Role: llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{
					{ID: "call_9", Type: "function", FunctionName: "ls", FunctionArgs: `{"path":"."}`},
				},
			},
		},
		Tools: []llm.Tool{
			{Name: "ls", Description: "List files.", Parameters: json.RawMessage(`{"type":"object"}`)},
		},
	}

	wire, err := wireRequest(req, "fallback-model", "", nil, "", false, 0)
	if err != nil {
		t.Fatalf("wireRequest error = %v, want nil", err)
	}

	if wire.Model != "llama3.1:latest" {
		t.Errorf("Model = %q, want the request's model", wire.Model)
	}
	if len(wire.Messages) != 4 {
		t.Fatalf("Messages = %d, want 4", len(wire.Messages))
	}
	m0 := wire.Messages[0]
	if m0.Role != "system" || m0.Content != "You are helpful." {
		t.Errorf("Messages[0] = %+v, want the system prompt", m0)
	}
	m2 := wire.Messages[2]
	if m2.Role != "tool" || m2.Content != "a.txt" || m2.ToolCallID != "call_9" {
		t.Errorf("Messages[2] = %+v, want the tool result with its call id", m2)
	}
	m3 := wire.Messages[3]
	if len(m3.ToolCalls) != 1 {
		t.Fatalf("Messages[3].ToolCalls = %d, want 1", len(m3.ToolCalls))
	}
	tc := m3.ToolCalls[0]
	if tc.ID != "call_9" || tc.Function.Name != "ls" {
		t.Errorf("Messages[3].ToolCalls[0] = %+v, want call_9/ls", tc)
	}
	var args map[string]any
	if err := json.Unmarshal(tc.Function.Arguments, &args); err != nil {
		t.Fatalf("tool call arguments not valid JSON: %v (%s)", err, tc.Function.Arguments)
	}
	if args["path"] != "." {
		t.Errorf("tool call arguments = %s, want the embedded object form", tc.Function.Arguments)
	}

	if len(wire.Tools) != 1 {
		t.Fatalf("Tools = %d, want 1", len(wire.Tools))
	}
	tool := wire.Tools[0]
	if tool.Type != "function" || tool.Function.Name != "ls" || tool.Function.Description != "List files." {
		t.Errorf("Tools[0] = %+v, want the function envelope for ls", tool)
	}
	if string(tool.Function.Parameters) != `{"type":"object"}` {
		t.Errorf("Tools[0].Function.Parameters = %s, want the schema verbatim", tool.Function.Parameters)
	}
}

func TestWireRequestDefaultModel(t *testing.T) {
	t.Parallel()

	wire, err := wireRequest(llm.Request{}, "fallback-model", "", nil, "", false, 0)
	if err != nil {
		t.Fatalf("wireRequest error = %v, want nil", err)
	}
	if wire.Model != "fallback-model" {
		t.Errorf("Model = %q, want the default model", wire.Model)
	}

	wire, err = wireRequest(llm.Request{Model: "explicit"}, "fallback-model", "", nil, "", false, 0)
	if err != nil {
		t.Fatalf("wireRequest error = %v, want nil", err)
	}
	if wire.Model != "explicit" {
		t.Errorf("Model = %q, want the request's model", wire.Model)
	}
}

func TestWireRequestEmptyArgsBecomeObject(t *testing.T) {
	t.Parallel()

	req := llm.Request{
		Messages: []llm.Message{{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{
				{ID: "call_1", Type: "function", FunctionName: "ping", FunctionArgs: ""},
			},
		}},
	}
	wire, err := wireRequest(req, "m", "", nil, "", false, 0)
	if err != nil {
		t.Fatalf("wireRequest error = %v, want nil", err)
	}
	got := string(wire.Messages[0].ToolCalls[0].Function.Arguments)
	if got != "{}" {
		t.Errorf("arguments = %q, want %q for empty FunctionArgs", got, "{}")
	}
}

func TestWireRequestInvalidArgsError(t *testing.T) {
	t.Parallel()

	req := llm.Request{
		Messages: []llm.Message{{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{
				{ID: "call_1", Type: "function", FunctionName: "ping", FunctionArgs: `{oops`},
			},
		}},
	}
	_, err := wireRequest(req, "m", "", nil, "", false, 0)
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("error = %v, want an invalid-arguments error", err)
	}
}

func TestWireRequestThinkMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		effort string
		want   any // the decoded "think" value; nil means the key is absent
	}{
		{effort: "", want: nil},
		{effort: "none", want: false},
		{effort: "minimal", want: "minimal"},
		{effort: "low", want: "low"},
		{effort: "medium", want: "medium"},
		{effort: "high", want: "high"},
		{effort: "xhigh", want: "xhigh"},
		{effort: "max", want: "max"},
	}

	for _, tc := range cases {
		t.Run("effort="+tc.effort, func(t *testing.T) {
			t.Parallel()

			wire, err := wireRequest(llm.Request{}, "m", tc.effort, nil, "", false, 0)
			if err != nil {
				t.Fatalf("wireRequest error = %v, want nil", err)
			}
			got := decodeRequest(t, wire)
			think, present := got["think"]
			if tc.want == nil {
				if present {
					t.Errorf("think = %v, want the field omitted for empty effort", think)
				}
				return
			}
			if !present {
				t.Fatalf("think field absent, want %v", tc.want)
			}
			if think != tc.want {
				t.Errorf("think = %v (%T), want %v (%T)", think, think, tc.want, tc.want)
			}
		})
	}
}

// TestWireRequestThinkFalseSerializes pins the omitempty-on-interface
// subtlety: think carries an interface, which is empty only when nil, so
// a held false must still reach the wire as "think": false.
func TestWireRequestThinkFalseSerializes(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(chatRequest{Model: "m", Think: false})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"think":false`) {
		t.Errorf("wire JSON = %s, want it to contain \"think\":false", data)
	}
}

func TestWireRequestNoThinkingResent(t *testing.T) {
	t.Parallel()

	req := llm.Request{
		Messages: []llm.Message{{
			Role:      llm.RoleAssistant,
			Content:   "final answer",
			Reasoning: "stale thinking",
		}},
	}
	wire, err := wireRequest(req, "m", "", nil, "", false, 0)
	if err != nil {
		t.Fatalf("wireRequest error = %v, want nil", err)
	}
	if got := wire.Messages[0].Thinking; got != "" {
		t.Errorf("Thinking = %q, want it never sent to the server", got)
	}
}

// TestWireRequestEmptyContentOmitted pins that empty content omits the
// content field entirely rather than sending "content": "": Ollama does
// not require the field, and the empty string can knock tool-call models
// out of structured tool-calling mode (ollama/ollama#14181).
func TestWireRequestEmptyContentOmitted(t *testing.T) {
	t.Parallel()

	req := llm.Request{
		Messages: []llm.Message{
			// A system prompt carries content; a tool-call assistant
			// message and a content-less user turn do not.
			llm.NewTextMessage(llm.RoleSystem, "You are helpful."),
			{
				Role: llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{
					{ID: "call_1", Type: "function", FunctionName: "ping", FunctionArgs: "{}"},
				},
			},
		},
	}
	wire, err := wireRequest(req, "m", "", nil, "", false, 0)
	if err != nil {
		t.Fatalf("wireRequest error = %v, want nil", err)
	}

	data, err := json.Marshal(wire.Messages)
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, `"content":"You are helpful."`) {
		t.Errorf("wire messages = %s, want the system prompt's content present", s)
	}
	if strings.Contains(s, `"content":""`) {
		t.Errorf("wire messages = %s, want empty content omitted, not sent as \"\"", s)
	}
	if !strings.Contains(s, `"tool_calls":[{`) {
		t.Errorf("wire messages = %s, want the tool calls present", s)
	}
}

// TestWireRequestFormat pins the structured-output mapping: the "json"
// string form and the schema object serialize verbatim onto the wire, and
// an unset format omits the field.
func TestWireRequestFormat(t *testing.T) {
	t.Parallel()

	req := llm.Request{
		Messages: []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")},
	}

	t.Run("string form serializes verbatim", func(t *testing.T) {
		t.Parallel()

		wire, err := wireRequest(req, "m", "", json.RawMessage(`"json"`), "", false, 0)
		if err != nil {
			t.Fatalf("wireRequest error = %v, want nil", err)
		}
		got := decodeRequest(t, wire)
		if got["format"] != "json" {
			t.Errorf("wire format = %v, want %q", got["format"], "json")
		}
	})

	t.Run("schema object serializes verbatim", func(t *testing.T) {
		t.Parallel()

		schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}}}`)
		wire, err := wireRequest(req, "m", "", schema, "", false, 0)
		if err != nil {
			t.Fatalf("wireRequest error = %v, want nil", err)
		}
		got := decodeRequest(t, wire)
		obj, ok := got["format"].(map[string]any)
		if !ok || obj["type"] != "object" {
			t.Errorf("wire format = %#v, want the schema object", got["format"])
		}
	})

	t.Run("unset omits the field", func(t *testing.T) {
		t.Parallel()

		wire, err := wireRequest(req, "m", "", nil, "", false, 0)
		if err != nil {
			t.Fatalf("wireRequest error = %v, want nil", err)
		}
		got := decodeRequest(t, wire)
		if _, present := got["format"]; present {
			t.Errorf("wire format = %v, want the field omitted", got["format"])
		}
	})
}

// TestWireRequestKeepAlive pins the keep_alive mapping: set serializes
// verbatim; unset omits the field.
func TestWireRequestKeepAlive(t *testing.T) {
	t.Parallel()

	req := llm.Request{
		Messages: []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")},
	}

	t.Run("set serializes verbatim", func(t *testing.T) {
		t.Parallel()

		wire, err := wireRequest(req, "m", "", nil, "5m", false, 0)
		if err != nil {
			t.Fatalf("wireRequest error = %v, want nil", err)
		}
		got := decodeRequest(t, wire)
		if got["keep_alive"] != "5m" {
			t.Errorf("wire keep_alive = %v, want %q", got["keep_alive"], "5m")
		}
	})

	t.Run("unset omits the field", func(t *testing.T) {
		t.Parallel()

		wire, err := wireRequest(req, "m", "", nil, "", false, 0)
		if err != nil {
			t.Fatalf("wireRequest error = %v, want nil", err)
		}
		got := decodeRequest(t, wire)
		if _, present := got["keep_alive"]; present {
			t.Errorf("wire keep_alive = %v, want the field omitted", got["keep_alive"])
		}
	})
}

// TestWireRequestToolChoice pins the tool-choice mapping: auto (and nil)
// omit the field entirely; none and required serialize as the bare
// string; force serializes as the object form.
func TestWireRequestToolChoice(t *testing.T) {
	t.Parallel()

	req := llm.Request{
		Messages: []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")},
	}

	t.Run("auto omitted", func(t *testing.T) {
		t.Parallel()

		wire, err := wireRequest(withToolChoice(req, &llm.ToolChoice{Mode: llm.ToolChoiceAuto}), "m", "", nil, "", false, 0)
		if err != nil {
			t.Fatalf("wireRequest error = %v, want nil", err)
		}
		if got := decodeRequest(t, wire)["tool_choice"]; got != nil {
			t.Errorf("wire tool_choice = %v, want the field omitted", got)
		}
	})

	t.Run("nil omitted", func(t *testing.T) {
		t.Parallel()

		wire, err := wireRequest(req, "m", "", nil, "", false, 0)
		if err != nil {
			t.Fatalf("wireRequest error = %v, want nil", err)
		}
		if got := decodeRequest(t, wire)["tool_choice"]; got != nil {
			t.Errorf("wire tool_choice = %v, want the field omitted", got)
		}
	})

	t.Run("none and required as bare strings", func(t *testing.T) {
		t.Parallel()

		for _, mode := range []llm.ToolChoiceMode{llm.ToolChoiceNone, llm.ToolChoiceRequired} {
			wire, err := wireRequest(withToolChoice(req, &llm.ToolChoice{Mode: mode}), "m", "", nil, "", false, 0)
			if err != nil {
				t.Fatalf("wireRequest error = %v, want nil", err)
			}
			if got := decodeRequest(t, wire)["tool_choice"]; got != string(mode) {
				t.Errorf("wire tool_choice = %v, want %q", got, string(mode))
			}
		}
	})

	t.Run("force as the object form", func(t *testing.T) {
		t.Parallel()

		wire, err := wireRequest(withToolChoice(req, &llm.ToolChoice{Mode: llm.ToolChoiceForce, ForceTool: "echo"}), "m", "", nil, "", false, 0)
		if err != nil {
			t.Fatalf("wireRequest error = %v, want nil", err)
		}
		got := decodeRequest(t, wire)
		obj, ok := got["tool_choice"].(map[string]any)
		if !ok {
			t.Fatalf("wire tool_choice = %#v, want the object form", got["tool_choice"])
		}
		if obj["type"] != "function" {
			t.Errorf("wire tool_choice type = %v, want function", obj["type"])
		}
		fn, _ := obj["function"].(map[string]any)
		if fn == nil || fn["name"] != "echo" {
			t.Errorf("wire tool_choice function = %#v, want name echo", obj["function"])
		}
	})
}

// withToolChoice returns req with tc set on the sampling-less copy.
func withToolChoice(req llm.Request, tc *llm.ToolChoice) llm.Request {
	req.ToolChoice = tc
	return req
}

func TestNeutralResponseContentAndThinking(t *testing.T) {
	t.Parallel()

	chunk := &chatResponse{
		Message: wireMessage{
			Role:     "assistant",
			Content:  "2 + 2 = 4",
			Thinking: "The user asks a sum.",
		},
		Done:            true,
		DoneReason:      "stop",
		PromptEvalCount: 12,
		EvalCount:       5,
	}

	resp := neutralResponse(chunk)
	if resp.Message.Role != llm.RoleAssistant {
		t.Errorf("Role = %q, want assistant", resp.Message.Role)
	}
	if got, want := resp.Message.Content, "2 + 2 = 4"; got != want {
		t.Errorf("Content = %q, want %q", got, want)
	}
	if got, want := resp.Message.Reasoning, "The user asks a sum."; got != want {
		t.Errorf("Reasoning = %q, want the thinking mapped to reasoning", got)
	}
	if resp.FinishReason != llm.FinishStop {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, llm.FinishStop)
	}
	if resp.Usage.PromptTokens != 12 || resp.Usage.CompletionTokens != 5 || resp.Usage.TotalTokens != 17 {
		t.Errorf("Usage = %+v, want prompt 12, completion 5, total 17", resp.Usage)
	}
	if resp.ID != "" {
		t.Errorf("ID = %q, want empty (Ollama has no response id)", resp.ID)
	}
}

func TestNeutralResponseFinishReasons(t *testing.T) {
	t.Parallel()

	cases := []struct {
		doneReason string
		want       string
	}{
		{doneReason: "stop", want: llm.FinishStop},
		{doneReason: "length", want: llm.FinishLength},
		{doneReason: "tool_calls", want: llm.FinishToolCalls},
		{doneReason: "unload", want: llm.FinishStop},
		{doneReason: "", want: llm.FinishStop},
	}

	for _, tc := range cases {
		resp := neutralResponse(&chatResponse{DoneReason: tc.doneReason})
		if resp.FinishReason != tc.want {
			t.Errorf("done_reason %q: FinishReason = %q, want %q", tc.doneReason, resp.FinishReason, tc.want)
		}
	}
}

func TestNeutralResponseToolCalls(t *testing.T) {
	t.Parallel()

	chunk := &chatResponse{
		Message: wireMessage{
			Role: "assistant",
			ToolCalls: []wireToolCall{
				{
					ID: "srv-id",
					Function: wireFnCall{
						Name:      "ls",
						Arguments: json.RawMessage(`{"path":"."}`),
					},
				},
				{
					Function: wireFnCall{
						Name: "ping",
					},
				},
			},
		},
		Done:       true,
		DoneReason: "tool_calls",
	}

	resp := neutralResponse(chunk)
	if resp.FinishReason != llm.FinishToolCalls {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, llm.FinishToolCalls)
	}
	if len(resp.Message.ToolCalls) != 2 {
		t.Fatalf("ToolCalls = %d, want 2", len(resp.Message.ToolCalls))
	}

	first := resp.Message.ToolCalls[0]
	if first.ID != "srv-id" || first.Type != llm.ToolCallType || first.FunctionName != "ls" {
		t.Errorf("ToolCalls[0] = %+v, want srv-id/function/ls", first)
	}
	if got, want := first.FunctionArgs, `{"path":"."}`; got != want {
		t.Errorf("ToolCalls[0].FunctionArgs = %q, want the object as a string", got)
	}

	// Ollama omits tool call ids: the client synthesizes one so the
	// engine's history matching and repair keep working.
	second := resp.Message.ToolCalls[1]
	if !strings.HasPrefix(second.ID, "call_") || len(second.ID) != len("call_")+16 {
		t.Errorf("ToolCalls[1].ID = %q, want call_ + 16 hex chars", second.ID)
	}
	if got, want := second.FunctionArgs, "{}"; got != want {
		t.Errorf("ToolCalls[1].FunctionArgs = %q, want %q for missing arguments", got, want)
	}
}
