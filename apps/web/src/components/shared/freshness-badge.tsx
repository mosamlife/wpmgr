import { cn, relativeTime } from "@/lib/utils";
import { useNow } from "@/lib/use-now";

// FreshnessBadge — "Updated 4m ago" / "Stale · 2d ago" / "Never", extracted from
// the per-card freshness logic in health/diagnostic-card.tsx (ADR-037 Batch 0).
// One source of truth for the fresh/stale boundary so every detail surface
// reads the same. Tokens only; tabular-nums on the relative number; a
// warning-token dot when past the staleness threshold.

// Default staleness boundary ≈ 1.5× a daily collection cadence (36h). Callers
// with a tighter SLA pass a smaller `staleAfterSeconds`.
const DEFAULT_STALE_AFTER_SECONDS = 129_600;

export interface FreshnessBadgeProps {
  /** ISO-8601 timestamp of the last collection, or null if never collected. */
  collectedAt: string | null;
  /** Seconds after `collectedAt` past which the value is considered stale. */
  staleAfterSeconds?: number;
  className?: string;
}

export function FreshnessBadge({
  collectedAt,
  staleAfterSeconds = DEFAULT_STALE_AFTER_SECONDS,
  className,
}: FreshnessBadgeProps) {
  // useNow must run before any early return to satisfy the Rules of Hooks.
  // It updates every 30 s — coarse enough not to thrash, fine enough that
  // freshness flips within half a minute of crossing the boundary.
  const now = useNow(30_000);

  const rel = collectedAt ? relativeTime(collectedAt) : null;

  if (!collectedAt || !rel) {
    return (
      <span
        className={cn(
          "inline-flex items-center gap-1.5 text-xs text-muted-foreground",
          className,
        )}
      >
        Never
      </span>
    );
  }

  const ageSeconds = (now - Date.parse(collectedAt)) / 1000;
  const stale = ageSeconds > staleAfterSeconds;

  if (stale) {
    return (
      <span
        className={cn(
          "inline-flex items-center gap-1.5 text-xs text-warning-subtle-fg",
          className,
        )}
      >
        <span
          aria-hidden="true"
          className="size-1.5 shrink-0 rounded-full bg-warning"
        />
        Stale
        <span aria-hidden="true">·</span>
        <span className="tabular-nums">{rel}</span>
      </span>
    );
  }

  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 text-xs text-muted-foreground",
        className,
      )}
    >
      Updated <span className="tabular-nums">{rel}</span>
    </span>
  );
}
