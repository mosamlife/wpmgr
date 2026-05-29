-- M7 / ADR-036 — URL rewriter source-URL capture.
--
-- Adds the four URL fields the agent records at backup time so a later restore
-- can rewrite siteurl / home / content / upload references from the snapshot's
-- source URLs to the target environment's URLs (dev->prod, staging->prod,
-- agency handoff). When all four are NULL/empty, the agent defensively reads
-- the URLs out of the dump's banner comments instead.
--
-- All four are nullable: pre-M7 snapshots already in the table do not have
-- these values and a restore of one of them will fall back to the dump
-- banner extraction path on the agent.

ALTER TABLE backup_snapshots
    ADD COLUMN source_site_url    TEXT,
    ADD COLUMN source_home_url    TEXT,
    ADD COLUMN source_content_url TEXT,
    ADD COLUMN source_upload_url  TEXT;

COMMENT ON COLUMN backup_snapshots.source_site_url IS
    'siteurl recorded at backup time. Used by restore to compute URL rewrites when restoring to a different environment (dev->prod, staging->prod).';
COMMENT ON COLUMN backup_snapshots.source_home_url IS
    'home_url recorded at backup time. See source_site_url.';
COMMENT ON COLUMN backup_snapshots.source_content_url IS
    'WP_CONTENT_URL recorded at backup time. See source_site_url.';
COMMENT ON COLUMN backup_snapshots.source_upload_url IS
    'wp_upload_dir()[''baseurl''] recorded at backup time. See source_site_url.';
