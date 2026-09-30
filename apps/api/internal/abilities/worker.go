package abilities

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// RefreshArgs is the River payload for one site's ability inventory refresh.
type RefreshArgs struct {
	TenantID  uuid.UUID `json:"tenant_id"`
	SiteID    uuid.UUID `json:"site_id" river:"unique"`
	Scheduled bool      `json:"scheduled"`
}

// Kind implements river.JobArgs.
func (RefreshArgs) Kind() string { return "ability_inventory_refresh" }

// RefreshRateWindow is how long one site's refresh blocks another.
const RefreshRateWindow = 2 * time.Minute

// InsertOpts: three attempts, one per site per window.
func (RefreshArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		MaxAttempts: 3,
		UniqueOpts:  river.UniqueOpts{ByArgs: true, ByPeriod: RefreshRateWindow},
	}
}

// RefreshTimeout bounds one refresh job.
const RefreshTimeout = 60 * time.Second

// RefreshWorker refreshes one site.
type RefreshWorker struct {
	river.WorkerDefaults[RefreshArgs]
	svc    *Service
	logger *slog.Logger
}

// NewRefreshWorker builds the worker.
func NewRefreshWorker(svc *Service, logger *slog.Logger) *RefreshWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &RefreshWorker{svc: svc, logger: logger}
}

// Timeout bounds the whole job.
func (w *RefreshWorker) Timeout(*river.Job[RefreshArgs]) time.Duration { return RefreshTimeout }

// Work refreshes one site. A declined refresh, a reply that broke the
// contract, a changed catalogue entry and a non-retryable refusal are not
// retried; transport errors and retryable refusals are.
func (w *RefreshWorker) Work(ctx context.Context, job *river.Job[RefreshArgs]) error {
	a := job.Args
	res, err := w.svc.Refresh(ctx, a.TenantID, a.SiteID, a.Scheduled)
	switch {
	case err == nil:
		w.logger.Info("ability inventory refreshed", slog.String("site_id", a.SiteID.String()),
			slog.Int("stored", res.Stored), slog.Int("skipped_names", res.SkippedNames), slog.Bool("truncated", res.Truncated))
		return nil
	case errors.Is(err, ErrNotSendable):
		w.logger.Info("ability inventory refresh declined", slog.String("site_id", a.SiteID.String()), slog.String("reason", err.Error()))
		return nil
	case errors.Is(err, ErrInvalidInventory), errors.Is(err, ErrEntryChanged):
		w.logger.Warn("ability inventory refresh: not stored", slog.String("site_id", a.SiteID.String()), slog.Any("error", err))
		return river.JobCancel(err)
	}
	if de, ok := domain.AsDomain(err); ok && de.Kind == domain.KindNotFound {
		return river.JobCancel(err)
	}
	var refusal *agentcmd.AbilityRunRefusal
	if errors.As(err, &refusal) && !refusal.Retryable {
		w.logger.Info("ability inventory refresh refused by the agent", slog.String("site_id", a.SiteID.String()), slog.String("code", refusal.Code))
		return river.JobCancel(err)
	}
	w.logger.Warn("ability inventory refresh failed", slog.String("site_id", a.SiteID.String()), slog.Any("error", err))
	return err
}

// SweepArgs is the daily sweep payload.
type SweepArgs struct{}

// Kind implements river.JobArgs.
func (SweepArgs) Kind() string { return "ability_inventory_sweep" }

// SweepInterval: each site is refreshed about once a day.
const SweepInterval = 24 * time.Hour

const sweepSpread = 6 * time.Hour

// Enqueuer inserts refresh jobs.
type Enqueuer interface {
	EnqueueRefresh(ctx context.Context, args RefreshArgs, scheduleAt time.Time) (queued bool, err error)
}

// RiverEnqueuer enqueues onto River.
type RiverEnqueuer struct{ client *river.Client[pgx.Tx] }

// NewRiverEnqueuer wraps the started River client.
func NewRiverEnqueuer(c *river.Client[pgx.Tx]) *RiverEnqueuer { return &RiverEnqueuer{client: c} }

// EnqueueRefresh inserts one refresh job.
func (e *RiverEnqueuer) EnqueueRefresh(ctx context.Context, args RefreshArgs, scheduleAt time.Time) (bool, error) {
	opts := args.InsertOpts()
	if !scheduleAt.IsZero() {
		opts.ScheduledAt = scheduleAt
	}
	res, err := e.client.Insert(ctx, args, &opts)
	if err != nil {
		return false, fmt.Errorf("enqueue ability inventory refresh: %w", err)
	}
	return !res.UniqueSkippedAsDuplicate, nil
}

// SweepWorker enqueues a refresh for every connected, unpaused site.
type SweepWorker struct {
	river.WorkerDefaults[SweepArgs]
	svc      *Service
	enqueuer Enqueuer
	logger   *slog.Logger
	jitter   func(max time.Duration) time.Duration
}

// NewSweepWorker builds the sweep worker; the enqueuer is set after River
// starts.
func NewSweepWorker(svc *Service, logger *slog.Logger) *SweepWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &SweepWorker{svc: svc, logger: logger, jitter: func(max time.Duration) time.Duration {
		return time.Duration(rand.Int64N(int64(max)))
	}}
}

// SetEnqueuer wires the River enqueuer.
func (w *SweepWorker) SetEnqueuer(e Enqueuer) { w.enqueuer = e }

// Work enqueues one jittered refresh per site. One failure does not stop the
// rest.
func (w *SweepWorker) Work(ctx context.Context, _ *river.Job[SweepArgs]) error {
	if w.enqueuer == nil {
		w.logger.Warn("ability sweep: enqueuer not wired; skipping")
		return nil
	}
	sites, err := w.svc.repo.ListSweepSites(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	queued, failed := 0, 0
	for _, s := range sites {
		ok, err := w.enqueuer.EnqueueRefresh(ctx, RefreshArgs{TenantID: s.TenantID, SiteID: s.SiteID, Scheduled: true}, now.Add(w.jitter(sweepSpread)))
		if err != nil {
			failed++
			w.logger.Warn("ability sweep: enqueue failed", slog.String("site_id", s.SiteID.String()), slog.Any("error", err))
			continue
		}
		if ok {
			queued++
		}
	}
	w.logger.Info("ability sweep", slog.Int("sites", len(sites)), slog.Int("queued", queued), slog.Int("failed", failed))
	return nil
}
