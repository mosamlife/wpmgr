// Pure helpers extracted from update-status.tsx and update-tasks-table.tsx to
// clear the react-refresh boundary: files that export both components AND
// non-component values trip the react-refresh/only-export-components rule.
// Surface agents import from here; the originals will remove their copies.

import type { Site, UpdateRun, UpdateTask } from "@wpmgr/api";

type TaskStatus = UpdateTask["status"];
type RunStatus = UpdateRun["status"];

/** Count task statuses and derive a 0..total "settled" progress figure. */
export function summarizeTasks(tasks: UpdateTask[]): {
  total: number;
  done: number;
  counts: Record<TaskStatus, number>;
} {
  const counts: Record<TaskStatus, number> = {
    pending: 0,
    running: 0,
    succeeded: 0,
    failed: 0,
    rolled_back: 0,
    skipped: 0,
    // GH #255 Phase 2: a task the wave gate never dispatched because its run
    // halted first. Distinct from `skipped` (the control plane declined this
    // one target) and `failed` (the site was contacted and did not come
    // back): nothing was ever sent here.
    cancelled: 0,
    // GH #463: not yet eligible for dispatch (parent run hasn't reached its
    // scheduled_at). Not terminal, so excluded from `done` below, matching
    // the control plane's own `terminal()` predicate.
    scheduled: 0,
    // GH #463: the parent run expired without dispatching. Terminal, and
    // counted into `done` below, same as `cancelled`.
    expired: 0,
  };
  for (const task of tasks) counts[task.status] += 1;
  const done =
    counts.succeeded +
    counts.failed +
    counts.rolled_back +
    counts.skipped +
    counts.cancelled +
    counts.expired;
  return { total: tasks.length, done, counts };
}

/**
 * Terminal task states, matching the control plane's own `terminal()`
 * predicate (apps/api/internal/update/model.go). A task in one of these
 * states has a final outcome and carries the server's retry classification.
 */
export function isTerminalTaskStatus(status: TaskStatus): boolean {
  return (
    status === "succeeded" ||
    status === "failed" ||
    status === "rolled_back" ||
    status === "skipped" ||
    status === "cancelled" ||
    // GH #463: the parent run expired without dispatching this task. Never
    // attempted, and final — matches the control plane's own terminal()
    // predicate (apps/api/internal/update/model.go), which includes
    // TaskExpired but deliberately excludes TaskScheduled.
    status === "expired"
  );
}

/**
 * Terminal run states. `halted` (GH #255 Phase 2) is specific to the agent
 * self-update channel: a staged-rollout wave failed to prove itself, so the
 * run stopped early and every task not already dispatched was cancelled.
 * Every surface that gates "is this run still going" (SSE close, poll
 * interval, the bulk-action-drawer's settled flag, the run detail page's
 * live indicator) must treat it as terminal exactly like `completed`, or a
 * halted run reads as perpetually in progress.
 */
export function isTerminalRunStatus(status: RunStatus | undefined): boolean {
  // GH #463: `expired` is terminal too — the run came due more than the
  // grace window ago and was never dispatched, and nothing further happens
  // to it. `scheduled` and `dispatching` are deliberately excluded: both
  // still have somewhere to go (scheduled -> dispatching -> running/expired;
  // dispatching -> running), so a surface gating "still going" must keep
  // polling/streaming through them.
  return status === "completed" || status === "halted" || status === "expired";
}

// GH #255 Phase 2: the agent self-update channel's own outcome vocabulary.
//
// `planAgentTasks` (apps/api/internal/update/service.go) already excludes a
// site the control plane's own inventory heuristic classifies as the
// plugin-directory build before a task is ever created for it, but the AGENT
// is the final authority on its own distribution: a task can still come back
// `skipped` this way when the two disagree (e.g. a renamed plugin folder the
// inventory heuristic could not identify). This is informational, never an
// error: the site updates through its own channel, nothing went wrong, so
// it must read distinctly from an ordinary skip.
const AGENT_NOT_ELIGIBLE_PATTERN = /no self-updater|outside this channel/i;

/** True for an agent-target task the agent itself declined as not able to
 * self-update, as opposed to any other reason a task might be skipped. */
export function isAgentNotEligible(
  targetType: string,
  status: string,
  detail?: string,
): boolean {
  if (targetType !== "agent" || status !== "skipped") return false;
  return AGENT_NOT_ELIGIBLE_PATTERN.test(detail ?? "");
}

/**
 * Human-readable explanation for a halted agent self-update run, taken from
 * the backend's own wording rather than re-derived client-side. Every task
 * the wave gate cancels carries `detail = "cancelled: " + reason` (see
 * apps/api/internal/update/agent_repo.go haltLocked), so any cancelled task
 * in the run carries the same reason. A run can halt with zero cancelled
 * tasks (e.g. a single-site canary already terminal when the gate re-judged
 * it, which is exactly what happens when its one task comes back `skipped`
 * rather than `cancelled`: haltLocked only cancels tasks still `pending`, and
 * `haltReasonFor` (apps/api/internal/update/agent_wave.go) is the backend's
 * own reason string for that shape of halt, but it is only ever computed and
 * logged/audited, never persisted onto the run row itself, so there is
 * nothing for this client to read back verbatim here). In that case fall
 * back to a summary built from the run's own task counts, mirroring the
 * backend's own wave tally (`tallyWave`/`haltReasonFor`) rather than
 * re-deriving different arithmetic client-side:
 *
 *   - `contacted` is `succeeded + failed + rolled_back + skipped`. A skipped
 *     task WAS contacted and answered (an old agent with no self-update
 *     route, a build the channel does not apply to, an "up to date" answer
 *     that did not match this run's premise), so it is not a site nobody
 *     heard from and it must never be folded into "nobody was contacted".
 *     Only `cancelled` means nothing was ever sent (see summarizeTasks
 *     above).
 *   - `failed` is `failed + rolled_back`, matching the backend's own
 *     tallyWave grouping.
 *
 * Returns null for a run that has not halted.
 */
export function haltReason(
  run: Pick<UpdateRun, "status" | "tasks"> | undefined,
): string | null {
  if (!run || run.status !== "halted") return null;
  const cancelledDetail = run.tasks?.find(
    (t) => t.status === "cancelled" && t.detail,
  )?.detail;
  if (cancelledDetail) {
    return cancelledDetail.startsWith("cancelled: ")
      ? cancelledDetail.slice("cancelled: ".length)
      : cancelledDetail;
  }
  const { counts } = summarizeTasks(run.tasks ?? []);
  const failed = counts.failed + counts.rolled_back;
  const contacted = counts.succeeded + failed + counts.skipped;
  if (contacted === 0) {
    return "The rollout was halted before any site could be contacted.";
  }
  if (counts.succeeded === 0) {
    return `The rollout was halted because no site confirmed the upgrade (${failed} failed, ${counts.skipped} skipped, of ${contacted} contacted).`;
  }
  return `The rollout was halted after ${failed} of ${contacted} contacted site${contacted === 1 ? "" : "s"} failed to confirm the upgrade.`;
}

/** Build a site id -> name lookup from the sites list cache. */
export function siteNameMap(sites: Site[] | undefined): Map<string, string> {
  const map = new Map<string, string>();
  for (const site of sites ?? []) map.set(site.id, site.name);
  return map;
}

// GH #210: the worst-case rollback failure. An update causes a site-wide
// PHP fatal, so the rollback command is undeliverable (it rides the same
// WordPress request that is fataling), and an agent-side watchdog attempts
// automatic filesystem-level recovery. The backend keeps the existing task
// status enum (failed/rolled_back) and communicates this condition purely
// through the detail/error text, so callers key off content rather than a
// new status value. Every surface that renders a task's outcome should use
// this helper rather than re-deriving the pattern locally, so the condition
// reads consistently (and never as a generic "rollback failed") everywhere.
//
// DISPLAY ONLY, AND DELIBERATELY SO (GH #336). This and `isAgentNotEligible`
// match prose the AGENT frequently authored, so they pick a badge label and
// nothing else. No safety or selection decision may read them: whether a task
// can be retried is the server's `retryable`/`retry_class` fields, which the
// control plane computes from the machine values it already holds when it
// writes the row (see features/updates/retry-contract.ts).
const SITE_DOWN_RECOVERY_STATUSES = new Set(["failed", "rolled_back"]);
const SITE_DOWN_RECOVERY_PATTERN =
  /site[- ]wide|site is down|not responding|undeliverable|filesystem recovery|automatic recovery|watchdog/i;

/**
 * True when a terminal (failed/rolled_back) task's detail/error text
 * describes the site-wide-fatal + undeliverable-rollback + auto-filesystem-
 * recovery condition, as opposed to an ordinary update failure or rollback.
 */
export function isSiteDownRecovery(
  status: string,
  detail?: string,
  error?: string,
): boolean {
  if (!SITE_DOWN_RECOVERY_STATUSES.has(status)) return false;
  return SITE_DOWN_RECOVERY_PATTERN.test(`${detail ?? ""} ${error ?? ""}`);
}

/** Distinct, actionable label for the site-down-recovery condition. Never
 * collapse this into the generic "Rolled back"/"Failed" copy. */
export const SITE_DOWN_RECOVERY_LABEL = "Site down, recovery attempted";

/** Fallback body copy when the backend detail is empty but the condition is
 * detected from `error` alone. */
export const SITE_DOWN_RECOVERY_FALLBACK_DETAIL =
  "The site went down site-wide during this update. Automatic filesystem recovery was attempted; manual filesystem recovery may be required.";
