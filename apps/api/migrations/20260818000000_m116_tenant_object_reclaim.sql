-- m116 - GH #408 finding 1: a durable record of the object-storage work that a
-- TENANT delete leaves behind, written in the SAME TRANSACTION as the delete.
--
-- This is m113 one level up and one table across, and it is deliberately the
-- same shape, because the defect is the same defect.
--
-- WHAT IS BROKEN
--
-- backup_chunks.tenant_id is ON DELETE CASCADE (m4). admin_delete_empty_tenant
-- hard-deletes the tenants row and frees ZERO object storage, so the same
-- statement destroys the entire chunk inventory for that tenant. After it,
-- chunks/<tenant_id>/ holds objects that nothing anywhere names: not the
-- retention collector, whose roster is derived from backup_chunks and
-- backup_snapshots, and not an operator, who has no id to work from. Delete an
-- organisation's last site and then the emptied organisation, and its chunk
-- ciphertext is stranded permanently. m113 said this gap was GH #408 and left it
-- open on purpose so its own mechanism could ship.
--
-- Two callers reach admin_delete_empty_tenant, and both are "Lane A":
--
--   DELETE /orgs/{orgId} for an org with zero sites and no other members
--   (internal/org/delete_handler.go), and
--
--   the superadmin orphan cleanup, which removes an org whose sole member's
--   user row was just deleted (internal/admin.Repo.DeleteEmptyTenant).
--
-- WHY THE RECORD, AND NOT A SWEEP IN LANE A
--
-- Lane B, the grace-window org.PurgeWorker, already deletes all seven
-- tenant-scoped object roots before its hard delete, and its own comments say
-- the DB cascade frees no storage. Making Lane A do the same inline was
-- considered and rejected on three separate grounds:
--
--   1. m113 already rejected it on the record: unbounded object-storage network
--      I/O inside an HTTP request, with no resume. A 100 GB tenant is roughly
--      25,600 serial deletes, because chunks target 4 MiB and blobstore has no
--      batch delete and no concurrency. That is about 21 minutes at 50 ms per
--      round trip.
--   2. It self-deadlocks. The existing mark-and-sweep's SweepTenantChunks
--      acquires its OWN pooled connection, because its advisory lock must be
--      session-scoped, so it cannot join Lane A's open transaction; its
--      per-chunk SELECT ... FOR UPDATE then blocks on rows the caller's own
--      uncommitted cascade has already deleted. Measured: "canceling statement
--      due to statement timeout / CONTEXT: while locking tuple in relation
--      backup_chunks". Postgres cannot break that cycle, because one side of it
--      is the application.
--   3. Routing Lane A into Lane B instead is wrong on correctness, not taste.
--      Both Lane A callers arrive with zero or about-to-be-zero memberships, so
--      a soft delete would leave an invisible tenant with no owner able to
--      restore it for the whole grace window. delete_handler.go already refuses
--      exactly that outcome by name.
--
-- So the record simply outlives the tenant, and an async worker drains the
-- storage afterwards with every guard re-established at drain time.
--
-- WHY THERE IS NO FOREIGN KEY, AND WHY THAT MUST STAY SO
--
-- The absent tenant_id FK is the entire point, and it is the m113 lesson
-- verbatim: a cascade onto a record of cleanup work destroys it in the exact
-- operation it exists to survive. m113's first version carried that FK and
-- adversarial review proved it reinstated the bug. Do not "tidy up" the
-- parentless row. TestGH408_TenantReclaimRecordSurvivesTheCascade exists to fail
-- loudly if someone does.
--
-- WHY THE INSERT IS IN THIS FUNCTION'S BODY AND NOT IN A GO CALLER
--
-- Two reasons, and the second is not obvious.
--
--   * There are two Lane A callers. One of them can be forgotten; a function
--     body cannot.
--   * The function ALREADY does PERFORM set_config('app.agent','on',true) (m91,
--     Security review Finding A) and restores it at its single return point. So
--     the INSERT already runs under exactly the GUC the _agent policy keys on,
--     whatever the caller set. Putting it in a Go caller would make the write
--     depend on that caller's transaction scope.
--
-- It is gated on GET DIAGNOSTICS ROW_COUNT, so the record exists if and only if
-- the tenants row really went. There is no ordering in which one exists without
-- the other: crash before commit, neither; crash after, both. An INSERT failure
-- aborts the tenant delete, and that is intended, m113's "deliberately fatal to
-- the transaction". Committing the delete without the record IS the reported
-- bug and is unrecoverable afterwards; a failed request is recoverable by
-- retrying.
--
-- WHY THE ROW STORES IDENTITY AND NOT A PREFIX STRING
--
-- One uuid, never a prefix. The worker derives every root from a code constant
-- plus one validated UUID, sharing org.ObjectStoragePrefixes with Lane B so the
-- two can never disagree about the root set. Storing a prefix would turn a
-- corrupt row into an arbitrary-prefix delete instruction, and the adjacency
-- here is one character: "tenant/" holds backup manifests, "tenants/" holds
-- white-label client report PDFs with client PII.
--
-- WHY THE next_attempt_at FLOOR IS 24 HOURS
--
-- Defence in depth, NOT the safety proof. The proof is in the worker's header
-- and its guards. The floor buys an operator who deleted the wrong organisation
-- a day to restore the database from a pre-delete dump before the bytes go.
-- Lane B effectively has seven days of grace; Lane A has none today.
--
-- WHY THERE IS NO SITE-SCOPE POLICY ON THIS TABLE
--
-- This is deliberate and is not the omission m112 was about. m113 needed the
-- RESTRICTIVE site-scope policy because ITS rows NAME OTHER SITES: a
-- collaborator invited to one site could otherwise reach reclaim rows naming
-- every other site in the organisation. This table has no site_id column at all,
-- so there is no site name in it for anyone to reach. tenant_isolation is kept
-- even though it is effectively vestigial here (these rows' tenants are gone by
-- construction, so no InTenantTx caller can ever match one) because a
-- tenant_id column with no isolation policy is the exact pattern the house rule
-- forbids, and because a future reader must not have to reconstruct this
-- argument to know it was made.
--
-- IDEMPOTENCE AND BOOT SAFETY
--
-- internal/db/migrate.go applies these on boot, in lexical order, each in its
-- own transaction, and a failure here takes the control plane down. Every
-- statement is IF NOT EXISTS guarded (CREATE POLICY has no IF NOT EXISTS in
-- Postgres 16, so each policy is wrapped in a pg_policies existence check, the
-- m94 pattern). Nothing is dropped, no existing data is touched. The function is
-- CREATE OR REPLACE with an unchanged name and signature, which is how a fixed
-- body reaches a database that already ran m35 and m91: editing an applied
-- migration file changes nothing, because migrate.go skips a version already in
-- schema_migrations. m114's header documents that mistake being made here once
-- already. The table and the function are in ONE file, and therefore one
-- transaction, so the function can never exist without the table it writes to.
-- Vendor-neutral: no extension, no server-version-specific syntax, no trigger
-- (updated_at is set with now() in SQL, matching the m36 note that this schema
-- has no set_updated_at()).

-- ---------------------------------------------------------------------------
-- tenant_object_reclaim
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."tenant_object_reclaim" (
        "id"              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
        -- NOT a foreign key, and that is the entire point. See the header.
        "tenant_id"       uuid        NOT NULL,
        -- Which tenant-scoped storage set to reclaim. 'tenant_storage' means all
        -- seven roots org.ObjectStoragePrefixes returns, which is every root
        -- Lane B sweeps, including chunks/<tenant>/. The set is CLOSED by the
        -- constraint below for the same reason m113/m115 closed its own: a task
        -- carrying a kind the worker cannot act on reclaims nothing while being
        -- the only record naming those objects. backup.TenantReclaimKinds is the
        -- code-side copy; tests/contract compares the two.
        "kind"            text        NOT NULL DEFAULT 'tenant_storage'
            CONSTRAINT "tenant_object_reclaim_kind_check" CHECK ("kind" IN ('tenant_storage')),
        "attempts"        int         NOT NULL DEFAULT 0,
        -- The 24 hour floor. Defence in depth, not the proof. See the header.
        "next_attempt_at" timestamptz NOT NULL DEFAULT now() + interval '24 hours',
        "last_error"      text,
        "completed_at"    timestamptz,
        "created_at"      timestamptz NOT NULL DEFAULT now(),
        "updated_at"      timestamptz NOT NULL DEFAULT now()
    );
END;
$$;

-- One task per (tenant, kind), and the arbiter the enqueue's ON CONFLICT clause
-- names. That clause REOPENS the row rather than doing nothing, for the reason
-- m113 spells out: DO NOTHING against an already completed row silently drops
-- new work, which is the failure this table exists to remove.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND tablename = 'tenant_object_reclaim'
          AND indexname = 'tenant_object_reclaim_tenant_kind_key'
    ) THEN
        CREATE UNIQUE INDEX "tenant_object_reclaim_tenant_kind_key"
            ON "public"."tenant_object_reclaim" ("tenant_id", "kind");
    END IF;
END;
$$;

-- The worker's due query. Partial on the open tasks so the index stays small
-- once the common case (everything reclaimed) dominates the table.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND tablename = 'tenant_object_reclaim'
          AND indexname = 'tenant_object_reclaim_due_idx'
    ) THEN
        CREATE INDEX "tenant_object_reclaim_due_idx"
            ON "public"."tenant_object_reclaim" ("next_attempt_at")
            WHERE "completed_at" IS NULL;
    END IF;
END;
$$;

DO $$
BEGIN
    ALTER TABLE "public"."tenant_object_reclaim" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."tenant_object_reclaim" FORCE ROW LEVEL SECURITY;
END;
$$;

-- Tenant path. Effectively vestigial by construction, and kept on purpose: see
-- the header. A tenant-scoped column with no isolation policy is the pattern the
-- house rule forbids, whatever the row lifecycle happens to make reachable.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'tenant_object_reclaim'
          AND policyname = 'tenant_object_reclaim_tenant_isolation'
    ) THEN
        CREATE POLICY "tenant_object_reclaim_tenant_isolation" ON "public"."tenant_object_reclaim"
            USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END;
$$;

-- Worker and operator path. The drain is cross-tenant by nature and runs under
-- InAgentTx; so does the enqueue inside admin_delete_empty_tenant (m91 sets
-- app.agent='on' in-body), and so does `wpmgr-cli reclaim`. FOR ALL with BOTH
-- USING and WITH CHECK, matching site_object_reclaim_agent.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'tenant_object_reclaim'
          AND policyname = 'tenant_object_reclaim_agent'
    ) THEN
        CREATE POLICY "tenant_object_reclaim_agent" ON "public"."tenant_object_reclaim"
            USING (current_setting('app.agent', true) = 'on')
            WITH CHECK (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;

-- The m1 ALTER DEFAULT PRIVILEGES grant covers a table created by the migration
-- owner, but a deployment whose migration role differs from the m1 role would
-- silently leave the app role unable to read its own reclaim queue. Stated
-- explicitly rather than inherited, and idempotent.
GRANT SELECT, INSERT, UPDATE ON "public"."tenant_object_reclaim" TO "wpmgr_app";

-- ---------------------------------------------------------------------------
-- admin_delete_empty_tenant: record the work, in the delete's own transaction
-- ---------------------------------------------------------------------------
--
-- Identical to the m91 body (which itself carried the m35 body forward with the
-- app.agent save/restore fix) except for the single INSERT gated on v_result.
-- The GUC discipline is preserved unchanged: v_prev_agent is captured before the
-- in-body set_config and restored on the ONLY return path, because
-- set_config(..., true) is scoped to the CALLER's transaction and an unrestored
-- 'on' would disable the tenant check for every statement that follows in it.
CREATE OR REPLACE FUNCTION "public"."admin_delete_empty_tenant"(p_tenant_id uuid)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_count integer;
    v_result boolean := false;
    v_prev_agent text := current_setting('app.agent', true);
BEGIN
    PERFORM set_config('app.agent', 'on', true);
    IF NOT (
        EXISTS (SELECT 1 FROM memberships m WHERE m.tenant_id = p_tenant_id)
        OR EXISTS (SELECT 1 FROM sites s WHERE s.tenant_id = p_tenant_id)
    ) THEN
        PERFORM set_config('app.tenant_id', p_tenant_id::text, true);
        DELETE FROM audit_log WHERE tenant_id = p_tenant_id;
        PERFORM set_config('app.tenant_id', '', true);
        DELETE FROM tenants t WHERE t.id = p_tenant_id;
        GET DIAGNOSTICS v_count = ROW_COUNT;
        v_result := v_count > 0;

        -- GH #408. The cascade above has just destroyed backup_chunks for this
        -- tenant, which was the only inventory naming chunks/<tenant_id>/, and
        -- this statement frees no object storage whatsoever. This row is now the
        -- only surviving name for that storage. It is written HERE, in the same
        -- transaction and gated on the delete having actually happened, so it
        -- exists if and only if the tenant row is really gone. A failure here
        -- aborts the delete on purpose: a committed delete with no record is the
        -- bug, and is unrecoverable; a failed request is not.
        IF v_result THEN
            INSERT INTO tenant_object_reclaim (tenant_id)
            VALUES (p_tenant_id)
            ON CONFLICT (tenant_id, kind) DO UPDATE
            SET completed_at    = NULL,
                attempts        = 0,
                next_attempt_at = now() + interval '24 hours',
                last_error      = NULL,
                updated_at      = now();
        END IF;
    END IF;
    PERFORM set_config('app.agent', coalesce(v_prev_agent, ''), true);
    RETURN v_result;
END;
$$;
REVOKE ALL ON FUNCTION "public"."admin_delete_empty_tenant"(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION "public"."admin_delete_empty_tenant"(uuid) TO "wpmgr_app";
