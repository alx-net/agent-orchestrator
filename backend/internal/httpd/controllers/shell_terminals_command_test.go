package controllers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	shelltermsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/shellterm"
)

type fakeSessionCommandRunner struct {
	got    *shelltermsvc.RunSessionCommandInput
	result shelltermsvc.SessionCommandResult
}

func (f *fakeSessionCommandRunner) RunSessionCommand(_ context.Context, in shelltermsvc.RunSessionCommandInput) (shelltermsvc.ShellTerminal, error) {
	f.got = &in
	f.result.SessionID, f.result.Command = in.SessionID, in.Command
	in.OnDone(f.result)
	return f.result.Terminal, nil
}

type fakeSessionCommandMessenger struct {
	sessionID domain.SessionID
	msg       ports.ChatUserMessage
}

func (f *fakeSessionCommandMessenger) Send(_ context.Context, id domain.SessionID, msg ports.ChatUserMessage) (domain.ConversationTurn, error) {
	f.sessionID, f.msg = id, msg
	return domain.ConversationTurn{}, nil
}

func runSessionCommandRequest(t *testing.T, c *controllers.ShellTerminalsController, origin string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	c.Register(r)
	req := httptest.NewRequest(http.MethodPost, "/sessions/kugelaudio-2/terminal-commands", strings.NewReader(`{"command":"scripts/plan"}`))
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestRunSessionCommandHandsTheResultToTheAgent(t *testing.T) {
	runner := &fakeSessionCommandRunner{result: shelltermsvc.SessionCommandResult{
		Terminal: shelltermsvc.ShellTerminal{HandleID: "shellterm-run1", Title: "Run: scripts/plan"},
		Finished: true, ExitCode: 0, Output: "No changes.",
	}}
	messenger := &fakeSessionCommandMessenger{}
	rec := runSessionCommandRequest(t, &controllers.ShellTerminalsController{Commands: runner, Messages: messenger}, "app://renderer")
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"handleId":"shellterm-run1"`) {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if runner.got.SessionID != "kugelaudio-2" || runner.got.Command != "scripts/plan" {
		t.Fatalf("ran %+v", runner.got)
	}
	if messenger.sessionID != "kugelaudio-2" || messenger.msg.Origin != domain.MessageOriginHuman ||
		messenger.msg.ClientMessageID != "terminal-command:shellterm-run1" || !strings.Contains(messenger.msg.Text, "No changes.") {
		t.Fatalf("sent %s %+v", messenger.sessionID, messenger.msg)
	}
}

// Agents call the daemon as native clients, without an Origin; the Run button
// must not become a way around the guards on their own shell.
func TestRunSessionCommandRefusesNativeClients(t *testing.T) {
	runner := &fakeSessionCommandRunner{}
	rec := runSessionCommandRequest(t, &controllers.ShellTerminalsController{Commands: runner}, "")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "SESSION_COMMAND_DESKTOP_ONLY") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if runner.got != nil {
		t.Fatal("ran a command for a native client")
	}
}
