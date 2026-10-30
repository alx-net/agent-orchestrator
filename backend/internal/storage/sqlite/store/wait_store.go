package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// CreateWait persists one validated pending wait.
func (s *Store) CreateWait(ctx context.Context, w domain.Wait) (domain.Wait, error) {
	if err := w.Validate(); err != nil {
		return domain.Wait{}, err
	}
	baseline, err := encodeWaitBaseline(w.Baseline)
	if err != nil {
		return domain.Wait{}, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	row, err := s.qw.CreateSessionWait(ctx, gen.CreateSessionWaitParams{
		ID: string(w.ID), SessionID: string(w.SessionID), ProjectID: string(w.ProjectID),
		Kind: string(w.Kind), Condition: string(w.Condition), PRNumber: int64(w.PRNumber),
		PRURL: w.PRURL, Note: w.Note, Baseline: baseline,
		CreatedAt: w.CreatedAt.UTC(), FireAt: nullTime(w.FireAt.UTC()), ExpiresAt: w.ExpiresAt.UTC(),
	})
	if err != nil {
		return domain.Wait{}, fmt.Errorf("create wait %s: %w", w.ID, err)
	}
	return waitFromGen(row), nil
}

// GetWait returns one wait in any state.
func (s *Store) GetWait(ctx context.Context, id domain.WaitID) (domain.Wait, bool, error) {
	row, err := s.qr.GetSessionWait(ctx, string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Wait{}, false, nil
	}
	if err != nil {
		return domain.Wait{}, false, fmt.Errorf("get wait %s: %w", id, err)
	}
	return waitFromGen(row), true, nil
}

// ListPendingWaits returns every unresolved wait, oldest first.
func (s *Store) ListPendingWaits(ctx context.Context) ([]domain.Wait, error) {
	rows, err := s.qr.ListPendingSessionWaits(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pending waits: %w", err)
	}
	return waitsFromGen(rows), nil
}

// ListPendingWaitsBySession returns one session's unresolved waits, oldest first.
func (s *Store) ListPendingWaitsBySession(ctx context.Context, id domain.SessionID) ([]domain.Wait, error) {
	rows, err := s.qr.ListPendingSessionWaitsBySession(ctx, string(id))
	if err != nil {
		return nil, fmt.Errorf("list pending waits for %s: %w", id, err)
	}
	return waitsFromGen(rows), nil
}

// ResolveWait moves a pending wait to a final state. It reports false when
// the wait was not pending any more, so a wait resolves at most once.
func (s *Store) ResolveWait(ctx context.Context, id domain.WaitID, state domain.WaitState, outcome, message string, at time.Time) (bool, error) {
	if state == domain.WaitPending {
		return false, fmt.Errorf("resolve wait %s: %w", id, domain.ErrInvalidWait)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.ResolveSessionWait(ctx, gen.ResolveSessionWaitParams{
		ID: string(id), State: string(state), Outcome: outcome, Message: message,
		ResolvedAt: nullTime(at.UTC()),
	})
	if err != nil {
		return false, fmt.Errorf("resolve wait %s: %w", id, err)
	}
	return n > 0, nil
}

// UpdateWaitTarget records the PR a pending wait resolved to and its baseline.
func (s *Store) UpdateWaitTarget(ctx context.Context, id domain.WaitID, prURL string, baseline domain.WaitBaseline) (bool, error) {
	encoded, err := encodeWaitBaseline(baseline)
	if err != nil {
		return false, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.UpdateSessionWaitTarget(ctx, gen.UpdateSessionWaitTargetParams{ID: string(id), PRURL: prURL, Baseline: encoded})
	if err != nil {
		return false, fmt.Errorf("update wait target %s: %w", id, err)
	}
	return n > 0, nil
}

// RecordWaitDeliveryFailure counts one wake that could not reach its session.
func (s *Store) RecordWaitDeliveryFailure(ctx context.Context, id domain.WaitID, lastError string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.qw.RecordSessionWaitDeliveryFailure(ctx, gen.RecordSessionWaitDeliveryFailureParams{ID: string(id), LastError: lastError}); err != nil {
		return fmt.Errorf("record wait delivery failure %s: %w", id, err)
	}
	return nil
}

// PruneResolvedWaits deletes resolved waits that resolved before cutoff.
func (s *Store) PruneResolvedWaits(ctx context.Context, cutoff time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.DeleteResolvedSessionWaitsBefore(ctx, nullTime(cutoff.UTC()))
	if err != nil {
		return 0, fmt.Errorf("prune resolved waits: %w", err)
	}
	return n, nil
}

func encodeWaitBaseline(b domain.WaitBaseline) (string, error) {
	if !b.Captured && !b.ReviewsCaptured && b.HeadSHA == "" && len(b.ReviewIDs) == 0 && len(b.CommentIDs) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return "", fmt.Errorf("encode wait baseline: %w", err)
	}
	return string(raw), nil
}

func waitsFromGen(rows []gen.SessionWait) []domain.Wait {
	out := make([]domain.Wait, 0, len(rows))
	for _, row := range rows {
		out = append(out, waitFromGen(row))
	}
	return out
}

func waitFromGen(row gen.SessionWait) domain.Wait {
	var baseline domain.WaitBaseline
	if row.Baseline != "" {
		// A corrupt baseline degrades to "not captured": the next observation
		// becomes the baseline instead of the wait failing to load.
		_ = json.Unmarshal([]byte(row.Baseline), &baseline)
	}
	return domain.Wait{
		ID:               domain.WaitID(row.ID),
		SessionID:        domain.SessionID(row.SessionID),
		ProjectID:        domain.ProjectID(row.ProjectID),
		Kind:             domain.WaitKind(row.Kind),
		Condition:        domain.WaitCondition(row.Condition),
		PRNumber:         int(row.PRNumber),
		PRURL:            row.PRURL,
		Note:             row.Note,
		Baseline:         baseline,
		FireAt:           timeFromNull(row.FireAt),
		ExpiresAt:        row.ExpiresAt,
		CreatedAt:        row.CreatedAt,
		State:            domain.WaitState(row.State),
		Outcome:          row.Outcome,
		Message:          row.Message,
		ResolvedAt:       timeFromNull(row.ResolvedAt),
		DeliveryAttempts: int(row.DeliveryAttempts),
		LastError:        row.LastError,
	}
}
