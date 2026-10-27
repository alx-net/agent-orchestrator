package sessionmanager

import (
	"context"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// usageLimitSwitchLimit bounds one move after a usage limit: reading every
// account's limits, the account transition, and the continuation.
const usageLimitSwitchLimit = 5 * time.Minute

// usageLimitSwitchTries is how many accounts one move tries when an account
// turns out unusable before its agent stops.
const usageLimitSwitchTries = 3

// usageLimitAccounts is the account service's part in moving a session off an
// account that ran out of usage.
type usageLimitAccounts interface {
	// MarkUsageLimited records the refusal and returns the session's account;
	// restart reports a refusal its folder's current login cannot have made.
	MarkUsageLimited(context.Context, domain.SessionID, domain.AgentHarness, *time.Time) (domain.AgentAccount, bool, error)
	HeadroomAccount(context.Context, domain.SessionID, domain.AgentHarness, string, ...string) (domain.AgentAccount, error)
	MCPNeedsAuth(string) []string
}

// usageLimitChat is Chat's part: the refused turn, its notice, and running it
// again on the new account.
type usageLimitChat interface {
	UsageLimitedTurn(context.Context, domain.SessionID, string) (domain.ConversationTurn, error)
	RecordUsageLimitSwitch(context.Context, domain.SessionID, string, domain.UsageLimitSwitch) error
	ContinueAfterAccountSwitch(context.Context, domain.SessionID, string) (domain.ConversationTurn, error)
}

// usageLimitSwitchRun is a session's move in progress; pending is the latest
// refusal heard while it ran, handled once it ends.
type usageLimitSwitchRun struct {
	pending string
}

// SetUsageLimitSwitch late-binds the user's "switch accounts automatically
// when a usage limit is reached" setting. Unset, it is on.
func (m *Manager) SetUsageLimitSwitch(enabled func() bool) {
	m.usageLimitSwitchMu.Lock()
	defer m.usageLimitSwitchMu.Unlock()
	m.usageLimitSwitchOn = enabled
}

// UsageLimitReached is told by Chat that the provider refused the session's
// turn for its account's usage limit. When another signed-in account of the
// provider has usage left, the session moves there through the account
// transition and the turn runs again at once, instead of waiting for the
// reset. It never blocks; each session moves on its own, one move at a time.
func (m *Manager) UsageLimitReached(id domain.SessionID, providerTurnID string) {
	m.usageLimitSwitchMu.Lock()
	enabled := m.usageLimitSwitchOn
	if enabled != nil && !enabled() {
		m.usageLimitSwitchMu.Unlock()
		return
	}
	if run, busy := m.usageLimitSwitches[id]; busy {
		// The move under way may be continuing on an account that refuses at
		// once; this refusal is handled when it ends.
		run.pending = providerTurnID
		m.usageLimitSwitchMu.Unlock()
		return
	}
	if m.usageLimitSwitches == nil {
		m.usageLimitSwitches = map[domain.SessionID]*usageLimitSwitchRun{}
	}
	run := &usageLimitSwitchRun{}
	m.usageLimitSwitches[id] = run
	m.usageLimitSwitchMu.Unlock()
	go func() {
		for next := providerTurnID; next != ""; {
			ctx, cancel := context.WithTimeout(m.versionContext(), usageLimitSwitchLimit)
			m.switchAfterUsageLimit(ctx, id, next)
			cancel()
			m.usageLimitSwitchMu.Lock()
			next, run.pending = run.pending, ""
			if next == "" {
				delete(m.usageLimitSwitches, id)
			}
			m.usageLimitSwitchMu.Unlock()
		}
	}()
}

// switchAfterUsageLimit moves the session off the account that refused its
// turn and runs the turn again. With no account to move to, the turn keeps
// today's usage-limit notice: resume by hand or at the reset.
func (m *Manager) switchAfterUsageLimit(ctx context.Context, id domain.SessionID, providerTurnID string) {
	accounts, ok := m.agentAccounts.(usageLimitAccounts)
	chat, chatOK := m.chat.(usageLimitChat)
	if !ok || !chatOK {
		return
	}
	rec, found, err := m.store.GetSession(ctx, id)
	if err != nil || !found || rec.IsTerminated || domain.NormalizeSessionMode(rec.Mode) != domain.SessionModeChat {
		return
	}
	if _, ok := domain.AgentAccountProviderFor(rec.Harness); !ok {
		return
	}
	turn, err := chat.UsageLimitedTurn(ctx, id, providerTurnID)
	if err != nil {
		m.logger.Debug("usage limit: turn not movable", "sessionID", id, "error", err)
		return
	}
	from, restart, err := accounts.MarkUsageLimited(ctx, id, rec.Harness, turn.UsageLimit.ResetsAt)
	if err != nil {
		m.logger.Warn("usage limit: account not recorded", "sessionID", id, "error", err)
		return
	}
	show := func(sw *domain.UsageLimitSwitch, phase domain.UsageLimitSwitchPhase, cause error) {
		sw.Phase, sw.Error = phase, ""
		if cause != nil {
			sw.Error = cause.Error()
		}
		if err := chat.RecordUsageLimitSwitch(context.WithoutCancel(ctx), id, turn.ID, *sw); err != nil {
			m.logger.Warn("usage limit: switch not shown", "sessionID", id, "phase", phase, "error", err)
		}
	}
	var tried []string
	var last *domain.UsageLimitSwitch
	for range usageLimitSwitchTries {
		target := from
		if !restart {
			target, err = accounts.HeadroomAccount(ctx, id, rec.Harness, rec.Metadata.Model, tried...)
			if err != nil {
				if !errors.Is(err, ports.ErrAgentAccountNoHeadroom) {
					m.logger.Warn("usage limit: no account chosen", "sessionID", id, "error", err)
				}
				break
			}
		}
		tried = append(tried, target.ID)
		sw := &domain.UsageLimitSwitch{FromAccountID: from.ID, ToAccountID: target.ID, MCPNeedsAuth: accounts.MCPNeedsAuth(target.ID)}
		last = sw
		show(sw, domain.UsageLimitSwitchSwitching, nil)
		transition, err := m.startAccountTransition(ctx, id, target.ID, domain.SessionInterfaceTransitionDrain, restart)
		if err != nil {
			if !restart && accountUnusable(err) {
				sw.Error = err.Error()
				continue // the account cannot run the agent: try the next one
			}
			show(sw, domain.UsageLimitSwitchFailed, err)
			return
		}
		settled, err := m.awaitInterfaceTransition(ctx, transition)
		if err == nil && settled.Phase != domain.SessionInterfaceTransitionCompleted {
			err = errors.New("the account switch did not complete")
			if settled.ErrorDetail != "" {
				err = errors.New(settled.ErrorDetail)
			}
		}
		if err != nil {
			show(sw, domain.UsageLimitSwitchFailed, err)
			return
		}
		if _, err := chat.ContinueAfterAccountSwitch(ctx, id, turn.ID); err != nil {
			m.logger.Info("usage limit: moved, but the turn was not run again", "sessionID", id, "error", err)
			show(sw, domain.UsageLimitSwitchSwitched, nil)
			return
		}
		show(sw, domain.UsageLimitSwitchContinued, nil)
		return
	}
	// Every account tried was unusable: the notice must not keep switching.
	if last != nil {
		show(last, domain.UsageLimitSwitchFailed, errors.New(last.Error))
	}
}

// accountUnusable reports a move refused for the target account itself, so
// another account may still work.
func accountUnusable(err error) bool {
	return errors.Is(err, ports.ErrAgentAccountSignedOut) || errors.Is(err, ports.ErrAgentAccountUnknown) ||
		errors.Is(err, ports.ErrAgentAccountUnshared) || errors.Is(err, ports.ErrAgentAccountUnchanged)
}

// awaitInterfaceTransition waits for a transition's worker to end and returns
// where the transition settled.
func (m *Manager) awaitInterfaceTransition(ctx context.Context, transition domain.SessionInterfaceTransition) (domain.SessionInterfaceTransition, error) {
	store, ok := m.store.(interfaceTransitionStore)
	if !ok {
		return domain.SessionInterfaceTransition{}, ErrInterfaceHandoffUnsupported
	}
	for {
		current, found, err := store.GetSessionInterfaceTransition(ctx, transition.ID)
		if err != nil {
			return domain.SessionInterfaceTransition{}, err
		}
		if !found {
			return domain.SessionInterfaceTransition{}, ErrInterfaceTransitionNotFound
		}
		if current.Phase.Terminal() {
			return current, nil
		}
		m.transitionMu.Lock()
		run := m.transitions[transition.SessionID]
		m.transitionMu.Unlock()
		var done <-chan struct{}
		if run != nil {
			done = run.done
		}
		select {
		case <-done:
		case <-time.After(interfaceTransitionPoll):
		case <-ctx.Done():
			return domain.SessionInterfaceTransition{}, ctx.Err()
		}
	}
}
