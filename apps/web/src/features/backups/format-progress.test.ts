import { describe, it, expect } from "vitest";

import { isSnapshotStalled } from "./format-progress";

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
