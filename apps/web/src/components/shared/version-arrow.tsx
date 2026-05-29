// VersionArrow — shared mono from→to version delta using lucide ArrowRight.
// Replaces the inline VersionArrow in available-updates-card.tsx and the local
// VersionDiff in update-tasks-table.tsx. Reused by Errors/Security (Batch 4)
// for patched-in versions. Pure presentational; no side-effects, no hooks.

import { ArrowRight } from "lucide-react";

import { cn } from "@/lib/utils";

export interface VersionArrowProps {
  from: string;
  to: string;
  className?: string;
}

export function VersionArrow({ from, to, className }: VersionArrowProps) {
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1 font-mono text-xs tabular-nums",
        className,
      )}
    >
      <span className="text-muted-foreground">{from}</span>
      <ArrowRight aria-hidden="true" className="size-3.5 shrink-0 text-muted-foreground" />
      <span className="text-foreground">{to}</span>
    </span>
  );
}
