import {
  memo,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { X, RotateCw } from "lucide-react";
import { AnimatePresence, motion } from "motion/react";

import { StatusDot, type StatusTone } from "@/components/status";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { drawerUp, fade } from "@/lib/motion-presets";
import { useSites } from "@/features/sites/use-sites";
import { useUpdateRun, useRunEventStream } from "@/features/updates/use-updates";
import type { UpdateTask } from "@wpmgr/api";

import {
  BulkActionContext,
  type BulkActionContextValue,
  type BulkActionRunRef,
} from "./use-bulk-action";

// Sprint 3 / surface 4.10 — bulk action drawer.
//
// Slides up from the bottom over any route. Reuses the existing run-event
// stream (useRunEventStream) and the run-detail query (useUpdateRun) so
// per-row progress arrives in real time without opening a second SSE
// channel. The drawer remembers in-flight runs across dismissal — the
// TopBar bell badge counts un-settled runs and reopens the most recent
// one on click.
//
// Animation rules (DESIGN.md "Motion" / Phase-4 Sprint-3 brief):
//   - Slide-up: translateY(100%) → translateY(0).
//   - Slide-down: faster exit (~75% of enter).
//   - ONLY transform + opacity animate. No width/height/top/left/padding/
//     margin transitions.
//   - prefers-reduced-motion collapses the slide via the global CSS rule.
//
// Phase 5: the panel + scrim are driven by the shared `drawerUp` and `fade`
// presets via motion/react so timing matches the dialog/save-bar/toolbar.
// We keep AnimatePresence around so exit transforms run before unmount —
// the previous hand-rolled "mounted" state machine is no longer needed.

// ---------------------------------------------------------------------------
// Provider
// ---------------------------------------------------------------------------

/**
 * BulkActionProvider — mount once inside the AppShell, above the route
 * Outlet. Owns the stack of in-flight runs and renders one drawer at a
 * time. Children read state via `useBulkAction()`.
 */
export function BulkActionProvider({ children }: { children: ReactNode }) {
  const [runs, setRuns] = useState<BulkActionRunRef[]>([]);
  const [currentRunId, setCurrentRunId] = useState<string | null>(null);
  const [visible, setVisible] = useState<boolean>(false);

  // Snapshot ref so the imperative callbacks below don't re-create on
  // every runs change — keeps `reopenLatest` referentially stable for
  // memoized callers (e.g. the TopBar Bell button).
  const runsRef = useRef<BulkActionRunRef[]>(runs);
  useEffect(() => {
    runsRef.current = runs;
  }, [runs]);

  const openWithRun = useCallback((runId: string, title: string) => {
    setRuns((prev) => {
      const existing = prev.find((r) => r.runId === runId);
      if (existing) {
        // Same run id triggered twice: refresh the title in place.
        return prev.map((r) =>
          r.runId === runId ? { ...r, title } : r,
        );
      }
      return [...prev, { runId, title, settled: false }];
    });
    setCurrentRunId(runId);
    setVisible(true);
  }, []);

  const open = useCallback((runId: string) => {
    setCurrentRunId(runId);
    setVisible(true);
  }, []);

  const close = useCallback(() => {
    setVisible(false);
  }, []);

  const reopenLatest = useCallback(() => {
    const all = runsRef.current;
    const inFlight = [...all].reverse().find((r) => !r.settled);
    const latest = inFlight ?? all[all.length - 1];
    if (!latest) return;
    setCurrentRunId(latest.runId);
    setVisible(true);
  }, []);

  const markSettled = useCallback((runId: string) => {
    setRuns((prev) => {
      const target = prev.find((r) => r.runId === runId);
      if (!target || target.settled) return prev;
      return prev.map((r) =>
        r.runId === runId ? { ...r, settled: true } : r,
      );
    });
  }, []);

  const inFlightCount = useMemo(
    () => runs.reduce((acc, r) => (r.settled ? acc : acc + 1), 0),
    [runs],
  );

  const current = useMemo(
    () => runs.find((r) => r.runId === currentRunId) ?? null,
    [runs, currentRunId],
  );
  const currentTitle = current?.title ?? "";

  const value = useMemo<BulkActionContextValue>(
    () => ({
      currentRunId,
      currentTitle,
      visible,
      open,
      openWithRun,
      close,
      reopenLatest,
      markSettled,
      inFlightCount,
    }),
    [
      currentRunId,
      currentTitle,
      visible,
      open,
      openWithRun,
      close,
      reopenLatest,
      markSettled,
      inFlightCount,
    ],
  );

  return (
    <BulkActionContext.Provider value={value}>
      {children}
      <BulkActionDrawer
        runId={currentRunId}
        title={currentTitle}
        visible={visible}
        onClose={close}
        onSettled={markSettled}
      />
    </BulkActionContext.Provider>
  );
}

// ---------------------------------------------------------------------------
// Drawer
// ---------------------------------------------------------------------------

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

/**
 * BulkActionDrawer — fixed bottom panel showing per-site progress for one
 * bulk update run. Rendered only when `runId !== null`. Slides up via
 * transform; backdrop fades via opacity. The drawer subscribes to the
 * existing SSE stream + polls the run-detail query as a safety net.
 *
 * Used inside <BulkActionProvider>, but accepts explicit props so it can
 * also be mounted standalone (e.g. Storybook, or a route that wants a
 * dedicated drawer without the global provider).
 */
export function BulkActionDrawer({
  runId,
  title,
  visible,
  onClose,
  onSettled,
  onRetry,
}: BulkActionDrawerProps) {
  // Esc closes the drawer (dismiss mid-run). Only attach the listener
  // while we're visible so we don't intercept Esc on routes that have
  // their own keyboard contracts.
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

  // Subscribe to the run AND the SSE stream. Both are enabled only when
  // we have a real runId so unmounted drawers don't open dead sockets.
  const enabled = Boolean(runId);
  const effectiveRunId = runId ?? "";
  const { data: run } = useUpdateRun(effectiveRunId, {
    poll: true,
    enabled,
  });
  useRunEventStream(effectiveRunId, { enabled });

  // Friendly hostname lookup. The sites list is already cached for any
  // route that has rendered the sites index; if absent, we fall back to
  // a shortened id.
  const { data: sites } = useSites();
  const siteNameMap = useMemo(() => {
    const map = new Map<string, string>();
    for (const site of sites ?? []) map.set(site.id, site.url || site.name);
    return map;
  }, [sites]);

  // Group tasks by target site so each row aggregates progress across
  // (plugin, theme, core) tasks running against the same hostname.
  const grouped = useMemo(
    () => groupBySite(run?.tasks ?? []),
    [run?.tasks],
  );

  // Aggregate counts for the footer. `done` includes succeeded + skipped;
  // `failed` is failed + rolled_back. In-flight is everything else.
  const totals = useMemo(() => countTotals(grouped), [grouped]);

  // Notify caller exactly once when the run reaches a terminal state.
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
          // The drawer host: a fixed full-viewport overlay so the scrim covers
          // everything below it. Clicking the scrim slides the panel down.
          className="fixed inset-0 z-50"
        >
          {/* Scrim. Fade via opacity, never width/height. */}
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

          {/* Panel. Fixed to the bottom of the viewport; the only animated
              properties are transform + opacity. max-h-[70vh] caps the panel
              height; overflow-y-auto handles long site lists. */}
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
            {/* Drag-handle visual. Purely cosmetic — no drag behavior. */}
            <div className="mx-auto mt-2 h-1.5 w-12 rounded-full bg-muted" aria-hidden="true" />

            <header className="flex items-start justify-between gap-4 px-6 pt-3 pb-2">
              <div className="min-w-0">
                <h2
                  id="bulk-drawer-title"
                  title={title}
                  className="truncate text-base font-semibold text-foreground"
                >
                  {title}
                </h2>
                <p className="mt-1 text-xs tabular-nums text-muted-foreground">
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

// ---------------------------------------------------------------------------
// Row
// ---------------------------------------------------------------------------

interface SiteRow {
  siteId: string;
  tasks: UpdateTask[];
  /** Worst-case status across the row's tasks. */
  rollup: UpdateTask["status"];
}

// Each SSE tick re-renders the drawer with a fresh `run` from react-query.
// React.memo skips per-row reconciliation when only one row's tasks changed.
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
  // Pick the most informative single task for the status text. Prefer a
  // failed/rolled_back task so the operator can see the error first; then
  // the currently-running task; then the most recently updated one.
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
  // Stable ordering: running first, then failed, then done. Keeps the
  // operator's eye where the action is.
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

/** Worst-case status for a multi-task row, prioritizing visible failure. */
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
  // Synthesize a reasonable progress string from target + versions when
  // the agent has not yet pushed a detail field.
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
