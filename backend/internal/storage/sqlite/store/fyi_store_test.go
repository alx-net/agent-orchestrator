package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// FYIs are durable: they survive a daemon restart, are claimed in arrival
// order, and a claim a crashed daemon held goes back to pending, never lost.
func TestFYIStoreKeepsMessagesUntilAcknowledged(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedProject(t, s, "ao")
	target, err := s.CreateSession(ctx, sampleRecord("ao"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, text := range []string{"PR #7 merged", "CI on main is green"} {
		// Same timestamp on purpose: arrival order must not depend on the clock.
		if _, err := s.CreateFYIMessage(ctx, domain.FYIMessage{
			TargetSessionID: target.ID, SenderSessionID: "ao-orchestrator", Message: text, CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateFYIMessage(ctx, domain.FYIMessage{TargetSessionID: target.ID, Message: "  ", CreatedAt: now}); !errors.Is(err, domain.ErrInvalidFYIMessage) {
		t.Fatalf("blank fyi err = %v, want invalid", err)
	}
	if _, err := s.CreateFYIMessage(ctx, domain.FYIMessage{TargetSessionID: target.ID, Message: strings.Repeat("x", domain.MaxFYIMessageBytes+1), CreatedAt: now}); !errors.Is(err, domain.ErrInvalidFYIMessage) {
		t.Fatalf("oversized fyi err = %v, want invalid", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if count, err := s.CountPendingFYIMessages(ctx, target.ID); err != nil || count != 2 {
		t.Fatalf("pending after restart = %d, err = %v", count, err)
	}
	claimed, err := s.ClaimPendingFYIMessages(ctx, target.ID, "turn-1", now)
	if err != nil || len(claimed) != 2 || claimed[0].Message != "PR #7 merged" || claimed[1].Message != "CI on main is green" ||
		claimed[0].SenderSessionID != "ao-orchestrator" || !claimed[0].CreatedAt.Equal(now) {
		t.Fatalf("claimed = %+v, err = %v", claimed, err)
	}
	if again, err := s.ClaimPendingFYIMessages(ctx, target.ID, "turn-2", now); err != nil || len(again) != 0 {
		t.Fatalf("second claim = %+v, err = %v; one turn owns a claim", again, err)
	}
	if released, err := s.ReleaseFYIMessages(ctx, target.ID, "turn-1"); err != nil || released != 2 {
		t.Fatalf("released = %d, err = %v", released, err)
	}
	if _, err := s.ClaimPendingFYIMessages(ctx, target.ID, "turn-3", now); err != nil {
		t.Fatal(err)
	}
	// A daemon that crashed holding the claim must not strand the messages.
	if requeued, err := s.RequeueClaimedFYIMessages(ctx); err != nil || requeued != 2 {
		t.Fatalf("requeued = %d, err = %v", requeued, err)
	}
	if _, err := s.ClaimPendingFYIMessages(ctx, target.ID, "turn-4", now); err != nil {
		t.Fatal(err)
	}
	if acked, err := s.AcknowledgeFYIMessages(ctx, target.ID, "stale-token", now); err != nil || acked != 0 {
		t.Fatalf("stale token acknowledged %d, err = %v", acked, err)
	}
	if acked, err := s.AcknowledgeFYIMessages(ctx, target.ID, "turn-4", now); err != nil || acked != 2 {
		t.Fatalf("acknowledged = %d, err = %v", acked, err)
	}
	if count, err := s.CountPendingFYIMessages(ctx, target.ID); err != nil || count != 0 {
		t.Fatalf("pending after acknowledgement = %d, err = %v", count, err)
	}
	if requeued, err := s.RequeueClaimedFYIMessages(ctx); err != nil || requeued != 0 {
		t.Fatalf("acknowledged fyis requeued = %d, err = %v", requeued, err)
	}
}
