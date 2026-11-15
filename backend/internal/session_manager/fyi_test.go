package sessionmanager

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	reportsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/report"
)

// fyiFakeStore adds an in-memory `ao send --fyi` outbox to the fake store.
type fyiFakeStore struct {
	*fakeStore
	fyiMu sync.Mutex
	fyis  []domain.FYIMessage
}

func (s *fyiFakeStore) CreateFYIMessage(_ context.Context, msg domain.FYIMessage) (domain.FYIMessage, error) {
	if err := msg.Validate(); err != nil {
		return domain.FYIMessage{}, err
	}
	s.fyiMu.Lock()
	defer s.fyiMu.Unlock()
	msg.ID, msg.DeliveryState = int64(len(s.fyis)+1), domain.ReportPending
	s.fyis = append(s.fyis, msg)
	return msg, nil
}

func (s *fyiFakeStore) CountPendingFYIMessages(_ context.Context, target domain.SessionID) (int, error) {
	s.fyiMu.Lock()
	defer s.fyiMu.Unlock()
	n := 0
	for _, row := range s.fyis {
		if row.TargetSessionID == target && row.DeliveryState == domain.ReportPending {
			n++
		}
	}
	return n, nil
}

func (s *fyiFakeStore) ClaimPendingFYIMessages(_ context.Context, target domain.SessionID, token string, at time.Time) ([]domain.FYIMessage, error) {
	s.fyiMu.Lock()
	defer s.fyiMu.Unlock()
	var claimed []domain.FYIMessage
	for i := range s.fyis {
		if s.fyis[i].TargetSessionID == target && s.fyis[i].DeliveryState == domain.ReportPending {
			s.fyis[i].DeliveryState, s.fyis[i].ClaimToken, s.fyis[i].ClaimedAt = domain.ReportClaimed, token, at
			claimed = append(claimed, s.fyis[i])
		}
	}
	return claimed, nil
}

func (s *fyiFakeStore) AcknowledgeFYIMessages(_ context.Context, target domain.SessionID, token string, _ time.Time) (int, error) {
	return s.settleFYIs(target, token, domain.ReportAcknowledged), nil
}

func (s *fyiFakeStore) ReleaseFYIMessages(_ context.Context, target domain.SessionID, token string) (int, error) {
	return s.settleFYIs(target, token, domain.ReportPending), nil
}

func (s *fyiFakeStore) settleFYIs(target domain.SessionID, token string, state domain.ReportDeliveryState) int {
	s.fyiMu.Lock()
	defer s.fyiMu.Unlock()
	n := 0
	for i := range s.fyis {
		if s.fyis[i].TargetSessionID == target && s.fyis[i].DeliveryState == domain.ReportClaimed && s.fyis[i].ClaimToken == token {
			s.fyis[i].DeliveryState = state
			n++
		}
	}
	return n
}

func newFYIManager(t *testing.T, msg *fakeMessenger) (*Manager, *fyiFakeStore) {
	t.Helper()
	st := &fyiFakeStore{fakeStore: newFakeStore()}
	st.sessions["mer-2"] = pastStartupGate(domain.SessionRecord{ID: "mer-2", ProjectID: "mer", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode})
	st.sessions["mer-1"] = domain.SessionRecord{ID: "mer-1", ProjectID: "mer", Kind: domain.KindOrchestrator, DisplayName: "Orchestrator"}
	at := time.Date(2026, 10, 28, 9, 30, 0, 0, time.UTC)
	m := New(Deps{Store: st, Messenger: msg, Clock: func() time.Time { return at }})
	m.SetFYIDelivery(reportsvc.NewCoordinator(reportsvc.CoordinatorDeps{FYIs: st}))
	return m, st
}

// An FYI never starts a turn. The next turn the session starts, here a plain
// `ao send` into its terminal, carries every waiting FYI in arrival order,
// labeled with sender and time, and each is delivered once.
func TestQueueFYIWaitsForTheNextTerminalTurn(t *testing.T) {
	msg := &fakeMessenger{}
	m, st := newFYIManager(t, msg)

	for i, text := range []string{"PR #7 merged as e75f34c", "CI on main is green"} {
		pending, err := m.QueueFYI(ctx, "mer-2", text, ports.MessageDeliveryOptions{SenderSessionID: "mer-1"})
		if err != nil || pending != i+1 {
			t.Fatalf("QueueFYI = %d, %v; want %d waiting", pending, err, i+1)
		}
	}
	if len(msg.msgs) != 0 {
		t.Fatalf("an fyi was written to the pane: %q", msg.msgs)
	}

	if err := m.SendWithOptions(ctx, "mer-2", "[from mer-1] please rebase", nil, ports.MessageDeliveryOptions{SenderSessionID: "mer-1"}); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("pane writes = %q, want one turn", msg.msgs)
	}
	got := msg.msgs[0]
	merged := "[fyi] mer-1 (sent 2026-10-28 09:30 UTC; informational, no reply needed)\nPR #7 merged as e75f34c"
	green := "CI on main is green"
	if !strings.HasPrefix(got, `<ao-session-delivery id="`) || !strings.HasSuffix(got, "</ao-session-delivery>") ||
		!strings.Contains(got, "[from mer-1] please rebase\n\n<ao-worker-reports>\n") ||
		!strings.Contains(got, merged) || strings.Index(got, merged) > strings.Index(got, green) {
		t.Fatalf("turn text = %q", got)
	}
	for _, row := range st.fyis {
		if row.DeliveryState != domain.ReportAcknowledged {
			t.Fatalf("fyi %d is %s after its turn was delivered", row.ID, row.DeliveryState)
		}
	}

	if err := m.SendWithOptions(ctx, "mer-2", "next", nil, ports.MessageDeliveryOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(msg.msgs[1], "[fyi]") {
		t.Fatalf("an acknowledged fyi was delivered twice: %q", msg.msgs[1])
	}
}

// A send the pane refused delivered nothing, so its FYIs wait for the next turn.
func TestQueueFYIStaysPendingWhenTheTurnIsNotDelivered(t *testing.T) {
	msg := &fakeMessenger{err: errors.New("pane gone")}
	m, st := newFYIManager(t, msg)
	if _, err := m.QueueFYI(ctx, "mer-2", "PR #7 merged", ports.MessageDeliveryOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := m.SendWithOptions(ctx, "mer-2", "please rebase", nil, ports.MessageDeliveryOptions{}); err == nil {
		t.Fatal("send succeeded through a failing pane")
	}
	if pending, _ := st.CountPendingFYIMessages(ctx, "mer-2"); pending != 1 {
		t.Fatalf("pending = %d, want the fyi kept for the next turn", pending)
	}
	if !strings.Contains(msg.msgs[0], "[fyi] the user") {
		t.Fatalf("attempted turn = %q", msg.msgs[0])
	}
}

func TestQueueFYIRefusesSessionsWithoutANextTurn(t *testing.T) {
	m, st := newFYIManager(t, &fakeMessenger{})
	st.sessions["mer-3"] = domain.SessionRecord{ID: "mer-3", ProjectID: "mer", Kind: domain.KindWorker, IsTerminated: true}
	if _, err := m.QueueFYI(ctx, "mer-3", "too late", ports.MessageDeliveryOptions{}); !errors.Is(err, ErrTerminated) {
		t.Fatalf("terminated target err = %v", err)
	}
	if _, err := m.QueueFYI(ctx, "mer-404", "nobody", ports.MessageDeliveryOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown target err = %v", err)
	}
	plain := New(Deps{Store: newFakeStore(), Messenger: &fakeMessenger{}})
	if _, err := plain.QueueFYI(ctx, "mer-2", "hi", ports.MessageDeliveryOptions{}); !errors.Is(err, ErrFYIUnsupported) {
		t.Fatalf("store without an outbox err = %v", err)
	}
}
