-- m124 - S6a: storage for the read-only MCP connection surface. SCHEMA ONLY.
--
-- THIS MIGRATION DELIBERATELY CHANGES NO BEHAVIOUR, in the m117/m122 sense: it
-- adds four tables, their indexes, their constraints, their RLS and their
-- grants. No route reads them, no service writes them, no worker touches them.
-- Every existing database lands with all four empty, and empty is the correct
-- and complete state for every organisation that has never connected an AI
-- client.
--
-- The Go half - the OAuth authorization server, dynamic client registration,
-- the PKCE exchange, the streamable-HTTP MCP transport, the per-request grant
-- re-check and the resolution of a grant's site scope into a site set - is S6b
-- and belongs to backend-architect. It lands on top of this. See "WHAT S6b
-- MUST DO" at the end of this header; three items there are load-bearing for
-- the boundary and one of them will fail SILENTLY if it is got wrong.
--
-- ===========================================================================
-- WHAT THIS STORES, IN ONE PARAGRAPH
-- ===========================================================================
--
-- An AI client (Claude Desktop, ChatGPT, Cursor, Codex CLI, ...) connects to
-- the control plane over an OAuth streamable-HTTP MCP endpoint and READS fleet
-- state. It can never write. A `mcp_grants` row is the durable statement that
-- one such client may read some subset of one organisation's sites.
-- `mcp_connection_tokens` holds the hashed long-lived credentials for the
-- headless path (CI, containers, SSH); `mcp_oauth_clients` and
-- `mcp_authorization_codes` hold the GUI path's registration and its
-- short-lived single-use PKCE codes.
--
-- ===========================================================================
-- THE QUESTION THIS MIGRATION WAS TOLD TO ANSWER OUT LOUD:
-- IS mcp_oauth_clients TENANT-SCOPED?
-- ===========================================================================
--
-- NO, AND IT CANNOT BE. It carries no tenant_id column at all. The reasoning,
-- and what stands in for tenant isolation, in full:
--
-- WHY NOT. RFC 7591 dynamic client registration is an UNAUTHENTICATED POST.
-- A client registers and receives a client_id BEFORE any human has authorized
-- anything, so at INSERT time there is no organisation to attribute the row
-- to. There is no value a tenant_id column could honestly hold.
--
-- WHY NOT A NULLABLE tenant_id, WHICH IS THE OBVIOUS ALTERNATIVE. Because it
-- would be this project's signature defect written into the schema. A nullable
-- tenant_id forces a policy shaped
--
--     USING (tenant_id = <app.tenant_id> OR tenant_id IS NULL)
--
-- whose second branch binds nothing, and it invites the Go layer to read NULL
-- as "not yet bound, therefore usable by whoever asks". Absence would mean
-- permitted. It is also precisely the disjunctive shape
-- scripts/check-rls-cross-tenant.sh names as its own worst blind spot -- except
-- that the guard now classifies per OR branch, so it would land in the audit
-- anyway, and the ledger row someone wrote to silence it would enshrine the
-- bug as a decision. No column at all is the honest encoding of "this row has
-- no organisation".
--
-- WHAT PREVENTS CLIENT-ID ENUMERATION ACROSS ORGS. Three things, and the first
-- is the one that matters:
--
--   1. THE ROW NAMES NO ORGANISATION, so there is nothing org-shaped to
--      enumerate. A client row is (client_id, hashed secret, redirect URIs,
--      client name). Reading every row in this table reveals which software
--      registered against this installation; it does not reveal which
--      organisation authorized any of it, because that binding is not here.
--      It is on mcp_grants.client_id, and mcp_grants IS tenant-isolated. The
--      org-revealing join is the protected half.
--
--   2. THE TABLE IS INVISIBLE TO ORDINARY TENANT TRANSACTIONS. It is ENABLE +
--      FORCE ROW LEVEL SECURITY with exactly two policies, each gated on a
--      dedicated GUC that only the OAuth endpoints set. Under FORCE RLS a
--      transaction that sets neither GUC matches zero rows. So a logged-in
--      operator, a worker, an agent transaction and every existing repository
--      call see an empty table. Absence of the GUC is REFUSAL, not access --
--      that is the fail-closed direction, and it is why the policies test a
--      GUC rather than being written USING (true).
--
--   3. A client_id IS NOT A SECRET. RFC 6749 section 2.2 is explicit that it is
--      exposed to the resource owner and cannot be treated as confidential.
--      The secret is client_secret_hash, which is hashed and never readable,
--      and possession of a client_id alone authorizes nothing: every request
--      still resolves to a mcp_grants row, under that grant's tenant.
--
-- The two policies are SPLIT BY COMMAND on purpose -- FOR INSERT for
-- registration, FOR SELECT for the token/authorize lookup -- rather than one
-- FOR ALL. Split is the tighter grant: it means the registration endpoint
-- cannot read the table and the lookup path cannot write to it. The guard's
-- section C explicitly understands and blesses this shape.
--
-- BOTH ARE CROSS-TENANT GRANTS AND BOTH GET A LEDGER ROW in
-- db/rls-cross-tenant-policies.txt. That is the guard working as designed, not
-- being evaded: a table with no tenant_id genuinely is a cross-tenant grant,
-- and the ledger exists so that an access mode is a decision somebody wrote
-- down rather than a default nobody typed.
--
-- ===========================================================================
-- DECISION 1: NO PERMISSIVE DEFAULT ON ANY SCOPE COLUMN. ABSENCE IS
--             UNREPRESENTABLE, NEVER PERMITTED
-- ===========================================================================
--
-- S6's exit gate is that a client requesting no recognised scope is REFUSED,
-- not granted a default. Three separate mechanisms carry that here, because a
-- single NOT NULL is not enough.
--
--   a. site_scope_mode IS NOT NULL WITH NO DEFAULT. A caller that forgets to
--      say what the grant covers gets 23502, not a grant. There is deliberately
--      no DEFAULT 'all' and no DEFAULT anything: the whole failure this slice
--      is differentiating against is a scope column whose unset value means
--      everything.
--
--   b. THE MODE AND ITS PAYLOAD ARE TIED BY CHECK, so "requested a scope and
--      named nothing" cannot be stored. mcp_grants_site_scope_payload_check
--      requires mode 'tags' to carry at least one tag and mode 'list' to carry
--      at least one site, and requires mode 'all' to carry neither. Without it
--      a row with mode 'list' and an EMPTY site array is storable, and an empty
--      allowlist is exactly the value a Go layer reads as "no filter applied,
--      therefore every site". That row is now unrepresentable.
--
--   c. THE ARRAYS DEFAULT TO '{}', AND THAT IS NOT A PERMISSIVE DEFAULT. Empty
--      names nothing and therefore grants nothing; it is the restrictive
--      direction, and (b) prevents it from co-existing with a mode that would
--      make it meaningful. Contrast api_keys.capabilities at m120, which is
--      deliberately NULLABLE WITH NO DEFAULT for the opposite reason: there,
--      '{}' as a default would have stripped every pre-existing key's
--      authority at boot. These tables start empty, so no such backfill
--      hazard exists and the reasoning does not transfer.
--
-- NO CAPABILITY COLUMN EXISTS, AND THAT IS A DECISION. The MCP surface is
-- read-only by construction -- "it can never write" is the specification, not
-- a configuration. Minting a capabilities column now would create the place
-- where a write capability could later appear without a migration and without
-- a review, and a nullable one would collapse to a zero-length []string in Go,
-- which is the fail-open m120's auth_model column exists to prevent. If the
-- surface ever needs differentiated reads, the column arrives with its own
-- migration, NOT NULL, no default, closed CHECK.
--
-- ===========================================================================
-- DECISION 2: LIVENESS IS A COLUMN, NOT AN INFERENCE FROM AN EXPIRY
-- ===========================================================================
--
-- Revocation must be effective on the NEXT REQUEST, not at token expiry. So
-- `status` is NOT NULL with a closed CHECK on both mcp_grants and
-- mcp_connection_tokens, and "is this grant live right now" is one indexed
-- read: mcp_grants_live_idx is a partial index on (tenant_id) WHERE status =
-- 'active', and mcp_connection_tokens_hash_key resolves a presented token to
-- its row by unique hash in one probe.
--
-- STATUS HAS NO DEFAULT EITHER, for the same reason as site_scope_mode: a
-- DEFAULT 'active' means a caller that forgot to say produces a LIVE grant.
-- The permissive value is never the one you get for free.
--
-- expires_at ON mcp_connection_tokens IS NOT THE LIVENESS GATE AND MUST NOT BE
-- READ AS ONE. It is nullable, and NULL means the token does not expire on a
-- clock -- which is the whole point of the documented headless path. A token
-- is live when status = 'active' AND (expires_at IS NULL OR expires_at >
-- now()); it is dead the instant status flips, whatever expires_at says.
-- Writing the check the other way round -- treating a future expiry as proof
-- of liveness -- reinstates exactly the "revocation waits for expiry"
-- behaviour this design rejects.
--
-- REVOCATION IS A STATUS FLIP, NOT A DELETE. Both tables keep the revoked row:
-- last_used_at and revoked_at on a dead grant are the record of what that
-- credential did while it was live, and deleting it destroys the only account
-- of it. Rows are freed with the organisation, by cascade, and not before.
--
-- ===========================================================================
-- DECISION 3: ROTATION IS WHY TOKENS ARE A TABLE AND NOT A COLUMN
-- ===========================================================================
--
-- Rotating a connection token means the old one and the new one are both live
-- for a window, so the operator can update CI, a container image and an SSH
-- host without an outage between issuing and cutting over. Two live
-- credentials cannot be two values of one column on mcp_grants, so tokens are
-- their own table with a foreign key back to the grant.
--
-- Nothing here LIMITS how many tokens a grant may have. That is deliberate: a
-- unique partial index on (grant_id) WHERE status = 'active' would cap it at
-- one and make rotation impossible, which is the constraint that looks like
-- tidiness and forbids the feature. A count limit, if one is ever wanted, is a
-- policy decision for S6b with an error contract, not a storage invariant.
--
-- ===========================================================================
-- DECISION 4: NO PLAINTEXT SECRET ANYWHERE
-- ===========================================================================
--
-- Not in a column, not in a default, not in a comment, not in a fixture.
-- Every credential in these four tables is stored as its hash and the
-- plaintext is returned to the caller exactly once, at creation, and never
-- again read back.
--
--   mcp_connection_tokens.token_hash    the presented token
--   mcp_oauth_clients.client_secret_hash the registered client secret
--   mcp_authorization_codes.code_hash    the one-time authorization code
--
-- THE PRIOR ART IS internal/agent/signature.go, which uses crypto/sha256 and
-- lower-case hex (signature.go:47, :57), and internal/apikey/apikey.go:102
-- (`sum := sha256.Sum256([]byte(secret))`) is the same construction applied to
-- exactly this problem -- a bearer credential resolved by hash. These columns
-- are `text` holding lower-case hex for that reason, and S6b must use the same
-- construction rather than inventing a second one.
--
-- WHY SHA-256 AND NOT bcrypt/argon2 HERE, stated because the opposite is the
-- reflex. These are HIGH-ENTROPY GENERATED SECRETS, not human passwords: a
-- work factor buys nothing against a 256-bit random value and costs a KDF on
-- every single MCP request, on the hot path of a surface designed to be
-- polled. This is the same call m1 made for api_keys and it is consistent with
-- it. It would be the WRONG call for anything a human chooses.
--
-- token_prefix EXISTS AND IS NOT A SECRET. It is the short public handle shown
-- in the UI so an operator can tell two tokens apart when deciding which to
-- revoke, mirroring api_keys.prefix. The lookup is on the HASH, not on the
-- prefix, so the prefix carries no authentication weight.
--
-- code_challenge IS NOT HASHED, AND THAT IS CORRECT, NOT AN OVERSIGHT. Under
-- PKCE (RFC 7636) the challenge is already SHA-256(verifier) and is public by
-- construction -- it travels in the authorize request. The SECRET is the
-- verifier, which the client keeps and this schema never stores.
--
-- ===========================================================================
-- DECISION 5: code_challenge_method IS 'S256' ONLY, WITH NO DEFAULT
-- ===========================================================================
--
-- The CHECK admits exactly 'S256'. 'plain' is NOT in the set, and there is no
-- DEFAULT, and both halves of that matter separately:
--
--   NO 'plain'   RFC 7636 section 7.2 keeps 'plain' only for clients that
--                cannot compute SHA-256. Every client this surface targets
--                can. Admitting it would let a client downgrade its own
--                protection by asking, which is a scope decision made by the
--                untrusted side.
--   NO DEFAULT   a missing method must not fall back to anything. If it
--                defaulted, a registration that omitted the field would be
--                stored as whatever the default was, and the obvious default
--                somebody would reach for is the permissive one.
--
-- This is the m115 lesson applied on the way in rather than converged
-- afterwards: close the set in the migration that creates the column.
--
-- ===========================================================================
-- DECISION 6: SINGLE-USE IS consumed_at, AND IT IS THE COLUMN A REPLAY
--             ATTACK TURNS ON
-- ===========================================================================
--
-- mcp_authorization_codes.consumed_at is NULL until the code is exchanged.
-- NULL means "not yet consumed" -- a fact, not a setting, so it gets no
-- manufactured default (the GH #509 rule m122 Decision 8 states).
--
-- The row is NOT deleted on exchange. Keeping the consumed row is what makes a
-- REPLAY detectable: a second presentation of the same code finds the row with
-- consumed_at already set and can refuse, and, per RFC 6749 section 4.1.2,
-- revoke the tokens issued from it. Deleting on exchange makes a replay
-- indistinguishable from an expired or forged code, which throws away the one
-- signal that says an authorization code leaked.
--
-- ===========================================================================
-- DECISION 7: THREE CROSS-TENANT LOOKUP POLICIES, EACH FOR SELECT ONLY,
--             EACH ON THE api_keys_prefix_lookup PATTERN
-- ===========================================================================
--
-- Authenticating a bearer credential is inherently cross-tenant: a request
-- arrives carrying a token, and resolving it is what ESTABLISHES which
-- organisation the request belongs to. The tenant cannot be set before the
-- lookup, because the lookup is how the tenant becomes known.
--
-- This schema already has that exact pattern and it is followed rather than
-- reinvented. m1 (20260527130000_auth_multitenancy.sql:107):
--
--   CREATE POLICY "api_keys_prefix_lookup" ON "public"."api_keys"
--     FOR SELECT USING (current_setting('app.apikey_lookup', true) = 'on');
--
-- with the ledger row recording that the last_used_at stamp is done separately
-- under InTenantTx. invitations_token_lookup and site_perf_config_rum_lookup
-- are the same shape.
--
-- So three policies, each gated on its own dedicated GUC and each FOR SELECT:
--
--   mcp_connection_tokens_lookup    app.mcp_token_lookup
--   mcp_authorization_codes_lookup  app.mcp_code_lookup
--   mcp_oauth_clients_lookup        app.mcp_client_lookup
--
-- SEPARATE GUCS, NOT ONE SHARED app.mcp_lookup. A single GUC would mean the
-- transaction that resolves a connection token also gets cross-tenant read of
-- every authorization code, for no reason other than that both are "MCP". Each
-- GUC opens exactly one table.
--
-- FOR SELECT AND NOT FOR ALL, WHICH IS THE LOAD-BEARING PART. See "WHAT S6b
-- MUST DO" item 1: these policies admit the READ and admit NOTHING to any
-- write, so a write attempted in the lookup transaction matches ZERO ROWS WITH
-- NO ERROR. That silence is the m84/#96, m89/#131 and GH #463 bug, and for
-- these tables it would mean an authorization code that is never marked
-- consumed and can therefore be replayed for as long as it has not expired.
-- The mode is narrow on purpose and the writes belong in a tenant transaction.
--
-- ===========================================================================
-- DECISION 8: NO app.agent POLICY ON ANY OF THE FOUR
-- ===========================================================================
--
-- The same departure from the generic house pattern that m122 Decision 6
-- argued, for the same reasons, and it is argued here rather than inherited.
--
--   1. The WordPress agent has no part in this surface. MCP is an inbound
--      connection from an AI client to the control plane; the agent is a
--      different protocol in the other direction and never reads a grant, a
--      token, a client or a code.
--   2. No cross-tenant control-plane worker reads them either. Every read
--      happens on a request, and the request either runs under a lookup GUC
--      (Decision 7) or under tenant scope once the grant has resolved.
--   3. An app.agent policy is PERMISSIVE and binds no tenant_id, so each one
--      is a cross-tenant grant the ledger has to carry. Adding four of them
--      over credential tables, on the chance a worker might one day want one,
--      is exactly backwards for a boundary.
--
-- EXPIRED-CODE CLEANUP IS THE ONE PLAUSIBLE FUTURE CALLER, and it does not
-- need a policy: a sweep of mcp_authorization_codes WHERE expires_at < now()
-- can run per tenant, and if it is ever wanted as one cross-tenant pass it
-- arrives in its own migration WITH a ledger row naming the caller. Not this
-- one, speculatively.
--
-- CONSEQUENCE, STATED SO NOBODY DISCOVERS IT AT RUNTIME: none of these four
-- tables is reachable from InAgentTx.
--
-- ===========================================================================
-- DECISION 9: THE SITE-SCOPE POLICY QUESTION, AND WHY THE ANSWER IS NOT THE
--             USUAL ONE
-- ===========================================================================
--
-- .claude/rules/db-migrations.md requires a RESTRICTIVE <table>_site_scope
-- policy on every SITE-KEYED table, and m112 exists because four tables
-- shipped without it. So the question is asked directly: are these tables
-- site-keyed?
--
-- NO. NOT ONE OF THE FOUR HAS A site_id COLUMN. A grant's site scope is a
-- MODE PLUS A PAYLOAD (mode 'all', or a tag-id array, or a site-id array),
-- which is a set the Go layer resolves into sites at request time -- not a
-- foreign key a row filter can compare against. There is no column for
-- `site_id = ANY(app.allowed_site_ids)` to be written over. The exemplar shape
-- is unavailable here because its precondition is absent, and claiming
-- otherwise by writing a policy that filters on nothing would be a gate that
-- always passes while looking like protection.
--
-- SO THE REAL EXPOSURE IS THE OTHER DIRECTION, AND IT IS CLOSED HERE. The
-- danger is not a site-scoped collaborator READING a grant. It is a
-- site-scoped collaborator CREATING one: a principal invited to a single site
-- who inserts a mcp_grants row with site_scope_mode = 'all' has just minted
-- itself an organisation-wide read credential. That is the m112 defect class
-- exactly -- a per-site principal reaching an organisation-wide object -- and
-- it is worse than m112's because the artefact it produces is a live bearer
-- credential that outlives the session that made it.
--
-- So mcp_grants and mcp_connection_tokens each carry THREE RESTRICTIVE
-- policies, FOR INSERT / UPDATE / DELETE, on m123's shape:
--
--     coalesce(current_setting('app.site_scope', true), '') <> 'on'
--
-- WITH CHECK on the INSERT gate (there is no existing row to test) and USING on
-- the UPDATE and DELETE gates (USING is what decides which existing rows the
-- statement may reach). A site-scoped principal can create no grant, revoke no
-- grant, and mint or destroy no token, in the database, whatever a handler
-- forgets.
--
-- WHY THREE COMMAND-SPECIFIC POLICIES AND NOT ONE FOR ALL. Because FOR ALL is
-- AND-combined onto SELECT as well, and gating the read is not wanted: a
-- site-scoped collaborator seeing that their organisation has connected an AI
-- client is not an escalation, and a restrictive SELECT here would break the
-- ordinary listing for exactly the principals most likely to be looking. This
-- is m123's reasoning and m123 exists because m122 got the INSERT-only version
-- of it wrong -- so the three-policy set is written completely here on the way
-- in, rather than as an INSERT gate plus a converge migration later.
--
-- NOT ON mcp_authorization_codes OR mcp_oauth_clients. Neither is created by a
-- dashboard principal: a code is minted by the authorize endpoint after a user
-- consents, and a client row is minted by an unauthenticated registration POST.
-- Neither ever runs with app.site_scope set, so a gate there would refuse
-- nothing and would only suggest, falsely, that a site-scoped principal had a
-- path to those tables.
--
-- STILL NOT ENFORCED IN THE DATABASE, AND SAID PLAINLY: which SITES a live
-- grant may read is resolved in Go from the mode and payload above. No RLS
-- policy reads those columns. That is the same boundary m120 drew for
-- api_keys.allowed_site_ids ("STORED HERE, NOT ENFORCED HERE ... the boundary
-- is application-enforced in Go at one audited chokepoint"), and it is stated
-- here so a reader does not infer from the presence of RLS that the site
-- subset is database-enforced. It is not. It is S6b's, at one chokepoint, and
-- it is named in WHAT S6b MUST DO below.
--
-- ===========================================================================
-- DECISION 10: NULL MEANS NEVER SET, AND THERE ARE TWO DIFFERENT ABSENCES IN
--              THE CLIENT IDENTITY
-- ===========================================================================
--
-- The brief asks for the protocol header value OR ITS ABSENCE, which is two
-- distinguishable facts and one column cannot hold both:
--
--   client_identity_recorded_at IS NULL   the client has never connected, so
--                                         nothing has ever been reported.
--   client_identity_recorded_at IS NOT NULL
--     AND protocol_version IS NULL        the client HAS connected and sent no
--                                         MCP-Protocol-Version header at all.
--
-- Collapsing these into "protocol_version IS NULL" would make a never-used
-- grant indistinguishable from a client that omits the header, and the second
-- is a compatibility signal an operator needs to see. Both are observations,
-- so neither gets a manufactured default -- the GH #509 rule.
--
-- last_used_at is NULL until first use for the same reason. Not epoch, not
-- created_at. "Never used" is a true and useful statement about a credential
-- and it is exactly what an operator deciding whether to revoke wants to read.
--
-- ===========================================================================
-- DECISION 11: 'none' AS AN AUTH METHOD MUST NOT MEAN "ANY SECRET WORKS"
-- ===========================================================================
--
-- A public OAuth client registered under PKCE has no client secret, so
-- client_secret_hash is nullable. That nullability is a hole unless something
-- ties it down: a NULL hash with an auth method that expects a secret is a row
-- where the comparison has nothing to compare against, and the Go reflex on a
-- failed comparison against NULL is not reliably "refuse".
--
-- So token_endpoint_auth_method is NOT NULL with no default over a closed set,
-- and mcp_oauth_clients_secret_matches_method_check makes the two agree by
-- construction:
--
--     ("token_endpoint_auth_method" = 'none') = ("client_secret_hash" IS NULL)
--
-- 'none' if and only if there is no secret. A confidential client with a NULL
-- hash is now unrepresentable, and so is a public client carrying one. Same
-- construction as m122's author_id_matches_type_check, for the same reason:
-- make the incoherent state impossible rather than checking for it later.
--
-- ===========================================================================
-- DECISION 12: CASCADES, AND WHAT DIES WITH THEM
-- ===========================================================================
--
-- The house rule from GH #402 / GH #408 is: when you add a cascade, ask what
-- audit or reclaim record dies with it. Asked, and answered for each.
--
--   mcp_grants.tenant_id -> tenants ON DELETE CASCADE. What dies is the grant
--   and, through it, its tokens and codes. What does NOT die is the audit
--   record: every grant creation, rotation and revocation belongs in audit_log,
--   which is a separate table with its own retention and its own append-only
--   revoke. The accountability claim rides on the ledger entry, not on the
--   credential row -- the same call m122 Decision 4 made.
--
--   mcp_connection_tokens.grant_id -> mcp_grants ON DELETE CASCADE, and
--   mcp_authorization_codes.grant_id likewise. A token without its grant
--   authorizes nothing and a code without its grant redeems to nothing, so
--   neither is a record worth stranding.
--
-- THESE TABLES HAVE NO OBJECT-STORAGE COMPONENT, so nothing here joins
-- purge_worker.go's roster of reclaim roots and the m113/m116 hazard -- a
-- cascade destroying the inventory of ciphertext still to be reclaimed -- does
-- not arise. There is no ciphertext. Both existing purge paths
-- (admin_purge_tenant, admin_delete_empty_tenant) are cascade-driven and
-- enumerate no tables, so all four tables are freed with no change to either
-- function.
--
-- mcp_authorization_codes.client_id AND mcp_grants.client_id CARRY NO FOREIGN
-- KEY to mcp_oauth_clients, deliberately. Neither ON DELETE action is right,
-- which is the tell that the column is a recorded fact rather than a live
-- reference: CASCADE would delete an organisation's grants because a client
-- registration was cleaned up, and SET NULL would erase which client a live
-- grant belongs to, breaking the authentication check on the next request. The
-- historical fact is that that client_id was presented. This is the same call
-- m122 made for author_id and audit_log made before it.
--
-- ===========================================================================
-- WHAT S6b (backend-architect) MUST DO, AND WHAT THIS FILE CANNOT
-- ===========================================================================
--
-- Nothing below enforces any of the following. Item 1 is the one that fails
-- SILENTLY.
--
--   1. THE LOOKUP TRANSACTION READS; IT MUST NEVER WRITE. The three lookup
--      policies are FOR SELECT. Marking a code consumed, stamping a token's
--      last_used_at, or updating a grant's client identity from inside the
--      lookup transaction will match ZERO ROWS AND RAISE NO ERROR under FORCE
--      ROW LEVEL SECURITY. The tenant is known the moment the credential
--      resolves, so every one of those writes belongs in a following
--      InTenantTx -- which is exactly what the api_keys ledger row records for
--      last_used_at. Get this wrong on mcp_authorization_codes and consumed_at
--      is never set, single-use silently becomes multi-use, and the code is
--      replayable until it expires. There will be no error in any log.
--   2. RESOLVING A GRANT'S SITE SCOPE INTO A SITE SET, at ONE audited
--      chokepoint (Decision 9). Mode 'tags' resolves through site_tags; mode
--      'list' is the array; mode 'all' is every site in the tenant. An empty
--      resolved set must mean NO SITES, never every site. The CHECK above stops
--      an empty payload being stored, but nothing stops Go from computing an
--      empty set (a tag that matches no site) and then treating it as absence
--      of a filter.
--   3. REFUSING A CLIENT THAT REQUESTS NO RECOGNISED SCOPE, rather than
--      granting a default. The schema makes the unset value unstorable; the
--      REQUEST side of that gate is S6b's and is S6's stated exit criterion.
--   4. RE-CHECKING THE GRANT ON EVERY REQUEST against current state -- status,
--      and the token's status and expiry -- so revocation lands on the next
--      request. The partial index exists to make that cheap; using it is S6b's.
--   5. ENFORCING READ-ONLY. No column here says "read-only"; the surface is
--      read-only because no write tool is exposed. That is a property of the
--      MCP tool set S6b builds, and it is the entire security claim of the
--      feature.
--   6. THE PLAINTEXT CREDENTIAL IS RETURNED ONCE, AT CREATION, AND NEVER READ
--      BACK. There is no column to read it back from; do not add a cache.
--
-- ===========================================================================
-- IDEMPOTENCE AND BOOT SAFETY
-- ===========================================================================
--
-- internal/db/migrate.go applies this on boot, inside main(), in one
-- transaction, in lexical order, so a failure here is a control-plane outage
-- on every install at once. Every statement is guarded: CREATE TABLE and
-- CREATE INDEX with IF NOT EXISTS, every policy wrapped in a pg_policies
-- existence check because PostgreSQL 16 has no CREATE POLICY IF NOT EXISTS
-- (the m94/m113/m122 pattern), foreign keys guarded on pg_constraint, and
-- GRANT is naturally idempotent. Nothing is dropped, no existing row is read
-- or written, and there is no backfill: all four tables start empty and empty
-- is correct for every existing organisation.
--
-- ORDINAL: 20260826000000, the first free one after m123
-- (20260825000000_m123_org_context_write_scope.sql), checked across EVERY REF
-- in the repository and not only main, because ordinal is APPLY order and not
-- commit order -- m113 carries 20260815000000 and was committed after m114 and
-- m115, and migrate.go does not verify atlas.sum.
--
-- CONVERGE PATH: NONE IS REQUIRED, and this is a statement about the world
-- rather than an omission. No prior version of this migration has ever been
-- applied to any database: the ordinal is new, all four tables are new, this
-- file corrects nothing and it edits no applied migration. There is therefore
-- no database in any state that needs converging onto this one, and no
-- m114/m115-shaped follow-up is owed.

-- ---------------------------------------------------------------------------
-- mcp_grants - the durable statement that one AI client may read one org
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS "public"."mcp_grants" (
    "id"        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    "tenant_id" uuid NOT NULL,

    -- Human-set. What the operator called this connection in the UI.
    "name" text NOT NULL CONSTRAINT "mcp_grants_name_not_blank_check"
        CHECK (length(btrim("name")) > 0),

    -- LIVENESS. NOT NULL, closed set, NO DEFAULT: a DEFAULT 'active' would mean
    -- a caller that forgot to say produces a live credential. See DECISION 2.
    "status" text NOT NULL CONSTRAINT "mcp_grants_status_check"
        CHECK ("status" IN ('active', 'revoked')),

    -- WHICH SITES THIS GRANT MAY TOUCH. NOT NULL, closed set, NO DEFAULT.
    -- There is deliberately no DEFAULT 'all'. See DECISION 1.
    "site_scope_mode" text NOT NULL CONSTRAINT "mcp_grants_site_scope_mode_check"
        CHECK ("site_scope_mode" IN ('all', 'tags', 'list')),
    -- The payload for modes 'tags' and 'list'. Empty grants nothing, which is
    -- the restrictive direction; the CHECK below stops empty from co-existing
    -- with a mode that would make it meaningful.
    "scope_tag_ids"  uuid[] NOT NULL DEFAULT '{}',
    "scope_site_ids" uuid[] NOT NULL DEFAULT '{}',
    -- "Requested a scope and named nothing" is unrepresentable. An empty
    -- allowlist is precisely the value a caller reads as "no filter, therefore
    -- everything", and it cannot be stored.
    CONSTRAINT "mcp_grants_site_scope_payload_check" CHECK (
        ("site_scope_mode" = 'all'
            AND cardinality("scope_tag_ids") = 0
            AND cardinality("scope_site_ids") = 0)
        OR ("site_scope_mode" = 'tags'
            AND cardinality("scope_tag_ids") > 0
            AND cardinality("scope_site_ids") = 0)
        OR ("site_scope_mode" = 'list'
            AND cardinality("scope_site_ids") > 0
            AND cardinality("scope_tag_ids") = 0)
    ),

    -- WHICH CLIENT. For the OAuth path this is the registered client_id; for
    -- the headless connection-token path there is no OAuth client and it is
    -- NULL. No foreign key - see DECISION 12.
    "client_id" text NULL,

    -- WHAT THE CLIENT REPORTED ABOUT ITSELF. All three are observations, so all
    -- three are nullable with no default. protocol_version IS NULL means the
    -- client sent no MCP-Protocol-Version header; recorded_at IS NULL means it
    -- has never connected at all. Two distinct absences. See DECISION 10.
    "client_name"                 text        NULL,
    "client_version"              text        NULL,
    "protocol_version"            text        NULL,
    "client_identity_recorded_at" timestamptz NULL,

    -- Who created it. No foreign key: SET NULL would erase the authorship of a
    -- live credential and CASCADE would destroy an organisation's connections
    -- because a member left. Same call as m122's author_id.
    "created_by_user_id" uuid NULL,

    "created_at"   timestamptz NOT NULL DEFAULT now(),
    -- NULL means never used. Not epoch, not created_at. See DECISION 10.
    "last_used_at" timestamptz NULL,
    "revoked_at"   timestamptz NULL,
    -- A revoked grant records when, and a live one cannot claim to have been
    -- revoked. Closed on the way in - the m115 lesson.
    CONSTRAINT "mcp_grants_revoked_at_matches_status_check"
        CHECK (("status" = 'revoked') = ("revoked_at" IS NOT NULL))
);

-- "Is this grant live right now" in one indexed read, and the tenant's list of
-- live connections without touching a revoked row. See DECISION 2.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND tablename = 'mcp_grants'
          AND indexname = 'mcp_grants_live_idx'
    ) THEN
        CREATE INDEX "mcp_grants_live_idx"
            ON "public"."mcp_grants" ("tenant_id")
            WHERE "status" = 'active';
    END IF;
END;
$$;

-- The house convention: an index on tenant_id. The partial index above does
-- not serve the purge cascade, which must reach revoked rows too.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND tablename = 'mcp_grants'
          AND indexname = 'mcp_grants_tenant_idx'
    ) THEN
        CREATE INDEX "mcp_grants_tenant_idx"
            ON "public"."mcp_grants" ("tenant_id");
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.mcp_grants'::regclass
          AND conname  = 'mcp_grants_tenant_id_fkey'
    ) THEN
        ALTER TABLE "public"."mcp_grants"
            ADD CONSTRAINT "mcp_grants_tenant_id_fkey"
            FOREIGN KEY ("tenant_id") REFERENCES "public"."tenants" ("id")
            ON UPDATE NO ACTION ON DELETE CASCADE;
    END IF;
END;
$$;

DO $$
BEGIN
    ALTER TABLE "public"."mcp_grants" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."mcp_grants" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'mcp_grants'
          AND policyname = 'mcp_grants_tenant_isolation'
    ) THEN
        CREATE POLICY "mcp_grants_tenant_isolation" ON "public"."mcp_grants"
            FOR ALL
            USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END;
$$;

-- A site-scoped collaborator must not MINT an organisation-wide read
-- credential. Three command-specific RESTRICTIVE gates, m123's shape; SELECT is
-- deliberately left alone. See DECISION 9.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'mcp_grants'
          AND policyname = 'mcp_grants_site_scope_insert'
    ) THEN
        CREATE POLICY "mcp_grants_site_scope_insert" ON "public"."mcp_grants"
            AS RESTRICTIVE FOR INSERT
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
            );
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'mcp_grants'
          AND policyname = 'mcp_grants_site_scope_update'
    ) THEN
        CREATE POLICY "mcp_grants_site_scope_update" ON "public"."mcp_grants"
            AS RESTRICTIVE FOR UPDATE
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
            );
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'mcp_grants'
          AND policyname = 'mcp_grants_site_scope_delete'
    ) THEN
        CREATE POLICY "mcp_grants_site_scope_delete" ON "public"."mcp_grants"
            AS RESTRICTIVE FOR DELETE
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- mcp_connection_tokens - hashed, never plaintext; its own table because
-- rotation means two live tokens at once. See DECISION 3 and DECISION 4.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS "public"."mcp_connection_tokens" (
    "id"        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    "tenant_id" uuid NOT NULL,
    "grant_id"  uuid NOT NULL,

    -- The short PUBLIC handle shown in the UI so an operator can tell two
    -- tokens apart while rotating. Carries no authentication weight; the lookup
    -- is on the hash. Mirrors api_keys.prefix.
    "token_prefix" text NOT NULL CONSTRAINT "mcp_connection_tokens_prefix_not_blank_check"
        CHECK (length(btrim("token_prefix")) > 0),
    -- THE CREDENTIAL, HASHED. Lower-case hex SHA-256, the construction at
    -- internal/apikey/apikey.go:102 and internal/agent/signature.go:47. The
    -- plaintext is returned once at creation and never stored. See DECISION 4.
    "token_hash" text NOT NULL CONSTRAINT "mcp_connection_tokens_hash_format_check"
        CHECK ("token_hash" ~ '^[0-9a-f]{64}$'),

    -- LIVENESS. NOT NULL, closed set, NO DEFAULT. See DECISION 2.
    "status" text NOT NULL CONSTRAINT "mcp_connection_tokens_status_check"
        CHECK ("status" IN ('active', 'revoked')),

    "created_at" timestamptz NOT NULL DEFAULT now(),
    -- NOT THE LIVENESS GATE. NULL means this token does not expire on a clock,
    -- which is the documented headless path. status is what revocation flips.
    -- See DECISION 2.
    "expires_at"   timestamptz NULL,
    "last_used_at" timestamptz NULL,
    "revoked_at"   timestamptz NULL,
    CONSTRAINT "mcp_connection_tokens_revoked_at_matches_status_check"
        CHECK (("status" = 'revoked') = ("revoked_at" IS NOT NULL))
);

-- The authentication probe: one unique-index lookup from a presented token's
-- hash to its row. Also makes a hash collision across tenants unstorable.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND tablename = 'mcp_connection_tokens'
          AND indexname = 'mcp_connection_tokens_hash_key'
    ) THEN
        CREATE UNIQUE INDEX "mcp_connection_tokens_hash_key"
            ON "public"."mcp_connection_tokens" ("token_hash");
    END IF;
END;
$$;

-- Deliberately NOT unique on (grant_id) WHERE status = 'active': that would cap
-- a grant at one live token and make rotation impossible. See DECISION 3.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND tablename = 'mcp_connection_tokens'
          AND indexname = 'mcp_connection_tokens_grant_idx'
    ) THEN
        CREATE INDEX "mcp_connection_tokens_grant_idx"
            ON "public"."mcp_connection_tokens" ("grant_id");
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND tablename = 'mcp_connection_tokens'
          AND indexname = 'mcp_connection_tokens_tenant_idx'
    ) THEN
        CREATE INDEX "mcp_connection_tokens_tenant_idx"
            ON "public"."mcp_connection_tokens" ("tenant_id");
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.mcp_connection_tokens'::regclass
          AND conname  = 'mcp_connection_tokens_tenant_id_fkey'
    ) THEN
        ALTER TABLE "public"."mcp_connection_tokens"
            ADD CONSTRAINT "mcp_connection_tokens_tenant_id_fkey"
            FOREIGN KEY ("tenant_id") REFERENCES "public"."tenants" ("id")
            ON UPDATE NO ACTION ON DELETE CASCADE;
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.mcp_connection_tokens'::regclass
          AND conname  = 'mcp_connection_tokens_grant_id_fkey'
    ) THEN
        ALTER TABLE "public"."mcp_connection_tokens"
            ADD CONSTRAINT "mcp_connection_tokens_grant_id_fkey"
            FOREIGN KEY ("grant_id") REFERENCES "public"."mcp_grants" ("id")
            ON UPDATE NO ACTION ON DELETE CASCADE;
    END IF;
END;
$$;

DO $$
BEGIN
    ALTER TABLE "public"."mcp_connection_tokens" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."mcp_connection_tokens" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'mcp_connection_tokens'
          AND policyname = 'mcp_connection_tokens_tenant_isolation'
    ) THEN
        CREATE POLICY "mcp_connection_tokens_tenant_isolation" ON "public"."mcp_connection_tokens"
            FOR ALL
            USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END;
$$;

-- Resolving a presented bearer token is what ESTABLISHES the tenant, so it
-- cannot run under one. FOR SELECT ONLY: the last_used_at stamp belongs in a
-- following InTenantTx, exactly as the api_keys ledger row records. A write
-- attempted under this policy matches zero rows with no error. See DECISION 7
-- and WHAT S6b MUST DO item 1.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'mcp_connection_tokens'
          AND policyname = 'mcp_connection_tokens_lookup'
    ) THEN
        CREATE POLICY "mcp_connection_tokens_lookup" ON "public"."mcp_connection_tokens"
            FOR SELECT
            USING (current_setting('app.mcp_token_lookup', true) = 'on');
    END IF;
END;
$$;

-- A site-scoped collaborator must not mint or destroy a connection token
-- against an organisation-wide grant. See DECISION 9.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'mcp_connection_tokens'
          AND policyname = 'mcp_connection_tokens_site_scope_insert'
    ) THEN
        CREATE POLICY "mcp_connection_tokens_site_scope_insert" ON "public"."mcp_connection_tokens"
            AS RESTRICTIVE FOR INSERT
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
            );
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'mcp_connection_tokens'
          AND policyname = 'mcp_connection_tokens_site_scope_update'
    ) THEN
        CREATE POLICY "mcp_connection_tokens_site_scope_update" ON "public"."mcp_connection_tokens"
            AS RESTRICTIVE FOR UPDATE
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
            );
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'mcp_connection_tokens'
          AND policyname = 'mcp_connection_tokens_site_scope_delete'
    ) THEN
        CREATE POLICY "mcp_connection_tokens_site_scope_delete" ON "public"."mcp_connection_tokens"
            AS RESTRICTIVE FOR DELETE
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- mcp_oauth_clients - RFC 7591 dynamic client registration.
--
-- NOT TENANT-SCOPED, AND CARRIES NO tenant_id COLUMN. Registration happens
-- before any user authorizes, so there is no organisation to attribute the row
-- to and a nullable tenant_id would make absence mean permitted. The full
-- argument, and what prevents client-id enumeration across orgs, is in the
-- header section "IS mcp_oauth_clients TENANT-SCOPED?".
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS "public"."mcp_oauth_clients" (
    "id" uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Public per RFC 6749 section 2.2. Not a secret; possession authorizes
    -- nothing on its own.
    "client_id" text NOT NULL,
    -- THE SECRET, HASHED, and NULL exactly when there is none. Lower-case hex
    -- SHA-256, the same construction as the connection token. See DECISION 4.
    "client_secret_hash" text NULL
        CONSTRAINT "mcp_oauth_clients_secret_format_check"
        CHECK ("client_secret_hash" IS NULL OR "client_secret_hash" ~ '^[0-9a-f]{64}$'),
    -- NOT NULL, closed set, NO DEFAULT.
    "token_endpoint_auth_method" text NOT NULL
        CONSTRAINT "mcp_oauth_clients_auth_method_check"
        CHECK ("token_endpoint_auth_method" IN ('none', 'client_secret_basic', 'client_secret_post')),
    -- 'none' if and only if there is no secret. A confidential client with a
    -- NULL hash - the row where the secret comparison has nothing to compare
    -- against - is now unrepresentable. See DECISION 11.
    CONSTRAINT "mcp_oauth_clients_secret_matches_method_check"
        CHECK (("token_endpoint_auth_method" = 'none') = ("client_secret_hash" IS NULL)),

    -- At least one redirect URI, always. An empty array would leave the
    -- redirect check with nothing to match against, and "matches nothing" is
    -- one careless Go comparison away from "matches anything".
    "redirect_uris" text[] NOT NULL
        CONSTRAINT "mcp_oauth_clients_redirect_uris_present_check"
        CHECK (cardinality("redirect_uris") > 0),

    -- What the client said about itself at registration. Observations, so
    -- nullable with no default.
    "client_name" text NULL,
    "client_uri"  text NULL,

    "created_at"   timestamptz NOT NULL DEFAULT now(),
    "last_used_at" timestamptz NULL
);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND tablename = 'mcp_oauth_clients'
          AND indexname = 'mcp_oauth_clients_client_id_key'
    ) THEN
        CREATE UNIQUE INDEX "mcp_oauth_clients_client_id_key"
            ON "public"."mcp_oauth_clients" ("client_id");
    END IF;
END;
$$;

DO $$
BEGIN
    ALTER TABLE "public"."mcp_oauth_clients" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."mcp_oauth_clients" FORCE ROW LEVEL SECURITY;
END;
$$;

-- TWO POLICIES, SPLIT BY COMMAND, EACH ON ITS OWN GUC. There is deliberately no
-- tenant_isolation policy, because there is no tenant_id to isolate on: under
-- FORCE ROW LEVEL SECURITY a transaction that sets neither GUC below matches
-- ZERO ROWS, so this table is invisible to every ordinary tenant transaction,
-- every worker and every agent transaction. Absence of the GUC is refusal.
--
-- Both are cross-tenant grants by the guard's definition and both carry a row
-- in db/rls-cross-tenant-policies.txt.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'mcp_oauth_clients'
          AND policyname = 'mcp_oauth_clients_registration'
    ) THEN
        CREATE POLICY "mcp_oauth_clients_registration" ON "public"."mcp_oauth_clients"
            FOR INSERT
            WITH CHECK (current_setting('app.mcp_client_register', true) = 'on');
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'mcp_oauth_clients'
          AND policyname = 'mcp_oauth_clients_lookup'
    ) THEN
        CREATE POLICY "mcp_oauth_clients_lookup" ON "public"."mcp_oauth_clients"
            FOR SELECT
            USING (current_setting('app.mcp_client_lookup', true) = 'on');
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- mcp_authorization_codes - short-lived, single-use, PKCE.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS "public"."mcp_authorization_codes" (
    "id" uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- The user has consented by the time a code exists, so the organisation IS
    -- known and this table is tenant-isolated - unlike mcp_oauth_clients.
    "tenant_id" uuid NOT NULL,
    "grant_id"  uuid NOT NULL,
    -- The client that will redeem it. No foreign key - see DECISION 12.
    "client_id" text NOT NULL,

    -- THE CODE, HASHED. Never the plaintext. See DECISION 4.
    "code_hash" text NOT NULL CONSTRAINT "mcp_authorization_codes_hash_format_check"
        CHECK ("code_hash" ~ '^[0-9a-f]{64}$'),

    -- PKCE (RFC 7636). The CHALLENGE is public by construction - it is already
    -- SHA-256 of the verifier and travels in the authorize request - so it is
    -- stored as sent and is not a secret. The verifier is the secret and this
    -- schema never sees it.
    "code_challenge" text NOT NULL
        CONSTRAINT "mcp_authorization_codes_challenge_not_blank_check"
        CHECK (length(btrim("code_challenge")) > 0),
    -- 'S256' ONLY, and NO DEFAULT. 'plain' is deliberately not in the set, and
    -- a missing method must not fall back to anything. See DECISION 5.
    "code_challenge_method" text NOT NULL
        CONSTRAINT "mcp_authorization_codes_challenge_method_check"
        CHECK ("code_challenge_method" IN ('S256')),

    -- Must match the redirect the code was issued for, per RFC 6749 4.1.3.
    "redirect_uri" text NOT NULL,

    "created_at" timestamptz NOT NULL DEFAULT now(),
    -- Short-lived, and NOT NULL: a code with no expiry is a code that never
    -- stops being redeemable.
    "expires_at" timestamptz NOT NULL,
    -- SINGLE-USE. NULL means not yet consumed - a fact, so no default. The row
    -- is kept after consumption so a REPLAY is detectable rather than looking
    -- like an expired or forged code. See DECISION 6.
    "consumed_at" timestamptz NULL
);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND tablename = 'mcp_authorization_codes'
          AND indexname = 'mcp_authorization_codes_hash_key'
    ) THEN
        CREATE UNIQUE INDEX "mcp_authorization_codes_hash_key"
            ON "public"."mcp_authorization_codes" ("code_hash");
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND tablename = 'mcp_authorization_codes'
          AND indexname = 'mcp_authorization_codes_tenant_idx'
    ) THEN
        CREATE INDEX "mcp_authorization_codes_tenant_idx"
            ON "public"."mcp_authorization_codes" ("tenant_id");
    END IF;
END;
$$;

-- Expiry sweeps, and the grant cascade.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND tablename = 'mcp_authorization_codes'
          AND indexname = 'mcp_authorization_codes_grant_idx'
    ) THEN
        CREATE INDEX "mcp_authorization_codes_grant_idx"
            ON "public"."mcp_authorization_codes" ("grant_id");
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.mcp_authorization_codes'::regclass
          AND conname  = 'mcp_authorization_codes_tenant_id_fkey'
    ) THEN
        ALTER TABLE "public"."mcp_authorization_codes"
            ADD CONSTRAINT "mcp_authorization_codes_tenant_id_fkey"
            FOREIGN KEY ("tenant_id") REFERENCES "public"."tenants" ("id")
            ON UPDATE NO ACTION ON DELETE CASCADE;
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.mcp_authorization_codes'::regclass
          AND conname  = 'mcp_authorization_codes_grant_id_fkey'
    ) THEN
        ALTER TABLE "public"."mcp_authorization_codes"
            ADD CONSTRAINT "mcp_authorization_codes_grant_id_fkey"
            FOREIGN KEY ("grant_id") REFERENCES "public"."mcp_grants" ("id")
            ON UPDATE NO ACTION ON DELETE CASCADE;
    END IF;
END;
$$;

DO $$
BEGIN
    ALTER TABLE "public"."mcp_authorization_codes" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."mcp_authorization_codes" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'mcp_authorization_codes'
          AND policyname = 'mcp_authorization_codes_tenant_isolation'
    ) THEN
        CREATE POLICY "mcp_authorization_codes_tenant_isolation" ON "public"."mcp_authorization_codes"
            FOR ALL
            USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END;
$$;

-- The token endpoint presents a code and no tenant: resolving the code is what
-- establishes it. FOR SELECT ONLY - and this is the one that fails silently if
-- S6b gets it wrong. Setting consumed_at inside this transaction matches ZERO
-- ROWS WITH NO ERROR under FORCE ROW LEVEL SECURITY, single-use quietly becomes
-- multi-use, and the code stays replayable until it expires. The consume
-- belongs in a following InTenantTx. See DECISION 7 and WHAT S6b MUST DO item 1.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'mcp_authorization_codes'
          AND policyname = 'mcp_authorization_codes_lookup'
    ) THEN
        CREATE POLICY "mcp_authorization_codes_lookup" ON "public"."mcp_authorization_codes"
            FOR SELECT
            USING (current_setting('app.mcp_code_lookup', true) = 'on');
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- Grants.
--
-- m1's ALTER DEFAULT PRIVILEGES (20260527130000_auth_multitenancy.sql:123-126)
-- already grants wpmgr_app SELECT, INSERT, UPDATE and DELETE on every table
-- created by the migration owner, these four included. They are written
-- explicitly anyway, so the privilege set is a statement in the file that
-- creates the table rather than an inheritance a reader has to go and find.
--
-- NO REVOKE HERE, unlike m122. These are not append-only tables: a grant is
-- revoked by flipping status, a token is rotated and revoked, and a code is
-- marked consumed. UPDATE is required on all three and withholding it would
-- break revocation, which is the property DECISION 2 exists to guarantee.
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT, UPDATE, DELETE ON "public"."mcp_grants"              TO wpmgr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON "public"."mcp_connection_tokens"   TO wpmgr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON "public"."mcp_oauth_clients"       TO wpmgr_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON "public"."mcp_authorization_codes" TO wpmgr_app;
