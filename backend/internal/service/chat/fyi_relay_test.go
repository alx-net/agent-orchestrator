package chat_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	reportsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/report"
)

// `ao send` into a Chat session is the next turn, so it carries the FYIs that
// were waiting, labeled and in arrival order, and acknowledges them. A retried
// delivery is recognized by its first payload, so an FYI that arrived since is
// not marked delivered by a duplicate that recorded nothing.
func TestRelayedTurnCarriesWaitingFYIsOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.svc.SetReportCoordinator(reportsvc.NewCoordinator(reportsvc.CoordinatorDeps{Store: h.st, FYIs: h.st, Now: h.now}))
	queue := func(text string) {
		t.Helper()
		if _, err := h.st.CreateFYIMessage(ctx, domain.FYIMessage{
			TargetSessionID: testSession, SenderSessionID: "orch-1", Message: text, CreatedAt: h.now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	queue("PR #7 merged as e75f34c")
	queue("CI on main is green")

	if _, err := h.svc.RelayChatTurnWithID(ctx, testSession, "rebase onto main", "delivery-1"); err != nil {
		t.Fatalf("relay: %v", err)
	}
	sent := h.conv.sentTexts()
	if len(sent) != 1 {
		t.Fatalf("provider received %q, want one turn", sent)
	}
	merged := "[fyi] orch-1 (sent 2026-08-02 10:00 UTC; informational, no reply needed)\nPR #7 merged as e75f34c"
	if !strings.HasPrefix(sent[0], "rebase onto main\n\n<ao-worker-reports>\n") ||
		!strings.Contains(sent[0], merged) || strings.Index(sent[0], merged) > strings.Index(sent[0], "CI on main is green") {
		t.Fatalf("turn text = %q", sent[0])
	}
	if pending, err := h.st.CountPendingFYIMessages(ctx, testSession); err != nil || pending != 0 {
		t.Fatalf("pending after delivery = %d, err = %v", pending, err)
	}

	queue("staging deploy finished")
	turnID, err := h.svc.RelayChatTurnWithID(ctx, testSession, "rebase onto main", "delivery-1")
	if err != nil || turnID != "" {
		t.Fatalf("retried relay = %q, %v; want the recorded delivery", turnID, err)
	}
	if pending, err := h.st.CountPendingFYIMessages(ctx, testSession); err != nil || pending != 1 {
		t.Fatalf("pending after duplicate = %d, err = %v; want the new fyi kept", pending, err)
	}
}
