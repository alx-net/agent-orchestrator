package chat_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// recordingSwitcher hears of usage-limited turns, as Session Manager does.
type recordingSwitcher struct {
	mu    sync.Mutex
	heard []string
}

func (r *recordingSwitcher) UsageLimitReached(id domain.SessionID, providerTurnID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.heard = append(r.heard, string(id)+"/"+providerTurnID)
}

func (r *recordingSwitcher) await(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		heard := append([]string(nil), r.heard...)
		r.mu.Unlock()
		if len(heard) >= n {
			return heard
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("switcher heard of %d usage limits, want %d", len(r.heard), n)
	return nil
}

func TestUsageLimitedTurnTellsTheAccountSwitcher(t *testing.T) {
	h := newHarnessForHarness(t, domain.HarnessClaudeCode)
	switcher := &recordingSwitcher{}
	h.svc.SetUsageLimitSwitcher(switcher)

	source := failUsageLimitedTurn(t, h, timedUsageLimit)
	heard := switcher.await(t, 1)
	if heard[0] != string(testSession)+"/"+source.ProviderTurnID {
		t.Fatalf("switcher heard %v, want %s/%s", heard, testSession, source.ProviderTurnID)
	}
	turn, err := h.svc.UsageLimitedTurn(context.Background(), testSession, source.ProviderTurnID)
	if err != nil || turn.ID != source.ID || turn.UsageLimit == nil {
		t.Fatalf("usage-limited turn = %+v, %v", turn, err)
	}

	// Another failure is not a usage limit and moves nothing.
	other, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
		Text: "next", ClientMessageID: uuid.NewString(), Origin: domain.MessageOriginHuman,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: other.ProviderTurnID,
		TurnState: domain.TurnStateFailed, Err: errors.New("connection reset by peer")})
	failedTurnSnapshot(t, h, other.ID)
	time.Sleep(20 * time.Millisecond)
	switcher.mu.Lock()
	defer switcher.mu.Unlock()
	if len(switcher.heard) != 1 {
		t.Fatalf("switcher heard %v, want only the usage limit", switcher.heard)
	}
	// The earlier turn is superseded now; it is no longer moved for.
	if _, err := h.svc.UsageLimitedTurn(context.Background(), testSession, source.ProviderTurnID); !errors.Is(err, chatsvc.ErrUsageLimitAutoResumeInvalid) {
		t.Fatalf("superseded turn = %v, want refused", err)
	}
}

// After a move the turn runs again at once, though the old account's reset is
// still ahead: the account it ran out on is no longer the session's.
func TestContinueAfterAccountSwitchRunsTheTurnBeforeTheReset(t *testing.T) {
	h := newHarnessForHarness(t, domain.HarnessClaudeCode)
	ctx := context.Background()
	source := failUsageLimitedTurn(t, h, timedUsageLimit)
	if source.UsageLimit == nil || source.UsageLimit.ResetsAt == nil || !source.UsageLimit.ResetsAt.After(h.now()) {
		t.Fatalf("fixture reset = %+v, want one in the future", source.UsageLimit)
	}

	retry, err := h.svc.ContinueAfterAccountSwitch(ctx, testSession, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retry.ID == source.ID || len(h.conv.sentMessages()) != 2 {
		t.Fatalf("retry = %+v, sent %d; want one new turn dispatched", retry, len(h.conv.sentMessages()))
	}
	if retryID, found, err := h.st.RetryTurnIDForSource(ctx, source.ConversationID, source.ID); err != nil || !found || retryID != retry.ID {
		t.Fatalf("retry link = %q, %v, %v", retryID, found, err)
	}
	// Once run again, a second continuation refuses rather than run it twice.
	if _, err := h.svc.ContinueAfterAccountSwitch(ctx, testSession, source.ID); !errors.Is(err, chatsvc.ErrUsageLimitAutoResumeInvalid) {
		t.Fatalf("second continuation = %v, want refused", err)
	}
	if len(h.conv.sentMessages()) != 2 {
		t.Fatal("the turn ran a third time")
	}
}

func TestRecordUsageLimitSwitchKeepsOneRowPerTurn(t *testing.T) {
	h := newHarnessForHarness(t, domain.HarnessClaudeCode)
	ctx := context.Background()
	source := failUsageLimitedTurn(t, h, timedUsageLimit)

	for _, phase := range []domain.UsageLimitSwitchPhase{domain.UsageLimitSwitchSwitching, domain.UsageLimitSwitchContinued} {
		if err := h.svc.RecordUsageLimitSwitch(ctx, testSession, source.ID, domain.UsageLimitSwitch{
			Phase: phase, FromAccountID: "device-claude", ToAccountID: "acct-b", MCPNeedsAuth: []string{"linear"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		for _, activity := range s.Activities {
			if activity.Status == domain.ActivityStatusCompleted && activity.Kind == domain.ActivityKindSystem {
				return true
			}
		}
		return false
	})
	var rows []domain.ConversationActivity
	for _, activity := range snapshot.Activities {
		var detail struct {
			Event string `json:"event"`
		}
		if json.Unmarshal(activity.Detail, &detail) == nil && detail.Event == chatsvc.UsageLimitSwitchEvent {
			rows = append(rows, activity)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("switch rows = %d, want one updated in place", len(rows))
	}
	var detail struct {
		Phase         string   `json:"phase"`
		AccountID     string   `json:"accountId"`
		FromAccountID string   `json:"fromAccountId"`
		MCPNeedsAuth  []string `json:"mcpNeedsAuth"`
	}
	if err := json.Unmarshal(rows[0].Detail, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Phase != "continued" || detail.AccountID != "acct-b" || detail.FromAccountID != "device-claude" || len(detail.MCPNeedsAuth) != 1 {
		t.Fatalf("switch row = %+v", detail)
	}
	if rows[0].TurnID != source.ID {
		t.Fatalf("switch row on turn %q, want the refused turn %q", rows[0].TurnID, source.ID)
	}
}
