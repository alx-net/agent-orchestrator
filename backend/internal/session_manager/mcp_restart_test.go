package sessionmanager

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type recordingToolServerRestarter struct {
	*recordingLauncher
	calls []domain.SessionID
	done  bool
}

func (r *recordingToolServerRestarter) RestartChatForToolServers(_ context.Context, id domain.SessionID) (bool, error) {
	r.calls = append(r.calls, id)
	return r.done, nil
}

func TestRestartForToolServersAsksTheChatServiceOfARunningAgent(t *testing.T) {
	restarter := &recordingToolServerRestarter{recordingLauncher: &recordingLauncher{live: true}, done: true}
	mgr, store, _ := newChatManager(restarter)
	seedChatResumeSession(store, domain.ActivityIdle)
	done, err := mgr.RestartAgentForToolServers(context.Background(), "mer-1")
	if err != nil || !done {
		t.Fatalf("done = %v, err = %v", done, err)
	}
	if len(restarter.calls) != 1 || restarter.calls[0] != "mer-1" {
		t.Fatalf("calls = %v", restarter.calls)
	}
}

// A busy agent reports not done, so the caller asks again at its turn's end.
func TestRestartForToolServersReportsABusyAgent(t *testing.T) {
	restarter := &recordingToolServerRestarter{recordingLauncher: &recordingLauncher{live: true}, done: false}
	mgr, store, _ := newChatManager(restarter)
	seedChatResumeSession(store, domain.ActivityActive)
	done, err := mgr.RestartAgentForToolServers(context.Background(), "mer-1")
	if err != nil || done {
		t.Fatalf("done = %v, err = %v", done, err)
	}
}

// Another operation on the session (a resume, a kill, a move) goes first.
func TestRestartForToolServersWaitsForAnotherOperation(t *testing.T) {
	restarter := &recordingToolServerRestarter{recordingLauncher: &recordingLauncher{live: true}, done: true}
	mgr, store, _ := newChatManager(restarter)
	seedChatResumeSession(store, domain.ActivityIdle)
	ctx := context.Background()
	if err := mgr.beginAgentOperation(ctx, "mer-1", agentOperationResume); err != nil {
		t.Fatal(err)
	}
	defer mgr.endAgentOperation("mer-1", agentOperationResume)
	done, err := mgr.RestartAgentForToolServers(ctx, "mer-1")
	if err != nil || done {
		t.Fatalf("done = %v, err = %v", done, err)
	}
	if len(restarter.calls) != 0 {
		t.Fatalf("restarted under another operation: %v", restarter.calls)
	}
}

// No agent running means nothing to restart: its next start reads current servers.
func TestRestartForToolServersLeavesAStoppedOrGoneSessionAlone(t *testing.T) {
	restarter := &recordingToolServerRestarter{recordingLauncher: &recordingLauncher{live: true}}
	mgr, store, _ := newChatManager(restarter)
	seedChatResumeSession(store, domain.ActivityExited)
	ctx := context.Background()
	if done, err := mgr.RestartAgentForToolServers(ctx, "mer-1"); err != nil || !done {
		t.Fatalf("exited: done = %v, err = %v", done, err)
	}
	if done, err := mgr.RestartAgentForToolServers(ctx, "no-such-session"); err != nil || !done {
		t.Fatalf("gone: done = %v, err = %v", done, err)
	}
	if len(restarter.calls) != 0 {
		t.Fatalf("calls = %v", restarter.calls)
	}
}
