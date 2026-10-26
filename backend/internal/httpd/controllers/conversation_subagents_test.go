package controllers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

// subagentStub answers the subagent routes and records what reached it.
type subagentStub struct {
	*fakeConversationService

	result  chatsvc.SubagentMessageResult
	err     error
	session domain.SessionID
	target  string
	texts   []string
	stops   int
}

func (s *subagentStub) MessageSubagent(
	_ context.Context,
	session domain.SessionID,
	toolUseID, text string,
) (chatsvc.SubagentMessageResult, error) {
	s.session, s.target = session, toolUseID
	s.texts = append(s.texts, text)
	return s.result, s.err
}

func (s *subagentStub) StopSubagent(_ context.Context, session domain.SessionID, toolUseID string) error {
	s.session, s.target = session, toolUseID
	s.stops++
	return s.err
}

func postSubagent(t *testing.T, svc controllers.ConversationService, path, body string) (int, map[string]any) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{
		Sessions:      newFakeSessionService(),
		Conversations: svc,
	}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)
	resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var decoded map[string]any
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode %s: %v (%s)", path, err, raw)
	}
	return resp.StatusCode, decoded
}

// The toolUseId is the Agent/Task activity's providerItemId, which a scoped ACP
// conversation builds with colons; the client percent-encodes it.
const scopedToolUseID = "acp:5:scope11:toolu_agent"

var (
	subagentMessagesURL = "/api/v1/sessions/p1-1/conversation/subagents/" + strings.ReplaceAll(scopedToolUseID, ":", "%3A") + "/messages"
	subagentStopURL     = "/api/v1/sessions/p1-1/conversation/subagents/" + strings.ReplaceAll(scopedToolUseID, ":", "%3A") + "/stop"
)

func TestSubagentMessageRouteQueuesForTheNamedSubagent(t *testing.T) {
	svc := &subagentStub{
		fakeConversationService: &fakeConversationService{},
		result:                  chatsvc.SubagentMessageResult{ID: "msg-1", State: "queued"},
	}
	status, body := postSubagent(t, svc, subagentMessagesURL, `{"text":"Skip the tests folder."}`)
	if status != http.StatusAccepted || body["id"] != "msg-1" || body["state"] != "queued" {
		t.Fatalf("status=%d body=%v", status, body)
	}
	if svc.session != "p1-1" || svc.target != scopedToolUseID || len(svc.texts) != 1 || svc.texts[0] != "Skip the tests folder." {
		t.Fatalf("service saw session=%q target=%q texts=%q", svc.session, svc.target, svc.texts)
	}
}

func TestSubagentStopRouteStopsTheNamedSubagent(t *testing.T) {
	svc := &subagentStub{fakeConversationService: &fakeConversationService{}}
	status, body := postSubagent(t, svc, subagentStopURL, ``)
	if status != http.StatusAccepted || body["ok"] != true {
		t.Fatalf("status=%d body=%v", status, body)
	}
	if svc.stops != 1 || svc.session != "p1-1" || svc.target != scopedToolUseID {
		t.Fatalf("service saw stops=%d session=%q target=%q", svc.stops, svc.session, svc.target)
	}
}

func TestSubagentMessageRouteRejectsBadBodiesBeforeTheService(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
	}{
		{"empty text", `{"text":"   "}`, "CHAT_SUBAGENT_TEXT_REQUIRED"},
		{"missing text", `{}`, "CHAT_SUBAGENT_TEXT_REQUIRED"},
		{"too long", fmt.Sprintf(`{"text":%q}`, strings.Repeat("a", 8<<10+1)), "CHAT_SUBAGENT_TEXT_TOO_LONG"},
		{"not JSON", `{"text":`, "INVALID_BODY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &subagentStub{fakeConversationService: &fakeConversationService{}}
			status, body := postSubagent(t, svc, subagentMessagesURL, tc.body)
			if status != http.StatusBadRequest || body["code"] != tc.code || body["error"] != "validation" {
				t.Fatalf("status=%d body=%v, want 400 %s", status, body, tc.code)
			}
			if len(svc.texts) != 0 {
				t.Fatalf("service was called with %q", svc.texts)
			}
		})
	}
}

func TestSubagentRoutesMapRefusalsOntoStableCodes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		kind   string
		code   string
	}{
		{"not running", fmt.Errorf("ACP: %w", chatsvc.ErrSubagentNotRunning), http.StatusConflict, "conflict", "CHAT_SUBAGENT_NOT_RUNNING"},
		{"not a Claude chat", chatsvc.ErrSubagentsUnsupported, http.StatusUnprocessableEntity, "unprocessable", "CHAT_SUBAGENTS_UNSUPPORTED"},
		{"terminal session", chatsvc.ErrNotChatMode, http.StatusConflict, "conflict", "SESSION_MODE_MISMATCH"},
		{"no live controller", chatsvc.ErrNoController, http.StatusConflict, "conflict", "CHAT_CONTROLLER_NOT_READY"},
		{"unknown session", ports.ErrSessionNotFound, http.StatusNotFound, "not_found", "SESSION_NOT_FOUND"},
		{"empty after trim", chatsvc.ErrSubagentTextRequired, http.StatusBadRequest, "validation", "CHAT_SUBAGENT_TEXT_REQUIRED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for path, body := range map[string]string{subagentMessagesURL: `{"text":"hello"}`, subagentStopURL: ``} {
				svc := &subagentStub{fakeConversationService: &fakeConversationService{}, err: tc.err}
				status, decoded := postSubagent(t, svc, path, body)
				if status != tc.status || decoded["error"] != tc.kind || decoded["code"] != tc.code {
					t.Fatalf("%s: status=%d body=%v, want %d %s", path, status, decoded, tc.status, tc.code)
				}
				if _, ok := decoded["requestId"]; !ok {
					t.Fatalf("%s: error envelope lost its request id: %v", path, decoded)
				}
			}
		})
	}
}

func TestSubagentRoutesAreNotImplementedWithoutTheService(t *testing.T) {
	for path, body := range map[string]string{subagentMessagesURL: `{"text":"hello"}`, subagentStopURL: ``} {
		status, decoded := postSubagent(t, &fakeConversationService{}, path, body)
		if status != http.StatusNotImplemented {
			t.Fatalf("%s: status=%d body=%v, want 501", path, status, decoded)
		}
	}
}
