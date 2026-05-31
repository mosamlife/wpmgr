import { useCallback } from "react";
import { useQueryClient, type QueryClient } from "@tanstack/react-query";

import {
  useSiteEvents,
  type SiteEvent,
} from "@/features/sites/use-site-events";

import {
  useJobsStore,
  type LiveJobRow,
} from "../jobs-store";
import type {
  AssetStatus,
  JobState,
  ListAssetsResponse,
  MediaAsset,
} from "../types";
import { mediaKeys } from "./useMediaAssets";

// useMediaEvents — projects the shared `/sites/events` SSE stream onto the
// media caches + the JobsDrawer store. The media.* types were added to
// SITE_EVENT_TYPES in use-site-events.ts (without that, these frames would be
// Zod-dropped before they ever reach here — ADR-043 §7, the recon's "drops
// unknown types" flag).
//
// Reducer contract (the testable part lives in `mediaEventReducer`):
//   • media.optimize.asset_done / media.restore.asset_done
//       → row-level setQueryData patch on every cached assets page + recompute
//         the summary. NO refetch (the cheap, live path).
//   • media.*.completed (optimize/restore/delete_originals/sync)
//       → invalidate assets + summary (authoritative re-read).
//   • media.optimize.started / media.optimize.progress / *.started
//       → update the JobsDrawer live rows (Zustand), open the drawer.
//   • media.job.failed
//       → mark the asset failed (patch) + mark the live row failed.
//
// Every frame is filtered by site_id first; events for other sites are ignored.

// ── Reading the free-form SSE `data` (typed `unknown` on the wire) ─────────
//
// Each media.* event carries a small map (apps/api/internal/media/service —
// e.g. asset_done: {job_id, wp_attachment_id, applied}; progress: {job_id,
// phase, variant}; started: {batch_job_id, queued_count, target_format};
// completed: {job_id, state}; job.failed: {job_id, reason}). We read fields
// defensively via these narrowers so a missing/renamed field degrades to
// undefined rather than throwing.

function asRecord(data: unknown): Record<string, unknown> {
  return typeof data === "object" && data !== null
    ? (data as Record<string, unknown>)
    : {};
}
function str(v: unknown): string | undefined {
  return typeof v === "string" ? v : undefined;
}
function num(v: unknown): number | undefined {
  return typeof v === "number" ? v : undefined;
}

// ── Pure asset-cache patching (unit-tested) ────────────────────────────────

/**
 * Apply a per-asset status change to a cached assets page and recompute its
 * summary rollup. Pure: returns a NEW page (or the same reference if nothing
 * matched). `wpAttachmentID` keys the row because the SSE payloads carry the WP
 * attachment id, not the CP asset uuid.
 */
export function patchAssetStatus(
  page: ListAssetsResponse,
  wpAttachmentID: number,
  status: AssetStatus,
): ListAssetsResponse {
  let changed = false;
  const items: MediaAsset[] = page.items.map((a) => {
    if (a.wp_attachment_id === wpAttachmentID && a.status !== status) {
      changed = true;
      return { ...a, status };
    }
    return a;
  });
  if (!changed) return page;
  return { ...page, items, summary: recomputeSummary(page, items) };
}

/**
 * Recompute the summary's optimized/pending/failed counts from the patched
 * items, holding `total` and `bytes_saved` from the server (bytes_saved needs
 * the authoritative per-variant numbers, which only arrive on the *.completed
 * invalidation). This keeps the four overview tiles honest between refetches.
 */
function recomputeSummary(
  page: ListAssetsResponse,
  items: MediaAsset[],
): ListAssetsResponse["summary"] {
  let optimized = 0;
  let pending = 0;
  let failed = 0;
  for (const a of items) {
    if (a.status === "optimized" || a.status === "originals_deleted")
      optimized += 1;
    else if (a.status === "pending" || a.status === "optimizing") pending += 1;
    else if (a.status === "failed") failed += 1;
  }
  return { ...page.summary, optimized, pending, failed };
}

/** Map an asset_done / restore_done / failed event to the resulting status. */
function statusForEvent(type: SiteEvent["type"]): AssetStatus | null {
  switch (type) {
    case "media.optimize.asset_done":
      return "optimized";
    case "media.restore.asset_done":
      return "restored";
    case "media.delete_originals.completed":
      return "originals_deleted";
    case "media.job.failed":
      return "failed";
    default:
      return null;
  }
}

function kindFor(
  type: SiteEvent["type"],
): LiveJobRow["kind"] | null {
  if (type.startsWith("media.optimize")) return "optimize";
  if (type.startsWith("media.restore")) return "restore";
  if (type.startsWith("media.delete_originals")) return "delete_originals";
  if (type.startsWith("media.sync")) return "sync";
  // media.job.failed carries no kind hint; the store merges with the prior
  // frame's kind, but we need a non-null value to upsert at all. "optimize" is
  // the common path; a prior started/progress frame for this job_id will have
  // already set the accurate kind, which the store preserves.
  if (type === "media.job.failed") return "optimize";
  return null;
}

function jobStateFor(type: SiteEvent["type"], rawState?: string): JobState {
  if (type.endsWith(".failed")) return "failed";
  if (type.endsWith(".completed") || type.endsWith(".asset_done")) {
    // Prefer the server's authoritative state when present (optimize.completed
    // carries it: succeeded | partially_succeeded | failed).
    if (
      rawState === "succeeded" ||
      rawState === "partially_succeeded" ||
      rawState === "failed" ||
      rawState === "cancelled"
    ) {
      return rawState;
    }
    return "succeeded";
  }
  return "in_progress";
}

// ── The hook ───────────────────────────────────────────────────────────────

/**
 * Subscribe the media caches + the JobsDrawer store to the shared SSE stream
 * for one site. Call once from MediaTab.
 */
export function useMediaEvents(siteId: string): void {
  const qc = useQueryClient();
  const upsertRow = useJobsStore((s) => s.upsertRow);
  const setOpen = useJobsStore((s) => s.setOpen);

  const handler = useCallback(
    (ev: SiteEvent) => {
      if (ev.site_id !== siteId) return;
      if (!ev.type.startsWith("media.")) return;
      mediaEventReducer(ev, {
        siteId,
        queryClient: qc,
        upsertRow: (row) => upsertRow(siteId, row),
        openDrawer: () => setOpen(siteId, true),
      });
    },
    [siteId, qc, upsertRow, setOpen],
  );

  useSiteEvents(handler);
}

// ── Pure reducer (drives the hook; unit-tested) ────────────────────────────

export interface MediaEventDeps {
  siteId: string;
  queryClient: Pick<QueryClient, "setQueriesData" | "invalidateQueries">;
  upsertRow: (row: LiveJobRow) => void;
  openDrawer: () => void;
}

/**
 * Project one media SSE event onto the caches + drawer store. Exported so the
 * routing logic can be unit-tested without a React tree (mirrors
 * use-updates.test.ts testing `applyEvent` directly).
 */
export function mediaEventReducer(
  ev: SiteEvent,
  deps: MediaEventDeps,
): void {
  const { siteId, queryClient, upsertRow, openDrawer } = deps;
  const data = asRecord(ev.data);
  const assetsKey = [...mediaKeys.all, "assets", siteId];

  // 1. Row-level live patch — asset_done / job.failed flip a single row's
  //    status across every cached assets page (no refetch).
  const newStatus = statusForEvent(ev.type);
  if (newStatus) {
    const wpId = num(data.wp_attachment_id);
    if (typeof wpId === "number") {
      queryClient.setQueriesData<ListAssetsResponse>(
        { queryKey: assetsKey },
        (page) =>
          page ? patchAssetStatus(page, wpId, newStatus) : page,
      );
    }
  }

  // 2. Completion → invalidate so the authoritative summary (incl. bytes_saved)
  //    and any list re-projects. asset_done is the cheap path above; the
  //    *.completed terminal frame is when we re-read.
  if (
    ev.type === "media.optimize.completed" ||
    ev.type === "media.restore.completed" ||
    ev.type === "media.delete_originals.completed" ||
    ev.type === "media.sync.completed"
  ) {
    void queryClient.invalidateQueries({ queryKey: assetsKey });
    void queryClient.invalidateQueries({
      queryKey: [...mediaKeys.all, "jobs", siteId],
    });
  }

  // 3. JobsDrawer live rows — started / progress / completion all upsert a row
  //    so the drawer reflects the live per-asset state. started + progress also
  //    open the drawer.
  const kind = kindFor(ev.type);
  const jobId = str(data.job_id) ?? str(data.batch_job_id);
  if (kind && jobId) {
    const isStartOrProgress =
      ev.type.endsWith(".started") || ev.type.endsWith(".progress");
    upsertRow({
      jobId,
      wpAttachmentID: num(data.wp_attachment_id),
      kind,
      progress: progressFromEvent(ev.type, str(data.phase)) ?? undefined,
      state: jobStateFor(ev.type, str(data.state)),
      reason: str(data.reason),
      updatedAt: Date.now(),
    });
    if (isStartOrProgress) openDrawer();
  }
}

/**
 * Derive a 0–100 progress for the drawer from a progress/completion event.
 * optimize.progress carries `phase` ("encoding" | "encoded"); we don't get a
 * true percentage on the wire, so we map the coarse phase to a representative
 * value and snap completions to 100.
 */
function progressFromEvent(
  type: SiteEvent["type"],
  phase: string | undefined,
): number | null {
  if (type.endsWith(".completed") || type.endsWith(".asset_done")) return 100;
  if (type === "media.optimize.progress") {
    return phase === "encoded" ? 75 : 25;
  }
  if (type.endsWith(".started")) return 0;
  return null;
}
