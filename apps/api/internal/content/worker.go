package content

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// mapAdminErr maps the SQL writer's refusals to domain errors.
func mapAdminErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "42501":
			return domain.Forbidden("superadmin_required", "superadmin access required")
		case "23514":
			return domain.Validation("invalid_entry", "entry failed a database check")
		}
	}
	return err
}

// ---------------------------------------------------------------------------
// RefreshArgs: one site's refresh
// ---------------------------------------------------------------------------

// RefreshArgs is the River payload for one site's inventory refresh.
type RefreshArgs struct {
	TenantID  uuid.UUID `json:"tenant_id"`
	SiteID    uuid.UUID `json:"site_id"`
	Scheduled bool      `json:"scheduled"`
}

// Kind implements river.JobArgs.
func (RefreshArgs) Kind() string { return "content_inventory_refresh" }

// InsertOpts bounds the job: three attempts, no storm. The unique window also
// carries the rate limit for an operator's Refresh: a second request for the
// same site inside it is not enqueued.
func (RefreshArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		MaxAttempts: 3,
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByPeriod: RefreshRateWindow,
		},
	}
}

// RefreshRateWindow is how long a site's refresh request blocks another.
const RefreshRateWindow = 2 * time.Minute

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

// Work refreshes one site. A declined refresh (agent below the floor, site not
// connected, paused) is a normal outcome and is not retried. A refusal the
// agent marks non-retryable, and a reply that broke the contract, are not
// retried either: repeating them would send the same request for the same
// answer. Transport errors and retryable refusals are retried by River with
// its backoff, at most twice more.
func (w *RefreshWorker) Work(ctx context.Context, job *river.Job[RefreshArgs]) error {
	a := job.Args
	res, err := w.svc.Refresh(ctx, a.TenantID, a.SiteID, a.Scheduled)
	switch {
	case err == nil:
		w.logger.Info("content inventory refreshed",
			slog.String("site_id", a.SiteID.String()), slog.Int("stored", res.Stored),
			slog.Int("skipped_unknown", res.SkippedUnknown), slog.Bool("truncated", res.Truncated))
		return nil
	case errors.Is(err, ErrNotSendable):
		w.logger.Info("content inventory refresh declined",
			slog.String("site_id", a.SiteID.String()), slog.String("reason", err.Error()))
		return nil
	case errors.Is(err, ErrInvalidProbeResponse):
		w.logger.Warn("content inventory refresh: reply failed validation",
			slog.String("site_id", a.SiteID.String()), slog.Any("error", err))
		return river.JobCancel(err)
	}
	if de, ok := domain.AsDomain(err); ok && de.Kind == domain.KindNotFound {
		return river.JobCancel(err)
	}
	var refusal *agentcmd.ContentProbeRefusal
	if errors.As(err, &refusal) && !refusal.Retryable {
		w.logger.Info("content inventory refresh refused by the agent",
			slog.String("site_id", a.SiteID.String()), slog.Any("error", err))
		return river.JobCancel(err)
	}
	w.logger.Warn("content inventory refresh failed",
		slog.String("site_id", a.SiteID.String()), slog.Any("error", err))
	return err
}

// ---------------------------------------------------------------------------
// SweepArgs: the periodic fan-out
// ---------------------------------------------------------------------------

// SweepArgs is the periodic sweep payload; it has no fields.
type SweepArgs struct{}

// Kind implements river.JobArgs.
func (SweepArgs) Kind() string { return "content_inventory_sweep" }

// SweepInterval is how often the sweep runs. Each site is refreshed about once
// a day: the sweep spreads a site's refresh across the day by a per-site
// jitter, so the fleet is not all asked at once.
const SweepInterval = 24 * time.Hour

// sweepSpread is the window the fan-out jitter spreads jobs across.
const sweepSpread = 6 * time.Hour

// Enqueuer inserts refresh jobs. The concrete type wraps river.Client.
type Enqueuer interface {
	EnqueueRefresh(ctx context.Context, args RefreshArgs, scheduleAt time.Time) (queued bool, err error)
}

// RiverEnqueuer enqueues onto River.
type RiverEnqueuer struct{ client *river.Client[pgx.Tx] }

// NewRiverEnqueuer wraps the started River client.
func NewRiverEnqueuer(c *river.Client[pgx.Tx]) *RiverEnqueuer { return &RiverEnqueuer{client: c} }

// EnqueueRefresh inserts one refresh job. queued is false when an equal job
// was already inside the unique window.
func (e *RiverEnqueuer) EnqueueRefresh(ctx context.Context, args RefreshArgs, scheduleAt time.Time) (bool, error) {
	opts := args.InsertOpts()
	if !scheduleAt.IsZero() {
		opts.ScheduledAt = scheduleAt
	}
	res, err := e.client.Insert(ctx, args, &opts)
	if err != nil {
		return false, fmt.Errorf("enqueue content refresh: %w", err)
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

// NewSweepWorker builds the sweep worker. The enqueuer is set after River
// starts.
func NewSweepWorker(svc *Service, logger *slog.Logger) *SweepWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &SweepWorker{svc: svc, logger: logger, jitter: func(max time.Duration) time.Duration {
		return time.Duration(rand.Int64N(int64(max)))
	}}
}

// SetEnqueuer wires the River enqueuer after the client has started.
func (w *SweepWorker) SetEnqueuer(e Enqueuer) { w.enqueuer = e }

// Work enumerates the sites and enqueues one jittered refresh for each. One
// site failing to enqueue does not stop the rest.
func (w *SweepWorker) Work(ctx context.Context, _ *river.Job[SweepArgs]) error {
	if w.enqueuer == nil {
		w.logger.Warn("content sweep: enqueuer not wired; skipping")
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
			w.logger.Warn("content sweep: enqueue failed", slog.String("site_id", s.SiteID.String()), slog.Any("error", err))
			continue
		}
		if ok {
			queued++
		}
	}
	w.logger.Info("content sweep", slog.Int("sites", len(sites)), slog.Int("queued", queued), slog.Int("failed", failed))
	return nil
}

// RequestRefresh enqueues an operator's refresh now. It returns a rate-limit
// error when a refresh for the site is already inside the unique window.
func (s *Service) RequestRefresh(ctx context.Context, enq Enqueuer, tenantID, siteID uuid.UUID) error {
	if enq == nil {
		return domain.ServiceUnavailable("content_probe_unavailable", "page checks are not available on this install")
	}
	queued, err := enq.EnqueueRefresh(ctx, RefreshArgs{TenantID: tenantID, SiteID: siteID}, time.Time{})
	if err != nil {
		return domain.Internal("content_refresh_enqueue_failed", "could not queue the page check").WithCause(err)
	}
	if !queued {
		return domain.RateLimited("content_refresh_recent", "a page check for this site was requested a moment ago")
	}
	return nil
}
