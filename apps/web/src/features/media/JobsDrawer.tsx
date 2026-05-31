import { useMemo } from "react";
import { AnimatePresence, motion } from "motion/react";
import { Activity, ChevronUp, Loader2, X } from "lucide-react";

import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import { drawerUp } from "@/lib/motion-presets";
import { StatusDot } from "@/components/status/status-dot";

import {
  runningCount,
  selectSiteRows,
  useJobsStore,
  type LiveJobRow,
} from "./jobs-store";
import { isTerminalJobState, type JobState } from "./types";

// JobsDrawer — slide-up drawer (drawerUp preset) showing live per-asset job
// rows fed by SSE (the jobs-store). It is dismissible WITHOUT cancelling: the X
// only collapses the drawer; the jobs keep running and an "N jobs running"
// badge lets the operator re-open it. Cancelling is a separate explicit action
// (the BulkActionBar / toolbar's Cancel-all).
//
// Layering: fixed bottom strip, z-40 (under the topbar's z-50 dialogs). Borders
// over shadows except the drawer itself, which literally floats (shadow-lg per
// DESIGN "drawer = lg").

const KIND_LABEL: Record<LiveJobRow["kind"], string> = {
  optimize: "Optimizing",
  restore: "Restoring",
  delete_originals: "Deleting originals",
  sync: "Syncing",
};

function stateTone(state: JobState): {
  tone: "info" | "success" | "destructive" | "muted";
  pulse: boolean;
} {
  switch (state) {
    case "queued":
    case "in_progress":
      return { tone: "info", pulse: true };
    case "succeeded":
    case "partially_succeeded":
      return { tone: "success", pulse: false };
    case "failed":
      return { tone: "destructive", pulse: false };
    case "cancelled":
      return { tone: "muted", pulse: false };
  }
}

export interface JobsDrawerProps {
  siteId: string;
}

export function JobsDrawer({ siteId }: JobsDrawerProps) {
  const rowsRecord = useJobsStore((s) => selectSiteRows(s, siteId));
  const open = useJobsStore((s) => s.openBySite[siteId] ?? false);
  const setOpen = useJobsStore((s) => s.setOpen);

  const rows = useMemo(
    () =>
      Object.values(rowsRecord).sort((a, b) => b.updatedAt - a.updatedAt),
    [rowsRecord],
  );
  const running = runningCount(rowsRecord);

  // Nothing to show: no live rows at all.
  if (rows.length === 0) return null;

  return (
    <>
      {/* Collapsed re-open badge — bottom-right pill when the drawer is closed
          but jobs are present/running. */}
      <AnimatePresence>
        {!open ? (
          <motion.div
            key="jobs-badge"
            initial={{ opacity: 0, y: 8 }}
            animate={{ opacity: 1, y: 0 }}
            exit={{ opacity: 0, y: 8 }}
            className="fixed bottom-4 right-4 z-40"
          >
            <Button
              type="button"
              size="sm"
              onClick={() => setOpen(siteId, true)}
              aria-label={
                running > 0
                  ? `${running} media jobs running. Open jobs drawer`
                  : "Open media jobs drawer"
              }
              className="gap-2 shadow-lg"
            >
              {running > 0 ? (
                <Loader2 aria-hidden="true" className="size-4 animate-spin" />
              ) : (
                <Activity aria-hidden="true" className="size-4" />
              )}
              <span className="tabular-nums">
                {running > 0
                  ? `${running} ${running === 1 ? "job" : "jobs"} running`
                  : "Jobs"}
              </span>
              <ChevronUp aria-hidden="true" className="size-4" />
            </Button>
          </motion.div>
        ) : null}
      </AnimatePresence>

      {/* The drawer itself. */}
      <AnimatePresence>
        {open ? (
          <motion.aside
            key="jobs-drawer"
            variants={drawerUp}
            initial="initial"
            animate="animate"
            exit="exit"
            role="region"
            aria-label="Media jobs"
            className="fixed inset-x-0 bottom-0 z-40 mx-auto max-w-[1200px] rounded-t-xl border border-[var(--color-border)] bg-[var(--color-card)] shadow-lg"
          >
            <header className="flex h-11 items-center justify-between border-b border-[var(--color-border)] px-4">
              <div className="flex items-center gap-2 text-sm font-medium text-[var(--color-foreground)]">
                <Activity
                  aria-hidden="true"
                  className="size-4 text-[var(--color-primary)]"
                />
                Media jobs
                {running > 0 ? (
                  <span className="rounded-full bg-[var(--color-info)]/10 px-2 py-0.5 text-xs tabular-nums text-[var(--color-info)]">
                    {running} running
                  </span>
                ) : null}
              </div>
              <Button
                type="button"
                variant="ghost"
                size="icon"
                onClick={() => setOpen(siteId, false)}
                aria-label="Collapse jobs drawer (jobs keep running)"
                className="size-7"
              >
                <X aria-hidden="true" className="size-4" />
              </Button>
            </header>

            <ul className="max-h-[40vh] divide-y divide-[var(--color-border)] overflow-y-auto">
              {rows.map((row) => (
                <JobRow key={row.jobId} row={row} />
              ))}
            </ul>
          </motion.aside>
        ) : null}
      </AnimatePresence>
    </>
  );
}

function JobRow({ row }: { row: LiveJobRow }) {
  const { tone, pulse } = stateTone(row.state);
  const terminal = isTerminalJobState(row.state);
  const pct =
    typeof row.progress === "number"
      ? Math.max(0, Math.min(100, Math.round(row.progress)))
      : null;

  return (
    <li className="flex items-center gap-3 px-4 py-2.5">
      <StatusDot tone={tone} pulse={pulse} />
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2">
          <span className="truncate text-sm text-[var(--color-foreground)]">
            {KIND_LABEL[row.kind]}
            {typeof row.wpAttachmentID === "number" ? (
              <span className="ml-1 font-mono text-xs tabular-nums text-[var(--color-muted-foreground)]">
                #{row.wpAttachmentID}
              </span>
            ) : null}
          </span>
        </div>
        {row.reason ? (
          <p className="truncate text-xs text-[var(--color-destructive)]">
            {row.reason}
          </p>
        ) : null}
      </div>

      {/* Progress / state on the right. */}
      <div className="flex w-28 shrink-0 items-center justify-end gap-2">
        {!terminal && pct !== null ? (
          <>
            <div
              className="h-1.5 w-16 overflow-hidden rounded-full bg-[var(--color-muted)]"
              role="progressbar"
              aria-valuenow={pct}
              aria-valuemin={0}
              aria-valuemax={100}
              aria-label="Encode progress"
            >
              <div
                className="h-full rounded-full bg-[var(--color-info)] transition-[width] duration-300"
                style={{ width: `${pct}%` }}
              />
            </div>
            <span className="font-mono text-xs tabular-nums text-[var(--color-muted-foreground)]">
              {pct}%
            </span>
          </>
        ) : (
          <span
            className={cn(
              "text-xs font-medium tabular-nums",
              tone === "success" && "text-[var(--color-success)]",
              tone === "destructive" && "text-[var(--color-destructive)]",
              tone === "muted" && "text-[var(--color-muted-foreground)]",
              tone === "info" && "text-[var(--color-info)]",
            )}
          >
            {stateText(row.state)}
          </span>
        )}
      </div>
    </li>
  );
}

function stateText(state: JobState): string {
  switch (state) {
    case "queued":
      return "Queued";
    case "in_progress":
      return "Running";
    case "succeeded":
      return "Done";
    case "partially_succeeded":
      return "Partial";
    case "failed":
      return "Failed";
    case "cancelled":
      return "Cancelled";
  }
}
