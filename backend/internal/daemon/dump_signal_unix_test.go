//go:build !windows

package daemon

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/diagnostics"
)

func TestSIGUSR1WritesAGoroutineDumpAndKeepsRunning(t *testing.T) {
	dumper := diagnostics.NewDumper(t.TempDir())
	stop := watchGoroutineDumpSignal(dumper, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer stop()

	// Signals only this test process.
	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		dumps, _ := filepath.Glob(filepath.Join(dumper.Dir(), "goroutines-*.txt"))
		if len(dumps) == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no dump after SIGUSR1, found %v", dumps)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
