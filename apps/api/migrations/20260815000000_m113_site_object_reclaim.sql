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
-- WHY THERE IS NO FOREIGN KEY TO sites, AND WHY THAT MUST STAY THAT WAY
--
-- A site_id FK with ON DELETE CASCADE would destroy this row in the very
-- statement it exists to survive, reinstating the bug exactly. The FK is to
-- tenants only, so a tenant hard-purge (admin_purge_tenant, after the org purge
-- worker has already swept every tenant-scoped root) cleans these up, which is
-- correct because at that point the objects are gone by a different route.
-- TestGH402_ReclaimTaskSurvivesSiteCascade exists to fail loudly if someone
-- "tidies this up" later.
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
-- A site whose backups went to a customer-owned bucket has bytes this worker
-- deliberately does not touch, and the log line should be able to say so.
--
-- GIVE-UP SEMANTICS
--
-- Past the retry cap the row is LEFT VISIBLE, never deleted. A stuck task is
-- the only remaining record that those objects exist, so deleting it on give-up
-- would recreate the bug this migration exists to fix. That is the GH #256
-- lesson applied one layer up: prefer leaving it behind over guessing.
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
        "tenant_id"        uuid        NOT NULL,
        -- Deliberately NOT a foreign key. See the header.
        "site_id"          uuid        NOT NULL,
        -- Which site-scoped storage root to reclaim. 'backup_manifest' is the
        -- only kind today.
        "kind"             text        NOT NULL DEFAULT 'backup_manifest',
        -- 'cp' | 'local' | 's3_compat' | NULL when the site had no destination
        -- row (the legacy control-plane-global bucket). Diagnostic only.
        "destination_kind" text,
        "attempts"         int         NOT NULL DEFAULT 0,
        "next_attempt_at"  timestamptz NOT NULL DEFAULT now(),
        "last_error"       text,
        "completed_at"     timestamptz,
        "created_at"       timestamptz NOT NULL DEFAULT now(),
        "updated_at"       timestamptz NOT NULL DEFAULT now(),
        CONSTRAINT "site_object_reclaim_tenant_id_fkey" FOREIGN KEY ("tenant_id")
            REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
    );
END;
$$;

-- One task per (tenant, site, kind). Makes the enqueue safely re-runnable with
-- ON CONFLICT DO NOTHING, which a future backfill of already-orphaned sites
-- will also need.
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
