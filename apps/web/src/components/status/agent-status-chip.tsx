import { cn } from "@/lib/utils";
import type { FleetAgentVersions } from "@wpmgr/api";
import {
  AGENT_STATUS_BG,
  AGENT_STATUS_ICON,
  agentStatusDisplayLabel,
  isFleetDerivedCurrent,
  type AgentStatus,
} from "./agent-status";

export interface AgentStatusChipProps {
  status: AgentStatus;
  /** The site's raw last-reported agent_version, rendered as a mono suffix when present. */
  version?: string | null;
  /**
   * Where the reference version behind `status` came from (see
   * FleetAgentVersions.reference_source). Omit only when the rollup itself
   * is unavailable to the caller; treated the same as "published" (no
   * qualifier) since that is the common case and there is nothing else to
   * say without it.
   */
  referenceSource?: FleetAgentVersions["reference_source"];
  className?: string;
}

/**
 * AgentStatusChip: small pill for the WPMgr agent plugin's per-site
 * freshness classification (GET /api/v1/fleet/agents' `status`). Mirrors
 * UpdateChip/BackupChip's pill treatment so the Sites table's Agent column
 * reads consistently with its Updates and Backup neighbors.
 *
 * `status` alone is not the whole story: it was computed against whatever
 * `latest_version` the rollup could find, and `referenceSource` says what
 * that was. When it is "published" (the release channel manifest), "Current"
 * really does mean "runs the newest agent that exists" and needs no
 * qualifier. When it is "fleet" (no manifest was readable, so the newest
 * agent_version already reported anywhere in this tenant's own fleet stood
 * in for it, the normal steady state for a self-hosted install), a site can
 * land in the "current" bucket while dozens of releases behind the real
 * latest, so this renders "Current in fleet" instead of a bare "Current"
 * and the title explains the reference. Every other status ("outdated",
 * "unknown", "ineligible") already reads the same regardless of source and
 * is left unchanged.
 */
export function AgentStatusChip({
  status,
  version,
  referenceSource,
  className,
}: AgentStatusChipProps) {
  const Icon = AGENT_STATUS_ICON[status];
  const fleetQualified = isFleetDerivedCurrent(status, referenceSource);
  const label = agentStatusDisplayLabel(status, referenceSource);
  const title = fleetQualified
    ? `${version ? `Agent ${version}, ` : ""}current in fleet (newest seen in this fleet, not a published release)`
    : version
      ? `Agent ${version}`
      : undefined;
  return (
    <span
      title={title}
      className={cn(
        "inline-flex items-center gap-1 rounded px-2 py-0.5 text-xs font-medium",
        AGENT_STATUS_BG[status],
        className,
      )}
    >
      <Icon aria-hidden="true" className="size-3" />
      <span>{label}</span>
      {version ? (
        <span className="font-mono text-[10px] tabular-nums opacity-75">
          {version}
        </span>
      ) : null}
    </span>
  );
}
