package voice

import (
	"encoding/json"
	"fmt"

	"github.com/overspecific/blorb/internal/llm"
)

// Message type discriminators on the Voice Agent wire.
const (
	typeSessionUpdate  = "session.update"
	typeInputAudio     = "input.audio"
	typeToolResult     = "tool.result"
	typeSessionEnd     = "session.end"
	typeSessionReady   = "session.ready"
	typeSessionUpdated = "session.updated"
	typeSessionEnded   = "session.ended"
	typeSessionError   = "session.error"
	typeSpeechStarted  = "input.speech.started"
	typeSpeechStopped  = "input.speech.stopped"
	typeUserDelta      = "transcript.user.delta"
	typeUserFinal      = "transcript.user"
	typeReplyStarted   = "reply.started"
	typeReplyAudio     = "reply.audio"
	typeAgentDelta     = "transcript.agent.delta"
	typeAgentFinal     = "transcript.agent"
	typeReplyDone      = "reply.done"
	typeToolCall       = "tool.call"
)

// audioPCM is the Voice Agent encoding for 24 kHz 16-bit little-endian mono
// PCM, the format the audio subprocesses speak.
const audioPCM = "audio/pcm"

// sessionUpdate is the first client message: it configures the session.
type sessionUpdate struct {
	Type    string        `json:"type"`
	Session sessionConfig `json:"session"`
}

// sessionConfig is the session configuration nested under session.update.
type sessionConfig struct {
	SystemPrompt string      `json:"system_prompt,omitempty"`
	Greeting     string      `json:"greeting,omitempty"`
	Tools        []toolDecl  `json:"tools"`
	Input        audioConfig `json:"input"`
	Output       audioOutput `json:"output"`
}

// audioConfig is the input stream configuration.
type audioConfig struct {
	Format audioFormat `json:"format"`
}

// audioFormat names the encoding of one direction's audio.
type audioFormat struct {
	Encoding string `json:"encoding"`
}

// audioOutput is the output stream configuration.
type audioOutput struct {
	Voice  string      `json:"voice,omitempty"`
	Format audioFormat `json:"format"`
	Volume *int        `json:"volume,omitempty"`
}

// toolDecl is one tool declaration in session.tools. It wraps llm.Tool, which
// lacks the type discriminator the Voice Agent API requires.
type toolDecl struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// toolDecls converts the registry's tool definitions into wire declarations.
func toolDecls(defs []llm.Tool) []toolDecl {
	out := make([]toolDecl, 0, len(defs))
	for _, d := range defs {
		params := d.Parameters
		if len(params) == 0 {
			params = json.RawMessage("{}")
		}
		out = append(out, toolDecl{
			Type:        "function",
			Name:        d.Name,
			Description: d.Description,
			Parameters:  params,
		})
	}
	return out
}

// inputAudio streams one base64 chunk of mic PCM up to the server.
type inputAudio struct {
	Type  string `json:"type"`
	Audio string `json:"audio"`
}

// toolResult returns one tool invocation's outcome. Result is a JSON-encoded
// string, not a nested object, as the API requires.
type toolResult struct {
	Type    string `json:"type"`
	CallID  string `json:"call_id"`
	Result  string `json:"result"`
	IsError bool   `json:"is_error,omitempty"`
}

// sessionEnd asks the server to end the session cleanly.
type sessionEnd struct {
	Type string `json:"type"`
}

// Event is one decoded server event. It is a single struct with every event's
// fields; Type selects which are populated.
type Event struct {
	Type string `json:"type"`

	// session.ready
	SessionID string `json:"session_id,omitempty"`

	// session.ended
	SessionDurationSeconds *float64 `json:"session_duration_seconds,omitempty"`
	AudioDurationSeconds   *float64 `json:"audio_duration_seconds,omitempty"`

	// session.error
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`

	// transcript.user.delta, transcript.user
	ItemID string `json:"item_id,omitempty"`
	Text   string `json:"text,omitempty"`

	// transcript.agent.delta
	Delta string `json:"delta,omitempty"`

	// transcript.agent
	Interrupted bool `json:"interrupted,omitempty"`

	// reply.started, reply.done
	ReplyID string `json:"reply_id,omitempty"`
	Status  string `json:"status,omitempty"`

	// reply.audio
	Data string `json:"data,omitempty"`

	// tool.call
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// decodeEvent decodes one server event. An empty or unknown type is an error:
// the read loop treats it as a protocol failure.
func decodeEvent(data []byte) (Event, error) {
	var ev Event
	if err := json.Unmarshal(data, &ev); err != nil {
		return Event{}, fmt.Errorf("decode voice event: %w", err)
	}
	if ev.Type == "" {
		return Event{}, fmt.Errorf("voice event has no type: %s", data)
	}
	return ev, nil
}
