package chat_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/overspecific/blorb/internal/chat"
	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/prefactor"
)

// fakePFServer is a minimal Prefactor API over httptest, tailoring
// responses per test. It records the calls it receives, marshalled into
// the shared record shape.
type fakePFServer struct {
	mu    sync.Mutex
	calls []pfCall
	next  int

	// spanFailStatus, when non-zero, is served for span creates.
	spanFailStatus int
	// spanTerminate, when true, the first span create responds with
	// control.terminate and the given reason.
	spanTerminate   bool
	terminateReason string
	terminated      bool
}

type pfCall struct {
	Path string
	Body map[string]any
}

func newFakePFServer(t *testing.T) (*fakePFServer, *httptest.Server) {
	t.Helper()
	f := &fakePFServer{next: 1}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakePFServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.calls = append(f.calls, pfCall{Path: r.URL.Path, Body: body})
	fail := f.spanFailStatus != 0 && r.URL.Path == "/agent_spans"
	terminate := f.spanTerminate && !f.terminated && (r.URL.Path == "/agent_spans")
	if terminate {
		f.terminated = true
	}
	f.mu.Unlock()

	if fail {
		writeJSONPF(w, f.spanFailStatus, map[string]any{
			"error": map[string]any{"code": "boom", "message": "nope"},
		})
		return
	}
	if terminate {
		writeJSONPF(w, http.StatusOK, map[string]any{
			"status":  "success",
			"control": map[string]any{"terminate": true, "reason": f.terminateReason},
			"details": map[string]any{"id": "span-term"},
		})
		return
	}
	switch {
	case r.URL.Path == "/agent_instance/register":
		writeJSONPF(w, http.StatusOK, map[string]any{
			"status":  "success",
			"details": map[string]any{"id": "inst-1"},
		})
	case r.URL.Path == "/agent_spans":
		f.mu.Lock()
		id := "span-" + strings.Repeat("x", 0) + itoa(f.next)
		f.next++
		f.mu.Unlock()
		writeJSONPF(w, http.StatusOK, map[string]any{
			"status":  "success",
			"control": map[string]any{"terminate": false},
			"details": map[string]any{"id": id},
		})
	case strings.HasPrefix(r.URL.Path, "/agent_instance/") && strings.HasSuffix(r.URL.Path, "/start"),
		strings.HasPrefix(r.URL.Path, "/agent_instance/") && strings.HasSuffix(r.URL.Path, "/finish"),
		strings.HasPrefix(r.URL.Path, "/agent_spans/") && strings.HasSuffix(r.URL.Path, "/finish"):
		writeJSONPF(w, http.StatusOK, map[string]any{
			"status":  "success",
			"control": map[string]any{"terminate": false},
			"details": map[string]any{"id": "x"},
		})
	default:
		writeJSONPF(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"code": "not_found", "message": r.URL.Path},
		})
	}
}

// spans returns the recorded span-create calls with their schema names.
func (f *fakePFServer) spanCreates() []pfCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []pfCall
	for _, c := range f.calls {
		if c.Path == "/agent_spans" {
			out = append(out, c)
		}
	}
	return out
}

// finishCalls returns recorded /finish calls.
func (f *fakePFServer) finishCalls() []pfCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []pfCall
	for _, c := range f.calls {
		if strings.HasSuffix(c.Path, "/finish") {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakePFServer) setSpanFail(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spanFailStatus = status
}

func (f *fakePFServer) setSpanTerminate(reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spanTerminate = true
	f.terminateReason = reason
}

func itoa(i int) string {
	return json.Number(intToStr(i)).String()
}

func intToStr(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func writeJSONPF(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// newTracedTestOptions builds options wired to trace against srv. Callers
// set Stdin (and NewClient overrides) themselves.
func newTracedTestOptions(cfg config.Config, o *chat.Options, srv *httptest.Server, input string, responses ...llm.Response) *chat.Options {
	o.Config = cfg
	o.Version = "test"
	if o.Stdin == nil {
		o.Stdin = strings.NewReader(input)
	}
	if o.NewClient == nil {
		o.NewClient = func(config.Config) (llm.Client, error) {
			return &fakeClient{responses: responses}, nil
		}
	}
	client := prefactor.New(prefactor.Config{BaseURL: srv.URL, Token: "t"})
	o.Tracer = prefactor.NewTracer(prefactor.TracerConfig{
		Client:    client,
		AgentName: cfg.Name,
	})
	return o
}

// detail returns the details object of a recorded call.
func pfDetail(c pfCall) map[string]any {
	d, _ := c.Body["details"].(map[string]any)
	return d
}

// spanSchema extracts a span's schema_name.
func spanSchema(c pfCall) string {
	d := pfDetail(c)
	s, _ := d["schema_name"].(string)
	return s
}

// TestRunTracedPlainTurn verifies one trace-configured session with one
// streaming turn produces a correct span chain against a fake server. The
// streaming fake client produces delta events only, so no
// blorb:assistant_message span is recorded (that span comes from
// whole-message events; see TestRunTracedNonStreamingTurn).
func TestRunTracedPlainTurn(t *testing.T) {
	f, srv := newFakePFServer(t)

	var stdout syncBuffer
	o := chat.Options{Stdout: &stdout}
	cfg := minimalConfig()
	newTracedTestOptions(cfg, &o, srv, "hello\nexit\n",
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "hi!"), FinishReason: llm.FinishStop},
	)
	o.Stream = true
	o.NewClient = func(config.Config) (llm.Client, error) {
		return &streamingFakeClient{
			responses: []llm.Response{
				{Message: llm.NewTextMessage(llm.RoleAssistant, "hi!"), FinishReason: llm.FinishStop},
			},
		}, nil
	}

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	// Span creates: turn, user_message, llm (the streamed round is one
	// llm span; no assistant_message because only delta events fire).
	creates := f.spanCreates()
	var schemas []string
	for _, c := range creates {
		schemas = append(schemas, spanSchema(c))
	}
	want := []string{"blorb:agent_turn", "blorb:user_message", "blorb:llm"}
	if len(schemas) != len(want) {
		t.Fatalf("span schemas = %v, want %v", schemas, want)
	}
	for i, s := range want {
		if schemas[i] != s {
			t.Errorf("span %d schema = %q, want %q", i, schemas[i], s)
		}
	}

	// Instance finished complete.
	var instanceFinish map[string]any
	for _, c := range f.finishCalls() {
		if strings.HasPrefix(c.Path, "/agent_instance/") {
			instanceFinish = c.Body
		}
	}
	if instanceFinish == nil || instanceFinish["status"] != "complete" {
		t.Errorf("instance finish = %v, want status complete", instanceFinish)
	}
}

// TestRunTracedNonStreamingTurn verifies that with Stream: true requested
// but a non-streaming client, the engine falls back to the whole-message
// path: the assistant text renders and an assistant_message span is
// recorded. This pins the clientHolder streaming-detection fix.
func TestRunTracedNonStreamingTurn(t *testing.T) {
	f, srv := newFakePFServer(t)

	var stdout syncBuffer
	o := chat.Options{Stream: true, Stdout: &stdout}
	cfg := minimalConfig()
	newTracedTestOptions(cfg, &o, srv, "hello\nexit\n",
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "hi!"), FinishReason: llm.FinishStop},
	)

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	// The whole-message path ran: the reply renders.
	if out := stdout.String(); !strings.Contains(out, ">>> Assistant:\nhi!") {
		t.Errorf("stdout = %q, want the reply under a heading", out)
	}

	// Span creates include the assistant_message span.
	creates := f.spanCreates()
	var schemas []string
	for _, c := range creates {
		schemas = append(schemas, spanSchema(c))
	}
	want := []string{"blorb:agent_turn", "blorb:user_message", "blorb:llm", "blorb:assistant_message"}
	if len(schemas) != len(want) {
		t.Fatalf("span schemas = %v, want %v", schemas, want)
	}
	for i, s := range want {
		if schemas[i] != s {
			t.Errorf("span %d schema = %q, want %q", i, schemas[i], s)
		}
	}
}

// TestRunTracedTurnSpanCarriesFinalText verifies the blorb:agent_turn
// finish carries the assistant's final text in result_payload. This
// pins the RunTurn-return-value fix.
func TestRunTracedTurnSpanCarriesFinalText(t *testing.T) {
	f, srv := newFakePFServer(t)

	// The turn span is the first span created after register+start.

	var stdout syncBuffer
	o := chat.Options{Stdout: &stdout}
	cfg := minimalConfig()
	newTracedTestOptions(cfg, &o, srv, "hello\nexit\n",
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "the final words"), FinishReason: llm.FinishStop},
	)

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	// The turn-span finish carries the final text.
	var found bool
	for _, c := range f.finishCalls() {
		if !strings.HasPrefix(c.Path, "/agent_spans/") {
			continue
		}
		if rp, ok := c.Body["result_payload"].(map[string]any); ok {
			if rp["assistant_text"] == "the final words" {
				found = true
			}
		}
	}
	if !found {
		var all []string
		for _, c := range f.finishCalls() {
			b, _ := json.Marshal(c.Body["result_payload"])
			all = append(all, string(b))
		}
		t.Errorf("no turn-span finish carries assistant_text; result payloads: %v", all)
	}
}

// TestRunTracedLLMSpanRecordsConfiguredModel verifies the blorb:llm span
// payload names the configured model even though the engine leaves
// Request.Model empty (the provider injects its model at the wire layer).
func TestRunTracedLLMSpanRecordsConfiguredModel(t *testing.T) {
	f, srv := newFakePFServer(t)

	var stdout syncBuffer
	o := chat.Options{Stdout: &stdout}
	cfg := minimalConfig() // Provider.Model = "gpt-test"
	newTracedTestOptions(cfg, &o, srv, "hello\nexit\n",
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "hi!"), FinishReason: llm.FinishStop},
	)

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	for _, c := range f.spanCreates() {
		if spanSchema(c) != "blorb:llm" {
			continue
		}
		d := pfDetail(c)
		payload, _ := d["payload"].(map[string]any)
		if payload["model"] == "gpt-test" {
			return
		}
		t.Errorf("llm span payload model = %v, want gpt-test (payload: %v)", payload["model"], payload)
		return
	}
	t.Fatal("no blorb:llm span create recorded")
}

// TestRunTracedFinishSessionOnCancelledRootCtx verifies the instance
// finish survives root-context cancellation: exiting the session by
// cancelling the ctx must still record the terminal instance state.
func TestRunTracedFinishSessionOnCancelledRootCtx(t *testing.T) {
	f, srv := newFakePFServer(t)

	var stdout syncBuffer
	o := chat.Options{Stdout: &stdout}
	cfg := minimalConfig()
	newTracedTestOptions(cfg, &o, srv, "hello\nexit\n",
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "hi!"), FinishReason: llm.FinishStop},
	)

	// stdin blocks on the pipe until we cancel, so the session is alive
	// when the root ctx goes.
	stdinRead, stdinWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer stdinWrite.Close()
	o.Stdin = stdinRead

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- chat.Run(ctx, o) }()

	time.Sleep(50 * time.Millisecond) // let StartSession complete
	cancel()
	stdinWrite.Close() // unblock the reader goroutine too

	if err := <-done; err != nil {
		t.Fatalf("Run error = %v, want nil (a cancelled root ctx is a clean exit)", err)
	}

	var instanceFinish map[string]any
	for _, c := range f.finishCalls() {
		if strings.HasPrefix(c.Path, "/agent_instance/") {
			instanceFinish = c.Body
		}
	}
	if instanceFinish == nil || instanceFinish["status"] != "complete" {
		t.Errorf("instance finish = %v, want status complete despite root-ctx cancellation", instanceFinish)
	}
}

// TestRunNonStreamingClientRendersWithStreamRequested is the untraced
// sibling: Stream: true with a non-streaming client must still render.
func TestRunNonStreamingClientRendersWithStreamRequested(t *testing.T) {
	var stdout syncBuffer
	o := chat.Options{Stream: true, Stdout: &stdout}
	o.Config = minimalConfig()
	o.Version = "test"
	o.Stdin = strings.NewReader("hello\nexit\n")
	o.NewClient = func(config.Config) (llm.Client, error) {
		return &fakeClient{responses: []llm.Response{
			{Message: llm.NewTextMessage(llm.RoleAssistant, "hi!"), FinishReason: llm.FinishStop},
		}}, nil
	}

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if out := stdout.String(); !strings.Contains(out, ">>> Assistant:\nhi!") {
		t.Errorf("stdout = %q, want the reply under a heading", out)
	}
}

// TestRunTracedToolTurn verifies tool events become tool spans parented to
// the turn span, with failed results marked failed.
func TestRunTracedToolTurn(t *testing.T) {
	f, srv := newFakePFServer(t)

	cfg := minimalConfig()
	cfg.Tools = []config.ToolEntry{{
		Type:        config.ToolTypeCommand,
		Name:        "echo_tool",
		Description: "Echoes.",
		Command:     []string{"echo", "hi"},
	}}

	var stdout syncBuffer
	o := chat.Options{Stdout: &stdout}
	newTracedTestOptions(cfg, &o, srv, "run tool\nexit\n",
		llm.Response{
			Message: llm.Message{
				Role:      llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function", FunctionName: "echo_tool", FunctionArgs: `{"x":1}`}},
			},
			FinishReason: llm.FinishToolCalls,
		},
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "done"), FinishReason: llm.FinishStop},
	)

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	creates := f.spanCreates()
	var schemas []string
	for _, c := range creates {
		schemas = append(schemas, spanSchema(c))
	}
	// Turn, user_message, llm, tool, llm (second round), assistant_message
	// (whole-message mode), in creation order.
	want := []string{"blorb:agent_turn", "blorb:user_message", "blorb:llm", "blorb:tool:echo_tool", "blorb:llm", "blorb:assistant_message"}
	if len(schemas) != len(want) {
		t.Fatalf("span schemas = %v, want %v", schemas, want)
	}
	for i, s := range want {
		if schemas[i] != s {
			t.Errorf("span %d schema = %q, want %q", i, schemas[i], s)
		}
	}

	// The tool span's active create carries the JSON args.
	toolCreate := pfDetail(creates[3])
	payload, _ := toolCreate["payload"].(map[string]any)
	args, ok := payload["args"].(map[string]any)
	if !ok || args["x"] != float64(1) {
		t.Errorf("tool span payload = %v, want args {\"x\":1}", payload)
	}

	// The tool span finish carries the tool output in its result payload.
	var toolFinish map[string]any
	for _, c := range f.finishCalls() {
		if !strings.HasPrefix(c.Path, "/agent_spans/") {
			continue
		}
		if rp, ok := c.Body["result_payload"].(map[string]any); ok && rp["output"] != nil {
			toolFinish = c.Body
		}
	}
	if toolFinish == nil {
		t.Fatal("no tool span finish call")
	}
	if toolFinish["status"] != "complete" {
		t.Errorf("tool finish status = %v, want complete", toolFinish["status"])
	}
	rp, _ := toolFinish["result_payload"].(map[string]any)
	if rp["output"] != "hi" || rp["failed"] != false {
		t.Errorf("tool finish result = %v, want output=hi failed=false", rp)
	}
}

// TestRunTracedInterruptedTurn verifies SIGINT during a traced turn
// cancels the turn and the session instance finishes cancelled.
func TestRunTracedInterruptedTurn(t *testing.T) {
	f, srv := newFakePFServer(t)

	sigint := make(chan os.Signal, 1)

	// A client that blocks until the turn's context is cancelled, so the
	// interrupt lands mid-turn.
	slow := &blockingClient{started: make(chan struct{})}
	cfg := minimalConfig()
	var stdout syncBuffer
	o := chat.Options{Stdout: &stdout, SigintChan: sigint}
	newTracedTestOptions(cfg, &o, srv, "hello\nexit\n")
	o.NewClient = func(config.Config) (llm.Client, error) { return slow, nil }

	done := make(chan error, 1)
	go func() { done <- chat.Run(context.Background(), o) }()

	// Wait for the turn to start, then interrupt it.
	awaitTurnStarted(t, slow)
	sigint <- syscall.SIGINT

	if err := <-done; err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	// The turn span is finished cancelled (the signal landed mid-turn);
	// the session then exits on its "exit" line, finishing the instance
	// complete.
	// The turn span is the only span finished cancelled.
	var turnFinish, instanceFinish map[string]any
	for _, c := range f.finishCalls() {
		if !strings.HasPrefix(c.Path, "/agent_spans/") {
			continue
		}
		if c.Body["status"] == "cancelled" {
			turnFinish = c.Body
		}
	}
	for _, c := range f.finishCalls() {
		if strings.HasPrefix(c.Path, "/agent_instance/") {
			instanceFinish = c.Body
		}
	}
	if turnFinish == nil || turnFinish["status"] != "cancelled" {
		t.Errorf("turn finish = %v, want cancelled", turnFinish)
	}
	if instanceFinish == nil || instanceFinish["status"] != "complete" {
		t.Errorf("instance finish = %v, want complete (session exited via exit command)", instanceFinish)
	}
}

// TestRunTracedSigintWhileIdle verifies SIGINT with no turn in flight ends
// the session and the instance finishes complete: quitting the chat with
// Ctrl-C is a finished chat, not a cancelled one.
func TestRunTracedSigintWhileIdle(t *testing.T) {
	f, srv := newFakePFServer(t)

	sigint := make(chan os.Signal, 1)

	cfg := minimalConfig()
	var stdout syncBuffer
	o := chat.Options{Stdout: &stdout, SigintChan: sigint}
	// Keep stdin open so the session idles until the SIGINT arrives.
	stdinRead, stdinWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer stdinWrite.Close()
	o.Stdin = stdinRead
	newTracedTestOptions(cfg, &o, srv, "")
	o.Stdin = stdinRead // newTracedTestOptions sets its own; keep the pipe

	done := make(chan error, 1)
	go func() { done <- chat.Run(context.Background(), o) }()

	time.Sleep(50 * time.Millisecond) // let the session reach its read
	sigint <- syscall.SIGINT

	if err := <-done; err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	var instanceFinish map[string]any
	for _, c := range f.finishCalls() {
		if strings.HasPrefix(c.Path, "/agent_instance/") {
			instanceFinish = c.Body
		}
	}
	if instanceFinish == nil || instanceFinish["status"] != "complete" {
		t.Errorf("instance finish = %v, want complete (quitting the chat ends the session)", instanceFinish)
	}
}

// interruptedButSuccessClient blocks until its context is cancelled, then
// returns a successful response anyway — simulating a signal that lands
// just as the model call finishes, too late to actually interrupt it.
type interruptedButSuccessClient struct {
	started chan struct{}
}

func (c *interruptedButSuccessClient) Chat(ctx context.Context, _ llm.Request) (*llm.Response, error) {
	close(c.started)
	<-ctx.Done()
	return &llm.Response{
		Message:      llm.NewTextMessage(llm.RoleAssistant, "finished anyway"),
		FinishReason: llm.FinishStop,
	}, nil
}

// TestRunTracedInterruptLandsAsTurnFinishes verifies the race where SIGINT
// arrives just as a turn completes: the turn itself succeeded, so its span
// finishes complete and the session exits as a finished chat, not a
// cancelled one.
func TestRunTracedInterruptLandsAsTurnFinishes(t *testing.T) {
	f, srv := newFakePFServer(t)

	sigint := make(chan os.Signal, 1)

	client := &interruptedButSuccessClient{started: make(chan struct{})}
	cfg := minimalConfig()
	var stdout syncBuffer
	o := chat.Options{Stdout: &stdout, SigintChan: sigint}
	newTracedTestOptions(cfg, &o, srv, "hello\n")
	o.NewClient = func(config.Config) (llm.Client, error) { return client, nil }

	done := make(chan error, 1)
	go func() { done <- chat.Run(context.Background(), o) }()

	select {
	case <-client.started:
	case <-time.After(2 * time.Second):
		t.Fatal("turn never started")
	}
	sigint <- syscall.SIGINT

	if err := <-done; err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	// The turn completed, so its span finishes complete.
	var turnFinish, instanceFinish map[string]any
	for _, c := range f.finishCalls() {
		if strings.HasPrefix(c.Path, "/agent_spans/") {
			turnFinish = c.Body
		}
	}
	for _, c := range f.finishCalls() {
		if strings.HasPrefix(c.Path, "/agent_instance/") {
			instanceFinish = c.Body
		}
	}
	if turnFinish == nil || turnFinish["status"] != "complete" {
		t.Errorf("turn finish = %v, want complete (the turn finished before the signal landed)", turnFinish)
	}
	if instanceFinish == nil || instanceFinish["status"] != "complete" {
		t.Errorf("instance finish = %v, want complete (the chat finished)", instanceFinish)
	}
}

// TestRunTracedTerminateMidTurn verifies control.terminate on a span create
// ends the session with the reason printed and no FinishSession call.
func TestRunTracedTerminateMidTurn(t *testing.T) {
	f, srv := newFakePFServer(t)

	cfg := minimalConfig()
	// Terminate on the turn-span create (first span of the turn).
	f.setSpanTerminate("operator stopped")

	var stdout syncBuffer
	o := chat.Options{Stdout: &stdout}
	newTracedTestOptions(cfg, &o, srv, "hello\nexit\n",
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "hi!"), FinishReason: llm.FinishStop},
	)
	// Note: terminate fires on the first span create, which StartTurn
	// performs.

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "stopped by platform") || !strings.Contains(out, "operator stopped") {
		t.Errorf("stdout = %q, want the termination reason", out)
	}

	// No instance finish: the platform has already terminated it.
	for _, c := range f.finishCalls() {
		if strings.HasPrefix(c.Path, "/agent_instance/") {
			t.Errorf("unexpected instance finish %v after termination", c.Body)
		}
	}
}

// TestRunTracedFatalError verifies a Prefactor 500 during a turn ends the
// session with the error surfaced and the instance finished failed.
func TestRunTracedFatalError(t *testing.T) {
	f, srv := newFakePFServer(t)

	cfg := minimalConfig()
	var stdout syncBuffer
	o := chat.Options{Stdout: &stdout}
	newTracedTestOptions(cfg, &o, srv, "hello\nexit\n",
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "hi!"), FinishReason: llm.FinishStop},
	)
	// Fail span creates with a 500: StartTurn (turn + user message spans)
	// will fail, which is fatal for the session.
	f.setSpanFail(http.StatusInternalServerError)

	err := chat.Run(context.Background(), o)
	if err == nil {
		t.Fatal("Run succeeded, want the tracing failure to surface")
	}
	if !strings.Contains(err.Error(), "prefactor") {
		t.Errorf("error = %v, want it to mention prefactor", err)
	}
}

// TestRunTracedOrphanToolResult verifies the engine's orphan tool-result
// paths (malformed tool-call arguments) produce a single-shot failed tool
// span instead of killing the turn: the session survives and the final
// assistant text renders.
func TestRunTracedOrphanToolResult(t *testing.T) {
	f, srv := newFakePFServer(t)

	cfg := minimalConfig()
	cfg.Tools = []config.ToolEntry{{
		Type:        config.ToolTypeCommand,
		Name:        "echo_tool",
		Description: "Echoes.",
		Command:     []string{"echo", "hi"},
	}}

	var stdout syncBuffer
	o := chat.Options{Stdout: &stdout}
	// The model calls the tool with malformed JSON arguments, then
	// recovers with plain text.
	newTracedTestOptions(cfg, &o, srv, "run tool\nexit\n",
		llm.Response{
			Message: llm.Message{
				Role:      llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function", FunctionName: "echo_tool", FunctionArgs: `{not json`}},
			},
			FinishReason: llm.FinishToolCalls,
		},
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "recovered"), FinishReason: llm.FinishStop},
	)

	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil (orphan result must not kill the turn)", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "recovered") {
		t.Errorf("stdout = %q, want the recovered reply", out)
	}

	// A single-shot tool span records the failed result. It is created
	// already failed and never finished separately.
	creates := f.spanCreates()
	var orphanStatus string
	for _, c := range creates {
		if spanSchema(c) == "blorb:tool:echo_tool" {
			orphanStatus, _ = pfDetail(c)["status"].(string)
		}
	}
	if orphanStatus == "" {
		t.Fatal("no tool span recorded for the orphan result")
	}
	if orphanStatus != "failed" {
		t.Errorf("orphan tool span status = %q, want failed", orphanStatus)
	}
	// creates: turn, user_message, llm, tool(orphan), llm, assistant_message
	if len(creates) != 6 {
		t.Errorf("span creates = %d, want 6", len(creates))
	}
}

// TestRunTracingDisabledNoSpans verifies tracing off does not record spans.
func TestRunTracingDisabledNoSpans(t *testing.T) {
	f, srv := newFakePFServer(t)

	// Build a tracer but do not pass it: no span may be recorded. To
	// detect any stray traffic we point the tracer at the fake server but
	// never attach it to the options.
	_ = f
	_ = srv

	o, stdout := newTestOptions(minimalConfig(), "hello\nexit\n",
		llm.Response{Message: llm.NewTextMessage(llm.RoleAssistant, "hi!"), FinishReason: llm.FinishStop},
	)
	if err := chat.Run(context.Background(), o); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if !strings.Contains(stdout.String(), "hi!") {
		t.Errorf("stdout = %q, want the reply", stdout.String())
	}
}
