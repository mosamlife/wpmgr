-- m138: font_transcode_results rows are removed with their site and with their
-- organisation.
--
-- THE CONTRACT
--
-- A font_transcode_results row belongs to exactly one organisation (tenant_id)
-- and names one site (site_id). From this migration on, the database removes
-- the row when either is deleted:
--
--   * deleting the site (DELETE FROM sites, which is what DELETE /sites/{id}
--     runs through internal/site's repository) removes every row naming it;
--   * purging the organisation (admin_purge_tenant, and admin_delete_empty_tenant
--     for an empty one) removes every row it owns, in the same statement that
--     removes the tenants row.
--
-- This is the same declaration font_results (m55) carries: two plain foreign
-- keys, font_transcode_results_tenant_fk -> tenants(id) and
-- font_transcode_results_site_fk -> sites(id), each ON DELETE CASCADE.
--
-- One consequence callers should know. The row is keyed per organisation
-- (PRIMARY KEY (source_hash, tenant_id)) and records the site that first asked
-- for the font. When that site is deleted the row goes with it even if another
-- site in the same organisation uses the same font file; that site's next
-- request finds no row and asks for the font again, exactly as it would for a
-- font it had never seen.
--
-- A write that names a site or an organisation that no longer exists is now
-- refused with SQLSTATE 23503 (foreign_key_violation) instead of being stored.
--
-- WHAT THIS DOES NOT DO
--
-- It removes database rows only. The WOFF2 object a row names in woff2_key is
-- not touched: object-storage reclamation is a separate mechanism and this
-- migration neither adds to it nor relies on it.
--
-- RLS is unchanged. tenant_isolation, agent_access and
-- font_transcode_results_site_scope (m54, m132) stay exactly as they are.
-- The cascade is carried out by PostgreSQL's referential-integrity machinery,
-- the same way the font_results cascade already is, and needs no policy change.
--
-- THE ORDER OF OPERATIONS, AND THE LOCKS
--
-- internal/db/migrate.go applies this file in ONE transaction, at boot, while
-- other instances may still be serving, so every lock below is held until the
-- file commits.
--
--   0. Bound every wait: set lock_timeout and statement_timeout for this
--      transaction only. See THE TIMEOUTS below for the values and what
--      happens when one fires.
--
--   1. LOCK tenants, sites, font_transcode_results IN SHARE ROW EXCLUSIVE MODE.
--      This is the lock ADD CONSTRAINT ... FOREIGN KEY takes on the table it
--      alters and on the table it references, so it is no stronger than what
--      step 4 would take anyway; it is only taken earlier. Taking it first is
--      what makes steps 2 to 4 one consistent picture: without it a site
--      deleted, or a row inserted, between the clean-up and the ADD CONSTRAINT
--      would leave a row the constraint forbids. What happens next depends on
--      the role that runs this file. As a superuser, the validation sees the
--      row, fails, and the control plane fails to boot. As a non-superuser
--      owner, the validation is subject to FORCE ROW LEVEL SECURITY and does
--      not see the row (see step 2), so the constraint is recorded as
--      validated while the row it forbids stays in the table, and nothing
--      reports it. The lock rules out both. SHARE ROW EXCLUSIVE blocks writes
--      to the three tables and does not block reads.
--      The order (tenants, sites, then the child) is the order an organisation
--      purge's cascade takes them in, so the two cannot deadlock on each other.
--
--   2. Remove the rows that name an organisation or a site that no longer
--      exists. They are the rows the constraints would refuse. A soft-deleted
--      organisation (tenants.deleted_at set) still has its tenants row, so its
--      rows are kept here and go when the purge removes the organisation.
--      font_transcode_results and sites are both FORCE ROW LEVEL SECURITY, and
--      this file runs as the owner, so app.agent is set to 'on' for this
--      transaction (the permissive agent_access / sites_agent policies) and
--      app.site_scope is cleared (the restrictive *_site_scope policies pass
--      when it is not 'on'). Without that the owner sees no rows at all, the
--      DELETE removes nothing, and step 4 does NOT fail: run as a
--      non-superuser owner on PostgreSQL 16, the constraints are recorded as
--      validated while the rows they forbid are still in the table. So this
--      step is the only thing that removes those rows; do not drop the
--      settings on the grounds that validation would catch it. Both settings
--      are restored before the block ends.
--
--   3. Index site_id, so the cascade on a site delete finds that site's rows
--      without reading the whole table. The existing index leads with
--      tenant_id and serves the organisation cascade already.
--
--   4. Add the two constraints, VALIDATED, one statement each.
--
-- WHY NOT `NOT VALID` + `VALIDATE CONSTRAINT`. The pair only lowers the lock
-- when the two halves run in separate transactions, and migrate.go runs a file
-- as one (m130 DECISION 9 sets this out in full). Here the lock is already
-- held from step 1, so the split would buy nothing and leave a constraint that
-- is briefly invalid. The validation reads font_transcode_results once per
-- constraint and probes the referenced primary key for each row; the table
-- holds one row per distinct font file per organisation.
--
-- THE TIMEOUTS
--
-- The file opens by setting lock_timeout = '2s' and statement_timeout = '10s'
-- with set_config(..., true), which is SET LOCAL: both are scoped to this
-- file's transaction, end with it, and reach no other migration and no other
-- connection.
--
-- Steps 0 and 1 are written as DO blocks rather than as bare SET LOCAL and
-- LOCK TABLE so that the file also runs statement by statement, outside a
-- transaction block, as schema tooling does when it replays migrations into a
-- throwaway database. There each statement is its own transaction, so the
-- settings and the lock end with the statement that took them, and the file
-- carries no locking guarantee; its guarantees are made under migrate.go's
-- single transaction only. Under that transaction the two forms are
-- equivalent: the settings and the lock hold until the file commits. The
-- settings are in their own block, ahead of the lock's, because a statement's
-- statement_timeout is fixed when the statement starts; the separate block is
-- what puts step 1 under the ten-second bound as well as the two-second one.
--
-- lock_timeout bounds step 1. LOCK takes the three tables one after another;
-- while it waits for one it already holds the ones before it, and every write
-- that arrives for the table it is waiting on queues behind it. With no bound,
-- one open transaction that has written a sites row would stall every write
-- to tenants and to sites on the running control plane for as long as that
-- transaction stayed open. PostgreSQL applies lock_timeout to each lock
-- acquisition separately, so the three together hold writers back for at
-- most six seconds. Ordinary transactions that write these tables are short;
-- two seconds lets a boot wait those out and refuses to wait on anything
-- longer, a long-running job or a session left idle in a transaction, which
-- is exactly the stall this bound exists to prevent.
--
-- statement_timeout bounds each statement once the locks are held, when every
-- write to the three tables waits on this file. Steps 2 to 4 each read
-- font_transcode_results, which holds one row per distinct font file per
-- organisation, and probe a primary key per row. Ten seconds is ample for
-- that, and if a statement ever needs longer, failing the migration is the
-- intended outcome rather than holding writes to tenants and sites for longer.
-- The timeout also counts lock waits, and step 1's worst case sits inside it.
--
-- WHEN A TIMEOUT FIRES. The statement fails (SQLSTATE 55P03 for the lock
-- wait, 57014 for the statement), the transaction rolls back, nothing in this
-- file persists, and schema_migrations gets no row for it. cmd/wpmgr applies
-- migrations before it listens, so the process exits without serving: a new
-- revision never becomes ready, and the instances already serving keep
-- serving (on Cloud Run the deploy fails and traffic stays on the previous
-- revision). The next boot runs this file again from the start, and since
-- every step is safe to repeat (below), a later boot, once the long
-- transaction has ended, applies it.
--
-- CONVERGENCE AND IDEMPOTENCY
--
-- No earlier version of this change was ever applied anywhere, so there is no
-- converge path to carry. Every step is safe to run again: the clean-up finds
-- nothing on a second run, the index is IF NOT EXISTS, and each constraint is
-- added only when a constraint of that name is absent. A database built from a
-- fresh schema reaches the same end state as one migrated here.

DO $$
BEGIN
    PERFORM set_config('lock_timeout', '2s', true);
    PERFORM set_config('statement_timeout', '10s', true);
END;
$$;

DO $$
BEGIN
    LOCK TABLE "public"."tenants", "public"."sites", "public"."font_transcode_results"
        IN SHARE ROW EXCLUSIVE MODE;
END;
$$;

DO $$
DECLARE
    v_prev_agent      text := current_setting('app.agent', true);
    v_prev_site_scope text := current_setting('app.site_scope', true);
    v_removed         bigint;
BEGIN
    PERFORM set_config('app.agent', 'on', true);
    PERFORM set_config('app.site_scope', '', true);

    DELETE FROM "public"."font_transcode_results" r
     WHERE NOT EXISTS (SELECT 1 FROM "public"."tenants" t WHERE t.id = r.tenant_id)
        OR NOT EXISTS (SELECT 1 FROM "public"."sites"   s WHERE s.id = r.site_id);
    GET DIAGNOSTICS v_removed = ROW_COUNT;
    RAISE NOTICE 'm138: removed % font_transcode_results row(s) naming a deleted site or organisation', v_removed;

    PERFORM set_config('app.agent', coalesce(v_prev_agent, ''), true);
    PERFORM set_config('app.site_scope', coalesce(v_prev_site_scope, ''), true);
END;
$$;

CREATE INDEX IF NOT EXISTS "font_transcode_results_site_idx"
    ON "public"."font_transcode_results" ("site_id");

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint c
        JOIN pg_class t ON t.oid = c.conrelid
        JOIN pg_namespace n ON n.oid = t.relnamespace
        WHERE n.nspname = 'public'
          AND t.relname = 'font_transcode_results'
          AND c.conname = 'font_transcode_results_tenant_fk'
    ) THEN
        ALTER TABLE "public"."font_transcode_results"
            ADD CONSTRAINT "font_transcode_results_tenant_fk"
            FOREIGN KEY ("tenant_id") REFERENCES "public"."tenants" ("id") ON DELETE CASCADE;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint c
        JOIN pg_class t ON t.oid = c.conrelid
        JOIN pg_namespace n ON n.oid = t.relnamespace
        WHERE n.nspname = 'public'
          AND t.relname = 'font_transcode_results'
          AND c.conname = 'font_transcode_results_site_fk'
    ) THEN
        ALTER TABLE "public"."font_transcode_results"
            ADD CONSTRAINT "font_transcode_results_site_fk"
            FOREIGN KEY ("site_id") REFERENCES "public"."sites" ("id") ON DELETE CASCADE;
    END IF;
END;
$$;
