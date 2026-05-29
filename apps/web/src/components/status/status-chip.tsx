import { cn } from "@/lib/utils";

import { StatusDot, type StatusTone } from "./status-dot";

export interface StatusChipProps {
  tone: StatusTone;
  /** Human-readable status word ("Up", "Down", "Pending check"). */
  label: string;
  /** Optional pre-formatted relative-time string ("14d", "4m"). */
  time?: string;
  /** Pulse the dot for live / currently-up states. */
  pulse?: boolean;
  className?: string;
}

/**
 * StatusChip — dot + label + time. The canonical status indicator across the
 * app. Per DESIGN: "Status dot ... Always paired with a label or time." The
 * label is required; time is optional. Separator (`·`, U+00B7) sits in
 * muted-foreground; time uses mono + tabular numerals so adjacent rows
 * align visually.
 */
export function StatusChip({
  tone,
  label,
  time,
  pulse = false,
  className,
}: StatusChipProps) {
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 text-xs font-medium",
        className,
      )}
    >
      <StatusDot tone={tone} pulse={pulse} />
      <span>{label}</span>
      {time ? (
        <>
          <span aria-hidden="true" className="text-muted-foreground">
            {"·"}
          </span>
          <span className="font-mono tabular-nums text-muted-foreground">
            {time}
          </span>
        </>
      ) : null}
    </span>
  );
}
