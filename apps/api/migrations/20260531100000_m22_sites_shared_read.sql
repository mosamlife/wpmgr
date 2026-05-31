-- M22 — let a user read the metadata of sites shared with them (cross-tenant).
--
-- The "Shared with me" surface needs each shared site's url + name + owning-org
-- name, but a site-scoped collaborator has NO membership in the owning tenant, so
-- the tenant-isolation policy hides those sites. This adds a SELECT-only,
-- self-read-style policy keyed on app.user_id (set by InUserTx): a user may read
-- a site row iff a non-expired site_shares row grants it to them.
--
-- PERMISSIVE + SELECT-only. It is OR-combined with the other permissive policies
-- but still AND-gated by the RESTRICTIVE sites_site_scope policy (M19), so it
-- cannot widen a site-scoped read. On bare-tenant/agent/enroll paths app.user_id
-- is unset so the subquery matches nothing; it only adds visibility under the
-- self-read (InUserTx) context. Idempotent.

DROP POLICY IF EXISTS sites_shared_read ON sites;
CREATE POLICY sites_shared_read ON sites
    FOR SELECT
    USING (EXISTS (
        SELECT 1 FROM site_shares s
        WHERE s.site_id = sites.id
          AND s.user_id = nullif(current_setting('app.user_id', true), '')::uuid
          AND (s.expires_at IS NULL OR s.expires_at > now())
    ));
