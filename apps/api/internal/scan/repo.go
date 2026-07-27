package scan

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Repo is the persistence layer for the scan domain. Operator-facing reads and
// writes use InTenantTx (app.tenant_id GUC). The worker also uses InTenantTx
// because River resolves the tenant from the job args. Cross-tenant GC uses
// InAgentTx. The wporg_core_checksums tables have no RLS so they use plain
// Pool queries.
type Repo struct {
	pool *db.Pool
}

// NewRepo wires a Repo with the shared pgx pool.
func NewRepo(pool *db.Pool) *Repo {
	return &Repo{pool: pool}
}

// ---------------------------------------------------------------------------
// scan_runs helpers
// ---------------------------------------------------------------------------

func scanRunFromRow(row pgx.Row) (Run, error) {
	var r Run
	var cursorRaw []byte
	var findingCountsRaw []byte
	var startedAt, finishedAt *time.Time
	var wpVersion, locale, errStr *string
	if err := row.Scan(
		&r.ID, &r.TenantID, &r.SiteID, &r.Kind, &r.Status,
		&cursorRaw, &r.FilesScanned,
		&wpVersion, &locale, &errStr,
		&findingCountsRaw,
		&r.CreatedAt, &startedAt, &finishedAt,
	); err != nil {
		return Run{}, err
	}
	if wpVersion != nil {
		r.WPVersion = *wpVersion
	}
	if locale != nil {
		r.Locale = *locale
	}
	if errStr != nil {
		r.Error = *errStr
	}
	if len(cursorRaw) > 0 {
		r.Cursor = json.RawMessage(cursorRaw)
	}
	if len(findingCountsRaw) > 0 {
		_ = json.Unmarshal(findingCountsRaw, &r.FindingCounts)
	}
	r.StartedAt = startedAt
	r.FinishedAt = finishedAt
	return r, nil
}

const runSelectCols = `id, tenant_id, site_id, kind, status,
	cursor, files_scanned, wp_version, locale, error, finding_counts,
	created_at, started_at, finished_at`

// InsertRun creates a new scan_run row in the queued state.
func (r *Repo) InsertRun(ctx context.Context, tenantID, siteID uuid.UUID, kind string) (Run, error) {
	var out Run
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`INSERT INTO scan_runs (id, tenant_id, site_id, kind, status, created_at)
			 VALUES (gen_random_uuid(), $1, $2, $3, 'queued', now())
			 RETURNING `+runSelectCols,
			tenantID, siteID, kind,
		)
		var err error
		out, err = scanRunFromRow(row)
		if err != nil {
			return domain.Internal("scan_run_insert_failed", "failed to insert scan run").WithCause(err)
		}
		return nil
	})
	return out, err
}

// GetRun returns a single scan run by ID (tenant-scoped).
func (r *Repo) GetRun(ctx context.Context, tenantID, runID uuid.UUID) (Run, error) {
	var out Run
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT `+runSelectCols+`
			 FROM scan_runs
			 WHERE tenant_id = $1 AND id = $2`,
			tenantID, runID,
		)
		var err error
		out, err = scanRunFromRow(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("scan_run_not_found", "scan run not found")
		}
		if err != nil {
			return domain.Internal("scan_run_get_failed", "failed to get scan run").WithCause(err)
		}
		return nil
	})
	return out, err
}

// ListRuns returns scan runs for a site, ordered by created_at DESC. limit is clamped.
func (r *Repo) ListRuns(ctx context.Context, tenantID, siteID uuid.UUID, limit int) ([]Run, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var out []Run
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+runSelectCols+`
			 FROM scan_runs
			 WHERE tenant_id = $1 AND site_id = $2
			 ORDER BY created_at DESC
			 LIMIT $3`,
			tenantID, siteID, limit,
		)
		if err != nil {
			return domain.Internal("scan_runs_list_failed", "failed to list scan runs").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			run, err := scanRunFromRow(rows)
			if err != nil {
				return domain.Internal("scan_runs_list_failed", "failed to read scan run").WithCause(err)
			}
			out = append(out, run)
		}
		return rows.Err()
	})
	return out, err
}

// MarkScanning transitions a run from queued → scanning, sets started_at.
func (r *Repo) MarkScanning(ctx context.Context, tenantID, runID uuid.UUID) (Run, error) {
	var out Run
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`UPDATE scan_runs
			 SET status = 'scanning', started_at = now()
			 WHERE tenant_id = $1 AND id = $2
			 RETURNING `+runSelectCols,
			tenantID, runID,
		)
		var err error
		out, err = scanRunFromRow(row)
		if err != nil {
			return domain.Internal("scan_run_mark_scanning_failed", "failed to mark scan run scanning").WithCause(err)
		}
		return nil
	})
	return out, err
}

// UpdateCursor updates the run's resume cursor and cumulative files_scanned count.
func (r *Repo) UpdateCursor(ctx context.Context, tenantID, runID uuid.UUID, cursor json.RawMessage, filesScanned int64) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE scan_runs
			 SET cursor = $3, files_scanned = files_scanned + $4
			 WHERE tenant_id = $1 AND id = $2`,
			tenantID, runID, cursor, filesScanned,
		)
		if err != nil {
			return domain.Internal("scan_run_cursor_update_failed", "failed to update scan run cursor").WithCause(err)
		}
		return nil
	})
}

// MarkDone transitions a run to done, records wp_version/locale/finding_counts, sets finished_at.
func (r *Repo) MarkDone(ctx context.Context, tenantID, runID uuid.UUID, wpVersion, locale string, findingCounts map[string]int) (Run, error) {
	countsJSON, _ := json.Marshal(findingCounts)
	var out Run
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`UPDATE scan_runs
			 SET status = 'done', finished_at = now(),
			     wp_version = $3, locale = $4, finding_counts = $5
			 WHERE tenant_id = $1 AND id = $2
			 RETURNING `+runSelectCols,
			tenantID, runID, wpVersion, locale, countsJSON,
		)
		var err error
		out, err = scanRunFromRow(row)
		if err != nil {
			return domain.Internal("scan_run_mark_done_failed", "failed to mark scan run done").WithCause(err)
		}
		return nil
	})
	return out, err
}

// MarkFailed transitions a run to failed, records the error message.
func (r *Repo) MarkFailed(ctx context.Context, tenantID, runID uuid.UUID, errMsg string) (Run, error) {
	var out Run
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`UPDATE scan_runs
			 SET status = 'failed', finished_at = now(), error = $3
			 WHERE tenant_id = $1 AND id = $2
			 RETURNING `+runSelectCols,
			tenantID, runID, errMsg,
		)
		var err error
		out, err = scanRunFromRow(row)
		if err != nil {
			return domain.Internal("scan_run_mark_failed", "failed to mark scan run failed").WithCause(err)
		}
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// scan_run_hashes
// ---------------------------------------------------------------------------

// InsertHashBatch inserts a batch of file hash rows. ON CONFLICT(run_id,path)
// DO NOTHING is idempotent so River retries cannot produce duplicates.
func (r *Repo) InsertHashBatch(ctx context.Context, tenantID uuid.UUID, rows []HashRow) error {
	if len(rows) == 0 {
		return nil
	}
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		for _, h := range rows {
			if _, err := tx.Exec(ctx,
				`INSERT INTO scan_run_hashes
					(tenant_id, run_id, path, size, md5, mtime, is_link)
				 VALUES ($1, $2, $3, $4, $5, $6, $7)
				 ON CONFLICT (run_id, path) DO NOTHING`,
				tenantID, h.RunID, h.Path, h.Size, h.MD5, h.Mtime, h.IsLink,
			); err != nil {
				return domain.Internal("scan_hash_insert_failed", "failed to insert scan hash").WithCause(err)
			}
		}
		return nil
	})
}

// ListHashes returns all hash rows for a run (used during diffCore after scan completes).
func (r *Repo) ListHashes(ctx context.Context, tenantID, runID uuid.UUID) ([]HashRow, error) {
	var out []HashRow
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, run_id, path, size, md5, mtime, is_link
			 FROM scan_run_hashes
			 WHERE tenant_id = $1 AND run_id = $2`,
			tenantID, runID,
		)
		if err != nil {
			return domain.Internal("scan_hashes_list_failed", "failed to list scan hashes").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			var h HashRow
			if err := rows.Scan(&h.ID, &h.TenantID, &h.RunID, &h.Path, &h.Size, &h.MD5, &h.Mtime, &h.IsLink); err != nil {
				return domain.Internal("scan_hashes_list_failed", "failed to read scan hash").WithCause(err)
			}
			out = append(out, h)
		}
		return rows.Err()
	})
	return out, err
}

// PurgeHashes deletes all hash rows for a completed/failed run.
func (r *Repo) PurgeHashes(ctx context.Context, tenantID, runID uuid.UUID) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`DELETE FROM scan_run_hashes WHERE tenant_id = $1 AND run_id = $2`,
			tenantID, runID,
		)
		if err != nil {
			return domain.Internal("scan_hashes_purge_failed", "failed to purge scan hashes").WithCause(err)
		}
		return nil
	})
}

// PurgeOrphanHashes deletes hash rows for runs older than the given age that
// are still in staging (no done/failed run cleanup ran). This is the GC
// backstop for runs that died mid-scan (River crash etc). Runs under the
// agent GUC so it is cross-tenant.
func (r *Repo) PurgeOrphanHashes(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	var deleted int64
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM scan_run_hashes
			 WHERE run_id IN (
			     SELECT id FROM scan_runs
			     WHERE created_at < $1
			       AND status NOT IN ('done', 'failed')
			 )`,
			cutoff,
		)
		if err != nil {
			return domain.Internal("scan_orphan_hashes_purge_failed", "failed to purge orphan scan hashes").WithCause(err)
		}
		deleted = tag.RowsAffected()
		return nil
	})
	return deleted, err
}

// ---------------------------------------------------------------------------
// scan_findings
// ---------------------------------------------------------------------------

const findingSelectCols = `id, tenant_id, site_id, run_id, finding_type, path, severity,
	expected_md5, actual_md5, dedup_key, ignored, ignored_by,
	created_at, last_seen_run`

func scanFindingFromRow(r pgx.Row) (Finding, error) {
	var f Finding
	var expectedMD5, actualMD5, ignoredBy *string
	if err := r.Scan(
		&f.ID, &f.TenantID, &f.SiteID, &f.RunID, &f.FindingType, &f.Path,
		&f.Severity, &expectedMD5, &actualMD5, &f.DeduKey, &f.Ignored, &ignoredBy,
		&f.CreatedAt, &f.LastSeenRun,
	); err != nil {
		return Finding{}, err
	}
	if expectedMD5 != nil {
		f.ExpectedMD5 = *expectedMD5
	}
	if actualMD5 != nil {
		f.ActualMD5 = *actualMD5
	}
	if ignoredBy != nil {
		f.IgnoredBy = *ignoredBy
	}
	return f, nil
}

// UpsertFindings deduplicates findings: ON CONFLICT(tenant_id, site_id, dedup_key)
// bumps last_seen_run + actual_md5 but PRESERVES ignored (operators who have
// silenced a finding keep it silenced on re-scan).
func (r *Repo) UpsertFindings(ctx context.Context, tenantID uuid.UUID, findings []Finding) error {
	if len(findings) == 0 {
		return nil
	}
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		for _, f := range findings {
			if _, err := tx.Exec(ctx,
				`INSERT INTO scan_findings
					(id, tenant_id, site_id, run_id, finding_type, path, severity,
					 expected_md5, actual_md5, dedup_key, ignored, ignored_by,
					 created_at, last_seen_run)
				 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6,
				         $7, $8, $9, false, null,
				         now(), $3)
				 ON CONFLICT (tenant_id, site_id, dedup_key) DO UPDATE
				   SET last_seen_run = EXCLUDED.last_seen_run,
				       actual_md5   = EXCLUDED.actual_md5,
				       severity     = EXCLUDED.severity,
				       finding_type = EXCLUDED.finding_type,
				       run_id       = EXCLUDED.run_id
				   -- PRESERVE: ignored + ignored_by (operators' decisions survive re-scan)`,
				tenantID, f.SiteID, f.RunID, f.FindingType, f.Path, f.Severity,
				nilIfEmpty(f.ExpectedMD5), nilIfEmpty(f.ActualMD5), f.DeduKey,
			); err != nil {
				return domain.Internal("scan_finding_upsert_failed", "failed to upsert scan finding").WithCause(err)
			}
		}
		return nil
	})
}

// ListFindings returns findings for a site. ignoredFilter: nil=all, true=ignored only, false=active only.
func (r *Repo) ListFindings(ctx context.Context, tenantID, siteID uuid.UUID, limit int, ignoredFilter *bool) ([]Finding, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []Finding
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		args := []any{tenantID, siteID, limit}
		q := `SELECT ` + findingSelectCols + `
			  FROM scan_findings
			  WHERE tenant_id = $1 AND site_id = $2`
		if ignoredFilter != nil {
			args = append(args, *ignoredFilter)
			q += ` AND ignored = $` + itoa(len(args))
		}
		q += ` ORDER BY created_at DESC LIMIT $3`

		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return domain.Internal("scan_findings_list_failed", "failed to list scan findings").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			f, err := scanFindingFromRow(rows)
			if err != nil {
				return domain.Internal("scan_findings_list_failed", "failed to read scan finding").WithCause(err)
			}
			out = append(out, f)
		}
		return rows.Err()
	})
	return out, err
}

// ListFindingsForRun returns findings for a specific run.
func (r *Repo) ListFindingsForRun(ctx context.Context, tenantID, siteID, runID uuid.UUID, limit int) ([]Finding, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []Finding
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+findingSelectCols+`
			 FROM scan_findings
			 WHERE tenant_id = $1 AND site_id = $2 AND last_seen_run = $3
			 ORDER BY created_at DESC
			 LIMIT $4`,
			tenantID, siteID, runID, limit,
		)
		if err != nil {
			return domain.Internal("scan_findings_list_failed", "failed to list scan findings for run").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			f, err := scanFindingFromRow(rows)
			if err != nil {
				return domain.Internal("scan_findings_list_failed", "failed to read scan finding").WithCause(err)
			}
			out = append(out, f)
		}
		return rows.Err()
	})
	return out, err
}

// GetFinding returns a single finding by ID (tenant-scoped).
func (r *Repo) GetFinding(ctx context.Context, tenantID, findingID uuid.UUID) (Finding, error) {
	var out Finding
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT `+findingSelectCols+`
			 FROM scan_findings
			 WHERE tenant_id = $1 AND id = $2`,
			tenantID, findingID,
		)
		f, err := scanFindingFromRow(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("scan_finding_not_found", "scan finding not found")
		}
		if err != nil {
			return domain.Internal("scan_finding_get_failed", "failed to get scan finding").WithCause(err)
		}
		out = f
		return nil
	})
	return out, err
}

// SetFindingIgnored sets the ignored flag and ignored_by on a finding.
func (r *Repo) SetFindingIgnored(ctx context.Context, tenantID, findingID uuid.UUID, ignored bool, ignoredBy string) (Finding, error) {
	var out Finding
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		var ignoredByVal *string
		if ignoredBy != "" {
			ignoredByVal = &ignoredBy
		}
		row := tx.QueryRow(ctx,
			`UPDATE scan_findings
			 SET ignored = $3, ignored_by = $4
			 WHERE tenant_id = $1 AND id = $2
			 RETURNING `+findingSelectCols,
			tenantID, findingID, ignored, ignoredByVal,
		)
		f, err := scanFindingFromRow(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("scan_finding_not_found", "scan finding not found")
		}
		if err != nil {
			return domain.Internal("scan_finding_ignore_failed", "failed to set scan finding ignored").WithCause(err)
		}
		out = f
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// wporg_core_checksums (no RLS — public reference)
// ---------------------------------------------------------------------------

// ChecksumRow is a single row from wporg_core_checksums.
type ChecksumRow struct {
	Path string
	MD5  string
}

// GetChecksumsMeta retrieves the freshness metadata for a version/locale pair.
// Returns (fetched_at, ok, found). found=false when no meta row exists yet.
func (r *Repo) GetChecksumsMeta(ctx context.Context, version, locale string) (time.Time, bool, bool, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT fetched_at, ok FROM wporg_core_checksums_meta
		 WHERE version = $1 AND locale = $2`,
		version, locale,
	)
	var fetchedAt time.Time
	var ok bool
	if err := row.Scan(&fetchedAt, &ok); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, false, false, nil
		}
		return time.Time{}, false, false, domain.Internal("checksums_meta_get_failed", "failed to get checksums meta").WithCause(err)
	}
	return fetchedAt, ok, true, nil
}

// UpsertChecksumsMeta records a fetch attempt. ok=false for negative-cache.
func (r *Repo) UpsertChecksumsMeta(ctx context.Context, version, locale string, ok bool) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO wporg_core_checksums_meta (version, locale, fetched_at, ok)
		 VALUES ($1, $2, now(), $3)
		 ON CONFLICT (version, locale) DO UPDATE
		   SET fetched_at = now(), ok = EXCLUDED.ok`,
		version, locale, ok,
	)
	if err != nil {
		return domain.Internal("checksums_meta_upsert_failed", "failed to upsert checksums meta").WithCause(err)
	}
	return nil
}

// GetChecksums returns all checksum rows for a version/locale pair.
func (r *Repo) GetChecksums(ctx context.Context, version, locale string) ([]ChecksumRow, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT path, md5 FROM wporg_core_checksums
		 WHERE version = $1 AND locale = $2`,
		version, locale,
	)
	if err != nil {
		return nil, domain.Internal("checksums_get_failed", "failed to get checksums").WithCause(err)
	}
	defer rows.Close()
	var out []ChecksumRow
	for rows.Next() {
		var c ChecksumRow
		if err := rows.Scan(&c.Path, &c.MD5); err != nil {
			return nil, domain.Internal("checksums_get_failed", "failed to read checksum row").WithCause(err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpsertChecksums bulk-inserts checksums. ON CONFLICT(version,locale,path) DO
// UPDATE so stale data is refreshed when a version is re-fetched.
func (r *Repo) UpsertChecksums(ctx context.Context, version, locale string, rows []ChecksumRow) error {
	if len(rows) == 0 {
		return nil
	}
	// Build a multi-value INSERT for efficiency.
	const batchSize = 500
	for i := 0; i < len(rows); i += batchSize {
		end := i + batchSize
		if end > len(rows) {
			end = len(rows)
		}
		batch := rows[i:end]
		args := make([]any, 0, len(batch)*4)
		placeholders := make([]string, 0, len(batch))
		for j, c := range batch {
			base := j*4 + 1
			placeholders = append(placeholders,
				"($"+itoa(base)+", $"+itoa(base+1)+", $"+itoa(base+2)+", $"+itoa(base+3)+", now())")
			args = append(args, version, locale, c.Path, c.MD5)
		}
		q := `INSERT INTO wporg_core_checksums (version, locale, path, md5, fetched_at)
			  VALUES ` + strings.Join(placeholders, ",") + `
			  ON CONFLICT (version, locale, path) DO UPDATE
			    SET md5 = EXCLUDED.md5, fetched_at = now()`
		if _, err := r.pool.Exec(ctx, q, args...); err != nil {
			return domain.Internal("checksums_upsert_failed", "failed to upsert checksums").WithCause(err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// wporg_plugin_checksums + wporg_plugin_checksums_meta (no RLS)
// ---------------------------------------------------------------------------

// GetPluginChecksumsMeta returns freshness metadata for a (kind, slug, version).
// Returns (fetchedAt, ok, found, err). found=false when no meta row exists.
func (r *Repo) GetPluginChecksumsMeta(ctx context.Context, kind, slug, version string) (time.Time, bool, bool, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT fetched_at, ok FROM wporg_plugin_checksums_meta
		 WHERE kind = $1 AND slug = $2 AND version = $3`,
		kind, slug, version,
	)
	var fetchedAt time.Time
	var ok bool
	if err := row.Scan(&fetchedAt, &ok); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, false, false, nil
		}
		return time.Time{}, false, false, domain.Internal("plugin_checksums_meta_get_failed", "failed to get plugin checksums meta").WithCause(err)
	}
	return fetchedAt, ok, true, nil
}

// UpsertPluginChecksumsMeta records a fetch attempt (positive or negative).
func (r *Repo) UpsertPluginChecksumsMeta(ctx context.Context, kind, slug, version string, ok bool) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO wporg_plugin_checksums_meta (kind, slug, version, fetched_at, ok)
		 VALUES ($1, $2, $3, now(), $4)
		 ON CONFLICT (kind, slug, version) DO UPDATE
		   SET fetched_at = now(), ok = EXCLUDED.ok`,
		kind, slug, version, ok,
	)
	if err != nil {
		return domain.Internal("plugin_checksums_meta_upsert_failed", "failed to upsert plugin checksums meta").WithCause(err)
	}
	return nil
}

// GetPluginChecksums returns all checksum rows for a (kind, slug, version).
// Multiple rows per path are expected (one per accepted md5 variant).
func (r *Repo) GetPluginChecksums(ctx context.Context, kind, slug, version string) ([]PluginChecksumRow, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT kind, slug, version, path, md5, sha256
		 FROM wporg_plugin_checksums
		 WHERE kind = $1 AND slug = $2 AND version = $3`,
		kind, slug, version,
	)
	if err != nil {
		return nil, domain.Internal("plugin_checksums_get_failed", "failed to get plugin checksums").WithCause(err)
	}
	defer rows.Close()
	var out []PluginChecksumRow
	for rows.Next() {
		var c PluginChecksumRow
		var sha256 *string
		if err := rows.Scan(&c.Kind, &c.Slug, &c.Version, &c.Path, &c.MD5, &sha256); err != nil {
			return nil, domain.Internal("plugin_checksums_get_failed", "failed to read plugin checksum row").WithCause(err)
		}
		if sha256 != nil {
			c.SHA256 = *sha256
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// pluginChecksumConflictKey mirrors wporg_plugin_checksums' primary key,
// which is also the ON CONFLICT target used by UpsertPluginChecksums.
type pluginChecksumConflictKey struct {
	Kind, Slug, Version, Path, MD5 string
}

// dedupePluginChecksumRows collapses rows that share the ON CONFLICT
// (kind, slug, version, path, md5) target down to a single row per key.
//
// This is required because a single multi-value INSERT ... ON CONFLICT DO
// UPDATE statement cannot affect the same target row twice: Postgres raises
// cardinality_violation ("ON CONFLICT DO UPDATE command cannot affect row a
// second time") if the VALUES list itself contains two rows with the same
// conflict key. wp.org's plugin-checksums API can list a duplicate md5
// within the same file's accepted-variant array (decodeMD5Variants passes
// such duplicates through unchanged), which produces exactly that duplicate
// key, so the batch must be deduplicated before the statement is built.
//
// Last-write-wins: rows are deduplicated in input order and a later
// occurrence of a key overwrites an earlier one. This mirrors what a
// sequential per-row upsert would have produced from the same input order,
// since each later row's DO UPDATE would overwrite the row before it.
func dedupePluginChecksumRows(rows []PluginChecksumRow) []PluginChecksumRow {
	if len(rows) < 2 {
		return rows
	}
	order := make([]pluginChecksumConflictKey, 0, len(rows))
	byKey := make(map[pluginChecksumConflictKey]PluginChecksumRow, len(rows))
	for _, row := range rows {
		key := pluginChecksumConflictKey{row.Kind, row.Slug, row.Version, row.Path, row.MD5}
		if _, seen := byKey[key]; !seen {
			order = append(order, key)
		}
		byKey[key] = row
	}
	out := make([]PluginChecksumRow, 0, len(order))
	for _, key := range order {
		out = append(out, byKey[key])
	}
	return out
}

// UpsertPluginChecksums bulk-inserts plugin/theme checksum rows.
// ON CONFLICT: md5 is in the PK so duplicate variants target the same row.
// sha256 is refreshed on conflict (COALESCE keeps a previously-stored value if
// this fetch didn't decode one, so a transient decode gap never regresses an
// already-known sha256 back to NULL); kind/slug/version/path/md5 are the
// conflict target itself and never change.
//
// The input batch is deduplicated by the conflict key before the statement is
// built; see dedupePluginChecksumRows.
func (r *Repo) UpsertPluginChecksums(ctx context.Context, rows []PluginChecksumRow) error {
	if len(rows) == 0 {
		return nil
	}
	rows = dedupePluginChecksumRows(rows)
	const batchSize = 500
	for i := 0; i < len(rows); i += batchSize {
		end := i + batchSize
		if end > len(rows) {
			end = len(rows)
		}
		batch := rows[i:end]
		args := make([]any, 0, len(batch)*6)
		placeholders := make([]string, 0, len(batch))
		for j, c := range batch {
			base := j*6 + 1
			placeholders = append(placeholders,
				"($"+itoa(base)+", $"+itoa(base+1)+", $"+itoa(base+2)+
					", $"+itoa(base+3)+", $"+itoa(base+4)+", $"+itoa(base+5)+", now())")
			args = append(args, c.Kind, c.Slug, c.Version, c.Path, c.MD5, nilIfEmpty(c.SHA256))
		}
		q := `INSERT INTO wporg_plugin_checksums (kind, slug, version, path, md5, sha256, fetched_at)
			  VALUES ` + strings.Join(placeholders, ",") + `
			  ON CONFLICT (kind, slug, version, path, md5) DO UPDATE
			    SET sha256 = COALESCE(EXCLUDED.sha256, wporg_plugin_checksums.sha256),
			        fetched_at = now()`
		if _, err := r.pool.Exec(ctx, q, args...); err != nil {
			return domain.Internal("plugin_checksums_upsert_failed", "failed to upsert plugin checksums").WithCause(err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// site_file_baseline (tenant-scoped, RLS)
// ---------------------------------------------------------------------------

// GetBaseline returns all baseline rows for a site. Used by diffFiles before
// computing the diff. Runs under InTenantTx.
func (r *Repo) GetBaseline(ctx context.Context, tenantID, siteID uuid.UUID) ([]BaselineRow, error) {
	var out []BaselineRow
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT site_id, tenant_id, path, md5, size, mtime, is_link, source, updated_run
			 FROM site_file_baseline
			 WHERE tenant_id = $1 AND site_id = $2`,
			tenantID, siteID,
		)
		if err != nil {
			return domain.Internal("baseline_get_failed", "failed to get file baseline").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			var b BaselineRow
			if err := rows.Scan(&b.SiteID, &b.TenantID, &b.Path, &b.MD5, &b.Size, &b.Mtime, &b.IsLink, &b.Source, &b.UpdatedRun); err != nil {
				return domain.Internal("baseline_get_failed", "failed to read baseline row").WithCause(err)
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	return out, err
}

// PromoteBaselineSelective advances the baseline for a site after a full/files
// scan run, but NEVER auto-advances past an unreviewed change.
//
// Design rule: the baseline must never be promoted past a live (non-ignored)
// finding. An unreviewed tampered file must remain flagged on every subsequent
// scan until the operator explicitly accepts it.
//
// coldStart is true when no baseline rows existed before this run (i.e. this is
// the very first scan). In that case we establish the full baseline from the
// run's hashes (all paths, no findings to skip). On subsequent runs only paths
// that produced NO active finding in this run advance; paths with a live
// finding keep their prior baseline row (or stay absent for file_added paths).
//
// activeFindingPaths is the set of paths that have an active (non-ignored)
// finding from this run. Those paths are excluded from promotion so the next
// scan re-flags them.
func (r *Repo) PromoteBaselineSelective(ctx context.Context, tenantID, siteID, runID uuid.UUID, coldStart bool, activeFindingPaths map[string]bool) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		if coldStart {
			// Cold start: no prior baseline exists. Establish the full baseline
			// from this run's hashes. No finding paths to exclude (cold-start
			// findings are core/plugin checksum findings that do not involve the
			// baseline table; those paths are still safe to baseline).
			if _, err := tx.Exec(ctx,
				`INSERT INTO site_file_baseline
					(site_id, tenant_id, path, md5, size, mtime, is_link, source, updated_run, updated_at)
				 SELECT $2, $1, path, md5, size, mtime, is_link, 'baseline', $3, now()
				 FROM scan_run_hashes
				 WHERE tenant_id = $1 AND run_id = $3
				 ON CONFLICT (site_id, path) DO UPDATE
				   SET md5 = EXCLUDED.md5, size = EXCLUDED.size, mtime = EXCLUDED.mtime,
				       is_link = EXCLUDED.is_link, source = EXCLUDED.source,
				       updated_run = EXCLUDED.updated_run, updated_at = now()`,
				tenantID, siteID, runID,
			); err != nil {
				return domain.Internal("baseline_promote_failed", "failed to establish cold-start baseline").WithCause(err)
			}
			return nil
		}

		// Subsequent run: upsert ONLY clean paths (no active finding).
		// Paths with an active finding keep their prior baseline row so the
		// next scan re-compares against the last known-good hash.
		//
		// Implementation: load the run's hashes and upsert them one by one,
		// skipping any path present in activeFindingPaths. A batch approach is
		// used for efficiency; the exclusion list is small in the common case.
		//
		// We deliberately do NOT DELETE any baseline rows on a normal run:
		//   - A removed file keeps its baseline row so file_removed re-surfaces
		//     until the operator accepts it.
		//   - A clean file gets its baseline row upserted to the current hash.
		rows, err := tx.Query(ctx,
			`SELECT path, md5, size, mtime, is_link
			 FROM scan_run_hashes
			 WHERE tenant_id = $1 AND run_id = $2`,
			tenantID, runID,
		)
		if err != nil {
			return domain.Internal("baseline_promote_failed", "failed to load run hashes for selective promotion").WithCause(err)
		}
		defer rows.Close()

		for rows.Next() {
			var path, md5 string
			var size, mtime int64
			var isLink bool
			if err := rows.Scan(&path, &md5, &size, &mtime, &isLink); err != nil {
				return domain.Internal("baseline_promote_failed", "failed to read hash row during promotion").WithCause(err)
			}
			if activeFindingPaths[path] {
				// This path has a live unreviewed finding — do NOT advance its baseline.
				// The prior baseline row (if any) is preserved so the next scan
				// re-flags the change. A file_added path with no prior baseline row
				// also stays absent so the next scan re-flags it as file_added.
				continue
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO site_file_baseline
					(site_id, tenant_id, path, md5, size, mtime, is_link, source, updated_run, updated_at)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, 'baseline', $8, now())
				 ON CONFLICT (site_id, path) DO UPDATE
				   SET md5 = EXCLUDED.md5, size = EXCLUDED.size, mtime = EXCLUDED.mtime,
				       is_link = EXCLUDED.is_link, source = EXCLUDED.source,
				       updated_run = EXCLUDED.updated_run, updated_at = now()`,
				siteID, tenantID, path, md5, size, mtime, isLink, runID,
			); err != nil {
				return domain.Internal("baseline_promote_failed", "failed to upsert baseline row").WithCause(err)
			}
		}
		return rows.Err()
	})
}

// AdvanceBaselineForPath updates (or inserts) the baseline row for a single
// path to the given MD5 hash. Called when an operator accepts / ignores a
// finding: the tampered hash becomes the new known-good for that path so
// subsequent scans do not re-flag an intentional or reviewed change.
//
// This is the ONLY mechanism by which an active-finding path advances its
// baseline — the normal promotion run intentionally skips it.
func (r *Repo) AdvanceBaselineForPath(ctx context.Context, tenantID, siteID uuid.UUID, path, md5 string, runID uuid.UUID) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO site_file_baseline
				(site_id, tenant_id, path, md5, size, mtime, is_link, source, updated_run, updated_at)
			 VALUES ($1, $2, $3, $4, 0, 0, false, 'accepted', $5, now())
			 ON CONFLICT (site_id, path) DO UPDATE
			   SET md5 = EXCLUDED.md5, source = EXCLUDED.source,
			       updated_run = EXCLUDED.updated_run, updated_at = now()`,
			siteID, tenantID, path, md5, runID,
		); err != nil {
			return domain.Internal("baseline_advance_failed", "failed to advance baseline for path").WithCause(err)
		}
		return nil
	})
}

// DeleteBaselineForPath removes a single baseline row for the given path. Used
// when an operator accepts a file_removed finding: the file is gone, so the
// baseline should no longer expect it, preventing perpetual re-flagging.
func (r *Repo) DeleteBaselineForPath(ctx context.Context, tenantID, siteID uuid.UUID, path string) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`DELETE FROM site_file_baseline
			 WHERE tenant_id = $1 AND site_id = $2 AND path = $3`,
			tenantID, siteID, path,
		); err != nil {
			return domain.Internal("baseline_delete_path_failed", "failed to delete baseline row for path").WithCause(err)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// site_managed_files (tenant-scoped, RLS)
// ---------------------------------------------------------------------------

// GetManagedFiles returns all managed-file rows for a site. Used by diffFiles.
func (r *Repo) GetManagedFiles(ctx context.Context, tenantID, siteID uuid.UUID) ([]ManagedFileRow, error) {
	var out []ManagedFileRow
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT site_id, tenant_id, path, md5, managed_by
			 FROM site_managed_files
			 WHERE tenant_id = $1 AND site_id = $2`,
			tenantID, siteID,
		)
		if err != nil {
			return domain.Internal("managed_files_get_failed", "failed to get managed files").WithCause(err)
		}
		defer rows.Close()
		for rows.Next() {
			var m ManagedFileRow
			if err := rows.Scan(&m.SiteID, &m.TenantID, &m.Path, &m.MD5, &m.ManagedBy); err != nil {
				return domain.Internal("managed_files_get_failed", "failed to read managed file row").WithCause(err)
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// UpsertManagedFiles inserts or updates rows in site_managed_files.
// Called by the record_managed_files agent-callback handler after the agent
// reports paths + hashes for its own writes.
func (r *Repo) UpsertManagedFiles(ctx context.Context, tenantID uuid.UUID, rows []ManagedFileRow) error {
	if len(rows) == 0 {
		return nil
	}
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		for _, m := range rows {
			if _, err := tx.Exec(ctx,
				`INSERT INTO site_managed_files
					(site_id, tenant_id, path, md5, managed_by, updated_at)
				 VALUES ($1, $2, $3, $4, $5, now())
				 ON CONFLICT (site_id, path) DO UPDATE
				   SET md5 = EXCLUDED.md5,
				       managed_by = EXCLUDED.managed_by,
				       updated_at = now()`,
				m.SiteID, tenantID, m.Path, m.MD5, m.ManagedBy,
			); err != nil {
				return domain.Internal("managed_files_upsert_failed", "failed to upsert managed file").WithCause(err)
			}
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// itoa is a tiny helper for building $-arg numbers in dynamic WHERE clauses.
func itoa(n int) string {
	if n <= 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
