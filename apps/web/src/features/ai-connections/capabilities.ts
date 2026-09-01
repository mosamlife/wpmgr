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

/** The eight names policy.go's capabilityVocabulary knows today. */
export const KNOWN_CAPABILITIES: readonly string[] = [
  "mcp.sites.read",
  "mcp.uptime.read",
  "mcp.backups.read",
  "mcp.security.read",
  "mcp.activity.read",
  "mcp.performance.read",
  "mcp.diagnostics.read",
  "mcp.content.read",
];

const CAPABILITY_LABELS: Readonly<Record<string, string>> = {
  "mcp.sites.read": "Sites",
  "mcp.uptime.read": "Uptime",
  "mcp.backups.read": "Backups",
  "mcp.security.read": "Security",
  "mcp.activity.read": "Activity",
  "mcp.performance.read": "Performance",
  "mcp.diagnostics.read": "Diagnostics",
  "mcp.content.read": "Content",
};

/**
 * Human label for a capability wire string.
 *
 * A NAME THIS MAP DOES NOT KNOW STILL RENDERS, AS ITSELF. Falling back to a
 * placeholder like "Unknown capability" would re-create, one layer up, the
 * exact defect #652 was filed over: a value the server sent, dropped before an
 * operator could see it. The raw string is always legible even unlabelled,
 * because every member of this vocabulary is deliberately spelled as a
 * `mcp.<noun>.read` wire string and not an opaque id.
 */
export function capabilityLabel(capability: string): string {
  return CAPABILITY_LABELS[capability] ?? capability;
}
