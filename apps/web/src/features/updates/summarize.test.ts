import { describe, it, expect } from "vitest";
import type { UpdateTask } from "@wpmgr/api";

import {
  isSiteDownRecovery,
  isTerminalRunStatus,
  isAgentNotEligible,
  haltReason,
} from "./summarize";

// GH #210 — pure-logic coverage for the site-down-recovery detector: the
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
