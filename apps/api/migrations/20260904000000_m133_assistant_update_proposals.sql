-- m133 - the proposal record: one site, one plugin, one from-version, one
-- to-version, proposed by a connection and decided by a human.
--
-- This migration CORRECTS NOTHING. It edits no applied migration and re-runs no
-- backfill. It is not an m114/m115-shaped repair. It is additive: one new table
-- and its policies and indexes. Not one existing object is dropped or altered.
--
-- CONVERGE PATH. None is owed. Every statement is CREATE TABLE IF NOT EXISTS,
-- CREATE INDEX IF NOT EXISTS, or a CREATE POLICY guarded by a pg_policies
-- existence check, so the file is idempotent and a database in any state
-- converges by applying this file and nothing else. There is no operator step.
-- The one case worth knowing about is that a database already carrying a table
-- or policy of these names keeps its own and this file skips it silently; see
-- PROVING IT at the foot.
--
-- ===========================================================================
-- WHY IT EXISTS, AND WHY IT IS FIRST
-- ===========================================================================
--
-- ADR-061 puts a human between the assistant and any change to a live site.
-- The assistant proposes; a person approves; a worker dispatches. This table is
-- the proposal, and it is the FIRST thing built because one of its columns
-- cannot be added later.
--
-- `presented_digest` is a fingerprint of exactly what the human was shown on
-- screen at the moment they decided. Every approval taken before that column
-- exists is permanently unverifiable -- there is no backfill that can recover
-- what a screen rendered last week. Adding it in a later migration would leave
-- a prefix of the audit trail that cannot answer "what did they actually see",
-- and that prefix would be exactly the early period when the surface was least
-- trusted. So it lands with the table or it is worthless.
--
-- ===========================================================================
-- DECISION 1: THE PRECEDENT IS site_db_scan_results, AND WHERE IT DIVERGES
-- ===========================================================================
--
-- site_db_scan_results (schema.sql, M39/M41) is the machine-proposes /
-- human-approves-item-by-item flow this codebase already has. Its orphan review
-- enumerates candidates a scan found and the operator ticks them one at a time.
--
-- TAKEN FROM IT, unchanged:
--
--   a. site_id + tenant_id side by side on a site-keyed row, with the tenant
--      index and the full RLS quartet, rather than reaching the tenant through
--      a join. A policy that has to join is a policy that can be joined around.
--   b. The proposal is a RECORD OF A COMPUTATION, not a command. Nothing in
--      this table causes anything. A row is inert until a human decides and a
--      separate worker acts. site_db_scan_results has the same property: the
--      clean is a different call over a different table.
--   c. The full preview is stored on the row rather than recomputed at decision
--      time, so the operator confirms against what was measured, not against
--      whatever the fleet looks like when they get round to it.
--
-- DELIBERATELY DIFFERENT, and each difference is the reason this is a new table
-- rather than a column on that one:
--
--   d. ONE ROW IS ONE DECISION, not one row per site holding a basket of
--      candidates. site_db_scan_results is upserted per site and its JSONB
--      columns hold many items; approving is per item, inside the document.
--      That shape cannot carry a per-item state machine, a per-item expiry or a
--      per-item fingerprint, and all three are load-bearing here. See
--      DECISION 2.
--   e. NOT UPSERTED. site_db_scan_results has PRIMARY KEY (site_id) and every
--      scan overwrites the last. A proposal that can be overwritten in place is
--      a proposal that can change underneath the person reading it, which is
--      the exact hazard presented_digest exists to close. Rows here are
--      append-only in spirit: inserted once, transitioned through states, never
--      rewritten in their facts.
--   f. THE PROPOSER IS A CREDENTIAL, NOT AN OPERATOR. site_db_scan_results is
--      operator-initiated end to end; no AI can reach it. Here the proposer is
--      an MCP connection, which is why proposed_by_grant_id exists at all and
--      why "who asked" is a first-class column rather than implicit.
--   g. NO AGENT POLICY. See DECISION 5.
--
-- ===========================================================================
-- DECISION 2: ONE SITE, ONE PLUGIN. BULK IS UNREPRESENTABLE, NOT DISCOURAGED
-- ===========================================================================
--
-- site_id, component_type, component_slug, from_version and to_version are
-- SCALARS. There is no array column here, no JSONB list, and no child table.
-- A proposal covering two sites, or two plugins on one site, cannot be written
-- -- not "is rejected by a handler", cannot be written.
--
-- That is deliberate and it is the design's own reasoning: bulk is where a
-- small mistake becomes a large one, and approving twelve sites at once is a
-- different approval conversation with a different consent bar. Making the
-- second conversation require a migration is the same gate m124 DECISION 1
-- built for capabilities, applied to blast radius instead of permission.
--
-- component_type is a CLOSED SET WITH ONE MEMBER, 'plugin'. Themes and core
-- updates are real and the update engine already performs both; they are absent
-- here because each is its own risk conversation -- a core update can take a
-- site down in a way a plugin update generally cannot -- and because a one
-- member set costs nothing to widen and everything to have skipped. The column
-- exists rather than being implied so that widening is an ALTER of a named
-- constraint on a reviewed diff, not the discovery that a text column silently
-- accepted 'core' all along.
--
-- from_version <> to_version is enforced. A proposal to change nothing is not a
-- proposal; it is a screen asking a human to consent to a no-op, which teaches
-- them that consent is a formality.
--
-- ===========================================================================
-- DECISION 3: THE STATE MACHINE, AND WHY 'approved_undispatched' IS A STATE
-- ===========================================================================
--
--   pending                Written by the propose tool. Inert. The only state
--                          from which a human decision is possible.
--   approved_undispatched  A HUMAN SAID YES AND NOTHING HAS BEEN SENT.
--   dispatched             A worker claimed the approved row and created the
--                          update run. Terminal here; the run's own status
--                          takes over from this point.
--   rejected               A human said no, and the row names which human.
--                          Terminal. DECISION 9.
--   expired                The window closed with no decision. Terminal, and
--                          never approved. See DECISION 4.
--
-- "TERMINAL" ABOVE DESCRIBES THE WORKFLOW, NOT AN ENFORCED PROPERTY. The
-- database has no transition guard: a CHECK sees only the finished row, so the
-- previous state constrains nothing about the next one, and a row CAN be moved
-- out of a state this list calls terminal. Section (6) says which moves remain
-- reachable and why closing them was left to a later decision. Read this list
-- as what the callers do, not as what the schema forbids.
--
-- ADR-061 requires 'approved, not yet dispatched' to be a committed state
-- rather than a moment between two statements, and states why in its own words:
-- dispatching inside the approving transaction holds a transaction open across
-- a network call to a machine that may be hanging, and dispatching immediately
-- after commit loses the action outright if the process dies in the gap. With
-- the state committed, a crash in the gap is an unclaimed row, which is what
-- recovery is for.
--
-- THE SHAPE IS update_runs', NOT A NEW ONE. update_runs already runs
-- scheduled -> dispatching -> ... with a partial index (update_runs_due_idx) the
-- dispatcher scans and a claim that moves the row out of that index so a second
-- replica cannot claim it twice. assistant_update_proposals_dispatch_idx below
-- is the same device for the same reason, and update_runs' 'expired' carries
-- the same meaning as ours: "passed its window undispatched. Terminal and never
-- retried."
--
-- ONE DIFFERENCE FROM update_runs, STATED BECAUSE IT IS A DEFECT THERE. That
-- column has NO CHECK CONSTRAINT -- its own comment says so, and says a typo'd
-- value "stores fine and silently never dispatches". This column has one. A
-- proposal whose state is a typo would be a human decision the dispatcher never
-- sees, and silence is the one failure mode an approval surface may not have.
--
-- ===========================================================================
-- DECISION 4: RUNNING OUT OF TIME MEANS REJECTED, AND THE DATABASE HOLDS IT
-- ===========================================================================
--
-- "There is no timer anywhere in this product that turns waiting into consent."
--
-- THIS PARAGRAPH ONCE CLAIMED MORE THAN THE SCHEMA DELIVERED, and the claim was
-- repeated onward as fact before a review disproved it by execution. It said
-- "approve anyway" and "extend" were "absent from this schema and neither can
-- be added without a migration". "Extend" was one UPDATE on expires_at. Section
-- (5) closes that by column privilege; section (6) lists what is still
-- reachable. Read section (6) before repeating any absolute claim from here.
--
-- WHAT IS TRUE, EXACTLY: there is no "approve anyway" CONTROL and no "extend"
-- CONTROL -- no column, no state and no value exists for either, so neither can
-- be implemented without a migration. The window itself is now immutable after
-- insert. What the schema does NOT do is prevent a privileged writer from
-- rewriting one human decision into another; see section (6) item 2.
--
-- The invariant is enforced STRUCTURALLY by assistant_update_proposals_
-- consent_within_window_check:
--
--     state NOT IN ('approved_undispatched','dispatched')
--     OR (decided_at IS NOT NULL AND decided_at < expires_at)
--
-- WHAT IT ACTUALLY GUARANTEES, STATED IN THE ONLY TERMS THAT ARE TRUE: an
-- approved row whose RECORDED decided_at is at or after its own expires_at is
-- unrepresentable. That is a property of the finished row, so it does hold
-- whatever path wrote it and whatever order the statements ran in.
--
-- IT IS NOT A GUARANTEE ABOUT THE CLOCK, AND THE DIFFERENCE IS THE WHOLE POINT.
-- The constraint compares decided_at to expires_at. It never compares either to
-- now(), and it cannot: PostgreSQL refuses a non-immutable function in a CHECK,
-- so there is no version of this constraint that knows what time it is. It
-- therefore guarantees the row is SELF-CONSISTENT, not that the approval
-- happened while the window was open. A caller that supplies its own decided_at
-- inside the closed window gets an approved row on an expired proposal, and a
-- review reached exactly that state by execution.
--
-- WHICH IS WHY (7)(c) IS A HARD REQUIREMENT AND NOT A STYLE NOTE. When the
-- approve statement sets decided_at = now() and lets the database evaluate it,
-- the constraint's comparison becomes a comparison against the real clock and
-- the guarantee above is the guarantee everyone wants: the window has closed,
-- PostgreSQL raises 23514, the approval does not happen. When decided_at comes
-- from a request body or a client clock instead, the constraint still passes
-- and it is checking the caller's arithmetic against itself. The invariant is
-- half schema and half caller, and the schema half cannot be made to cover the
-- other one.
--
-- WHY A CHECK AND NOT A TRIGGER. There is no CREATE TRIGGER anywhere in this
-- migration tree or in db/schema.sql (0 matches for 'CREATE TRIGGER' in
-- schema.sql). Introducing the first one on an authorisation path, in the
-- migration that also introduces the table, is inventing a mechanism where the
-- codebase has a precedent. The CHECK reaches the same guarantee for the case
-- that matters and is visible in \d.
--
-- WHAT THIS CHECK IS AND IS NOT. It is a constraint over the FINISHED ROW: it
-- compares two columns as they end up, and it cannot see what either of them
-- said a moment earlier. That is the limit of the mechanism, not a defect in
-- this particular constraint, and it is why section (6) states the guarantees
-- positively rather than claiming the schema forbids everything in this area --
-- which is what an earlier revision claimed, wrongly, and had repeated onward
-- as fact before a review disproved it.
--
-- The residual set this leaves is tracked in the maintainers' private notes,
-- not here. See section (6).
--
-- SECOND HALF OF THE SAME INVARIANT: expiry never names a human.
-- assistant_update_proposals_expiry_is_not_a_decision_check requires
-- decided_by_user_id IS NULL whenever state = 'expired'. A sweeper writing
-- 'expired' cannot accidentally attribute the outcome to the person who did not
-- answer, which is the shape a later audit read would misreport as a decision.
--
-- ===========================================================================
-- DECISION 5: RLS -- WHICH POLICIES, AND THE ONE DELIBERATELY OMITTED
-- ===========================================================================
--
-- This table is tenant-scoped AND site-keyed, so it joins the set m19 started
-- and m132 last extended. Three policies, and one sibling policy left out on
-- purpose.
--
--   assistant_update_proposals_tenant_isolation   PERMISSIVE, FOR ALL.
--       The ordinary organisation boundary every tenant-keyed table carries.
--
--   assistant_update_proposals_site_scope         RESTRICTIVE, FOR ALL.
--       The m19 predicate byte for byte, as m132 used it on 22 tables. Without
--       it the database refuses another TENANT and has no opinion about another
--       SITE -- which is precisely what m112 cost: four email tables shipped
--       without this and seven privilege-escalation doors were closed in
--       handlers before anyone asked why they kept appearing. A proposal is a
--       change to a named site, so a site-scoped connection seeing a proposal
--       for a site outside its scope is both an information leak and, once the
--       approve path exists, a route to a change it could not have proposed.
--       RESTRICTIVE policies are AND-combined and can only ever subtract, so
--       this is inert on every path that does not set app.site_scope.
--
--   assistant_update_proposals_agent              PERMISSIVE, FOR ALL.
--       The dispatch worker and the expiry sweep. Both run outside any one
--       organisation's transaction and must see rows across tenants.
--
-- ON THE THIRD POLICY, BECAUSE ITS NAME IS MISLEADING AND THE FIRST DRAFT OF
-- THIS MIGRATION GOT IT WRONG. `app.agent` reads as "the WordPress plugin", and
-- on that reading this table should have no such policy: a plugin on a
-- customer's server has no business reading proposals -- it is handed a signed
-- command by the existing update path and never learns why. That reading is
-- incomplete. `app.agent` is this codebase's CROSS-TENANT SERVICE CONTEXT, set
-- by pool.InAgentTx, and it is what the update dispatcher itself runs under:
-- dispatch_repo.go:162 says so in as many words -- "CROSS-TENANT, under
-- InAgentTx, admitted by the m118 update_runs_agent policy" -- and
-- ClaimUpdateRunForDispatch, ListDueUpdateRuns and FinishUpdateRunDispatch all
-- open InAgentTx at dispatch_repo.go:181, :223 and :267.
--
-- The first draft of this file invented a fourth GUC, `app.service`, to name
-- that context "honestly". It does not exist. `git grep -hon
-- "current_setting('app\.[a-z_]*'" -- apps/api/migrations apps/api/db/schema.sql
-- | sed "s/.*current_setting(//" | sort | uniq -c | sort -rn` returns app.agent
-- 297 times and app.service twice, both of them that draft's own lines. A GUC
-- nothing sets is a policy nothing satisfies, and the dispatch worker would
-- have found this table empty at 3am with every proof still green, because no
-- proof in this file exercises the worker's path.
--
-- So the policy is the ordinary `_agent` one, matching the 83 siblings that
-- carry it (`grep -rhoE 'CREATE POLICY "?[a-z_0-9]+_agent"?'
-- apps/api/migrations/*.sql | sort -u | grep -c .` -> 83) and matching m118's
-- update_runs_agent specifically, which exists for this exact worker.
--
-- WHAT THAT COSTS, STATED RATHER THAN GLOSSED. The predicate carries NO TENANT
-- TERM, so it is a deliberate cross-tenant hole, safe only because every caller
-- that opens InAgentTx has already bound the work. That is the standing bargain
-- on 83 tables and this table does not renegotiate it. The site-scope policy
-- above is RESTRICTIVE and therefore still subtracts even inside an agent
-- transaction that sets app.site_scope; the agent policy cannot widen past it.
--
-- ===========================================================================
-- DECISION 6: THE FINGERPRINT
-- ===========================================================================
--
-- ONE COLUMN. presented_digest, NOT NULL, set at INSERT, 64 lowercase hex --
-- a SHA-256 over the control-plane-derived facts that the approval surface
-- renders. It buys three things at once:
--
--   a. APPROVING TWICE IS IMPOSSIBLE. The approve statement is
--      `... WHERE id = $1 AND presented_digest = $2 AND state = 'pending'`.
--      The second submission of the same screen matches zero rows because the
--      state has moved, and it does so in the database rather than in a handler
--      that has to remember to look.
--   b. APPROVING AFTER THE PROPOSAL CHANGED UNDERNEATH THE READER IS
--      IMPOSSIBLE. The digest travels with the rendered page and comes back
--      with the decision. If any fact changed, it does not match and the
--      approval is refused rather than silently applied to a different change
--      from the one that was read.
--   c. THE AUDIT TRAIL CAN SAY WHAT WAS DISPLAYED. A year on, the row still
--      carries the fingerprint of the screen, so "did they see the downgrade
--      flag" has an answer.
--
-- IT IS NOT UNIQUE, and that is not an oversight. A rejected proposal followed
-- by an honest re-proposal of the identical change is legitimate and must not
-- collide. Uniqueness would turn a correct workflow into a 23505 and teach
-- callers to perturb the digest, which destroys (b). The anti-spam guard is
-- assistant_update_proposals_one_live_per_component_idx instead: at most one
-- PENDING proposal per (tenant, site, component), so the assistant cannot queue
-- five asks for the same plugin and have an operator approve two of them.
--
-- WHAT IS EXCLUDED FROM THE DIGEST. `note` -- the single quarantined
-- model-authored free text ADR-061 Decision 3 permits -- is deliberately NOT
-- covered, per that decision: "It is excluded from the plan digest, so editing
-- it cannot invalidate an approval and cannot be used to churn one." The digest
-- covers facts. The note is not a fact and never becomes one.
--
-- THE DIGEST IS COMPUTED BY THE CONTROL PLANE, NOT SUPPLIED BY THE PROPOSER.
-- A column the proposer fills is a fingerprint of what the proposer says was
-- shown. That is a note to the Go layer, not something this file can enforce.
--
-- ===========================================================================
-- DECISION 7: WHO ASKED, AND WHY IT IS NOT A FOREIGN KEY
-- ===========================================================================
--
-- proposed_by_grant_id is the mcp_grants row -- the connection and its
-- capability set -- that asked. NOT NULL: a proposal whose origin is unknown is
-- one nobody can revoke the source of.
--
-- NO FOREIGN KEY, for the reason mcp_grants itself gives for client_id:
-- "neither ON DELETE action is right for a recorded fact." CASCADE would delete
-- the proposal history when a connection is deleted, destroying the record of
-- what that connection asked for -- the audit question one most wants answered
-- after a connection is removed. SET NULL destroys the same fact more quietly.
-- RESTRICT makes deleting a connection fail, so the safety action an operator
-- takes in an incident is blocked by the evidence of the incident.
--
-- decided_by_user_id gets THE SAME RESOLUTION, and an earlier draft of this
-- file got it wrong in a way worth recording, because the wrong version looked
-- more careful than the right one.
--
-- That draft wrote `REFERENCES users ON DELETE SET NULL`, matching
-- update_runs.created_by, whose comment says "SET NULL on user deletion so the
-- run history survives". Copying a neighbour is usually right and here it was
-- not, for two reasons that only appear when the column is an APPROVAL rather
-- than an authorship stamp:
--
--   a. IT MAKES AN ORDINARY ADMINISTRATIVE ACTION ERASE AN AUDIT RECORD.
--      Offboarding an employee sets decided_by_user_id to NULL on every
--      approval they ever gave. The row goes on saying an approval happened
--      while no longer saying who gave it, which is the one question an audit
--      of an approval exists to answer. An audit record whose subject can be
--      removed by routine account cleanup is not an audit record.
--   b. IT IS INCOMPATIBLE WITH DECISION 8, and not merely in tension with it.
--      DECISION 8 requires decided_by_user_id to be present on an approved row.
--      A DELETE on users would drive every such row into violation, so the
--      DELETE ITSELF FAILS -- offboarding blocked by the evidence, which is the
--      exact failure this file rejects one paragraph above for grants. The two
--      cannot both hold with a foreign key present, and DECISION 8 is the one
--      that carries the safety property.
--
-- So the FK goes and the column stays: NOT a foreign key, required whenever the
-- state is an approval, and untouched by anything that happens to the users
-- table afterwards. update_runs.created_by is a different kind of column -- who
-- started a job -- and its SET NULL is right for what it is.
--
-- WHAT IS LOST, SAID PLAINLY: nothing enforces that decided_by_user_id names a
-- row that exists, so a bad writer can store a uuid that never was a user. That
-- is the same exposure proposed_by_grant_id carries and it is the price of the
-- record surviving deletion. The alternative prices are worse: RESTRICT blocks
-- offboarding, SET NULL erases the approver.
--
-- ===========================================================================
-- DECISION 8: AN APPROVAL NAMES A HUMAN, OR IT IS NOT AN APPROVAL
-- ===========================================================================
--
-- assistant_update_proposals_approval_names_a_human_check requires
-- decided_by_user_id IS NOT NULL whenever state is 'approved_undispatched' or
-- 'dispatched'. An approved proposal with no named approver is unrepresentable.
--
-- WHY THIS IS IN THE SCHEMA AND NOT IN THE APPROVE HANDLER. The existing
-- site-write path gates on permission plus organisation scope and nothing more,
-- and update_runs.created_by is documented NULL for an API-key principal
-- (schema.sql: "created_by is the acting user (NULL for an API-key
-- principal)"). So the shape already exists in this codebase where a
-- non-human principal reaches a write path and the actor column is simply
-- empty. If the approve path inherited that shape, an approval with nobody's
-- name on it would be written silently and would look exactly like a real one.
--
-- The constraint turns that into an error at the moment of the write. This is
-- the strongest form the "no automation escape hatch" rule can take: an
-- API-key principal HAS no user id, so a credential cannot satisfy this
-- constraint at all. "A machine approved this" is not refused by a check
-- somebody has to remember to write -- it is a row the database will not store.
-- ADR-061 forbids an auto-approve tier, a policy engine that can approve, a
-- trusted-automation flag and an approve button in an email; this constraint is
-- what makes all four unimplementable by accident rather than merely
-- prohibited.
--
-- 'rejected' IS DELIBERATELY NOT COVERED, and the asymmetry is the point.
-- Requiring a named human to REJECT would block the legitimate system-initiated
-- rejections this table will need -- the site was deleted, the plugin is gone,
-- the target version was pulled. Rejection is the fail-safe direction: a
-- machine rejecting a change is the conservative outcome and costs an operator
-- a second ask, whereas a machine approving one changes a live site. The
-- constraint guards the direction where being wrong is expensive.
--
-- 'expired' REMAINS FORBIDDEN FROM NAMING A HUMAN, by the separate check in
-- DECISION 4. The two constraints are complementary and both are needed: one
-- says an approval must name someone, the other says a timeout must not.
--
-- AN EARLIER VERSION OF THIS PARAGRAPH OVERCLAIMED and is corrected here rather
-- than deleted. It said the two constraints "make the approver column mean
-- exactly one thing -- a person decided -- in every state it is populated".
-- They do not, and a review disproved it by execution.
--
-- WHAT DECISION 8 ACTUALLY GUARANTEES, and it is the part that carries the
-- safety property: an approved row always names SOME human, and a machine can
-- never be that human, because an API-key principal has no user id to supply.
-- "A machine approved this" remains a row the database will not store.
--
-- What is NOT guaranteed is that a named human's recorded decision is the one
-- they made. Closing that needs a transition guard, which needs a trigger,
-- which this tree does not have; section (6) records the reasoning and the
-- residual set is tracked in the maintainers' private notes rather than here.
--
-- tenant_id IS a foreign key with ON DELETE CASCADE, matching every other
-- tenant-keyed table. Asked as m113/m116 require: what audit or reclaim record
-- dies with this cascade? Nothing that outlives the tenant. A proposal names a
-- site that is being deleted, in an organisation that is being deleted, and it
-- reserves no object storage and no external resource -- unlike backup_chunks,
-- whose ciphertext outlives the row and is why m113 exists. There is nothing
-- here for a reclaim sweep to find later, so the cascade strands nothing.

-- ===========================================================================
-- DECISION 9: A REJECTION NAMES ITS HUMAN, AND THE ROW CANNOT BE DELETED
-- ===========================================================================
--
-- Two additions a review reached by execution as wpmgr_app. Both are the same
-- observation from different directions: DECISION 8 protects a COLUMN inside a
-- row, and a column is only as durable as the row and the states around it.
--
-- THE ROW WAS ERASABLE. The application role held DELETE on this table -- the
-- observed relacl was `wpmgr_app=ard/wpmgr`, where the `d` is DELETE -- and an
-- approved proposal was deleted with it. Section (5) argues at length that
-- presented_digest and expires_at cannot be rewritten; none of that survives a
-- writer who can drop the whole row instead. Every precedent section (5)
-- invokes (audit_log in m2, the two context tables in m122) revokes DELETE
-- alongside UPDATE, and this table now does the same. It is append-and-decide
-- like all three: an outcome is a state here, never an absence, which is the
-- entire reason 'expired' and 'rejected' exist as values.
--
-- THE APPROVER WAS ERASABLE IN PLACE. decided_by_user_id has to stay inside the
-- column UPDATE grant, because an approval writes it. Before this decision
-- 'rejected' was the one decided state with no requirement to name anyone, so
-- moving an approved row to 'rejected' while nulling the column was legal, and
-- it removed the record of who approved without leaving a gap a reader would
-- notice. The new rejection_names_a_human_check closes it, and the rule it
-- establishes is the simple one worth remembering: EVERY DECIDED STATE NAMES A
-- HUMAN, and expiry -- which is not a decision -- may name nobody.
--
-- WHY REQUIRING IT OF 'rejected' IS RIGHT ON ITS OWN MERITS, not just as a
-- side effect of closing that path: a rejection IS a human decision under
-- ADR-061. The automatic outcome has its own state, 'expired', and DECISION 4
-- forbids that one from naming anyone. A decided-but-anonymous rejection was
-- therefore a state with no meaning in the design, and one that an audit read
-- would have to guess at. No caller writes 'rejected' today, so nothing is
-- broken by requiring it now, and requiring it later would need a backfill.
--
-- WHAT NEITHER ADDITION DOES: replace a transition guard. The decider can
-- still be overwritten with a different human, and states are still not
-- terminal. Section (6) names those in full.

-- ===========================================================================
-- (1) THE TABLE
-- ===========================================================================

CREATE TABLE IF NOT EXISTS "public"."assistant_update_proposals" (
    "id" uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Tenancy and site key side by side, per DECISION 1(a).
    "tenant_id" uuid NOT NULL
        REFERENCES "public"."tenants" ("id") ON DELETE CASCADE,
    "site_id"   uuid NOT NULL
        REFERENCES "public"."sites" ("id") ON DELETE CASCADE,

    -- WHO ASKED. No FK, per DECISION 7.
    "proposed_by_grant_id" uuid NOT NULL,

    -- WHAT IS PROPOSED. Scalars only, per DECISION 2.
    "component_type" text NOT NULL
        CONSTRAINT "assistant_update_proposals_component_type_check"
        CHECK ("component_type" IN ('plugin')),
    "component_slug" text NOT NULL
        CONSTRAINT "assistant_update_proposals_component_slug_not_blank_check"
        CHECK (length(btrim("component_slug")) > 0),
    "from_version" text NOT NULL
        CONSTRAINT "assistant_update_proposals_from_version_not_blank_check"
        CHECK (length(btrim("from_version")) > 0),
    "to_version" text NOT NULL
        CONSTRAINT "assistant_update_proposals_to_version_not_blank_check"
        CHECK (length(btrim("to_version")) > 0),
    CONSTRAINT "assistant_update_proposals_version_changes_check"
        CHECK ("from_version" <> "to_version"),

    -- THE FINGERPRINT. DECISION 6. SHA-256, lowercase hex, set at INSERT.
    "presented_digest" text NOT NULL
        CONSTRAINT "assistant_update_proposals_presented_digest_shape_check"
        CHECK ("presented_digest" ~ '^[0-9a-f]{64}$'),

    -- THE ONE QUARANTINED PROPOSER-CONTROLLED FREE TEXT, ADR-061 Decision 3.
    -- Every other column on this table is control-plane-derived or a closed
    -- enum. Excluded from the digest on purpose. Bounded so a model cannot
    -- push a wall of text onto a decision surface; the renderer still has to
    -- treat it as hostile, which is Session C's two text cleaners, not this
    -- file's job.
    "note" text NULL
        CONSTRAINT "assistant_update_proposals_note_length_check"
        CHECK ("note" IS NULL OR length("note") <= 2000),

    -- THE STATE MACHINE. DECISION 3. Closed set, NOT NULL, NO DEFAULT: a
    -- DEFAULT here would mean a caller that forgot to say produces a row in
    -- whichever state the default names.
    "state" text NOT NULL
        CONSTRAINT "assistant_update_proposals_state_check"
        CHECK ("state" IN (
            'pending',
            'approved_undispatched',
            'dispatched',
            'rejected',
            'expired'
        )),

    "created_at" timestamptz NOT NULL DEFAULT now(),

    -- THE WINDOW. NOT NULL, NO DEFAULT: a proposal with no expiry is one that
    -- waits forever, and a stale ask approved months later is the surprise the
    -- window exists to prevent.
    "expires_at" timestamptz NOT NULL,
    CONSTRAINT "assistant_update_proposals_window_is_positive_check"
        CHECK ("expires_at" > "created_at"),

    -- THE DECISION.
    "decided_at" timestamptz NULL,
    -- THE APPROVER. A RECORDED FACT, NOT A FOREIGN KEY. See DECISION 7.
    "decided_by_user_id" uuid NULL,

    -- A decided state has a decision time. Without this, 'rejected' with a NULL
    -- decided_at is representable and the audit trail cannot say when.
    CONSTRAINT "assistant_update_proposals_decided_states_have_time_check"
        CHECK (
            ("state" IN ('approved_undispatched', 'dispatched', 'rejected'))
            = ("decided_at" IS NOT NULL)
        ),

    -- RUNNING OUT OF TIME IS NEVER CONSENT. DECISION 4.
    CONSTRAINT "assistant_update_proposals_consent_within_window_check"
        CHECK (
            "state" NOT IN ('approved_undispatched', 'dispatched')
            OR ("decided_at" IS NOT NULL AND "decided_at" < "expires_at")
        ),

    -- Expiry never names a human. DECISION 4, second half.
    CONSTRAINT "assistant_update_proposals_expiry_is_not_a_decision_check"
        CHECK ("state" <> 'expired' OR "decided_by_user_id" IS NULL),

    -- AN APPROVAL NAMES A HUMAN, OR IT IS NOT AN APPROVAL. DECISION 8.
    -- This is the constraint that makes "a machine approved this" a state the
    -- database refuses rather than a rule a handler remembers.
    CONSTRAINT "assistant_update_proposals_approval_names_a_human_check"
        CHECK (
            "state" NOT IN ('approved_undispatched', 'dispatched')
            OR "decided_by_user_id" IS NOT NULL
        ),

    -- A REJECTION NAMES ITS HUMAN TOO. DECISION 9.
    -- Every decided state that is not expiry names the person who decided it.
    -- Expiry is the one decided-looking state with no human, and it is not a
    -- decision -- the constraint above this one forbids it naming anyone.
    --
    -- This is not symmetry for its own sake. decided_by_user_id is inside the
    -- column UPDATE grant section (5) hands back, because an approval has to be
    -- able to write it. Without this constraint,
    --
    --     SET state = 'rejected', decided_by_user_id = NULL
    --
    -- is a legal statement against an already-approved row, and it erases who
    -- approved while leaving a plausible decided row behind. A review executed
    -- exactly that. With this constraint that statement raises 23514: a
    -- rejection cannot be anonymous, so there is no decided state a writer can
    -- move an approved row into that drops the name.
    --
    -- WHAT THIS DOES NOT CLOSE, stated here so the next reader does not read
    -- more into it than it says: it stops the name being ERASED, not
    -- REPLACED. Overwriting decided_by_user_id with a different user id is
    -- still accepted, because a CHECK sees only the finished row and cannot
    -- tell that the column just changed. Section (6) has the full list.
    CONSTRAINT "assistant_update_proposals_rejection_names_a_human_check"
        CHECK (
            "state" <> 'rejected'
            OR "decided_by_user_id" IS NOT NULL
        ),

    -- THE HANDOFF. Set by the dispatch worker in the same statement that moves
    -- the row to 'dispatched', so "dispatched" and "here is the run" are one
    -- fact and cannot disagree. No FK, same recorded-fact reasoning as
    -- DECISION 7: an update run may be pruned, and pruning it must not erase
    -- that this proposal was acted on.
    "dispatched_update_run_id" uuid NULL,
    CONSTRAINT "assistant_update_proposals_dispatch_pointer_check"
        CHECK (("state" = 'dispatched') = ("dispatched_update_run_id" IS NOT NULL))
);

-- ===========================================================================
-- (2) INDEXES
-- ===========================================================================

-- Tenant index, as every tenant-keyed table carries.
CREATE INDEX IF NOT EXISTS "assistant_update_proposals_tenant_idx"
    ON "public"."assistant_update_proposals" ("tenant_id");

-- Site index: the approval surface and every per-site read start here.
CREATE INDEX IF NOT EXISTS "assistant_update_proposals_site_idx"
    ON "public"."assistant_update_proposals" ("site_id");

-- THE DISPATCH QUEUE, partial on the one state the worker claims. Same device
-- as update_runs_due_idx: claiming moves the row out of this index, so a second
-- replica or a restart cannot claim it twice. DECISION 3.
CREATE INDEX IF NOT EXISTS "assistant_update_proposals_dispatch_idx"
    ON "public"."assistant_update_proposals" ("decided_at")
    WHERE "state" = 'approved_undispatched';

-- THE EXPIRY SWEEP, partial on the only state that can expire. The sweep reads
-- this index and nothing else; a full scan of every proposal ever made, run on
-- a timer, is how a sweep becomes the slowest query on the box.
CREATE INDEX IF NOT EXISTS "assistant_update_proposals_expiry_sweep_idx"
    ON "public"."assistant_update_proposals" ("expires_at")
    WHERE "state" = 'pending';

-- AT MOST ONE LIVE ASK PER COMPONENT. DECISION 6. Partial on 'pending', so a
-- rejected or expired proposal does not block an honest re-ask.
CREATE UNIQUE INDEX IF NOT EXISTS "assistant_update_proposals_one_live_per_component_idx"
    ON "public"."assistant_update_proposals"
       ("tenant_id", "site_id", "component_type", "component_slug")
    WHERE "state" = 'pending';

-- ===========================================================================
-- (3) ROW LEVEL SECURITY
-- ===========================================================================
--
-- ENABLE and FORCE both. FORCE matters because the table owner is the role the
-- migrations run as; without FORCE the owner bypasses every policy below and a
-- proof run as that role passes while proving nothing.

ALTER TABLE "public"."assistant_update_proposals" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "public"."assistant_update_proposals" FORCE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'assistant_update_proposals'
          AND policyname = 'assistant_update_proposals_tenant_isolation'
    ) THEN
        CREATE POLICY "assistant_update_proposals_tenant_isolation"
            ON "public"."assistant_update_proposals"
            USING ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK ("tenant_id" = nullif(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END;
$$;

-- The m19 site-scope predicate, byte for byte as m132 applied it to 22 tables.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'assistant_update_proposals'
          AND policyname = 'assistant_update_proposals_site_scope'
    ) THEN
        CREATE POLICY "assistant_update_proposals_site_scope"
            ON "public"."assistant_update_proposals"
            AS RESTRICTIVE FOR ALL
            USING (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            )
            WITH CHECK (
                coalesce(current_setting('app.site_scope', true), '') <> 'on'
                OR "site_id" = ANY (
                    string_to_array(
                        nullif(current_setting('app.allowed_site_ids', true), ''), ','
                    )::uuid[]
                )
            );
    END IF;
END;
$$;

-- The cross-tenant service context, set by pool.InAgentTx. This is what admits
-- the dispatch worker and the expiry sweep, exactly as m118's update_runs_agent
-- admits the update dispatcher. DECISION 5.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'assistant_update_proposals'
          AND policyname = 'assistant_update_proposals_agent'
    ) THEN
        CREATE POLICY "assistant_update_proposals_agent"
            ON "public"."assistant_update_proposals"
            USING (current_setting('app.agent', true) = 'on')
            WITH CHECK (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;

-- ===========================================================================
-- (4) PROVING IT
-- ===========================================================================
--
-- Every proof runs as the provisioned application role, NOT as superuser and
-- NOT as the table owner. The table is FORCE ROW LEVEL SECURITY, so a proof run
-- as the owner or as a BYPASSRLS role passes with the policies inert. Each
-- proof asserts current_user, rolsuper and rolbypassrls from INSIDE the
-- transaction under test.
--
-- The proofs that must exist, and each has a mutation behind it -- drop the
-- policy, watch the proof go red naming the leak, restore, watch it go green:
--
--   1. A foreign tenant sees nothing.        (tenant_isolation)
--   2. A site-scoped principal does not see a proposal for a site outside its
--      scope, and cannot INSERT one either.  (site_scope, USING and WITH CHECK)
--   3. An expired row cannot be approved.    (consent_within_window_check)
--   4. Approval cannot be attributed to a human on an expired row.
--                                            (expiry_is_not_a_decision_check)
--
-- The leak assertion is ordered FIRST in each proof. A leak check sitting
-- behind a row-count check is a leak check that never runs when the count is
-- what fails, which leaves the assertion itself unproven.

-- ===========================================================================
-- (5) THE FACTS ARE IMMUTABLE AFTER INSERT, BY COLUMN PRIVILEGE
-- ===========================================================================
--
-- WHY THIS EXISTS: AN EARLIER VERSION OF THIS FILE CLAIMED SOMETHING FALSE.
-- It said of "approve anyway" and "extend" that "both are absent from this
-- schema and neither can be added without a migration that has to argue for
-- it". A review disproved that by execution, and this section is the fix.
--
-- THE LESSON, WHICH GENERALISES BEYOND THIS TABLE: consent_within_window_check
-- compares two columns. A CHECK constraint sees only the finished row, so it
-- cannot tell that either column has just changed. The constraint was
-- therefore guarding a RELATION BETWEEN TWO MUTABLE COLUMNS while the comment
-- claimed it was guarding the passage of time. Any constraint whose meaning
-- depends on a column staying put needs that column held still by something
-- other than the constraint.
--
-- The gap is closed below. The detail of how it was reached is not written here
-- for the reason section (6) gives.
--
-- THE MECHANISM IS COLUMN PRIVILEGE, NOT A TRIGGER, AND IT HAS PRECEDENT HERE.
-- This tree contains ZERO `CREATE TRIGGER` (0 matches in db/schema.sql and 0
-- across apps/api/migrations). It does contain the exact device below:
-- m2's auth migration REVOKEs UPDATE/DELETE on audit_log so the trail is
-- append-only, and m122 does the same for org_context_versions and
-- site_context_versions. Revoking the table privilege and granting back only
-- the columns that may legitimately move is that same device at column
-- granularity, and it is enforced by PostgreSQL before any policy or
-- constraint is consulted.
--
-- WHAT BECOMES IMMUTABLE, AND WHY EACH ONE MATTERS:
--
--   expires_at, created_at    The window cannot be moved. "Extend" is now
--                             genuinely absent rather than merely
--                             unimplemented.
--   presented_digest          THE FINGERPRINT CANNOT BE REWRITTEN. This was a
--                             hole nobody had named: an UPDATE could re-point
--                             the digest at a freshly rendered screen, and the
--                             column whose entire purpose is to prove what was
--                             displayed would then prove whatever the last
--                             writer wanted. DECISION 6 claimed three
--                             properties for this column; without this REVOKE
--                             it reliably had none of them.
--   tenant_id, site_id,       The proposal cannot become a proposal about a
--   component_*, *_version,   different site, plugin or version after a human
--   proposed_by_grant_id      read it. Approving-after-the-facts-moved was
--                             supposed to be caught by the digest comparison;
--                             this makes the facts unable to move at all.
--
-- WHAT STAYS UPDATABLE, and it is the minimum the workflow needs: state,
-- decided_at, decided_by_user_id, dispatched_update_run_id, and note.
--
-- THE ORDER MATTERS. The REVOKE must precede the GRANT: a table-level UPDATE
-- privilege covers every column, and PostgreSQL will not carve a column out of
-- it -- `REVOKE UPDATE (col)` against a table-level grant reports that no
-- privileges could be revoked and changes nothing. Revoke the table privilege
-- first, then grant the column list back.
--
-- THE TEST HARNESS HAS TO KNOW. apps/api/tests/rls_integration_test.go runs a
-- blanket `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES` AFTER the
-- migrations, which re-adds BOTH the table-level UPDATE and the DELETE this
-- section removes -- the DELETE re-revoke is as necessary as the UPDATE one,
-- and a proof of undeletability without it passes against a privilege no real
-- install has. That
-- file already carries re-revokes for audit_log and the two context tables,
-- and its own comment says why: without them "every test in this package would
-- run against privileges no real install has -- including the append-only
-- proofs, which would then pass by testing nothing." This table now needs the
-- same treatment and it has been added there. A proof of immutability that
-- runs with the privilege restored is the exact vacuous shape that comment
-- describes.

-- THE ROW ITSELF HAS TO SURVIVE, OR NONE OF THE ABOVE MEANS ANYTHING.
--
-- Every precedent this section names -- audit_log in m2, org_context_versions
-- and site_context_versions in m122 -- revokes DELETE in the same breath as
-- UPDATE, and for one reason: a column made immutable inside a row that can be
-- deleted is not immutable, it is merely inconvenient. An attacker who cannot
-- rewrite presented_digest can drop the row that carries it and the approval
-- stops having happened. A review executed that DELETE as wpmgr_app against an
-- approved row and it succeeded; the observed relacl was `wpmgr_app=ard/wpmgr`,
-- and the `d` is DELETE.
--
-- This table is append-and-decide, exactly like the three above it. Nothing in
-- the workflow deletes a proposal: expiry writes 'expired', a refusal writes
-- 'rejected', and both are states rather than absences precisely so the record
-- of the ask survives the answer. TRUNCATE goes with it for the same reason
-- the precedents include it -- it is DELETE without a WHERE clause.
--
-- Rows still leave by CASCADE when their tenant or site is deleted. That is
-- the deliberate lifecycle boundary the FKs declare, and it is not reachable
-- by an UPDATE or a DELETE this role can issue.
REVOKE DELETE, TRUNCATE ON "public"."assistant_update_proposals" FROM "wpmgr_app";

REVOKE UPDATE ON "public"."assistant_update_proposals" FROM "wpmgr_app";

GRANT UPDATE ("state", "decided_at", "decided_by_user_id",
              "dispatched_update_run_id", "note")
    ON "public"."assistant_update_proposals" TO "wpmgr_app";

-- ===========================================================================
-- (6) WHAT REMAINS POSSIBLE, NAMED IN FULL RATHER THAN IMPLIED ABSENT
-- ===========================================================================
--
-- WHAT THIS TABLE GUARANTEES. Stated positively and completely, so a reader can
-- tell what they may rely on without inferring it from the constraints:
--
--   1. A MACHINE CAN NEVER APPEAR AS THE APPROVER. An approved row always names
--      a human, and a credential has no human to name. DECISION 8. This is the
--      property the whole design exists for and it holds absolutely.
--   2. THE WINDOW CANNOT BE MOVED after insert. Section (5).
--   3. THE FINGERPRINT CANNOT BE REWRITTEN after insert. Same mechanism, and it
--      is what makes DECISION 6's three properties real rather than intended.
--   4. THE FACTS CANNOT CHANGE UNDER A READER -- tenant, site, component,
--      versions and proposer are all immutable once written.
--   5. EXPIRY CAN NEVER NAME A HUMAN. DECISION 4.
--   6. TENANT AND SITE ISOLATION, each proven under mutation.
--   7. THE ROW CANNOT BE DELETED by the application role. Section (5) revokes
--      DELETE and TRUNCATE, as m2 does for audit_log and m122 for the two
--      context tables. Rows leave only by the tenant/site CASCADE.
--   8. NO DECISION IS ANONYMOUS. Approved, dispatched and rejected all name a
--      human; expired names nobody and may not name anyone. The approver of an
--      approved row cannot be erased by moving the row to 'rejected'.
--
-- THE GUARANTEES ARE PARTIAL, AND WHAT IS MISSING IS ONE THING WITH THREE
-- FACES. Every one of them is the same absence: THERE IS NO TRANSITION GUARD.
-- A CHECK sees the finished row and never the row it replaced, and a column
-- privilege can say whether a column may move but not in which direction. So
-- the PREVIOUS state of a row constrains NOTHING about its next state, and
-- these three consequences follow and have each been reached by execution as
-- wpmgr_app through the production dispatch:
--
--   a. STATES ARE NOT TERMINAL. A dispatched row can be moved back to
--      'pending'. Nothing marks a decided row as finished.
--   b. THE DECIDER CAN BE REPLACED, though no longer erased. Guarantee 8 above
--      makes an anonymous decision unrepresentable; it does not stop
--      decided_by_user_id being overwritten with a DIFFERENT user id.
--   c. THE WINDOW CHECK VERIFIES ARITHMETIC, NOT TIME. It compares decided_at
--      to expires_at and never to now(), which a CHECK cannot call. A caller
--      that supplies its own decided_at rather than letting the database
--      evaluate now() can therefore approve a proposal whose window has
--      closed. DECISION 4 and (7)(c) both carry this; it is repeated here
--      because this is the section a reader consults for what is NOT true.
--
-- These are stated because the alternative is worse. This file previously
-- claimed absolutes in this area that a review disproved by execution, and the
-- claims had been repeated onward as fact before that happened. A reader who is
-- told only "the guarantees are partial" builds on whichever absolute they
-- remember. Naming the three costs little: they are all one sentence -- the
-- previous state constrains nothing -- and that sentence was already in this
-- section, in the paragraph below, as the reason a trigger is what would close
-- it. What is NOT written here is the severity assessment and the option
-- analysis, which stay in the maintainers' private notes, because this
-- repository is public and a commit cannot be recalled.
--
-- WHY THE REMAINDER IS NOT SIMPLY CLOSED HERE: what would close it is a
-- transition guard, and that requires a trigger. This tree has ZERO
-- `CREATE TRIGGER` -- a CHECK constraint sees only the finished row and cannot
-- compare it to what came before, and a column privilege cannot express "this
-- column may change, but not in that direction". Introducing the first trigger
-- in the tree, on an authorisation path, inside the migration that also
-- introduces the table, was judged the wrong call. That is a decision, not an
-- oversight, and it is written down so the next person can overturn it
-- deliberately.
--
-- ===========================================================================
-- (7) WHAT THE GO LAYER MUST NOW DO -- NOT WORK THIS FILE PERFORMS
-- ===========================================================================
--
-- A note for backend-architect, here because whoever writes that code will be
-- reading this file.
--
--   a. THE DIGEST IS COMPUTED SERVER-SIDE over the control-plane-derived facts
--      the approval surface renders, and `note` is excluded from it. A digest
--      the proposer supplies fingerprints the proposer's claim about the screen.
--   b. THE APPROVE STATEMENT CARRIES THE DIGEST AND THE STATE:
--      `... SET state = 'approved_undispatched', decided_at = now(),
--       decided_by_user_id = $3
--        WHERE id = $1 AND presented_digest = $2 AND state = 'pending'`
--      Zero rows affected is the refusal, and it is the only refusal needed for
--      double-approval and for approval after the facts moved.
--   c. decided_at IS now(), ALWAYS, EVALUATED BY THE DATABASE. It is never
--      taken from a request body, never from a client clock, and never passed
--      in as a parameter. This is a hard requirement on the caller, not a
--      preference: the window constraint can only be as good as the value it
--      is given. DECISION 4.
--   d. THE DISPATCH WORKER TAKES THE SAME LOCK THE REST OF THE CODEBASE TAKES.
--      org.LifecycleLockKey via pg_advisory_xact_lock(hashtext($1), hashtext($2))
--      where a proposal's dispatch races an organisation delete or restore.
--   e. NOTHING WRITES 'approved_undispatched' EXCEPT A HUMAN DECISION PATH.
--      ADR-061: no auto-approve tier, no policy engine that can approve, no
--      "trusted automation may approve" flag, no approve button in a
--      notification email, no "remember this choice for this session".
--   f. THE EXPIRY SWEEP WRITES 'expired' AND NEVER decided_by_user_id.
