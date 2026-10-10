import { describe, it, expect } from "vitest";

import { abilityTiming, clockTime } from "./ability-card-model";

// The Timing row of a pending ability card. created_at and expires_at cross the
// wire as RFC 3339 instants (apps/api/internal/abilityrequest/handler.go,
// RequestDTO), and the control plane opens the approval window for 24 hours
// (apps/api/internal/mcp/ability_write.go, abilityRequestWindow).
//
// Nothing here depends on the runner's time zone: the instant used is on a
// different calendar day from the instant 24 hours later in every zone, and
// each expectation is derived from the same instants.

const DAY_MS = 24 * 60 * 60 * 1000;

/** The text after "closes " in a Timing row. */
function closeText(timing: string): string {
  const at = timing.indexOf(" · closes ");
  return at < 0 ? "" : timing.slice(at + " · closes ".length);
}

const weekdayOf = (iso: string): string => new Intl.DateTimeFormat([], { weekday: "short" }).format(new Date(iso));

describe("abilityTiming", () => {
  it("names the day a request closes when it closes on a later day", () => {
    const created_at = "2026-10-10T04:11:41Z";
    const expires_at = new Date(Date.parse(created_at) + DAY_MS).toISOString();
    const timing = abilityTiming({ created_at, expires_at });

    // The ask stays a time of day.
    expect(timing.startsWith(`Asked ${clockTime(created_at)} · closes `)).toBe(true);
    // The close is not the same words as the ask, nor the bare time it used to be.
    const closes = closeText(timing);
    expect(closes).not.toBe(clockTime(created_at));
    expect(closes).not.toBe(clockTime(expires_at));
    expect(timing).not.toBe(`Asked ${clockTime(created_at)} · closes ${clockTime(expires_at)}`);
    // It names the day, and the day it names is not the ask's.
    expect(closes).toContain(weekdayOf(expires_at));
    expect(weekdayOf(expires_at)).not.toBe(weekdayOf(created_at));
  });

  it("leaves a close on the ask's own day as a time of day", () => {
    const created_at = new Date(2026, 9, 10, 9, 55).toISOString();
    const expires_at = new Date(2026, 9, 10, 11, 0).toISOString();
    expect(abilityTiming({ created_at, expires_at })).toBe(
      `Asked ${clockTime(created_at)} · closes ${clockTime(expires_at)}`,
    );
  });
});
