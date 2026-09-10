import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from "@tanstack/react-query";
import {
  listDbSnapshots,
  createDbSnapshot,
  revertDbSnapshot,
  deleteDbSnapshot,
  type DbSnapshotEntry,
  type DbSnapshotList,
  type DbSnapshotCreateResult,
  type DbSnapshotRevertResult,
} from "@wpmgr/api";
import { toError } from "@/features/auth/use-auth";

// useDbSnapshots — local database snapshot hooks (#189).
//
// Snapshots are a fast LOCAL safety-net stored on the WP server filesystem.
// They are distinct from durable backups (off-site, unencrypted client side).
// Use them to capture the DB state before a risky change (plugin update,
// search-replace, bulk edit) and revert in one click if something goes wrong.
//
// Data flow:
//   useDbSnapshotList  — GET  /api/v1/sites/{siteId}/perf/db/snapshots
//   useCreateSnapshot  — POST /api/v1/sites/{siteId}/perf/db/snapshots
//   useRevertSnapshot  — POST /api/v1/sites/{siteId}/perf/db/snapshots/{id}/revert
//   useDeleteSnapshot  — DELETE /api/v1/sites/{siteId}/perf/db/snapshots/{id}
//
// All mutations invalidate the snapshot list query so the UI stays consistent.

export type { DbSnapshotEntry };

// snapshotListKey returns the TanStack Query key for the snapshot list.
export const snapshotListKey = (siteId: string) =>
  ["sites", siteId, "db-snapshots"] as const;

// useDbSnapshotList fetches the current snapshot manifest for a site.
export function useDbSnapshotList(
  siteId: string,
): UseQueryResult<DbSnapshotList, Error> {
  return useQuery({
    queryKey: snapshotListKey(siteId),
    queryFn: async (): Promise<DbSnapshotList> => {
      const { data, error } = await listDbSnapshots({ path: { siteId } });
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response from snapshot list");
      return data;
    },
    // Snapshots are local filesystem state — no need to poll frequently.
    staleTime: 30_000,
  });
}

// CreateSnapshotInput is the payload for the create mutation.
export interface CreateSnapshotInput {
  label?: string;
  retention?: number;
}

// useCreateSnapshot triggers a new local database snapshot.
export function useCreateSnapshot(
  siteId: string,
): UseMutationResult<DbSnapshotCreateResult, Error, CreateSnapshotInput> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (
      input: CreateSnapshotInput,
    ): Promise<DbSnapshotCreateResult> => {
      const { data, error } = await createDbSnapshot({
        path: { siteId },
        body: {
          label: input.label,
          retention: input.retention,
        },
      });
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response from snapshot create");
      if (!data.ok) {
        throw new Error(data.detail ?? "Snapshot creation failed");
      }
      return data;
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: snapshotListKey(siteId) });
    },
  });
}

// RevertSnapshotInput is the payload for the revert mutation.
export interface RevertSnapshotInput {
  snapshotId: string;
  // confirm MUST equal "REVERT" — checked agent-side via hash_equals.
  confirm: "REVERT";
  skipSafetySnapshot?: boolean;
}

// useRevertSnapshot replaces the live database with a captured snapshot.
// This is DESTRUCTIVE — the confirm token and a UI-level dialog are both
// required before this mutation is called.
export function useRevertSnapshot(
  siteId: string,
): UseMutationResult<DbSnapshotRevertResult, Error, RevertSnapshotInput> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (
      input: RevertSnapshotInput,
    ): Promise<DbSnapshotRevertResult> => {
      const { data, error } = await revertDbSnapshot({
        path: { siteId, snapshotId: input.snapshotId },
        body: {
          confirm: input.confirm,
          skip_safety_snapshot: input.skipSafetySnapshot,
        },
      });
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response from snapshot revert");
      if (!data.ok) {
        throw new Error(data.detail ?? "Snapshot revert failed");
      }
      return data;
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: snapshotListKey(siteId) });
    },
  });
}

// useDeleteSnapshot removes a single snapshot from the agent's local store.
export function useDeleteSnapshot(
  siteId: string,
): UseMutationResult<void, Error, string> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (snapshotId: string): Promise<void> => {
      const { data, error } = await deleteDbSnapshot({
        path: { siteId, snapshotId },
      });
      if (error) throw toError(error);
      if (data && !data.ok) {
        throw new Error(
          (data as { ok: boolean; detail?: string }).detail ??
            "Snapshot deletion failed",
        );
      }
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: snapshotListKey(siteId) });
    },
  });
}
