-- m101 — GH #245: honest-unknown severity fallback + Wordfence feed alternation.
--
-- Root cause: Wordfence Intelligence v3 enforces ~1 request / 30 min GLOBALLY
-- per API key. The feed worker fetched the Scanner feed then slept only 2s
-- before fetching the Production feed in the SAME run, so Production was
-- deterministically 429'd every cycle. mergeEnrichment therefore never ran:
-- every site_vulnerabilities row was stored with cvss_score = NULL and
-- cvss_rating = '', and SeverityFromRating's unconditional "no data" fallback
-- silently bucketed that as 'low' — including a real CVSS 9.8 core RCE.
--
-- Fix (application-side; this migration only widens storage for it):
--   1. SeverityFromRating (internal/vuln/model.go) now returns 'unknown', not
--      'low', when neither a rating nor a score is available — a "no data"
--      finding must never be silently indistinguishable from a confirmed-low
--      one. The severity CHECK constraint is widened to allow the new value.
--   2. The feed worker (internal/vuln/worker.go) now alternates Scanner and
--      Production across successive hourly runs — one HTTP request per run —
--      via the persisted next_feed_kind cursor below, instead of fetching
--      both feeds in the same run. This keeps every run comfortably clear of
--      the ~30-min rate-limit window, so Production (which carries the
--      CVSS/CVE/CWE enrichment) actually lands.
--   3. enrichment_ok/last_enrichment_at track the Production-fetch pipeline's
--      health independently of the pre-existing ok/fetched_at/record_count
--      fields (which track Scanner-driven DETECTION freshness and gate
--      RescanSite) — a Production hiccup must never block detection rescans.
--
-- next_feed_kind is internal worker state only; it is never exposed via the
-- API (see admin.VulnFeedStatus / vuln.FeedMeta, which surface enrichment_ok/
-- last_enrichment_at instead).
--
-- Idempotent throughout: ADD COLUMN IF NOT EXISTS (mirrors m92/m93) and
-- DROP CONSTRAINT IF EXISTS + ADD CONSTRAINT (mirrors m87's cadence-widen).

ALTER TABLE "public"."wordfence_vuln_feed_meta"
    ADD COLUMN IF NOT EXISTS "enrichment_ok" boolean NOT NULL DEFAULT false;
ALTER TABLE "public"."wordfence_vuln_feed_meta"
    ADD COLUMN IF NOT EXISTS "last_enrichment_at" timestamptz;
ALTER TABLE "public"."wordfence_vuln_feed_meta"
    ADD COLUMN IF NOT EXISTS "next_feed_kind" text NOT NULL DEFAULT 'scanner';

ALTER TABLE "public"."wordfence_vuln_feed_meta"
    DROP CONSTRAINT IF EXISTS "wordfence_vuln_feed_meta_next_feed_chk";
ALTER TABLE "public"."wordfence_vuln_feed_meta"
    ADD CONSTRAINT "wordfence_vuln_feed_meta_next_feed_chk"
    CHECK ("next_feed_kind" IN ('scanner', 'production'));

-- Widen the severity CHECK to allow the new honest-unknown bucket. Must land
-- before any code path writes 'unknown' — migrations apply on boot before the
-- HTTP/worker layer starts serving, so a single deploy is always ordered
-- correctly.
ALTER TABLE "public"."site_vulnerabilities"
    DROP CONSTRAINT IF EXISTS "site_vulnerabilities_severity_chk";
ALTER TABLE "public"."site_vulnerabilities"
    ADD CONSTRAINT "site_vulnerabilities_severity_chk"
    CHECK ("severity" IN ('critical', 'high', 'medium', 'low', 'unknown'));
