package llm_test

import (
	"encoding/json"
	"testing"

	"github.com/overspecific/goblorb/internal/llm"
)

func TestMessageMarshalJSON(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		msg  llm.Message
		want string
	}{
		{
			name: "system",
			msg:  llm.NewTextMessage(llm.RoleSystem, "You are helpful."),
			want: `{"role":"system","content":"You are helpful."}`,
		},
		{
			name: "user",
			msg:  llm.NewTextMessage(llm.RoleUser, "hello"),
			want: `{"role":"user","content":"hello"}`,
		},
		{
			name: "assistant plain",
			msg:  llm.NewTextMessage(llm.RoleAssistant, "hi there"),
			want: `{"role":"assistant","content":"hi there"}`,
		},
		{
			name: "assistant with tool calls",
			msg: llm.Message{
				Role:    llm.RoleAssistant,
				Content: "",
				ToolCalls: []llm.ToolCall{
					{ID: "call_1", Type: "function", FunctionName: "ls", FunctionArgs: `{"path":"."}`},
				},
			},
			want: `{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","name":"ls","arguments":"{\"path\":\".\"}"}]}`,
		},
		{
			name: "tool result",
			msg:  llm.NewToolResultMessage("call_1", "file list", false),
			want: `{"role":"tool","content":"file list","tool_call_id":"call_1"}`,
		},
		{
			name: "failed tool result",
			msg:  llm.NewToolResultMessage("call_2", "boom", true),
			want: `{"role":"tool","content":"Tool call failed: boom","tool_call_id":"call_2"}`,
		},
		{
			name: "tool result omits empty fields",
			msg:  llm.NewToolResultMessage("call_1", "out", false),
			want: `{"role":"tool","content":"out","tool_call_id":"call_1"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.msg)
			if err != nil {
				t.Fatalf("Marshal error = %v, want nil", err)
			}
			if string(got) != tc.want {
				t.Errorf("Marshal = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestToolCallDecodedArgs(t *testing.T) {
	t.Parallel()

	t.Run("encoded object", func(t *testing.T) {
		tc := llm.ToolCall{FunctionName: "ls", FunctionArgs: `{"path":"."}`}

		raw, err := tc.DecodedArgs()
		if err != nil {
			t.Fatalf("DecodedArgs error = %v, want nil", err)
		}
		if string(raw) != `{"path":"."}` {
			t.Errorf("DecodedArgs = %s, want %s", raw, `{"path":"."}`)
		}
	})

	t.Run("empty args yield empty object", func(t *testing.T) {
		tc := llm.ToolCall{FunctionName: "ping", FunctionArgs: ""}

		raw, err := tc.DecodedArgs()
		if err != nil {
			t.Fatalf("DecodedArgs error = %v, want nil", err)
		}
		if string(raw) != "{}" {
			t.Errorf("DecodedArgs = %s, want {}", raw)
		}
	})

	t.Run("invalid encoded args", func(t *testing.T) {
		tc := llm.ToolCall{FunctionName: "broken", FunctionArgs: `not json`}

		_, err := tc.DecodedArgs()
		if err == nil {
			t.Fatal("DecodedArgs succeeded, want error")
		}
	})
}

func TestToolMarshalJSON(t *testing.T) {
	t.Parallel()

	tool := llm.Tool{
		Name:        "ls",
		Description: "List files.",
		Parameters:  json.RawMessage(`{"type":"object"}`),
	}

	got, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("Marshal error = %v, want nil", err)
	}
	want := `{"name":"ls","description":"List files.","parameters":{"type":"object"}}`
	if string(got) != want {
		t.Errorf("Marshal = %s, want %s", got, want)
	}
}

func TestFinishReasonValuesPassThrough(t *testing.T) {
	t.Parallel()

	for _, reason := range []string{llm.FinishStop, llm.FinishToolCalls, llm.FinishLength, "", "exotic_reason"} {
		resp := llm.Response{FinishReason: reason}

		data, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("Marshal error = %v, want nil", err)
		}

		var back llm.Response
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatalf("Unmarshal error = %v, want nil", err)
		}
		if back.FinishReason != reason {
			t.Errorf("FinishReason round-trip = %q, want %q", back.FinishReason, reason)
		}
	}
}
