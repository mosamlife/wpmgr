// Resolving "how many sites, and which" for the consent screen.
//
// TWO RULES LIVE HERE, AND THEY PULL IN OPPOSITE DIRECTIONS.
//
// RULE 1: an empty resolved site set means NO SITES, never every site.
//
// m124 obligation 2 (apps/api/migrations/20260826000000_m124_mcp_connection_surface.sql
// lines 518-524): "An empty resolved set must mean NO SITES, never every site.
// The CHECK above stops an empty payload being stored, but nothing stops Go
// from computing an empty set (a tag that matches no site) and then treating it
// as absence of a filter." Closed three times in the backend.
//
// In a UI the trap is nastier: `sites.length === 0` reads naturally as "no
// filter applied", and the sentence that falls out of it is indistinguishable
// from the sentence for mode 'all'. A user would approve fleet-wide read access
// believing they had scoped it to one tag.
//
// RULE 2: the list we hold is neither exhaustive nor fixed, and the copy must
// not imply it is either.
//
//   NOT EXHAUSTIVE. listSites is a paged endpoint. useSites asks for
//   DEFAULT_SITES_LIMIT (200) rows and SiteList is `{ items }` -- there is no
//   `total` and no `has_more` on the wire, so a full page is indistinguishable
//   from a full page with more behind it. An operator with more sites than the
//   cap would, under a naive screen, approve access to sites the screen never
//   showed them. That is a PARTIAL RESULT RENDERED AS A COMPLETE ONE, which is
//   this project's signature defect, landed on the one screen where the cost is
//   consent given for something the person was not shown.
//
//   NOT FIXED. The server resolves 'all' and 'tags' against CURRENT tenant data
//   at read time, so those grants cover sites added after approval. A list
//   presented as the definition of the grant would be wrong the moment a site
//   is enrolled.
//
// The consequence for the copy is that mode 'all' and mode 'tags' MUST NOT
// state a fleet size. What they state is the RULE ("every site", "every site
// carrying this tag") plus a floor -- "at least N, which is what we could load"
// -- which is true no matter what the API withheld, because we are holding
// those N rows. Only mode 'list' enumerates exhaustively, because the operator
// picked from what they saw and the grant is exactly that fixed set.
//
// A FLOOR IS NOT AN APPROXIMATION. "At least N" is backed by rows in hand. A
// total is not available on the wire and is not guessed; see the PR for the
// routing note.
//
// WHAT THIS RESOLUTION IS AND IS NOT. It is a PREVIEW. The authoritative
// resolution happens server-side inside InTenantTx at one audited chokepoint,
// because a uuid[] column has no foreign key over its elements and only a join
// through `sites` under RLS drops ids belonging to another organisation. The
// screen must not claim otherwise.

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

/**
 * What this dashboard managed to load of the tenant's fleet.
 *
 * `complete` is the load-bearing field. It is false whenever the page came back
 * full, because a full page and a full page with more behind it are the same
 * response. False does not mean "there are definitely more"; it means "we
 * cannot say there are not", and the copy is written for that weaker claim.
 */
export interface FleetSnapshot {
  readonly sites: readonly ScopedSite[];
  readonly complete: boolean;
}

/**
 * Build a snapshot from a page of sites and the page size that was requested.
 *
 * A short page is complete: we asked for `requestedLimit` and got fewer, so
 * there is nothing behind it. A full page is NOT complete.
 */
export function snapshotFromPage(
  sites: readonly ScopedSite[],
  requestedLimit: number,
): FleetSnapshot {
  return { sites, complete: sites.length < requestedLimit };
}

export type ResolvedSiteScope =
  /**
   * Every site in the organisation, now and in future. `shown` is what we could
   * load, and it is a sample rather than a definition -- see `listComplete`.
   */
  | { readonly kind: "all"; readonly shown: readonly ScopedSite[]; readonly listComplete: boolean }
  /**
   * A named set. `basis` decides whether it is fixed: 'list' is the operator's
   * own pick and is exhaustive and fixed; 'tags' is a rule that future sites can
   * fall into, and is only as complete as the page we resolved it against.
   */
  | {
      readonly kind: "sites";
      readonly sites: readonly ScopedSite[];
      readonly basis: "tags" | "list";
      readonly listComplete: boolean;
    }
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
   * What we loaded of the fleet, or null when the load has not finished or
   * failed. NULL IS NOT AN EMPTY SNAPSHOT here, and the two produce different
   * outcomes on purpose.
   */
  readonly fleet: FleetSnapshot | null;
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
  const { mode, selectedTagNames, selectedSiteIds, fleet, tagsBySiteId } = input;

  if (fleet === null) {
    // Cannot see the fleet. Whether that is a pending load or a failed one, the
    // honest answer is the same: we do not know which sites this covers.
    return { kind: "unresolved", because: input.sitesLoading ? "loading" : "failed" };
  }

  if (mode === "all") {
    // The only branch that may say "all". Note it is reached on the MODE, never
    // on a set turning out to be empty: an organisation with zero sites still
    // gets kind 'all', because the grant is genuinely open-ended and will pick
    // up the first site added.
    return { kind: "all", shown: fleet.sites, listComplete: fleet.complete };
  }

  if (mode === "tags") {
    if (selectedTagNames.length === 0) return { kind: "none", because: "no-selection" };
    const wanted = new Set(selectedTagNames);
    const matched = fleet.sites.filter((site) =>
      (tagsBySiteId[site.id] ?? []).some((tag) => wanted.has(tag)),
    );
    // A tag that matches no site IN THE PAGE WE HOLD. When the page is complete
    // that is a real zero and lands on `none`. When it is not, we have not
    // looked at every site, so "matches nothing" is a claim we cannot make --
    // it is an unresolved scope, not an empty one, and it blocks approval
    // rather than either granting everything or asserting a false zero.
    if (matched.length === 0) {
      return fleet.complete
        ? { kind: "none", because: "no-matches" }
        : { kind: "unresolved", because: "failed" };
    }
    return { kind: "sites", sites: matched, basis: "tags", listComplete: fleet.complete };
  }

  // mode === "list"
  if (selectedSiteIds.length === 0) return { kind: "none", because: "no-selection" };
  const wanted = new Set(selectedSiteIds);
  const matched = fleet.sites.filter((site) => wanted.has(site.id));
  // Selected ids that resolve to nothing this organisation owns. Same landing
  // as above, for the same reason.
  if (matched.length === 0) return { kind: "none", because: "no-matches" };
  // ALWAYS complete. The operator picked these ids out of the list they were
  // shown, so the grant is exactly the set on screen regardless of what the
  // page withheld. What the truncation costs here is CHOICE, not accuracy, and
  // the picker says so separately.
  return { kind: "sites", sites: matched, basis: "list", listComplete: true };
}

/**
 * The sentence the screen shows for a resolved scope.
 *
 * Deliberately a pure function over the union so the copy for every branch is
 * visible in one place and can be read against both rules above. Two invariants
 * a reader should be able to check by eye:
 *
 *   - no branch other than `all` contains "all" or "every" as a claim about
 *     coverage;
 *   - no branch claims a complete list unless `listComplete` is true, and the
 *     open-ended bases ('all', 'tags') always say that future sites are covered;
 *   - no branch ASSERTS THAT MORE EXIST. `listComplete: false` means "we cannot
 *     tell", not "there are more" -- a page that came back exactly full with
 *     nothing behind it satisfies it. The rows in hand support a floor and the
 *     absence of a claim; they do not support the existence of a 201st site.
 */
function siteCount(n: number): string {
  return n === 1 ? "1 site" : `${n} sites`;
}

export function describeSiteScope(scope: ResolvedSiteScope): string {
  switch (scope.kind) {
    case "all": {
      const future = "Any site added later is covered too, without asking you again.";
      if (!scope.listComplete) {
        // NO TOTAL, AND NO ASSERTION THAT ONE IS LARGER. We hold `shown.length`
        // rows and cannot see past them. "There are more than that" would be a
        // claim about the 201st site, which is exactly the row we do not have;
        // a fleet of exactly 200 makes that sentence false. What the rows
        // support is the floor and an admission of the limit.
        return `Every site in this organisation. We can list ${siteCount(scope.shown.length)} here and cannot tell you whether there are others. ${future}`;
      }
      return `Every site in this organisation. That is ${siteCount(scope.shown.length)} today, listed below. ${future}`;
    }
    case "sites": {
      if (scope.basis === "list") {
        // Exhaustive AND fixed: the operator picked these out of what they saw.
        return `${siteCount(scope.sites.length)}, listed below. No other site is covered, including sites added later.`;
      }
      const future =
        "Any site given one of these tags later is covered too, without asking you again.";
      if (!scope.listComplete) {
        const verb = scope.sites.length === 1 ? "carries" : "carry";
        return `At least ${siteCount(scope.sites.length)} ${verb} these tags. We could not check every site in this organisation, so there may be others. ${future}`;
      }
      const verb = scope.sites.length === 1 ? "carries" : "carry";
      return `${siteCount(scope.sites.length)} ${verb} these tags today, listed below. ${future}`;
    }
    case "none":
      // 2026-08-23 revision (wireframes.html:3559): an empty allowlist "is a
      // working state, not an error -- it is how you mint a credential now and
      // decide its reach later." Both branches say that consequence plainly
      // rather than treating the choice as incomplete.
      return scope.because === "no-selection"
        ? "No sites are selected. That is a working state, not an error: this connection will read nothing and propose nothing until you give it sites to cover, now or later."
        : "This selection matches no sites, so this connection will read nothing and propose nothing right now. That is a working state, not an error: you can widen its scope later.";
    case "unresolved":
      return scope.because === "loading"
        ? "Working out which sites this covers."
        : "We could not read every site in this organisation, so we cannot tell you which sites this connection would cover. Do not approve until this loads.";
  }
}

/**
 * Whether a scope may be approved.
 *
 * 2026-08-23 revision (wireframes.html:3559): "A connection with an empty
 * allowlist can read nothing and propose nothing. That is a working state, not
 * an error -- it is how you mint a credential now and decide its reach later."
 * `none` is therefore approvable, on equal footing with `all` and `sites`.
 * Narrowing to nothing is not refused here because it is enforced where it
 * actually matters: by which tools are registered for the connection
 * (wireframes.html:1743, "narrowing is applied by unregistering the tool, not
 * by denying it at call time"), not by a client-side gate on this screen. The
 * older rule that blocked an empty allowlist (wireframes.html:1293) is
 * superseded.
 *
 * `unresolved` is the only kind that still blocks. Consenting to a scope
 * nobody has read is not consent, whether the read is pending or failed.
 *
 * A TRUNCATED LIST DOES NOT BLOCK. It is disclosed instead. Blocking would
 * refuse every organisation past the page size, and the operator choosing "every
 * site" has understood the rule they are granting; what they must not be given
 * is a false count or a list that poses as the whole fleet.
 */
export function isScopeApprovable(scope: ResolvedSiteScope): boolean {
  return scope.kind !== "unresolved";
}

/**
 * Resolve selected tag NAMES to the ids a mint or approval request must carry.
 *
 * SHARED BETWEEN THE CONSENT SCREEN AND THE WIZARD, because both hold tag
 * choices as names (what the operator clicked) and both wire endpoints
 * (approvalRequestDTO, mintConnectionRequestDTO) want `scope_tag_ids` as
 * UUIDs. A tag id survives a rename; a name does not, so translating late and
 * once, here, is what keeps a renamed tag from silently changing which sites a
 * stored grant covers.
 *
 * NULL, NOT A SHORTER ARRAY, when a selected name no longer resolves. The
 * registry can go null after a name is ticked (still loading, or reloaded
 * without that tag), and `(tags ?? []).filter(...)` would silently produce an
 * array missing exactly the tag the operator chose -- narrowing the request
 * without telling anyone. Null is "cannot build this payload" and every caller
 * must refuse to submit on it, loudly, rather than sending the smaller set.
 *
 * PARTIAL RESOLUTION IS THE SAME ABSENCE. Filtering the registry down to the
 * ticked names returns whatever survived, so a selection of two tags where one
 * was deleted resolved to the ONE that remained and the request went out
 * covering half of what was chosen. Narrowing is not the safer direction when
 * nobody is told it happened: the operator reads the scope line, sees both
 * tags they ticked, and deploys a credential that carries one. Every selected
 * name must resolve or the whole payload is null.
 *
 * EMPTY IN, EMPTY OUT. No ticked names resolves to `[]`, not null: there is
 * nothing unresolved about choosing nothing. It is NOT a mintable payload --
 * ValidateSiteScopeRequest (apps/api/internal/mcp/scope.go:177-182) refuses
 * mode 'tags' with an empty list, and mint.go:264 runs the same check -- but
 * that refusal belongs to the callers' gates, stated in their own words, not
 * smuggled in here as a null that every caller then reports as a registry
 * failure the operator cannot act on.
 */
export function resolveTagIds(
  selectedTagNames: readonly string[],
  tags: readonly { readonly id: string; readonly name: string }[] | null,
): readonly string[] | null {
  if (tags === null) return null;
  const ids: string[] = [];
  for (const name of selectedTagNames) {
    const tag = tags.find((candidate) => candidate.name === name);
    if (tag === undefined) return null;
    if (!ids.includes(tag.id)) ids.push(tag.id);
  }
  return ids;
}
