-- m147 - install_owner: the durable record of the account that set up this
-- install.
--
-- WHAT IT GUARANTEES. After this file, install_owner holds at most one row,
-- naming the user who ran first-run bootstrap and the organisation bootstrap
-- created with it. wpmgr_app may read the row and may insert it once; no
-- statement the control plane issues can change it, delete it or empty the
-- table. PRIVILEGES below says what that does not cover. The control plane
-- records the row inside the bootstrap transaction from now on; this file
-- backfills it for installs bootstrapped before that.
--
-- WHY A TABLE. Until now nothing durable named that account. The only proof is
-- the audit_log row bootstrap writes: action 'auth.register', actor_id =
-- target_id = the user, metadata {"bootstrap": true}. No other writer uses the
-- "bootstrap" key. instance_settings is the wrong home: its values are
-- encrypted, so reading this fact would need the instance key.
--
-- NO FOREIGN KEYS, ON PURPOSE. Deleting the user or the organisation leaves
-- the row naming an id that no longer exists. That dangling row is the record
-- that the authority ended with the account and nobody inherits it. A cascade
-- or SET NULL would also need UPDATE or DELETE on this table, which wpmgr_app
-- does not hold, so it would make every user or organisation delete fail with
-- 42501 on an install whose migrator is wpmgr_app.
--
-- NO ROW LEVEL SECURITY, ON PURPOSE. The row is install-global, like users,
-- tenants and system_audit_log: it belongs to no tenant and no site, so there
-- is no tenant or site to scope it to. The privilege set below is the guard.
--
-- PRIVILEGES. m1's default privileges grant wpmgr_app SELECT, INSERT, UPDATE
-- and DELETE on every new table; SELECT and INSERT are granted again here by
-- name so they hold whichever role runs this file. UPDATE, DELETE and
-- TRUNCATE are revoked, and the singleton primary key refuses a second row.
-- The REVOKE binds the statements the control plane issues, and it does so
-- on single-DSN installs too, where wpmgr_app also owns the table. It does
-- not bind a role that owns the table and acts on purpose: an owner can give
-- itself back any privilege on its own table, so an operator running SQL by
-- hand as the owner can still change the row. That is no wider than the trust
-- the owner already holds, since the same role can turn off row level
-- security on every table it owns.
--
-- BACKFILL. The EARLIEST bootstrap audit row that still exists is taken first
-- (LIMIT 1), and only then is its actor id checked. Its user is recorded even
-- if that user has since been deleted, so a later account does not inherit
-- the authority from a deleted first account whose bootstrap row survives.
-- The row has to survive, though. If the first organisation was purged before
-- the upgrade to this version (purging clears its audit_log) and the install
-- was bootstrapped again, the later account is recorded either way: by this
-- backfill when the re-bootstrap came before the upgrade, or by that setup
-- into the empty table when it came after. If the purge came after the
-- upgrade, the row already exists and a re-bootstrap records nothing, because
-- bootstrap inserts with ON CONFLICT DO NOTHING. What decides the outcome is
-- when the purge happened relative to the upgrade, not when the re-bootstrap
-- happened.
-- If the earliest row's actor id is not a uuid, or there is no bootstrap row
-- at all (it was never written, or it went with the first organisation:
-- deleting or purging an organisation clears its audit_log), nothing is
-- recorded. An empty table is filled by the next successful setup, which
-- records its account. Until then the install-owner arm admits nobody.
-- Superadmins (WPMGR_SUPERADMIN_EMAILS) are admitted by their own arm, not
-- this one, and are the remedy in the meantime.
--
-- audit_log is FORCE ROW LEVEL SECURITY, and the production migrator is a
-- NOSUPERUSER NOBYPASSRLS table owner with no app.* setting in scope, so a
-- bare read sees no rows at all. The read runs with app.agent set to 'on' for
-- this transaction only, which audit_log_agent (m92) admits for SELECT, and the
-- previous value is restored straight after.
--
-- IDEMPOTENT. The table and grants are no-ops the second time, and the insert
-- is ON CONFLICT DO NOTHING, so a row already present, from bootstrap or an
-- earlier run, is never replaced.
--
-- ORDINAL. 20260910000000 sorts after every migration it reads from (m1's
-- audit_log and tenants, m92's audit_log_agent policy).

CREATE TABLE IF NOT EXISTS "public"."install_owner" (
  "singleton"   boolean     NOT NULL DEFAULT true,
  "user_id"     uuid        NOT NULL,
  "tenant_id"   uuid        NOT NULL,
  "source"      text        NOT NULL,
  "recorded_at" timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT "install_owner_pkey" PRIMARY KEY ("singleton"),
  CONSTRAINT "install_owner_singleton_check" CHECK ("singleton"),
  CONSTRAINT "install_owner_source_check" CHECK ("source" IN ('bootstrap', 'backfill'))
);

GRANT SELECT, INSERT ON "public"."install_owner" TO wpmgr_app;
REVOKE UPDATE, DELETE, TRUNCATE ON "public"."install_owner" FROM wpmgr_app;

DO $$
DECLARE
    prev_agent text := current_setting('app.agent', true);
BEGIN
    PERFORM set_config('app.agent', 'on', true);

    INSERT INTO "public"."install_owner" ("singleton", "user_id", "tenant_id", "source")
    SELECT true, first_row.owner_id, first_row.tenant_id, 'backfill'
    FROM (
        SELECT CASE
                   WHEN earliest.actor_id ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                   THEN earliest.actor_id::uuid
               END AS owner_id,
               earliest.tenant_id
        FROM (
            SELECT a.actor_id, a.tenant_id
            FROM "public"."audit_log" a
            WHERE a.action = 'auth.register'
              AND a.metadata->>'bootstrap' = 'true'
              AND a.actor_id = a.target_id
            ORDER BY a.created_at, a.id
            LIMIT 1
        ) earliest
    ) first_row
    WHERE first_row.owner_id IS NOT NULL
    ON CONFLICT ("singleton") DO NOTHING;

    PERFORM set_config('app.agent', coalesce(prev_agent, ''), true);
END;
$$;
