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
} as const satisfies Readonly<Record<string, string>>;

/** A capability wire string this build's vocabulary knows. */
export type Capability = keyof typeof CAPABILITY_LABELS;

/**
 * The eight names policy.go's capabilityVocabulary knows today, DERIVED from
 * CAPABILITY_LABELS's keys rather than written out again. See the file header:
 * this is what makes the two-lists-disagree shape impossible here rather than
 * merely tested for.
 */
export const KNOWN_CAPABILITIES: readonly Capability[] = Object.keys(
  CAPABILITY_LABELS,
) as Capability[];

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
  // Seated but unreachable -- see CONFERRABLE_CAPABILITIES below for why this
  // one is never offered as a live checkbox.
  "mcp.content.read": "Read post and page content.",
} as const;

/**
 * The seven capabilities the server will actually confer, mirrored from
 * apps/api/internal/mcp/policy.go's scopeCapabilities[ScopeRead] (lines
 * 190-198). `mcp.content.read` is deliberately excluded: policy.go's own
 * comment above CapContentRead (~line 100-104) says there is no post/page
 * table, no agent command that returns content, and ADR-062 holds it behind
 * ship blockers -- the eighth name exists only because the DB CHECK constraint
 * (m131 DECISION 5) requires the Go vocabulary and the database to agree on
 * all eight, not because it can be granted.
 *
 * DERIVED, NOT A HAND-COPIED SUBSET. Filtering KNOWN_CAPABILITIES rather than
 * writing the seven names out again means a capability added to
 * CAPABILITY_LABELS defaults to "not conferrable" until this list is updated
 * on purpose -- the safer direction for a name the picker cannot yet prove the
 * server will honour.
 */
export const CONFERRABLE_CAPABILITIES: readonly Capability[] = KNOWN_CAPABILITIES.filter(
  (c) => c !== "mcp.content.read",
);
