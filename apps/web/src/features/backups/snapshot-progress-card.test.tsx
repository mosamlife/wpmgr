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

// GH #791 — a snapshot still `running` and being retried (a fresh
// `attempt_error` on file) shows the "retrying" copy plus the last error,
// distinct from the plain GH #279 stall hint. `attempt_error` is a plain
// field on the generated `BackupSnapshot` type (the #791 API slice).
const RETRYING_TEXT = /hasn't started on the site yet\. retrying automatically/i;

function buildRetryingSnapshot(attemptError: string): BackupSnapshot {
  return buildSnapshot({ status: "running", attempt_error: attemptError });
}

describe("SnapshotProgressCard — GH #791 retrying indicator", () => {
  it("shows the retrying copy and the last error when running with an attempt_error", () => {
    const snapshot = buildRetryingSnapshot("Could not connect to the site.");
    renderWithProviders(<SnapshotProgressCard snapshot={snapshot} />);
    expect(screen.getByText(RETRYING_TEXT)).toBeInTheDocument();
    expect(
      screen.getByText(/Last error: Could not connect to the site\./),
    ).toBeInTheDocument();
  });

  it("does not show the retrying copy for a healthy running snapshot (no attempt_error)", () => {
    const snapshot = buildSnapshot({ status: "running" });
    renderWithProviders(<SnapshotProgressCard snapshot={snapshot} />);
    expect(screen.queryByText(RETRYING_TEXT)).not.toBeInTheDocument();
  });

  it("prefers the retrying hint over the plain stalled hint when both apply", () => {
    const snapshot = {
      ...buildRetryingSnapshot("The site did not answer in time."),
      stalled_at: "2026-07-23T00:05:00Z",
    };
    renderWithProviders(<SnapshotProgressCard snapshot={snapshot} />);
    expect(screen.getByText(RETRYING_TEXT)).toBeInTheDocument();
    expect(screen.queryByText(HINT_TEXT)).not.toBeInTheDocument();
  });

  // GH #791 adv-review finding 4 — "This backup hasn't started on the site
  // yet" is false once real progress exists: the runner DID reach the site
  // (phase moved past "queued") before the CP lost contact again on a later
  // attempt. Adversarial review reproduced this through the real route with
  // archiving progress at 1200/5000 files showing beside the "hasn't
  // started" copy.
  const PROGRESS_CONTRADICTION_TEXT = /hasn't started on the site yet/i;
  const RETRYING_WITH_PROGRESS_TEXT =
    /^Retrying automatically\. Last error: The site did not answer in time\.$/;

  it("drops 'hasn't started on the site yet' once the snapshot has real progress", () => {
    const snapshot: BackupSnapshot = {
      ...buildRetryingSnapshot("The site did not answer in time."),
      progress: {
        phase: "archiving_files",
        phase_detail: { files_done: 1200, files_total: 5000 },
      },
    };
    renderWithProviders(<SnapshotProgressCard snapshot={snapshot} />);
    expect(screen.queryByText(PROGRESS_CONTRADICTION_TEXT)).not.toBeInTheDocument();
    expect(screen.getByText(RETRYING_WITH_PROGRESS_TEXT)).toBeInTheDocument();
  });

  it("keeps 'hasn't started on the site yet' when the phase is still 'queued' (no progress made)", () => {
    const snapshot = buildRetryingSnapshot("The site did not answer in time.");
    renderWithProviders(<SnapshotProgressCard snapshot={snapshot} />);
    expect(screen.getByText(RETRYING_TEXT)).toBeInTheDocument();
    expect(screen.queryByText(RETRYING_WITH_PROGRESS_TEXT)).not.toBeInTheDocument();
  });

  // GH #791 adv-review nit 8 — an unrecognized phase id must not make
  // "hasn't started on the site yet" appear beside real file/byte counters.
  // `formatProgress` falls back to `phase: "queued"` for any phase id
  // outside the closed PHASE_IDS set, but `phase_detail`'s counters are
  // still read off the wire regardless — so a phase the web has never seen
  // must still count as progress once a counter is above 0.
  it("drops 'hasn't started on the site yet' when an unrecognised phase id still carries a file counter", () => {
    const snapshot: BackupSnapshot = {
      ...buildRetryingSnapshot("The site did not answer in time."),
      progress: {
        phase: "some_future_phase_the_web_does_not_know",
        phase_detail: { files_done: 1200, files_total: 5000 },
      },
    };
    renderWithProviders(<SnapshotProgressCard snapshot={snapshot} />);
    expect(screen.queryByText(PROGRESS_CONTRADICTION_TEXT)).not.toBeInTheDocument();
    expect(screen.getByText(RETRYING_WITH_PROGRESS_TEXT)).toBeInTheDocument();
  });
});
