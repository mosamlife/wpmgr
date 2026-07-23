import { describe, it, expect } from "vitest";

import { backupEventSchema, isStallHintPhase } from "./use-backup-stream";

// GH #279 — the CP two-tier watchdog publishes "stalled"/"resumed" SSE
// frames on the existing backup Hub (see hub.go / service.go). These are
// hints, not pipeline phases: unlike `use-updates.ts`'s `applyEvent` reducer
// (which patches every field it's given), the backup stream must NOT let a
// hint frame clobber the cached `progress.phase` — the pull-refetch it
// triggers instead is exercised in the render-level indicator tests
// (`stalled-hint.test.ts` covers the gating; the SSE wiring is asserted here
// at the two decidable units: schema acceptance and phase classification).

describe("backupEventSchema — GH #279 stall hints", () => {
  it("accepts a 'stalled' frame (status stays running) instead of dropping it as malformed", () => {
    const frame = {
      snapshot_id: "snap-1",
      phase: "stalled",
      phase_detail: {},
      status: "running",
      ts: "2026-07-23T00:00:00Z",
    };
    expect(() => backupEventSchema.parse(frame)).not.toThrow();
  });

  it("accepts a 'resumed' frame (status stays running) instead of dropping it as malformed", () => {
    const frame = {
      snapshot_id: "snap-1",
      phase: "resumed",
      phase_detail: {},
      status: "running",
      ts: "2026-07-23T00:01:00Z",
    };
    expect(() => backupEventSchema.parse(frame)).not.toThrow();
  });
});

describe("isStallHintPhase", () => {
  it("classifies 'stalled' and 'resumed' as hints", () => {
    expect(isStallHintPhase("stalled")).toBe(true);
    expect(isStallHintPhase("resumed")).toBe(true);
  });

  it("classifies real pipeline phases as NOT hints, so they still patch the cache", () => {
    expect(isStallHintPhase("archiving_files")).toBe(false);
    expect(isStallHintPhase("encrypting_uploading")).toBe(false);
    expect(isStallHintPhase("completed")).toBe(false);
    expect(isStallHintPhase("failed")).toBe(false);
  });
});
