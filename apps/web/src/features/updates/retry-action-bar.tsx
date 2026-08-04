import { useEffect, useState } from "react";
import { AnimatePresence, motion } from "motion/react";
import { RotateCcw, X } from "lucide-react";

import { Button } from "@/components/ui/button";
import { drawerUp } from "@/lib/motion-presets";
import type { UpdateTask } from "@wpmgr/api";

import {
  countByRetryClass,
  formatRetryBreakdown,
  retryActionLabel,
  updatesNoun,
  type RetryTask,
} from "./retry-contract";

// GH #336 sticky action bar for the run detail task table.
//
// Follows the media BulkActionBar grammar (sticky bottom, one clear button, a
// two-line count block, verb-first primary carrying the count) rather than the
// Sites toolbar's action mode, because that grammar already encodes the thing
// this surface needs: the number on the button is the number of things that
// will be requested, and the breakdown underneath says what they are.
//
// THE UNIT IS TASKS, LABELLED AS UPDATES. `count` is a task count in every
// string here, including the aria-label and the live announcement.

export function RetryActionBar({
  selectedTasks,
  target,
  dryRun,
  onClear,
  onRetry,
}: {
  /** The effective selection: tasks the server still marks retryable. */
  selectedTasks: RetryTask[];
  /** Shared target type of the selection, or null when it is mixed. */
  target: UpdateTask["target_type"] | null;
  /** True when the run being retried was a dry run. */
  dryRun: boolean;
  onClear: () => void;
  onRetry: () => void;
}) {
  const count = selectedTasks.length;
  const show = count > 0;
  const breakdown = formatRetryBreakdown(countByRetryClass(selectedTasks));
  const label = retryActionLabel({ count, target, dryRun });

  // Announce the SETTLED count only. Ticking through 300 rows with a header
  // select-all must not queue 300 announcements, so the live region is
  // debounced and every intermediate value is cancelled before it is read.
  const [announcement, setAnnouncement] = useState("");
  useEffect(() => {
    const id = window.setTimeout(() => {
      setAnnouncement(count === 0 ? "" : `${updatesNoun(count)} selected.`);
    }, 500);
    return () => window.clearTimeout(id);
  }, [count]);

  return (
    <>
      <span aria-live="polite" className="sr-only">
        {announcement}
      </span>
      <AnimatePresence>
        {show ? (
          <motion.div
            key="retry-action-bar"
            variants={drawerUp}
            initial="initial"
            animate="animate"
            exit="exit"
            role="toolbar"
            aria-label={`${updatesNoun(count)} selected`}
            className="sticky bottom-4 z-30 mx-auto flex w-fit max-w-full items-center gap-2 rounded-lg border border-[var(--color-border)] bg-[var(--color-card)] px-3 py-2 shadow-lg"
          >
            <div className="flex items-center gap-2 pr-1">
              <Button
                type="button"
                variant="ghost"
                size="icon"
                onClick={onClear}
                aria-label="Clear selection"
                className="size-7"
              >
                <X aria-hidden="true" className="size-4" />
              </Button>
              <div className="flex flex-col">
                <span className="text-sm font-medium tabular-nums text-[var(--color-foreground)]">
                  {count.toLocaleString()} selected
                </span>
                {breakdown ? (
                  <span className="text-[11px] tabular-nums text-[var(--color-muted-foreground)]">
                    {breakdown}
                  </span>
                ) : null}
              </div>
            </div>

            <div
              className="h-5 w-px bg-[var(--color-border)]"
              aria-hidden="true"
            />

            <Button type="button" size="sm" onClick={onRetry}>
              <RotateCcw aria-hidden="true" className="size-4" />
              {label}
            </Button>
          </motion.div>
        ) : null}
      </AnimatePresence>
    </>
  );
}
