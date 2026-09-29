package band

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/logging"
	"github.com/overspecific/blorb/internal/tools"
)

// ToolNames is the fixed set of Band platform tools, in ToolEntries
// declaration order.
const (
	ToolSendMessage       = "band_send_message"
	ToolSendEvent         = "band_send_event"
	ToolGetParticipants   = "band_get_participants"
	ToolAddParticipant    = "band_add_participant"
	ToolRemoveParticipant = "band_remove_participant"
	ToolLookupPeers       = "band_lookup_peers"
	ToolCreateChatroom    = "band_create_chatroom"
)

// ToolEntries returns the seven Band platform tools as config entries:
// one per tool, in a fixed order, each with a plain-English description
// and a JSON-schema args object. These are never declared in blorb.json;
// the band command prepends them to every agent's registry.
func ToolEntries() []config.ToolEntry {
	return []config.ToolEntry{
		{
			Type:        config.ToolTypeBand,
			Name:        ToolSendMessage,
			Description: "Send a chat message to the Band room. Reply by calling this tool, mentioning the participants you address.",
			Band:        ToolSendMessage,
			ArgsSchema: json.RawMessage(`{"type":"object","properties":{` +
				`"content":{"type":"string","description":"The message text."},` +
				`"mentions":{"type":"array","minItems":1,"items":{"type":"object","properties":{` +
				`"id":{"type":"string","description":"The mentioned participant's id."},` +
				`"handle":{"type":"string","description":"The mentioned participant's handle, without the @ prefix."},` +
				`"name":{"type":"string","description":"The mentioned participant's display name."}},` +
				`"additionalProperties":false}}},"required":["content","mentions"],"additionalProperties":false}`),
		},
		{
			Type:        config.ToolTypeBand,
			Name:        ToolSendEvent,
			Description: "Post a non-message event to the Band room: your status as thought, error, or task.",
			Band:        ToolSendEvent,
			ArgsSchema: json.RawMessage(`{"type":"object","properties":{` +
				`"content":{"type":"string","description":"The event text."},` +
				`"message_type":{"type":"string","enum":["thought","error","task"],` +
				`"description":"What kind of event this is."},` +
				`"metadata":{"type":"object","description":"Optional structured details."}},` +
				`"required":["content","message_type"],"additionalProperties":false}`),
		},
		{
			Type:        config.ToolTypeBand,
			Name:        ToolGetParticipants,
			Description: "List the participants of the Band room you are in.",
			Band:        ToolGetParticipants,
			ArgsSchema:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		},
		{
			Type:        config.ToolTypeBand,
			Name:        ToolAddParticipant,
			Description: "Add a participant to the Band room you are in.",
			Band:        ToolAddParticipant,
			ArgsSchema: json.RawMessage(`{"type":"object","properties":{` +
				`"participant_id":{"type":"string","description":"The participant's id (user UUID or agent id)."},` +
				`"role":{"type":"string","enum":["owner","admin","member"],` +
				`"description":"The participant's role; member by default."}},` +
				`"required":["participant_id"],"additionalProperties":false}`),
		},
		{
			Type:        config.ToolTypeBand,
			Name:        ToolRemoveParticipant,
			Description: "Remove a participant from the Band room you are in.",
			Band:        ToolRemoveParticipant,
			ArgsSchema: json.RawMessage(`{"type":"object","properties":{` +
				`"participant_id":{"type":"string","description":"The participant's id."}},` +
				`"required":["participant_id"],"additionalProperties":false}`),
		},
		{
			Type:        config.ToolTypeBand,
			Name:        ToolLookupPeers,
			Description: "List the agents and users you can interact with across Band.",
			Band:        ToolLookupPeers,
			ArgsSchema: json.RawMessage(`{"type":"object","properties":{` +
				`"page":{"type":"integer","description":"Page number, from 1."},` +
				`"page_size":{"type":"integer","description":"How many peers per page."}},` +
				`"additionalProperties":false}`),
		},
		{
			Type:        config.ToolTypeBand,
			Name:        ToolCreateChatroom,
			Description: "Create a new Band chat room; you are added as its owner.",
			Band:        ToolCreateChatroom,
			ArgsSchema: json.RawMessage(`{"type":"object","properties":{` +
				`"title":{"type":"string","description":"The new room's title."},` +
				`"task_id":{"type":"string","description":"A task to associate the room with."}},` +
				`"additionalProperties":false}`),
		},
	}
}

// NewToolExecutor builds a room-bound Band tool executor. The band
// command's room runtime calls this; it is exported so tests can build
// executors against fake clients too.
func NewToolExecutor(client *Client, roomID, agentID string, onSendMessage func()) tools.BandExecutor {
	return &toolExecutor{client: client, roomID: roomID, agentID: agentID, onSendMessage: onSendMessage}
}

// toolExecutor executes Band platform tools bound to one room: it holds
// the API client, the room the bound agent acts in, this Band agent's
// own id (for auto-joining created rooms), and a callback the room
// runtime uses to notice message sends.
type toolExecutor struct {
	client *Client
	roomID string
	// agentID is this Band agent's own id, used to add itself to rooms
	// it creates.
	agentID string
	// onSendMessage fires after each successful band_send_message so
	// the room runtime knows the LLM answered through the tool this
	// turn.
	onSendMessage func()
}

// that toolExecutor implements tools.BandExecutor at compile time.
var _ tools.BandExecutor = (*toolExecutor)(nil)

// RunBandTool dispatches one Band platform tool call. Missing required
// arguments are a failed ToolResult, not a Go error: the LLM should see
// and fix the problem. A platform failure likewise surfaces as Err: true
// carrying the server's message.
func (e *toolExecutor) RunBandTool(ctx context.Context, name string, args json.RawMessage, sink logging.Sink) (tools.ToolResult, error) {
	switch name {
	case ToolSendMessage:
		return e.sendMessage(ctx, args)
	case ToolSendEvent:
		return e.sendEvent(ctx, args)
	case ToolGetParticipants:
		return e.getParticipants(ctx)
	case ToolAddParticipant:
		return e.addParticipant(ctx, args)
	case ToolRemoveParticipant:
		return e.removeParticipant(ctx, args)
	case ToolLookupPeers:
		return e.lookupPeers(ctx, args)
	case ToolCreateChatroom:
		return e.createChatroom(ctx, args)
	default:
		return tools.ToolResult{}, fmt.Errorf("unknown band tool %q", name)
	}
}

// argsError renders a failed ToolResult explaining what is missing.
func argsError(tool, problem string) (tools.ToolResult, error) {
	return tools.ToolResult{Output: fmt.Sprintf("band tool %s: %s", tool, problem), Err: true}, nil
}

// resultJSON encodes one value as the tool's output text.
func resultJSON(v any) (tools.ToolResult, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("encode band tool result: %w", err)
	}
	return tools.ToolResult{Output: string(body)}, nil
}

func (e *toolExecutor) sendMessage(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
	var parsed struct {
		Content  string    `json:"content"`
		Mentions []Mention `json:"mentions"`
	}
	if err := json.Unmarshal(args, &parsed); err != nil {
		return argsError(ToolSendMessage, "arguments are not valid JSON: "+err.Error())
	}
	if strings.TrimSpace(parsed.Content) == "" {
		return argsError(ToolSendMessage, `arguments must include a non-empty "content" string`)
	}
	if len(parsed.Mentions) == 0 {
		return argsError(ToolSendMessage, `arguments must include at least one mention`)
	}

	_, err := e.client.SendMessage(ctx, e.roomID, parsed.Content, parsed.Mentions)
	if err != nil {
		return platformFailure(err)
	}
	if e.onSendMessage != nil {
		e.onSendMessage()
	}
	return resultJSON(map[string]any{"sent": true})
}

func (e *toolExecutor) sendEvent(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
	var parsed struct {
		Content     string          `json:"content"`
		MessageType string          `json:"message_type"`
		Metadata    json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(args, &parsed); err != nil {
		return argsError(ToolSendEvent, "arguments are not valid JSON: "+err.Error())
	}
	if strings.TrimSpace(parsed.Content) == "" {
		return argsError(ToolSendEvent, `arguments must include a non-empty "content" string`)
	}
	switch parsed.MessageType {
	case "thought", "error", "task":
	default:
		return argsError(ToolSendEvent, fmt.Sprintf("message_type %q must be one of thought, error, task", parsed.MessageType))
	}

	if err := e.client.SendEvent(ctx, e.roomID, parsed.Content, parsed.MessageType, parsed.Metadata); err != nil {
		return platformFailure(err)
	}
	return resultJSON(map[string]any{"sent": true})
}

func (e *toolExecutor) getParticipants(ctx context.Context) (tools.ToolResult, error) {
	participants, err := e.client.ListParticipants(ctx, e.roomID)
	if err != nil {
		return platformFailure(err)
	}
	return resultJSON(participants)
}

func (e *toolExecutor) addParticipant(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
	var parsed struct {
		ParticipantID string `json:"participant_id"`
		Role          string `json:"role"`
	}
	if err := json.Unmarshal(args, &parsed); err != nil {
		return argsError(ToolAddParticipant, "arguments are not valid JSON: "+err.Error())
	}
	if strings.TrimSpace(parsed.ParticipantID) == "" {
		return argsError(ToolAddParticipant, `arguments must include a non-empty "participant_id" string`)
	}

	participant, err := e.client.AddParticipant(ctx, e.roomID, parsed.ParticipantID, parsed.Role)
	if err != nil {
		return platformFailure(err)
	}
	return resultJSON(participant)
}

func (e *toolExecutor) removeParticipant(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
	var parsed struct {
		ParticipantID string `json:"participant_id"`
	}
	if err := json.Unmarshal(args, &parsed); err != nil {
		return argsError(ToolRemoveParticipant, "arguments are not valid JSON: "+err.Error())
	}
	if strings.TrimSpace(parsed.ParticipantID) == "" {
		return argsError(ToolRemoveParticipant, `arguments must include a non-empty "participant_id"`)
	}

	if err := e.client.RemoveParticipant(ctx, e.roomID, parsed.ParticipantID); err != nil {
		return platformFailure(err)
	}
	return resultJSON(map[string]any{"removed": true})
}

func (e *toolExecutor) lookupPeers(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
	peers, err := e.client.LookupPeers(ctx)
	if err != nil {
		return platformFailure(err)
	}
	return resultJSON(peers)
}

func (e *toolExecutor) createChatroom(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
	var parsed struct {
		Title  string `json:"title"`
		TaskID string `json:"task_id"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &parsed); err != nil {
			return argsError(ToolCreateChatroom, "arguments are not valid JSON: "+err.Error())
		}
	}

	room, err := e.client.CreateChat(ctx, parsed.Title, parsed.TaskID)
	if err != nil {
		return platformFailure(err)
	}
	// Auto-add this agent as the new room's owner so it can act there.
	if _, err := e.client.AddParticipant(ctx, room.ID, e.agentID, "owner"); err != nil {
		return platformFailure(err)
	}
	return resultJSON(room)
}

// platformFailure converts a rejected API call into a failed ToolResult
// carrying the server's message, so the LLM sees and can retry.
func platformFailure(err error) (tools.ToolResult, error) {
	var apiErr *APIError
	msg := err.Error()
	if asAPIError(err, &apiErr) && apiErr.Message != "" {
		msg = fmt.Sprintf("the platform rejected the call (status %d): %s", apiErr.Status, apiErr.Message)
	}
	return tools.ToolResult{Output: msg, Err: true}, nil
}

// asAPIError is errors.As for the single concrete case.
func asAPIError(err error, target **APIError) bool {
	if apiErr, ok := err.(*APIError); ok {
		*target = apiErr
		return true
	}
	return false
}
