package site

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
)

// ADR-039 timeout thresholds. The agent heartbeats every 60s; the sweeper uses
// generous 3×/6× margins so a traffic-gated wp-cron site does not flap.
const (
	// degradeAfter — connected→degraded when last_seen_at is older than this.
	degradeAfter = 180 * time.Second
	// disconnectAfter — degraded→disconnected when last_seen_at is older than this.
	disconnectAfter = 360 * time.Second
)

// SweeperTransitioner is the subset of the ConnectionService the timeout sweeper
// drives. It is the ONLY caller of the degraded/disconnected transitions
// (ADR-039). Satisfied by *connService via the tenant-aware variants.
type SweeperTransitioner interface {
	MarkDegradedTenant(ctx context.Context, tenantID, siteID uuid.UUID) error
	MarkDisconnectedTenant(ctx context.Context, tenantID, siteID uuid.UUID, reason string) error
}

// EventPruner deletes site_events older than a cutoff (the ADR-038 ring-buffer
// prune). Satisfied by events.Publisher.
type EventPruner interface {
	PruneOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
}

// Sweeper performs the periodic connection-timeout sweep (ADR-039) and the
// periodic site_events prune (ADR-038). Pure logic over the repo + service so
// it is unit/integration-testable directly without River.
type Sweeper struct {
	repo            Repo
	svc             SweeperTransitioner
	pruner          EventPruner
	degradeAfter    time.Duration
	disconnectAfter time.Duration
	eventRetention  time.Duration
}

// NewSweeper builds a Sweeper. pruner may be nil (the events prune is skipped).
func NewSweeper(repo Repo, svc SweeperTransitioner, pruner EventPruner) *Sweeper {
	return &Sweeper{
		repo:            repo,
		svc:             svc,
		pruner:          pruner,
		degradeAfter:    degradeAfter,
		disconnectAfter: disconnectAfter,
		eventRetention:  5 * time.Minute,
	}
}

// SetThresholds overrides the timeout/retention windows (tests).
func (w *Sweeper) SetThresholds(degrade, disconnect, retention time.Duration) {
	if degrade > 0 {
		w.degradeAfter = degrade
	}
	if disconnect > 0 {
		w.disconnectAfter = disconnect
	}
	if retention > 0 {
		w.eventRetention = retention
	}
}

// Sweep runs one timeout pass relative to now: it disconnects degraded sites
// past the disconnect cutoff, then degrades connected sites past the degrade
// cutoff. It returns (degraded, disconnected) counts. Disconnect is processed
// BEFORE degrade so a site that crosses both thresholds in one pass walks
// connected→degraded→disconnected over two passes (never skipping a state).
func (w *Sweeper) Sweep(ctx context.Context, now time.Time) (degraded, disconnected int, err error) {
	// 1. degraded → disconnected (past 360s).
	disCutoff := now.Add(-w.disconnectAfter)
	toDisconnect, err := w.repo.ListToDisconnect(ctx, disCutoff)
	if err != nil {
		return 0, 0, err
	}
	for _, ref := range toDisconnect {
		if derr := w.svc.MarkDisconnectedTenant(ctx, ref.TenantID, ref.ID, "heartbeat_timeout"); derr != nil {
			return degraded, disconnected, derr
		}
		disconnected++
	}

	// 2. connected → degraded (past 180s).
	degCutoff := now.Add(-w.degradeAfter)
	toDegrade, err := w.repo.ListToDegrade(ctx, degCutoff)
	if err != nil {
		return degraded, disconnected, err
	}
	for _, ref := range toDegrade {
		if derr := w.svc.MarkDegradedTenant(ctx, ref.TenantID, ref.ID); derr != nil {
			return degraded, disconnected, derr
		}
		degraded++
	}
	return degraded, disconnected, nil
}

// PruneEvents deletes site_events older than the retention window. No-op when no
// pruner is wired. Returns the number of rows deleted.
func (w *Sweeper) PruneEvents(ctx context.Context, now time.Time) (int64, error) {
	if w.pruner == nil {
		return 0, nil
	}
	return w.pruner.PruneOlderThan(ctx, now.Add(-w.eventRetention))
}

// ---- River worker ----

// SweepArgs is the River job payload for the periodic timeout sweep.
type SweepArgs struct{}

// Kind implements river.JobArgs.
func (SweepArgs) Kind() string { return "site_connection_sweep" }

// SweepWorker runs the timeout sweep as a River job (every 15s per ADR-039).
type SweepWorker struct {
	river.WorkerDefaults[SweepArgs]
	sweeper *Sweeper
}

// NewSweepWorker builds the River worker around a Sweeper.
func NewSweepWorker(s *Sweeper) *SweepWorker { return &SweepWorker{sweeper: s} }

// Work runs one timeout sweep.
func (w *SweepWorker) Work(ctx context.Context, _ *river.Job[SweepArgs]) error {
	_, _, err := w.sweeper.Sweep(ctx, time.Now())
	return err
}

// EventPruneArgs is the River job payload for the periodic site_events prune.
type EventPruneArgs struct{}

// Kind implements river.JobArgs.
func (EventPruneArgs) Kind() string { return "site_events_prune" }

// EventPruneWorker runs the site_events ring-buffer prune as a River job (once
// a minute per ADR-038).
type EventPruneWorker struct {
	river.WorkerDefaults[EventPruneArgs]
	sweeper *Sweeper
}

// NewEventPruneWorker builds the prune worker.
func NewEventPruneWorker(s *Sweeper) *EventPruneWorker { return &EventPruneWorker{sweeper: s} }

// Work runs one site_events prune.
func (w *EventPruneWorker) Work(ctx context.Context, _ *river.Job[EventPruneArgs]) error {
	_, err := w.sweeper.PruneEvents(ctx, time.Now())
	return err
}
