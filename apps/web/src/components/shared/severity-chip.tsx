import { cn } from "@/lib/utils";
import type { SiteActivityEvent } from "@wpmgr/api";

// SeverityChip — the labeled severity badge, promoted to /components/shared so
// Errors, Health, and Backup can import it without depending on the activity
// feature (ADR-037 "Impeccable pass", Batch 0). Token-only classes (subtle
// backgrounds keep dark + light + AA all holding) and a leading dot so the chip
// reads as a status, not decoration.

type Severity = SiteActivityEvent["severity"];

const CHIP: Record<Severity, string> = {
  high: "bg-destructive-subtle text-destructive-subtle-fg",
  medium: "bg-warning-subtle text-warning-subtle-fg",
  low: "bg-muted text-muted-foreground",
};

const DOT: Record<Severity, string> = {
  high: "bg-destructive",
  medium: "bg-warning",
  low: "bg-muted-foreground",
};

const WORD: Record<Severity, string> = {
  high: "High",
  medium: "Medium",
  low: "Low",
};

export function SeverityChip({ severity }: { severity: Severity }) {
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 rounded px-2 py-0.5 text-xs font-medium",
        CHIP[severity],
      )}
    >
      <span
        aria-hidden="true"
        className={cn("size-1.5 rounded-full", DOT[severity])}
      />
      {WORD[severity]}
    </span>
  );
}
