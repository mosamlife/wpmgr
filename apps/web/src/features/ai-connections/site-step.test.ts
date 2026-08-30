import { describe, it, expect } from "vitest";

import {
  isScopeApprovable,
  resolveSiteScope,
  snapshotFromPage,
  type FleetSnapshot,
  type ResolvedSiteScope,
  type ScopedSite,
} from "@/features/mcp-consent/site-scope";

import {
  SITE_STEP_MODES,
  canLeaveSiteStep,
  emptyTokenFieldLabel,
  excludedSitesLabel,
  fleetTotal,
  scopeCountLabel,
  scopeTokenLabel,
  siteStepBlockedReason,
  sitesInScope,
} from "./site-step";

// Step 3's arithmetic, which is the half of the screen that can lie with a
// number rather than with a sentence.
//
// NO EXPECTED STRING IS FROZEN HERE. Every assertion is either a number, a
// property of the string (contains this digit, contains no digit, does not
// contain this word), or a string built from the same function under test with
// different inputs. Freezing prose is how five separate tests on this surface
// ended up asserting that the copy had not changed rather than that it was
// true, and a reworded sentence should not redden a suite that is about
// arithmetic.

function site(n: number): ScopedSite {
  return { id: `s${n}`, name: `site-${n}.example`, url: `https://site-${n}.example` };
}

function fleetOf(count: number, limit: number): FleetSnapshot {
  return snapshotFromPage(
    Array.from({ length: count }, (_, i) => site(i)),
    limit,
  );
}

const NO_TAGS: Readonly<Record<string, readonly string[]>> = {};

function resolve(over: {
  mode: "all" | "tags" | "list";
  tagNames?: readonly string[];
  siteIds?: readonly string[];
  fleet: FleetSnapshot | null;
  tagsBySiteId?: Readonly<Record<string, readonly string[]>>;
  sitesLoading?: boolean;
}): ResolvedSiteScope {
  return resolveSiteScope({
    mode: over.mode,
    selectedTagNames: over.tagNames ?? [],
    selectedSiteIds: over.siteIds ?? [],
    fleet: over.fleet,
    tagsBySiteId: over.tagsBySiteId ?? NO_TAGS,
    sitesLoading: over.sitesLoading ?? false,
  });
}

describe("the denominator never invents a fleet size", () => {
  it("reports unknown, not zero, when the fleet did not load", () => {
    // A failed load rendered as "0 of 0" is the failure-as-empty defect. The
    // total must have no number in it at all.
    expect(fleetTotal(null)).toEqual({ kind: "unknown" });
  });

  it("reports an exact total only when the page came back short", () => {
    const total = fleetTotal(fleetOf(60, 200));
    expect(total).toEqual({ kind: "exact", n: 60 });
  });

  it("reports a floor, not an exact total, when the page came back full", () => {
    // 200 rows out of a 200-row request is indistinguishable from 200 out of
    // 5000. Anything that reads this as a fleet size is claiming a row it does
    // not hold.
    const total = fleetTotal(fleetOf(200, 200));
    expect(total.kind).toBe("floor");
    expect(total).not.toEqual({ kind: "exact", n: 200 });
  });
});

describe("the count line", () => {
  it("prints the wireframe's ratio for an empty selection against a known fleet", () => {
    const fleet = fleetOf(60, 200);
    const scope = resolve({ mode: "list", fleet });
    const label = scopeCountLabel(scope, fleetTotal(fleet));

    // The shape the wireframe specifies, asserted as its parts rather than as
    // a frozen sentence: zero in scope, sixty in the fleet, in that order.
    expect(sitesInScope(scope)).toBe(0);
    expect(label.startsWith("0 of 60")).toBe(true);
    expect(label).not.toMatch(/at least/i);
  });

  it("degrades the denominator to a floor when the fleet page was full", () => {
    const full = fleetOf(200, 200);
    const short = fleetOf(200, 500);
    const scope = (f: FleetSnapshot) => resolve({ mode: "list", fleet: f });

    const floored = scopeCountLabel(scope(full), fleetTotal(full));
    const exact = scopeCountLabel(scope(short), fleetTotal(short));

    // Same numerator, same row count, different entitlement to claim it.
    expect(floored).toMatch(/at least/i);
    expect(exact).not.toMatch(/at least/i);
    expect(floored).not.toBe(exact);
  });

  it("counts the selected sites, not the fleet, once something is picked", () => {
    const fleet = fleetOf(60, 200);
    const scope = resolve({ mode: "list", siteIds: ["s0", "s1", "s2"], fleet });
    expect(sitesInScope(scope)).toBe(3);
    expect(scopeCountLabel(scope, fleetTotal(fleet)).startsWith("3 of 60")).toBe(true);
  });

  it("prints NO number at all while the answer is still being worked out", () => {
    // "Loading" must not render as a zero. A zero here is a confident answer,
    // and the screen does not have one yet.
    const scope = resolve({ mode: "tags", tagNames: ["a"], fleet: null, sitesLoading: true });
    expect(scope.kind).toBe("unresolved");
    expect(sitesInScope(scope)).toBeNull();
    expect(scopeCountLabel(scope, fleetTotal(null))).not.toMatch(/\d/);
  });

  it("prints NO number at all when the fleet load failed", () => {
    const scope = resolve({ mode: "list", siteIds: ["s0"], fleet: null, sitesLoading: false });
    expect(scope.kind).toBe("unresolved");
    expect(scopeCountLabel(scope, fleetTotal(null))).not.toMatch(/\d/);
  });

  it("distinguishes the pending answer from the failed one", () => {
    const loading = resolve({ mode: "list", fleet: null, sitesLoading: true });
    const failed = resolve({ mode: "list", fleet: null, sitesLoading: false });
    expect(scopeCountLabel(loading, fleetTotal(null))).not.toBe(
      scopeCountLabel(failed, fleetTotal(null)),
    );
    expect(siteStepBlockedReason(loading)).not.toBe(siteStepBlockedReason(failed));
  });

  it("says nothing is blocked when the scope resolved", () => {
    const fleet = fleetOf(3, 200);
    expect(siteStepBlockedReason(resolve({ mode: "list", fleet }))).toBeNull();
    expect(siteStepBlockedReason(resolve({ mode: "all", fleet }))).toBeNull();
  });
});

describe("an empty scope is a working state, and this is where that is decided", () => {
  it("lets the operator leave step 3 with nothing selected", () => {
    // The wireframe, verbatim on this point: an empty scope "is a working
    // state, not an error". An earlier revision of this surface blocked the
    // step here, and that is the behaviour being corrected.
    const scope = resolve({ mode: "list", fleet: fleetOf(60, 200) });
    expect(scope).toEqual({ kind: "none", because: "no-selection" });
    expect(canLeaveSiteStep(scope)).toBe(true);
  });

  it("disagrees with the approval gate on exactly that input", () => {
    // Two gates exist because they differ here and nowhere else. If these ever
    // agree on every input, one of them is redundant and the empty state has
    // silently become an error again.
    const fleet = fleetOf(60, 200);
    const empty = resolve({ mode: "list", fleet });
    const picked = resolve({ mode: "list", siteIds: ["s1"], fleet });
    const every = resolve({ mode: "all", fleet });
    const unknown = resolve({ mode: "list", fleet: null });

    expect([canLeaveSiteStep(empty), isScopeApprovable(empty)]).toEqual([true, false]);
    // And they agree everywhere else.
    for (const scope of [picked, every, unknown]) {
      expect(canLeaveSiteStep(scope)).toBe(isScopeApprovable(scope));
    }
  });

  it("still refuses to move on when nobody has read the fleet", () => {
    const scope = resolve({ mode: "list", siteIds: ["s1"], fleet: null });
    expect(canLeaveSiteStep(scope)).toBe(false);
    expect(siteStepBlockedReason(scope)).not.toBeNull();
  });

  it("holds the step when a tag matches nothing in a page that is not the whole fleet", () => {
    // "Matches nothing" is a claim that needs every site. Against a full page
    // it is unresolved, not zero, and unresolved blocks.
    const fleet = fleetOf(200, 200);
    const scope = resolve({ mode: "tags", tagNames: ["nope"], fleet, tagsBySiteId: {} });
    expect(scope.kind).toBe("unresolved");
    expect(canLeaveSiteStep(scope)).toBe(false);
  });

  it("calls it a real zero once the page is known to be the whole fleet", () => {
    const fleet = fleetOf(60, 200);
    const scope = resolve({ mode: "tags", tagNames: ["nope"], fleet, tagsBySiteId: {} });
    expect(scope).toEqual({ kind: "none", because: "no-matches" });
    expect(canLeaveSiteStep(scope)).toBe(true);
  });
});

describe("the sites this connection cannot reach", () => {
  it("offers the excluded count only against an exact fleet", () => {
    const exact = fleetOf(60, 200);
    const floored = fleetOf(200, 200);
    const pick = (f: FleetSnapshot) => resolve({ mode: "list", siteIds: ["s0", "s1"], fleet: f });

    const label = excludedSitesLabel(pick(exact), fleetTotal(exact));
    expect(label).not.toBeNull();
    expect(label).toContain(String(60 - 2));

    // A floor denominator makes the subtraction meaningless: the rows we never
    // loaded are in neither half.
    expect(excludedSitesLabel(pick(floored), fleetTotal(floored))).toBeNull();
  });

  it("says never only for a fixed list, because a tag is not fixed", () => {
    const fleet = fleetOf(10, 200);
    const byList = resolve({ mode: "list", siteIds: ["s0"], fleet });
    const byTag = resolve({
      mode: "tags",
      tagNames: ["seo"],
      fleet,
      tagsBySiteId: { s0: ["seo"] },
    });

    expect(excludedSitesLabel(byList, fleetTotal(fleet))).toMatch(/never/i);
    // A site given that tag next month IS reached, so "never" would be false.
    expect(excludedSitesLabel(byTag, fleetTotal(fleet))).not.toMatch(/never/i);
  });

  it("offers nothing to see when the mode excludes nothing", () => {
    const fleet = fleetOf(10, 200);
    expect(excludedSitesLabel(resolve({ mode: "all", fleet }), fleetTotal(fleet))).toBeNull();
  });

  it("offers nothing while the scope is unresolved", () => {
    const fleet = fleetOf(200, 200);
    const scope = resolve({ mode: "tags", tagNames: ["x"], fleet, tagsBySiteId: {} });
    expect(excludedSitesLabel(scope, fleetTotal(fleet))).toBeNull();
  });
});

describe("the modes and their labels", () => {
  it("offers exactly the three the schema permits, in the wireframe's order", () => {
    expect(SITE_STEP_MODES.map((m) => m.value)).toEqual(["all", "tags", "list"]);
  });

  it("never puts a wire value on screen", () => {
    // 'list' is the column value; "Named sites" is what a person reads.
    for (const mode of SITE_STEP_MODES) {
      expect(mode.label).not.toBe(mode.value);
    }
  });

  it("prefixes a tag token and leaves a site token alone", () => {
    expect(scopeTokenLabel("tags", "client-retainer")).toBe("tag:client-retainer");
    expect(scopeTokenLabel("list", "acme.example")).toBe("acme.example");
  });

  it("names the thing the empty field is missing, per mode", () => {
    expect(emptyTokenFieldLabel("tags")).not.toBe(emptyTokenFieldLabel("list"));
  });
});
