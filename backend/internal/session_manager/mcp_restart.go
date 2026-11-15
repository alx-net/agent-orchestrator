package sessionmanager

import (
	"context"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ChatToolServerRestarter restarts a chat's idle agent with its conversation kept.
type ChatToolServerRestarter interface {
	RestartChatForToolServers(ctx context.Context, id domain.SessionID) (bool, error)
}

// RestartAgentForToolServers restarts a session's agent, conversation kept, so it
// loads MCP server changes it could not take in place. It reports done when
// nothing is left to do; a busy agent reports not done, so the caller asks again,
// and it is never stopped under a turn. The checks are the ones a move to a new
// harness release uses, in both modes.
func (m *Manager) RestartAgentForToolServers(ctx context.Context, id domain.SessionID) (bool, error) {
	rec, err := m.getRecord(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if rec.IsTerminated || rec.HibernatedAt != nil || rec.Activity.State == domain.ActivityExited {
		return true, nil // no agent runs: its next start reads the current servers
	}
	if domain.NormalizeSessionMode(rec.Mode) == domain.SessionModeChat {
		return m.restartChatForToolServers(ctx, id)
	}
	return m.restartTerminalForToolServers(ctx, id)
}

func (m *Manager) restartChatForToolServers(ctx context.Context, id domain.SessionID) (bool, error) {
	restarter, ok := m.chat.(ChatToolServerRestarter)
	if !ok {
		return true, nil
	}
	if err := m.beginAgentOperation(ctx, id, agentOperationHibernate); err != nil {
		if errors.Is(err, errAgentOperationInProgress) {
			return false, nil
		}
		return false, err
	}
	defer m.endAgentOperation(id, agentOperationHibernate)
	if active, err := m.hasActiveInterfaceTransition(ctx, id); err != nil || active {
		return false, err
	}
	return restarter.RestartChatForToolServers(ctx, id)
}

func (m *Manager) restartTerminalForToolServers(ctx context.Context, id domain.SessionID) (bool, error) {
	if err := m.beginAgentOperation(ctx, id, agentOperationResume); err != nil {
		if errors.Is(err, errAgentOperationInProgress) {
			return false, nil
		}
		return false, err
	}
	defer m.endAgentOperation(id, agentOperationResume)
	if active, err := m.hasActiveInterfaceTransition(ctx, id); err != nil || active {
		return false, err
	}
	rec, err := m.getRecord(ctx, id)
	if err != nil {
		return false, err
	}
	if !m.terminalRestartIdle(rec, 0) {
		return false, nil
	}
	// Close the terminal's input path, then require the idle report to be newer
	// than any keystroke accepted before the gate: text being typed is never lost.
	lastInputAt, releaseInput := m.beginTerminalInputDrain(rec)
	if releaseInput != nil {
		defer releaseInput()
	}
	current, err := m.getRecord(ctx, id)
	if err != nil {
		return false, err
	}
	if current.Metadata.RuntimeLaunchID != rec.Metadata.RuntimeLaunchID ||
		!m.terminalRestartIdle(current, 0) || !m.terminalIdleForRestart(ctx, current, lastInputAt) {
		return false, nil
	}
	inspector, ok := m.runtime.(ports.WorkloadCommandInspector)
	if !ok {
		return true, nil
	}
	commands, err := inspector.RunningWorkloadCommands(ctx, runtimeHandle(current.Metadata), ports.SupervisedProcessRef{
		SessionID: current.ID, LaunchID: current.Metadata.RuntimeLaunchID,
	})
	if err != nil || commands > 0 {
		// A command the agent started (tests, a dev server) would die with it.
		return false, nil
	}
	if !terminalRefreshIdentityVerified(current) {
		return true, nil // it cannot resume natively, so a restart would lose it
	}
	releaseHarness, err := m.beginHarnessUse(current.Harness)
	if err != nil {
		return false, nil
	}
	defer releaseHarness()
	if _, err := m.resumeAgentRecordWithPolicy(ctx, "mcp server change", current, true); err != nil {
		return false, err
	}
	m.logger.Info("terminal session restarted to load MCP server changes", "sessionID", id, "harness", current.Harness)
	return true, nil
}
