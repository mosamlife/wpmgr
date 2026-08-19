import { describe, it, expect } from "vitest";
import type { UpdateTask } from "@wpmgr/api";

import { makeUpdateTask, serverRetryFields } from "@/test/update-task-fixtures";

import {
  isSiteDownRecovery,
  isTerminalRunStatus,
  isAgentNotEligible,
  haltReason,
  summarizeTasks,
} from "./summarize";

// GH #463: regression coverage for a self-hosted control plane sending a
// task status literal outside this bundle's generated TaskStatus union
// (`tsc` never checked the wire value). Before the fix,
// `counts[task.status] += 1` on a status the `counts` initializer never
// declared was `undefined += 1` — NaN — written onto a brand-new stray
// property keyed by that unrecognized string (verified directly: the
// pre-fix `counts` object comes back with an extra `"<unknown-status>": NaN`
// entry alongside its 9 declared keys). `done`/`total`/the progress
// percentage at $runId.tsx:292 happen to stay finite regardless in the
// current shape of this function, because `done` sums six explicitly named,
// already-initialized keys rather than reducing over the whole `counts`
// object — but `summary.counts` no longer matches its own declared type
// (`Record<TaskStatus, number>`), which is exactly the kind of drift that
// breaks the next piece of code that DOES iterate `counts` wholesale (a
// stacked bar, an `Object.values` reduction). The fix's `key in counts`
// guard keeps `counts` to exactly its declared keys, and keeps `done`
// undercounting (never crediting an unrecognized status as complete) as an
// explicit invariant rather than an accident of the current arithmetic.
describe("summarizeTasks", () => {
  it("does not add a stray NaN-valued property for a status outside the TaskStatus union", () => {
    const tasks = [makeUpdateTask({ status: "reconciling" as UpdateTask["status"] })];
    const { counts } = summarizeTasks(tasks);
    expect(Object.prototype.hasOwnProperty.call(counts, "reconciling")).toBe(false);
    expect(Object.keys(counts).sort()).toEqual(
      [
        "pending",
        "running",
        "succeeded",
        "failed",
        "rolled_back",
        "skipped",
        "cancelled",
        "scheduled",
        "expired",
      ].sort(),
    );
  });

  it("does not produce NaN for a single task whose status is outside the TaskStatus union", () => {
    const tasks = [makeUpdateTask({ status: "reconciling" as UpdateTask["status"] })];
    const { done, total } = summarizeTasks(tasks);
    expect(Number.isFinite(done)).toBe(true);
    expect(done).toBe(0);
    expect(total).toBe(1);
  });

  it("excludes an unknown-status task from done in a mix of known and unknown statuses", () => {
    const tasks = [
      makeUpdateTask({ id: "44444444-4444-4444-4444-444444444441", status: "succeeded" }),
      makeUpdateTask({ id: "44444444-4444-4444-4444-444444444442", status: "failed" }),
      makeUpdateTask({
        id: "44444444-4444-4444-4444-444444444443",
        status: "reconciling" as UpdateTask["status"],
      }),
    ];
    const { done, total, counts } = summarizeTasks(tasks);
    expect(Number.isFinite(done)).toBe(true);
    // Only the succeeded + failed tasks count; the unknown-status task is not
    // folded into any bucket, so it must not appear in `done`.
    expect(done).toBe(2);
    expect(total).toBe(3);
    expect(counts.succeeded).toBe(1);
    expect(counts.failed).toBe(1);
    expect(Object.prototype.hasOwnProperty.call(counts, "reconciling")).toBe(false);
  });

  it("keeps the $runId.tsx:292 progress percentage finite for an unknown status", () => {
    const tasks = [
      makeUpdateTask({ id: "44444444-4444-4444-4444-444444444441", status: "succeeded" }),
      makeUpdateTask({
        id: "44444444-4444-4444-4444-444444444442",
        status: "reconciling" as UpdateTask["status"],
      }),
    ];
    const { done, total } = summarizeTasks(tasks);
    const pct = Math.round((done / total) * 100);
    expect(Number.isFinite(pct)).toBe(true);
  });
});

// GH #210: pure-logic coverage for the site-down-recovery detector: the
// worst-case rollback failure (site-wide PHP fatal, undeliverable rollback,
// automatic filesystem recovery attempted by the agent watchdog). The
// backend keeps the existing failed/rolled_back status and communicates the
// condition purely through detail/error text, so this is the single source
// of truth every rendering surface (TaskStatusBadge, UpdateTasksTable,
// AvailableUpdatesCard's RowStateLine) keys off.

describe("isSiteDownRecovery", () => {
  it("is true for a rolled_back task whose detail describes the condition", () => {
    expect(
      isSiteDownRecovery(
        "rolled_back",
        "The site went down site-wide; automatic filesystem recovery was attempted.",
        undefined,
      ),
    ).toBe(true);
  });

  it("is true for a failed task whose error (not detail) describes the condition", () => {
    expect(
      isSiteDownRecovery(
        "failed",
        undefined,
        "Site is not responding; the rollback command was undeliverable and an agent watchdog attempted automatic recovery.",
      ),
    ).toBe(true);
  });

  it("is false for an ordinary rolled_back/failed task with unrelated detail/error text", () => {
    expect(
      isSiteDownRecovery("rolled_back", "agent reported update failure", "activation check failed"),
    ).toBe(false);
    expect(isSiteDownRecovery("failed", "connection timed out", undefined)).toBe(false);
  });

  it("is false for a non-terminal-failure status even if the text matches (e.g. a stray log line on a running task)", () => {
    expect(
      isSiteDownRecovery("running", "watchdog check in progress, site-wide scan", undefined),
    ).toBe(false);
    expect(isSiteDownRecovery("succeeded", "site-wide cache cleared", undefined)).toBe(false);
    expect(isSiteDownRecovery("pending", undefined, undefined)).toBe(false);
    expect(isSiteDownRecovery("skipped", undefined, undefined)).toBe(false);
  });

  it("is false when detail/error is empty on a terminal status", () => {
    expect(isSiteDownRecovery("failed", undefined, undefined)).toBe(false);
    expect(isSiteDownRecovery("rolled_back", "", "")).toBe(false);
  });
});

// GH #255 Phase 2: the agent self-update channel's own terminal-state and
// vocabulary helpers.

describe("isTerminalRunStatus", () => {
  it("treats completed and halted as terminal", () => {
    expect(isTerminalRunStatus("completed")).toBe(true);
    expect(isTerminalRunStatus("halted")).toBe(true);
  });

  it("treats pending, running and undefined as not terminal", () => {
    expect(isTerminalRunStatus("pending")).toBe(false);
    expect(isTerminalRunStatus("running")).toBe(false);
    expect(isTerminalRunStatus(undefined)).toBe(false);
  });
});

describe("isAgentNotEligible", () => {
  it("is true for a skipped agent task whose detail names the not-eligible condition", () => {
    expect(
      isAgentNotEligible(
        "agent",
        "skipped",
        "this agent build has no self-updater and is upgraded outside this channel",
      ),
    ).toBe(true);
  });

  it("is false for a non-agent target, even with matching detail text", () => {
    expect(
      isAgentNotEligible(
        "plugin",
        "skipped",
        "this agent build has no self-updater",
      ),
    ).toBe(false);
  });

  it("is false for an agent task that is not skipped", () => {
    expect(isAgentNotEligible("agent", "running", "no self-updater")).toBe(
      false,
    );
  });

  it("is false for a skipped agent task with unrelated detail text", () => {
    expect(
      isAgentNotEligible("agent", "skipped", "cancelled: the run was halted"),
    ).toBe(false);
  });
});

describe("haltReason", () => {
  function task(overrides: Partial<UpdateTask>): UpdateTask {
    const status = overrides.status ?? "cancelled";
    return {
      id: "task-1",
      run_id: "run-1",
      tenant_id: "tenant-1",
      site_id: "site-1",
      target_type: "agent",
      target_slug: "wpmgr",
      status: "cancelled",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
      // GH #336: the server always writes the retry pair its own
      // retryClassify would produce for this status.
      ...serverRetryFields(status),
      ...overrides,
    };
  }

  it("returns null for a run that has not halted", () => {
    expect(haltReason({ status: "running", tasks: [] })).toBeNull();
    expect(haltReason(undefined)).toBeNull();
  });

  it("strips the 'cancelled: ' prefix from a cancelled task's detail", () => {
    expect(
      haltReason({
        status: "halted",
        tasks: [
          task({
            detail:
              "cancelled: wave 0 (the canary) failed on 1 of 1 site(s)",
          }),
        ],
      }),
    ).toBe("wave 0 (the canary) failed on 1 of 1 site(s)");
  });

  it("returns the detail verbatim when it has no 'cancelled: ' prefix", () => {
    expect(
      haltReason({
        status: "halted",
        tasks: [task({ detail: "the run was halted" })],
      }),
    ).toBe("the run was halted");
  });

  it("falls back to a counts-derived summary when no task was cancelled", () => {
    // A single-site canary run: the one task is already terminal (failed) by
    // the time the gate re-judges it, so haltLocked has nothing left to
    // cancel. Zero confirmations, so the "no site confirmed" wording applies.
    expect(
      haltReason({
        status: "halted",
        tasks: [task({ status: "failed" })],
      }),
    ).toBe(
      "The rollout was halted because no site confirmed the upgrade (1 failed, 0 skipped, of 1 contacted).",
    );
  });

  it("counts a partial-confirmation halt against the sites that were actually contacted", () => {
    // Wave 1 of 3: two sites confirmed, one failed, so the wave's own
    // failure-rate threshold halted the run. `confirmed` is nonzero here,
    // which is the branch this case pins.
    expect(
      haltReason({
        status: "halted",
        tasks: [
          task({ id: "t1", status: "succeeded" }),
          task({ id: "t2", status: "succeeded" }),
          task({ id: "t3", status: "failed" }),
        ],
      }),
    ).toBe(
      "The rollout was halted after 1 of 3 contacted sites failed to confirm the upgrade.",
    );
  });

  it("falls back to the 'never contacted' summary when nothing was even attempted", () => {
    expect(
      haltReason({
        status: "halted",
        tasks: [task({ status: "cancelled", detail: undefined })],
      }),
    ).toBe("The rollout was halted before any site could be contacted.");
  });

  // GH self-update mod_php unlock, D-A: a run whose only task came back
  // `skipped` (the agent answered: an old agent with no self-update route,
  // a build this channel does not apply to, an unconfirmed "up to date") is
  // NOT a site nobody heard from. Before this fix `contacted` excluded
  // `skipped` entirely, so this exact shape rendered the false "before any
  // site could be contacted" sentence for a site that was, in fact,
  // contacted and answered.
  it("counts a skipped task as contacted, not as never-contacted", () => {
    expect(
      haltReason({
        status: "halted",
        tasks: [
          task({
            status: "skipped",
            detail:
              "not attempted: this site's agent predates the self-update channel and has no self-update route",
          }),
        ],
      }),
    ).toBe(
      "The rollout was halted because no site confirmed the upgrade (0 failed, 1 skipped, of 1 contacted).",
    );
  });
});
