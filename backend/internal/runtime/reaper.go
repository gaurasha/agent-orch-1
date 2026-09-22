package runtime

import (
	"context"
	"time"

	"github.com/gaurasha/agent-orch/backend/internal/obs"
	"github.com/gaurasha/agent-orch/backend/internal/store"
)

// Reaper is the platform's failure detector, in its entirety.
//
// There is no membership protocol, no leader election and no heartbeat service
// to get wrong. Two periodic sweeps against the database:
//
//   - expired leases go back to QUEUED, so a run whose worker died is picked up
//     by another worker within one lease TTL;
//   - tool calls left IN_FLIGHT past a deadline are failed with an explicitly
//     ambiguous message, so a replaying worker eventually gets a definite
//     answer instead of blocking forever on a call nobody will ever resolve.
//
// It is safe to run many reapers concurrently: both operations are single
// idempotent SQL statements.
type Reaper struct {
	store    store.Store
	interval time.Duration
	// stuckAfter should comfortably exceed the longest tool timeout, or we will
	// declare live calls abandoned.
	stuckAfter time.Duration
	metrics    *obs.Metrics
	log        *obs.Logger
}

func NewReaper(st store.Store, interval, stuckAfter time.Duration, m *obs.Metrics, log *obs.Logger) *Reaper {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if stuckAfter <= 0 {
		stuckAfter = 5 * time.Minute
	}
	return &Reaper{store: st, interval: interval, stuckAfter: stuckAfter, metrics: m, log: log}
}

func (r *Reaper) Run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	r.log.Info("reaper started", "interval", r.interval.String(), "stuck_after", r.stuckAfter.String())
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := r.store.ReapExpiredLeases(ctx); err != nil {
				r.log.Error("reaping expired leases failed", "err", err)
			} else if n > 0 {
				r.metrics.LeaseReaped(n)
				r.log.Warn("reclaimed runs whose worker stopped renewing", "count", n)
			}
			if n, err := r.store.ReapStuckToolCalls(ctx, r.stuckAfter); err != nil {
				r.log.Error("reaping stuck tool calls failed", "err", err)
			} else if n > 0 {
				r.log.Warn("failed tool calls with no reported outcome; agents will be told the result is unknown",
					"count", n)
			}
		}
	}
}
