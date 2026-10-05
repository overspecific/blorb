package decision_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/overspecific/blorb/internal/llm"
	"github.com/overspecific/blorb/internal/llm/decision"
	"github.com/overspecific/blorb/internal/logging"
)

func newTestClient(t *testing.T, baseURL string, opts ...func(*decision.Config)) *decision.Client {
	t.Helper()
	cfg := decision.Config{BaseURL: baseURL}
	for _, opt := range opts {
		opt(&cfg)
	}
	c, err := decision.New(cfg)
	if err != nil {
		t.Fatalf("decision.New error = %v, want nil", err)
	}
	return c
}

func sampleRequest() llm.DecisionRequest {
	return llm.DecisionRequest{
		Model: "jev",
		State: json.RawMessage(`"customer is upset"`),
		Questions: map[string]llm.DecisionQuestion{
			"priority": {
				Type:         "choice",
				Instructions: json.RawMessage(`"How urgent?"`),
				Criteria:     json.RawMessage(`{"low":"Low","high":"High"}`),
			},
			"escalate": {
				Type:         "noul",
				Instructions: json.RawMessage(`"Escalate?"`),
			},
		},
	}
}

func TestDecideRequestRoundTrip(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotReq map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Errorf("unmarshal request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "jev-1.13.0",
			"answers": {
				"priority": {"type":"choice","choice":"high","probabilities":{"low":0.1,"high":0.9},"confidence":0.8},
				"escalate": {"type":"noul","noul":0.95}
			},
			"usage": {"input_tokens": 12, "output_tokens": 5}
		}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	resp, err := c.Decide(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Decide error = %v, want nil", err)
	}

	if gotPath != "/systemone" {
		t.Errorf("request path = %q, want /systemone", gotPath)
	}
	if got, want := gotReq["model"], "jev"; got != want {
		t.Errorf("request model = %v, want %v", got, want)
	}
	if got, want := gotReq["state"], "customer is upset"; got != want {
		t.Errorf("request state = %v, want %v", got, want)
	}
	questions, ok := gotReq["questions"].(map[string]any)
	if !ok || len(questions) != 2 {
		t.Fatalf("request questions = %#v, want 2 entries", gotReq["questions"])
	}
	prio, _ := questions["priority"].(map[string]any)
	if prio["type"] != "choice" || prio["instructions"] != "How urgent?" {
		t.Errorf("priority question = %v, want choice with instructions", prio)
	}
	criteria, _ := prio["criteria"].(map[string]any)
	if criteria["low"] != "Low" || criteria["high"] != "High" {
		t.Errorf("priority criteria = %v, want the raw object", criteria)
	}

	if resp.Model != "jev-1.13.0" {
		t.Errorf("resp.Model = %q, want jev-1.13.0", resp.Model)
	}
	choice := resp.Answers["priority"]
	if choice.Type != "choice" || choice.Choice != "high" {
		t.Errorf("priority answer = %+v, want choice high", choice)
	}
	if choice.Confidence == nil || *choice.Confidence != 0.8 {
		t.Errorf("priority confidence = %v, want 0.8", choice.Confidence)
	}
	if choice.Noul != nil || choice.Score != nil {
		t.Errorf("priority noul/score = %v/%v, want nil", choice.Noul, choice.Score)
	}
	noul := resp.Answers["escalate"]
	if noul.Noul == nil || *noul.Noul != 0.95 {
		t.Errorf("escalate noul = %v, want 0.95", noul.Noul)
	}
	if noul.Confidence != nil || noul.Choice != "" {
		t.Errorf("escalate confidence/choice = %v/%q, want nil/empty", noul.Confidence, noul.Choice)
	}
	if resp.Usage.PromptTokens != 12 || resp.Usage.CompletionTokens != 5 || resp.Usage.TotalTokens != 17 {
		t.Errorf("usage = %+v, want 12/5/17", resp.Usage)
	}
	if resp.Stats.Elapsed <= 0 {
		t.Errorf("Stats.Elapsed = %v, want positive", resp.Stats.Elapsed)
	}
}

func TestDecideOmitsEmptyModel(t *testing.T) {
	t.Parallel()

	var gotReq map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Errorf("unmarshal request body: %v", err)
		}
		_, _ = w.Write([]byte(`{"model":"jev","answers":{},"usage":{}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	req := sampleRequest()
	req.Model = ""
	if _, err := c.Decide(context.Background(), req); err != nil {
		t.Fatalf("Decide error = %v, want nil", err)
	}
	if _, ok := gotReq["model"]; ok {
		t.Errorf("request model = %v, want the field omitted", gotReq["model"])
	}
}

func TestDecideTrimsTrailingSlash(t *testing.T) {
	t.Parallel()

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"model":"jev","answers":{},"usage":{}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL+"/")
	if _, err := c.Decide(context.Background(), sampleRequest()); err != nil {
		t.Fatalf("Decide error = %v, want nil", err)
	}
	if gotPath != "/systemone" {
		t.Errorf("request path = %q, want /systemone", gotPath)
	}
}

func TestDecideBearerHeader(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		key    string
		want   string
		wantOK bool
	}{
		{"with key", "secret", "Bearer secret", true},
		{"without key", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth []string
			var present bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Values("Authorization")
				present = len(gotAuth) > 0
				_, _ = w.Write([]byte(`{"model":"jev","answers":{},"usage":{}}`))
			}))
			defer srv.Close()

			c := newTestClient(t, srv.URL, func(cfg *decision.Config) { cfg.APIKey = tc.key })
			if _, err := c.Decide(context.Background(), sampleRequest()); err != nil {
				t.Fatalf("Decide error = %v, want nil", err)
			}
			if present != tc.wantOK {
				t.Fatalf("Authorization present = %v, want %v", present, tc.wantOK)
			}
			if tc.wantOK && gotAuth[0] != tc.want {
				t.Errorf("Authorization = %q, want %q", gotAuth[0], tc.want)
			}
		})
	}
}

func TestDecideScoreAnswer(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"model": "jev-1.13.0",
			"answers": {
				"frustration": {"type":"score","score":0,"probabilities":{"0":1.0},"confidence":1.0}
			},
			"usage": {}
		}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	resp, err := c.Decide(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Decide error = %v, want nil", err)
	}
	score := resp.Answers["frustration"]
	if score.Score == nil || *score.Score != 0 {
		t.Errorf("score = %v, want a meaningful zero", score.Score)
	}
}

func TestDecideErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		status   int
		body     string
		errParts []string
	}{
		{"typed", 422, `{"type":"validation_error","message":"questions is required"}`, []string{"422", "validation_error", "questions is required"}},
		{"type only", 422, `{"type":"validation_error"}`, []string{"422", "validation_error"}},
		{"message only", 422, `{"message":"bad"}`, []string{"422", "bad"}},
		{"non-json", 500, "internal failure", []string{"500", "internal failure"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := newTestClient(t, srv.URL)
			_, err := c.Decide(context.Background(), sampleRequest())
			if err == nil {
				t.Fatalf("Decide error = nil, want an error")
			}
			for _, part := range tc.errParts {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("Decide error = %q, want it to contain %q", err.Error(), part)
				}
			}
		})
	}
}

func TestDecideSinkRecords(t *testing.T) {
	t.Parallel()

	sink := logging.NewBuffered(0)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"jev","answers":{},"usage":{}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, func(cfg *decision.Config) { cfg.Sink = sink })
	if _, err := c.Decide(context.Background(), sampleRequest()); err != nil {
		t.Fatalf("Decide error = %v, want nil", err)
	}
	records := sink.Records()
	if len(records) != 2 {
		t.Fatalf("sink records = %d, want 2", len(records))
	}
	if records[0].Kind != logging.KindLLMRequest || records[0].URL == "" {
		t.Errorf("record[0] = %+v, want a request record with a URL", records[0])
	}
	if records[1].Kind != logging.KindLLMResponse {
		t.Errorf("record[1].Kind = %q, want llm-response", records[1].Kind)
	}
	if got := records[1].Headers["Status"]; len(got) != 1 || got[0] != "200" {
		t.Errorf("record[1] Status = %v, want 200", got)
	}
}

func TestNewRejectsEmptyBaseURL(t *testing.T) {
	t.Parallel()

	if _, err := decision.New(decision.Config{}); err == nil {
		t.Fatal("New(empty) error = nil, want an error")
	}
}

func TestDecideCancelledContext(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"jev","answers":{},"usage":{}}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := newTestClient(t, srv.URL)
	if _, err := c.Decide(ctx, sampleRequest()); err == nil {
		t.Fatal("Decide(cancelled) error = nil, want an error")
	}
}
