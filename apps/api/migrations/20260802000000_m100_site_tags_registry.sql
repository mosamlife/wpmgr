-- M100 — GH #230 "rich tags": tenant-level tag registry.
--
-- Problem: sites.tags is a bare text[] with no place to hang metadata (color,
-- stable id for rename/merge) or to discover a tenant's full tag vocabulary
-- independent of which sites currently carry it (an unused tag has nowhere to
-- live). There is also no server-side rename/merge/delete-fleet-wide flow —
-- every site's tags array must be edited independently.
--
-- Fix (Model A: registry over kept text[]): sites.tags STAYS the assignment
-- store (unchanged; sites_tags_idx GIN index unchanged). This migration adds
-- ONLY a new tenant-level registry table, site_tags, that owns tag existence,
-- color, and canonical name. There is NO join table — sites.tags remains the
-- single source of truth for "which sites carry this tag"; site_tags is
-- metadata + a name index. Tag names are CASE-SENSITIVE, matching the
-- existing site.normalizeTags + `= ANY(tags)` semantics; renaming a tag onto
-- an existing name (merge:true) is the remedy for case-insensitive
-- duplicates, not a DB-level constraint.
--
-- BINDING INVARIANT (enforced in application code, internal/sitetag +
-- internal/site): every path that writes tag names onto a site upserts those
-- names into site_tags in the SAME transaction. There are exactly three such
-- write paths: site.Service.SetTags, pairing-code minting (at create time,
-- the operator-authenticated tx — NOT the public /enroll consume path, which
-- runs under the app.enroll GUC and needs zero registry writes), and
-- POST /api/v1/tags/bulk-apply. Unused (usage 0) registry rows are legitimate
-- and expected. There are no advisory locks around rename/delete racing a
-- concurrent SetTags — last-write-wins, never a dangling reference, because
-- site_tags rows are never referenced by FK from sites.tags (it is a plain
-- text[], not a join table).
--
-- RLS = EXACTLY the m63 clients-table pattern (tenant-level registry, NOT
-- site-keyed): ENABLE + FORCE + tenant_isolation + agent. No m19/m94
-- RESTRICTIVE site_scope policy (this table has no site_id column and is not
-- site-keyed) and no app.enroll policy (the enroll path never touches this
-- table by design).
--
-- Idempotent throughout (DO $$ ... IF NOT EXISTS ... $$), mirrors m93/m94/m99.

DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."site_tags" (
        "id"         uuid        NOT NULL DEFAULT gen_random_uuid(),
        "tenant_id"  uuid        NOT NULL,
        "name"       text        NOT NULL,
        -- '' = auto (client picks a deterministic color from the name); else a
        -- lowercase '#rrggbb' hex code (app-layer normalizes to lowercase; the
        -- CHECK below is case-insensitive so it never rejects a pre-normalized
        -- value written outside the app, e.g. during a future data fix).
        "color"      text        NOT NULL DEFAULT '',
        "created_at" timestamptz NOT NULL DEFAULT now(),
        "updated_at" timestamptz NOT NULL DEFAULT now(),
        PRIMARY KEY ("id"),
        CONSTRAINT "site_tags_tenant_id_fkey" FOREIGN KEY ("tenant_id")
            REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
        -- Exact-case unique per tenant (site.normalizeTags/ANY(tags) semantics).
        CONSTRAINT "site_tags_tenant_name_key" UNIQUE ("tenant_id", "name"),
        -- Backs a future composite FK the same way clients_id_tenant_key does;
        -- not referenced by any FK today (sites.tags has no join table).
        CONSTRAINT "site_tags_id_tenant_key" UNIQUE ("id", "tenant_id"),
        CONSTRAINT "site_tags_name_nonempty" CHECK (btrim("name") != '' AND char_length("name") <= 64),
        CONSTRAINT "site_tags_color_format" CHECK ("color" = '' OR "color" ~* '^#[0-9a-f]{6}$')
    );
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename  = 'site_tags'
          AND indexname  = 'site_tags_tenant_idx'
    ) THEN
        CREATE INDEX "site_tags_tenant_idx" ON "public"."site_tags" ("tenant_id");
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- Row-Level Security (mirrors m63 clients exactly: tenant isolation + the
-- app.agent cross-tenant path; no site_scope RESTRICTIVE policy — this is a
-- tenant-level registry, not a site-keyed table).
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    ALTER TABLE "public"."site_tags" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."site_tags" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_tags'
          AND policyname = 'site_tags_tenant_isolation'
    ) THEN
        CREATE POLICY "site_tags_tenant_isolation" ON "public"."site_tags"
            USING      ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'site_tags'
          AND policyname = 'site_tags_agent'
    ) THEN
        CREATE POLICY "site_tags_agent" ON "public"."site_tags"
            USING      (current_setting('app.agent', true) = 'on')
            WITH CHECK (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- Day-1 backfill: adopt every tag name currently in use — either assigned to
-- a live site or sitting on an unredeemed, unexpired pairing code — into the
-- registry, preserving exact casing, with color left '' (auto). This runs
-- cross-tenant, so (m18 precedent) RLS is disabled around it for the
-- duration and restored immediately after, all inside this migration's own
-- transaction (the boot runner wraps the whole file in one tx — see
-- internal/db/migrate.go applyOne). ON CONFLICT DO NOTHING makes this safe
-- to re-run.
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    ALTER TABLE "public"."sites" DISABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."pairing_codes" DISABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."site_tags" DISABLE ROW LEVEL SECURITY;

    -- Tags currently assigned to a site. char_length(t.tag) <= 64 is
    -- LOAD-BEARING, not cosmetic: two live write paths (site-first
    -- MintEnrollmentCode and the legacy POST /enroll) never enforced the
    -- 64-char cap before this release (see internal/site validation
    -- follow-up), so a real prod site can carry an over-length tag today. An
    -- unguarded INSERT here would violate site_tags_name_nonempty's
    -- char_length<=64 CHECK and abort this migration's single boot
    -- transaction (crash-loop on deploy). An over-length tag simply stays
    -- OFF the registry — it remains on sites.tags untouched (still renders
    -- as a chip via name-derived auto color, still filterable via
    -- ?tags=/?tags_match=) and is harmless to leave unregistered.
    INSERT INTO "public"."site_tags" ("tenant_id", "name")
    SELECT DISTINCT s."tenant_id", t."tag"
    FROM "public"."sites" s
    CROSS JOIN LATERAL unnest(s."tags") AS t("tag")
    WHERE btrim(t."tag") != ''
      AND char_length(t."tag") <= 64
    ON CONFLICT ("tenant_id", "name") DO NOTHING;

    -- Tags sitting on an unexpired, unredeemed pairing code (about to become
    -- a site's tags once the code is consumed). Same over-length guard as
    -- above and for the same reason (CreatePairingCodeInput DID validate
    -- max=64 already, but a code minted via the site-first flow could still
    -- carry a long tag inherited from the unvalidated MintEnrollmentInput
    -- path — see the follow-up fix).
    INSERT INTO "public"."site_tags" ("tenant_id", "name")
    SELECT DISTINCT pc."tenant_id", t."tag"
    FROM "public"."pairing_codes" pc
    CROSS JOIN LATERAL unnest(pc."tags") AS t("tag")
    WHERE btrim(t."tag") != ''
      AND char_length(t."tag") <= 64
      AND pc."consumed_at" IS NULL
      AND pc."expires_at" > now()
    ON CONFLICT ("tenant_id", "name") DO NOTHING;

    ALTER TABLE "public"."site_tags" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."site_tags" FORCE ROW LEVEL SECURITY;
    ALTER TABLE "public"."pairing_codes" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."pairing_codes" FORCE ROW LEVEL SECURITY;
    ALTER TABLE "public"."sites" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."sites" FORCE ROW LEVEL SECURITY;
END;
$$;
