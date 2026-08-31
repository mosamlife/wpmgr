-- m127 - ADR-064 S4/S5: give an MCP grant an ABSOLUTE EXPIRY, an IDLE EXPIRY
-- and an EXPLICIT PER-CONNECTION CAPABILITY SET.
--
-- This migration CORRECTS NOTHING. It edits no applied migration, it re-runs no
-- earlier backfill, and no database in any state needs converging onto it
-- beyond the one-time backfill in step (0) below, which is described in
-- DECISION 9. There is no m114/m115-shaped follow-up owed.
--
-- WHY IT EXISTS. The connection wizard shipped with 3 of the 10 steps the
-- wireframes specify. Steps 4 (choose what it may do) and 5 (expiry and
-- security) cannot be built at all, because mcp_grants has nowhere to put their
-- data: no expiry of either kind, and no capability set. m124 DECISION 1
-- declined to mint a capabilities column and said why; internal/mcp/service.go
-- names the exact column this migration must add, in the comment at the
-- OrgDefaultCapabilities call site:
--
--     "the COLUMN it would read from arrives with its own migration, NOT NULL,
--      no default, closed CHECK, as DECISION 1 requires."
--
-- That is the specification for DECISION 1 below, written by the code that will
-- read the column. This file implements it literally.
--
-- ===========================================================================
-- DECISION 1: THE CAPABILITY SET IS NOT NULL, HAS NO DEFAULT, AND ITS
--             VOCABULARY IS CLOSED BY CHECK
-- ===========================================================================
--
-- m124 DECISION 1 established the rule for the site axis: absence must be
-- UNREPRESENTABLE or must mean REFUSED, never permitted. The capability axis
-- gets the same treatment, by three separate mechanisms, because a single
-- NOT NULL is not enough.
--
--   a. NOT NULL WITH NO DEFAULT. A nullable capabilities column that Go reads
--      as "no narrowing applied, therefore every capability" is the precise
--      defect this whole surface has been fighting, and it is the one shape
--      CapabilitySet's zero value was designed to make impossible in Go. The
--      column now makes it impossible in the database too: a caller that omits
--      capabilities gets 23502, not an unrestricted connection.
--
--      THIS IS DELIBERATELY THE OPPOSITE OF api_keys.capabilities AT m120,
--      which is NULLABLE with no default. m120's reasoning does not transfer
--      and m124 DECISION 1(c) already said why: api_keys had a live fleet of
--      pre-existing rows whose authority came from a `role` column, so '{}'
--      would have stripped every key in the fleet at boot and NULL had to mean
--      "this key has no capability set at all". mcp_grants has no second
--      source of authority to fall back to. There is nothing for NULL to mean
--      here except "unknown", and an unknown grant is refused, so the value is
--      not worth being able to store.
--
--   b. AN EMPTY ARRAY IS PERMITTED AND MEANS EXACTLY ZERO CAPABILITIES. It is
--      the restrictive direction: a connection holding '{}' reaches no tool.
--      CapabilitySet in internal/mcp/policy.go has the same property and no
--      method answering "does this mean everything", so the two agree.
--
--   c. THE VOCABULARY IS CLOSED BY CHECK, against the registry, BY NAME. This
--      is the mechanism S7's exit gate asks for -- discovery never grants what
--      the registry does not hold -- pushed down to the one place that cannot
--      be forgotten at a call site.
--
--      THE VOCABULARY IS `mcp.sites.read`, AND THAT IS THE WHOLE LIST TODAY.
--      internal/mcp/policy.go declares capabilityVocabulary as a closed map
--      with exactly that one entry, and CapSitesRead is commented "the only
--      capability the Phase 1 surface has". The CHECK below admits that one
--      string and nothing else.
--
--      THE WIREFRAMES NAME THREE GROUPS AND THIS CHECK ADMITS ONE, ON PURPOSE.
--      Step 4 draws site.content.read, site.inventory.read and
--      site.content.propose. None of those exist in the registry. Admitting a
--      name the registry does not hold would store a capability that resolves
--      to no tool -- a grant the operator believes they made and the surface
--      silently does not honour, which is the same shape as the dropped scope
--      token scope.go already refuses. So Step 4 can be built now and will
--      honestly offer one group until each further group arrives WITH ITS OWN
--      MIGRATION EXTENDING THIS CHECK.
--
--      THAT COUPLING IS THE POINT, NOT AN INCONVENIENCE. m124 DECISION 1
--      declined a capabilities column specifically because it "would create the
--      place a write capability could appear without a migration and without a
--      review". The closed CHECK is what buys the column back without buying
--      that hazard: a write capability cannot reach this table until someone
--      writes a migration naming it, which is a reviewed artefact. Adding a
--      Capability to policy.go alone is NOT sufficient and will fail 23514 at
--      the INSERT.
--
--   d. THE CEILING IS 64, AND IT IS A PROPERTY OF THE CREDENTIAL. The wireframe
--      says so in those words ("A connection holds at most 64", "The ceiling is
--      a property of the credential, not a licence tier"), and it is the same
--      figure m120 chose for api_keys.capabilities. Shape is otherwise checked
--      exactly as m120 checks it -- one dimension, no NULL element, no empty
--      string -- using only IMMUTABLE expressions so the constraint is valid in
--      a CHECK.
--
--      NOT CHECKED: element uniqueness. Deduplication needs a subquery
--      (cardinality vs. a DISTINCT unnest) and CHECK forbids one. A duplicate
--      is harmless to a set-membership read and merely consumes ceiling; the
--      API layer should dedupe before insert.
--
-- ===========================================================================
-- DECISION 2: TWO EXPIRIES, AND NULL MEANS A DIFFERENT THING IN EACH
-- ===========================================================================
--
-- Absolute expiry and idle expiry are DIFFERENT FACTS. The brief requires that
-- a NULL in either must not read as the other, nor as "never expires" when the
-- operator chose a date. They are stated separately here because they are
-- stored differently, and the difference is taken straight from the wireframe.
--
--   expires_at timestamptz NOT NULL, NO DEFAULT.
--     NULL IS UNREPRESENTABLE. Not "NULL means never" -- there is no NULL.
--     Step 5 offers four choices: 30 days, 90 days, 1 year, On a date. THERE IS
--     NO "NEVER" OPTION ON THIS CONTROL. A never-expiring connection is not a
--     thing the product offers, so it is not a thing the schema can hold, and
--     the read-time predicate in DECISION 3 therefore has NO NULL BRANCH FOR A
--     CALLER TO GET BACKWARDS. That is the entire reason this column is NOT
--     NULL rather than nullable-with-a-convention.
--
--   idle_expire_after_days integer NULL, NO DEFAULT.
--     NULL MEANS NEVER IDLE-EXPIRE, and it means only that. Step 5's second
--     control does offer "Never" alongside 30 days and 90 days, so this axis
--     genuinely has three states and the third is representable. It is an
--     INTERVAL LENGTH, not an instant: the deadline is computed at read time
--     from the activity stamp, so it moves whenever the connection is used,
--     which is what "expire it if unused for 30 days" means. Storing a
--     precomputed idle deadline would require rewriting the row on every
--     request and would be wrong the moment a request was not recorded.
--
--     Stored as a day count rather than an interval because the control offers
--     whole days, an integer needs no pgtype.Interval at the Go boundary, and a
--     day count cannot smuggle a month or a microsecond into a security
--     deadline.
--
--   THE TWO ARE INDEPENDENT AND BOTH BIND. A grant is refused if it is past its
--   absolute expiry OR past its idle deadline. Neither can rescue the other.
--
-- ===========================================================================
-- DECISION 3: EXPIRY IS PART OF THE `authorized` VERDICT, NOT A SECOND READ
-- ===========================================================================
--
-- ReCheckMCPRequestAuthorizationInTenantTx already runs on EVERY REQUEST and
-- already returns `authorized` as the whole verdict in one column, "computed
-- here so that no caller reassembles it". Both expiries join that predicate in
-- this migration's companion change to db/query/mcp_connections.sql.
--
-- A GRANT PAST EITHER EXPIRY NOW FAILS THE SAME PREDICATE A REVOKED GRANT
-- FAILS. It is one boolean, one index probe on a primary key, no extra round
-- trip, and no opportunity for a caller to forget the check -- which is the
-- brief's requirement that expiry be enforceable at the point of use, cheaply.
--
-- THE WIREFRAME'S "no silent read-only period" IS A CONSEQUENCE OF THIS SHAPE.
-- Expiry flips `authorized` to false outright; it does not degrade the
-- connection to a reduced capability set. There is no code path here that could
-- produce a half-working connection, because there is one boolean and not a
-- tier.
--
-- NO SWEEP JOB IS REQUIRED FOR ENFORCEMENT. Expiry is evaluated at read time
-- against now(), so a grant stops working at the instant it expires whether or
-- not any background worker ran. A sweep is needed only for the wireframe's
-- courtesy email ("14 days and 1 day before") and, optionally, for flipping
-- status to a terminal value for display. Neither is an enforcement dependency,
-- and a sweep that fails to run must never be able to extend a credential.
--
-- NO NEW INDEX. The hot path reaches the grant by primary key through the join
-- on t.grant_id, so both expiry predicates are evaluated on an already-fetched
-- row and index nothing. mcp_grants_live_idx (partial on status = 'active')
-- already serves the notification sweep's candidate scan. Adding an index on
-- expires_at would earn nothing on either path.
--
-- ===========================================================================
-- DECISION 4: IDLE EXPIRY READS coalesce(last_used_at, created_at), AND IT IS
--             INERT BY CONSTRUCTION UNTIL THE ACTIVITY STAMP IS WIRED
-- ===========================================================================
--
-- THIS IS THE MOST DANGEROUS THING IN THIS MIGRATION AND IT IS WRITTEN DOWN
-- RATHER THAN LEFT TO BE DISCOVERED.
--
-- mcp_grants.last_used_at IS NEVER WRITTEN TODAY. TouchMCPGrantInTenantTx
-- exists in db/query/mcp_connections.sql, generates cleanly, and HAS ZERO GO
-- CALLERS:
--
--   grep -rn "TouchMCPGrant" apps/api --include="*.go" | grep -v "internal/db/sqlc/"
--     -> 0 matches
--
-- TouchMCPConnectionTokenInTenantTx is in the same state, also 0 callers. So
-- every mcp_grants row in existence has last_used_at IS NULL regardless of how
-- heavily its connection is used.
--
-- CONSEQUENCE, STATED PLAINLY: if a non-NULL idle_expire_after_days is ever
-- written while the stamp is still unwired, coalesce(last_used_at, created_at)
-- collapses to created_at permanently, and EVERY AFFECTED CONNECTION DIES
-- idle_expire_after_days AFTER IT WAS CREATED NO MATTER HOW ACTIVE IT IS. With
-- the wireframe's pre-selected 30 days that is a fleet-wide outage of every AI
-- connection, thirty days after this ships, with no operator action and nothing
-- in any log naming the cause.
--
-- THE SCHEMA IS SAFE BY CONSTRUCTION AGAINST THAT, AND ONLY BY CONSTRUCTION:
-- idle_expire_after_days is NULL with NO DEFAULT, and NULL means never
-- idle-expire, so no existing row and no row inserted by a caller that does not
-- mention the column can idle-expire. The hazard is unreachable until Go
-- deliberately writes a value into it.
--
-- THE ORDERING CONSTRAINT THIS PLACES ON backend-architect IS HARD:
-- TouchMCPGrantInTenantTx must be called on the request path BEFORE the API is
-- allowed to persist any non-NULL idle_expire_after_days. Wiring the stamp is a
-- prerequisite of Step 5's second control, not a follow-up to it.
--
-- coalesce(last_used_at, created_at) is the right expression once the stamp is
-- live -- a grant that has never been used is idle since it was created, which
-- is precisely the credential the wireframe is aimed at ("a connection nobody
-- has used in a month is a credential nobody is watching") -- and it is also
-- what makes the unwired state degrade to "expires at created_at + N" rather
-- than to something unbounded. It fails CLOSED, which is why the outage above
-- is an outage and not a silent extension of every credential. That is the
-- correct direction and it is still an outage.
--
-- ===========================================================================
-- DECISION 5: RLS IS INHERITED, NOT ADDED, AND WHICH DISPATCH HELPER BINDS IT
-- ===========================================================================
--
-- THIS MIGRATION CREATES NO TABLE AND NO POLICY. It adds three columns to
-- mcp_grants, which m124 already put under ENABLE plus FORCE ROW LEVEL
-- SECURITY with five policies: the permissive mcp_grants_tenant_isolation and
-- the four RESTRICTIVE mcp_grants_site_scope_{select,insert,update,delete}.
-- RLS filters ROWS, not columns, so a new column on a protected table is
-- protected by the existing policies with no further statement. Adding a
-- redundant policy here would be a second thing to keep in sync.
--
-- NO ROW IS OWED IN db/rls-cross-tenant-policies.txt. That ledger records
-- policies that grant access ACROSS tenants. Every policy on mcp_grants is
-- tenant-isolated, none of them is cross-tenant, and this migration adds no
-- policy of any kind. The ledger is unchanged and check-rls-cross-tenant.sh
-- must stay green without an edit.
--
-- WHICH DISPATCH HELPER A CALLER MUST USE: db.RunTenantTx, ALWAYS, AND THAT IS
-- A CORRECTNESS REQUIREMENT AND NOT A CONVENTION. The four site_scope policies
-- are RESTRICTIVE and each tests
-- coalesce(current_setting('app.site_scope', true), '') <> 'on'. RunTenantTx
-- (internal/db/db.go:772, dispatching through dispatchTenantTx at :755-763) is
-- THE ONLY THING THAT SETS THAT GUC. A call site that picks InTenantTx,
-- InTenantTxAsUser or InScopedTenantTx itself leaves it unset, the coalesced
-- empty string is not equal to 'on', THE RESTRICTIVE CHECK PASSES, and the
-- statement proceeds with no error raised anywhere.
--
-- A live privilege escalation of exactly that shape was fixed in this area on
-- 2026-08-30: a call site used InTenantTx directly, app.site_scope was never
-- set, and the policy admitted the write. A POLICY THAT TESTS A GUC IS INERT ON
-- ANY PATH THAT DOES NOT SET IT. Capabilities and expiry are now part of what a
-- site-scoped collaborator must not be able to widen, so every write to these
-- three columns inherits that requirement.
--
-- ===========================================================================
-- DECISION 6: THE BACKFILL PRESERVES TODAY'S EFFECTIVE AUTHORITY EXACTLY, AND
--             IS NOT A PERMISSIVE DEFAULT
-- ===========================================================================
--
-- Two of the three columns are NOT NULL, and mcp_grants is no longer guaranteed
-- empty -- m124 applied on 2026-08-26 and this file lands on 2026-08-29, so a
-- production row may exist. m124 DECISION 1(c) explicitly rested on "these
-- tables start empty, so no such backfill hazard exists"; THAT SENTENCE HAS
-- EXPIRED and this migration must handle rows.
--
-- A DEFAULT CLAUSE AND A ONE-TIME BACKFILL ARE NOT THE SAME THING. No column
-- below carries a DEFAULT. Step (0) sets a value on rows that already exist and
-- then the NOT NULL is armed, so every FUTURE insert must still say. The
-- distinction is the whole of constraint 1 in the brief.
--
--   capabilities -> ARRAY['mcp.sites.read'].
--     THIS GRANTS NOTHING NEW AND IT IS NOT A WILDCARD. It is the exact,
--     complete set every existing grant already holds, read off the code rather
--     than chosen: internal/mcp/service.go resolves the capability set as
--     OrgDefaultCapabilities(grantScopes()), grantScopes() returns exactly
--     {ScopeRead} for every live grant by construction, and scopeCapabilities
--     maps ScopeRead to exactly {CapSitesRead}. So the backfill writes down what
--     the running system already computes. Any existing connection keeps working
--     with precisely the authority it has today, and gains none.
--
--     '{}' WAS CONSIDERED AND REJECTED. It is the more restrictive value, but it
--     would silently strip every existing connection to zero capabilities the
--     moment backend-architect wires the read, which is an outage caused by a
--     migration rather than by an operator decision -- and it would be an
--     outage justified as safety while the safe-and-correct value was known and
--     derivable. Fail-closed is the right default when the answer is unknown.
--     Here it is known.
--
--   expires_at -> now() + interval '90 days'.
--     90 days is the wireframe's pre-selected choice. now() AND NOT
--     created_at + 90 days, deliberately: keying off created_at would make any
--     grant older than 90 days expire the instant this migration applies,
--     inside main() at boot, on every install at once. now() + 90 days cannot
--     be in the past for any row, so no existing connection is killed by
--     applying this file, and every operator gets a full 90 days of notice to
--     choose a real expiry. This is the m110/m111 lesson: a backfill that can
--     land a row in an invalid or hostile state is the thing that needs the
--     repair migration.
--
--   idle_expire_after_days -> left NULL.
--     Nothing to backfill. NULL is a legal, meaningful value on this column
--     (never idle-expire) and it is the only safe one while DECISION 4's stamp
--     is unwired.
--
-- EVERY STATEMENT BELOW IS IDEMPOTENT AND THE FILE IS RE-RUNNABLE. The backfill
-- is guarded by IS NULL so a second application updates zero rows and cannot
-- push an operator-chosen expiry 90 days into the future.

-- ---------------------------------------------------------------------------
-- (0) Columns, added nullable so existing rows survive the ALTER.
-- ---------------------------------------------------------------------------

ALTER TABLE "public"."mcp_grants"
    ADD COLUMN IF NOT EXISTS "capabilities" text[];

ALTER TABLE "public"."mcp_grants"
    ADD COLUMN IF NOT EXISTS "expires_at" timestamptz;

ALTER TABLE "public"."mcp_grants"
    ADD COLUMN IF NOT EXISTS "idle_expire_after_days" integer;

-- ---------------------------------------------------------------------------
-- (1) Backfill. WHERE ... IS NULL makes this a no-op on a second run and
--     protects a value an operator has already chosen. See DECISION 6.
-- ---------------------------------------------------------------------------

UPDATE "public"."mcp_grants"
   SET "capabilities" = ARRAY['mcp.sites.read']::text[]
 WHERE "capabilities" IS NULL;

UPDATE "public"."mcp_grants"
   SET "expires_at" = now() + interval '90 days'
 WHERE "expires_at" IS NULL;

-- ---------------------------------------------------------------------------
-- (2) Arm NOT NULL, now that no row violates it. SET NOT NULL is a no-op when
--     the column is already NOT NULL, so this is safe to re-run.
--     idle_expire_after_days stays NULLABLE -- NULL is a real value there.
-- ---------------------------------------------------------------------------

ALTER TABLE "public"."mcp_grants"
    ALTER COLUMN "capabilities" SET NOT NULL;

ALTER TABLE "public"."mcp_grants"
    ALTER COLUMN "expires_at" SET NOT NULL;

-- ---------------------------------------------------------------------------
-- (3) Constraints. Each guarded by a pg_constraint probe so the file re-runs.
-- ---------------------------------------------------------------------------

-- Shape of the capability set. m120's constraint verbatim in intent: one
-- dimension (a literal like '{{a},{b}}' would otherwise be flattened by
-- unnest() into capabilities nobody granted), no NULL element, no empty string,
-- and the 64 ceiling the wireframe states. IMMUTABLE expressions only.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.mcp_grants'::regclass
          AND conname  = 'mcp_grants_capabilities_shape_check'
    ) THEN
        ALTER TABLE "public"."mcp_grants"
            ADD CONSTRAINT "mcp_grants_capabilities_shape_check"
            CHECK (
                coalesce(array_ndims("capabilities"), 1) = 1
                AND cardinality("capabilities") <= 64
                AND array_position("capabilities", NULL) IS NULL
                AND NOT ('' = ANY ("capabilities"))
            );
    END IF;
END;
$$;

-- Closed vocabulary. See DECISION 1(c). `<@` is array containment and is
-- IMMUTABLE; the empty array is contained by every array, so '{}' -- zero
-- capabilities, the restrictive value -- passes, which is intended.
--
-- EXTENDING THIS LIST IS A MIGRATION, and that is the review gate m124
-- DECISION 1 asked for. Keep it in lockstep with capabilityVocabulary in
-- apps/api/internal/mcp/policy.go.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.mcp_grants'::regclass
          AND conname  = 'mcp_grants_capabilities_vocabulary_check'
    ) THEN
        ALTER TABLE "public"."mcp_grants"
            ADD CONSTRAINT "mcp_grants_capabilities_vocabulary_check"
            CHECK ("capabilities" <@ ARRAY['mcp.sites.read']::text[]);
    END IF;
END;
$$;

-- A grant cannot be born already expired. Nonsense rather than danger -- an
-- already-expired grant fails the DECISION 3 predicate and is refused -- but it
-- is unrepresentable rather than merely harmless, which is the house standard.
-- Immediate termination is REVOCATION (status = 'revoked'), not a backdated
-- expiry, and keeping the two mechanisms distinct keeps revoked_at honest.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.mcp_grants'::regclass
          AND conname  = 'mcp_grants_expires_at_after_created_check'
    ) THEN
        ALTER TABLE "public"."mcp_grants"
            ADD CONSTRAINT "mcp_grants_expires_at_after_created_check"
            CHECK ("expires_at" > "created_at");
    END IF;
END;
$$;

-- The idle window is a positive number of days, or NULL for "never". Zero is
-- refused because "expire it if unused for 0 days" has no meaning and would
-- read as an immediate kill dressed up as a policy; ten years is the ceiling
-- so a fat-fingered value cannot become an effectively-absent control.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.mcp_grants'::regclass
          AND conname  = 'mcp_grants_idle_expire_after_days_check'
    ) THEN
        ALTER TABLE "public"."mcp_grants"
            ADD CONSTRAINT "mcp_grants_idle_expire_after_days_check"
            CHECK (
                "idle_expire_after_days" IS NULL
                OR ("idle_expire_after_days" >= 1
                    AND "idle_expire_after_days" <= 3650)
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- (4) Grants.
--
-- Nothing to add. m1's ALTER DEFAULT PRIVILEGES and m124's explicit
-- GRANT SELECT, INSERT, UPDATE, DELETE ON mcp_grants TO wpmgr_app are
-- table-level and cover columns added later; PostgreSQL has no per-column
-- grant in force on this table that a new column could fall outside of. Stated
-- rather than left silent, because "the new column is invisible to wpmgr_app"
-- is the kind of failure that looks like an RLS bug for a day.
-- ---------------------------------------------------------------------------
