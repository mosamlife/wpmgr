package backup

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

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
	// older than `threshold`. Cross-tenant — runs under `app.agent='on'` for the
	// watchdog periodic. The caller marks each stalled snapshot failed via
	// FailSnapshot (which IS tenant-scoped).
	ListStalledRunningSnapshots(ctx context.Context, threshold time.Duration) ([]StalledSnapshot, error)

	// Manifest.
	ListManifest(ctx context.Context, tenantID, snapshotID uuid.UUID) ([]ManifestEntry, error)
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
	// ListTenantsForGC enumerates tenants with completed snapshots across ALL
	// tenants for the periodic retention GC (cross-tenant, under app.agent).
	ListTenantsForGC(ctx context.Context) ([]uuid.UUID, error)
	// AdvanceScheduleRun records an enqueued scheduled backup and advances
	// next_run_at, tenant-scoped.
	AdvanceScheduleRun(ctx context.Context, tenantID, scheduleID uuid.UUID, next time.Time) error

	// Retention GC.
	ListExpiredSnapshots(ctx context.Context, tenantID uuid.UUID, before time.Time) ([]Snapshot, error)
	ListCompletedSnapshotsForSite(ctx context.Context, tenantID, siteID uuid.UUID) ([]SnapshotMeta, error)
	ListSiteIDsWithSnapshots(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error)
	SetSnapshotArchived(ctx context.Context, tenantID, snapshotID uuid.UUID, archived bool) error
	// DeleteSnapshotAndDecref deletes a snapshot, decrements the refcount of every
	// chunk its manifest referenced, and returns the s3 keys of chunks that
	// reached refcount zero (the caller deletes those objects from storage, then
	// calls DeleteOrphanChunks). Tenant-scoped, atomic.
	DeleteSnapshotAndDecref(ctx context.Context, tenantID, snapshotID uuid.UUID) (orphans []Orphan, err error)
	DeleteOrphanChunks(ctx context.Context, tenantID uuid.UUID, hashes []string) error
}

// CreateSnapshotInput creates a pending snapshot.
type CreateSnapshotInput struct {
	TenantID     uuid.UUID
	SiteID       uuid.UUID
	CreatedBy    uuid.UUID
	Kind         string
	AgeRecipient string
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
}

// StalledSnapshot is the cross-tenant projection used by the M5.6 progress
// watchdog: enough to mark the snapshot failed in its own tenant scope.
type StalledSnapshot struct {
	ID                uuid.UUID
	TenantID          uuid.UUID
	SiteID            uuid.UUID
	StartedAt         *time.Time
	ProgressUpdatedAt *time.Time
}

// SnapshotMeta is the slim projection used by the retention archive computation.
type SnapshotMeta struct {
	ID        uuid.UUID
	CreatedAt time.Time
	Archived  bool
}

// Orphan is a chunk that reached refcount zero during snapshot deletion.
type Orphan struct {
	Blake3 string
	S3Key  string
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
		row, err := sqlc.New(tx).CreateBackupSnapshot(ctx, sqlc.CreateBackupSnapshotParams{
			TenantID:     in.TenantID,
			SiteID:       in.SiteID,
			CreatedBy:    createdBy,
			Kind:         in.Kind,
			AgeRecipient: in.AgeRecipient,
		})
		if err != nil {
			return domain.Internal("backup_snapshot_create_failed", "failed to create snapshot").WithCause(err)
		}
		out = toSnapshot(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) GetSnapshot(ctx context.Context, tenantID, snapshotID uuid.UUID) (Snapshot, error) {
	var out Snapshot
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetBackupSnapshot(ctx, sqlc.GetBackupSnapshotParams{ID: snapshotID, TenantID: tenantID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("backup_snapshot_not_found", "backup snapshot not found")
			}
			return domain.Internal("backup_snapshot_get_failed", "failed to load snapshot").WithCause(err)
		}
		out = toSnapshot(row)
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
		row, err := sqlc.New(tx).GetBackupSnapshot(ctx, sqlc.GetBackupSnapshotParams{ID: snapshotID, TenantID: tenantID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("backup_snapshot_not_found", "backup snapshot not found")
			}
			return domain.Internal("backup_snapshot_get_failed", "failed to load snapshot").WithCause(err)
		}
		out = toSnapshot(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) ListSnapshotsForSite(ctx context.Context, tenantID, siteID uuid.UUID, limit, offset int32) ([]Snapshot, error) {
	var out []Snapshot
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListBackupSnapshotsForSite(ctx, sqlc.ListBackupSnapshotsForSiteParams{
			TenantID: tenantID, SiteID: siteID, Limit: limit, Offset: offset,
		})
		if err != nil {
			return domain.Internal("backup_snapshot_list_failed", "failed to list snapshots").WithCause(err)
		}
		out = make([]Snapshot, 0, len(rows))
		for _, row := range rows {
			out = append(out, toSnapshot(row))
		}
		return nil
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
// pattern as ListDueSchedules / ListTenantsForGC). The watchdog then transitions
// each row to failed via FailSnapshot (tenant-scoped).
func (r *pgRepo) ListStalledRunningSnapshots(ctx context.Context, threshold time.Duration) ([]StalledSnapshot, error) {
	var out []StalledSnapshot
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		// pgtype.Interval round-trips as microseconds — convert duration to µs.
		ival := pgtype.Interval{Microseconds: threshold.Microseconds(), Valid: true}
		rows, err := sqlc.New(tx).ListStalledRunningSnapshots(ctx, ival)
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
			out = append(out, s)
		}
		return nil
	})
	return out, err
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
				chunkRefs++
			}
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
		rows, err := sqlc.New(tx).ListBackupChunksByHashes(ctx, sqlc.ListBackupChunksByHashesParams{TenantID: tenantID, Column2: hashes})
		if err != nil {
			return domain.Internal("backup_chunk_existing_failed", "failed to query existing chunks").WithCause(err)
		}
		for _, row := range rows {
			out[row.Blake3] = toChunk(row)
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) GetSchedule(ctx context.Context, tenantID, siteID uuid.UUID) (Schedule, error) {
	var out Schedule
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).GetBackupScheduleForSite(ctx, sqlc.GetBackupScheduleForSiteParams{TenantID: tenantID, SiteID: siteID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("backup_schedule_not_found", "backup schedule not found")
			}
			return domain.Internal("backup_schedule_get_failed", "failed to load schedule").WithCause(err)
		}
		out = toSchedule(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) UpsertSchedule(ctx context.Context, in UpsertScheduleInput) (Schedule, error) {
	var out Schedule
	err := r.pool.InTenantTx(ctx, in.TenantID, func(tx pgx.Tx) error {
		// Convert int32 pointer fields to *int16 for sqlc.
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
		row, err := sqlc.New(tx).UpsertBackupSchedule(ctx, sqlc.UpsertBackupScheduleParams{
			TenantID:           in.TenantID,
			SiteID:             in.SiteID,
			Cadence:            in.Cadence,
			Kind:               in.Kind,
			Enabled:            in.Enabled,
			RetentionDays:      in.RetentionDays,
			MonthlyArchiveKeep: in.MonthlyArchiveKeep,
			NextRunAt:          in.NextRunAt,
			RunHour:            int16(in.RunHour),
			RunMinute:          int16(in.RunMinute),
			DayOfWeek:          dow,
			DayOfMonth:         dom,
			FrequencyHours:     fh,
			KeepLast:           in.KeepLast,
		})
		if err != nil {
			return domain.Internal("backup_schedule_upsert_failed", "failed to save schedule").WithCause(err)
		}
		out = toSchedule(row)
		return nil
	})
	return out, err
}

func (r *pgRepo) ListDueSchedules(ctx context.Context, now time.Time, limit int32) ([]Schedule, error) {
	var out []Schedule
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListDueBackupSchedules(ctx, sqlc.ListDueBackupSchedulesParams{NextRunAt: now, Limit: limit})
		if err != nil {
			return domain.Internal("backup_schedule_due_failed", "failed to list due schedules").WithCause(err)
		}
		out = make([]Schedule, 0, len(rows))
		for _, row := range rows {
			out = append(out, toSchedule(row))
		}
		return nil
	})
	return out, err
}

func (r *pgRepo) ListTenantsForGC(ctx context.Context) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListTenantsWithCompletedSnapshots(ctx)
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
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListCompletedSnapshotsForSite(ctx, sqlc.ListCompletedSnapshotsForSiteParams{TenantID: tenantID, SiteID: siteID})
		if err != nil {
			return domain.Internal("backup_completed_list_failed", "failed to list completed snapshots").WithCause(err)
		}
		out = make([]SnapshotMeta, 0, len(rows))
		for _, row := range rows {
			out = append(out, SnapshotMeta{ID: row.ID, CreatedAt: row.CreatedAt, Archived: row.Archived})
		}
		return nil
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

func (r *pgRepo) DeleteSnapshotAndDecref(ctx context.Context, tenantID, snapshotID uuid.UUID) ([]Orphan, error) {
	var orphans []Orphan
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		entries, err := q.ListManifestEntries(ctx, sqlc.ListManifestEntriesParams{SnapshotID: snapshotID, TenantID: tenantID})
		if err != nil {
			return domain.Internal("backup_manifest_list_failed", "failed to list manifest entries").WithCause(err)
		}
		for _, e := range entries {
			for _, h := range e.ChunkHashes {
				row, derr := q.DecrementChunkRefcount(ctx, sqlc.DecrementChunkRefcountParams{TenantID: tenantID, Blake3: h})
				if derr != nil {
					if errors.Is(derr, pgx.ErrNoRows) {
						continue // chunk already gone (concurrent GC); skip.
					}
					return domain.Internal("backup_chunk_decref_failed", "failed to decrement chunk refcount").WithCause(derr)
				}
				if row.Refcount == 0 {
					orphans = append(orphans, Orphan{Blake3: row.Blake3, S3Key: row.S3Key})
				}
			}
		}
		// Delete the snapshot (manifest entries cascade).
		if _, err := q.DeleteBackupSnapshot(ctx, sqlc.DeleteBackupSnapshotParams{ID: snapshotID, TenantID: tenantID}); err != nil {
			return domain.Internal("backup_snapshot_delete_failed", "failed to delete snapshot").WithCause(err)
		}
		return nil
	})
	return orphans, err
}

func (r *pgRepo) DeleteOrphanChunks(ctx context.Context, tenantID uuid.UUID, hashes []string) error {
	if len(hashes) == 0 {
		return nil
	}
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		for _, h := range hashes {
			if _, err := q.DeleteOrphanChunk(ctx, sqlc.DeleteOrphanChunkParams{TenantID: tenantID, Blake3: h}); err != nil {
				return domain.Internal("backup_orphan_delete_failed", "failed to delete orphan chunk").WithCause(err)
			}
		}
		return nil
	})
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
