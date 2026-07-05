import { describe, it, expect, vi } from "vitest";

import {
  summarizeBulkDeleteResult,
  runBulkDeleteBackups,
  type BulkDeleteBackupsResponse,
  type BulkDeleteBackupsDeps,
  type BulkDeleteBackupsResultItem,
} from "./use-bulk-delete-backups";

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function item(
  id: string,
  outcome: "deleted" | "skipped",
  code?: BulkDeleteBackupsResultItem["code"],
  message?: string,
): BulkDeleteBackupsResultItem {
  return { id, outcome, code, message };
}

function response(
  results: BulkDeleteBackupsResultItem[],
  overrides: Partial<BulkDeleteBackupsResponse> = {},
): BulkDeleteBackupsResponse {
  const deleted = results.filter((r) => r.outcome === "deleted").length;
  const skipped = results.filter((r) => r.outcome === "skipped").length;
  return {
    dry_run: false,
    counts: { requested: results.length, deleted, skipped },
    results,
    reclaimed_bytes_estimate: 0,
    ...overrides,
  };
}

// ---------------------------------------------------------------------------
// summarizeBulkDeleteResult — every skip code + all-deleted / all-skipped / partial
// ---------------------------------------------------------------------------

describe("summarizeBulkDeleteResult", () => {
  it("reports a plain success title when nothing was skipped", () => {
    const result = response([item("a", "deleted"), item("b", "deleted")]);
    const summary = summarizeBulkDeleteResult(result);

    expect(summary.toastTitle).toBe("2 snapshots deleted");
    expect(summary.isAllSkipped).toBe(false);
    expect(summary.toastDescription).toBeUndefined();
    expect(summary.skipLines).toHaveLength(0);
  });

  it("uses singular copy for exactly one deletion", () => {
    const result = response([item("a", "deleted")]);
    const summary = summarizeBulkDeleteResult(result);
    expect(summary.toastTitle).toBe("1 snapshot deleted");
  });

  it("flags isAllSkipped and never celebrates when deleted === 0", () => {
    const result = response([
      item("a", "skipped", "snapshot_locked"),
      item("b", "skipped", "chain_has_dependents"),
    ]);
    const summary = summarizeBulkDeleteResult(result);

    expect(summary.isAllSkipped).toBe(true);
    expect(summary.toastTitle).toBe("0 deleted, 2 skipped");
  });

  it("reports partial success with a reason breakdown", () => {
    const result = response([
      item("a", "deleted"),
      item("b", "skipped", "snapshot_locked"),
    ]);
    const summary = summarizeBulkDeleteResult(result);

    expect(summary.isAllSkipped).toBe(false);
    expect(summary.toastTitle).toBe("1 deleted, 1 skipped");
    expect(summary.toastDescription).toBe("1 locked");
  });

  it.each([
    ["snapshot_not_found", "not found"],
    ["snapshot_in_progress", "still running"],
    ["snapshot_locked", "locked"],
    ["chain_has_dependents", "has dependents"],
    ["restore_in_progress", "restore in progress"],
  ] as const)(
    "covers the %s skip code with a human label",
    (code, label) => {
      const result = response([item("a", "skipped", code)]);
      const summary = summarizeBulkDeleteResult(result);
      expect(summary.toastDescription).toBe(`1 ${label}`);
      expect(summary.skipLines[0]).toBe(`a: ${label}`);
    },
  );

  it("prefers the server message over the code label in per-id skip lines", () => {
    const result = response([
      item("aabbccdd", "skipped", "snapshot_locked", "Locked by an operator"),
    ]);
    const summary = summarizeBulkDeleteResult(result);
    expect(summary.skipLines[0]).toBe("aabbccdd: Locked by an operator");
  });

  it("groups multiple skips of the same reason into one count", () => {
    const result = response([
      item("a", "skipped", "snapshot_in_progress"),
      item("b", "skipped", "snapshot_in_progress"),
      item("c", "skipped", "snapshot_locked"),
    ]);
    const summary = summarizeBulkDeleteResult(result);
    // Order follows first-seen reason.
    expect(summary.toastDescription).toBe("2 still running, 1 locked");
  });

  it("falls back to a generic label when code is absent", () => {
    const result = response([item("a", "skipped")]);
    const summary = summarizeBulkDeleteResult(result);
    expect(summary.skipLines[0]).toBe("a: skipped");
  });
});

// ---------------------------------------------------------------------------
// runBulkDeleteBackups — side-effect pipeline (cache invalidation, selection clear)
// ---------------------------------------------------------------------------

function makeDeps(
  serverResponse: BulkDeleteBackupsResponse,
): {
  deps: BulkDeleteBackupsDeps;
  bulkDeleteFn: ReturnType<typeof vi.fn>;
  invalidateList: ReturnType<typeof vi.fn>;
  removeDetailQuery: ReturnType<typeof vi.fn>;
  clearSelection: ReturnType<typeof vi.fn>;
  showSummary: ReturnType<typeof vi.fn>;
} {
  const bulkDeleteFn = vi.fn().mockResolvedValue(serverResponse);
  const invalidateList = vi.fn();
  const removeDetailQuery = vi.fn();
  const clearSelection = vi.fn();
  const showSummary = vi.fn();

  const deps: BulkDeleteBackupsDeps = {
    bulkDeleteFn,
    invalidateList,
    removeDetailQuery,
    clearSelection,
    showSummary,
  };

  return { deps, bulkDeleteFn, invalidateList, removeDetailQuery, clearSelection, showSummary };
}

describe("runBulkDeleteBackups", () => {
  it("calls the API once with the full id list", async () => {
    const result = response([item("a", "deleted"), item("b", "deleted")]);
    const { deps, bulkDeleteFn } = makeDeps(result);

    await runBulkDeleteBackups(["a", "b"], deps);

    expect(bulkDeleteFn).toHaveBeenCalledTimes(1);
    expect(bulkDeleteFn).toHaveBeenCalledWith(["a", "b"]);
  });

  it("invalidates the list exactly once regardless of batch size", async () => {
    const result = response([
      item("a", "deleted"),
      item("b", "deleted"),
      item("c", "skipped", "snapshot_locked"),
    ]);
    const { deps, invalidateList } = makeDeps(result);

    await runBulkDeleteBackups(["a", "b", "c"], deps);

    expect(invalidateList).toHaveBeenCalledTimes(1);
  });

  it("removes the detail cache only for actually-deleted ids, never for skipped ids", async () => {
    const result = response([
      item("a", "deleted"),
      item("b", "skipped", "chain_has_dependents"),
      item("c", "deleted"),
    ]);
    const { deps, removeDetailQuery } = makeDeps(result);

    await runBulkDeleteBackups(["a", "b", "c"], deps);

    expect(removeDetailQuery).toHaveBeenCalledTimes(2);
    expect(removeDetailQuery).toHaveBeenCalledWith("a");
    expect(removeDetailQuery).toHaveBeenCalledWith("c");
    expect(removeDetailQuery).not.toHaveBeenCalledWith("b");
  });

  it("clears the selection on completion, including when everything was skipped", async () => {
    const result = response([item("a", "skipped", "snapshot_locked")]);
    const { deps, clearSelection } = makeDeps(result);

    await runBulkDeleteBackups(["a"], deps);

    expect(clearSelection).toHaveBeenCalledTimes(1);
  });

  it("shows a summary derived from the response and returns it to the caller", async () => {
    const result = response([item("a", "deleted"), item("b", "deleted")]);
    const { deps, showSummary } = makeDeps(result);

    const out = await runBulkDeleteBackups(["a", "b"], deps);

    expect(out).toBe(result);
    expect(showSummary).toHaveBeenCalledTimes(1);
    expect(showSummary).toHaveBeenCalledWith(
      expect.objectContaining({ toastTitle: "2 snapshots deleted" }),
    );
  });

  it("propagates a hard failure without clearing selection or invalidating", async () => {
    const bulkDeleteFn = vi.fn().mockRejectedValue(new Error("network down"));
    const invalidateList = vi.fn();
    const removeDetailQuery = vi.fn();
    const clearSelection = vi.fn();
    const showSummary = vi.fn();
    const deps: BulkDeleteBackupsDeps = {
      bulkDeleteFn,
      invalidateList,
      removeDetailQuery,
      clearSelection,
      showSummary,
    };

    await expect(runBulkDeleteBackups(["a"], deps)).rejects.toThrow(
      "network down",
    );
    expect(invalidateList).not.toHaveBeenCalled();
    expect(clearSelection).not.toHaveBeenCalled();
    expect(showSummary).not.toHaveBeenCalled();
  });
});
