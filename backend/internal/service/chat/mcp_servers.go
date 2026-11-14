package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// MCPAgentRestartedEvent names the quiet timeline line that says AO restarted
// an agent to load a tool server change.
const MCPAgentRestartedEvent = "mcp.agent_restarted"

// LiveMCPController is the session's running controller when it can report and
// change its tool servers in place. It never wakes a hibernated session: that
// picks up the current servers when it starts.
func (s *Service) liveMCPController(id domain.SessionID) (*Controller, error) {
	controller, err := s.Controller(id)
	if err != nil {
		return nil, err
	}
	if controller.State() == ports.ChatControllerStopped {
		return nil, ErrNoController
	}
	return controller, nil
}

// LiveMCPServers reads the tool servers the session's running agent has now.
// It returns ErrNoController when no agent is running and ErrMCPReloadUnsupported
// when the running one cannot say.
func (s *Service) LiveMCPServers(ctx context.Context, id domain.SessionID) ([]domain.ConversationMCPServer, error) {
	controller, err := s.liveMCPController(id)
	if err != nil {
		return nil, err
	}
	return controller.LiveMCPServers(ctx)
}

// ReconnectMCPServer has the session's running agent connect again to a server.
// reason is the provider's own word when it refused.
func (s *Service) ReconnectMCPServer(ctx context.Context, id domain.SessionID, name string) ([]domain.ConversationMCPServer, string, error) {
	controller, err := s.liveMCPController(id)
	if err != nil {
		return nil, "", err
	}
	return controller.ReconnectMCPServer(ctx, name)
}

// ApplyMCPServers adds, replaces or drops servers in the session's running agent.
func (s *Service) ApplyMCPServers(ctx context.Context, id domain.SessionID, change ports.ChatMCPChange) ([]domain.ConversationMCPServer, map[string]string, error) {
	controller, err := s.liveMCPController(id)
	if err != nil {
		return nil, nil, err
	}
	return controller.ApplyMCPServers(ctx, change)
}

// RestartChatForToolServers restarts an idle agent, conversation kept, so it
// loads tool servers it could not take in place. It reports done when there is
// nothing left to do: the agent restarted, or none is running (a later start
// picks up the servers). A busy agent reports not done, so the caller asks
// again; it is never stopped under a turn, a queued message, or an approval.
func (s *Service) RestartChatForToolServers(ctx context.Context, id domain.SessionID) (bool, error) {
	if _, err := s.liveMCPController(id); err != nil {
		if errors.Is(err, ErrNoController) {
			return true, nil
		}
		return false, err
	}
	stopped, err := s.hibernateChat(ctx, id, hibernateOptions{toolServers: true, ignoreViews: true})
	if err != nil || !stopped {
		return false, err
	}
	if err := s.recordMCPAgentRestart(ctx, id); err != nil {
		s.log.Warn("could not note the agent restart in the chat", "session", id, "err", err)
	}
	s.startBackgroundWake(id)
	return true, nil
}

// recordMCPAgentRestart adds the one quiet line that says why the agent restarted.
func (s *Service) recordMCPAgentRestart(ctx context.Context, id domain.SessionID) error {
	reader, ok := s.store.(interface {
		LatestConversationTurn(context.Context, string) (domain.ConversationTurn, error)
	})
	if !ok {
		return nil
	}
	conversation, err := s.store.ConversationForSession(ctx, id)
	if errors.Is(err, domain.ErrNoConversation) {
		return nil
	}
	if err != nil {
		return err
	}
	turn, err := reader.LatestConversationTurn(ctx, conversation.ID)
	if errors.Is(err, domain.ErrNoConversationTurn) {
		return nil
	}
	if err != nil {
		return err
	}
	detail, err := json.Marshal(map[string]string{"event": MCPAgentRestartedEvent})
	if err != nil {
		return fmt.Errorf("encode agent restart: %w", err)
	}
	return s.store.UpsertActivity(ctx, conversation.ID, turn.ProviderTurnID, domain.ConversationActivity{
		ID:             s.newID(),
		Kind:           domain.ActivityKindSystem,
		Status:         domain.ActivityStatusCompleted,
		Summary:        "Restarted the agent to load MCP server changes",
		Detail:         detail,
		ProviderItemID: "mcp-agent-restart:" + s.newID(),
	}, s.now())
}
