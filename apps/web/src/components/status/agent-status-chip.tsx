import { cn } from "@/lib/utils";
import {
  AGENT_STATUS_BG,
  AGENT_STATUS_ICON,
  AGENT_STATUS_LABEL,
  type AgentStatus,
} from "./agent-status";

export interface AgentStatusChipProps {
  status: AgentStatus;
  /** The site's raw last-reported agent_version, rendered as a mono suffix when present. */
  version?: string | null;
  className?: string;
}

/**
 * AgentStatusChip: small pill for the WPMgr agent plugin's freshness
 * relative to the currently published release. Mirrors UpdateChip/
 * BackupChip's pill treatment so the Sites table's Agent column reads
 * consistently with its Updates and Backup neighbors.
 */
export function AgentStatusChip({
  status,
  version,
  className,
}: AgentStatusChipProps) {
  const Icon = AGENT_STATUS_ICON[status];
  return (
    <span
      title={version ? `Agent ${version}` : undefined}
      className={cn(
        "inline-flex items-center gap-1 rounded px-2 py-0.5 text-xs font-medium",
        AGENT_STATUS_BG[status],
        className,
      )}
    >
      <Icon aria-hidden="true" className="size-3" />
      <span>{AGENT_STATUS_LABEL[status]}</span>
      {version ? (
        <span className="font-mono text-[10px] tabular-nums opacity-75">
          {version}
        </span>
      ) : null}
    </span>
  );
}
