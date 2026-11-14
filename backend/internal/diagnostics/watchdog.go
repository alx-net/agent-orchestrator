package diagnostics

import (
	"context"
	"log/slog"
	"time"
)

// LockState is a snapshot of a lock for the watchdog.
type LockState struct {
	Held      bool
	HeldSince time.Time
	// Holder says where the current holder took the lock.
	Holder string
	// Waiters is how many callers are blocked on the lock; OldestWait is when
	// the longest of them started waiting.
	Waiters    int
	OldestWait time.Time
}

// LockWatchdog polls a lock and, when it has been held or waited on for longer
// than Threshold, logs an ERROR naming the holder and writes a goroutine dump.
// It reports each stall once (and again every DumpEvery while it lasts), so a
// wedged daemon leaves a few dumps rather than one per poll.
type LockWatchdog struct {
	Name      string
	State     func() LockState
	Threshold time.Duration
	Interval  time.Duration
	// DumpEvery is the least time between two dumps from this watchdog.
	DumpEvery time.Duration
	Dumper    *Dumper
	Log       *slog.Logger
	Now       func() time.Time

	stall      time.Time // start of the stall last reported; zero when none
	reportedAt time.Time
	dumpedAt   time.Time
}

// Run polls until ctx ends.
func (w *LockWatchdog) Run(ctx context.Context) {
	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.Check()
		}
	}
}

// Check looks at the lock once.
func (w *LockWatchdog) Check() {
	now := time.Now()
	if w.Now != nil {
		now = w.Now()
	}
	st := w.State()
	var heldFor, waitedFor time.Duration
	if st.Held && !st.HeldSince.IsZero() {
		heldFor = now.Sub(st.HeldSince)
	}
	if st.Waiters > 0 && !st.OldestWait.IsZero() {
		waitedFor = now.Sub(st.OldestWait)
	}
	if heldFor < w.Threshold && waitedFor < w.Threshold {
		if !w.stall.IsZero() {
			w.Log.Warn("lock moving again", "lock", w.Name, "stalled_for", now.Sub(w.stall).Round(time.Second))
			w.stall = time.Time{}
		}
		return
	}
	// A stall is one holder's acquisition, or (holders changing but someone
	// starved) one waiter's wait.
	start := st.OldestWait
	if heldFor >= w.Threshold {
		start = st.HeldSince
	}
	if start.Equal(w.stall) && now.Sub(w.reportedAt) < w.DumpEvery {
		return
	}
	w.stall, w.reportedAt = start, now
	attrs := []any{
		"lock", w.Name,
		"held", st.Held,
		"held_for", heldFor.Round(time.Second),
		"holder", st.Holder,
		"waiters", st.Waiters,
		"oldest_wait", waitedFor.Round(time.Second),
	}
	if w.Dumper != nil && (w.dumpedAt.IsZero() || now.Sub(w.dumpedAt) >= w.DumpEvery) {
		w.dumpedAt = now
		path, err := w.Dumper.Dump("lock " + w.Name + " stuck: held by " + st.Holder)
		if err != nil {
			attrs = append(attrs, "dump_error", err)
		} else {
			attrs = append(attrs, "goroutine_dump", path)
		}
	}
	w.Log.Error("lock stuck", attrs...)
}
