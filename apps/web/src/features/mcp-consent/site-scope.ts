// Resolving "how many sites, and which" for the consent screen.
//
// THE RULE THIS FILE EXISTS TO HOLD: an empty resolved site set means NO SITES,
// never every site.
//
// That conflation has been closed three times in the backend on this stack and
// is named again in m124 obligation 2
// (apps/api/migrations/20260826000000_m124_mcp_connection_surface.sql lines
// 518-524): "An empty resolved set must mean NO SITES, never every site. The
// CHECK above stops an empty payload being stored, but nothing stops Go from
// computing an empty set (a tag that matches no site) and then treating it as
// absence of a filter."
//
// The same trap is available in a UI, in a nastier form. `sites.length === 0`
// reads naturally as "no filter applied", and the sentence that falls out of it
// -- "this client will be able to read your sites" -- is indistinguishable from
// the sentence for mode 'all'. A user would approve fleet-wide read access
// believing they had scoped it to one tag.
//
// So the resolution is a discriminated union with a distinct `none` member, and
// there is no code path from `none` to a sentence containing the word "all".
//
// WHAT THIS RESOLUTION IS AND IS NOT. It is a PREVIEW, computed from the site
// list this dashboard can see. The authoritative resolution happens server-side
// inside InTenantTx at one audited chokepoint, because a uuid[] column has no
// foreign key over its elements and only a join through `sites` under RLS drops
// ids belonging to another organisation. The screen must not claim otherwise.

export type SiteScopeMode = "all" | "tags" | "list";

// Mirrors SiteScopeModeAll / SiteScopeModeTags / SiteScopeModeList in
// apps/api/internal/mcp/scope.go. There is deliberately no default mode: an
// absent mode is refused server-side rather than read as 'all'.
export const SITE_SCOPE_MODES: readonly SiteScopeMode[] = ["all", "tags", "list"];

export interface ScopedSite {
  readonly id: string;
  readonly name: string;
  readonly url: string;
}

export type ResolvedSiteScope =
  /**
   * Every site in the organisation, including sites added after this grant is
   * created. `sites` is what exists today and is shown as a count, not as the
   * definition -- the definition is open-ended and the copy must say so.
   */
  | { readonly kind: "all"; readonly sites: readonly ScopedSite[] }
  /** A closed, non-empty set. `sites` is exhaustive. */
  | { readonly kind: "sites"; readonly sites: readonly ScopedSite[] }
  /**
   * The selection is valid and resolves to NOTHING. Distinct from `all` by
   * construction, and distinct from `unresolved`: we know the answer and the
   * answer is zero.
   */
  | { readonly kind: "none"; readonly because: "no-selection" | "no-matches" }
  /**
   * We do not know yet, or we could not find out. NOT a zero and NOT an
   * everything -- an unresolved scope must block approval, because approving it
   * would be consenting to a set nobody has read.
   */
  | { readonly kind: "unresolved"; readonly because: "loading" | "failed" };

export interface ResolveInput {
  readonly mode: SiteScopeMode;
  /** Tag names selected for mode 'tags'. */
  readonly selectedTagNames: readonly string[];
  /** Site ids selected for mode 'list'. */
  readonly selectedSiteIds: readonly string[];
  /**
   * Every site the dashboard can see, or null when the site list has not
   * loaded or its load failed. NULL IS NOT AN EMPTY ARRAY here, and the two
   * produce different outcomes on purpose.
   */
  readonly allSites: readonly ScopedSite[] | null;
  /** Tag names carried by each site, keyed by site id. */
  readonly tagsBySiteId: Readonly<Record<string, readonly string[]>>;
  readonly sitesLoading: boolean;
}

/**
 * Resolve the operator's selection into the set of sites the grant will cover.
 *
 * Every branch that cannot name a set returns `unresolved` or `none`. No branch
 * returns `all` except mode 'all'.
 */
export function resolveSiteScope(input: ResolveInput): ResolvedSiteScope {
  const { mode, selectedTagNames, selectedSiteIds, allSites, tagsBySiteId } = input;

  if (allSites === null) {
    // Cannot see the fleet. Whether that is a pending load or a failed one, the
    // honest answer is the same: we do not know which sites this covers.
    return { kind: "unresolved", because: input.sitesLoading ? "loading" : "failed" };
  }

  if (mode === "all") {
    // The only branch that may say "all". Note it is reached on the MODE, never
    // on a set turning out to be empty: an organisation with zero sites still
    // gets kind 'all', because the grant is genuinely open-ended and will pick
    // up the first site added. The copy layer handles the "and you have none
    // today" wrinkle without changing what was granted.
    return { kind: "all", sites: allSites };
  }

  if (mode === "tags") {
    if (selectedTagNames.length === 0) return { kind: "none", because: "no-selection" };
    const wanted = new Set(selectedTagNames);
    const matched = allSites.filter((site) =>
      (tagsBySiteId[site.id] ?? []).some((tag) => wanted.has(tag)),
    );
    // A tag that matches no site. THIS is the case the backend rule is about,
    // and it lands on `none`, never on a filter-less read.
    if (matched.length === 0) return { kind: "none", because: "no-matches" };
    return { kind: "sites", sites: matched };
  }

  // mode === "list"
  if (selectedSiteIds.length === 0) return { kind: "none", because: "no-selection" };
  const wanted = new Set(selectedSiteIds);
  const matched = allSites.filter((site) => wanted.has(site.id));
  // Selected ids that resolve to nothing this organisation owns. Same landing
  // as above, for the same reason.
  if (matched.length === 0) return { kind: "none", because: "no-matches" };
  return { kind: "sites", sites: matched };
}

/**
 * The sentence the screen shows for a resolved scope.
 *
 * Deliberately a pure function over the union so the copy for every branch is
 * visible in one place and can be read against the rule. No branch other than
 * `all` may contain the words "all" or "every".
 */
export function describeSiteScope(scope: ResolvedSiteScope): string {
  switch (scope.kind) {
    case "all":
      return scope.sites.length === 1
        ? "Every site in this organisation. That is 1 site today, and any site added later is covered too."
        : `Every site in this organisation. That is ${scope.sites.length} sites today, and any site added later is covered too.`;
    case "sites":
      return scope.sites.length === 1
        ? "1 site, listed below. No other site is covered, including sites added later."
        : `${scope.sites.length} sites, listed below. No other site is covered, including sites added later.`;
    case "none":
      return scope.because === "no-selection"
        ? "No sites are selected yet. Choose what this client may read before approving."
        : "This selection matches no sites, so this connection would be able to read nothing. That is not the same as covering every site.";
    case "unresolved":
      return scope.because === "loading"
        ? "Working out which sites this covers."
        : "We could not load your sites, so we cannot tell you which sites this connection would cover. Do not approve until this loads.";
  }
}

/**
 * Whether a scope may be approved.
 *
 * `none` is refusable rather than blocked-with-a-warning: server-side, an empty
 * payload is unstorable for mode 'list' and a tag matching nothing resolves to
 * an empty set, so approving it creates a grant that reads nothing. Better to
 * say that on this screen than to mint a credential that silently does nothing.
 * `unresolved` is blocked because consenting to an unread set is not consent.
 */
export function isScopeApprovable(scope: ResolvedSiteScope): boolean {
  return scope.kind === "all" || scope.kind === "sites";
}
