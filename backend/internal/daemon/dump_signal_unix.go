//go:build !windows

package daemon

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/aoagents/agent-orchestrator/backend/internal/diagnostics"
)

// watchGoroutineDumpSignal writes a goroutine dump on SIGUSR1. SIGQUIT prints
// the same stacks but kills the daemon, which is the opposite of what you want
// when it is stuck and holding everyone's sessions.
func watchGoroutineDumpSignal(dumper *diagnostics.Dumper, log *slog.Logger) (stop func()) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR1)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-signals:
				path, err := dumper.Dump("SIGUSR1")
				if err != nil {
					log.Error("goroutine dump failed", "err", err)
					continue
				}
				log.Warn("goroutine dump written", "path", path)
			}
		}
	}()
	return func() {
		signal.Stop(signals)
		close(done)
	}
}
