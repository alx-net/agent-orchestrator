package daemon

import (
	"context"
	"log/slog"

	scmmulti "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/multi"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	waitsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/wait"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// startWaits starts the `ao wait` evaluator. Wakes go through lifecycle's
// guarded automation-message path, like CI and review nudges. The provider is
// only used for PRs no live AO session tracks; with no SCM credentials it is
// nil, and only observer-tracked PRs (and timers) can be waited on. Pending
// waits are durable, so the first pass after a restart picks them back up.
func startWaits(ctx context.Context, store *sqlite.Store, lcm *lifecycle.Manager, provider *scmmulti.Provider, logger *slog.Logger) (*waitsvc.Service, <-chan struct{}) {
	deps := waitsvc.Deps{Store: store, Deliverer: lcm, Logger: logger}
	// Assigned only when present: a nil *Provider in the interface would read
	// as an available provider.
	if provider != nil {
		deps.PRs = provider
	}
	service := waitsvc.New(deps)
	done := service.Start(ctx)
	return service, done
}
