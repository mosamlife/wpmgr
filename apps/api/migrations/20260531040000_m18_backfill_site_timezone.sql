-- Backfill site WordPress timezone (m18).
--
-- m17 added sites.wp_timezone / wp_gmt_offset and wired the diagnostics ingest
-- to populate them, but existing sites only get populated on their NEXT
-- diagnostics push. The agent already shipped the timezone in the `identity`
-- category (agent_diagnostics.payload->>'timezone' / ->>'gmt_offset') during
-- prior collections, so copy it onto the site row now — no need to wait a day.
--
-- sites has FORCE ROW LEVEL SECURITY, so a plain UPDATE during migration (no
-- app.tenant_id GUC) would match no policy and silently affect 0 rows. Disable
-- RLS for the duration of the backfill, then restore ENABLE + FORCE. Runs in
-- ONE transaction (the boot runner wraps the whole file).

DO $$
BEGIN
    ALTER TABLE "public"."sites" DISABLE ROW LEVEL SECURITY;

    UPDATE "public"."sites" s
    SET wp_timezone   = COALESCE(NULLIF(d.payload->>'timezone', ''), s.wp_timezone),
        wp_gmt_offset = COALESCE(NULLIF(d.payload->>'gmt_offset', '')::real, s.wp_gmt_offset)
    FROM "public"."agent_diagnostics" d
    WHERE d.site_id = s.id
      AND d.tenant_id = s.tenant_id
      AND d.category = 'identity';

    ALTER TABLE "public"."sites" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."sites" FORCE ROW LEVEL SECURITY;
END;
$$;
