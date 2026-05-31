import { cn } from "@/lib/utils";

// SavingsBadge — "−87%", tabular-nums. Computes the percentage reduction from
// original → current bytes. Green for a real saving, muted when there's no data
// or no change yet. tabular-nums so adjacent rows' percentages align.

/**
 * Compute the savings percentage (0–100) from original → current bytes.
 * Returns null when it can't be computed (missing/zero original, or current
 * larger than original — we don't show a "negative saving").
 */
function savingsPercent(
  originalBytes: number,
  currentBytes: number,
): number | null {
  if (!Number.isFinite(originalBytes) || originalBytes <= 0) return null;
  if (!Number.isFinite(currentBytes) || currentBytes < 0) return null;
  if (currentBytes >= originalBytes) return null;
  return Math.round(((originalBytes - currentBytes) / originalBytes) * 100);
}

export interface SavingsBadgeProps {
  originalBytes: number;
  currentBytes: number;
  className?: string;
}

export function SavingsBadge({
  originalBytes,
  currentBytes,
  className,
}: SavingsBadgeProps) {
  const pct = savingsPercent(originalBytes, currentBytes);

  if (pct === null) {
    return (
      <span
        className={cn(
          "font-mono text-xs tabular-nums text-[var(--color-muted-foreground)]",
          className,
        )}
        aria-label="No savings yet"
      >
        —
      </span>
    );
  }

  return (
    <span
      className={cn(
        "font-mono text-xs font-medium tabular-nums text-[var(--color-success)]",
        className,
      )}
      aria-label={`${pct} percent smaller`}
    >
      {"−"}
      {pct}%
    </span>
  );
}
