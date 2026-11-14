package controllers

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/sessioncheckout"
)

// CheckoutService is the controller-facing `ao checkout` contract.
type CheckoutService interface {
	Checkout(ctx context.Context, in sessioncheckout.Input) (sessioncheckout.Output, error)
}

// CheckoutsController owns a session's project-checkout route.
type CheckoutsController struct{ Svc CheckoutService }

// Register mounts the checkout route on r.
func (c *CheckoutsController) Register(r chi.Router) {
	r.Post("/sessions/{sessionId}/checkouts", c.create)
}

func (c *CheckoutsController) create(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/sessions/{sessionId}/checkouts")
		return
	}
	var req CreateCheckoutRequest
	if err := decodeJSONStrict(r, &req); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	out, err := c.Svc.Checkout(r.Context(), sessioncheckout.Input{
		SessionID: sessionID(r), Project: req.Project, Ref: req.Ref, Refresh: req.Refresh,
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	status := http.StatusOK
	if out.Action == sessioncheckout.ActionCreated {
		status = http.StatusCreated
	}
	envelope.WriteJSON(w, status, CheckoutResponse{
		ProjectID: out.Project.ID, ProjectName: sessioncheckout.DisplayName(out.Project),
		Path: out.Path, Ref: out.Ref, Commit: out.Commit, ShortCommit: out.ShortCommit,
		CommitDate: out.CommitDate, Subject: out.Subject, Action: string(out.Action),
		Note: out.Note, FetchWarning: out.FetchWarning, Submodules: out.Submodules,
	})
}
