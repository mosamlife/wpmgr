-- m154: seat the `mcp:site` OAuth scope and its two capabilities,
-- 'mcp.ability.read' and 'mcp.ability.request'.
--
-- ===========================================================================
-- WHAT THIS FILE DOES
-- ===========================================================================
--
--   1. Widens the two OAuth scope vocabulary CHECKs from {mcp:cache, mcp:read}
--      to {mcp:cache, mcp:read, mcp:site}, dropping and re-adding THE SAME
--      NAMES m136 and m137 chose, in m150's shape.
--   2. Widens mcp_grants_capabilities_vocabulary_check from m135's nine
--      members to eleven, adding 'mcp.ability.read' and 'mcp.ability.request',
--      dropping and re-adding the same name, in m135's shape.
--
-- It creates no table, no column and no policy. m155 creates the ability
-- catalogue and the per-site ability inventory the new tools read.
--
-- ===========================================================================
-- DECISION 1: WHAT THE SCOPE AND THE TWO CAPABILITIES CONFER
-- ===========================================================================
--
--   mcp.ability.read     may list a site's registered abilities, describe one,
--                        and run an ability the WPMgr catalogue classes as a
--                        READ. A read changes nothing on the site.
--
--   mcp.ability.request  may ASK to run an ability the catalogue classes as a
--                        WRITE, and read the status of that ask. As with
--                        mcp.cache.purge under ADR-061, the tool writes a
--                        pending request and nothing reaches the site until a
--                        person holding the entry's operator permission
--                        approves that one request in WPMgr.
--
-- `mcp:site` confers both. The CHECK is a CEILING on what a grant MAY hold; a
-- grant holds 'mcp.ability.request' only when an operator ticks it, and the
-- Go layer requires the creating operator to hold the content-edit
-- permission to mint it. Neither is a default.
--
-- THE NAMES. `mcp.` is the frozen prefix (m131 DECISION 1). `ability` is the
-- domain. `.read` and `.request` are the verbs: `.request` rather than
-- `.write` because the capability confers an ask, not a change, and rather
-- than `.propose` because the ask names a catalogued operation with its own
-- arguments, not a drafted diff.
--
-- ===========================================================================
-- DECISION 2: WIDENING IS A DROP AND RE-ADD, VALIDATED ON ADD
-- ===========================================================================
--
-- A CHECK's expression cannot be altered in place. Each pair runs in the one
-- transaction the runner wraps this file in, so no column is ever
-- unconstrained. Every re-add is validated (not NOT VALID): widening a
-- containment CHECK cannot invalidate a row the narrower one admitted, so the
-- scan is a formality, and pg_constraint reports convalidated = true.
--
-- Nothing is backfilled. Every existing grant and client registration holds
-- exactly what it held before.
--
-- ===========================================================================
-- DECISION 3: THE LITERAL LIST IS THE WHOLE POST-STATE
-- ===========================================================================
--
-- Each constraint is dropped by name and re-added from a literal, so its
-- post-state is a function of this file alone. A later migration that drops
-- and re-adds the same name REPLACES this list; it must carry every member
-- below or it silently removes one (m135 DECISION 4 is the worked example).
-- At the time of writing no open branch re-adds any of these three names.
--
-- ===========================================================================
-- DECISION 4: THE GO PARITY TESTS GO RED WHEN THIS APPLIES, AND MERGE ORDER
-- ===========================================================================
--
-- mcp_m136_scope_vocabulary_parity_test.go,
-- mcp_m137_client_scope_vocabulary_parity_test.go and
-- mcp_m131_capability_vocabulary_parity_test.go compare these lists with the
-- closed sets in internal/mcp (recognisedScopes, capabilityVocabulary). Until
-- the Go change that adds ScopeSite, CapAbilityRead and CapAbilityRequest
-- lands on the same branch, the database holds members Go does not, and
-- those tests fail. That is the handoff.
--
-- THIS FILE MERGES TOGETHER WITH THAT GO CHANGE, NEVER ALONE. It widens
-- constraints the application must agree with, which is exactly the case the
-- merge-order rule covers.
--
-- CONVERGE PATH. None needed: every database, whatever it held, ends with the
-- literal lists below after applying this file.

-- ===========================================================================
-- (1) THE GRANT SCOPE VOCABULARY
-- ===========================================================================

ALTER TABLE "public"."mcp_grants"
    DROP CONSTRAINT IF EXISTS "mcp_grants_oauth_scopes_vocabulary_check";

-- KEEP IN LOCKSTEP with recognisedScopes in internal/mcp/scope.go and with
-- mcp_oauth_clients_registered_scopes_vocabulary_check below. Alphabetical.
ALTER TABLE "public"."mcp_grants"
    ADD CONSTRAINT "mcp_grants_oauth_scopes_vocabulary_check"
    CHECK ("oauth_scopes" <@ ARRAY['mcp:cache', 'mcp:read', 'mcp:site']::text[]);

-- ===========================================================================
-- (2) THE REGISTERED CLIENT SCOPE VOCABULARY
-- ===========================================================================

ALTER TABLE "public"."mcp_oauth_clients"
    DROP CONSTRAINT IF EXISTS "mcp_oauth_clients_registered_scopes_vocabulary_check";

ALTER TABLE "public"."mcp_oauth_clients"
    ADD CONSTRAINT "mcp_oauth_clients_registered_scopes_vocabulary_check"
    CHECK ("registered_scopes" <@ ARRAY['mcp:cache', 'mcp:read', 'mcp:site']::text[]);

-- ===========================================================================
-- (3) THE CAPABILITY VOCABULARY
-- ===========================================================================

ALTER TABLE "public"."mcp_grants"
    DROP CONSTRAINT IF EXISTS "mcp_grants_capabilities_vocabulary_check";

-- KEEP IN LOCKSTEP with capabilityVocabulary in internal/mcp/policy.go.
-- Alphabetical, the order AllCapabilities() sorts into.
ALTER TABLE "public"."mcp_grants"
    ADD CONSTRAINT "mcp_grants_capabilities_vocabulary_check"
    CHECK ("capabilities" <@ ARRAY[
        'mcp.ability.read',
        'mcp.ability.request',
        'mcp.activity.read',
        'mcp.backups.read',
        'mcp.cache.purge',
        'mcp.content.read',
        'mcp.diagnostics.read',
        'mcp.performance.read',
        'mcp.security.read',
        'mcp.sites.read',
        'mcp.uptime.read'
    ]::text[]);
