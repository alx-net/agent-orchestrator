package acp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// handleMCPExtension answers AO's claude-agent-acp MCP requests the way the
// patched bridge does.
func (a *fakeAgent) handleMCPExtension(method string, raw json.RawMessage) (any, error) {
	var params map[string]any
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mcpCalls = append(a.mcpCalls, fakeExtensionCall{method: method, params: params})
	response := map[string]any{"servers": a.mcpServers}
	if method == mcpReconnectMethod && a.mcpReconnectRefusal != "" {
		response["error"] = a.mcpReconnectRefusal
	}
	if method == mcpApplyMethod {
		response["errors"] = a.mcpApplyErrors
	}
	return response, nil
}

func (a *fakeAgent) mcpCallsOf(method string) []fakeExtensionCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []fakeExtensionCall
	for _, call := range a.mcpCalls {
		if call.method == method {
			out = append(out, call)
		}
	}
	return out
}

func TestACPMCPControlNeedsTheAdvertisedExtension(t *testing.T) {
	conv := startSubagentConversation(t, &fakeAgent{})
	if conv.Capabilities().Has(ports.ChatCapabilityMCPReload) {
		t.Fatal("a bridge without the extension must not offer MCP reload")
	}
	if _, err := conv.MCPServerStatus(context.Background()); err == nil {
		t.Fatal("status on an unpatched bridge should fail")
	}
}

func TestACPMCPStatusTranslatesTheSDKsWords(t *testing.T) {
	count := 3
	agent := &fakeAgent{mcpControl: true, mcpServers: []mcpBridgeServer{
		{Name: "linear", Status: "needs-auth", Scope: "user"},
		{Name: "sentry", Status: "connected", Scope: "user", ToolCount: &count},
		{Name: "spec", Status: "failed", Error: "exit 1"},
		{Name: "slow", Status: "pending"},
		{Name: "off", Status: "disabled"},
		{Name: "new", Status: "some-new-state"},
	}}
	conv := startSubagentConversation(t, agent)
	if !conv.Capabilities().Has(ports.ChatCapabilityMCPReload) {
		t.Fatal("the extension should grant MCP reload")
	}
	servers, err := conv.MCPServerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"linear": ports.ChatMCPStatusNeedsAuth, "sentry": ports.ChatMCPStatusReady, "spec": ports.ChatMCPStatusFailed,
		"slow": ports.ChatMCPStatusStarting, "off": ports.ChatMCPStatusDisabled, "new": "some-new-state",
	}
	for _, server := range servers {
		if want[server.Name] != server.Status {
			t.Errorf("%s = %q, want %q", server.Name, server.Status, want[server.Name])
		}
	}
	if servers[1].ToolCount == nil || *servers[1].ToolCount != 3 || servers[2].Error != "exit 1" {
		t.Fatalf("details lost: %+v", servers)
	}
}

func TestACPMCPReconnectSendsTheServerAndKeepsARefusal(t *testing.T) {
	agent := &fakeAgent{mcpControl: true, mcpReconnectRefusal: "Server status: needs-auth", mcpServers: []mcpBridgeServer{{Name: "linear", Status: "needs-auth"}}}
	conv := startSubagentConversation(t, agent)
	result, err := conv.ReconnectMCPServer(context.Background(), "linear")
	if err != nil {
		t.Fatal(err)
	}
	if result.Error != "Server status: needs-auth" || len(result.Servers) != 1 {
		t.Fatalf("result = %+v", result)
	}
	calls := agent.mcpCallsOf(mcpReconnectMethod)
	if len(calls) != 1 || calls[0].params["serverName"] != "linear" || calls[0].params["sessionId"] == "" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestACPMCPApplySendsConfigsAndRemovals(t *testing.T) {
	agent := &fakeAgent{mcpControl: true, mcpApplyErrors: map[string]string{"bad": "no route"}, mcpServers: []mcpBridgeServer{{Name: "ok", Status: "connected"}}}
	conv := startSubagentConversation(t, agent)
	result, err := conv.ApplyMCPServers(context.Background(), ports.ChatMCPChange{
		Set:    map[string]json.RawMessage{"ok": json.RawMessage(`{"type":"http","url":"https://x.test"}`)},
		Remove: []string{"gone"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Errors["bad"] != "no route" {
		t.Fatalf("errors = %v", result.Errors)
	}
	calls := agent.mcpCallsOf(mcpApplyMethod)
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	set, _ := calls[0].params["set"].(map[string]any)
	config, _ := set["ok"].(map[string]any)
	removed, _ := calls[0].params["remove"].([]any)
	if config["url"] != "https://x.test" || len(removed) != 1 || removed[0] != "gone" {
		t.Fatalf("params = %+v", calls[0].params)
	}
}

func TestACPMCPReloadReconnectsOnlyWhatIsDown(t *testing.T) {
	agent := &fakeAgent{mcpControl: true, mcpServers: []mcpBridgeServer{
		{Name: "up", Status: "connected"}, {Name: "off", Status: "disabled"}, {Name: "broken", Status: "failed"},
	}}
	conv := startSubagentConversation(t, agent)
	result, err := conv.ReloadMCPServers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Authoritative || len(result.Servers) != 3 {
		t.Fatalf("result = %+v", result)
	}
	calls := agent.mcpCallsOf(mcpReconnectMethod)
	if len(calls) != 1 || calls[0].params["serverName"] != "broken" {
		t.Fatalf("reconnected = %+v, want only the failed one", calls)
	}
}

// The report is the whole list, so a removed server disappears, and an unchanged
// reading is not written again.
func TestACPMCPStatusReportsTheWholeListWhenItChanges(t *testing.T) {
	previous := mcpStartupReportDelays
	mcpStartupReportDelays = []time.Duration{0}
	t.Cleanup(func() { mcpStartupReportDelays = previous })

	agent := &fakeAgent{mcpControl: true, mcpServers: []mcpBridgeServer{{Name: "linear", Status: "needs-auth"}}}
	conv := startSubagentConversation(t, agent)
	event := awaitMCPEvent(t, conv)
	if !event.MCPServersComplete || len(event.MCPServers) != 1 || event.MCPServers[0].Status != ports.ChatMCPStatusNeedsAuth {
		t.Fatalf("event = %+v", event)
	}

	conv.publishMCPServers(event.MCPServers)
	select {
	case again := <-conv.Events():
		if again.Kind == ports.ChatEventMCPServers {
			t.Fatalf("an unchanged reading was reported again: %+v", again)
		}
	case <-time.After(50 * time.Millisecond):
	}
}

func awaitMCPEvent(t *testing.T, conv *conversation) ports.ChatEvent {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-conv.Events():
			if event.Kind == ports.ChatEventMCPServers {
				return event
			}
		case <-deadline:
			t.Fatal("no MCP status event")
			return ports.ChatEvent{}
		}
	}
}
