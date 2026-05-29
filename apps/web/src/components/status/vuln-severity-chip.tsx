import { cn } from "@/lib/utils";

export type VulnSeverity = "critical" | "high" | "medium" | "low";

export interface VulnSeverityChipProps {
  severity: VulnSeverity;
  /** Optional count, rendered as "{count} {severity}" when present. */
  count?: number;
  className?: string;
}

const severityWord: Record<VulnSeverity, string> = {
  critical: "Critical",
  high: "High",
  medium: "Medium",
  low: "Low",
};

const severityClasses: Record<VulnSeverity, string> = {
  critical: "bg-severity-critical text-destructive-foreground",
  high: "bg-severity-high text-destructive-foreground",
  // Medium and Low surfaces are warm/yellow and blue at lighter L; dark text
  // hits AA against both backgrounds.
  medium: "bg-severity-medium text-foreground",
  low: "bg-severity-low text-foreground",
};

/**
 * VulnSeverityChip — discrete 4-step vulnerability severity indicator.
 *
 * Per DESIGN: severity is a *discrete* scale, never a continuous gradient,
 * and the severity *word* must always appear (never a bare dot). Counts
 * compose in front of the word ("12 Critical") so that operators can scan
 * a list and prioritize by the leading number.
 */
export function VulnSeverityChip({
  severity,
  count,
  className,
}: VulnSeverityChipProps) {
  const word = severityWord[severity];
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1 rounded px-2 py-0.5 text-xs font-medium",
        severityClasses[severity],
        className,
      )}
    >
      {typeof count === "number" ? (
        <span className="font-mono tabular-nums">{count}</span>
      ) : null}
      <span>{word}</span>
    </span>
  );
}
