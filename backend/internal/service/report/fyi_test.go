package report

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestPrepareFYIsLabelsSenderAndTimeInArrivalOrder(t *testing.T) {
	now := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	store := newCoordinatorStore(now)
	store.sessions = []domain.SessionRecord{{ID: "w1", ProjectID: "p", Kind: domain.KindWorker, DisplayName: "Backend"}}
	fyis := &fyiOutbox{}
	fyis.add("w2", "w1", "PR #7 merged as e75f34c", now)
	fyis.add("w2", "", "Use the staging config", now.Add(time.Minute))
	fyis.add("other", "w1", "not for w2", now)
	c := NewCoordinator(CoordinatorDeps{Store: store, FYIs: fyis, Now: func() time.Time { return now }, NewToken: func() string { return "turn" }})

	batch, err := c.PrepareFYIs(context.Background(), "w2")
	if err != nil || len(batch.FYIs) != 2 {
		t.Fatalf("prepare = %#v, err = %v", batch, err)
	}
	got := batch.AppendToUserMessage("please rebase")
	first := "[fyi] [Backend](ao://sessions/p/w1) (sent 2026-09-14 10:00 UTC; informational, no reply needed)\nPR #7 merged as e75f34c"
	second := "[fyi] the user (sent 2026-09-14 10:01 UTC; informational, no reply needed)\nUse the staging config"
	if !strings.HasPrefix(got, "please rebase\n\n<ao-worker-reports>\nReports since your previous turn:\n\n") ||
		!strings.HasSuffix(got, "\n</ao-worker-reports>") ||
		!strings.Contains(got, first) || !strings.Contains(got, second) ||
		strings.Index(got, first) > strings.Index(got, second) || strings.Contains(got, "not for w2") {
		t.Fatalf("turn text = %q", got)
	}
	if fyis.pending("w2") != 0 {
		t.Fatal("claimed fyis still pending")
	}
	if err := c.AcceptPiggyback(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if fyis.acknowledged("w2") != 2 || fyis.pending("other") != 1 {
		t.Fatalf("acknowledged = %d, other pending = %d", fyis.acknowledged("w2"), fyis.pending("other"))
	}
	if next, err := c.PrepareFYIs(context.Background(), "w2"); err != nil || len(next.FYIs) != 0 || next.AppendToUserMessage("x") != "x" {
		t.Fatalf("next turn = %#v, err = %v; an acknowledged fyi is delivered once", next, err)
	}
}

func TestPreparePiggybackCarriesOrchestratorReportsAndFYIsTogether(t *testing.T) {
	now := time.Date(2026, 9, 14, 13, 0, 0, 0, time.UTC)
	store := newCoordinatorStore(now)
	store.sessions = []domain.SessionRecord{{ID: "orch", ProjectID: "p", Kind: domain.KindOrchestrator, Mode: domain.SessionModeChat}}
	store.add(reportAt("info", "worker", now, now.Add(time.Hour), "context"))
	fyis := &fyiOutbox{}
	fyis.add("orch", "worker", "CI on #12 is green", now)
	c := NewCoordinator(CoordinatorDeps{Store: store, FYIs: fyis, Delivery: &coordinatorDelivery{}, Now: func() time.Time { return now }, NewToken: func() string { return "piggyback" }})

	batch, err := c.PreparePiggyback(context.Background(), "orch")
	if err != nil || len(batch.Reports) != 1 || len(batch.FYIs) != 1 {
		t.Fatalf("prepare = %#v, err = %v", batch, err)
	}
	got := batch.AppendToUserMessage("status?")
	if strings.Count(got, "<ao-worker-reports>") != 1 || strings.Index(got, "[checkpoint]") > strings.Index(got, "[fyi] worker") {
		t.Fatalf("combined turn = %q", got)
	}
	// A refused turn hands both back for the next one.
	if err := c.ReleasePiggyback(context.Background(), batch, errors.New("refused")); err == nil {
		t.Fatal("release dropped the refusal cause")
	}
	if fyis.pending("orch") != 1 || store.reports[0].DeliveryState != domain.ReportPending {
		t.Fatalf("after release: fyis pending = %d, report = %s", fyis.pending("orch"), store.reports[0].DeliveryState)
	}
}

// A report delivery is already the "Reports since your previous turn" list, so
// FYIs riding on it become further entries instead of a nested envelope.
func TestFYIsJoinAStandaloneReportDelivery(t *testing.T) {
	now := time.Date(2026, 9, 14, 13, 0, 0, 0, time.UTC)
	batch := PreparedBatch{Token: "t", Target: "orch", FYIs: []domain.FYIMessage{{
		TargetSessionID: "orch", SenderSessionID: "w1", SenderProjectID: "p", Message: "merged", CreatedAt: now,
	}}}
	delivery := PreparedBatch{Reports: []domain.ReportRecord{reportAt("r", "w2", now, now, "done soon")}}.Render()
	got := batch.AppendToUserMessage(delivery)
	if got != delivery+"\n\n[fyi] [w1](ao://sessions/p/w1) (sent 2026-09-14 13:00 UTC; informational, no reply needed)\nmerged" {
		t.Fatalf("delivery = %q", got)
	}
}

type fyiOutbox struct {
	mu   sync.Mutex
	rows []domain.FYIMessage
}

func (o *fyiOutbox) add(target, sender domain.SessionID, text string, at time.Time) {
	o.rows = append(o.rows, domain.FYIMessage{
		ID: int64(len(o.rows) + 1), TargetSessionID: target, SenderSessionID: sender,
		Message: text, CreatedAt: at, DeliveryState: domain.ReportPending,
	})
}

func (o *fyiOutbox) count(target domain.SessionID, state domain.ReportDeliveryState) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, row := range o.rows {
		if row.TargetSessionID == target && row.DeliveryState == state {
			n++
		}
	}
	return n
}

func (o *fyiOutbox) pending(target domain.SessionID) int {
	return o.count(target, domain.ReportPending)
}

func (o *fyiOutbox) acknowledged(target domain.SessionID) int {
	return o.count(target, domain.ReportAcknowledged)
}

func (o *fyiOutbox) ClaimPendingFYIMessages(_ context.Context, target domain.SessionID, token string, at time.Time) ([]domain.FYIMessage, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var claimed []domain.FYIMessage
	for i := range o.rows {
		if o.rows[i].TargetSessionID == target && o.rows[i].DeliveryState == domain.ReportPending {
			o.rows[i].DeliveryState, o.rows[i].ClaimToken, o.rows[i].ClaimedAt = domain.ReportClaimed, token, at
			claimed = append(claimed, o.rows[i])
		}
	}
	return claimed, nil
}

func (o *fyiOutbox) AcknowledgeFYIMessages(_ context.Context, target domain.SessionID, token string, at time.Time) (int, error) {
	return o.settle(target, token, func(row *domain.FYIMessage) {
		row.DeliveryState, row.AcknowledgedAt = domain.ReportAcknowledged, at
	}), nil
}

func (o *fyiOutbox) ReleaseFYIMessages(_ context.Context, target domain.SessionID, token string) (int, error) {
	return o.settle(target, token, func(row *domain.FYIMessage) {
		row.DeliveryState, row.ClaimToken, row.ClaimedAt = domain.ReportPending, "", time.Time{}
	}), nil
}

func (o *fyiOutbox) settle(target domain.SessionID, token string, apply func(*domain.FYIMessage)) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for i := range o.rows {
		if o.rows[i].TargetSessionID == target && o.rows[i].DeliveryState == domain.ReportClaimed && o.rows[i].ClaimToken == token {
			apply(&o.rows[i])
			n++
		}
	}
	return n
}
