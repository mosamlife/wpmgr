import { describe, expect, it } from "vitest";

import {
  SCHEDULE_MAX_LEAD_DAYS,
  SCHEDULE_SKEW_GRACE_MS,
  formatAbsolute,
  formatCountdown,
  toLocalInputValue,
  validateSchedule,
} from "./schedule";

const NOW = Date.UTC(2026, 7, 19, 6, 0, 0); // 2026-08-19T06:00:00Z

describe("formatCountdown", () => {
  it("counts down in days and hours past a day out", () => {
    expect(formatCountdown(new Date(NOW + 3 * 86400_000 + 4 * 3600_000).toISOString(), NOW)).toBe(
      "3d 4h",
    );
  });

  it("counts down in hours and minutes inside a day", () => {
    expect(
      formatCountdown(new Date(NOW + 8 * 3600_000 + 42 * 60_000).toISOString(), NOW),
    ).toBe("8h 42m");
  });

  it("counts down in minutes, then seconds, as it closes in", () => {
    expect(formatCountdown(new Date(NOW + 5 * 60_000).toISOString(), NOW)).toBe("5m");
    expect(formatCountdown(new Date(NOW + 45_000).toISOString(), NOW)).toBe("45s");
  });

  // A countdown that runs negative is how an operator learns not to trust the
  // number. Null is the caller's signal to stop saying "starts in".
  it("returns null once the instant has passed", () => {
    expect(formatCountdown(new Date(NOW - 1000).toISOString(), NOW)).toBeNull();
    expect(formatCountdown(new Date(NOW).toISOString(), NOW)).toBeNull();
  });

  it("returns null rather than NaN for an unparseable instant", () => {
    expect(formatCountdown("not a date", NOW)).toBeNull();
  });
});

describe("formatAbsolute", () => {
  // The defect: the run page printed the raw ISO string, so an operator was
  // never told which zone the time was in.
  it("always names the zone the time is expressed in", () => {
    const out = formatAbsolute("2026-08-19T02:00:00Z", "Australia/Sydney");
    expect(out).toContain("Australia/Sydney");
    // 02:00 UTC is 12:00 the same day in Sydney (AEST, UTC+10).
    expect(out).toContain("12:00");
    expect(out).toContain("19");
  });

  it("renders the same instant differently in two zones", () => {
    const iso = "2026-08-19T02:00:00Z";
    expect(formatAbsolute(iso, "Europe/London")).not.toBe(
      formatAbsolute(iso, "Australia/Sydney"),
    );
  });

  it("returns the input unchanged rather than 'Invalid Date'", () => {
    expect(formatAbsolute("nonsense", "UTC")).toBe("nonsense");
  });
});

describe("validateSchedule", () => {
  it("accepts an empty value: no schedule means run now", () => {
    expect(validateSchedule("", NOW)).toBeNull();
  });

  it("accepts a future time inside the cap", () => {
    expect(
      validateSchedule(toLocalInputValue(new Date(NOW + 3600_000)), NOW),
    ).toBeNull();
  });

  // Mirrors apps/api/internal/update/service.go:319, which refuses with
  // domain.Validation("schedule_in_past", ...).
  it("refuses a past time with the server's own code", () => {
    const problem = validateSchedule(
      toLocalInputValue(new Date(NOW - 60 * 60_000)),
      NOW,
    );
    expect(problem?.code).toBe("schedule_in_past");
  });

  // scheduleSkewGrace (service.go:289) is 2 minutes: the instant is built
  // from a browser clock, so "now" routinely lands slightly behind the
  // server's. A client bound stricter than the server would refuse a
  // schedule the API accepts, which is the one direction this must not fail.
  it("tolerates the server's clock-skew grace instead of refusing inside it", () => {
    const justBehind = new Date(NOW - (SCHEDULE_SKEW_GRACE_MS - 1000));
    expect(validateSchedule(toLocalInputValue(justBehind), NOW)).toBeNull();
  });

  // Asserting only at the boundary value would hold for ANY grace: widen it to
  // an hour or narrow it to a second and the test above still passes, because
  // it never probes the other side. These two pin the transition itself, so a
  // change to SCHEDULE_SKEW_GRACE_MS that stops matching the server's
  // scheduleSkewGrace (service.go:289) reddens one of them.
  //
  // The `datetime-local` control has minute resolution, so the probes step a
  // whole minute either side rather than a second: a sub-minute offset would
  // be erased by toLocalInputValue and both cases would collapse onto the
  // same instant, which is the shape that made the original assertion vacuous.
  it("accepts a time a minute INSIDE the skew grace", () => {
    const inside = new Date(NOW - SCHEDULE_SKEW_GRACE_MS + 60_000);
    expect(validateSchedule(toLocalInputValue(inside), NOW)).toBeNull();
  });

  it("refuses a time a minute OUTSIDE the skew grace", () => {
    const outside = new Date(NOW - SCHEDULE_SKEW_GRACE_MS - 60_000);
    expect(validateSchedule(toLocalInputValue(outside), NOW)?.code).toBe(
      "schedule_in_past",
    );
  });

  // Mirrors service.go:322, domain.Validation("schedule_too_far", ...).
  it("refuses beyond the 30-day cap with the server's own code", () => {
    const tooFar = new Date(NOW + (SCHEDULE_MAX_LEAD_DAYS + 1) * 86400_000);
    expect(validateSchedule(toLocalInputValue(tooFar), NOW)?.code).toBe(
      "schedule_too_far",
    );
  });

  it("accepts a time just inside the cap", () => {
    const nearly = new Date(NOW + (SCHEDULE_MAX_LEAD_DAYS - 1) * 86400_000);
    expect(validateSchedule(toLocalInputValue(nearly), NOW)).toBeNull();
  });

  it("reports an unparseable value rather than passing it through", () => {
    expect(validateSchedule("13:00 tomorrow", NOW)?.code).toBe(
      "schedule_unparseable",
    );
  });
});

describe("toLocalInputValue", () => {
  // toISOString() would shift to UTC and silently move the operator's time.
  it("formats in LOCAL time, not UTC", () => {
    const at = new Date(2026, 7, 19, 2, 0, 0);
    expect(toLocalInputValue(at)).toBe("2026-08-19T02:00");
  });

  it("zero-pads every component", () => {
    expect(toLocalInputValue(new Date(2026, 0, 5, 9, 7))).toBe("2026-01-05T09:07");
  });
});
