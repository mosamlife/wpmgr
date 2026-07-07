// purge_worker.go — GH #152 part 2: the async grace-window purge.
//
// A periodic River job finds every soft-deleted tenant whose grace window has
// elapsed and, for each, in order: (a) best-effort revoke every connected
// site, (b) mark the point-of-no-return (purge_started_at, adversarial-review
// fast-follow M2) then delete the tenant's object-storage prefixes across ALL
// SEVEN tenant-scoped roots (the ONLY thing that frees those bytes — the DB
// cascade inside admin_purge_tenant frees zero storage), (c) the privileged
// hard delete via admin_purge_tenant. Every step is idempotent/resumable: a
// crash mid-purge is safe to retry on the next tick (see purgeOne's per-step
// doc comments).
//
// The seven object-storage roots (adversarial-review fast-follow H1 — the
// original build only deleted the first two, silently orphaning the other
// five in object storage forever, including client-PII report PDFs):
//
//	chunks/<tenantID>/       — backup chunk ciphertext (backup/model.go chunkS3Key)
//	tenant/<tenantID>/       — backup manifest.json index objects (backup/service.go manifestIndexKey)
//	tenants/<tenantID>/      — white-label report PDFs/HTML (report/worker.go blobKey;
//	                           NOTE plural "tenants/", distinct from the singular "tenant/" above)
//	screenshots/<tenantID>/  — site screenshot webp captures (screenshot/capture/worker.go)
//	media/<tenantID>/        — Media Optimizer job src/out objects + font-src uploads
//	                           (media/domain.go JobPrefix; media/font/args.go DeriveSourceKey)
//	fonts/<tenantID>/        — transcoded/subset WOFF2 outputs (media/font/args.go DeriveWoff2Key)
//	rucss-src/<tenantID>/    — RUCSS source-bundle temp objects (perf/rucss.go rucssBundleKey)
package org

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// DefaultPurgeGrace is the fallback grace window (WPMGR_ORG_PURGE_GRACE_DAYS)
// between a Lane-B soft-delete and the destructive purge.
const DefaultPurgeGrace = 7 * 24 * time.Hour

// PurgeQueue is the River queue the periodic purge sweep runs on.
const PurgeQueue = "org_purge"

// PurgeArgs is the (empty) periodic job payload.
type PurgeArgs struct{}

// Kind implements river.JobArgs.
func (PurgeArgs) Kind() string { return "org_purge" }

// SiteRevoker is the subset of site.ConnectionService the purge worker needs.
type SiteRevoker interface {
	Revoke(ctx context.Context, in site.ActorSiteInput) (site.Site, error)
}

// SiteLister resolves the (non-archived) site IDs owned by a tenant.
type SiteLister interface {
	ListAllSiteIDs(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error)
}

// ObjectPurger is the object-storage capability the purge needs: list keys
// under a prefix, and delete a key. *blobstore.Store satisfies this directly
// (List(ctx, prefix) ([]string, error); Delete(ctx, key) error) — no adapter
// needed.
type ObjectPurger interface {
	List(ctx context.Context, prefix string) ([]string, error)
	Delete(ctx context.Context, key string) error
}

// PurgeWorker drives the daily grace-window purge sweep.
type PurgeWorker struct {
	river.WorkerDefaults[PurgeArgs]
	pool    *db.Pool
	sites   SiteLister   // nil disables the revoke step (best-effort anyway)
	revoker SiteRevoker  // nil disables the revoke step
	store   ObjectPurger // nil disables the object-storage purge step (no S3 configured)
	grace   time.Duration
	logger  *slog.Logger
}

// NewPurgeWorker builds a PurgeWorker. grace <= 0 uses DefaultPurgeGrace.
// sites/revoker/store may be nil (best-effort steps are skipped, logged).
func NewPurgeWorker(pool *db.Pool, sites SiteLister, revoker SiteRevoker, store ObjectPurger, grace time.Duration, logger *slog.Logger) *PurgeWorker {
	if grace <= 0 {
		grace = DefaultPurgeGrace
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &PurgeWorker{pool: pool, sites: sites, revoker: revoker, store: store, grace: grace, logger: logger}
}

// Timeout gives the sweep 30 minutes — generous headroom for a handful of
// tenants each needing a site-revoke pass plus an object-storage list/delete
// pass, well above the expected wall time this early-stage feature will see.
func (w *PurgeWorker) Timeout(*river.Job[PurgeArgs]) time.Duration {
	return 30 * time.Minute
}

// Work runs one purge sweep: every tenant past its grace window, one at a
// time. A single tenant's failure is logged and does NOT abort the sweep —
// every other eligible tenant still gets a chance this tick, and the failed
// one is retried automatically on the next tick (nothing here is destructive
// until purgeOne's very last step).
func (w *PurgeWorker) Work(ctx context.Context, _ *river.Job[PurgeArgs]) error {
	cutoff := time.Now().UTC().Add(-w.grace)
	tenants, err := sqlc.New(w.pool.Pool).ListTenantsPendingPurge(ctx, pgtype.Timestamptz{Time: cutoff, Valid: true})
	if err != nil {
		return fmt.Errorf("org purge: list pending: %w", err)
	}
	var purged, skipped, failed int
	for _, t := range tenants {
		outcome, perr := w.purgeOne(ctx, t.ID)
		switch {
		case perr != nil:
			failed++
			w.logger.Error("org purge: tenant purge failed (will retry next tick)",
				slog.String("tenant_id", t.ID.String()), slog.Any("error", perr))
		case outcome == purgeSkipped:
			skipped++
		default:
			purged++
		}
	}
	w.logger.Info("org purge: sweep complete",
		slog.Int("purged", purged), slog.Int("skipped", skipped), slog.Int("failed", failed))
	return nil
}

// objectStoragePrefixes returns the SEVEN tenant-scoped object-storage roots
// that must be purged for tenantID (adversarial-review fast-follow H1). Every
// root is a fixed 36-char-UUID prefix with a trailing "/", so the existing
// no-empty-prefix / no-cross-tenant guarantees hold for each. Exported as its
// own function (rather than inlined in purgeOne) so a unit test can assert
// the exact set without needing a live Postgres.
func objectStoragePrefixes(tenantID uuid.UUID) []string {
	id := tenantID.String()
	return []string{
		"chunks/" + id + "/",      // backup chunk ciphertext (backup/model.go chunkS3Key)
		"tenant/" + id + "/",      // backup manifest.json index objects (backup/service.go manifestIndexKey)
		"tenants/" + id + "/",     // white-label report PDFs/HTML (report/worker.go blobKey) — plural, distinct from "tenant/"
		"screenshots/" + id + "/", // site screenshot webp captures (screenshot/capture/worker.go)
		"media/" + id + "/",       // Media Optimizer job src/out + font-src uploads (media/domain.go, media/font/args.go)
		"fonts/" + id + "/",       // transcoded/subset WOFF2 outputs (media/font/args.go DeriveWoff2Key)
		"rucss-src/" + id + "/",   // RUCSS source-bundle temp objects (perf/rucss.go rucssBundleKey)
	}
}

type purgeOutcome int

const (
	purgeDone purgeOutcome = iota
	purgeSkipped
)

// purgeOne purges exactly one tenant.
//
// Locking: takes the per-tenant "org_lifecycle" SESSION advisory lock
// (mirrors backup gc.go's SweepTenantChunks pattern — a pg_try_advisory_lock
// on a dedicated, pinned connection, held for the WHOLE purge including
// network I/O) rather than a transaction-scoped lock: revoke + object-storage
// list/delete are slow network calls, not just DB work, so a xact-scoped lock
// would either hold a transaction open across them or need re-acquiring
// mid-purge — the session lock avoids both. It shares the SAME key namespace
// as DELETE /orgs/{orgId} and POST /orgs/{orgId}/restore's own
// pg_advisory_xact_lock (see delete_handler.go's orgLifecycleLockKey doc
// comment), so a delete/restore request and this worker's purge of the SAME
// tenant always serialize — in particular, a concurrent restore either wins
// the lock first (this purge then finds deleted_at cleared under the lock and
// skips) or loses it (the restore blocks until the purge either completes,
// at which point RestoreTenant affects 0 rows and the caller gets
// "already purged", or fails/skips without purging, at which point the
// restore proceeds normally). A tenant already being purged by another CP
// instance (or a still-running previous tick) is skipped, not retried, this
// tick.
func (w *PurgeWorker) purgeOne(ctx context.Context, tenantID uuid.UUID) (purgeOutcome, error) {
	conn, err := w.pool.Pool.Acquire(ctx)
	if err != nil {
		return purgeDone, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	var acquired bool
	if err := conn.QueryRow(ctx,
		`SELECT pg_try_advisory_lock(hashtext($1), hashtext($2))`,
		orgLifecycleLockKey, tenantID.String(),
	).Scan(&acquired); err != nil {
		return purgeDone, fmt.Errorf("advisory lock: %w", err)
	}
	if !acquired {
		w.logger.Info("org purge: tenant lock held elsewhere, skipping this tick",
			slog.String("tenant_id", tenantID.String()))
		return purgeSkipped, nil
	}
	defer func() {
		if _, uerr := conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext($1), hashtext($2))`, orgLifecycleLockKey, tenantID.String()); uerr != nil {
			w.logger.Warn("org purge: advisory unlock failed (released on connection close regardless)",
				slog.String("tenant_id", tenantID.String()), slog.Any("error", uerr))
		}
	}()

	// Re-check under the lock: a concurrent restore may have already cleared
	// deleted_at (see delete_handler.go's restore, which blocks on this same
	// lock and only proceeds once it wins the race).
	fresh, gerr := sqlc.New(w.pool.Pool).GetTenant(ctx, tenantID)
	if gerr != nil {
		if errors.Is(gerr, pgx.ErrNoRows) {
			return purgeDone, nil // already purged by a previous run
		}
		return purgeDone, fmt.Errorf("reload tenant: %w", gerr)
	}
	if !fresh.DeletedAt.Valid {
		return purgeSkipped, nil // restored out from under us; nothing to purge
	}

	// Step (a): best-effort revoke every connected site. Offline agents never
	// receive this — they only learn on their next heartbeat/poll, which will
	// never come once the tenant no longer exists — so this is advisory, not a
	// hard dependency for the purge to proceed. Idempotent: Revoke on an
	// already-revoked/archived site is either a no-op success (self-transition)
	// or a domain.KindConflict "illegal_transition" that we treat the same way
	// (nothing left to do for that site).
	if w.revoker != nil && w.sites != nil {
		siteIDs, serr := w.sites.ListAllSiteIDs(ctx, tenantID)
		if serr != nil {
			w.logger.Warn("org purge: list sites failed (continuing best-effort)",
				slog.String("tenant_id", tenantID.String()), slog.Any("error", serr))
		}
		for _, sid := range siteIDs {
			if _, rerr := w.revoker.Revoke(ctx, site.ActorSiteInput{TenantID: tenantID, SiteID: sid, Reason: "org_deleted"}); rerr != nil {
				if de, ok := domain.AsDomain(rerr); ok && de.Kind == domain.KindConflict {
					continue // already revoked/archived/not-yet-enrolled — nothing to do
				}
				w.logger.Warn("org purge: revoke site failed (continuing best-effort)",
					slog.String("tenant_id", tenantID.String()), slog.String("site_id", sid.String()), slog.Any("error", rerr))
			}
		}
	}

	// Point-of-no-return marker (adversarial-review fast-follow M2): set
	// BEFORE the first object-storage delete below. Object deletion is
	// irreversible; a DB-only soft-delete is not. Without this marker, a
	// transient storage fault partway through step (b) would leave deleted_at
	// still set (lock released, error retried next tick) while some objects
	// are already gone — a restore in that window would resurrect a tenant
	// whose backup_chunks/snapshot rows point at partially-missing objects.
	// MarkPurgeStarted is idempotent (0 rows affected on a resumed/retried
	// purge just means a prior attempt already set it — not an error) and
	// RestoreTenant refuses (0 rows -> 409 purge_in_progress) once this is set.
	if _, merr := sqlc.New(w.pool.Pool).MarkPurgeStarted(ctx, tenantID); merr != nil {
		return purgeDone, fmt.Errorf("mark purge started: %w", merr)
	}

	// Step (b): delete object-storage prefixes across all SEVEN tenant-scoped
	// roots (objectStoragePrefixes — see the H1 fast-follow doc comment at the
	// top of this file). This is the ONLY thing that frees these bytes —
	// admin_purge_tenant's DB cascade frees zero storage. Idempotent: Delete
	// on an already-missing key is a no-op success (blobstore.Store.Delete
	// treats 404 as ok), so a retry after a partial/crashed prior attempt just
	// re-lists (fewer or zero keys) and finishes cleanly. Unlike step (a), a
	// failure HERE aborts this tenant's purge for this tick (returned as an
	// error, retried next tick) — we must not hard-delete the tenant row while
	// its storage prefixes are still only partially freed, or tenantID (the
	// only thing that names those prefixes) becomes unrecoverable.
	if w.store != nil {
		for _, prefix := range objectStoragePrefixes(tenantID) {
			keys, lerr := w.store.List(ctx, prefix)
			if lerr != nil {
				return purgeDone, fmt.Errorf("list object prefix %q: %w", prefix, lerr)
			}
			for _, k := range keys {
				if derr := w.store.Delete(ctx, k); derr != nil {
					return purgeDone, fmt.Errorf("delete object %q: %w", k, derr)
				}
			}
		}
	}

	// Step (c): the privileged hard delete. Runs LAST — everything above must
	// have succeeded (or been skippable) before we destroy the row that
	// anchors tenantID's identity and the last chance to resolve its
	// object-storage prefixes.
	if _, perr := sqlc.New(w.pool.Pool).AdminPurgeTenant(ctx, tenantID); perr != nil {
		return purgeDone, fmt.Errorf("admin_purge_tenant: %w", perr)
	}
	w.logger.Info("org purge: tenant purged", slog.String("tenant_id", tenantID.String()))
	return purgeDone, nil
}
