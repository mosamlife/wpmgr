-- m136 - give an MCP grant a PER-GRANT OAUTH SCOPE SET.
--
-- This migration CORRECTS NOTHING. It edits no applied migration, it re-runs no
-- earlier backfill, and it is not an m114/m115-shaped repair of m124, m127,
-- m131 or m135. Every one of those is correct and its design intent is
-- preserved here in full.
--
-- CONVERGE PATH. A database that has already applied m124 (which created
-- mcp_grants) converges by applying this file and nothing else. There is no
-- separate converge migration owed and no operator step. The reason is that
-- every statement below is guarded -- ADD COLUMN IF NOT EXISTS, an IS NULL
-- backfill, SET NOT NULL which is a no-op when already armed, and a
-- pg_constraint probe before each ADD CONSTRAINT -- so the post-state of this
-- file is a function of this file alone and not of what the database held
-- before. A database created fresh from db/schema.sql and a database that has
-- run every migration since m1 end with the identical column and the identical
-- four constraints.
--
-- ORDINAL. 20260907000000 / m136. origin/main's migrations end at m135
-- (20260906000000), merged by #738 as 0f84ca3d, which seats 'mcp.cache.purge'
-- in the CAPABILITY vocabulary. This file is the only migration on this branch
-- that origin/main does not already carry. m134 (20260905000000) never reached
-- origin/main -- it was authored in the same commit as m133 and only m133
-- landed -- and 'mcp.updates.propose' is in neither db/schema.sql nor
-- internal/mcp/policy.go, so it is not live. THIS FILE IS INDEPENDENT OF BOTH:
-- it touches no capability constraint and adds a column neither of them
-- mentions. m135's lower ordinal settles the order -- it applies first, on
-- every database -- and nothing below depends on that.
--
-- ===========================================================================
-- WHY IT EXISTS
-- ===========================================================================
--
-- internal/mcp/policy.go derives a grant's capability ceiling from its OAUTH
-- SCOPES, and the scopes are a HARD-CODED CONSTANT because there has only ever
-- been one scope:
--
--     func grantScopes() []Scope {
--         return []Scope{ScopeRead}
--     }
--
-- Its own comment says the reasoning "EXPIRES the moment a second scope is
-- recognised" and asks for "one obvious place to replace with a real per-grant
-- read". This column is that place. m124 DECISION 1 declined to mint it, and
-- that decision was right at the time and is what this file now retires, on
-- purpose and with a review, rather than by accident.
--
-- THE CONSEQUENCE OF THE CONSTANT IS NOT "MINT-ONLY", IT IS UNREACHABLE. Every
-- capability path narrows through the ceiling that constant produces
-- (mint.go:611 and :618, connection_tools.go:98, service.go:1443), so a
-- capability no scope confers cannot be stored at mint AND would be narrowed
-- away by Authenticate on every request even if a row somehow held it. A
-- capability seated in the vocabulary but conferred by no scope is therefore
-- dead in both directions. 'mcp.cache.purge' (m135) is in exactly that state.
--
-- ONLY THREE WAYS FORWARD EXISTED AND TWO ARE CLOSED.
--
--   1. CONFER THE WRITE VIA THE EXISTING READ SCOPE. REFUSED. Every live grant
--      holds ScopeRead by construction -- ParseRequestedScopes refuses anything
--      recognisedScopes does not carry, and it carries one entry -- so widening
--      scopeCapabilities[ScopeRead] retroactively hands a write capability to
--      every grant this surface has ever minted, with no second consent from
--      anyone. It is the precise failure m127 DECISION 1 and m131's closing
--      note both name: a credential nobody chose the terms of. A read scope
--      conferring a write is what this surface's own comments forbid.
--
--   2. A SECOND SCOPE. Requires per-grant scope storage, because with a second
--      scope in the registry the constant above stops being a description of
--      reality and becomes a WIDENING: a connection granted only the new scope
--      would still be handed ScopeRead's capabilities. That is this migration.
--
--   3. LEAVE IT UNREACHABLE. Today's state. It ships nothing.
--
-- ===========================================================================
-- DECISION 1: THE COLUMN IS `oauth_scopes`, NOT `scopes`
-- ===========================================================================
--
-- mcp_grants ALREADY CARRIES THREE COLUMNS WITH "scope" IN THE NAME AND NONE OF
-- THEM IS THIS: site_scope_mode, scope_tag_ids and scope_site_ids are the SITE
-- axis -- WHICH SITES a grant may touch (m124 DECISION 1) -- and they are
-- unrelated to WHAT A GRANT MAY DO. A bare `scopes` column sitting in the same
-- row as `scope_site_ids` is an invitation to read one for the other, in a
-- query, in a review, or in a policy predicate, and the two axes are joined by
-- nothing.
--
-- `oauth_scopes` cannot be misread as the site axis and names the RFC 6749
-- thing it actually holds. The cost is four characters; the thing bought is
-- that the two scope axes are never confusable by sight. The site axis keeps
-- its names: renaming three live columns to tidy this up would be a schema
-- change on an authorisation path bought with nothing.
--
-- ===========================================================================
-- DECISION 2: text[] WITH A CLOSED CHECK, MIRRORING `capabilities`
-- ===========================================================================
--
-- CONFIRMED AGAINST THE CODE, NOT ASSUMED.
--
--   a. AN ARRAY AND NOT A SCALAR. RFC 6749 section 3.3 makes `scope` a SET --
--      "a list of space-delimited, case-sensitive strings" -- and
--      ParseRequestedScopes already returns []Scope and already dedupes
--      repetition. A single text column would have to re-encode that set as a
--      delimited string, and a delimited string is the shape that makes
--      "mcp:read mcp:write" indistinguishable from a single scope literally
--      named "mcp:read mcp:write". The Go type is a slice; the column is an
--      array.
--
--   b. text[] AND NOT AN ENUM. `capabilities` is text[] on this same table and
--      the vocabulary is closed by CHECK rather than by type. That is not an
--      accident of history: a Postgres enum cannot be narrowed, its members
--      cannot be removed, and ALTER TYPE ... ADD VALUE could not run inside the
--      single transaction this runner wraps each migration in before PG 12 and
--      still carries its own hazards. A CHECK over a literal array is droppable
--      and re-addable in one transaction, which is exactly what m131 did to
--      widen the capability vocabulary.
--
--   c. AND -- the decisive reason -- THE CONSTRAINT IS READ BACK AT RUNTIME.
--      m131 DECISION 5 established that the database's closed set and Go's
--      closed set must be PROVED equal rather than asked to agree in a comment,
--      and the parity test does it by extracting the literals from
--      pg_get_constraintdef:
--
--          SELECT m[1] FROM pg_constraint c
--            CROSS JOIN LATERAL regexp_matches(
--                     pg_get_constraintdef(c.oid), '''([^'']*)''::text', 'g') AS m
--           WHERE c.conrelid = 'public.mcp_grants'::regclass
--             AND c.conname  = '<name>';
--
--      A `<@ ARRAY[...]::text[]` CHECK renders in exactly that shape, so the
--      same extraction works verbatim against
--      mcp_grants_oauth_scopes_vocabulary_check. An enum would need a different
--      catalogue query and a second parity mechanism. One mechanism, two
--      constraints.
--
--      THAT PARITY TEST IS OWED FOR THIS CONSTRAINT AND IS NOT IN THIS FILE --
--      it is Go, and Go is not this migration's lane. See section (5).
--
--   d. SHAPE IS CHECKED AS m120 AND m127 CHECK IT: one dimension (a nested
--      literal like '{{a},{b}}' would otherwise be flattened by unnest() into
--      scopes nobody granted), no NULL element, no empty string. IMMUTABLE
--      expressions only, so the constraint is valid in a CHECK.
--
--      THE CEILING IS 16, AND IT IS A PATHOLOGICAL-INPUT BOUND RATHER THAN A
--      POLICY. It is deliberately NOT m127's 64: 64 is the wireframe's stated
--      property of the credential for CAPABILITIES, which an operator ticks
--      individually. Scopes are not ticked; they are requested by a client from
--      a registry that holds ONE member today and is expected to hold a
--      handful. With the vocabulary CHECK in force, duplication is the only way
--      to approach 16 at all.
--
--      NOT CHECKED: element uniqueness, for m127's reason -- deduplication
--      needs a subquery (cardinality vs. a DISTINCT unnest) and CHECK forbids
--      one. A duplicate is harmless to the set-membership read the ceiling is
--      derived by, and ParseRequestedScopes already dedupes before any INSERT.
--
-- ===========================================================================
-- DECISION 3: NOT NULL, AND EMPHATICALLY NO DATABASE DEFAULT
-- ===========================================================================
--
-- m127 DECISION 1(a) is the reasoning and it transfers without amendment.
--
--   NOT NULL. A nullable scope column that Go reads as "no scope restriction
--   recorded, therefore the ceiling is everything" is the exact defect this
--   whole surface has been fighting. NULL could only mean "unknown", and an
--   unknown grant is refused, so the value is not worth being able to store. A
--   caller that omits the column gets 23502 -- a loud failure at INSERT -- and
--   not an unrestricted connection.
--
--   NO DEFAULT, AND THIS IS THE PART THAT MATTERS MOST HERE. A DEFAULT
--   ARRAY['mcp:read'] would be indistinguishable from this migration's backfill
--   at the moment it applies and completely different forever after: a backfill
--   sets a value on rows that ALREADY EXIST, once; a DEFAULT silently seats a
--   scope on every FUTURE insert that forgets to say. policy.go's own comment
--   on DefaultGrantCapabilities says it in as many words -- "It is deliberately
--   NOT a database DEFAULT -- see the column's NOT NULL and m127's reasoning
--   about a credential nobody chose the terms of". A DEFAULT here would rebuild
--   that trap in a new place, one layer below where anybody is looking for it,
--   and it would do so on the column from which the entire capability ceiling
--   is derived.
--
--   THE DISTINCTION IS NOT THEORETICAL. The moment a second scope is
--   recognised, a DEFAULT of the read scope means an INSERT that meant to grant
--   the write scope and omitted the column grants the read scope instead, and
--   nothing anywhere fails. With no DEFAULT it is 23502.
--
--   WHAT THE DEFAULT WOULD HAVE BOUGHT -- not having to touch the Go INSERT
--   path -- IS NOT A SAVING. backend-architect must touch that path anyway to
--   replace grantScopes(); see section (5).
--
-- ===========================================================================
-- DECISION 4: A ZERO-LENGTH SCOPE SET IS UNREPRESENTABLE
-- ===========================================================================
--
-- THIS IS THE ONE PLACE THIS COLUMN DELIBERATELY DIVERGES FROM `capabilities`,
-- SO THE DIVERGENCE IS ARGUED RATHER THAN ASSUMED.
--
-- m127 DECISION 1(b) permits capabilities = '{}' and is right to: '{}' is the
-- RESTRICTIVE direction there, a connection holding it reaches no tool, and
-- CapabilitySet's zero value has the same property, so the database and Go
-- agree. Zero capabilities is a coherent, safe, gradual state.
--
-- ZERO SCOPES IS NOT THE SAME KIND OF VALUE. It is not "a grant that may do
-- less"; it is a grant that no OAuth exchange could ever have produced.
-- ParseRequestedScopes refuses an absent, empty or whitespace-only `scope`
-- parameter outright -- "absence is refusal, never 'everything we have'" -- so
-- the authorisation layer has ALREADY DECLARED a scopeless grant impossible.
-- A row holding '{}' is a credential in a state the front door cannot produce,
-- and the only ways to get one are a bug or a deliberate write.
--
-- THE PRECEDENT ON THIS TABLE IS EXACTLY THIS SHAPE.
-- mcp_grants_site_scope_payload_check exists to make "requested a scope and
-- named nothing" unrepresentable on the site axis, for the same reason: an
-- empty allowlist "grants nothing and is not a way to request every site". This
-- constraint is that sentence applied to the authority axis.
--
-- IT IS ALSO THE AXIS WHERE AN EMPTY VALUE IS MOST LIKELY TO BE MISREAD. The
-- capability ceiling is DERIVED from this column, and a derivation over an
-- empty input produces an empty ceiling that is indistinguishable, downstream,
-- from a deliberate narrowing -- so a scopeless row would look exactly like a
-- correctly minted, harmless grant right up until somebody "fixed" the empty
-- ceiling by falling back to a default.
--
-- IT IS NOT A WAY TO KILL A CREDENTIAL, AND THAT MATTERS. Immediate termination
-- is REVOCATION (status = 'revoked'), which m127's expiry constraint keeps
-- distinct from a backdated expiry for the same reason: one mechanism per
-- meaning, so revoked_at stays honest. Narrowing a live grant to zero authority
-- by emptying its scope set would be a third, undiscoverable mechanism whose
-- audit trail is nothing at all.
--
-- IT IS ITS OWN NAMED CONSTRAINT rather than a clause folded into the shape
-- check, so that the SQLSTATE 23514 an operator or a test sees NAMES WHICH RULE
-- WAS BROKEN. Three rules, three names, three distinguishable failures.
--
-- ===========================================================================
-- DECISION 5: THE VOCABULARY IS SEATED AT `mcp:read` AND NOTHING ELSE
-- ===========================================================================
--
-- THIS MIGRATION SEATS NO SECOND SCOPE. It builds the PLACE a second scope will
-- be stored, and it stops there.
--
-- recognisedScopes in internal/mcp/scope.go is the other closed set and it
-- holds exactly one entry, ScopeRead = 'mcp:read'. m131 DECISION 5's rule
-- applies to this pair too: two closed sets with two answers is worse than one
-- open set. A scope the database accepts and Go does not is refused at a
-- different layer with a different error; a scope Go accepts and the database
-- does not is a 23514 at INSERT on a path an operator reached through a wizard.
-- So the constraint below holds exactly what recognisedScopes holds, today.
--
-- THE WRITE SCOPE ARRIVES IN ITS OWN MIGRATION, WITH ITS OWN REVIEW. That is
-- m131 DECISION 2's stance and scope.go already states the same rule in its own
-- words -- "A write scope arrives with its own migration and its own review,
-- never by being appended here." Seating it here to save a later migration
-- would spend the gate to save the toll, which is backwards: the toll is the
-- cheap part and the gate is the point. Widening a containment CHECK is
-- MONOTONE (m131 DECISION 4), so that later migration is a drop-and-re-add of
-- this constraint by name and cannot invalidate a single existing row.
--
-- EVERY MEMBER OF THIS VOCABULARY IS `mcp:read`, WHICH IS A READ SCOPE, and
-- that is checkable by looking rather than by trust. The first migration to add
-- a member that is not 'mcp:read' is the one that owes the write review.
--
-- ===========================================================================
-- DECISION 6: THE BACKFILL PRESERVES TODAY'S AUTHORITY EXACTLY
-- ===========================================================================
--
-- THIS MIGRATION RAISES A CEILING AND MOVES NO FLOOR. It is the same stance
-- m131 DECISION 4 took by refusing a backfill, applied to a case that cannot
-- refuse one: the column is NOT NULL, mcp_grants is not guaranteed empty, and
-- so a value must be written for rows that already exist.
--
--   oauth_scopes -> ARRAY['mcp:read'].
--
--     THIS GRANTS NOTHING NEW AND IT IS NOT A WILDCARD. It is the exact,
--     complete set every existing grant already holds, READ OFF THE CODE rather
--     than chosen: grantScopes() returns exactly {ScopeRead} for every live
--     grant, and it can do so correctly because ParseRequestedScopes refuses
--     anything recognisedScopes does not carry and recognisedScopes carries
--     exactly one entry. The backfill writes down what the running system
--     already computes. Every existing connection keeps precisely the authority
--     it has today and gains none; its derived capability ceiling is byte-for-
--     byte the ceiling it had before this file applied.
--
--     '{}' WAS NOT AN OPTION and would not have been the safe choice even if
--     DECISION 4 permitted it. It would silently strip every existing
--     connection to zero authority the moment backend-architect wires the read
--     -- an outage caused by a migration rather than by an operator decision,
--     justified as safety while the safe-and-correct value was known and
--     derivable. Fail-closed is the right default when the answer is unknown.
--     Here it is known.
--
--     A WIDER BACKFILL WAS NEVER AN OPTION. There is no wider value to write:
--     the vocabulary holds one member.
--
-- A DEFAULT CLAUSE AND A ONE-TIME BACKFILL ARE NOT THE SAME THING; see
-- DECISION 3. No column below carries a DEFAULT.
--
-- EVERY STATEMENT IS IDEMPOTENT AND THE FILE IS RE-RUNNABLE. The backfill is
-- guarded by IS NULL, so a second application updates zero rows and cannot
-- overwrite a scope set an operator or the API has since chosen.
--
-- ===========================================================================
-- DECISION 7: RLS IS INHERITED, NOT ADDED
-- ===========================================================================
--
-- THIS MIGRATION CREATES NO TABLE AND NO POLICY. It adds one column to
-- mcp_grants, which m124 already put under ENABLE plus FORCE ROW LEVEL SECURITY
-- with five policies: the permissive mcp_grants_tenant_isolation and the four
-- RESTRICTIVE mcp_grants_site_scope_{select,insert,update,delete}. RLS filters
-- ROWS, not columns, so a new column on a protected table is protected by the
-- existing policies with no further statement, and a redundant policy here
-- would be a second thing to keep in sync.
--
-- NO ROW IS OWED IN db/rls-cross-tenant-policies.txt. That ledger records
-- policies granting access ACROSS tenants. Every policy on mcp_grants is
-- tenant-isolated, none is cross-tenant, and this migration adds no policy of
-- any kind. check-rls-cross-tenant.sh must stay green without an edit.
--
-- WHICH DISPATCH HELPER A WRITER MUST USE: db.RunTenantTx, ALWAYS, and that is
-- a correctness requirement and not a convention. The four site_scope policies
-- are RESTRICTIVE and each tests
-- coalesce(current_setting('app.site_scope', true), '') <> 'on'. RunTenantTx is
-- the only thing that sets that GUC; a call site that picks InTenantTx,
-- InTenantTxAsUser or InScopedTenantTx itself leaves it unset, the coalesced
-- empty string is not equal to 'on', THE RESTRICTIVE CHECK PASSES, and the
-- write proceeds with no error raised anywhere. A live privilege escalation of
-- exactly that shape was fixed in this area on 2026-08-30. oauth_scopes is now
-- part of what a site-scoped collaborator must not be able to widen, so every
-- write to it inherits that requirement.
--
-- NO NEW INDEX. The hot path reaches the grant by primary key through the join
-- on t.grant_id, so this column is read off an already-fetched row and indexes
-- nothing. mcp_grants_live_idx and mcp_grants_tenant_idx are untouched.
--
-- NO GRANT STATEMENT. m1's ALTER DEFAULT PRIVILEGES and m124's explicit
-- GRANT SELECT, INSERT, UPDATE, DELETE ON mcp_grants TO wpmgr_app are
-- table-level and cover columns added later; there is no per-column grant in
-- force on this table that a new column could fall outside of. Stated rather
-- than left silent, because "the new column is invisible to wpmgr_app" is the
-- kind of failure that looks like an RLS bug for a day.

-- ---------------------------------------------------------------------------
-- (0) The column, added NULLABLE so existing rows survive the ALTER.
-- ---------------------------------------------------------------------------

ALTER TABLE "public"."mcp_grants"
    ADD COLUMN IF NOT EXISTS "oauth_scopes" text[];

-- ---------------------------------------------------------------------------
-- (1) Backfill. WHERE ... IS NULL makes this a no-op on a second run and
--     protects a value the API has since chosen. See DECISION 6.
-- ---------------------------------------------------------------------------

UPDATE "public"."mcp_grants"
   SET "oauth_scopes" = ARRAY['mcp:read']::text[]
 WHERE "oauth_scopes" IS NULL;

-- ---------------------------------------------------------------------------
-- (2) Arm NOT NULL, now that no row violates it. SET NOT NULL is a no-op when
--     the column is already NOT NULL, so this is safe to re-run.
-- ---------------------------------------------------------------------------

ALTER TABLE "public"."mcp_grants"
    ALTER COLUMN "oauth_scopes" SET NOT NULL;

-- ---------------------------------------------------------------------------
-- (3) Constraints. Each guarded by a pg_constraint probe so the file re-runs.
--     Each ADDED VALIDATED, never NOT VALID: PostgreSQL scans the table and
--     would refuse this migration outright rather than leave an unchecked row.
-- ---------------------------------------------------------------------------

-- Shape. See DECISION 2(d).
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.mcp_grants'::regclass
          AND conname  = 'mcp_grants_oauth_scopes_shape_check'
    ) THEN
        ALTER TABLE "public"."mcp_grants"
            ADD CONSTRAINT "mcp_grants_oauth_scopes_shape_check"
            CHECK (
                coalesce(array_ndims("oauth_scopes"), 1) = 1
                AND cardinality("oauth_scopes") <= 16
                AND array_position("oauth_scopes", NULL) IS NULL
                AND NOT ('' = ANY ("oauth_scopes"))
            );
    END IF;
END;
$$;

-- A grant holds at least one scope. See DECISION 4. Its own name so the 23514
-- says which rule was broken.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.mcp_grants'::regclass
          AND conname  = 'mcp_grants_oauth_scopes_not_empty_check'
    ) THEN
        ALTER TABLE "public"."mcp_grants"
            ADD CONSTRAINT "mcp_grants_oauth_scopes_not_empty_check"
            CHECK (cardinality("oauth_scopes") >= 1);
    END IF;
END;
$$;

-- Closed vocabulary. See DECISION 5. `<@` is array containment and is
-- IMMUTABLE. Containment alone would admit '{}' -- the empty array is contained
-- by every array -- which is why emptiness is refused by its own constraint
-- above rather than by this one.
--
-- KEEP IN LOCKSTEP with recognisedScopes in apps/api/internal/mcp/scope.go.
-- EXTENDING THIS LIST IS A MIGRATION. The name is chosen to be stable: the
-- parity test owed in section (5) looks the constraint up BY NAME, and a later
-- migration that widens this list must drop and re-add THIS name, exactly as
-- m131 did for the capability vocabulary.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.mcp_grants'::regclass
          AND conname  = 'mcp_grants_oauth_scopes_vocabulary_check'
    ) THEN
        ALTER TABLE "public"."mcp_grants"
            ADD CONSTRAINT "mcp_grants_oauth_scopes_vocabulary_check"
            CHECK ("oauth_scopes" <@ ARRAY['mcp:read']::text[]);
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- (4) WHAT THIS MIGRATION DOES NOT DO
-- ---------------------------------------------------------------------------
--
-- It does not widen a single existing grant. Every grant that could reach
-- {mcp:read}'s capabilities before this file can reach exactly those after it,
-- and no grant can reach more.
--
-- It does not make 'mcp.cache.purge' reachable. That capability is seated in
-- the capability vocabulary by m135 and is conferred by no scope; conferring it
-- needs a NEW SCOPE (a migration widening the vocabulary above) AND a Go change
-- mapping that scope to it. Neither is in this file.
--
-- It does not change how any capability is computed today. grantScopes() is
-- still a constant and still returns {ScopeRead}; this column is WRITTEN by the
-- backfill and READ BY NOTHING until section (5) lands.
--
-- ---------------------------------------------------------------------------
-- (5) WHAT THE GO LAYER MUST NOW DO -- HANDED TO backend-architect
-- ---------------------------------------------------------------------------
--
-- A note to the next engineer, not work this file performs. It is here because
-- whoever does that work will be reading this file.
--
--   a. THE INSERT PATH MUST NAME THE COLUMN, AND IT MUST DO SO IN THE SAME
--      CHANGE THAT REGENERATES sqlc. The column is NOT NULL WITH NO DEFAULT
--      (DECISION 3), so every INSERT into mcp_grants that does not name it
--      fails 23502. That is the intended, loud failure, and it is why this
--      migration lands FIRST and separately: between this commit and that one,
--      the Go insert path is red, on purpose and visibly, rather than silently
--      minting scopeless credentials.
--
--   b. grantScopes() BECOMES A REAL PER-GRANT READ of this column. It currently
--      returns a constant and policy.go says so in its own comment, naming this
--      as "one obvious place to replace with a real per-grant read". The
--      function must be handed the row's oauth_scopes.
--
--   c. THE READ MUST NOT FILTER UNKNOWN SCOPES, for the reason
--      capabilitiesFromColumn already gives for capabilities: dropping an
--      unknown name would hand the ceiling computation a set that had already
--      been quietly reduced, so a row carrying a scope outside this build's
--      registry would authenticate as though it carried only the known ones.
--      The refusal belongs where an unheld scope rejects the whole set, which
--      is what ParseRequestedScopes already does for a request.
--
--   d. AN EMPTY OR NIL COLUMN VALUE MUST BE TREATED AS "NO AUTHORITY" AND NEVER
--      AS "UNRESTRICTED". DECISION 4 makes it unstorable, but the Go read must
--      not depend on the database being the only writer.
--
--   e. THE PARITY TEST IS OWED (DECISION 2(c) and DECISION 5). It must read
--      mcp_grants_oauth_scopes_vocabulary_check back through
--      pg_get_constraintdef and assert BOTH directions against recognisedScopes
--      -- no member Go lacks, no member the database lacks -- and it must FAIL
--      rather than pass when the extraction returns no rows, because an empty
--      extraction reads as "the sets agree" and is the exact shape of a check
--      that guards nothing.
--
--   f. TestGrantScopesIsExactOnlyWhileOneScopeExists, which policy.go names as
--      the pin on the constant, is the test that must change when (b) lands.
--
--   g. NONE OF THAT CONFERS A WRITE CAPABILITY. It makes the scope set
--      per-grant and honest. The write scope itself is a further migration
--      widening the vocabulary in section (3), plus the scopeCapabilities
--      mapping that confers a capability from it, under their own review.
