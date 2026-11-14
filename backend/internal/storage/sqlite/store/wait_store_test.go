package store_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func TestWaitStoreRoundTripsAcrossRestartAndResolvesOnce(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedProject(t, s, "ao")
	sess, err := s.CreateSession(ctx, sampleRecord("ao"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	pr := domain.Wait{
		ID: "w-1", SessionID: sess.ID, ProjectID: sess.ProjectID, Kind: domain.WaitKindPR,
		Condition: domain.WaitForAny, PRNumber: 12, PRURL: "https://github.com/o/r/pull/12", Note: "why",
		Baseline:  domain.WaitBaseline{Captured: true, HeadSHA: "abc", ChecksDone: true, ReviewsCaptured: true, ReviewIDs: []string{"r1"}, CommentIDs: []string{"c1"}},
		CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour), State: domain.WaitPending,
	}
	timer := domain.Wait{
		ID: "w-2", SessionID: sess.ID, ProjectID: sess.ProjectID, Kind: domain.WaitKindTime,
		CreatedAt: now.Add(time.Second), FireAt: now.Add(time.Hour), ExpiresAt: now.Add(time.Hour), State: domain.WaitPending,
	}
	for _, w := range []domain.Wait{pr, timer} {
		if _, err := s.CreateWait(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateWait(ctx, domain.Wait{ID: "w-bad", SessionID: sess.ID, Kind: domain.WaitKindPR, CreatedAt: now, ExpiresAt: now}); err == nil {
		t.Fatal("created a PR wait without a condition or number")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	pending, err := s.ListPendingWaits(ctx)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending = %+v err=%v", pending, err)
	}
	if got := pending[0]; got.ID != pr.ID || !reflect.DeepEqual(got.Baseline, pr.Baseline) || got.Note != "why" ||
		got.Condition != domain.WaitForAny || !got.ExpiresAt.Equal(pr.ExpiresAt) || !got.FireAt.IsZero() {
		t.Fatalf("reloaded pr wait = %+v", got)
	}
	if got := pending[1]; got.ID != timer.ID || !got.FireAt.Equal(timer.FireAt) {
		t.Fatalf("reloaded timer = %+v", got)
	}

	if err := s.RecordWaitDeliveryFailure(ctx, pr.ID, "suppressed_awaiting_user"); err != nil {
		t.Fatal(err)
	}
	resolvedAt := now.Add(time.Minute)
	if ok, err := s.ResolveWait(ctx, pr.ID, domain.WaitFired, domain.WaitOutcomeChecksPassed, "the wake", resolvedAt); err != nil || !ok {
		t.Fatalf("first resolve ok=%v err=%v", ok, err)
	}
	if ok, err := s.ResolveWait(ctx, pr.ID, domain.WaitCancelled, domain.WaitOutcomeCancelled, "", resolvedAt); err != nil || ok {
		t.Fatalf("second resolve ok=%v err=%v, want a no-op", ok, err)
	}
	got, ok, err := s.GetWait(ctx, pr.ID)
	if err != nil || !ok || got.State != domain.WaitFired || got.Message != "the wake" || got.DeliveryAttempts != 1 ||
		got.LastError != "suppressed_awaiting_user" || !got.ResolvedAt.Equal(resolvedAt) {
		t.Fatalf("resolved = %+v ok=%v err=%v", got, ok, err)
	}
	bySession, err := s.ListPendingWaitsBySession(ctx, sess.ID)
	if err != nil || len(bySession) != 1 || bySession[0].ID != timer.ID {
		t.Fatalf("pending by session = %+v err=%v", bySession, err)
	}
	if n, err := s.PruneResolvedWaits(ctx, resolvedAt.Add(time.Second)); err != nil || n != 1 {
		t.Fatalf("pruned %d err=%v", n, err)
	}
	if _, ok, _ := s.GetWait(ctx, pr.ID); ok {
		t.Fatal("pruned wait still present")
	}
}
