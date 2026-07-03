-- m88: cross-run dedup guard for update_tasks (#131 hardening)
--
-- Root cause: nothing prevented multiple update RUNS from creating tasks for
-- the SAME (site, target) at the same time. planTasks/normalizeItems only
-- dedup WITHIN one run, and the River queue caps concurrency per TENANT (not
-- per site+target). So a scheduled auto-update, an operator "Update all", and
-- a client-portal trigger firing within the same ~1s window for the same
-- (site, plugin) could all create tasks, and up to perTenantLimit of them run
-- concurrently. That (a) races the agent's own rollback-snapshot pruning — a
-- task's own snapshot can be deleted by a sibling task's cleanup, producing
-- "rollback FAILED / Invalid snapshot id" — and (b) runs concurrent
-- WordPress Plugin_Upgrader instances against the same plugin directory,
-- which can corrupt wp-content/plugins/<slug>.
--
-- Fix: a partial UNIQUE index on (tenant_id, site_id, target_type,
-- target_slug) restricted to the in-flight statuses ('pending','running')
-- means Postgres itself rejects a second in-flight task for the same target
-- while one already exists — this is the authoritative, race-free guard (a
-- service-level pre-check narrows the window further but cannot itself be
-- atomic). Once a task reaches a terminal status (succeeded/failed/
-- rolled_back/skipped) it falls out of the partial index automatically, so a
-- legitimate SEQUENTIAL re-update after the prior one finished is unaffected.
--
-- Pre-dedup (ship-blocker fix): on a LIVE database that already ran the
-- buggy pre-m88 code, multiple pending/running tasks can already exist for
-- the same (tenant, site, target_type, target_slug) — that is the exact bug
-- this migration fixes, and there is no reaper, so they persist indefinitely.
-- CREATE UNIQUE INDEX below would then fail outright on the duplicate data,
-- and since the migration runner wraps each migration in a tx and aborts
-- boot on error (internal/db/migrate.go), that would hard-block the API from
-- booting on prod and on every self-hoster who already has one of these
-- collisions sitting in their table.
--
-- Terminalize all but the newest in-flight row per target as 'skipped' before
-- the index is created, so CREATE UNIQUE INDEX always succeeds. "Newest" is
-- (created_at, id) DESC — the same composite tiebreaker used elsewhere in the
-- project for created_at-ordered comparisons, since a burst of same-instant
-- inserts shares created_at and a bare created_at comparison would not
-- totally order them. The UPDATE is naturally idempotent/safe to re-run: once
-- no duplicates remain, the EXISTS predicate matches nothing and it is a
-- no-op; wrapped in a DO $$ block to keep this migration's style consistent
-- with the rest of the file (mirrors m36's idempotent-migration convention).
DO $$
BEGIN
    UPDATE update_tasks u
    SET status = 'skipped', finished_at = now(), updated_at = now(),
        detail = 'skipped: superseded by a newer duplicate in-flight task for the same target (m88 dedup)'
    WHERE u.status IN ('pending', 'running')
      AND EXISTS (
          SELECT 1 FROM update_tasks v
          WHERE v.tenant_id = u.tenant_id
            AND v.site_id = u.site_id
            AND v.target_type = u.target_type
            AND v.target_slug = u.target_slug
            AND v.status IN ('pending', 'running')
            AND (v.created_at, v.id) > (u.created_at, u.id)
      );
END $$;

-- Plain CREATE INDEX (the migration runner wraps each migration in a tx, so
-- CONCURRENTLY is unavailable — see m85); IF NOT EXISTS makes it re-run safe.

CREATE UNIQUE INDEX IF NOT EXISTS update_tasks_inflight_target_idx ON update_tasks
    (tenant_id, site_id, target_type, target_slug)
    WHERE status IN ('pending', 'running');
