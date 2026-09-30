package band

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/overspecific/blorb/internal/logging"
)

// apiPrefix is the path prefix every Agent API endpoint sits under.
const apiPrefix = "/api/v1/agent"

// clientTimeout bounds each REST request when no client is injected.
const clientTimeout = 30 * time.Second

// Client wraps the Band Agent API: JSON over HTTP with an X-API-Key,
// the {"data": ...} response envelope, and error decoding into APIError.
type Client struct {
	restURL string
	apiKey  string
	http    *http.Client
	sink    logging.Sink
}

// NewClient builds a client against restURL using apiKey on every
// request. sink receives best-effort wire records; nil means no logging.
func NewClient(restURL, apiKey string, sink logging.Sink) *Client {
	if sink == nil {
		sink = logging.NewNop()
	}
	return &Client{
		restURL: strings.TrimSuffix(restURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: clientTimeout},
		sink:    sink,
	}
}

// APIError is a structured error the Agent API returns on a non-2xx.
type APIError struct {
	Code      string
	Message   string
	RequestID string
	Status    int
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("band api error: status %d", e.Status)
	if e.Code != "" {
		msg += " code " + e.Code
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// do issues one JSON request and decodes one response. reqBody encodes
// as the request body (nil means none); respBody receives the inner
// "data" value (nil to skip decoding). Non-2xx decodes an error envelope
// and returns *APIError.
func (c *Client) do(ctx context.Context, method, path string, reqBody, respBody any) error {
	return c.doMeta(ctx, method, path, reqBody, respBody, nil)
}

// doMeta is do with the response envelope's "metadata" object also
// decoded into metaOut when non-nil, for the cursor-paginated endpoints
// that carry next_cursor and has_more there.
func (c *Client) doMeta(ctx context.Context, method, path string, reqBody, respBody, metaOut any) error {
	fullURL := c.restURL + apiPrefix + path

	var reqReader io.Reader
	if reqBody != nil {
		body, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("encode %s %s: %w", method, path, err)
		}
		reqReader = bytes.NewReader(body)
		c.logWire("band-request", method, fullURL, body)
	} else {
		c.logWire("band-request", method, fullURL, nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, reqReader)
	if err != nil {
		return fmt.Errorf("build %s %s: %w", method, path, err)
	}
	req.Header.Set("X-API-Key", c.apiKey)
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.logWire("band-response", method, fullURL, []byte(err.Error()))
		return fmt.Errorf("%s %s: %w", method, fullURL, err)
	}
	defer resp.Body.Close()

	respBytes, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		c.logWire("band-response", method, fullURL, nil)
		return fmt.Errorf("read %s %s response: %w", method, fullURL, readErr)
	}
	c.logWire("band-response", method, fullURL, respBytes)

	if resp.StatusCode >= 300 {
		return decodeAPIError(resp.StatusCode, respBytes)
	}

	if len(respBytes) > 0 && (respBody != nil || metaOut != nil) {
		var envelope struct {
			Data     json.RawMessage `json:"data"`
			Metadata json.RawMessage `json:"metadata"`
		}
		if err := json.Unmarshal(respBytes, &envelope); err != nil {
			return fmt.Errorf("decode %s %s: %w", method, path, err)
		}
		if respBody != nil && len(envelope.Data) > 0 {
			if err := json.Unmarshal(envelope.Data, respBody); err != nil {
				return fmt.Errorf("decode %s %s data: %w", method, path, err)
			}
		}
		if metaOut != nil && len(envelope.Metadata) > 0 {
			if err := json.Unmarshal(envelope.Metadata, metaOut); err != nil {
				return fmt.Errorf("decode %s %s metadata: %w", method, path, err)
			}
		}
	}
	return nil
}

// decodeAPIError parses a non-2xx body into a typed error; the status is
// always carried, even when the body does not decode.
func decodeAPIError(status int, body []byte) error {
	apiErr := &APIError{Status: status}
	var wire struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &wire); err == nil {
		apiErr.Code = wire.Error.Code
		apiErr.Message = wire.Error.Message
		apiErr.RequestID = wire.Error.RequestID
	}
	return apiErr
}

// logWire writes one best-effort record to the sink.
func (c *Client) logWire(kind, method, url string, body []byte) {
	_ = c.sink.Write(logging.Record{
		Time:   time.Now(),
		Kind:   logging.Kind(kind),
		Method: method,
		URL:    url,
		Body:   body,
	})
}

// AgentProfile is the agent's own profile, as /me returns it.
// (Mirror of the types.go shape kept close to the endpoint for
// readability; the type itself lives in types.go.)

// Me fetches the agent's own profile. Used to validate the key at
// startup.
func (c *Client) Me(ctx context.Context) (AgentProfile, error) {
	var profile AgentProfile
	if err := c.do(ctx, http.MethodGet, "/me", nil, &profile); err != nil {
		return AgentProfile{}, err
	}
	return profile, nil
}

// ListChats returns the rooms the agent is in, following pagination
// until exhausted.
func (c *Client) ListChats(ctx context.Context) ([]ChatRoom, error) {
	var out []ChatRoom
	page := 1
	for {
		var rooms []ChatRoom
		if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/chats?page=%d&page_size=%d", page, listChatsPageSize), nil, &rooms); err != nil {
			return nil, err
		}
		out = append(out, rooms...)
		if len(rooms) < listChatsPageSize {
			return out, nil
		}
		page++
	}
}

// listChatsPageSize is how many rooms one page asks for.
const listChatsPageSize = 100

// ListParticipants returns a room's participants. The endpoint answers
// in one response; following pages are not documented, so one call
// suffices.
func (c *Client) ListParticipants(ctx context.Context, chatID string) ([]ChatParticipant, error) {
	var participants []ChatParticipant
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/chats/%s/participants", chatID), nil, &participants); err != nil {
		return nil, err
	}
	return participants, nil
}

// AddParticipant adds one participant to a room; role may be empty (the
// platform defaults to member).
func (c *Client) AddParticipant(ctx context.Context, chatID, participantID, role string) (ChatParticipant, error) {
	participant := map[string]any{"participant_id": participantID}
	if role != "" {
		participant["role"] = role
	}
	var participantOut ChatParticipant
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/chats/%s/participants", chatID),
		map[string]any{"participant": participant}, &participantOut); err != nil {
		return ChatParticipant{}, err
	}
	return participantOut, nil
}

// RemoveParticipant removes one participant from a room.
func (c *Client) RemoveParticipant(ctx context.Context, chatID, participantID string) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("/chats/%s/participants/%s", chatID, participantID), nil, nil)
}

// peersPageSize is how many peers one page asks for.
const peersPageSize = 100

// LookupPeers lists the agents and users the agent can interact with,
// following pagination until exhausted.
func (c *Client) LookupPeers(ctx context.Context) ([]Peer, error) {
	var out []Peer
	page := 1
	for {
		var peers []Peer
		if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/peers?page=%d&page_size=%d", page, peersPageSize), nil, &peers); err != nil {
			return nil, err
		}
		out = append(out, peers...)
		if len(peers) < peersPageSize {
			return out, nil
		}
		page++
	}
}

// CreateChat creates a room; either field may be empty.
func (c *Client) CreateChat(ctx context.Context, title, taskID string) (ChatRoom, error) {
	body := map[string]any{}
	chatBody := map[string]any{}
	if title != "" {
		chatBody["title"] = title
	}
	if taskID != "" {
		chatBody["task_id"] = taskID
	}
	if len(chatBody) > 0 {
		body["chat"] = chatBody
	}

	var room ChatRoom
	if err := c.do(ctx, http.MethodPost, "/chats", body, &room); err != nil {
		return ChatRoom{}, err
	}
	return room, nil
}

// NextMessage claims the next queued message for a room, or nil when the
// queue is empty (204).
func (c *Client) NextMessage(ctx context.Context, chatID string) (*ChatMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.restURL+apiPrefix+"/chats/"+chatID+"/messages/next", nil)
	if err != nil {
		return nil, fmt.Errorf("build next message request: %w", err)
	}
	req.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET next message: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read next message response: %w", err)
	}
	c.logWire("band-response", http.MethodGet, c.restURL+apiPrefix+"/chats/"+chatID+"/messages/next", respBytes)

	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode >= 300 {
		return nil, decodeAPIError(resp.StatusCode, respBytes)
	}

	// The response arrives in the same {"data": ...} envelope as every
	// other endpoint; unwrap it before decoding the message.
	var envelope struct {
		Data ChatMessage `json:"data"`
	}
	if err := json.Unmarshal(respBytes, &envelope); err != nil {
		return nil, fmt.Errorf("decode next message: %w", err)
	}
	return &envelope.Data, nil
}

// MarkProcessing tells the platform the message is being handled.
func (c *Client) MarkProcessing(ctx context.Context, chatID, messageID string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/chats/%s/messages/%s/processing", chatID, messageID), map[string]any{}, nil)
}

// MarkProcessed tells the platform the message completed.
func (c *Client) MarkProcessed(ctx context.Context, chatID, messageID string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/chats/%s/messages/%s/processed", chatID, messageID), map[string]any{}, nil)
}

// MarkFailed reports a message as failed with the error text.
func (c *Client) MarkFailed(ctx context.Context, chatID, messageID, errorText string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/chats/%s/messages/%s/failed", chatID, messageID), map[string]any{"error": errorText}, nil)
}

// SendMessage posts one message to a room with optional mentions.
func (c *Client) SendMessage(ctx context.Context, chatID, content string, mentions []Mention) (MessageSent, error) {
	mentionsWire := []Mention{}
	if mentions != nil {
		mentionsWire = mentions
	}
	reqBody := map[string]any{
		"message": map[string]any{
			"content":  content,
			"mentions": mentionsWire,
		},
	}
	var sent MessageSent
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/chats/%s/messages", chatID), reqBody, &sent); err != nil {
		return MessageSent{}, err
	}
	return sent, nil
}

// SendEvent posts one non-message event (tool_call, tool_result, thought,
// error, task, attention) to a room.
func (c *Client) SendEvent(ctx context.Context, chatID, content, messageType string, metadata json.RawMessage) error {
	event := map[string]any{
		"content":      content,
		"message_type": messageType,
	}
	if len(metadata) > 0 {
		event["metadata"] = metadata
	}
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/chats/%s/events", chatID), map[string]any{"event": event}, nil)
}

// Context fetches a room's recent messages, following the endpoint's
// cursor pagination (limit 100) until has_more is false, concatenating
// the pages oldest-first. The cursor is the opaque next_cursor from the
// response metadata, not a message id.
func (c *Client) Context(ctx context.Context, chatID string) ([]ChatMessage, error) {
	var out []ChatMessage
	cursor := ""
	for {
		params := url.Values{}
		params.Set("limit", strconv.Itoa(contextPageSize))
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var (
			messages []ChatMessage
			meta     struct {
				NextCursor string `json:"next_cursor"`
				HasMore    bool   `json:"has_more"`
			}
		)
		path := fmt.Sprintf("/chats/%s/context?%s", chatID, params.Encode())
		if err := c.doMeta(ctx, http.MethodGet, path, nil, &messages, &meta); err != nil {
			return nil, err
		}
		out = append(out, messages...)
		if !meta.HasMore || meta.NextCursor == "" {
			return out, nil
		}
		cursor = meta.NextCursor
	}
}

// contextPageSize is how many context messages one page asks for.
const contextPageSize = 100
