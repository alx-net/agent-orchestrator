package chat_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

// aliasConversation reports the provider's own id for a model alias, as the ACP
// driver does for Claude Code's "fable" once the session runs claude-fable-5-2.
type aliasConversation struct {
	*fakeConversation
	alias, id string
}

func (c *aliasConversation) SendTurn(ctx context.Context, msg ports.ChatUserMessage) (ports.ChatTurnRef, error) {
	ref, err := c.fakeConversation.SendTurn(ctx, msg)
	if err == nil && msg.Settings.Model == c.alias {
		ref.Model = c.id
	}
	return ref, err
}

// A conversation saved with an alias is saved with the provider's full id once
// a turn has been sent with it, so the stored choice converges on full ids and
// later sends have nothing to translate.
func TestSendSavesTheProviderIDForAModelAlias(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	conv := &aliasConversation{fakeConversation: newFakeConversation(), alias: "fable", id: "claude-fable-5-2"}
	svc := chatsvc.New(chatsvc.Options{
		Store: st, Sessions: st,
		Drivers: fakeRegistry{driver: fakeDriver{conv: conv}},
		Log:     slog.New(slog.DiscardHandler),
		NewID:   func() string { return "model-alias-id" },
	})
	t.Cleanup(func() { _ = svc.Stop(ctx, testSession) })
	if _, err := svc.Start(ctx, chatsvc.StartConfig{
		SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessClaudeCode,
		WorkspacePath: t.TempDir(),
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := svc.SetTurnSettings(ctx, testSession, domain.ConversationSettings{
		Model: "fable", ReasoningEffort: "high",
	}); err != nil {
		t.Fatalf("SetTurnSettings: %v", err)
	}

	if _, err := svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "hi", ClientMessageID: "model-alias-send", Origin: domain.MessageOriginHuman,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	conv.mu.Lock()
	sent := append([]ports.ChatUserMessage(nil), conv.sent...)
	conv.mu.Unlock()
	if len(sent) != 1 || sent[0].Settings.Model != "fable" {
		t.Fatalf("sent = %+v, want one turn sent with the saved alias", sent)
	}
	conversation, err := st.ConversationForSession(ctx, testSession)
	if err != nil {
		t.Fatalf("ConversationForSession: %v", err)
	}
	if got := conversation.Settings; got.Model != "claude-fable-5-2" || got.ReasoningEffort != "high" {
		t.Fatalf("saved settings = %+v, want the provider's id and the effort kept", got)
	}
	_, settings, err := svc.Models(ctx, testSession)
	if settings.Model != "claude-fable-5-2" {
		t.Fatalf("next-turn model = %q (err %v), want the provider's id", settings.Model, err)
	}
}
