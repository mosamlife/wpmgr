// The v1 capability vocabulary, mirrored from apps/api/internal/mcp/policy.go's
// capabilityVocabulary. This is the one place a human label is attached to a
// capability wire string, so the connections list and the capability picker
// (#660) share it instead of drifting into two label maps that disagree.
//
// THE SERVER DOES NOT FILTER AN UNKNOWN CAPABILITY OUT OF WHAT IT RETURNS.
// capabilitiesFromColumn in policy.go reads mcp_grants.capabilities back
// without dropping anything outside this build's vocabulary, on purpose: doing
// so here would let a UI's own (possibly stale) label map decide what is true
// about a stored grant. capabilityLabel below preserves that property -- an
// unrecognised string still renders, as itself, rather than being silently
// dropped or replaced with "unknown".
//
// THERE IS EXACTLY ONE LIST, NOT TWO. A review on #652 caught that an earlier
// version of this file wrote KNOWN_CAPABILITIES and CAPABILITY_LABELS out
// separately, so the picker in #660 could update one and silently leave the
// other stale -- the same shape that let scopeCapabilities and
// capabilityVocabulary disagree in policy.go (tracked there as #653) and that
// let the Go vocabulary and the database CHECK disagree until m131 pinned them
// against each other. Restating a runtime test that compares two lists would
// only re-add the second list one file over. Instead CAPABILITY_LABELS is the
// only place a capability name is spelled, and everything else below is
// derived from its keys, so a capability added to one is a member of the other
// BY CONSTRUCTION -- there is no second collection left to fall out of sync,
// and no mutation of "add it in one place and not the other" is expressible
// any more, in a test or otherwise.
export const CAPABILITY_LABELS = {
  "mcp.sites.read": "Sites",
  "mcp.uptime.read": "Uptime",
  "mcp.backups.read": "Backups",
  "mcp.security.read": "Security",
  "mcp.activity.read": "Activity",
  "mcp.performance.read": "Performance",
  "mcp.diagnostics.read": "Diagnostics",
  "mcp.content.read": "Content",
  // The first WRITE name in the vocabulary (tracka-cache-purge design v7,
  // ADR-061 option B: per-call human approval, no automation may ever
  // approve). It does not end in `.read`: calling it does not change
  // anything by itself, it only creates a request that a person in WPMgr
  // must approve before anything runs. See CAPABILITY_KIND below for the
  // property that makes this row impossible to render next to the reads by
  // accident.
  "mcp.cache.purge": "Ask to clear the site cache",
  // The site-tools engine (scope mcp:site). The first is a read that is NOT in
  // any preset, because it can return page text; the second is a request, like
  // the cache row: nothing runs until a person approves it in WPMgr.
  "mcp.ability.read": "See this site's tools and read its published pages",
  "mcp.ability.request": "Ask to make changes through the site's tools. You approve each one.",
} as const satisfies Readonly<Record<string, string>>;

/** A capability wire string this build's vocabulary knows. */
export type Capability = keyof typeof CAPABILITY_LABELS;

/**
 * The nine names policy.go's capabilityVocabulary knows today, DERIVED from
 * CAPABILITY_LABELS's keys rather than written out again. See the file header:
 * this is what makes the two-lists-disagree shape impossible here rather than
 * merely tested for.
 */
export const KNOWN_CAPABILITIES: readonly Capability[] = Object.keys(
  CAPABILITY_LABELS,
) as Capability[];

/**
 * What KIND of thing a capability is: something the connection can see, or
 * something it can ASK for -- never something it can just do. `Record<Capability,
 * "read" | "write">` rather than a partial map so TypeScript refuses to
 * compile a tenth capability added to CAPABILITY_LABELS without a kind here,
 * the same "one list, derived everywhere else" property CAPABILITY_DESCRIPTIONS
 * already holds, extended to the one distinction that decides which of the two
 * visibly separate groups a row renders in (design v7 S2.1: "write rows
 * visibly distinct, never pre-ticked, in no preset"). That holds exactly for
 * mcp.cache.purge, on the wizard and on the consent screen. The site-tools rows
 * are in no preset either, but their opening state differs by surface: the
 * connection wizard opens them clear, and the consent screen opens them ticked
 * when the requesting app asked for mcp:site and the server offers them.
 *
 * "write" IS NOT "this runs unattended". Every write capability in this
 * vocabulary gates its own call behind a person approving a specific request
 * (ADR-061 option B); the kind only decides how the row is grouped and
 * described on screen, never whether it needs approval -- that is true of the
 * one write capability unconditionally, not toggled by this field.
 */
export const CAPABILITY_KIND: Readonly<Record<Capability, "read" | "write">> = {
  "mcp.sites.read": "read",
  "mcp.uptime.read": "read",
  "mcp.backups.read": "read",
  "mcp.security.read": "read",
  "mcp.activity.read": "read",
  "mcp.performance.read": "read",
  "mcp.diagnostics.read": "read",
  "mcp.content.read": "read",
  "mcp.cache.purge": "write",
  "mcp.ability.read": "read",
  "mcp.ability.request": "write",
};

/**
 * The two site-tools capabilities. They render in their own box (see
 * AbilityCapabilityBox), never in the plain read list and never in a preset:
 * the read can return page text. The connection wizard opens both rows clear.
 * The consent screen opens both ticked when the requesting app asked for
 * mcp:site and the server offers them, and the person can clear either one.
 * The request needs the read, on both surfaces: see nextAbilityTicks.
 */
export const ABILITY_CAPABILITIES = ["mcp.ability.read", "mcp.ability.request"] as const;

export function isAbilityCapability(capability: string): boolean {
  return (ABILITY_CAPABILITIES as readonly string[]).includes(capability);
}

/** One of the two site-tools rows: "see what the site can do", or "ask for changes". */
export type AbilityRow = "read" | "request";

/** Whether each of the two site-tools rows is ticked. */
export interface AbilityTicks {
  readonly read: boolean;
  readonly request: boolean;
}

/**
 * The two site-tools rows after the person sets one of them.
 *
 * "ASK FOR CHANGES" NEEDS "SEE WHAT THE SITE CAN DO". The tool that carries a
 * request is declared with the read capability, so a connection holding the
 * request alone cannot call it. The two rows therefore move together in two
 * directions and stay independent in the others:
 *
 *   - ticking the request ticks the read;
 *   - clearing the read clears the request;
 *   - clearing the request leaves the read as it was;
 *   - ticking the read leaves the request as it was.
 *
 * The result never has the request ticked without the read, whatever `current`
 * held.
 */
export function nextAbilityTicks(
  current: AbilityTicks,
  row: AbilityRow,
  ticked: boolean,
): AbilityTicks {
  if (row === "request") {
    return ticked ? { read: true, request: true } : { read: current.read, request: false };
  }
  return ticked ? { read: true, request: current.request } : { read: false, request: false };
}

/**
 * The kind of a capability wire string, for a name this build may not know.
 * Falls back to "read" -- the quieter, more conservative badge -- rather than
 * refusing to render, for the same reason capabilityLabel falls back to the
 * raw string: an unrecognised name a live grant actually holds must still be
 * shown, not dropped.
 */
export function capabilityKind(capability: string): "read" | "write" {
  return (CAPABILITY_KIND as Readonly<Record<string, "read" | "write">>)[capability] ?? "read";
}

/**
 * Human label for a capability wire string.
 *
 * A NAME THIS MAP DOES NOT KNOW STILL RENDERS, AS ITSELF. Falling back to a
 * placeholder like "Unknown capability" would re-create, one layer up, the
 * exact defect #652 was filed over: a value the server sent, dropped before an
 * operator could see it. The raw string is always legible even unlabelled,
 * because every member of this vocabulary is deliberately spelled as a
 * `mcp.<noun>.read` wire string and not an opaque id.
 *
 * Takes a bare `string`, not `Capability`, on purpose: the server does not
 * filter the column it returns (see the file header), so a live grant can
 * hold a name outside this build's vocabulary and this function has to accept
 * it rather than refuse to compile against it.
 */
export function capabilityLabel(capability: string): string {
  return (CAPABILITY_LABELS as Readonly<Record<string, string>>)[capability] ?? capability;
}

/**
 * What each capability actually permits, in an operator's words, for the
 * capability picker (step 4, "Choose what it may do", TOKEN path only --
 * connect-wizard.tsx). `Record<Capability, string>` rather than a partial map
 * so TypeScript itself refuses to compile a ninth capability added to
 * CAPABILITY_LABELS above without a blurb here -- the same "one list, derived
 * everywhere else" property the file header describes, extended to copy.
 *
 * ALL EIGHT ARE READ-ONLY, INCLUDING THE ONE THAT CANNOT BE GRANTED YET. There
 * is no ninth "write" or "propose" capability anywhere in this build --
 * mcp.sites.write and mcp.sites.restart appear only as REJECTED-VALUE test
 * fixtures in apps/api, never as something Authenticate can hold. Every blurb
 * below describes seeing something, never changing it.
 */
export const CAPABILITY_DESCRIPTIONS: Readonly<Record<Capability, string>> = {
  "mcp.sites.read": "See the fleet inventory: site names, URLs and tags.",
  "mcp.uptime.read": "See uptime checks and outage history for sites in scope.",
  "mcp.backups.read": "See backup runs, their status, and when each last completed.",
  "mcp.security.read": "See security findings and scan results for sites in scope.",
  "mcp.activity.read": "See the activity log: what changed, and when.",
  "mcp.performance.read": "See Core Web Vitals and other performance metrics.",
  "mcp.diagnostics.read": "See health checks and diagnostic reports for sites in scope.",
  // Seated but unreachable -- see CONFERRABLE_READS below for why this
  // one is never offered as a live checkbox.
  "mcp.content.read": "Read post and page content.",
  // WRITE. Every other blurb above describes seeing something; this one
  // describes asking. It never changes anything by itself: calling the tool
  // creates a request, and nothing runs until a person allowed to clear
  // caches on that site approves it in WPMgr (ADR-061 option B). The wizard
  // and the consent screen render this row in its own bordered group, never
  // pre-ticked on either surface and never part of a preset (design v7 S2.1,
  // S6 row W1). The two site-tools rows follow a different opening rule: see
  // ABILITY_CAPABILITIES.
  "mcp.cache.purge":
    "Ask to clear the page cache on one site at a time, for the whole site or one page " +
    "address. Nothing runs until someone allowed to clear caches on that site approves " +
    "the request in WPMgr. If approved, WPMgr deletes its own cached pages for that " +
    "site. It skips every hosting cache, because WPMgr has not yet confirmed that any " +
    "of them clears only this site, so visitors may still get cached pages from the " +
    "host until they expire. Pages load slower until the cache refills.",
  "mcp.ability.read":
    "List the tools a site offers, describe one, and run the ones WPMgr has reviewed as " +
    "read-only, such as reading a published page. This can return the text of pages on " +
    "the sites you chose. It changes nothing.",
  "mcp.ability.request":
    "Ask to make a change through one of a site's reviewed tools. Nothing runs until " +
    "someone allowed to edit that site's content approves the request in WPMgr. You " +
    "approve each request one at a time.",
} as const;

/**
 * The seven READ capabilities the server will actually confer, mirrored from
 * apps/api/internal/mcp/policy.go's scopeCapabilities[ScopeRead] (lines
 * 190-198). `mcp.content.read` is deliberately excluded: policy.go's own
 * comment above CapContentRead (~line 100-104) says there is no post/page
 * table, no agent command that returns content, and ADR-062 holds it behind
 * ship blockers -- the eighth name exists only because the DB CHECK constraint
 * (m131 DECISION 5) requires the Go vocabulary and the database to agree on
 * all eight, not because it can be granted.
 *
 * DERIVED, NOT A HAND-COPIED SUBSET, AND DERIVED BY KIND. Filtering
 * KNOWN_CAPABILITIES by `kind === "read"` rather than writing the six names
 * out again means a capability added to CAPABILITY_LABELS defaults to "not
 * conferrable as a read" until this list is updated on purpose -- the safer
 * direction for a name the picker cannot yet prove the server will honour --
 * and means the one write name can never end up in a set this file calls
 * "reads" by a second list quietly disagreeing with CAPABILITY_KIND.
 *
 * THIS IS THE SET "READ EVERYTHING" ACTUALLY MEANS (ruling 33; design v7
 * S2.1 "today :224 sends CONFERRABLE_CAPABILITIES, which would pick up the
 * write row"). A preset must never include a capability that needs its own
 * per-call approval, so the preset is built from this list and never from
 * CONFERRABLE_CAPABILITIES below.
 */
export const CONFERRABLE_READS: readonly Capability[] = KNOWN_CAPABILITIES.filter(
  (c) => c !== "mcp.content.read" && !isAbilityCapability(c) && CAPABILITY_KIND[c] === "read",
);

/**
 * Every capability this build's picker may offer at all -- the seven
 * conferrable reads plus the one write, `mcp.cache.purge`. Use this for "does
 * the picker know this name", never for a preset: see CONFERRABLE_READS for
 * why the two must not be the same list.
 */
export const CONFERRABLE_CAPABILITIES: readonly Capability[] = KNOWN_CAPABILITIES.filter(
  (c) => c !== "mcp.content.read",
);
