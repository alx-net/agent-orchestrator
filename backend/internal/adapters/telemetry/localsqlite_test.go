package telemetry

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	sqlitestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type pruneCountingStore struct {
	backlog int64
	calls   int
}

func (*pruneCountingStore) CreateTelemetryEvent(context.Context, sqlitestore.TelemetryEventRecord) error {
	return nil
}

func (s *pruneCountingStore) PruneTelemetryEventsBefore(_ context.Context, _ time.Time, limit int64) (int64, error) {
	s.calls++
	deleted := min(limit, s.backlog)
	s.backlog -= deleted
	return deleted, nil
}

// Break caught: one prune pass deleted at most one batch per hour, so a sink
// writing more than that per hour never caught up with its retention window.
func TestLocalSQLiteSinkPruneDrainsFullBatches(t *testing.T) {
	for _, tc := range []struct {
		name      string
		backlog   int64
		wantCalls int
		wantLeft  int64
	}{
		{name: "partial batch stops", backlog: 10, wantCalls: 1},
		{name: "exact batches confirm empty", backlog: 3 * localPruneBatchLimit, wantCalls: 4},
		{name: "bounded per pass", backlog: (localPruneMaxBatches + 5) * localPruneBatchLimit, wantCalls: localPruneMaxBatches, wantLeft: 5 * localPruneBatchLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &pruneCountingStore{backlog: tc.backlog}
			sink := &LocalSQLiteSink{store: store, log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: time.Now}
			sink.maybePrune()
			if store.calls != tc.wantCalls || store.backlog != tc.wantLeft {
				t.Fatalf("prune calls = %d, left = %d; want %d, %d", store.calls, store.backlog, tc.wantCalls, tc.wantLeft)
			}
		})
	}
}
