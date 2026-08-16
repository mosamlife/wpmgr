import { describe, it, expect } from "vitest";
import type { Site } from "@wpmgr/api";

import {
  MONITORING_PAUSE_CONTINUES,
  MONITORING_PAUSE_SCOPE_SENTENCE,
  MONITORING_PAUSE_STOPS,
  fleetCountLabel,
  uptimeBadgeFor,
  isMonitoringPaused,
  monitoringDetailMessage,
  monitoringMenuFor,
  monitoringRequestErrorMessage,
  pausedBadgeFor,
  pausedCount,
  refusedSitesSentence,
  splitByPauseState,
  summarizeMonitoringResult,
} from "./monitoring-pause";

// GH #414 phase 4b. Before this feature there was no coverage of these
// surfaces at all, so this file pins every decision the interface makes:
// what the paused badge says, what the uptime badge is ALLOWED to say while
// paused (health_status is untouched and unmuted; see monitoring-pause.ts's
// file header), what the fleet count reports, which menu items a mixed
// selection offers, and that the dialog copy names backups as continuing.

const NOW = Date.parse("2026-08-15T12:00:00Z");

function buildSite(overrides: Partial<Site> = {}): Site {
  return {
    id: "site-1",
    tenant_id: "tenant-1",
    url: "https://example.com",
    name: "Example",
    status: "active",
    wp_version: "6.8",
    php_version: "8.3",
    health_status: "healthy",
    multisite: false,
    tags: [],
    ...overrides,
  } as unknown as Site;
}

function pausedSite(overrides: Partial<Site> = {}): Site {
  return buildSite({
    monitoring_paused_at: "2026-08-15T10:00:00Z",
    ...overrides,
  });
}

describe("isMonitoringPaused", () => {
  it("reads the timestamp, since there is no boolean on the wire", () => {
    expect(isMonitoringPaused(buildSite())).toBe(false);
    expect(isMonitoringPaused(pausedSite())).toBe(true);
  });
});

// ── 1. The paused badge ────────────────────────────────────────────────────

describe("pausedBadgeFor", () => {
  it("renders nothing for an active site", () => {
    expect(pausedBadgeFor(buildSite(), () => null, NOW)).toBeNull();
  });

  it("names who paused it, when, and the reason", () => {
    const view = pausedBadgeFor(
      pausedSite({
        monitoring_paused_by: "user-7",
        monitoring_paused_reason: "Migrating to the new host",
      }),
      (id) => (id === "user-7" ? "Dana Ruiz" : null),
      NOW,
    );
    expect(view).not.toBeNull();
    expect(view?.label).toBe("Monitoring paused");
    expect(view?.title).toContain("by Dana Ruiz");
    expect(view?.title).toContain("2h ago");
    expect(view?.title).toContain("Reason: Migrating to the new host");
  });

  it("says it stays paused when there is no resume time", () => {
    const view = pausedBadgeFor(pausedSite(), () => null, NOW);
    expect(view?.title).toContain("Stays paused until someone resumes it.");
  });

  it("names the resume time when the pause has one", () => {
    const view = pausedBadgeFor(
      pausedSite({ monitoring_resume_at: "2026-08-16T12:00:00Z" }),
      () => null,
      NOW,
    );
    expect(view?.title).toContain("Resumes automatically in 1d.");
  });

  it("omits the actor when the pausing user was deleted or was an API key", () => {
    // paused_by is ON DELETE SET NULL, and an API-key actor has no users row.
    const view = pausedBadgeFor(pausedSite(), () => null, NOW);
    expect(view?.title).not.toContain(" by ");
  });

  it("carries the scope sentence into the hover text", () => {
    const view = pausedBadgeFor(pausedSite(), () => null, NOW);
    expect(view?.title).toContain(MONITORING_PAUSE_SCOPE_SENTENCE);
  });
});

// ── 2. The uptime badge must not lie ───────────────────────────────────────
//
// health_status is deliberately absent from every case here: it has two
// writers (the connection sweep, the health-check worker) that never stop
// under pause, so it is never muted or dated. Only site.up / site.uptime_pct
// — the uptime prober's own result — freezes; see monitoring-pause.ts's file
// header for the full mechanism.

describe("uptimeBadgeFor", () => {
  it("is a confident green Up while monitoring is ACTIVE", () => {
    const view = uptimeBadgeFor(buildSite({ up: true }), NOW);
    expect(view.label).toBe("Up");
    expect(view.tone).toBe("success");
    expect(view.pulse).toBe(true);
    expect(view.time).toBeNull();
  });

  it("NEVER shows a confident up state while paused", () => {
    const view = uptimeBadgeFor(
      pausedSite({ up: true, health_checked_at: "2026-08-15T09:00:00Z" }),
      NOW,
    );
    // The whole point of the phase: no green, no pulse, and an explicit stamp.
    expect(view.tone).not.toBe("success");
    expect(view.tone).toBe("muted");
    expect(view.pulse).toBe(false);
  });

  it("keeps the last verdict but dates it with health_checked_at", () => {
    const view = uptimeBadgeFor(
      pausedSite({ up: true, health_checked_at: "2026-08-15T09:00:00Z" }),
      NOW,
    );
    expect(view.label).toBe("Up");
    expect(view.time).toBe("as of 3h ago");
    expect(view.description).toContain("not being refreshed");
  });

  it("renders a never-probed paused site as Not checked, never as now", () => {
    const view = uptimeBadgeFor(pausedSite(), NOW);
    expect(view.label).toBe("Not checked");
    expect(view.tone).toBe("muted");
    expect(view.time).toBeNull();
    expect(view.label).not.toBe("Up");
  });

  it("drops the red from a paused down site too", () => {
    // Symmetry matters: a stale "Down" is as much a lie as a stale "Up" once
    // the prober stopped confirming it.
    const view = uptimeBadgeFor(
      pausedSite({
        up: false,
        health_checked_at: "2026-08-15T09:00:00Z",
      }),
      NOW,
    );
    expect(view.tone).toBe("muted");
    expect(view.label).toBe("Down");
    expect(view.time).toBe("as of 3h ago");
  });

  it("never reads health_status — it stays live and out of scope here", () => {
    // A paused site whose health_status disagrees with its uptime state
    // (the real-world case this whole fix exists for) must not change what
    // uptimeBadgeFor returns.
    const withHealthy = uptimeBadgeFor(
      pausedSite({ up: false, health_status: "healthy" }),
      NOW,
    );
    const withUnreachable = uptimeBadgeFor(
      pausedSite({ up: false, health_status: "unreachable" }),
      NOW,
    );
    expect(withHealthy).toEqual(withUnreachable);
  });
});

// ── 3. The fleet count ─────────────────────────────────────────────────────

describe("fleetCountLabel", () => {
  it("reports the paused number alongside the total", () => {
    expect(fleetCountLabel(34, 2)).toBe("34 sites enrolled, 2 paused");
  });

  it("stays exactly as it was when nothing is paused", () => {
    expect(fleetCountLabel(34, 0)).toBe("34 sites enrolled");
    expect(fleetCountLabel(1, 0)).toBe("1 site enrolled");
  });

  it("counts the paused sites out of a real list", () => {
    const sites = [buildSite(), pausedSite(), pausedSite(), buildSite()];
    expect(pausedCount(sites)).toBe(2);
    expect(fleetCountLabel(sites.length, pausedCount(sites))).toBe(
      "4 sites enrolled, 2 paused",
    );
  });
});

// ── 4. The menu toggles ────────────────────────────────────────────────────

describe("monitoringMenuFor", () => {
  it("offers Resume only, for an all-paused selection", () => {
    const menu = monitoringMenuFor([pausedSite(), pausedSite()]);
    expect(menu.pause).toBeNull();
    expect(menu.resume?.label).toBe("Resume monitoring on 2 sites");
    expect(menu.resume?.count).toBe(2);
  });

  it("offers Pause only, for an all-active selection", () => {
    const menu = monitoringMenuFor([buildSite(), buildSite(), buildSite()]);
    expect(menu.resume).toBeNull();
    expect(menu.pause?.label).toBe("Pause monitoring on 3 sites...");
    expect(menu.pause?.count).toBe(3);
  });

  it("offers BOTH for a mixed selection, each with its own count", () => {
    const menu = monitoringMenuFor([
      buildSite(),
      buildSite(),
      buildSite(),
      pausedSite(),
      pausedSite(),
    ]);
    // Five selected; neither item claims five.
    expect(menu.pause?.count).toBe(3);
    expect(menu.resume?.count).toBe(2);
    expect(menu.pause?.label).toContain("3 sites");
    expect(menu.resume?.label).toContain("2 sites");
    expect(menu.pause?.label).not.toContain("5");
    expect(menu.resume?.label).not.toContain("5");
  });

  it("singularises each count independently", () => {
    const menu = monitoringMenuFor([buildSite(), pausedSite()]);
    expect(menu.pause?.label).toBe("Pause monitoring on 1 site...");
    expect(menu.resume?.label).toBe("Resume monitoring on 1 site");
  });

  it("splits the selection so each request only sends its own half", () => {
    const active = buildSite({ id: "a" });
    const paused = pausedSite({ id: "p" });
    const split = splitByPauseState([active, paused]);
    expect(split.active.map((s) => s.id)).toEqual(["a"]);
    expect(split.paused.map((s) => s.id)).toEqual(["p"]);
  });
});

// ── 5. The scope copy ──────────────────────────────────────────────────────

describe("the pause scope copy", () => {
  it("names BACKUPS as continuing", () => {
    // The single most important string in this phase. Backups silently
    // stopping is the one failure people do not recover from.
    expect(MONITORING_PAUSE_CONTINUES).toContain("Backups");
    expect(MONITORING_PAUSE_CONTINUES).toContain("keep running");
    expect(MONITORING_PAUSE_SCOPE_SENTENCE).toContain("Backups");
  });

  it("names connection tracking as continuing", () => {
    expect(MONITORING_PAUSE_CONTINUES).toContain("connection tracking");
    expect(MONITORING_PAUSE_SCOPE_SENTENCE).toContain("connection tracking");
  });

  it("names what actually stops: uptime checks/alerts and SCHEDULED screenshots/rescans only", () => {
    expect(MONITORING_PAUSE_STOPS).toContain("Uptime checks");
    expect(MONITORING_PAUSE_STOPS).toContain("uptime alerts");
    expect(MONITORING_PAUSE_STOPS).toContain("scheduled screenshots");
    expect(MONITORING_PAUSE_STOPS).toContain("scheduled vulnerability rescans");
    expect(MONITORING_PAUSE_STOPS.toLowerCase()).not.toContain("backup");
  });

  it("does NOT claim update checks or scans stop: both are operator-only with no scheduled producer to pause", () => {
    // RefreshInventoryArgs has exactly two producers, both operator-driven
    // (cmd/wpmgr/siteadapter.go:167 Refresh click, internal/update/worker.go:971
    // post-update), and ScanRunArgs has exactly one (internal/scan/service.go:58,
    // the operator clicking Scan). Neither has a scheduled producer, so pause
    // has nothing to filter and both belong on "continues".
    expect(MONITORING_PAUSE_STOPS).not.toContain("update checks");
    // Word-boundary match: "rescans" legitimately appears (vulnerability
    // rescans DO stop), but the standalone word "scans" must not.
    expect(MONITORING_PAUSE_STOPS.toLowerCase()).not.toMatch(/\bscans\b/);
    expect(MONITORING_PAUSE_CONTINUES).toContain("update checks");
    expect(MONITORING_PAUSE_CONTINUES).toContain("scans");
  });
});

// ── 6. The error codes ─────────────────────────────────────────────────────

describe("monitoringRequestErrorMessage", () => {
  // Each of these is a code the Go handler actually returns:
  // request_too_large + too_many_sites + site_ids_required from
  // internal/site/monitoring_handler.go, principal_required from
  // internal/site/monitoring.go:192, resume_at_in_past from the published 422
  // description on PauseSiteMonitoringErrors.
  it.each([
    ["request_too_large", "too large"],
    ["too_many_sites", "at most 200"],
    ["site_ids_required", "No sites"],
    ["principal_required", "Sign in again"],
    ["resume_at_in_past", "in the future"],
  ])("gives %s its own message", (code, fragment) => {
    const message = monitoringRequestErrorMessage(code);
    expect(message).not.toBeNull();
    expect(message).toContain(fragment);
  });

  it("gives each code a DISTINCT message rather than one generic failure", () => {
    const codes = [
      "request_too_large",
      "too_many_sites",
      "site_ids_required",
      "principal_required",
      "resume_at_in_past",
    ];
    const messages = codes.map((c) => monitoringRequestErrorMessage(c));
    expect(new Set(messages).size).toBe(codes.length);
  });

  it("returns null for a code it does not know, so the server's own text wins", () => {
    expect(monitoringRequestErrorMessage("invalid_body")).toBeNull();
  });
});

describe("monitoringDetailMessage", () => {
  // site_archived / site_revoked arrive on a 200 with ok:false, pinned by
  // apps/api/tests/gh414_monitoring_guards_test.go:214-215.
  it.each([
    ["site_archived", "archived"],
    ["site_revoked", "revoked"],
    ["site_not_found", "no longer exists"],
    ["invalid_site_id", "could not be identified"],
    ["forbidden", "outside your site access"],
  ])("gives the %s outcome its own message", (detail, fragment) => {
    expect(monitoringDetailMessage(detail)).toContain(fragment);
  });

  it("has no message for a success detail", () => {
    expect(monitoringDetailMessage("paused")).toBeNull();
    expect(monitoringDetailMessage("already_paused")).toBeNull();
  });
});

describe("principal_required does not overclaim (review finding, monitoring-pause.ts:293)", () => {
  // The Go service only ever confirms "an authenticated principal is
  // required" (apps/api/internal/site/monitoring.go:192,229) — it never
  // learns or reports WHY there is no principal. "Your session has expired"
  // asserted a specific, unconfirmed cause; a stale tab that never had a
  // session, or a revoked session, would be told the same wrong story.
  it("names the remedy without asserting the unconfirmed cause of an expired session", () => {
    const message = monitoringRequestErrorMessage("principal_required");
    expect(message).not.toMatch(/expired/i);
    expect(message).toContain("Sign in again");
  });
});

describe("refusedSitesSentence — grammatical for every detail code (review finding, sites/index.tsx:876)", () => {
  // GH #414 published nine `detail` values on MonitoringResult
  // (apps/api/internal/site/monitoring_handler.go:60-63); the toast used to
  // glue every one of them onto a fixed "the site is" stem, which produced
  // "the site is no longer exists" and "the site is could not be identified"
  // for two of them.
  const KNOWN_DETAILS = [
    "site_archived",
    "site_revoked",
    "site_not_found",
    "invalid_site_id",
    "forbidden",
  ];

  it.each(KNOWN_DETAILS)("reads as one grammatical sentence for %s", (detail) => {
    const message = monitoringDetailMessage(detail) ?? "the site could not be changed";
    const sentence = refusedSitesSentence([{ message }]);
    expect(sentence.startsWith("Skipped because the site")).toBe(true);
    expect(sentence).not.toMatch(/site is no longer exists/);
    expect(sentence).not.toMatch(/site is could not/);
    expect(sentence).not.toMatch(/\bis is\b/);
    expect(sentence.endsWith(".")).toBe(true);
  });

  it("handles a detail code this client build has not seen without producing nonsense", () => {
    // e.g. `site_expired` — a code the server could start sending tomorrow
    // that the client bundle was built before.
    const message = monitoringDetailMessage("site_expired") ?? "the site could not be changed";
    expect(refusedSitesSentence([{ message }])).toBe(
      "Skipped because the site could not be changed.",
    );
  });
});

describe("summarizeMonitoringResult", () => {
  it("separates moved, already-in-state and refused", () => {
    const summary = summarizeMonitoringResult([
      { site_id: "a", ok: true, changed: true, detail: "paused" },
      { site_id: "b", ok: true, changed: false, detail: "already_paused" },
      { site_id: "c", ok: false, changed: false, detail: "site_archived" },
    ]);
    expect(summary.changed).toBe(1);
    expect(summary.unchanged).toBe(1);
    expect(summary.refused).toHaveLength(1);
    expect(summary.refused[0]?.message).toContain("archived");
  });

  it("falls back rather than dropping an unknown refusal on the floor", () => {
    const summary = summarizeMonitoringResult([
      { site_id: "a", ok: false, changed: false, detail: "something_new" },
    ]);
    expect(summary.refused[0]?.message).toBe("the site could not be changed");
  });

  // `ok` and `changed` are independent booleans on the wire — see
  // MonitoringResult in packages/openapi-client/src/generated/types.gen.ts:402-413
  // — not a discriminated union, so nothing in the published type stops a
  // server from ever pairing changed:true with ok:false. The current handler
  // happens to maintain the invariant by construction (every
  // monitoringResultDTO{} literal that sets Changed also sets OK: true; see
  // apps/api/internal/site/monitoring_handler.go:340-343 versus the four
  // rejection/not-found/refusal literals at lines 277, 285, 322 and 331,
  // none of which ever sets Changed), but that invariant is not enforced by
  // the type, so the summary must not trust `changed` on its own. Without the
  // `ok` half of the conjunct at monitoring-pause.ts:346, this fixture would
  // be folded into `changed` and a refused site would be reported to the user
  // as paused.
  it("treats a refusal as refused even if `changed` is set, never as a success", () => {
    const summary = summarizeMonitoringResult([
      { site_id: "a", ok: false, changed: true, detail: "site_archived" },
    ]);
    expect(summary.changed).toBe(0);
    expect(summary.unchanged).toBe(0);
    expect(summary.refused).toHaveLength(1);
    expect(summary.refused[0]?.siteId).toBe("a");
    expect(summary.refused[0]?.message).toContain("archived");
  });
});
