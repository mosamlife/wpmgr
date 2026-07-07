import { describe, it, expect } from "vitest";

import { buildBreadcrumbCrumbs, humanize, ROUTELESS_SEGMENTS } from "./top-bar-helpers";

// ---------------------------------------------------------------------------
// GH #150 (#2 & #3) — breadcrumb 404 on routeless parents
//
// `/restores` and `/schedule-runs` have no index route (only `$restoreId`/
// `$runId` detail routes), so linkifying the parent segment 404s. These tests
// pin: (a) a routeless parent renders as a non-navigable crumb (`to: null`)
// even though it isn't the last segment, (b) an ordinary parent segment is
// still linkified, and (c) the humanize map covers both new labels.
// ---------------------------------------------------------------------------

describe("buildBreadcrumbCrumbs", () => {
  it("returns a single Home crumb for the root path", () => {
    expect(buildBreadcrumbCrumbs("/")).toEqual([{ label: "Home", to: null }]);
  });

  it("linkifies every non-last segment for an ordinary route", () => {
    expect(buildBreadcrumbCrumbs("/sites/abc123")).toEqual([
      { label: "Sites", to: "/sites" },
      { label: "abc123", to: null },
    ]);
  });

  it("does NOT linkify a routeless parent segment (restores)", () => {
    expect(buildBreadcrumbCrumbs("/restores/rst_1")).toEqual([
      { label: "Restores", to: null },
      { label: "rst_1", to: null },
    ]);
  });

  it("does NOT linkify a routeless parent segment (schedule-runs)", () => {
    expect(buildBreadcrumbCrumbs("/schedule-runs/run_42")).toEqual([
      { label: "Schedule runs", to: null },
      { label: "run_42", to: null },
    ]);
  });

  it("still linkifies a routeless segment's own ancestors normally", () => {
    // Hypothetical deeper nesting under a routeless segment: only the
    // routeless segment itself loses its link, ancestors before it are
    // unaffected.
    expect(buildBreadcrumbCrumbs("/sites/abc123/restores/rst_1")).toEqual([
      { label: "Sites", to: "/sites" },
      { label: "abc123", to: "/sites/abc123" },
      { label: "Restores", to: null },
      { label: "rst_1", to: null },
    ]);
  });

  it("the last segment is always non-navigable regardless of the allowlist", () => {
    const crumbs = buildBreadcrumbCrumbs("/settings");
    const last = crumbs.at(-1);
    expect(last?.to).toBeNull();
  });
});

describe("humanize", () => {
  it("maps restores to a title-cased human label", () => {
    expect(humanize("restores")).toBe("Restores");
  });

  it("maps schedule-runs to a human label", () => {
    expect(humanize("schedule-runs")).toBe("Schedule runs");
  });

  it("passes through an unmapped segment (e.g. a raw id) unchanged", () => {
    expect(humanize("rst_01hxyz")).toBe("rst_01hxyz");
  });
});

describe("ROUTELESS_SEGMENTS", () => {
  it("contains exactly the known routeless parents", () => {
    expect(ROUTELESS_SEGMENTS.has("restores")).toBe(true);
    expect(ROUTELESS_SEGMENTS.has("schedule-runs")).toBe(true);
    expect(ROUTELESS_SEGMENTS.has("sites")).toBe(false);
  });
});
