package store

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/diagnostics"
)

// lockHolderDepth is how many frames of the holder's stack are kept. Enough to
// get past the store's own helpers to the service that called them.
const lockHolderDepth = 10

// contextMutex is the store's single-writer lock. It also records who holds it
// and how long callers have waited, so the daemon's watchdog can name the
// holder when every write stops. Recording costs one runtime.Callers and one
// small allocation per acquisition; frames are only resolved when reported.
type contextMutex struct {
	token  chan struct{}
	holder atomic.Pointer[lockHolder]

	waitMu  sync.Mutex // guards the fields below; taken only when the lock is busy
	waitSeq uint64
	waiting map[uint64]time.Time
}

type lockHolder struct {
	since time.Time
	note  string
	n     int
	pcs   [lockHolderDepth]uintptr
}

func newContextMutex() *contextMutex {
	mutex := &contextMutex{token: make(chan struct{}, 1), waiting: map[uint64]time.Time{}}
	mutex.token <- struct{}{}
	return mutex
}

func (m *contextMutex) Lock() {
	m.take(nil)
	m.hold("")
}

// lockFor is Lock with a note saying what the holder is doing, for locks held
// across caller code (a provider event projection runs the controller's
// projection inside it).
func (m *contextMutex) lockFor(note string) {
	m.take(nil)
	m.hold(note)
}

func (m *contextMutex) LockContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !m.take(ctx.Done()) {
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		m.token <- struct{}{}
		return err
	}
	m.hold("")
	return nil
}

func (m *contextMutex) Unlock() {
	// Clear before releasing, or the next holder's record could be wiped.
	m.holder.Store(nil)
	m.token <- struct{}{}
}

// take waits for the token. It returns false when done closes first.
func (m *contextMutex) take(done <-chan struct{}) bool {
	select {
	case <-m.token:
		return true
	default:
	}
	m.waitMu.Lock()
	m.waitSeq++
	id := m.waitSeq
	m.waiting[id] = time.Now()
	m.waitMu.Unlock()
	defer func() {
		m.waitMu.Lock()
		delete(m.waiting, id)
		m.waitMu.Unlock()
	}()
	select {
	case <-m.token:
		return true
	case <-done:
		return false
	}
}

// hold records the caller of Lock, lockFor or LockContext as the holder.
func (m *contextMutex) hold(note string) {
	h := &lockHolder{since: time.Now(), note: note}
	// Skip runtime.Callers, hold, and the Lock method.
	h.n = runtime.Callers(3, h.pcs[:])
	m.holder.Store(h)
}

func (m *contextMutex) state() diagnostics.LockState {
	var st diagnostics.LockState
	if h := m.holder.Load(); h != nil {
		st.Held, st.HeldSince, st.Holder = true, h.since, h.describe()
	} else if len(m.token) == 0 {
		// Taken but not yet recorded, or held across a hold/Unlock race.
		st.Held = true
	}
	m.waitMu.Lock()
	st.Waiters = len(m.waiting)
	for _, at := range m.waiting {
		if st.OldestWait.IsZero() || at.Before(st.OldestWait) {
			st.OldestWait = at
		}
	}
	m.waitMu.Unlock()
	return st
}

func (h *lockHolder) describe() string {
	var b strings.Builder
	if h.note != "" {
		b.WriteString(h.note)
		b.WriteString(": ")
	}
	frames := runtime.CallersFrames(h.pcs[:h.n])
	for i := 0; ; i++ {
		f, more := frames.Next()
		if i > 0 {
			b.WriteString(" <- ")
		}
		name := f.Function
		if slash := strings.LastIndexByte(name, '/'); slash >= 0 {
			name = name[slash+1:]
		}
		fmt.Fprintf(&b, "%s:%d", name, f.Line)
		if !more {
			break
		}
	}
	return b.String()
}

// WriterLockState reports who holds the store's writer and how long writes
// have queued for it, for the daemon's stuck-writer watchdog.
func (s *Store) WriterLockState() diagnostics.LockState {
	return s.writeMu.state()
}
