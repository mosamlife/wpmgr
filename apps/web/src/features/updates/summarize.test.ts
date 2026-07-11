import { describe, it, expect } from "vitest";

import { isSiteDownRecovery } from "./summarize";

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
