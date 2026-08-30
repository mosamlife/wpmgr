import { describe, it, expect } from "vitest";

import {
  describeSiteScope,
  isScopeApprovable,
  resolveSiteScope,
  resolveTagIds,
  snapshotFromPage,
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
    fleet: { sites: SITES, complete: true },
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

  it("an empty resolution IS approvable -- reading nothing is a working state, not an error (2026-08-23 revision)", () => {
    expect(isScopeApprovable(resolveSiteScope(input({ mode: "tags", selectedTagNames: ["gone"] })))).toBe(true);
    expect(isScopeApprovable(resolveSiteScope(input({ mode: "list", selectedSiteIds: [] })))).toBe(true);
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
    const scope = resolveSiteScope(input({ mode: "all", fleet: { sites: [], complete: true } }));
    expect(scope.kind).toBe("all");
    expect(describeSiteScope(scope)).toMatch(/added later/i);
  });
});

describe("isScopeApprovable — an empty allowlist is approvable, an unread one never is", () => {
  // The 2026-08-23 wireframe revision (line 3559) rules that a connection with
  // an empty allowlist "can read nothing and propose nothing. That is a
  // working state, not an error." This block pins BOTH halves of that with
  // equal force: `none` (however it was reached) now passes, and `unresolved`
  // (however it was reached) still fails. Weakening either half back to the
  // pre-2026-08-23 shape must redden this block, not just the copy tests.
  it("accepts a resolved-to-nothing scope, from no selection and from no matches alike", () => {
    expect(isScopeApprovable({ kind: "none", because: "no-selection" })).toBe(true);
    expect(isScopeApprovable({ kind: "none", because: "no-matches" })).toBe(true);
  });

  it("still refuses a scope nobody has read, whether pending or failed", () => {
    expect(isScopeApprovable({ kind: "unresolved", because: "loading" })).toBe(false);
    expect(isScopeApprovable({ kind: "unresolved", because: "failed" })).toBe(false);
  });
});

describe("resolveSiteScope — an unread fleet is not an empty one", () => {
  it("a site list that has not loaded is unresolved, not none and not all", () => {
    const scope = resolveSiteScope(input({ fleet: null, sitesLoading: true }));
    expect(scope.kind).toBe("unresolved");
    expect(scope.kind === "unresolved" && scope.because).toBe("loading");
  });

  it("a FAILED site load is unresolved and blocks approval", () => {
    const scope = resolveSiteScope(input({ fleet: null, sitesLoading: false }));
    expect(scope.kind).toBe("unresolved");
    expect(scope.kind === "unresolved" && scope.because).toBe("failed");
    expect(isScopeApprovable(scope)).toBe(false);
    expect(describeSiteScope(scope)).toMatch(/could not read every site/i);
  });

  it("mode 'all' does not paper over an unread fleet", () => {
    // Tempting shortcut: mode 'all' does not need the site list to be correct,
    // so return `all` regardless. It would then be impossible to tell the user
    // how many sites that is, and the count is half the checklist item.
    const scope = resolveSiteScope(input({ mode: "all", fleet: null, sitesLoading: false }));
    expect(scope.kind).toBe("unresolved");
  });
});

describe("resolveSiteScope — a real selection names its sites", () => {
  it("a tag that matches enumerates exactly the matching sites", () => {
    const scope = resolveSiteScope(input({ mode: "tags", selectedTagNames: ["prod"] }));
    expect(scope.kind).toBe("sites");
    expect(scope.kind === "sites" && scope.sites.map((s) => s.id)).toEqual(["s1"]);
    const sentence = describeSiteScope(scope);
    expect(sentence).toMatch(/^1 site carries these tags today/);
    // A TAG IS A RULE, NOT A LIST. A site given this tag tomorrow is covered
    // without a second consent, so the copy may never present the enumeration
    // as the fixed extent of the grant.
    expect(sentence).toMatch(/given one of these tags later is covered/i);
  });

  it("a count is never offered without the list behind it", () => {
    const scope = resolveSiteScope(input({ mode: "list", selectedSiteIds: ["s1", "s2"] }));
    expect(scope.kind === "sites" && scope.sites).toHaveLength(2);
    const sentence = describeSiteScope(scope);
    expect(sentence).toMatch(/2 sites, listed below/);
    // A hand-picked list IS exhaustive and fixed, and is the one basis allowed
    // to say so.
    expect(sentence).toMatch(/No other site is covered/);
  });
});


// ---------------------------------------------------------------------------
// A page of sites is not a fleet.
//
// listSites is paged; useSites asks for DEFAULT_SITES_LIMIT rows and SiteList
// is `{ items }` with no `total` and no `has_more`. So a full page and a full
// page with more behind it are the same response, and an operator with more
// sites than the page size would -- under a naive screen -- approve access to
// sites the screen never showed them. Partial rendered as complete, on the one
// screen where the cost is consent for something unseen.
// ---------------------------------------------------------------------------

describe("snapshotFromPage — a full page is never called complete", () => {
  it("marks a short page complete: we asked for more and got fewer", () => {
    expect(snapshotFromPage(SITES, 200).complete).toBe(true);
  });

  it("marks a FULL page incomplete, because nothing on the wire says otherwise", () => {
    const full = Array.from({ length: 200 }, (_, i) => ({
      id: `s${i}`,
      name: `Site ${i}`,
      url: `https://s${i}.example`,
    }));
    expect(snapshotFromPage(full, 200).complete).toBe(false);
  });

  it("marks an empty page complete", () => {
    expect(snapshotFromPage([], 200).complete).toBe(true);
  });
});

describe("an org larger than the page cap is never shown the page as the fleet", () => {
  const CAPPED = { sites: SITES, complete: false };

  it("mode 'all' states no total and says there are more", () => {
    const scope = resolveSiteScope(input({ mode: "all", fleet: CAPPED }));
    expect(scope.kind).toBe("all");
    expect(scope.kind === "all" && scope.listComplete).toBe(false);
    const sentence = describeSiteScope(scope);
    // "That is N sites today" is the sentence that must NOT appear: it presents
    // the page as the fleet size.
    expect(sentence).not.toMatch(/That is \d+ sites? today/);
    expect(sentence).toMatch(/cannot tell you whether there are others/i);
    expect(sentence).toMatch(/added later is covered/i);
    // AND IT MUST NOT ASSERT THAT MORE EXIST. `listComplete: false` means we
    // cannot tell, not that there is a 201st site -- a page that came back
    // exactly full with nothing behind it satisfies it and makes any such
    // sentence false. The rows support a floor, not an inequality.
    expect(sentence).not.toMatch(/there are more/i);
  });

  it("mode 'all' on a COMPLETE list may state the exact count", () => {
    // The guard must not fire on honest work: when the page really is the whole
    // fleet, refusing to give a number would be its own defect.
    const scope = resolveSiteScope(input({ mode: "all" }));
    expect(scope.kind === "all" && scope.listComplete).toBe(true);
    expect(describeSiteScope(scope)).toMatch(/That is 2 sites today/);
  });

  it("mode 'tags' on a capped list gives a floor, never a total", () => {
    const scope = resolveSiteScope(
      input({ mode: "tags", selectedTagNames: ["prod"], fleet: CAPPED }),
    );
    expect(scope.kind === "sites" && scope.listComplete).toBe(false);
    const sentence = describeSiteScope(scope);
    // Count and noun agree. "At least 1 sites" is the tell of a template that
    // never read its own number.
    expect(sentence).toMatch(/^At least 1 site carries these tags/);
    expect(sentence).not.toMatch(/1 sites/);
    expect(sentence).toMatch(/could not check every site/i);
    expect(sentence).not.toMatch(/there are more/i);
  });

  it("a tag matching nothing IN A CAPPED PAGE is unresolved, not a zero", () => {
    // We have not looked at every site, so "matches nothing" is a claim we
    // cannot make. Asserting a false zero here would be the mirror of the
    // empty-means-all bug: it would tell the operator the grant reads nothing
    // when it may read plenty.
    const scope = resolveSiteScope(
      input({ mode: "tags", selectedTagNames: ["archived"], fleet: CAPPED }),
    );
    expect(scope.kind).toBe("unresolved");
    expect(isScopeApprovable(scope)).toBe(false);
  });

  it("a hand-picked list stays exhaustive even when the picker was capped", () => {
    // What truncation costs mode 'list' is CHOICE, not accuracy: the grant is
    // exactly the ids the operator ticked, all of which they saw.
    const scope = resolveSiteScope(
      input({ mode: "list", selectedSiteIds: ["s1"], fleet: CAPPED }),
    );
    expect(scope.kind === "sites" && scope.listComplete).toBe(true);
    expect(describeSiteScope(scope)).toMatch(/No other site is covered/);
  });
});


describe("no branch asserts that more sites exist", () => {
  // `listComplete: false` means "we cannot tell", never "there are more". Every
  // sentence the screen can produce is checked against that, so a future copy
  // edit cannot quietly reintroduce the inequality.
  const CAPPED = { sites: SITES, complete: false };

  const everySentence = [
    resolveSiteScope(input({ mode: "all" })),
    resolveSiteScope(input({ mode: "all", fleet: CAPPED })),
    resolveSiteScope(input({ mode: "all", fleet: { sites: [], complete: true } })),
    resolveSiteScope(input({ mode: "tags", selectedTagNames: ["prod"] })),
    resolveSiteScope(input({ mode: "tags", selectedTagNames: ["prod"], fleet: CAPPED })),
    resolveSiteScope(input({ mode: "tags", selectedTagNames: [] })),
    resolveSiteScope(input({ mode: "list", selectedSiteIds: ["s1"] })),
    resolveSiteScope(input({ mode: "list", selectedSiteIds: ["s1", "s2"] })),
    resolveSiteScope(input({ mode: "list", selectedSiteIds: [] })),
    resolveSiteScope(input({ fleet: null, sitesLoading: true })),
    resolveSiteScope(input({ fleet: null, sitesLoading: false })),
  ].map(describeSiteScope);

  it("never claims more sites exist than it can see", () => {
    for (const sentence of everySentence) {
      expect(sentence).not.toMatch(/there are more/i);
      expect(sentence).not.toMatch(/more than that/i);
    }
  });

  it("never disagrees with itself about singular and plural", () => {
    for (const sentence of everySentence) {
      expect(sentence).not.toMatch(/\b1 sites\b/);
      expect(sentence).not.toMatch(/\b1 sites? carry\b/);
    }
  });
});

// ---------------------------------------------------------------------------
// resolveTagIds
//
// The whole value of this function is the distinction between three answers
// that a `.filter()` collapses into one: the ids (send this), `[]` (send an
// empty scope, which is a choice) and `null` (send NOTHING, because we cannot
// build the payload the operator asked for). The failure it exists to stop is
// silent NARROWING, which no error surface ever reports because the request
// succeeds -- the operator ticks two tags, one no longer resolves, and a
// credential goes out covering half of what its own screen says it covers.
// ---------------------------------------------------------------------------

const REGISTRY = [
  { id: "tag-uuid-prod", name: "prod" },
  { id: "tag-uuid-staging", name: "staging" },
];

describe("resolveTagIds", () => {
  it("resolves every ticked name to its id, in the order they were ticked", () => {
    expect(resolveTagIds(["staging", "prod"], REGISTRY)).toEqual([
      "tag-uuid-staging",
      "tag-uuid-prod",
    ]);
  });

  it("refuses the whole payload when one of two ticked tags no longer resolves", () => {
    // THE REGRESSION. Filtering the registry down to the ticked names returned
    // ["tag-uuid-prod"] here: one real id, a request the server accepts, and a
    // token covering one tag while the screen that minted it named two. The
    // only honest answer is that this payload cannot be built.
    expect(resolveTagIds(["prod", "deleted-since-step-3"], REGISTRY)).toBeNull();
    expect(resolveTagIds(["deleted-since-step-3", "prod"], REGISTRY)).toBeNull();
  });

  it("refuses when NO ticked name resolves, rather than falling back to an empty scope", () => {
    // A narrowing all the way down to nothing is still a narrowing, and `[]`
    // is a different sentence to the operator: it means "covers nothing on
    // purpose", not "we lost your selection".
    expect(resolveTagIds(["gone", "also-gone"], REGISTRY)).toBeNull();
  });

  it("refuses on an unread registry even when nothing is ticked", () => {
    // null registry is our request, not the organisation. Distinct from the
    // empty-selection case directly below, which is the operator's answer.
    expect(resolveTagIds([], null)).toBeNull();
    expect(resolveTagIds(["prod"], null)).toBeNull();
  });

  it("returns an empty array, NOT null, for an empty selection", () => {
    const resolved = resolveTagIds([], REGISTRY);
    expect(resolved).toEqual([]);
    // The two must be distinguishable at a call site, because they lead to
    // different screens: connect-wizard.tsx:153 feeds this into
    // mintBlockedReason as `scopeTagIds !== null` and reports null as a
    // registry failure the operator is told to fix. An empty tick list is not
    // a failure and must not be reported as one.
    expect(resolved).not.toBeNull();
    expect(resolveTagIds([], REGISTRY) === null).toBe(false);
  });

  it("collapses a duplicated name to one id rather than sending it twice", () => {
    expect(resolveTagIds(["prod", "prod"], REGISTRY)).toEqual(["tag-uuid-prod"]);
  });

  it("resolves against an empty registry only when nothing is ticked", () => {
    expect(resolveTagIds([], [])).toEqual([]);
    expect(resolveTagIds(["prod"], [])).toBeNull();
  });
});
