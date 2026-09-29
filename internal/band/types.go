// Package band connects blorb to the Band platform as a remote agent:
// the Phoenix Channels socket receiving @mentions, the REST Agent API
// client replying and managing rooms, the per-room agent runtime, and the
// long-running frontend loop.
package band

import (
	"time"
)

// ChatMessage is one message in a Band room, as the API and the
// subscriptions socket carry it.
type ChatMessage struct {
	ID          string          `json:"id"`
	Content     string          `json:"content"`
	SenderID    string          `json:"sender_id"`
	SenderName  string          `json:"sender_name"`
	SenderType  string          `json:"sender_type"`
	MessageType string          `json:"message_type"`
	InsertedAt  string          `json:"inserted_at"`
	Metadata    MessageMetadata `json:"metadata"`
}

// MessageMetadata carries a message's optional extras; mentions ride
// here on inbound messages.
type MessageMetadata struct {
	Mentions []Mention `json:"mentions,omitempty"`
}

// Mention names a participant referenced by a message.
type Mention struct {
	ID     string `json:"id,omitempty"`
	Handle string `json:"handle,omitempty"`
	Name   string `json:"name,omitempty"`
}

// ChatRoom is one Band chat room.
type ChatRoom struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	TaskID     string `json:"task_id"`
	InsertedAt string `json:"inserted_at"`
	UpdatedAt  string `json:"updated_at"`
}

// ChatParticipant is one participant of a room.
type ChatParticipant struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Role   string `json:"role"`
	Status string `json:"status"`
	Handle string `json:"handle"`
	Name   string `json:"name"`
}

// Peer is one agent or user reachable for lookup, as the peers endpoint
// lists them.
type Peer struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Handle string `json:"handle"`
	Type   string `json:"type"`
}

// AgentProfile is the agent's own profile, as /me returns it.
type AgentProfile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// MessageSent is the response to a sent message.
type MessageSent struct {
	Message ChatMessage `json:"message"`
}

// heartBeatIntervalDefault is how often the socket sends a heartbeat
// when no override is set.
const heartBeatIntervalDefault = 30 * time.Second
