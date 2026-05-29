/**
 * PhaseStepper — vertical pipeline visualization of the backup phases.
 *
 * Per the UX research dossier (backup-progress-ux-patterns.md):
 *   - Horizontal steppers fail because phase durations are wildly unequal
 *     (<1s queued, ~14s db, 1-5min archive, 5-15min upload, <1s manifest).
 *   - A vertical stepper lets the ACTIVE step expand to hold its sub-bar +
 *     current-artifact text without the whole layout dancing.
 *   - Past steps get a check + green; current pulses with a primary ring;
 *     future steps are hollow circles in muted color.
 *   - On failure: the active step turns destructive, future steps stay
 *     hollow (don't auto-checkmark).
 *
 * All animations gated by `motion-safe:`. With `prefers-reduced-motion`,
 * the pulsing ring becomes a static thicker border.
 */
import { CheckCircle2, Circle, CircleX, Loader2 } from "lucide-react";

import { cn } from "@/lib/utils";

export type StepStatus = "completed" | "active" | "pending" | "failed";

export interface StepperPhase {
  id: string;
  label: string;
  status: StepStatus;
}

interface PhaseStepperProps {
  phases: StepperPhase[];
  /** Optional className passed through to root <ol>. */
  className?: string;
  /** When true, show a horizontal compact dot-strip instead of vertical (used for the inline table-cell variant). */
  compact?: boolean;
  /** Optional label of the active phase (shown after the dot-strip in compact mode). */
  compactActiveLabel?: string;
}

function StepIcon({ status }: { status: StepStatus }) {
  switch (status) {
    case "completed":
      return <CheckCircle2 className="size-5 text-green-600" aria-hidden />;
    case "active":
      return (
        <Loader2
          className="size-5 text-[var(--color-primary)] motion-safe:animate-spin motion-reduce:animate-none"
          aria-hidden
        />
      );
    case "failed":
      return <CircleX className="size-5 text-[var(--color-destructive)]" aria-hidden />;
    case "pending":
    default:
      return <Circle className="size-5 text-[var(--color-muted-foreground)] opacity-60" aria-hidden />;
  }
}

export function PhaseStepper({
  phases,
  className,
  compact = false,
  compactActiveLabel,
}: PhaseStepperProps) {
  if (compact) {
    // Compact dot-strip for the table-cell inline variant. Each phase is a
    // small dot; the active one is a slightly larger filled dot with a ring.
    return (
      <ol
        className={cn("inline-flex items-center gap-1", className)}
        aria-label="Backup phase progress"
      >
        {phases.map((p) => (
          <li
            key={p.id}
            title={`${p.label} — ${p.status}`}
            aria-label={`${p.label}: ${p.status}`}
            className={cn(
              "size-1.5 rounded-full",
              p.status === "completed" && "bg-green-600",
              p.status === "active" &&
                "size-2 bg-[var(--color-primary)] ring-2 ring-[var(--color-primary)]/40 motion-safe:animate-pulse",
              p.status === "failed" && "size-2 bg-[var(--color-destructive)]",
              p.status === "pending" &&
                "border border-[var(--color-muted-foreground)] bg-transparent opacity-50",
            )}
          />
        ))}
        {compactActiveLabel ? (
          <span className="ml-2 text-xs font-medium">{compactActiveLabel}</span>
        ) : null}
      </ol>
    );
  }

  return (
    <ol className={cn("relative space-y-3", className)} aria-label="Backup pipeline">
      {phases.map((p, idx) => {
        const isLast = idx === phases.length - 1;
        return (
          <li key={p.id} className="relative flex items-start gap-3 pb-2">
            <div className="relative flex flex-col items-center">
              <StepIcon status={p.status} />
              {!isLast ? (
                <span
                  aria-hidden
                  className={cn(
                    "mt-1 h-6 w-px",
                    p.status === "completed"
                      ? "bg-green-600/40"
                      : "bg-[var(--color-muted-foreground)]/20",
                  )}
                />
              ) : null}
            </div>
            <div className="flex-1 pt-0.5">
              <span
                className={cn(
                  "text-sm",
                  p.status === "completed" && "text-[var(--color-foreground)]",
                  p.status === "active" && "font-medium text-[var(--color-foreground)]",
                  p.status === "failed" && "font-medium text-[var(--color-destructive)]",
                  p.status === "pending" && "text-[var(--color-muted-foreground)]",
                )}
              >
                {p.label}
              </span>
            </div>
          </li>
        );
      })}
    </ol>
  );
}
