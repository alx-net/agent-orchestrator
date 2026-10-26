package conpty

import (
	"strings"
	"sync"
	"testing"
)

// TestRingAppendPartialThenComplete verifies partial-line accumulation and
// that Snapshot/Tail reflect only completed lines.
func TestRingAppendPartialThenComplete(t *testing.T) {
	r := NewRing()
	r.Append([]byte("hel"))
	r.Append([]byte("lo\nwor"))

	snap := string(r.Snapshot())
	if snap != "hello\n" {
		t.Errorf("Snapshot = %q, want %q", snap, "hello\n")
	}

	tail := r.Tail(10)
	if tail != "hello\n" {
		t.Errorf("Tail(10) = %q, want %q", tail, "hello\n")
	}

	// Flush the partial "wor"
	r.FlushPartial()
	snap = string(r.Snapshot())
	if snap != "hello\nwor" {
		t.Errorf("after FlushPartial Snapshot = %q, want %q", snap, "hello\nwor")
	}
}

// TestRingExceedsMaxOutputLines verifies the buffer trims to MaxOutputLines.
func TestRingExceedsMaxOutputLines(t *testing.T) {
	r := NewRing()
	// Push 1005 lines.
	for i := 0; i < 1005; i++ {
		r.Append([]byte("x\n"))
	}

	snap := r.Snapshot()
	got := strings.Count(string(snap), "\n")
	if got != MaxOutputLines {
		t.Errorf("stored %d lines, want %d", got, MaxOutputLines)
	}
}

// TestRingFlushPartialNoNewline verifies FlushPartial pushes a trailing line.
func TestRingFlushPartialNoNewline(t *testing.T) {
	r := NewRing()
	r.Append([]byte("line1\npartial"))
	r.FlushPartial()

	snap := string(r.Snapshot())
	if !strings.Contains(snap, "partial") {
		t.Errorf("Snapshot missing 'partial': %q", snap)
	}

	// Calling FlushPartial again is a no-op.
	r.FlushPartial()
	snap2 := string(r.Snapshot())
	if snap2 != snap {
		t.Errorf("second FlushPartial changed snapshot: %q -> %q", snap, snap2)
	}
}

// TestRingTailEdgeCases covers n > stored count and n <= 0.
func TestRingTailEdgeCases(t *testing.T) {
	r := NewRing()
	r.Append([]byte("a\nb\n"))

	if got := r.Tail(100); got != "a\nb\n" {
		t.Errorf("Tail(100) = %q, want %q", got, "a\nb\n")
	}
	if got := r.Tail(0); got != "" {
		t.Errorf("Tail(0) = %q, want empty", got)
	}
	if got := r.Tail(-1); got != "" {
		t.Errorf("Tail(-1) = %q, want empty", got)
	}
}

// TestRingANSIRoundTrip verifies raw ANSI escape sequences survive storage intact.
func TestRingANSIRoundTrip(t *testing.T) {
	ansi := "\x1b[31mhi\x1b[0m\n"
	r := NewRing()
	r.Append([]byte(ansi))

	snap := string(r.Snapshot())
	if snap != ansi {
		t.Errorf("Snapshot = %q, want %q", snap, ansi)
	}
	tail := r.Tail(1)
	if tail != ansi {
		t.Errorf("Tail(1) = %q, want %q", tail, ansi)
	}
}

// TestRingTailSubset verifies Tail returns exactly the last n lines.
func TestRingTailSubset(t *testing.T) {
	r := NewRing()
	for i := 0; i < 10; i++ {
		r.Append([]byte("line\n"))
	}

	tail3 := r.Tail(3)
	if got := strings.Count(tail3, "\n"); got != 3 {
		t.Errorf("Tail(3) contains %d newlines, want 3", got)
	}
}

// TestRingSnapshotExcludesPartial verifies the in-progress partial line is NOT
// included in Snapshot (matches TS semantics: only outputBuffer, not partialLine).
func TestRingSnapshotExcludesPartial(t *testing.T) {
	r := NewRing()
	r.Append([]byte("complete\npartial"))

	snap := string(r.Snapshot())
	if strings.Contains(snap, "partial") {
		t.Errorf("Snapshot includes partial line: %q", snap)
	}
	if !strings.Contains(snap, "complete\n") {
		t.Errorf("Snapshot missing complete line: %q", snap)
	}
}

func TestRingReplayIncludesPartialTUIOutput(t *testing.T) {
	r := NewRing()
	r.Append([]byte("complete\n\x1b[?1049h\rcurrent tui"))

	if got, want := string(r.Replay()), "complete\n\x1b[?1049h\rcurrent tui"; got != want {
		t.Errorf("Replay = %q, want %q", got, want)
	}
}

// TestRingCapsPartialLine verifies a TUI that never emits "\n" cannot grow the
// partial line without bound, and that only the newest bytes are kept.
func TestRingCapsPartialLine(t *testing.T) {
	r := NewRing()
	chunk := []byte("\r" + strings.Repeat("x", 1023))
	for i := 0; i < 1000; i++ { // ~1 MiB of redraws with no newline
		r.Append(chunk)
	}
	r.Append([]byte("\rtail"))

	replay := r.Replay()
	if len(replay) != MaxPartialLineBytes {
		t.Fatalf("Replay len = %d, want %d", len(replay), MaxPartialLineBytes)
	}
	if !strings.HasSuffix(string(replay), "\rtail") {
		t.Fatalf("Replay lost the newest bytes: ...%q", replay[len(replay)-16:])
	}
	if c := cap(r.partialLine); c > 4*MaxPartialLineBytes {
		t.Fatalf("partialLine capacity = %d, want bounded", c)
	}

	// Completing the line stores only the capped tail.
	r.Append([]byte("\nnext"))
	if got := len(r.Snapshot()); got != MaxPartialLineBytes+1 {
		t.Fatalf("Snapshot len = %d, want %d", got, MaxPartialLineBytes+1)
	}
	if got, want := string(r.Replay()[MaxPartialLineBytes+1:]), "next"; got != want {
		t.Fatalf("partial after newline = %q, want %q", got, want)
	}
}

// TestRingConcurrent validates the advertised goroutine-safety of Ring under the
// race detector. It spawns 10 writer goroutines (Append) and 10 reader goroutines
// (Snapshot + Tail) that all run concurrently; any data race will be caught by
// "go test -race". The test itself only asserts no panic and no race.
func TestRingConcurrent(t *testing.T) {
	const goroutines = 10
	const iters = 100

	r := NewRing()
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				r.Append([]byte("line\n"))
			}
		}()
	}

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				_ = r.Snapshot()
				_ = r.Tail(10)
			}
		}()
	}

	wg.Wait()
}
