import { create } from "zustand";

import type { JobState } from "./types";

// JobsDrawer store — UI-only client state (Zustand). This holds the LIVE,
// per-asset progress rows the JobsDrawer renders while a batch runs, plus the
// drawer open/dismissed flags. It is intentionally NOT server state: the
// authoritative job records live in TanStack Query (useMediaJobs); this store
// only mirrors the ephemeral SSE progress stream so the drawer can update
// row-by-row without a refetch. (Rule: never mix server state into Zustand.)
//
// Keyed by site_id so switching sites shows that site's running jobs. A row is
// keyed by its ULID job_id (optimize/restore fan out one job per attachment).

/** One live job row shown in the drawer (sourced from SSE progress frames). */
export interface LiveJobRow {
  jobId: string;
  /** WP attachment id (for a human-readable "Attachment #123" label). */
  wpAttachmentID?: number;
  kind: "optimize" | "restore" | "delete_originals" | "sync";
  /** 0–100; undefined until the first progress frame. */
  progress?: number;
  state: JobState;
  /** Failure reason, when state === "failed". */
  reason?: string;
  /** Wall-clock of the last update (for sort stability / staleness). */
  updatedAt: number;
}

interface JobsState {
  /** Per-site live rows, keyed siteId → (jobId → row). */
  rowsBySite: Record<string, Record<string, LiveJobRow>>;
  /** Whether the drawer is currently expanded, per site. */
  openBySite: Record<string, boolean>;
  /** Set the drawer open/closed for a site (close = dismiss-without-cancel). */
  setOpen: (siteId: string, open: boolean) => void;
  /** Upsert a live row (called by the SSE handler on progress/start frames). */
  upsertRow: (siteId: string, row: LiveJobRow) => void;
  /** Drop a terminal/cleared row from the live set. */
  removeRow: (siteId: string, jobId: string) => void;
  /** Clear every live row for a site (e.g. after Cancel-all). */
  clearSite: (siteId: string) => void;
}

export const useJobsStore = create<JobsState>((set) => ({
  rowsBySite: {},
  openBySite: {},
  setOpen: (siteId, open) =>
    set((s) => ({ openBySite: { ...s.openBySite, [siteId]: open } })),
  upsertRow: (siteId, row) =>
    set((s) => {
      const existing = s.rowsBySite[siteId] ?? {};
      const prev = existing[row.jobId];
      // Preserve a known wpAttachmentID/kind if a later frame omits it.
      const merged: LiveJobRow = {
        ...prev,
        ...row,
        wpAttachmentID: row.wpAttachmentID ?? prev?.wpAttachmentID,
      };
      return {
        rowsBySite: {
          ...s.rowsBySite,
          [siteId]: { ...existing, [row.jobId]: merged },
        },
      };
    }),
  removeRow: (siteId, jobId) =>
    set((s) => {
      const existing = s.rowsBySite[siteId];
      if (!existing || !(jobId in existing)) return s;
      const next = { ...existing };
      delete next[jobId];
      return { rowsBySite: { ...s.rowsBySite, [siteId]: next } };
    }),
  clearSite: (siteId) =>
    set((s) => ({ rowsBySite: { ...s.rowsBySite, [siteId]: {} } })),
}));

/** Stable empty object so selectors don't churn when a site has no rows. */
const EMPTY_ROWS: Record<string, LiveJobRow> = {};

/** Select the live rows for a site as a stable record. */
export function selectSiteRows(
  state: JobsState,
  siteId: string,
): Record<string, LiveJobRow> {
  return state.rowsBySite[siteId] ?? EMPTY_ROWS;
}

/** Count the non-terminal ("running") rows for a site — the re-open badge. */
export function runningCount(rows: Record<string, LiveJobRow>): number {
  let n = 0;
  for (const r of Object.values(rows)) {
    if (
      r.state === "queued" ||
      r.state === "in_progress"
    ) {
      n += 1;
    }
  }
  return n;
}
