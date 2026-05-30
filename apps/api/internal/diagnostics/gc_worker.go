package diagnostics

import (
	"context"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
)

// ---------------------------------------------------------------------------
// PHP-error retention GC (periodic River job) — S1.1 (D)
// ---------------------------------------------------------------------------

// ErrorsGCArgs is the River job payload for the periodic PHP-error retention
// GC. It carries no fields; the worker deletes stale rows across all tenants
// using the agent GUC (app.agent = 'on') so the per-tenant RLS isolation
// policy is bypassed for this cross-tenant maintenance sweep, exactly as the
// backup retention GC does.
type ErrorsGCArgs struct{}

// Kind implements river.JobArgs.
func (ErrorsGCArgs) Kind() string { return "php_errors_retention_gc" }

// ErrorsGCWorker deletes agent_php_errors rows whose last_seen_at is older
// than the configured retention window. The default retention is 30 days —
// stale fingerprints that haven't re-appeared are unlikely to be actionable
// and accumulate over time on busy sites.
//
// The DELETE runs cross-tenant under app.agent (the same GUC the backup GC
// uses) so a single job pass sweeps the whole table without needing to
// enumerate tenant IDs. The WHERE clause is intentionally simple (no site
// filter) so Postgres can use the existing agent_php_errors_site_lastseen_idx
// via a bitmap scan; LIMIT 5000 caps the per-pass blast radius and keeps the
// transaction short.
type ErrorsGCWorker struct {
	river.WorkerDefaults[ErrorsGCArgs]
	repo      *Repo
	retention time.Duration
	logger    *slog.Logger
}

// NewErrorsGCWorker builds the PHP-error GC worker. retention is the
// last_seen_at age threshold after which a row is eligible for deletion; pass
// 0 to use the default of 30 days.
func NewErrorsGCWorker(repo *Repo, retention time.Duration, logger *slog.Logger) *ErrorsGCWorker {
	if logger == nil {
		logger = slog.Default()
	}
	if retention <= 0 {
		retention = 30 * 24 * time.Hour
	}
	return &ErrorsGCWorker{repo: repo, retention: retention, logger: logger}
}

// Work runs one GC pass.
func (w *ErrorsGCWorker) Work(ctx context.Context, _ *river.Job[ErrorsGCArgs]) error {
	deleted, err := w.repo.DeleteStaleErrors(ctx, w.retention)
	if err != nil {
		w.logger.Warn("php errors retention GC error", slog.Any("error", err))
		return err
	}
	if deleted > 0 {
		w.logger.Info("php errors retention GC",
			slog.Int64("rows_deleted", deleted),
			slog.Duration("retention", w.retention))
	}
	return nil
}
