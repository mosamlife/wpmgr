import { useEffect, useState } from "react";
import { CalendarClock } from "lucide-react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import {
  GRACE_WINDOW_HOURS,
  formatAbsolute,
  formatClockAndDay,
  formatCountdown,
  outstandingWork,
} from "./schedule";
import type { UpdateRun } from "@wpmgr/api";

/**
 * How often a live countdown re-renders. One second reads as a clock rather
 * than a stale label, and a scheduled run is the one screen an operator sits
 * on waiting for something to happen. Cheap: one `setInterval` per mounted
 * countdown, cleared on unmount, and only ever mounted for a run that is
 * actually `scheduled`.
 */
const TICK_MS = 1000;

/** Current wall clock, re-read every second while mounted. */
function useTick(active: boolean): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return;
    const id = window.setInterval(() => setNow(Date.now()), TICK_MS);
    return () => window.clearInterval(id);
  }, [active]);
  return now;
}

/**
 * "Starts in 8h 42m · 02:00 19 Aug 2026 Europe/London".
 *
 * GH #463: relative AND absolute, and the absolute always carries its zone.
 * The countdown answers "how long have I got"; the stamp answers "is that the
 * time I meant"; the zone is what makes the second answer true. Before this,
 * the run page printed the raw ISO string and the wizard's `datetime-local`
 * named no zone at all, so an operator in one zone scheduling a site in
 * another was shown neither.
 */
export function ScheduleCountdown({
  scheduledAt,
  className,
}: {
  scheduledAt: string;
  className?: string;
}) {
  const now = useTick(true);
  const countdown = formatCountdown(scheduledAt, now);
  const absolute = formatAbsolute(scheduledAt);

  return (
    <span
      className={cn(
        "inline-flex flex-wrap items-baseline gap-x-1.5 tabular-nums",
        className,
      )}
    >
      <span className="font-medium text-[var(--color-foreground)]">
        {countdown ? `Starts in ${countdown}` : "Due now"}
      </span>
      <span aria-hidden="true" className="text-[var(--color-muted-foreground)]">
        ·
      </span>
      <time
        dateTime={scheduledAt}
        className="text-[var(--color-muted-foreground)]"
      >
        {absolute}
      </time>
    </span>
  );
}

/**
 * GH #463 — the expired state. This is the feature, not an error message.
 *
 * A schedule that quietly stops is the defining failure of this product
 * category, so this says three things in this order: what was supposed to
 * happen, why it did not, and that NOTHING WAS TOUCHED. Then one action.
 *
 * Two deliberate choices that are easy to undo by accident:
 *
 *  1. `warning`, never `destructive`. A missed schedule is not a broken site.
 *     Red here would tell an agency that their client's sites failed when
 *     nothing was ever sent to them, which is both false and the more
 *     alarming of the two readings. Same call as the `expired` run tone in
 *     features/updates/update-status.tsx.
 *  2. No apology. "Sorry, something went wrong" hides the one fact the
 *     operator needs, which is that their fleet is untouched and the updates
 *     are still outstanding.
 *
 * The grace window is stated as a number the control plane enforces
 * (apps/api/internal/update/dispatch.go:45), not as a vague "too long ago".
 */
export function ExpiredRunNotice({
  run,
  onRunNow,
}: {
  run: UpdateRun;
  /**
   * Re-run the work now. Undefined when the operator's role or the run's
   * shape makes retry unavailable, in which case no button is rendered at
   * all: a disabled action with no stated cause is worse than none.
   */
  onRunNow?: () => void;
}) {
  const { updates, sites } = outstandingWork(run);
  // `updated_at` is the instant the dispatcher moved this run to `expired`,
  // which is the moment WPMgr reached it. There is no separate `expired_at`
  // on the wire, and inventing one client-side would be a guess.
  const reachedAt = formatClockAndDay(run.updated_at);
  const scheduledFor = run.scheduled_at
    ? formatClockAndDay(run.scheduled_at)
    : null;

  return (
    <div
      role="status"
      className={cn(
        "rounded-xl border p-4 sm:p-5",
        "border-[var(--color-warning)]/30 bg-[var(--color-warning-subtle)]",
        "text-[var(--color-warning-subtle-fg)]",
      )}
    >
      <div className="flex gap-3">
        <CalendarClock
          aria-hidden="true"
          className="mt-0.5 size-5 shrink-0 text-[var(--color-warning)]"
        />
        <div className="space-y-2">
          <h2 className="text-sm font-semibold text-[var(--color-foreground)]">
            This run didn&rsquo;t start, and nothing was sent to your sites
          </h2>
          <p className="text-sm">
            {scheduledFor ? `Scheduled for ${scheduledFor}. ` : null}
            WPMgr couldn&rsquo;t reach it until {reachedAt}, past the{" "}
            {GRACE_WINDOW_HOURS}-hour window for update runs.
          </p>
          <p className="text-sm">
            All {updates} update{updates === 1 ? "" : "s"}{" "}
            {updates === 1 ? "is" : "are"} still outstanding across {sites}{" "}
            site{sites === 1 ? "" : "s"}. No site was contacted.
          </p>
          {onRunNow ? (
            <div className="pt-1">
              <Button type="button" size="sm" onClick={onRunNow}>
                Run now
              </Button>
            </div>
          ) : null}
        </div>
      </div>
    </div>
  );
}

/**
 * GH #463 — the waiting state on the run detail page. Says plainly that the
 * fleet has not been touched yet, which is the fact an operator opening a
 * scheduled run is checking for.
 */
export function ScheduledRunNotice({
  scheduledAt,
  onCancel,
}: {
  scheduledAt: string;
  /** Omitted entirely while no cancel endpoint exists; see the route file. */
  onCancel?: () => void;
}) {
  return (
    <div
      role="status"
      className={cn(
        "rounded-xl border p-4 sm:p-5",
        "border-[var(--color-border)] bg-[var(--color-muted)]/40",
      )}
    >
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex gap-3">
          <CalendarClock
            aria-hidden="true"
            className="mt-0.5 size-5 shrink-0 text-[var(--color-muted-foreground)]"
          />
          <div className="space-y-1.5">
            <h2 className="text-sm font-semibold text-[var(--color-foreground)]">
              Waiting for its start time
            </h2>
            <ScheduleCountdown scheduledAt={scheduledAt} className="text-sm" />
            <p className="text-sm text-[var(--color-muted-foreground)]">
              No site has been contacted yet. You can still update these
              plugins immediately in the meantime.
            </p>
          </div>
        </div>
        {onCancel ? (
          <Button type="button" size="sm" variant="outline" onClick={onCancel}>
            Cancel
          </Button>
        ) : null}
      </div>
    </div>
  );
}
