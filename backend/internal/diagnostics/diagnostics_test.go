package diagnostics

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDumpWritesEveryGoroutineAndKeepsTheNewest(t *testing.T) {
	d := NewDumper(t.TempDir())
	at := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	d.now = func() time.Time { at = at.Add(time.Second); return at }

	var paths []string
	for i := 0; i < defaultKeep+2; i++ {
		path, err := d.Dump("test")
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	body, err := os.ReadFile(paths[len(paths)-1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), "reason: test\n") || !strings.Contains(string(body), "goroutine ") ||
		!strings.Contains(string(body), "TestDumpWritesEveryGoroutineAndKeepsTheNewest") {
		t.Fatalf("dump lacks header or stacks:\n%.400s", body)
	}
	left, _ := filepath.Glob(filepath.Join(d.Dir(), dumpPrefix+"*"))
	if len(left) != defaultKeep {
		t.Fatalf("kept %d dumps, want %d", len(left), defaultKeep)
	}
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatalf("oldest dump survived pruning: %v", err)
	}
}

func TestLockWatchdogReportsAStallOnceAndDumps(t *testing.T) {
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	since := now
	state := LockState{Held: true, HeldSince: since, Holder: "store.(*Store).Example:12"}
	var logs bytes.Buffer
	w := &LockWatchdog{
		Name: "store writer", State: func() LockState { return state },
		Threshold: 45 * time.Second, DumpEvery: 10 * time.Minute,
		Dumper: NewDumper(t.TempDir()),
		Log:    slog.New(slog.NewTextHandler(&logs, nil)),
		Now:    func() time.Time { return now },
	}
	dumps := func() int {
		m, _ := filepath.Glob(filepath.Join(w.Dumper.Dir(), dumpPrefix+"*"))
		return len(m)
	}

	now = since.Add(30 * time.Second)
	w.Check()
	if logs.Len() != 0 || dumps() != 0 {
		t.Fatalf("reported a lock held under the threshold: %s", logs.String())
	}

	now = since.Add(50 * time.Second)
	w.Check()
	out := logs.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "store.(*Store).Example:12") ||
		!strings.Contains(out, "goroutine_dump=") || dumps() != 1 {
		t.Fatalf("stall not reported with holder and dump (dumps=%d): %s", dumps(), out)
	}

	logs.Reset()
	now = since.Add(2 * time.Minute)
	w.Check()
	if logs.Len() != 0 || dumps() != 1 {
		t.Fatalf("same stall reported again too soon: %s", logs.String())
	}

	now = since.Add(11 * time.Minute)
	w.Check()
	if !strings.Contains(logs.String(), "level=ERROR") || dumps() != 2 {
		t.Fatalf("long stall not reported again (dumps=%d): %s", dumps(), logs.String())
	}

	logs.Reset()
	state = LockState{}
	w.Check()
	if !strings.Contains(logs.String(), "lock moving again") {
		t.Fatalf("recovery not logged: %s", logs.String())
	}
}

func TestLockWatchdogReportsAStarvedWaiter(t *testing.T) {
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	var logs bytes.Buffer
	w := &LockWatchdog{
		Name: "store writer",
		State: func() LockState {
			return LockState{Held: true, HeldSince: now.Add(-time.Second), Waiters: 3, OldestWait: now.Add(-time.Minute)}
		},
		Threshold: 45 * time.Second, DumpEvery: 10 * time.Minute,
		Log: slog.New(slog.NewTextHandler(&logs, nil)),
		Now: func() time.Time { return now },
	}
	w.Check()
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "waiters=3") {
		t.Fatalf("starved waiter not reported: %s", logs.String())
	}
}
