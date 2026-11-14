package store

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// CreateFYIMessage persists one pending `ao send --fyi` message.
func (s *Store) CreateFYIMessage(ctx context.Context, msg domain.FYIMessage) (domain.FYIMessage, error) {
	if err := msg.Validate(); err != nil {
		return domain.FYIMessage{}, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	row, err := s.qw.CreateFYIMessage(ctx, gen.CreateFYIMessageParams{
		TargetSessionID: string(msg.TargetSessionID), SenderSessionID: string(msg.SenderSessionID),
		Message: msg.Message, CreatedAt: msg.CreatedAt,
	})
	if err != nil {
		return domain.FYIMessage{}, fmt.Errorf("create fyi message for %s: %w", msg.TargetSessionID, err)
	}
	return fyiFromGen(row), nil
}

// CountPendingFYIMessages counts the FYI messages a session's next turn will carry.
func (s *Store) CountPendingFYIMessages(ctx context.Context, target domain.SessionID) (int, error) {
	count, err := s.qr.CountPendingFYIMessages(ctx, string(target))
	if err != nil {
		return 0, fmt.Errorf("count pending fyi messages for %s: %w", target, err)
	}
	return int(count), nil
}

// ClaimPendingFYIMessages atomically claims every pending FYI for target, in
// arrival order.
func (s *Store) ClaimPendingFYIMessages(ctx context.Context, target domain.SessionID, token string, at time.Time) ([]domain.FYIMessage, error) {
	if target == "" || token == "" || at.IsZero() {
		return nil, domain.ErrInvalidFYIMessage
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.ClaimPendingFYIMessages(ctx, gen.ClaimPendingFYIMessagesParams{
		TargetSessionID: string(target), ClaimToken: token, ClaimedAt: nullTime(at),
	})
	if err != nil {
		return nil, fmt.Errorf("claim fyi messages for %s: %w", target, err)
	}
	out := make([]domain.FYIMessage, len(rows))
	for i := range rows {
		out[i] = fyiFromGen(rows[i])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// AcknowledgeFYIMessages completes only the claim holding token.
func (s *Store) AcknowledgeFYIMessages(ctx context.Context, target domain.SessionID, token string, at time.Time) (int, error) {
	if target == "" || token == "" || at.IsZero() {
		return 0, domain.ErrInvalidFYIMessage
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	ids, err := s.qw.AcknowledgeFYIMessages(ctx, gen.AcknowledgeFYIMessagesParams{
		TargetSessionID: string(target), ClaimToken: token, AcknowledgedAt: nullTime(at),
	})
	if err != nil {
		return 0, fmt.Errorf("acknowledge fyi messages for %s: %w", target, err)
	}
	return len(ids), nil
}

// ReleaseFYIMessages returns the claim holding token to pending for the next turn.
func (s *Store) ReleaseFYIMessages(ctx context.Context, target domain.SessionID, token string) (int, error) {
	if target == "" || token == "" {
		return 0, domain.ErrInvalidFYIMessage
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	ids, err := s.qw.ReleaseFYIMessages(ctx, gen.ReleaseFYIMessagesParams{
		TargetSessionID: string(target), ClaimToken: token,
	})
	if err != nil {
		return 0, fmt.Errorf("release fyi messages for %s: %w", target, err)
	}
	return len(ids), nil
}

// RequeueClaimedFYIMessages recovers claims owned by a prior daemon process.
func (s *Store) RequeueClaimedFYIMessages(ctx context.Context) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	count, err := s.qw.RequeueClaimedFYIMessages(ctx)
	if err != nil {
		return 0, fmt.Errorf("requeue claimed fyi messages: %w", err)
	}
	return count, nil
}

func fyiFromGen(r gen.FyiMessage) domain.FYIMessage {
	return domain.FYIMessage{
		ID: r.ID, TargetSessionID: domain.SessionID(r.TargetSessionID),
		SenderSessionID: domain.SessionID(r.SenderSessionID), Message: r.Message, CreatedAt: r.CreatedAt,
		DeliveryState: domain.ReportDeliveryState(r.DeliveryState), ClaimToken: r.ClaimToken,
		ClaimedAt: timeFromNull(r.ClaimedAt), AcknowledgedAt: timeFromNull(r.AcknowledgedAt),
	}
}
