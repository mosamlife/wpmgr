import { describe, it, expect } from "vitest";

import { isIncidentOngoing, formatIncidentDuration } from "./incident-format";

// ---------------------------------------------------------------------------
// GH #148 — incidents panel hardening
//
// Bug: the panel used to decide "ongoing" from `duration_seconds === null`
// and format duration unconditionally, which rendered "NaNh" whenever the
// backend sent a non-finite (undefined/NaN) duration that wasn't exactly
// `null`. These tests pin the hardened contract: `ongoing` boolean is the
// source of truth, `Number.isFinite` is the backstop, and the formatter can
// never emit "NaN" in any branch.
// ---------------------------------------------------------------------------

describe("isIncidentOngoing", () => {
  it("is ongoing when the API says ongoing=true, regardless of duration", () => {
    expect(isIncidentOngoing({ ongoing: true, duration_seconds: null })).toBe(true);
    expect(isIncidentOngoing({ ongoing: true, duration_seconds: 42 })).toBe(true);
  });

  it("is not ongoing when ongoing=false and duration is a finite number", () => {
    expect(isIncidentOngoing({ ongoing: false, duration_seconds: 531000 })).toBe(false);
    expect(isIncidentOngoing({ ongoing: false, duration_seconds: 0 })).toBe(false);
  });

  it("falls back to ongoing when duration is null even if ongoing=false (defensive)", () => {
    expect(isIncidentOngoing({ ongoing: false, duration_seconds: null })).toBe(true);
  });

  it("falls back to ongoing when duration is NaN/undefined (never trusts a bad payload)", () => {
    expect(
      isIncidentOngoing({
        ongoing: false,
        duration_seconds: Number.NaN,
      }),
    ).toBe(true);
    expect(
      isIncidentOngoing({
        ongoing: false,
        duration_seconds: undefined as unknown as number | null,
      }),
    ).toBe(true);
  });
});

describe("formatIncidentDuration", () => {
  it("returns null when ongoing (no duration to show)", () => {
    expect(formatIncidentDuration(531000, true)).toBeNull();
    expect(formatIncidentDuration(null, true)).toBeNull();
  });

  it("returns null (never NaN) when duration is not a finite number", () => {
    expect(formatIncidentDuration(null, false)).toBeNull();
    expect(formatIncidentDuration(Number.NaN, false)).toBeNull();
    expect(formatIncidentDuration(undefined, false)).toBeNull();
  });

  it("formats sub-minute durations in seconds", () => {
    expect(formatIncidentDuration(45, false)).toBe("45s");
  });

  it("formats sub-hour durations in whole minutes", () => {
    expect(formatIncidentDuration(150, false)).toBe("3m");
  });

  it("formats hour-scale durations with one decimal (the reported 147.5h case)", () => {
    expect(formatIncidentDuration(531_000, false)).toBe("147.5h");
  });

  it("formats a zero-second duration as 0s, not falsy-empty", () => {
    expect(formatIncidentDuration(0, false)).toBe("0s");
  });
});
