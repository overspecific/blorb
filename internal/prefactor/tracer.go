package prefactor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/overspecific/blorb/internal/llm"
)

// Span statuses.
const (
	StatusActive    = "active"
	StatusComplete  = "complete"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// Instance terminal statuses.
const (
	InstanceComplete  = "complete"
	InstanceFailed    = "failed"
	InstanceCancelled = "cancelled"
)

// ErrTerminated is returned by tracer calls after the platform sent
// control.terminate: someone stopped the run from the Prefactor web app.
// The platform's reason is wrapped inside a *TerminatedError; check with
// errors.Is.
var ErrTerminated = errors.New("prefactor: terminated by platform")

// TerminatedError is the error wrapping ErrTerminated, carrying the
// platform's termination reason.
type TerminatedError struct {
	Reason string
}

func (e *TerminatedError) Error() string {
	if e.Reason == "" {
		return ErrTerminated.Error()
	}
	return fmt.Sprintf("%s: %s", ErrTerminated.Error(), e.Reason)
}

func (e *TerminatedError) Is(target error) bool {
	return target == ErrTerminated
}

// TracerConfig configures a Tracer.
type TracerConfig struct {
	Client *Client
	// AgentID and EnvironmentID are optional deployment scopes.
	AgentID       string
	EnvironmentID string
	// AgentName is the blorb agent's name; it names the registered agent
	// version.
	AgentName string
	// AgentVersion names this build/deployment of the agent, e.g. an app
	// version. When empty it is derived from the agent name.
	AgentVersion string
	// Now overrides the clock for span timestamps; nil means time.Now.
	Now func() time.Time
}

// Tracer maps a blorb session onto the Prefactor model: the session is an
// agent instance, each user turn is a blorb:agent_turn span, and messages,
// LLM calls, and tool calls within it are child spans.
//
// Error model: tracing is a hard dependency. Every method returns an error
// and every error is propagated - a session does not continue untraced.
// There is no disabled state; construct a Tracer only when tracing is
// configured. Once a control.terminate is observed, ErrTerminated is
// returned from the observing call and then from every subsequent call.
//
// Span IDs are server-assigned: every create call is synchronous, so span
// handles always carry the ID from the create response.
type Tracer struct {
	client   *Client
	agentID  string
	envID    string
	agentNm  string
	agentVer string
	now      func() time.Time

	// idemSeq numbers idempotency keys; each must be unique within the
	// session so a retried create cannot duplicate a span.
	idemSeq  atomic.Uint64
	idemSalt string

	mu         sync.Mutex
	instanceID string
	terminated bool
	started    bool
	finished   bool
}

// NewTracer creates a Tracer. No network calls happen until StartSession.
func NewTracer(cfg TracerConfig) *Tracer {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	version := cfg.AgentVersion
	if version == "" {
		version = "blorb-" + cfg.AgentName
	}
	return &Tracer{
		client:   cfg.Client,
		agentID:  cfg.AgentID,
		envID:    cfg.EnvironmentID,
		agentNm:  cfg.AgentName,
		agentVer: version,
		now:      now,
		idemSalt: strconv.FormatInt(now().UnixNano(), 36),
	}
}

// nextIdempotencyKey returns a session-unique key for one mutating call.
func (t *Tracer) nextIdempotencyKey() string {
	n := t.idemSeq.Add(1)
	return "blorb-" + t.idemSalt + "-" + strconv.FormatUint(n, 36)
}

// StartSession registers the agent instance (with the span schema) and
// starts it, storing the instance ID for subsequent calls.
func (t *Tracer) StartSession(ctx context.Context, schemaVersion AgentSchemaVersion) error {
	t.mu.Lock()
	if t.terminated {
		t.mu.Unlock()
		return ErrTerminated
	}
	if t.started {
		t.mu.Unlock()
		return fmt.Errorf("prefactor: session already started")
	}
	t.mu.Unlock()

	resp, err := t.client.RegisterInstance(ctx, RegisterInstanceRequest{
		AgentID:       t.agentID,
		EnvironmentID: t.envID,
		AgentVersion: AgentVersion{
			Name:               t.agentName(),
			ExternalIdentifier: t.agentVer,
		},
		AgentSchemaVersion: &schemaVersion,
		IdempotencyKey:     t.nextIdempotencyKey(),
	})
	if err != nil {
		return fmt.Errorf("prefactor: register instance: %w", err)
	}
	if err := t.observeControl(resp.Control); err != nil {
		return err
	}
	if resp.Details.ID == "" {
		return fmt.Errorf("prefactor: register instance: response has no instance id")
	}

	startResp, err := t.client.StartInstance(ctx, resp.Details.ID, StartInstanceRequest{
		IdempotencyKey: t.nextIdempotencyKey(),
	})
	if err != nil {
		return fmt.Errorf("prefactor: start instance: %w", err)
	}
	if err := t.observeControl(startResp.Control); err != nil {
		return err
	}

	t.mu.Lock()
	t.instanceID = resp.Details.ID
	t.started = true
	t.mu.Unlock()
	return nil
}

// agentName returns the configured agent name or a fallback when unset.
func (t *Tracer) agentName() string {
	if t.agentNm != "" {
		return t.agentNm
	}
	return "blorb"
}

// observeControl converts a control response into ErrTerminated when the
// platform requested the run stop, and latches the terminated state.
func (t *Tracer) observeControl(c Control) error {
	if !c.Terminate {
		return nil
	}
	t.mu.Lock()
	t.terminated = true
	t.mu.Unlock()
	return &TerminatedError{Reason: c.Reason}
}

// Turn is the handle for one blorb:agent_turn span. Its methods create and
// finish child spans.
type Turn struct {
	tracer *Tracer
	spanID string
}

// LLMSpan is the handle for an in-flight blorb:llm span.
type LLMSpan struct {
	turn   *Turn
	spanID string
}

// ToolSpan is the handle for an in-flight blorb:tool span.
type ToolSpan struct {
	turn   *Turn
	spanID string
	name   string
}

// StartTurn creates the turn's blorb:agent_turn span (active) and a
// blorb:user_message child span recorded complete immediately.
func (t *Tracer) StartTurn(ctx context.Context, userMessage string) (*Turn, error) {
	t.mu.Lock()
	if err := t.checkLocked(); err != nil {
		t.mu.Unlock()
		return nil, err
	}
	instanceID := t.instanceID
	t.mu.Unlock()

	now := t.now()

	turnResp, err := t.client.CreateSpan(ctx, CreateSpanRequest{
		Details: SpanDetailsForCreate{
			AgentInstanceID: instanceID,
			SchemaName:      SchemaAgentTurn,
			Status:          StatusActive,
			Payload:         map[string]any{"user_message": userMessage},
			StartedAt:       ptr(now),
		},
		IdempotencyKey: t.nextIdempotencyKey(),
	})
	if err != nil {
		return nil, fmt.Errorf("prefactor: start turn: %w", err)
	}
	if err := t.observeControl(turnResp.Control); err != nil {
		return nil, err
	}

	msgResp, err := t.client.CreateSpan(ctx, CreateSpanRequest{
		Details: SpanDetailsForCreate{
			AgentInstanceID: instanceID,
			SchemaName:      SchemaUserMessage,
			Status:          StatusComplete,
			Payload:         map[string]any{"content": userMessage},
			ParentSpanID:    turnResp.Details.ID,
			StartedAt:       ptr(now),
			FinishedAt:      ptr(now),
		},
		IdempotencyKey: t.nextIdempotencyKey(),
	})
	if err != nil {
		return nil, fmt.Errorf("prefactor: record user message: %w", err)
	}
	if err := t.observeControl(msgResp.Control); err != nil {
		return nil, err
	}

	return &Turn{tracer: t, spanID: turnResp.Details.ID}, nil
}

// checkLocked validates session state under a held mu.
func (t *Tracer) checkLocked() error {
	if t.terminated {
		return ErrTerminated
	}
	if !t.started {
		return fmt.Errorf("prefactor: session not started")
	}
	return nil
}

// instanceID returns the session's instance ID, checking state first.
func (t *Tracer) currentInstanceID() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.checkLocked(); err != nil {
		return "", err
	}
	return t.instanceID, nil
}

// ptr returns a pointer to t, for optional timestamp fields.
func ptr(v time.Time) *time.Time {
	return &v
}

// LLMCall starts an active blorb:llm child span for one LLM API call.
func (tn *Turn) LLMCall(ctx context.Context, req llm.Request) (*LLMSpan, error) {
	spanID, err := tn.startChild(ctx, SchemaLLM, llmPayload(req), "start llm span")
	if err != nil {
		return nil, err
	}
	return &LLMSpan{turn: tn, spanID: spanID}, nil
}

// llmPayload builds the blorb:llm params payload from the request.
func llmPayload(req llm.Request) map[string]any {
	messages := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		msg := map[string]any{
			"role":    string(m.Role),
			"content": m.Content,
		}
		if m.Reasoning != "" {
			msg["reasoning"] = m.Reasoning
		}
		if len(m.ToolCalls) > 0 {
			calls := make([]map[string]any, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				calls = append(calls, map[string]any{
					"id":        tc.ID,
					"name":      tc.FunctionName,
					"arguments": tc.FunctionArgs,
				})
			}
			msg["tool_calls"] = calls
		}
		messages = append(messages, msg)
	}
	return map[string]any{
		"model":    req.Model,
		"messages": messages,
	}
}

// startChild creates an active child span of the turn.
func (tn *Turn) startChild(ctx context.Context, schema string, payload map[string]any, what string) (string, error) {
	instanceID, err := tn.tracer.currentInstanceID()
	if err != nil {
		return "", err
	}

	resp, err := tn.tracer.client.CreateSpan(ctx, CreateSpanRequest{
		Details: SpanDetailsForCreate{
			AgentInstanceID: instanceID,
			SchemaName:      schema,
			Status:          StatusActive,
			Payload:         payload,
			ParentSpanID:    tn.spanID,
			StartedAt:       ptr(tn.tracer.now()),
		},
		IdempotencyKey: tn.tracer.nextIdempotencyKey(),
	})
	if err != nil {
		return "", fmt.Errorf("prefactor: %s: %w", what, err)
	}
	if err := tn.tracer.observeControl(resp.Control); err != nil {
		return "", err
	}
	return resp.Details.ID, nil
}

// Complete finishes the span with the response: content, reasoning, tool
// calls, usage, and finish reason.
func (ls *LLMSpan) Complete(resp llm.Response) error {
	return ls.complete(llmResultPayload(resp), nil)
}

// Fail finishes the span as failed with the error text.
func (ls *LLMSpan) Fail(err error) error {
	return ls.complete(nil, err)
}

func (ls *LLMSpan) complete(result map[string]any, err error) error {
	return ls.turn.finishSpan(ls.spanID, result, err, "finish llm span")
}

// llmResultPayload builds the blorb:llm result payload from the response.
func llmResultPayload(resp llm.Response) map[string]any {
	toolCalls := make([]map[string]any, 0, len(resp.Message.ToolCalls))
	for _, tc := range resp.Message.ToolCalls {
		toolCalls = append(toolCalls, map[string]any{
			"id":        tc.ID,
			"name":      tc.FunctionName,
			"arguments": tc.FunctionArgs,
		})
	}
	return map[string]any{
		"content":       resp.Message.Content,
		"reasoning":     resp.Message.Reasoning,
		"tool_calls":    toolCalls,
		"usage":         usagePayload(resp.Usage),
		"finish_reason": resp.FinishReason,
		"status":        StatusComplete,
	}
}

// usagePayload builds the usage sub-payload.
func usagePayload(u llm.Usage) map[string]any {
	return map[string]any{
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
		"total_tokens":      u.TotalTokens,
	}
}

// ToolCall starts an active blorb:tool:<name> child span. args is the raw
// JSON arguments string from the tool call event, embedded as JSON when it
// parses.
func (tn *Turn) ToolCall(ctx context.Context, name string, args string) (*ToolSpan, error) {
	payload := map[string]any{"args": toolArgsValue(args)}
	spanID, err := tn.startChild(ctx, ToolSchemaName(name), payload, fmt.Sprintf("start tool %q span", name))
	if err != nil {
		return nil, err
	}
	return &ToolSpan{turn: tn, spanID: spanID, name: name}, nil
}

// OrphanToolResult records a single-shot completed blorb:tool:<name> span
// for a tool result that arrives without a preceding tool call event (the
// engine emits these for malformed tool-call arguments and when no tools
// are configured). failed marks the result as a tool failure.
func (tn *Turn) OrphanToolResult(ctx context.Context, name, output string, failed bool) error {
	instanceID, err := tn.tracer.currentInstanceID()
	if err != nil {
		return err
	}

	now := tn.tracer.now()
	status := StatusComplete
	if failed {
		status = StatusFailed
	}
	resp, err := tn.tracer.client.CreateSpan(ctx, CreateSpanRequest{
		Details: SpanDetailsForCreate{
			AgentInstanceID: instanceID,
			SchemaName:      ToolSchemaName(name),
			Status:          status,
			Payload:         map[string]any{"args": map[string]any{}},
			ResultPayload:   map[string]any{"output": output, "failed": failed},
			ParentSpanID:    tn.spanID,
			StartedAt:       ptr(now),
			FinishedAt:      ptr(now),
		},
		IdempotencyKey: tn.tracer.nextIdempotencyKey(),
	})
	if err != nil {
		return fmt.Errorf("prefactor: record tool %q result: %w", name, err)
	}
	return tn.tracer.observeControl(resp.Control)
}

// Complete finishes the tool span with its output.
func (ts *ToolSpan) Complete(output string, failed bool) error {
	return ts.complete(map[string]any{"output": output, "failed": failed}, nil)
}

// Fail finishes the tool span as failed with the error text.
func (ts *ToolSpan) Fail(err error) error {
	return ts.complete(nil, err)
}

func (ts *ToolSpan) complete(result map[string]any, err error) error {
	return ts.turn.finishSpan(ts.spanID, result, err, "finish tool span")
}

// toolArgsValue embeds a JSON string arg, decoding to raw JSON when valid
// so tool args appear as JSON objects in the payload rather than strings.
func toolArgsValue(args string) any {
	if args == "" {
		return map[string]any{}
	}
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(args), &raw); err == nil {
		return raw
	}
	return args
}

// AssistantMessage records a completed blorb:assistant_message span.
func (tn *Turn) AssistantMessage(ctx context.Context, content, reasoning string) error {
	instanceID, err := tn.tracer.currentInstanceID()
	if err != nil {
		return err
	}

	now := tn.tracer.now()
	resp, err := tn.tracer.client.CreateSpan(ctx, CreateSpanRequest{
		Details: SpanDetailsForCreate{
			AgentInstanceID: instanceID,
			SchemaName:      SchemaAssistantMessage,
			Status:          StatusComplete,
			Payload: map[string]any{
				"content":   content,
				"reasoning": reasoning,
			},
			ResultPayload: map[string]any{"content": content},
			ParentSpanID:  tn.spanID,
			StartedAt:     ptr(now),
			FinishedAt:    ptr(now),
		},
		IdempotencyKey: tn.tracer.nextIdempotencyKey(),
	})
	if err != nil {
		return fmt.Errorf("prefactor: record assistant message: %w", err)
	}
	return tn.tracer.observeControl(resp.Control)
}

// finishSpan finishes a child span of the turn with a terminal status.
func (tn *Turn) finishSpan(spanID string, result map[string]any, err error, whatFailed string) error {
	status := StatusComplete
	if err != nil {
		status = StatusFailed
		if result == nil {
			result = map[string]any{"error": err.Error()}
		}
	}

	finish := FinishSpanRequest{Status: status, ResultPayload: result, IdempotencyKey: tn.tracer.nextIdempotencyKey()}
	resp, ferr := tn.tracer.client.FinishSpan(context.Background(), spanID, finish)
	if ferr != nil {
		return fmt.Errorf("prefactor: %s: %w", whatFailed, ferr)
	}
	return tn.tracer.observeControl(resp.Control)
}

// Complete finishes the turn span as complete with the final assistant text.
func (tn *Turn) Complete(finalText string) error {
	return tn.finish(StatusComplete, map[string]any{
		"assistant_text": finalText,
		"status":         StatusComplete,
	}, "complete turn")
}

// Fail finishes the turn span as failed with the error text.
func (tn *Turn) Fail(err error) error {
	return tn.finish(StatusFailed, map[string]any{
		"error":  err.Error(),
		"status": StatusFailed,
	}, "fail turn")
}

// Cancel finishes the turn span as cancelled.
func (tn *Turn) Cancel() error {
	return tn.finish(StatusCancelled, map[string]any{
		"status": StatusCancelled,
	}, "cancel turn")
}

// finish finishes the turn span with the given status and result.
func (tn *Turn) finish(status string, result map[string]any, what string) error {
	resp, err := tn.tracer.client.FinishSpan(context.Background(), tn.spanID, FinishSpanRequest{
		Status:         status,
		ResultPayload:  result,
		IdempotencyKey: tn.tracer.nextIdempotencyKey(),
	})
	if err != nil {
		return fmt.Errorf("prefactor: %s: %w", what, err)
	}
	return tn.tracer.observeControl(resp.Control)
}

// FinishSession finishes the instance with a terminal status
// (complete/failed/cancelled).
func (t *Tracer) FinishSession(ctx context.Context, status string) error {
	t.mu.Lock()
	if t.terminated {
		t.mu.Unlock()
		return ErrTerminated
	}
	if t.finished {
		t.mu.Unlock()
		return fmt.Errorf("prefactor: session already finished")
	}
	if !t.started || t.instanceID == "" {
		t.mu.Unlock()
		return fmt.Errorf("prefactor: session not started")
	}
	t.finished = true
	instanceID := t.instanceID
	t.mu.Unlock()

	_, err := t.client.FinishInstance(ctx, instanceID, FinishInstanceRequest{
		Status:         status,
		IdempotencyKey: t.nextIdempotencyKey(),
	})
	if err != nil {
		return fmt.Errorf("prefactor: finish instance: %w", err)
	}
	return nil
}
