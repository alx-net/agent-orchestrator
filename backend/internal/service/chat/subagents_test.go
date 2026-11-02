package chat_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

type subagentMessageCall struct {
	toolUseID string
	msg       ports.ChatSubagentMessage
}

// subagentRecorder is a provider that can address one subagent directly.
type subagentRecorder struct {
	*fakeConversation

	mu       sync.Mutex
	messages []subagentMessageCall
	stops    []string
	err      error
}

func newSubagentRecorder(supported bool) *subagentRecorder {
	conv := newFakeConversation()
	caps := productionCaps()
	caps[ports.ChatCapabilitySubagents] = supported
	conv.setCapabilities(caps)
	return &subagentRecorder{fakeConversation: conv}
}

func (s *subagentRecorder) MessageSubagent(
	_ context.Context,
	toolUseID string,
	msg ports.ChatSubagentMessage,
) (ports.ChatSubagentMessageReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, subagentMessageCall{toolUseID: toolUseID, msg: msg})
	if s.err != nil {
		return ports.ChatSubagentMessageReceipt{}, s.err
	}
	return ports.ChatSubagentMessageReceipt{ID: msg.ID, State: "queued"}, nil
}

func (s *subagentRecorder) StopSubagent(_ context.Context, toolUseID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stops = append(s.stops, toolUseID)
	return s.err
}

func TestSubagentMessageAndStopReachTheNamedSubagent(t *testing.T) {
	provider := newSubagentRecorder(true)
	h := newHarnessWithConversation(t, provider)
	ctx := context.Background()

	result, err := h.svc.MessageSubagent(ctx, testSession, "toolu_agent", "  Skip the tests folder.  ")
	if err != nil {
		t.Fatalf("MessageSubagent: %v", err)
	}
	if result.ID == "" || result.State != "queued" {
		t.Fatalf("result = %#v", result)
	}
	if err := h.svc.StopSubagent(ctx, testSession, "toolu_agent"); err != nil {
		t.Fatalf("StopSubagent: %v", err)
	}
	if _, err := h.svc.MessageSubagent(ctx, testSession, "toolu_agent", " \n "); !errors.Is(err, chatsvc.ErrSubagentTextRequired) {
		t.Fatalf("empty message error = %v", err)
	}

	provider.mu.Lock()
	messages, stops := provider.messages, provider.stops
	provider.mu.Unlock()
	// AO mints the id, so the receipt and the provider's inbox name the same message.
	if len(messages) != 1 || messages[0].toolUseID != "toolu_agent" ||
		messages[0].msg.Text != "Skip the tests folder." || messages[0].msg.ID != result.ID {
		t.Fatalf("provider saw %#v, want one trimmed message under %q", messages, result.ID)
	}
	if len(stops) != 1 || stops[0] != "toolu_agent" {
		t.Fatalf("provider stops = %v", stops)
	}

	provider.mu.Lock()
	provider.err = ports.ErrChatSubagentNotRunning
	provider.mu.Unlock()
	if _, err := h.svc.MessageSubagent(ctx, testSession, "toolu_agent", "still there?"); !errors.Is(err, chatsvc.ErrSubagentNotRunning) {
		t.Fatalf("finished subagent message error = %v", err)
	}
	if err := h.svc.StopSubagent(ctx, testSession, "toolu_agent"); !errors.Is(err, chatsvc.ErrSubagentNotRunning) {
		t.Fatalf("finished subagent stop error = %v", err)
	}
}

func TestSubagentControlNeedsTheNegotiatedCapability(t *testing.T) {
	provider := newSubagentRecorder(false)
	h := newHarnessWithConversation(t, provider)
	ctx := context.Background()

	if _, err := h.svc.MessageSubagent(ctx, testSession, "toolu_agent", "hello"); !errors.Is(err, chatsvc.ErrSubagentsUnsupported) {
		t.Fatalf("MessageSubagent error = %v", err)
	}
	if err := h.svc.StopSubagent(ctx, testSession, "toolu_agent"); !errors.Is(err, chatsvc.ErrSubagentsUnsupported) {
		t.Fatalf("StopSubagent error = %v", err)
	}
	if _, err := h.svc.MessageSubagent(ctx, "missing-1", "toolu_agent", "hello"); !errors.Is(err, ports.ErrSessionNotFound) {
		t.Fatalf("unknown session error = %v", err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.messages) != 0 || len(provider.stops) != 0 {
		t.Fatalf("provider reached without the capability: %v %v", provider.messages, provider.stops)
	}
}
