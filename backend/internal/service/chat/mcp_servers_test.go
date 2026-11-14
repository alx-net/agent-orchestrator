package chat_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// mcpControlRecorder is a conversation whose provider can change its tool
// servers in place.
type mcpControlRecorder struct {
	*fakeConversation
	mu         sync.Mutex
	reconnects []string
	applied    []ports.ChatMCPChange
	servers    []ports.ChatMCPServer
	failures   map[string]string
}

func (r *mcpControlRecorder) Capabilities() ports.ChatCapabilities {
	caps := ports.ChatCapabilities{}
	for key, value := range r.fakeConversation.Capabilities() {
		caps[key] = value
	}
	caps[ports.ChatCapabilityMCPReload] = true
	return caps
}

func (r *mcpControlRecorder) MCPServerStatus(context.Context) ([]ports.ChatMCPServer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.servers, nil
}

func (r *mcpControlRecorder) ReconnectMCPServer(_ context.Context, name string) (ports.ChatMCPReconnectResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reconnects = append(r.reconnects, name)
	return ports.ChatMCPReconnectResult{Servers: r.servers}, nil
}

func (r *mcpControlRecorder) ApplyMCPServers(_ context.Context, change ports.ChatMCPChange) (ports.ChatMCPApplyResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applied = append(r.applied, change)
	return ports.ChatMCPApplyResult{Servers: r.servers, Errors: r.failures}, nil
}

func (r *mcpControlRecorder) ReloadMCPServers(context.Context) (ports.ChatMCPReloadResult, error) {
	return ports.ChatMCPReloadResult{Servers: r.servers, Authoritative: true}, nil
}

// An agent running `ao mcp login` is mid-turn by definition, so a reconnect must
// reach it then, where a whole reload is refused.
func TestReconnectMCPServerWorksDuringATurn(t *testing.T) {
	count := 2
	recorder := &mcpControlRecorder{fakeConversation: newFakeConversation(), servers: []ports.ChatMCPServer{
		{Name: "linear", Status: ports.ChatMCPStatusReady, ToolCount: &count, Scope: "user"},
	}}
	h := newHarnessWithConversation(t, recorder)
	if _, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
		Text: "work", Origin: domain.MessageOriginHuman,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := h.svc.ReloadMCPServers(context.Background(), testSession); !errors.Is(err, chatsvc.ErrTurnRunning) {
		t.Fatalf("reload err = %v, want ErrTurnRunning", err)
	}

	servers, reason, err := h.svc.ReconnectMCPServer(context.Background(), testSession, "linear")
	if err != nil || reason != "" {
		t.Fatalf("reconnect during a turn: reason %q err %v", reason, err)
	}
	if len(servers) != 1 || servers[0].Status != ports.ChatMCPStatusReady || servers[0].ToolCount == nil || *servers[0].ToolCount != 2 {
		t.Fatalf("servers = %+v", servers)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.reconnects) != 1 || recorder.reconnects[0] != "linear" {
		t.Fatalf("reconnects = %v", recorder.reconnects)
	}
}

func TestApplyMCPServersReportsPerServerFailures(t *testing.T) {
	recorder := &mcpControlRecorder{
		fakeConversation: newFakeConversation(),
		servers:          []ports.ChatMCPServer{{Name: "ok", Status: ports.ChatMCPStatusReady}},
		failures:         map[string]string{"bad": "no route"},
	}
	h := newHarnessWithConversation(t, recorder)
	servers, failures, err := h.svc.ApplyMCPServers(context.Background(), testSession, ports.ChatMCPChange{Remove: []string{"bad"}})
	if err != nil {
		t.Fatal(err)
	}
	if failures["bad"] != "no route" || len(servers) != 1 {
		t.Fatalf("servers %+v failures %v", servers, failures)
	}
}

// The running agent is the source of truth: a server it no longer lists is gone
// from the snapshot, unlike the per-server notices other providers send.
func TestCompleteMCPReportReplacesTheList(t *testing.T) {
	count := 4
	conv := newFakeConversation()
	h := newHarnessWithConversation(t, conv)
	conv.emit(
		ports.ChatEvent{Kind: ports.ChatEventMCPServers, MCPServersComplete: true, MCPServers: []ports.ChatMCPServer{
			{Name: "linear", Status: ports.ChatMCPStatusNeedsAuth}, {Name: "gone", Status: ports.ChatMCPStatusReady},
		}},
		ports.ChatEvent{Kind: ports.ChatEventMCPServers, MCPServersComplete: true, MCPServers: []ports.ChatMCPServer{
			{Name: "linear", Status: ports.ChatMCPStatusReady, ToolCount: &count, Scope: "user"},
		}},
	)
	snapshot := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return len(s.Conversation.MCPServers) == 1 && s.Conversation.MCPServers[0].Status == ports.ChatMCPStatusReady
	})
	server := snapshot.Conversation.MCPServers[0]
	if server.Name != "linear" || server.ToolCount == nil || *server.ToolCount != 4 || server.Scope != "user" {
		t.Fatalf("server = %+v", server)
	}
}

// No controller means no running agent: its next start reads the current servers,
// so there is nothing to restart and nothing to wait for.
func TestRestartForToolServersWithNoAgentIsDone(t *testing.T) {
	h := newHarness(t)
	done, err := h.svc.RestartChatForToolServers(context.Background(), "no-such-session")
	if err != nil || !done {
		t.Fatalf("done = %v err = %v", done, err)
	}
}

// Never mid-turn: a busy agent is left alone and asked again later.
func TestRestartForToolServersWaitsForTheTurn(t *testing.T) {
	recorder := &mcpControlRecorder{fakeConversation: newFakeConversation()}
	h := newHarnessWithConversation(t, recorder)
	if _, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
		Text: "work", Origin: domain.MessageOriginHuman,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	done, err := h.svc.RestartChatForToolServers(context.Background(), testSession)
	if err != nil || done {
		t.Fatalf("restart during a turn: done = %v err = %v", done, err)
	}
}

// The agent's own MCP report saves the list under the store's writer; a request
// reading or changing the agent's servers saves it too. They took the writer and
// the controller's MCP lock in opposite orders, so an `ao mcp add` that landed
// beside a report deadlocked, and every store write in the daemon queued behind
// it: chats froze and messages could not be sent.
func TestMCPReportsAndRequestsDoNotDeadlock(t *testing.T) {
	recorder := &mcpControlRecorder{fakeConversation: newFakeConversation(), servers: []ports.ChatMCPServer{
		{Name: "hubspot", Status: ports.ChatMCPStatusNeedsAuth},
	}}
	h := newHarnessWithConversation(t, recorder)
	const rounds = 5000
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 1; i <= rounds; i++ {
				count := i
				recorder.emit(ports.ChatEvent{Kind: ports.ChatEventMCPServers, MCPServersComplete: true, MCPServers: []ports.ChatMCPServer{
					{Name: "hubspot", Status: ports.ChatMCPStatusReady, ToolCount: &count},
				}})
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				if _, err := h.svc.LiveMCPServers(context.Background(), testSession); err != nil {
					t.Errorf("LiveMCPServers: %v", err)
					return
				}
				if _, _, err := h.svc.ApplyMCPServers(context.Background(), testSession, ports.ChatMCPChange{}); err != nil {
					t.Errorf("ApplyMCPServers: %v", err)
					return
				}
			}
		}()
		wg.Wait()
	}()
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		t.Fatal("MCP reports and requests deadlocked")
	}
	// The store still takes writes.
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return len(s.Conversation.MCPServers) == 1 })
}
