import { describe, it, expect } from "vitest";
import { screen } from "@testing-library/react";
import type { BackupSnapshot } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { SnapshotProgressCard } from "./snapshot-progress-card";

// GH #279 — the CP two-tier watchdog stamps `stalled_at` on a running
// snapshot that has gone quiet past the soft threshold. The card must show a
// calm "taking longer than expected" hint ONLY while status is still
// "running" AND stalled_at is set — never for a healthy running snapshot,
// and never once the snapshot reaches a terminal state (the CP always
// clears stalled_at before failing/completing a run, but the UI gate does
// not rely on that alone — see `stalled-hint.test.ts` for the pure-function
// coverage of the gate itself; this pins the same contract at the render
// layer). jsdom has no EventSource (`typeof EventSource === "undefined"`),
// so `useBackupStream` safely no-ops here rather than opening a connection.

const HINT_TEXT = /taking longer than expected/i;

function buildSnapshot(overrides: Partial<BackupSnapshot> = {}): BackupSnapshot {
  return {
    id: "snap-279",
    tenant_id: "tenant-1",
    site_id: "site-42",
    kind: "full",
    status: "running",
    created_at: "2026-07-23T00:00:00Z",
    updated_at: "2026-07-23T00:00:00Z",
    progress: {},
    ...overrides,
  };
}

describe("SnapshotProgressCard — GH #279 stall indicator", () => {
  it("shows the taking-longer hint when running with stalled_at set", () => {
    const snapshot = buildSnapshot({
      status: "running",
      stalled_at: "2026-07-23T00:05:00Z",
    });
    renderWithProviders(<SnapshotProgressCard snapshot={snapshot} />);
    expect(screen.getByText(HINT_TEXT)).toBeInTheDocument();
  });

  it("does not show the hint for a healthy running snapshot (no stalled_at)", () => {
    const snapshot = buildSnapshot({ status: "running" });
    renderWithProviders(<SnapshotProgressCard snapshot={snapshot} />);
    expect(screen.queryByText(HINT_TEXT)).not.toBeInTheDocument();
  });

  it("does not show the hint for a completed snapshot, even with a stale stalled_at", () => {
    const snapshot = buildSnapshot({
      status: "completed",
      stalled_at: "2026-07-23T00:05:00Z",
      finished_at: "2026-07-23T00:10:00Z",
    });
    renderWithProviders(<SnapshotProgressCard snapshot={snapshot} />);
    expect(screen.queryByText(HINT_TEXT)).not.toBeInTheDocument();
  });

  it("does not show the hint for a failed snapshot, even with a stale stalled_at", () => {
    const snapshot = buildSnapshot({
      status: "failed",
      stalled_at: "2026-07-23T00:05:00Z",
      finished_at: "2026-07-23T00:10:00Z",
      error: "stopped responding; no progress within the allowed time",
    });
    renderWithProviders(<SnapshotProgressCard snapshot={snapshot} />);
    expect(screen.queryByText(HINT_TEXT)).not.toBeInTheDocument();
  });
});
