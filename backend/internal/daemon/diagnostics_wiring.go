package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/diagnostics"
)

const (
	// writerStallThreshold is how long the store's writer may be held, or a
	// write wait for it, before the daemon calls it stuck. Writes take
	// milliseconds; nothing legitimate holds it for anywhere near this.
	writerStallThreshold = 45 * time.Second
	writerWatchInterval  = 5 * time.Second
	writerStallDumpEvery = 10 * time.Minute
)

// startWriterWatchdog reports a store writer that stops moving: every store
// write in the daemon queues behind it, so the UI freezes with no error. It
// runs until ctx ends; pass one that outlives shutdown, which can hang on the
// same lock.
func startWriterWatchdog(ctx context.Context, state func() diagnostics.LockState, dumper *diagnostics.Dumper, log *slog.Logger) {
	w := &diagnostics.LockWatchdog{
		Name:      "store writer",
		State:     state,
		Threshold: writerStallThreshold,
		Interval:  writerWatchInterval,
		DumpEvery: writerStallDumpEvery,
		Dumper:    dumper,
		Log:       log,
	}
	go w.Run(ctx)
}
