package persistenthost

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func oversizedToolResult(t *testing.T) []byte {
	t.Helper()
	blob := strings.Repeat("QUJD", 5<<20) // 20 MiB of base64, like a PDF read
	frame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"_meta":     map[string]any{ACPEventIDMetaKey: "evt-1"},
			"sessionId": "s1",
			"update": map[string]any{
				"sessionUpdate": "tool_call_update",
				"toolCallId":    "toolu_1",
				"status":        "completed",
				"rawOutput":     []any{map[string]any{"type": "text", "text": blob}},
				"content": []any{
					map[string]any{"type": "content", "content": map[string]any{"type": "image", "mimeType": "image/png", "data": blob}},
				},
				"_meta": map[string]any{"claudeCode": map[string]any{"toolName": "Read", "toolResponse": blob}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return append(frame, '\n')
}

func TestSlimACPFrameKeepsTheUpdateButNotThePayload(t *testing.T) {
	frame := oversizedToolResult(t)
	slimmed := SlimACPFrame(frame)
	if len(slimmed) >= acpSlimFrameBytes {
		t.Fatalf("slimmed frame is %d bytes", len(slimmed))
	}
	if !bytes.HasSuffix(slimmed, []byte("\n")) || bytes.Count(slimmed, []byte("\n")) != 1 {
		t.Fatal("a slimmed frame must stay one newline-terminated line")
	}
	var got struct {
		Method string `json:"method"`
		Params struct {
			Meta   map[string]string `json:"_meta"`
			Update struct {
				SessionUpdate string `json:"sessionUpdate"`
				ToolCallID    string `json:"toolCallId"`
				Status        string `json:"status"`
				RawOutput     []struct {
					Text string `json:"text"`
				} `json:"rawOutput"`
				Content []struct {
					Content struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"content"`
			} `json:"update"`
		} `json:"params"`
	}
	if err := json.Unmarshal(slimmed, &got); err != nil {
		t.Fatal(err)
	}
	update := got.Params.Update
	if got.Method != "session/update" || got.Params.Meta[ACPEventIDMetaKey] != "evt-1" ||
		update.SessionUpdate != "tool_call_update" || update.ToolCallID != "toolu_1" || update.Status != "completed" {
		t.Fatalf("identity lost: %+v", got)
	}
	if !strings.HasPrefix(update.RawOutput[0].Text, "QUJD") || !strings.Contains(update.RawOutput[0].Text, "omitted from AO's view") {
		t.Fatalf("text was not cut with a note: %.40q", update.RawOutput[0].Text)
	}
	// A cut-short image would not decode, so the block becomes text.
	if block := update.Content[0].Content; block.Type != "text" || !strings.Contains(block.Text, "image omitted") {
		t.Fatalf("image block = %+v", block)
	}
}

func TestSlimACPFrameLeavesOtherFramesAlone(t *testing.T) {
	small := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"status":"completed","n":1.50}}}` + "\n")
	if got := SlimACPFrame(small); !bytes.Equal(got, small) {
		t.Fatalf("small frame changed: %s", got)
	}
	response := append([]byte(`{"jsonrpc":"2.0","id":1,"result":{"text":"`+strings.Repeat("x", 3<<20)+`"}}`), '\n')
	if got := SlimACPFrame(response); !bytes.Equal(got, response) {
		t.Fatal("a response must never be altered")
	}
	malformed := append([]byte(`{"method":"session/update",`), bytes.Repeat([]byte("x"), 3<<20)...)
	if got := SlimACPFrame(malformed); !bytes.Equal(got, malformed) {
		t.Fatal("malformed output must reach the protocol layer unchanged")
	}
}

func TestSlimACPFrameCutsOnACharacterBoundary(t *testing.T) {
	frame, err := json.Marshal(map[string]any{"method": "session/update", "params": map[string]any{"update": map[string]any{"text": strings.Repeat("é", 2<<20)}}})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(SlimACPFrame(frame), &got); err != nil {
		t.Fatal(err)
	}
	if text := got["params"].(map[string]any)["update"].(map[string]any)["text"].(string); strings.ContainsRune(text, '�') {
		t.Fatal("cut through a multi-byte character")
	}
}
