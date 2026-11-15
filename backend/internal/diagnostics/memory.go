package diagnostics

import (
	"context"
	"log/slog"
	"runtime"
	"time"
)

// MemoryMonitor logs the daemon's memory now and then, and saves a heap profile
// when the live heap grows past a threshold, so a daemon that bloats leaves the
// evidence of what it holds instead of only a large number in Activity Monitor.
type MemoryMonitor struct {
	Dumper *Dumper
	Log    *slog.Logger
	// Every is how often memory is logged.
	Every time.Duration
	// ProfileAbove is the live heap, in bytes, past which a profile is saved.
	ProfileAbove uint64
	// ProfileEvery limits automatic profiles to one per this long.
	ProfileEvery time.Duration

	lastProfile time.Time
}

// Run logs until ctx ends.
func (m *MemoryMonitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.Every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.Check(time.Now())
		}
	}
}

// Check logs memory once and saves a profile if the heap is past the threshold.
func (m *MemoryMonitor) Check(now time.Time) {
	m.Log.Info("daemon " + MemorySummary())
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	if m.ProfileAbove == 0 || stats.HeapAlloc < m.ProfileAbove || m.Dumper == nil {
		return
	}
	if !m.lastProfile.IsZero() && now.Sub(m.lastProfile) < m.ProfileEvery {
		return
	}
	m.lastProfile = now
	path, err := m.Dumper.HeapProfile()
	if err != nil {
		m.Log.Error("heap profile failed", "err", err)
		return
	}
	m.Log.Warn("daemon heap is large; heap profile written", "path", path, "heapMiB", stats.HeapAlloc>>20)
}
