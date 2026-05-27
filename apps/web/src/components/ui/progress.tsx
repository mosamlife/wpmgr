import * as React from "react";

import { cn } from "@/lib/utils";

// Minimal, accessible progress bar. Exposes the ARIA progressbar role with
// value/min/max so screen readers announce completion. `value` and `max` are
// clamped defensively. No external dependency.
export interface ProgressProps extends React.HTMLAttributes<HTMLDivElement> {
  value: number;
  max?: number;
  label?: string;
}

const Progress = React.forwardRef<HTMLDivElement, ProgressProps>(
  ({ className, value, max = 100, label, ...props }, ref) => {
    const safeMax = max > 0 ? max : 1;
    const clamped = Math.min(Math.max(value, 0), safeMax);
    const pct = Math.round((clamped / safeMax) * 100);
    return (
      <div
        ref={ref}
        role="progressbar"
        aria-valuemin={0}
        aria-valuemax={safeMax}
        aria-valuenow={clamped}
        aria-label={label}
        className={cn(
          "h-2 w-full overflow-hidden rounded-full bg-[var(--color-muted)]",
          className,
        )}
        {...props}
      >
        <div
          className="h-full bg-[var(--color-primary)] transition-[width] duration-300"
          style={{ width: `${pct}%` }}
        />
      </div>
    );
  },
);
Progress.displayName = "Progress";

export { Progress };
