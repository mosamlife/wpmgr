-- m86: probed_at index so the retention GC delete is index-driven, not a seq scan
--
-- The UptimeProbeGCWorker daily delete is:
--   DELETE FROM site_uptime_probes
--   WHERE id IN (SELECT id FROM site_uptime_probes WHERE probed_at < $1 LIMIT N)
--
-- After m85 the table's indexes lead on (site_id, ...) and (tenant_id, ...);
-- NONE leads on probed_at. So a predicate on probed_at alone cannot use an
-- index and Postgres sequentially scans the whole table to find the old rows.
-- On a large table that daily full scan is wasteful AND evicts the hot pages
-- the fleet-uptime query depends on from the shared buffer pool — which would
-- re-introduce the cold-cache latency m85 just removed, once per day.
--
-- Fix: a plain btree index leading on probed_at lets the retention subquery do
-- an index range scan over just the oldest rows. The write cost is one extra
-- index maintained per probe insert, acceptable for a low-rate append table and
-- far cheaper than the recurring full scan.
--
-- Plain CREATE INDEX (the migration runner wraps each migration in a tx, so
-- CONCURRENTLY is unavailable — see m85); IF NOT EXISTS makes it re-run safe.

CREATE INDEX IF NOT EXISTS site_uptime_probes_probed_at_idx
    ON site_uptime_probes (probed_at);
