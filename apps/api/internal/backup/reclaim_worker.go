// reclaim_worker.go, GH #402: reclaim the object-storage prefix a deleted site
// leaves behind.
//
// THE BUG. DELETE /sites/{id} was a bare `DELETE FROM sites`.
// backup_snapshots.site_id is ON DELETE CASCADE, so the same statement
// destroyed every snapshot row for that site, and those rows were the only
// database record naming the site's per-snapshot manifest.json objects. Both
// deleters of that object (deleteSnapshotCore and the retention GC's metadata
// prune) need a live snapshot row to build the key, and the GC's site roster is
// itself derived from backup_snapshots, so after the cascade nothing could name
// those objects again. One deleted site with 90 completed snapshots left 90
// orphans, permanently. The site repo now records a site_object_reclaim row in
// the delete's own transaction; this worker is what acts on it.
//
// THE DANGER THIS FILE IS DESIGNED AGAINST. Chunks are content-addressed and
// deduplicated TENANT-WIDE, so a chunk introduced by the deleted site may still
// be the only copy of bytes a different, LIVE site needs to restore. Deleting
// one of those is silent, unrecoverable, and far worse than the storage leak
// being fixed. This worker therefore adds ZERO new chunk-delete authority, and
// it is safe BY CONSTRUCTION rather than by argument:
//
//	manifests:  tenant/<tenantID>/site/<siteID>/backup/<snapshotID>/manifest.json
//	chunks:     chunks/<tenantID>/<blake3>
//
// Those roots are disjoint. A reclaimer scoped to the manifest root is
// STRUCTURALLY INCAPABLE of deleting a chunk however badly it is written, and
// manifestIndexKey is the only writer under that root anywhere in this
// codebase. The proof that a chunk is unreferenced stays exactly where it was,
// in the ADR-050 mark-and-sweep (gc.go), untouched by this file.
//
// THE PREFIX IS DERIVED, NEVER STORED. The task row carries identity
// (tenant_id, site_id, kind); the prefix is rebuilt here from a code constant
// plus two validated UUIDs. A stored prefix would turn a corrupt row into an
// arbitrary-prefix delete instruction, and the adjacency is one character:
// "tenant/" holds backup manifests, "tenants/" holds white-label client report
// PDFs with client PII (internal/org/purge_worker.go documents the same trap).
// The control-plane store is also built with no PathPrefix, so every key sits
// at bucket root and there is no second containment layer underneath this one.
//
// CRASH BEHAVIOUR. Object deletes are idempotent (blobstore.Store.Delete treats
// 404 as success). The worker lists the prefix, deletes each key, and marks the
// task complete ONLY after the whole prefix drained with no error, so a crash
// or storage fault partway leaves the task incomplete and the next tick
// re-lists a shorter set and finishes. Failures back off and record the reason.
// Past the retry cap the task is LEFT VISIBLE, never deleted: a stuck task is
// the only remaining record that those objects exist, so deleting it on give-up
// would recreate the exact bug this file fixes. That is the GH #256 lesson
// applied one layer up, prefer leaving it behind over guessing.
//
// No point-of-no-return marker is needed here, unlike the tenant purge's
// MarkPurgeStarted. That marker exists because a tenant soft-delete is
// restorable and partial object deletion would resurrect a tenant pointing at
// missing objects. Here nothing is restorable: the site row is hard-gone before
// the task ever exists, so there is no restore to interlock with.
//
// THE TASK OUTLIVES ITS TENANT, ON PURPOSE. m113 carries no foreign key to
// tenants either, so a hard-deleted tenant leaves the task row standing and
// TenantGone is a reclaim signal rather than a skip. That is not tidiness lost,
// it is the entire point: admin_delete_empty_tenant (org delete Lane A, and the
// superadmin orphan cleanup) destroys a tenant row with ZERO object-storage
// cleanup, so a cascade here would have deleted the record by exactly the
// operation that should have triggered it. Only Lane B (the grace-window
// org.PurgeWorker) sweeps the seven tenant roots before its hard delete, and
// TenantPendingPurge is what stands off for it.
package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

const (
	// ReclaimQueue is the River queue the periodic reclaim sweep runs on.
	ReclaimQueue = "site_object_reclaim"

	// ReclaimKindBackupManifest names the per-snapshot manifest.json root,
	// tenant/<tenantID>/site/<siteID>/. It is the only kind today. The other
	// site-scoped roots (rucss/<tenant>/<site>/, screenshots/<tenant>/<site>/)
	// can reuse this engine later by adding a kind, not a mechanism.
	//
	// internal/site duplicates this literal deliberately, to keep the site
	// domain free of a dependency on this one; tests/contract asserts the two
	// agree.
	ReclaimKindBackupManifest = "backup_manifest"

	// reclaimMaxAttempts is the give-up cap. A task past it stops being
	// returned by the due query but is never deleted.
	reclaimMaxAttempts = 8

	// reclaimBatchSize bounds the work one tick will take on.
	reclaimBatchSize = 100

	// reclaimBaseBackoff is the first retry delay; it doubles per attempt up to
	// reclaimMaxBackoff.
	reclaimBaseBackoff = 15 * time.Minute
	reclaimMaxBackoff  = 24 * time.Hour
)

// ReclaimKinds is the closed set of kinds this worker can act on, and the
// single place a new one is added.
//
// It exists because the same set is written down twice more, once as the
// site_object_reclaim_kind_check constraint in db/schema.sql and m113, and once
// in m115 for databases that predate that constraint. A kind added here and not
// there cannot be inserted; a kind added there and not here is accepted by the
// database and then refused by the worker. tests/contract compares this slice
// against the constraint text so neither half can move alone.
var ReclaimKinds = []string{ReclaimKindBackupManifest}

// KnownReclaimKind reports whether kind is one this worker can derive a prefix
// for.
func KnownReclaimKind(kind string) bool {
	for _, k := range ReclaimKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// ReclaimArgs is the (empty) periodic job payload.
type ReclaimArgs struct{}

// Kind implements river.JobArgs.
func (ReclaimArgs) Kind() string { return "site_object_reclaim" }

// InsertOpts routes the job to its own queue so a long prefix drain never
// competes for the shared default workers.
func (ReclaimArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: ReclaimQueue}
}

// ObjectReclaimer is the object-storage capability this worker needs: list keys
// under a prefix, and delete a key. *blobstore.Store satisfies it directly, no
// adapter needed. Deliberately narrow: no Put, no presign, and nothing that
// could reach a chunk key.
type ObjectReclaimer interface {
	List(ctx context.Context, prefix string) ([]string, error)
	Delete(ctx context.Context, key string) error
}

// ReclaimTask is one unit of reclamation work: identity only, no prefix.
type ReclaimTask struct {
	ID              uuid.UUID
	TenantID        uuid.UUID
	SiteID          uuid.UUID
	Kind            string
	DestinationKind string
	Attempts        int32
	// LastError is the reason the previous attempt failed. Carried so the
	// stuck-task report can say WHY a task gave up without a second query.
	LastError string
}

// TenantReclaimState is the tenant's lifecycle as this worker must read it.
// Three states, not two: "the tenant row is missing" and "the tenant row is
// soft-deleted" call for OPPOSITE actions, and collapsing them is what made an
// earlier version of this worker strand a task forever.
type TenantReclaimState int

const (
	// TenantLive: the tenant row is present and not scheduled for deletion.
	// Ordinary case, proceed to the site guard.
	TenantLive TenantReclaimState = iota

	// TenantPendingPurge: deleted_at or purge_started_at is set. The org
	// grace-window PurgeWorker owns the whole tenant/<id>/ root for these and
	// holds a SESSION advisory lock in a different namespace, so this worker
	// stands off rather than trying to serialise across two lock namespaces.
	// Nothing is lost: Lane B sweeps every one of these prefixes itself.
	TenantPendingPurge

	// TenantGone: no tenant row at all. Since m113 dropped the tenant foreign
	// key, the task row OUTLIVES its tenant, and this is the state that makes
	// that worth doing. It is reached from admin_delete_empty_tenant (org
	// delete Lane A and the superadmin orphan cleanup), which hard-deletes a
	// tenant with ZERO object-storage cleanup. The task is now the only record
	// naming those objects, so the worker proceeds. Treating this as a skip,
	// which the first version did, means the objects are never reclaimed and
	// GH #402 is merely relocated.
	TenantGone
)

// ReclaimStore is the persistence half of the worker, split out so the guards
// can be tested without a container.
type ReclaimStore interface {
	// ListDue returns open tasks that are due and still under the attempt cap.
	ListDue(ctx context.Context, maxAttempts, limit int32) ([]ReclaimTask, error)
	// ListStuck returns open tasks that have exhausted the attempt cap and so
	// no longer appear in ListDue. They are never deleted; this is how they
	// stay visible.
	ListStuck(ctx context.Context, maxAttempts, limit int32) ([]ReclaimTask, error)
	// SiteExists reports whether the site row is STILL present. Reads the raw
	// sites table, not a filtered helper: see reclaimOne.
	SiteExists(ctx context.Context, tenantID, siteID uuid.UUID) (bool, error)
	// TenantState reports the tenant's lifecycle state, distinguishing a
	// soft-deleted tenant (skip) from an absent one (proceed).
	TenantState(ctx context.Context, tenantID uuid.UUID) (TenantReclaimState, error)
	// Complete closes a task that fully drained.
	Complete(ctx context.Context, id uuid.UUID) error
	// Cancel closes a task WITHOUT any storage deletion, recording why.
	//
	// Reserved for the one outcome that is a PROOF there is nothing to reclaim:
	// the site row still exists. A closed task appears in neither the due query
	// nor the stuck report, so cancelling anything the worker merely could not
	// do would strand those objects silently and permanently. Everything else
	// goes through Fail.
	Cancel(ctx context.Context, id uuid.UUID, reason string) error
	// Fail records an attempt and backs the task off. Never deletes the row.
	Fail(ctx context.Context, id uuid.UUID, backoff time.Duration, reason string) error
}

// ReclaimWorker drives the periodic site-object reclamation sweep.
type ReclaimWorker struct {
	river.WorkerDefaults[ReclaimArgs]
	state  ReclaimStore
	store  ObjectReclaimer // nil disables the storage step (no object storage configured)
	logger *slog.Logger
	batch  int32
	max    int32
}

// NewReclaimWorker builds a ReclaimWorker. state or store may be nil; a nil
// store makes the sweep a no-op, matching self-host installs with no object
// storage at all.
func NewReclaimWorker(state ReclaimStore, store ObjectReclaimer, logger *slog.Logger) *ReclaimWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &ReclaimWorker{
		state:  state,
		store:  store,
		logger: logger,
		batch:  reclaimBatchSize,
		max:    reclaimMaxAttempts,
	}
}

// Timeout bounds one sweep. A batch is at most reclaimBatchSize prefixes, each
// a list plus a delete per key, so 15 minutes is generous headroom.
func (w *ReclaimWorker) Timeout(*river.Job[ReclaimArgs]) time.Duration { return 15 * time.Minute }

// SiteObjectPrefix derives the storage prefix a site owns for the given kind.
//
// It REFUSES a degenerate input rather than returning something that would
// delete more than intended. A nil UUID would otherwise render as
// 00000000-0000-0000-0000-000000000000 and produce a syntactically valid but
// semantically wrong prefix; refusing is the only safe answer, because the
// caller cannot tell the difference and object deletion is irreversible.
func SiteObjectPrefix(kind string, tenantID, siteID uuid.UUID) (string, error) {
	if !KnownReclaimKind(kind) {
		return "", fmt.Errorf("site object reclaim: unknown kind %q, known kinds are %v",
			kind, ReclaimKinds)
	}
	if tenantID == uuid.Nil || siteID == uuid.Nil {
		return "", errors.New("site object reclaim: refusing to derive a prefix from a nil tenant or site id")
	}
	prefix := "tenant/" + tenantID.String() + "/site/" + siteID.String() + "/"
	// Belt and braces on the shape itself. "tenant/" + 36 + "/site/" + 36 + "/".
	if !strings.HasPrefix(prefix, "tenant/") || !strings.HasSuffix(prefix, "/") ||
		len(prefix) != len("tenant/")+36+len("/site/")+36+1 {
		return "", fmt.Errorf("site object reclaim: derived prefix %q failed its shape check", prefix)
	}
	return prefix, nil
}

// ReclaimPrefix is the storage half: derive the prefix, list it, delete every
// key under it. Idempotent and resumable, and it returns on the FIRST delete
// error so the caller records a failure and retries the whole prefix later
// (re-listing a shorter set) rather than reporting a partial drain as done.
func (w *ReclaimWorker) ReclaimPrefix(ctx context.Context, tenantID, siteID uuid.UUID) error {
	return w.reclaimKindPrefix(ctx, ReclaimKindBackupManifest, tenantID, siteID)
}

func (w *ReclaimWorker) reclaimKindPrefix(ctx context.Context, kind string, tenantID, siteID uuid.UUID) error {
	prefix, err := SiteObjectPrefix(kind, tenantID, siteID)
	if err != nil {
		return err
	}
	if w.store == nil {
		return nil
	}
	keys, lerr := w.store.List(ctx, prefix)
	if lerr != nil {
		return fmt.Errorf("list %q: %w", prefix, lerr)
	}
	for _, k := range keys {
		// The store may only ever hand back keys under the prefix it was asked
		// for, but this is the last line before an irreversible delete and it
		// costs nothing.
		if !strings.HasPrefix(k, prefix) {
			return fmt.Errorf("site object reclaim: refusing to delete %q, which is outside %q", k, prefix)
		}
		if derr := w.store.Delete(ctx, k); derr != nil {
			return fmt.Errorf("delete %q: %w", k, derr)
		}
	}
	return nil
}

// Work runs one reclamation sweep. A single task's failure is recorded and does
// NOT abort the sweep; every other due task still gets its chance this tick.
func (w *ReclaimWorker) Work(ctx context.Context, _ *river.Job[ReclaimArgs]) error {
	// With no persistence or no object storage there is nothing this sweep can
	// safely do. It leaves every task OPEN rather than closing them: if object
	// storage is configured later, the work is still on the books. Closing them
	// here would recreate the bug in miniature.
	if w.state == nil || w.store == nil {
		return nil
	}
	tasks, err := w.state.ListDue(ctx, w.max, w.batch)
	if err != nil {
		return fmt.Errorf("site object reclaim: list due: %w", err)
	}
	var done, skipped, cancelled, failed int
	for _, t := range tasks {
		switch w.reclaimOne(ctx, t) {
		case reclaimDone:
			done++
		case reclaimSkipped:
			skipped++
		case reclaimCancelled:
			cancelled++
		default:
			failed++
		}
	}
	if len(tasks) > 0 {
		w.logger.Info("site object reclaim: sweep complete",
			slog.Int("reclaimed", done), slog.Int("skipped", skipped),
			slog.Int("cancelled", cancelled), slog.Int("failed", failed))
	}
	w.reportStuck(ctx)
	return nil
}

// reportStuck re-announces every task that has exhausted the attempt cap.
//
// Those rows are kept on purpose (they are the last record that the objects
// exist), but keeping is not the same as surfacing. Without this, a task that
// gave up produced exactly one Error line at the moment of its eighth failure
// and then went quiet forever, so a permanently stuck prefix looked identical
// to a healthy fleet. Re-reading and re-logging every tick makes the condition
// persist in the logs for as long as it persists in the table, which is what
// makes it alertable. It never mutates anything.
func (w *ReclaimWorker) reportStuck(ctx context.Context) {
	stuck, err := w.state.ListStuck(ctx, w.max, w.batch)
	if err != nil {
		w.logger.Warn("site object reclaim: could not read the stuck-task list", slog.Any("error", err))
		return
	}
	if len(stuck) == 0 {
		return
	}
	w.logger.Error("site object reclaim: tasks past the attempt cap are NOT being retried; "+
		"their objects are still in storage and these rows are the only record of them",
		slog.Int("stuck", len(stuck)), slog.Int("max_attempts", int(w.max)))
	for _, t := range stuck {
		prefix, perr := SiteObjectPrefix(t.Kind, t.TenantID, t.SiteID)
		if perr != nil {
			prefix = "(underivable: " + perr.Error() + ")"
		}
		// These are the lines an operator actually alerts on, so they carry the
		// same supported command the failure reason does, and for the same reason
		// (GH #408 finding 3): a pastable statement here would be one the
		// application role silently discards.
		w.logger.Error("site object reclaim: stuck task",
			slog.String("task_id", t.ID.String()),
			slog.String("tenant_id", t.TenantID.String()),
			slog.String("site_id", t.SiteID.String()),
			slog.String("prefix", prefix),
			slog.Int("attempts", int(t.Attempts)),
			slog.String("last_error", t.LastError),
			slog.String("recovery", "wpmgr-cli reclaim retry --task "+t.ID.String()))
	}
}

// ReclaimRetryHint is the supported way to reopen a stuck site task, and it is
// deliberately NOT a SQL statement (GH #408 finding 3).
//
// It is a function rather than a literal at each call site so the three places
// an operator meets this advice (the guard-1 failure reason, the every-tick
// stuck report, and the tenant worker's equivalent) cannot drift apart.
func ReclaimRetryHint(taskID uuid.UUID) string {
	return " (the task is kept and retried, never cancelled: its objects are still in storage and " +
		"this row is the only record of them. Correct it with: wpmgr-cli reclaim retry --task " +
		taskID.String() + " which runs as the ordinary application role and exits non-zero if it " +
		"changes nothing)"
}

type reclaimOutcome int

const (
	reclaimDone reclaimOutcome = iota
	reclaimSkipped
	reclaimCancelled
	reclaimFailed
)

// reclaimOne handles exactly one task, guards first.
func (w *ReclaimWorker) reclaimOne(ctx context.Context, t ReclaimTask) reclaimOutcome {
	// GUARD 1: the derived prefix must be well formed. Checked before any DB
	// round-trip so a corrupt row can never even reach the storage calls.
	//
	// This FAILS the task, it does not cancel it, and the difference is the
	// whole point. Cancel closes the row, which takes it out of the due query
	// AND out of the stuck report, so nothing ever mentions it again. That is
	// only correct when the worker has PROVED there is nothing to reclaim, which
	// is guard 3's case and guard 3's case alone: the site row still exists.
	//
	// An underivable prefix is not proof of anything. It is the worker not
	// knowing where to look, and the objects it cannot name are still sitting in
	// the bucket. Cancelling here strands them permanently, which is GH #402
	// delivered by the remedy for it. The likeliest way to get here is the
	// operator backfill the m113 header documents: a mistyped kind in a
	// hand-written INSERT. The database now refuses that insert outright
	// (site_object_reclaim_kind_check, m113 and m115), so this path is the
	// residue, rows written before that constraint existed. Failing leaves the
	// row open, backs it off, and after the attempt cap hands it to the
	// every-tick stuck report with this reason attached, where an operator can
	// see the bad value and correct it.
	//
	// This used to print a bare UPDATE with an "ON A SUPERUSER CONNECTION"
	// caveat. GH #408 finding 3: run verbatim as the wpmgr_app role with no GUC
	// that statement is err=nil, rows=0, and the row is byte-for-byte unchanged,
	// because the RLS USING clause hides it and Postgres has nothing to complain
	// about. The defect was never a missing warning (the caveat was right there
	// in the string) but a remedy that cannot work on the connection the operator
	// has, so a longer caveat would have been a different fix from the right one.
	// The message now names a command that runs as the ordinary application role
	// and exits non-zero if it changes nothing.
	// TestGH408_WorkerPrintsNoRunnableSQL fails the moment a pastable statement
	// goes back in.
	prefix, perr := SiteObjectPrefix(t.Kind, t.TenantID, t.SiteID)
	if perr != nil {
		w.fail(ctx, t, perr.Error()+ReclaimRetryHint(t.ID))
		return reclaimFailed
	}

	// GUARD 2: stand off a tenant that Lane B already owns, and ONLY that one.
	//
	// TenantPendingPurge means the org grace-window purge worker is going to
	// delete the whole tenant/<id>/ root itself, holding a session advisory lock
	// in a DIFFERENT namespace; skipping avoids a concurrent-delete race rather
	// than trying to serialise across two lock namespaces. This must come BEFORE
	// guard 3, because a soft-deleted tenant's other sites are still present.
	//
	// TenantGone is NOT a skip. admin_delete_empty_tenant (org delete Lane A and
	// the superadmin orphan cleanup) hard-deletes a tenant row and sweeps no
	// storage at all, so a task whose tenant has vanished is the last record
	// naming those objects. Skipping it would leave them orphaned forever, which
	// is the same defect this worker exists to fix, only at tenant level.
	tstate, terr := w.state.TenantState(ctx, t.TenantID)
	if terr != nil {
		w.fail(ctx, t, fmt.Sprintf("tenant state lookup failed: %v", terr))
		return reclaimFailed
	}
	if tstate == TenantPendingPurge {
		return reclaimSkipped
	}

	// GUARD 3: re-verify that the site is GENUINELY gone.
	//
	// This is the restored-dump and staging-pointed-at-production valve. The
	// control-plane store is constructed with no PathPrefix, so every key sits
	// at bucket root: a control plane whose database is older than the bucket it
	// is pointed at would otherwise treat LIVE manifests as orphans.
	//
	// It reads the raw sites table, deliberately NOT ListAllSiteIDs, which
	// filters connection_state <> 'archived'. An archived site is LIVE and
	// restorable, and using the obvious-looking helper would delete its backups'
	// manifests. TestGH402_ReclaimWorker_ArchivedSiteIsNotOrphaned exists to
	// fail if anyone reaches for it.
	exists, serr := w.state.SiteExists(ctx, t.TenantID, t.SiteID)
	if serr != nil {
		w.fail(ctx, t, fmt.Sprintf("site existence check failed: %v", serr))
		return reclaimFailed
	}
	if exists {
		reason := "site row still exists; refusing to reclaim a live site's objects"
		w.logger.Warn("site object reclaim: "+reason,
			slog.String("tenant_id", t.TenantID.String()),
			slog.String("site_id", t.SiteID.String()),
			slog.String("prefix", prefix))
		if cerr := w.state.Cancel(ctx, t.ID, reason); cerr != nil {
			return reclaimFailed
		}
		return reclaimCancelled
	}

	// Storage. Control-plane store only, and that is enough for this job: the
	// manifest index object is written to the control-plane bucket for EVERY
	// site whatever its backup destination is (main.go wires SetIndexPutter to
	// that store), so no deleted site's manifests fall outside this sweep. What
	// a customer-owned destination holds is the backup payload, which this
	// worker has no credentials for and never reaches into, which is why
	// destination_kind rides on the task and is logged.
	if rerr := w.reclaimKindPrefix(ctx, t.Kind, t.TenantID, t.SiteID); rerr != nil {
		w.fail(ctx, t, rerr.Error())
		return reclaimFailed
	}

	if cerr := w.state.Complete(ctx, t.ID); cerr != nil {
		w.logger.Error("site object reclaim: prefix drained but the task could not be closed (it will re-run, which is harmless)",
			slog.String("task_id", t.ID.String()), slog.Any("error", cerr))
		return reclaimFailed
	}
	w.logger.Info("site object reclaim: prefix reclaimed",
		slog.String("tenant_id", t.TenantID.String()),
		slog.String("site_id", t.SiteID.String()),
		slog.String("prefix", prefix),
		slog.String("destination_kind", t.DestinationKind))
	return reclaimDone
}

func (w *ReclaimWorker) fail(ctx context.Context, t ReclaimTask, reason string) {
	backoff := reclaimBackoff(t.Attempts)
	if ferr := w.state.Fail(ctx, t.ID, backoff, reason); ferr != nil {
		w.logger.Error("site object reclaim: could not record a failed attempt",
			slog.String("task_id", t.ID.String()), slog.Any("error", ferr))
	}
	next := t.Attempts + 1
	logAt := w.logger.Warn
	if next >= w.max {
		// The task is now past the cap. It stays in the table on purpose: it is
		// the last record that those objects exist.
		logAt = w.logger.Error
	}
	logAt("site object reclaim: attempt failed",
		slog.String("task_id", t.ID.String()),
		slog.String("tenant_id", t.TenantID.String()),
		slog.String("site_id", t.SiteID.String()),
		slog.Int("attempts", int(next)),
		slog.Int("max_attempts", int(w.max)),
		slog.String("reason", reason))
}

// reclaimBackoff doubles from reclaimBaseBackoff up to reclaimMaxBackoff.
func reclaimBackoff(attempts int32) time.Duration {
	d := reclaimBaseBackoff
	for i := int32(0); i < attempts && d < reclaimMaxBackoff; i++ {
		d *= 2
	}
	if d > reclaimMaxBackoff {
		d = reclaimMaxBackoff
	}
	return d
}

// ---------------------------------------------------------------------------
// Postgres ReclaimStore
// ---------------------------------------------------------------------------

type pgReclaimStore struct{ pool *db.Pool }

// NewReclaimStore builds the Postgres-backed ReclaimStore. Every method runs
// under InAgentTx: the sweep is cross-tenant by nature, and site_object_reclaim
// carries the matching _agent policy (m113). sites is read through its existing
// sites_agent policy; tenants has no RLS.
func NewReclaimStore(pool *db.Pool) ReclaimStore { return &pgReclaimStore{pool: pool} }

func (s *pgReclaimStore) ListDue(ctx context.Context, maxAttempts, limit int32) ([]ReclaimTask, error) {
	var out []ReclaimTask
	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListDueSiteObjectReclaims(ctx, sqlc.ListDueSiteObjectReclaimsParams{
			MaxAttempts: maxAttempts,
			RowLimit:    limit,
		})
		if err != nil {
			return err
		}
		out = reclaimTasksFromRows(rows)
		return nil
	})
	return out, err
}

func (s *pgReclaimStore) ListStuck(ctx context.Context, maxAttempts, limit int32) ([]ReclaimTask, error) {
	var out []ReclaimTask
	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListStuckSiteObjectReclaims(ctx, sqlc.ListStuckSiteObjectReclaimsParams{
			MaxAttempts: maxAttempts,
			RowLimit:    limit,
		})
		if err != nil {
			return err
		}
		out = reclaimTasksFromRows(rows)
		return nil
	})
	return out, err
}

func reclaimTasksFromRows(rows []sqlc.SiteObjectReclaim) []ReclaimTask {
	out := make([]ReclaimTask, 0, len(rows))
	for _, r := range rows {
		t := ReclaimTask{
			ID: r.ID, TenantID: r.TenantID, SiteID: r.SiteID,
			Kind: r.Kind, Attempts: r.Attempts,
		}
		if r.DestinationKind != nil {
			t.DestinationKind = *r.DestinationKind
		}
		if r.LastError != nil {
			t.LastError = *r.LastError
		}
		out = append(out, t)
	}
	return out
}

func (s *pgReclaimStore) SiteExists(ctx context.Context, tenantID, siteID uuid.UUID) (bool, error) {
	var exists bool
	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		got, err := sqlc.New(tx).SiteExistsForReclaim(ctx, sqlc.SiteExistsForReclaimParams{
			SiteID: siteID, TenantID: tenantID,
		})
		if err != nil {
			return err
		}
		exists = got
		return nil
	})
	return exists, err
}

func (s *pgReclaimStore) TenantState(ctx context.Context, tenantID uuid.UUID) (TenantReclaimState, error) {
	state := TenantLive
	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetTenantDeletionStateForReclaim(ctx, tenantID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// No tenant row. The task did NOT cascade away with it (m113
				// carries no tenant foreign key, deliberately), so this is the
				// state in which the task is the only surviving name for those
				// objects. Reclaim, do not skip.
				state = TenantGone
				return nil
			}
			return err
		}
		if row.DeletedAt.Valid || row.PurgeStartedAt.Valid {
			state = TenantPendingPurge
		}
		return nil
	})
	return state, err
}

func (s *pgReclaimStore) Complete(ctx context.Context, id uuid.UUID) error {
	return s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		_, err := sqlc.New(tx).CompleteSiteObjectReclaim(ctx, id)
		return err
	})
}

func (s *pgReclaimStore) Cancel(ctx context.Context, id uuid.UUID, reason string) error {
	return s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		_, err := sqlc.New(tx).CancelSiteObjectReclaim(ctx, sqlc.CancelSiteObjectReclaimParams{
			ID: id, LastError: &reason,
		})
		return err
	})
}

func (s *pgReclaimStore) Fail(ctx context.Context, id uuid.UUID, backoff time.Duration, reason string) error {
	return s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		_, err := sqlc.New(tx).FailSiteObjectReclaim(ctx, sqlc.FailSiteObjectReclaimParams{
			ID:        id,
			Backoff:   pgtype.Interval{Microseconds: backoff.Microseconds(), Valid: true},
			LastError: &reason,
		})
		return err
	})
}
