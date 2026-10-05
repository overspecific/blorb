package llm

import (
	"context"
	"encoding/json"
)

// DecisionRequest is one decision call: a state to evaluate and the
// typed questions to ask about it. All questions are evaluated in
// parallel against the same state; no question sees another's
// answer. State and questions are passed to the server verbatim.
type DecisionRequest struct {
	Model     string                      `json:"model,omitempty"`
	State     json.RawMessage             `json:"state"`
	Questions map[string]DecisionQuestion `json:"questions"`
}

// DecisionQuestion is one typed question. Type is choice, noul, or
// score; see the config package for the criteria rules.
type DecisionQuestion struct {
	Type         string          `json:"type"`
	Instructions json.RawMessage `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria,omitempty"`
}

// DecisionAnswer is one question's typed answer. Which fields are
// set depends on the question's type: choice sets Choice,
// Probabilities and Confidence; score sets Score, Probabilities and
// Confidence; noul sets Noul (the calibrated probability of yes).
// The pointer fields keep a meaningful zero (a score of 0, a noul
// of 0) distinguishable from an absent field.
type DecisionAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
	Noul          *float64           `json:"noul,omitempty"`
	Score         *float64           `json:"score,omitempty"`
}

// DecisionResponse is the outcome of one decision call: the server's
// resolved model name, one answer per question (keyed by the
// request's question names), and the reported usage.
type DecisionResponse struct {
	Model   string
	Answers map[string]DecisionAnswer
	Usage   Usage
	Stats   CallStats
}

// DeciderClient is the provider-neutral decision client seam. The
// engine depends only on this interface; the real client lives in
// internal/llm/decision and tests use fakes.
type DeciderClient interface {
	Decide(ctx context.Context, req DecisionRequest) (*DecisionResponse, error)
}
