-- m118 - GH #463 Phase 0 of the deferred-dispatch feature. SCHEMA ONLY.
--
-- THIS MIGRATION DELIBERATELY CHANGES NO BEHAVIOUR. It adds one RLS policy,
-- one partial index and two column comments. No worker reads them, no route
-- writes them, and no existing row is touched. It ships alone, and first,
-- because the Go code of Phase 1 is unwritable-without-a-silent-bug until it
-- exists.
--
-- ---------------------------------------------------------------------------
-- WHY THIS CANNOT WAIT FOR THE CODE THAT NEEDS IT
-- ---------------------------------------------------------------------------
--
-- update_runs was created by m3 with exactly ONE policy,
-- update_runs_tenant_isolation, keyed on app.tenant_id. Every query written
-- against it in the three months since has run under InTenantTx, so one policy
-- has always been enough and nobody noticed the other one missing.
--
-- The #463 dispatcher is the FIRST cross-tenant reader of this table. It runs
-- on a tick under InAgentTx (internal/db/db.go InAgentTx: app.agent = 'on',
-- app.tenant_id UNSET) and asks "which runs, belonging to ANY tenant, are due
-- to dispatch?". Under FORCE ROW LEVEL SECURITY with only a tenant_isolation
-- policy present, that context satisfies no policy at all:
-- nullif(current_setting('app.tenant_id', true), '')::uuid is NULL, and
-- tenant_id = NULL is NULL, which is not TRUE, so no row is admitted.
--
-- The failure is SILENT. Postgres does not error; it returns the empty set.
-- The dispatcher would log a cheerful "0 due runs" on every tick, forever, and
-- the feature would look shipped and do nothing. That is not a hypothesis. This
-- repository has shipped this exact bug twice already, and both post-mortems
-- are written into the schema:
--
--   m84 / Issue #96  backup_schedules. The scheduler's claim returned zero rows
--                    on every tick, so no backup schedule ever advanced.
--   m89 / #131       update_tasks. THE SIBLING OF THIS VERY TABLE. The stale-task
--                    reaper's cross-tenant sweep returned zero rows on every
--                    sweep. m89 calls it "the same bug class as m84's
--                    backup_schedules fix".
--
-- This is the third table in the same family and the same defect. Adding the
-- policy before the dispatcher exists means Phase 1 is written against a
-- database that can actually answer it.
--
-- ---------------------------------------------------------------------------
-- DECISION 1: FOR ALL, not FOR SELECT. This is the whole lesson of Issue #96.
-- ---------------------------------------------------------------------------
--
-- The obvious policy is a read policy: the dispatcher is "a scan", so let it
-- SELECT. That is precisely the mistake m84 had to undo, and writing it here
-- would reproduce Issue #96 line for line.
--
-- Two independent reasons a FOR SELECT policy is wrong for this consumer:
--
--   1. The dispatcher WRITES. Its job is not to read due runs, it is to CLAIM
--      them: status 'scheduled' -> 'dispatching', so a second tick, a second
--      control-plane replica, or a restart mid-dispatch does not dispatch the
--      same run twice. A FOR SELECT policy admits the row to the SELECT and
--      admits NOTHING to the UPDATE, so the UPDATE matches zero rows. It does
--      not error. It reports "0 rows affected" and the run sits at 'scheduled'
--      forever, re-read and re-skipped on every tick.
--
--   2. Even a pure read would break, because the read will not be pure. The
--      claim is a locking read - SELECT ... FOR UPDATE, or UPDATE ... WHERE id
--      IN (SELECT ... FOR UPDATE SKIP LOCKED), which is the shape every claim
--      in this codebase uses. PostgreSQL applies BOTH the SELECT policy and the
--      UPDATE policy to a SELECT ... FOR UPDATE, because the row lock is a
--      declaration of intent to write. With only a FOR SELECT policy the UPDATE
--      USING is unsatisfied and the LOCKING SELECT ITSELF returns zero rows -
--      the counter-intuitive half of #96, where the plain SELECT worked in
--      psql and the real query returned nothing.
--
-- So: FOR ALL, with WITH CHECK mirroring USING so the claim's new row version
-- is admitted too. This matches backup_schedules_scheduler (m84, which spells
-- the same reasoning out at db/schema.sql), backup_schedule_runs_agent, and
-- update_tasks_agent (m89), which is FOR ALL by omission - CREATE POLICY
-- defaults to FOR ALL, and m89 relies on that default. This policy states FOR
-- ALL explicitly rather than relying on the default, because the default is the
-- thing a reviewer cannot see and this is the exact clause the bug turns on.
--
-- THE GUC IS COMPARED AGAINST THE LITERAL 'on'. Not 'true', not '1', not a
-- cast to boolean. InAgentTx executes set_config('app.agent', 'on', true) and
-- every sibling policy in this schema compares to 'on'. current_setting(...,
-- true) returns NULL when the GUC was never set, and NULL = 'on' is NULL, not
-- FALSE - which is why this policy admits nothing outside InAgentTx and is
-- therefore not a widening of any operator path. Permissive policies are OR'd,
-- so an operator request (app.tenant_id set, app.agent unset) is admitted by
-- update_runs_tenant_isolation exactly as before and by this policy never.
--
-- ---------------------------------------------------------------------------
-- DECISION 2: the due-scan index, and why its predicate is only the status
-- ---------------------------------------------------------------------------
--
-- The dispatcher's tick asks "WHERE status = 'scheduled' AND scheduled_at <=
-- now()", cross-tenant. The only index on this table today is
-- update_runs_tenant_id_created_at_idx (tenant_id, created_at DESC), whose
-- leading column the dispatcher does not filter on at all, so that query is a
-- sequential scan over every run this installation has ever created - a table
-- that only grows, scanned on every tick, to find the handful of rows that are
-- due. The index below is created now, with the policy, so the dispatcher never
-- ships against an unindexed whole-table predicate; migrate.go runs each
-- migration in one transaction and therefore cannot use CREATE INDEX
-- CONCURRENTLY, so taking the lock now - while 'scheduled' matches zero rows,
-- because no code writes that value yet - is far cheaper than taking it later.
--
-- It mirrors backup_schedules_due_idx (next_run_at) WHERE enabled: the ordering
-- column indexed, the "is this row even a candidate" test in the predicate. A
-- run leaves the index the instant the dispatcher claims it ('scheduled' ->
-- 'dispatching'), so the index stays proportional to the pending queue and not
-- to the run history, which is the entire point of the partial form.
--
-- The predicate is ONLY "status = 'scheduled'". It deliberately does NOT also
-- say "AND scheduled_at IS NOT NULL", even though scheduled_at is nullable and
-- a NULL-scheduled_at row can never satisfy "scheduled_at <= now()":
--
--   * The planner proves a partial index usable from the query's own WHERE
--     clauses. "status = 'scheduled'" appears verbatim in the dispatcher's
--     query, so this predicate is discharged directly and the index is usable
--     with no clause the consumer must remember to repeat. That is the failure
--     mode m117's sites_monitoring_resume_due_idx comment documents at length,
--     and the way to avoid it is to keep the predicate to clauses the query
--     already states for its own reasons.
--   * The rows it would exclude are rows where status = 'scheduled' and
--     scheduled_at IS NULL - a state that should not exist (a scheduled run
--     with no time to run at), and if it does exist, one we WANT visible in
--     this index so it can be found rather than silently unindexed.
--
-- ---------------------------------------------------------------------------
-- DECISION 3: the status comments are the contract, because nothing else is
-- ---------------------------------------------------------------------------
--
-- Neither update_runs.status nor update_tasks.status has a CHECK constraint;
-- both are plain text NOT NULL DEFAULT 'pending' (m3, lines 6 and 28). Verified
-- rather than assumed. So there is no DDL to change to admit a new value, and
-- consequently NOTHING in the database will reject a typo: a dispatcher that
-- writes 'sheduled' will store it, the partial index will not contain the row,
-- and the run will never dispatch, silently. The column comment is the only
-- contract that exists, which is why naming the values here - BEFORE any code
-- writes them - is a real deliverable and not documentation housekeeping.
--
-- COMMENT ON COLUMN is used here, rather than relying on db/schema.sql's inline
-- "--" comments alone, so the contract lives in the DATABASE, where \d+ shows
-- it to whoever is debugging at 3am, instead of only in a file they would have
-- to know to open. db/schema.sql carries the SAME contract text in the same
-- commit, but as inline "--" comments: that file contains no COMMENT ON
-- STATEMENT anywhere (verified at this commit:
-- grep -cE '^\s*COMMENT ON' db/schema.sql -> 0), it is sqlc's input rather than
-- a dump of the database, and it is already well behind the migrations on RLS.
-- Introducing a construct it has never used, to restate text it already
-- carries, would buy nothing. The policy and the index below ARE mirrored there
-- as real DDL.
--
-- THE ANCHOR IN THAT PATTERN IS LOAD-BEARING; do not "simplify" it away. The
-- unanchored grep -c 'COMMENT ON' db/schema.sql returns 2 at this commit, and
-- both hits are PROSE THIS COMMIT ADDED - the two inline comments in that file
-- that point back at the COMMENT ON COLUMN statements below. Neither is a
-- statement. Only the anchored form counts statements, which is the thing being
-- claimed here.
--
-- Recorded because it is this migration's own subject in miniature: the
-- unanchored claim was TRUE WHEN WRITTEN, and writing it down is what made it
-- false. A comment whose own quoted command disproves it misleads the next
-- reader in exactly the way the silent zero-rows scan above would have.
--
-- A NOTE FOR PHASE 1, stated here because this comment outlives the phase doc:
-- these strings are declared ahead of the code that writes them. Phase 1 must
-- use exactly these spellings. If Phase 1 concludes it needs a different value
-- or a different meaning, that is a NEW ordinal amending the comment - never an
-- edit to this file, which will already have applied.
--
-- ---------------------------------------------------------------------------
-- WHAT THIS MIGRATION DELIBERATELY DOES NOT ADD
-- ---------------------------------------------------------------------------
--
-- No update_runs_site_scope RESTRICTIVE policy. m19 gave update_tasks one and
-- gave update_runs none, and that asymmetry is correct rather than an m112-style
-- omission: the site-scope policy is "site_id = ANY (app.allowed_site_ids)",
-- and update_runs HAS NO site_id COLUMN. It is a tenant-level grouping row
-- (id, tenant_id, created_by, status, dry_run, scheduled_at); the site linkage
-- lives entirely on update_tasks, which is site-keyed and which m19 duly
-- scoped. Scoping update_runs would mean a correlated EXISTS over update_tasks
-- in a RESTRICTIVE policy on every read of the table - a behaviour change to
-- the operator and client-portal read paths, a new plan for every run query,
-- and squarely a security-reviewer decision. It is not a Phase 0 change and it
-- is not made silently: see the note handed back with this migration.
--
-- No GRANT. The grants on update_runs are table-level and already in place; a
-- policy is not a privilege and adding one grants nothing.
--
-- No CHECK constraint on status. Adding one would be a strictly larger and
-- riskier change than this phase needs - it would have to enumerate every value
-- Phase 1 through Phase N will ever write, and getting that list wrong turns a
-- typo into a 23514 outage inside main() rather than a caught bug. The comment
-- is the contract here, consistent with audit_log.action and every other status
-- column in this schema.
--
-- ---------------------------------------------------------------------------
-- IDEMPOTENCE AND CONVERGE PATH
-- ---------------------------------------------------------------------------
--
-- Every statement is a no-op on second application: the policy is guarded by a
-- pg_policies existence check (the m89 pattern for this exact policy on the
-- sibling table), the index by a pg_indexes check (the m116/m117 house
-- pattern), and COMMENT ON COLUMN is idempotent by nature. migrate.go applies
-- this on boot inside main(), in one transaction, so a failure here is a
-- control-plane outage on every install at once; nothing below can fail on a
-- database that has already run it.
--
-- CONVERGE PATH: none is required, and this is not a formality. No prior
-- version of this migration has ever been applied to any database. This is a
-- new ordinal - 20260820000000, after m117's 20260819000000, which is the
-- highest ordinal reachable from ANY ref in this repository at the time of
-- writing and not merely the highest on main. It corrects nothing, it edits no
-- applied file, and it changes no existing row, so a database at m117 and a
-- database created fresh from this migration set reach the same end state.

-- ---------------------------------------------------------------------------
-- The agent policy. FOR ALL - see DECISION 1.
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public'
          AND tablename  = 'update_runs'
          AND policyname = 'update_runs_agent'
    ) THEN
        CREATE POLICY "update_runs_agent" ON "public"."update_runs"
            FOR ALL
            USING (current_setting('app.agent', true) = 'on')
            WITH CHECK (current_setting('app.agent', true) = 'on');
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- The due-scan index - see DECISION 2. Mirrors backup_schedules_due_idx.
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE schemaname = 'public' AND tablename = 'update_runs'
          AND indexname = 'update_runs_due_idx'
    ) THEN
        CREATE INDEX "update_runs_due_idx"
            ON "public"."update_runs" ("scheduled_at")
            WHERE "status" = 'scheduled';
    END IF;
END;
$$;

-- ---------------------------------------------------------------------------
-- The status contracts - see DECISION 3. Comment-only; no constraint exists on
-- either column, so this is the only statement of what the values mean.
-- ---------------------------------------------------------------------------

COMMENT ON COLUMN "public"."update_runs"."status" IS
$c$Run lifecycle. No CHECK constraint exists; this comment is the contract.

  pending      Created and its tasks enqueued for immediate execution. The m3
               default, and still the only state an immediate run passes
               through.
  scheduled    (#463) Created with a future scheduled_at and NOT yet handed to
               the worker. The dispatcher's due-scan selects exactly these,
               and update_runs_due_idx is partial on this value.
  dispatching  (#463) Claimed by the dispatcher for this tick. The row has left
               update_runs_due_idx, so a concurrent tick, a second replica or a
               restart mid-dispatch cannot claim it again. Transient: the same
               transaction that sets it enqueues the work.
  running      At least one task is running.
  completed    Every task reached a terminal state.
  expired      (#463) The run passed its dispatch window without being
               dispatched - the control plane was down across scheduled_at, or
               the run sat past the point where executing it is still what the
               operator asked for. Terminal, and NEVER retried: a deferred bulk
               update that fires days late is a surprise, not a service.
               Distinct from 'completed' with failures, which was attempted.

Cross-tenant readers of this column run under InAgentTx and are admitted by
update_runs_agent (m118), not by update_runs_tenant_isolation.$c$;

COMMENT ON COLUMN "public"."update_tasks"."status" IS
$c$Per-task lifecycle. No CHECK constraint exists; this comment is the contract.

  pending      Created, awaiting execution.
  running      In flight on the agent.
  succeeded    Applied.
  failed       Attempted and failed.
  rolled_back  Attempted, failed, and reverted from the snapshot.
  skipped      Not attempted, by decision at plan time.
  scheduled    (#463) Belongs to a run that is 'scheduled' and is not yet
               eligible for execution. NOTE: 'scheduled' is NOT one of the
               statuses in update_tasks_inflight_target_idx, whose predicate is
               status IN ('pending','running'). That index is the authoritative
               cross-run dedup guard (m88), so a scheduled task does NOT reserve
               its (tenant, site, target) pair against a concurrent immediate
               run. See the note handed to backend-architect with this
               migration: Phase 1 must decide deliberately whether scheduled
               tasks reserve their target, and widening that unique index is a
               separate migration with a data-dedup step, not a comment.
  expired      (#463) The parent run expired without dispatching, so this task
               was never attempted. Terminal.

The RESTRICTIVE update_tasks_site_scope policy (m19) and the cross-tenant
update_tasks_agent policy (m89) both apply to this table; update_runs carries
the agent policy from m118 but no site-scope policy, because it has no
site_id.$c$;
