import { describe, it, expect } from "vitest";

import { formatRequestWindow } from "./request-window";

// created_at and expires_at cross the wire as RFC 3339 instants
// (apps/api/internal/abilityrequest/handler.go, RequestDTO: time.Time), and the
// control plane opens every approval window for 24 hours
// (apps/api/internal/mcp/ability_write.go abilityRequestWindow, and
// mcp/write_rail.go requestWindow).
//
// Nothing here depends on the runner's time zone. Where a case needs two times
// on one calendar day, or on two, the instants are built from local calendar
// fields, and every expectation is derived from the same instants.

const UNREADABLE = "an unreadable time";
const DAY_MS = 24 * 60 * 60 * 1000;

/** The wire form of a local wall-clock time. */
const local = (y: number, month: number, d: number, h: number, min: number): string =>
  new Date(y, month - 1, d, h, min).toISOString();

/** What the old card printed for any instant: the time of day, and nothing else. */
const timeOfDay = (iso: string): string =>
  new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });

const weekdayOf = (iso: string): string => new Intl.DateTimeFormat([], { weekday: "short" }).format(new Date(iso));
const monthOf = (iso: string): string => new Intl.DateTimeFormat([], { month: "short" }).format(new Date(iso));

describe("formatRequestWindow", () => {
  it("names the day a request closes when it closes on a later day", () => {
    const created = "2026-10-10T04:11:41Z";
    const expires = new Date(Date.parse(created) + DAY_MS).toISOString();
    const w = formatRequestWindow(created, expires);

    expect(w.asked).toBe(timeOfDay(created));
    expect(w.closes).not.toBe(w.asked);
    expect(w.closes).not.toBe(timeOfDay(expires));
    expect(w.closes).toContain(weekdayOf(expires));
    expect(w.closes).toContain(monthOf(expires));
    // The weekday of the ask is not the weekday of the close, so naming the
    // close's day tells the two apart.
    expect(weekdayOf(created)).not.toBe(weekdayOf(expires));
  });

  it("keeps a close on the ask's own day as a time of day alone", () => {
    const created = local(2026, 10, 10, 9, 55);
    const expires = local(2026, 10, 10, 11, 0);
    expect(formatRequestWindow(created, expires)).toEqual({
      asked: timeOfDay(created),
      closes: timeOfDay(expires),
    });
  });

  it("names the day when a short window crosses midnight", () => {
    const created = local(2026, 10, 10, 23, 30);
    const expires = local(2026, 10, 11, 0, 30);
    const w = formatRequestWindow(created, expires);
    expect(w.asked).toBe(timeOfDay(created));
    expect(w.closes).toContain(weekdayOf(expires));
    expect(w.closes).not.toBe(timeOfDay(expires));
  });

  it("names the day across the end of a month and of a year", () => {
    for (const [created, expires] of [
      [local(2026, 10, 31, 22, 0), local(2026, 11, 1, 22, 0)],
      [local(2026, 12, 31, 22, 0), local(2027, 1, 1, 22, 0)],
    ] as const) {
      const w = formatRequestWindow(created, expires);
      expect(w.closes).not.toBe(timeOfDay(expires));
      expect(w.closes).toContain(weekdayOf(expires));
      expect(w.closes).toContain(monthOf(expires));
    }
  });

  it("names the close's day when the ask cannot be read, since it cannot be placed on the ask's day", () => {
    const expires = local(2026, 10, 10, 11, 0);
    const w = formatRequestWindow("not a time", expires);
    expect(w.asked).toBe(UNREADABLE);
    expect(w.closes).not.toBe(timeOfDay(expires));
    expect(w.closes).toContain(weekdayOf(expires));
  });

  it("says an unreadable close is unreadable, never now or blank", () => {
    const created = local(2026, 10, 10, 9, 55);
    expect(formatRequestWindow(created, "")).toEqual({ asked: timeOfDay(created), closes: UNREADABLE });
    expect(formatRequestWindow("", "")).toEqual({ asked: UNREADABLE, closes: UNREADABLE });
  });
});
