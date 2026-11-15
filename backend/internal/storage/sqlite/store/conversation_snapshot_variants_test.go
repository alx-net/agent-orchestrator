package store_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// heavyConversationFixture is conversationFixture for benchmarks too.
func heavyConversationFixture(tb testing.TB) (*sqlite.Store, domain.SessionID, string) {
	tb.Helper()
	s := sqlitetest.MustOpen(tb)
	ctx := context.Background()
	if err := s.UpsertProject(ctx, domain.ProjectRecord{ID: "hist", Path: "/tmp/hist", RegisteredAt: histClock}); err != nil {
		tb.Fatalf("seed project: %v", err)
	}
	rec := sampleRecord("hist")
	rec.Mode = domain.SessionModeChat
	session, err := s.CreateSession(ctx, rec)
	if err != nil {
		tb.Fatalf("create session: %v", err)
	}
	conversation, err := s.CreateConversation(ctx, "conv-1", domain.ConversationScopeSession, "hist", session.ID, histClock)
	if err != nil {
		tb.Fatalf("create conversation: %v", err)
	}
	if err := s.ClaimChatControllerGeneration(ctx, session.ID, "gen-1"); err != nil {
		tb.Fatalf("claim controller generation: %v", err)
	}
	return s, session.ID, conversation.ID
}

// seedHeavyTurns writes turns whose command activity carries outputBytes of
// command output and streamed text, the shape of a long agent conversation.
func seedHeavyTurns(t testing.TB, s *sqlite.Store, conversation string, session domain.SessionID, turns, outputBytes int) {
	t.Helper()
	ctx := context.Background()
	output := strings.Repeat("o", outputBytes)
	for i := range turns {
		turnID := fmt.Sprintf("turn-%d", i)
		providerTurn := "provider-" + turnID
		at := histClock.Add(time.Duration(i) * time.Minute)
		created, err := s.AppendUserMessage(ctx, conversation, session, "gen-1", domain.ConversationMessage{
			ID: turnID + "-msg", Text: "prompt " + turnID, Origin: domain.MessageOriginHuman,
		}, turnID, at)
		if err != nil || !created {
			t.Fatalf("append %s: created=%v err=%v", turnID, created, err)
		}
		if err := s.BindTurnToProvider(ctx, turnID, providerTurn, at); err != nil {
			t.Fatalf("bind %s: %v", turnID, err)
		}
		item := turnID + "-exec"
		if err := s.UpsertActivity(ctx, conversation, providerTurn, domain.ConversationActivity{
			ID: turnID + "-act", Kind: domain.ActivityKindCommand, Status: domain.ActivityStatusRunning,
			Summary: "go test ./...", ProviderItemID: item, Detail: []byte(`{"command":"go test ./..."}`),
		}, at); err != nil {
			t.Fatalf("activity %s: %v", turnID, err)
		}
		if _, err := s.AppendCommandOutput(ctx, conversation, item, output, at); err != nil {
			t.Fatalf("command output %s: %v", turnID, err)
		}
		if _, err := s.AppendActivityStreamedText(ctx, conversation, item, output, at); err != nil {
			t.Fatalf("streamed text %s: %v", turnID, err)
		}
		if err := s.AppendAssistantDelta(ctx, conversation, turnID+"-reply", providerTurn, "reply "+turnID, turnID+"-reply", at); err != nil {
			t.Fatalf("reply %s: %v", turnID, err)
		}
		if err := s.SettleTurn(ctx, conversation, providerTurn, domain.TurnStateCompleted, "", at); err != nil {
			t.Fatalf("settle %s: %v", turnID, err)
		}
	}
}

// The narrower snapshots chat start reads must match the full one in everything
// but what they leave out, including the rolled-back filter.
func TestConversationSnapshotVariantsMatchFullSnapshot(t *testing.T) {
	s, session, conversation := heavyConversationFixture(t)
	ctx := context.Background()
	seedHeavyTurns(t, s, conversation, session, 3, 64)
	if _, err := s.RollbackTurns(ctx, conversation, "turn-2", histClock.Add(time.Hour)); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	full, err := s.LoadConversationSnapshot(ctx, conversation)
	if err != nil {
		t.Fatalf("full snapshot: %v", err)
	}
	if len(full.Turns) != 3 || len(full.Messages) == 0 || len(full.Activities) != 2 {
		t.Fatalf("full snapshot = %d turns, %d messages, %d activities; want 3, some, 2",
			len(full.Turns), len(full.Messages), len(full.Activities))
	}
	if full.Activities[0].CommandOutput == "" || full.Activities[0].StreamedText == "" {
		t.Fatalf("full snapshot lost activity output: %+v", full.Activities[0])
	}

	lite, err := s.LoadConversationSnapshotWithoutActivityOutput(ctx, conversation)
	if err != nil {
		t.Fatalf("snapshot without output: %v", err)
	}
	want := full
	want.Activities = append([]domain.ConversationActivity(nil), full.Activities...)
	for i := range want.Activities {
		want.Activities[i].CommandOutput = ""
		want.Activities[i].StreamedText = ""
	}
	if !reflect.DeepEqual(lite, want) {
		t.Fatalf("snapshot without output differs from the full snapshot:\n got %+v\nwant %+v", lite, want)
	}

	turns, err := s.LoadConversationTurnsSnapshot(ctx, conversation)
	if err != nil {
		t.Fatalf("turns snapshot: %v", err)
	}
	want = full
	want.Messages, want.Activities = nil, nil
	if !reflect.DeepEqual(turns, want) {
		t.Fatalf("turns snapshot differs from the full snapshot:\n got %+v\nwant %+v", turns, want)
	}
}

// BenchmarkConversationSnapshotForChatStart compares what chat start used to read
// (full, at each of its call sites) with the narrower reads it now uses: turns
// for live reconnect and resume checks, no output for native history import.
//
//	go test ./internal/storage/sqlite/store/ -run '^$' -bench ConversationSnapshotForChatStart -benchmem
func BenchmarkConversationSnapshotForChatStart(b *testing.B) {
	s, session, conversation := heavyConversationFixture(b)
	// 200 turns, each with 32 KiB of command output and 32 KiB of streamed text.
	seedHeavyTurns(b, s, conversation, session, 200, 32*1024)
	ctx := context.Background()
	for _, variant := range []struct {
		name string
		load func(context.Context, string) (any, error)
	}{
		{"full", func(ctx context.Context, id string) (any, error) { return s.LoadConversationSnapshot(ctx, id) }},
		{"without_output", func(ctx context.Context, id string) (any, error) {
			return s.LoadConversationSnapshotWithoutActivityOutput(ctx, id)
		}},
		{"turns_only", func(ctx context.Context, id string) (any, error) { return s.LoadConversationTurnsSnapshot(ctx, id) }},
	} {
		b.Run(variant.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := variant.load(ctx, conversation); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
