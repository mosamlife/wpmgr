import { ArrowUpCircle, Ban, CheckCircle2, HelpCircle } from "lucide-react";

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
