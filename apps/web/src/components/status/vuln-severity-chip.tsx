import { cn } from "@/lib/utils";

export type VulnSeverity = "critical" | "high" | "medium" | "low" | "unknown";

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
  unknown: "Unknown",
};

const severityClasses: Record<VulnSeverity, string> = {
  critical: "bg-severity-critical text-destructive-foreground",
  high: "bg-severity-high text-destructive-foreground",
  // Medium and Low surfaces are warm/yellow and blue at lighter L; dark text
  // hits AA against both backgrounds.
  medium: "bg-severity-medium text-foreground",
  low: "bg-severity-low text-foreground",
  // Unknown is a genuinely unrated finding (no CVSS score reached the CP),
  // not a confirmed-low. GH #245: a CVSS ingestion bug once silently
  // bucketed every unrated finding as "Low", hiding a critical RCE behind a
  // muted label. Solid neutral slate with its own dedicated foreground
  // token (mirroring Critical/High, not Medium/Low's pale-surface pattern)
  // so it is deliberately, unmistakably distinct from the Low chip and
  // reads as "needs triage", never as "safe".
  unknown: "bg-severity-unknown text-severity-unknown-foreground",
};

/**
 * VulnSeverityChip — discrete 5-step vulnerability severity indicator.
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
