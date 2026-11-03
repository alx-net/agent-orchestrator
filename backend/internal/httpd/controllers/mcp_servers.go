package controllers

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/mcpservers"
)

// MCPServerService is the HTTP boundary of the MCP server catalog.
type MCPServerService interface {
	List(context.Context, mcpservers.ListInput) (mcpservers.List, error)
	Add(context.Context, mcpservers.AddInput) (mcpservers.Propagation, error)
	SetKey(context.Context, string, map[string]string) (mcpservers.Propagation, error)
	Remove(context.Context, string) (mcpservers.Propagation, error)
	Approve(context.Context, mcpservers.TargetInput) (mcpservers.Propagation, error)
	Test(context.Context, mcpservers.TargetInput) (mcpservers.TestResult, error)
	Connect(context.Context, mcpservers.TargetInput) (mcpservers.Operation, error)
	GetOperation(string) (mcpservers.Operation, error)
	CancelOperation(string) (mcpservers.Operation, error)
	SessionServers(context.Context, domain.SessionID, bool) (mcpservers.SessionList, error)
	Reconnect(context.Context, domain.SessionID, string) (mcpservers.SessionList, error)
}

// MCPServersController exposes MCP server status, sign-in, tests and changes.
type MCPServersController struct {
	Svc MCPServerService
}

// Register adds the MCP server routes.
func (c *MCPServersController) Register(r chi.Router) {
	r.Get("/mcp/servers", c.list)
	r.Post("/mcp/servers", c.add)
	r.Patch("/mcp/servers/{serverName}", c.setKey)
	r.Delete("/mcp/servers/{serverName}", c.remove)
	r.Post("/mcp/approve", c.approve)
	r.Post("/mcp/test", c.test)
	r.Post("/mcp/connect", c.connect)
	r.Get("/mcp/connect/{operationId}", c.operation)
	r.Post("/mcp/connect/{operationId}/cancel", c.cancel)
	r.Get("/sessions/{sessionId}/mcp-servers", c.sessionServers)
	r.Post("/sessions/{sessionId}/mcp-servers/{serverName}/reconnect", c.reconnect)
}

func queryFlag(r *http.Request, name string) bool {
	value := r.URL.Query().Get(name)
	return value == "true" || value == "1"
}

func (c *MCPServersController) list(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/mcp/servers")
		return
	}
	list, err := c.Svc.List(r.Context(), mcpservers.ListInput{
		Refresh: queryFlag(r, "refresh"), Wait: queryFlag(r, "wait"), NoCheck: queryFlag(r, "noCheck"),
	})
	if err != nil {
		writeMCPServerError(w, r, err)
		return
	}
	out := MCPServersResponse{Servers: mcpServerPayloads(list.Servers), Accounts: make([]MCPAccountRef, 0, len(list.Accounts)), Checking: list.Checking}
	for _, account := range list.Accounts {
		out.Accounts = append(out.Accounts, MCPAccountRef{ID: account.ID, Email: account.Email, Device: account.Device})
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *MCPServersController) add(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/mcp/servers")
		return
	}
	var in AddMCPServerRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	applied, err := c.Svc.Add(r.Context(), mcpservers.AddInput{
		Name: in.Name, URL: in.URL, Transport: in.Transport, Command: in.Command, Args: in.Args, Env: in.Env, Headers: in.Headers,
	})
	if err != nil {
		writeMCPServerError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusCreated, MCPChangeResponse{Applied: mcpPropagationPayload(applied)})
}

func (c *MCPServersController) setKey(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "PATCH", "/api/v1/mcp/servers/{serverName}")
		return
	}
	var in UpdateMCPServerKeyRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	applied, err := c.Svc.SetKey(r.Context(), chi.URLParam(r, "serverName"), in.Headers)
	if err != nil {
		writeMCPServerError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, MCPChangeResponse{Applied: mcpPropagationPayload(applied)})
}

func (c *MCPServersController) remove(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "DELETE", "/api/v1/mcp/servers/{serverName}")
		return
	}
	applied, err := c.Svc.Remove(r.Context(), chi.URLParam(r, "serverName"))
	if err != nil {
		writeMCPServerError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, MCPChangeResponse{Applied: mcpPropagationPayload(applied)})
}

func decodeMCPTarget(w http.ResponseWriter, r *http.Request) (mcpservers.TargetInput, bool) {
	var in MCPServerTargetRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return mcpservers.TargetInput{}, false
	}
	return mcpservers.TargetInput{Name: in.Name, SessionID: in.SessionID, AccountID: in.AccountID, ProjectID: in.ProjectID}, true
}

func (c *MCPServersController) approve(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/mcp/approve")
		return
	}
	in, ok := decodeMCPTarget(w, r)
	if !ok {
		return
	}
	applied, err := c.Svc.Approve(r.Context(), in)
	if err != nil {
		writeMCPServerError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, MCPChangeResponse{Applied: mcpPropagationPayload(applied)})
}

func (c *MCPServersController) test(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/mcp/test")
		return
	}
	in, ok := decodeMCPTarget(w, r)
	if !ok {
		return
	}
	result, err := c.Svc.Test(r.Context(), in)
	if err != nil {
		writeMCPServerError(w, r, err)
		return
	}
	out := MCPServerTestResponse{
		OK: result.OK, Name: result.Name, AccountID: result.AccountID, Status: result.Status, RawStatus: result.RawStatus,
		Reason: result.Reason, Error: result.Error, ToolCount: result.ToolCount, Tools: result.Tools, Auth: result.Auth,
		Action: result.Action, CheckedAt: result.CheckedAt,
	}
	if result.Live != nil {
		out.Live = &MCPLiveStatus{Status: result.Live.Status, ToolCount: result.Live.ToolCount, Error: result.Live.Error}
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *MCPServersController) connect(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/mcp/connect")
		return
	}
	in, ok := decodeMCPTarget(w, r)
	if !ok {
		return
	}
	op, err := c.Svc.Connect(r.Context(), in)
	if err != nil {
		writeMCPServerError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusAccepted, MCPConnectOperationResponse{Operation: mcpOperationPayload(op)})
}

func (c *MCPServersController) operation(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/mcp/connect/{operationId}")
		return
	}
	op, err := c.Svc.GetOperation(chi.URLParam(r, "operationId"))
	if err != nil {
		writeMCPServerError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, MCPConnectOperationResponse{Operation: mcpOperationPayload(op)})
}

func (c *MCPServersController) cancel(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/mcp/connect/{operationId}/cancel")
		return
	}
	op, err := c.Svc.CancelOperation(chi.URLParam(r, "operationId"))
	if err != nil {
		writeMCPServerError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, MCPConnectOperationResponse{Operation: mcpOperationPayload(op)})
}

func (c *MCPServersController) sessionServers(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/sessions/{sessionId}/mcp-servers")
		return
	}
	list, err := c.Svc.SessionServers(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")), queryFlag(r, "refresh"))
	if err != nil {
		writeMCPServerError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, SessionMCPServersResponse{Servers: mcpServerPayloads(list.Servers), AccountID: list.AccountID, Live: list.Live})
}

func (c *MCPServersController) reconnect(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/sessions/{sessionId}/mcp-servers/{serverName}/reconnect")
		return
	}
	list, err := c.Svc.Reconnect(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")), chi.URLParam(r, "serverName"))
	if err != nil {
		writeMCPServerError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, SessionMCPServersResponse{Servers: mcpServerPayloads(list.Servers), AccountID: list.AccountID, Live: list.Live})
}

func mcpServerPayloads(servers []mcpservers.Server) []MCPServerPayload {
	out := make([]MCPServerPayload, 0, len(servers))
	for _, server := range servers {
		payload := MCPServerPayload{
			Name: server.Name, Transport: server.Transport, Target: server.Target, Scope: server.Scope,
			ProjectID: server.ProjectID, ProjectName: server.ProjectName, Auth: server.Auth,
			HeaderNames: server.HeaderNames, Editable: server.Editable, ManageURL: server.ManageURL,
			Accounts: make([]MCPServerAccountStatus, 0, len(server.Accounts)),
		}
		for _, status := range server.Accounts {
			payload.Accounts = append(payload.Accounts, MCPServerAccountStatus{
				AccountID: status.AccountID, Status: status.Status, RawStatus: status.RawStatus, ToolCount: status.ToolCount,
				Error: status.Error, CheckedAt: status.CheckedAt, Action: status.Action, Live: status.Live,
			})
		}
		out = append(out, payload)
	}
	return out
}

func mcpPropagationPayload(applied mcpservers.Propagation) MCPPropagation {
	return MCPPropagation{InPlace: applied.InPlace, Restarting: applied.Restarting, Failed: applied.Failed}
}

func mcpOperationPayload(op mcpservers.Operation) MCPConnectOperation {
	var finished *time.Time
	if op.FinishedAt != nil {
		at := *op.FinishedAt
		finished = &at
	}
	return MCPConnectOperation{
		ID: op.ID, Name: op.Name, AccountID: op.AccountID, SessionID: op.SessionID, State: op.State,
		AuthURL: op.AuthURL, Error: op.Error, ManageURL: op.ManageURL, Applied: mcpPropagationPayload(op.Applied),
		StartedAt: op.StartedAt, FinishedAt: finished,
	}
}

func writeMCPServerError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, mcpservers.ErrInvalid):
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation", "MCP_SERVER_INVALID", err.Error(), nil)
	case errors.Is(err, mcpservers.ErrServerExists):
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict", "MCP_SERVER_EXISTS", err.Error(), nil)
	case errors.Is(err, mcpservers.ErrNotEditable):
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict", "MCP_SERVER_NOT_EDITABLE", err.Error(), nil)
	case errors.Is(err, mcpservers.ErrUnknownServer):
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "MCP_SERVER_NOT_FOUND", err.Error(), nil)
	case errors.Is(err, mcpservers.ErrUnknownOperation):
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "MCP_OPERATION_NOT_FOUND", "Unknown or finished sign-in", nil)
	case errors.Is(err, mcpservers.ErrUnknownSession):
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "SESSION_NOT_FOUND", "Unknown session", nil)
	case errors.Is(err, ports.ErrAgentAccountUnknown):
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "AGENT_ACCOUNT_NOT_FOUND", "Unknown agent account", nil)
	case errors.Is(err, mcpservers.ErrHarnessUnsupported):
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict", "MCP_HARNESS_UNSUPPORTED", err.Error(), nil)
	case errors.Is(err, mcpservers.ErrUnavailable):
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict", "MCP_UNAVAILABLE", err.Error(), nil)
	default:
		envelope.WriteError(w, r, err)
	}
}
