package persistenthost

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

const (
	// acpSlimFrameBytes is the size above which a session update is slimmed.
	acpSlimFrameBytes = 2 << 20
	// acpSlimStringBytes is the longest string an oversized update keeps.
	acpSlimStringBytes = 64 << 10
	// acpSlimLastResortBytes applies when an update made of many strings is
	// still over the frame size after the first pass.
	acpSlimLastResortBytes = 1 << 10
)

// SlimACPFrame shrinks an oversized session/update, usually a tool result
// carrying a whole PDF or image. It only changes what AO records and shows: the
// agent receives the tool's output inside its own process, never through this
// channel. Without it one such frame exceeds the reader's limit and ends the
// connection, and replaying it on every reconnect keeps the session down.
func SlimACPFrame(frame []byte) []byte {
	if len(frame) <= acpSlimFrameBytes {
		return frame
	}
	decoder := json.NewDecoder(bytes.NewReader(frame))
	decoder.UseNumber()
	var envelope map[string]any
	if decoder.Decode(&envelope) != nil || envelope["method"] != "session/update" {
		return frame
	}
	params, ok := envelope["params"].(map[string]any)
	if !ok {
		return frame
	}
	original := params["update"]
	for _, limit := range []int{acpSlimStringBytes, acpSlimLastResortBytes} {
		params["update"] = slimACPValue(original, limit)
		var encoded bytes.Buffer
		encoder := json.NewEncoder(&encoded) // Encode ends the frame with its newline.
		encoder.SetEscapeHTML(false)
		if encoder.Encode(envelope) != nil {
			return frame
		}
		if encoded.Len() <= acpSlimFrameBytes || limit == acpSlimLastResortBytes {
			return encoded.Bytes()
		}
	}
	return frame
}

func slimACPValue(value any, limit int) any {
	switch typed := value.(type) {
	case string:
		if len(typed) <= limit {
			return typed
		}
		cut := limit
		for cut > 0 && !utf8.RuneStart(typed[cut]) {
			cut--
		}
		head := typed[:cut]
		return head + "\n[" + slimACPSize(len(typed)-len(head)) + " omitted from AO's view]"
	case []any:
		slimmed := make([]any, len(typed))
		for i, item := range typed {
			slimmed[i] = slimACPValue(item, limit)
		}
		return slimmed
	case map[string]any:
		// A binary block cut short would no longer decode, so it becomes a note.
		if kind, ok := typed["type"].(string); ok && kind != "text" {
			if data, ok := typed["data"].(string); ok && len(data) > limit {
				return map[string]any{"type": "text", "text": "[" + kind + " omitted from AO's view: " + slimACPSize(len(data)) + "]"}
			}
		}
		slimmed := make(map[string]any, len(typed))
		for key, item := range typed {
			slimmed[key] = slimACPValue(item, limit)
		}
		return slimmed
	default:
		return value
	}
}

func slimACPSize(size int) string {
	return fmt.Sprintf("%.1f MB", float64(size)/(1<<20))
}
