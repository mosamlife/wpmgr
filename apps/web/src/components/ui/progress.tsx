/**
 * Radix-backed Progress primitive. Supports both determinate and
 * indeterminate states — pass `value={null}` for indeterminate.
 *
 * Why Radix (vs the previous hand-rolled): we need `value={null}` semantics
 * so the bar can render a shimmer for opaque phases (queued / dumping_db /
 * submitting_manifest) without juggling `aria-valuenow={undefined}`
 * ourselves. Radix's <Progress.Root value={null}> drops aria-valuenow
 * cleanly and adds `data-state="indeterminate"` we can hook for the
 * shimmer.
 *
 * Smooth-fill trick (UX research dossier — backup-progress-implementation.md):
 *   - Indicator is `width: 100%` ALWAYS, positioned via `transform: translateX`.
 *   - We translate by `-(100% - pct%)` so 0% is fully off-canvas left.
 *   - `transition: transform 600ms ease-out` is LONGER than the SSE
 *     inter-event gap (~330 ms at ~3 events/sec). Each new value catches
 *     the in-flight tween, producing a continuous fill illusion rather
 *     than a staircase.
 *   - `motion-reduce:` short-circuits the transition to instant.
 *
 * Indeterminate state uses a 1.5 s shimmer keyframe defined in globals.css
 * (`@keyframes wpmgr-shimmer`).
 */
import * as React from "react";
import * as ProgressPrimitive from "@radix-ui/react-progress";

import { cn } from "@/lib/utils";

interface ProgressProps
  extends React.ComponentPropsWithoutRef<typeof ProgressPrimitive.Root> {
  /**
   * Value 0–100, or `null`/undefined for indeterminate. Pass null for phases
   * with no measurable percent (queued, dumping_db, submitting_manifest).
   */
  value?: number | null;
  /** Optional aria label override. */
  label?: string;
}

const Progress = React.forwardRef<
  React.ElementRef<typeof ProgressPrimitive.Root>,
  ProgressProps
>(({ className, value, label, ...props }, ref) => {
  const isIndeterminate = value === null || value === undefined;
  const safe = isIndeterminate
    ? undefined
    : Math.min(100, Math.max(0, Math.round(value)));

  return (
    <ProgressPrimitive.Root
      ref={ref}
      value={isIndeterminate ? null : safe}
      aria-label={label}
      className={cn(
        "relative h-2 w-full overflow-hidden rounded-full bg-[var(--color-muted)]",
        className,
      )}
      {...props}
    >
      <ProgressPrimitive.Indicator
        className={cn(
          "h-full w-full flex-1 bg-[var(--color-primary)]",
          "motion-safe:transition-transform motion-safe:duration-[600ms] motion-safe:ease-out",
          isIndeterminate
            ? "motion-safe:animate-[wpmgr-shimmer_1.5s_ease-in-out_infinite] motion-reduce:opacity-50"
            : "",
        )}
        style={
          isIndeterminate
            ? undefined
            : { transform: `translateX(-${100 - (safe ?? 0)}%)` }
        }
      />
    </ProgressPrimitive.Root>
  );
});
Progress.displayName = "Progress";

export { Progress };
