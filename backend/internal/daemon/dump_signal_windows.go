//go:build windows

package daemon

import (
	"log/slog"

	"github.com/aoagents/agent-orchestrator/backend/internal/diagnostics"
)

// Windows has no SIGUSR1; the stuck-writer watchdog still writes dumps there.
func watchGoroutineDumpSignal(*diagnostics.Dumper, *slog.Logger) (stop func()) { return func() {} }
