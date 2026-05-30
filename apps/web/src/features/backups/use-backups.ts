import { useSyncExternalStore } from "react";
import {
  useQuery,
  useMutation,
  useQueryClient,
  type UseQueryResult,
  type UseMutationResult,
} from "@tanstack/react-query";
import {
  createBackup,
  listBackups,
  getBackup,
  createRestore,
  getBackupSchedule,
  putBackupSchedule,
  type BackupCreate,
  type BackupSnapshot,
  type BackupSnapshotDetail,
  type RestoreCreate,
  type BackupSchedule,
  type BackupScheduleUpdate,
} from "@wpmgr/api";

import { toError } from "@/features/auth/use-auth";
import { isStreamLive, subscribeLiveStreams } from "./use-backup-stream";

// Server-state hooks for the M4 Backups domain (snapshots, restore, schedule).
// Built on the generated @wpmgr/api SDK; each call returns
// `{ data, error, response }` which we unwrap so TanStack Query owns
// loading/error/success. The control plane runs backups and restores
// asynchronously, so the snapshot-detail hook polls (refetchInterval) while a
// job is pending/running and stops once it reaches a terminal state.

export const backupsKeys = {
  all: ["backups"] as const,
  listFor: (siteId: string) => [...backupsKeys.all, "list", siteId] as const,
  detail: (snapshotId: string) =>
    [...backupsKeys.all, "detail", snapshotId] as const,
  scheduleFor: (siteId: string) =>
    [...backupsKeys.all, "schedule", siteId] as const,
};

/** Terminal backup/restore states — polling stops once a snapshot reaches one. */
const TERMINAL: ReadonlySet<BackupSnapshot["status"]> = new Set([
  "completed",
  "failed",
]);

export function isTerminal(status: BackupSnapshot["status"]): boolean {
  return TERMINAL.has(status);
}

/** A 404 surfaced as a typed error so callers can render a not-found state. */
export class NotFoundError extends Error {
  constructor(message = "Not found") {
    super(message);
    this.name = "NotFoundError";
  }
}

/** List a site's backup snapshots (newest first as returned by the API). */
export function useBackups(
  siteId: string,
): UseQueryResult<BackupSnapshot[], Error> {
  return useQuery({
    queryKey: backupsKeys.listFor(siteId),
    queryFn: async () => {
      const { data, error } = await listBackups({ path: { siteId } });
      if (error) throw toError(error);
      return data?.items ?? [];
    },
    // Refresh the list periodically while any snapshot is still in flight so a
    // freshly triggered backup advances its badge without a manual reload.
    refetchInterval: (query) =>
      (query.state.data ?? []).some((s) => !isTerminal(s.status))
        ? 3000
        : false,
  });
}

/**
 * Fetch a single snapshot with its manifest summary. Polls every 2s while the
 * snapshot (or an in-progress restore) is pending/running, stopping at a
 * terminal state.
 */
export function useBackup(
  snapshotId: string,
): UseQueryResult<BackupSnapshotDetail, Error> {
  // Subscribe to the SSE-live registry so React schedules a re-evaluation of
  // `refetchInterval` whenever a stream opens or closes for this snapshot.
  // We don't actually need to read the resulting boolean — the snapshotId
  // check inside refetchInterval is the source of truth — but useSyncExternalStore
  // is what triggers TanStack Query to re-evaluate the interval.
  useSyncExternalStore(
    subscribeLiveStreams,
    () => isStreamLive(snapshotId),
    () => false,
  );

  return useQuery({
    queryKey: backupsKeys.detail(snapshotId),
    queryFn: async () => {
      const { data, error, response } = await getBackup({
        path: { snapshotId },
      });
      if (response?.status === 404) {
        throw new NotFoundError("Backup snapshot not found");
      }
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    // M5.6 / ADR-032: when a `useBackupStream(snapshotId)` SSE channel is
    // healthy the live cache patches drive UI freshness and polling is
    // suppressed. When the stream is absent or has fallen back, poll every
    // 1 s (tightened from 1.5 s per WPvivid UX research recommending ≤1 s
    // for a "live" feel). Stops once the snapshot is terminal.
    refetchInterval: (query) => {
      if (!query.state.data) return false;
      if (isTerminal(query.state.data.snapshot.status)) return false;
      if (isStreamLive(snapshotId)) return false;
      return 1000;
    },
  });
}

/**
 * Start a backup (operator+). On success, invalidates the site's snapshot list
 * so the new pending snapshot appears.
 */
export function useCreateBackup(
  siteId: string,
): UseMutationResult<BackupSnapshot, Error, BackupCreate> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: BackupCreate) => {
      const { data, error, response } = await createBackup({
        path: { siteId },
        body,
      });
      if (response?.status === 422) {
        throw toError(error ?? { message: "Validation failed" });
      }
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: backupsKeys.listFor(siteId),
      });
    },
  });
}

/** The 202 body from POST /backups/{snapshotId}/restore (includes restore_run_id alongside snapshot fields). */
export interface CreateRestoreResult {
  snapshot: BackupSnapshot;
  restore_run_id: string | null;
}

/**
 * Start a restore job from a snapshot (operator+). The API returns the snapshot
 * whose status reflects the queued restore; we seed the detail cache with it so
 * polling can track the restore to completion.
 *
 * The result includes `restore_run_id` (from the 202 body alongside the snapshot
 * fields) so the caller can navigate to /restores/{restore_run_id} immediately.
 */
export function useCreateRestore(
  snapshotId: string,
): UseMutationResult<CreateRestoreResult, Error, RestoreCreate> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: RestoreCreate) => {
      const { data, error, response } = await createRestore({
        path: { snapshotId },
        body,
      });
      if (response?.status === 422) {
        throw toError(error ?? { message: "Validation failed" });
      }
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      // The API 202 body is the snapshot fields merged with `restore_run_id`.
      // The generated type only knows BackupSnapshot; we read the extra field
      // from the raw body by casting (it's present on the wire but not typed).
      const raw = data as Record<string, unknown>;
      const restoreRunId =
        typeof raw.restore_run_id === "string" ? raw.restore_run_id : null;
      return { snapshot: data, restore_run_id: restoreRunId };
    },
    onSuccess: ({ snapshot }) => {
      queryClient.setQueryData<BackupSnapshotDetail>(
        backupsKeys.detail(snapshotId),
        (prev) => (prev ? { ...prev, snapshot } : prev),
      );
      void queryClient.invalidateQueries({
        queryKey: backupsKeys.detail(snapshotId),
      });
    },
  });
}

/**
 * Fetch a site's backup schedule. Returns `null` on 404 (no schedule
 * configured) so the editor can render its defaults rather than an error.
 */
export function useBackupSchedule(
  siteId: string,
): UseQueryResult<BackupSchedule | null, Error> {
  return useQuery({
    queryKey: backupsKeys.scheduleFor(siteId),
    queryFn: async () => {
      const { data, error, response } = await getBackupSchedule({
        path: { siteId },
      });
      if (response?.status === 404) return null;
      if (error) throw toError(error);
      return data ?? null;
    },
  });
}

/** Create or update a site's backup schedule (operator+). */
export function usePutBackupSchedule(
  siteId: string,
): UseMutationResult<BackupSchedule, Error, BackupScheduleUpdate> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: BackupScheduleUpdate) => {
      const { data, error, response } = await putBackupSchedule({
        path: { siteId },
        body,
      });
      if (response?.status === 422) {
        throw toError(error ?? { message: "Validation failed" });
      }
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    onSuccess: (schedule) => {
      queryClient.setQueryData(backupsKeys.scheduleFor(siteId), schedule);
    },
  });
}
