package llm_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/overspecific/blorb/internal/llm"
)

type mockStreamingClient struct{}

var _ llm.StreamingClient = (*mockStreamingClient)(nil)

func (c *mockStreamingClient) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return &llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "mock")}, nil
}

func (c *mockStreamingClient) ChatStream(ctx context.Context, req llm.Request, onDelta func(llm.Delta) error) (*llm.Response, error) {
	deltas := []llm.Delta{
		{Content: "hel"},
		{Content: "lo"},
	}
	for _, d := range deltas {
		if err := onDelta(d); err != nil {
			return nil, err
		}
	}
	return &llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "hello")}, nil
}

func TestStreamingClientInterface(t *testing.T) {
	t.Parallel()

	var c llm.Client = &mockStreamingClient{}
	var sc llm.StreamingClient = &mockStreamingClient{}
	_ = sc

	resp, err := c.Chat(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("Chat error = %v, want nil", err)
	}
	if resp.Message.Content != "mock" {
		t.Errorf("Chat content = %q, want %q", resp.Message.Content, "mock")
	}

	var deltas []llm.Delta
	resp, err = sc.ChatStream(context.Background(), llm.Request{}, func(d llm.Delta) error {
		deltas = append(deltas, d)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream error = %v, want nil", err)
	}
	if len(deltas) != 2 || deltas[0].Content != "hel" || deltas[1].Content != "lo" {
		t.Errorf("deltas = %v, want [hel lo]", deltas)
	}
	if resp.Message.Content != "hello" {
		t.Errorf("ChatStream content = %q, want %q", resp.Message.Content, "hello")
	}

	streamErr := context.Canceled
	if _, err := sc.ChatStream(context.Background(), llm.Request{}, func(d llm.Delta) error {
		return streamErr
	}); err == nil || err != streamErr {
		t.Errorf("onDelta error = %v, want %v (returned as-is)", err, streamErr)
	}
}

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
		// The neutral shape is deliberately NOT the OpenAI wire shape:
		// tool call fields are flat rather than nested under "function".
		// Wire conversion is the openai package's responsibility and is
		// tested there against literal wire JSON.
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

func TestMessageOutputBytes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		msg  llm.Message
		want llm.OutputBytes
	}{
		{
			name: "empty message",
			msg:  llm.Message{},
			want: llm.OutputBytes{},
		},
		{
			name: "content only",
			msg:  llm.NewTextMessage(llm.RoleAssistant, "hello"),
			want: llm.OutputBytes{Content: 5},
		},
		{
			name: "content and reasoning",
			msg: llm.Message{
				Content:   "hi",
				Reasoning: "thinking hard",
			},
			want: llm.OutputBytes{Content: 2, Reasoning: 13},
		},
		{
			name: "two tool calls",
			msg: llm.Message{
				ToolCalls: []llm.ToolCall{
					{ID: "call_1", Type: "function", FunctionName: "ls", FunctionArgs: `{"path":"."}`},
					{ID: "", FunctionName: "pwd", FunctionArgs: `{}`},
				},
			},
			// Name+args lengths only: ls(2)+12 + pwd(3)+2 = 19.
			// The id on the first call is server-generated and
			// excluded.
			want: llm.OutputBytes{ToolCalls: 19},
		},
		{
			name: "everything",
			msg: llm.Message{
				Content:   "abc",
				Reasoning: "zz",
				ToolCalls: []llm.ToolCall{
					{ID: "call_1", Type: "function", FunctionName: "ls", FunctionArgs: `{}`},
				},
			},
			want: llm.OutputBytes{Content: 3, Reasoning: 2, ToolCalls: 4},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := tc.msg.OutputBytes()
			if got != tc.want {
				t.Errorf("OutputBytes = %+v, want %+v", got, tc.want)
			}
			if total := got.Total(); total != tc.want.Content+tc.want.Reasoning+tc.want.ToolCalls {
				t.Errorf("Total() = %d, want %d", total, tc.want.Content+tc.want.Reasoning+tc.want.ToolCalls)
			}
		})
	}
}

func TestResponseCallStatsJSON(t *testing.T) {
	t.Parallel()

	t.Run("non-zero stats round-trip", func(t *testing.T) {
		t.Parallel()

		resp := llm.Response{
			Stats: llm.CallStats{
				Output:  llm.OutputBytes{Content: 11, Reasoning: 22, ToolCalls: 33},
				Elapsed: 1500 * time.Millisecond,
			},
		}

		data, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("Marshal error = %v, want nil", err)
		}

		var obj struct {
			Stats struct {
				Output struct {
					ContentBytes   int `json:"content_bytes"`
					ReasoningBytes int `json:"reasoning_bytes"`
					ToolCallBytes  int `json:"tool_call_bytes"`
				} `json:"output"`
				ElapsedNS int64 `json:"elapsed_ns"`
			} `json:"stats"`
		}
		if err := json.Unmarshal(data, &obj); err != nil {
			t.Fatalf("Unmarshal error = %v, want nil", err)
		}
		if obj.Stats.Output.ContentBytes != 11 {
			t.Errorf("stats.output.content_bytes = %d, want 11", obj.Stats.Output.ContentBytes)
		}
		if obj.Stats.Output.ReasoningBytes != 22 {
			t.Errorf("stats.output.reasoning_bytes = %d, want 22", obj.Stats.Output.ReasoningBytes)
		}
		if obj.Stats.Output.ToolCallBytes != 33 {
			t.Errorf("stats.output.tool_call_bytes = %d, want 33", obj.Stats.Output.ToolCallBytes)
		}
		if obj.Stats.ElapsedNS != 1500000000 {
			t.Errorf("stats.elapsed_ns = %d, want 1500000000", obj.Stats.ElapsedNS)
		}

		var back llm.Response
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatalf("Unmarshal error = %v, want nil", err)
		}
		if back.Stats != resp.Stats {
			t.Errorf("Stats round-trip = %+v, want %+v", back.Stats, resp.Stats)
		}
	})

	t.Run("zero stats always present", func(t *testing.T) {
		t.Parallel()

		data, err := json.Marshal(llm.Response{})
		if err != nil {
			t.Fatalf("Marshal error = %v, want nil", err)
		}
		want := `"stats":{"output":{"content_bytes":0,"reasoning_bytes":0,"tool_call_bytes":0},"elapsed_ns":0}`
		if !strings.Contains(string(data), want) {
			t.Errorf("Marshal = %s, want to contain %s", data, want)
		}
	})
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
