// Wizard step 3 -- "Choose which sites this connection may touch".
//
// The RESOLUTION lives in features/mcp-consent/site-scope.ts and is not
// duplicated here. That module already carries the two rules this screen must
// obey (an empty resolved set means NO SITES, never every site; and the page we
// hold is neither exhaustive nor fixed), and it was hardened on the consent
// screen where the same mistakes cost consent. What lives HERE is only what
// step 3 needs and the consent screen does not:
//
//   1. THE COUNT IN THE WIREFRAME'S SHAPE -- "0 of 60 sites." The consent
//      screen deliberately never prints a fleet size, because it prints a
//      sentence about coverage instead. Step 3 prints a ratio, and a ratio has
//      a denominator, so the denominator is where the truncation trap lands on
//      this screen. `FleetTotal` is that denominator made explicit: exact when
//      the page came back short, a FLOOR when it came back full, and unknown
//      when we could not read the fleet at all. No branch invents a total.
//
//   2. A DIFFERENT GATE. `isScopeApprovable` refuses `none`, because approving
//      an empty grant mints a credential that reads nothing. Step 3 is not an
//      approval and MUST NOT refuse `none`: the wireframe states in as many
//      words that an empty scope "is a working state, not an error. It is how
//      you mint a credential now and decide its reach later." The two gates
//      disagree on exactly one input and that is the point of having two.
//
// Every string a caller renders comes from a function in this file, so the copy
// for every branch is readable in one place and a test can derive its
// expectation from the same function rather than freezing a sentence.

import type {
  FleetSnapshot,
  ResolvedSiteScope,
  SiteScopeMode,
} from "@/features/mcp-consent/site-scope";

/**
 * The denominator of the count, and how much we are entitled to claim about it.
 *
 * `floor` is not a smaller `exact`. It means "we hold n rows and cannot see
 * past them", which supports "at least n" and supports NO assertion that an
 * n+1th site exists -- a fleet of exactly n satisfies it. `unknown` is not a
 * zero: it is the failed or pending read, and it prints no number at all.
 */
export type FleetTotal =
  | { readonly kind: "exact"; readonly n: number }
  | { readonly kind: "floor"; readonly n: number }
  | { readonly kind: "unknown" };

/**
 * Read the denominator off the snapshot.
 *
 * NULL IS `unknown`, NOT ZERO. A failed site load rendered as "0 of 0 sites" is
 * the failure-as-empty defect this surface has already shipped three times.
 */
export function fleetTotal(fleet: FleetSnapshot | null): FleetTotal {
  if (fleet === null) return { kind: "unknown" };
  return fleet.complete
    ? { kind: "exact", n: fleet.sites.length }
    : { kind: "floor", n: fleet.sites.length };
}

/**
 * How many sites the scope resolves to, or null when that is not knowable.
 *
 * `all` counts the rows we hold, which is a floor and is labelled as one by the
 * denominator beside it. `unresolved` is null and never zero: "we do not know"
 * and "none" are different answers and the count line renders them differently.
 */
export function sitesInScope(scope: ResolvedSiteScope): number | null {
  switch (scope.kind) {
    case "all":
      return scope.shown.length;
    case "sites":
      return scope.sites.length;
    case "none":
      return 0;
    case "unresolved":
      return null;
  }
}

function plural(n: number): string {
  return n === 1 ? "site" : "sites";
}

/**
 * The count line, in the wireframe's shape: "0 of 60 sites."
 *
 * The ratio is only printed when both halves are knowable. When the fleet page
 * came back full the denominator becomes "at least 60", because a full page and
 * a full page with more behind it are the same response; when the fleet did not
 * load there is no ratio at all, and the line says so rather than printing a
 * zero that reads as a confident answer.
 */
export function scopeCountLabel(scope: ResolvedSiteScope, total: FleetTotal): string {
  const n = sitesInScope(scope);

  if (n === null) {
    return scope.kind === "unresolved" && scope.because === "loading"
      ? "Working out how many sites this reaches."
      : "We could not read this organisation's sites, so we cannot tell you how many this reaches.";
  }

  switch (total.kind) {
    case "unknown":
      // Unreachable while resolveSiteScope returns `unresolved` for a null
      // fleet, and written out rather than thrown because the honest sentence
      // costs one line and a crash costs the step.
      return "We could not read this organisation's sites, so we cannot tell you how many this reaches.";
    case "exact":
      return `${n} of ${total.n} ${plural(total.n)}.`;
    case "floor":
      return `${n} of at least ${total.n} ${plural(total.n)}.`;
  }
}

/**
 * The sites this connection can NOT reach, when that is a number we may state.
 *
 * Null means "do not offer this", and it is null far more often than not:
 *
 *   - a FLOOR denominator makes the subtraction meaningless, because the sites
 *     we never loaded are neither in the scope nor in the excluded count;
 *   - mode `all` excludes nothing, so there is no set to show;
 *   - an unresolved scope has no numerator to subtract.
 *
 * The WORD is chosen from the basis, because the wireframe's "can never reach"
 * is true of a fixed list and false of a tag: a site given that tag next month
 * is reached, and the tag branch says so instead.
 */
export function excludedSitesLabel(
  scope: ResolvedSiteScope,
  total: FleetTotal,
): string | null {
  if (total.kind !== "exact") return null;
  if (scope.kind === "all" || scope.kind === "unresolved") return null;
  const covered = scope.kind === "none" ? 0 : scope.sites.length;
  const excluded = total.n - covered;
  if (excluded <= 0) return null;
  const basis = scope.kind === "sites" ? scope.basis : "list";
  return basis === "list"
    ? `See the ${excluded} this connection can never reach`
    : `See the ${excluded} it does not reach today`;
}

/**
 * Whether the wizard may move past step 3.
 *
 * THIS IS NOT `isScopeApprovable`, AND THE DIFFERENCE IS THE WHOLE POINT OF THE
 * SCREEN. An empty scope passes here. The wireframe is explicit that it is "a
 * working state, not an error", and an earlier revision of this surface that
 * blocked the step on an empty allowlist is the behaviour being corrected.
 *
 * `unresolved` still blocks, for the reason it blocks everywhere else: carrying
 * a selection whose meaning nobody has read is not a decision, and the count
 * beside it would be a number we cannot stand behind.
 */
export function canLeaveSiteStep(scope: ResolvedSiteScope): boolean {
  return scope.kind !== "unresolved";
}

/** Why the step is held, for the operator, or null when it is not held. */
export function siteStepBlockedReason(scope: ResolvedSiteScope): string | null {
  if (scope.kind !== "unresolved") return null;
  return scope.because === "loading"
    ? "Reading this organisation's sites."
    : "We could not read this organisation's sites, so we cannot tell you what this selection covers. Nothing is wrong with your choice; the list did not load.";
}

export interface SiteScopeModeOption {
  readonly value: SiteScopeMode;
  readonly label: string;
}

/**
 * The three modes, in the wireframe's order and wording.
 *
 * Order is load-bearing only in that the wireframe fixes it; the VALUES are the
 * three the schema permits (mcp_grants.site_scope_mode) and the labels are the
 * operator-facing names for them. `list` is "Named sites" on screen and `list`
 * on the wire, and nothing renders the wire word.
 */
export const SITE_STEP_MODES: readonly SiteScopeModeOption[] = [
  { value: "all", label: "All sites" },
  { value: "tags", label: "By tag" },
  { value: "list", label: "Named sites" },
];

/** The label on a token in the field. Tags carry the `tag:` prefix; sites do not. */
export function scopeTokenLabel(mode: SiteScopeMode, value: string): string {
  return mode === "tags" ? `tag:${value}` : value;
}

/**
 * What the empty field says when nothing is picked.
 *
 * Mode `all` never reaches this: it has no tokens by construction and an empty
 * token field there would read as "nothing selected" on the one mode that
 * selects everything.
 */
export function emptyTokenFieldLabel(mode: SiteScopeMode): string {
  return mode === "tags" ? "No tags selected" : "No sites selected";
}
