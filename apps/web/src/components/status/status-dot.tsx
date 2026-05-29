import { motion, useReducedMotion } from "motion/react";

import { cn } from "@/lib/utils";
import { statusPulse } from "@/lib/motion-presets";
import { useStatusPulse } from "@/components/status/use-status-pulse";

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
  /**
   * Render a low-opacity ping ring underneath the dot for live / streaming
   * states (e.g. an update task that is currently running). Perpetual loop,
   * always-on while true. Different concern from `pulseOnChange` below.
   */
  pulse?: boolean;
  /**
   * Run a ONE-SHOT 600ms scale + opacity pulse whenever the `tone` prop
   * changes. Use for state transitions that the operator should notice once
   * (a site flipping from Up to Down) without converting the dot into a
   * perpetual attention magnet. Defaults to false so existing call sites
   * keep their semantics.
   */
  pulseOnChange?: boolean;
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
 *
 * Phase 5 motion:
 *   • `pulse`         — perpetual `motion-safe:animate-ping` loop. Same as
 *                       before, unchanged. Used for "this is live right now".
 *   • `pulseOnChange` — ONE-SHOT scale+opacity pulse on tone change. Driven
 *                       by `statusPulse` from @/lib/motion-presets so the
 *                       timing matches everywhere. Respects
 *                       `prefers-reduced-motion` — when set, the pulse
 *                       collapses to a no-op.
 */
export function StatusDot({
  tone,
  pulse = false,
  pulseOnChange = false,
  label,
  className,
}: StatusDotProps) {
  const bg = toneToBg[tone];
  const reduced = useReducedMotion();
  // A monotonic counter that bumps on every tone change. We feed it to
  // motion's `key` so the animation re-mounts and runs from frame 0 each
  // time, instead of trying to interpolate from whatever state it was in.
  const pulseKey = useStatusPulse(tone);
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
      {pulseOnChange && !reduced && pulseKey > 0 ? (
        // Sibling layer that paints a transient ring matching the tone.
        // Sits behind the dot via `inset-0` so the pulse reads as the dot
        // itself flexing, not a separate element. `key={pulseKey}` forces a
        // re-mount on every tone change, which is what makes this a true
        // one-shot animation rather than a loop.
        <motion.span
          key={pulseKey}
          aria-hidden="true"
          initial={{ scale: 1, opacity: 0.6 }}
          animate={statusPulse()}
          className={cn(
            "absolute inset-0 rounded-full",
            bg,
          )}
        />
      ) : null}
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
