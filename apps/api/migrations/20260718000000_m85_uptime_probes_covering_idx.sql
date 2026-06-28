-- m85: covering index on site_uptime_probes for cold-cache fleet uptime query
--
-- Root cause: QueryFleetUptime runs two LEFT JOIN LATERALs per site:
--   lat: ORDER BY probed_at DESC LIMIT 1 (latest probe)
--   agg: COUNT/AVG over 30-day window
-- Both filter (site_id = s.id AND tenant_id = $1). The existing
-- site_uptime_probes_site_time_idx covers (site_id, probed_at DESC) but does
-- not include tenant_id, up, or total_ms, so the aggregate LATERAL performs a
-- heap fetch per row to read those columns. On a cold Postgres buffer cache
-- (Cloud SQL db-g1-small, shared-core) this heap I/O is ~8s for a 30-day
-- window even for 2 sites.
--
-- Fix: add a covering index (site_id, tenant_id, probed_at DESC) INCLUDE (up,
-- total_ms). Both laterals can then be satisfied entirely from the index
-- (index-only scan) when the visibility map marks pages as all-visible. For
-- recently-inserted pages not yet vacuumed, the executor hits the heap only for
-- the visibility check — those pages are hot in the shared buffer pool, so
-- cold-cache cost collapses to the index scan alone.
--
-- Note on CONCURRENTLY: the migration runner (internal/db/migrate.go) wraps
-- every migration in a pgx.BeginFunc transaction. CREATE INDEX CONCURRENTLY
-- cannot run inside an explicit transaction (Postgres error 25001). We therefore
-- use plain CREATE INDEX, which takes a ShareLock for the duration of the build.
-- On a table with at most a few million rows this completes in seconds; the CP
-- has no other readers blocked on this table at boot time. The IF NOT EXISTS
-- guard makes re-runs safe.
--
-- The old site_uptime_probes_site_time_idx (site_id, probed_at DESC) becomes
-- redundant once this covering index exists: the new index is a superset for the
-- lat LATERAL (it leads on site_id, covers probed_at DESC, and includes tenant_id
-- so the tenant_id = $1 filter resolves without a heap fetch). We DROP the old
-- index here so the table carries one index instead of two for the same access
-- path. The tenant-wide summary index (site_uptime_probes_tenant_time_idx) is
-- kept — it serves a different query shape (tenant-wide recent scans).
--
-- Idempotent: both CREATE INDEX IF NOT EXISTS and DROP INDEX IF EXISTS are safe
-- to re-run.

DO $$
BEGIN
    -- Drop the narrower index that is a subset of the new covering index.
    -- IF EXISTS guards against re-run on an already-migrated DB.
    DROP INDEX IF EXISTS site_uptime_probes_site_time_idx;
END;
$$;

CREATE INDEX IF NOT EXISTS site_uptime_probes_agg_idx
    ON site_uptime_probes (site_id, tenant_id, probed_at DESC)
    INCLUDE (up, total_ms);
