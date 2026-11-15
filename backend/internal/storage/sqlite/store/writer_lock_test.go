package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func holdWriterForTest(m *contextMutex) { m.Lock() }

func TestWriterLockStateNamesTheHolderAndItsWaiters(t *testing.T) {
	m := newContextMutex()
	if st := m.state(); st.Held || st.Waiters != 0 {
		t.Fatalf("idle lock state = %+v", st)
	}

	holdWriterForTest(m)
	st := m.state()
	if !st.Held || st.HeldSince.IsZero() {
		t.Fatalf("held lock state = %+v", st)
	}
	if !strings.Contains(st.Holder, "holdWriterForTest") {
		t.Fatalf("holder = %q, want the caller of Lock", st.Holder)
	}

	ctx, cancel := context.WithCancel(context.Background())
	waited := make(chan error, 1)
	go func() { waited <- m.LockContext(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for m.state().Waiters != 1 {
		if time.Now().After(deadline) {
			t.Fatal("waiter never registered")
		}
		time.Sleep(time.Millisecond)
	}
	if st := m.state(); st.OldestWait.IsZero() {
		t.Fatalf("waiting lock state = %+v", st)
	}
	cancel()
	if err := <-waited; err == nil {
		t.Fatal("cancelled wait took the lock")
	}
	if st := m.state(); st.Waiters != 0 {
		t.Fatalf("waiter still counted after giving up: %+v", st)
	}

	m.Unlock()
	if st := m.state(); st.Held || st.Holder != "" {
		t.Fatalf("released lock state = %+v", st)
	}
}

func TestWriterLockNoteLeadsTheHolder(t *testing.T) {
	m := newContextMutex()
	m.lockFor("provider event projection x")
	defer m.Unlock()
	if st := m.state(); !strings.HasPrefix(st.Holder, "provider event projection x: ") {
		t.Fatalf("holder = %q", st.Holder)
	}
}
