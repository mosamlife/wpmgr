-- M6 / Track 4 (CP-side): SQL inspection cache marker on backup_snapshots.
-- ----------------------------------------------------------------------------
-- The /api/v1/backups/{snapshotId}/sql-inspection endpoint has three
-- resolution tiers:
--   1. Agent-supplied inspection artifact in the snapshot manifest
--      (manifest entry of kind="inspection" or path="sql-inspection.json").
--   2. CP legacy cache: a JSON object the CP stored at
--      inspection-cache/{snapshot_id}.json after streaming the DB artifact
--      through internal/restore/sqlinspect. This column is the existence
--      marker for that cached object.
--   3. Enqueue the SqlInspectLegacy River job and return 202 Accepted.
--
-- Storing a TIMESTAMPTZ (rather than a boolean) lets the GC + operator-side
-- diagnostics see WHEN the cache was last populated; a stale cache for a
-- redacted-recipient or recovery-bucket-swapped tenant can be force-rebuilt
-- by NULL'ing this column. Manifest-supplied inspection ("agent" source)
-- doesn't use this column at all — its existence-of-truth is the manifest
-- itself; this column is only the legacy/streaming cache marker.
ALTER TABLE "public"."backup_snapshots"
  ADD COLUMN "sql_inspection_cached_at" timestamptz;

COMMENT ON COLUMN "public"."backup_snapshots"."sql_inspection_cached_at" IS
  'When the CP wrote a legacy inspection cache for this snapshot. NULL means no cache; manifest-based inspection has its own resolution path.';
