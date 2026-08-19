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
import { isTerminalRunStatus } from "@/features/updates/summarize";
import type { UpdateTask, BulkResult, Site, SiteTag } from "@wpmgr/api";

import { TagPicker, type TagPickerState } from "@/features/sites/tag-picker";
import { useTags, useCreateTag, useBulkApplyTags } from "@/features/tags/use-tags";
import { toast } from "@/components/toast";

import {
  BulkActionContext,
  type BulkActionContextValue,
  type BulkActionRunRef,
  type TagEditStatus,
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
// We keep AnimatePresence around so exit transforms run before unmount, // the previous hand-rolled "mounted" state machine is no longer needed.

// ---------------------------------------------------------------------------
// Provider
// ---------------------------------------------------------------------------

/** The stable id for a tracked ref, regardless of its kind. */
function refId(ref: BulkActionRunRef): string {
  return ref.kind === "update-run" ? ref.runId : ref.id;
}

let tagEditCounter = 0;
/** Client-generated id for a new tag-edit session (no server id exists yet). */
function nextTagEditId(): string {
  tagEditCounter += 1;
  return `tag-edit-${Date.now()}-${tagEditCounter}`;
}

/**
 * BulkActionProvider — mount once inside the AppShell, above the route
 * Outlet. Owns the stack of tracked refs (update runs AND tag-edit sessions)
 * and renders one drawer at a time. Children read state via `useBulkAction()`.
 */
export function BulkActionProvider({ children }: { children: ReactNode }) {
  const [runs, setRuns] = useState<BulkActionRunRef[]>([]);
  const [currentId, setCurrentId] = useState<string | null>(null);
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
      const existing = prev.find((r) => r.kind === "update-run" && r.runId === runId);
      if (existing) {
        // Same run id triggered twice: refresh the title in place.
        return prev.map((r) =>
          r.kind === "update-run" && r.runId === runId ? { ...r, title } : r,
        );
      }
      return [...prev, { kind: "update-run", runId, title, settled: false }];
    });
    setCurrentId(runId);
    setVisible(true);
  }, []);

  const openTagEdit = useCallback((siteIds: string[]) => {
    const id = nextTagEditId();
    const title = `Tag ${siteIds.length} ${siteIds.length === 1 ? "site" : "sites"}`;
    setRuns((prev) => [
      ...prev,
      { kind: "tag-edit", id, title, siteIds, status: "editing", settled: false },
    ]);
    setCurrentId(id);
    setVisible(true);
    return id;
  }, []);

  const updateTagEdit = useCallback(
    (id: string, patch: Partial<{ status: TagEditStatus; results: BulkResult[] }>) => {
      setRuns((prev) =>
        prev.map((r) =>
          r.kind === "tag-edit" && r.id === id
            ? { ...r, ...patch, settled: patch.status === "done" ? true : r.settled }
            : r,
        ),
      );
    },
    [],
  );

  const open = useCallback((id: string) => {
    setCurrentId(id);
    setVisible(true);
  }, []);

  const close = useCallback(() => {
    setVisible(false);
  }, []);

  const reopenLatest = useCallback(() => {
    const all = runsRef.current;
    const inFlight = [...all]
      .reverse()
      .find((r) => r.kind === "update-run" && !r.settled);
    const latest = inFlight ?? all[all.length - 1];
    if (!latest) return;
    setCurrentId(refId(latest));
    setVisible(true);
  }, []);

  const markSettled = useCallback((runId: string) => {
    setRuns((prev) => {
      const target = prev.find((r) => r.kind === "update-run" && r.runId === runId);
      if (!target || target.settled) return prev;
      return prev.map((r) =>
        r.kind === "update-run" && r.runId === runId ? { ...r, settled: true } : r,
      );
    });
  }, []);

  // Bell-badge count: ONLY unsettled update-runs. A tag-edit session in
  // progress never inflates the badge.
  const inFlightCount = useMemo(
    () =>
      runs.reduce(
        (acc, r) => (r.kind === "update-run" && !r.settled ? acc + 1 : acc),
        0,
      ),
    [runs],
  );

  const current = useMemo(
    () => runs.find((r) => refId(r) === currentId) ?? null,
    [runs, currentId],
  );

  const value = useMemo<BulkActionContextValue>(
    () => ({
      current,
      visible,
      open,
      openWithRun,
      openTagEdit,
      updateTagEdit,
      close,
      reopenLatest,
      markSettled,
      inFlightCount,
    }),
    [
      current,
      visible,
      open,
      openWithRun,
      openTagEdit,
      updateTagEdit,
      close,
      reopenLatest,
      markSettled,
      inFlightCount,
    ],
  );

  return (
    <BulkActionContext.Provider value={value}>
      {children}
      <BulkActionDrawerHost
        current={current}
        visible={visible}
        onClose={close}
        onSettled={markSettled}
        onTagEditUpdate={updateTagEdit}
      />
    </BulkActionContext.Provider>
  );
}

// ---------------------------------------------------------------------------
// Host — switch-renders the update-run drawer or the tag-edit drawer
// ---------------------------------------------------------------------------

function BulkActionDrawerHost({
  current,
  visible,
  onClose,
  onSettled,
  onTagEditUpdate,
}: {
  current: BulkActionRunRef | null;
  visible: boolean;
  onClose: () => void;
  onSettled: (runId: string) => void;
  onTagEditUpdate: (
    id: string,
    patch: Partial<{ status: TagEditStatus; results: BulkResult[] }>,
  ) => void;
}) {
  if (!current) return null;
  if (current.kind === "update-run") {
    // The update-run branch is BYTE-IDENTICAL to before generalization — see
    // <BulkActionDrawer> below. SSE plumbing and per-row rendering untouched.
    return (
      <BulkActionDrawer
        runId={current.runId}
        title={current.title}
        visible={visible}
        onClose={onClose}
        onSettled={onSettled}
      />
    );
  }
  return (
    <TagEditDrawer
      entry={current}
      visible={visible}
      onClose={onClose}
      onUpdate={onTagEditUpdate}
    />
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
    // `halted` (GH #255 Phase 2) is terminal exactly like `completed`: an
    // agent self-update run that a wave gate stopped early still needs to
    // settle, or its bell badge count would never clear.
    if (!isTerminalRunStatus(run.status)) return;
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
    // GH #463: waiting on its own start time, not yet actionable — same
    // tier as an ordinary queued task.
    case "scheduled":
      return 1;
    case "failed":
      return 2;
    case "rolled_back":
      return 3;
    case "succeeded":
      return 4;
    case "skipped":
      return 5;
    // GH #255 Phase 2: never dispatched because its run halted first.
    case "cancelled":
      return 6;
    // GH #463: never dispatched because its run expired first. Same tier as
    // `cancelled` — nothing was sent, distinct cause.
    case "expired":
      return 6;
    default:
      return 7;
  }
}

/** Worst-case status for a multi-task row, prioritizing visible failure. */
function rollupStatus(tasks: UpdateTask[]): UpdateTask["status"] {
  if (tasks.some((t) => t.status === "running")) return "running";
  if (tasks.some((t) => t.status === "pending")) return "pending";
  // GH #463: any task still waiting on its schedule means this row is not
  // done either — must be checked before the terminal-state rollups below,
  // or a row of entirely `scheduled` tasks falls through to "succeeded".
  if (tasks.some((t) => t.status === "scheduled")) return "scheduled";
  if (tasks.some((t) => t.status === "failed")) return "failed";
  if (tasks.some((t) => t.status === "rolled_back")) return "rolled_back";
  if (tasks.every((t) => t.status === "skipped")) return "skipped";
  // A halted run cancels every task it had not already dispatched, so this
  // must never fall through to the "succeeded" default below, which would
  // show an untouched site as done.
  if (tasks.every((t) => t.status === "cancelled")) return "cancelled";
  // GH #463: an expired run never dispatched any of its tasks either — same
  // reasoning as `cancelled` immediately above, different cause. Must also
  // never fall through to "succeeded".
  if (tasks.every((t) => t.status === "expired")) return "expired";
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
    case "cancelled":
      return "muted";
    // GH #463: neither reads as an error — see features/updates/update-
    // status.tsx for the full reasoning on why `scheduled`/`expired` stay
    // out of "destructive".
    case "scheduled":
      return "muted";
    case "expired":
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
    case "cancelled":
      return "Cancelled";
    // GH #463: distinct label from "Cancelled" — one is an operator's
    // choice, the other is a missed schedule window.
    case "scheduled":
      return "Scheduled";
    case "expired":
      return "Expired";
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
  // GH #255 Phase 2: an armed agent task has no detail text until beat 3
  // resolves it; the generic target/version synthesis below would read
  // "Updating {slug}", which is misleading for a task that is only waiting
  // on the site's own cron.
  if (task.target_type === "agent" && task.status === "running") {
    return "Waiting for the upgraded agent to report back";
  }
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

// ---------------------------------------------------------------------------
// TagEditDrawer / TagEditPanel — GH #230 "rich tags" bulk tag editor
// ---------------------------------------------------------------------------
//
// Shares the same shell chrome (scrim, slide-up panel, Esc-to-close) as
// <BulkActionDrawer> above, but the body is a single tri-state <TagPicker>
// mounted INLINE (not in a Popover) plus a live add/remove diff footer.
// Nothing here is optimistic: every toggle only mutates local pending state;
// the operator's "Apply to N sites" click is the one and only network call
// (POST /tags/bulk-apply).

export interface TagEditEntry {
  kind: "tag-edit";
  id: string;
  title: string;
  siteIds: string[];
  status: TagEditStatus;
  results?: BulkResult[];
  settled: boolean;
}

export interface TagEditDrawerProps {
  entry: TagEditEntry;
  visible: boolean;
  onClose: () => void;
  onUpdate: (
    id: string,
    patch: Partial<{ status: TagEditStatus; results: BulkResult[] }>,
  ) => void;
}

export function TagEditDrawer({ entry, visible, onClose, onUpdate }: TagEditDrawerProps) {
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

  return (
    <AnimatePresence>
      {visible ? (
        <div aria-hidden={!visible} className="fixed inset-0 z-50">
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
            aria-labelledby="tag-edit-drawer-title"
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
            <TagEditPanel key={entry.id} entry={entry} onClose={onClose} onUpdate={onUpdate} />
          </motion.section>
        </div>
      ) : null}
    </AnimatePresence>
  );
}

/** mixed -> checked -> unchecked -> checked -> … (mixed is only ever the
 *  INITIAL state; once cycled away, an operator can only get back to it by
 *  reopening the drawer, which reseeds `current` from a fresh initial map).
 *  Exported for direct unit coverage (see bulk-action-drawer.test.tsx). */
// eslint-disable-next-line react-refresh/only-export-components -- pure helper intentionally co-located with TagEditPanel/Drawer; exported only for direct unit-test coverage of the tri-state cycle logic.
export function cycleTagState(state: TagPickerState): TagPickerState {
  if (state === "mixed") return "checked";
  if (state === "checked") return "unchecked";
  return "checked";
}

/**
 * Derive each registry tag's initial tri-state from the selected sites'
 * cached tag sets: checked = every site has it, unchecked = no site has it,
 * mixed = some do. Exported for direct unit coverage.
 *
 * `totalSelectedCount` defaults to `sites.length` (the pre-existing, "we
 * already have everyone" behavior). Pass the ACTUAL number of selected
 * sites explicitly when `sites` might be a partial resolve (adversarial-
 * verify MEDIUM: selection can include sites from the archived view, or
 * otherwise beyond whatever list caches happened to be loaded) — when fewer
 * sites resolved than were selected, every tag's initial state is "mixed"
 * (uncertain), NEVER "unchecked" or "checked": asserting either would be
 * lying about the sites we couldn't see. The panel's loading-gate (below)
 * exists specifically to make this branch rare, not to replace it.
 */
// eslint-disable-next-line react-refresh/only-export-components -- see cycleTagState above.
export function deriveInitialTagState(
  tags: readonly SiteTag[],
  sites: readonly { tags: string[] }[],
  totalSelectedCount: number = sites.length,
): Map<string, TagPickerState> {
  const map = new Map<string, TagPickerState>();
  if (totalSelectedCount === 0) {
    for (const tag of tags) map.set(tag.id, "unchecked");
    return map;
  }
  const hasUnknownSites = sites.length < totalSelectedCount;
  for (const tag of tags) {
    if (hasUnknownSites) {
      map.set(tag.id, "mixed");
      continue;
    }
    const count = sites.filter((s) => s.tags.includes(tag.name)).length;
    if (count === 0) map.set(tag.id, "unchecked");
    else if (count === sites.length) map.set(tag.id, "checked");
    else map.set(tag.id, "mixed");
  }
  return map;
}

/**
 * Pure add/remove diff between the frozen `initial` tri-state map and the
 * operator's `current` (pending) one. A tag only lands in `add`/`remove`
 * when its state actually CHANGED from initial — an untouched "mixed" tag
 * (never clicked) contributes nothing, matching the "mixed re-enterable
 * only by reopening" rule (no click ever happened, so there is no diff to
 * apply for it). Exported for direct unit coverage.
 */
// eslint-disable-next-line react-refresh/only-export-components -- see cycleTagState above.
export function computeTagDiff(
  tags: readonly SiteTag[],
  initial: ReadonlyMap<string, TagPickerState>,
  current: ReadonlyMap<string, TagPickerState>,
): { add: string[]; remove: string[] } {
  const add: string[] = [];
  const remove: string[] = [];
  for (const tag of tags) {
    const initialState = initial.get(tag.id) ?? "unchecked";
    const state = current.get(tag.id) ?? initialState;
    if (state === initialState) continue;
    if (state === "checked") add.push(tag.name);
    else if (state === "unchecked") remove.push(tag.name);
  }
  return { add, remove };
}

function TagEditPanel({
  entry,
  onClose,
  onUpdate,
}: {
  entry: TagEditEntry;
  onClose: () => void;
  onUpdate: (
    id: string,
    patch: Partial<{ status: TagEditStatus; results: BulkResult[] }>,
  ) => void;
}) {
  // GH #230 adversarial-verify MEDIUM: `useSites()` alone is scoped to the
  // ACTIVE view — a selection can include archived sites (or, in principle,
  // any site outside whatever list caches happen to be loaded). Fetch BOTH
  // buckets so the tri-state baseline can actually resolve every selected
  // site's tags, not just the ones that happen to be in the active list.
  const { data: activeSites, isPending: activePending } = useSites();
  const { data: archivedSites, isPending: archivedPending } = useSites({ view: "archived" });
  const { data: allTags } = useTags();
  const createTag = useCreateTag();
  const bulkApply = useBulkApplyTags();

  const allSites = useMemo(() => {
    // Dedupe by id: a site could in principle be present in both query
    // results (e.g. a stale cache mid-transition), and double-counting it
    // would corrupt the tri-state denominator below.
    const byId = new Map<string, Site>();
    for (const site of activeSites ?? []) byId.set(site.id, site);
    for (const site of archivedSites ?? []) byId.set(site.id, site);
    return [...byId.values()];
  }, [activeSites, archivedSites]);

  const sites = useMemo(
    () => allSites.filter((s) => entry.siteIds.includes(s.id)),
    [allSites, entry.siteIds],
  );

  const totalSelected = entry.siteIds.length;
  const allResolved = sites.length === totalSelected;
  // Block seeding while either bucket is still loading AND we don't yet have
  // every selected site resolved — once both queries settle, `sites` is
  // final (barring a genuinely deleted site, which `deriveInitialTagState`
  // still handles honestly via its `hasUnknownSites` branch rather than by
  // blocking forever).
  const stillResolving = (activePending || archivedPending) && !allResolved;

  // Freeze the initial per-tag tri-state exactly once, the first render
  // where the registry AND the selected sites are resolved (or resolving is
  // done, even if incomplete) — this is the diff baseline. Follows the same
  // "adjust state during render" pattern used elsewhere in this codebase
  // (see site-tags-editor's server-key reset) rather than an effect, so
  // there is no extra render/flash before the picker shows the right
  // glyphs. Held in state (not a ref): this codebase's stricter React
  // Compiler lint config forbids reading/writing a ref's `.current` during
  // render.
  const [initial, setInitial] = useState<Map<string, TagPickerState> | null>(null);
  const [current, setCurrent] = useState<Map<string, TagPickerState>>(new Map());
  if (initial === null && allTags && !stillResolving) {
    const seed = deriveInitialTagState(allTags, sites, totalSelected);
    setInitial(seed);
    setCurrent(new Map(seed));
  }

  function getState(tag: SiteTag): TagPickerState {
    return current.get(tag.id) ?? "unchecked";
  }

  function handleToggle(tag: SiteTag) {
    setCurrent((prev) => {
      const next = new Map(prev);
      next.set(tag.id, cycleTagState(prev.get(tag.id) ?? "unchecked"));
      return next;
    });
  }

  async function handleCreate(name: string): Promise<SiteTag> {
    return createTag.mutateAsync({ name });
  }

  const diff = useMemo(
    () => computeTagDiff(allTags ?? [], initial ?? new Map(), current),
    [allTags, current, initial],
  );

  const siteNameMap = useMemo(() => {
    const map = new Map<string, string>();
    for (const site of allSites) map.set(site.id, site.url || site.name);
    return map;
  }, [allSites]);

  const applying = entry.status === "applying";
  const done = entry.status === "done";
  const canApply =
    !stillResolving &&
    initial !== null &&
    !applying &&
    !done &&
    (diff.add.length > 0 || diff.remove.length > 0);

  async function handleApply() {
    onUpdate(entry.id, { status: "applying" });
    try {
      const result = await bulkApply.mutateAsync({
        site_ids: entry.siteIds,
        add: diff.add.length > 0 ? diff.add : undefined,
        remove: diff.remove.length > 0 ? diff.remove : undefined,
      });
      const results = result.results ?? [];
      onUpdate(entry.id, { status: "done", results });
      const ok = results.filter((r) => r.ok).length;
      const total = entry.siteIds.length;
      if (ok === total) {
        toast.success(`Tags updated on ${ok} of ${total} sites`);
      } else {
        toast.error(`Tags updated on ${ok} of ${total} sites`, {
          description: "Some sites failed, see the drawer for details.",
        });
      }
    } catch (err) {
      onUpdate(entry.id, { status: "editing" });
      toast.error("Could not update tags", {
        description: err instanceof Error ? err.message : undefined,
      });
    }
  }

  return (
    <>
      <header className="flex items-start justify-between gap-4 px-6 pt-3 pb-2">
        <h2 id="tag-edit-drawer-title" className="text-base font-semibold text-foreground">
          Edit tags · {entry.siteIds.length} {entry.siteIds.length === 1 ? "site" : "sites"}
        </h2>
        <Button type="button" variant="ghost" size="icon" aria-label="Close drawer" onClick={onClose}>
          <X aria-hidden="true" />
        </Button>
      </header>

      <div className="max-h-[calc(70vh-11rem)] overflow-y-auto px-6 pb-2">
        {stillResolving || initial === null ? (
          // GH #230 adversarial-verify MEDIUM: block rather than seed from a
          // partial resolve — the tri-state must never assert "none/all of
          // these sites have this tag" while some selected sites' tags are
          // still unknown (e.g. an archived-view selection this component's
          // own active-view query wouldn't otherwise see).
          <p role="status" className="py-6 text-center text-sm text-muted-foreground">
            Loading tags for {totalSelected} {totalSelected === 1 ? "site" : "sites"}…
          </p>
        ) : done && entry.results ? (
          <ul className="divide-y divide-border">
            {entry.results.map((r) => (
              <li key={r.site_id} className="flex items-center gap-3 py-2.5">
                <StatusDot
                  tone={r.ok ? "success" : "destructive"}
                  label={r.ok ? "Applied" : "Failed"}
                />
                <span
                  className="min-w-0 flex-1 truncate font-mono text-sm text-foreground"
                  title={siteNameMap.get(r.site_id) ?? r.site_id}
                >
                  {siteNameMap.get(r.site_id) ?? shortId(r.site_id)}
                </span>
                <span className="min-w-0 flex-[2] truncate text-sm text-muted-foreground">
                  {r.detail}
                </span>
              </li>
            ))}
          </ul>
        ) : (
          <div className="rounded-md border border-border">
            <TagPicker getState={getState} onToggle={handleToggle} onCreate={handleCreate} />
          </div>
        )}
      </div>

      <footer className="flex flex-wrap items-center justify-between gap-3 border-t border-border bg-muted/40 px-6 py-3">
        <p className="text-xs text-muted-foreground">
          {done ? (
            "Done."
          ) : (
            <>
              Add <span className="font-mono tabular-nums">{diff.add.length}</span> tag(s) · Remove{" "}
              <span className="font-mono tabular-nums">{diff.remove.length}</span> tag(s) ·{" "}
              <span className="font-mono tabular-nums">{entry.siteIds.length}</span> sites
            </>
          )}
        </p>
        <div className="flex items-center gap-2">
          <Button type="button" variant="outline" size="sm" onClick={onClose}>
            {done ? "Close" : "Cancel"}
          </Button>
          {!done ? (
            <Button
              type="button"
              size="sm"
              disabled={!canApply}
              onClick={() => void handleApply()}
            >
              {applying ? "Applying…" : `Apply to ${entry.siteIds.length} sites`}
            </Button>
          ) : null}
        </div>
      </footer>
    </>
  );
}
