package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// handleSubagentExtension answers AO's claude-agent-acp subagent requests the
// way the patched bridge does.
func (a *fakeAgent) handleSubagentExtension(method string, raw json.RawMessage) (any, error) {
	var params map[string]any
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.subagentCalls = append(a.subagentCalls, fakeExtensionCall{method: method, params: params})
	err := a.subagentErr
	a.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if method == subagentStopMethod {
		return map[string]any{"ok": true}, nil
	}
	return map[string]any{"id": params["id"], "state": "queued"}, nil
}

func startSubagentConversation(t *testing.T, agent *fakeAgent) *conversation {
	t.Helper()
	driver := New(Config{
		Harness:      domain.HarnessClaudeCode,
		Capabilities: ports.ChatCapabilities{ports.ChatCapabilityStreaming: true},
		Probe:        func(context.Context) error { return nil },
		Launch:       func(context.Context, LaunchConfig) (Launch, error) { return Launch{Command: "fake"}, nil },
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	driver.useTestProcess(fakeSpawn(agent))
	opened, err := driver.Start(context.Background(), ports.ChatStartConfig{
		WorkspacePath: t.TempDir(), ProviderScopeID: "scope-1",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	return opened.(*conversation)
}

func TestACPSubagentRequestsTargetTheProviderToolCall(t *testing.T) {
	agent := &fakeAgent{subagentControl: true}
	conv := startSubagentConversation(t, agent)
	if !conv.Capabilities().Has(ports.ChatCapabilitySubagents) {
		t.Fatal("subagents capability was not derived from ACP initialize metadata")
	}
	// The route names the subagent by its activity's providerItemId, which a
	// scoped conversation derives from the provider's tool call id.
	itemID := conv.providerItemID("toolu_agent")
	if itemID == "toolu_agent" {
		t.Fatalf("provider item id %q is not scoped", itemID)
	}

	receipt, err := conv.MessageSubagent(context.Background(), itemID, ports.ChatSubagentMessage{ID: "msg-1", Text: "  Skip the tests folder. "})
	if err != nil {
		t.Fatalf("MessageSubagent: %v", err)
	}
	if receipt != (ports.ChatSubagentMessageReceipt{ID: "msg-1", State: "queued"}) {
		t.Fatalf("receipt = %#v", receipt)
	}
	if err := conv.StopSubagent(context.Background(), itemID); err != nil {
		t.Fatalf("StopSubagent: %v", err)
	}

	agent.mu.Lock()
	calls := append([]fakeExtensionCall(nil), agent.subagentCalls...)
	agent.mu.Unlock()
	sessionID := conv.ProviderConversationID()
	want := []fakeExtensionCall{
		{method: "_claude/subagent_message", params: map[string]any{
			"sessionId": sessionID, "toolUseId": "toolu_agent", "text": "Skip the tests folder.", "id": "msg-1",
		}},
		{method: "_claude/subagent_stop", params: map[string]any{"sessionId": sessionID, "toolUseId": "toolu_agent"}},
	}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Fatalf("extension calls = %v, want %v", calls, want)
	}
}

func TestACPSubagentRequestsMapBridgeRefusals(t *testing.T) {
	for _, tc := range []struct {
		name        string
		err         error
		notRunning  bool
		unsupported bool
	}{
		{name: "finished subagent", notRunning: true, err: &acpsdk.RequestError{
			Code: -32602, Message: "Invalid params: That subagent is not running.",
			Data: map[string]any{"reason": "subagentNotRunning"},
		}},
		{name: "unpatched bridge", unsupported: true, err: acpsdk.NewMethodNotFound(subagentMessageMethod)},
		{name: "other invalid params", err: &acpsdk.RequestError{Code: -32602, Message: "Invalid params"}},
		{name: "internal error", err: &acpsdk.RequestError{Code: -32603, Message: "Internal error"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &fakeAgent{subagentControl: true, subagentErr: tc.err}
			conv := startSubagentConversation(t, agent)
			_, messageErr := conv.MessageSubagent(context.Background(), "toolu_agent", ports.ChatSubagentMessage{ID: "m", Text: "hi"})
			stopErr := conv.StopSubagent(context.Background(), "toolu_agent")
			for label, err := range map[string]error{"message": messageErr, "stop": stopErr} {
				if err == nil {
					t.Fatalf("%s: want an error", label)
				}
				if errors.Is(err, ports.ErrChatSubagentNotRunning) != tc.notRunning ||
					errors.Is(err, ports.ErrChatSubagentsUnsupported) != tc.unsupported {
					t.Fatalf("%s error = %v (notRunning=%v unsupported=%v)", label, err, tc.notRunning, tc.unsupported)
				}
			}
		})
	}
}

func TestACPSubagentRequestsNeedTheAdvertisedExtension(t *testing.T) {
	agent := &fakeAgent{}
	conv := startSubagentConversation(t, agent)
	if conv.Capabilities().Has(ports.ChatCapabilitySubagents) {
		t.Fatal("subagents capability without the extension")
	}
	if _, err := conv.MessageSubagent(context.Background(), "toolu_agent", ports.ChatSubagentMessage{ID: "m", Text: "hi"}); !errors.Is(err, ports.ErrChatSubagentsUnsupported) {
		t.Fatalf("MessageSubagent error = %v", err)
	}
	if err := conv.StopSubagent(context.Background(), "toolu_agent"); !errors.Is(err, ports.ErrChatSubagentsUnsupported) {
		t.Fatalf("StopSubagent error = %v", err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.subagentCalls) != 0 {
		t.Fatalf("extension called without being advertised: %v", agent.subagentCalls)
	}
}

// The bridge reports the whole inbox on every change; AO replaces the list in
// the Agent call's detail and leaves the rest of the call alone.
func TestACPDriverMapsSubagentInboxOntoAgentCall(t *testing.T) {
	agent := &fakeAgent{subagentControl: true}
	conv := startSubagentConversation(t, agent)
	_ = nextEvent(t, conv.Events())
	conv.mu.Lock()
	conv.activeTurn = "turn-1"
	conv.mu.Unlock()
	send := func(update acpsdk.SessionUpdate) {
		t.Helper()
		if err := agent.conn.SessionUpdate(context.Background(), acpsdk.SessionNotification{
			SessionId: acpsdk.SessionId(conv.ProviderConversationID()), Update: update,
		}); err != nil {
			t.Fatalf("session update: %v", err)
		}
	}
	inbox := func(toolCallID string, entries ...any) acpsdk.SessionUpdate {
		return acpsdk.SessionUpdate{ToolCallUpdate: &acpsdk.SessionToolCallUpdate{
			SessionUpdate: "tool_call_update", ToolCallId: acpsdk.ToolCallId(toolCallID),
			Meta: map[string]any{"claudeCode": map[string]any{"subagentInbox": entries}},
		}}
	}
	inboxOf := func(event ports.ChatEvent) string {
		t.Helper()
		var detail map[string]any
		if err := json.Unmarshal(event.Detail, &detail); err != nil {
			t.Fatalf("detail: %v", err)
		}
		if detail["nestedAgent"] != true || detail["subagentType"] != "Explore" {
			t.Fatalf("agent call lost its own detail: %#v", detail)
		}
		encoded, _ := json.Marshal(detail["subagentInbox"])
		return string(encoded)
	}

	send(acpsdk.SessionUpdate{ToolCall: &acpsdk.SessionUpdateToolCall{
		SessionUpdate: "tool_call", ToolCallId: "toolu_agent", Title: "Scan the parser",
		Kind: acpsdk.ToolKindThink, Status: acpsdk.ToolCallStatusInProgress,
		RawInput: map[string]any{"description": "Scan the parser", "prompt": "Find the retry loop.", "subagent_type": "Explore"},
		Meta:     map[string]any{"claudeCode": map[string]any{"toolName": "Agent", "subagent": true}},
	}})
	started := nextEvent(t, conv.Events())
	if started.ProviderItemID != conv.providerItemID("toolu_agent") {
		t.Fatalf("agent call = %#v", started)
	}

	send(inbox("toolu_agent",
		map[string]any{"id": "m1", "text": "Skip tests.", "state": "queued", "at": float64(1767225600000)},
		map[string]any{"id": "", "text": "no id", "state": "queued", "at": float64(1)},
		map[string]any{"id": "m9", "text": "bad state", "state": "read", "at": float64(1)},
		"not an entry",
	))
	queued := nextEvent(t, conv.Events())
	if queued.Kind != ports.ChatEventActivityStarted || queued.ActivityStatus != domain.ActivityStatusRunning ||
		queued.Summary != "Scan the parser" {
		t.Fatalf("inbox event = %#v, want the running agent row unchanged", queued)
	}
	if got := inboxOf(queued); got != `[{"at":1767225600000,"id":"m1","state":"queued","text":"Skip tests."}]` {
		t.Fatalf("queued inbox = %s", got)
	}

	send(inbox("toolu_agent",
		map[string]any{"id": "m1", "text": "Skip tests.", "state": "delivered", "at": float64(1767225600000), "deliveredAt": float64(1767225603000)},
		map[string]any{"id": "m2", "text": "Too late.", "state": "dropped", "at": float64(1767225604000)},
	))
	if got := inboxOf(nextEvent(t, conv.Events())); got != `[{"at":1767225600000,"deliveredAt":1767225603000,"id":"m1","state":"delivered","text":"Skip tests."},`+
		`{"at":1767225604000,"id":"m2","state":"dropped","text":"Too late."}]` {
		t.Fatalf("replaced inbox = %s", got)
	}

	many := make([]any, 0, 25)
	for n := range 25 {
		many = append(many, map[string]any{"id": fmt.Sprintf("m%d", n), "text": "x", "state": "queued", "at": float64(n + 1)})
	}
	send(inbox("toolu_agent", many...))
	var capped []map[string]any
	if err := json.Unmarshal([]byte(inboxOf(nextEvent(t, conv.Events()))), &capped); err != nil {
		t.Fatalf("decode capped inbox: %v", err)
	}
	if len(capped) != 20 || capped[0]["id"] != "m5" || capped[19]["id"] != "m24" {
		t.Fatalf("capped inbox = %v", capped)
	}

	// An inbox report for a call this turn never saw writes nothing; the thought
	// after it is the next event.
	send(inbox("unknown-tool", map[string]any{"id": "m1", "text": "x", "state": "queued", "at": float64(1)}))
	send(acpsdk.UpdateAgentThoughtText("Checking the retry loop"))
	if next := nextEvent(t, conv.Events()); next.ActivityKind != domain.ActivityKindReasoning {
		t.Fatalf("next event = %#v, want the thought", next)
	}
}
