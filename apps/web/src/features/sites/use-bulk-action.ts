import { createContext, useContext } from "react";

// Sprint 3 / surface 4.10 — bulk action drawer state hook.
//
// The actual <BulkActionProvider> lives next to <BulkActionDrawer> in
// bulk-action-drawer.tsx so the provider can render the drawer host (one
// JSX file, no circular imports). This module just exposes the context
// type, the React context object, and the hook callers use.
//
// Shape of the system:
//
//   ┌────────────────────────────────────────────────────────────┐
//   │ SitesToolbar fires "Update plugins on N sites"             │
//   │ → POSTs /api/v1/updates → receives { run_id }              │
//   │ → calls openWithRun(run_id, "Update plugins on N sites")  │
//   └─────────────────────────────┬──────────────────────────────┘
//                                 ▼
//   ┌────────────────────────────────────────────────────────────┐
//   │ BulkActionProvider remembers a stack of in-flight runs.    │
//   │ currentRunId is the one the drawer is currently showing.   │
//   │ `visible` flips when the user dismisses mid-run; the run   │
//   │ stays tracked until SSE reports completion.                │
//   └─────────────────────────────┬──────────────────────────────┘
//                                 ▼
//   ┌──────────────────┐  reopenLatest  ┌──────────────────────┐
//   │ TopBar.Bell      │ ─────────────► │ BulkActionDrawer     │
//   │  badge = count   │                │  (slide-up animation)│
//   └──────────────────┘                └──────────────────────┘
//
// The provider owns nothing about the SSE stream itself — that stays in
// useRunEventStream / useUpdateRun. We just track which runs the operator
// has started and whether each one has settled so the bell badge can
// shrink as runs finish.

export interface BulkActionRunRef {
  /** Backend-issued run id. */
  runId: string;
  /** Display title shown in the drawer header ("Update plugins on 47 sites"). */
  title: string;
  /** True once the run-detail status flips to "completed". */
  settled: boolean;
}

export interface BulkActionContextValue {
  /** Currently-displayed run, or null if the drawer is closed/hidden. */
  currentRunId: string | null;
  /** Display title for the currently-displayed run. */
  currentTitle: string;
  /** True when the drawer panel should be visible (slid up). */
  visible: boolean;
  /** Open the drawer to an existing run (no POST). */
  open: (runId: string) => void;
  /** Begin tracking a freshly-created run and show the drawer for it. */
  openWithRun: (runId: string, title: string) => void;
  /** Hide the drawer (slide down). Run stays tracked. */
  close: () => void;
  /** Re-show the drawer for the most recent un-settled run. No-op otherwise. */
  reopenLatest: () => void;
  /** Mark a run as settled (called by the drawer when SSE says completed). */
  markSettled: (runId: string) => void;
  /** Number of runs currently in flight (not yet settled). */
  inFlightCount: number;
}

const NOOP_CONTEXT: BulkActionContextValue = {
  currentRunId: null,
  currentTitle: "",
  visible: false,
  open: () => {},
  openWithRun: () => {},
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
