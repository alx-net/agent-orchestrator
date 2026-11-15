package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// UsageLimitSwitchEvent marks, on a turn the provider refused for its
// account's usage limit, AO moving the session to an account with usage left.
const UsageLimitSwitchEvent = "account.limit_switch"

// UsageLimitSwitcher moves a session whose turn hit its account's usage limit
// to another account. It is told the session and the provider turn id, and
// must not block: it hears from inside the controller's event pipeline.
type UsageLimitSwitcher interface {
	UsageLimitReached(domain.SessionID, string)
}

// continueAfterSwitchWait bounds how long a continuation waits for the
// session's new agent to accept the turn.
const continueAfterSwitchWait = 30 * time.Second

// SetUsageLimitSwitcher late-binds what moves a usage-limited session to
// another account; Session Manager is built after Chat.
func (s *Service) SetUsageLimitSwitcher(switcher UsageLimitSwitcher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usageLimitSwitcher = switcher
}

func (s *Service) usageLimitReached(id domain.SessionID, providerTurnID string) {
	s.mu.RLock()
	switcher := s.usageLimitSwitcher
	s.mu.RUnlock()
	if switcher != nil {
		switcher.UsageLimitReached(id, providerTurnID)
	}
}

// UsageLimitedTurn is the session's turn the provider refused for its
// account's usage limit, by provider turn id, while it is still the latest
// turn and nothing ran it again. A turn the user has moved past is refused.
func (s *Service) UsageLimitedTurn(ctx context.Context, id domain.SessionID, providerTurnID string) (domain.ConversationTurn, error) {
	store, ok := s.store.(usageLimitStore)
	reader, readable := s.store.(interface {
		TurnByProviderID(context.Context, string, string) (domain.ConversationTurn, error)
	})
	if !ok || !readable {
		return domain.ConversationTurn{}, ErrUsageLimitAutoResumeInvalid
	}
	conversation, err := s.store.ConversationForSession(ctx, id)
	if err != nil {
		return domain.ConversationTurn{}, err
	}
	turn, err := reader.TurnByProviderID(ctx, conversation.ID, providerTurnID)
	if err != nil {
		return domain.ConversationTurn{}, err
	}
	if err := usageLimitTurnActionable(turn, id); err != nil {
		return domain.ConversationTurn{}, err
	}
	if _, retried, err := s.store.RetryTurnIDForSource(ctx, conversation.ID, turn.ID); err != nil {
		return domain.ConversationTurn{}, err
	} else if retried {
		return domain.ConversationTurn{}, fmt.Errorf("%w: this turn already has a retry", ErrUsageLimitAutoResumeInvalid)
	}
	latest, err := store.LatestAutoResumeTurn(ctx, conversation.ID)
	if err != nil {
		return domain.ConversationTurn{}, err
	}
	if latest.ID != turn.ID {
		return domain.ConversationTurn{}, fmt.Errorf("%w: a newer turn superseded this prompt", ErrUsageLimitAutoResumeInvalid)
	}
	return turn, nil
}

// RecordUsageLimitSwitch shows on the usage-limited turn where AO's move to
// another account stands: one row per turn, updated as the move goes on.
func (s *Service) RecordUsageLimitSwitch(ctx context.Context, id domain.SessionID, turnID string, sw domain.UsageLimitSwitch) error {
	turn, err := s.store.TurnByID(ctx, turnID)
	if err != nil {
		return err
	}
	if turn.HandledBySessionID != id || turn.ProviderTurnID == "" {
		return domain.ErrNoConversationTurn
	}
	detail, err := json.Marshal(struct {
		Event         string   `json:"event"`
		Phase         string   `json:"phase"`
		AccountID     string   `json:"accountId"`
		FromAccountID string   `json:"fromAccountId"`
		Error         string   `json:"error,omitempty"`
		MCPNeedsAuth  []string `json:"mcpNeedsAuth,omitempty"`
	}{UsageLimitSwitchEvent, string(sw.Phase), sw.ToAccountID, sw.FromAccountID, sw.Error, sw.MCPNeedsAuth})
	if err != nil {
		return fmt.Errorf("encode usage-limit switch: %w", err)
	}
	status, summary := domain.ActivityStatusCompleted, "Continued on another account"
	switch sw.Phase {
	case domain.UsageLimitSwitchSwitching:
		status, summary = domain.ActivityStatusRunning, "Switching account"
	case domain.UsageLimitSwitchSwitched:
		summary = "Switched account"
	case domain.UsageLimitSwitchFailed:
		status, summary = domain.ActivityStatusFailed, "Account switch failed"
	}
	return s.store.UpsertActivity(ctx, turn.ConversationID, turn.ProviderTurnID, domain.ConversationActivity{
		ID:             s.newID(),
		Kind:           domain.ActivityKindSystem,
		Status:         status,
		Summary:        summary,
		Detail:         detail,
		ProviderItemID: "usage-limit-switch:" + turn.ID,
	}, s.now())
}

// ContinueAfterAccountSwitch runs the usage-limited turn again at once, on the
// account the session moved to: the retry auto-resume would make at the
// reset. It waits briefly for the new agent to accept work, and refuses a turn
// the user has since moved past or one only they may run again.
func (s *Service) ContinueAfterAccountSwitch(ctx context.Context, id domain.SessionID, turnID string) (domain.ConversationTurn, error) {
	ctx, cancel := context.WithTimeout(ctx, continueAfterSwitchWait)
	defer cancel()
	for {
		turn, err := s.continueAfterAccountSwitch(ctx, id, turnID)
		if !errors.Is(err, ErrTurnRunning) && !errors.Is(err, ErrControllerHandoff) && !errors.Is(err, ErrNoController) {
			return turn, err
		}
		select {
		case <-ctx.Done():
			return domain.ConversationTurn{}, errors.Join(err, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (s *Service) continueAfterAccountSwitch(ctx context.Context, id domain.SessionID, turnID string) (domain.ConversationTurn, error) {
	controller, release, err := s.workingController(ctx, id)
	if err != nil {
		return domain.ConversationTurn{}, err
	}
	defer release()
	return controller.retryAfterAccountSwitch(ctx, turnID)
}

// retryAfterAccountSwitch is retryUsageLimitTurn without the reset deadline:
// the account the turn ran out on is no longer the session's.
func (c *Controller) retryAfterAccountSwitch(ctx context.Context, turnID string) (domain.ConversationTurn, error) {
	if !c.sendMu.TryLock() {
		return domain.ConversationTurn{}, ErrTurnRunning
	}
	defer c.sendMu.Unlock()
	store, ok := c.store.(usageLimitStore)
	if !ok {
		return domain.ConversationTurn{}, ErrUsageLimitAutoResumeInvalid
	}
	source, err := c.store.TurnByID(ctx, turnID)
	if err != nil {
		return domain.ConversationTurn{}, err
	}
	if source.ConversationID != c.conversation.ID {
		return domain.ConversationTurn{}, domain.ErrNoConversationTurn
	}
	if err := usageLimitTurnActionable(source, c.sessionID); err != nil {
		return domain.ConversationTurn{}, err
	}
	if err := validateUsageLimitRetry(ctx, c.store, store, source); err != nil {
		return domain.ConversationTurn{}, err
	}
	return c.retryTurnLocked(ctx, turnID, false)
}
