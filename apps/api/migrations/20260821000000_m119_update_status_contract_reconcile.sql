-- m119 - GH #482. Reconciles the status contracts on update_runs and
-- update_tasks against the code that writes them. COMMENT-ONLY. No table, no
-- policy, no index, no constraint, no row touched.
--
-- ---------------------------------------------------------------------------
-- WHY THIS IS A NEW ORDINAL AND NOT AN EDIT TO m118
-- ---------------------------------------------------------------------------
--
-- m118 (20260820000000) installed the COMMENT ON COLUMN statements this file
-- amends, and m118 has applied. internal/db/migrate.go sorts the embedded
-- versions lexically, records each in schema_migrations, and skips anything
-- already present, so a database that has run m118 will never read that file
-- again however it is edited. Editing it would be a silent no-op that looked
-- like a fix. m118's own closing note anticipated exactly this case and said so:
-- "If Phase 1 concludes it needs a different value or a different meaning, that
-- is a NEW ordinal amending the comment - never an edit to this file."
--
-- This is that new ordinal. 20260821000000 is the next free one:
--
--   git log --all --name-only --format= -- apps/api/migrations/ \
--     | grep -oE '2026[0-9]{10}_m[0-9]+' | sort -u | tail -4
--   -> 20260817000000_m115
--      20260818000000_m116
--      20260819000000_m117
--      20260820000000_m118
--
-- That is every commit reachable from every ref, not merely main, because the
-- filename ordinal is APPLY order and not commit order - m113 was committed
-- after m114 and applies before it.
--
-- ---------------------------------------------------------------------------
-- WHAT WAS WRONG: two values the code writes, declared nowhere
-- ---------------------------------------------------------------------------
--
-- Neither column has a CHECK constraint (m3 created both as plain
-- text NOT NULL DEFAULT 'pending'), so the column comment is the entire
-- contract - m118's DECISION 3 says so in as many words. A value missing from
-- it is not a documentation gap, it is the absence of the only definition that
-- exists.
--
-- 'halted' has been written to update_runs.status since the agent self-update
-- wave machine shipped, long before #463, and appeared in no migration at all:
--
--   grep -ln halted apps/api/migrations/*.sql
--   -> (no output)   [control: grep -lc completed apps/api/migrations/*.sql
--                     matches real files, so the empty result is a real
--                     absence and not a broken pattern]
--
-- #463 then gave it a SECOND writer. It is now reached two ways, by two
-- different subsystems, and was declared by neither:
--
--   internal/update/agent_repo.go  haltLocked()          a wave gate refused to
--                                                        advance a rollout
--   internal/update/cancel_repo.go CancelScheduledRun()  an operator cancelled a
--                                                        scheduled run before it
--                                                        fired
--
-- 'cancelled' on update_tasks.status is the same defect on the sibling column,
-- found by the same sweep and fixed in the same migration rather than left for
-- a third ordinal.
--
-- ---------------------------------------------------------------------------
-- THE RECONCILIATION, IN FULL
-- ---------------------------------------------------------------------------
--
-- The whole list is compared here rather than the one reported omission
-- patched, because a contract that was short by one value for months is not
-- evidence of one mistake, it is evidence that nothing compares the two sides.
-- Both sets are extracted mechanically and diffed; the commands are in the PR
-- and the results are these.
--
--   update_runs.status
--     code writes (internal/update/model.go, "Run statuses" const block):
--       completed  dispatching  expired  halted  pending  running  scheduled
--     m118 declared:
--       completed  dispatching  expired          pending  running  scheduled
--     missing from the contract: halted
--
--   update_tasks.status
--     code writes (internal/update/model.go, "Task statuses" const block):
--       cancelled  expired  failed  pending  rolled_back  running  scheduled
--       skipped    succeeded
--     m118 declared:
--                  expired  failed  pending  rolled_back  running  scheduled
--       skipped    succeeded
--     missing from the contract: cancelled
--
-- After this migration both lists are equal, in both directions: every value
-- the code can write is declared, and no value is declared that the code cannot
-- write. The second direction matters as much as the first - a contract listing
-- a value nothing writes sends the next reader looking for a state machine that
-- does not exist.
--
-- ---------------------------------------------------------------------------
-- WHAT THIS MIGRATION DELIBERATELY DOES NOT ADD
-- ---------------------------------------------------------------------------
--
-- No CHECK constraint on either column. It is the obvious response to a
-- contract that drifted unnoticed for months, and it is deliberately NOT taken
-- here: it is a behaviour change on a live table, it converts a bad write from
-- a degraded row into a 23514 error inside a worker, and it takes an ACCESS
-- EXCLUSIVE lock during a boot-time migration in main(). It deserves its own
-- decision, its own ordinal and its own security review, and it is written up
-- for that decision in the PR rather than smuggled in behind a comment change.
-- Adding it in the same migration that fixes the list would also mean the first
-- constraint this schema has ever had on these columns is written by the person
-- who has just demonstrated the list was wrong.
--
-- No change to update_tasks_inflight_target_idx or to any other predicate.
-- 'halted' and 'cancelled' are both terminal and neither appears in any partial
-- index predicate or reaper sweep, so declaring them changes no plan and no
-- query. Verified rather than assumed: the only status predicates on these
-- tables are update_runs_due_idx (status = 'scheduled') and
-- update_tasks_inflight_target_idx (status IN ('pending','running')).
--
-- ---------------------------------------------------------------------------
-- IDEMPOTENCE AND CONVERGE PATH
-- ---------------------------------------------------------------------------
--
-- COMMENT ON COLUMN is idempotent by construction and needs no existence guard:
-- it does not append to or merge with the existing comment, it REPLACES it
-- wholesale. Applying this file twice sets the identical text twice; the second
-- application changes nothing observable. That is also why it needs no DO block
-- of the kind m118's policy and index needed - there is no "already exists"
-- state to skip, only a value to set.
--
-- CONVERGE PATH: none is required, and this is not a formality. This migration
-- corrects a comment, not data and not structure, and a comment is
-- last-write-wins with no history. A database that applied m118 and now applies
-- m119 ends with exactly the comment text below; a database created fresh from
-- the whole migration set applies m118's text and then overwrites it with the
-- same. Both reach an identical end state, and no row, index, policy or
-- constraint differs between them. There is no population of databases carrying
-- a wrong VALUE that needs converging, because nothing here ever wrote a value
-- based on the incomplete list - the list was only ever read by humans.
--
-- migrate.go applies this on boot inside main(), in one transaction. Neither
-- statement below can fail on a database that has already run it.

COMMENT ON COLUMN "public"."update_runs"."status" IS
$c$Run lifecycle. No CHECK constraint exists; this comment is the contract.
Reconciled against internal/update/model.go by m119 (#482).

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
  halted       (m119/#482 - written since the agent self-update wave machine
               shipped, and declared by no migration until m119.) Terminal. The
               run was STOPPED rather than finished, and is deliberately not
               spelled 'completed', which would erase that fact. Reached two
               ways, by two subsystems:
                 - a wave gate refused to advance an agent self-update rollout
                   (update/agent_repo.go haltLocked). Tasks underneath are a
                   MIXTURE of real outcomes: those already dispatched run to
                   their own conclusion and are never overwritten, only the
                   still-'pending' ones become 'cancelled'.
                 - an operator cancelled a scheduled run before it fired
                   (update/cancel_repo.go CancelScheduledRun, #463). Tasks
                   underneath are UNIFORMLY 'cancelled' and nothing was ever
                   sent to any site.
               The run vocabulary has no separate 'cancelled': cancel_repo.go
               reuses this value on purpose rather than minting a status no
               existing reader can render, and the task statuses underneath are
               what distinguish the two cases.
  expired      (#463) The run passed its dispatch window without being
               dispatched - the control plane was down across scheduled_at, or
               the run sat past the point where executing it is still what the
               operator asked for. Terminal, and NEVER retried: a deferred bulk
               update that fires days late is a surprise, not a service.
               Distinct from 'completed' with failures, which was attempted, and
               from 'halted', which was stopped by a gate or a human.

Cross-tenant readers of this column run under InAgentTx and are admitted by
update_runs_agent (m118), not by update_runs_tenant_isolation.$c$;

COMMENT ON COLUMN "public"."update_tasks"."status" IS
$c$Per-task lifecycle. No CHECK constraint exists; this comment is the contract.
Reconciled against internal/update/model.go by m119 (#482).

  pending      Created, awaiting execution.
  running      In flight on the agent.
  succeeded    Applied.
  failed       Attempted and failed.
  rolled_back  Attempted, failed, and reverted from the snapshot.
  skipped      Not attempted, by decision at plan time - the control plane
               declined this particular target.
  cancelled    (m119/#482 - written since the wave machine shipped, and declared
               by no migration until m119.) Terminal. NOTHING WAS EVER SENT TO
               THIS SITE, and a human or a gate decided that. Written by
               update/agent_repo.go haltLocked (only over tasks still 'pending';
               a 'running' task is left alone, because its command is already
               delivered and marking it cancelled would both record a falsehood
               and stop the confirm poll that is the control plane's only way to
               learn whether the site upgraded or bricked) and by
               update/cancel_repo.go CancelScheduledRun (#463, over the
               'scheduled' tasks of a run an operator cancelled).
               Distinct from 'skipped', where the control plane declined the
               target rather than a human stopping the run; from 'failed', where
               the site WAS contacted; and from 'expired', below.
  scheduled    (#463) Belongs to a run that is 'scheduled' and is not yet
               eligible for execution. NOTE: 'scheduled' is NOT one of the
               statuses in update_tasks_inflight_target_idx, whose predicate is
               status IN ('pending','running'). That index is the authoritative
               cross-run dedup guard (m88), so a scheduled task does NOT reserve
               its (tenant, site, target) pair against a concurrent immediate
               run.
  expired      (#463) The parent run expired without dispatching, so this task
               was never attempted. Terminal. NOT a spelling of 'cancelled':
               'cancelled' records a decision somebody made, 'expired' records
               that the window closed while the control plane was unavailable.

The RESTRICTIVE update_tasks_site_scope policy (m19) and the cross-tenant
update_tasks_agent policy (m89) both apply to this table; update_runs carries
the agent policy from m118 but no site-scope policy, because it has no
site_id.$c$;
