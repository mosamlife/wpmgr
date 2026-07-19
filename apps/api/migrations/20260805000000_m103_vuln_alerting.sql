-- m103 — GH #247: vulnerability alerting, the THIRD signal on the existing
-- per-tenant alert channel (alert_configs), alongside downtime and
-- notify_security.
--
-- alert_configs gains three columns:
--   notify_vulns            — opt-in (default off), mirrors notify_security.
--   vuln_min_severity       — the operator-configurable alert threshold.
--                              'unknown' is deliberately NOT a selectable
--                              value here: an unknown-severity finding always
--                              alerts regardless of threshold (see
--                              internal/vuln/alertdispatch.go) because it is
--                              a newest, un-enriched Scanner-only entry that
--                              may never resolve to a rated severity — #247's
--                              motivating CVE sat as 'unknown' pre-#245.
--   vuln_include_in_digest  — gates a "new vulnerabilities" section on the
--                              existing email digest; defaults ON.
-- Recipients + webhook delivery are NOT duplicated here — vuln alerts reuse
-- the existing email_recipients/webhook_url/webhook_secret columns on this
-- same row, exactly like notify_security.
--
-- site_vulnerabilities gains notified_at (nullable): NULL means "not yet
-- alerted"; the dispatch job atomically claims a tenant's batch by setting it
-- to now() (see internal/vuln.Repo.ClaimUnnotifiedFindings). CRITICAL:
-- existing rows are backfilled to notified_at = now() below — without this,
-- the very first dispatch run would treat every pre-existing open finding
-- across every tenant as "new" and email the tenant's entire historical
-- backlog in one batch. New-enrollment first scans are NOT suppressed (an
-- open finding created AFTER this migration has notified_at = NULL from
-- INSERT's column default, so it alerts normally on the next dispatch).
--
-- Idempotent throughout: ADD COLUMN IF NOT EXISTS (mirrors m101/m92/m93),
-- DROP CONSTRAINT IF EXISTS + ADD CONSTRAINT (mirrors m101's severity widen),
-- CREATE INDEX IF NOT EXISTS. The backfill UPDATE is naturally idempotent
-- (subsequent runs match zero rows once notified_at is no longer NULL for
-- pre-existing rows).

ALTER TABLE "public"."alert_configs"
    ADD COLUMN IF NOT EXISTS "notify_vulns" boolean NOT NULL DEFAULT false;
ALTER TABLE "public"."alert_configs"
    ADD COLUMN IF NOT EXISTS "vuln_min_severity" text NOT NULL DEFAULT 'high';
ALTER TABLE "public"."alert_configs"
    ADD COLUMN IF NOT EXISTS "vuln_include_in_digest" boolean NOT NULL DEFAULT true;

ALTER TABLE "public"."alert_configs"
    DROP CONSTRAINT IF EXISTS "alert_configs_vuln_min_severity_chk";
ALTER TABLE "public"."alert_configs"
    ADD CONSTRAINT "alert_configs_vuln_min_severity_chk"
    CHECK ("vuln_min_severity" IN ('critical', 'high', 'medium', 'low'));

ALTER TABLE "public"."site_vulnerabilities"
    ADD COLUMN IF NOT EXISTS "notified_at" timestamptz;

-- Backfill: every finding that existed before this migration is treated as
-- already-notified so the first dispatch run never alerts on the backlog.
-- Restricted to rows this migration itself just added the column for
-- (notified_at IS NULL) so a re-run of this file is a no-op the second time.
UPDATE "public"."site_vulnerabilities"
SET "notified_at" = now()
WHERE "notified_at" IS NULL;

CREATE INDEX IF NOT EXISTS "idx_site_vuln_tenant_unnotified"
    ON "public"."site_vulnerabilities" ("tenant_id")
    WHERE "status" = 'open' AND "notified_at" IS NULL;
