package band

import (
	"encoding/json"
	"testing"
)

func TestRoomEventID(t *testing.T) {
	t.Run("flat room object", func(t *testing.T) {
		id, err := roomEventID(json.RawMessage(`{"id":"room-1","title":"Support Chat"}`))
		if err != nil {
			t.Fatalf("roomEventID error = %v, want nil", err)
		}
		if id != "room-1" {
			t.Errorf("roomEventID = %q, want %q", id, "room-1")
		}
	})

	t.Run("missing id", func(t *testing.T) {
		if _, err := roomEventID(json.RawMessage(`{"title":"no id"}`)); err == nil {
			t.Error("roomEventID error = nil for a payload without an id, want an error")
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		if _, err := roomEventID(json.RawMessage(`not json`)); err == nil {
			t.Error("roomEventID error = nil for malformed json, want an error")
		}
	})
}
