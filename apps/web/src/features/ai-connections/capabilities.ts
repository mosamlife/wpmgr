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
