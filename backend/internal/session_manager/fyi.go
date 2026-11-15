package sessionmanager

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	reportsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/report"
)

// ErrFYIUnsupported reports a store that cannot hold `ao send --fyi` messages.
var ErrFYIUnsupported = errors.New("fyi messages are not supported by this store")

// fyiStore is the optional durable outbox `ao send --fyi` writes to.
type fyiStore interface {
	CreateFYIMessage(context.Context, domain.FYIMessage) (domain.FYIMessage, error)
	CountPendingFYIMessages(context.Context, domain.SessionID) (int, error)
}

// FYIDelivery claims the FYIs waiting for a session's next turn, using the
// report piggyback batch so both settle the same way. *report.Coordinator.
type FYIDelivery interface {
	PrepareFYIs(context.Context, domain.SessionID) (reportsvc.PreparedBatch, error)
	AcceptPiggyback(context.Context, reportsvc.PreparedBatch) error
	ReleasePiggyback(context.Context, reportsvc.PreparedBatch, error) error
}

// SetFYIDelivery late-binds FYI piggybacking for terminal sends. Chat sends
// carry FYIs through the Chat service.
func (m *Manager) SetFYIDelivery(delivery FYIDelivery) {
	m.fyis = delivery
}

// QueueFYI stores an informational message for id without starting a turn.
// The next turn id starts, whatever starts it, carries it. It returns how many
// FYIs are now waiting for id.
func (m *Manager) QueueFYI(ctx context.Context, id domain.SessionID, message string, options ports.MessageDeliveryOptions) (int, error) {
	store, ok := m.store.(fyiStore)
	if !ok {
		return 0, ErrFYIUnsupported
	}
	rec, found, err := m.store.GetSession(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("fyi %s: session: %w", id, err)
	}
	if !found {
		return 0, ErrNotFound
	}
	if rec.IsTerminated {
		// It would never have a next turn to ride on.
		return 0, fmt.Errorf("fyi %s: %w", id, ErrTerminated)
	}
	if _, err := store.CreateFYIMessage(ctx, domain.FYIMessage{
		TargetSessionID: id, SenderSessionID: domain.SessionID(strings.TrimSpace(options.SenderSessionID)),
		Message: message, CreatedAt: m.clock().UTC(),
	}); err != nil {
		return 0, fmt.Errorf("fyi %s: %w", id, err)
	}
	return store.CountPendingFYIMessages(ctx, id)
}

// prepareFYIs claims the FYIs a terminal send carries. An empty message is the
// deliberate Enter that submits an existing draft, so it carries none. A claim
// failure never blocks the send: the FYIs stay pending for a later turn.
func (m *Manager) prepareFYIs(ctx context.Context, id domain.SessionID, message string) reportsvc.PreparedBatch {
	if m.fyis == nil || strings.TrimSpace(message) == "" {
		return reportsvc.PreparedBatch{}
	}
	batch, err := m.fyis.PrepareFYIs(ctx, id)
	if err != nil {
		m.logger.Warn("send: fyi messages stay pending", "sessionID", id, "error", err)
		return reportsvc.PreparedBatch{}
	}
	return batch
}

// settleFYIs acknowledges FYIs written into the pane and releases the rest for
// the next turn.
func (m *Manager) settleFYIs(ctx context.Context, id domain.SessionID, batch reportsvc.PreparedBatch, delivered bool) {
	if m.fyis == nil || len(batch.FYIs) == 0 {
		return
	}
	var err error
	if delivered {
		err = m.fyis.AcceptPiggyback(ctx, batch)
	} else {
		err = m.fyis.ReleasePiggyback(ctx, batch, errors.New("terminal send was not delivered"))
	}
	if err != nil {
		m.logger.Warn("send: settle fyi messages", "sessionID", id, "delivered", delivered, "error", err)
	}
}
