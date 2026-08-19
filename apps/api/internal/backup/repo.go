package backup

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// isUniqueViolation reports whether err is a Postgres UNIQUE constraint
// violation (SQLSTATE 23505). Used to treat a partial-unique-index collision on
// backup_snapshots(site_id) WHERE status IN ('pending','running') as an
// already-in-flight signal rather than a hard error.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// Repo is the tenant-scoped persistence for backup snapshots/manifests/chunks/
// schedules, plus the cross-tenant scheduler enumeration. Every tenant-scoped
// method runs inside a tenant transaction so RLS enforces isolation even if a
// query omitted its tenant filter.
type Repo interface {
	// Snapshots.
	CreateSnapshot(ctx context.Context, in CreateSnapshotInput) (Snapshot, error)
	GetSnapshot(ctx context.Context, tenantID, snapshotID uuid.UUID) (Snapshot, error)
	// GetSnapshotScoped performs the same lookup as GetSnapshot but routes the
	// database transaction through pool.RunTenantTx keyed on the supplied
	// principal. For a site-scoped principal (Scope=="site") this activates
	// InScopedTenantTx so the RESTRICTIVE backup_snapshots_site_scope RLS
	// policy denies access to non-granted sites BEFORE any presigned URL is
	// minted. For org-scoped and legacy (Scope=="") principals it is
	// behaviourally identical to GetSnapshot. Used by PresignChunks and
	// PlanRestore as the gate lookup.
	GetSnapshotScoped(ctx context.Context, p db.ScopedPrincipal, tenantID, snapshotID uuid.UUID) (Snapshot, error)
	ListSnapshotsForSite(ctx context.Context, tenantID, siteID uuid.UUID, limit, offset int32) ([]Snapshot, error)
	MarkSnapshotRunning(ctx context.Context, tenantID, snapshotID uuid.UUID) (Snapshot, error)
	CompleteSnapshot(ctx context.Context, tenantID, snapshotID uuid.UUID, totalSize, chunkCount int64) (Snapshot, error)
	FailSnapshot(ctx context.Context, tenantID, snapshotID uuid.UUID, errMsg string) (Snapshot, error)
	// UpdateSnapshotProgress replaces the JSONB progress payload with the given
	// raw bytes (caller-validated JSON) and bumps progress_updated_at to now.
	// Tenant-scoped — the caller passes the tenant from the verified agent
	// identity, never from the request body.
	UpdateSnapshotProgress(ctx context.Context, tenantID, snapshotID uuid.UUID, progress []byte) (Snapshot, error)
	// ListStalledRunningSnapshots enumerates `status='running'` snapshots whose
	// last progress update (or start time, if no progress was ever posted) is
	// older than `soft`. Each row also carries a Hard flag computed against the
	// (longer) `hard` threshold. Cross-tenant — runs under `app.agent='on'` for
	// the watchdog periodic (GH #279 two-tier policy). The caller stamps a
	// soft-only row via MarkSnapshotStalled and fails a hard row via
	// FailStalledSnapshot (both tenant-scoped).
	ListStalledRunningSnapshots(ctx context.Context, soft, hard time.Duration) ([]StalledSnapshot, error)
	// MarkSnapshotStalled stamps stalled_at=now() on a still-running snapshot
	// whose progress has gone quiet past the soft threshold (GH #279). Returns
	// marked=false with no error when the row was already stalled or is no
	// longer running — an idempotent no-op, never an error. Tenant-scoped.
	MarkSnapshotStalled(ctx context.Context, tenantID, snapshotID uuid.UUID) (bool, error)
	// ClearSnapshotStalled clears stalled_at when the snapshot is still running
	// (the GH #279 proof-of-life predicate). Returns cleared=false with no
	// error when the row was not running or was not currently stalled — this
	// is what prevents a late presign/manifest/progress POST from reviving a
	// snapshot the watchdog already hard-failed or the operator cancelled.
	// Tenant-scoped.
	ClearSnapshotStalled(ctx context.Context, tenantID, snapshotID uuid.UUID) (bool, error)
	// FailStalledSnapshot is the TOCTOU-safe hard-fail path used ONLY by the
	// progress watchdog's hard-deadline branch (GH #279 must-fix). Unlike
	// FailSnapshot's blind UPDATE — which CancelSnapshot depends on to fail a
	// still-'pending' row and so must keep no status guard — this adds
	// "AND status='running'" so a row that completed, was cancelled, was
	// agent-failed, or resumed between ListStalledRunningSnapshots' commit and
	// this call is left untouched. Returns the number of rows actually
	// transitioned (0 or 1); the caller uses this to decide whether to publish
	// the 'failed' SSE event and send the failure notification. Tenant-scoped.
	FailStalledSnapshot(ctx context.Context, tenantID, snapshotID uuid.UUID, errMsg string) (int64, error)
	// GetLatestCompletedSnapshot returns the most recent completed snapshot for
	// (tenantID, siteID). Used by resolveChainForSite to determine is_incremental.
	// Returns domain.NotFound when no completed snapshot exists.
	GetLatestCompletedSnapshot(ctx context.Context, tenantID, siteID uuid.UUID) (Snapshot, error)

	// Manifest.
	ListManifest(ctx context.Context, tenantID, snapshotID uuid.UUID) ([]ManifestEntry, error)
	// HasFilesList reports whether the snapshot carries a `files-list` manifest
	// entry (ADR-051). The chain auto-base resolver uses it to decide whether a
	// prior snapshot is diffable under the archive-delta model. Tenant-scoped.
	HasFilesList(ctx context.Context, tenantID, snapshotID uuid.UUID) (bool, error)
	// RecordManifest atomically records a submitted manifest: it upserts each
	// referenced chunk (storing not-yet-stored ones), increments refcounts for
	// every chunk reference, inserts the manifest entries, and completes the
	// snapshot. Returns the total chunk references and how many chunks were newly
	// stored.
	RecordManifest(ctx context.Context, in RecordManifestInput) (chunkRefs, storedCount int64, err error)

	// Chunk dedup: which of the given hashes are already stored for the tenant.
	ExistingChunkHashes(ctx context.Context, tenantID uuid.UUID, hashes []string) (map[string]Chunk, error)

	// Schedules.
	GetSchedule(ctx context.Context, tenantID, siteID uuid.UUID) (Schedule, error)
	UpsertSchedule(ctx context.Context, in UpsertScheduleInput) (Schedule, error)
	// ListDueSchedules enumerates enabled, due schedules across ALL tenants for
	// the periodic scheduler (cross-tenant, under app.agent).
	ListDueSchedules(ctx context.Context, now time.Time, limit int32) ([]Schedule, error)
	// ClaimAndAdvanceDueSchedules atomically selects enabled, due schedules with
	// FOR UPDATE SKIP LOCKED, advances each one's next_run_at to the supplied
	// nextAt values, and returns the fired slots — all in a single agent transaction.
	// The caller supplies a map from schedule ID to the already-computed next time
	// (tz/jitter math stays in Go). Only schedules whose advance succeeded are
	// returned: a row that cannot be locked (already held by another scheduler pass)
	// is silently skipped, preventing duplicate fires across concurrent CP instances
	// and RunOnStart re-fires.
	ClaimAndAdvanceDueSchedules(ctx context.Context, now time.Time, nextAt map[uuid.UUID]time.Time) ([]Schedule, error)
	// ListTenantsForGC enumerates the tenants the periodic retention GC should
	// visit, cross-tenant under app.agent. GH #402: that is tenants with a
	// completed snapshot UNION tenants with chunk rows, not the former alone.
	// Deleting the site that held a tenant's LAST completed snapshot used to
	// drop the tenant off this roster permanently, so its chunk bytes were
	// never swept again. This widens ENUMERATION only; every delete guard in
	// gc.go still applies unchanged to a newly-visited tenant.
	ListTenantsForGC(ctx context.Context) ([]uuid.UUID, error)
	// AdvanceScheduleRun records an enqueued scheduled backup and advances
	// next_run_at, tenant-scoped.
	AdvanceScheduleRun(ctx context.Context, tenantID, scheduleID uuid.UUID, next time.Time) error
	// CountInFlightSnapshots returns the count of pending/running snapshots for
	// (tenantID, siteID). Used as a belt-and-suspenders guard before CreateSnapshot
	// to skip duplicate fires that slip past the partial-unique index. Tenant-scoped.
	CountInFlightSnapshots(ctx context.Context, tenantID, siteID uuid.UUID) (int64, error)
	// HealOverdueSchedules advances every enabled schedule whose next_run_at is
	// already <= now to the next future occurrence, using per-site tz/jitter math.
	// Implemented as a Go loop over raw schedule rows (not a bulk SQL update) so
	// nextOccurrence can apply DST-aware per-site timezone math. Cross-tenant —
	// runs under app.agent. Called once at boot before the scheduler starts.
	HealOverdueSchedules(ctx context.Context, now time.Time, compute func(sched Schedule, now time.Time) time.Time) (int, error)
	// ReconcileDuplicateInflightSnapshots marks the older of any duplicate
	// pending/running snapshots for the same site as failed with the reason
	// "duplicate_in_flight_healed". Called once at boot before the partial-unique
	// index migration is applied (or at migration time via Go boot task).
	ReconcileDuplicateInflightSnapshots(ctx context.Context) (int, error)

	// SetSnapshotLocked sets the locked flag on a snapshot (Track C, m49). When
	// locked=true the retention GC will never auto-prune the snapshot; the
	// operator must explicitly unlock it first. Tenant-scoped.
	SetSnapshotLocked(ctx context.Context, tenantID, snapshotID uuid.UUID, locked bool) (Snapshot, error)

	// Fleet endpoints (operator-scoped, no :siteId).

	// FleetListSnapshots returns a paginated list of backup snapshots across the
	// supplied set of site IDs. Routes the transaction through RunTenantTx so a
	// site-scoped principal activates InScopedTenantTx and the RESTRICTIVE
	// backup_snapshots_site_scope RLS policy acts as the DB-level backstop.
	FleetListSnapshots(ctx context.Context, p db.ScopedPrincipal, tenantID uuid.UUID, f FleetListFilter) (FleetSnapshotPage, error)

	// FleetBackupHealth returns one FleetBackupHealthItem per requested site.
	// The health status is derived in Go from the raw correlated-subquery row
	// (last_completed_at, last_failed_at, in_flight_count, schedule_cadence).
	// Routes through RunTenantTx — same scoping as FleetListSnapshots.
	FleetBackupHealth(ctx context.Context, p db.ScopedPrincipal, tenantID uuid.UUID, siteIDs []uuid.UUID) ([]FleetBackupHealthItem, error)

	// GetBackupSettings returns the settings row for the site. Returns
	// domain.NotFound when no row exists (caller applies safe defaults).
	// Tenant scoping is enforced via InTenantTx + the FK from sites which
	// is RLS-protected.
	GetBackupSettings(ctx context.Context, tenantID, siteID uuid.UUID) (SiteBackupSettings, error)

	// UpsertBackupSettings creates or updates the settings row for the site.
	// All seven Track-A and Track-B columns are written in one UPSERT so the
	// caller must merge existing notification fields before calling when only
	// updating content fields (and vice-versa).
	UpsertBackupSettings(ctx context.Context, tenantID uuid.UUID, in SiteBackupSettings) (SiteBackupSettings, error)

	// Retention GC.
	ListExpiredSnapshots(ctx context.Context, tenantID uuid.UUID, before time.Time) ([]Snapshot, error)
	ListCompletedSnapshotsForSite(ctx context.Context, tenantID, siteID uuid.UUID) ([]SnapshotMeta, error)
	ListSiteIDsWithSnapshots(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error)
	SetSnapshotArchived(ctx context.Context, tenantID, snapshotID uuid.UUID, archived bool) error

	// ADR-050 MARK-AND-SWEEP retention GC.

	// DeleteSnapshot removes a snapshot row (manifest entries + file index cascade
	// via FK ON DELETE CASCADE). Metadata-only: it does NOT touch object storage
	// and does NOT decref chunks (refcount is observability-only post-ADR-050 —
	// chunk reachability is decided by the mark-and-sweep pass, never by refcount).
	// Tenant-scoped.
	DeleteSnapshot(ctx context.Context, tenantID, snapshotID uuid.UUID) error
	// ListInFlightSnapshotFloor returns the MIN(created_at) among pending/running
	// snapshots for the tenant, or the zero time when none are in flight. The GC
	// uses min(markStart, this) as the chunk-deletion grace floor so an in-flight
	// backup re-referencing an old chunk (whose manifest is not yet visible at
	// mark time) cannot have that chunk swept. Tenant-scoped.
	ListInFlightSnapshotFloor(ctx context.Context, tenantID uuid.UUID) (time.Time, error)
	// DBNow returns the database clock (SELECT now()). The GC captures markStart
	// from the DB — never the app clock — so the grace floor compares against the
	// same time source as backup_chunks.created_at. Tenant-scoped.
	DBNow(ctx context.Context, tenantID uuid.UUID) (time.Time, error)
	// SweepTenantChunks runs the ADR-050 chunk sweep for one tenant. It first
	// takes a SESSION-level per-tenant pg_try_advisory_lock(hashtext('backup_gc'),
	// hashtext(tenant)) (released via pg_advisory_unlock in a defer). If the lock
	// is not acquired it sets *acquired=false and returns nil (the tenant is
	// skipped — another sweep is in progress). Otherwise it sets *acquired=true
	// and streams every chunk keyset-paged by (created_at, blake3) using SHORT
	// per-page transactions, so no pooled connection is pinned across object-store
	// I/O (avoiding Cloud SQL's idle_in_transaction_session_timeout). For each
	// chunk it invokes del(SweepChunk, stillReferenced) INSIDE the per-chunk
	// FOR UPDATE critical section (on the SAME pinned connection) — del does the
	// object-FIRST delete and returns true when the row should now be removed;
	// the repo then removes those rows in the SAME short-lived tx, re-checking
	// GREATEST(created_at, last_referenced_at) < floor at the DB. The session
	// lock keeps the per-tenant sweep exclusive across the whole pass.
	//
	// stillReferenced is the GH #168 P2 ground-truth guard, LAZILY evaluated:
	// calling it runs "does any backup_manifest_entries row for the tenant still
	// list this hash?" on the SAME pinned connection/tx sweepOneChunk already
	// holds the row's FOR UPDATE lock under — never a second pooled connection
	// (a second Acquire from inside an already-pinned-connection critical
	// section risks pool exhaustion under concurrent sweeps; see gc.go's
	// sweepChunks for why del calls it only when the liveSet check didn't
	// already decide). Deliberately scoped to backup_manifest_entries only (NOT
	// backup_file_index — see chunkStillReferencedOnTx's doc for why that
	// table's per-file HISTORY rows would make the check unsound). If it
	// reports true, del should skip the delete regardless of what the
	// (possibly buggy) liveSet computation concluded — ground truth always
	// outranks the computed liveSet proxy. Tenant-scoped.
	SweepTenantChunks(ctx context.Context, tenantID uuid.UUID, floor time.Time, acquired *bool, del func(c SweepChunk, stillReferenced func() (bool, error)) (bool, error)) error

	// CompleteIncrementalManifest atomically records an incremental submission:
	// it inserts the backup_file_index rows, optionally records the DB-dump
	// manifest (chunk upsert + refcount + manifest insert), and completes the
	// snapshot — all in ONE transaction (ADR-050 STEP 2). Returns
	// (chunkRefs, storedCount). Tenant-scoped.
	CompleteIncrementalManifest(ctx context.Context, in CompleteIncrementalInput) (chunkRefs, storedCount int64, err error)

	// Chain-aware bulk delete (issue #115).

	// GetSnapshotsByIDs resolves a batch of snapshot IDs to their rows in ONE
	// query, tenant-scoped. An id that does not exist, or belongs to another
	// tenant, is simply absent from the returned map — RLS plus the explicit
	// tenant_id filter already collapse "does not exist" and "wrong tenant" into
	// the same observable outcome, so the caller (BulkDeleteSnapshots) never gets
	// a chance to leak the difference. Returns an empty (non-nil) map for an
	// empty ids slice without touching the database.
	GetSnapshotsByIDs(ctx context.Context, tenantID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]Snapshot, error)

	// HasActiveRestore reports, for each supplied "restore group key" (see
	// restoreGroupKey — a chain's own chain_id for a chained snapshot, or a
	// standalone snapshot's own id), whether at least one restore_runs row in
	// an ACTIVE status (queued|running) currently targets ANY snapshot sharing
	// that key. A restore plans over the WHOLE chain up to its target
	// generation, so an active restore anchored on any member makes every
	// member of that chain unsafe to delete — grouping by chain (rather than by
	// the exact snapshot id a restore_runs row references) is what makes this
	// check correct for siblings the caller never even mentioned. Tenant-scoped.
	// Returns an empty (non-nil) map for an empty groupKeys slice without
	// touching the database.
	HasActiveRestore(ctx context.Context, tenantID uuid.UUID, groupKeys []uuid.UUID) (map[uuid.UUID]bool, error)

	// ADR-049 incremental restore chain planner.

	// ListChainSnapshots returns all snapshots belonging to chainID whose
	// generation is <= maxGeneration, ordered by generation ASC. The base
	// (generation 0) is included because the base snapshot's chain_id is set to
	// its own ID. Tenant-scoped. Returns an empty slice (not an error) when no
	// rows match.
	ListChainSnapshots(ctx context.Context, tenantID uuid.UUID, chainID uuid.UUID, maxGeneration int) ([]Snapshot, error)

	// ListCompletedChainSnapshots is the GH #168 hardened variant of
	// ListChainSnapshots: it applies "AND status='completed'" IN SQL so a
	// failed/aborted retry that reused a (chain_id, generation) pair (no unique
	// constraint covers terminal rows pre-m96) can never be returned in place of
	// the real completed row for that generation. reachableChunks (the shared
	// GC-mark/restore reachability oracle) uses this instead of ListChainSnapshots
	// so a duplicate-generation row is filtered at the SQL layer, independent of
	// the Go-side byGen dedup (chainGenWinner) — two independent layers protecting
	// the same invariant. Ordered by generation ASC, id ASC: the id tiebreak
	// makes the (rare, pre-migration-only) case of two genuinely completed rows
	// at the same generation deterministic rather than depending on physical scan
	// order. Tenant-scoped. Returns an empty slice (not an error) when no rows
	// match.
	ListCompletedChainSnapshots(ctx context.Context, tenantID uuid.UUID, chainID uuid.UUID, maxGeneration int) ([]Snapshot, error)

	// ADR-048 incremental backup file index.

	// InsertFileIndexBatch inserts a batch of FileIndexEntry rows into
	// backup_file_index for a completed incremental snapshot. Tenant-scoped.
	InsertFileIndexBatch(ctx context.Context, tenantID, snapshotID uuid.UUID, entries []FileIndexEntry) error
	// CountFileIndex returns the number of backup_file_index rows for a snapshot.
	// Used by the streaming endpoint's soft-cap check. Tenant-scoped.
	CountFileIndex(ctx context.Context, tenantID, snapshotID uuid.UUID) (int64, error)
	// StreamFileIndex calls fn for each FileIndexEntry ordered by file_path ASC.
	// The iteration stops when fn returns a non-nil error. Uses a server-side
	// cursor to avoid loading all rows into memory. Tenant-scoped.
	StreamFileIndex(ctx context.Context, tenantID, snapshotID uuid.UUID, fn func(FileIndexEntry) error) error
	// StreamChainEffectiveFileIndex calls fn for each surviving FileIndexEntry of
	// the MERGED effective file tree for chainID over generations 0..maxGeneration,
	// ordered by file_path ASC. It reuses the same latest-version-wins + tombstone
	// merge that reachableChunks/planRestoreChain apply (walk generations ascending,
	// a later generation's entry for a path overwrites an earlier one, a tombstone
	// removes the path), so the streamed view equals the restore view at maxGeneration.
	// Surviving entries are non-tombstone by construction. The full merged set is
	// held in memory (bounded by the live file count, the same envelope restore
	// already pays) so the result can be sorted by file_path before emitting.
	// Tenant-scoped.
	StreamChainEffectiveFileIndex(ctx context.Context, tenantID, chainID uuid.UUID, maxGeneration int, fn func(FileIndexEntry) error) error
	// UpdateSnapshotCycleStats stamps the incremental cycle telemetry counters
	// on a snapshot row after SubmitIncrementalManifest completes the snapshot.
	UpdateSnapshotCycleStats(ctx context.Context, tenantID, snapshotID uuid.UUID, in CycleStatsInput) error
}

// CreateSnapshotInput creates a pending snapshot.
type CreateSnapshotInput struct {
	TenantID     uuid.UUID
	SiteID       uuid.UUID
	CreatedBy    uuid.UUID
	Kind         string
	AgeRecipient string
	// ADR-048 incremental fields. Zero values produce a full-base snapshot row.
	IsIncremental    bool
	ParentSnapshotID *uuid.UUID
	BaseSnapshotID   *uuid.UUID
	ChainID          *uuid.UUID
	Generation       int
	// DestinationID (M7 / ADR-036 P1): the site_destinations row this snapshot's
	// chunks should be stored against. uuid.Nil (the zero value) routes to the
	// legacy CP-managed bucket — the pre-existing, always-managed-storage
	// behaviour for every caller that does not resolve a destination.
	DestinationID uuid.UUID
}

// CycleStatsInput is the set of incremental telemetry counters stamped at
// SubmitIncrementalManifest time.
type CycleStatsInput struct {
	CycleFilesScanned  int64
	CycleFilesChanged  int64
	CycleFilesDeleted  int64
	CycleBytesUploaded int64
}

// UpsertScheduleInput creates/updates a per-site schedule.
type UpsertScheduleInput struct {
	TenantID           uuid.UUID
	SiteID             uuid.UUID
	Cadence            string
	Kind               string
	Enabled            bool
	RetentionDays      int32
	MonthlyArchiveKeep int32
	NextRunAt          time.Time
	// New timing fields (M17).
	RunHour        int32
	RunMinute      int32
	DayOfWeek      *int32
	DayOfMonth     *int32
	FrequencyHours *int32
	KeepLast       int32
	// ADR-048 P5: per-schedule incremental opt-in + optional base-window override.
	IncrementalEnabled bool
	BaseWindowDays     *int32
}

// StalledSnapshot is the cross-tenant projection used by the M5.6 progress
// watchdog: enough to mark the snapshot failed (or soft-stamp it) in its own
// tenant scope. GH #279: StalledAt mirrors the persisted column (nil until
// the watchdog stamps it) and Hard reports whether the row has ALSO crossed
// the hard threshold — the watchdog uses Hard to decide fail vs. stamp.
type StalledSnapshot struct {
	ID                uuid.UUID
	TenantID          uuid.UUID
	SiteID            uuid.UUID
	StartedAt         *time.Time
	ProgressUpdatedAt *time.Time
	StalledAt         *time.Time
	Hard              bool
}

// SnapshotMeta is the slim projection used by the retention archive computation.
// ADR-050 widened it to carry the chain columns so the mark-and-sweep GC can do
// chain-aware expansion (pin a carry-forward chunk's origin generation under a
// live tip) and pick the highest retained generation per chain.
// m49 adds Locked so the GC phase-0 selection can skip locked snapshots.
type SnapshotMeta struct {
	ID            uuid.UUID
	CreatedAt     time.Time
	Archived      bool
	ChainID       *uuid.UUID
	Generation    int
	IsIncremental bool
	// Locked snapshots (Track C, m49) are never auto-pruned; the GC selection
	// treats them as permanently retained regardless of age/count rules.
	Locked bool
}

// SweepChunk is the slim projection the sweep streams: enough to test
// a chunk against the live set + grace floor and to delete its object + row.
// ADR-050 data-loss fix: LastReferencedAt is carried so the per-row delete
// decision uses GREATEST(CreatedAt, LastReferencedAt) < floor — an OLD chunk an
// in-flight backup re-referenced via tenant-global dedup has a fresh
// LastReferencedAt and is therefore protected.
type SweepChunk struct {
	Blake3           string
	S3Key            string
	CreatedAt        time.Time
	LastReferencedAt time.Time
}

// CompleteIncrementalInput is the atomic-completion payload for ADR-050 STEP 2:
// it folds the file-index batch insert, optional DB-manifest recording, and the
// snapshot completion into ONE transaction so a concurrent sweep can never
// observe status='completed' before the file_index rows it must walk are
// visible.
type CompleteIncrementalInput struct {
	TenantID   uuid.UUID
	SnapshotID uuid.UUID
	// FileEntries are the backup_file_index rows (changed files + tombstones).
	FileEntries []FileIndexEntry
	// DBManifest, when non-nil, records the DB-dump manifest entries + chunks via
	// the same RecordManifest logic (chunk upsert + refcount + manifest insert +
	// snapshot completion). When nil the snapshot is completed directly with the
	// supplied TotalSize/ChunkRefs (the files-only path).
	DBManifest *RecordManifestInput
	// TotalSize / ChunkRefs are used only on the files-only path (DBManifest nil)
	// to complete the snapshot.
	TotalSize int64
	ChunkRefs int64
}

// ChunkUpload describes a chunk reference in a submitted manifest: its hash,
// ciphertext size, and content-addressed s3 key.
type ChunkUpload struct {
	Blake3 string
	Size   int64
	S3Key  string
}

// RecordManifestInput is the validated input for recording a submitted manifest.
type RecordManifestInput struct {
	TenantID   uuid.UUID
	SnapshotID uuid.UUID
	Entries    []ManifestEntryInput
	// Chunks is the de-duplicated set of distinct chunks referenced by the
	// manifest (hash -> upload metadata) used to upsert backup_chunks rows.
	Chunks map[string]ChunkUpload
}

// ManifestEntryInput is one file/db entry to persist.
type ManifestEntryInput struct {
	Path        string
	EntryKind   string
	TableName   string
	ChunkHashes []string
	Size        int64
	Mode        int32
}

type pgRepo struct {
	pool *db.Pool
}

// NewRepo builds a Repo backed by the pgx pool with RLS enforcement.
func NewRepo(pool *db.Pool) Repo { return &pgRepo{pool: pool} }

func (r *pgRepo) CreateSnapshot(ctx context.Context, in CreateSnapshotInput) (Snapshot, error) {
	var out Snapshot
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		var createdBy pgtype.UUID
		if in.CreatedBy != uuid.Nil {
			createdBy = pgtype.UUID{Bytes: in.CreatedBy, Valid: true}
		}
		var destinationID pgtype.UUID
		if in.DestinationID != uuid.Nil {
			destinationID = pgtype.UUID{Bytes: in.DestinationID, Valid: true}
		}
		row, err := sqlc.New(tx).CreateBackupSnapshot(ctx, sqlc.CreateBackupSnapshotParams{
			TenantID:      in.TenantID,
			SiteID:        in.SiteID,
			CreatedBy:     createdBy,
			Kind:          in.Kind,
			AgeRecipient:  in.AgeRecipient,
			DestinationID: destinationID,
		})
		if err != nil {
			return domain.Internal("backup_snapshot_create_failed", "failed to create snapshot").WithCause(err)
		}
		out = toSnapshot(row)

		// ADR-048/050: stamp incremental chain fields when this is an incremental
		// run. We do this with a raw UPDATE because the sqlc-generated
		// CreateBackupSnapshot predates the m44 columns — updating the generated
		// code requires regenerating sqlc which is out of scope for this migration.
		// The UPDATE is within the same transaction.
		//
		// chain_id resolution (ADR-050, m46): a generation-0 snapshot (a full base
		// OR a plain full backup) anchors its OWN chain, so chain_id = its own id
		// when no explicit chain_id was supplied. Without this a base's chain_id
		// stays NULL and the whole chain is unresolvable by ListChainSnapshots /
		// planRestoreChain / the retention-GC mark walk. Increments always pass an
		// explicit ChainID (the base's). This is the forward counterpart to the m46
		// backfill of existing bases.
		resolvedChainID := in.ChainID
		if resolvedChainID == nil && in.Generation == 0 {
			id := out.ID
			resolvedChainID = &id
		}
		if in.IsIncremental || in.Generation > 0 || in.ParentSnapshotID != nil || resolvedChainID != nil {
			var parentID, baseID, chainID *[16]byte
			if in.ParentSnapshotID != nil {
				b := [16]byte(*in.ParentSnapshotID)
				parentID = &b
			}
			if in.BaseSnapshotID != nil {
				b := [16]byte(*in.BaseSnapshotID)
				baseID = &b
			}
			if resolvedChainID != nil {
				b := [16]byte(*resolvedChainID)
				chainID = &b
			}
			_, uerr := tx.Exec(ctx,
				`UPDATE backup_snapshots
				    SET is_incremental=$3, parent_snapshot_id=$4, base_snapshot_id=$5,
				        chain_id=$6, generation=$7
				  WHERE id=$1 AND tenant_id=$2`,
				out.ID, in.TenantID,
				in.IsIncremental,
				parentID,
				baseID,
				chainID,
				in.Generation,
			)
			if uerr != nil {
				return domain.Internal("backup_snapshot_create_failed", "failed to stamp incremental fields").WithCause(uerr)
			}
			out.IsIncremental = in.IsIncremental
			out.ParentSnapshotID = in.ParentSnapshotID
			out.BaseSnapshotID = in.BaseSnapshotID
			out.ChainID = resolvedChainID
			out.Generation = in.Generation
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) GetLatestCompletedSnapshot(ctx context.Context, tenantID, siteID uuid.UUID) (Snapshot, error) {
	var out Snapshot
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			snapshotSelectColumns+` FROM backup_snapshots
			  WHERE tenant_id=$1 AND site_id=$2 AND status='completed'
			  ORDER BY created_at DESC
			  LIMIT 1`,
			tenantID, siteID,
		)
		s, err := scanSnapshotWithChainFields(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("backup_snapshot_not_found", "no completed snapshot found for site")
			}
			return domain.Internal("backup_snapshot_get_failed", "failed to query latest snapshot").WithCause(err)
		}
		out = s
		return nil
	})
	return out, err
}

func (r *pgRepo) GetSnapshot(ctx context.Context, tenantID, snapshotID uuid.UUID) (Snapshot, error) {
	var out Snapshot
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			snapshotSelectColumns+` FROM backup_snapshots WHERE id=$1 AND tenant_id=$2`,
			snapshotID, tenantID,
		)
		s, err := scanSnapshotWithChainFields(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("backup_snapshot_not_found", "backup snapshot not found")
			}
			return domain.Internal("backup_snapshot_get_failed", "failed to load snapshot").WithCause(err)
		}
		out = s
		return nil
	})
	return out, err
}

// GetSnapshotScoped performs the same lookup as GetSnapshot but routes the
// transaction through pool.RunTenantTx so a site-scoped principal activates
// InScopedTenantTx. For org-scoped/legacy principals behaviour is identical
// to GetSnapshot. See the Repo interface for the full contract.
func (r *pgRepo) GetSnapshotScoped(ctx context.Context, p db.ScopedPrincipal, tenantID, snapshotID uuid.UUID) (Snapshot, error) {
	var out Snapshot
	err := r.pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			snapshotSelectColumns+` FROM backup_snapshots WHERE id=$1 AND tenant_id=$2`,
			snapshotID, tenantID,
		)
		s, err := scanSnapshotWithChainFields(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("backup_snapshot_not_found", "backup snapshot not found")
			}
			return domain.Internal("backup_snapshot_get_failed", "failed to load snapshot").WithCause(err)
		}
		out = s
		return nil
	})
	return out, err
}

func (r *pgRepo) ListSnapshotsForSite(ctx context.Context, tenantID, siteID uuid.UUID, limit, offset int32) ([]Snapshot, error) {
	var out []Snapshot
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			snapshotSelectColumns+` FROM backup_snapshots
			  WHERE tenant_id=$1 AND site_id=$2
			  ORDER BY created_at DESC
			  LIMIT $3 OFFSET $4`,
			tenantID, siteID, limit, offset,
		)
		if err != nil {
			return domain.Internal("backup_snapshot_list_failed", "failed to list snapshots").WithCause(err)
		}
		defer rows.Close()
		out = make([]Snapshot, 0)
		for rows.Next() {
			s, serr := scanSnapshotWithChainFields(rows)
			if serr != nil {
				return domain.Internal("backup_snapshot_list_scan_failed", "failed to scan snapshot row").WithCause(serr)
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, err
}

func (r *pgRepo) MarkSnapshotRunning(ctx context.Context, tenantID, snapshotID uuid.UUID) (Snapshot, error) {
	return r.mutateSnapshot(ctx, tenantID, func(q *sqlc.Queries) (sqlc.BackupSnapshot, error) {
		return q.MarkBackupSnapshotRunning(ctx, sqlc.MarkBackupSnapshotRunningParams{ID: snapshotID, TenantID: tenantID})
	}, "backup_snapshot_run_failed")
}

func (r *pgRepo) CompleteSnapshot(ctx context.Context, tenantID, snapshotID uuid.UUID, totalSize, chunkCount int64) (Snapshot, error) {
	return r.mutateSnapshot(ctx, tenantID, func(q *sqlc.Queries) (sqlc.BackupSnapshot, error) {
		return q.CompleteBackupSnapshot(ctx, sqlc.CompleteBackupSnapshotParams{
			ID: snapshotID, TenantID: tenantID, TotalSize: totalSize, ChunkCount: chunkCount,
		})
	}, "backup_snapshot_complete_failed")
}

func (r *pgRepo) FailSnapshot(ctx context.Context, tenantID, snapshotID uuid.UUID, errMsg string) (Snapshot, error) {
	return r.mutateSnapshot(ctx, tenantID, func(q *sqlc.Queries) (sqlc.BackupSnapshot, error) {
		return q.FailBackupSnapshot(ctx, sqlc.FailBackupSnapshotParams{ID: snapshotID, TenantID: tenantID, Error: errMsg})
	}, "backup_snapshot_fail_failed")
}

// UpdateSnapshotProgress is tenant-scoped (RLS enforces it); the agent handler
// passes the tenant from the verified Ed25519 identity.
func (r *pgRepo) UpdateSnapshotProgress(ctx context.Context, tenantID, snapshotID uuid.UUID, progress []byte) (Snapshot, error) {
	return r.mutateSnapshot(ctx, tenantID, func(q *sqlc.Queries) (sqlc.BackupSnapshot, error) {
		return q.UpdateBackupSnapshotProgress(ctx, sqlc.UpdateBackupSnapshotProgressParams{
			ID: snapshotID, TenantID: tenantID, Progress: progress,
		})
	}, "backup_snapshot_progress_failed")
}

// ListStalledRunningSnapshots runs cross-tenant under app.agent='on' (same
// pattern as ListDueSchedules / ListTenantsForGC). GH #279 two-tier policy:
// the watchdog stamps a soft-only row via MarkSnapshotStalled and fails a
// hard row via FailSnapshot (both tenant-scoped).
func (r *pgRepo) ListStalledRunningSnapshots(ctx context.Context, soft, hard time.Duration) ([]StalledSnapshot, error) {
	var out []StalledSnapshot
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		// pgtype.Interval round-trips as microseconds — convert duration to µs.
		softIval := pgtype.Interval{Microseconds: soft.Microseconds(), Valid: true}
		hardIval := pgtype.Interval{Microseconds: hard.Microseconds(), Valid: true}
		rows, err := sqlc.New(tx).ListStalledRunningSnapshots(ctx, sqlc.ListStalledRunningSnapshotsParams{
			SoftInterval: softIval,
			HardInterval: hardIval,
		})
		if err != nil {
			return domain.Internal("backup_snapshot_stalled_list_failed", "failed to list stalled snapshots").WithCause(err)
		}
		out = make([]StalledSnapshot, 0, len(rows))
		for _, row := range rows {
			s := StalledSnapshot{ID: row.ID, TenantID: row.TenantID, SiteID: row.SiteID}
			if row.StartedAt.Valid {
				t := row.StartedAt.Time
				s.StartedAt = &t
			}
			if row.ProgressUpdatedAt.Valid {
				t := row.ProgressUpdatedAt.Time
				s.ProgressUpdatedAt = &t
			}
			if row.StalledAt.Valid {
				t := row.StalledAt.Time
				s.StalledAt = &t
			}
			s.Hard = row.Hard != nil && *row.Hard
			out = append(out, s)
		}
		return nil
	})
	return out, err
}

// MarkSnapshotStalled is the GH #279 soft-stall stamp. Tenant-scoped; the
// underlying query's `status='running' AND stalled_at IS NULL` guard makes
// this idempotent — a row already stalled (or no longer running) reports
// marked=false with no error, never pgx.ErrNoRows surfaced as a failure.
func (r *pgRepo) MarkSnapshotStalled(ctx context.Context, tenantID, snapshotID uuid.UUID) (bool, error) {
	var marked bool
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := sqlc.New(tx).MarkBackupSnapshotStalled(ctx, sqlc.MarkBackupSnapshotStalledParams{ID: snapshotID, TenantID: tenantID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Already stalled, no longer running, or missing — idempotent no-op.
				return nil
			}
			return domain.Internal("backup_snapshot_stall_mark_failed", "failed to mark snapshot stalled").WithCause(err)
		}
		marked = true
		return nil
	})
	return marked, err
}

// ClearSnapshotStalled is the GH #279 proof-of-life clear. Tenant-scoped; the
// status='running' predicate is the anti-resurrection guarantee — see the
// Repo interface doc.
func (r *pgRepo) ClearSnapshotStalled(ctx context.Context, tenantID, snapshotID uuid.UUID) (bool, error) {
	var cleared bool
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		n, err := sqlc.New(tx).ClearBackupSnapshotStalled(ctx, sqlc.ClearBackupSnapshotStalledParams{ID: snapshotID, TenantID: tenantID})
		if err != nil {
			return domain.Internal("backup_snapshot_stall_clear_failed", "failed to clear stalled snapshot").WithCause(err)
		}
		cleared = n > 0
		return nil
	})
	return cleared, err
}

// FailStalledSnapshot is the TOCTOU-safe hard-fail path for the progress
// watchdog (GH #279 must-fix). The "AND status='running'" guard baked into
// FailStalledBackupSnapshot is what FailSnapshot's blind UPDATE lacks — see
// the Repo interface doc for why the shared FailSnapshot must stay unguarded.
func (r *pgRepo) FailStalledSnapshot(ctx context.Context, tenantID, snapshotID uuid.UUID, errMsg string) (int64, error) {
	var n int64
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).FailStalledBackupSnapshot(ctx, sqlc.FailStalledBackupSnapshotParams{
			ID: snapshotID, TenantID: tenantID, Error: errMsg,
		})
		if err != nil {
			return domain.Internal("backup_snapshot_stall_fail_failed", "failed to hard-fail stalled snapshot").WithCause(err)
		}
		n = rows
		return nil
	})
	return n, err
}

func (r *pgRepo) mutateSnapshot(ctx context.Context, tenantID uuid.UUID, fn func(*sqlc.Queries) (sqlc.BackupSnapshot, error), code string) (Snapshot, error) {
	var out Snapshot
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := fn(sqlc.New(tx))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("backup_snapshot_not_found", "backup snapshot not found")
			}
			return domain.Internal(code, "failed to update snapshot").WithCause(err)
		}
		out = toSnapshot(row)
		return nil
	})
	return out, err
}

// SetSnapshotLocked sets or clears the per-snapshot lock flag (Track C, m49).
// Runs inside a tenant transaction (RLS enforced). The UPDATE is issued first,
// then the updated row is re-read with snapshotSelectColumns so all chain +
// locked columns are returned correctly.
func (r *pgRepo) SetSnapshotLocked(ctx context.Context, tenantID, snapshotID uuid.UUID, locked bool) (Snapshot, error) {
	var out Snapshot
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx,
			`UPDATE backup_snapshots
			    SET locked=$3, updated_at=now()
			  WHERE id=$1 AND tenant_id=$2`,
			snapshotID, tenantID, locked,
		)
		if err != nil {
			return domain.Internal("backup_snapshot_lock_failed", "failed to update snapshot lock").WithCause(err)
		}
		if res.RowsAffected() == 0 {
			return domain.NotFound("backup_snapshot_not_found", "backup snapshot not found")
		}
		row := tx.QueryRow(ctx,
			snapshotSelectColumns+` FROM backup_snapshots WHERE id=$1 AND tenant_id=$2`,
			snapshotID, tenantID,
		)
		s, serr := scanSnapshotWithChainFields(row)
		if serr != nil {
			return domain.Internal("backup_snapshot_lock_read_failed", "failed to re-read snapshot after lock update").WithCause(serr)
		}
		out = s
		return nil
	})
	return out, err
}

func (r *pgRepo) ListManifest(ctx context.Context, tenantID, snapshotID uuid.UUID) ([]ManifestEntry, error) {
	var out []ManifestEntry
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListManifestEntries(ctx, sqlc.ListManifestEntriesParams{SnapshotID: snapshotID, TenantID: tenantID})
		if err != nil {
			return domain.Internal("backup_manifest_list_failed", "failed to list manifest entries").WithCause(err)
		}
		out = make([]ManifestEntry, 0, len(rows))
		for _, row := range rows {
			out = append(out, toManifestEntry(row))
		}
		return nil
	})
	return out, err
}

// HasFilesList reports whether the snapshot carries a `files-list` manifest
// entry (ADR-051). Reuses ListManifestEntries (a per-snapshot scan, cheap) so
// no new sqlc query is needed.
func (r *pgRepo) HasFilesList(ctx context.Context, tenantID, snapshotID uuid.UUID) (bool, error) {
	var has bool
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListManifestEntries(ctx, sqlc.ListManifestEntriesParams{SnapshotID: snapshotID, TenantID: tenantID})
		if err != nil {
			return domain.Internal("backup_manifest_list_failed", "failed to list manifest entries").WithCause(err)
		}
		for _, row := range rows {
			if row.EntryKind == EntryKindFilesList {
				has = true
				break
			}
		}
		return nil
	})
	return has, err
}

func (r *pgRepo) RecordManifest(ctx context.Context, in RecordManifestInput) (int64, int64, error) {
	var chunkRefs, storedCount, totalSize int64
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)

		// 1. Upsert each distinct chunk (idempotent; content-addressed). A chunk
		// whose row's refcount is still 0 after the upsert and that did not exist
		// before is "newly stored". We detect newly-stored by checking existence
		// first.
		for hash, up := range in.Chunks {
			_, getErr := q.GetBackupChunk(ctx, sqlc.GetBackupChunkParams{TenantID: in.TenantID, Blake3: hash})
			existed := getErr == nil
			if getErr != nil && !errors.Is(getErr, pgx.ErrNoRows) {
				return domain.Internal("backup_chunk_get_failed", "failed to check chunk existence").WithCause(getErr)
			}
			if _, err := q.UpsertBackupChunk(ctx, sqlc.UpsertBackupChunkParams{
				TenantID: in.TenantID, Blake3: hash, S3Key: up.S3Key, Size: up.Size,
			}); err != nil {
				return domain.Internal("backup_chunk_upsert_failed", "failed to upsert chunk").WithCause(err)
			}
			if !existed {
				storedCount++
			}
		}

		// 2. Insert manifest entries and increment refcounts for every chunk
		// reference (a chunk referenced N times across entries gets +N).
		referenced := map[string]struct{}{}
		for _, e := range in.Entries {
			if _, err := q.CreateManifestEntry(ctx, sqlc.CreateManifestEntryParams{
				SnapshotID:  in.SnapshotID,
				TenantID:    in.TenantID,
				Path:        e.Path,
				EntryKind:   e.EntryKind,
				TableName:   e.TableName,
				ChunkHashes: e.ChunkHashes,
				Size:        e.Size,
				Mode:        e.Mode,
			}); err != nil {
				return domain.Internal("backup_manifest_insert_failed", "failed to insert manifest entry").WithCause(err)
			}
			totalSize += e.Size
			for _, h := range e.ChunkHashes {
				if _, err := q.IncrementChunkRefcount(ctx, sqlc.IncrementChunkRefcountParams{TenantID: in.TenantID, Blake3: h}); err != nil {
					return domain.Internal("backup_chunk_incref_failed", "failed to increment chunk refcount").WithCause(err)
				}
				referenced[h] = struct{}{}
				chunkRefs++
			}
		}

		// 2b. ADR-050 belt: keep every referenced chunk's last_referenced_at fresh
		// at completion so a just-completed snapshot's chunks also clear the sweep's
		// GREATEST(created_at, last_referenced_at) < floor predicate, not only the
		// presign-time dedup touch.
		if terr := touchReferencedChunks(ctx, tx, in.TenantID, referenced); terr != nil {
			return terr
		}

		// 3. Complete the snapshot.
		if _, err := q.CompleteBackupSnapshot(ctx, sqlc.CompleteBackupSnapshotParams{
			ID: in.SnapshotID, TenantID: in.TenantID, TotalSize: totalSize, ChunkCount: chunkRefs,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("backup_snapshot_not_found", "backup snapshot not found")
			}
			return domain.Internal("backup_snapshot_complete_failed", "failed to complete snapshot").WithCause(err)
		}
		return nil
	})
	return chunkRefs, storedCount, err
}

func (r *pgRepo) ExistingChunkHashes(ctx context.Context, tenantID uuid.UUID, hashes []string) (map[string]Chunk, error) {
	out := map[string]Chunk{}
	if len(hashes) == 0 {
		return out, nil
	}
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		// ADR-050 mark-and-sweep data-loss fix: this is the dedup oracle
		// PresignChunks relies on to decide "already stored, skip upload" WITHOUT
		// re-uploading — which would otherwise leave an OLD chunk's created_at
		// ancient while an in-flight (status='running', not yet in the mark set)
		// backup re-references it. So we bump last_referenced_at = now() (DB clock)
		// for exactly the chunks we report as existing, in the SAME statement as
		// the existence read (UPDATE ... RETURNING). This guarantees any chunk
		// reported existing has just been touched, so a concurrent sweep — which
		// deletes only when GREATEST(created_at, last_referenced_at) < floor —
		// cannot delete it: its last_referenced_at >= the in-flight snapshot's
		// start >= inflightFloor >= effectiveFloor. Raw SQL (not sqlc) so the
		// touch and the read share one round-trip.
		rows, err := tx.Query(ctx,
			`UPDATE backup_chunks
			    SET last_referenced_at = now(), updated_at = now()
			  WHERE tenant_id = $1 AND blake3 = ANY($2::text[])
			RETURNING id, tenant_id, blake3, s3_key, size, refcount,
			          created_at, updated_at`,
			tenantID, hashes,
		)
		if err != nil {
			return domain.Internal("backup_chunk_existing_failed", "failed to query existing chunks").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			var c Chunk
			if serr := rows.Scan(
				&c.ID, &c.TenantID, &c.Blake3, &c.S3Key, &c.Size, &c.Refcount,
				&c.CreatedAt, &c.UpdatedAt,
			); serr != nil {
				return domain.Internal("backup_chunk_existing_scan_failed", "failed to scan existing chunk row").WithCause(serr)
			}
			out[c.Blake3] = c
		}
		return rows.Err()
	})
	return out, err
}

func (r *pgRepo) GetSchedule(ctx context.Context, tenantID, siteID uuid.UUID) (Schedule, error) {
	var out Schedule
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			scheduleSelectColumns+` FROM backup_schedules WHERE tenant_id=$1 AND site_id=$2`,
			tenantID, siteID,
		)
		s, serr := scanScheduleRow(row)
		if serr != nil {
			if errors.Is(serr, pgx.ErrNoRows) {
				return domain.NotFound("backup_schedule_not_found", "backup schedule not found")
			}
			return domain.Internal("backup_schedule_get_failed", "failed to load schedule").WithCause(serr)
		}
		out = s
		return nil
	})
	return out, err
}

func (r *pgRepo) UpsertSchedule(ctx context.Context, in UpsertScheduleInput) (Schedule, error) {
	var out Schedule
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		// Coerce *int32 timing fields to *int16 (DB column type smallint).
		var dow *int16
		if in.DayOfWeek != nil {
			v := int16(*in.DayOfWeek)
			dow = &v
		}
		var dom *int16
		if in.DayOfMonth != nil {
			v := int16(*in.DayOfMonth)
			dom = &v
		}
		var fh *int16
		if in.FrequencyHours != nil {
			v := int16(*in.FrequencyHours)
			fh = &v
		}
		row := tx.QueryRow(ctx, `
INSERT INTO backup_schedules (
    tenant_id, site_id, cadence, kind, enabled, retention_days,
    monthly_archive_keep, next_run_at,
    run_hour, run_minute, day_of_week, day_of_month, frequency_hours, keep_last,
    incremental_enabled, base_window_days
)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
ON CONFLICT (site_id)
DO UPDATE SET
    cadence               = EXCLUDED.cadence,
    kind                  = EXCLUDED.kind,
    enabled               = EXCLUDED.enabled,
    retention_days        = EXCLUDED.retention_days,
    monthly_archive_keep  = EXCLUDED.monthly_archive_keep,
    next_run_at           = EXCLUDED.next_run_at,
    run_hour              = EXCLUDED.run_hour,
    run_minute            = EXCLUDED.run_minute,
    day_of_week           = EXCLUDED.day_of_week,
    day_of_month          = EXCLUDED.day_of_month,
    frequency_hours       = EXCLUDED.frequency_hours,
    keep_last             = EXCLUDED.keep_last,
    incremental_enabled   = EXCLUDED.incremental_enabled,
    base_window_days      = EXCLUDED.base_window_days,
    updated_at            = now()
RETURNING `+scheduleColumnList,
			in.TenantID, in.SiteID,
			in.Cadence, in.Kind, in.Enabled,
			in.RetentionDays, in.MonthlyArchiveKeep, in.NextRunAt,
			int16(in.RunHour), int16(in.RunMinute),
			dow, dom, fh, in.KeepLast,
			in.IncrementalEnabled, in.BaseWindowDays,
		)
		s, serr := scanScheduleRow(row)
		if serr != nil {
			return domain.Internal("backup_schedule_upsert_failed", "failed to save schedule").WithCause(serr)
		}
		out = s
		return nil
	})
	return out, err
}

func (r *pgRepo) ListDueSchedules(ctx context.Context, now time.Time, limit int32) ([]Schedule, error) {
	var out []Schedule
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		pgRows, err := tx.Query(ctx,
			scheduleSelectColumnsQualified+` FROM backup_schedules
			  `+scheduleSiteJoin+`
			  WHERE backup_schedules.enabled = true AND backup_schedules.next_run_at <= $1
			  `+scheduleSiteStateGuard+`
			  ORDER BY backup_schedules.next_run_at ASC
			  LIMIT $2`,
			now, limit,
		)
		if err != nil {
			return domain.Internal("backup_schedule_due_failed", "failed to list due schedules").WithCause(err)
		}
		defer pgRows.Close()
		out = make([]Schedule, 0)
		for pgRows.Next() {
			s, serr := scanScheduleRow(pgRows)
			if serr != nil {
				return domain.Internal("backup_schedule_due_scan_failed", "failed to scan due schedule").WithCause(serr)
			}
			out = append(out, s)
		}
		return pgRows.Err()
	})
	return out, err
}

func (r *pgRepo) ListTenantsForGC(ctx context.Context) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListTenantsForBackupGC(ctx)
		if err != nil {
			return domain.Internal("backup_gc_tenants_failed", "failed to list tenants for GC").WithCause(err)
		}
		out = rows
		return nil
	})
	return out, err
}

func (r *pgRepo) AdvanceScheduleRun(ctx context.Context, tenantID, scheduleID uuid.UUID, next time.Time) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := sqlc.New(tx).AdvanceBackupScheduleRun(ctx, sqlc.AdvanceBackupScheduleRunParams{ID: scheduleID, TenantID: tenantID, NextRunAt: next})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("backup_schedule_not_found", "backup schedule not found")
			}
			return domain.Internal("backup_schedule_advance_failed", "failed to advance schedule").WithCause(err)
		}
		return nil
	})
}

func (r *pgRepo) GetBackupSettings(ctx context.Context, tenantID, siteID uuid.UUID) (SiteBackupSettings, error) {
	var out SiteBackupSettings
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT tenant_id, site_id, backup_components, include_core,
			        exclude_paths, exclude_extensions, exclude_file_size_mb,
			        notify_on_completion, notify_recipients, created_at, updated_at
			   FROM site_backup_settings
			  WHERE tenant_id = $1 AND site_id = $2`,
			tenantID, siteID,
		)
		s, serr := scanBackupSettingsRow(row)
		if serr != nil {
			if errors.Is(serr, pgx.ErrNoRows) {
				return domain.NotFound("backup_settings_not_found", "backup settings not found")
			}
			return domain.Internal("backup_settings_get_failed", "failed to load backup settings").WithCause(serr)
		}
		out = s
		return nil
	})
	return out, err
}

func (r *pgRepo) UpsertBackupSettings(ctx context.Context, tenantID uuid.UUID, in SiteBackupSettings) (SiteBackupSettings, error) {
	var out SiteBackupSettings
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		// Nullable JSONB slices: nil → NULL on write (the CHECK constraint on
		// exclude_file_size_mb is CHECK (> 0), so 0 maps to NULL).
		var excludeSizeMb *int32
		if in.ExcludeFileSizeMB > 0 {
			v := in.ExcludeFileSizeMB
			excludeSizeMb = &v
		}
		// notify_recipients has a NOT NULL DEFAULT '[]' constraint so we normalise
		// nil to empty slice.
		notifyRecip := in.NotifyRecipients
		if notifyRecip == nil {
			notifyRecip = []string{}
		}
		row := tx.QueryRow(ctx, `
INSERT INTO site_backup_settings
  (tenant_id, site_id, backup_components, include_core,
   exclude_paths, exclude_extensions, exclude_file_size_mb,
   notify_on_completion, notify_recipients, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,now())
ON CONFLICT (site_id) DO UPDATE SET
  backup_components    = EXCLUDED.backup_components,
  include_core         = EXCLUDED.include_core,
  exclude_paths        = EXCLUDED.exclude_paths,
  exclude_extensions   = EXCLUDED.exclude_extensions,
  exclude_file_size_mb = EXCLUDED.exclude_file_size_mb,
  notify_on_completion = EXCLUDED.notify_on_completion,
  notify_recipients    = EXCLUDED.notify_recipients,
  updated_at           = now()
RETURNING tenant_id, site_id, backup_components, include_core,
          exclude_paths, exclude_extensions, exclude_file_size_mb,
          notify_on_completion, notify_recipients, created_at, updated_at`,
			tenantID,
			in.SiteID,
			in.BackupComponents,
			in.IncludeCore,
			in.ExcludePaths,
			in.ExcludeExtensions,
			excludeSizeMb,
			in.NotifyOnCompletion,
			notifyRecip,
		)
		s, serr := scanBackupSettingsRow(row)
		if serr != nil {
			return domain.Internal("backup_settings_upsert_failed", "failed to save backup settings").WithCause(serr)
		}
		out = s
		return nil
	})
	return out, err
}

// scanBackupSettingsRow scans a site_backup_settings row. Nullable JSONB arrays
// decode as []string (NULL → nil; callers normalise to []string{} as needed).
// exclude_file_size_mb: NULL → 0 in Go (0 = no filter).
func scanBackupSettingsRow(row rowScanner) (SiteBackupSettings, error) {
	var (
		out           SiteBackupSettings
		excludeSizeMb *int32
	)
	err := row.Scan(
		&out.TenantID,
		&out.SiteID,
		&out.BackupComponents,
		&out.IncludeCore,
		&out.ExcludePaths,
		&out.ExcludeExtensions,
		&excludeSizeMb,
		&out.NotifyOnCompletion,
		&out.NotifyRecipients,
		&out.CreatedAt,
		&out.UpdatedAt,
	)
	if err != nil {
		return SiteBackupSettings{}, err
	}
	if excludeSizeMb != nil {
		out.ExcludeFileSizeMB = *excludeSizeMb
	}
	return out, nil
}

func (r *pgRepo) ListExpiredSnapshots(ctx context.Context, tenantID uuid.UUID, before time.Time) ([]Snapshot, error) {
	var out []Snapshot
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListExpiredBackupSnapshots(ctx, sqlc.ListExpiredBackupSnapshotsParams{TenantID: tenantID, CreatedAt: before})
		if err != nil {
			return domain.Internal("backup_expired_list_failed", "failed to list expired snapshots").WithCause(err)
		}
		out = make([]Snapshot, 0, len(rows))
		for _, row := range rows {
			out = append(out, toSnapshot(row))
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) ListCompletedSnapshotsForSite(ctx context.Context, tenantID, siteID uuid.UUID) ([]SnapshotMeta, error) {
	var out []SnapshotMeta
	// Raw SQL (not sqlc) because ADR-050 widened the projection to carry the
	// chain columns; regenerating sqlc is out of scope for this migration (same
	// m44/m46 raw-query precedent).
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, created_at, archived, chain_id, generation, is_incremental, locked
			   FROM backup_snapshots
			  WHERE tenant_id = $1 AND site_id = $2 AND status = 'completed'
			  ORDER BY created_at DESC`,
			tenantID, siteID,
		)
		if err != nil {
			return domain.Internal("backup_completed_list_failed", "failed to list completed snapshots").WithCause(err)
		}
		defer rows.Close()
		out = make([]SnapshotMeta, 0)
		for rows.Next() {
			var m SnapshotMeta
			var chainID pgtype.UUID
			if serr := rows.Scan(&m.ID, &m.CreatedAt, &m.Archived, &chainID, &m.Generation, &m.IsIncremental, &m.Locked); serr != nil {
				return domain.Internal("backup_completed_scan_failed", "failed to scan completed snapshot row").WithCause(serr)
			}
			if chainID.Valid {
				id := uuid.UUID(chainID.Bytes)
				m.ChainID = &id
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

func (r *pgRepo) ListSiteIDsWithSnapshots(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListBackupSiteIDsForTenant(ctx, tenantID)
		if err != nil {
			return domain.Internal("backup_site_ids_failed", "failed to list backup site ids").WithCause(err)
		}
		out = rows
		return nil
	})
	return out, err
}

func (r *pgRepo) SetSnapshotArchived(ctx context.Context, tenantID, snapshotID uuid.UUID, archived bool) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		if err := sqlc.New(tx).SetBackupSnapshotArchived(ctx, sqlc.SetBackupSnapshotArchivedParams{ID: snapshotID, TenantID: tenantID, Archived: archived}); err != nil {
			return domain.Internal("backup_snapshot_archive_failed", "failed to set snapshot archived").WithCause(err)
		}
		return nil
	})
}

// DeleteSnapshot removes a snapshot metadata-only (ADR-050). Manifest entries
// and file-index rows cascade via their FK ON DELETE CASCADE. It deliberately
// does NOT decref chunks and does NOT touch object storage: post-ADR-050 the
// only authority over chunk liveness is the mark-and-sweep pass.
func (r *pgRepo) DeleteSnapshot(ctx context.Context, tenantID, snapshotID uuid.UUID) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := sqlc.New(tx).DeleteBackupSnapshot(ctx, sqlc.DeleteBackupSnapshotParams{ID: snapshotID, TenantID: tenantID}); err != nil {
			return domain.Internal("backup_snapshot_delete_failed", "failed to delete snapshot").WithCause(err)
		}
		return nil
	})
}

// GetSnapshotsByIDs resolves ids in ONE query. See the Repo interface doc.
func (r *pgRepo) GetSnapshotsByIDs(ctx context.Context, tenantID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]Snapshot, error) {
	out := make(map[uuid.UUID]Snapshot, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			snapshotSelectColumns+` FROM backup_snapshots WHERE tenant_id=$1 AND id = ANY($2)`,
			tenantID, ids,
		)
		if err != nil {
			return domain.Internal("backup_snapshot_list_failed", "failed to load snapshots").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			s, serr := scanSnapshotWithChainFields(rows)
			if serr != nil {
				return domain.Internal("backup_snapshot_list_scan_failed", "failed to scan snapshot row").WithCause(serr)
			}
			out[s.ID] = s
		}
		return rows.Err()
	})
	return out, err
}

// HasActiveRestore reports which of the supplied restore-group keys have an
// active restore in flight. See the Repo interface doc for the grouping
// semantics. restore_runs does not carry chain_id directly, so this joins
// backup_snapshots to resolve COALESCE(chain_id, id) — a chained snapshot's
// grouping key is its chain_id (shared by every generation, including the
// base, whose own id IS the chain_id); a standalone snapshot's grouping key is
// its own id.
func (r *pgRepo) HasActiveRestore(ctx context.Context, tenantID uuid.UUID, groupKeys []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := make(map[uuid.UUID]bool, len(groupKeys))
	if len(groupKeys) == 0 {
		return out, nil
	}
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT COALESCE(bs.chain_id, bs.id) AS grp
			FROM restore_runs rr
			JOIN backup_snapshots bs ON bs.id = rr.snapshot_id AND bs.tenant_id = rr.tenant_id
			WHERE rr.tenant_id = $1
			  AND rr.status IN ('queued', 'running')
			  AND COALESCE(bs.chain_id, bs.id) = ANY($2)`,
			tenantID, groupKeys,
		)
		if err != nil {
			return domain.Internal("backup_active_restore_check_failed", "failed to check active restores").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			var grp uuid.UUID
			if serr := rows.Scan(&grp); serr != nil {
				return domain.Internal("backup_active_restore_scan_failed", "failed to scan active restore row").WithCause(serr)
			}
			out[grp] = true
		}
		return rows.Err()
	})
	return out, err
}

func (r *pgRepo) ListInFlightSnapshotFloor(ctx context.Context, tenantID uuid.UUID) (time.Time, error) {
	var out time.Time
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		var floor pgtype.Timestamptz
		row := tx.QueryRow(ctx,
			`SELECT min(created_at)::timestamptz
			   FROM backup_snapshots
			  WHERE tenant_id = $1 AND status IN ('pending','running')`,
			tenantID,
		)
		if serr := row.Scan(&floor); serr != nil {
			return domain.Internal("backup_inflight_floor_failed", "failed to read in-flight snapshot floor").WithCause(serr)
		}
		if floor.Valid {
			out = floor.Time
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) DBNow(ctx context.Context, tenantID uuid.UUID) (time.Time, error) {
	var out time.Time
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		if serr := tx.QueryRow(ctx, `SELECT now()`).Scan(&out); serr != nil {
			return domain.Internal("backup_db_now_failed", "failed to read database clock").WithCause(serr)
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) SweepTenantChunks(ctx context.Context, tenantID uuid.UUID, floor time.Time, acquired *bool, del func(c SweepChunk, stillReferenced func() (bool, error)) (bool, error)) (err error) {
	*acquired = false

	// 1. PIN ONE pooled connection for the WHOLE sweep pass. The per-tenant GC
	//    advisory lock is SESSION-scoped, so it only stays held while every later
	//    statement runs on the SAME backing session. Taking it inside a pooled tx
	//    that commits would return the connection to the pool and silently drop
	//    the lock — then a second concurrent same-tenant pass could ALSO acquire
	//    it and both would sweep. Acquire + Release bookends the session; the
	//    short per-page / per-chunk txns below all run on this one conn.
	conn, aerr := r.pool.Acquire(ctx)
	if aerr != nil {
		return domain.Internal("backup_gc_conn_failed", "failed to acquire GC connection").WithCause(aerr)
	}
	defer conn.Release()

	// 2. Take the SESSION-level per-tenant GC advisory lock ON THE PINNED CONN.
	//    Two-int form: (hashtext('backup_gc'), hashtext(tenant)) — namespaced so
	//    it collides only with another GC sweep for the SAME tenant. Because it is
	//    held on this one conn for the whole pass, a concurrent same-tenant pass
	//    fails pg_try_advisory_lock and skips.
	var got bool
	if lerr := conn.QueryRow(ctx,
		`SELECT pg_try_advisory_lock(hashtext('backup_gc'), hashtext($1))`,
		tenantID.String(),
	).Scan(&got); lerr != nil {
		return domain.Internal("backup_gc_lock_failed", "failed to take GC advisory lock").WithCause(lerr)
	}
	if !got {
		return nil // another sweep holds it; *acquired stays false.
	}
	*acquired = true
	// Always release the session lock on the SAME conn, on every path (incl.
	// error), BEFORE Release returns the conn to the pool.
	//
	// GH #483: on db.CleanupContext, never on ctx, and NOT best-effort in the
	// sense this comment used to claim. "A failed unlock is harmless, the lock
	// drops when the session closes" is false for a POOLED connection: the
	// session does not close, it goes back to the pool. Worse, the unlock on a
	// cancelled ctx never reaches the wire at all — pgx returns early — so the
	// connection returns healthy and still holding this tenant's GC lock, and
	// every later sweep for the tenant takes a different connection, fails
	// pg_try_advisory_lock and returns with *acquired false. GC for that tenant
	// silently stops until MaxConnLifetime (30 min) closes the connection.
	defer func() {
		cctx, ccancel := db.CleanupContext(ctx)
		defer ccancel()
		_, _ = conn.Exec(cctx,
			`SELECT pg_advisory_unlock(hashtext('backup_gc'), hashtext($1))`,
			tenantID.String(),
		)
	}()

	// 3. Stream every chunk, keyset-paged by (created_at, blake3). Each page read
	//    AND each per-chunk delete is its OWN short transaction ON THE PINNED CONN
	//    (conn.Begin) so no long tx pins idle across object-store I/O (avoiding
	//    Cloud SQL's idle_in_transaction_session_timeout) while the session lock
	//    stays held the whole pass. Every such tx replicates InTenantTx's RLS
	//    setup (SET LOCAL app.tenant_id) so RLS still scopes the reads/deletes.
	const pageSize = 5000
	var (
		haveCursor bool
		curTime    time.Time
		curHash    string
	)
	for {
		// 3a. SHORT read tx on the pinned conn: fetch one page of candidates.
		var batch []SweepChunk
		readErr := r.inTenantTxOnConn(ctx, conn, tenantID, func(tx pgx.Tx) error {
			var (
				rows pgx.Rows
				qerr error
			)
			if !haveCursor {
				rows, qerr = tx.Query(ctx,
					`SELECT blake3, s3_key, created_at, last_referenced_at
					   FROM backup_chunks
					  WHERE tenant_id = $1
					  ORDER BY created_at ASC, blake3 ASC
					  LIMIT $2`,
					tenantID, pageSize,
				)
			} else {
				rows, qerr = tx.Query(ctx,
					`SELECT blake3, s3_key, created_at, last_referenced_at
					   FROM backup_chunks
					  WHERE tenant_id = $1
					    AND (created_at, blake3) > ($2, $3)
					  ORDER BY created_at ASC, blake3 ASC
					  LIMIT $4`,
					tenantID, curTime, curHash, pageSize,
				)
			}
			if qerr != nil {
				return domain.Internal("backup_sweep_list_failed", "failed to list chunks for sweep").WithCause(qerr)
			}
			defer rows.Close()
			for rows.Next() {
				var c SweepChunk
				if serr := rows.Scan(&c.Blake3, &c.S3Key, &c.CreatedAt, &c.LastReferencedAt); serr != nil {
					return domain.Internal("backup_sweep_scan_failed", "failed to scan sweep chunk row").WithCause(serr)
				}
				batch = append(batch, c)
			}
			if rerr := rows.Err(); rerr != nil {
				return domain.Internal("backup_sweep_iter_failed", "failed iterating sweep chunks").WithCause(rerr)
			}
			return nil
		})
		if readErr != nil {
			return readErr
		}

		// 3b. Per-candidate: a SHORT per-chunk tx on the pinned conn that holds a
		//     row-level FOR UPDATE lock ACROSS the object delete. This serializes the
		//     object delete against a concurrent dedup touch (ExistingChunkHashes'
		//     UPDATE ... last_referenced_at=now()): once we lock the row, that UPDATE
		//     BLOCKS until we commit, and we re-check the floor under the lock so a
		//     touch that won the race makes us skip (no object delete). Because chunk
		//     keys are content-addressed this serialization is REQUIRED — without the
		//     held lock a touch->re-upload could re-PUT the same key the sweep then
		//     deletes.
		for _, c := range batch {
			if serr := r.sweepOneChunk(ctx, conn, tenantID, c, floor, del); serr != nil {
				return serr
			}
		}

		if len(batch) < pageSize {
			return nil
		}
		last := batch[len(batch)-1]
		curTime, curHash, haveCursor = last.CreatedAt, last.Blake3, true
	}
}

// sweepOneChunk runs the FIX-A per-chunk critical section for one sweep candidate
// inside a SHORT transaction on the pinned conn:
//
//  1. SELECT ... FOR UPDATE locks the row (a concurrent ExistingChunkHashes dedup
//     touch on this chunk now BLOCKS until this tx commits) and re-reads the
//     FRESH created_at / last_referenced_at.
//  2. Re-check GREATEST(created_at, last_referenced_at) < floor UNDER the lock.
//     If a touch won the race the boundary is now >= floor -> skip (no delete).
//  3. del(freshChunk) consults the live set + floor and, when still deletable,
//     deletes the OBJECT while we STILL HOLD the lock (idempotent; 404 == ok).
//  4. DELETE the row (object-FIRST/row-SECOND), then COMMIT releases the lock.
//
// Object-first/row-second within the locked tx preserves crash self-heal: a
// crash after the object delete but before COMMIT rolls the tx back, leaving the
// row present with its object gone — the dangling-row case the next sweep heals
// idempotently. A missing row (already swept by a prior partial pass) is a no-op.
func (r *pgRepo) sweepOneChunk(ctx context.Context, conn sweepConn, tenantID uuid.UUID, c SweepChunk, floor time.Time, del func(SweepChunk, func() (bool, error)) (bool, error)) error {
	return r.inTenantTxOnConn(ctx, conn, tenantID, func(tx pgx.Tx) error {
		// 1. Lock the row and re-read the fresh liveness boundary.
		fresh := SweepChunk{Blake3: c.Blake3}
		row := tx.QueryRow(ctx,
			`SELECT s3_key, created_at, last_referenced_at
			   FROM backup_chunks
			  WHERE tenant_id = $1 AND blake3 = $2
			  FOR UPDATE`,
			tenantID, c.Blake3,
		)
		if serr := row.Scan(&fresh.S3Key, &fresh.CreatedAt, &fresh.LastReferencedAt); serr != nil {
			if errors.Is(serr, pgx.ErrNoRows) {
				return nil // row already gone — nothing to do.
			}
			return domain.Internal("backup_sweep_lock_failed", "failed to lock sweep chunk row").WithCause(serr)
		}

		// 2. Re-check the grace floor under the lock. A dedup touch that committed
		//    before we took the lock raised last_referenced_at to ~now, so the
		//    boundary is now >= floor and we MUST keep the chunk.
		boundary := fresh.CreatedAt
		if fresh.LastReferencedAt.After(boundary) {
			boundary = fresh.LastReferencedAt
		}
		if !boundary.Before(floor) {
			return nil // a touch won the race — keep object + row.
		}

		// 3. del consults the live set + floor on the FRESH projection and, when the
		//    chunk is still deletable, deletes the OBJECT while we hold the lock.
		//    stillReferenced (GH #168 P2) is a LAZY closure bound to THIS pinned tx
		//    — del calls it only if it needs to (the liveSet check is cheap and
		//    usually short-circuits first), and when called it runs the EXISTS
		//    query on the SAME connection/tx already holding the FOR UPDATE lock,
		//    never acquiring a second pooled connection.
		stillReferenced := func() (bool, error) {
			return chunkStillReferencedOnTx(ctx, tx, tenantID, c.Blake3)
		}
		remove, ferr := del(fresh, stillReferenced)
		if ferr != nil {
			return ferr
		}
		if !remove {
			return nil // del decided to keep (live, still-referenced, or re-checked floor).
		}

		// 4. Row-SECOND: delete the row, still under the held lock and re-checking
		//    GREATEST(...) < floor at the DB (defense-in-depth).
		return r.deleteSweptChunkOnTx(ctx, tx, tenantID, c.Blake3, floor)
	})
}

// chunkStillReferencedOnTx is the GH #168 P2 ground-truth guard's query,
// runnable on ANY transaction (the pinned sweep tx, via sweepOneChunk's lazy
// closure). It checks backup_manifest_entries ONLY — deliberately NOT
// backup_file_index. backup_manifest_entries rows are per-generation, and for
// every model that writes them (ADR-051 archive-delta parts/files-list/
// tombstones, and legacy full/DB-dump manifests) every row belonging to a
// still-retained (non-metadata-pruned) snapshot is, by construction, part of
// what a retained generation needs — there is no "superseded-but-still-
// present" row in that table. backup_file_index is different: it is an
// ADR-048 per-file HISTORY table, and a still-retained (e.g. carry-forward-
// pinned) generation's rows can legitimately include paths that were later
// SUPERSEDED by a newer generation's version of the same path — those rows'
// chunk_hashes are genuinely dead even though the row itself persists. Treating
// "any backup_file_index row mentions this hash" as ground truth would make
// GC permanently retain every historical version of every file for as long as
// its owning generation survives, defeating retention for the legacy model
// entirely — the opposite failure mode from #168 (leak, not loss). Uses the
// array-containment operator (`@>`) rather than `= ANY(...)` so the m96 GIN
// index on chunk_hashes can serve the lookup instead of a sequential scan; the
// tenant_id index narrows the row set either way. The caller is responsible
// for tenant scoping (the query includes an explicit tenant_id filter, but —
// unlike the rest of this package — this does NOT run inside InTenantTx/RLS,
// since it executes on a caller-supplied tx that is already tenant-scoped).
func chunkStillReferencedOnTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, blake3 string) (bool, error) {
	var referenced bool
	row := tx.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM backup_manifest_entries
		    WHERE tenant_id = $1 AND chunk_hashes @> ARRAY[$2]::text[]
		 )`,
		tenantID, blake3,
	)
	if err := row.Scan(&referenced); err != nil {
		return false, domain.Internal("backup_chunk_still_referenced_failed", "failed to check whether a chunk is still manifest-referenced").WithCause(err)
	}
	return referenced, nil
}

// sweepConn is the minimal pinned-connection surface the sweep needs: just the
// ability to begin a transaction. *pgxpool.Conn satisfies it.
type sweepConn interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// inTenantTxOnConn runs fn inside a transaction begun ON THE PINNED CONN, with
// app.tenant_id set for the lifetime of the tx (SET LOCAL) — mirroring
// db.Pool.InTenantTx's RLS setup exactly, but WITHOUT returning the connection to
// the pool, so the surrounding session advisory lock stays held. The tx commits
// when fn returns nil and rolls back otherwise.
func (r *pgRepo) inTenantTxOnConn(ctx context.Context, conn sweepConn, tenantID uuid.UUID, fn func(tx pgx.Tx) error) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return domain.Internal("backup_sweep_tx_begin_failed", "failed to begin sweep tx").WithCause(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, serr := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID.String()); serr != nil {
		return domain.Internal("backup_sweep_set_tenant_failed", "failed to set app.tenant_id for sweep tx").WithCause(serr)
	}
	if ferr := fn(tx); ferr != nil {
		return ferr
	}
	if cerr := tx.Commit(ctx); cerr != nil {
		return domain.Internal("backup_sweep_tx_commit_failed", "failed to commit sweep tx").WithCause(cerr)
	}
	return nil
}

// deleteSweptChunkOnTx removes a chunk row by hash, re-checking the grace-floor
// predicate at the DB (defense-in-depth: a chunk re-referenced after the sweep
// read it has a fresh last_referenced_at and so fails GREATEST(...) < floor).
// Runs on the supplied SHORT delete transaction. The caller deletes the object
// FIRST (idempotent), then this.
func (r *pgRepo) deleteSweptChunkOnTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, blake3 string, floor time.Time) error {
	if _, err := tx.Exec(ctx,
		`DELETE FROM backup_chunks
		  WHERE tenant_id = $1 AND blake3 = $2
		    AND GREATEST(created_at, last_referenced_at) < $3`,
		tenantID, blake3, floor,
	); err != nil {
		return domain.Internal("backup_swept_chunk_delete_failed", "failed to delete swept chunk row").WithCause(err)
	}
	return nil
}

// touchReferencedChunks bumps last_referenced_at = now() for the given chunk
// hashes inside the supplied completion transaction (ADR-050 belt). It is a
// no-op for an empty set. Sharing the tx means a completed snapshot's chunks are
// stamped fresh atomically with its completion, so the sweep's
// GREATEST(created_at, last_referenced_at) < floor predicate keeps them.
func touchReferencedChunks(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, referenced map[string]struct{}) error {
	if len(referenced) == 0 {
		return nil
	}
	hashes := make([]string, 0, len(referenced))
	for h := range referenced {
		hashes = append(hashes, h)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE backup_chunks
		    SET last_referenced_at = now(), updated_at = now()
		  WHERE tenant_id = $1 AND blake3 = ANY($2::text[])`,
		tenantID, hashes,
	); err != nil {
		return domain.Internal("backup_chunk_touch_failed", "failed to refresh chunk last_referenced_at").WithCause(err)
	}
	return nil
}

// CompleteIncrementalManifest folds the file-index insert, optional DB-manifest
// recording, and snapshot completion into ONE transaction (ADR-050 STEP 2).
func (r *pgRepo) CompleteIncrementalManifest(ctx context.Context, in CompleteIncrementalInput) (int64, int64, error) {
	var chunkRefs, storedCount int64
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)

		// 1. Insert backup_file_index rows (changed files + tombstones).
		for _, e := range in.FileEntries {
			hashes := e.ChunkHashes
			if hashes == nil {
				hashes = []string{}
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO backup_file_index
				   (tenant_id, snapshot_id, file_path, file_size, file_mtime,
				    file_blake3, chunk_hashes, is_tombstone)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
				 ON CONFLICT (snapshot_id, file_path) DO UPDATE
				   SET file_size    = EXCLUDED.file_size,
				       file_mtime   = EXCLUDED.file_mtime,
				       file_blake3  = EXCLUDED.file_blake3,
				       chunk_hashes = EXCLUDED.chunk_hashes,
				       is_tombstone = EXCLUDED.is_tombstone`,
				in.TenantID, in.SnapshotID, e.FilePath, e.FileSize, e.FileMtime,
				e.FileBlake3, hashes, e.IsTombstone,
			); err != nil {
				return domain.Internal("backup_file_index_insert_failed", "failed to insert file index entry").WithCause(err)
			}
		}

		// 2. DB-inline path: record the DB-dump manifest (chunk upsert + refcount +
		//    manifest insert + completion) inside this same tx. Mirrors
		//    RecordManifest's body so completion stays atomic with the file-index
		//    rows above.
		if in.DBManifest != nil {
			var totalSize int64
			referenced := map[string]struct{}{}
			for hash, up := range in.DBManifest.Chunks {
				_, getErr := q.GetBackupChunk(ctx, sqlc.GetBackupChunkParams{TenantID: in.TenantID, Blake3: hash})
				existed := getErr == nil
				if getErr != nil && !errors.Is(getErr, pgx.ErrNoRows) {
					return domain.Internal("backup_chunk_get_failed", "failed to check chunk existence").WithCause(getErr)
				}
				if _, err := q.UpsertBackupChunk(ctx, sqlc.UpsertBackupChunkParams{
					TenantID: in.TenantID, Blake3: hash, S3Key: up.S3Key, Size: up.Size,
				}); err != nil {
					return domain.Internal("backup_chunk_upsert_failed", "failed to upsert chunk").WithCause(err)
				}
				if !existed {
					storedCount++
				}
			}
			for _, e := range in.DBManifest.Entries {
				if _, err := q.CreateManifestEntry(ctx, sqlc.CreateManifestEntryParams{
					SnapshotID:  in.SnapshotID,
					TenantID:    in.TenantID,
					Path:        e.Path,
					EntryKind:   e.EntryKind,
					TableName:   e.TableName,
					ChunkHashes: e.ChunkHashes,
					Size:        e.Size,
					Mode:        e.Mode,
				}); err != nil {
					return domain.Internal("backup_manifest_insert_failed", "failed to insert manifest entry").WithCause(err)
				}
				totalSize += e.Size
				for _, h := range e.ChunkHashes {
					if _, err := q.IncrementChunkRefcount(ctx, sqlc.IncrementChunkRefcountParams{TenantID: in.TenantID, Blake3: h}); err != nil {
						return domain.Internal("backup_chunk_incref_failed", "failed to increment chunk refcount").WithCause(err)
					}
					referenced[h] = struct{}{}
					chunkRefs++
				}
			}
			// ADR-050 belt: refresh last_referenced_at for the DB-dump's chunks at
			// completion (see RecordManifest). The carry-forward file chunks are kept
			// fresh by the presign-time touch in ExistingChunkHashes.
			if terr := touchReferencedChunks(ctx, tx, in.TenantID, referenced); terr != nil {
				return terr
			}
			if _, err := q.CompleteBackupSnapshot(ctx, sqlc.CompleteBackupSnapshotParams{
				ID: in.SnapshotID, TenantID: in.TenantID, TotalSize: totalSize, ChunkCount: chunkRefs,
			}); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return domain.NotFound("backup_snapshot_not_found", "backup snapshot not found")
				}
				return domain.Internal("backup_snapshot_complete_failed", "failed to complete snapshot").WithCause(err)
			}
			return nil
		}

		// 3. Files-only path: complete the snapshot directly with caller-supplied
		//    counters.
		if _, err := q.CompleteBackupSnapshot(ctx, sqlc.CompleteBackupSnapshotParams{
			ID: in.SnapshotID, TenantID: in.TenantID, TotalSize: in.TotalSize, ChunkCount: in.ChunkRefs,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("backup_snapshot_not_found", "backup snapshot not found")
			}
			return domain.Internal("backup_snapshot_complete_failed", "failed to complete snapshot").WithCause(err)
		}
		chunkRefs = in.ChunkRefs
		return nil
	})
	return chunkRefs, storedCount, err
}

func toSnapshot(s sqlc.BackupSnapshot) Snapshot {
	out := Snapshot{
		ID:           s.ID,
		TenantID:     s.TenantID,
		SiteID:       s.SiteID,
		Kind:         s.Kind,
		Status:       s.Status,
		AgeRecipient: s.AgeRecipient,
		TotalSize:    s.TotalSize,
		ChunkCount:   s.ChunkCount,
		Error:        s.Error,
		Archived:     s.Archived,
		CreatedAt:    s.CreatedAt,
		UpdatedAt:    s.UpdatedAt,
	}
	if s.CreatedBy.Valid {
		id := uuid.UUID(s.CreatedBy.Bytes)
		out.CreatedBy = &id
	}
	if s.DestinationID.Valid {
		out.DestinationID = uuid.UUID(s.DestinationID.Bytes)
	}
	if s.StartedAt.Valid {
		t := s.StartedAt.Time
		out.StartedAt = &t
	}
	if s.FinishedAt.Valid {
		t := s.FinishedAt.Time
		out.FinishedAt = &t
	}
	out.Progress = s.Progress
	if s.ProgressUpdatedAt.Valid {
		t := s.ProgressUpdatedAt.Time
		out.ProgressUpdatedAt = &t
	}
	if s.StalledAt.Valid {
		t := s.StalledAt.Time
		out.StalledAt = &t
	}
	return out
}

func toManifestEntry(m sqlc.BackupManifestEntry) ManifestEntry {
	hashes := m.ChunkHashes
	if hashes == nil {
		hashes = []string{}
	}
	return ManifestEntry{
		ID:          m.ID,
		SnapshotID:  m.SnapshotID,
		TenantID:    m.TenantID,
		Path:        m.Path,
		EntryKind:   m.EntryKind,
		TableName:   m.TableName,
		ChunkHashes: hashes,
		Size:        m.Size,
		Mode:        m.Mode,
		CreatedAt:   m.CreatedAt,
	}
}

func toChunk(c sqlc.BackupChunk) Chunk {
	return Chunk{
		ID:        c.ID,
		TenantID:  c.TenantID,
		Blake3:    c.Blake3,
		S3Key:     c.S3Key,
		Size:      c.Size,
		Refcount:  c.Refcount,
		CreatedAt: c.CreatedAt,
		UpdatedAt: c.UpdatedAt,
	}
}

func toSchedule(s sqlc.BackupSchedule) Schedule {
	out := Schedule{
		ID:                 s.ID,
		TenantID:           s.TenantID,
		SiteID:             s.SiteID,
		Cadence:            s.Cadence,
		Kind:               s.Kind,
		Enabled:            s.Enabled,
		RetentionDays:      s.RetentionDays,
		MonthlyArchiveKeep: s.MonthlyArchiveKeep,
		RunHour:            int32(s.RunHour),
		RunMinute:          int32(s.RunMinute),
		KeepLast:           s.KeepLast,
		IncrementalEnabled: s.IncrementalEnabled,
		BaseWindowDays:     s.BaseWindowDays,
		NextRunAt:          s.NextRunAt,
		CreatedAt:          s.CreatedAt,
		UpdatedAt:          s.UpdatedAt,
	}
	if s.DayOfWeek != nil {
		v := int32(*s.DayOfWeek)
		out.DayOfWeek = &v
	}
	if s.DayOfMonth != nil {
		v := int32(*s.DayOfMonth)
		out.DayOfMonth = &v
	}
	if s.FrequencyHours != nil {
		v := int32(*s.FrequencyHours)
		out.FrequencyHours = &v
	}
	if s.LastRunAt.Valid {
		t := s.LastRunAt.Time
		out.LastRunAt = &t
	}
	return out
}

// scheduleColumnList is the ordered column list for backup_schedules after m50.
// Track-A and Track-B columns were moved to site_backup_settings in m50; they
// are no longer present on this table. Raw reads and RETURNING clauses use this
// constant paired with scanScheduleRow.
const scheduleColumnList = `id, tenant_id, site_id, cadence, kind, enabled, retention_days,
    monthly_archive_keep, run_hour, run_minute, day_of_week, day_of_month,
    frequency_hours, keep_last, incremental_enabled, base_window_days,
    next_run_at, last_run_at, created_at, updated_at`

const scheduleSelectColumns = `SELECT ` + scheduleColumnList

// scheduleColumnListQualified is scheduleColumnList with every column
// qualified by the backup_schedules table name. GH #282: the scheduler
// queries below JOIN sites (and check tenants) to guard against an archived/
// revoked site or a soft-deleted tenant, and several columns in this list
// ("id", "tenant_id") also exist on sites/tenants, so an unqualified SELECT
// against the joined query would raise a Postgres "column reference is
// ambiguous" error. This constant keeps scanScheduleRow's column order intact
// while resolving the ambiguity.
const scheduleColumnListQualified = `backup_schedules.id, backup_schedules.tenant_id, backup_schedules.site_id,
    backup_schedules.cadence, backup_schedules.kind, backup_schedules.enabled, backup_schedules.retention_days,
    backup_schedules.monthly_archive_keep, backup_schedules.run_hour, backup_schedules.run_minute,
    backup_schedules.day_of_week, backup_schedules.day_of_month,
    backup_schedules.frequency_hours, backup_schedules.keep_last, backup_schedules.incremental_enabled,
    backup_schedules.base_window_days,
    backup_schedules.next_run_at, backup_schedules.last_run_at, backup_schedules.created_at, backup_schedules.updated_at`

const scheduleSelectColumnsQualified = `SELECT ` + scheduleColumnListQualified

// scheduleSiteStateGuard is the GH #282 non-destructive site-state guard
// shared by the three scheduler queries (ListDueSchedules,
// ClaimAndAdvanceDueSchedules, HealOverdueSchedules): a schedule is only DUE
// when its site is not deliberately unmanaged (archived or revoked) and its
// tenant has not been soft-deleted (m93). A transiently disconnected/degraded/
// pending-enrollment site keeps firing its schedule, because a failed backup
// on those states is still actionable operator signal; only a deliberate
// archive or revoke should stop the schedule. Requires the caller's FROM
// clause to include "JOIN sites s ON s.id = backup_schedules.site_id AND
// s.tenant_id = backup_schedules.tenant_id".
const scheduleSiteStateGuard = `
    AND s.connection_state NOT IN ('archived', 'revoked')
    AND NOT EXISTS (
        SELECT 1 FROM tenants t
         WHERE t.id = backup_schedules.tenant_id AND t.deleted_at IS NOT NULL
    )`

const scheduleSiteJoin = `JOIN sites s ON s.id = backup_schedules.site_id AND s.tenant_id = backup_schedules.tenant_id`

// scanScheduleRow scans a row produced by scheduleSelectColumns into a
// Schedule. After m50, Track-A and Track-B fields have moved to
// site_backup_settings and are no longer present on backup_schedules.
func scanScheduleRow(row rowScanner) (Schedule, error) {
	var (
		out       Schedule
		dow       *int16
		dom       *int16
		fh        *int16
		lastRunAt pgtype.Timestamptz
	)
	err := row.Scan(
		&out.ID, &out.TenantID, &out.SiteID, &out.Cadence, &out.Kind,
		&out.Enabled, &out.RetentionDays, &out.MonthlyArchiveKeep,
		&out.RunHour, &out.RunMinute,
		&dow, &dom, &fh, &out.KeepLast,
		&out.IncrementalEnabled, &out.BaseWindowDays,
		&out.NextRunAt, &lastRunAt, &out.CreatedAt, &out.UpdatedAt,
	)
	if err != nil {
		return Schedule{}, err
	}
	if dow != nil {
		v := int32(*dow)
		out.DayOfWeek = &v
	}
	if dom != nil {
		v := int32(*dom)
		out.DayOfMonth = &v
	}
	if fh != nil {
		v := int32(*fh)
		out.FrequencyHours = &v
	}
	if lastRunAt.Valid {
		t := lastRunAt.Time
		out.LastRunAt = &t
	}
	return out, nil
}

// rowScanner is a minimal interface satisfied by pgx.Row and pgx.Rows.Scan.
type rowScanner interface {
	Scan(dest ...any) error
}

// snapshotSelectColumns is the canonical projection for a full backup_snapshots
// row, including the ADR-048/050 chain columns (is_incremental … chain_id,
// generation) and the cycle counter columns. The sqlc-generated SELECT *
// queries expand to the pre-m44 column list and so omit these; raw reads that
// must surface incremental metadata use this constant paired with
// scanSnapshotWithChainFields. The column order MUST match that scan helper's
// Scan() argument order exactly.
const snapshotSelectColumns = `SELECT id, tenant_id, site_id, created_by, kind, status, age_recipient,
        total_size, chunk_count, error, archived, progress, progress_updated_at,
        started_at, finished_at, created_at, updated_at,
        is_incremental, parent_snapshot_id, base_snapshot_id, chain_id, generation,
        cycle_files_scanned, cycle_files_changed, cycle_files_deleted, cycle_bytes_uploaded,
        locked, destination_id, stalled_at`

// scanSnapshotWithChainFields scans a row that includes the ADR-048 chain
// columns (is_incremental … cycle_bytes_uploaded) plus the m49 locked column,
// the M7 / ADR-036 P1 destination_id column, and the m104 / GH #279 stalled_at
// column. The SELECT must project all standard snapshot columns plus the four
// chain UUID columns, the four cycle counter columns, locked, destination_id,
// and stalled_at (appended last) — in the exact order listed here.
func scanSnapshotWithChainFields(row rowScanner) (Snapshot, error) {
	var (
		s               Snapshot
		createdBy       pgtype.UUID
		startedAt       pgtype.Timestamptz
		finishedAt      pgtype.Timestamptz
		progressUpdated pgtype.Timestamptz
		parentID        pgtype.UUID
		baseID          pgtype.UUID
		chainID         pgtype.UUID
		destinationID   pgtype.UUID
		stalledAt       pgtype.Timestamptz
	)
	err := row.Scan(
		&s.ID, &s.TenantID, &s.SiteID, &createdBy, &s.Kind, &s.Status,
		&s.AgeRecipient, &s.TotalSize, &s.ChunkCount, &s.Error, &s.Archived,
		&s.Progress, &progressUpdated, &startedAt, &finishedAt,
		&s.CreatedAt, &s.UpdatedAt,
		&s.IsIncremental, &parentID, &baseID, &chainID, &s.Generation,
		&s.CycleFilesScanned, &s.CycleFilesChanged, &s.CycleFilesDeleted, &s.CycleBytesUploaded,
		&s.Locked, &destinationID, &stalledAt,
	)
	if err != nil {
		return Snapshot{}, err
	}
	if stalledAt.Valid {
		t := stalledAt.Time
		s.StalledAt = &t
	}
	if createdBy.Valid {
		id := uuid.UUID(createdBy.Bytes)
		s.CreatedBy = &id
	}
	if destinationID.Valid {
		s.DestinationID = uuid.UUID(destinationID.Bytes)
	}
	if startedAt.Valid {
		t := startedAt.Time
		s.StartedAt = &t
	}
	if finishedAt.Valid {
		t := finishedAt.Time
		s.FinishedAt = &t
	}
	if progressUpdated.Valid {
		t := progressUpdated.Time
		s.ProgressUpdatedAt = &t
	}
	if parentID.Valid {
		id := uuid.UUID(parentID.Bytes)
		s.ParentSnapshotID = &id
	}
	if baseID.Valid {
		id := uuid.UUID(baseID.Bytes)
		s.BaseSnapshotID = &id
	}
	if chainID.Valid {
		id := uuid.UUID(chainID.Bytes)
		s.ChainID = &id
	}
	return s, nil
}

// ----------------------------------------------------------------------------
// ADR-048 file index repo implementations
// ----------------------------------------------------------------------------

func (r *pgRepo) InsertFileIndexBatch(ctx context.Context, tenantID, snapshotID uuid.UUID, entries []FileIndexEntry) error {
	if len(entries) == 0 {
		return nil
	}
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		for _, e := range entries {
			hashes := e.ChunkHashes
			if hashes == nil {
				hashes = []string{}
			}
			_, err := tx.Exec(ctx,
				`INSERT INTO backup_file_index
				   (tenant_id, snapshot_id, file_path, file_size, file_mtime,
				    file_blake3, chunk_hashes, is_tombstone)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
				 ON CONFLICT (snapshot_id, file_path) DO UPDATE
				   SET file_size    = EXCLUDED.file_size,
				       file_mtime   = EXCLUDED.file_mtime,
				       file_blake3  = EXCLUDED.file_blake3,
				       chunk_hashes = EXCLUDED.chunk_hashes,
				       is_tombstone = EXCLUDED.is_tombstone`,
				tenantID, snapshotID, e.FilePath, e.FileSize, e.FileMtime,
				e.FileBlake3, hashes, e.IsTombstone,
			)
			if err != nil {
				return domain.Internal("backup_file_index_insert_failed", "failed to insert file index entry").WithCause(err)
			}
		}
		return nil
	})
}

func (r *pgRepo) CountFileIndex(ctx context.Context, tenantID, snapshotID uuid.UUID) (int64, error) {
	var count int64
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT count(*) FROM backup_file_index WHERE tenant_id=$1 AND snapshot_id=$2`,
			tenantID, snapshotID,
		)
		return row.Scan(&count)
	})
	if err != nil {
		return 0, domain.Internal("backup_file_index_count_failed", "failed to count file index").WithCause(err)
	}
	return count, nil
}

func (r *pgRepo) StreamFileIndex(ctx context.Context, tenantID, snapshotID uuid.UUID, fn func(FileIndexEntry) error) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, snapshot_id, file_path, file_size, file_mtime,
			        file_blake3, chunk_hashes, is_tombstone, created_at
			   FROM backup_file_index
			  WHERE tenant_id=$1 AND snapshot_id=$2
			  ORDER BY file_path ASC`,
			tenantID, snapshotID,
		)
		if err != nil {
			return domain.Internal("backup_file_index_stream_failed", "failed to stream file index").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			var e FileIndexEntry
			var hashes []string
			if serr := rows.Scan(
				&e.ID, &e.TenantID, &e.SnapshotID, &e.FilePath, &e.FileSize,
				&e.FileMtime, &e.FileBlake3, &hashes, &e.IsTombstone, &e.CreatedAt,
			); serr != nil {
				return domain.Internal("backup_file_index_scan_failed", "failed to scan file index row").WithCause(serr)
			}
			if hashes == nil {
				hashes = []string{}
			}
			e.ChunkHashes = hashes
			if ferr := fn(e); ferr != nil {
				return ferr
			}
		}
		return rows.Err()
	})
}

// StreamChainEffectiveFileIndex builds the merged effective file tree for a
// chain and streams the surviving entries sorted by file_path. The merge is the
// SAME latest-version-wins + tombstone walk that reachableChunks (service.go)
// and planRestoreChain use, so the agent's prev-index equals the restore view.
func (r *pgRepo) StreamChainEffectiveFileIndex(ctx context.Context, tenantID, chainID uuid.UUID, maxGeneration int, fn func(FileIndexEntry) error) error {
	if maxGeneration < 0 {
		maxGeneration = 0
	}
	chainSnaps, err := r.ListChainSnapshots(ctx, tenantID, chainID, maxGeneration)
	if err != nil {
		return err
	}

	// Index by generation so the walk tolerates GC gaps (an older non-pinned
	// generation may already be pruned). Walk ascending so a later generation's
	// entry for the same path OVERWRITES the earlier one (latest-version-wins).
	byGen := map[int]Snapshot{}
	maxGen := -1
	for _, cs := range chainSnaps {
		byGen[cs.Generation] = cs
		if cs.Generation > maxGen {
			maxGen = cs.Generation
		}
	}

	winMap := map[string]*FileIndexEntry{}
	for gen := 0; gen <= maxGen; gen++ {
		cs, ok := byGen[gen]
		if !ok {
			continue
		}
		streamErr := r.StreamFileIndex(ctx, tenantID, cs.ID, func(e FileIndexEntry) error {
			eCopy := e // copy to heap so the pointer is stable
			if e.IsTombstone {
				delete(winMap, e.FilePath)
			} else {
				winMap[e.FilePath] = &eCopy
			}
			return nil
		})
		if streamErr != nil {
			return streamErr
		}
	}

	// Emit sorted by file_path ASC to match the single-snapshot stream contract
	// (the agent's CASE-A matcher and clients may rely on path order). winMap is
	// a Go map (unordered) so an explicit sort is required.
	paths := make([]string, 0, len(winMap))
	for p := range winMap {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if ferr := fn(*winMap[p]); ferr != nil {
			return ferr
		}
	}
	return nil
}

func (r *pgRepo) UpdateSnapshotCycleStats(ctx context.Context, tenantID, snapshotID uuid.UUID, in CycleStatsInput) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE backup_snapshots
			    SET cycle_files_scanned=$3, cycle_files_changed=$4,
			        cycle_files_deleted=$5, cycle_bytes_uploaded=$6,
			        updated_at=now()
			  WHERE id=$1 AND tenant_id=$2`,
			snapshotID, tenantID,
			in.CycleFilesScanned, in.CycleFilesChanged,
			in.CycleFilesDeleted, in.CycleBytesUploaded,
		)
		if err != nil {
			return domain.Internal("backup_snapshot_cycle_stats_failed", "failed to update cycle stats").WithCause(err)
		}
		return nil
	})
}

// ----------------------------------------------------------------------------
// Fleet backup repo methods (operator-scoped, no :siteId)
// ----------------------------------------------------------------------------

// FleetListSnapshots returns a paginated, filtered list of backup snapshots
// across the supplied set of site IDs. Routes through RunTenantTx so a
// site-scoped principal activates InScopedTenantTx and the RESTRICTIVE
// backup_snapshots_site_scope RLS policy acts as the DB-level backstop.
func (r *pgRepo) FleetListSnapshots(ctx context.Context, p db.ScopedPrincipal, tenantID uuid.UUID, f FleetListFilter) (FleetSnapshotPage, error) {
	params := sqlc.FleetListSnapshotsParams{
		TenantID:     tenantID,
		SiteIdsFilter: len(f.SiteIDs) > 0,
		SiteIds:      f.SiteIDs,
		StatusFilter: f.Status != "",
		StatusVal:    f.Status,
		AfterFilter:  f.CreatedAfter != nil,
		BeforeFilter: f.CreatedBefore != nil,
		RowOffset:    f.Offset,
		RowLimit:     f.Limit,
	}
	if f.CreatedAfter != nil {
		params.CreatedAfter = *f.CreatedAfter
	}
	if f.CreatedBefore != nil {
		params.CreatedBefore = *f.CreatedBefore
	}

	countParams := sqlc.FleetListSnapshotsCountParams{
		TenantID:      params.TenantID,
		SiteIdsFilter: params.SiteIdsFilter,
		SiteIds:       params.SiteIds,
		StatusFilter:  params.StatusFilter,
		StatusVal:     params.StatusVal,
		AfterFilter:   params.AfterFilter,
		CreatedAfter:  params.CreatedAfter,
		BeforeFilter:  params.BeforeFilter,
		CreatedBefore: params.CreatedBefore,
	}

	var page FleetSnapshotPage
	err := r.pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		rows, err := q.FleetListSnapshots(ctx, params)
		if err != nil {
			return domain.Internal("fleet_backup_list_failed", "failed to list fleet snapshots").WithCause(err)
		}
		page.Items = make([]Snapshot, 0, len(rows))
		for _, row := range rows {
			page.Items = append(page.Items, fleetSnapshotRowToSnapshot(row))
		}

		total, err := q.FleetListSnapshotsCount(ctx, countParams)
		if err != nil {
			return domain.Internal("fleet_backup_count_failed", "failed to count fleet snapshots").WithCause(err)
		}
		nextOff := int64(f.Offset) + int64(len(rows))
		if nextOff < total {
			page.NextOffset = &nextOff
		}
		return nil
	})
	if err != nil {
		return FleetSnapshotPage{}, err
	}
	return page, nil
}

// FleetBackupHealth returns one FleetBackupHealthItem per requested site with a
// server-derived health classification. Routes through RunTenantTx — site-scoped
// principals activate InScopedTenantTx and the RESTRICTIVE RLS policy.
func (r *pgRepo) FleetBackupHealth(ctx context.Context, p db.ScopedPrincipal, tenantID uuid.UUID, siteIDs []uuid.UUID) ([]FleetBackupHealthItem, error) {
	var out []FleetBackupHealthItem
	err := r.pool.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).FleetBackupHealth(ctx, sqlc.FleetBackupHealthParams{
			TenantID: tenantID,
			SiteIds:  siteIDs,
		})
		if err != nil {
			return domain.Internal("fleet_backup_health_failed", "failed to query fleet backup health").WithCause(err)
		}
		out = make([]FleetBackupHealthItem, 0, len(rows))
		for _, row := range rows {
			item := FleetBackupHealthItem{
				SiteID:          row.SiteID,
				SiteName:        row.SiteName,
				SiteURL:         row.SiteUrl,
				InFlightCount:   row.InFlightCount,
				LatestSizeBytes: toInt64Interface(row.LatestSizeBytes),
				ScheduleCadence: row.ScheduleCadence,
			}
			// Coerce the interface{} timestamps from correlated subqueries.
			item.LastCompletedAt = toTimeInterface(row.LastCompletedAt)
			item.LastFailedAt = toTimeInterface(row.LastFailedAt)
			// Schedule next_run_at.
			if row.NextRunAt.Valid {
				t := row.NextRunAt.Time
				item.NextRunAt = &t
			}
			item.Status = classifyBackupHealth(item)
			out = append(out, item)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// classifyBackupHealth derives the FleetBackupHealthStatus from a
// FleetBackupHealthItem. The completed/failed timestamps and schedule cadence
// have already been resolved from the DB row. Classification priority:
//
//  1. unprotected — no completed backup ever
//  2. failed      — most recent event was a failure (last_failed_at > last_completed_at)
//  3. in_flight   — in-flight backup exists, no recent completion to anchor protection
//  4. stale       — completed exists but older than 2× cadence (or >48h with no schedule)
//  5. protected   — recent completed backup, no newer failure
func classifyBackupHealth(item FleetBackupHealthItem) FleetBackupHealthStatus {
	completedAt := item.LastCompletedAt
	failedAt := item.LastFailedAt

	if completedAt == nil && failedAt == nil {
		// No history at all.
		if item.InFlightCount > 0 {
			return HealthStatusInFlight
		}
		return HealthStatusUnprotected
	}
	if completedAt == nil {
		// Only failures ever occurred.
		if item.InFlightCount > 0 {
			return HealthStatusInFlight
		}
		return HealthStatusFailed
	}
	// A completed backup exists.
	if failedAt != nil && failedAt.After(*completedAt) {
		return HealthStatusFailed
	}
	// Check staleness: completed exists but older than threshold.
	staleness := staleDuration(item.ScheduleCadence)
	if time.Since(*completedAt) > staleness {
		return HealthStatusStale
	}
	return HealthStatusProtected
}

// staleDuration returns the staleness threshold (2× cadence) for a given
// schedule cadence string. Falls back to 48h when the cadence is unset.
func staleDuration(cadence *string) time.Duration {
	if cadence == nil || *cadence == "" {
		return 48 * time.Hour
	}
	switch *cadence {
	case CadenceHourly:
		return 2 * time.Hour
	case CadenceEveryNHours:
		// Without the actual N we use a conservative 48h.
		return 48 * time.Hour
	case CadenceDaily:
		return 48 * time.Hour
	case CadenceWeekly:
		return 14 * 24 * time.Hour
	case CadenceMonthly:
		return 62 * 24 * time.Hour
	}
	return 48 * time.Hour
}

// toTimeInterface coerces an interface{} value from a correlated subquery
// returning timestamptz (or NULL). pgx scans nullable timestamps as
// pgtype.Timestamptz; we extract the Go time.Time when valid.
func toTimeInterface(v interface{}) *time.Time {
	if v == nil {
		return nil
	}
	switch t := v.(type) {
	case pgtype.Timestamptz:
		if !t.Valid {
			return nil
		}
		tt := t.Time
		return &tt
	case time.Time:
		if t.IsZero() {
			return nil
		}
		return &t
	}
	return nil
}

// toInt64Interface coerces an interface{} column value (typically from a
// correlated subquery returning bigint or NULL) to int64. Returns 0 on NULL
// or unrecognised type (mirrors the equivalent helper in perf/repo.go).
func toInt64Interface(v interface{}) int64 {
	if v == nil {
		return 0
	}
	switch t := v.(type) {
	case int64:
		return t
	case int32:
		return int64(t)
	case float64:
		return int64(t)
	default:
		return 0
	}
}

// fleetSnapshotRowToSnapshot converts a sqlc.FleetListSnapshotsRow (the
// columns returned by the fleet list query) to the canonical Snapshot domain
// type. This mirrors toSnapshot but operates on the fleet-specific row type
// which shares the same column set as backup_snapshots.
func fleetSnapshotRowToSnapshot(row sqlc.FleetListSnapshotsRow) Snapshot {
	out := Snapshot{
		ID:            row.ID,
		TenantID:      row.TenantID,
		SiteID:        row.SiteID,
		Kind:          row.Kind,
		Status:        row.Status,
		AgeRecipient:  row.AgeRecipient,
		TotalSize:     row.TotalSize,
		ChunkCount:    row.ChunkCount,
		Error:         row.Error,
		Archived:      row.Archived,
		IsIncremental: row.IsIncremental,
		Generation:    int(row.Generation),
		Locked:        row.Locked,
		Progress:      row.Progress,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
	}
	if row.CreatedBy.Valid {
		id := uuid.UUID(row.CreatedBy.Bytes)
		out.CreatedBy = &id
	}
	if row.StartedAt.Valid {
		t := row.StartedAt.Time
		out.StartedAt = &t
	}
	if row.FinishedAt.Valid {
		t := row.FinishedAt.Time
		out.FinishedAt = &t
	}
	if row.ProgressUpdatedAt.Valid {
		t := row.ProgressUpdatedAt.Time
		out.ProgressUpdatedAt = &t
	}
	if row.ChainID.Valid {
		id := uuid.UUID(row.ChainID.Bytes)
		out.ChainID = &id
	}
	if row.ParentSnapshotID.Valid {
		id := uuid.UUID(row.ParentSnapshotID.Bytes)
		out.ParentSnapshotID = &id
	}
	if row.BaseSnapshotID.Valid {
		id := uuid.UUID(row.BaseSnapshotID.Bytes)
		out.BaseSnapshotID = &id
	}
	return out
}

// ----------------------------------------------------------------------------
// ADR-049 incremental restore chain planner repo methods
// ----------------------------------------------------------------------------

// ListChainSnapshots returns all snapshots for (tenantID, chainID) with
// generation <= maxGeneration, ordered by generation ASC. The base snapshot
// (generation 0) has chain_id = its own ID, so it is included when chainID
// matches. This uses the raw-SQL path (not sqlc) because the result columns
// include the ADR-048 chain fields that the generated queries do not select.
func (r *pgRepo) ListChainSnapshots(ctx context.Context, tenantID uuid.UUID, chainID uuid.UUID, maxGeneration int) ([]Snapshot, error) {
	var out []Snapshot
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			snapshotSelectColumns+` FROM backup_snapshots
			  WHERE tenant_id = $1
			    AND chain_id  = $2
			    AND generation <= $3
			  ORDER BY generation ASC`,
			tenantID, chainID, maxGeneration,
		)
		if err != nil {
			return domain.Internal("backup_chain_snapshots_list_failed", "failed to list chain snapshots").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			s, serr := scanSnapshotWithChainFields(rows)
			if serr != nil {
				return domain.Internal("backup_chain_snapshots_scan_failed", "failed to scan chain snapshot row").WithCause(serr)
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, err
}

// ListCompletedChainSnapshots is the GH #168 hardened variant of
// ListChainSnapshots: identical predicate plus "AND status='completed'", so a
// failed/aborted retry that reused a (chain_id, generation) pair is never
// returned. `, id ASC` is an additional deterministic tiebreak (belt-and-
// suspenders alongside the m96 partial unique index) for the narrow window
// before that index exists / on a not-yet-migrated self-host database where two
// completed rows could still share a generation.
func (r *pgRepo) ListCompletedChainSnapshots(ctx context.Context, tenantID uuid.UUID, chainID uuid.UUID, maxGeneration int) ([]Snapshot, error) {
	var out []Snapshot
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			snapshotSelectColumns+` FROM backup_snapshots
			  WHERE tenant_id = $1
			    AND chain_id  = $2
			    AND generation <= $3
			    AND status = 'completed'
			  ORDER BY generation ASC, id ASC`,
			tenantID, chainID, maxGeneration,
		)
		if err != nil {
			return domain.Internal("backup_chain_snapshots_list_failed", "failed to list completed chain snapshots").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			s, serr := scanSnapshotWithChainFields(rows)
			if serr != nil {
				return domain.Internal("backup_chain_snapshots_scan_failed", "failed to scan chain snapshot row").WithCause(serr)
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, err
}

// ----------------------------------------------------------------------------
// Scheduler: atomic claim-and-advance, in-flight guard, heal helpers (issue #68)
// ----------------------------------------------------------------------------

// ClaimAndAdvanceDueSchedules atomically claims all due schedules with FOR UPDATE
// SKIP LOCKED, computes their next occurrence via the caller-supplied nextAt map,
// advances next_run_at in the SAME transaction, and returns the fired slots. All
// work runs under app.agent so cross-tenant schedule rows are visible.
//
// Schedules for which nextAt has no entry are skipped (the caller must compute
// nextAt for every candidate before calling). A schedule whose lock is contended
// (held by another CP instance) is silently skipped — SKIP LOCKED ensures it.
func (r *pgRepo) ClaimAndAdvanceDueSchedules(ctx context.Context, now time.Time, nextAt map[uuid.UUID]time.Time) ([]Schedule, error) {
	var out []Schedule
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		// 1. Lock all due, enabled rows — skip any already locked by a peer.
		// GH #282: FOR UPDATE OF backup_schedules restricts the row lock to the
		// backup_schedules rows only. Postgres rejects a bare FOR UPDATE against
		// a query with an outer/joined table without naming the target relation
		// once more than one relation is in the FROM list, and even where it
		// would be accepted, an unqualified FOR UPDATE would also lock the
		// joined sites row, which is undesired and unnecessary since sites is
		// read-only here.
		pgRows, err := tx.Query(ctx,
			scheduleSelectColumnsQualified+` FROM backup_schedules
			  `+scheduleSiteJoin+`
			  WHERE backup_schedules.enabled = true AND backup_schedules.next_run_at <= $1
			  `+scheduleSiteStateGuard+`
			  ORDER BY backup_schedules.next_run_at ASC
			  FOR UPDATE OF backup_schedules SKIP LOCKED`,
			now,
		)
		if err != nil {
			return domain.Internal("backup_schedule_claim_failed", "failed to claim due schedules").WithCause(err)
		}
		defer pgRows.Close()
		var candidates []Schedule
		for pgRows.Next() {
			s, serr := scanScheduleRow(pgRows)
			if serr != nil {
				return domain.Internal("backup_schedule_claim_scan_failed", "failed to scan schedule row").WithCause(serr)
			}
			candidates = append(candidates, s)
		}
		if err := pgRows.Err(); err != nil {
			return domain.Internal("backup_schedule_claim_iter_failed", "iterator error claiming schedules").WithCause(err)
		}

		// 2. For each locked row, advance next_run_at using the pre-computed nextAt.
		for _, sched := range candidates {
			next, ok := nextAt[sched.ID]
			if !ok {
				// Caller did not supply a nextAt for this schedule (site lookup
				// failed before claim, see ClaimDueSchedules). Log so this is
				// never silent; the row stays due and will be retried next tick.
				slog.Default().Warn("backup_scheduler: no nextAt for claimed schedule — site lookup must have failed",
					slog.String("schedule_id", sched.ID.String()),
					slog.String("site_id", sched.SiteID.String()),
					slog.String("tenant_id", sched.TenantID.String()))
				continue
			}
			tag, uerr := tx.Exec(ctx,
				`UPDATE backup_schedules
				    SET next_run_at = $3, last_run_at = now(), updated_at = now()
				  WHERE id = $1 AND tenant_id = $2`,
				sched.ID, sched.TenantID, next,
			)
			if uerr != nil {
				return domain.Internal("backup_schedule_advance_claim_failed", "failed to advance claimed schedule").WithCause(uerr)
			}
			if tag.RowsAffected() == 0 {
				// Row disappeared between SELECT and UPDATE (deleted mid-tx): skip.
				continue
			}
			out = append(out, sched)
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) CountInFlightSnapshots(ctx context.Context, tenantID, siteID uuid.UUID) (int64, error) {
	var count int64
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*)
			   FROM backup_snapshots
			  WHERE tenant_id = $1 AND site_id = $2
			    AND status IN ('pending', 'running')`,
			tenantID, siteID,
		).Scan(&count)
	})
	return count, err
}

// HealOverdueSchedules walks all enabled schedules with next_run_at <= now
// cross-tenant and advances each to the next future occurrence via the caller's
// compute function (which encapsulates nextOccurrence + per-site tz + jitter).
// Returns the number of rows healed.
func (r *pgRepo) HealOverdueSchedules(ctx context.Context, now time.Time, compute func(sched Schedule, now time.Time) time.Time) (int, error) {
	// Read all overdue enabled schedules cross-tenant.
	var overdueRows []Schedule
	if err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		pgRows, err := tx.Query(ctx,
			scheduleSelectColumnsQualified+` FROM backup_schedules
			  `+scheduleSiteJoin+`
			  WHERE backup_schedules.enabled = true AND backup_schedules.next_run_at <= $1
			  `+scheduleSiteStateGuard,
			now,
		)
		if err != nil {
			return domain.Internal("backup_schedule_heal_list_failed", "failed to list overdue schedules").WithCause(err)
		}
		defer pgRows.Close()
		for pgRows.Next() {
			s, serr := scanScheduleRow(pgRows)
			if serr != nil {
				return domain.Internal("backup_schedule_heal_scan_failed", "failed to scan overdue schedule").WithCause(serr)
			}
			overdueRows = append(overdueRows, s)
		}
		return pgRows.Err()
	}); err != nil {
		return 0, err
	}

	healed := 0
	for _, sched := range overdueRows {
		next := compute(sched, now)
		if err := r.pool.InTenantTx(ctx, sched.TenantID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`UPDATE backup_schedules
				    SET next_run_at = $3, updated_at = now()
				  WHERE id = $1 AND tenant_id = $2 AND enabled = true AND next_run_at <= $4`,
				sched.ID, sched.TenantID, next, now,
			)
			return err
		}); err != nil {
			// Log and continue: one site's tz failure must not block the others.
			continue
		}
		healed++
	}
	return healed, nil
}

// ReconcileDuplicateInflightSnapshots marks the OLDER duplicate pending/running
// snapshots as failed so the partial-unique index can be created cleanly. For
// each (site_id) with more than one in-flight snapshot, all but the newest are
// failed with "duplicate_in_flight_healed". Cross-tenant under app.agent for the
// SELECT; per-tenant for each fail update.
func (r *pgRepo) ReconcileDuplicateInflightSnapshots(ctx context.Context) (int, error) {
	type dup struct {
		id       uuid.UUID
		tenantID uuid.UUID
	}
	var dups []dup

	// Find the older in-flight duplicates across all tenants.
	if err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, tenant_id
			  FROM backup_snapshots
			 WHERE status IN ('pending', 'running')
			   AND id NOT IN (
			       SELECT DISTINCT ON (site_id) id
			         FROM backup_snapshots
			        WHERE status IN ('pending', 'running')
			        ORDER BY site_id, created_at DESC
			   )`)
		if err != nil {
			return domain.Internal("backup_dup_inflight_list_failed", "failed to list duplicate in-flight snapshots").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			var d dup
			if serr := rows.Scan(&d.id, &d.tenantID); serr != nil {
				return domain.Internal("backup_dup_inflight_scan_failed", "failed to scan duplicate snapshot").WithCause(serr)
			}
			dups = append(dups, d)
		}
		return rows.Err()
	}); err != nil {
		return 0, err
	}

	failed := 0
	for _, d := range dups {
		_ = r.pool.InTenantTx(ctx, d.tenantID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				UPDATE backup_snapshots
				   SET status = 'failed', error = 'duplicate_in_flight_healed',
				       finished_at = now(), updated_at = now()
				 WHERE id = $1 AND tenant_id = $2
				   AND status IN ('pending', 'running')`,
				d.id, d.tenantID,
			)
			return err
		})
		failed++
	}
	return failed, nil
}
