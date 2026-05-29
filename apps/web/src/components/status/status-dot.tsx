import { cn } from "@/lib/utils";

/**
 * Tone palette for status indicators across the app. Maps 1:1 to semantic
 * color tokens (success / warning / destructive / info) plus a neutral
 * "muted" tone used for unknown / pending / not-yet-checked states.
 */
export type StatusTone =
  | "success"
  | "warning"
  | "destructive"
  | "info"
  | "muted";

export interface StatusDotProps {
  tone: StatusTone;
  /** Render a low-opacity ping ring underneath the dot for live states. */
  pulse?: boolean;
  /**
   * Accessible label. REQUIRED when the dot stands alone with no sibling
   * text label. When a visible sibling label conveys the same meaning,
   * leave this undefined and the dot becomes aria-hidden.
   */
  label?: string;
  className?: string;
}

const toneToBg: Record<StatusTone, string> = {
  success: "bg-success",
  warning: "bg-warning",
  destructive: "bg-destructive",
  info: "bg-info",
  muted: "bg-muted-foreground",
};

/**
 * StatusDot — 8px filled circle in a semantic color.
 *
 * DESIGN contract: never use a colored dot alone. Always pair with a visible
 * label or time string (see StatusChip), OR pass an explicit `label` prop so
 * screen readers can announce the state.
 */
export function StatusDot({
  tone,
  pulse = false,
  label,
  className,
}: StatusDotProps) {
  const bg = toneToBg[tone];
  const a11y = label
    ? { role: "img" as const, "aria-label": label }
    : { "aria-hidden": true as const };

  return (
    <span
      {...a11y}
      className={cn(
        "relative inline-block size-2 shrink-0 rounded-full",
        bg,
        className,
      )}
    >
      {pulse ? (
        <span
          aria-hidden="true"
          className={cn(
            "absolute inset-0 rounded-full opacity-25 motion-safe:animate-ping",
            bg,
          )}
        />
      ) : null}
    </span>
  );
}
