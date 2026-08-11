-- m113 - GH #402: a durable record of the object-storage work that a site
-- delete leaves behind, written in the SAME TRANSACTION as the delete.
--
-- WHAT WAS BROKEN
--
-- DELETE /sites/{id} was a bare `DELETE FROM sites`. backup_snapshots.site_id
-- is ON DELETE CASCADE, so the same statement destroyed every snapshot row for
-- that site, and those rows were the only database record naming the site's
-- per-snapshot manifest.json objects in storage. Both deleters of that object
-- (deleteSnapshotCore and the retention GC's metadata prune) need a live
-- snapshot row to find the key, and the GC's site roster is itself derived from
-- backup_snapshots, so after the cascade nothing could ever name those objects
-- again. A site with 90 completed snapshots left 90 orphans, permanently.
--
-- Chunks are a different story and deliberately need no help here.
-- backup_chunks has NO foreign key to sites, only to tenants, so the cascade
-- never touches it. It remains the complete tenant-wide inventory, and the
-- ADR-050 mark-and-sweep recomputes reachability over the SURVIVING snapshots,
-- so a deleted site's exclusive chunks are already reclaimed and chunks still
-- shared with a live site are already spared. Nothing in this migration or its
-- worker adds any authority to delete a chunk.
--
-- WHY A TABLE AND NOT SYNCHRONOUS DELETION
--
-- 90 snapshots is 90 individual object deletes; blobstore has no batch delete.
-- That is unbounded network work inside an HTTP request, and worse it has no
-- resume: a crash after the DB commit loses the work forever, because the
-- cascade has already destroyed the rows that described it.
--
-- WHY THE ROW IS WRITTEN IN THE DELETE'S OWN TRANSACTION
--
-- The obvious alternative, writing the record in an EARLIER separate
-- transaction "before the cascade fires", is unsafe: if that commits and the
-- delete then rolls back, the database holds a durable instruction to delete
-- the manifests of a site that is still live. Same-transaction insert closes
-- that window completely. The reclaim record exists if and only if the site row
-- is actually gone; it cannot be lost independently of the thing it describes
-- and it cannot be spuriously present.
--
-- WHY THERE IS NO FOREIGN KEY TO EITHER PARENT, AND WHY THAT MUST STAY SO
--
-- A site_id FK with ON DELETE CASCADE would destroy this row in the very
-- statement it exists to survive, reinstating the bug exactly.
--
-- A tenant_id FK with ON DELETE CASCADE is the SAME mistake one level up, and
-- the first version of this migration had it. Adversarial review proved it
-- against real Postgres: seed a tenant, a site, a completed snapshot and its
-- manifest object; delete the site (the reclaim row is written and present);
-- then call admin_delete_empty_tenant on the now-empty tenant. It returns true,
-- the reclaim row count drops to zero, and the manifest object is still sitting
-- in the bucket. GH #402 came straight back at tenant level.
--
-- The reason is a real asymmetry between the two tenant-delete lanes:
--
--   Lane B, the grace-window org.PurgeWorker, DOES sweep all seven
--   tenant-scoped object roots (including tenant/<id>/, which contains every
--   one of these prefixes) and only then calls admin_purge_tenant. For that
--   lane a cascading reclaim row would have been harmless.
--
--   Lane A, admin_delete_empty_tenant, sweeps NOTHING. It is reached from
--   DELETE /orgs/{orgId} for an org with zero sites and zero other members, and
--   from the superadmin orphan cleanup (internal/admin.Repo.DeleteEmptyTenant).
--   Both hard-delete the tenant row inside a request transaction. Its guard is
--   "no memberships and no sites", which was read as "owns no objects"; an org
--   whose sites were all deleted first satisfies that guard and owns objects.
--   Making Lane A sweep first would mean unbounded object-storage network I/O
--   inside an HTTP request with no resume, which is the exact thing this table
--   exists to avoid; routing Lane A through Lane B would put a seven-day grace
--   window on deleting an empty org. Neither is a trade worth making for a
--   record whose only job is to be durable.
--
-- So the record simply outlives both parents. A record of what to clean up must
-- survive the deletion it describes; a cascade onto it is a contradiction in
-- terms. The tenant row being absent when the worker runs is a RECLAIM signal,
-- not an error, and the worker's tenant-state guard says so explicitly.
-- TestGH402_ReclaimTaskSurvivesSiteCascade and
-- TestGH402_ReclaimTaskSurvivesTenantHardDelete exist to fail loudly if someone
-- "tidies this up" later.
--
-- The cost is a row with no referential parent. That is the intended cost: the
-- row is small, it self-completes on the next sweep, and the alternative is
-- losing the only surviving name of a bucket prefix.
--
-- WHY THE ROW STORES IDENTITY AND NOT A PREFIX STRING
--
-- (tenant_id, site_id, kind) only. The worker derives the storage prefix from a
-- code constant plus two validated UUIDs. Storing a prefix would turn a corrupt
-- row into an arbitrary-prefix delete instruction, and the adjacency here is one
-- character: "tenant/" holds backup manifests, "tenants/" holds white-label
-- client report PDFs with client PII. internal/org/purge_worker.go already
-- documents that trap.
--
-- kind is the extension point. rucss/<tenant>/<site>/ and
-- screenshots/<tenant>/<site>/ are also site-scoped storage roots and can reuse
-- this engine later without inventing a second mechanism.
--
-- destination_kind is recorded for operator diagnosis only, never a credential.
-- It does NOT decide whether a prefix is swept. Manifest index objects always
-- live in the control-plane bucket, whatever a site's backup destination is
-- (cmd/wpmgr/main.go wires SetIndexPutter to the control-plane store), so every
-- deleted site's manifests are in scope here. What a customer-owned destination
-- holds is the backup PAYLOAD, which this worker never reaches into and has no
-- credentials for; the log line should be able to say so.
--
-- GIVE-UP SEMANTICS
--
-- Past the retry cap the row is LEFT VISIBLE, never deleted. A stuck task is
-- the only remaining record that those objects exist, so deleting it on give-up
-- would recreate the bug this migration exists to fix. That is the GH #256
-- lesson applied one layer up: prefer leaving it behind over guessing. The
-- sweep re-counts those rows every tick and logs them at Error with their
-- prefixes, so a stuck task is loud rather than merely retained.
--
-- WHAT THIS DOES NOT DO: OBJECTS ALREADY ORPHANED BEFORE m113
--
-- This table only ever receives rows from deletes that happen AFTER it exists.
-- Manifests orphaned by deletes that already happened have no row here and no
-- other record anywhere, so nothing reclaims them and the bytes stay on the
-- bill. Reclaiming those is a deliberate, operator-driven step, because the
-- only remaining evidence is the bucket itself.
--
-- The prefix to inspect, exactly, is:
--
--   tenant/<tenant_id>/site/<site_id>/
--
-- Note the literal "site/" segment in the middle; tenant/<tenant_id>/<site_id>/
-- matches nothing. And note the SINGULAR "tenant/": the plural "tenants/" root
-- holds white-label client report PDFs with client PII.
--
-- An operator can backfill a known-deleted site into this engine rather than
-- deleting by hand, which keeps every guard in the worker (including the
-- refusal to touch a site whose row still exists) in play:
--
--   INSERT INTO site_object_reclaim (tenant_id, site_id, kind)
--   VALUES ('<tenant_id>', '<site_id>', 'backup_manifest')
--   ON CONFLICT (tenant_id, site_id, kind) DO UPDATE
--     SET completed_at = NULL, attempts = 0, next_attempt_at = now();
--
-- Get the kind wrong in that statement and the database refuses the whole
-- INSERT, naming site_object_reclaim_kind_check. That is deliberate: a task
-- carrying a kind the worker cannot derive a prefix for is a task that reclaims
-- nothing, and nothing else in the database names those objects, so a typo
-- accepted here would strand them exactly as GH #402 did. The column default is
-- the correct value, so omitting kind entirely also works.
--
-- IDEMPOTENCE AND BOOT SAFETY
--
-- internal/db/migrate.go applies these on boot, in lexical order, each in its
-- own transaction, and a failure here takes the control plane down. Every
-- statement is IF NOT EXISTS guarded (CREATE POLICY has no IF NOT EXISTS in
-- Postgres 16, so each policy is wrapped in a pg_policies existence check, the
-- m94 pattern). Nothing is dropped, no existing data is touched, no column
-- changes. The worst a re-run can do is nothing. Vendor-neutral: no extension,
-- no server-version-specific syntax, no trigger (updated_at is set with now()
-- in SQL, matching the m36 note that this schema has no set_updated_at()).

-- ---------------------------------------------------------------------------
-- site_object_reclaim
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."site_object_reclaim" (
        "id"               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
        -- NEITHER of these is a foreign key. See the header: a cascade from
        -- either parent destroys the record in the operation it exists to
        -- survive, and for tenants that operation is admin_delete_empty_tenant.
        "tenant_id"        uuid        NOT NULL,
        "site_id"          uuid        NOT NULL,
        -- Which site-scoped storage root to reclaim. 'backup_manifest' is the
        -- only kind today, and the set is CLOSED by the constraint below.
        --
        -- The operator remedy in this header is a hand-written INSERT, so a typo
        -- in this column is a realistic event, and an expensive one: the worker
        -- cannot derive a prefix for a kind it does not know, and there is no
        -- other record anywhere of the objects that row was meant to reclaim.
        -- The constraint puts that failure in front of the person typing the
        -- statement, at the moment they can fix it. A database that predates the
        -- constraint gets it from m115 (NOT VALID, so it cannot fail a boot), and
        -- the worker treats an unknown kind as a retryable failure rather than a
        -- cancel so a row written before either still stays visible.
        --
        -- backup.ReclaimKinds is the code-side copy of this set; tests/contract
        -- compares the two, so a kind cannot be added to one half alone.
        "kind"             text        NOT NULL DEFAULT 'backup_manifest'
            CONSTRAINT "site_object_reclaim_kind_check" CHECK ("kind" IN ('backup_manifest')),
        -- 'cp' | 'local' | 's3_compat' | NULL when the site had no destination
        -- row (the legacy control-plane-global bucket). Diagnostic only.
        "destination_kind" text,
        "attempts"         int         NOT NULL DEFAULT 0,
        "next_attempt_at"  timestamptz NOT NULL DEFAULT now(),
        "last_error"       text,
        "completed_at"     timestamptz,
        "created_at"       timestamptz NOT NULL DEFAULT now(),
        "updated_at"       timestamptz NOT NULL DEFAULT now()
    );
END;
$$;

-- Converging a database that applied the PRE-REVIEW version of this file (which
-- did carry the tenant foreign key, and had no site-scope policy) is NOT done
-- here, and cannot be. internal/db/migrate.go skips any version already present
-- in schema_migrations, so editing this file in place changes nothing on the one
-- database that needs changing. That work is m114, a version no database has
-- applied yet. This file is now only ever read by a database that has never seen
-- it, so it simply describes the correct end state.

-- One task per (tenant, site, kind), and the arbiter the enqueue's ON CONFLICT
-- clause names. That clause REOPENS the row (completed_at back to NULL,
-- attempts back to 0) rather than doing nothing: DO NOTHING was the first
-- shape, and against an already completed row for the same key it silently
-- dropped new work, which is the failure this table exists to remove. An
-- operator backfill of an already-orphaned site relies on the same clause.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND tablename = 'site_object_reclaim'
          AND indexname = 'site_object_reclaim_site_kind_key'
    ) THEN
        CREATE UNIQUE INDEX "site_object_reclaim_site_kind_key"
            ON "public"."site_object_reclaim" ("tenant_id", "site_id", "kind");
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND tablename = 'site_object_reclaim'
          AND indexname = 'site_object_reclaim_tenant_idx'
    ) THEN
        CREATE INDEX "site_object_reclaim_tenant_idx"
            ON "public"."site_object_reclaim" ("tenant_id");
    END IF;
END;
$$;

-- The worker's due query. Partial on the open tasks so the index stays small
-- once the common case (everything reclaimed) dominates the table.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND tablename = 'site_object_reclaim'
          AND indexname = 'site_object_reclaim_due_idx'
    ) THEN
        CREATE INDEX "site_object_reclaim_due_idx"
            ON "public"."site_object_reclaim" ("next_attempt_at")
            WHERE "completed_at" IS NULL;
    END IF;
END;
$$;

DO $$
BEGIN
    ALTER TABLE "public"."site_object_reclaim" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."site_object_reclaim" FORCE ROW LEVEL SECURITY;
END;
$$;

-- Operator path: the INSERT rides inside DELETE /sites/{id}'s InTenantTx.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'site_object_reclaim'
          AND policyname = 'site_object_reclaim_tenant_isolation'
    ) THEN
        CREATE POLICY "site_object_reclaim_tenant_isolation" ON "public"."site_object_reclaim"
            USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END;
$$;

-- Worker path: the reclaim sweep is cross-tenant and runs under InAgentTx.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'site_object_reclaim'
          AND policyname = 'site_object_reclaim_agent'
    ) THEN
        CREATE POLICY "site_object_reclaim_agent" ON "public"."site_object_reclaim"
            USING (current_setting('app.agent', true) = 'on')
            WITH CHECK (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;

-- Collaborator path: the m19 AS RESTRICTIVE site-scope policy that every other
-- site-keyed table carries, including site_destinations in the same schema
-- hunk. Without it the only thing the database refuses is another TENANT, and a
-- collaborator invited to exactly one site could reach reclaim rows naming
-- every other site in the organisation.
--
-- m112 shipped one migration earlier for exactly this reason: the email tables
-- were the ones missing this policy, three review rounds closed seven
-- privilege-escalation doors in handlers, and the fourth round worked out that
-- they kept appearing because the database had no opinion. A new site-keyed
-- table that ships without the policy is the eighth door waiting to be found.
--
-- Unlike m112 this is a single FOR ALL policy, the plain m19 exemplar shape.
-- The email tables needed a read/write split because a site_id IS NULL
-- organisation row is a shipped feature there that inheritance depends on.
-- site_object_reclaim.site_id is NOT NULL, so there is no such row, nothing to
-- keep readable, and no reason to split.
--
-- RESTRICTIVE policies are AND-combined with the permissive ones above, so this
-- can only ever subtract. Both real writers leave app.site_scope unset (the
-- enqueue rides DELETE /sites/{id}'s InTenantTx, the worker runs InAgentTx), so
-- for them the first branch is a tautology and behaviour is unchanged.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'site_object_reclaim'
          AND policyname = 'site_object_reclaim_site_scope'
    ) THEN
        CREATE POLICY "site_object_reclaim_site_scope" ON "public"."site_object_reclaim"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
                )
            );
    END IF;
END;
$$;
