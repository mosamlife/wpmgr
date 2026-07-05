import {
  useMutation,
  useQueryClient,
  type UseMutationResult,
} from "@tanstack/react-query";
import {
  bulkDeleteBackups,
  type BulkDeleteBackupsResponse,
  type BulkDeleteBackupsResultItem,
} from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";
import { toast } from "@/components/toast";
import { backupsKeys } from "./use-backups";

export type { BulkDeleteBackupsResponse, BulkDeleteBackupsResultItem };

// Issue #115 — bulk-delete snapshots, chain-aware, right-sized confirmation.
//
// POST /api/v1/sites/:siteId/backups/bulk-delete (operationId bulkDeleteBackups)
// is generated in the @wpmgr/api SDK (packages/openapi-client/src/index.ts
// re-exports the op fn + its request/response types).
//
// Partial success is the NORMAL case: a 200 can carry zero deletions if every
// id was skipped (already-locked, still in progress, or a chain dependent).
// The mutation never treats a skip-heavy response as an error — only a
// non-2xx is a hard failure.

/** The server's skip reason codes, derived from the generated result item so it can never drift from the schema. */
export type BulkDeleteSkipCode = NonNullable<
  BulkDeleteBackupsResultItem["code"]
>;

/** Human label for every skip code the server can return. */
export const SKIP_CODE_LABEL: Record<BulkDeleteSkipCode, string> = {
  snapshot_not_found: "not found",
  snapshot_in_progress: "still running",
  snapshot_locked: "locked",
  chain_has_dependents: "has dependents",
  restore_in_progress: "restore in progress",
};

function skipLabel(item: BulkDeleteBackupsResultItem): string {
  return item.code ? (SKIP_CODE_LABEL[item.code] ?? item.code) : "skipped";
}

/**
 * Type-to-confirm token for the STRONG confirm dialog (adapted from
 * useOrphanDelete.ts's computeConfirmToken). One token for the WHOLE batch,
 * never per-row ids: "DELETE N SNAPSHOTS".
 */
export function computeBulkDeleteToken(count: number): string {
  return `DELETE ${count} SNAPSHOTS`;
}

export interface BulkDeleteSummary {
  toastTitle: string;
  toastDescription?: string;
  /** True when nothing was deleted; the toast must not "celebrate". */
  isAllSkipped: boolean;
  /** One human line per skipped id, e.g. "a1b2c3d4: locked". */
  skipLines: string[];
}

/**
 * Maps a bulk-delete response into toast copy + a per-id reason list.
 * Pure and exported so every skip code (and the all-skipped edge case) is
 * covered by use-bulk-delete-backups.test.ts without mounting a component.
 */
export function summarizeBulkDeleteResult(
  result: BulkDeleteBackupsResponse,
): BulkDeleteSummary {
  const { deleted, skipped } = result.counts;
  const skippedResults = result.results.filter((r) => r.outcome === "skipped");

  const reasonCounts = new Map<string, number>();
  for (const r of skippedResults) {
    const label = skipLabel(r);
    reasonCounts.set(label, (reasonCounts.get(label) ?? 0) + 1);
  }
  const reasonParts = Array.from(reasonCounts.entries()).map(
    ([label, n]) => `${n} ${label}`,
  );
  const skipLines = skippedResults.map(
    (r) => `${r.id.slice(0, 8)}: ${r.message ?? skipLabel(r)}`,
  );

  if (skipped === 0) {
    return {
      toastTitle: `${deleted} snapshot${deleted === 1 ? "" : "s"} deleted`,
      isAllSkipped: false,
      skipLines,
    };
  }

  if (deleted === 0) {
    return {
      toastTitle: `0 deleted, ${skipped} skipped`,
      toastDescription: reasonParts.join(", "),
      isAllSkipped: true,
      skipLines,
    };
  }

  return {
    toastTitle: `${deleted} deleted, ${skipped} skipped`,
    toastDescription: reasonParts.join(", "),
    isAllSkipped: false,
    skipLines,
  };
}

/**
 * Injectable dependency surface so the whole request → summary → cache
 * side-effect pipeline is testable without a QueryClientProvider or a real
 * network call (mirrors the runBulkBackup pattern in use-bulk-backup.ts).
 */
export interface BulkDeleteBackupsDeps {
  bulkDeleteFn: (ids: string[]) => Promise<BulkDeleteBackupsResponse>;
  invalidateList: () => void;
  removeDetailQuery: (id: string) => void;
  clearSelection: () => void;
  showSummary: (summary: BulkDeleteSummary) => void;
}

/**
 * Runs one bulk-delete call end to end: request, toast summary, invalidate
 * the list once, drop the per-snapshot detail cache for every actually
 * DELETED id (skipped ids keep their detail cache — the row still exists),
 * and clear the caller's selection state.
 */
export async function runBulkDeleteBackups(
  ids: string[],
  deps: BulkDeleteBackupsDeps,
): Promise<BulkDeleteBackupsResponse> {
  const result = await deps.bulkDeleteFn(ids);
  const summary = summarizeBulkDeleteResult(result);

  deps.showSummary(summary);
  deps.invalidateList();
  for (const r of result.results) {
    if (r.outcome === "deleted") deps.removeDetailQuery(r.id);
  }
  deps.clearSelection();

  return result;
}

async function bulkDeleteBackupsRequest(
  siteId: string,
  ids: string[],
): Promise<BulkDeleteBackupsResponse> {
  const { data, error } = await bulkDeleteBackups({
    path: { siteId },
    body: { ids },
  });
  if (error) throw toError(error);
  if (!data) throw new Error("Empty response from bulk-delete");
  return data;
}

function showBulkDeleteToast(summary: BulkDeleteSummary): void {
  if (summary.isAllSkipped) {
    // Nothing deleted — never a success toast. Keep it visible long enough to read the reasons.
    toast.error(summary.toastTitle, { description: summary.toastDescription });
    return;
  }
  toast.success(summary.toastTitle, { description: summary.toastDescription });
}

/**
 * Bulk-delete a batch of snapshots for a site (operator+). `onSelectionClear`
 * is the caller's `BackupsSelection.clear` — selection lives in the component
 * (features/backups/use-backups-selection.ts), not in this hook, so it is
 * injected rather than owned here.
 */
export function useBulkDeleteBackups(
  siteId: string,
  onSelectionClear: () => void,
): UseMutationResult<BulkDeleteBackupsResponse, Error, string[]> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (ids: string[]) =>
      runBulkDeleteBackups(ids, {
        bulkDeleteFn: (batchIds) => bulkDeleteBackupsRequest(siteId, batchIds),
        invalidateList: () => {
          void queryClient.invalidateQueries({
            queryKey: backupsKeys.listFor(siteId),
          });
        },
        removeDetailQuery: (id) => {
          queryClient.removeQueries({ queryKey: backupsKeys.detail(id) });
        },
        clearSelection: onSelectionClear,
        showSummary: showBulkDeleteToast,
      }),
  });
}
