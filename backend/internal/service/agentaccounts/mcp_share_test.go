package agentaccounts

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// sharingClaude records which folders the computer's MCP sign-ins were
// copied into.
type sharingClaude struct {
	*fakeClaude
	shared []string
}

func (c *sharingClaude) ShareMCPAuth(_ context.Context, home string) error {
	c.shared = append(c.shared, home)
	return nil
}

func TestMCPSignInsAreSharedAtLaunchOnlyWhileNoOtherAgentUsesTheAccount(t *testing.T) {
	ctx := context.Background()
	store := &memStore{
		accounts: []domain.AgentAccount{{ID: "b", Provider: domain.AgentAccountProviderClaude, Home: "/b", Email: "b@example.com"}},
		defaults: map[domain.AgentAccountProvider]string{},
		bindings: map[domain.SessionID]string{"s1": "b", "s2": "device-claude"},
	}
	claude := &sharingClaude{fakeClaude: &fakeClaude{signedInAs: map[string]string{"": "a@example.com", "/b": "b@example.com"}}}
	svc := New(store, []ports.AgentAccountProviderCLI{claude}, t.TempDir(), nil, nil)

	// Unknown activity: nothing is written into the account's credentials.
	if _, err := svc.SessionLaunchEnv(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if len(claude.shared) != 0 {
		t.Fatalf("shared without knowing who runs on the account: %v", claude.shared)
	}

	var asked []string
	busy := true
	svc.SetAgentActivity(func(_ context.Context, accountID string, except domain.SessionID) bool {
		asked = append(asked, accountID+"/"+string(except))
		return busy
	})
	if _, err := svc.SessionLaunchEnv(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if len(claude.shared) != 0 || len(asked) != 1 || asked[0] != "b/s1" {
		t.Fatalf("shared=%v asked=%v; another agent runs on b", claude.shared, asked)
	}
	busy = false
	if _, err := svc.SessionLaunchEnv(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if len(claude.shared) != 1 || claude.shared[0] != "/b" {
		t.Fatalf("shared = %v, want /b once no other agent uses it", claude.shared)
	}
	// The computer's own folder is the source, never a target.
	if _, err := svc.SessionLaunchEnv(ctx, "s2"); err != nil {
		t.Fatal(err)
	}
	if len(claude.shared) != 1 {
		t.Fatalf("shared into the computer's folder: %v", claude.shared)
	}
}
