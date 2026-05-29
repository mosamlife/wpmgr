import { memo, useEffect, useMemo, useRef } from "react";
import { RotateCw, X } from "lucide-react";
import { AnimatePresence, motion } from "motion/react";

import { StatusDot, type StatusTone } from "@/components/status";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { drawerUp, fade } from "@/lib/motion-presets";
import { useSites } from "@/features/sites/use-sites";
import {
  useUpdateRun,
  useRunEventStream,
} from "@/features/updates/use-updates";
import type { UpdateTask } from "@wpmgr/api";

// Phase 6 perf — extracted from bulk-action-drawer.tsx so the drawer panel
// (motion variants, SSE wiring, row helpers) can be code-split via React.lazy.
// The BulkActionProvider stays eager because it owns the run-tracking
// context that the TopBar bell + every authed route reads on first render;
// only the visible drawer chrome is deferred.

export interface BulkActionDrawerProps {
  /** Backend run id to display, or null to render nothing. */
  runId: string | null;
  /** Header title, e.g. "Update plugins on 47 sites". */
  title: string;
  /** True when the panel should be visible (slid up). */
  visible: boolean;
  /** Caller closes (slide down). The run stays tracked elsewhere. */
  onClose: () => void;
  /** Fired exactly once when the run reaches a terminal status. */
  onSettled?: (runId: string) => void;
  /** Optional manual retry hook for a failed task. Not yet wired to backend. */
  onRetry?: (taskId: string) => void;
}

function BulkActionDrawer({
  runId,
  title,
  visible,
  onClose,
  onSettled,
  onRetry,
}: BulkActionDrawerProps) {
  useEffect(() => {
    if (!visible) return;
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") {
        e.preventDefault();
        onClose();
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [visible, onClose]);

  const enabled = Boolean(runId);
  const effectiveRunId = runId ?? "";
  const { data: run } = useUpdateRun(effectiveRunId, {
    poll: true,
    enabled,
  });
  useRunEventStream(effectiveRunId, { enabled });

  const { data: sites } = useSites();
  const siteNameMap = useMemo(() => {
    const map = new Map<string, string>();
    for (const site of sites ?? []) map.set(site.id, site.url || site.name);
    return map;
  }, [sites]);

  const grouped = useMemo(
    () => groupBySite(run?.tasks ?? []),
    [run?.tasks],
  );

  const totals = useMemo(() => countTotals(grouped), [grouped]);

  const settledRef = useRef<string | null>(null);
  useEffect(() => {
    if (!run || !runId) return;
    if (run.status !== "completed") return;
    if (settledRef.current === runId) return;
    settledRef.current = runId;
    onSettled?.(runId);
  }, [run, runId, onSettled]);

  if (!runId) return null;

  return (
    <AnimatePresence>
      {visible ? (
        <div
          aria-hidden={!visible}
          className="fixed inset-0 z-50"
        >
          <motion.button
            type="button"
            aria-label="Close drawer"
            tabIndex={-1}
            onClick={onClose}
            variants={fade}
            initial="initial"
            animate="animate"
            exit="exit"
            className="absolute inset-0 bg-[var(--scrim)]"
          />

          <motion.section
            role="dialog"
            aria-modal="false"
            aria-labelledby="bulk-drawer-title"
            variants={drawerUp}
            initial="initial"
            animate="animate"
            exit="exit"
            className={cn(
              "absolute bottom-0 left-0 right-0",
              "max-h-[70vh] overflow-hidden",
              "rounded-t-xl border-t border-border bg-card text-card-foreground shadow-lg",
            )}
          >
            <div className="mx-auto mt-2 h-1.5 w-12 rounded-full bg-muted" aria-hidden="true" />

            <header className="flex items-start justify-between gap-4 px-6 pt-3 pb-2">
              <div className="min-w-0">
                <h2
                  id="bulk-drawer-title"
                  // Phase 6 (harden): `title` exposes the full string when the
                  // headline truncates ("Update plugins on 1,247 sites" in
                  // German becomes 40+ chars).
                  title={title}
                  className="text-base font-semibold text-foreground truncate"
                >
                  {title}
                </h2>
                <p className="mt-1 text-xs text-muted-foreground tabular-nums">
                  <span className="font-mono">{totals.done}</span> /{" "}
                  <span className="font-mono">{totals.total}</span> done
                  {totals.failed > 0 ? (
                    <>
                      {" · "}
                      <span className="text-destructive">
                        <span className="font-mono">{totals.failed}</span>{" "}
                        failed
                      </span>
                    </>
                  ) : null}
                  {totals.inFlight > 0 ? (
                    <>
                      {" · "}
                      <span className="font-mono">{totals.inFlight}</span> in
                      progress
                    </>
                  ) : null}
                </p>
              </div>
              <Button
                type="button"
                variant="ghost"
                size="icon"
                aria-label="Close drawer"
                onClick={onClose}
              >
                <X aria-hidden="true" />
              </Button>
            </header>

            <div className="max-h-[calc(70vh-9rem)] overflow-y-auto px-6 pb-2">
              {grouped.length === 0 ? (
                <p className="py-6 text-sm text-muted-foreground">
                  Scheduling tasks. Progress will appear as the agents pick up
                  work.
                </p>
              ) : (
                <ul className="divide-y divide-border">
                  {grouped.map((row) => (
                    <BulkSiteRow
                      key={row.siteId}
                      row={row}
                      hostname={siteNameMap.get(row.siteId) ?? shortId(row.siteId)}
                      onRetry={onRetry}
                    />
                  ))}
                </ul>
              )}
            </div>

            <footer className="border-t border-border bg-muted/40 px-6 py-3">
              <p className="text-xs text-muted-foreground">
                You can close this drawer; we will keep updating and ping you
                when done.
              </p>
            </footer>
          </motion.section>
        </div>
      ) : null}
    </AnimatePresence>
  );
}

// Default export for React.lazy compatibility. Named export retained for
// non-lazy callers (Storybook, tests).
export default BulkActionDrawer;
export { BulkActionDrawer };

// ---------------------------------------------------------------------------
// Row
// ---------------------------------------------------------------------------

interface SiteRow {
  siteId: string;
  tasks: UpdateTask[];
  rollup: UpdateTask["status"];
}

// Phase 6 perf: each SSE tick re-renders the drawer with a fresh `run` from
// react-query. Wrapping the row in React.memo skips per-row reconciliation
// when only one row's tasks changed.
const BulkSiteRow = memo(function BulkSiteRow({
  row,
  hostname,
  onRetry,
}: {
  row: SiteRow;
  hostname: string;
  onRetry?: (taskId: string) => void;
}) {
  const tone = toneFor(row.rollup);
  const isRunning = row.rollup === "running" || row.rollup === "pending";
  const lead = pickLead(row.tasks);
  const failed = row.tasks.find(
    (t) => t.status === "failed" || t.status === "rolled_back",
  );

  return (
    <li className="flex items-center gap-3 py-2.5">
      <StatusDot
        tone={tone}
        pulse={isRunning}
        label={`${hostname}: ${statusLabel(row.rollup)}`}
      />
      {/* Phase 6 (harden): `title` exposes the full hostname/detail when the
          cell truncates — 1000-row drawers stay scannable without overflow. */}
      <span
        className="min-w-0 flex-1 truncate font-mono text-sm text-foreground"
        title={hostname}
      >
        {hostname}
      </span>
      <span
        className="hidden min-w-0 flex-[2] truncate text-sm text-muted-foreground sm:inline"
        title={detailFor(lead)}
      >
        {detailFor(lead)}
      </span>
      <span className="text-xs uppercase tracking-wide text-muted-foreground">
        {statusLabel(row.rollup)}
      </span>
      {failed && onRetry ? (
        <Button
          type="button"
          variant="outline"
          size="sm"
          aria-label={`Retry update on ${hostname}`}
          onClick={() => onRetry(failed.id)}
        >
          <RotateCw aria-hidden="true" />
          Retry update
        </Button>
      ) : null}
    </li>
  );
});

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function groupBySite(tasks: UpdateTask[]): SiteRow[] {
  const byId = new Map<string, UpdateTask[]>();
  for (const task of tasks) {
    const list = byId.get(task.site_id) ?? [];
    list.push(task);
    byId.set(task.site_id, list);
  }
  const rows: SiteRow[] = [];
  for (const [siteId, list] of byId) {
    rows.push({ siteId, tasks: list, rollup: rollupStatus(list) });
  }
  rows.sort((a, b) => priority(a.rollup) - priority(b.rollup));
  return rows;
}

function priority(status: UpdateTask["status"]): number {
  switch (status) {
    case "running":
      return 0;
    case "pending":
      return 1;
    case "failed":
      return 2;
    case "rolled_back":
      return 3;
    case "succeeded":
      return 4;
    case "skipped":
      return 5;
    default:
      return 6;
  }
}

function rollupStatus(tasks: UpdateTask[]): UpdateTask["status"] {
  if (tasks.some((t) => t.status === "running")) return "running";
  if (tasks.some((t) => t.status === "pending")) return "pending";
  if (tasks.some((t) => t.status === "failed")) return "failed";
  if (tasks.some((t) => t.status === "rolled_back")) return "rolled_back";
  if (tasks.every((t) => t.status === "skipped")) return "skipped";
  return "succeeded";
}

function toneFor(status: UpdateTask["status"]): StatusTone {
  switch (status) {
    case "running":
    case "pending":
      return "info";
    case "succeeded":
      return "success";
    case "failed":
    case "rolled_back":
      return "destructive";
    case "skipped":
      return "muted";
    default:
      return "muted";
  }
}

function statusLabel(status: UpdateTask["status"]): string {
  switch (status) {
    case "running":
      return "Updating";
    case "pending":
      return "Queued";
    case "succeeded":
      return "Done";
    case "failed":
      return "Failed";
    case "rolled_back":
      return "Rolled back";
    case "skipped":
      return "Skipped";
    default:
      return "Unknown";
  }
}

function pickLead(tasks: UpdateTask[]): UpdateTask | undefined {
  if (tasks.length === 0) return undefined;
  return (
    tasks.find((t) => t.status === "failed") ??
    tasks.find((t) => t.status === "rolled_back") ??
    tasks.find((t) => t.status === "running") ??
    tasks.find((t) => t.status === "pending") ??
    tasks[tasks.length - 1]
  );
}

function detailFor(task: UpdateTask | undefined): string {
  if (!task) return "";
  if (task.error) return task.error;
  if (task.detail) return task.detail;
  const target =
    task.target_type === "core"
      ? "WordPress core"
      : `${task.target_slug}`;
  if (task.from_version && task.to_version) {
    return `Updating ${target} ${task.from_version} → ${task.to_version}`;
  }
  if (task.to_version) {
    return `Updating ${target} → ${task.to_version}`;
  }
  return `Updating ${target}`;
}

interface Totals {
  total: number;
  done: number;
  failed: number;
  inFlight: number;
}

function countTotals(rows: SiteRow[]): Totals {
  let done = 0;
  let failed = 0;
  let inFlight = 0;
  for (const row of rows) {
    if (row.rollup === "succeeded" || row.rollup === "skipped") {
      done += 1;
    } else if (row.rollup === "failed" || row.rollup === "rolled_back") {
      failed += 1;
    } else {
      inFlight += 1;
    }
  }
  return { total: rows.length, done, failed, inFlight };
}

function shortId(id: string): string {
  return id.length > 8 ? `${id.slice(0, 8)}…` : id;
}
