package daemon

import (
	"context"
	"errors"
	"log/slog"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentaccounts"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/mcpservers"
)

// mcpSessionStore is the session reading the MCP server service needs.
type mcpSessionStore interface {
	GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error)
	ListAllSessions(ctx context.Context) ([]domain.SessionRecord, error)
}

// mcpSessions answers which Claude Code sessions exist and which run.
type mcpSessions struct {
	store    mcpSessionStore
	accounts *agentaccounts.Service
	chat     *chatsvc.Service
}

func (m mcpSessions) session(ctx context.Context, rec domain.SessionRecord) (mcpservers.Session, error) {
	accountID, err := m.accounts.SessionAccountID(ctx, rec.ID, rec.Harness)
	if err != nil {
		return mcpservers.Session{}, err
	}
	if accountID == "" {
		accountID = domain.DeviceAgentAccountID(domain.AgentAccountProviderClaude)
	}
	return mcpservers.Session{
		ID: rec.ID, AccountID: accountID, WorkspacePath: rec.Metadata.WorkspacePath, ProjectID: string(rec.ProjectID),
		Chat: domain.NormalizeSessionMode(rec.Mode) == domain.SessionModeChat,
	}, nil
}

// MCPSession returns a Claude Code session.
func (m mcpSessions) MCPSession(ctx context.Context, id domain.SessionID) (mcpservers.Session, error) {
	rec, found, err := m.store.GetSession(ctx, id)
	if err != nil {
		return mcpservers.Session{}, err
	}
	if !found {
		return mcpservers.Session{}, mcpservers.ErrUnknownSession
	}
	if rec.Harness != domain.HarnessClaudeCode {
		return mcpservers.Session{}, mcpservers.ErrHarnessUnsupported
	}
	return m.session(ctx, rec)
}

// MCPRunningSessions lists Claude Code sessions with an agent running. A
// hibernated session has none: it reads the current servers when it wakes.
func (m mcpSessions) MCPRunningSessions(ctx context.Context) ([]mcpservers.Session, error) {
	recs, err := m.store.ListAllSessions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]mcpservers.Session, 0, len(recs))
	for _, rec := range recs {
		if rec.IsTerminated || rec.Harness != domain.HarnessClaudeCode || rec.HibernatedAt != nil || rec.Activity.State == domain.ActivityExited {
			continue
		}
		session, err := m.session(ctx, rec)
		if err != nil {
			return nil, err
		}
		if session.Chat && !m.chat.HasLiveChatController(rec.ID) {
			continue
		}
		out = append(out, session)
	}
	return out, nil
}

// mcpLiveAgents maps the chat service's errors to the MCP service's words.
type mcpLiveAgents struct {
	chat *chatsvc.Service
}

func mcpLiveError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, chatsvc.ErrNoController):
		return mcpservers.ErrNotLive
	case errors.Is(err, chatsvc.ErrMCPReloadUnsupported):
		return mcpservers.ErrInPlaceUnsupported
	}
	return err
}

func (m mcpLiveAgents) LiveMCPServers(ctx context.Context, id domain.SessionID) ([]domain.ConversationMCPServer, error) {
	servers, err := m.chat.LiveMCPServers(ctx, id)
	return servers, mcpLiveError(err)
}

func (m mcpLiveAgents) ReconnectMCPServer(ctx context.Context, id domain.SessionID, name string) ([]domain.ConversationMCPServer, string, error) {
	servers, reason, err := m.chat.ReconnectMCPServer(ctx, id, name)
	return servers, reason, mcpLiveError(err)
}

func (m mcpLiveAgents) ApplyMCPServers(ctx context.Context, id domain.SessionID, change ports.ChatMCPChange) ([]domain.ConversationMCPServer, map[string]string, error) {
	servers, failures, err := m.chat.ApplyMCPServers(ctx, id, change)
	return servers, failures, mcpLiveError(err)
}

// mcpRestarter restarts an agent through the session manager.
type mcpRestarter struct {
	manager sessionLifecycle
}

func (m mcpRestarter) RestartAgentForToolServers(ctx context.Context, id domain.SessionID) (bool, error) {
	restarter, ok := m.manager.(interface {
		RestartAgentForToolServers(context.Context, domain.SessionID) (bool, error)
	})
	if !ok {
		return true, nil
	}
	return restarter.RestartAgentForToolServers(ctx, id)
}

// newMCPServers builds the MCP server catalog.
func newMCPServers(ctx context.Context, store interface {
	mcpSessionStore
	mcpservers.Projects
}, accounts *agentaccounts.Service, chat *chatsvc.Service, manager sessionLifecycle, log *slog.Logger) *mcpservers.Service {
	return mcpservers.New(mcpservers.Deps{
		Context: ctx, CLI: accounts.ClaudeMCPCLI(), Accounts: accounts,
		Sessions:  mcpSessions{store: store, accounts: accounts, chat: chat},
		Live:      mcpLiveAgents{chat: chat},
		Restarter: mcpRestarter{manager: manager},
		Projects:  store, Log: log,
	})
}
