package controllers_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/sessioncheckout"
)

type fakeCheckoutService struct {
	input  sessioncheckout.Input
	action sessioncheckout.Action
	err    error
}

func (f *fakeCheckoutService) Checkout(_ context.Context, in sessioncheckout.Input) (sessioncheckout.Output, error) {
	f.input = in
	if f.err != nil {
		return sessioncheckout.Output{}, f.err
	}
	return sessioncheckout.Output{
		Project: domain.ProjectRecord{ID: "kugelaudio", DisplayName: "KugelAudio"},
		Result:  sessioncheckout.Result{Path: "/ws/repos/kugelaudio", Ref: "origin/master", Commit: "abc", ShortCommit: "abc", Action: f.action},
	}, nil
}

func TestCheckoutsAPI_CreateRefreshAndErrors(t *testing.T) {
	svc := &fakeCheckoutService{action: sessioncheckout.ActionCreated}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{Checkouts: svc}, httpd.ControlDeps{}))
	defer srv.Close()

	body, status, _ := doRequest(t, srv, "POST", "/api/v1/sessions/sa-1/checkouts", `{"project":"KugelAudio","ref":"v1","refresh":true}`)
	if status != http.StatusCreated || svc.input != (sessioncheckout.Input{SessionID: "sa-1", Project: "KugelAudio", Ref: "v1", Refresh: true}) {
		t.Fatalf("status=%d body=%s input=%+v", status, body, svc.input)
	}
	for _, want := range []string{`"projectId":"kugelaudio"`, `"projectName":"KugelAudio"`, `"path":"/ws/repos/kugelaudio"`, `"action":"created"`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("body missing %s: %s", want, body)
		}
	}

	svc.action = sessioncheckout.ActionUpdated
	if _, status, _ = doRequest(t, srv, "POST", "/api/v1/sessions/sa-1/checkouts", `{"project":"kugelaudio"}`); status != http.StatusOK {
		t.Fatalf("refresh status = %d", status)
	}
	if _, status, _ = doRequest(t, srv, "POST", "/api/v1/sessions/sa-1/checkouts", `{"project":"kugelaudio","bogus":1}`); status != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d", status)
	}
	svc.err = apierr.NotFound("CHECKOUT_PROJECT_NOT_FOUND", `No project "curb". Available: kugelaudio (KugelAudio)`)
	if body, status, _ = doRequest(t, srv, "POST", "/api/v1/sessions/sa-1/checkouts", `{"project":"curb"}`); status != http.StatusNotFound || !strings.Contains(string(body), "Available: kugelaudio") {
		t.Fatalf("not found status=%d body=%s", status, body)
	}
}
