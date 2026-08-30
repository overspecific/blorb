// Package prefactor is a minimal, dependency-free client for the Prefactor
// agent-activity API plus the blorb tracing layer built on it.
package prefactor

import (
	"fmt"
	"time"
)

// Control is the platform's control signal carried on span and instance
// responses. Terminate being true means someone stopped the run from the
// Prefactor web app.
type Control struct {
	Terminate bool   `json:"terminate"`
	Reason    string `json:"reason,omitempty"`
}

// InstanceDetails describes a registered agent instance. ID is assigned by
// the server at registration time.
type InstanceDetails struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
}

// AgentVersion identifies the agent being registered.
type AgentVersion struct {
	Name               string         `json:"name"`
	ExternalIdentifier string         `json:"external_identifier,omitempty"`
	Description        string         `json:"description,omitempty"`
	RuntimeEnvironment map[string]any `json:"runtime_environment,omitempty"`
}

// AgentSchemaVersion declares the span types the agent emits.
type AgentSchemaVersion struct {
	ExternalIdentifier string           `json:"external_identifier"`
	SpanTypeSchemas    []SpanTypeSchema `json:"span_type_schemas,omitempty"`
}

// SpanTypeSchema is one span type in an agent schema version: its name,
// the params it carries, and optionally its result shape and render
// templates.
type SpanTypeSchema struct {
	Name         string         `json:"name"`
	ParamsSchema map[string]any `json:"params_schema,omitempty"`
	ResultSchema map[string]any `json:"result_schema,omitempty"`
	Title        string         `json:"title,omitempty"`
	Description  string         `json:"description,omitempty"`
	Template     string         `json:"template,omitempty"`
}

// RegisterInstanceRequest is the body of POST /agent_instance/register.
type RegisterInstanceRequest struct {
	AgentID            string              `json:"agent_id,omitempty"`
	EnvironmentID      string              `json:"environment_id,omitempty"`
	AgentVersion       AgentVersion        `json:"agent_version"`
	AgentSchemaVersion *AgentSchemaVersion `json:"agent_schema_version,omitempty"`
	IdempotencyKey     string              `json:"idempotency_key,omitempty"`
}

// RegisterInstanceResponse is the response of POST /agent_instance/register;
// Details.ID is the new instance ID.
type RegisterInstanceResponse struct {
	Status  string          `json:"status"`
	Details InstanceDetails `json:"details"`
}

// StartInstanceRequest is the body of POST /agent_instance/{id}/start.
// The timestamp and idempotency key are pointers so the all-optional body
// serializes as {} (never a zero-time timestamp).
type StartInstanceRequest struct {
	Timestamp      *time.Time `json:"timestamp,omitempty"`
	IdempotencyKey string     `json:"idempotency_key,omitempty"`
}

// FinishInstanceRequest is the body of POST /agent_instance/{id}/finish.
// The timestamp is a pointer so it serializes only when set.
type FinishInstanceRequest struct {
	Status         string     `json:"status"`
	Timestamp      *time.Time `json:"timestamp,omitempty"`
	IdempotencyKey string     `json:"idempotency_key,omitempty"`
}

// SpanDetailsForCreate is the details object of POST /agent_spans. The
// timestamps are pointers so they serialize only when set.
type SpanDetailsForCreate struct {
	AgentInstanceID   string         `json:"agent_instance_id"`
	SchemaName        string         `json:"schema_name"`
	Status            string         `json:"status"`
	Payload           map[string]any `json:"payload,omitempty"`
	ResultPayload     map[string]any `json:"result_payload,omitempty"`
	ParentSpanID      string         `json:"parent_span_id,omitempty"`
	StartedAt         *time.Time     `json:"started_at,omitempty"`
	FinishedAt        *time.Time     `json:"finished_at,omitempty"`
	SensitiveEncoding string         `json:"sensitive_encoding,omitempty"`
}

// CreateSpanRequest is the body of POST /agent_spans.
type CreateSpanRequest struct {
	Details        SpanDetailsForCreate `json:"details"`
	IdempotencyKey string               `json:"idempotency_key,omitempty"`
}

// SpanDetails describes a span as returned by the server; ID is assigned at
// create time.
type SpanDetails struct {
	ID              string         `json:"id"`
	AgentInstanceID string         `json:"agent_instance_id,omitempty"`
	SchemaName      string         `json:"schema_name,omitempty"`
	Status          string         `json:"status,omitempty"`
	Payload         map[string]any `json:"payload,omitempty"`
	ResultPayload   map[string]any `json:"result_payload,omitempty"`
	ParentSpanID    string         `json:"parent_span_id,omitempty"`
	StartedAt       time.Time      `json:"started_at,omitempty"`
	FinishedAt      time.Time      `json:"finished_at,omitempty"`
	CreatedAt       time.Time      `json:"created_at,omitempty"`
}

// SpanResponse is the response of span create and finish calls.
type SpanResponse struct {
	Status  string      `json:"status"`
	Control Control     `json:"control"`
	Details SpanDetails `json:"details"`
}

// FinishSpanRequest is the body of POST /agent_spans/{id}/finish. The
// timestamp is a pointer so it serializes only when set.
type FinishSpanRequest struct {
	Status         string         `json:"status"`
	ResultPayload  map[string]any `json:"result_payload,omitempty"`
	Timestamp      *time.Time     `json:"timestamp,omitempty"`
	IdempotencyKey string         `json:"idempotency_key,omitempty"`
}

// APIError is a non-2xx API response converted to an error.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
	// RetryAfter is the server's retry hint from a Retry-After or
	// retry_after_ms header, when present on a retryable response.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("prefactor: api error: status %d: %s: %s", e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("prefactor: api error: status %d: %s", e.StatusCode, e.Message)
}
