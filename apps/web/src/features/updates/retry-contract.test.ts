import { describe, it, expect } from "vitest";
import type { UpdateRunRetryResult, UpdateTask } from "@wpmgr/api";

import { parseWireTask, serverRetryFields } from "@/test/update-task-fixtures";

import {
  countByRetryClass,
  distinctSiteCount,
  exclusionReasonLabel,
  formatRetryBreakdown,
  groupExclusions,
  hasServerRetryFields,
  isDefaultRetrySelected,
  isRetrySelectable,
  isRetryWireTask,
  retryActionLabel,
  retryAvailability,
  retryNeedsReview,
  runCarriesRetryContract,
  sharedTargetType,
  taskSiteLabel,
} from "./retry-contract";

// GH #336 policy, unit and wire coverage.
//
// Every fixture here is a JSON document in the shape
// GET /api/v1/updates/{runId} actually returns, round-tripped through
// `parseWireTask` (which runs the application's own wire guard), so a fixture
// that a server would never produce fails the test instead of proving
// nothing. The retryable/retry_class pair always comes from
// `serverRetryFields`, which mirrors the control plane's retryClassify.

const RUN_ID = "9c3f1f9e-1f6e-4a5e-9a2f-0f0f0f0f0f0f";
const TENANT_ID = "22222222-2222-2222-2222-222222222222";

/**
 * The incident from the issue: a 21 site agent rollout whose canary failed,
 * correctly cancelling the other 20 without touching them.
 */
function incidentRunTasks(): UpdateTask[] {
  const canary = parseWireTask({
    id: "task-canary",
    run_id: RUN_ID,
    tenant_id: TENANT_ID,
    site_id: "site-canary",
    site_name: "shop.example.com",
    target_type: "agent",
    target_slug: "wpmgr",
    desired_version: "0.61.114",
    from_version: "0.61.112",
    status: "failed",
    detail: "the agent did not confirm the new version",
    created_at: "2026-08-04T09:00:00Z",
    updated_at: "2026-08-04T09:10:00Z",
    ...serverRetryFields("failed"),
  });
  const withheld = Array.from({ length: 20 }, (_, i) =>
    parseWireTask({
      id: `task-withheld-${i}`,
      run_id: RUN_ID,
      tenant_id: TENANT_ID,
      site_id: `site-${i}`,
      site_name: `site-${i}.example.com`,
      target_type: "agent",
      target_slug: "wpmgr",
      desired_version: "0.61.114",
      from_version: "0.61.112",
      status: "cancelled",
      detail: "cancelled: the canary site did not confirm the new version",
      created_at: "2026-08-04T09:00:00Z",
      updated_at: "2026-08-04T09:10:00Z",
      ...serverRetryFields("cancelled"),
    }),
  );
  return [canary, ...withheld];
}

describe("retry contract wire guard", () => {
  it("accepts the run detail shape the control plane emits, with and without site_name", () => {
    const tasks = incidentRunTasks();
    expect(tasks).toHaveLength(21);
    expect(tasks.every((t) => isRetryWireTask(t))).toBe(true);
    expect(runCarriesRetryContract(tasks)).toBe(true);

    // site_name is optional by spec (empty only when the site row could not
    // be read), so a task without it is still a valid wire task.
    const noName = parseWireTask({
      id: "task-no-name",
      run_id: RUN_ID,
      tenant_id: TENANT_ID,
      site_id: "site-x",
      target_type: "plugin",
      target_slug: "akismet/akismet.php",
      status: "failed",
      created_at: "2026-08-04T09:00:00Z",
      updated_at: "2026-08-04T09:10:00Z",
      ...serverRetryFields("failed"),
    });
    expect(hasServerRetryFields(noName)).toBe(true);
  });

  it("rejects a task from a control plane that predates the retry contract, and turns the whole surface off", () => {
    // Exactly what an older control plane sends: no retryable, no retry_class.
    const legacy: unknown = JSON.parse(
      JSON.stringify({
        id: "task-legacy",
        run_id: RUN_ID,
        tenant_id: TENANT_ID,
        site_id: "site-legacy",
        target_type: "plugin",
        target_slug: "akismet/akismet.php",
        status: "failed",
        created_at: "2026-08-04T09:00:00Z",
        updated_at: "2026-08-04T09:10:00Z",
      }),
    );
    expect(isRetryWireTask(legacy)).toBe(false);

    // A run of such tasks carries no contract, so the page renders no retry
    // affordance at all rather than a client-invented policy.
    const legacyTasks = [legacy] as UpdateTask[];
    expect(runCarriesRetryContract(legacyTasks)).toBe(false);
    expect(runCarriesRetryContract([])).toBe(false);
  });

  it("rejects a retry_class value outside the contract enum", () => {
    const bogus: unknown = JSON.parse(
      JSON.stringify({
        id: "task-bogus",
        run_id: RUN_ID,
        tenant_id: TENANT_ID,
        site_id: "site-bogus",
        target_type: "plugin",
        target_slug: "akismet/akismet.php",
        status: "failed",
        created_at: "2026-08-04T09:00:00Z",
        updated_at: "2026-08-04T09:10:00Z",
        retryable: true,
        retry_class: "probably_fine",
      }),
    );
    expect(isRetryWireTask(bogus)).toBe(false);
  });
});

describe("default selection comes from the server's retry_class", () => {
  it("pre-selects every failed and never_ran task in the incident run, and nothing else", () => {
    const tasks = incidentRunTasks();
    const defaults = tasks.filter((t) => isDefaultRetrySelected(t));
    expect(defaults).toHaveLength(21);
    expect(defaults.filter((t) => t.retry_class === "failed")).toHaveLength(1);
    expect(defaults.filter((t) => t.retry_class === "never_ran")).toHaveLength(
      20,
    );
  });

  it("leaves reverted and skipped selectable but never pre-selected, and never_applicable neither", () => {
    const rows: { status: UpdateTask["status"]; selectable: boolean; def: boolean }[] =
      [
        { status: "failed", selectable: true, def: true },
        { status: "cancelled", selectable: true, def: true },
        { status: "rolled_back", selectable: true, def: false },
        { status: "skipped", selectable: true, def: false },
        { status: "succeeded", selectable: false, def: false },
        { status: "running", selectable: false, def: false },
        { status: "pending", selectable: false, def: false },
      ];
    for (const row of rows) {
      const task = parseWireTask({
        id: `task-${row.status}`,
        run_id: RUN_ID,
        tenant_id: TENANT_ID,
        site_id: "site-1",
        site_name: "one.example.com",
        target_type: "plugin",
        target_slug: "akismet/akismet.php",
        status: row.status,
        created_at: "2026-08-04T09:00:00Z",
        updated_at: "2026-08-04T09:10:00Z",
        ...serverRetryFields(row.status),
      });
      expect(isRetrySelectable(task), row.status).toBe(row.selectable);
      expect(isDefaultRetrySelected(task), row.status).toBe(row.def);
    }
  });

  it("never selects a task the server marked unretryable, whatever its class says", () => {
    // A control plane may withdraw retryability for a reason this client does
    // not model. `retryable` is the authority; the class is only vocabulary.
    const task = parseWireTask({
      id: "task-withdrawn",
      run_id: RUN_ID,
      tenant_id: TENANT_ID,
      site_id: "site-1",
      target_type: "agent",
      target_slug: "wpmgr",
      status: "failed",
      created_at: "2026-08-04T09:00:00Z",
      updated_at: "2026-08-04T09:10:00Z",
      retryable: false,
      retry_class: "failed",
    });
    expect(isRetrySelectable(task)).toBe(false);
    expect(isDefaultRetrySelected(task)).toBe(false);
  });
});

describe("the unit is tasks, labelled as updates", () => {
  it("labels the incident selection as 21 agent updates, not 21 sites", () => {
    const tasks = incidentRunTasks();
    expect(
      retryActionLabel({
        count: tasks.length,
        target: sharedTargetType(tasks),
      }),
    ).toBe("Retry 21 agent updates");
  });

  it("counts tasks, not sites, for a 20 site by 5 plugin run", () => {
    const tasks = Array.from({ length: 100 }, (_, i) =>
      parseWireTask({
        id: `task-${i}`,
        run_id: RUN_ID,
        tenant_id: TENANT_ID,
        site_id: `site-${i % 20}`,
        site_name: `site-${i % 20}.example.com`,
        target_type: "plugin",
        target_slug: `plugin-${i % 5}/plugin.php`,
        status: "failed",
        created_at: "2026-08-04T09:00:00Z",
        updated_at: "2026-08-04T09:10:00Z",
        ...serverRetryFields("failed"),
      }),
    );
    expect(distinctSiteCount(tasks)).toBe(20);
    expect(
      retryActionLabel({ count: tasks.length, target: sharedTargetType(tasks) }),
    ).toBe("Retry 100 plugin updates");
  });

  it("drops the target adjective for a mixed selection and singularises at one", () => {
    expect(retryActionLabel({ count: 18, target: null })).toBe(
      "Retry 18 updates",
    );
    expect(retryActionLabel({ count: 1, target: null })).toBe("Retry 1 update");
    expect(retryActionLabel({ count: 0, target: "plugin" })).toBe(
      "Retry updates",
    );
  });

  it("makes a dry run impossible to miss in the label itself", () => {
    expect(
      retryActionLabel({ count: 12, target: null, dryRun: true }),
    ).toBe("Retry 12 updates (dry run)");
    expect(retryActionLabel({ count: 0, target: null, dryRun: true })).toBe(
      "Retry updates (dry run)",
    );
  });

  it("breaks the selection down by server class, not by prose", () => {
    const counts = countByRetryClass(incidentRunTasks());
    expect(counts).toEqual({ failed: 1, never_ran: 20 });
    expect(formatRetryBreakdown(counts)).toBe("20 not attempted, 1 failed");
  });
});

describe("site identity comes from the task row", () => {
  const task = parseWireTask({
    id: "task-1",
    run_id: RUN_ID,
    tenant_id: TENANT_ID,
    site_id: "44444444-4444-4444-4444-444444444444",
    site_name: "shop.example.com",
    target_type: "plugin",
    target_slug: "akismet/akismet.php",
    status: "failed",
    created_at: "2026-08-04T09:00:00Z",
    updated_at: "2026-08-04T09:10:00Z",
    ...serverRetryFields("failed"),
  });

  it("prefers the task row over the capped sites cache, which may not contain the site at all", () => {
    // The sites list is paginated server side; on a run wider than one page
    // the cache simply does not have the row. The task row always does.
    expect(taskSiteLabel(task, new Map())).toBe("shop.example.com");
  });

  it("falls back to the cache, then to a short id, when the row carries no name", () => {
    const nameless = { ...task, site_name: "" };
    expect(
      taskSiteLabel(nameless, new Map([[task.site_id, "cached.example.com"]])),
    ).toBe("cached.example.com");
    expect(taskSiteLabel(nameless, new Map())).toBe("44444444...");
  });
});

describe("the retry result accounts for every requested task", () => {
  function wireResult(json: string): UpdateRunRetryResult {
    return JSON.parse(json) as UpdateRunRetryResult;
  }

  it("flags a shortfall for review and groups exclusions by reason, keeping the server sentence", () => {
    const result = wireResult(
      JSON.stringify({
        run_id: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
        requested: 21,
        created: 18,
        excluded: [
          {
            task_id: "task-withheld-1",
            reason: "site_not_enrolled",
            message: "site-1.example.com is no longer enrolled.",
          },
          {
            task_id: "task-withheld-2",
            reason: "agent_current",
            message: "site-2.example.com already runs agent 0.61.116.",
          },
          {
            task_id: "task-withheld-3",
            reason: "agent_current",
            message: "site-3.example.com already runs agent 0.61.116.",
          },
        ],
      }),
    );

    expect(retryNeedsReview(result)).toBe(true);
    const groups = groupExclusions(result.excluded);
    expect(groups.map((g) => g.reason)).toEqual([
      "site_not_enrolled",
      "agent_current",
    ]);
    expect(groups[1]?.items).toHaveLength(2);
    expect(groups[1]?.items[0]?.message).toContain("already runs agent 0.61.116");
  });

  it("treats a server warning on a complete commit as something the operator must still read", () => {
    const result = wireResult(
      JSON.stringify({
        run_id: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
        requested: 4,
        created: 4,
        excluded: [],
        warning:
          "The run was created but 2 tasks could not be queued; they will stay pending.",
      }),
    );
    expect(retryNeedsReview(result)).toBe(true);
  });

  it("navigates silently only when everything requested was created and nothing was flagged", () => {
    const result = wireResult(
      JSON.stringify({
        run_id: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
        requested: 21,
        created: 21,
        excluded: [],
      }),
    );
    expect(retryNeedsReview(result)).toBe(false);
  });

  it("shows a reason code it has never seen rather than collapsing it to unknown", () => {
    expect(exclusionReasonLabel("agent_current")).toBe(
      "already on the published agent version",
    );
    expect(exclusionReasonLabel("some_future_reason")).toBe(
      "some_future_reason",
    );
  });
});

describe("who gets the retry surface, and what a run without one says", () => {
  const agentTasks = incidentRunTasks();
  const pluginFailed = parseWireTask({
    id: "task-plugin-failed",
    run_id: RUN_ID,
    tenant_id: TENANT_ID,
    site_id: "site-1",
    site_name: "one.example.com",
    target_type: "plugin",
    target_slug: "akismet/akismet.php",
    status: "failed",
    created_at: "2026-08-04T09:00:00Z",
    updated_at: "2026-08-04T09:10:00Z",
    ...serverRetryFields("failed"),
  });
  const pluginRunning = parseWireTask({
    ...pluginFailed,
    id: "task-plugin-running",
    status: "running",
    ...serverRetryFields("running"),
  });
  const pluginSucceeded = parseWireTask({
    ...pluginFailed,
    id: "task-plugin-succeeded",
    status: "succeeded",
    ...serverRetryFields("succeeded"),
  });

  it("shows nothing at all on a control plane that predates the contract", () => {
    const legacy = [
      { ...pluginFailed, retryable: undefined, retry_class: undefined },
    ] as unknown as UpdateTask[];
    expect(
      retryAvailability({
        tasks: legacy,
        selectableCount: 1,
        runStatus: "completed",
        canOperate: true,
        canManageAgents: true,
      }),
    ).toEqual({ available: false, note: null });
  });

  it("shows nothing to a viewer, and nothing to an operator on an agent rollout", () => {
    expect(
      retryAvailability({
        tasks: [pluginFailed],
        selectableCount: 1,
        runStatus: "completed",
        canOperate: false,
        canManageAgents: false,
      }),
    ).toEqual({ available: false, note: null });

    // Agent self-update is infrastructure, not content: owner/admin only,
    // matching the Sites page's own gate on the same operation.
    expect(
      retryAvailability({
        tasks: agentTasks,
        selectableCount: 21,
        runStatus: "halted",
        canOperate: true,
        canManageAgents: false,
      }),
    ).toEqual({ available: false, note: null });
  });

  it("offers the retry on the halted agent rollout from the issue", () => {
    expect(
      retryAvailability({
        tasks: agentTasks,
        selectableCount: 21,
        runStatus: "halted",
        canOperate: true,
        canManageAgents: true,
      }),
    ).toEqual({ available: true, note: null });
  });

  it("waits for an agent rollout to finish, because a second wave gate must not race the first", () => {
    expect(
      retryAvailability({
        tasks: agentTasks,
        selectableCount: 1,
        runStatus: "running",
        canOperate: true,
        canManageAgents: true,
      }),
    ).toEqual({
      available: false,
      note: "Retry becomes available when this rollout finishes.",
    });
  });

  it("offers the retry on a plugin run that is still going, because a failed task's target is not in flight", () => {
    expect(
      retryAvailability({
        tasks: [pluginFailed, pluginRunning],
        selectableCount: 1,
        runStatus: "running",
        canOperate: true,
        canManageAgents: false,
      }),
    ).toEqual({ available: true, note: null });
  });

  it("says so when everything succeeded, rather than leaving a silent gap", () => {
    expect(
      retryAvailability({
        tasks: [pluginSucceeded],
        selectableCount: 0,
        runStatus: "completed",
        canOperate: true,
        canManageAgents: false,
      }),
    ).toEqual({
      available: false,
      note: "Every update in this run succeeded. There is nothing to retry.",
    });
  });

  it("says what it is waiting for when nothing has settled yet", () => {
    expect(
      retryAvailability({
        tasks: [pluginRunning, pluginSucceeded],
        selectableCount: 0,
        runStatus: "running",
        canOperate: true,
        canManageAgents: false,
      }),
    ).toEqual({
      available: false,
      note: "Nothing can be retried yet. 1 update in this run is still going.",
    });
  });

  it("says plainly when a settled run has nothing the control plane will run again", () => {
    const structurallyUnretryable = parseWireTask({
      ...pluginFailed,
      id: "task-ineligible",
      target_type: "agent",
      target_slug: "wpmgr",
      status: "skipped",
      retryable: false,
      retry_class: "skipped",
    });
    expect(
      retryAvailability({
        tasks: [structurallyUnretryable],
        selectableCount: 0,
        runStatus: "completed",
        canOperate: true,
        canManageAgents: true,
      }),
    ).toEqual({
      available: false,
      note: "No update in this run can be retried.",
    });
  });
});
