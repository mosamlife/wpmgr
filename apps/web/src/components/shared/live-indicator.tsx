import { cn } from "@/lib/utils";

// LiveIndicator — one token-colored connection/polling dot (ADR-037 Batch 0),
// replacing the scattered off-token bg-green-500 / bg-amber-500 duplicates.
// DESIGN.md: a status is always dot + label + (here) tone, never a bare color.
//
//   • live       → success dot, subtle pulse (animate-pulse; collapsed by the
//                  global prefers-reduced-motion rule, plus motion-reduce here).
//   • connecting → warning dot, pulse.
//   • idle       → muted-foreground dot, no motion.
//   • error      → destructive dot, no motion.

export type LiveState = "live" | "connecting" | "idle" | "error";

export interface LiveIndicatorProps {
  state: LiveState;
  /** Optional text label rendered beside the dot. */
  label?: string;
  className?: string;
}

const DOT: Record<LiveState, string> = {
  live: "bg-success",
  connecting: "bg-warning",
  idle: "bg-muted-foreground",
  error: "bg-destructive",
};

const PULSE: Record<LiveState, boolean> = {
  live: true,
  connecting: true,
  idle: false,
  error: false,
};

const DEFAULT_LABEL: Record<LiveState, string> = {
  live: "Live",
  connecting: "Connecting",
  idle: "Idle",
  error: "Disconnected",
};

export function LiveIndicator({ state, label, className }: LiveIndicatorProps) {
  const text = label ?? DEFAULT_LABEL[state];
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 text-xs text-muted-foreground",
        className,
      )}
    >
      <span
        aria-hidden="true"
        className={cn(
          "size-1.5 shrink-0 rounded-full",
          DOT[state],
          PULSE[state] && "animate-pulse motion-reduce:animate-none",
        )}
      />
      <span>{text}</span>
    </span>
  );
}
