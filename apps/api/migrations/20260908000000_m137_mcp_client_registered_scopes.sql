-- m137 - record on each OAuth CLIENT the scopes it is registered to request.
--
-- This migration CORRECTS NOTHING. It edits no applied migration, it re-runs no
-- earlier backfill, and it is not an m114/m115-shaped repair of m124, m126,
-- m127, m136 or anything else. Every one of those is correct and its design
-- intent is preserved here in full. m136 is its direct sibling and this file
-- leans on its reasoning by name rather than restating it.
--
-- CONVERGE PATH. A database that has already applied m124 (which created
-- mcp_oauth_clients) and m126 (which dropped last_used_at from it) converges by
-- applying this file and nothing else. There is no separate converge migration
-- owed and no operator step. The reason is the reason m136 gives: every
-- statement below is guarded -- ADD COLUMN IF NOT EXISTS, an IS NULL backfill,
-- SET NOT NULL which is a no-op when already armed, and a pg_constraint probe
-- before each ADD CONSTRAINT -- so the post-state of this file is a function of
-- this file alone and not of what the database held before. A database created
-- fresh from db/schema.sql and a database that has run every migration since m1
-- end with the identical column and the identical three constraints.
--
-- ORDINAL. 20260908000000 / m137. origin/main's migrations end at m136
-- (20260907000000), merged by #745 as 7bed036f, which gave mcp_grants its
-- per-grant oauth_scopes column. Confirmed by listing every migration ever
-- added anywhere in this repository's history, not merely on this branch:
--
--     git log --all --diff-filter=A --name-only --format='%h %d' \
--       -- 'apps/api/migrations/*.sql' | grep -E 'migrations/2026(09|1[0-2])'
--
-- which returns m130 through m136 and nothing above. m134 (20260905000000)
-- appears in that history and NEVER REACHED origin/main -- m136's header
-- records why -- so 20260908000000 is the next free ordinal and is also the
-- next ordinal in apply order. Nothing unmerged sits between m136 and this file.
--
-- ===========================================================================
-- WHY IT EXISTS
-- ===========================================================================
--
-- Authorize (internal/mcp/service.go) validates the requested scope against the
-- GLOBAL registry and against nothing else:
--
--     // THE EXIT GATE. A client requesting no recognised scope is refused
--     // here, before any consent screen is drawn, and is never granted a
--     // default.
--     scopes, err := ParseRequestedScopes(req.Scope)
--
-- ParseRequestedScopes answers one question -- "is every member of this string
-- in recognisedScopes?" -- and recognisedScopes is a package-level map shared by
-- every client in the fleet. The client row is then looked up, and it is
-- consulted about its REDIRECT URIS and about nothing else: mcp_oauth_clients
-- records client_id, the secret hash, the auth method, redirect_uris and two
-- display fields. There is nowhere on that row to say WHAT THIS CLIENT MAY ASK
-- FOR, so nothing ties a client to a scope set and every client is implicitly
-- registered for the whole registry.
--
-- THIS COLUMN IS THE MISSING RIGHT-HAND SIDE OF THAT COMPARISON. The check
-- itself -- requested must be contained by registered -- is Go and is the next
-- engineer's; see section (5). This file builds the place the answer is stored
-- and makes every value it can hold a safe one.
--
-- THE MECHANISM THIS CLOSES IS ALREADY WRITTEN DOWN AT THE CALL SITE, in the
-- long comment above the ParseRequestedScopes call in Approve, and it is not
-- restated here. Read it there. What matters to the schema is the shape of the
-- fix and not the shape of the attack: the authority a client may request
-- becomes a per-row fact chosen at registration, rather than a global constant
-- every row inherits.
--
-- NOTHING IS REACHABLE TODAY AND THAT IS NOT AN ARGUMENT AGAINST THE COLUMN.
-- recognisedScopes holds exactly one entry, so "every recognised scope" and
-- "mcp:read" are the same set and a client cannot request anything it should
-- not. The registry is expected to grow -- m136 exists precisely to make it
-- safe to grow -- and the day a second scope is recognised is the day the
-- global check becomes a widening. The storage lands BEFORE that day, on
-- purpose, so the second scope's migration is a one-line vocabulary widening
-- under review and not a scramble to invent per-client storage at the same
-- time. m136 DECISION 5 took the same stance for the same reason.
--
-- ===========================================================================
-- DECISION 1: THE COLUMN IS `registered_scopes`, NOT `oauth_scopes`
-- ===========================================================================
--
-- m136 DECISION 1 named mcp_grants.oauth_scopes rather than `scopes` because
-- that table already carried three site-axis columns with "scope" in the name.
-- THAT PARTICULAR HAZARD DOES NOT EXIST HERE: mcp_oauth_clients carries no site
-- axis and no other scope column, so `oauth_scopes` would have been unambiguous
-- on this table read alone.
--
-- IT IS REFUSED FOR A DIFFERENT REASON: THESE TWO COLUMNS ARE NOT THE SAME
-- FACT, AND THE WHOLE DEFECT IS THAT SOMETHING CONFLATED TWO SCOPE SETS.
--
--   mcp_grants.oauth_scopes            what one connection WAS GRANTED, by a
--                                      human, at consent. The capability
--                                      ceiling is derived from it.
--   mcp_oauth_clients.registered_scopes what this client MAY ASK FOR, recorded
--                                      unauthenticated at registration. It
--                                      confers nothing and is derived into
--                                      nothing.
--
-- The relationship between them is containment in one direction only: once
-- section (5) lands, every grant's oauth_scopes is a subset of the registering
-- client's registered_scopes, never the reverse, and never equal by
-- construction. Giving both columns the same name would put that asymmetry
-- behind an identical spelling, in a codebase whose live bug is that a scope
-- set was checked against the wrong other scope set. `registered_scopes` names
-- the RFC 7591 registration fact -- section 2's client metadata field is
-- literally `scope` -- and cannot be read as "what this client got".
--
-- THE CONSTRAINT NAMES FOLLOW THE COLUMN, and are chosen to be STABLE: the
-- vocabulary constraint is looked up BY NAME by the parity test owed in section
-- (5), and a later migration widening the vocabulary must drop and re-add THIS
-- name, exactly as m131 did for capabilities and m136 provided for scopes.
--
-- ===========================================================================
-- DECISION 2: text[] WITH A CLOSED CHECK -- CONFIRMED AGAINST m136, NOT
--             ASSUMED FROM IT
-- ===========================================================================
--
-- The brief proposed mirroring mcp_grants.oauth_scopes. It is right, and each
-- leg of m136 DECISION 2 was re-checked against THIS column rather than
-- inherited:
--
--   a. AN ARRAY AND NOT A SCALAR. RFC 6749 section 3.3 makes `scope` a SET and
--      RFC 7591 section 2 carries the same production into registration
--      metadata. The value this column is compared against is
--      ParseRequestedScopes' []Scope. A delimited single text column would make
--      "mcp:read mcp:write" indistinguishable from one scope literally named
--      "mcp:read mcp:write" -- and on THIS column that ambiguity sits on the
--      right-hand side of an authorisation comparison, which is worse than
--      where m136 found it. Array.
--
--   b. text[] AND NOT AN ENUM, for m136 DECISION 2(b)'s reasons, which are
--      properties of PostgreSQL and transfer unchanged: an enum cannot be
--      narrowed, its members cannot be removed, and a CHECK over a literal
--      array is droppable and re-addable inside the single transaction this
--      runner wraps each migration in. m131 widened a vocabulary that way.
--
--   c. THE CONSTRAINT IS READ BACK AT RUNTIME, and this is the decisive leg.
--      apps/api/tests/mcp_m136_scope_vocabulary_parity_test.go and
--      mcp_m131_capability_vocabulary_parity_test.go both extract the literals
--      from pg_get_constraintdef:
--
--          SELECT m[1] FROM pg_constraint c
--            CROSS JOIN LATERAL regexp_matches(
--                     pg_get_constraintdef(c.oid), '''([^'']*)''::text', 'g') AS m
--           WHERE c.conrelid = 'public.mcp_oauth_clients'::regclass
--             AND c.conname  = 'mcp_oauth_clients_registered_scopes_vocabulary_check';
--
--      A `<@ ARRAY[...]::text[]` CHECK renders in exactly that shape, so the
--      SAME extraction works verbatim on this constraint with only the regclass
--      and conname changed. An enum would need a second catalogue query and a
--      second parity mechanism. One mechanism, now three constraints.
--
--      THAT PARITY TEST IS OWED FOR THIS CONSTRAINT AND IS NOT IN THIS FILE --
--      it is Go, and Go is not this migration's lane. See section (5).
--
--   d. SHAPE IS CHECKED AS m120, m127 AND m136 CHECK IT: one dimension (a
--      nested literal like '{{a},{b}}' would otherwise be flattened by unnest()
--      into scopes nobody registered), no NULL element, no empty string.
--      IMMUTABLE expressions only, so the constraint is valid in a CHECK.
--
--      NOT CHECKED: element uniqueness, for m127's and m136's reason --
--      deduplication needs a subquery (cardinality vs. a DISTINCT unnest) and
--      CHECK forbids one. A duplicate is harmless to the set-containment read
--      this column exists to serve, and merely consumes ceiling.
--
-- ===========================================================================
-- DECISION 3: NOT NULL, AND NO DATABASE DEFAULT
-- ===========================================================================
--
-- m127 DECISION 1(a) and m136 DECISION 3 are the reasoning and both transfer.
-- Restated only where this column differs.
--
--   NOT NULL. A nullable registration-scope column that Go reads as "no scope
--   restriction recorded, therefore the client may request anything" is the
--   exact defect this file exists to close, rebuilt one layer down. NULL could
--   only mean "unknown", and the answer to an unknown registration is refusal,
--   so the value is not worth being able to store. A caller that omits the
--   column gets 23502 -- a loud failure at INSERT -- and not a client
--   registered for the whole registry.
--
--   NO DEFAULT, AND THE TRAP IS SHARPER HERE THAN IT WAS ON mcp_grants. A
--   DEFAULT ARRAY['mcp:read'] would be indistinguishable from this migration's
--   backfill at the moment it applies and completely different forever after: a
--   backfill sets a value on rows that ALREADY EXIST, once; a DEFAULT silently
--   seats a scope on every FUTURE registration that forgets to say. policy.go
--   already states the rule in its own words on DefaultGrantCapabilities -- "It
--   is deliberately NOT a database DEFAULT -- see the column's NOT NULL and
--   m127's reasoning about a credential nobody chose the terms of".
--
--   WHY SHARPER. THE REGISTRATION ENDPOINT IS UNAUTHENTICATED. Service.Register
--   accepts redirect_uris, an auth method and two display strings; it reads no
--   `scope` metadata at all today. So the party whose omission a DEFAULT would
--   paper over is an anonymous caller, and the value papered in would be an
--   authorisation bound. The moment a second scope is recognised, a DEFAULT
--   means a registration that meant to name the write scope and failed to plumb
--   it through registers for the read scope instead, silently, with nothing
--   anywhere failing. With no DEFAULT it is 23502 and somebody reads this file.
--
--   WHAT THE DEFAULT WOULD HAVE BOUGHT -- not having to touch the Go INSERT
--   path -- IS NOT A SAVING. backend-architect must touch RegisterMCPOAuthClient
--   anyway to decide what an omitted `scope` means; see section (5)(a).
--
--   THE COST IS REAL AND IS ACCEPTED DELIBERATELY. Between this commit and that
--   one, RegisterMCPOAuthClient does not name this column, so every dynamic
--   client registration fails 23502. That is the intended, visible failure and
--   it is why this migration lands FIRST and separately -- exactly as m136
--   section (5)(a) chose for mcp_grants -- rather than silently registering
--   clients whose scope set the schema chose for them. The Go half follows in
--   its own reviewed change.
--
-- ===========================================================================
-- DECISION 4: A ZERO-LENGTH SCOPE SET IS UNREPRESENTABLE
-- ===========================================================================
--
-- m136 DECISION 4 reached the same answer for mcp_grants. THIS IS NOT A COPY OF
-- IT: m136's argument is about a derived capability ceiling, this column is
-- derived into nothing, so the argument has to be made again from this table.
-- It is, and it is made from the column sitting four lines above this one.
--
-- THE CASE FOR PERMITTING '{}' IS REAL AND IS STATED BEFORE IT IS REFUSED. A
-- client registered to request nothing is coherent in a way a grant holding no
-- scopes is not: the row is an IDENTITY record, not a credential. Such a client
-- could still authenticate at the token endpoint; it would simply be refused at
-- /authorize. That is the fully restrictive direction, and m127 DECISION 1(b)
-- permits capabilities = '{}' on exactly that reasoning.
--
-- IT LOSES TO THIS TABLE'S OWN PRECEDENT, ONE COLUMN AWAY. m124 put
-- mcp_oauth_clients_redirect_uris_present_check on redirect_uris and said why
-- in words that describe this column without alteration:
--
--     "At least one redirect URI, always. An empty array would leave the
--      redirect check with nothing to match against, and 'matches nothing' is
--      one careless Go comparison away from 'matches anything'."
--
-- registered_scopes has the SAME JOB as redirect_uris: it is a stored set that
-- an incoming request is matched against, and it exists for no other purpose.
-- An empty one leaves the scope check with nothing on its right-hand side, and
-- the careless Go comparison is not hypothetical -- it is the two-line shape
-- `if len(client.RegisteredScopes) == 0 { // nothing registered, skip }` that
-- any reviewer can picture and that reintroduces precisely the global-registry
-- behaviour this file removes. The strongest thing the schema can do about a
-- degenerate right-hand side is to make it not exist.
--
-- THE "COHERENT" READING ALSO BUYS NOTHING IN PRACTICE. A scopeless client row
-- is not a kill switch: registration is unauthenticated and free, so anyone who
-- could be disabled that way re-registers in one request. It is not a
-- revocation mechanism either -- this table has none, and m136 DECISION 4
-- records why inventing a second undiscoverable one is wrong. No code path
-- produces such a row; only a bug or a deliberate write does.
--
-- IT IS ITS OWN NAMED CONSTRAINT rather than a clause folded into the shape
-- check, so that the SQLSTATE 23514 an operator or a test sees NAMES WHICH RULE
-- WAS BROKEN. Three rules, three names, three distinguishable failures. It is
-- named `_present_check` and not `_not_empty_check` to match
-- mcp_oauth_clients_redirect_uris_present_check, the sibling it is reasoned
-- from, rather than m136's name on the other table.
--
-- ===========================================================================
-- DECISION 5: THE CARDINALITY CEILING IS 16, THE SAME BOUND m136 CHOSE
-- ===========================================================================
--
-- Asked rather than assumed, and the answer is the same number for the same
-- reason plus one reason of its own.
--
-- IT IS A PATHOLOGICAL-INPUT BOUND, NOT A POLICY. It is deliberately NOT m127's
-- 64: 64 is the wireframe's stated property of the credential for CAPABILITIES,
-- which an operator ticks individually. Scopes are not ticked; they are named
-- by a client against a registry that holds ONE member today and is expected to
-- hold a handful. With the vocabulary CHECK in force, duplication is the only
-- way to approach 16 at all.
--
-- THE REASON OF ITS OWN IS THE CONTAINMENT DIRECTION. Once section (5) lands, a
-- grant's oauth_scopes is a subset of the registering client's
-- registered_scopes. The client bound must therefore be at least the grant
-- bound, or the schema would permit a client whose registration cannot be fully
-- exercised by any grant. There is no reason for it to be MORE than the grant
-- bound either -- a client that may ask for more scopes than any single grant
-- can hold is a shape nothing wants. Equal, at 16, on both.
--
-- THE BOUND IS ALSO NOT LOAD-BEARING FOR SECURITY and is not presented as
-- though it were. The vocabulary CHECK is what confines the CONTENT; this bound
-- confines only the SIZE, against an array that arrives over an
-- unauthenticated endpoint.
--
-- ===========================================================================
-- DECISION 6: THE VOCABULARY IS SEATED AT `mcp:read` AND NOTHING ELSE
-- ===========================================================================
--
-- THIS MIGRATION SEATS NO SECOND SCOPE. It builds the place a per-client scope
-- set is stored, and it stops there.
--
-- recognisedScopes in internal/mcp/scope.go holds exactly one entry,
-- ScopeRead = 'mcp:read'. m131 DECISION 5's rule applies to this pair as it
-- applies to m136's: two closed sets with two answers is worse than one open
-- set. A scope the database accepts and Go does not is refused at a different
-- layer with a different error; a scope Go accepts and the database does not is
-- a 23514 at INSERT on the unauthenticated registration path. So the constraint
-- below holds exactly what recognisedScopes holds, today.
--
-- THERE ARE NOW TWO CONSTRAINTS THAT MUST TRACK recognisedScopes -- this one
-- and mcp_grants_oauth_scopes_vocabulary_check -- AND THE SECOND SCOPE'S
-- MIGRATION MUST WIDEN BOTH. Widening a containment CHECK is MONOTONE (m131
-- DECISION 4), so that migration is a drop-and-re-add of two constraints by
-- name and cannot invalidate a single existing row. Missing one of the two is
-- not a silent widening in either direction: miss this one and registration
-- refuses the new scope with 23514; miss m136's and the approval refuses it.
-- Both fail closed, loudly, which is why two constraints are acceptable here.
--
-- EVERY MEMBER OF THIS VOCABULARY IS `mcp:read`, WHICH IS A READ SCOPE, and
-- that is checkable by looking rather than by trust. The first migration to add
-- a member that is not 'mcp:read' is the one that owes the write review.
--
-- ===========================================================================
-- DECISION 7: THE BACKFILL PRESERVES TODAY'S REACH EXACTLY
-- ===========================================================================
--
-- THIS MIGRATION MOVES NO FLOOR AND NO CEILING. State that plainly, because it
-- is the property the whole change stands on: EVERY CLIENT REGISTERED BEFORE
-- THIS FILE CAN DO EXACTLY WHAT IT COULD DO BEFORE THIS FILE, AND NOTHING MORE.
--
--   registered_scopes -> ARRAY['mcp:read'].
--
--     THIS GRANTS NOTHING NEW AND IT IS NOT A WILDCARD. It is the exact,
--     complete set an existing client can already obtain, READ OFF THE CODE
--     rather than chosen. The derivation is closed: Authorize refuses any
--     request whose scope is not in recognisedScopes, recognisedScopes carries
--     exactly one entry, and absence of the parameter is a refusal rather than
--     a default -- so 'mcp:read' is not merely the only scope an existing
--     client is LIKELY to hold, it is the only scope any client has ever been
--     able to obtain through /authorize. The backfill writes down what the
--     running system already permits.
--
--     THE VALUE IS ALSO A CEILING AND NOT JUST A FLOOR. Because the vocabulary
--     holds one member, ARRAY['mcp:read'] is simultaneously "everything this
--     client can do today" and "everything the registry can express". There is
--     no wider value to write, so the backfill cannot widen anything even by
--     mistake.
--
--     '{}' WAS NOT AN OPTION and would not have been the safe choice even if
--     DECISION 4 permitted it. It would silently strip every existing client of
--     the ability to start an authorization the moment backend-architect wires
--     the containment check -- an outage caused by a migration rather than by
--     an operator decision, justified as safety while the safe-and-correct
--     value was known and derivable. Fail-closed is the right default when the
--     answer is unknown. Here the answer is known.
--
-- A DEFAULT CLAUSE AND A ONE-TIME BACKFILL ARE NOT THE SAME THING; see
-- DECISION 3. No column below carries a DEFAULT.
--
-- EVERY STATEMENT IS IDEMPOTENT AND THE FILE IS RE-RUNNABLE. The backfill is
-- guarded by IS NULL, so a second application updates zero rows and cannot
-- overwrite a scope set an operator or the API has since chosen.
--
-- ===========================================================================
-- DECISION 8: RLS IS INHERITED, NOT ADDED
-- ===========================================================================
--
-- THIS MIGRATION CREATES NO TABLE AND NO POLICY. It adds one column to
-- mcp_oauth_clients, which m124 already put under ENABLE plus FORCE ROW LEVEL
-- SECURITY. RLS filters ROWS, not columns, so a new column on a protected table
-- is protected by the existing policies with no further statement, and a
-- redundant policy here would be a second thing to keep in sync.
--
-- NO SITE-SCOPE POLICY IS OWED, AND THE REASON IS STRUCTURAL RATHER THAN AN
-- EXEMPTION. The site-scope policy set belongs to SITE-KEYED tables. This table
-- is keyed by nothing of the sort: m124's header section "IS mcp_oauth_clients
-- TENANT-SCOPED?" records that it carries NO tenant_id at all, because
-- registration happens before any user has consented and therefore before any
-- organisation is known. It has no tenant column to isolate on and no site
-- column to scope by, so there is no site_scope policy to add and no
-- tenant_isolation policy either.
--
-- WHAT PROTECTS IT INSTEAD IS STRICTER, NOT LOOSER: m124 DECISION 7 gives it
-- exactly two policies, SPLIT BY COMMAND, each gated on its own GUC --
--
--     mcp_oauth_clients_registration  FOR INSERT  app.mcp_client_register
--     mcp_oauth_clients_lookup        FOR SELECT  app.mcp_client_lookup
--
-- -- and under FORCE ROW LEVEL SECURITY a transaction that sets neither GUC
-- matches ZERO ROWS. The table is invisible to every ordinary tenant
-- transaction, every worker and every agent transaction. ABSENCE OF THE GUC IS
-- REFUSAL, which is the same fail-closed direction a site_scope policy buys,
-- reached by a different mechanism. There is also no UPDATE policy and no
-- DELETE policy, so no transaction of any kind can modify this column after the
-- INSERT that wrote it; the backfill below runs at migration time as the
-- bootstrap superuser, before any policy applies, which is the only window in
-- which this column is writable after the fact.
--
-- NO ROW IS OWED IN db/rls-cross-tenant-policies.txt AND THE FILE IS NOT
-- TOUCHED. That ledger records POLICIES granting access across tenants. Both of
-- this table's policies are cross-tenant and both already carry a row, entered
-- by m124; this migration adds, removes and renames no policy, so the ledger is
-- already correct. check-rls-cross-tenant.sh must stay green without an edit.
--
-- WHICH DISPATCH HELPER A WRITER MUST USE: db.Pool.InMCPClientRegisterTx to
-- write and db.Pool.InMCPClientLookupTx to read, and that is a correctness
-- requirement and not a convention. Those two helpers are the only things that
-- set the two GUCs above; a call site that opens an ordinary tenant transaction
-- instead sees zero rows and writes nothing, which fails closed but looks like
-- a missing row rather than a missing GUC.
--
-- NO NEW INDEX. Every read of this table is by client_id through
-- mcp_oauth_clients_client_id_key, so this column is read off an
-- already-fetched row and indexes nothing.
--
-- NO GRANT STATEMENT. m1's ALTER DEFAULT PRIVILEGES and m124's explicit
-- GRANT SELECT, INSERT, UPDATE, DELETE ON mcp_oauth_clients TO wpmgr_app are
-- table-level and cover columns added later; there is no per-column grant in
-- force on this table that a new column could fall outside of. Stated rather
-- than left silent, because "the new column is invisible to wpmgr_app" is the
-- kind of failure that looks like an RLS bug for a day.

-- ---------------------------------------------------------------------------
-- (0) The column, added NULLABLE so existing rows survive the ALTER.
-- ---------------------------------------------------------------------------

ALTER TABLE "public"."mcp_oauth_clients"
    ADD COLUMN IF NOT EXISTS "registered_scopes" text[];

-- ---------------------------------------------------------------------------
-- (1) Backfill. WHERE ... IS NULL makes this a no-op on a second run and
--     protects a value the API has since chosen. See DECISION 7.
-- ---------------------------------------------------------------------------

UPDATE "public"."mcp_oauth_clients"
   SET "registered_scopes" = ARRAY['mcp:read']::text[]
 WHERE "registered_scopes" IS NULL;

-- ---------------------------------------------------------------------------
-- (2) Arm NOT NULL, now that no row violates it. SET NOT NULL is a no-op when
--     the column is already NOT NULL, so this is safe to re-run.
-- ---------------------------------------------------------------------------

ALTER TABLE "public"."mcp_oauth_clients"
    ALTER COLUMN "registered_scopes" SET NOT NULL;

-- ---------------------------------------------------------------------------
-- (3) Constraints. Each guarded by a pg_constraint probe so the file re-runs.
--     Each ADDED VALIDATED, never NOT VALID: PostgreSQL scans the table and
--     would refuse this migration outright rather than leave an unchecked row.
-- ---------------------------------------------------------------------------

-- Shape. See DECISION 2(d) and DECISION 5.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.mcp_oauth_clients'::regclass
          AND conname  = 'mcp_oauth_clients_registered_scopes_shape_check'
    ) THEN
        ALTER TABLE "public"."mcp_oauth_clients"
            ADD CONSTRAINT "mcp_oauth_clients_registered_scopes_shape_check"
            CHECK (
                coalesce(array_ndims("registered_scopes"), 1) = 1
                AND cardinality("registered_scopes") <= 16
                AND array_position("registered_scopes", NULL) IS NULL
                AND NOT ('' = ANY ("registered_scopes"))
            );
    END IF;
END;
$$;

-- A registered client names at least one scope. See DECISION 4. Its own name so
-- the 23514 says which rule was broken, and named to match
-- mcp_oauth_clients_redirect_uris_present_check, the sibling it reasons from.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.mcp_oauth_clients'::regclass
          AND conname  = 'mcp_oauth_clients_registered_scopes_present_check'
    ) THEN
        ALTER TABLE "public"."mcp_oauth_clients"
            ADD CONSTRAINT "mcp_oauth_clients_registered_scopes_present_check"
            CHECK (cardinality("registered_scopes") >= 1);
    END IF;
END;
$$;

-- Closed vocabulary. See DECISION 6. `<@` is array containment and is
-- IMMUTABLE. Containment alone would admit '{}' -- the empty array is contained
-- by every array -- which is why emptiness is refused by its own constraint
-- above rather than by this one.
--
-- KEEP IN LOCKSTEP with recognisedScopes in apps/api/internal/mcp/scope.go, AND
-- WITH mcp_grants_oauth_scopes_vocabulary_check, which holds the same set on
-- the other table. EXTENDING EITHER LIST IS A MIGRATION, and the migration that
-- extends one must extend the other. The name is chosen to be stable: the
-- parity test owed in section (5) looks the constraint up BY NAME.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.mcp_oauth_clients'::regclass
          AND conname  = 'mcp_oauth_clients_registered_scopes_vocabulary_check'
    ) THEN
        ALTER TABLE "public"."mcp_oauth_clients"
            ADD CONSTRAINT "mcp_oauth_clients_registered_scopes_vocabulary_check"
            CHECK ("registered_scopes" <@ ARRAY['mcp:read']::text[]);
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- (4) WHAT THIS MIGRATION DOES NOT DO
-- ---------------------------------------------------------------------------
--
-- It does not narrow a single existing client. Every client that could start an
-- authorization for 'mcp:read' before this file can start exactly that after
-- it, and no client could ever start one for anything else.
--
-- It does not constrain /authorize. Authorize still validates against
-- recognisedScopes alone and still consults the client row for redirect URIs
-- only; this column is WRITTEN by the backfill and READ BY NOTHING until
-- section (5) lands. The escalation path is closed by that change, not by this
-- one -- this file makes the change possible and makes every storable value
-- safe.
--
-- It does not add a second scope to the vocabulary, and it does not confer a
-- capability. Both are further migrations under their own review.
--
-- It does not touch mcp_grants, m136's column or any policy.
--
-- ---------------------------------------------------------------------------
-- (5) WHAT THE GO LAYER MUST NOW DO -- HANDED TO backend-architect
-- ---------------------------------------------------------------------------
--
-- A note to the next engineer, not work this file performs. It is here because
-- whoever does that work will be reading this file.
--
--   a. THE REGISTRATION INSERT MUST NAME THE COLUMN, AND IT MUST DO SO IN THE
--      SAME CHANGE THAT REGENERATES sqlc. The column is NOT NULL WITH NO
--      DEFAULT (DECISION 3), so RegisterMCPOAuthClient in
--      db/query/mcp_connections.sql fails 23502 until it does. That is the
--      intended, loud failure, and it is why this migration lands FIRST and
--      separately: between this commit and that one, dynamic client
--      registration is red, on purpose and visibly, rather than silently
--      registering clients whose scope set the schema chose.
--
--      WHAT AN OMITTED `scope` MEANS AT REGISTRATION IS A DECISION THE SCHEMA
--      DELIBERATELY DOES NOT MAKE. RFC 7591 section 2 says an omitted `scope`
--      leaves the server to decide; Service.Register reads no `scope` today, so
--      SOMETHING must be chosen. Both defensible answers -- refuse the
--      registration, or register for the full recognised registry -- are Go
--      policy and both are safe under this schema. The one answer the schema
--      forecloses is "store nothing and decide later", which is what NOT NULL
--      is for. Note that while recognisedScopes holds one member the two
--      answers differ only in whether a client that says nothing gets a row at
--      all; they diverge the day a second scope lands, so choose deliberately
--      now.
--
--   b. Authorize MUST CHECK CONTAINMENT AGAINST THE CLIENT ROW, AFTER
--      ParseRequestedScopes AND AFTER LookupClient. The registry check stays --
--      it answers "is this a scope this server knows" -- and the new check
--      answers "is this a scope THIS CLIENT registered for". Both, in that
--      order, and the existing exact-match redirect check is the model: it
--      compares in Go against the stored array and takes no parameter into SQL.
--
--   c. THE REFUSAL IS RFC 6749 section 5.2 `invalid_scope`, which is
--      ErrCodeInvalidScope, which the OAuth handlers already map explicitly to
--      400 rather than letting KindValidation render 422. Reuse it; do not mint
--      a new code that falls through to the generic renderer.
--
--   d. THE CHECK MUST NOT SPECIAL-CASE AN EMPTY STORED SET. DECISION 4 makes
--      '{}' unstorable, but the Go read must not depend on the database being
--      the only writer: an empty or nil column value means NO AUTHORITY TO
--      REQUEST ANYTHING and must never mean "unrestricted". Containment against
--      an empty set already yields that answer, so the correct implementation
--      is to write no special case at all -- and the wrong one is the
--      `if len(...) == 0 { skip }` shape DECISION 4 names.
--
--   e. Approve MUST APPLY THE SAME CHECK, AND THIS IS THE HALF THAT IS EASY TO
--      MISS. The long comment above ParseRequestedScopes in Approve already
--      records that the approval BODY round-trips through the browser and is
--      re-parsed against registry membership alone, so a tampered body naming a
--      scope the authorize call never carried is accepted on registry
--      membership. Constraining Authorize alone leaves that door; the stored
--      client row is available at Approve too -- it already calls LookupClient
--      and re-matches the redirect there for exactly this reason -- so the
--      containment check belongs in both places. This does not by itself
--      complete the authorize-to-approve binding that comment asks for; it
--      narrows what a tampered body can name to the client's own registration.
--
--   f. THE PARITY TEST IS OWED (DECISION 2(c) and DECISION 6). It must read
--      mcp_oauth_clients_registered_scopes_vocabulary_check back through
--      pg_get_constraintdef and assert BOTH directions against recognisedScopes
--      -- no member Go lacks, no member the database lacks -- and it must FAIL
--      rather than pass when the extraction returns no rows, because an empty
--      extraction reads as "the sets agree" and is the exact shape of a check
--      that guards nothing. mcp_m136_scope_vocabulary_parity_test.go is the
--      template and changes only regclass and conname.
--
--   g. A CONSTRAINT TEST IS OWED TOO, on the model of
--      mcp_m136_scope_column_constraints_test.go: the three refusals here are
--      executed by nothing in this repository until one exists, and two of them
--      (a 17-member array, a NULL element) are unreachable through the Go
--      layer, so it must insert directly -- through InMCPClientRegisterTx, as
--      wpmgr_app, asserting the role inside the transaction. It must carry the
--      accept arm as well: a constraint set that refuses everything guards
--      nothing.
--
--   h. NONE OF THAT ADDS A SCOPE OR A CAPABILITY. It makes the requestable
--      scope set per-client and honest.
