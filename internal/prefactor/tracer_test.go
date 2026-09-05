package prefactor_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/prefactor"
)

// call is one recorded API call made by the code under test.
type call struct {
	Method string
	Path   string
	Body   map[string]any
}

// fakePF is an in-memory Prefactor API: it hands out IDs and records every
// call. A mutex guards against the tracer's internal concurrency and the
// HTTP server's handler goroutines.
type fakePF struct {
	mu     sync.Mutex
	calls  []call
	nextID int

	// failOn, when set, maps a path prefix to a status code and error body
	// served for any matching POST.
	failOn     string
	failStatus int
	failBody   map[string]any

	// terminateOn, when set, causes matching paths to respond with
	// control.terminate true and the given reason.
	terminateOn   string
	terminateOnce bool
	terminated    bool
}

func newFakePF(t *testing.T) (*fakePF, *prefactor.Client) {
	t.Helper()
	f := &fakePF{nextID: 1}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	client := prefactor.New(prefactor.Config{BaseURL: srv.URL, Token: "t"})
	return f, client
}

func (f *fakePF) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	f.mu.Lock()
	f.calls = append(f.calls, call{Method: r.Method, Path: r.URL.Path, Body: body})

	if f.failOn != "" && strings.HasPrefix(r.URL.Path, f.failOn) {
		status := f.failStatus
		errBody := f.failBody
		f.mu.Unlock()
		writeJSON(w, status, errBody)
		return
	}
	if f.terminateOn != "" && strings.HasPrefix(r.URL.Path, f.terminateOn) && !f.terminated {
		f.terminated = true
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "success",
			"control": map[string]any{"terminate": true, "reason": "operator stopped this run"},
			"details": map[string]any{"id": f.id()},
		})
		return
	}
	f.mu.Unlock()

	if r.URL.Path == "/agent_instance/register" {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "success",
			"details": map[string]any{"id": f.id()},
		})
		return
	}
	if strings.HasSuffix(r.URL.Path, "/start") || strings.HasSuffix(r.URL.Path, "/finish") &&
		strings.HasPrefix(r.URL.Path, "/agent_instance/") {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "success",
			"details": map[string]any{"id": "inst-1"},
		})
		return
	}
	if r.URL.Path == "/agent_spans" {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "success",
			"control": map[string]any{"terminate": false},
			"details": map[string]any{"id": f.id()},
		})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/agent_spans/") && strings.HasSuffix(r.URL.Path, "/finish") {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "success",
			"control": map[string]any{"terminate": false},
			"details": map[string]any{"id": "span-x", "status": "complete"},
		})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]any{
		"error": map[string]any{"code": "not_found", "message": "unexpected path " + r.URL.Path},
	})
}

// id mints a fresh ID; caller holds f.mu.
func (f *fakePF) id() string {
	id := "id-" + strconv.Itoa(f.nextID)
	f.nextID++
	return id
}

// snapshot returns the recorded calls.
func (f *fakePF) snapshot() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

// reset clears the recorded calls.
func (f *fakePF) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func (f *fakePF) setFail(pathPrefix string, status int, body map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOn, f.failStatus, f.failBody = pathPrefix, status, body
}

func (f *fakePF) setTerminate(pathPrefix string, once bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminateOn, f.terminateOnce = pathPrefix, once
}

// detail extracts the details object from a recorded call.
func detail(c call) map[string]any {
	d, _ := c.Body["details"].(map[string]any)
	return d
}

// find returns the first call matching a path prefix, failing the test when
// absent.
func find(t *testing.T, calls []call, pathPrefix string) call {
	t.Helper()
	for _, c := range calls {
		if strings.HasPrefix(c.Path, pathPrefix) {
			return c
		}
	}
	t.Fatalf("no call to %s among %d calls", pathPrefix, len(calls))
	return call{}
}

// newTestTracer builds a tracer over the fake, with a fixed clock.
func newTestTracer(t *testing.T, f *fakePF, client *prefactor.Client) *prefactor.Tracer {
	t.Helper()
	return prefactor.NewTracer(prefactor.TracerConfig{
		Client:       client,
		AgentName:    "helper",
		AgentVersion: "blorb-helper_v1",
		Now:          func() time.Time { return time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC) },
	})
}

func testSchema() prefactor.AgentSchemaVersion {
	return prefactor.AgentSchemaVersion{
		ExternalIdentifier: "blorb-schema-test",
		SpanTypeSchemas: []prefactor.SpanTypeSchema{
			{Name: prefactor.SchemaAgentTurn},
		},
	}
}

// testRequest builds an LLM request for tests.
func testRequest() llm.Request {
	return llm.Request{
		Model: "gpt-4o-mini",
		Messages: []llm.Message{
			llm.NewTextMessage(llm.RoleSystem, "You are helpful."),
			llm.NewTextMessage(llm.RoleUser, "hello"),
		},
	}
}

func testResponse() llm.Response {
	return llm.Response{
		ID: "resp-1",
		Message: llm.Message{
			Role:    llm.RoleAssistant,
			Content: "hi there",
		},
		FinishReason: llm.FinishStop,
		Usage:        llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
}

func TestTracerFullSession(t *testing.T) {
	f, client := newFakePF(t)
	tr := newTestTracer(t, f, client)
	ctx := context.Background()

	if err := tr.StartSession(ctx, testSchema()); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	turn, err := tr.StartTurn(ctx, "hello")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}

	ls, err := turn.LLMCall(ctx, testRequest())
	if err != nil {
		t.Fatalf("LLMCall: %v", err)
	}
	if err := ls.Complete(testResponse()); err != nil {
		t.Fatalf("LLMSpan.Complete: %v", err)
	}

	// A successful tool call.
	ts, err := turn.ToolCall(ctx, "list_files", `{"path":"."}`)
	if err != nil {
		t.Fatalf("ToolCall: %v", err)
	}
	if err := ts.Complete("file.txt", false); err != nil {
		t.Fatalf("ToolSpan.Complete: %v", err)
	}

	// A failed tool call.
	ts2, err := turn.ToolCall(ctx, "boom", `{}`)
	if err != nil {
		t.Fatalf("ToolCall: %v", err)
	}
	if err := ts2.Complete("tool failed with exit code 1", true); err != nil {
		t.Fatalf("ToolSpan.Complete: %v", err)
	}

	if err := turn.AssistantMessage(ctx, "hi there", ""); err != nil {
		t.Fatalf("AssistantMessage: %v", err)
	}

	if err := turn.Complete("hi there"); err != nil {
		t.Fatalf("Turn.Complete: %v", err)
	}

	if err := tr.FinishSession(ctx, prefactor.InstanceComplete); err != nil {
		t.Fatalf("FinishSession: %v", err)
	}

	calls := f.snapshot()

	// Instance lifecycle.
	reg := find(t, calls, "/agent_instance/register")
	sv, ok := reg.Body["agent_schema_version"].(map[string]any)
	if !ok || sv["external_identifier"] != "blorb-schema-test" {
		t.Errorf("register: agent_schema_version = %v, want external_identifier blorb-schema-test", reg.Body["agent_schema_version"])
	}
	find(t, calls, "/agent_instance/id-1/start")
	fin := find(t, calls, "/agent_instance/id-1/finish")
	if fin.Body["status"] != prefactor.InstanceComplete {
		t.Errorf("instance finish status = %v, want complete", fin.Body["status"])
	}

	// Span creation order: turn, user_message, llm, tool, tool, assistant_message.
	spanCalls := make([]call, 0)
	for _, c := range calls {
		if c.Path == "/agent_spans" {
			spanCalls = append(spanCalls, c)
		}
	}
	if len(spanCalls) != 6 {
		t.Fatalf("span creates = %d, want 6 (turn, user_message, llm, tool, tool, assistant_message)", len(spanCalls))
	}

	turnID := "id-2" // first span created after register+start (ids 1, 2, ...)
	// Actually, mint order: instance id-1 (register), then turn span id-2.
	wantSchema := []string{
		prefactor.SchemaAgentTurn,
		prefactor.SchemaUserMessage,
		prefactor.SchemaLLM,
		prefactor.ToolSchemaName("list_files"),
		prefactor.ToolSchemaName("boom"),
		prefactor.SchemaAssistantMessage,
	}
	for i, c := range spanCalls {
		d := detail(c)
		if d["agent_instance_id"] != "id-1" {
			t.Errorf("span %d: agent_instance_id = %v, want id-1", i, d["agent_instance_id"])
		}
		if d["schema_name"] != wantSchema[i] {
			t.Errorf("span %d: schema_name = %v, want %v", i, d["schema_name"], wantSchema[i])
		}
		switch i {
		case 0:
			if d["status"] != prefactor.StatusActive {
				t.Errorf("turn span status = %v, want active", d["status"])
			}
		case 1:
			if d["parent_span_id"] != turnID {
				t.Errorf("user_message parent = %v, want %v", d["parent_span_id"], turnID)
			}
			if d["status"] != prefactor.StatusComplete {
				t.Errorf("user_message status = %v, want complete", d["status"])
			}
		case 2, 3, 4:
			if d["parent_span_id"] != turnID {
				t.Errorf("span %d parent = %v, want %v", i, d["parent_span_id"], turnID)
			}
			if d["status"] != prefactor.StatusActive {
				t.Errorf("span %d status = %v, want active", i, d["status"])
			}
		case 5:
			if d["parent_span_id"] != turnID {
				t.Errorf("assistant_message parent = %v, want %v", d["parent_span_id"], turnID)
			}
			if d["status"] != prefactor.StatusComplete {
				t.Errorf("assistant_message status = %v, want complete", d["status"])
			}
		}
	}

	// Span finishes: llm, tool, tool, then the turn (order may vary but all
	// present with correct statuses).
	finishes := make(map[string]string) // span id -> status
	for _, c := range calls {
		if strings.HasPrefix(c.Path, "/agent_spans/") && strings.HasSuffix(c.Path, "/finish") {
			finishes[c.Path] = fmt.Sprint(c.Body["status"])
		}
	}
	finishStatuses := make(map[string]int)
	for _, status := range finishes {
		finishStatuses[status]++
	}
	if finishStatuses["complete"] != 4 { // llm, tool, tool, turn
		t.Errorf("complete finishes = %d, want 4; all: %v", finishStatuses["complete"], finishes)
	}

	// The turn finish payload carries the final text.
	var turnFinish map[string]any
	for _, c := range calls {
		if c.Path == "/agent_spans/id-2/finish" {
			turnFinish = c.Body
		}
	}
	if turnFinish == nil {
		t.Fatal("no finish call for the turn span id-2")
	}
	rp, _ := turnFinish["result_payload"].(map[string]any)
	if rp["assistant_text"] != "hi there" {
		t.Errorf("turn finish assistant_text = %v, want hi there", rp["assistant_text"])
	}
}

func TestTracerLLMSpanPayloads(t *testing.T) {
	f, client := newFakePF(t)
	tr := newTestTracer(t, f, client)
	ctx := context.Background()

	if err := tr.StartSession(ctx, testSchema()); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	turn, err := tr.StartTurn(ctx, "hello")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}

	req := testRequest()
	req.Messages = append(req.Messages, llm.Message{
		Role:      llm.RoleAssistant,
		Content:   "let me check",
		ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function", FunctionName: "list_files", FunctionArgs: `{"path":"."}`}},
	})
	ls, err := turn.LLMCall(ctx, req)
	if err != nil {
		t.Fatalf("LLMCall: %v", err)
	}

	resp := testResponse()
	resp.Message.Reasoning = "thinking hard"
	// Non-zero stats: the result payload must carry the nested
	// output_bytes breakdown and the millisecond-rounded elapsed.
	resp.Stats = llm.CallStats{
		Output:  llm.OutputBytes{Content: 8, Reasoning: 13, ToolCalls: 21},
		Elapsed: 1500 * time.Millisecond,
	}
	if err := ls.Complete(resp); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	calls := f.snapshot()
	var llmCreate map[string]any
	for _, c := range calls {
		if c.Path == "/agent_spans" && detail(c)["schema_name"] == prefactor.SchemaLLM {
			llmCreate = detail(c)
		}
	}
	if llmCreate == nil {
		t.Fatal("no blorb:llm span create")
	}
	if llmCreate["payload"] == nil {
		t.Fatal("llm span has no payload")
	}
	payloadBytes, _ := json.Marshal(llmCreate["payload"])
	var payload struct {
		Model    string `json:"model"`
		Messages []struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				Name string `json:"name"`
			} `json:"tool_calls,omitempty"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Model != "gpt-4o-mini" {
		t.Errorf("payload model = %q, want gpt-4o-mini", payload.Model)
	}
	if len(payload.Messages) != 3 {
		t.Fatalf("payload messages = %d, want 3", len(payload.Messages))
	}
	if len(payload.Messages[2].ToolCalls) != 1 || payload.Messages[2].ToolCalls[0].Name != "list_files" {
		t.Errorf("assistant tool_calls not in payload: %v", payload.Messages[2])
	}

	// Result payload on finish.
	var finish map[string]any
	for _, c := range calls {
		if strings.HasPrefix(c.Path, "/agent_spans/") && strings.HasSuffix(c.Path, "/finish") {
			finish = c.Body
		}
	}
	if finish == nil {
		t.Fatal("no llm span finish")
	}
	rpBytes, _ := json.Marshal(finish["result_payload"])
	var result struct {
		Content   string `json:"content"`
		Reasoning string `json:"reasoning"`
		Usage     struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
		Stats struct {
			OutputBytes struct {
				Content   int `json:"content"`
				Reasoning int `json:"reasoning"`
				ToolCalls int `json:"tool_calls"`
				Total     int `json:"total"`
			} `json:"output_bytes"`
			ElapsedMS int64 `json:"elapsed_ms"`
		} `json:"stats"`
		FinishReason string `json:"finish_reason"`
		Status       string `json:"status"`
	}
	if err := json.Unmarshal(rpBytes, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.Content != "hi there" || result.Reasoning != "thinking hard" {
		t.Errorf("result content/reasoning = %q/%q, want hi there/thinking hard", result.Content, result.Reasoning)
	}
	if result.Usage.TotalTokens != 15 {
		t.Errorf("usage total_tokens = %d, want 15", result.Usage.TotalTokens)
	}
	if result.Stats.OutputBytes.Content != 8 || result.Stats.OutputBytes.Reasoning != 13 || result.Stats.OutputBytes.ToolCalls != 21 {
		t.Errorf("stats.output_bytes = %+v, want content 8, reasoning 13, tool_calls 21", result.Stats.OutputBytes)
	}
	if result.Stats.OutputBytes.Total != 42 {
		t.Errorf("stats.output_bytes.total = %d, want 42", result.Stats.OutputBytes.Total)
	}
	if result.Stats.ElapsedMS != 1500 {
		t.Errorf("stats.elapsed_ms = %d, want 1500 (millisecond-rounded)", result.Stats.ElapsedMS)
	}
	if result.FinishReason != llm.FinishStop || result.Status != "complete" {
		t.Errorf("finish_reason/status = %q/%q, want stop/complete", result.FinishReason, result.Status)
	}
}

func TestTracerFailPaths(t *testing.T) {
	f, client := newFakePF(t)
	tr := newTestTracer(t, f, client)
	ctx := context.Background()

	if err := tr.StartSession(ctx, testSchema()); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	turn, err := tr.StartTurn(ctx, "hello")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}

	// Failed LLM span.
	ls, err := turn.LLMCall(ctx, testRequest())
	if err != nil {
		t.Fatalf("LLMCall: %v", err)
	}
	if err := ls.Fail(errors.New("connection reset")); err != nil {
		t.Fatalf("LLMSpan.Fail: %v", err)
	}

	// Failed tool span.
	ts, err := turn.ToolCall(ctx, "boom", `{}`)
	if err != nil {
		t.Fatalf("ToolCall: %v", err)
	}
	if err := ts.Fail(errors.New("process exploded")); err != nil {
		t.Fatalf("ToolSpan.Fail: %v", err)
	}

	if err := turn.Fail(errors.New("something broke")); err != nil {
		t.Fatalf("Turn.Fail: %v", err)
	}
	if err := tr.FinishSession(ctx, prefactor.InstanceFailed); err != nil {
		t.Fatalf("FinishSession: %v", err)
	}

	calls := f.snapshot()
	failed := 0
	for _, c := range calls {
		if strings.HasPrefix(c.Path, "/agent_spans/") && strings.HasSuffix(c.Path, "/finish") {
			if c.Body["status"] != prefactor.StatusFailed {
				t.Errorf("finish %s status = %v, want failed", c.Path, c.Body["status"])
				continue
			}
			failed++
			if rp, ok := c.Body["result_payload"].(map[string]any); ok {
				if rp["error"] == nil {
					t.Errorf("finish %s: result_payload has no error text", c.Path)
				}
			}
		}
	}
	if failed != 3 {
		t.Errorf("failed finishes = %d, want 3 (llm, tool, turn)", failed)
	}

	fin := find(t, calls, "/agent_instance/id-1/finish")
	if fin.Body["status"] != prefactor.InstanceFailed {
		t.Errorf("instance finish status = %v, want failed", fin.Body["status"])
	}
}

func TestTracerTurnCancel(t *testing.T) {
	f, client := newFakePF(t)
	tr := newTestTracer(t, f, client)
	ctx := context.Background()

	if err := tr.StartSession(ctx, testSchema()); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	turn, err := tr.StartTurn(ctx, "hello")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	if err := turn.Cancel(); err != nil {
		t.Fatalf("Turn.Cancel: %v", err)
	}
	if err := tr.FinishSession(ctx, prefactor.InstanceCancelled); err != nil {
		t.Fatalf("FinishSession: %v", err)
	}

	calls := f.snapshot()
	turnFin := find(t, calls, "/agent_spans/id-2/finish")
	if turnFin.Body["status"] != prefactor.StatusCancelled {
		t.Errorf("turn finish status = %v, want cancelled", turnFin.Body["status"])
	}
	fin := find(t, calls, "/agent_instance/id-1/finish")
	if fin.Body["status"] != prefactor.InstanceCancelled {
		t.Errorf("instance finish status = %v, want cancelled", fin.Body["status"])
	}
}

func TestTracerStartSessionRegisterFailure(t *testing.T) {
	f, client := newFakePF(t)
	f.setFail("/agent_instance/register", http.StatusInternalServerError, map[string]any{
		"error": map[string]any{"code": "boom", "message": "nope"},
	})
	tr := newTestTracer(t, f, client)

	err := tr.StartSession(context.Background(), testSchema())
	if err == nil {
		t.Fatal("StartSession succeeded, want error on register failure")
	}
	if !strings.Contains(err.Error(), "register instance") {
		t.Errorf("error = %v, want it to mention register instance", err)
	}
	var apiErr *prefactor.APIError
	if !errors.As(err, &apiErr) {
		t.Errorf("error = %v, want a wrapped *prefactor.APIError", err)
	}
}

func TestTracerStartTurnFailure(t *testing.T) {
	f, client := newFakePF(t)
	tr := newTestTracer(t, f, client)
	ctx := context.Background()

	if err := tr.StartSession(ctx, testSchema()); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	f.setFail("/agent_spans", http.StatusInternalServerError, map[string]any{
		"error": map[string]any{"code": "boom", "message": "nope"},
	})
	_, err := tr.StartTurn(ctx, "hello")
	if err == nil {
		t.Fatal("StartTurn succeeded, want error on span-create failure")
	}
	if !strings.Contains(err.Error(), "start turn") {
		t.Errorf("error = %v, want it to mention start turn", err)
	}
}

func TestTracerSpanCreateFailure(t *testing.T) {
	f, client := newFakePF(t)
	tr := newTestTracer(t, f, client)
	ctx := context.Background()

	if err := tr.StartSession(ctx, testSchema()); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	turn, err := tr.StartTurn(ctx, "hello")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}

	// The turn span and user message went through; now fail everything on
	// /agent_spans.
	f.reset()
	f.setFail("/agent_spans", http.StatusInternalServerError, map[string]any{
		"error": map[string]any{"code": "boom", "message": "nope"},
	})

	_, err = turn.LLMCall(ctx, testRequest())
	if err == nil {
		t.Error("LLMCall succeeded, want error")
	}
	_, err = turn.ToolCall(ctx, "x", `{}`)
	if err == nil {
		t.Error("ToolCall succeeded, want error")
	}
	err = turn.AssistantMessage(ctx, "x", "")
	if err == nil {
		t.Error("AssistantMessage succeeded, want error")
	}
}

func TestTracerTerminatedOnSpanCreate(t *testing.T) {
	f, client := newFakePF(t)
	tr := newTestTracer(t, f, client)
	ctx := context.Background()

	if err := tr.StartSession(ctx, testSchema()); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	turn, err := tr.StartTurn(ctx, "hello")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}

	f.setTerminate("/agent_spans", true)

	_, err = turn.LLMCall(ctx, testRequest())
	if !errors.Is(err, prefactor.ErrTerminated) {
		t.Fatalf("LLMCall error = %v, want ErrTerminated", err)
	}
	if !strings.Contains(err.Error(), "operator stopped this run") {
		t.Errorf("error = %v, want it to carry the platform reason", err)
	}

	// Subsequent calls keep returning ErrTerminated.
	if _, err := turn.ToolCall(ctx, "x", "{}"); !errors.Is(err, prefactor.ErrTerminated) {
		t.Errorf("ToolCall error = %v, want ErrTerminated after termination", err)
	}
	if err := tr.FinishSession(ctx, prefactor.InstanceComplete); !errors.Is(err, prefactor.ErrTerminated) {
		t.Errorf("FinishSession error = %v, want ErrTerminated after termination", err)
	}
}

func TestTracerTerminatedOnStartSession(t *testing.T) {
	f, client := newFakePF(t)
	f.setTerminate("/agent_instance/register", true)
	tr := newTestTracer(t, f, client)

	err := tr.StartSession(context.Background(), testSchema())
	if !errors.Is(err, prefactor.ErrTerminated) {
		t.Fatalf("StartSession error = %v, want ErrTerminated", err)
	}
	if !strings.Contains(err.Error(), "operator stopped this run") {
		t.Errorf("error = %v, want it to carry the platform reason", err)
	}
}

func TestTracerFinishSessionNotStarted(t *testing.T) {
	f, client := newFakePF(t)
	tr := newTestTracer(t, f, client)

	if err := tr.FinishSession(context.Background(), prefactor.InstanceComplete); err == nil {
		t.Error("FinishSession succeeded before StartSession, want error")
	}
}

func TestTracerDoubleFinishSession(t *testing.T) {
	f, client := newFakePF(t)
	tr := newTestTracer(t, f, client)

	if err := tr.StartSession(context.Background(), testSchema()); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := tr.FinishSession(context.Background(), prefactor.InstanceComplete); err != nil {
		t.Fatalf("FinishSession: %v", err)
	}
	if err := tr.FinishSession(context.Background(), prefactor.InstanceComplete); err == nil {
		t.Error("second FinishSession succeeded, want error")
	}
}

func TestTracerTurnWithoutSession(t *testing.T) {
	f, client := newFakePF(t)
	tr := newTestTracer(t, f, client)

	if _, err := tr.StartTurn(context.Background(), "hello"); err == nil {
		t.Error("StartTurn succeeded before StartSession, want error")
	}
}

// TestTracerIdempotencyKeysUnique verifies every mutating call
// (register, start, span create, span finish, instance finish) carries a
// non-empty idempotency key, and that all keys within the session are
// unique so retried creates cannot duplicate spans.
func TestTracerIdempotencyKeysUnique(t *testing.T) {
	f, client := newFakePF(t)
	tr := newTestTracer(t, f, client)
	ctx := context.Background()

	if err := tr.StartSession(ctx, testSchema()); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	turn, err := tr.StartTurn(ctx, "hello")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	ls, err := turn.LLMCall(ctx, testRequest())
	if err != nil {
		t.Fatalf("LLMCall: %v", err)
	}
	if err := ls.Complete(testResponse()); err != nil {
		t.Fatalf("LLMSpan.Complete: %v", err)
	}
	if err := turn.Complete("hi there"); err != nil {
		t.Fatalf("Turn.Complete: %v", err)
	}
	if err := tr.FinishSession(ctx, prefactor.InstanceComplete); err != nil {
		t.Fatalf("FinishSession: %v", err)
	}

	// Every mutating call must carry a non-empty, unique idempotency key.
	seen := make(map[string]string) // key -> path
	for _, c := range f.snapshot() {
		key, _ := c.Body["idempotency_key"].(string)
		switch {
		case strings.HasPrefix(c.Path, "/agent_instance/"):
			// start/finish instance
		case c.Path == "/agent_instance/register",
			c.Path == "/agent_spans",
			strings.HasPrefix(c.Path, "/agent_spans/") && strings.HasSuffix(c.Path, "/finish"):
		default:
			continue
		}
		if key == "" {
			t.Errorf("mutating call %s has no idempotency_key", c.Path)
			continue
		}
		if prev, dup := seen[key]; dup {
			t.Errorf("duplicate idempotency key %q used for %s and %s", key, prev, c.Path)
		}
		seen[key] = c.Path
	}
	// register, start, turn create, user_message create, llm create,
	// llm finish, turn finish, instance finish = 8 mutating calls.
	const wantKeys = 8
	if len(seen) != wantKeys {
		t.Errorf("recorded %d unique keys, want %d", len(seen), wantKeys)
	}
}
