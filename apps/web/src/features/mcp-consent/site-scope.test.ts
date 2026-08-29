import { describe, it, expect } from "vitest";

import {
  describeSiteScope,
  isScopeApprovable,
  resolveSiteScope,
  type ResolveInput,
  type ScopedSite,
} from "./site-scope";

// m124 obligation 2: "An empty resolved set must mean NO SITES, never every
// site." Closed three times in the backend on this stack; this file holds the
// same line in the UI, where the failure is worse because the empty set is
// approved by a human who was told the wrong thing.

const SITES: ScopedSite[] = [
  { id: "s1", name: "Alpha", url: "https://alpha.example" },
  { id: "s2", name: "Beta", url: "https://beta.example" },
];

const TAGS_BY_SITE = { s1: ["prod"], s2: ["staging"] };

function input(over: Partial<ResolveInput>): ResolveInput {
  return {
    mode: "list",
    selectedTagNames: [],
    selectedSiteIds: [],
    allSites: SITES,
    tagsBySiteId: TAGS_BY_SITE,
    sitesLoading: false,
    ...over,
  };
}

/** Every word that must never appear in copy for a scope that is not mode 'all'. */
const EVERYTHING_WORDS = /\ball\b|\bevery\b|\bany site\b/i;

describe("resolveSiteScope — an empty resolution is never everything", () => {
  it("a tag matching no site resolves to none, not to all", () => {
    const scope = resolveSiteScope(
      input({ mode: "tags", selectedTagNames: ["archived"] }),
    );
    expect(scope.kind).toBe("none");
    expect(scope.kind === "none" && scope.because).toBe("no-matches");
  });

  it("the sentence for a tag matching no site cannot be read as everything", () => {
    const scope = resolveSiteScope(
      input({ mode: "tags", selectedTagNames: ["archived"] }),
    );
    const sentence = describeSiteScope(scope);
    expect(sentence).toMatch(/matches no sites/i);
    expect(sentence).toMatch(/read nothing/i);
    // The load-bearing assertion. "all"/"every" appearing here would be the
    // exact conflation the backend rule forbids.
    expect(sentence.replace(/not the same as covering every site/i, "")).not.toMatch(
      EVERYTHING_WORDS,
    );
  });

  it("no selection at all resolves to none, not to all", () => {
    expect(resolveSiteScope(input({ mode: "list", selectedSiteIds: [] })).kind).toBe("none");
    expect(resolveSiteScope(input({ mode: "tags", selectedTagNames: [] })).kind).toBe("none");
  });

  it("picked site ids that this organisation does not own resolve to none", () => {
    // scope_site_ids is a uuid[] with no foreign key over its elements, so a
    // foreign id is representable. Locally it must vanish into `none`, never
    // into an unfiltered read.
    const scope = resolveSiteScope(input({ mode: "list", selectedSiteIds: ["not-ours"] }));
    expect(scope.kind).toBe("none");
  });

  it("an empty resolution is not approvable", () => {
    expect(isScopeApprovable(resolveSiteScope(input({ mode: "tags", selectedTagNames: ["gone"] })))).toBe(false);
    expect(isScopeApprovable(resolveSiteScope(input({ mode: "list", selectedSiteIds: [] })))).toBe(false);
  });

  it("only mode 'all' ever produces kind 'all'", () => {
    const modes = ["all", "tags", "list"] as const;
    for (const mode of modes) {
      const scope = resolveSiteScope(input({ mode, selectedTagNames: [], selectedSiteIds: [] }));
      expect(scope.kind === "all").toBe(mode === "all");
    }
  });

  it("an organisation with zero sites still gets 'all' from mode 'all', not 'none'", () => {
    // The grant is genuinely open-ended and picks up the first site added, so
    // collapsing it to `none` would understate what was granted -- the mirror
    // image of the bug above and just as wrong.
    const scope = resolveSiteScope(input({ mode: "all", allSites: [] }));
    expect(scope.kind).toBe("all");
    expect(describeSiteScope(scope)).toMatch(/added later/i);
  });
});

describe("resolveSiteScope — an unread fleet is not an empty one", () => {
  it("a site list that has not loaded is unresolved, not none and not all", () => {
    const scope = resolveSiteScope(input({ allSites: null, sitesLoading: true }));
    expect(scope.kind).toBe("unresolved");
    expect(scope.kind === "unresolved" && scope.because).toBe("loading");
  });

  it("a FAILED site load is unresolved and blocks approval", () => {
    const scope = resolveSiteScope(input({ allSites: null, sitesLoading: false }));
    expect(scope.kind).toBe("unresolved");
    expect(scope.kind === "unresolved" && scope.because).toBe("failed");
    expect(isScopeApprovable(scope)).toBe(false);
    expect(describeSiteScope(scope)).toMatch(/could not load/i);
  });

  it("mode 'all' does not paper over an unread fleet", () => {
    // Tempting shortcut: mode 'all' does not need the site list to be correct,
    // so return `all` regardless. It would then be impossible to tell the user
    // how many sites that is, and the count is half the checklist item.
    const scope = resolveSiteScope(input({ mode: "all", allSites: null, sitesLoading: false }));
    expect(scope.kind).toBe("unresolved");
  });
});

describe("resolveSiteScope — a real selection names its sites", () => {
  it("a tag that matches enumerates exactly the matching sites", () => {
    const scope = resolveSiteScope(input({ mode: "tags", selectedTagNames: ["prod"] }));
    expect(scope.kind).toBe("sites");
    expect(scope.kind === "sites" && scope.sites.map((s) => s.id)).toEqual(["s1"]);
    const sentence = describeSiteScope(scope);
    expect(sentence).toMatch(/^1 site, listed below/);
    expect(sentence).toMatch(/No other site is covered/);
  });

  it("a count is never offered without the list behind it", () => {
    const scope = resolveSiteScope(input({ mode: "list", selectedSiteIds: ["s1", "s2"] }));
    expect(scope.kind === "sites" && scope.sites).toHaveLength(2);
    expect(describeSiteScope(scope)).toMatch(/2 sites, listed below/);
  });
});
