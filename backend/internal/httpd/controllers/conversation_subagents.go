package controllers

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

const (
	subagentMessagesPath = "/api/v1/sessions/{sessionId}/conversation/subagents/{toolUseId}/messages"
	subagentStopPath     = "/api/v1/sessions/{sessionId}/conversation/subagents/{toolUseId}/stop"
	// maxSubagentMessageBytes keeps a message small enough that the last 20 a
	// subagent was sent fit the activity detail rewritten on every update.
	maxSubagentMessageBytes = 8 << 10
)

type subagentService interface {
	MessageSubagent(ctx context.Context, session domain.SessionID, toolUseID, text string) (chatsvc.SubagentMessageResult, error)
	StopSubagent(ctx context.Context, session domain.SessionID, toolUseID string) error
}

// messageSubagent queues a message for one running subagent.
//
// 202: the subagent reads it at its next tool call, not when this returns. Its
// state after that is reported on the spawning activity's detail.subagentInbox.
func (c *ConversationsController) messageSubagent(w http.ResponseWriter, r *http.Request) {
	svc, ok := c.Svc.(subagentService)
	if !ok {
		apispec.NotImplemented(w, r, http.MethodPost, subagentMessagesPath)
		return
	}
	toolUseID, ok := subagentToolUseID(w, r)
	if !ok {
		return
	}
	var req SendSubagentMessageRequest
	if !decodeConversationBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation",
			"CHAT_SUBAGENT_TEXT_REQUIRED", "subagent message text is required", nil)
		return
	}
	if len(req.Text) > maxSubagentMessageBytes {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation",
			"CHAT_SUBAGENT_TEXT_TOO_LONG", "subagent message text is longer than 8 KiB", nil)
		return
	}
	result, err := svc.MessageSubagent(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")), toolUseID, req.Text)
	if err != nil {
		writeSubagentError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusAccepted, SendSubagentMessageResponse{ID: result.ID, State: result.State})
}

// stopSubagent stops one running subagent and leaves the main turn running.
func (c *ConversationsController) stopSubagent(w http.ResponseWriter, r *http.Request) {
	svc, ok := c.Svc.(subagentService)
	if !ok {
		apispec.NotImplemented(w, r, http.MethodPost, subagentStopPath)
		return
	}
	toolUseID, ok := subagentToolUseID(w, r)
	if !ok {
		return
	}
	if err := svc.StopSubagent(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")), toolUseID); err != nil {
		writeSubagentError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusAccepted, StopSubagentResponse{OK: true})
}

// subagentToolUseID decodes the providerItemId a client percent-encodes; scoped
// ACP ids contain colons.
func subagentToolUseID(w http.ResponseWriter, r *http.Request) (string, bool) {
	toolUseID, err := url.PathUnescape(chi.URLParam(r, "toolUseId"))
	if err != nil || strings.TrimSpace(toolUseID) == "" {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation",
			"CHAT_SUBAGENT_INVALID", "subagent identifier is invalid", nil)
		return "", false
	}
	return toolUseID, true
}

// writeSubagentError maps the refusals specific to subagent control, then
// falls through to the shared Chat mapping (unknown session, TUI session, no
// live controller).
func writeSubagentError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, chatsvc.ErrSubagentTextRequired):
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation",
			"CHAT_SUBAGENT_TEXT_REQUIRED", "subagent message text is required", nil)

	case errors.Is(err, chatsvc.ErrSubagentsUnsupported):
		// Permanent for this conversation: only Claude Code chat sessions on AO's
		// bundled bridge can address a single subagent. A client should hide the
		// control rather than retry.
		envelope.WriteAPIError(w, r, http.StatusUnprocessableEntity, "unprocessable",
			"CHAT_SUBAGENTS_UNSUPPORTED",
			"this agent cannot message or stop a single subagent", nil)

	case errors.Is(err, chatsvc.ErrSubagentNotRunning):
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict",
			"CHAT_SUBAGENT_NOT_RUNNING",
			"that subagent is not running; it finished or was stopped", nil)

	default:
		writeConversationError(w, r, err)
	}
}
