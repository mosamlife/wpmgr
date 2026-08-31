// Screen 8 (wireframes.html#s8, "Revised 2026-08-24") — the site-allowlist
// honesty surface. Pure copy and a banned-word guard, kept separate from the
// rendering component so the guard test can run over plain strings.
//
// WHAT THIS SCREEN EXISTS TO STOP. Site scoping is a check WPMgr makes when an
// assistant asks to do something. It is NOT a boundary inside the database:
// there is no per-connection database role, no row-level policy keyed on a
// grant, nothing that would stop a bug elsewhere in this codebase from
// reaching a site outside the list. The check is real and it runs on every
// request, but it is one gate in application code, not a wall. Copy on this
// screen must say exactly that and nothing stronger.
//
// THE BANNED LIST IS THE WIREFRAME'S OWN (wireframes.html:2844-2846), not a
// paraphrase of it: "isolated, isolation, sandboxed, walled off, cannot
// reach, impossible, guaranteed, secure, safe, airtight, hard limit." Every
// one of those words claims a boundary this feature does not have.
export const ENFORCEMENT_BANNED_WORDS: readonly string[] = [
  "isolated",
  "isolation",
  "sandboxed",
  "walled off",
  "cannot reach",
  "impossible",
  "guaranteed",
  "secure",
  "safe",
  "airtight",
  "hard limit",
];

function bannedWordPattern(word: string): RegExp {
  // Multi-word phrases match as a literal substring. Single words need a word
  // boundary on BOTH sides, so "safe" fires on "Safe." and on "safe," but not
  // inside "safety" and NOT inside "unsafe": "un" and "safe" are both word
  // characters, so there is no boundary between "n" and "s" for the leading
  // \b to match. PREFIXED AND SUFFIXED FORMS ARE OUT OF REACH BY DESIGN and
  // widening the pattern would redden honest copy -- "unsafe" is the opposite
  // of the overclaim this list exists to catch, and a guard that flags a
  // truthful warning is a guard someone switches off.
  const escaped = word.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  return new RegExp(word.includes(" ") ? escaped : `\\b${escaped}\\b`, "i");
}

/**
 * Every banned word or phrase found in `text`, in list order. Empty means
 * clean. Used both by the guard test (over the literal strings below) and by
 * a render test (over the mounted screen's text content), because a guard
 * that only ever reads its own source can be satisfied by a source that lies
 * about what actually renders.
 */
export function bannedWordHits(text: string): string[] {
  return ENFORCEMENT_BANNED_WORDS.filter((word) => bannedWordPattern(word).test(text));
}

/**
 * The sentence every enforcement box opens with, for the two modes that carry
 * an actual list (named sites, and sites-by-tag). It states the mechanism
 * (checked per request, before any site is contacted) and the consequence of
 * missing the list (refused, recorded), without saying WHERE the check lives
 * relative to anything else.
 */
export const ENFORCEMENT_CHECK_SENTENCE =
  "Every request from this assistant is checked against this list before WPMgr contacts any site. A request for a site that is not on the list is refused and recorded in the audit log.";

/**
 * Named-sites mode (basis 'list'). The set is fixed by construction — the
 * operator picked it — so the only thing left to say is that nothing else is
 * covered, including a site enrolled afterwards.
 */
export const ENFORCEMENT_LIST_MODE_SENTENCE =
  "This is a fixed list. No other site is covered by this connection, including one added to the organisation later.";

/**
 * Tag mode's drift sentence (wireframes.html:2884-2885, and this screen's
 * whole reason for existing per the build brief). NOT SOFTENED: it says
 * "included automatically" and "without anyone approving it" in exactly
 * those words, because that is the fact an operator choosing tag mode is
 * agreeing to and the one this screen must not let them miss.
 */
export const ENFORCEMENT_TAG_DRIFT_SENTENCE =
  "Sites added to these tags later will be included automatically, without anyone approving it.";

/**
 * Every-site mode. Quoted from wireframes.html:2890-2891 verbatim: there is
 * no list to check against, so the sentence says that rather than describing
 * an absent list as a boundary.
 */
export const ENFORCEMENT_ALL_MODE_SENTENCE =
  "Nothing is checked against a list because there is no list. This connection can read any site in the organisation, including one added after you approved it.";

/**
 * What we know about how often this assistant has been refused a site.
 *
 * THREE STATES, NOT A NUMBER. `unavailable` is today's only real value: no
 * endpoint on the control plane counts refusals per connection yet (grep
 * confirms `/api/v1/mcp/connections` returns only `site_scope_mode`, not a
 * count). `zero` and `count` exist so the box is ready the day that endpoint
 * ships, and so a test can pin the difference between "none happened" and
 * "we don't know" — the exact collapse this screen exists to prevent.
 * Rendering `unavailable` as `zero` would fabricate a fact nobody measured;
 * rendering it as a bare 0 or a dash would read as data.
 */
export type RefusalsSummary =
  | { readonly kind: "unavailable" }
  | { readonly kind: "zero"; readonly windowDays: number }
  | { readonly kind: "count"; readonly count: number; readonly windowDays: number };

/**
 * The sentence for a refusals summary, or null for `unavailable` when the
 * caller has chosen to omit the block entirely rather than render an explicit
 * placeholder (see `describeRefusalsExplicit` for the other choice).
 */
export function describeRefusals(summary: RefusalsSummary): string {
  switch (summary.kind) {
    case "unavailable":
      return "We do not yet track how many requests have been refused for this connection.";
    case "zero":
      return "No requests have been refused.";
    case "count":
      return `Refused ${summary.count} time${summary.count === 1 ? "" : "s"} in the last ${summary.windowDays} days.`;
  }
}

// "How we check this" dialog copy (wireframes.html:2904-2914), kept as named
// constants so the render test and the banned-word guard both read the exact
// strings that ship, not a re-typed approximation of them.
export const HOW_WE_CHECK_TITLE = "How WPMgr checks site access";

export const HOW_WE_CHECK_MECHANISM =
  "When an assistant asks to do something, WPMgr resolves which sites the request covers, then checks each one against that assistant's list. Sites that are not on the list are dropped from the request and written to the audit log as refused. The assistant is told which sites were refused and why.";

export const HOW_WE_CHECK_SCOPE =
  "This check runs in one place, on every assistant request, and it runs before anything reaches a site.";

export const HOW_WE_CHECK_HEADING = "What this does not do";

export const HOW_WE_CHECK_REMEDY =
  "It is a check WPMgr makes when the assistant asks. It is not a separate boundary inside the database. If a site must never be touched by any assistant, pause the assistant or take the site off every list, rather than relying on the check alone.";

export const HOW_WE_CHECK_AUDIT_PATH = "Security → Audit → Denied";

/**
 * Every user-facing string this screen ships, gathered in one place so the
 * banned-word guard can scan the whole surface without depending on a render.
 * A NEW STRING ADDED TO THIS SCREEN AND NOT ADDED HERE IS NOT GUARDED — the
 * render-level test in site-enforcement-box.test.tsx is the backstop for
 * that gap.
 */
export function allEnforcementScreenStrings(): readonly string[] {
  return [
    ENFORCEMENT_CHECK_SENTENCE,
    ENFORCEMENT_LIST_MODE_SENTENCE,
    ENFORCEMENT_TAG_DRIFT_SENTENCE,
    ENFORCEMENT_ALL_MODE_SENTENCE,
    describeRefusals({ kind: "unavailable" }),
    describeRefusals({ kind: "zero", windowDays: 7 }),
    describeRefusals({ kind: "count", count: 2, windowDays: 7 }),
    HOW_WE_CHECK_TITLE,
    HOW_WE_CHECK_MECHANISM,
    HOW_WE_CHECK_SCOPE,
    HOW_WE_CHECK_HEADING,
    HOW_WE_CHECK_REMEDY,
    HOW_WE_CHECK_AUDIT_PATH,
  ];
}
