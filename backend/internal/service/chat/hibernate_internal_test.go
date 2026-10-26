package chat

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestClosedChatViewTimestampExpiresAfterGrace(t *testing.T) {
	now := time.Now()
	svc := New(Options{Now: func() time.Time { return now }})
	id := domain.SessionID("closed-chat")
	svc.setViewLease(id, "view", true)
	svc.setViewLease(id, "view", false)
	if !svc.hasChatView(id) {
		t.Fatal("closing a view skipped the warm grace period")
	}
	now = now.Add(chatHibernateGrace)
	if svc.hasChatView(id) || len(svc.viewClosedAt) != 0 {
		t.Fatalf("expired view retained: %v", svc.viewClosedAt)
	}
}

func TestStopForgetsPendingVersionSwitch(t *testing.T) {
	svc := New(Options{})
	id := domain.SessionID("switching-chat")
	svc.noteVersionSwitch(id, "1.2.3")
	if err := svc.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.takeVersionSwitch(id); ok {
		t.Fatal("stopped session kept its pending release switch")
	}
}
