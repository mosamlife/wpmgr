// tenant_reclaim_worker.go, GH #408: free the object storage a HARD-deleted
// tenant leaves behind.
//
// THE BUG. backup_chunks.tenant_id is ON DELETE CASCADE (m4).
// admin_delete_empty_tenant hard-deletes the tenants row and frees ZERO object
// storage, so the same statement destroys the entire chunk inventory for that
// tenant. Afterwards chunks/<tenantID>/ holds objects nothing names: the
// retention collector's roster is derived from backup_chunks and
// backup_snapshots, and an operator has no id to work from. Delete an
// organisation's last site and then the emptied organisation, and its chunk
// ciphertext is stranded permanently. This is GH #402 one level up and one table
// across, and m113 said so while deliberately leaving it open.
// admin_delete_empty_tenant now records a tenant_object_reclaim row in the
// delete's own transaction (m116); this worker is what acts on it.
//
// WHY THIS IS A SEPARATE WORKER AND NOT A NEW KIND ON THE m113 ENGINE.
//
// m113's central safety claim is mechanical, not behavioural: its reclaimer is
// scoped to the manifest root, chunks live under a disjoint root, and therefore
// it is "STRUCTURALLY INCAPABLE of deleting a chunk however badly it is
// written". That sentence is worth more than the code it describes. Adding chunk
// authority to that engine would falsify it for the WHOLE engine, including for
// the manifest path that legitimately relies on it. So the only chunk-delete
// authority added by GH #408 lives in this one file, and this header says so.
// (Mechanically the two also do not fit: site_object_reclaim.site_id is NOT NULL
// and its kind set is closed by a constraint m115 exists to enforce.)
//
// THE DANGER THIS FILE IS DESIGNED AGAINST, and the proof.
//
// Chunks are content-addressed and deduplicated TENANT-WIDE, so the bytes a
// deleted tenant stored may be byte-identical to bytes a LIVE site in a
// DIFFERENT organisation still needs. Deleting one of those is silent and
// unrecoverable, and far worse than the leak being fixed. The ADR-050
// mark-and-sweep is the usual proof that a chunk is unreferenced, and it is NOT
// available here: run inline it self-deadlocks (its advisory lock is
// session-scoped so it takes its own connection, and its per-chunk
// SELECT ... FOR UPDATE then blocks on rows the caller's own uncommitted cascade
// has already deleted), and run afterwards it is inert, because every row it
// reasons over cascaded away with the tenant.
//
// The substitute is a different argument, and a stronger one:
//
//  1. Every chunk object of tenant T is at chunks/<T>/<blake3> and nothing else
//     is (chunkS3Key, model.go).
//  2. Dedup NEVER crosses a tenant: the oracle is
//     WHERE tenant_id = $1 AND blake3 = ANY($2) and the unique index is
//     (tenant_id, blake3). Identical content in two tenants is TWO objects.
//  3. Only a site restores, and a site cannot outlive its tenant
//     (sites.tenant_id ON DELETE CASCADE) nor be created against an absent one.
//  4. Lane A fires only on zero memberships AND zero sites, re-checked inside
//     the SECURITY DEFINER function in the same transaction.
//  5. Therefore when the record is written, no live site in ANY tenant can
//     reference an object under chunks/<T>/, and none ever can again. Lane B
//     already deletes chunks/<id>/ wholesale on exactly this basis.
//  6. The record is written in the delete's own transaction, so it exists if and
//     only if the tenant row is really gone.
//  7. And the guards below re-establish 3 to 5 INDEPENDENTLY at drain time, the
//     analogue of m113's GUARD 3 and the collector's ground-truth re-check.
//
// Note which direction of staleness is dangerous, because it is this design's
// best property: a database OLDER than the bucket holds no record and does
// nothing. Only a database that genuinely performed the delete holds an
// instruction. That is strictly safer than a bucket scan, which inverts the
// direction of trust and makes a full-root LIST the input to an irreversible
// delete.
//
// TestGH408_ChunkSharedWithALiveSiteInAnotherTenantSurvivesTheDrain is the
// executable form of that proof and fails loudly if anyone ever keys this drain
// on a content hash instead of the tenant-namespaced prefix.
//
// THE ROOTS ARE SHARED, NOT COPIED. org.ObjectStoragePrefixes is the single
// oracle for what a tenant owns, so the Lane A drain and the Lane B purge can
// never disagree about the root set. A second list would drift, and it would
// drift towards a silent leak.
//
// CRASH BEHAVIOUR AND RESUME. Per tick, per task: re-list every root fresh,
// delete up to a bounded key budget, then stop. The task is marked complete ONLY
// when a FRESH re-list of ALL roots returns zero keys. A crash, a storage fault,
// a pod eviction or a hit timeout therefore leaves the task OPEN with its
// objects partly gone, and the next tick re-lists a shorter set and continues.
// Deletes are idempotent (blobstore.Store.Delete treats 404 as success), so
// re-running a drained prefix is free. Objects go FIRST and the row closes
// SECOND, always, so a crash between them leaves a re-runnable task rather than
// a completed task with objects still in the bucket. The explicit key budget is
// the one place this deviates from m113's "list, delete all, complete": that is
// fine for one site's manifests, but a 100 GB tenant is roughly 25,600 serial
// presigned deletes (4 MiB chunk targets, no batch delete, no concurrency),
// about 21 minutes at 50 ms per round trip, which sits right on this worker's
// timeout. A budget makes resumption deterministic and testable instead of
// dependent on where a timeout lands. A batch that drained partially and was
// marked complete would recreate GH #402 exactly.
//
// A PARTIAL DRAIN CANNOT LOSE DATA, only leak, and that is a genuinely stronger
// position than the site case. The tenant row is hard gone, it had zero sites,
// and no live site in any tenant can reference chunks/<T>/. A half-drained
// tenant costs money, not somebody's backups.
//
// GIVE-UP KEEPS THE ROW. Past the attempt cap the task is LEFT VISIBLE, never
// deleted, and re-logged at Error every tick. Deleting it would destroy the last
// record naming those objects, which is GH #402 delivered by the remedy for it,
// and it is the GH #256 lesson (prefer leaving it behind over guessing) one
// layer up. There is no Cancel on this table at all: unlike the site case, no
// outcome here PROVES there is nothing to reclaim.
//
// LANE B STAND-OFF. A tenant_object_reclaim row only ever comes from a Lane A
// HARD delete, so it can never name a tenant the grace-window purge is working
// on. Where the two do overlap the overlap is harmless: a tenant task draining
// tenant/<T>/ covers a site task's tenant/<T>/site/<S>/ subtree, and because
// deletes are idempotent whichever runs second lists a shorter set and finishes.
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
	"github.com/mosamlife/wpmgr/apps/api/internal/org"
)

const (
	// TenantReclaimQueue is the River queue the tenant drain runs on, separate
	// from the site reclaim queue so a long tenant drain never starves it.
	TenantReclaimQueue = "tenant_object_reclaim"

	// TenantReclaimKindStorage names every tenant-scoped storage root
	// org.ObjectStoragePrefixes returns. It is the only kind, and the set is
	// CLOSED by tenant_object_reclaim_kind_check (m116).
	TenantReclaimKindStorage = "tenant_storage"

	// tenantReclaimMaxAttempts is the give-up cap. A task past it stops being
	// returned by the due query but is NEVER deleted.
	tenantReclaimMaxAttempts = 8

	// tenantReclaimBatchSize bounds how many TASKS one tick takes on.
	tenantReclaimBatchSize = 20

	// tenantReclaimKeyBudget bounds how many OBJECTS one tick deletes for one
	// task. See the header: this is what makes resumption deterministic rather
	// than a function of where the timeout lands.
	tenantReclaimKeyBudget = 5000

	tenantReclaimBaseBackoff = 15 * time.Minute
	tenantReclaimMaxBackoff  = 24 * time.Hour
)

// TenantReclaimKinds is the closed set of kinds this worker acts on, and the
// code-side copy of the tenant_object_reclaim_kind_check constraint.
// tests/contract compares the two so neither half can move alone.
var TenantReclaimKinds = []string{TenantReclaimKindStorage}

// KnownTenantReclaimKind reports whether kind is one this worker can act on.
func KnownTenantReclaimKind(kind string) bool {
	for _, k := range TenantReclaimKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// TenantReclaimArgs is the (empty) periodic job payload.
type TenantReclaimArgs struct{}

// Kind implements river.JobArgs.
func (TenantReclaimArgs) Kind() string { return "tenant_object_reclaim" }

// InsertOpts routes the job to its own queue.
func (TenantReclaimArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: TenantReclaimQueue}
}

// TenantReclaimTask is one unit of work: identity and triage, never a prefix.
type TenantReclaimTask struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	Kind      string
	Attempts  int32
	LastError string
	// CreatedAt is when the work was recorded. `wpmgr-cli reclaim list` prints
	// its age, which is what separates a task working through its backoff from
	// one that has been stranded since a delete months ago.
	CreatedAt time.Time
}

// TenantReclaimStore is the persistence half, split out so every guard can be
// tested without a container.
type TenantReclaimStore interface {
	// ListDue returns open tasks that are due and still under the attempt cap.
	ListDue(ctx context.Context, maxAttempts, limit int32) ([]TenantReclaimTask, error)
	// ListStuck returns open tasks past the cap. They are never deleted; this is
	// how they stay visible.
	ListStuck(ctx context.Context, maxAttempts, limit int32) ([]TenantReclaimTask, error)
	// TenantExists reports whether a tenants row with this id is present again.
	TenantExists(ctx context.Context, tenantID uuid.UUID) (bool, error)
	// SitesExist reports whether any sites row still names this tenant.
	SitesExist(ctx context.Context, tenantID uuid.UUID) (bool, error)
	// ChunkRowsExist reports whether any backup_chunks row still names it.
	ChunkRowsExist(ctx context.Context, tenantID uuid.UUID) (bool, error)
	// Complete closes a task whose every root re-listed empty.
	Complete(ctx context.Context, id uuid.UUID) error
	// Fail records an attempt and backs the task off. Never deletes the row.
	Fail(ctx context.Context, id uuid.UUID, backoff time.Duration, reason string) error
}

// TenantReclaimWorker drives the periodic tenant-storage drain.
type TenantReclaimWorker struct {
	river.WorkerDefaults[TenantReclaimArgs]
	state  TenantReclaimStore
	store  ObjectReclaimer // nil disables the storage step (no object storage configured)
	logger *slog.Logger
	batch  int32
	max    int32
	budget int
}

// NewTenantReclaimWorker builds a TenantReclaimWorker. state or store may be
// nil; a nil store makes the sweep a no-op, matching a self-host install with no
// object storage at all.
func NewTenantReclaimWorker(state TenantReclaimStore, store ObjectReclaimer, logger *slog.Logger) *TenantReclaimWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &TenantReclaimWorker{
		state:  state,
		store:  store,
		logger: logger,
		batch:  tenantReclaimBatchSize,
		max:    tenantReclaimMaxAttempts,
		budget: tenantReclaimKeyBudget,
	}
}

// Timeout bounds one sweep. The per-task key budget is the real bound on work;
// this is the backstop.
func (w *TenantReclaimWorker) Timeout(*river.Job[TenantReclaimArgs]) time.Duration {
	return 15 * time.Minute
}

// TenantObjectPrefixes derives every storage root a tenant owns.
//
// It REFUSES a degenerate input rather than returning something that would
// delete more than intended: a nil UUID renders as
// 00000000-0000-0000-0000-000000000000 and produces syntactically valid but
// semantically wrong prefixes, and object deletion is irreversible. The roots
// themselves come from org.ObjectStoragePrefixes, shared with the Lane B purge.
func TenantObjectPrefixes(kind string, tenantID uuid.UUID) ([]string, error) {
	if !KnownTenantReclaimKind(kind) {
		return nil, fmt.Errorf("tenant object reclaim: unknown kind %q, known kinds are %v",
			kind, TenantReclaimKinds)
	}
	if tenantID == uuid.Nil {
		return nil, errors.New("tenant object reclaim: refusing to derive prefixes from a nil tenant id")
	}
	roots := org.ObjectStoragePrefixes(tenantID)
	id := tenantID.String()
	for _, r := range roots {
		// Belt and braces on the shape itself, one line before an irreversible
		// delete. Every root must END with the tenant's own id and a slash, which
		// is what makes it impossible for one to be an unscoped bucket root.
		if !strings.HasSuffix(r, "/"+id+"/") {
			return nil, fmt.Errorf("tenant object reclaim: derived root %q is not scoped to tenant %s", r, id)
		}
	}
	if len(roots) == 0 {
		return nil, errors.New("tenant object reclaim: refusing to run with an empty root set")
	}
	return roots, nil
}

// Work runs one drain sweep. A single task's failure is recorded and does NOT
// abort the sweep.
func (w *TenantReclaimWorker) Work(ctx context.Context, _ *river.Job[TenantReclaimArgs]) error {
	// With no persistence or no object storage there is nothing this sweep can
	// safely do. It leaves every task OPEN rather than closing them: if object
	// storage is configured later, the work is still on the books.
	if w.state == nil || w.store == nil {
		return nil
	}
	tasks, err := w.state.ListDue(ctx, w.max, w.batch)
	if err != nil {
		return fmt.Errorf("tenant object reclaim: list due: %w", err)
	}
	var done, partial, refused, failed int
	for _, t := range tasks {
		switch w.reclaimOne(ctx, t) {
		case tenantReclaimDone:
			done++
		case tenantReclaimPartial:
			partial++
		case tenantReclaimRefused:
			refused++
		default:
			failed++
		}
	}
	if len(tasks) > 0 {
		w.logger.Info("tenant object reclaim: sweep complete",
			slog.Int("reclaimed", done), slog.Int("partial", partial),
			slog.Int("refused", refused), slog.Int("failed", failed))
	}
	w.reportStuck(ctx)
	return nil
}

// reportStuck re-announces every task past the attempt cap, every tick. Those
// rows are kept on purpose, but keeping is not the same as surfacing: without
// this a task that gave up produced one Error line at the moment of its last
// failure and then went quiet forever. It never mutates anything.
func (w *TenantReclaimWorker) reportStuck(ctx context.Context) {
	stuck, err := w.state.ListStuck(ctx, w.max, w.batch)
	if err != nil {
		w.logger.Warn("tenant object reclaim: could not read the stuck-task list", slog.Any("error", err))
		return
	}
	if len(stuck) == 0 {
		return
	}
	w.logger.Error("tenant object reclaim: tasks past the attempt cap are NOT being retried; "+
		"their objects are still in storage and these rows are the only record of them",
		slog.Int("stuck", len(stuck)), slog.Int("max_attempts", int(w.max)))
	for _, t := range stuck {
		roots, perr := TenantObjectPrefixes(t.Kind, t.TenantID)
		rootList := strings.Join(roots, " ")
		if perr != nil {
			rootList = "(underivable: " + perr.Error() + ")"
		}
		w.logger.Error("tenant object reclaim: stuck task",
			slog.String("task_id", t.ID.String()),
			slog.String("tenant_id", t.TenantID.String()),
			slog.String("roots", rootList),
			slog.Int("attempts", int(t.Attempts)),
			slog.String("last_error", t.LastError),
			slog.String("recovery", TenantReclaimRetryCommand(t.ID)))
	}
}

// TenantReclaimRetryCommand is the supported way to reopen a stuck TENANT task,
// and it is deliberately NOT a SQL statement (GH #408 finding 3).
//
// The site reclaim worker used to print a bare UPDATE here. Run verbatim as the
// application role with no GUC that statement is err=nil, rows=0, and the row is
// byte-for-byte unchanged, because the RLS USING clause hides it and Postgres
// has nothing to complain about. That is not a missing warning, it is a remedy
// that cannot work on the connection the operator has, so a longer caveat would
// have been a different fix from the right one. This command runs through the
// ordinary application role and exits non-zero if it changes nothing.
//
// It names THIS table's kind, not the site engine's: `reclaim retry` resolves an
// id in the tenant table first, and --kind tenant_storage keeps it there rather
// than falling through to the site table. The string itself comes from
// reclaimRetryCommand (reclaim_ops.go), the single constructor both engines use.
func TenantReclaimRetryCommand(taskID uuid.UUID) string {
	return reclaimRetryCommand(taskID, TenantReclaimKindStorage)
}

// TenantReclaimRetryHint is TenantReclaimRetryCommand wrapped in the advice
// sentence a failed tenant task carries in last_error.
func TenantReclaimRetryHint(taskID uuid.UUID) string {
	return reclaimRetryAdvice(taskID, TenantReclaimKindStorage)
}

type tenantReclaimOutcome int

const (
	tenantReclaimDone tenantReclaimOutcome = iota
	tenantReclaimPartial
	tenantReclaimRefused
	tenantReclaimFailed
)

// reclaimOne handles exactly one task: guards first, then a bounded drain.
//
// Every guard REFUSES by failing the task, never by closing it. There is no
// outcome here that proves there is nothing to reclaim, so closing a task the
// worker merely could not do would strand those objects silently and
// permanently, and nothing else in the database names them.
func (w *TenantReclaimWorker) reclaimOne(ctx context.Context, t TenantReclaimTask) tenantReclaimOutcome {
	// GUARD 1: the roots must be derivable and every one of them scoped to this
	// tenant. Checked before any DB round trip, so a corrupt row cannot reach a
	// storage call at all.
	roots, perr := TenantObjectPrefixes(t.Kind, t.TenantID)
	if perr != nil {
		w.fail(ctx, t, perr.Error()+TenantReclaimRetryHint(t.ID))
		return tenantReclaimFailed
	}

	// GUARD 2: the tenant row must still be ABSENT.
	//
	// This is the restored-dump and staging-pointed-at-production valve, and the
	// tenant-level analogue of m113's GUARD 3. The control-plane store is
	// constructed with no PathPrefix, so every key sits at bucket root: a control
	// plane whose database is older than the bucket it is pointed at would
	// otherwise treat a LIVE organisation's storage as an orphan. Refusing leaves
	// the task open, so the objects are not forgotten either.
	exists, terr := w.state.TenantExists(ctx, t.TenantID)
	if terr != nil {
		w.fail(ctx, t, fmt.Sprintf("tenant existence check failed: %v", terr))
		return tenantReclaimFailed
	}
	if exists {
		reason := "a tenants row with this id EXISTS again; refusing to delete a live organisation's storage"
		w.logger.Warn("tenant object reclaim: "+reason, slog.String("tenant_id", t.TenantID.String()))
		w.fail(ctx, t, reason)
		return tenantReclaimRefused
	}

	// GUARD 3: no site may name this tenant. Impossible via the cascade, cheap
	// belt and braces against a partial dump restore, and asserted separately so
	// a future refactor cannot silently drop it.
	sites, serr := w.state.SitesExist(ctx, t.TenantID)
	if serr != nil {
		w.fail(ctx, t, fmt.Sprintf("site existence check failed: %v", serr))
		return tenantReclaimFailed
	}
	if sites {
		reason := "sites rows still name this tenant; refusing to reclaim storage a live site may restore from"
		w.logger.Warn("tenant object reclaim: "+reason, slog.String("tenant_id", t.TenantID.String()))
		w.fail(ctx, t, reason)
		return tenantReclaimRefused
	}

	// GUARD 4: no chunk row may name this tenant. If the inventory is back, the
	// mark-and-sweep owns those objects again and this drain must not.
	chunks, cerr := w.state.ChunkRowsExist(ctx, t.TenantID)
	if cerr != nil {
		w.fail(ctx, t, fmt.Sprintf("chunk inventory check failed: %v", cerr))
		return tenantReclaimFailed
	}
	if chunks {
		reason := "backup_chunks rows still name this tenant; the retention collector owns those objects, not this drain"
		w.logger.Warn("tenant object reclaim: "+reason, slog.String("tenant_id", t.TenantID.String()))
		w.fail(ctx, t, reason)
		return tenantReclaimRefused
	}

	deleted, drained, derr := w.drain(ctx, roots)
	if derr != nil {
		w.fail(ctx, t, derr.Error())
		return tenantReclaimFailed
	}
	if !drained {
		// Objects went, the row stays open, and the next tick re-lists a shorter
		// set. This is the resume path and it is deliberately not an error.
		w.logger.Info("tenant object reclaim: partial drain, task stays open for the next tick",
			slog.String("tenant_id", t.TenantID.String()),
			slog.Int("deleted", deleted), slog.Int("budget", w.budget))
		return tenantReclaimPartial
	}

	// Objects FIRST, row SECOND. A crash between them leaves a re-runnable task,
	// never a completed task with objects still in the bucket.
	if cerr := w.state.Complete(ctx, t.ID); cerr != nil {
		w.logger.Error("tenant object reclaim: storage drained but the task could not be closed (it will re-run, which is harmless)",
			slog.String("task_id", t.ID.String()), slog.Any("error", cerr))
		return tenantReclaimFailed
	}
	w.logger.Info("tenant object reclaim: tenant storage reclaimed",
		slog.String("tenant_id", t.TenantID.String()),
		slog.Int("deleted", deleted), slog.Int("roots", len(roots)))
	return tenantReclaimDone
}

// drain deletes up to the per-tick key budget across roots, then reports whether
// a FRESH re-list of every root came back empty.
//
// The re-list is the completion proof and is not an optimisation to skip: a
// batch that drained partially and was marked complete would recreate GH #402
// exactly. It returns on the FIRST delete error so the caller records a failure
// and retries later against a shorter list.
func (w *TenantReclaimWorker) drain(ctx context.Context, roots []string) (int, bool, error) {
	if w.store == nil {
		return 0, false, nil
	}
	budget := w.budget
	deleted := 0
	for _, root := range roots {
		keys, lerr := w.store.List(ctx, root)
		if lerr != nil {
			return deleted, false, fmt.Errorf("list %q: %w", root, lerr)
		}
		for _, k := range keys {
			if budget <= 0 {
				return deleted, false, nil
			}
			// The store may only ever hand back keys under the prefix it was
			// asked for, but this is the last line before an irreversible delete
			// and it costs nothing.
			if !strings.HasPrefix(k, root) {
				return deleted, false, fmt.Errorf(
					"tenant object reclaim: refusing to delete %q, which is outside %q", k, root)
			}
			if derr := w.store.Delete(ctx, k); derr != nil {
				return deleted, false, fmt.Errorf("delete %q: %w", k, derr)
			}
			deleted++
			budget--
		}
	}
	// Fresh re-list of EVERY root. Only an empty answer closes the task.
	for _, root := range roots {
		keys, lerr := w.store.List(ctx, root)
		if lerr != nil {
			return deleted, false, fmt.Errorf("re-list %q: %w", root, lerr)
		}
		if len(keys) > 0 {
			return deleted, false, nil
		}
	}
	return deleted, true, nil
}

func (w *TenantReclaimWorker) fail(ctx context.Context, t TenantReclaimTask, reason string) {
	backoff := tenantReclaimBackoff(t.Attempts)
	if ferr := w.state.Fail(ctx, t.ID, backoff, reason); ferr != nil {
		w.logger.Error("tenant object reclaim: could not record a failed attempt",
			slog.String("task_id", t.ID.String()), slog.Any("error", ferr))
	}
	next := t.Attempts + 1
	logAt := w.logger.Warn
	if next >= w.max {
		// The task is now past the cap. It stays in the table on purpose: it is
		// the last record that those objects exist.
		logAt = w.logger.Error
	}
	logAt("tenant object reclaim: attempt failed",
		slog.String("task_id", t.ID.String()),
		slog.String("tenant_id", t.TenantID.String()),
		slog.Int("attempts", int(next)),
		slog.Int("max_attempts", int(w.max)),
		slog.String("reason", reason),
		slog.String("recovery", TenantReclaimRetryCommand(t.ID)))
}

// tenantReclaimBackoff doubles from the base up to the cap.
func tenantReclaimBackoff(attempts int32) time.Duration {
	d := tenantReclaimBaseBackoff
	for i := int32(0); i < attempts && d < tenantReclaimMaxBackoff; i++ {
		d *= 2
	}
	if d > tenantReclaimMaxBackoff {
		d = tenantReclaimMaxBackoff
	}
	return d
}

// ---------------------------------------------------------------------------
// Postgres TenantReclaimStore
// ---------------------------------------------------------------------------

type pgTenantReclaimStore struct{ pool *db.Pool }

// NewTenantReclaimStore builds the Postgres-backed TenantReclaimStore. Every
// method runs under InAgentTx: the drain is cross-tenant by nature and
// tenant_object_reclaim carries the matching _agent policy (m116). sites and
// backup_chunks are read through their existing _agent policies; tenants has no
// RLS.
func NewTenantReclaimStore(pool *db.Pool) TenantReclaimStore {
	return &pgTenantReclaimStore{pool: pool}
}

func (s *pgTenantReclaimStore) ListDue(ctx context.Context, maxAttempts, limit int32) ([]TenantReclaimTask, error) {
	var out []TenantReclaimTask
	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListDueTenantObjectReclaims(ctx, sqlc.ListDueTenantObjectReclaimsParams{
			MaxAttempts: maxAttempts,
			RowLimit:    limit,
		})
		if err != nil {
			return err
		}
		out = tenantReclaimTasksFromRows(rows)
		return nil
	})
	return out, err
}

func (s *pgTenantReclaimStore) ListStuck(ctx context.Context, maxAttempts, limit int32) ([]TenantReclaimTask, error) {
	var out []TenantReclaimTask
	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListStuckTenantObjectReclaims(ctx, sqlc.ListStuckTenantObjectReclaimsParams{
			MaxAttempts: maxAttempts,
			RowLimit:    limit,
		})
		if err != nil {
			return err
		}
		out = tenantReclaimTasksFromRows(rows)
		return nil
	})
	return out, err
}

func tenantReclaimTasksFromRows(rows []sqlc.TenantObjectReclaim) []TenantReclaimTask {
	out := make([]TenantReclaimTask, 0, len(rows))
	for _, r := range rows {
		t := TenantReclaimTask{
			ID: r.ID, TenantID: r.TenantID, Kind: r.Kind,
			Attempts: r.Attempts, CreatedAt: r.CreatedAt,
		}
		if r.LastError != nil {
			t.LastError = *r.LastError
		}
		out = append(out, t)
	}
	return out
}

func (s *pgTenantReclaimStore) TenantExists(ctx context.Context, tenantID uuid.UUID) (bool, error) {
	var exists bool
	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		got, err := sqlc.New(tx).TenantExistsForReclaim(ctx, tenantID)
		if err != nil {
			return err
		}
		exists = got
		return nil
	})
	return exists, err
}

func (s *pgTenantReclaimStore) SitesExist(ctx context.Context, tenantID uuid.UUID) (bool, error) {
	var exists bool
	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		got, err := sqlc.New(tx).SitesExistForTenantReclaim(ctx, tenantID)
		if err != nil {
			return err
		}
		exists = got
		return nil
	})
	return exists, err
}

func (s *pgTenantReclaimStore) ChunkRowsExist(ctx context.Context, tenantID uuid.UUID) (bool, error) {
	var exists bool
	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		got, err := sqlc.New(tx).ChunkRowsExistForTenantReclaim(ctx, tenantID)
		if err != nil {
			return err
		}
		exists = got
		return nil
	})
	return exists, err
}

func (s *pgTenantReclaimStore) Complete(ctx context.Context, id uuid.UUID) error {
	return s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		_, err := sqlc.New(tx).CompleteTenantObjectReclaim(ctx, id)
		return err
	})
}

func (s *pgTenantReclaimStore) Fail(ctx context.Context, id uuid.UUID, backoff time.Duration, reason string) error {
	return s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		_, err := sqlc.New(tx).FailTenantObjectReclaim(ctx, sqlc.FailTenantObjectReclaimParams{
			ID:        id,
			Backoff:   pgtype.Interval{Microseconds: backoff.Microseconds(), Valid: true},
			LastError: &reason,
		})
		return err
	})
}
