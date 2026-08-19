import type {
  UpdateRun,
  UpdateRunRetryExclusion,
  UpdateRunRetryResult,
  UpdateTask,
} from "@wpmgr/api";

import { isTerminalRunStatus, summarizeTasks } from "./summarize";

// GH #336 retry, client side of the contract.
//
// THE SERVER DECIDES, THE CLIENT RENDERS. Every safety decision on this
// surface (may this task be retried at all, is it on by default) reads a
// SERVER field. Nothing here inspects `detail` or `error` prose, and nothing
// here reaches for the sites cache: a run's task list is unpaginated while
// `useSites()` is capped by the control plane's default page size, so site
// identity for display AND selection comes from the TASK ROW (`site_name`,
// `site_id`) and never from a paginated list query.
//
// The contract, from packages/openapi/openapi.yaml:
//
//   GET /api/v1/updates/{runId}
//     UpdateTask.retryable    bool      REQUIRED
//     UpdateTask.retry_class  enum      REQUIRED
//     UpdateTask.site_name    string    optional (empty only if the site row
//                                       could not be read)
//
//   POST /api/v1/updates/runs/{id}/retry
//     request   UpdateRunRetryRequest  { task_ids }
//     200       UpdateRunRetryResult   { run_id?, requested, created,
//                                        excluded[], warning? }
//     where created + excluded.length === requested, always.
//
// The two decision fields are required by the spec, but a self-hosted control
// plane can be older than the browser bundle it serves, so their presence is
// still checked at runtime: an older control plane gets NO retry affordance
// rather than one driven by a client-invented policy. See
// `runCarriesRetryContract`.

/** Every retry class the control plane may put on a task. */
export const RETRY_CLASSES = [
  // Terminal, but nothing was ever sent to the site: the run stopped first.
  "never_ran",
  // The site was contacted and the update did not succeed.
  "failed",
  // Applied, then reverted automatically after a failed health check.
  "reverted",
  // The control plane or the agent declined this target.
  "skipped",
  // Succeeded, or not finished yet. Never selectable.
  "not_applicable",
] as const;

export type RetryClass = (typeof RETRY_CLASSES)[number];

/**
 * A task as this surface reads it. Identical to the generated `UpdateTask`;
 * the alias exists so the retry code reads as one vocabulary and so the
 * runtime guards below have a name to return.
 */
export type RetryTask = UpdateTask;

export type RetryResult = UpdateRunRetryResult;
export type RetryExclusion = UpdateRunRetryExclusion;

function isRetryClass(value: unknown): value is RetryClass {
  return (
    typeof value === "string" &&
    (RETRY_CLASSES as readonly string[]).includes(value)
  );
}

/**
 * Full structural check of ONE wire task against every field the spec marks
 * required, including the GH #336 decision pair.
 *
 * Used by the tests as the gate that a fixture is a shape a server actually
 * produces. GH #322 shipped a feature that rendered nothing while its tests
 * passed against hand-built fixtures; a fixture that does not survive this
 * guard is not evidence of anything.
 */
export function isRetryWireTask(value: unknown): value is RetryTask {
  if (typeof value !== "object" || value === null) return false;
  for (const key of [
    "id",
    "run_id",
    "tenant_id",
    "site_id",
    "target_type",
    "target_slug",
    "status",
    "created_at",
    "updated_at",
  ]) {
    if (!(key in value)) return false;
    const field: unknown = (value as Record<string, unknown>)[key];
    if (typeof field !== "string") return false;
  }
  if (!("retryable" in value) || typeof value.retryable !== "boolean") {
    return false;
  }
  if (!("retry_class" in value) || !isRetryClass(value.retry_class)) {
    return false;
  }
  // site_name is optional by spec (empty only when the site row could not be
  // read), so it is checked for TYPE when present and never for presence.
  if ("site_name" in value && typeof value.site_name !== "string") return false;
  return true;
}

/** Cheap runtime check of the two decision fields on an already-typed task. */
export function hasServerRetryFields(task: RetryTask): boolean {
  return (
    typeof task.retryable === "boolean" && isRetryClass(task.retry_class)
  );
}

/**
 * True when EVERY task in the run carries the server's retry decision. False
 * turns the whole retry surface off: no checkboxes, no bar, no button.
 */
export function runCarriesRetryContract(tasks: readonly RetryTask[]): boolean {
  if (tasks.length === 0) return false;
  return tasks.every((task) => hasServerRetryFields(task));
}

/** Server verdict, verbatim. The client never widens or second-guesses it. */
export function isRetrySelectable(task: RetryTask): boolean {
  return task.retryable === true;
}

/**
 * Default selection: `failed` and `never_ran` only.
 *
 * `never_ran` (the control plane's `cancelled`) means the task was withheld
 * when its run stopped, so nothing was ever sent to the site: it is a first
 * attempt, not a retry, and it is the safest thing in the run. `reverted` and
 * `skipped` are selectable but never pre-selected, and `not_applicable` is
 * neither.
 */
export function isDefaultRetrySelected(task: RetryTask): boolean {
  if (!isRetrySelectable(task)) return false;
  return task.retry_class === "failed" || task.retry_class === "never_ran";
}

const RETRY_CLASS_LABEL: Record<RetryClass, string> = {
  // Nobody cancelled these: the system withheld them when the run stopped.
  never_ran: "not attempted",
  failed: "failed",
  reverted: "rolled back",
  skipped: "skipped",
  not_applicable: "not applicable",
};

/** Lower-case class label for inline prose ("20 not attempted, 1 failed"). */
export function retryClassLabel(cls: RetryClass | undefined): string {
  if (cls === undefined) return "unclassified";
  return RETRY_CLASS_LABEL[cls];
}

/**
 * Why a row has no checkbox. Rendered for assistive technology in the select
 * cell; the visible cause is already in the Status column, and a disabled
 * control with no stated cause is worse than no control.
 */
export function notRetryableReason(task: RetryTask): string {
  if (task.status === "succeeded") {
    return "Cannot be retried: this update succeeded.";
  }
  // GH #463: `scheduled` joins this bucket — it has not started either, it
  // just has not reached its start time yet. Same reason a checkbox is
  // absent as pending/running: there is nothing to retry, only something to
  // wait for (or cancel).
  if (
    task.status === "pending" ||
    task.status === "running" ||
    task.status === "scheduled"
  ) {
    return "Cannot be retried: this update has not finished yet.";
  }
  // GH #463: `expired` is its own case, not the generic terminal fallback
  // below. Nothing was ever sent to the site (same as `cancelled`/
  // `never_ran`), so this text must not read like a failed update.
  //
  // The control plane classifies an expired task as retryable/`never_ran`
  // (apps/api/internal/update/model.go:405, `case TaskCancelled, TaskExpired:
  // return true, RetryClassNeverRan`), so in practice an expired row DOES get
  // a checkbox and this string is unreachable for it. It stays because this
  // function is keyed on `status` while selectability is keyed on the
  // server's `retry_class`, and a control plane that withheld the class would
  // otherwise fall through to the generic "cannot run this update again"
  // below, which is the wrong story. This client never overrides the server
  // verdict — see `isRetrySelectable` above — it only has to describe
  // whatever reason applies accurately.
  if (task.status === "expired") {
    return "Cannot be retried: this update expired before it could run.";
  }
  return "Cannot be retried: the control plane cannot run this update again.";
}

/**
 * Display name for a task's site, from the TASK ROW. `siteNames` is a legacy
 * fallback for a control plane that does not send `site_name` yet; it is
 * built from the capped sites list and must never drive selection.
 */
export function taskSiteLabel(
  task: RetryTask,
  siteNames?: Map<string, string>,
): string {
  const fromRow = task.site_name?.trim();
  if (fromRow) return fromRow;
  const fromCache = siteNames?.get(task.site_id);
  if (fromCache) return fromCache;
  return task.site_id.length > 8
    ? `${task.site_id.slice(0, 8)}...`
    : task.site_id;
}

/** "core", "agent", or the slug for a plugin/theme task. */
export function taskTargetLabel(task: RetryTask): string {
  if (task.target_type === "core") return "core";
  if (task.target_type === "agent") return "agent";
  return task.target_slug || task.target_type;
}

/** Row checkbox label. A row is a (site, target) pair, so both are named. */
export function taskSelectLabel(
  task: RetryTask,
  siteNames?: Map<string, string>,
): string {
  return `Select ${taskTargetLabel(task)} update on ${taskSiteLabel(task, siteNames)}`;
}

type TargetType = UpdateTask["target_type"];

/**
 * The single target type shared by every task in the set, or null when the
 * set is mixed. Only ever an adjective in a label ("21 agent updates"), never
 * a change of unit.
 */
export function sharedTargetType(
  tasks: readonly RetryTask[],
): TargetType | null {
  const first = tasks[0];
  if (!first) return null;
  return tasks.every((t) => t.target_type === first.target_type)
    ? first.target_type
    : null;
}

/**
 * THE UNIT IS TASKS, LABELLED AS UPDATES. A 20 site x 5 plugin run has 100
 * failed tasks across 20 sites and selection is over tasks, so counting sites
 * would lie. Every label, aria-label and dialog string on this surface reads
 * its count from here, so there is exactly one rule and no branch.
 */
export function retryActionLabel(options: {
  count: number;
  target?: TargetType | null;
  dryRun?: boolean;
}): string {
  const { count, target, dryRun = false } = options;
  const suffix = dryRun ? " (dry run)" : "";
  if (count === 0) return `Retry updates${suffix}`;
  const adjective = target ? `${target} ` : "";
  const noun = count === 1 ? "update" : "updates";
  return `Retry ${count} ${adjective}${noun}${suffix}`;
}

/** "12 updates" / "1 update". Used wherever a count needs its noun inline. */
export function updatesNoun(count: number): string {
  return `${count} ${count === 1 ? "update" : "updates"}`;
}

/** "7 sites" / "1 site". Only ever a qualifier, never the counted object. */
export function sitesNoun(count: number): string {
  return `${count} ${count === 1 ? "site" : "sites"}`;
}

/** Distinct sites covered by a task set, from `task.site_id` alone. */
export function distinctSiteCount(tasks: readonly RetryTask[]): number {
  return new Set(tasks.map((t) => t.site_id)).size;
}

export type RetryClassCounts = Partial<Record<RetryClass, number>>;

/** Tally a task set by server retry class. */
export function countByRetryClass(
  tasks: readonly RetryTask[],
): RetryClassCounts {
  const counts: RetryClassCounts = {};
  for (const task of tasks) {
    const cls = task.retry_class;
    if (!isRetryClass(cls)) continue;
    counts[cls] = (counts[cls] ?? 0) + 1;
  }
  return counts;
}

/** "20 not attempted, 1 failed". Empty string when there is nothing to say. */
export function formatRetryBreakdown(counts: RetryClassCounts): string {
  return RETRY_CLASSES.filter((cls) => (counts[cls] ?? 0) > 0)
    .map((cls) => `${counts[cls] ?? 0} ${retryClassLabel(cls)}`)
    .join(", ");
}

// ---------------------------------------------------------------------------
// Exclusions
//
// Every exclusion carries a server-authored `message` with the specifics, and
// it is rendered AS IS. This map is only the short grouping label above those
// sentences, and it falls through to the raw code: a reason this build has not
// seen before is still the operator's answer to "why did this one not run", so
// it is shown rather than collapsed into "unknown".
// ---------------------------------------------------------------------------

const EXCLUSION_REASON_LABEL: Record<string, string> = {
  not_in_run: "not part of this run",
  not_retryable: "no longer retryable",
  site_not_found: "site no longer found",
  site_not_enrolled: "site no longer connected",
  agent_current: "already on the published agent version",
  agent_ineligible: "updated outside the agent self-update channel",
  agent_version_unknown: "agent version could not be read",
  target_in_flight: "already being applied by another run",
  duplicate_target: "duplicate of another selected update",
};

/** Short grouping label for an exclusion reason, or the raw code. */
export function exclusionReasonLabel(reason: string): string {
  return EXCLUSION_REASON_LABEL[reason] ?? reason;
}

/** Group exclusions by reason, preserving first-seen order. */
export function groupExclusions(
  excluded: readonly RetryExclusion[],
): { reason: string; items: RetryExclusion[] }[] {
  const order: string[] = [];
  const byReason = new Map<string, RetryExclusion[]>();
  for (const item of excluded) {
    const bucket = byReason.get(item.reason);
    if (bucket) {
      bucket.push(item);
    } else {
      order.push(item.reason);
      byReason.set(item.reason, [item]);
    }
  }
  return order.map((reason) => ({
    reason,
    items: byReason.get(reason) ?? [],
  }));
}

// ---------------------------------------------------------------------------
// Whether the retry surface exists at all on this run, for this operator
// ---------------------------------------------------------------------------

export interface RetryAvailability {
  /** Render the checkboxes, the bar and the header action. */
  available: boolean;
  /**
   * The one line under the Tasks heading when there is nothing to select but
   * the operator could otherwise have retried. Null means say nothing: either
   * the affordance is live, or this operator/control plane never had it.
   */
  note: string | null;
}

/**
 * Every gate on the retry surface, in one place so it can be pinned by a
 * test rather than read out of JSX.
 *
 * The agent split is argued from the mechanism, not from taste: an agent
 * rollout is wave gated per run, so offering a retry while the first rollout
 * is still walking its waves would race a second gate against it. A
 * plugin/theme/core run has no such gate, and a failed task's target is not
 * in flight, so a long run accumulating failures can be retried while it is
 * still going. That is the 3am case, and hiding the affordance for the whole
 * duration of a 500 task run would be a decision, not a detail.
 */
export function retryAvailability(input: {
  tasks: readonly RetryTask[];
  /** Tasks the server marks retryable. */
  selectableCount: number;
  runStatus: UpdateRun["status"];
  /** Operator or above: may start an update run. */
  canOperate: boolean;
  /** Owner or admin: may start an agent rollout (infrastructure, not content). */
  canManageAgents: boolean;
}): RetryAvailability {
  const { tasks, selectableCount, runStatus, canOperate, canManageAgents } =
    input;

  // An older control plane sends no decision fields. No affordance, no note:
  // there is nothing for this operator to act on and nothing to explain.
  if (!runCarriesRetryContract(tasks)) return { available: false, note: null };

  // The whole /updates group is org scoped on the server, so a site-scoped
  // collaborator never reaches this page: there is no partial-permission
  // subset of tasks to render, only "this operator may start runs" or not.
  const isAgentRun = sharedTargetType(tasks) === "agent";
  const mayRetry = isAgentRun ? canManageAgents : canOperate;
  if (!mayRetry) return { available: false, note: null };

  if (isAgentRun && !isTerminalRunStatus(runStatus)) {
    return {
      available: false,
      note: "Retry becomes available when this rollout finishes.",
    };
  }

  if (selectableCount > 0) return { available: true, note: null };

  const summary = summarizeTasks([...tasks]);
  if (summary.counts.succeeded === summary.total) {
    return {
      available: false,
      note: "Every update in this run succeeded. There is nothing to retry.",
    };
  }
  if (summary.done < summary.total) {
    const outstanding = summary.total - summary.done;
    return {
      available: false,
      note: `Nothing can be retried yet. ${updatesNoun(outstanding)} in this run ${outstanding === 1 ? "is" : "are"} still going.`,
    };
  }
  return {
    available: false,
    note: "No update in this run can be retried.",
  };
}

/**
 * The one predicate that decides whether a retry response may navigate away
 * silently. A shortfall OR a server warning has to be read first: an
 * incomplete enqueue leaves tasks pending against a reaper, so silence there
 * is expensive.
 */
export function retryNeedsReview(result: RetryResult): boolean {
  return result.created !== result.requested || Boolean(result.warning);
}
