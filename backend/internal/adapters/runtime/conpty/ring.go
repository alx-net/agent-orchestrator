package conpty

import (
	"bytes"
	"strings"
	"sync"
)

// MaxOutputLines is the rolling line-buffer cap, matching MAX_OUTPUT_LINES in pty-host.ts.
const MaxOutputLines = 1000

// MaxPartialLineBytes caps the retained tail of the in-progress line. Full-screen
// TUIs repaint with carriage returns and cursor moves and may never emit "\n",
// so without a cap the partial line would grow for the life of the process.
const MaxPartialLineBytes = 64 * 1024

// Ring is a bounded rolling buffer of terminal output lines, ANSI codes preserved.
// It mirrors the appendOutput state machine from pty-host.ts.
// Concurrent Append and Snapshot/Tail calls are safe.
type Ring struct {
	mu          sync.Mutex
	lines       []string // each entry is "line\n" (or bare text on FlushPartial)
	partialLine []byte   // may hold up to 2*MaxPartialLineBytes; read via partialTail
}

// NewRing returns an empty Ring.
func NewRing() *Ring {
	return &Ring{}
}

// Append mirrors appendOutput from pty-host.ts: prepend the current partialLine,
// split on newlines, store completed lines with "\n" re-appended, keep the last
// element as the new partialLine, then trim to MaxOutputLines. Only the last
// MaxPartialLineBytes of an unterminated line are retained.
func (r *Ring) Append(raw []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	last := bytes.LastIndexByte(raw, '\n')
	if last < 0 {
		// No line completed: extend the partial line in place (amortized append)
		// instead of re-copying it on every chunk.
		r.appendPartialLocked(raw)
		return
	}
	text := string(r.partialTail()) + string(raw[:last])
	for _, line := range strings.Split(text, "\n") {
		r.lines = append(r.lines, line+"\n")
	}
	r.partialLine = r.partialLine[:0]
	r.appendPartialLocked(raw[last+1:])
	if len(r.lines) > MaxOutputLines {
		// ponytail: slice off the head; ceiling: O(n) copy on every trim cycle.
		// Upgrade path: circular buffer if trim rate is very high.
		r.lines = r.lines[len(r.lines)-MaxOutputLines:]
	}
}

// appendPartialLocked extends partialLine, compacting it back to the last
// MaxPartialLineBytes once it reaches twice that size so trimming stays amortized.
func (r *Ring) appendPartialLocked(b []byte) {
	r.partialLine = append(r.partialLine, b...)
	if len(r.partialLine) > 2*MaxPartialLineBytes {
		n := copy(r.partialLine, r.partialLine[len(r.partialLine)-MaxPartialLineBytes:])
		r.partialLine = r.partialLine[:n]
	}
}

// partialTail returns the retained (at most MaxPartialLineBytes) partial line.
func (r *Ring) partialTail() []byte {
	if len(r.partialLine) > MaxPartialLineBytes {
		return r.partialLine[len(r.partialLine)-MaxPartialLineBytes:]
	}
	return r.partialLine
}

// FlushPartial pushes any in-progress partial line as a final entry.
// Called on PTY exit to mirror the pty-host.ts onExit handler.
func (r *Ring) FlushPartial() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.partialLine) == 0 {
		return
	}
	r.lines = append(r.lines, string(r.partialTail()))
	r.partialLine = nil
}

// Snapshot returns all stored lines concatenated as raw bytes for scrollback replay.
// The in-progress partialLine is NOT included (matches TS outputBuffer.join("")).
func (r *Ring) Snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	return []byte(strings.Join(r.lines, ""))
}

// Replay returns the complete terminal byte stream retained for a newly
// attached viewer, including the current non-newline-terminated fragment.
// Full-screen TUIs commonly repaint with cursor-control sequences and carriage
// returns instead of newlines, so excluding partialLine can otherwise replay an
// empty screen even though the process has already drawn its UI.
func (r *Ring) Replay() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	return []byte(strings.Join(r.lines, "") + string(r.partialTail()))
}

// Tail returns the last n stored lines joined as a string.
// Mirrors the MSG_GET_OUTPUT_REQ handler: start = max(0, len-lines).
// n <= 0 returns "".
func (r *Ring) Tail(n int) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	if n <= 0 {
		return ""
	}
	start := len(r.lines) - n
	if start < 0 {
		start = 0
	}
	return strings.Join(r.lines[start:], "")
}
