import { describe, it, expect } from "vitest";

import {
  isIncidentOngoing,
  formatIncidentDuration,
  humanizeIncidentReason,
} from "./incident-format";

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

describe("humanizeIncidentReason", () => {
  it("maps the two fixed fatal-detection codes to readable copy", () => {
    expect(humanizeIncidentReason("wp_fatal_error")).toBe(
      "WordPress fatal error page",
    );
    expect(humanizeIncidentReason("wp_db_error")).toBe(
      "Database connection error page",
    );
  });

  it("is case-insensitive on the fixed codes", () => {
    expect(humanizeIncidentReason("WP_FATAL_ERROR")).toBe(
      "WordPress fatal error page",
    );
  });

  it("extracts the status code from an 'http status NNN' reason", () => {
    expect(humanizeIncidentReason("http status 500")).toBe(
      "Site returned HTTP 500",
    );
    expect(humanizeIncidentReason("http status 503")).toBe(
      "Site returned HTTP 503",
    );
  });

  it("recognizes an ssrf_blocked prefix", () => {
    expect(humanizeIncidentReason("ssrf_blocked: dial tcp 10.0.0.1:443")).toBe(
      "Blocked by outbound security policy",
    );
  });

  it("recognizes a timeout / deadline-exceeded transport error", () => {
    expect(
      humanizeIncidentReason("context deadline exceeded (Client.Timeout)"),
    ).toBe("Connection timed out");
    expect(humanizeIncidentReason("i/o timeout")).toBe("Connection timed out");
  });

  it("recognizes a connection-refused transport error", () => {
    expect(
      humanizeIncidentReason("dial tcp 127.0.0.1:443: connection refused"),
    ).toBe("Connection refused");
  });

  it("recognizes a DNS failure", () => {
    expect(
      humanizeIncidentReason("dial tcp: lookup example.com: no such host"),
    ).toBe("DNS lookup failed");
  });

  it("recognizes a TLS/certificate error", () => {
    expect(
      humanizeIncidentReason("x509: certificate has expired or is not yet valid"),
    ).toBe("TLS certificate error");
  });

  it("falls back to the raw reason for anything unrecognized (never drops the signal)", () => {
    expect(humanizeIncidentReason("some brand-new prober reason")).toBe(
      "some brand-new prober reason",
    );
  });

  it("returns a calm default for an empty/absent reason", () => {
    expect(humanizeIncidentReason("")).toBe("No specific cause recorded");
    expect(humanizeIncidentReason(null)).toBe("No specific cause recorded");
    expect(humanizeIncidentReason(undefined)).toBe("No specific cause recorded");
  });
});
