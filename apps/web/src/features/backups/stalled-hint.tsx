/**
 * StalledHint — GH #279 "taking longer than expected" indicator.
 *
 * The CP's two-tier watchdog stamps `stalled_at` on a running snapshot once
 * it has gone quiet past the soft threshold (default 3 minutes) but is not
 * yet failed (the hard threshold, default 30 minutes). The run may still
 * complete normally, so this is deliberately a CALM status hint, not an
 * error: no destructive color, no icon, no side-stripe border. It shares
 * styling with the rest of the app's subtle "medium" status language (see
 * `components/shared/severity-chip.tsx`).
 *
 * Driven entirely by the pulled `stalled_at` field on the snapshot DTO
 * (never by the SSE frame directly) — see `use-backup-stream.ts`'s
 * `isStallHintPhase` for why the "stalled"/"resumed" SSE frames only trigger
 * a refetch instead of patching the cache. See `format-progress.ts`'s
 * `isSnapshotStalled` for the gating logic that decides when to render this.
 */
import { cn } from "@/lib/utils";

const STALLED_COPY =
  "This backup is taking longer than expected, but it is still running.";

export function StalledHint({ compact = false }: { compact?: boolean }) {
  return (
    <p className={cn("text-warning-subtle-fg", compact ? "text-[10px]" : "text-xs")}>
      {STALLED_COPY}
    </p>
  );
}
