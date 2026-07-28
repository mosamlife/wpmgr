import { ArrowUpCircle, Ban, CheckCircle2, HelpCircle } from "lucide-react";
import type { FleetAgentVersions } from "@wpmgr/api";

// Shared status -> icon/label/tint maps for the agent-freshness classification.
// Split from agent-status-chip.tsx (the component) so this module can export
// plain constants without tripping the react-refresh/only-export-components
// rule, mirrors features/fleet/uptime-status.ts + StatusChip.tsx.

/**
 * Agent-version freshness classification, matching the control plane's
 * `FleetAgentSite.status` (GET /api/v1/fleet/agents). "ineligible" means
 * the site runs a build that can never self-update (the wordpress.org
 * distribution ships with the self-updater stripped), so it is reported
 * distinctly rather than as "outdated": there is nothing the operator can
 * action from here, and those sites update through their own channel.
 */
export type AgentStatus = "current" | "outdated" | "unknown" | "ineligible";

export const AGENT_STATUS_LABEL: Record<AgentStatus, string> = {
  current: "Current",
  outdated: "Outdated",
  unknown: "Unknown",
  ineligible: "Not self-updating",
};

export const AGENT_STATUS_BG: Record<AgentStatus, string> = {
  current: "bg-success-subtle text-success-subtle-fg",
  outdated: "bg-warning-subtle text-warning-subtle-fg",
  unknown: "bg-muted text-muted-foreground",
  ineligible: "bg-muted text-muted-foreground",
};

export const AGENT_STATUS_ICON: Record<AgentStatus, typeof CheckCircle2> = {
  current: CheckCircle2,
  outdated: ArrowUpCircle,
  unknown: HelpCircle,
  ineligible: Ban,
};

/**
 * True only for the case that overclaims if left unlabeled: status
 * "current" classified against a reference that came from this tenant's own
 * fleet (the newest agent_version any of its sites has reported) rather than
 * the published release manifest. A site in this bucket can be "current"
 * while dozens of releases behind the real latest agent build. See
 * FleetAgentVersions.reference_source.
 *
 * "outdated" needs no equivalent qualifier: it stays true regardless of
 * where the reference came from (a site behind the newest agent this fleet
 * has seen is, at minimum, also behind whatever the real latest is).
 */
export function isFleetDerivedCurrent(
  status: AgentStatus,
  referenceSource: FleetAgentVersions["reference_source"] | undefined,
): boolean {
  return status === "current" && referenceSource === "fleet";
}

/**
 * Display label for prose/tooltips (NOT the filter facet name, which stays
 * AGENT_STATUS_LABEL so URL search params and filter derivation are
 * unaffected). Returns "Current in fleet" for the fleet-derived "current"
 * case per isFleetDerivedCurrent; every other status/source combination
 * falls through to the plain AGENT_STATUS_LABEL.
 */
export function agentStatusDisplayLabel(
  status: AgentStatus,
  referenceSource: FleetAgentVersions["reference_source"] | undefined,
): string {
  if (isFleetDerivedCurrent(status, referenceSource)) return "Current in fleet";
  return AGENT_STATUS_LABEL[status];
}
