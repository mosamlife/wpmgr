import { describe, it, expect } from "vitest";
import type { BackupSnapshot } from "@wpmgr/api";

import {
  formatProgress,
  hasVisibleProgress,
  isSnapshotRetrying,
  isSnapshotStalled,
  snapshotAttemptError,
} from "./format-progress";

// GH #279 — the "taking longer than expected" indicator must show ONLY
// while the run is genuinely still active: `status="running"` AND a
// `stalled_at` timestamp from the CP watchdog. A completed/failed snapshot
// must never show it, even if a stale `stalled_at` value were somehow still
// present on the DTO (the CP always clears it before a terminal
// transition, but the UI gate should not rely on that alone).

describe("isSnapshotStalled", () => {
  it("is true when running with stalled_at set", () => {
    expect(
      isSnapshotStalled({ status: "running", stalled_at: "2026-07-23T00:00:00Z" }),
    ).toBe(true);
  });

  it("is false when running with no stalled_at (healthy)", () => {
    expect(isSnapshotStalled({ status: "running", stalled_at: undefined })).toBe(
      false,
    );
  });

  it("is false when completed, even with a stalled_at value present", () => {
    expect(
      isSnapshotStalled({ status: "completed", stalled_at: "2026-07-23T00:00:00Z" }),
    ).toBe(false);
  });

  it("is false when failed, even with a stalled_at value present", () => {
    expect(
      isSnapshotStalled({ status: "failed", stalled_at: "2026-07-23T00:00:00Z" }),
    ).toBe(false);
  });

  it("is false when pending", () => {
    expect(
      isSnapshotStalled({ status: "pending", stalled_at: "2026-07-23T00:00:00Z" }),
    ).toBe(false);
  });
});

// GH #791 — the control plane records `attempt_error` separately from
// `error`: `error` keeps meaning the final failure reason only, so a reader
// (an API consumer, the MCP read tools, or this UI) never mistakes a running
// row that is merely being retried for a failed one. isSnapshotRetrying must
// therefore gate on BOTH status="running" AND a non-empty attempt_error.

function buildSnapshot(overrides: Partial<BackupSnapshot> = {}): BackupSnapshot {
  return {
    id: "snap-791",
    tenant_id: "tenant-1",
    site_id: "site-42",
    kind: "full",
    status: "running",
    created_at: "2026-09-29T00:00:00Z",
    updated_at: "2026-09-29T00:00:00Z",
    progress: {},
    ...overrides,
  };
}

/**
 * `attempt_error` is a plain field on the generated `BackupSnapshot` type
 * (the #791 API slice) — no cast needed to attach it.
 */
function buildSnapshotWithAttemptError(
  attemptError: string | undefined,
  overrides: Partial<BackupSnapshot> = {},
): BackupSnapshot {
  return buildSnapshot({ attempt_error: attemptError, ...overrides });
}

describe("isSnapshotRetrying", () => {
  it("is true when running with a non-empty attempt_error", () => {
    expect(
      isSnapshotRetrying({ status: "running", attempt_error: "The site did not answer in time." }),
    ).toBe(true);
  });

  it("is false when running with no attempt_error", () => {
    expect(isSnapshotRetrying({ status: "running", attempt_error: undefined })).toBe(false);
  });

  it("is false when running with an empty-string attempt_error (the DB default)", () => {
    expect(isSnapshotRetrying({ status: "running", attempt_error: "" })).toBe(false);
  });

  it("is false when completed, even with an attempt_error value present", () => {
    expect(
      isSnapshotRetrying({ status: "completed", attempt_error: "stale value" }),
    ).toBe(false);
  });

  it("is false when failed, even with an attempt_error value present", () => {
    expect(
      isSnapshotRetrying({ status: "failed", attempt_error: "stale value" }),
    ).toBe(false);
  });

  it("is false when pending", () => {
    expect(
      isSnapshotRetrying({ status: "pending", attempt_error: "stale value" }),
    ).toBe(false);
  });
});

// GH #791 contract test — pins `attempt_error` on the REAL generated
// `BackupSnapshot` type (the #791 API slice: "expose attempt_error on
// backups and schedule runs, and the retrying phase"). This literal object
// construction is checked by `pnpm -C apps/web typecheck`: no `as
// BackupSnapshot` cast, no `Record<string, unknown>` read — if the backend
// ever renames or drops the field, this fails to COMPILE before any runtime
// assertion runs. Mirrors `tags-contract.test.ts`'s pattern for pinning a
// generated shape.
describe("GH #791 contract — attempt_error on the real generated BackupSnapshot", () => {
  it("assigns directly, with no cast, and both helpers read it correctly", () => {
    const snapshot: BackupSnapshot = {
      id: "snap-real-shape",
      tenant_id: "tenant-1",
      site_id: "site-42",
      kind: "full",
      status: "running",
      created_at: "2026-09-29T00:00:00Z",
      updated_at: "2026-09-29T00:00:00Z",
      progress: {},
      attempt_error: "The site did not answer in time.",
    };
    expect(snapshotAttemptError(snapshot)).toBe("The site did not answer in time.");
    expect(isSnapshotRetrying(snapshot)).toBe(true);
  });
});

describe("snapshotAttemptError", () => {
  it("reads a non-empty attempt_error off the wire", () => {
    const snapshot = buildSnapshotWithAttemptError("Could not connect to the site.");
    expect(snapshotAttemptError(snapshot)).toBe("Could not connect to the site.");
  });

  it("returns null when attempt_error is absent", () => {
    expect(snapshotAttemptError(buildSnapshot())).toBeNull();
  });

  it("returns null when attempt_error is the empty-string DB default", () => {
    expect(snapshotAttemptError(buildSnapshotWithAttemptError(""))).toBeNull();
  });
});

// GH #791 — the "second bug": FailSnapshot publishes the failure reason on
// the wire as `phase_detail.error` (service.go), but this helper has always
// read `phase_detail.message`, which that path never sets — so a live
// failure reason never actually reached the UI. `.error` must be read as a
// fallback.
describe("formatProgress — errorMessage fallback (GH #791)", () => {
  it("reads phase_detail.message when present (existing behaviour)", () => {
    const snapshot = buildSnapshot({
      status: "failed",
      progress: { phase: "failed", phase_detail: { message: "from message" } },
    });
    expect(formatProgress(snapshot).errorMessage).toBe("from message");
  });

  it("falls back to phase_detail.error when message is absent", () => {
    const snapshot = buildSnapshot({
      status: "failed",
      progress: { phase: "failed", phase_detail: { error: "from error" } },
    });
    expect(formatProgress(snapshot).errorMessage).toBe("from error");
  });

  it("is null when neither phase_detail.message nor .error is present", () => {
    const snapshot = buildSnapshot({
      status: "failed",
      progress: { phase: "failed", phase_detail: {} },
    });
    expect(formatProgress(snapshot).errorMessage).toBeNull();
  });
});

// GH #791 adv-review nit 8 — `hasVisibleProgress` must not gate solely on
// `phase !== "queued"`: `formatProgress` falls back to "queued" for any
// phase id outside the closed PHASE_IDS set, but `phase_detail`'s counters
// are read off the wire regardless of whether the phase was recognised.
describe("hasVisibleProgress", () => {
  it("is false for a fresh queued snapshot with no counters", () => {
    const snapshot = buildSnapshot({
      status: "running",
      progress: { phase: "queued" },
    });
    expect(hasVisibleProgress(formatProgress(snapshot))).toBe(false);
  });

  it("is true once the phase has moved past queued", () => {
    const snapshot = buildSnapshot({
      status: "running",
      progress: { phase: "archiving_files", phase_detail: {} },
    });
    expect(hasVisibleProgress(formatProgress(snapshot))).toBe(true);
  });

  it("is true for an unrecognised phase id that still carries a files counter", () => {
    const snapshot = buildSnapshot({
      status: "running",
      progress: {
        phase: "some_future_phase_the_web_does_not_know",
        phase_detail: { files_done: 1200, files_total: 5000 },
      },
    });
    const fp = formatProgress(snapshot);
    expect(fp.phase).toBe("queued"); // unrecognised phase id falls back
    expect(fp.filesDone).toBe(1200); // but the counter still reads through
    expect(hasVisibleProgress(fp)).toBe(true);
  });

  it("is true for an unrecognised phase id that carries only a bytes counter", () => {
    const snapshot = buildSnapshot({
      status: "running",
      progress: {
        phase: "some_future_phase_the_web_does_not_know",
        phase_detail: { bytes_written: 4096 },
      },
    });
    expect(hasVisibleProgress(formatProgress(snapshot))).toBe(true);
  });

  it("is false for an unrecognised phase id whose counters are all zero", () => {
    const snapshot = buildSnapshot({
      status: "running",
      progress: {
        phase: "some_future_phase_the_web_does_not_know",
        phase_detail: { files_done: 0, files_total: 5000 },
      },
    });
    expect(hasVisibleProgress(formatProgress(snapshot))).toBe(false);
  });
});
