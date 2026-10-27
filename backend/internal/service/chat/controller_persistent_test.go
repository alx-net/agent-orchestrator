package chat_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	sqlite "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type receiptConversation struct {
	*terminatingConversation
	ack     func(string) error
	eventMu sync.Mutex
}

func (c *receiptConversation) Close() error {
	c.eventMu.Lock()
	defer c.eventMu.Unlock()
	return c.fakeConversation.Close()
}

func (c *receiptConversation) emit(events ...ports.ChatEvent) {
	c.eventMu.Lock()
	defer c.eventMu.Unlock()
	c.fakeConversation.emit(events...)
}

func (c *receiptConversation) AcknowledgeProviderEvent(_ context.Context, id string) error {
	return c.ack(id)
}

type flakyReceiptStore struct {
	chatsvc.Store
	failures atomic.Int32 // -1 fails permanently; positive counts transient failures.
}

func (s *flakyReceiptStore) ProjectProviderEvent(
	ctx context.Context, conversationID string, session domain.SessionID,
	generation, providerEventID, method, payloadJSON string, now time.Time,
	project func(context.Context) error,
) (bool, error) {
	return s.Store.ProjectProviderEvent(ctx, conversationID, session, generation,
		providerEventID, method, payloadJSON, now, func(txCtx context.Context) error {
			if err := project(txCtx); err != nil {
				return err
			}
			if method == string(ports.ChatEventMessageDelta) && s.failures.Load() != 0 {
				if s.failures.Load() > 0 {
					s.failures.Add(-1)
				}
				return errors.New("injected event transaction rollback")
			}
			return nil
		})
}

func TestPersistentProjectionNeverAcknowledgesPastFailedOutput(t *testing.T) {
	for _, failures := range []int32{1, -1} {
		name := "transient failure retries in order"
		if failures < 0 {
			name = "permanent failure detaches without acknowledging completion"
		}
		t.Run(name, func(t *testing.T) {
			acknowledged := make(chan string, 4)
			conv := &receiptConversation{
				terminatingConversation: &terminatingConversation{fakeConversation: newFakeConversation()},
				ack:                     func(id string) error { acknowledged <- id; return nil },
			}
			h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
				flaky := &flakyReceiptStore{Store: st}
				flaky.failures.Store(failures)
				return flaky
			})
			turn, err := h.ctrl.Send(context.Background(), ports.ChatUserMessage{Text: "first"})
			if err != nil {
				t.Fatal(err)
			}
			conv.emit(
				ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: turn.ProviderTurnID},
				ports.ChatEvent{Kind: ports.ChatEventMessageDelta, ProviderTurnID: turn.ProviderTurnID, ProviderItemID: "answer", ProviderEventID: "output", Delta: "retained answer"},
				ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: turn.ProviderTurnID, ProviderEventID: "completion", TurnState: domain.TurnStateCompleted},
			)
			if failures > 0 {
				for _, want := range []string{"output", "completion"} {
					select {
					case got := <-acknowledged:
						if got != want {
							t.Fatalf("ack %q, want %q", got, want)
						}
					case <-time.After(3 * time.Second):
						t.Fatalf("missing ack %s", want)
					}
				}
				snapshot := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
					return len(s.Turns) == 1 && s.Turns[0].State == domain.TurnStateCompleted
				})
				if len(snapshot.Messages) != 2 || snapshot.Messages[1].Text != "retained answer" {
					t.Fatalf("acknowledged without exactly-once output: %+v", snapshot.Messages)
				}
				return
			}
			done := make(chan struct{})
			go func() { h.ctrl.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("failed projection did not detach")
			}
			select {
			case id := <-acknowledged:
				t.Fatalf("acknowledged past failed output: %s", id)
			default:
			}
			if conv.terminated.Load() {
				t.Fatal("projection failure destroyed replay owner")
			}
			snapshot, err := h.st.LoadConversationSnapshot(context.Background(), h.ctrl.ConversationID())
			if err != nil || len(snapshot.Turns) != 1 || snapshot.Turns[0].State != domain.TurnStateRunning {
				t.Fatalf("failed projection falsely settled turn: %+v, %v", snapshot.Turns, err)
			}
			if err := h.svc.Stop(context.Background(), testSession); err != nil || h.hostStops.Load() != 1 {
				t.Fatalf("explicit stop after failed attachment did not destroy host: calls=%d err=%v", h.hostStops.Load(), err)
			}
		})
	}
}

func TestPersistentCompletionIsAcknowledgedBeforeQueuedTurnDispatch(t *testing.T) {
	for _, ackFails := range []bool{false, true} {
		name := "acknowledged"
		if ackFails {
			name = "ack failure retains queue"
		}
		t.Run(name, func(t *testing.T) {
			var acknowledged, dispatchedBeforeAck atomic.Bool
			conv := &receiptConversation{
				terminatingConversation: &terminatingConversation{fakeConversation: newFakeConversation()},
				ack: func(id string) error {
					if id == "completion" {
						if ackFails {
							return errors.New("connection lost before ack")
						}
						acknowledged.Store(true)
					}
					return nil
				},
			}
			conv.onSend = func(id string) {
				if id == "provider-turn-2" && !acknowledged.Load() {
					dispatchedBeforeAck.Store(true)
				}
			}
			h := newHarnessWithConversation(t, conv)
			ctx := context.Background()
			turn, err := h.ctrl.Send(ctx, ports.ChatUserMessage{Text: "first"})
			if err != nil {
				t.Fatal(err)
			}
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: turn.ProviderTurnID})
			h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				return len(s.Turns) == 1 && s.Turns[0].State == domain.TurnStateRunning
			})
			queued, err := h.ctrl.Send(ctx, ports.ChatUserMessage{Text: "second"})
			if err != nil || queued.State != domain.TurnStateQueued {
				t.Fatalf("queue: %+v, %v", queued, err)
			}
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: turn.ProviderTurnID, ProviderEventID: "completion", TurnState: domain.TurnStateCompleted})
			if ackFails {
				done := make(chan struct{})
				go func() { h.ctrl.Wait(); close(done) }()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("ack failure did not detach")
				}
				if len(conv.sentTexts()) != 1 || conv.terminated.Load() {
					t.Fatal("ack failure dispatched queued work or destroyed provider")
				}
			} else {
				h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
					return turnStateByText(t, s)["second"] == domain.TurnStateRunning
				})
			}
			if dispatchedBeforeAck.Load() {
				t.Fatal("queued turn dispatched before previous completion was acknowledged")
			}
		})
	}
}

func TestProviderPreservationReportRequiresLiveController(t *testing.T) {
	provider := &terminatingConversation{fakeConversation: newFakeConversation()}
	h := newHarnessWithConversation(t, provider)
	if !h.svc.PreservesProviderOnRestart(testSession) || h.svc.PreservesProviderOnRestart("missing") {
		t.Fatal("preservation report does not match established ownership")
	}
	h.svc.StopAll(context.Background())
	if h.hostStops.Load() != 0 {
		t.Fatal("daemon shutdown destroyed persistent ownership")
	}
	if h.svc.PreservesProviderOnRestart(testSession) {
		t.Fatal("missing controller advertised live ownership")
	}
	if err := h.svc.Stop(context.Background(), testSession); err != nil || h.hostStops.Load() != 1 {
		t.Fatalf("explicit stop with no registered controller: calls=%d err=%v", h.hostStops.Load(), err)
	}
	ordinary := newHarness(t)
	if ordinary.svc.PreservesProviderOnRestart(testSession) {
		t.Fatal("ordinary conversation advertised persistent ownership")
	}
}

func TestPersistentTransportFailureRetainsWorkForReconnect(t *testing.T) {
	provider := &terminatingConversation{fakeConversation: newFakeConversation()}
	h := newHarnessWithConversation(t, provider)
	ctx := context.Background()
	turn, err := h.ctrl.Send(ctx, ports.ChatUserMessage{Text: "running"})
	if err != nil {
		t.Fatal(err)
	}
	provider.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: turn.ProviderTurnID})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return len(s.Turns) == 1 && s.Turns[0].State == domain.TurnStateRunning
	})
	if _, err := h.ctrl.Send(ctx, ports.ChatUserMessage{Text: "queued"}); err != nil {
		t.Fatal(err)
	}
	provider.emit(ports.ChatEvent{
		Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerStopped,
		Err: ports.ErrChatRecoveryInconclusive,
	})
	_ = provider.Close()
	h.ctrl.Wait()
	snapshot, err := h.st.LoadConversationSnapshot(ctx, h.ctrl.ConversationID())
	if err != nil {
		t.Fatal(err)
	}
	states := turnStateByText(t, snapshot)
	if states["running"] != domain.TurnStateRunning || states["queued"] != domain.TurnStateQueued {
		t.Fatalf("transport failure settled surviving provider work: %v", states)
	}
	if h.ctrl.State() != ports.ChatControllerStopped || provider.terminated.Load() {
		t.Fatal("lost controller remained live or terminated its persistent provider")
	}
}

// questionConversation is a reattached persistent provider whose turn is
// paused on a structured question, like Claude Code's AskUserQuestion.
type questionConversation struct {
	*reconnectedReceiptConversation
	answers chan string
}

func (c *questionConversation) ResolveInput(_ context.Context, requestID string, _ ports.ChatInputResponse) error {
	c.answers <- requestID
	return nil
}

// A daemon restart while the agent waits on a question: the host replays the
// still-pending request to the reattached controller. That replay must not stop
// the controller, and the answer must reach the provider request still waiting.
func TestReattachedControllerKeepsAPendingQuestionAnswerable(t *testing.T) {
	ctx := context.Background()
	const requestID = "acp-request:host:2"
	question := ports.ChatEvent{
		Kind: ports.ChatEventInputRequested, RequestID: requestID, Summary: "Please answer the following questions.",
		Input: &ports.ChatInputRequest{Mode: ports.ChatInputModeForm, Message: "Please answer the following questions."},
	}
	first := &receiptConversation{
		terminatingConversation: &terminatingConversation{fakeConversation: newFakeConversation()},
		ack:                     func(string) error { return nil },
	}
	h := newHarnessWithConversation(t, first)
	turn, err := h.ctrl.Send(ctx, ports.ChatUserMessage{Text: "pick one"})
	if err != nil {
		t.Fatal(err)
	}
	question.ProviderTurnID = turn.ProviderTurnID
	first.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: turn.ProviderTurnID, ProviderEventID: "started"}, question)
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return len(s.Activities) == 1 && s.Activities[0].Status == domain.ActivityStatusPending
	})
	h.svc.StopAll(ctx)
	h.ctrl.Wait()

	acks := make(chan string, 8)
	second := &questionConversation{
		reconnectedReceiptConversation: &reconnectedReceiptConversation{receiptConversation: &receiptConversation{
			terminatingConversation: &terminatingConversation{fakeConversation: newFakeConversation()},
			ack:                     func(id string) error { acks <- id; return nil },
		}},
		answers: make(chan string, 1),
	}
	svc := chatsvc.New(chatsvc.Options{
		Store: h.st, Sessions: h.st, Reader: fullSnapshotReader(h.st),
		Drivers: fakeRegistry{driver: fakeDriver{conv: second}},
		Log:     slog.New(slog.DiscardHandler),
		NewID:   func() string { return fmt.Sprintf("restart-%d", time.Now().UnixNano()) },
		Now:     h.now,
	})
	t.Cleanup(func() { svc.StopAll(ctx) })
	if _, err := svc.Start(ctx, chatsvc.StartConfig{
		SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex,
		WorkspacePath: t.TempDir(), ProviderConversationID: "thread-1",
	}); err != nil {
		t.Fatalf("reattach: %v", err)
	}
	// The host replays the pending request (it has no event id of its own),
	// then later output.
	second.emit(question, ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerBusy, ProviderEventID: "barrier"})
	for id := ""; id != "barrier"; {
		select {
		case id = <-acks:
		case <-time.After(3 * time.Second):
			t.Fatal("reattached controller stopped projecting after the replayed question")
		}
	}
	if !svc.HasLiveChatController(testSession) {
		t.Fatal("replayed question stopped the reattached controller")
	}
	if err := svc.ResolveInput(ctx, testSession, requestID, ports.ChatInputResponse{
		Action: ports.ChatInputActionAccept, Content: map[string]any{"question_0": "ENG-1837"},
	}); err != nil {
		t.Fatalf("answer replayed question: %v", err)
	}
	if got := <-second.answers; got != requestID {
		t.Fatalf("answer reached request %q, want %q", got, requestID)
	}
	snapshot, err := h.st.LoadConversationSnapshot(ctx, h.ctrl.ConversationID())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Activities) != 1 || snapshot.Activities[0].Status != domain.ActivityStatusResolved {
		t.Fatalf("activities = %+v, want the one question resolved", snapshot.Activities)
	}
	if len(snapshot.Turns) != 1 || snapshot.Turns[0].State != domain.TurnStateRunning {
		t.Fatalf("turns = %+v, want the paused turn still running", snapshot.Turns)
	}
}

func TestAwakeReconnectCandidatesRequireObservedTransportLoss(t *testing.T) {
	for _, deliberate := range []bool{false, true} {
		t.Run(map[bool]string{false: "lost attachment", true: "explicit close"}[deliberate], func(t *testing.T) {
			provider := &terminatingConversation{fakeConversation: newFakeConversation()}
			h := newHarnessWithConversation(t, provider)
			if got := h.svc.InterruptedAwakeAgents(); len(got) != 0 {
				t.Fatal("live controller offered as interrupted")
			}
			if deliberate {
				if err := h.ctrl.Close(context.Background()); err != nil {
					t.Fatal(err)
				}
			} else {
				provider.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerStopped, Err: ports.ErrChatRecoveryInconclusive})
				_ = provider.Close()
				h.ctrl.Wait()
			}
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				got := h.svc.InterruptedAwakeAgents()
				if !deliberate && len(got) == 1 {
					if got[0].SessionID != testSession || got[0].Owner.ControllerGeneration != h.ctrl.Generation() || got[0].Owner.ProviderConversationID != h.ctrl.ProviderConversationID() {
						t.Fatalf("candidate lost provider identity: %+v", got)
					}
					return
				}
				if deliberate && len(got) > 0 {
					t.Fatal("explicit close offered for automatic reconnect")
				}
				time.Sleep(time.Millisecond)
			}
			if !deliberate {
				t.Fatal("observed loss did not retain a recovery candidate")
			}
		})
	}
}

func TestAwakeReconnectProofIsInvalidatedByExplicitStop(t *testing.T) {
	provider := &terminatingConversation{fakeConversation: newFakeConversation()}
	h := newHarnessWithConversation(t, provider)
	provider.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerStopped, Err: ports.ErrChatRecoveryInconclusive})
	_ = provider.Close()
	h.ctrl.Wait()
	var target domain.AwakeAgentTarget
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if list := h.svc.InterruptedAwakeAgents(); len(list) == 1 {
			target = list[0]
			break
		}
		time.Sleep(time.Millisecond)
	}
	if target.SessionID == "" {
		t.Fatal("missing interrupted proof")
	}
	if err := h.svc.Stop(context.Background(), testSession); err != nil {
		t.Fatal(err)
	}
	if len(h.svc.InterruptedAwakeAgents()) != 0 {
		t.Fatal("explicit stop retained recovery proof")
	}
	_, err := h.svc.Start(context.Background(), chatsvc.StartConfig{SessionID: testSession, Harness: domain.HarnessCodex, ReconnectOnly: true, AwakeReconnectTarget: &target, ExpectedControllerOwner: target.Owner, ProviderConversationID: target.Owner.ProviderConversationID})
	if !errors.Is(err, ports.ErrChatRecoveryInconclusive) {
		t.Fatalf("stale proof reached provider startup: %v", err)
	}
}
