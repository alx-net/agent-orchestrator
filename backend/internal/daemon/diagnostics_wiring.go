package daemon

import (
	"context"
	"log/slog"
	"os"
	"runtime/debug"
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

// daemonMemoryLimit is the soft limit the Go runtime collects toward. With the
// default GOGC alone the heap may grow to twice what is live before a
// collection, and freed memory reaches the OS only slowly: the daemon showed
// 2.2 GB in Activity Monitor. GOMEMLIMIT in the environment overrides it.
const daemonMemoryLimit = 1536 << 20

// startMemoryWatch sets the soft memory limit, logs the daemon's memory every
// ten minutes, and saves a heap profile when the live heap passes 1 GiB.
func startMemoryWatch(ctx context.Context, dumper *diagnostics.Dumper, log *slog.Logger) {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(daemonMemoryLimit)
	}
	m := &diagnostics.MemoryMonitor{
		Dumper: dumper, Log: log, Every: 10 * time.Minute,
		ProfileAbove: 1 << 30, ProfileEvery: time.Hour,
	}
	go m.Run(ctx)
}
