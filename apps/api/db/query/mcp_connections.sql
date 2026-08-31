-- S6a-q: the query layer for m124's four MCP tables. QUERIES ONLY.
--
-- m124 (20260826000000_m124_mcp_connection_surface.sql) shipped the schema and
-- nothing calls it. This file is the missing middle: the statements S6b's Go
-- half issues, written here so the GUC scope of each one is a property of the
-- query rather than something a handler has to remember.
--
-- ===========================================================================
-- THE TRANSACTION SCOPE IS PART OF THE CONTRACT, AND IT IS IN THE NAME
-- ===========================================================================
--
-- m124 Decision 7 gives three tables a cross-tenant lookup policy that is
-- FOR SELECT ONLY, and "WHAT S6b MUST DO" item 1 is the one that fails
-- SILENTLY: a write issued inside a lookup transaction matches ZERO ROWS AND
-- RAISES NO ERROR under FORCE ROW LEVEL SECURITY. So every name here says
-- which transaction it belongs in:
--
--   ...ForLookup      runs under its dedicated lookup GUC, cross-tenant, and
--                     READS. It must never be followed by a write in the same
--                     transaction.
--   ...InTenantTx     runs under InTenantTx with app.tenant_id set, after the
--                     credential has resolved and the tenant is therefore
--                     known. Every write lives here.
--
-- The GUCs are one table each, deliberately (Decision 7):
--   app.mcp_client_register  INSERT on mcp_oauth_clients
--   app.mcp_client_lookup    SELECT on mcp_oauth_clients
--   app.mcp_code_lookup      SELECT on mcp_authorization_codes
--   app.mcp_token_lookup     SELECT on mcp_connection_tokens
--
-- ===========================================================================
-- EVERY MUTATING QUERY RETURNS SOMETHING THE CALLER CAN CHECK
-- ===========================================================================
--
-- Not one :exec in this file. The house precedent is api_keys.sql:51
--
--     -- name: TouchAPIKey :exec
--     UPDATE api_keys SET last_used_at = now() WHERE id = $1 AND tenant_id = $2;
--
-- and it is deliberately NOT followed. An :exec UPDATE that matched no row is
-- indistinguishable at the call site from one that matched, which is precisely
-- the silence Decision 7 warns about: the RLS refusal and the successful write
-- return the same nothing. Every UPDATE here is :one or :many with RETURNING,
-- so a zero-row write surfaces as pgx.ErrNoRows or an empty slice and the
-- caller cannot proceed as though it had worked.
--
-- ===========================================================================
-- WHAT IS DELIBERATELY NOT HERE
-- ===========================================================================
--
--   * No UPDATE of mcp_oauth_clients.last_used_at. The table has exactly two
--     policies -- FOR INSERT and FOR SELECT (Decision 7) -- and no UPDATE
--     policy at all, so under FORCE RLS that stamp would match zero rows from
--     every transaction that exists. Writing the query would ship a statement
--     that can only ever silently do nothing. Stamping it needs a new
--     migration adding a FOR UPDATE policy with a ledger row, not a query.
--   * No cross-tenant sweep of expired authorization codes. Decision 8 says a
--     cleanup pass runs per tenant, and that a cross-tenant one arrives in its
--     own migration with a ledger row naming the caller. There is no policy
--     that would admit it today.
--   * No DELETE of a grant, a token or a code. Decision 2: revocation is a
--     status flip, and the revoked row is the record of what the credential
--     did while it was live. Rows are freed with the organisation, by cascade.

-- ===========================================================================
-- mcp_oauth_clients -- RFC 7591 dynamic client registration
-- ===========================================================================

-- name: RegisterMCPOAuthClient :execrows
-- REQUIRES app.mcp_client_register = 'on'. Registration is an unauthenticated
-- POST, so there is no tenant to run under -- this table carries no tenant_id
-- at all (m124's opening question).
--
-- THERE IS NO RETURNING CLAUSE, AND THERE CANNOT BE ONE. This is the correction
-- of a real break found in review of PR #572; the first cut of this query ended
-- in RETURNING * and every dynamic client registration would have failed at
-- runtime.
--
-- WHY. mcp_oauth_clients has two policies, SPLIT BY COMMAND (m124 Decision 7):
-- FOR INSERT gated on app.mcp_client_register, FOR SELECT gated on
-- app.mcp_client_lookup. PostgreSQL enforces the SELECT policy on rows a
-- RETURNING clause hands back, as a WithCheckOption. So under the register GUC
-- alone the INSERT passes its own policy, the RETURNING then fails the SELECT
-- policy, and the WHOLE TRANSACTION ROLLS BACK. Executed as wpmgr_app:
--
--   INSERT ... (no RETURNING)   -> INSERT 0 1, COMMIT
--   INSERT ... RETURNING *      -> ERROR 42501, ROLLBACK
--   INSERT ... RETURNING id     -> ERROR 42501, ROLLBACK
--
-- IT IS NOT ABOUT WHICH COLUMNS ARE NAMED. Returning a single column, even the
-- server-generated id, fails identically -- the check is on visibility of the
-- row, not on the column list. Anyone tempted to "just return the id" should
-- read that line again.
--
-- THE FIX IS NOT TO SET BOTH GUCS IN THE REGISTRATION TRANSACTION, though that
-- also makes the error go away. It would destroy the property the split exists
-- to create. m124 Decision 7: "Split is the tighter grant: it means the
-- registration endpoint cannot read the table and the lookup path cannot write
-- to it." m124's security review verified exactly that -- inside the
-- registration transaction, SELECT count(*) returned 0 -- and recorded it as a
-- defence. This endpoint is UNAUTHENTICATED. Granting it the lookup GUC would
-- let an unauthenticated POST enumerate every registered client on the
-- installation, client_secret_hash included, and would replace "the database
-- refuses" with "the handler happens not to ask". That the failing read is the
-- proof the boundary works is the whole point, not an inconvenience.
--
-- NOR A NEW POLICY. A FOR SELECT policy gated on the register GUC grants read
-- of the WHOLE table to the registering transaction, which is the same hole by
-- another route. Narrowing it to the row being inserted would need a second GUC
-- carrying the client_id, a new policy, and a cross-tenant ledger row -- strictly
-- more surface than reading the row back in the transaction already built to
-- read it.
--
-- SO THE CALLER RE-READS. Insert here, then resolve the row in a following
-- InMCPClientLookupTx via GetMCPOAuthClientByClientIDForLookup. That is the
-- same two-transaction split every other credential path in this file uses --
-- lookup then consume, lookup then touch -- applied in the other direction, and
-- client_id is uniquely indexed so the re-read resolves exactly the row just
-- written.
--
-- :execrows, NOT :exec, AND THE DISTINCTION FROM AN UPDATE MATTERS. This file's
-- rule is that every mutating query returns something checkable, because an
-- UPDATE can match zero rows in silence. An INSERT ... VALUES cannot: with no
-- ON CONFLICT and no INSERT ... SELECT, it writes exactly one row or raises.
-- :execrows still hands the caller a count to assert on, so the rule holds
-- without a RETURNING clause the policy set forbids.
--
-- The caller supplies client_id (public per RFC 6749 2.2) and, for a
-- confidential client, the lower-case hex SHA-256 of the secret. The schema
-- ties the two together: mcp_oauth_clients_secret_matches_method_check makes
-- 'none' hold exactly when client_secret_hash IS NULL, so a public client
-- carrying a secret and a confidential client without one both fail here with
-- 23514 rather than reaching a Go comparison against NULL (Decision 11).
INSERT INTO mcp_oauth_clients (
    client_id, client_secret_hash, token_endpoint_auth_method,
    redirect_uris, client_name, client_uri
) VALUES (
    $1, $2, $3, $4, $5, $6
);

-- name: GetMCPOAuthClientByClientIDForLookup :one
-- REQUIRES app.mcp_client_lookup = 'on'. Serves both the authorize endpoint
-- (resolve the client before rendering consent) and the token endpoint
-- (authenticate the client before exchanging a code).
--
-- RETURNS THE ROW WHOLE, INCLUDING redirect_uris, BECAUSE THE CALLER MUST
-- EXACT-MATCH AGAINST IT. That is PR #569 finding F2 and "WHAT S6b MUST DO"
-- item 7: there is no redirect_uri parameter on this query and there must not
-- be, because a SQL-side match invites `= ANY(...)` against an array the
-- caller never inspected, and the comparison that has to happen is an exact
-- string match the consent screen also has to render. client_name and
-- client_uri are ATTACKER-CONTROLLED -- registration is unauthenticated and the
-- unique index is on client_id alone, so two clients may both call themselves
-- "Claude Desktop" -- and must be presented as unverified.
SELECT * FROM mcp_oauth_clients
WHERE client_id = $1;

-- ===========================================================================
-- mcp_authorization_codes -- the PKCE exchange
-- ===========================================================================

-- name: CreateMCPAuthorizationCode :one
-- Runs InTenantTx. The user has consented by the time a code exists, so the
-- organisation IS known and this table is tenant-isolated. code_hash is the
-- lower-case hex SHA-256 of the code; the plaintext is returned to the client
-- once, here, and never stored (Decision 4).
INSERT INTO mcp_authorization_codes (
    tenant_id, grant_id, client_id, code_hash,
    code_challenge, code_challenge_method, redirect_uri, expires_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8
)
RETURNING *;

-- name: GetMCPAuthorizationCodeByHashForLookup :one
-- REQUIRES app.mcp_code_lookup = 'on'. Resolving a presented code is what
-- establishes the tenant, so it cannot run under one.
--
-- THIS QUERY FILTERS ON NEITHER consumed_at NOR expires_at, AND THAT IS THE
-- POINT. Decision 6: the row is kept after consumption so that a REPLAY is
-- DETECTABLE. A query that returned only unconsumed, unexpired codes would
-- collapse three different facts -- forged, expired, already redeemed -- into
-- one empty result, and the third is the signal that says an authorization
-- code leaked. RFC 6749 4.1.2 requires that a second presentation revoke the
-- tokens already issued from that code, which is impossible if the caller
-- cannot tell it apart from a typo.
--
-- So the caller gets the liveness columns and the pre-computed verdicts, and
-- distinguishes:
--   no row                          unknown or forged code
--   is_consumed                     REPLAY -- refuse, and revoke the grant
--   is_expired                      expired -- refuse, ordinary
--   is_redeemable                   exchange it, via the consume query below
--
-- EVERY VERDICT IS COALESCE()d AND CAST, AND THAT IS ABOUT THE GENERATED GO
-- TYPE, NOT ABOUT THE SQL. None of these expressions can evaluate to NULL.
-- But sqlc infers nullability from the column the expression touches, and the
-- first cut of this file generated `IsConsumed interface{}` and
-- `IsRedeemable *bool` -- a verdict a caller has to type-assert or dereference,
-- which is the fail-open shape these columns exist to close. A nil *bool read
-- carelessly is either a panic or a silent false. COALESCE(..., false)::boolean
-- makes the generated field a plain bool with no absent case to mishandle.
SELECT
    mcp_authorization_codes.*,
    COALESCE(mcp_authorization_codes.consumed_at IS NOT NULL, false)::boolean AS is_consumed,
    COALESCE(mcp_authorization_codes.expires_at <= now(), false)::boolean     AS is_expired,
    COALESCE(mcp_authorization_codes.consumed_at IS NULL
        AND mcp_authorization_codes.expires_at > now(), false)::boolean       AS is_redeemable
FROM mcp_authorization_codes
WHERE mcp_authorization_codes.code_hash = $1;

-- name: ConsumeMCPAuthorizationCodeInTenantTx :one
-- MUST RUN IN A SEPARATE TRANSACTION FROM THE LOOKUP ABOVE, UNDER InTenantTx.
-- This is "WHAT S6b MUST DO" item 1, and it is why this is a second query
-- rather than a second statement in the first transaction.
--
-- mcp_authorization_codes_lookup is FOR SELECT. Issued inside the lookup
-- transaction this UPDATE matches ZERO ROWS AND RAISES NO ERROR: consumed_at
-- is never set, single-use silently becomes multi-use, and the code stays
-- replayable until it expires, with nothing in any log. A security reviewer
-- reproduced exactly that (UPDATE 0, code still replayable). The tenant is
-- known the moment the lookup resolves -- the row carries tenant_id -- so the
-- write belongs in the InTenantTx that follows it.
--
-- THE PREDICATE IS THE SINGLE-USE GUARANTEE, NOT THE LOOKUP. `consumed_at IS
-- NULL` here makes this an atomic compare-and-set: two concurrent exchanges of
-- the same code both pass the lookup, and exactly one matches this UPDATE. The
-- loser gets pgx.ErrNoRows. Checking redeemability in Go between the two
-- transactions is a TOCTOU window; this closes it in the database.
--
-- :one, SO A ZERO-ROW WRITE IS AN ERROR. If this returns pgx.ErrNoRows the
-- code was NOT consumed and no token may be issued -- either it was redeemed
-- by a racing exchange, or it expired between the two transactions, or the
-- statement is running in a transaction whose RLS refuses it. All three mean
-- refuse. Never treat "no row" as "already fine".
UPDATE mcp_authorization_codes
SET consumed_at = now()
WHERE tenant_id = $1
  AND id = $2
  AND consumed_at IS NULL
  AND expires_at > now()
RETURNING *;

-- ===========================================================================
-- mcp_grants -- the durable statement that a client may read some sites
-- ===========================================================================

-- name: CreateMCPGrant :one
-- THE POLICY ON THIS INSERT ONLY BINDS IF THE CALLER SET THE GUC IT READS.
-- mcp_grants_site_scope_insert is RESTRICTIVE FOR INSERT and its WITH CHECK
-- coalesces current_setting('app.site_scope', true) to the empty string and
-- compares it against 'on', so it refuses a site-scoped collaborator only
-- inside a transaction where app.site_scope has actually been set to 'on'.
-- Nothing else sets it: reach this query through db.RunTenantTx, which
-- dispatches on the principal's scope and is the sole thing that does (its doc
-- comment in internal/db/db.go states the rule for every table, not just this
-- one). A call site that picks InTenantTx, InTenantTxAsUser or
-- InScopedTenantTx itself leaves the setting unset, the coalesced empty string
-- is not equal to 'on', the RESTRICTIVE check passes, and the row inserts with
-- no error raised anywhere.
--
-- Decision 9 -- a per-site principal must not mint an organisation-wide read
-- credential -- is therefore a property of the policy AND the dispatch
-- together. The policy alone does not carry it.
--
-- No parameter defaults to a permissive value because no COLUMN does: status
-- and site_scope_mode are NOT NULL with no default, so a caller that omits
-- either gets 23502 rather than a live organisation-wide grant (Decision 1).
-- The caller passes status explicitly -- 'active' -- rather than relying on the
-- schema to assume it.
--
-- m127 EXTENDS THAT RULE TO EXPIRY AND CAPABILITIES, and the two new NOT NULL
-- columns behave the same way: a caller that omits capabilities or expires_at
-- gets 23502, NOT an unrestricted or never-expiring connection.
--
-- THAT 23502 IS THE DESIGNED BEHAVIOUR AND IT IS CURRENTLY REACHABLE.
-- CreateMCPGrantParams is a named-field struct literal at
-- internal/mcp/service.go:570 and in three integration fixtures, so adding
-- fields does NOT break the build -- it leaves them zero, which is nil/invalid,
-- which is NULL, which is refused at the INSERT. backend-architect must supply
-- all three. A loud 23502 at creation is the correct failure and is strictly
-- better than the alternative of a DEFAULT that quietly mints a credential
-- nobody chose the terms of.
--
-- idle_expire_after_days is passed explicitly and MAY be NULL: NULL is a real,
-- meaningful value there ("never idle-expire"). m127 DECISION 4 made a non-NULL
-- value conditional on the activity stamp being wired, and IT NOW IS:
-- TouchMCPGrantInTenantTx is reached through Repo.TouchActivity, via
-- Service.RecordActivity, from the transport's tools/list and tools/call arms.
-- A non-NULL window is therefore representable. Callers still pass NULL because
-- NOTHING ASKS THE OPERATOR FOR A WINDOW YET -- not because the stamp is
-- missing. Read that distinction before removing the NULL: the guard is waiting
-- on an input, not on a fix.
--
-- setup_client (m128) IS THE OPERATOR'S CHOICE AT S29 STEP 2 and is passed
-- explicitly, like status, rather than being left to the schema. It is NULLABLE
-- with NO DEFAULT precisely so a caller that never asked -- any path that is
-- not the step-2 wizard -- can pass NULL and mean "no operator choice was
-- recorded". PASS NULL RATHER THAN 'generic' ON SUCH A PATH: 'generic' asserts
-- the operator saw nine cards and chose "Other MCP client", which is a
-- different fact and the one S29 step 9 distinguishes.
--
-- Do NOT derive it from client_name. That column is self-reported at
-- `initialize`, is NULL until the client first connects, and inferring a
-- choice from it manufactures a fact the operator never stated.
--
-- m127 AND m128 ARE INDEPENDENT ON THIS INSERT AND THE MERGE MUST KEEP THEM SO.
-- The three m127 columns carry AUTHORITY and are refused when absent (two are
-- NOT NULL); setup_client carries NONE and is legally absent. Omitting
-- setup_client from this list would silently discard the operator's step-2
-- choice on every create; omitting any m127 column would mint a credential
-- nobody chose the terms of. Both failures compile and generate cleanly.
INSERT INTO mcp_grants (
    tenant_id, name, status, site_scope_mode, scope_tag_ids, scope_site_ids,
    client_id, created_by_user_id, capabilities, expires_at,
    idle_expire_after_days, setup_client
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
)
RETURNING *;

-- name: GetMCPGrant :one
-- Runs inside a tenant transaction chosen by the principal's scope: its one
-- caller, ConnectionStatusSnapshot, dispatches through db.Pool.RunTenantTx,
-- not a fixed InTenantTx. mcp_grants carries RESTRICTIVE `_site_scope`
-- policies keyed on app.site_scope, and only the scoped dispatch -- via
-- InScopedTenantTx -- sets that GUC; a caller that picked InTenantTx directly
-- would read this row with those policies inert. tenant_id is in the WHERE as
-- well as in the RLS policy -- the house convention, defense in depth -- so a
-- grant id from another organisation returns no rows rather than a row.
SELECT * FROM mcp_grants
WHERE tenant_id = $1 AND id = $2;

-- name: ListMCPGrantsForOrg :many
-- Runs inside a tenant transaction chosen by the principal's scope: its
-- caller, ListGrants, dispatches through db.Pool.RunTenantTx, not a fixed
-- InTenantTx, precisely so that dispatch can reach InScopedTenantTx and set
-- app.site_scope. Refused outright for a site-scoped session by
-- mcp_grants_site_scope_select (PR #569 finding F1): scope_site_ids enumerates
-- every site the organisation has granted MCP access to, and RLS filters rows,
-- not columns, so total refusal is the only shape that closes it.
--
-- RETURNS REVOKED GRANTS TOO. Decision 2 keeps the revoked row precisely so
-- last_used_at and revoked_at remain readable -- the record of what the
-- credential did while it was live is what an operator reviews. Filtering to
-- status = 'active' here would hide it. Newest first; the caller renders the
-- status column rather than inferring liveness from presence in this list.
SELECT * FROM mcp_grants
WHERE tenant_id = $1
ORDER BY created_at DESC, id DESC;

-- name: RevokeMCPGrantWithTokensInTenantTx :one
-- Runs inside a tenant transaction chosen by the principal's scope: its
-- caller, RevokeGrantWithTokens, dispatches through db.Pool.RunTenantTx, not a
-- fixed InTenantTx, so that a site-constrained principal reaches
-- InScopedTenantTx and mcp_grants' RESTRICTIVE `_site_scope` policies engage
-- rather than sitting inert. ONE STATEMENT, BOTH TABLES, BECAUSE A GRANT AND
-- ITS TOKENS MUST NOT BE REVOKED SEPARATELY.
--
-- A security review of this stack proved the two are currently independent --
-- it observed `grant_status revoked / token_status active` -- and a live token
-- on a revoked grant means the UI's "revoke" button did nothing an attacker
-- would notice. Decision 2 requires revocation to land on the NEXT REQUEST, so
-- both status columns have to flip together or the guarantee is only as good
-- as the caller's memory to issue a second query.
--
-- Emitting these as two queries would leave that ordering to a handler. They
-- are one CTE so the atomicity is a property of the statement.
--
-- THE RETURN SHAPE IS FOUR DISTINGUISHABLE OUTCOMES, none of which is silence.
-- (0,0) IS A SUCCESS, NOT A FAILURE -- read that one before writing the handler:
--
--   pgx.ErrNoRows            no such grant in this tenant, or RLS refused the
--                            read (a site-scoped session) -- the caller 404s.
--                            Nothing was written. This is the ONLY outcome that
--                            means "the grant is not there".
--   grants_revoked = 1       flipped now, with tokens_revoked tokens.
--   grants_revoked = 0 and
--   tokens_revoked > 0       the grant was ALREADY revoked and its tokens were
--                            not. This converges exactly the half-revoked state
--                            the review found. Re-running is the repair, and
--                            the count is how the caller knows repair happened.
--   grants_revoked = 0 and
--   tokens_revoked = 0       THE GRANT EXISTS AND WAS ALREADY FULLY REVOKED --
--                            status 'revoked' with no active token left. The
--                            requested end state already holds, so this is an
--                            IDEMPOTENT RETRY AND IT SUCCEEDED. Return the same
--                            2xx as a first revoke.
--
-- THE LAST ONE IS SPELLED OUT BECAUSE THE PLAUSIBLE GUESS IS WRONG. Two zeroes
-- look like "nothing happened, therefore something went wrong", and a handler
-- that maps them to 404 or 500 reports a correctly revoked credential as a
-- failure -- which invites the operator to retry, or worse, to believe the
-- credential is still live. The row came back at all, which is what says the
-- grant is visible; the counts describe only how much work was left to do.
-- Distinguishing "not found" from "nothing left to do" is precisely why the
-- outer SELECT reads FROM target instead of returning a bare count.
--
-- The outer SELECT reads FROM target, so a grant this transaction cannot see
-- yields zero rows rather than a row of zeroes -- absence stays absence and is
-- never coerced into "revoked nothing, fine". The token CTE's grant_id
-- subquery is NULL when target is empty, and `grant_id = NULL` matches no row,
-- so an invisible grant cannot reach another organisation's tokens either.
--
-- revoked_at is set in the same SET as status because
-- mcp_grants_revoked_at_matches_status_check and its token counterpart require
-- the two to agree: flipping status alone raises 23514 rather than storing a
-- revoked row that cannot say when.
WITH target AS (
    SELECT tg.id AS grant_id FROM mcp_grants tg
    WHERE tg.tenant_id = $1 AND tg.id = $2
),
revoked_tokens AS (
    UPDATE mcp_connection_tokens rt
    SET status = 'revoked', revoked_at = now()
    WHERE rt.tenant_id = $1
      AND rt.grant_id = (SELECT target.grant_id FROM target)
      AND rt.status = 'active'
    RETURNING rt.id
),
revoked_grant AS (
    UPDATE mcp_grants rg
    SET status = 'revoked', revoked_at = now()
    WHERE rg.tenant_id = $1
      AND rg.id = (SELECT target.grant_id FROM target)
      AND rg.status = 'active'
    RETURNING rg.id
)
SELECT
    (SELECT count(*) FROM revoked_grant)::bigint  AS grants_revoked,
    (SELECT count(*) FROM revoked_tokens)::bigint AS tokens_revoked
FROM target;

-- name: TouchMCPGrantInTenantTx :one
-- Runs InTenantTx, NEVER in the token-lookup transaction -- the same silent
-- zero-row write as the consume query above ("WHAT S6b MUST DO" item 1).
-- :one, so a refused stamp is pgx.ErrNoRows rather than nothing at all.
UPDATE mcp_grants
SET last_used_at = now()
WHERE tenant_id = $1 AND id = $2
RETURNING id, last_used_at;

-- name: RecordMCPGrantClientIdentityInTenantTx :one
-- Runs InTenantTx. Decision 10: there are TWO distinguishable absences in the
-- client identity and this query preserves both.
--
-- client_identity_recorded_at is stamped to now() UNCONDITIONALLY, including
-- when the client sent no MCP-Protocol-Version header, because that stamp is
-- what separates "has never connected" (recorded_at IS NULL) from "connected
-- and sent no header" (recorded_at IS NOT NULL, protocol_version IS NULL). The
-- second is a compatibility signal an operator needs; collapsing it into the
-- first would hide it. So protocol_version is passed through as NULL when the
-- header was absent -- it must NOT be defaulted to a string here.
UPDATE mcp_grants
SET client_name                 = $3,
    client_version              = $4,
    protocol_version            = $5,
    client_identity_recorded_at = now()
WHERE tenant_id = $1 AND id = $2
RETURNING *;

-- ===========================================================================
-- mcp_connection_tokens -- the headless bearer path, and rotation
-- ===========================================================================

-- name: CreateMCPConnectionToken :one
-- Runs InTenantTx from RedeemAuthorizationCode's token exchange, and inside a
-- tenant transaction chosen by the principal's scope, via db.Pool.RunTenantTx,
-- from CreateGrantWithCode and CreateGrantWithToken -- NOT a fixed InTenantTx
-- on those two paths. mcp_connection_tokens carries a RESTRICTIVE
-- `_site_scope_insert` policy keyed on app.site_scope; only the scoped
-- dispatch, via InScopedTenantTx, sets that GUC, so a caller minting a token
-- alongside a grant must go through RunTenantTx or the policy sits inert for a
-- site-constrained principal. token_hash is lower-case hex SHA-256 (Decision 4, the
-- internal/apikey/apikey.go:102 construction); the plaintext is returned to
-- the operator once, here, and there is no column to read it back from.
--
-- Nothing limits how many live tokens a grant may have, deliberately: that is
-- what makes rotation possible (Decision 3). status is passed explicitly for
-- the same reason as on the grant -- there is no DEFAULT 'active' to lean on.
-- expires_at is NULL for a token that does not expire on a clock, which is the
-- documented headless path and NOT a liveness hole: status is what revocation
-- flips, and the authorization query below reads status first.
INSERT INTO mcp_connection_tokens (
    tenant_id, grant_id, token_prefix, token_hash, status, expires_at
) VALUES (
    $1, $2, $3, $4, $5, $6
)
RETURNING *;

-- name: GetMCPConnectionTokenByHashForLookup :one
-- REQUIRES app.mcp_token_lookup = 'on'. STEP 1 OF 2 IN AUTHENTICATION, and it
-- READS ONLY -- stamping last_used_at here is the silent zero-row write.
--
-- WHY THIS DOES NOT JOIN mcp_grants, WHICH IS THE OBVIOUS THING TO WRITE.
-- Decision 7 gives each lookup GUC exactly one table, and mcp_grants has NO
-- lookup policy -- only mcp_grants_tenant_isolation, which tests app.tenant_id.
-- In this transaction the tenant is not yet known, so app.tenant_id is unset,
-- so the join matches zero rows and authentication fails for every request
-- with no error to explain it. Verified against the policy set in
-- db/schema.sql rather than assumed. The grant re-check is step 2, below,
-- under the tenant this query establishes.
--
-- THE LIVENESS PREDICATE IS COMPUTED IN SQL, NOT LEFT TO GO. Decision 2 states
-- it exactly once and it is easy to invert: a token is live when status =
-- 'active' AND (expires_at IS NULL OR expires_at > now()). Writing it the
-- other way round -- treating a future expiry as proof of liveness -- reinstates
-- "revocation waits for expiry", which is the behaviour this design rejects.
-- NULL expires_at means no clock expiry, NOT expired. is_live carries that.
--
-- The row is returned whatever its status so the caller can log a presented
-- revoked token distinctly from an unknown one; it is is_live, not the
-- presence of a row, that authenticates. is_live here is necessary and not
-- sufficient -- the grant has not been consulted yet.
--
-- COALESCE(..., false)::boolean so the generated field is a plain bool rather
-- than a *bool -- see the note on GetMCPAuthorizationCodeByHashForLookup. A
-- nil-pointer liveness verdict is the fail-open shape this column closes.
SELECT
    mcp_connection_tokens.*,
    COALESCE(mcp_connection_tokens.status = 'active'
        AND (mcp_connection_tokens.expires_at IS NULL
             OR mcp_connection_tokens.expires_at > now()), false)::boolean AS is_live
FROM mcp_connection_tokens
WHERE mcp_connection_tokens.token_hash = $1;

-- name: ReCheckMCPRequestAuthorizationInTenantTx :one
-- Runs InTenantTx with the tenant the lookup above established. STEP 2 OF 2,
-- AND IT RUNS ON EVERY REQUEST, NOT ONLY AT CONNECT.
--
-- This is "WHAT S6b MUST DO" item 4. Decision 2 makes liveness a column so
-- revocation lands on the NEXT REQUEST rather than at token expiry; that is
-- only true if something re-reads both status columns per request. Caching the
-- grant after connect, or checking only the token, gives back exactly the
-- delayed-revocation behaviour the schema was shaped to prevent. Both reads
-- are index probes -- the token by primary key, the grant by primary key --
-- so per-request is affordable, which is why mcp_grants_live_idx exists.
--
-- `authorized` is the whole verdict in one column, computed here so that no
-- caller reassembles it: the grant must be active AND the token active AND the
-- token not past a non-NULL expiry. A caller that reads `authorized` cannot
-- get the NULL-expiry branch backwards. The component statuses are returned
-- alongside so a refusal can be logged with its reason.
--
-- The grant is joined on tenant_id as well as id: the token and its grant must
-- belong to the same organisation, checked in the join rather than assumed
-- from the foreign key.
-- m127 DECISION 3 ADDS BOTH GRANT EXPIRIES TO THIS SAME VERDICT, rather than to
-- a second read the caller might forget. A grant past its absolute expiry, or
-- past its idle deadline, now fails THE SAME PREDICATE a revoked grant fails.
-- Both are evaluated on the already-fetched grant row, so neither costs a round
-- trip and neither needs an index.
--
-- The wireframe's "there is no silent read-only period" is a consequence of
-- this shape: expiry flips one boolean to false outright and cannot degrade a
-- connection to a reduced capability set, because there is one boolean here and
-- not a tier.
--
-- g.capabilities is returned so the capability set is read from the SAME ROW
-- and the SAME transaction as the verdict it was authorized under (S7's exit
-- gate: discovery never grants what the registry does not hold, and it cannot
-- hold what this row does not carry).
SELECT
    g.id                          AS grant_id,
    g.name                        AS grant_name,
    g.status                      AS grant_status,
    g.site_scope_mode             AS site_scope_mode,
    g.scope_tag_ids               AS scope_tag_ids,
    g.scope_site_ids              AS scope_site_ids,
    g.client_id                   AS client_id,
    g.capabilities                AS grant_capabilities,
    g.expires_at                  AS grant_expires_at,
    g.idle_expire_after_days      AS grant_idle_expire_after_days,
    g.last_used_at                AS grant_last_used_at,
    t.id                          AS token_id,
    t.token_prefix                AS token_prefix,
    t.status                      AS token_status,
    t.expires_at                  AS token_expires_at,
    -- The component reasons, so a refusal can be logged with its cause. Both
    -- COALESCE to TRUE -- the FAIL-CLOSED direction for a negative flag, since
    -- these say "expired". An absent value must never read as "still valid".
    -- They are diagnostics; `authorized` below is the verdict.
    COALESCE(g.expires_at <= now(), true)::boolean AS grant_absolute_expired,
    COALESCE(g.idle_expire_after_days IS NOT NULL
        AND COALESCE(g.last_used_at, g.created_at)
            + make_interval(days => g.idle_expire_after_days) <= now(),
        true)::boolean AS grant_idle_expired,
    -- The two tenant-level reasons (m130), so a refusal names its own cause
    -- instead of presenting as a mysterious auth error. No COALESCE is needed
    -- or wanted on these three: IS NULL and IS NOT NULL never return NULL, and
    -- the tenant row is always present because the join is an inner join on a
    -- table with no RLS. The reason string is a diagnostic for the operator
    -- console and MUST NOT be returned to the MCP client -- it is an internal
    -- incident note, not a protocol-visible error message.
    (tn.assistant_enabled_at IS NULL)::boolean  AS tenant_assistant_disabled,
    (tn.assistant_paused_at IS NOT NULL)::boolean AS tenant_assistant_paused,
    tn.assistant_paused_reason                  AS tenant_assistant_paused_reason,
    -- COALESCE(..., false)::boolean so this generates as a plain bool and not
    -- a *bool. This is the single column the whole MCP request
    -- boundary turns on; handing it back as a pointer with an absent case is
    -- how a nil comes to be read as anything other than refusal.
    COALESCE(g.status = 'active'
        AND t.status = 'active'
        AND (t.expires_at IS NULL OR t.expires_at > now())
        -- ABSOLUTE EXPIRY. NO `IS NULL OR` BRANCH, and its absence is the
        -- point: mcp_grants.expires_at is NOT NULL (m127 DECISION 2), so
        -- unlike t.expires_at above there is no nullable case here for a
        -- reader to get backwards.
        AND g.expires_at > now()
        -- IDLE EXPIRY. NULL means never idle-expire, which is a DIFFERENT fact
        -- from the absolute expiry above and is why the NULL branch appears on
        -- this line and not that one. coalesce(last_used_at, created_at):
        -- a grant never used is idle since it was created.
        --
        -- SEE m127 DECISION 4 BEFORE WRITING A NON-NULL VALUE INTO THIS COLUMN.
        -- TouchMCPGrantInTenantTx now runs on the request path -- through
        -- Repo.TouchActivity, via Service.RecordActivity, from the transport's
        -- tools/list and tools/call arms -- so last_used_at advances with real
        -- use and this deadline advances with it. DECISION 4's prerequisite is
        -- MET. The column stays NULL on every row today only because nothing
        -- asks the operator for a window yet, and that is the reason to keep it
        -- NULL: an actively used connection must never idle-expire.
        AND (g.idle_expire_after_days IS NULL
             OR COALESCE(g.last_used_at, g.created_at)
                + make_interval(days => g.idle_expire_after_days) > now())
        -- TENANT ENABLEMENT IS DELIBERATELY *NOT* IN THIS PREDICATE YET, AND
        -- THE OMISSION IS LOAD-BEARING. See m130 DECISION 5. Adding
        -- `AND tn.assistant_enabled_at IS NOT NULL` here is a ONE-LINE CHANGE
        -- THAT MUST NOT BE MADE UNTIL A GO PATH WRITES THAT COLUMN: a tenant
        -- created after m130 applied has it NULL, so the line refuses every
        -- connection that tenant will ever make. That was executed, not
        -- reasoned about -- it refused a live bearer in
        -- TestMCPActivityStampLandsAsAppRole with
        -- "this connection has been revoked or has expired". The column is
        -- inert by construction until the enable control exists, which is the
        -- m127 DECISION 4 shape.
        --
        -- THE KILL SWITCH (m130 DECISION 3). NULL means not paused, which is
        -- RUNNING -- the permissive direction, and the reason every existing
        -- row keeps working untouched. Because this verdict is recomputed on
        -- EVERY REQUEST, engaging the switch refuses the very next request on
        -- an already-issued, otherwise perfectly valid connection. That is the
        -- requirement: a switch that only blocks new grants while existing
        -- tokens keep reading is not a kill switch. Do not cache this.
        AND tn.assistant_paused_at IS NULL,
        false)::boolean AS authorized
FROM mcp_connection_tokens t
JOIN mcp_grants g
    ON g.id = t.grant_id AND g.tenant_id = t.tenant_id
-- INNER JOIN, and it is safe as an inner join ONLY because tenants has no RLS
-- (m130 DECISION 1/3). No policy can filter this row away, the foreign key on
-- t.tenant_id guarantees it exists, and t.tenant_id is already a bound
-- parameter, so this is a primary-key probe and costs no extra round trip.
JOIN tenants tn
    ON tn.id = t.tenant_id
WHERE t.tenant_id = $1 AND t.id = $2;

-- name: ListMCPConnectionTokensForGrant :many
-- Runs InTenantTx. The rotation UI: token_prefix is the public handle that
-- lets an operator tell two live tokens apart when deciding which to revoke
-- (Decision 4). Revoked tokens are included for the same reason revoked grants
-- are -- last_used_at and revoked_at are the record. Never selects token_hash
-- for display; it is returned because the row is, and must not be rendered.
SELECT * FROM mcp_connection_tokens
WHERE tenant_id = $1 AND grant_id = $2
ORDER BY created_at DESC, id DESC;

-- name: TouchMCPConnectionTokenInTenantTx :one
-- Runs InTenantTx, NEVER in the lookup transaction that just resolved this
-- token -- mcp_connection_tokens_lookup is FOR SELECT and the stamp would
-- match zero rows in silence. This is the same split the api_keys ledger row
-- records for last_used_at, and the same one m124 Decision 7 cites as the
-- precedent being followed.
--
-- :one rather than the :exec that api_keys.sql uses, so a stamp that hit no
-- row is a distinguishable failure at the call site instead of a no-op that
-- reads as success.
UPDATE mcp_connection_tokens
SET last_used_at = now()
WHERE tenant_id = $1 AND id = $2
RETURNING id, last_used_at;

-- name: RevokeMCPConnectionTokenInTenantTx :one
-- Runs InTenantTx. Revokes ONE token and leaves the grant alone -- this is the
-- cut-over half of rotation (Decision 3): issue the new token, update CI, then
-- retire the old one without an outage and without touching the grant.
--
-- Revoking a grant is NOT this query. Use RevokeMCPGrantWithTokensInTenantTx,
-- which flips both and cannot leave a live token behind.
--
-- :one with `status = 'active'` in the predicate, so re-revoking an already
-- revoked token returns pgx.ErrNoRows rather than reporting a second success.
-- revoked_at is set with status to satisfy
-- mcp_connection_tokens_revoked_at_matches_status_check.
UPDATE mcp_connection_tokens
SET status = 'revoked', revoked_at = now()
WHERE tenant_id = $1 AND id = $2 AND status = 'active'
RETURNING *;

-- ===========================================================================
-- Site-scope resolution -- "WHAT S6b MUST DO" item 2
-- ===========================================================================

-- name: ResolveMCPGrantScopeSitesInTenantTx :many
-- MUST RUN INSIDE A TENANT TRANSACTION CHOSEN BY THE PRINCIPAL'S SCOPE, AND
-- THAT IS A CORRECTNESS REQUIREMENT RATHER THAN A CONVENTION. Since #649
-- (ADR-061 A11 item 2) the caller is db.Pool.RunTenantTx, which dispatches on
-- db.ScopedPrincipal to InScopedTenantTx, InTenantTxAsUser or InTenantTx --
-- NOT a fixed InTenantTx call. The dispatch is not incidental: InScopedTenantTx
-- is the only one of the three that sets app.site_scope, and sites_site_scope
-- (m19) -- the RESTRICTIVE policy this query's join through `sites` passes
-- through -- is inert without it. A caller that ran plain InTenantTx here would
-- leave a site-constrained principal's scope unenforced.
--
-- Separately, and for the reason below: scope_site_ids is a uuid[] and
-- PostgreSQL has no foreign key over array elements, so the column ACCEPTS ANY
-- UUID -- including a site id belonging to another organisation, or one that
-- never existed. No CHECK can refuse that; the constraint needed is a
-- cross-table membership test. This query IS that test: joining through
-- `sites` under tenant isolation silently drops every id that is not this
-- tenant's. Resolve outside a tenant transaction, or against a cached site
-- list, and a foreign UUID survives all the way to the read.
--
-- ONE QUERY, BECAUSE ITEM 2 SAYS ONE AUDITED CHOKEPOINT. Three per-mode
-- queries would be three places for the empty-set mistake to be made.
--
-- THE `ELSE false` IS LOAD-BEARING. An unrecognised or NULL mode resolves to
-- NO SITES, never every site. That is the fail-closed direction and it is the
-- single most important line in this file: item 2's stated hazard is Go
-- computing an empty set and then treating it as absence of a filter.
--
-- MODE 'tags' IS A TWO-HOP RESOLUTION AND THE HOPS DO NOT MATCH TYPES.
-- mcp_grants.scope_tag_ids is uuid[] naming site_tags.id, but sites.tags is
-- text[] holding tag NAMES -- there is no join table (see the site_tags
-- comment in db/schema.sql). So the ids are resolved to names first, under
-- this tenant, and matched with the && operator that sites_tags_idx serves.
-- A caller that passed the uuids straight to `sites.tags && $4` would match
-- nothing and get an empty set, which item 2 warns is one careless step from
-- "no filter, therefore everything".
--
-- WHEN NO TAG ID RESOLVES, array_agg RETURNS NULL, `sites.tags && NULL` IS
-- NULL, AND NULL IS NOT TRUE -- so the row is excluded and the result is
-- empty. An empty result means NO SITES. It must never be read as "no
-- restriction". The caller enforces that; the database cannot.
--
-- No connection_state filter: whether an archived site is visible over MCP is
-- a product decision for S6b, not one to bury in a resolver.
SELECT s.id
FROM sites s
WHERE s.tenant_id = $1
  AND CASE $2::text
        WHEN 'all'  THEN true
        WHEN 'list' THEN s.id = ANY($3::uuid[])
        WHEN 'tags' THEN s.tags && (
            SELECT array_agg(t.name)
            FROM site_tags t
            WHERE t.tenant_id = $1 AND t.id = ANY($4::uuid[])
        )
        ELSE false
      END
ORDER BY s.id;
