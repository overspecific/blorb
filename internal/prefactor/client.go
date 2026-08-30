package prefactor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultRetries is the number of retries for retryable responses and
// transport errors when Config.Retries is unset.
const DefaultRetries = 2

// Config configures a Client.
type Config struct {
	// BaseURL is the API base URL, e.g. https://app.prefactorai.com/api/v1.
	BaseURL string
	// Token is the bearer token sent as the Authorization header.
	Token string
	// HTTPClient is the HTTP client to use; nil means a default client.
	HTTPClient *http.Client
	// Retries is the number of retries on 429/503 and transport errors;
	// zero means DefaultRetries, negative means a single attempt with
	// no retries.
	Retries int
}

// Client is a minimal HTTP client for the Prefactor API surface blorb uses.
type Client struct {
	base    string
	token   string
	http    *http.Client
	retries int
}

// New creates a Client from cfg.
func New(cfg Config) *Client {
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	retries := cfg.Retries
	if retries == 0 {
		retries = DefaultRetries
	}
	if retries < 0 {
		retries = 0
	}
	return &Client{
		base:    strings.TrimRight(cfg.BaseURL, "/"),
		token:   cfg.Token,
		http:    httpClient,
		retries: retries,
	}
}

// RegisterInstance registers a new agent instance and returns it with the
// server-assigned instance ID.
func (c *Client) RegisterInstance(ctx context.Context, req RegisterInstanceRequest) (RegisterInstanceResponse, error) {
	var out RegisterInstanceResponse
	if err := c.do(ctx, http.MethodPost, "/agent_instance/register", req, &out); err != nil {
		return RegisterInstanceResponse{}, err
	}
	return out, nil
}

// StartInstance starts a registered instance, returning the details and the
// platform control signal.
func (c *Client) StartInstance(ctx context.Context, instanceID string, req StartInstanceRequest) (InstanceResponse, error) {
	var out InstanceResponse
	if err := c.do(ctx, http.MethodPost, "/agent_instance/"+instanceID+"/start", req, &out); err != nil {
		return InstanceResponse{}, err
	}
	return out, nil
}

// CreateSpan creates a span and returns the response carrying the
// server-assigned span ID and the platform control signal.
func (c *Client) CreateSpan(ctx context.Context, req CreateSpanRequest) (SpanResponse, error) {
	var out SpanResponse
	if err := c.do(ctx, http.MethodPost, "/agent_spans", req, &out); err != nil {
		return SpanResponse{}, err
	}
	return out, nil
}

// FinishSpan finishes a span. A 409 with code invalid_action (the span was
// already finished) is treated as success and returns the zero response.
func (c *Client) FinishSpan(ctx context.Context, spanID string, req FinishSpanRequest) (SpanResponse, error) {
	var out SpanResponse
	err := c.do(ctx, http.MethodPost, "/agent_spans/"+spanID+"/finish", req, &out)
	if apiErr, ok := err.(*APIError); ok && apiErr.StatusCode == http.StatusConflict && apiErr.Code == "invalid_action" {
		return SpanResponse{}, nil
	}
	if err != nil {
		return SpanResponse{}, err
	}
	return out, nil
}

// FinishInstance finishes an instance with a terminal status
// (complete/failed/cancelled).
func (c *Client) FinishInstance(ctx context.Context, instanceID string, req FinishInstanceRequest) (InstanceDetails, error) {
	var out InstanceResponse
	if err := c.do(ctx, http.MethodPost, "/agent_instance/"+instanceID+"/finish", req, &out); err != nil {
		return InstanceDetails{}, err
	}
	return out.Details, nil
}

// do sends one API call: builds the request with auth headers, sends it
// (retrying 429/503 and transport errors with linear backoff honouring
// Retry-After), decodes the response envelope into out, and converts
// non-2xx responses into an *APIError.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("prefactor: marshal request: %w", err)
		}
		payload = data
	}

	var lastErr error
	for attempt := 0; attempt <= c.retries; attempt++ {
		if attempt > 0 {
			delay := retryDelay(lastErr, attempt)
			if delay > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(delay):
				}
			}
		}

		var reqBody io.Reader
		if payload != nil {
			reqBody = bytes.NewReader(payload)
		}

		req, err := http.NewRequestWithContext(ctx, method, c.base+path, reqBody)
		if err != nil {
			return fmt.Errorf("prefactor: build request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = err
			continue
		}

		apiErr, decErr := handleResponse(resp, out)
		if decErr == nil {
			return nil
		}
		if apiErr != nil && retryableStatus(apiErr.StatusCode) {
			lastErr = apiErr
			continue
		}
		return decErr
	}
	return fmt.Errorf("prefactor: %s %s: %w", method, path, lastErr)
}

// handleResponse closes the response and either decodes the success
// envelope into out (no error) or converts a non-2xx response into an
// *APIError (returned as apiErr with decErr non-nil).
func handleResponse(resp *http.Response, out any) (apiErr *APIError, decErr error) {
	defer func() {
		if err := resp.Body.Close(); err != nil {
			// Best-effort close; body reads have already finished by now.
			_ = err
		}
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &APIError{StatusCode: resp.StatusCode}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if readErr == nil && len(data) > 0 {
			var errBody struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
				Code    string `json:"code"`
				Message string `json:"message"`
				Detail  string `json:"detail"`
				// retry_after_ms rides in the body of rate-limited
				// responses per the API docs.
				RetryAfterMS int64 `json:"retry_after_ms"`
			}
			if json.Unmarshal(data, &errBody) == nil {
				apiErr.Code = errBody.Error.Code
				apiErr.Message = errBody.Error.Message
				if apiErr.Code == "" {
					apiErr.Code = errBody.Code
				}
				if apiErr.Message == "" {
					apiErr.Message = errBody.Message
				}
				if apiErr.Message == "" {
					apiErr.Message = errBody.Detail
				}
				if apiErr.RetryAfter == 0 && errBody.RetryAfterMS > 0 && retryableStatus(resp.StatusCode) {
					apiErr.RetryAfter = time.Duration(errBody.RetryAfterMS) * time.Millisecond
				}
			}
			if apiErr.Message == "" {
				apiErr.Message = strings.TrimSpace(string(data))
			}
		}
		if apiErr.Message == "" {
			apiErr.Message = http.StatusText(resp.StatusCode)
		}
		if hint := parseRetryAfter(resp.Header.Get("Retry-After")); hint > 0 {
			apiErr.RetryAfter = hint
		}
		return apiErr, apiErr
	}

	if out == nil {
		return nil, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return nil, fmt.Errorf("prefactor: decode response: %w", err)
	}
	return nil, nil
}

// parseRetryAfter parses a Retry-After header value, supporting the
// seconds form only (HTTP-date form is ignored).
func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	secs, err := strconv.Atoi(value)
	if err != nil || secs < 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// retryableStatus reports whether a status code should be retried.
func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable
}

// retryDelay computes the backoff delay before the next attempt,
// preferring the server's retry hint over linear backoff.
func retryDelay(err error, attempt int) time.Duration {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
		return apiErr.RetryAfter
	}
	return time.Duration(attempt) * time.Second
}
