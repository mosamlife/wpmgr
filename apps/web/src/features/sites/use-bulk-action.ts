import { createContext, useContext } from "react";
import type { BulkResult } from "@wpmgr/api";

// Sprint 3 / surface 4.10 — bulk action drawer state hook.
//
// The actual <BulkActionProvider> lives next to <BulkActionDrawer> in
// bulk-action-drawer.tsx so the provider can render the drawer host (one
// JSX file, no circular imports). This module just exposes the context
// type, the React context object, and the hook callers use.
//
// GH #230 "rich tags" generalized the single "update run" shape into a
// discriminated union so the same drawer/provider can also track a bulk
// tag-edit session. The "update-run" branch (SSE plumbing, bell-badge
// counting) is UNCHANGED — only "tag-edit" is new.
//
// Shape of the system:
//
//   ┌────────────────────────────────────────────────────────────┐
//   │ SitesToolbar fires "Update plugins on N sites"             │
//   │ → POSTs /api/v1/updates → receives { run_id }              │
//   │ → calls openWithRun(run_id, "Update plugins on N sites")  │
//   │                                                              │
//   │ SitesToolbar fires "Tag N sites"                            │
//   │ → calls openTagEdit(siteIds) (no network call yet — the     │
//   │   picker stages an add/remove diff; POST happens on Apply)  │
//   └─────────────────────────────┬──────────────────────────────┘
//                                 ▼
//   ┌────────────────────────────────────────────────────────────┐
//   │ BulkActionProvider remembers a stack of tracked refs.       │
//   │ `current` is the one the drawer is currently showing.       │
//   │ `visible` flips when the user dismisses mid-run; the ref    │
//   │ stays tracked until it settles (update-run: SSE completed;  │
//   │ tag-edit: status "done").                                   │
//   └─────────────────────────────┬──────────────────────────────┘
//                                 ▼
//   ┌──────────────────┐  reopenLatest  ┌──────────────────────┐
//   │ TopBar.Bell      │ ─────────────► │ BulkActionDrawer     │
//   │  badge = count   │                │  (slide-up animation)│
//   └──────────────────┘                └──────────────────────┘
//
// The bell badge (`inFlightCount`) counts ONLY unsettled "update-run" refs —
// a tag-edit session never contributes to it (it's a synchronous-feeling
// edit, not a long-running background job the operator needs paging back to).

export type TagEditStatus = "editing" | "applying" | "done";

export type BulkActionRunRef =
  | {
      kind: "update-run";
      /** Backend-issued run id. */
      runId: string;
      /** Display title shown in the drawer header ("Update plugins on 47 sites"). */
      title: string;
      /** True once the run-detail status flips to "completed". */
      settled: boolean;
    }
  | {
      kind: "tag-edit";
      /** Client-generated id for this tag-edit session. */
      id: string;
      /** Display title shown in the drawer header ("Tag 12 sites"). */
      title: string;
      siteIds: string[];
      status: TagEditStatus;
      results?: BulkResult[];
      /** True once status is "done" (mirrors update-run's `settled`, kept
       *  for symmetry even though tag-edit never counts toward the badge). */
      settled: boolean;
    };

export interface BulkActionContextValue {
  /** Currently-displayed ref, or null if the drawer is closed/hidden. */
  current: BulkActionRunRef | null;
  /** True when the drawer panel should be visible (slid up). */
  visible: boolean;
  /** Open the drawer to an existing tracked ref by its id (runId or
   *  tag-edit id). No-op if not found. */
  open: (id: string) => void;
  /** Begin tracking a freshly-created update run and show the drawer for it. */
  openWithRun: (runId: string, title: string) => void;
  /** Begin tracking a new tag-edit session and show the drawer for it.
   *  Returns the generated session id. */
  openTagEdit: (siteIds: string[]) => string;
  /** Patch a tracked tag-edit ref's status/results by id. */
  updateTagEdit: (
    id: string,
    patch: Partial<{ status: TagEditStatus; results: BulkResult[] }>,
  ) => void;
  /** Hide the drawer (slide down). The ref stays tracked. */
  close: () => void;
  /** Re-show the drawer for the most recent un-settled update run. No-op
   *  when none exists (tag-edit sessions are never auto-reopened this way). */
  reopenLatest: () => void;
  /** Mark an update-run as settled (called by the drawer when SSE says completed). */
  markSettled: (runId: string) => void;
  /** Number of update-runs currently in flight (not yet settled). Tag-edit
   *  sessions never count toward this. */
  inFlightCount: number;
}

const NOOP_CONTEXT: BulkActionContextValue = {
  current: null,
  visible: false,
  open: () => {},
  openWithRun: () => {},
  openTagEdit: () => "",
  updateTagEdit: () => {},
  close: () => {},
  reopenLatest: () => {},
  markSettled: () => {},
  inFlightCount: 0,
};

/** Internal context. Default to the no-op shape so the hook is safe outside
 *  the provider (the toolbar can call it before the drawer is mounted). */
export const BulkActionContext =
  createContext<BulkActionContextValue>(NOOP_CONTEXT);

/**
 * Bulk-action context hook. Components inside <BulkActionProvider> read the
 * real state here; components outside the provider get a no-op shape so
 * they degrade silently in isolated tests / server-rendered shells.
 */
export function useBulkAction(): BulkActionContextValue {
  return useContext(BulkActionContext);
}
