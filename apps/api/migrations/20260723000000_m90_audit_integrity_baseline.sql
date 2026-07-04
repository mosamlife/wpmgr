-- M90 — audit-integrity re-baseline.
--
-- audit_log is append-only and hash-chained; a historical chain break caused
-- by a concurrent-append race that pre-dates the per-tenant advisory-lock fix
-- in Recorder.Record can never be repaired in place (UPDATE/DELETE are
-- revoked from the app role), so Verify() would report it forever with no way
-- for an operator to acknowledge a benign, already-understood break.
--
-- Adds one tenant-scoped table, audit_integrity_baseline, holding at most one
-- row per tenant: the chain-head snapshot (id/created_at/hash) an operator
-- moved the trusted "verify from here forward" anchor to, plus who did it and
-- when. Verify walks the full chain from genesis when no row exists (default,
-- unchanged behaviour); when a row exists, it seeds the running hash with
-- baseline_hash and only walks entries strictly after
-- (baseline_created_at, baseline_id) — a break before the baseline is
-- permanently acknowledged, while any tampering after the baseline is still
-- detected. Re-baselining never alters or deletes any audit_log row.
--
-- RLS: ENABLE + FORCE with a single tenant_isolation policy (app.tenant_id
-- GUC, operator/API path). No app.agent policy — this is an operator-only
-- surface; the agent has no reason to read or write it.
--
-- Idempotent: every statement is IF-NOT-EXISTS guarded inside a DO block
-- (mirrors the m36 template), safe to re-run.

DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS "public"."audit_integrity_baseline" (
        "tenant_id"           uuid        PRIMARY KEY,
        "baseline_created_at" timestamptz NOT NULL,
        "baseline_id"         uuid        NOT NULL,
        "baseline_hash"       text        NOT NULL,
        "set_by"              uuid,
        "set_at"              timestamptz NOT NULL DEFAULT now(),
        CONSTRAINT "audit_integrity_baseline_tenant_fkey" FOREIGN KEY ("tenant_id")
            REFERENCES "public"."tenants" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
        CONSTRAINT "audit_integrity_baseline_set_by_fkey" FOREIGN KEY ("set_by")
            REFERENCES "public"."users" ("id") ON UPDATE NO ACTION ON DELETE SET NULL
    );
END;
$$;

DO $$
BEGIN
    ALTER TABLE "public"."audit_integrity_baseline" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."audit_integrity_baseline" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'audit_integrity_baseline'
          AND policyname = 'audit_integrity_baseline_tenant_isolation'
    ) THEN
        CREATE POLICY "audit_integrity_baseline_tenant_isolation" ON "public"."audit_integrity_baseline"
            USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END;
$$;
