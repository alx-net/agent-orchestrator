package controllers

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	waitsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/wait"
)

// WaitService is the controller-facing `ao wait` contract.
type WaitService interface {
	Register(ctx context.Context, in waitsvc.RegisterInput) (waitsvc.RegisterResult, error)
	List(ctx context.Context, sessionID domain.SessionID) ([]domain.Wait, error)
	Cancel(ctx context.Context, sessionID domain.SessionID, id domain.WaitID) (domain.Wait, error)
}

// WaitsController owns a session's zero-token wait routes.
type WaitsController struct{ Svc WaitService }

// Register mounts the wait routes on r.
func (c *WaitsController) Register(r chi.Router) {
	r.Get("/sessions/{sessionId}/waits", c.list)
	r.Post("/sessions/{sessionId}/waits", c.create)
	r.Delete("/sessions/{sessionId}/waits/{waitId}", c.cancel)
}

func (c *WaitsController) create(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/sessions/{sessionId}/waits")
		return
	}
	var req CreateWaitRequest
	if err := decodeJSONStrict(r, &req); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	if req.DurationSeconds < 0 || req.TimeoutSeconds < 0 {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation", "WAIT_DURATION_INVALID", "Durations must not be negative", nil)
		return
	}
	res, err := c.Svc.Register(r.Context(), waitsvc.RegisterInput{
		SessionID: sessionID(r), Kind: domain.WaitKind(req.Kind),
		PRNumber: req.PRNumber, PRURL: req.PRURL, Condition: domain.WaitCondition(req.For),
		Duration: time.Duration(req.DurationSeconds) * time.Second,
		Timeout:  time.Duration(req.TimeoutSeconds) * time.Second,
		Note:     req.Note,
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	status := http.StatusCreated
	if res.Existing {
		status = http.StatusOK
	}
	envelope.WriteJSON(w, status, CreateWaitResponse{Wait: waitResponse(res.Wait), Existing: res.Existing})
}

func (c *WaitsController) list(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/sessions/{sessionId}/waits")
		return
	}
	waits, err := c.Svc.List(r.Context(), sessionID(r))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	out := ListWaitsResponse{Waits: make([]WaitResponse, 0, len(waits))}
	for _, wait := range waits {
		out.Waits = append(out.Waits, waitResponse(wait))
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *WaitsController) cancel(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "DELETE", "/api/v1/sessions/{sessionId}/waits/{waitId}")
		return
	}
	id, err := url.PathUnescape(chi.URLParam(r, "waitId"))
	if err != nil || id == "" {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "WAIT_ID_INVALID", "Invalid wait id", nil)
		return
	}
	wait, err := c.Svc.Cancel(r.Context(), sessionID(r), domain.WaitID(id))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, WaitEnvelope{Wait: waitResponse(wait)})
}

func waitResponse(w domain.Wait) WaitResponse {
	out := WaitResponse{
		ID: string(w.ID), SessionID: string(w.SessionID), Kind: string(w.Kind), For: string(w.Condition),
		PRNumber: w.PRNumber, PRURL: w.PRURL, Note: w.Note, State: string(w.State), Outcome: w.Outcome,
		CreatedAt: w.CreatedAt, ExpiresAt: w.ExpiresAt,
	}
	if !w.FireAt.IsZero() {
		at := w.FireAt
		out.FireAt = &at
	}
	if !w.ResolvedAt.IsZero() {
		at := w.ResolvedAt
		out.ResolvedAt = &at
	}
	return out
}
