import { cn } from "@/lib/utils";

// PHPSeverityChip — domain-specific severity chip for PHP E_* error levels.
// Collocated in /features/errors because the PHP severity scale (6 values) is
// not the same as the activity SeverityChip (3-value high/medium/low union) or
// VulnSeverityChip (4-value critical/high/medium/low). Token-only colors;
// dot + label per DESIGN.md "severity = chip with dot+label, never a bare dot".
//
// Mapping rationale (token-only, no off-token palette colors):
//   fatal       → destructive-subtle  (hard crash, site may be down)
//   warning     → warning-subtle      (non-fatal but operator should act)
//   notice      → muted               (informational, low urgency)
//   deprecated  → muted               (NOT purple — purple-for-status violates design rules)
//   bootstrap   → muted               (initialization noise)
//   unknown     → muted               (unrecognised code)

type PhpSeverity =
  | "fatal"
  | "warning"
  | "notice"
  | "deprecated"
  | "bootstrap"
  | "unknown";

const CHIP: Record<string, string> = {
  fatal: "bg-destructive-subtle text-destructive-subtle-fg",
  warning: "bg-warning-subtle text-warning-subtle-fg",
  notice: "bg-muted text-muted-foreground",
  deprecated: "bg-muted text-muted-foreground",
  bootstrap: "bg-muted text-muted-foreground",
  unknown: "bg-muted text-muted-foreground",
};

const DOT: Record<string, string> = {
  fatal: "bg-destructive",
  warning: "bg-warning",
  notice: "bg-muted-foreground",
  deprecated: "bg-muted-foreground",
  bootstrap: "bg-muted-foreground",
  unknown: "bg-muted-foreground",
};

const WORD: Record<string, string> = {
  fatal: "Fatal",
  warning: "Warning",
  notice: "Notice",
  deprecated: "Deprecated",
  bootstrap: "Bootstrap",
  unknown: "Unknown",
};

export function PHPSeverityChip({ severity }: { severity: string }) {
  // Normalise to a known key; fall back to 'unknown' for any future additions.
  const key: PhpSeverity =
    severity in CHIP ? (severity as PhpSeverity) : "unknown";

  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 rounded px-2 py-0.5 text-xs font-medium",
        CHIP[key],
      )}
    >
      <span
        aria-hidden="true"
        className={cn("size-1.5 rounded-full", DOT[key])}
      />
      {WORD[key]}
    </span>
  );
}
