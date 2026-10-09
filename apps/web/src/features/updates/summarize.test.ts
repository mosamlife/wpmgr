import { describe, it, expect } from "vitest";
import type { UpdateTask } from "@wpmgr/api";

import { makeUpdateTask, serverRetryFields } from "@/test/update-task-fixtures";

import {
  CORE_LEFT_AS_IS_DETAIL,
  CORE_NO_CHANGE_UNHEALTHY_DETAIL,
  CORE_ROLLBACK_UNDELIVERABLE_DETAIL,
  HEALTH_CHECK_FAILED_REASON,
  PLUGIN_SITE_DOWN_DETAIL,
  ROLLBACK_RAW_ERROR,
} from "@/test/update-task-details";

import {
  siteDownFallbackDetail,
  siteDownKind,
  siteDownLabel,
  isRedirectFailure,
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

describe("siteDownKind", () => {
  it("is recovery_attempted for a rolled_back plugin whose detail describes the condition", () => {
    expect(
      siteDownKind({
        status: "rolled_back",
        target_type: "plugin",
        detail:
          "The site went down site-wide; automatic filesystem recovery was attempted.",
      }),
    ).toBe("recovery_attempted");
  });

  it("is recovery_attempted for a failed theme whose error (not detail) describes the condition", () => {
    expect(
      siteDownKind({
        status: "failed",
        target_type: "theme",
        error:
          "Site is not responding; the rollback command was undeliverable and an agent watchdog attempted automatic recovery.",
      }),
    ).toBe("recovery_attempted");
  });

  it("is recovery_attempted for the sentence the control plane writes for a plugin", () => {
    expect(
      siteDownKind({
        status: "failed",
        target_type: "plugin",
        detail: PLUGIN_SITE_DOWN_DETAIL,
        error: ROLLBACK_RAW_ERROR,
      }),
    ).toBe("recovery_attempted");
  });

  it("is null for an ordinary rolled_back/failed task with unrelated detail/error text", () => {
    expect(
      siteDownKind({
        status: "rolled_back",
        target_type: "plugin",
        detail: "agent reported update failure",
        error: "activation check failed",
      }),
    ).toBeNull();
    expect(
      siteDownKind({
        status: "failed",
        target_type: "plugin",
        detail: "connection timed out",
      }),
    ).toBeNull();
  });

  it("is null for a non-terminal-failure status even if the text matches (e.g. a stray log line on a running task)", () => {
    const text = "watchdog check in progress, site-wide scan";
    for (const status of ["running", "succeeded", "pending", "skipped"]) {
      expect(
        siteDownKind({ status, target_type: "plugin", detail: text }),
        status,
      ).toBeNull();
    }
  });

  it("is null when detail/error is empty on a terminal status", () => {
    expect(
      siteDownKind({ status: "failed", target_type: "plugin" }),
    ).toBeNull();
    expect(
      siteDownKind({
        status: "rolled_back",
        target_type: "plugin",
        detail: "",
        error: "",
      }),
    ).toBeNull();
  });

  // GH #415: the agent's update watchdog exists for plugins and themes.
  // WordPress core has no automatic recovery, and the control plane says so in
  // its own sentence when core's rollback cannot be delivered.
  describe("for WordPress core (GH #415)", () => {
    it("is manual_recovery for the sentence the control plane writes when core's rollback could not be delivered", () => {
      expect(
        siteDownKind({
          status: "failed",
          target_type: "core",
          detail: CORE_ROLLBACK_UNDELIVERABLE_DETAIL,
          error: ROLLBACK_RAW_ERROR,
        }),
      ).toBe("manual_recovery");
    });

    it("is the same text, a different answer: plugin and core split on the target type alone", () => {
      const task = { status: "failed", detail: CORE_ROLLBACK_UNDELIVERABLE_DETAIL };
      expect(siteDownKind({ ...task, target_type: "plugin" })).toBe(
        "recovery_attempted",
      );
      expect(siteDownKind({ ...task, target_type: "core" })).toBe(
        "manual_recovery",
      );
    });

    it.each([
      ["left as is", CORE_LEFT_AS_IS_DETAIL],
      ["reported no change but unhealthy", CORE_NO_CHANGE_UNHEALTHY_DETAIL],
    ])("is null for core that %s: an ordinary failure", (_label, detail) => {
      expect(
        siteDownKind({
          status: "failed",
          target_type: "core",
          detail,
          error: HEALTH_CHECK_FAILED_REASON,
        }),
      ).toBeNull();
    });

    it("is null for a core task that was rolled back: it was restored, so the site is not down", () => {
      expect(
        siteDownKind({
          status: "rolled_back",
          target_type: "core",
          detail: CORE_ROLLBACK_UNDELIVERABLE_DETAIL,
        }),
      ).toBeNull();
    });

    it("ignores the error log: for core only the control plane's own detail decides", () => {
      expect(
        siteDownKind({
          status: "failed",
          target_type: "core",
          detail: CORE_LEFT_AS_IS_DETAIL,
          error: "Site is not responding; watchdog attempted automatic recovery.",
        }),
      ).toBeNull();
    });
  });
});

describe("siteDownLabel and siteDownFallbackDetail", () => {
  it("never tells an operator that recovery was attempted for core", () => {
    expect(siteDownLabel("manual_recovery")).toBe(
      "Site down, manual recovery needed",
    );
    expect(siteDownFallbackDetail("manual_recovery")).toMatch(
      /nothing restores wordpress core automatically/i,
    );
    expect(siteDownFallbackDetail("manual_recovery")).not.toMatch(
      /recovery was attempted/i,
    );
  });

  it("keeps the recovery-attempted copy for a plugin or theme", () => {
    expect(siteDownLabel("recovery_attempted")).toBe(
      "Site down, recovery attempted",
    );
    expect(siteDownFallbackDetail("recovery_attempted")).toMatch(
      /automatic filesystem recovery was attempted/i,
    );
  });
});

// GH #755 round 2: pure-logic coverage for the redirect-failure detector
// against the server's five composed-copy variants (N4: an agent self-update
// redirect comes back `skipped`, not `failed`/`rolled_back`, so it must not
// fall through to the generic truncated-detail treatment).
//   A  (target the saved address will move to)
//   D  (the site redirects its command address back to itself)
//   B1 (redirect to another path on the site)
//   B2 (redirect that drops HTTPS)
//   C  (a redirect that names no usable address)
const REDIRECT_COPY_A =
  "https://example.com redirects to https://www.example.com, so no command was sent. If WordPress on the site reports https://www.example.com as its address, the saved address updates to https://www.example.com automatically at a later check-in from the site. That update does not happen while another site in this workspace uses https://www.example.com: if one does, remove or change the duplicate site.";
const REDIRECT_COPY_D =
  "https://example.com redirects its command address back to itself (HTTP 301), so no command was sent. Exempt /wp-json/wpmgr/ from the redirect on the site or its CDN.";
const REDIRECT_COPY_B1 =
  "https://example.com redirects to https://staging.example.com/wp-json/wpmgr/v1/command/metadata, so no command was sent. Exempt /wp-json/wpmgr/ from the redirect on the site or its CDN.";
const REDIRECT_COPY_B2 =
  "https://example.com redirects to http://example.com/wp-json/wpmgr/v1/command/metadata, which drops HTTPS, so no command was sent. Exempt /wp-json/wpmgr/ from the redirect on the site or its CDN.";
const REDIRECT_COPY_C =
  "https://example.com answered with a redirect (HTTP 302) that names no usable address, so no command was sent. Exempt /wp-json/wpmgr/ from the redirect on the site or its CDN.";

describe("isRedirectFailure", () => {
  it.each([
    ["A", REDIRECT_COPY_A],
    ["D", REDIRECT_COPY_D],
    ["B1", REDIRECT_COPY_B1],
    ["B2", REDIRECT_COPY_B2],
    ["C", REDIRECT_COPY_C],
  ])("is true for a failed task whose detail is copy %s", (_label, copy) => {
    expect(isRedirectFailure("failed", `Update not started. ${copy}`, undefined)).toBe(true);
  });

  it("is true for a skipped agent self-update task (N4): status skipped, not failed/rolled_back", () => {
    expect(
      isRedirectFailure(
        "skipped",
        `Agent self-update not started. ${REDIRECT_COPY_A}`,
        "agent_self_update command: https://example.com redirects to https://www.example.com/wp-json/wpmgr/v1/command/agent_self_update (HTTP 301); commands are sent only to the site's saved address, so the redirect was not followed",
      ),
    ).toBe(true);
  });

  it("is false for a site-down-recovery detail (distinct condition, must not cross-fire)", () => {
    expect(
      isRedirectFailure(
        "rolled_back",
        "The site went down site-wide; automatic filesystem recovery was attempted.",
        undefined,
      ),
    ).toBe(false);
  });

  it("is false for an ordinary dry-run command failure", () => {
    expect(isRedirectFailure("failed", "dry-run command failed", undefined)).toBe(false);
  });

  it("is false for a succeeded task even if the detail text happens to be copy A", () => {
    expect(isRedirectFailure("succeeded", REDIRECT_COPY_A, undefined)).toBe(false);
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
