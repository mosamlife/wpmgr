import { describe, it, expect } from "vitest";
import { act, renderHook } from "@testing-library/react";
import type { UpdateTask } from "@wpmgr/api";

import { parseWireTask, serverRetryFields } from "@/test/update-task-fixtures";

import { useRetrySelection } from "./use-retry-selection";

// GH #336 selection invariants.
//
//   1. UNTOUCHED selection IS the server's default set, recomputed as the SSE
//      stream patches the cache, so a task that fails while the operator reads
//      the page joins the default rather than being silently left out.
//   2. TOUCHED selection is explicit and is never re-derived from what is on
//      screen. Ticking one row must not move the others.
//   3. The EFFECTIVE set is always re-filtered through the server's
//      `retryable`, so the number on the button is the number that will be
//      requested even after a later frame changes a task.

const RUN_ID = "9c3f1f9e-1f6e-4a5e-9a2f-0f0f0f0f0f0f";
const TENANT_ID = "22222222-2222-2222-2222-222222222222";

function task(overrides: Partial<UpdateTask>): UpdateTask {
  const status = overrides.status ?? "failed";
  return parseWireTask({
    id: "task-1",
    run_id: RUN_ID,
    tenant_id: TENANT_ID,
    site_id: "site-1",
    site_name: "one.example.com",
    target_type: "plugin",
    target_slug: "akismet/akismet.php",
    status: "failed",
    created_at: "2026-08-04T09:00:00Z",
    updated_at: "2026-08-04T09:10:00Z",
    ...serverRetryFields(status),
    ...overrides,
  });
}

const failed = task({ id: "t-failed", status: "failed" });
const cancelled = task({ id: "t-cancelled", status: "cancelled" });
const reverted = task({ id: "t-reverted", status: "rolled_back" });
const succeeded = task({ id: "t-succeeded", status: "succeeded" });
const running = task({ id: "t-running", status: "running" });

describe("useRetrySelection", () => {
  it("starts on the server's default set: failed and never_ran, nothing else", () => {
    const { result } = renderHook(() =>
      useRetrySelection([failed, cancelled, reverted, succeeded, running]),
    );
    expect(result.current.count).toBe(2);
    expect([...result.current.selected].sort()).toEqual([
      "t-cancelled",
      "t-failed",
    ]);
    // reverted is selectable, just never pre-selected; succeeded and running
    // are not selectable at all.
    expect(result.current.selectableTasks.map((t) => t.id)).toEqual([
      "t-failed",
      "t-cancelled",
      "t-reverted",
    ]);
  });

  it("keeps tracking the server default while untouched, so a task that settles mid-stream joins the selection", () => {
    const { result, rerender } = renderHook(
      ({ tasks }: { tasks: UpdateTask[] }) => useRetrySelection(tasks),
      { initialProps: { tasks: [failed, running] } },
    );
    expect(result.current.count).toBe(1);

    // The SSE stream settles the running task and the reclassifying read
    // brings back the server's verdict for it.
    const settled = task({ id: "t-running", status: "failed" });
    rerender({ tasks: [failed, settled] });
    expect(result.current.count).toBe(2);
  });

  it("freezes into an explicit set once the operator touches it, and never re-derives from later rows", () => {
    const { result, rerender } = renderHook(
      ({ tasks }: { tasks: UpdateTask[] }) => useRetrySelection(tasks),
      { initialProps: { tasks: [failed, running] } },
    );

    act(() => {
      result.current.toggle("t-reverted");
    });
    // A row not in the list yet is still an explicit choice: the selection is
    // the operator's, not a projection of what is rendered.
    expect(result.current.selected.has("t-reverted")).toBe(true);
    expect(result.current.selected.has("t-failed")).toBe(true);

    const settled = task({ id: "t-running", status: "failed" });
    rerender({ tasks: [failed, settled, reverted] });
    // The newly settled task did NOT join: the selection is explicit now.
    expect(result.current.selected.has("t-running")).toBe(false);
    expect(result.current.count).toBe(2); // failed + reverted
  });

  it("selects and clears every selectable task from one control, never the unselectable ones", () => {
    const { result } = renderHook(() =>
      useRetrySelection([failed, cancelled, reverted, succeeded, running]),
    );

    act(() => {
      result.current.setAllSelectable(true);
    });
    expect(result.current.count).toBe(3);
    expect(result.current.allSelectableSelected).toBe(true);
    expect(result.current.someSelectableSelected).toBe(false);
    expect(result.current.selected.has("t-succeeded")).toBe(false);

    act(() => {
      result.current.toggle("t-reverted");
    });
    expect(result.current.count).toBe(2);
    expect(result.current.allSelectableSelected).toBe(false);
    expect(result.current.someSelectableSelected).toBe(true);

    act(() => {
      result.current.clear();
    });
    expect(result.current.count).toBe(0);
  });

  it("drops a ticked task from the effective set the moment the server stops calling it retryable", () => {
    const { result, rerender } = renderHook(
      ({ tasks }: { tasks: UpdateTask[] }) => useRetrySelection(tasks),
      { initialProps: { tasks: [failed, cancelled] } },
    );
    expect(result.current.count).toBe(2);

    // The reclassifying read comes back with the cancelled task now
    // succeeded: it is still in `selected` (the operator ticked it) but it is
    // no longer requested, and the button's number says so.
    const nowSucceeded = task({ id: "t-cancelled", status: "succeeded" });
    rerender({ tasks: [failed, nowSucceeded] });
    expect(result.current.count).toBe(1);
    expect(result.current.selectedTasks.map((t) => t.id)).toEqual(["t-failed"]);
  });
});
