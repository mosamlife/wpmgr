/**
 * StalledHint — GH #279 "taking longer than expected" indicator, extended by
 * GH #791 to also cover a snapshot whose initial dispatch is still being
 * retried.
 *
 * Two calm, non-destructive hints share this component, gated by whether a
 * `lastError` is supplied:
 *   - GH #279 "stalled": the CP's two-tier watchdog stamps `stalled_at` on a
 *     running snapshot that has gone quiet past the soft threshold (default
 *     3 minutes) but is not yet failed (the hard threshold, default 30
 *     minutes). The run may still complete normally. See
 *     `format-progress.ts`'s `isSnapshotStalled`.
 *   - GH #791 "retrying": the snapshot has never gotten past the initial
 *     command dispatch — the CP records a fresh `attempt_error` (m148
 *     `backup_snapshots.attempt_error`) each time that dispatch bounces off
 *     a transient failure and keeps retrying. See `format-progress.ts`'s
 *     `isSnapshotRetrying`.
 *
 * The "retrying" copy has two forms, chosen by `hasProgress`: a dispatch
 * that has never reached the runner reads "hasn't started on the site yet";
 * once real progress exists (the runner got in, then the CP lost contact
 * again on a later attempt), saying "hasn't started" beside a live file
 * counter would contradict what's on screen, so that case drops to
 * "Retrying automatically." alone. Adversarial review caught the
 * contradiction with a real route showing archiving progress alongside the
 * "hasn't started" copy.
 *
 * Deliberately a CALM status hint either way, not an error: no destructive
 * color, no icon, no side-stripe border — it shares styling with the rest of
 * the app's subtle "medium" status language (see
 * `components/shared/severity-chip.tsx`).
 *
 * Driven entirely by the pulled snapshot fields (never by the SSE frame
 * directly) — see `use-backup-stream.ts`'s `isStallHintPhase` and the
 * `retrying` frame handling for why those SSE frames only trigger a refetch
 * instead of patching the cache.
 */
import { cn } from "@/lib/utils";

const STALLED_COPY =
  "This backup is taking longer than expected, but it is still running.";
const RETRYING_COPY =
  "This backup hasn't started on the site yet. Retrying automatically.";

export function StalledHint({
  compact = false,
  lastError = null,
  hasProgress = false,
}: {
  compact?: boolean;
  /**
   * GH #791 — the last attempt's error (`backup_snapshots.attempt_error`).
   * When present, this renders the "retrying" copy (plus the error) instead
   * of the plain "stalled" copy — see module doc above.
   */
  lastError?: string | null;
  /**
   * GH #791 adv-review finding 4 — true once the snapshot has emitted any
   * real phase beyond `queued` (see `format-progress.ts`'s
   * `formatProgress().phase`), i.e. the runner DID reach the site at some
   * point before the CP lost contact again on a later attempt. "This backup
   * hasn't started on the site yet" is false in that case, so it's dropped
   * and only "Retrying automatically." plus the last error remains.
   */
  hasProgress?: boolean;
}) {
  const textClass = cn("text-warning-subtle-fg", compact ? "text-[10px]" : "text-xs");

  if (lastError) {
    if (compact) {
      return <p className={textClass}>Retrying. Last error: {lastError}</p>;
    }
    if (hasProgress) {
      return (
        <p className={textClass}>Retrying automatically. Last error: {lastError}</p>
      );
    }
    return (
      <div className="space-y-0.5">
        <p className={textClass}>{RETRYING_COPY}</p>
        <p className={textClass}>Last error: {lastError}</p>
      </div>
    );
  }

  return <p className={textClass}>{STALLED_COPY}</p>;
}
