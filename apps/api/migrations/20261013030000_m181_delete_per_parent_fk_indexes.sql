-- m180: leading-key indexes for referential actions on job and report history.
-- The API startup runner applies each migration in a transaction, so index
-- builds use CREATE INDEX rather than CREATE INDEX CONCURRENTLY.

CREATE INDEX IF NOT EXISTS media_optimization_jobs_asset_id_idx
    ON media_optimization_jobs (asset_id);

CREATE INDEX IF NOT EXISTS rucss_jobs_result_id_idx
    ON rucss_jobs (result_id);

CREATE INDEX IF NOT EXISTS backup_schedule_runs_snapshot_id_idx
    ON backup_schedule_runs (snapshot_id);

CREATE INDEX IF NOT EXISTS generated_reports_schedule_id_idx
    ON generated_reports (schedule_id);
