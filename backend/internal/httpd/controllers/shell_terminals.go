package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	shelltermsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/shellterm"
)

// ShellTerminalService is the controller-facing standalone shell terminal
// contract.
type ShellTerminalService interface {
	OpenShellTerminal(ctx context.Context, in shelltermsvc.OpenShellTerminalInput) (shelltermsvc.ShellTerminal, error)
	ListShellTerminalsForCurrentAppRun(ctx context.Context) ([]shelltermsvc.ShellTerminal, error)
	RenameShellTerminal(ctx context.Context, handleID, title string) (shelltermsvc.ShellTerminal, error)
	CloseShellTerminal(ctx context.Context, handleID string) error
}

// SessionCommandRunner runs a command the user picked from an agent's reply.
type SessionCommandRunner interface {
	RunSessionCommand(ctx context.Context, in shelltermsvc.RunSessionCommandInput) (shelltermsvc.ShellTerminal, error)
}

// SessionCommandMessenger hands a finished command's result to the agent.
type SessionCommandMessenger interface {
	Send(ctx context.Context, id domain.SessionID, msg ports.ChatUserMessage) (domain.ConversationTurn, error)
}

// ShellTerminalsController owns the /shell-terminals routes: standalone shells
// the user opens by hand, independent of any agent session.
type ShellTerminalsController struct {
	Svc ShellTerminalService
	// Commands and Messages back the Run button on agents' code blocks.
	Commands SessionCommandRunner
	Messages SessionCommandMessenger
	Log      *slog.Logger
}

// Register mounts the bounded shell terminal REST routes.
func (c *ShellTerminalsController) Register(r chi.Router) {
	r.Get("/shell-terminals", c.list)
	r.Post("/shell-terminals", c.open)
	r.Patch("/shell-terminals/{handleId}", c.rename)
	r.Delete("/shell-terminals/{handleId}", c.close)
	r.Post("/sessions/{sessionId}/terminal-commands", c.runSessionCommand)
}

// runSessionCommand serves the Run button. It runs whatever command it is
// given as the user, so it only accepts browser requests from an allowed
// renderer origin (the CORS middleware rejects other origins): agents and
// other native clients send no Origin and cannot use it to get around the
// guards on their own shell.
func (c *ShellTerminalsController) runSessionCommand(w http.ResponseWriter, r *http.Request) {
	if c.Commands == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/sessions/{sessionId}/terminal-commands")
		return
	}
	if r.Header.Get("Origin") == "" {
		envelope.WriteAPIError(w, r, http.StatusForbidden, "forbidden", "SESSION_COMMAND_DESKTOP_ONLY",
			"Only the desktop app can run a command for you", nil)
		return
	}
	var req RunSessionCommandRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	if strings.TrimSpace(req.Command) == "" {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation", "SESSION_COMMAND_REQUIRED", "A command is required", nil)
		return
	}
	sessionID := domain.SessionID(chi.URLParam(r, "sessionId"))
	terminal, err := c.Commands.RunSessionCommand(r.Context(), shelltermsvc.RunSessionCommandInput{
		SessionID: sessionID,
		Shell:     req.Shell,
		Command:   req.Command,
		OnDone:    c.deliverSessionCommandResult,
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusCreated, ShellTerminalEnvelope{
		ShellTerminal: shellTerminalResponse(terminal),
	})
}

// deliverSessionCommandResult queues the result like a message the user sent,
// since the user chose to run the command.
func (c *ShellTerminalsController) deliverSessionCommandResult(result shelltermsvc.SessionCommandResult) {
	if c.Messages == nil {
		return
	}
	_, err := c.Messages.Send(context.Background(), result.SessionID, ports.ChatUserMessage{
		Text:            result.AgentMessage(),
		ClientMessageID: "terminal-command:" + result.Terminal.HandleID,
		Origin:          domain.MessageOriginHuman,
	})
	if err != nil && c.Log != nil {
		c.Log.Warn("deliver terminal command result", "sessionId", result.SessionID, "handleId", result.Terminal.HandleID, "error", err)
	}
}

func (c *ShellTerminalsController) list(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/shell-terminals")
		return
	}
	terminals, err := c.Svc.ListShellTerminalsForCurrentAppRun(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ListShellTerminalsResponse{
		ShellTerminals: shellTerminalResponses(terminals),
	})
}

func (c *ShellTerminalsController) open(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/shell-terminals")
		return
	}
	// An empty body is a valid request: it means "open a shell with no project
	// context", which the service resolves to the daemon data dir.
	var req OpenShellTerminalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	terminal, err := c.Svc.OpenShellTerminal(r.Context(), shelltermsvc.OpenShellTerminalInput{
		ProjectID:     domain.ProjectID(req.ProjectID),
		SessionID:     domain.SessionID(req.SessionID),
		Shell:         req.Shell,
		StartOnAttach: req.StartOnAttach,
		Title:         req.Title,
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusCreated, ShellTerminalEnvelope{
		ShellTerminal: shellTerminalResponse(terminal),
	})
}

func (c *ShellTerminalsController) rename(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "PATCH", "/api/v1/shell-terminals/{handleId}")
		return
	}
	var req UpdateShellTerminalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	handleID, err := url.PathUnescape(chi.URLParam(r, "handleId"))
	if err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "SHELL_TERMINAL_ID_INVALID", "Invalid shell terminal id", nil)
		return
	}
	terminal, err := c.Svc.RenameShellTerminal(r.Context(), handleID, req.Title)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ShellTerminalEnvelope{
		ShellTerminal: shellTerminalResponse(terminal),
	})
}

func (c *ShellTerminalsController) close(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "DELETE", "/api/v1/shell-terminals/{handleId}")
		return
	}
	handleID, err := url.PathUnescape(chi.URLParam(r, "handleId"))
	if err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "SHELL_TERMINAL_ID_INVALID", "Invalid shell terminal id", nil)
		return
	}
	if err := c.Svc.CloseShellTerminal(r.Context(), handleID); err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func shellTerminalResponses(in []shelltermsvc.ShellTerminal) []ShellTerminalResponse {
	out := make([]ShellTerminalResponse, 0, len(in))
	for _, t := range in {
		out = append(out, shellTerminalResponse(t))
	}
	return out
}

func shellTerminalResponse(t shelltermsvc.ShellTerminal) ShellTerminalResponse {
	return ShellTerminalResponse{
		HandleID:   t.HandleID,
		ProjectID:  string(t.ProjectID),
		SessionID:  string(t.SessionID),
		WorkingDir: t.WorkingDir,
		Title:      t.Title,
		CreatedAt:  t.CreatedAt,
	}
}
