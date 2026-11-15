package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/mcpservers"
)

type fakeMCPService struct {
	err     error
	list    mcpservers.List
	input   mcpservers.ListInput
	added   mcpservers.AddInput
	target  mcpservers.TargetInput
	key     map[string]string
	removed string
	session domain.SessionID
	refresh bool
}

func (f *fakeMCPService) List(_ context.Context, in mcpservers.ListInput) (mcpservers.List, error) {
	f.input = in
	return f.list, f.err
}

func (f *fakeMCPService) Add(_ context.Context, in mcpservers.AddInput) (mcpservers.Propagation, error) {
	f.added = in
	return mcpservers.Propagation{InPlace: []string{"s1"}}, f.err
}

func (f *fakeMCPService) SetKey(_ context.Context, _ string, headers map[string]string) (mcpservers.Propagation, error) {
	f.key = headers
	return mcpservers.Propagation{}, f.err
}

func (f *fakeMCPService) Remove(_ context.Context, name string) (mcpservers.Propagation, error) {
	f.removed = name
	return mcpservers.Propagation{Restarting: []string{"s1"}}, f.err
}

func (f *fakeMCPService) Approve(_ context.Context, in mcpservers.TargetInput) (mcpservers.Propagation, error) {
	f.target = in
	return mcpservers.Propagation{}, f.err
}

func (f *fakeMCPService) Test(_ context.Context, in mcpservers.TargetInput) (mcpservers.TestResult, error) {
	f.target = in
	return mcpservers.TestResult{OK: true, Name: in.Name, Reason: "Connected.", Live: &mcpservers.LiveStatus{Status: "connected"}}, f.err
}

func (f *fakeMCPService) Connect(_ context.Context, in mcpservers.TargetInput) (mcpservers.Operation, error) {
	f.target = in
	return mcpservers.Operation{ID: "op1", Name: in.Name, State: mcpservers.OpWaiting, StartedAt: time.Unix(0, 0)}, f.err
}

func (f *fakeMCPService) GetOperation(string) (mcpservers.Operation, error) {
	return mcpservers.Operation{ID: "op1", State: mcpservers.OpConnected}, f.err
}

func (f *fakeMCPService) CancelOperation(string) (mcpservers.Operation, error) {
	return mcpservers.Operation{ID: "op1", State: mcpservers.OpCancelled}, f.err
}

func (f *fakeMCPService) SessionServers(_ context.Context, id domain.SessionID, refresh bool) (mcpservers.SessionList, error) {
	f.session, f.refresh = id, refresh
	return mcpservers.SessionList{AccountID: "a", Live: true}, f.err
}

func (f *fakeMCPService) Reconnect(_ context.Context, id domain.SessionID, name string) (mcpservers.SessionList, error) {
	f.session, f.removed = id, name
	return mcpservers.SessionList{AccountID: "a", Live: true}, f.err
}

func mcpRequest(t *testing.T, svc MCPServerService, method, path, body string) (int, map[string]any) {
	t.Helper()
	router := chi.NewRouter()
	(&MCPServersController{Svc: svc}).Register(router)
	srv := httptest.NewServer(router)
	defer srv.Close()
	req, err := http.NewRequestWithContext(context.Background(), method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if len(data) > 0 {
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("decode %s: %v", data, err)
		}
	}
	return resp.StatusCode, out
}

func TestMCPListPassesItsFreshnessFlagsAndShapesTheCatalog(t *testing.T) {
	count := 3
	at := time.Unix(100, 0).UTC()
	svc := &fakeMCPService{list: mcpservers.List{
		Checking: true,
		Accounts: []mcpservers.AccountRef{{ID: "device-claude", Device: true}},
		Servers: []mcpservers.Server{{
			Name: "linear", Transport: "http", Scope: mcpservers.ScopeGlobal, Auth: mcpservers.AuthOAuth,
			Accounts: []mcpservers.AccountStatus{{AccountID: "device-claude", Status: mcpservers.StatusConnected, ToolCount: &count, CheckedAt: &at}},
		}},
	}}
	status, body := mcpRequest(t, svc, http.MethodGet, "/mcp/servers?refresh=true&wait=1", "")
	if status != http.StatusOK || !svc.input.Refresh || !svc.input.Wait || svc.input.NoCheck {
		t.Fatalf("status %d input %+v", status, svc.input)
	}
	if body["checking"] != true {
		t.Fatalf("body = %v", body)
	}
	servers, _ := body["servers"].([]any)
	server, _ := servers[0].(map[string]any)
	accounts, _ := server["accounts"].([]any)
	account, _ := accounts[0].(map[string]any)
	if server["name"] != "linear" || account["status"] != "connected" || account["toolCount"] != float64(3) {
		t.Fatalf("server = %v", server)
	}
}

func TestMCPAddTakesHeadersAndReportsWhereItLanded(t *testing.T) {
	svc := &fakeMCPService{}
	status, body := mcpRequest(t, svc, http.MethodPost, "/mcp/servers", `{"name":"acme","url":"https://a.test","headers":{"X-Api-Key":"k"}}`)
	if status != http.StatusCreated || svc.added.Name != "acme" || svc.added.Headers["X-Api-Key"] != "k" {
		t.Fatalf("status %d added %+v", status, svc.added)
	}
	applied, _ := body["applied"].(map[string]any)
	if inPlace, _ := applied["inPlace"].([]any); len(inPlace) != 1 {
		t.Fatalf("body = %v", body)
	}
	if status, _ := mcpRequest(t, svc, http.MethodPost, "/mcp/servers", `{"name":"x","nonsense":1}`); status != http.StatusBadRequest {
		t.Fatalf("an unknown field: status %d", status)
	}
}

func TestMCPTargetsReachTheService(t *testing.T) {
	svc := &fakeMCPService{}
	want := mcpservers.TargetInput{Name: "x", SessionID: "s1", AccountID: "a", ProjectID: "p"}
	body := `{"name":"x","sessionId":"s1","accountId":"a","projectId":"p"}`
	if status, out := mcpRequest(t, svc, http.MethodPost, "/mcp/connect", body); status != http.StatusAccepted || svc.target != want {
		t.Fatalf("connect: status %d target %+v body %v", status, svc.target, out)
	}
	svc.target = mcpservers.TargetInput{}
	if status, _ := mcpRequest(t, svc, http.MethodPost, "/mcp/approve", body); status != http.StatusOK || svc.target != want {
		t.Fatalf("approve: status %d target %+v", status, svc.target)
	}
	svc.target = mcpservers.TargetInput{}
	status, out := mcpRequest(t, svc, http.MethodPost, "/mcp/test", body)
	if status != http.StatusOK || svc.target != want || out["ok"] != true {
		t.Fatalf("test: status %d body %v", status, out)
	}
	if live, _ := out["live"].(map[string]any); live["status"] != "connected" {
		t.Fatalf("test live = %v", out["live"])
	}
}

func TestMCPOperationRoutes(t *testing.T) {
	svc := &fakeMCPService{}
	status, body := mcpRequest(t, svc, http.MethodGet, "/mcp/connect/op1", "")
	operation, _ := body["operation"].(map[string]any)
	if status != http.StatusOK || operation["state"] != mcpservers.OpConnected {
		t.Fatalf("get: status %d body %v", status, body)
	}
	status, body = mcpRequest(t, svc, http.MethodPost, "/mcp/connect/op1/cancel", "")
	operation, _ = body["operation"].(map[string]any)
	if status != http.StatusOK || operation["state"] != mcpservers.OpCancelled {
		t.Fatalf("cancel: status %d body %v", status, body)
	}
}

func TestMCPSessionRoutes(t *testing.T) {
	svc := &fakeMCPService{}
	status, body := mcpRequest(t, svc, http.MethodGet, "/sessions/s1/mcp-servers?refresh=true", "")
	if status != http.StatusOK || svc.session != "s1" || !svc.refresh || body["live"] != true {
		t.Fatalf("list: status %d body %v", status, body)
	}
	status, _ = mcpRequest(t, svc, http.MethodPost, "/sessions/s2/mcp-servers/linear/reconnect", "")
	if status != http.StatusOK || svc.session != "s2" || svc.removed != "linear" {
		t.Fatalf("reconnect: status %d session %s name %s", status, svc.session, svc.removed)
	}
}

func TestMCPKeyAndRemoveRoutes(t *testing.T) {
	svc := &fakeMCPService{}
	if status, _ := mcpRequest(t, svc, http.MethodPatch, "/mcp/servers/acme", `{"headers":{"K":"v"}}`); status != http.StatusOK || svc.key["K"] != "v" {
		t.Fatalf("key: status %d key %v", status, svc.key)
	}
	status, body := mcpRequest(t, svc, http.MethodDelete, "/mcp/servers/acme", "")
	if status != http.StatusOK || svc.removed != "acme" {
		t.Fatalf("remove: status %d removed %s", status, svc.removed)
	}
	applied, _ := body["applied"].(map[string]any)
	if restarting, _ := applied["restarting"].([]any); len(restarting) != 1 {
		t.Fatalf("body = %v", body)
	}
}

func TestMCPErrorsCarryTheirCodes(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{mcpservers.ErrInvalid, http.StatusBadRequest, "MCP_SERVER_INVALID"},
		{mcpservers.ErrServerExists, http.StatusConflict, "MCP_SERVER_EXISTS"},
		{mcpservers.ErrNoSignIn, http.StatusConflict, "MCP_SERVER_NO_SIGN_IN"},
		{mcpservers.ErrNotEditable, http.StatusConflict, "MCP_SERVER_NOT_EDITABLE"},
		{mcpservers.ErrUnknownServer, http.StatusNotFound, "MCP_SERVER_NOT_FOUND"},
		{mcpservers.ErrUnknownOperation, http.StatusNotFound, "MCP_OPERATION_NOT_FOUND"},
		{mcpservers.ErrUnknownSession, http.StatusNotFound, "SESSION_NOT_FOUND"},
		{mcpservers.ErrHarnessUnsupported, http.StatusConflict, "MCP_HARNESS_UNSUPPORTED"},
		{mcpservers.ErrUnavailable, http.StatusConflict, "MCP_UNAVAILABLE"},
	} {
		status, body := mcpRequest(t, &fakeMCPService{err: tc.err}, http.MethodPost, "/mcp/connect", `{"name":"x"}`)
		if status != tc.status || body["code"] != tc.code {
			t.Errorf("%v: status %d body %v", tc.err, status, body)
		}
	}
	if status, _ := mcpRequest(t, &fakeMCPService{err: errors.New("boom")}, http.MethodPost, "/mcp/connect", `{"name":"x"}`); status != http.StatusInternalServerError {
		t.Fatalf("an unexpected error: status %d", status)
	}
}

func TestMCPRoutesReportWhenTheServiceIsMissing(t *testing.T) {
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/mcp/servers"},
		{http.MethodPost, "/mcp/connect"},
		{http.MethodGet, "/sessions/s1/mcp-servers"},
	} {
		if status, _ := mcpRequest(t, nil, tc.method, tc.path, "{}"); status != http.StatusNotImplemented {
			t.Errorf("%s %s: status %d", tc.method, tc.path, status)
		}
	}
}
