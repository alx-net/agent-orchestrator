package controllers_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	waitsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/wait"
)

type fakeWaitService struct {
	input     waitsvc.RegisterInput
	existing  bool
	cancelled domain.WaitID
	err       error
}

func (f *fakeWaitService) Register(_ context.Context, in waitsvc.RegisterInput) (waitsvc.RegisterResult, error) {
	f.input = in
	if f.err != nil {
		return waitsvc.RegisterResult{}, f.err
	}
	created := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	return waitsvc.RegisterResult{Existing: f.existing, Wait: domain.Wait{
		ID: "w-1", SessionID: in.SessionID, Kind: in.Kind, Condition: in.Condition, PRNumber: in.PRNumber,
		State: domain.WaitPending, CreatedAt: created, ExpiresAt: created.Add(in.Timeout),
	}}, nil
}

func (f *fakeWaitService) List(_ context.Context, id domain.SessionID) ([]domain.Wait, error) {
	return []domain.Wait{{ID: "w-1", SessionID: id, Kind: domain.WaitKindTime, State: domain.WaitPending, FireAt: time.Unix(60, 0).UTC()}}, f.err
}

func (f *fakeWaitService) Cancel(_ context.Context, _ domain.SessionID, id domain.WaitID) (domain.Wait, error) {
	f.cancelled = id
	if f.err != nil {
		return domain.Wait{}, f.err
	}
	return domain.Wait{ID: id, State: domain.WaitCancelled, ResolvedAt: time.Unix(5, 0).UTC()}, nil
}

func TestWaitsAPI_RegisterListCancel(t *testing.T) {
	svc := &fakeWaitService{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{Waits: svc}, httpd.ControlDeps{}))
	defer srv.Close()

	body, status, _ := doRequest(t, srv, "POST", "/api/v1/sessions/app-1/waits", `{"kind":"pr","prNumber":5807,"for":"merge","timeoutSeconds":3600,"note":"why"}`)
	if status != http.StatusCreated || svc.input.SessionID != "app-1" || svc.input.Kind != domain.WaitKindPR || svc.input.PRNumber != 5807 ||
		svc.input.Condition != domain.WaitForMerge || svc.input.Timeout != time.Hour || svc.input.Note != "why" {
		t.Fatalf("status=%d body=%s input=%+v", status, body, svc.input)
	}
	if !strings.Contains(string(body), `"id":"w-1"`) || !strings.Contains(string(body), `"for":"merge"`) || strings.Contains(string(body), "fireAt") {
		t.Fatalf("body = %s", body)
	}

	svc.existing = true
	if _, status, _ = doRequest(t, srv, "POST", "/api/v1/sessions/app-1/waits", `{"kind":"pr","prNumber":5807}`); status != http.StatusOK {
		t.Fatalf("existing wait status = %d", status)
	}
	if _, status, _ = doRequest(t, srv, "POST", "/api/v1/sessions/app-1/waits", `{"kind":"pr","prNumber":5807,"bogus":1}`); status != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d", status)
	}
	if _, status, _ = doRequest(t, srv, "POST", "/api/v1/sessions/app-1/waits", `{"kind":"time","durationSeconds":-1}`); status != http.StatusBadRequest {
		t.Fatalf("negative duration status = %d", status)
	}

	body, status, _ = doRequest(t, srv, "GET", "/api/v1/sessions/app-1/waits", "")
	if status != http.StatusOK || !strings.Contains(string(body), `"waits":[{`) || !strings.Contains(string(body), `"fireAt":"1970-01-01T00:01:00Z"`) {
		t.Fatalf("list status=%d body=%s", status, body)
	}

	body, status, _ = doRequest(t, srv, "DELETE", "/api/v1/sessions/app-1/waits/w-1", "")
	if status != http.StatusOK || svc.cancelled != "w-1" || !strings.Contains(string(body), `"state":"cancelled"`) {
		t.Fatalf("cancel status=%d body=%s", status, body)
	}
	svc.err = apierr.Conflict("WAIT_NOT_PENDING", "already resolved", nil)
	if body, status, _ = doRequest(t, srv, "DELETE", "/api/v1/sessions/app-1/waits/w-1", ""); status != http.StatusConflict || !strings.Contains(string(body), "WAIT_NOT_PENDING") {
		t.Fatalf("conflict status=%d body=%s", status, body)
	}
}
