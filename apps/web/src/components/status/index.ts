/**
 * Status indicator primitives. Pure presentational components — props in,
 * JSX out. No data fetching. See DESIGN.md "Components" for the contract.
 */
export { StatusDot } from "./status-dot";
export type { StatusDotProps, StatusTone } from "./status-dot";
export { useStatusPulse } from "./use-status-pulse";

export { StatusChip } from "./status-chip";
export type { StatusChipProps } from "./status-chip";

export { UpdateChip } from "./update-chip";
export type { UpdateChipProps } from "./update-chip";

export { BackupChip } from "./backup-chip";
export type {
  BackupChipProps,
  BackupChipStatus,
} from "./backup-chip";

export { VulnSeverityChip } from "./vuln-severity-chip";
export type {
  VulnSeverityChipProps,
  VulnSeverity,
} from "./vuln-severity-chip";

export { ConnectionStateBadge } from "./connection-state-badge";
export type { ConnectionStateBadgeProps } from "./connection-state-badge";
