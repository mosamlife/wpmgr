-- m150: seat the `mcp:cache` OAuth scope, and let cache_purge_audit name the
-- AI connection a clear was asked for by.
--
-- ===========================================================================
-- WHAT THIS FILE DOES
-- ===========================================================================
--
--   1. Widens the two OAuth scope vocabulary CHECKs from {mcp:read} to
--      {mcp:cache, mcp:read}. Both names are the stable names m136 and m137
--      chose, so this is a drop and re-add of THOSE names, in m135's shape.
--   2. Adds cache_purge_audit.initiator_grant_id, nullable, with no default and
--      no foreign key.
--
-- It creates no table and no policy. m151 creates the request table the new
-- scope's tool writes to.
--
-- ===========================================================================
-- DECISION 1: WHAT `mcp:cache` CONFERS, AND WHAT IT DOES NOT
-- ===========================================================================
--
-- `mcp:cache` confers the `mcp.cache.purge` capability m135 seated. Under
-- ADR-061 that capability now means "may ASK to clear a site's page cache":
-- the tool it unlocks writes a pending request, and nothing reaches the site
-- until a person approves that one request in WPMgr. No automation can approve
-- a request, and nothing in this file or in m151 provides a path by which one
-- could.
--
-- m136 and m137 wrote that no write scope was seated and that one would arrive
-- "with its own migration and its own review, never by being appended". This
-- is that migration. The sentence in schema.sql that said no write scope is
-- seated is rewritten in the same commit, because it is now false.
--
-- ===========================================================================
-- DECISION 2: WIDENING IS A DROP AND RE-ADD, VALIDATED ON ADD
-- ===========================================================================
--
-- A CHECK's expression cannot be altered in place. Both statements of each
-- pair run in the one transaction the runner wraps this file in, so there is
-- no instant at which either column is unconstrained.
--
-- The re-add is NOT `NOT VALID`. Widening a containment CHECK cannot
-- invalidate a row that satisfied the narrower one, so the validating scan is
-- a formality on every real install; running it anyway means pg_constraint
-- reports convalidated = true and nobody has to reason about a constraint that
-- was never checked against the rows it guards.
--
-- The two constraints move together. m137's header states the rule: widen one
-- and not the other and the missed one refuses the new scope with 23514, loud
-- and at a named constraint, rather than widening silently.
--
-- THE GO PARITY TESTS GO RED THE MOMENT THIS FILE APPLIES, AND THAT IS THE
-- HANDOFF. mcp_m136_scope_vocabulary_parity_test.go and
-- mcp_m137_client_scope_vocabulary_parity_test.go compare these arrays with
-- recognisedScopes in internal/mcp/scope.go. The database now holds a member
-- Go does not, until the Go change that adds ScopeCache lands on the same
-- branch. The migration lands first and the Go that depends on it follows.
--
-- ===========================================================================
-- DECISION 3: cache_purge_audit.initiator_grant_id
-- ===========================================================================
--
-- NULLABLE, NO DEFAULT. Every existing insert, and every dashboard purge from
-- now on, leaves it NULL; only the AI request dispatch sets it. initiator_user_id
-- is already nullable, so an old-shape insert keeps working unchanged.
--
-- NO FOREIGN KEY, for m133 DECISION 7's reason: this records WHICH connection
-- asked, as a fact. A connection is revoked, never deleted, today, but neither
-- ON DELETE action would be right for a recorded fact if one ever were.
--
-- NO NEW INDEX. The per-site hourly count of AI clears filters on site_id and
-- created_at, which idx_cache_purge_site (site_id, created_at DESC) already
-- serves; initiator_grant_id IS NOT NULL is a residual filter over at most an
-- hour of one site's rows.
--
-- ROW SECURITY IS UNCHANGED AND IS INHERITED. cache_purge_audit carries three
-- policies: cache_purge_audit_tenant_isolation, cache_purge_audit_agent, and
-- the RESTRICTIVE cache_purge_audit_site_scope FOR ALL that m132 created.
-- What that means for the AI clear path, stated so nobody has to re-derive it:
--
--   a. Every read and write of this table on that path runs under a principal
--      whose allowed set is exactly the one site, so USING and WITH CHECK both
--      admit the row being written.
--   b. The site-scope policy filters by SITE, not by initiator. A read under
--      that single-site principal therefore sees the dashboard's own purge rows
--      on the same site (initiator_grant_id NULL). The whole-site cooldown
--      relies on that: "any initiator's whole-site clear in the last few
--      minutes" is visible to it.
--   c. The per-site hourly cap on AI clears filters initiator_grant_id IS NOT
--      NULL explicitly, so dashboard purges never count against it.

-- ===========================================================================
-- (1) THE GRANT SCOPE VOCABULARY
-- ===========================================================================

ALTER TABLE "public"."mcp_grants"
    DROP CONSTRAINT IF EXISTS "mcp_grants_oauth_scopes_vocabulary_check";

-- KEEP IN LOCKSTEP with recognisedScopes in internal/mcp/scope.go and with
-- mcp_oauth_clients_registered_scopes_vocabulary_check below. Alphabetical.
ALTER TABLE "public"."mcp_grants"
    ADD CONSTRAINT "mcp_grants_oauth_scopes_vocabulary_check"
    CHECK ("oauth_scopes" <@ ARRAY['mcp:cache', 'mcp:read']::text[]);

-- ===========================================================================
-- (2) THE REGISTERED CLIENT SCOPE VOCABULARY
-- ===========================================================================

ALTER TABLE "public"."mcp_oauth_clients"
    DROP CONSTRAINT IF EXISTS "mcp_oauth_clients_registered_scopes_vocabulary_check";

ALTER TABLE "public"."mcp_oauth_clients"
    ADD CONSTRAINT "mcp_oauth_clients_registered_scopes_vocabulary_check"
    CHECK ("registered_scopes" <@ ARRAY['mcp:cache', 'mcp:read']::text[]);

-- ===========================================================================
-- (3) WHICH CONNECTION ASKED FOR A CLEAR
-- ===========================================================================

ALTER TABLE "public"."cache_purge_audit"
    ADD COLUMN IF NOT EXISTS "initiator_grant_id" uuid NULL;
