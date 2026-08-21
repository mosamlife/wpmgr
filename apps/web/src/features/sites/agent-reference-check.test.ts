import { describe, it, expect } from "vitest";
import type { AgentMirrorStatus } from "@wpmgr/api";

import { describeReferenceCheck } from "./agent-reference-check";

// GH #322: pins the exact operator-facing copy for the upstream
// agent-release mirror's freshness, mapped from the REAL control-plane
// contract (agentrelease/handler.go's agentMirrorDTO, generated as
// AgentMirrorStatus), not a hand-rolled shape. The load-bearing part: which
// states must NEVER be conflated. An age is only ever rendered from
// last_success_at, never last_attempt_at, and rate_limited must never read
// as a failure.

const hoursAgo = (h: number) => new Date(Date.now() - h * 3_600_000).toISOString();
const minutesAgo = (m: number) => new Date(Date.now() - m * 60_000).toISOString();

// Every "never recorded" field is nullable in the generated AgentMirrorStatus,
// so this builder needs no cast: the fixture below IS the wire shape the
// control plane sends (agentrelease/handler.go's formatMirrorTime and
// nonEmptyMirrorString both return a real JSON null before the first mirror
// run). If a null here ever stops typechecking, the spec has drifted from the
// handler and the spec is what to fix, not this file.
function mirror(overrides: Partial<AgentMirrorStatus> = {}): AgentMirrorStatus {
  return {
    enabled: true,
    status: "ok",
    stale_after_seconds: 46_800,
    can_check_now: false,
    last_success_at: hoursAgo(3),
    last_success_outcome: "unchanged",
    last_success_version: "0.61.112",
    last_attempt_at: hoursAgo(3),
    last_attempt_outcome: "unchanged",
    last_attempt_detail: null,
    last_attempt_trigger: "periodic",
    last_mirrored_at: null,
    last_mirrored_version: null,
    ...overrides,
  };
}

describe("describeReferenceCheck", () => {
  it("renders nothing when agent_mirror is undefined (rollup not loaded yet)", () => {
    expect(describeReferenceCheck(undefined, "published")).toBeUndefined();
    expect(describeReferenceCheck(undefined, undefined)).toBeUndefined();
  });

  it("renders nothing when disabled and the reference is published (hosted, or a self-host's own channel): there is no periodic check to time", () => {
    const m = mirror({ enabled: false, status: "disabled" });
    expect(describeReferenceCheck(m, "published")).toBeUndefined();
  });

  it("disabled with a published reference stays silent regardless of reference source nuance beyond 'published'", () => {
    const m = mirror({ enabled: false, status: "disabled" });
    expect(describeReferenceCheck(m, "published")).toBeUndefined();
  });

  it("disabled with a fleet or absent reference states the setting plainly, as info", () => {
    const m = mirror({ enabled: false, status: "disabled" });
    const msg = describeReferenceCheck(m, "fleet");
    expect(msg?.title).toBe("Release mirroring is off");
    expect(msg?.body).toBe(
      "WPMgr never checks upstream for a newer agent release on this install. Set WPMGR_UPDATE_AGENT_MIRROR_ENABLED to turn it on.",
    );
    expect(msg?.tone).toBe("info");
  });

  it("pending: enabled but no attempt has ever run", () => {
    const m = mirror({
      status: "pending",
      last_attempt_at: null,
      last_attempt_outcome: null,
      last_success_at: null,
      last_success_outcome: null,
      last_success_version: null,
    });
    const msg = describeReferenceCheck(m, "none");
    expect(msg?.title).toBe("First check has not run yet");
    expect(msg?.tone).toBe("info");
  });

  it("a fresh confirmed success (status ok) reads as info, and always ages from last_success_at", () => {
    const m = mirror({ status: "ok", last_attempt_outcome: "unchanged" });
    const msg = describeReferenceCheck(m, "published");
    expect(msg?.tone).toBe("info");
    expect(msg?.title).toBe("Reference checked 3h ago");
    expect(msg?.body).toBe(
      "The newest agent release upstream was 0.61.112. WPMgr checks again about every 6 hours.",
    );
  });

  it("a 'mirrored' outcome (a new release was actually published) reads the same as 'unchanged'/'current'", () => {
    const m = mirror({ status: "ok", last_attempt_outcome: "mirrored" });
    const msg = describeReferenceCheck(m, "published");
    expect(msg?.tone).toBe("info");
    expect(msg?.body).toContain("The newest agent release upstream was 0.61.112");
  });

  it("a 'current' outcome reads the same as 'unchanged'/'mirrored'", () => {
    const m = mirror({ status: "ok", last_attempt_outcome: "current" });
    const msg = describeReferenceCheck(m, "published");
    expect(msg?.tone).toBe("info");
    expect(msg?.body).toContain("The newest agent release upstream was 0.61.112");
  });

  it("stale: a confirmed success outside the threshold warns and names the reference version, still aged from last_success_at", () => {
    const m = mirror({ status: "stale", last_attempt_outcome: "unchanged" });
    const msg = describeReferenceCheck(m, "published");
    expect(msg?.tone).toBe("warn");
    expect(msg?.title).toBe("Reference checked 3h ago");
    expect(msg?.body).toBe(
      "That is longer than the usual 6 hour cycle, so a newer release may exist and WPMgr would not know. Sites shown as current are compared against 0.61.112.",
    );
  });

  it("a failed attempt with a fresh success (status ok) states BOTH ages separately and never merges them (the bug this issue is about)", () => {
    const m = mirror({
      status: "ok",
      last_success_at: hoursAgo(9),
      last_success_version: "0.61.112",
      last_attempt_at: minutesAgo(10),
      last_attempt_outcome: "upstream_unavailable",
    });
    const msg = describeReferenceCheck(m, "published");
    expect(msg?.title).toBe("Last check failed 10m ago");
    expect(msg?.body).toBe(
      "The reference is still 0.61.112, last confirmed 9h ago. A newer release may exist. WPMgr tries again on its next scheduled run.",
    );
    // Tone stays neutral: one failed attempt against a fresh success is
    // genuinely fine and must not cry wolf.
    expect(msg?.tone).toBe("info");
  });

  it("the same failed attempt escalates to warn once status is stale (the underlying success is itself old)", () => {
    const m = mirror({ status: "stale", last_attempt_outcome: "storage_error" });
    const msg = describeReferenceCheck(m, "published");
    expect(msg?.tone).toBe("warn");
    expect(msg?.title).toMatch(/^Last check failed /);
  });

  it("'refused' is a known mismatch (upstream answered, this install deliberately did not mirror it) and always warns", () => {
    const m = mirror({
      status: "ok",
      last_attempt_outcome: "refused",
      last_attempt_detail: "the upstream release is not newer than the version already mirrored",
    });
    const msg = describeReferenceCheck(m, "published");
    expect(msg?.title).toBe("Upstream release not mirrored");
    expect(msg?.body).toBe(
      "The last check reached upstream and did not mirror what it found: the upstream release is not newer than the version already mirrored. Sites are compared against 0.61.112, last confirmed 3h ago.",
    );
    expect(msg?.tone).toBe("warn");
  });

  it("'refused' with no detail recorded degrades to a generic sentence, never a literal 'null'", () => {
    const m = mirror({
      status: "ok",
      last_attempt_outcome: "refused",
      last_attempt_detail: null,
    });
    const msg = describeReferenceCheck(m, "published");
    expect(msg?.body).toContain("did not mirror what it found: the release was not accepted");
    expect(msg?.body).not.toMatch(/null/i);
  });

  it("'standing_down' (foreign_channel) is informational, never a warning: nothing is broken and the operator chose this", () => {
    const m = mirror({ status: "standing_down", last_attempt_outcome: "foreign_channel" });
    const msg = describeReferenceCheck(m, "published");
    expect(msg?.title).toBe("Mirroring stood down");
    expect(msg?.tone).toBe("info");
  });

  it("'misconfigured' (not_configured) always warns, regardless of any success history, and never self-heals", () => {
    const m = mirror({
      status: "misconfigured",
      last_attempt_outcome: "not_configured",
      last_attempt_detail: "object storage or the HTTP client is not configured",
    });
    const msg = describeReferenceCheck(m, "published");
    expect(msg?.title).toBe("Upstream mirror is misconfigured");
    expect(msg?.body).toContain("object storage or the HTTP client is not configured");
    expect(msg?.body).toContain("The reference is still 0.61.112, last confirmed 3h ago.");
    expect(msg?.tone).toBe("warn");
  });

  it("'misconfigured' with no success ever recorded omits the reference sentence rather than naming a version that never existed", () => {
    const m = mirror({
      status: "misconfigured",
      last_attempt_outcome: "not_configured",
      last_attempt_detail: "object storage or the HTTP client is not configured",
      last_success_at: null,
      last_success_outcome: null,
      last_success_version: null,
    });
    const msg = describeReferenceCheck(m, "published");
    expect(msg?.body).not.toContain("last confirmed");
    expect(msg?.tone).toBe("warn");
  });

  it("'rate_limited' explicitly states this is expected and not an error, and is info while the success is fresh (status ok)", () => {
    const m = mirror({ status: "ok", last_attempt_outcome: "rate_limited" });
    const msg = describeReferenceCheck(m, "published");
    expect(msg?.title).toBe("Last check was skipped");
    expect(msg?.body).toBe(
      "Upstream limits how often WPMgr may ask, so the last run did not get an answer. This is expected and not an error. The reference is 0.61.112, last confirmed 3h ago.",
    );
    expect(msg?.tone).toBe("info");
  });

  it("'rate_limited' still warns once status is stale, appending the six-hour-cycle note", () => {
    const m = mirror({ status: "stale", last_attempt_outcome: "rate_limited" });
    const msg = describeReferenceCheck(m, "published");
    expect(msg?.tone).toBe("warn");
    expect(msg?.body).toContain("This is expected and not an error.");
    expect(msg?.body).toContain(
      "That is longer than the usual 6 hour cycle, so a newer release may exist and WPMgr would not know.",
    );
  });

  it("stale with no success ever recorded warns immediately, with no staleness grace period", () => {
    const m = mirror({
      status: "stale",
      last_attempt_at: minutesAgo(5),
      last_attempt_outcome: "upstream_unavailable",
      last_attempt_detail: "the upstream release could not be reached",
      last_success_at: null,
      last_success_outcome: null,
      last_success_version: null,
    });
    const msg = describeReferenceCheck(m, "none");
    expect(msg?.title).toBe("No check has ever succeeded");
    expect(msg?.body).toBe(
      "Release mirroring is on, but no check has completed on this install. Last try 5m ago: the upstream release could not be reached. Until one succeeds there is no published reference version.",
    );
    expect(msg?.tone).toBe("warn");
  });

  it("reference_source 'none' with mirroring on prepends a lead line naming the cause", () => {
    const m = mirror({
      status: "stale",
      last_attempt_at: minutesAgo(5),
      last_attempt_outcome: "upstream_unavailable",
      last_success_at: null,
      last_success_outcome: null,
      last_success_version: null,
    });
    const msg = describeReferenceCheck(m, "none");
    expect(msg?.lead).toBe("WPMgr has no reference agent version yet.");
    expect(msg?.title).toBe("No check has ever succeeded");
  });

  it("does not add the 'none' lead line when a reference version does exist (published)", () => {
    const m = mirror();
    const msg = describeReferenceCheck(m, "published");
    expect(msg?.lead).toBeUndefined();
  });

  it("does not add the 'none' lead line for a fleet-derived reference (only 'none' triggers it)", () => {
    const m = mirror();
    const msg = describeReferenceCheck(m, "fleet");
    expect(msg?.lead).toBeUndefined();
  });
});
