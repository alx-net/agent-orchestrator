package chat

import (
	"context"
	"errors"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Subagent control: a message for, or a stop of, one running subagent, sent to
// it directly rather than through the main agent's turn. The main agent keeps
// working either way; only the named subagent hears the message or stops.

var (
	// ErrSubagentsUnsupported reports a conversation whose provider cannot address
	// a single subagent. Permanent for that conversation.
	ErrSubagentsUnsupported = ports.ErrChatSubagentsUnsupported
	// ErrSubagentNotRunning reports a subagent that has finished, was stopped, or
	// was never spawned by that tool call.
	ErrSubagentNotRunning = ports.ErrChatSubagentNotRunning
	// ErrSubagentTextRequired refuses an empty message.
	ErrSubagentTextRequired = errors.New("subagent message text is required")
)

// SubagentMessageResult names an accepted subagent message and its state.
type SubagentMessageResult struct {
	ID    string
	State string
}

// MessageSubagent queues text for the running subagent spawned by toolUseID,
// the provider item id of its Agent/Task activity.
//
// Like Interrupt it needs the live controller and never wakes a hibernated
// session: a session only hibernates once no subagent is running.
func (s *Service) MessageSubagent(
	ctx context.Context,
	id domain.SessionID,
	toolUseID, text string,
) (SubagentMessageResult, error) {
	if strings.TrimSpace(text) == "" {
		return SubagentMessageResult{}, ErrSubagentTextRequired
	}
	controller, err := s.subagentController(ctx, id)
	if err != nil {
		return SubagentMessageResult{}, err
	}
	return controller.MessageSubagent(ctx, toolUseID, text)
}

// StopSubagent stops the running subagent spawned by toolUseID.
func (s *Service) StopSubagent(ctx context.Context, id domain.SessionID, toolUseID string) error {
	controller, err := s.subagentController(ctx, id)
	if err != nil {
		return err
	}
	return controller.StopSubagent(ctx, toolUseID)
}

func (s *Service) subagentController(ctx context.Context, id domain.SessionID) (*Controller, error) {
	if _, err := s.requireChatSession(ctx, id); err != nil {
		return nil, err
	}
	return s.Controller(id)
}

// subagents returns the provider's subagent port only when the conversation
// negotiated it. The ACP driver implements it for every agent.
func (c *Controller) subagents() (ports.ChatSubagentController, bool) {
	controller, ok := c.conv.(ports.ChatSubagentController)
	return controller, ok && c.Capabilities().Has(ports.ChatCapabilitySubagents)
}

// MessageSubagent hands one subagent a message under an AO-minted id.
func (c *Controller) MessageSubagent(ctx context.Context, toolUseID, text string) (SubagentMessageResult, error) {
	controller, ok := c.subagents()
	if !ok {
		return SubagentMessageResult{}, ErrSubagentsUnsupported
	}
	if c.handoffActive() {
		return SubagentMessageResult{}, ErrControllerHandoff
	}
	receipt, err := controller.MessageSubagent(ctx, toolUseID, ports.ChatSubagentMessage{
		ID: c.newID(), Text: strings.TrimSpace(text),
	})
	if err != nil {
		return SubagentMessageResult{}, err
	}
	return SubagentMessageResult{ID: receipt.ID, State: receipt.State}, nil
}

// StopSubagent stops one subagent and leaves the main turn running.
func (c *Controller) StopSubagent(ctx context.Context, toolUseID string) error {
	controller, ok := c.subagents()
	if !ok {
		return ErrSubagentsUnsupported
	}
	if c.handoffActive() {
		return ErrControllerHandoff
	}
	return controller.StopSubagent(ctx, toolUseID)
}
