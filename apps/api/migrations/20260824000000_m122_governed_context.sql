-- m122 - ADR-064 slice S3: storage for governed per-organisation and per-site
-- context. SCHEMA ONLY.
--
-- THIS MIGRATION DELIBERATELY CHANGES NO BEHAVIOUR, in the m117 sense: it adds
-- two tables, their indexes, their constraints, their RLS and their grants. No
-- route reads them, no service writes them, no worker touches them. Every
-- existing database lands with both tables empty, and empty is the correct and
-- complete state for every organisation that has never authored context.
--
-- The Go half - the seven-layer resolution function, the write-time widen
-- check, the secret scan, the fail-closed audit append, the effective-context
-- preview and the `context.*` capabilities - is ADR-064's S4 and belongs to
-- backend-architect. It lands on top of this. See "WHAT S4 MUST DO" at the end
-- of this header.
--
-- Specification: docs/adr/ADR-064-governed-site-org-context.md (Status:
-- Proposed). Every decision below cites the ADR decision it implements. Where
-- the ADR is ambiguous this header says so in the AMBIGUITIES section rather
-- than resolving it silently.
--
-- ===========================================================================
-- WHAT THIS STORES, IN ONE PARAGRAPH
-- ===========================================================================
--
-- Human-authored information about an organisation (ADR-064 layer 2) or a site
-- (layer 3) that a future model-facing surface is handed alongside what it
-- observes for itself. Each table is an APPEND-ONLY VERSION LOG, not a mutable
-- settings row: "the current context" is defined by ADR-064 Decision 3 as "the
-- latest version row", a read rather than a second representation to keep in
-- sync. Every row carries the full resulting snapshot, its version number, its
-- author, its provenance and its timestamp.
--
-- ===========================================================================
-- DECISION 1: two columns, restrictions and guidance, never one
-- ===========================================================================
--
-- ADR-064 Decision 3 splits every field into two kinds, and Decision 1 says
-- the split is "load-bearing", not descriptive:
--
--   RESTRICTIONS  a closed structured set where "does this edit widen what a
--                 higher layer set" is a well-defined comparison a machine can
--                 make. Decision 4's write-time rejection runs over these.
--   GUIDANCE      free text - brand voice, audience, terminology. "Wider" and
--                 "narrower" are NOT defined relations over arbitrary prose,
--                 and ADR-064 is explicit that it does not pretend otherwise.
--
-- They are two separate columns because that is what makes the split
-- mechanical instead of a naming convention. If both lived in one document,
-- the widen-check would have to decide which keys inside it are restrictions,
-- and a write path that got that classification wrong would let a guidance key
-- masquerade as a restriction - or, far worse in the direction that costs a
-- boundary, let a restriction be edited through the code path that does not
-- check. Two columns make that class unrepresentable: the widen-check reads
-- `restrictions` and nothing else, and it cannot be handed guidance by
-- accident.
--
-- The secret scan (ADR-064 Decision 10) reads BOTH. That asymmetry is the
-- point of the split and is stated here so a later reader does not "simplify"
-- the two columns back into one.
--
-- WHY jsonb AND NOT NAMED COLUMNS. ADR-064 fixes neither vocabulary. It gives
-- an illustrative guidance list ("brand voice, audience, terminology notes,
-- style preferences") and names no restriction key at all - Decision 4 puts
-- the authoritative layer-1 restriction set in code, explicitly NOT in a table
-- row ("WPMgr's layer-1 policy is not a row in either context table"). Minting
-- named columns here would invent a closed vocabulary the ADR declined to fix,
-- and would put every new guidance kind behind a migration, which is the wrong
-- shape for a surface whose whole character is free text. The inner shape is
-- fixed in Go, next to the layer-1 set it has to be compared against.
--
-- The jsonb_typeof CHECK below is the part that must not be dropped. Without
-- it a bare string or an array lands in `restrictions`, and the widen-check
-- Decision 4 builds on top has no object to compare. That is the m115 lesson
-- applied on the way in rather than converged afterwards: m113 left a check
-- constraint open, the value it should have refused got written, and m115 had
-- to go round every database that had already applied m113.
--
-- ===========================================================================
-- DECISION 2: append-only is enforced by PRIVILEGE, not by convention
-- ===========================================================================
--
-- ADR-064's "What has to exist before this ships" requires "Version, author,
-- and provenance columns on both context tables, with UPDATE/DELETE revoked
-- from the application role at the privilege level, the same way the audit log
-- already enforces append-only."
--
-- So the grants at the bottom of this file are SELECT + INSERT, followed by an
-- explicit REVOKE of UPDATE, DELETE and TRUNCATE from wpmgr_app.
--
-- THE REVOKE IS NOT REDUNDANT AND MUST NOT BE DROPPED. The m1 auth migration
-- (20260527130000_auth_multitenancy.sql:123-126) sets
--
--   ALTER DEFAULT PRIVILEGES IN SCHEMA public
--     GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO wpmgr_app;
--
-- so every table created by the migration owner - including these two -
-- receives UPDATE and DELETE automatically, without anyone typing them. A
-- table that merely omits an UPDATE grant is still updatable. audit_log is
-- revoked for exactly this reason at m1:130 and these two tables follow it.
--
-- Append-only is why history is exact by construction (ADR-064 Decision 5)
-- rather than by a second audit trail bolted alongside, and it is what makes
-- Decision 7's claim - that an auditor can later prove what instruction set a
-- model was given at a point in time - a property of the storage rather than a
-- promise about the callers.
--
-- ===========================================================================
-- DECISION 3: the organisation key IS tenant_id, and on the site table it is
--             STAMPED rather than derived
-- ===========================================================================
--
-- In this schema an organisation is a tenant: `/api/v1/orgs/{orgId}` addresses
-- a `tenants` row, and `sites.tenant_id REFERENCES tenants (id)`. ADR-064
-- says "organisation" throughout; this file says `tenant_id`, and they are the
-- same key. The tables are named for the ADR's vocabulary (and for the route
-- and capability names it fixes - `context.org.read`, `context.org.write`) and
-- the column is named for the schema's.
--
-- On site_context_versions, ADR-064 Decision 3 is emphatic that this column is
-- NOT the site's current owner:
--
--   "A context version row means 'as of when this was written,' so it stores
--    the organisation id that owned the site AT THE TIME OF THAT WRITE, set
--    once and never rewritten by a later transfer."
--
-- Decision 12 then depends on that stamp: a transfer resets the site's active
-- context without retroactively reassigning authorship of everything written
-- before it, and Decision 13's history routes authorize against the STAMPED
-- organisation rather than the site's current one.
--
-- DO NOT ADD THE COMPOSITE FOREIGN KEY. `sites_id_tenant_key UNIQUE
-- (id, tenant_id)` exists (schema.sql:444) and one table in this schema uses
-- it as `FOREIGN KEY (site_id, tenant_id) REFERENCES sites (id, tenant_id)`.
-- Adding that here would look like tidiness and would be a defect: it forces
-- every row's stamp to equal the site's CURRENT owner, which is precisely the
-- property Decision 3 forbids. It would work perfectly until the day site
-- transfer ships and then refuse every pre-transfer row at once. The two
-- single-column foreign keys below are deliberate.
--
-- ===========================================================================
-- DECISION 4: both foreign keys are ON DELETE CASCADE - the opposite call to
--             m113/m116, for a stated reason
-- ===========================================================================
--
-- site_object_reclaim and tenant_object_reclaim carry NO foreign key at all,
-- because a cascade onto a record of cleanup work destroys it in the very
-- operation it exists to survive. The house rule that came out of GH #402 and
-- GH #408 is: when you add a cascade, ask what audit or reclaim record dies
-- with it.
--
-- Asked, and answered. What dies with this cascade is the context snapshots.
-- What does NOT die with it is the accountability record, because ADR-064
-- Decision 7 puts every context write in the hash-chained audit_log in the
-- same transaction, and audit_log is a separate table with its own retention
-- and its own append-only privilege revoke. The claim ADR-064 exists to
-- support - that an organisation's auditor can prove what a model was given -
-- rides on the ledger entry, not on the snapshot.
--
-- And these tables have no object-storage component at all. ADR-064 Decision
-- 12 states that explicitly, so that the next person reading purge_worker.go's
-- roster does not have to wonder whether context belongs on it:
--
--   "Context storage does not add an eighth root - it has no object-storage
--    component at all."
--
-- Verified against the code rather than taken from the ADR: the seven roots
-- are enumerated at internal/org/purge_worker.go:161-173 and asserted
-- identical to the Lane-A drain set by TestGH408_TenantDrainRootsAreLaneBsRoots
-- (internal/backup/gh408_tenant_drain_roots_test.go:28). Nothing here adds to
-- that list, so nothing there changes.
--
-- So the cascade is correct, and it is what makes ADR-064 Decision 12's
-- retention rule true: "retained for the life of the organisation or site ...
-- and is removed only when the organisation or site is removed". Both purge
-- paths are cascade-driven (admin_purge_tenant, m93:114-145;
-- admin_delete_empty_tenant, m116:252-299) and neither enumerates tables, so
-- these two tables are freed with no change to either function.
--
-- WHY THE CASCADE IS NOT BLOCKED BY THIS TABLE'S OWN RLS. Worth recording,
-- because it is the obvious worry and the answer is not obvious.
-- admin_delete_empty_tenant BLANKS app.tenant_id to '' before it deletes the
-- tenants row (m116:270-271), and under FORCE ROW LEVEL SECURITY a
-- tenant_isolation policy matches zero rows when that GUC is blank. The
-- question is whether the cascaded child DELETE is therefore refused, leaving
-- a foreign-key violation and an org nobody can delete.
--
-- It is not. PostgreSQL's referential actions bypass row security. Measured on
-- postgres:16.4-alpine, as a NOSUPERUSER NOBYPASSRLS role owning the tables,
-- with a child table carrying ENABLE + FORCE RLS and exactly the
-- tenant_isolation policy below: with app.tenant_id set to '', the child row
-- was invisible to a direct SELECT (0 rows), and DELETE on the parent still
-- succeeded and still removed the child. Neither the delete nor the cascade
-- errored. That is why these tables need no app.agent escape hatch for the
-- purge path - see Decision 6.
--
-- ===========================================================================
-- DECISION 5: version is monotonic per SUBJECT, and the unique index is what
--             turns ADR-064's open question 2 from silence into an error
-- ===========================================================================
--
-- ADR-064 Decision 2 gives every context row "a version number that increments
-- on every accepted write". A writer computes max(version) + 1 for its subject
-- and inserts.
--
-- UNIQUE (tenant_id, version) on the org table and UNIQUE (site_id, version)
-- on the site table are therefore not decoration. ADR-064's open question 2
-- records that PATCH concurrency is UNDECIDED and assigns it to
-- backend-architect:
--
--   "Two concurrent PATCH calls touching disjoint fields can both read version
--    N and both succeed, and the later write's snapshot will not carry
--    whichever fields the earlier write changed."
--
-- Without the unique index that is a lost update: two rows both claiming to be
-- version N+1, and "the latest version row" - which is ADR-064's definition of
-- the current context - becomes ambiguous, resolved by whichever the planner
-- returned. With it, one writer commits and the other gets SQLSTATE 23505 and
-- can be told to retry. The database cannot decide the HTTP contract, but it
-- can refuse to let the undecided case corrupt history quietly, which is the
-- half that must not wait for the decision.
--
-- WHY THE SITE TABLE'S KEY IS (site_id, version) AND NOT
-- (tenant_id, site_id, version). Version numbers must stay unique and
-- monotonic across a transfer. Decision 12's cleared version is "itself a
-- version" and continues the sequence; keying on the stamp as well would let
-- version 4 exist twice for one site under two different organisations, and
-- "restore version 4" would then name two different snapshots. Site history is
-- one sequence with a stamp that changes partway down it, not two sequences.
--
-- These unique indexes are also the read path. "The latest version row for
-- that organisation or site" is a backwards scan of the same btree, so no
-- separate ordering index is needed and none is created.
--
-- WHY THE ORG TABLE GETS NO SEPARATE tenant_id INDEX. The house convention is
-- an index on tenant_id, and it is satisfied here rather than skipped:
-- tenant_id is the LEADING column of org_context_versions_version_key, so
-- lookups and the purge cascade use that index already. A second index on the
-- same leading column would be paid for on every insert and read by nothing.
-- The site table DOES get one, because there tenant_id leads no index and the
-- tenant cascade would otherwise sequentially scan the whole table.
--
-- ===========================================================================
-- DECISION 6: NO app.agent POLICY ON EITHER TABLE
-- ===========================================================================
--
-- This is a deliberate departure from the generic house pattern
-- (.claude/rules/db-migrations.md lists a `<table>_agent` policy among what a
-- new site-keyed table needs), argued in place, the way m116 argued its way
-- out of the site-scope policy. Four reasons, in order of weight.
--
--   1. ADR-064 Decision 2 forbids the thing such a policy would enable. "The
--      agent never holds an opinion about layers 1 through 3, never merges
--      them locally, and is never asked to resolve a conflict between them."
--      The agent's role is layer 4 - reporting what it observes - and layer 4
--      is not stored here.
--
--   2. No cross-tenant control-plane worker reads context either. Resolution
--      (Decision 8) runs on a request, under tenant scope. The materialised
--      effective-context cache of Decision 2 is keyed per site and invalidated
--      on write; it is not a fleet sweep.
--
--   3. The purge path does not need it. That was the one real candidate, and
--      it is settled by measurement rather than by reading: referential
--      actions bypass row security, so the cascade fires whatever GUC the
--      purge function has set. See Decision 4 above for the probe and its
--      result.
--
--   4. It would be a cross-tenant grant with no caller. app.agent policies are
--      PERMISSIVE and do not bind tenant_id, so each one is a cross-tenant
--      grant that scripts/check-rls-cross-tenant.sh requires a ledger row for.
--      The only honest rationale available today would be "no code path runs
--      under this policy", and db/rls-cross-tenant-policies.txt is explicit
--      that a ledger row exists because "somebody had to look", not to
--      pre-authorise a hole for a caller that may never arrive. Granting
--      cross-tenant read of an organisation's governing instructions on the
--      chance a worker might one day want it is exactly backwards for a
--      boundary.
--
-- If a future cross-tenant worker genuinely needs to read context, the correct
-- order is: a new migration adding the policy, WITH a ledger row naming the
-- caller and stating what it does. Not this one, speculatively.
--
-- CONSEQUENCE, STATED SO NOBODY DISCOVERS IT AT RUNTIME: neither table is
-- reachable from InAgentTx. A background job that needs context must run under
-- a tenant transaction. That is a constraint on S4, not an accident.
--
-- ===========================================================================
-- DECISION 7: the site-scope policies, and what each one actually refuses
-- ===========================================================================
--
-- ADR-064's "What has to exist before this ships" requires:
--
--   "A restrictive site-scope policy on both context tables, not only tenant
--    isolation ... Tenant isolation alone would let any principal scoped to
--    the tenant read or resolve another site's context; the restrictive policy
--    is what confines a read to the sites a principal can actually see."
--
-- and ADR-061 Decision 4 (ADR-061:370-371) pre-commits the deferred site-scope
-- migration's scope, which ADR-064's Relationship section extends to name
-- these two tables. This migration is where that lands for them.
--
-- site_context_versions - one RESTRICTIVE FOR ALL policy on site_id, the plain
-- m19/m113 exemplar shape. site_id is NOT NULL, so unlike the m112 email
-- tables there is no inheriting `site_id IS NULL` organisation row to keep
-- readable and no reason to split read from write. ADR-064 Decision 3 puts the
-- organisation layer in its OWN TABLE rather than as a null-keyed row in this
-- one, which is what makes the simple shape correct here.
--
--   SELECT  a collaborator invited to site A cannot read site B's context.
--   INSERT  a collaborator invited to site A cannot author a context version
--           naming site B. That is the WITH CHECK half, and it is the half
--           that stops the write.
--
-- org_context_versions - this one needs care, and the obvious reading of the
-- ADR sentence above is wrong.
--
-- The org table has no site_id, so "confine a read to the sites a principal
-- can see" cannot mean filtering its rows by site. Worse, a restrictive SELECT
-- gate here would BREAK a read ADR-064 explicitly requires: Decision 6 says
-- read access follows fleet-read access, "at the organisation AND the site
-- scope that cover that site", and Decision 8's effective-context preview for
-- a site renders layer 2's surviving contribution. A site-scoped collaborator
-- is entitled to read the organisation context that governs their own site;
-- they cannot understand the rules they are working under otherwise.
--
-- What they are NOT entitled to do is WRITE it. ADR-064 Decision 6:
-- "organisation-scope write is held by organisation administrators; site-scope
-- write additionally requires access to that specific site." A site-scoped
-- collaborator authoring an organisation-wide context version is precisely the
-- m112 defect class - a per-site principal reaching the organisation row - and
-- m112 exists because three review rounds closed seven separate instances of
-- it in handlers before anyone asked why they kept appearing.
--
-- So org_context_versions_site_scope_insert is RESTRICTIVE FOR INSERT with a
-- WITH CHECK that is simply false whenever app.site_scope is 'on'. The table
-- is append-only (Decision 2 above), so INSERT is the entire write surface and
-- one policy closes all of it.
--
--   SELECT  unaffected. No restrictive SELECT policy exists, so tenant
--           isolation alone governs the read, and the collaborator sees their
--           organisation's context. This is deliberate, per Decision 6/8.
--   INSERT  refused outright for any site-scoped principal, in the database,
--           whatever a handler forgets.
--
-- For an ordinary organisation member, a service path or a worker,
-- app.site_scope is unset and both policies' first branch is a tautology, so
-- behaviour is unchanged. RESTRICTIVE policies are AND-combined with the
-- permissive ones and can only ever subtract.
--
-- ===========================================================================
-- DECISION 8: NULL means never set (GH #509)
-- ===========================================================================
--
-- The columns that record a FACT get no manufactured default:
--
--   author_id                 NULL when the author is not a principal with an
--                             id - ADR-064 Decision 12's transfer version,
--                             whose author is "the transfer operation".
--   restored_from_version_id  NULL means "this version is not a restore". Not
--                             a sentinel, not the zero uuid.
--
-- The columns that record a SETTING do get one, and GH #509's rule is exactly
-- that distinction: a default is right for a setting and wrong for an
-- observation. `restrictions` and `guidance` default to '{}' because ADR-064
-- Decision 3 requires every version row to carry "the full resulting
-- snapshot"; a row exists only because somebody wrote it, so an empty document
-- is a true statement that this version sets nothing of that kind, not an
-- absence pretending to be a value.
--
-- created_at gets DEFAULT now() because now() at insert IS the measurement,
-- not a stand-in for one.
--
-- author_type is NOT NULL with no default: every write has an author kind, and
-- a default would let a caller that forgot to say who wrote something produce
-- a row that claims a kind it never asserted.
--
-- ===========================================================================
-- DECISION 9: the provenance set is CLOSED, and holds exactly what the ADR
--             DECIDES - not what it speculates about
-- ===========================================================================
--
--   'manual'    an operator edit (ADR-064 Decision 3).
--   'restore'   ADR-064 Decision 5: restoring version N creates a new version
--               attributed to whoever asked, "with provenance recorded as
--               `restore` and a pointer to the version restored".
--   'transfer'  ADR-064 Decision 12: the cleared version written when a site
--               moves organisation, "author: the transfer operation,
--               provenance: `transfer`".
--
-- 'import' is DELIBERATELY ABSENT. Decision 3 mentions "a later machine-
-- assisted import IF ONE IS EVER BUILT" - a hypothetical, not a decision.
-- Minting a value nothing can write would be manufacturing vocabulary; when an
-- import path is actually designed it arrives with its own migration, the way
-- site_object_reclaim.kind is a closed set extended by a later ordinal.
--
-- 'transfer' is in the set even though no site-to-organisation transfer
-- mechanism exists anywhere in this codebase - ADR-064 verified that directly
-- and records it as a prerequisite it depends on and does not build. The
-- difference from 'import' is that ADR-064 DECIDES this value; the contract is
-- written and only its caller is missing. The closed CHECK is the m115 lesson
-- applied on the way in.
--
-- ===========================================================================
-- AMBIGUITIES IN ADR-064, AND WHAT THIS MIGRATION CHOSE
-- ===========================================================================
--
-- Recorded rather than resolved silently. Each is a place a later reader could
-- reasonably have read the ADR differently.
--
-- A. "A restrictive site-scope policy on BOTH context tables." Taken literally
--    against a table with no site_id, this is unimplementable as a row filter,
--    and implemented as a SELECT filter it would break the layer-2 read
--    Decision 6 and Decision 8 both require. Chosen reading: the org table's
--    restrictive gate is on WRITE, not read - see Decision 7 above. This is
--    the reading that satisfies both passages at once; the literal one
--    satisfies neither.
--
-- B. Byte budgets (ADR-064 Decision 9). NO write-time size CHECK is added.
--    Decision 9 specifies TRUNCATION at resolution ("truncation happens at a
--    field or record boundary ... marked explicitly rather than silently
--    dropped", and starting "at the lowest surviving layer"), which is a
--    property of the resolution function, not of storage. A write-time
--    refusal would be different behaviour with a different error contract, and
--    Decision 13 enumerates exactly two write refusals - 409 for a widening
--    restriction and 422 for a credential-shaped value. Inventing a third, and
--    a byte threshold the ADR never states, would be designing past the
--    specification. Consequence S4 must own: nothing in the database bounds
--    the size of a context row today.
--
-- C. The restriction and guidance vocabularies are not fixed by the ADR. See
--    Decision 1: they are jsonb here and their shape belongs in Go, beside the
--    layer-1 restriction set Decision 4 already puts there.
--
-- D. "The author's principal id" (Decision 3) does not say which principal
--    kinds can author. `author_type` is a closed discriminator over 'user',
--    'api_key' and 'system': 'api_key' because ADR-064 Decision 6 gates these
--    routes on the ADR-061 Decision 5 capability registry, which lives on API
--    keys, and 'system' because Decision 12's transfer version has no human
--    author. If a fourth kind is ever needed it arrives with a migration.
--
-- ===========================================================================
-- WHY THERE IS NO FOREIGN KEY ON author_id
-- ===========================================================================
--
-- Neither ON DELETE action is correct, which is the tell that the column is
-- not a reference:
--
--   SET NULL  would MUTATE AN APPEND-ONLY ROW, erasing the authorship of a
--             historical version when the author's user account is deleted.
--             ADR-064 Decision 3 says author and provenance travel with each
--             row; a deletion elsewhere in the schema must not quietly rewrite
--             a governance record.
--   CASCADE   would delete an organisation's context history because a member
--             left. Absurd, and stated because it is the schema default people
--             reach for.
--
-- audit_log makes the identical call for the identical reason - actor_type
-- plus a bare actor_id, no foreign key (schema.sql:769-770) - and audit_log is
-- the table ADR-064 Decision 7 explicitly models this one's accountability
-- posture on. A dangling author id is the correct outcome here: the historical
-- fact is that that principal wrote it.
--
-- restored_from_version_id has no foreign key either, and for a sharper
-- reason. The constraint that actually matters on a restore is ADR-064
-- Decision 12's - a restore may not cross an organisation stamp, and a
-- pre-transfer version id is refused "outright and unconditionally, for every
-- caller". That is a comparison of two rows' tenant_id values, which no
-- single-column foreign key expresses. Adding one would imply a guarantee it
-- does not provide, and this schema has already paid for that shape once.
--
-- ===========================================================================
-- WHAT S4 (backend-architect) MUST DO, AND WHAT THIS FILE CANNOT
-- ===========================================================================
--
-- Nothing below enforces any of the following. They are ADR-064 requirements
-- that live in Go by the ADR's own design, listed here so the boundary between
-- the two slices is explicit rather than assumed:
--
--   * The never-widen check on RESTRICTIONS ONLY, at the write chokepoint,
--     against every layer above - not only the nearest (Decision 4). ADR-064
--     Decision 1 and the Consequences are both emphatic that GUIDANCE gets no
--     mechanical check and that claiming one "would be a check that always
--     passes without ever having tested the thing it claims to test". Do not
--     build it.
--   * Restrictions must also reach the TOOL-DISPATCH chokepoint as a deny
--     input (Decision 4), not merely be shown to the model as fenced prose.
--   * The secret scan over BOTH columns, refusing with the category and never
--     echoing the match (Decision 10).
--   * The fail-closed audit append in the same transaction as the INSERT
--     (Decision 7). This does not exist yet: audit.Record is best-effort today
--     (internal/audit/audit.go:498-500) and ADR-064 names the fail-closed
--     variant as required new work shared with ADR-061.
--   * One resolution function shared by the model-facing path and the preview
--     (Decision 8), refusing rather than returning an empty result when it
--     cannot complete (Decision 14).
--   * PATCH concurrency (open question 2). The unique indexes here turn the
--     undecided case into SQLSTATE 23505 rather than a lost update; deciding
--     the wire contract is still open and is assigned to backend-architect.
--   * History routes authorized against the STAMPED tenant_id, not the site's
--     current owner, and restore refused across a stamp boundary
--     (Decisions 12 and 13).
--
-- ===========================================================================
-- IDEMPOTENCE AND BOOT SAFETY
-- ===========================================================================
--
-- internal/db/migrate.go applies this on boot, inside main(), in one
-- transaction, in lexical order, so a failure here is a control-plane outage
-- on every install at once. Every statement is guarded: CREATE TABLE and
-- CREATE INDEX with IF NOT EXISTS, each policy wrapped in a pg_policies
-- existence check because PostgreSQL 16 has no CREATE POLICY IF NOT EXISTS
-- (the m94/m113 pattern), and GRANT/REVOKE are naturally idempotent. Nothing
-- is dropped, no existing row is read or written, and there is no backfill:
-- both tables start empty and empty is correct for every existing
-- organisation.
--
-- ORDINAL: 20260824000000, the first free one after m121
-- (20260823000000_m121_site_components_updated_at.sql), checked across every
-- ref in the repository and not only main, because ordinal is APPLY order and
-- not commit order - m113 carries 20260815000000 and was committed after m114
-- and m115, and migrate.go does not verify atlas.sum.
--
-- CONVERGE PATH: none is required, and this is a statement about the world
-- rather than an omission. No prior version of this migration has ever been
-- applied to any database: the ordinal is new, the two tables are new, this
-- file corrects nothing and it edits no applied migration. There is therefore
-- no database in any state that needs converging onto this one, and no m114/
-- m115-shaped follow-up is owed.

-- ---------------------------------------------------------------------------
-- org_context_versions - ADR-064 layer 2
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS "public"."org_context_versions" (
    "id"        uuid   PRIMARY KEY DEFAULT gen_random_uuid(),
    -- The organisation. In this schema an organisation IS a tenant, so this
    -- column is both the subject key and the tenant-isolation key.
    "tenant_id" uuid   NOT NULL,
    -- Monotonic from 1 per organisation. See DECISION 5: the unique index over
    -- this is what makes a concurrent double-write an error instead of a lost
    -- update, while ADR-064's open question 2 is still open.
    "version"   bigint NOT NULL CONSTRAINT "org_context_versions_version_positive_check"
        CHECK ("version" >= 1),

    -- THE TWO KINDS OF FIELD (ADR-064 Decision 3). Two columns, never one:
    -- the never-widen check reads restrictions and must not be reachable by
    -- guidance. See DECISION 1.
    --
    -- Structured, mechanically comparable. Decision 4's widen-check runs here.
    "restrictions" jsonb NOT NULL DEFAULT '{}'::jsonb
        CONSTRAINT "org_context_versions_restrictions_object_check"
        CHECK (jsonb_typeof("restrictions") = 'object'),
    -- Free text. ADR-064 is deliberate that "wider" and "narrower" are not
    -- defined relations over prose, and that no mechanical check applies here.
    "guidance"     jsonb NOT NULL DEFAULT '{}'::jsonb
        CONSTRAINT "org_context_versions_guidance_object_check"
        CHECK (jsonb_typeof("guidance") = 'object'),

    -- WHO. No foreign key, deliberately - see the header. author_id is NULL
    -- exactly when the author has no principal id, which today means the
    -- transfer operation of ADR-064 Decision 12.
    "author_type" text NOT NULL CONSTRAINT "org_context_versions_author_type_check"
        CHECK ("author_type" IN ('user', 'api_key', 'system')),
    "author_id"   uuid NULL,
    -- A 'system' author has no id and a credentialed one always does. This
    -- deliberately does NOT constrain which of users or api_keys the id names:
    -- that is what author_type is for, and no single column can reference two
    -- tables.
    CONSTRAINT "org_context_versions_author_id_matches_type_check"
        CHECK (("author_type" = 'system') = ("author_id" IS NULL)),

    -- HOW. Closed set; 'import' is deliberately absent. See DECISION 9.
    "provenance" text NOT NULL CONSTRAINT "org_context_versions_provenance_check"
        CHECK ("provenance" IN ('manual', 'restore', 'transfer')),
    -- Which version this one reproduces. NULL means "not a restore" - never a
    -- sentinel. No foreign key: the check that matters is a cross-row stamp
    -- comparison no single-column reference can express.
    "restored_from_version_id" uuid NULL,
    -- A restore names the version it restored, and nothing else does. This is
    -- the m115 lesson: close the constraint on the way in.
    CONSTRAINT "org_context_versions_restore_pointer_check"
        CHECK (("provenance" = 'restore') = ("restored_from_version_id" IS NOT NULL)),

    "created_at" timestamptz NOT NULL DEFAULT now()
);

-- The subject key, the concurrency guard and the "latest version" read path,
-- all in one index. tenant_id LEADS, so this also serves tenant lookups and
-- the purge cascade; that is why no separate tenant_id index is created here.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND tablename = 'org_context_versions'
          AND indexname = 'org_context_versions_version_key'
    ) THEN
        CREATE UNIQUE INDEX "org_context_versions_version_key"
            ON "public"."org_context_versions" ("tenant_id", "version");
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.org_context_versions'::regclass
          AND conname  = 'org_context_versions_tenant_id_fkey'
    ) THEN
        ALTER TABLE "public"."org_context_versions"
            ADD CONSTRAINT "org_context_versions_tenant_id_fkey"
            FOREIGN KEY ("tenant_id") REFERENCES "public"."tenants" ("id")
            ON UPDATE NO ACTION ON DELETE CASCADE;
    END IF;
END;
$$;

DO $$
BEGIN
    ALTER TABLE "public"."org_context_versions" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."org_context_versions" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'org_context_versions'
          AND policyname = 'org_context_versions_tenant_isolation'
    ) THEN
        CREATE POLICY "org_context_versions_tenant_isolation" ON "public"."org_context_versions"
            FOR ALL
            USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END;
$$;

-- A site-scoped collaborator may READ their organisation's context - ADR-064
-- Decision 6 requires it and Decision 8's preview renders it - but may never
-- AUTHOR a version of it. The table is append-only, so INSERT is the whole
-- write surface and this one policy closes all of it. See DECISION 7.
--
-- Note there is deliberately NO restrictive SELECT policy on this table. Its
-- absence is the mechanism by which the layer-2 read keeps working; do not
-- "complete the set" by adding one.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'org_context_versions'
          AND policyname = 'org_context_versions_site_scope_insert'
    ) THEN
        CREATE POLICY "org_context_versions_site_scope_insert" ON "public"."org_context_versions"
            AS RESTRICTIVE FOR INSERT
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- site_context_versions - ADR-064 layer 3
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS "public"."site_context_versions" (
    "id"        uuid   PRIMARY KEY DEFAULT gen_random_uuid(),
    -- THE ORGANISATION STAMP, per ADR-064 Decision 3: the organisation that
    -- owned this site AT THE TIME OF THIS WRITE. Set once, never rewritten by
    -- a later transfer. It is NOT a live view of the site's current owner, and
    -- the composite foreign key that would force it to be one is deliberately
    -- absent - see the header.
    "tenant_id" uuid   NOT NULL,
    "site_id"   uuid   NOT NULL,
    -- Monotonic from 1 per SITE, not per (site, organisation): the sequence
    -- continues across a transfer so a restore reference stays unambiguous.
    "version"   bigint NOT NULL CONSTRAINT "site_context_versions_version_positive_check"
        CHECK ("version" >= 1),

    "restrictions" jsonb NOT NULL DEFAULT '{}'::jsonb
        CONSTRAINT "site_context_versions_restrictions_object_check"
        CHECK (jsonb_typeof("restrictions") = 'object'),
    "guidance"     jsonb NOT NULL DEFAULT '{}'::jsonb
        CONSTRAINT "site_context_versions_guidance_object_check"
        CHECK (jsonb_typeof("guidance") = 'object'),

    "author_type" text NOT NULL CONSTRAINT "site_context_versions_author_type_check"
        CHECK ("author_type" IN ('user', 'api_key', 'system')),
    "author_id"   uuid NULL,
    CONSTRAINT "site_context_versions_author_id_matches_type_check"
        CHECK (("author_type" = 'system') = ("author_id" IS NULL)),

    "provenance" text NOT NULL CONSTRAINT "site_context_versions_provenance_check"
        CHECK ("provenance" IN ('manual', 'restore', 'transfer')),
    "restored_from_version_id" uuid NULL,
    CONSTRAINT "site_context_versions_restore_pointer_check"
        CHECK (("provenance" = 'restore') = ("restored_from_version_id" IS NOT NULL)),

    "created_at" timestamptz NOT NULL DEFAULT now()
);

-- Subject key, concurrency guard and latest-version read path. Keyed on
-- site_id ALONE alongside version - see DECISION 5 for why the stamp is not
-- part of it.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND tablename = 'site_context_versions'
          AND indexname = 'site_context_versions_version_key'
    ) THEN
        CREATE UNIQUE INDEX "site_context_versions_version_key"
            ON "public"."site_context_versions" ("site_id", "version");
    END IF;
END;
$$;

-- tenant_id leads no other index here, and the tenant purge cascade would
-- otherwise sequentially scan this table.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND tablename = 'site_context_versions'
          AND indexname = 'site_context_versions_tenant_idx'
    ) THEN
        CREATE INDEX "site_context_versions_tenant_idx"
            ON "public"."site_context_versions" ("tenant_id");
    END IF;
END;
$$;

-- Two SINGLE-COLUMN foreign keys, never the composite. Both ON DELETE CASCADE:
-- ADR-064 Decision 12 retains context for the life of the organisation or the
-- site and frees it with them, and the accountability record that must outlive
-- the snapshot lives in audit_log, not here. See DECISION 4.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.site_context_versions'::regclass
          AND conname  = 'site_context_versions_tenant_id_fkey'
    ) THEN
        ALTER TABLE "public"."site_context_versions"
            ADD CONSTRAINT "site_context_versions_tenant_id_fkey"
            FOREIGN KEY ("tenant_id") REFERENCES "public"."tenants" ("id")
            ON UPDATE NO ACTION ON DELETE CASCADE;
    END IF;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.site_context_versions'::regclass
          AND conname  = 'site_context_versions_site_id_fkey'
    ) THEN
        ALTER TABLE "public"."site_context_versions"
            ADD CONSTRAINT "site_context_versions_site_id_fkey"
            FOREIGN KEY ("site_id") REFERENCES "public"."sites" ("id")
            ON UPDATE NO ACTION ON DELETE CASCADE;
    END IF;
END;
$$;

DO $$
BEGIN
    ALTER TABLE "public"."site_context_versions" ENABLE ROW LEVEL SECURITY;
    ALTER TABLE "public"."site_context_versions" FORCE ROW LEVEL SECURITY;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'site_context_versions'
          AND policyname = 'site_context_versions_tenant_isolation'
    ) THEN
        CREATE POLICY "site_context_versions_tenant_isolation" ON "public"."site_context_versions"
            FOR ALL
            USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END;
$$;

-- The m19/m113 exemplar shape, one RESTRICTIVE FOR ALL policy on site_id.
-- USING confines the read; WITH CHECK stops a collaborator authoring a context
-- version that names a site it was never granted. site_id is NOT NULL, so
-- unlike m112's email tables there is no inheriting organisation row here to
-- keep readable and no reason to split read from write - ADR-064 Decision 3
-- puts layer 2 in its own table instead.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'site_context_versions'
          AND policyname = 'site_context_versions_site_scope'
    ) THEN
        CREATE POLICY "site_context_versions_site_scope" ON "public"."site_context_versions"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
                )
            );
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- Grants: append-only at the privilege level (ADR-064's own requirement)
--
-- The REVOKE is the load-bearing half. m1's ALTER DEFAULT PRIVILEGES
-- (20260527130000_auth_multitenancy.sql:123-126) already granted wpmgr_app
-- SELECT, INSERT, UPDATE and DELETE on every future table in this schema,
-- these two included, without anyone typing it. Omitting an UPDATE grant does
-- not withhold UPDATE; revoking it does. audit_log is revoked at m1:130 for
-- the same reason and these follow it.
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT ON "public"."org_context_versions"  TO wpmgr_app;
GRANT SELECT, INSERT ON "public"."site_context_versions" TO wpmgr_app;

REVOKE UPDATE, DELETE, TRUNCATE ON "public"."org_context_versions"  FROM wpmgr_app;
REVOKE UPDATE, DELETE, TRUNCATE ON "public"."site_context_versions" FROM wpmgr_app;
