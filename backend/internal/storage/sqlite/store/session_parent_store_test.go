package store_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// The spawning session survives every session read path and later full-row
// writes, and a session without one reads back empty.
func TestSessionParentRoundTrips(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")

	parent := sampleRecord("mer")
	parent.Kind = domain.KindOrchestrator
	parent, err := s.CreateSession(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	child := sampleRecord("mer")
	child.ParentSessionID = parent.ID
	child, err = s.CreateSession(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	// A generic update must not clear the write-once parent.
	child.DisplayName = "renamed"
	if err := s.UpdateSession(ctx, child); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.GetSession(ctx, child.ID)
	if err != nil || !ok || got.ParentSessionID != parent.ID {
		t.Fatalf("child parent = %q (ok=%v err=%v), want %q", got.ParentSessionID, ok, err, parent.ID)
	}
	if got, _, _ := s.GetSession(ctx, parent.ID); got.ParentSessionID != "" {
		t.Fatalf("orchestrator parent = %q, want empty", got.ParentSessionID)
	}
	byProject, err := s.ListSessions(ctx, "mer")
	if err != nil {
		t.Fatal(err)
	}
	all, err := s.ListAllSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	live, err := s.ListLiveSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for name, list := range map[string][]domain.SessionRecord{"by project": byProject, "all": all, "live": live} {
		parents := map[domain.SessionID]domain.SessionID{}
		for _, rec := range list {
			parents[rec.ID] = rec.ParentSessionID
		}
		if parents[child.ID] != parent.ID || parents[parent.ID] != "" {
			t.Fatalf("%s parents = %v, want %s -> %s only", name, parents, child.ID, parent.ID)
		}
	}
}

func TestTaskPreparationPromotionRecordsParent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	parent, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatal(err)
	}
	prepared := sampleRecord("mer")
	prepared.IsTaskPreparation = true
	prepared.ProvisionState = domain.SessionProvisionProvisioning
	created, err := s.CreateSession(ctx, prepared)
	if err != nil {
		t.Fatal(err)
	}

	visible := sampleRecord("mer")
	visible.ParentSessionID = parent.ID
	if ok, err := s.PromoteTaskPreparation(ctx, created.ID, visible); err != nil || !ok {
		t.Fatalf("promote preparation = %v, %v", ok, err)
	}
	got, ok, err := s.GetSession(ctx, created.ID)
	if err != nil || !ok || got.ParentSessionID != parent.ID {
		t.Fatalf("promoted parent = %q (ok=%v err=%v), want %q", got.ParentSessionID, ok, err, parent.ID)
	}
}
