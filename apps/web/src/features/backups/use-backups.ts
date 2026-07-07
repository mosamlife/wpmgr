import { useSyncExternalStore } from "react";
import {
  useQuery,
  useMutation,
  useQueryClient,
  type UseQueryResult,
  type UseMutationResult,
} from "@tanstack/react-query";
import {
  client,
  createBackup,
  listBackups,
  getBackup,
  deleteBackup,
  cancelBackup,
  lockBackup,
  unlockBackup,
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

import { extractByoDestinationRequired } from "@/lib/api";
import { toError } from "@/features/auth/use-auth";
import { isStreamLive, subscribeLiveStreams } from "./use-backup-stream";

// Server-state hooks for the M4 Backups domain (snapshots, restore, schedule).
// Built on the generated @wpmgr/api SDK; each call returns
// `{ data, error, response }` which we unwrap so TanStack Query owns
// loading/error/success. The control plane runs backups and restores
// asynchronously, so the snapshot-detail hook polls (refetchInterval) while a
// job is pending/running and stops once it reaches a terminal state.

// ---------------------------------------------------------------------------
// m50 backup-settings domain types (hand-rolled; endpoints not yet generated)
// ---------------------------------------------------------------------------

/** Track-A: backup scope/exclusions settings. GET response from /backup-settings/contents. */
export interface SiteBackupSettingsContents {
  site_id: string;
  backup_components: string[] | null;
  include_core: boolean;
  exclude_paths: string[] | null;
  exclude_extensions: string[] | null;
  exclude_file_size_mb: number | null;
  created_at: string;
  updated_at: string;
}

/** PUT request body for /backup-settings/contents. */
export interface SiteBackupSettingsContentsUpdate {
  backup_components?: string[] | null;
  include_core?: boolean;
  exclude_paths?: string[] | null;
  exclude_extensions?: string[] | null;
  exclude_file_size_mb?: number | null;
}

/** Track-B: notification settings. GET response from /backup-settings/notifications. */
export interface SiteBackupSettingsNotifications {
  site_id: string;
  notify_on_completion: "always" | "on_failure" | "never";
  notify_recipients: string[];
  created_at: string;
  updated_at: string;
}

/** PUT request body for /backup-settings/notifications. */
export interface SiteBackupSettingsNotificationsUpdate {
  notify_on_completion?: "always" | "on_failure" | "never";
  notify_recipients?: string[];
}

export const backupsKeys = {
  all: ["backups"] as const,
  listFor: (siteId: string) => [...backupsKeys.all, "list", siteId] as const,
  detail: (snapshotId: string) =>
    [...backupsKeys.all, "detail", snapshotId] as const,
  scheduleFor: (siteId: string) =>
    [...backupsKeys.all, "schedule", siteId] as const,
  backupSettingsContentsFor: (siteId: string) =>
    ["backups", "settings", "contents", siteId] as const,
  backupSettingsNotificationsFor: (siteId: string) =>
    ["backups", "settings", "notifications", siteId] as const,
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
    // 1 s (≤1 s gives a "live" feel for in-progress operations).
    // Stops once the snapshot is terminal.
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
      // 402 byo_destination_required (M16 Phase B backup-destinations Phase
      // 2): the resolved destination is CP-managed storage and the tenant's
      // plan does not permit it. Run through the shared interceptor so
      // callers can swap to BackupDestinationRequiredPrompt instead of a raw
      // error string.
      const destinationRequired = extractByoDestinationRequired(
        response?.status,
        error,
      );
      if (destinationRequired) throw destinationRequired;
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
 * Delete a terminal snapshot (operator+). Chain-safe on the server: deleting a
 * base/mid-chain increment that has dependent later increments is refused with a
 * 422 (chain_has_dependents); a running snapshot must be cancelled first.
 *
 * On success we invalidate the site's snapshot list (and the detail cache) so
 * the deleted row disappears. `siteId` is required for the list invalidation —
 * the snapshot row carries it (snapshot.site_id) for callers in the list view.
 */
export function useDeleteBackup(
  snapshotId: string,
  siteId: string,
): UseMutationResult<void, Error, void> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      const { error, response } = await deleteBackup({ path: { snapshotId } });
      // 204 No Content is the success shape; the SDK leaves `data` undefined.
      if (response?.status === 422) {
        throw toError(error ?? { message: "This backup cannot be deleted yet" });
      }
      if (error) throw toError(error);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: backupsKeys.listFor(siteId),
      });
      queryClient.removeQueries({ queryKey: backupsKeys.detail(snapshotId) });
    },
  });
}

/**
 * Cancel a running/pending snapshot (operator+). The server marks it failed
 * ("cancelled by operator"), which both makes it deletable and rejects any late
 * agent manifest submit. A terminal snapshot is refused with a 409. On success
 * we patch the detail cache with the now-failed snapshot and invalidate the
 * site's list so the badge flips immediately.
 */
export function useCancelBackup(
  snapshotId: string,
  siteId: string,
): UseMutationResult<BackupSnapshot, Error, void> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      const { data, error } = await cancelBackup({ path: { snapshotId } });
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    onSuccess: (snapshot) => {
      queryClient.setQueryData<BackupSnapshotDetail>(
        backupsKeys.detail(snapshotId),
        (prev) => (prev ? { ...prev, snapshot } : prev),
      );
      void queryClient.invalidateQueries({
        queryKey: backupsKeys.listFor(siteId),
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

/**
 * Track C (m49) — lock a completed snapshot against retention GC (operator+).
 * PATCH /api/v1/backups/:snapshotId/lock sets locked=true.
 * Returns the updated snapshot with locked=true.
 *
 * Optimistic update: flip locked=true in the list cache immediately so the
 * icon flips on click without waiting for the background invalidation refetch.
 * On error the previous list data is restored.
 */
export function useLockBackup(
  snapshotId: string,
  siteId: string,
): UseMutationResult<BackupSnapshot, Error, void> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      const { data, error, response } = await lockBackup({
        path: { snapshotId },
      });
      if (response?.status === 409) {
        throw toError(
          error ?? { message: "Cannot lock a pending or running snapshot" },
        );
      }
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    onMutate: async () => {
      await queryClient.cancelQueries({ queryKey: backupsKeys.listFor(siteId) });
      const previous = queryClient.getQueryData<BackupSnapshot[]>(
        backupsKeys.listFor(siteId),
      );
      queryClient.setQueryData<BackupSnapshot[]>(
        backupsKeys.listFor(siteId),
        (prev) =>
          prev?.map((s) =>
            s.id === snapshotId ? { ...s, locked: true } : s,
          ),
      );
      return { previous };
    },
    onError: (_err, _vars, context) => {
      if (context?.previous !== undefined) {
        queryClient.setQueryData(
          backupsKeys.listFor(siteId),
          context.previous,
        );
      }
    },
    onSuccess: (snapshot) => {
      queryClient.setQueryData<BackupSnapshotDetail>(
        backupsKeys.detail(snapshotId),
        (prev) => (prev ? { ...prev, snapshot } : prev),
      );
      queryClient.setQueryData<BackupSnapshot[]>(
        backupsKeys.listFor(siteId),
        (prev) =>
          prev?.map((s) => (s.id === snapshotId ? snapshot : s)),
      );
    },
    onSettled: () => {
      void queryClient.invalidateQueries({
        queryKey: backupsKeys.listFor(siteId),
      });
    },
  });
}

// ---------------------------------------------------------------------------
// m50 backup-settings hooks
// ---------------------------------------------------------------------------

/**
 * GET /api/v1/sites/:siteId/backup-settings/contents
 * Returns the Track-A scope/exclusion settings, or null when no settings row
 * exists yet (404 = safe defaults apply; the card renders its default state).
 */
export function useBackupSettingsContents(
  siteId: string,
): UseQueryResult<SiteBackupSettingsContents | null, Error> {
  return useQuery({
    queryKey: backupsKeys.backupSettingsContentsFor(siteId),
    queryFn: async () => {
      const result = await client.get<SiteBackupSettingsContents, unknown, false>({
        url: `/api/v1/sites/${encodeURIComponent(siteId)}/backup-settings/contents`,
      });
      if (result.response?.status === 404) return null;
      if (result.error !== undefined) throw toError(result.error);
      return result.data ?? null;
    },
    enabled: Boolean(siteId),
  });
}

/**
 * PUT /api/v1/sites/:siteId/backup-settings/contents
 * Persists Track-A scope/exclusion settings. On success seeds the query cache
 * so the ContentsCard reflects the persisted state immediately.
 */
export function usePutBackupSettingsContents(
  siteId: string,
): UseMutationResult<SiteBackupSettingsContents, Error, SiteBackupSettingsContentsUpdate> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: SiteBackupSettingsContentsUpdate) => {
      const result = await client.put<SiteBackupSettingsContents, unknown, false>({
        url: `/api/v1/sites/${encodeURIComponent(siteId)}/backup-settings/contents`,
        body,
        headers: { "Content-Type": "application/json" },
      });
      if (result.response?.status === 422) {
        throw toError(result.error ?? { message: "Validation failed" });
      }
      if (result.error !== undefined) throw toError(result.error);
      if (!result.data) throw new Error("Empty response");
      return result.data;
    },
    onSuccess: (data) => {
      queryClient.setQueryData(
        backupsKeys.backupSettingsContentsFor(siteId),
        data,
      );
    },
  });
}

/**
 * GET /api/v1/sites/:siteId/backup-settings/notifications
 * Returns the Track-B notification settings, or null on 404 (no row yet).
 */
export function useBackupSettingsNotifications(
  siteId: string,
): UseQueryResult<SiteBackupSettingsNotifications | null, Error> {
  return useQuery({
    queryKey: backupsKeys.backupSettingsNotificationsFor(siteId),
    queryFn: async () => {
      const result = await client.get<SiteBackupSettingsNotifications, unknown, false>({
        url: `/api/v1/sites/${encodeURIComponent(siteId)}/backup-settings/notifications`,
      });
      if (result.response?.status === 404) return null;
      if (result.error !== undefined) throw toError(result.error);
      return result.data ?? null;
    },
    enabled: Boolean(siteId),
  });
}

/**
 * PUT /api/v1/sites/:siteId/backup-settings/notifications
 * Persists Track-B notification settings. On success seeds the query cache.
 */
export function usePutBackupSettingsNotifications(
  siteId: string,
): UseMutationResult<SiteBackupSettingsNotifications, Error, SiteBackupSettingsNotificationsUpdate> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: SiteBackupSettingsNotificationsUpdate) => {
      const result = await client.put<SiteBackupSettingsNotifications, unknown, false>({
        url: `/api/v1/sites/${encodeURIComponent(siteId)}/backup-settings/notifications`,
        body,
        headers: { "Content-Type": "application/json" },
      });
      if (result.response?.status === 422) {
        throw toError(result.error ?? { message: "Validation failed" });
      }
      if (result.error !== undefined) throw toError(result.error);
      if (!result.data) throw new Error("Empty response");
      return result.data;
    },
    onSuccess: (data) => {
      queryClient.setQueryData(
        backupsKeys.backupSettingsNotificationsFor(siteId),
        data,
      );
    },
  });
}

/**
 * Track C (m49) — unlock a snapshot so it becomes GC-eligible again (operator+).
 * DELETE /api/v1/backups/:snapshotId/lock sets locked=false.
 * Returns the updated snapshot with locked=false.
 *
 * Optimistic update: flip locked=false in the list cache immediately so the
 * icon flips on click without waiting for the background invalidation refetch.
 * On error the previous list data is restored.
 */
export function useUnlockBackup(
  snapshotId: string,
  siteId: string,
): UseMutationResult<BackupSnapshot, Error, void> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      const { data, error } = await unlockBackup({
        path: { snapshotId },
      });
      if (error) throw toError(error);
      if (!data) throw new Error("Empty response");
      return data;
    },
    onMutate: async () => {
      await queryClient.cancelQueries({ queryKey: backupsKeys.listFor(siteId) });
      const previous = queryClient.getQueryData<BackupSnapshot[]>(
        backupsKeys.listFor(siteId),
      );
      queryClient.setQueryData<BackupSnapshot[]>(
        backupsKeys.listFor(siteId),
        (prev) =>
          prev?.map((s) =>
            s.id === snapshotId ? { ...s, locked: false } : s,
          ),
      );
      return { previous };
    },
    onError: (_err, _vars, context) => {
      if (context?.previous !== undefined) {
        queryClient.setQueryData(
          backupsKeys.listFor(siteId),
          context.previous,
        );
      }
    },
    onSuccess: (snapshot) => {
      queryClient.setQueryData<BackupSnapshotDetail>(
        backupsKeys.detail(snapshotId),
        (prev) => (prev ? { ...prev, snapshot } : prev),
      );
      queryClient.setQueryData<BackupSnapshot[]>(
        backupsKeys.listFor(siteId),
        (prev) =>
          prev?.map((s) => (s.id === snapshotId ? snapshot : s)),
      );
    },
    onSettled: () => {
      void queryClient.invalidateQueries({
        queryKey: backupsKeys.listFor(siteId),
      });
    },
  });
}
