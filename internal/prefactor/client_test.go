package prefactor_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/overspecific/blorb/internal/prefactor"
)

// muxServer is a test helper: starts an httptest.Server running the given
// handler and returns a Client pointed at it plus the server's handlers
// call recording via the handler itself.
func newTestClient(t *testing.T, handler http.Handler) (*prefactor.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg := prefactor.Config{
		BaseURL: srv.URL,
		Token:   "test-token",
	}
	return prefactor.New(cfg), srv
}

func TestRegisterInstance(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any

	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"status": "success",
			"details": map[string]any{
				"id": "inst-123",
			},
		})
	}))

	req := prefactor.RegisterInstanceRequest{
		AgentID:      "agent-1",
		AgentVersion: prefactor.AgentVersion{Name: "blorb-helper"},
		AgentSchemaVersion: &prefactor.AgentSchemaVersion{
			ExternalIdentifier: "blorb-schema-abc",
		},
	}
	resp, err := client.RegisterInstance(context.Background(), req)
	if err != nil {
		t.Fatalf("RegisterInstance error = %v, want nil", err)
	}

	if gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer test-token")
	}
	if gotPath != "/agent_instance/register" {
		t.Errorf("path = %q, want /agent_instance/register", gotPath)
	}
	if gotBody["agent_id"] != "agent-1" {
		t.Errorf("agent_id = %v, want agent-1", gotBody["agent_id"])
	}
	av, ok := gotBody["agent_version"].(map[string]any)
	if !ok || av["name"] != "blorb-helper" {
		t.Errorf("agent_version = %v, want name blorb-helper", gotBody["agent_version"])
	}
	sv, ok := gotBody["agent_schema_version"].(map[string]any)
	if !ok || sv["external_identifier"] != "blorb-schema-abc" {
		t.Errorf("agent_schema_version = %v, want external_identifier blorb-schema-abc", gotBody["agent_schema_version"])
	}
	if resp.Details.ID != "inst-123" {
		t.Errorf("Details.ID = %q, want inst-123", resp.Details.ID)
	}
	if resp.Status != "success" {
		t.Errorf("Status = %q, want success", resp.Status)
	}
}

func TestStartInstance(t *testing.T) {
	var gotPath string
	var gotBody map[string]any

	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"status": "success",
			"details": map[string]any{
				"id":         "inst-123",
				"started_at": "2026-08-30T10:00:00Z",
			},
		})
	}))

	details, err := client.StartInstance(context.Background(), "inst-123", prefactor.StartInstanceRequest{})
	if err != nil {
		t.Fatalf("StartInstance error = %v, want nil", err)
	}

	if gotPath != "/agent_instance/inst-123/start" {
		t.Errorf("path = %q, want /agent_instance/inst-123/start", gotPath)
	}
	if len(gotBody) != 0 {
		t.Errorf("request body = %v, want empty for all-optional fields", gotBody)
	}
	if details.ID != "inst-123" {
		t.Errorf("Details.ID = %q, want inst-123", details.ID)
	}
}

func TestCreateSpan(t *testing.T) {
	var gotBody map[string]any

	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent_spans" {
			t.Errorf("path = %q, want /agent_spans", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"status": "success",
			"control": map[string]any{
				"terminate": false,
			},
			"details": map[string]any{
				"id":             "span-456",
				"schema_name":    "blorb:llm",
				"status":         "active",
				"payload":        map[string]any{"model": "gpt-4o-mini"},
				"parent_span_id": "span-1",
			},
		})
	}))

	resp, err := client.CreateSpan(context.Background(), prefactor.CreateSpanRequest{
		Details: prefactor.SpanDetailsForCreate{
			AgentInstanceID: "inst-123",
			SchemaName:      "blorb:llm",
			Status:          "active",
			Payload:         map[string]any{"model": "gpt-4o-mini"},
			ParentSpanID:    "span-1",
			StartedAt:       ptrTime(time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)),
		},
	})
	if err != nil {
		t.Fatalf("CreateSpan error = %v, want nil", err)
	}

	details, ok := gotBody["details"].(map[string]any)
	if !ok {
		t.Fatalf("details = %v, want an object", gotBody["details"])
	}
	if details["agent_instance_id"] != "inst-123" {
		t.Errorf("agent_instance_id = %v, want inst-123", details["agent_instance_id"])
	}
	if details["schema_name"] != "blorb:llm" {
		t.Errorf("schema_name = %v, want blorb:llm", details["schema_name"])
	}
	if details["status"] != "active" {
		t.Errorf("status = %v, want active", details["status"])
	}
	if details["parent_span_id"] != "span-1" {
		t.Errorf("parent_span_id = %v, want span-1", details["parent_span_id"])
	}
	if started, ok := details["started_at"].(string); !ok || started != "2026-08-30T10:00:00Z" {
		t.Errorf("started_at = %v, want 2026-08-30T10:00:00Z", details["started_at"])
	}

	if resp.Details.ID != "span-456" {
		t.Errorf("Details.ID = %q, want span-456", resp.Details.ID)
	}
	if resp.Control.Terminate {
		t.Error("Control.Terminate = true, want false")
	}
}

func TestCreateSpanTerminate(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"status": "success",
			"control": map[string]any{
				"terminate": true,
				"reason":    "stopped by operator",
			},
			"details": map[string]any{"id": "span-1"},
		})
	}))

	resp, err := client.CreateSpan(context.Background(), prefactor.CreateSpanRequest{})
	if err != nil {
		t.Fatalf("CreateSpan error = %v, want nil", err)
	}
	if !resp.Control.Terminate {
		t.Error("Control.Terminate = false, want true")
	}
	if resp.Control.Reason != "stopped by operator" {
		t.Errorf("Control.Reason = %q, want %q", resp.Control.Reason, "stopped by operator")
	}
}

func TestFinishSpan(t *testing.T) {
	var gotPath string
	var gotBody map[string]any

	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"status":  "success",
			"details": map[string]any{"id": "span-456", "status": "complete"},
		})
	}))

	resp, err := client.FinishSpan(context.Background(), "span-456", prefactor.FinishSpanRequest{
		Status:        "complete",
		ResultPayload: map[string]any{"content": "hi"},
	})
	if err != nil {
		t.Fatalf("FinishSpan error = %v, want nil", err)
	}

	if gotPath != "/agent_spans/span-456/finish" {
		t.Errorf("path = %q, want /agent_spans/span-456/finish", gotPath)
	}
	if gotBody["status"] != "complete" {
		t.Errorf("status = %v, want complete", gotBody["status"])
	}
	if resp.Details.ID != "span-456" {
		t.Errorf("Details.ID = %q, want span-456", resp.Details.ID)
	}
}

func TestFinishSpanAlreadyFinished(t *testing.T) {
	attempts := 0
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		writeJSON(t, w, http.StatusConflict, map[string]any{
			"error": map[string]any{
				"code":    "invalid_action",
				"message": "span already finished",
			},
		})
	}))

	resp, err := client.FinishSpan(context.Background(), "span-1", prefactor.FinishSpanRequest{Status: "complete"})
	if err != nil {
		t.Fatalf("FinishSpan error = %v, want nil for 409 invalid_action", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (conflict must not be retried)", attempts)
	}
	if resp.Details.ID != "" {
		t.Errorf("response = %+v, want zero value", resp)
	}
}

func TestFinishInstance(t *testing.T) {
	var gotPath string
	var gotBody map[string]any

	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"status":  "success",
			"details": map[string]any{"id": "inst-123"},
		})
	}))

	details, err := client.FinishInstance(context.Background(), "inst-123", prefactor.FinishInstanceRequest{
		Status: "complete",
	})
	if err != nil {
		t.Fatalf("FinishInstance error = %v, want nil", err)
	}

	if gotPath != "/agent_instance/inst-123/finish" {
		t.Errorf("path = %q, want /agent_instance/inst-123/finish", gotPath)
	}
	if gotBody["status"] != "complete" {
		t.Errorf("status = %v, want complete", gotBody["status"])
	}
	if details.ID != "inst-123" {
		t.Errorf("Details.ID = %q, want inst-123", details.ID)
	}
}

func TestRetry429ThenSuccess(t *testing.T) {
	var attempts int32

	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			writeJSON(t, w, http.StatusTooManyRequests, map[string]any{
				"error": map[string]any{"code": "rate_limited", "message": "slow down"},
			})
			return
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"status":  "success",
			"details": map[string]any{"id": "span-1"},
		})
	}))

	resp, err := client.CreateSpan(context.Background(), prefactor.CreateSpanRequest{})
	if err != nil {
		t.Fatalf("CreateSpan error = %v, want nil", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
	if resp.Details.ID != "span-1" {
		t.Errorf("Details.ID = %q, want span-1", resp.Details.ID)
	}
}

func TestRetry503ThenSuccess(t *testing.T) {
	var attempts int32

	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) < 3 {
			w.Header().Set("retry_after_ms", "10")
			writeJSON(t, w, http.StatusServiceUnavailable, map[string]string{})
			return
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"status":  "success",
			"details": map[string]any{"id": "span-2"},
		})
	}))

	resp, err := client.CreateSpan(context.Background(), prefactor.CreateSpanRequest{})
	if err != nil {
		t.Fatalf("CreateSpan error = %v, want nil", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
	if resp.Details.ID != "span-2" {
		t.Errorf("Details.ID = %q, want span-2", resp.Details.ID)
	}
}

func TestRetriesExhausted(t *testing.T) {
	var attempts int32

	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Retry-After", "0")
		writeJSON(t, w, http.StatusTooManyRequests, map[string]any{
			"error": map[string]any{"code": "rate_limited", "message": "slow down"},
		})
	}))

	_, err := client.CreateSpan(context.Background(), prefactor.CreateSpanRequest{})
	if err == nil {
		t.Fatal("CreateSpan succeeded, want error after retries exhausted")
	}
	if attempts != 1+prefactor.DefaultRetries {
		t.Errorf("attempts = %d, want %d", attempts, 1+prefactor.DefaultRetries)
	}
	var apiErr *prefactor.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *prefactor.APIError", err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("StatusCode = %d, want 429", apiErr.StatusCode)
	}
	if apiErr.Code != "rate_limited" {
		t.Errorf("Code = %q, want rate_limited", apiErr.Code)
	}
}

func TestNoRetryOn4xx(t *testing.T) {
	var attempts int32

	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		writeJSON(t, w, http.StatusUnauthorized, map[string]any{
			"error": map[string]any{"code": "unauthorized", "message": "bad token"},
		})
	}))

	_, err := client.CreateSpan(context.Background(), prefactor.CreateSpanRequest{})
	if err == nil {
		t.Fatal("CreateSpan succeeded, want error")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (4xx must not be retried)", attempts)
	}
	var apiErr *prefactor.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *prefactor.APIError", err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want 401", apiErr.StatusCode)
	}
	if apiErr.Code != "unauthorized" {
		t.Errorf("Code = %q, want unauthorized", apiErr.Code)
	}
	if apiErr.Message != "bad token" {
		t.Errorf("Message = %q, want bad token", apiErr.Message)
	}
}

func TestTransportErrorRetryThenSuccess(t *testing.T) {
	var attempts int32

	var handler http.HandlerFunc = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"status":  "success",
			"details": map[string]any{"id": "span-3"},
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			// Close without responding to force a transport error.
			panic(http.ErrAbortHandler)
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	client := prefactor.New(prefactor.Config{BaseURL: srv.URL, Token: "t"})
	resp, err := client.CreateSpan(context.Background(), prefactor.CreateSpanRequest{})
	if err != nil {
		t.Fatalf("CreateSpan error = %v, want nil after transport retry", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
	if resp.Details.ID != "span-3" {
		t.Errorf("Details.ID = %q, want span-3", resp.Details.ID)
	}
}

func TestContextCancellation(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		writeJSON(t, w, http.StatusOK, map[string]any{"status": "success", "details": map[string]any{}})
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := client.CreateSpan(ctx, prefactor.CreateSpanRequest{})
	if err == nil {
		t.Fatal("CreateSpan succeeded, want context error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("CreateSpan took %v, want it to return promptly on context cancellation", elapsed)
	}
}

func TestNoRetriesConfigured(t *testing.T) {
	var attempts int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Retry-After", "0")
		writeJSON(t, w, http.StatusTooManyRequests, map[string]any{
			"error": map[string]any{"code": "rate_limited", "message": "no"},
		})
	}))
	t.Cleanup(srv.Close)

	client := prefactor.New(prefactor.Config{BaseURL: srv.URL, Token: "t", Retries: -1})
	_, err := client.CreateSpan(context.Background(), prefactor.CreateSpanRequest{})
	if err == nil {
		t.Fatal("CreateSpan succeeded, want error")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 with Retries = -1 (negative means a single attempt)", attempts)
	}
}

func TestMalformedErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "this is not json")
	}))
	t.Cleanup(srv.Close)

	client := prefactor.New(prefactor.Config{BaseURL: srv.URL, Token: "t"})
	_, err := client.CreateSpan(context.Background(), prefactor.CreateSpanRequest{})
	var apiErr *prefactor.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *prefactor.APIError", err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", apiErr.StatusCode)
	}
}

func TestMalformedSuccessResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "{not json")
	}))
	t.Cleanup(srv.Close)

	client := prefactor.New(prefactor.Config{BaseURL: srv.URL, Token: "t"})
	_, err := client.CreateSpan(context.Background(), prefactor.CreateSpanRequest{})
	if err == nil {
		t.Fatal("CreateSpan succeeded, want decode error")
	}
	if !strings.Contains(err.Error(), "decode response") {
		t.Errorf("error = %v, want a decode error", err)
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func ptrTime(t time.Time) *time.Time {
	return &t
}
