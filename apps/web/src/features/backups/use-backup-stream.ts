import { useEffect, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { z } from "zod";
import type { BackupSnapshotDetail } from "@wpmgr/api";

import { BUILD_VERSION } from "@/lib/build";
import { backupsKeys } from "./use-backups";

// M5.6 — live backup-snapshot progress via Server-Sent Events. Mirrors the
// proven M3 `useRunEventStream` pattern from `features/updates/use-updates.ts`:
//   - EventSource opened in useEffect (NEVER inside a queryFn — see prior
//     research dossier; queryFns must remain pure async)
//   - Each `event: progress` frame is validated with Zod and PATCHED into the
//     `useBackup(snapshotId)` query cache so the existing UI rerenders
//   - On terminal status the EventSource is closed
//   - On hard onerror (after EventSource's own native reconnects) we increment
//     a failure count; after 6 we close and let the 1 s polling fallback in
//     `useBackup` take over
//   - We publish the SSE-live status in a module-level Set so `useBackup` can
//     suppress polling while a healthy stream is feeding the cache (V0 quick
//     and dirty per task brief — Context would also work)
//
// The CP wire contract (see backend M5.6 SSE handler):
//   event: progress
//   data: {"snapshot_id":"<uuid>","phase":"...","phase_detail":{...},
//          "status":"pending|running|completed|failed","ts":"<iso8601>"}
//   (interleaved 15 s `:\n\n` heartbeat comments — EventSource ignores natively)

/** Zod schema — source of truth for the wire payload. */
export const backupEventSchema = z.object({
  snapshot_id: z.string(),
  phase: z.enum([
    // Backup phases (M5.6 / ADR-033)
    "queued",
    "started",
    "dumping_db",
    "archiving_files",
    "compressing_files",
    "encrypting",
    "uploading",
    "encrypting_uploading",
    "submitting_manifest",
    // Restore phases (M5.6 / ADR-034) — share the same SSE channel by
    // snapshot_id; the frontend reads `phase` to decide which stepper to render
    "preflight",
    "download_artifacts",
    "verify_artifacts",
    "maintenance_on",
    "stage_files",
    "swap_files",
    "restore_db",
    "migrate_db",
    "swap_db",
    "post_hooks",
    "maintenance_off",
    "cleanup",
    "rolled_back",
    // Terminal — shared
    "completed",
    "failed",
  ]),
  phase_detail: z.record(z.string(), z.unknown()),
  status: z.enum(["pending", "running", "completed", "failed"]),
  ts: z.string(),
});

export type BackupEvent = z.infer<typeof backupEventSchema>;

/**
 * Module-level coordination between `useBackupStream` (the producer of live
 * SSE updates) and `useBackup` (the consumer that should stop polling while
 * the stream is healthy). V0 design choice: a `Set<string>` keyed by snapshot
 * id, with a tiny subscriber list so the polling hook can recompute its
 * `refetchInterval` when the set membership changes. The alternative (React
 * Context) would force every snapshot detail consumer to live under a single
 * provider — overkill for the one-snapshot-detail-per-page reality.
 */
const liveStreams = new Set<string>();
type Listener = () => void;
const listeners = new Set<Listener>();

function notify(): void {
  listeners.forEach((l) => l());
}

export function isStreamLive(snapshotId: string): boolean {
  return liveStreams.has(snapshotId);
}

/** Subscribe to live-stream membership changes (used by `useBackup`). */
export function subscribeLiveStreams(listener: Listener): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/** Max consecutive hard EventSource errors before giving up on SSE entirely. */
const MAX_FAILURES = 6;

export type BackupStreamState = {
  isLive: boolean;
  failureCount: number;
};

/**
 * Subscribe to the live backup-progress SSE stream for a snapshot and patch
 * deltas into the `useBackup` query cache. Returns the current transport
 * state so the UI can render a live/polling indicator.
 *
 * Same-origin endpoint via Vite proxy, so the session cookie flows
 * automatically (`withCredentials: true`).
 */
export function useBackupStream(snapshotId: string): BackupStreamState {
  const queryClient = useQueryClient();
  const [state, setState] = useState<BackupStreamState>({
    isLive: false,
    failureCount: 0,
  });

  useEffect(() => {
    if (!snapshotId) return;
    if (typeof EventSource === "undefined") {
      // SSR or ancient browser — leave polling as the only mechanism.
       
      console.warn("[wpmgr-sse] EventSource not available — polling only");
      return;
    }

    let closed = false;
    let failures = 0;
    let markedLive = false;
    const url = `/api/v1/backups/${encodeURIComponent(snapshotId)}/events`;
     
    console.log("[wpmgr-sse] mount + open EventSource", { build: BUILD_VERSION, snapshotId, url });
    const source = new EventSource(url, { withCredentials: true });

    const markLive = () => {
      if (markedLive) return;
      markedLive = true;
      liveStreams.add(snapshotId);
      notify();
      setState({ isLive: true, failureCount: failures });
    };

    const teardown = () => {
      if (closed) return;
      closed = true;
      source.close();
      if (markedLive) {
        markedLive = false;
        liveStreams.delete(snapshotId);
        notify();
      }
    };

    source.onopen = () => {
      if (closed) return;
       
      console.log("[wpmgr-sse] onopen", { snapshotId });
      failures = 0;
      markLive();
    };

    // The CP uses a named `progress` event, not the default unnamed one.
    const onProgress = (msg: MessageEvent<string>) => {
      if (closed) return;
      // Belt-and-braces: onopen is sometimes deferred behind a proxy; if the
      // first frame arrives first, count that as live too.
      markLive();
      let parsed: BackupEvent;
      try {
        const raw = JSON.parse(msg.data) as unknown;
        parsed = backupEventSchema.parse(raw);
      } catch (err) {
        // Malformed frame (or a comment that somehow surfaced as data) — drop.
         
        console.warn("[wpmgr-sse] dropped malformed frame", { snapshotId, err, raw: msg.data?.slice?.(0, 200) });
        return;
      }
       
      console.log("[wpmgr-sse] event", { phase: parsed.phase, status: parsed.status, detail: parsed.phase_detail });
      if (parsed.snapshot_id !== snapshotId) return;

      queryClient.setQueryData<BackupSnapshotDetail>(
        backupsKeys.detail(snapshotId),
        (prev) => {
          if (!prev) return prev;
          return {
            ...prev,
            snapshot: {
              ...prev.snapshot,
              status: parsed.status,
              progress: { phase: parsed.phase, phase_detail: parsed.phase_detail },
              progress_updated_at: parsed.ts,
            },
          };
        },
      );

      // NOTE — we DO NOT auto-close on terminal status. A snapshot is a
      // long-lived entity that can have restore events overlaid on top of a
      // completed backup (ADR-034). If we close here, the FIRST frame echo
      // of a stale terminal state (the SSE handler's initial-state snapshot)
      // causes the EventSource to close instantly, browser native reconnect
      // re-opens, gets the same terminal frame, closes again — and any
      // subsequent restore events are dropped. The stream stays open for
      // the lifetime of the page; teardown happens on unmount only.
    };
    source.addEventListener("progress", onProgress as EventListener);

    source.onerror = (err) => {
      if (closed) return;
      // EventSource will auto-reconnect on its own; we only count and bail out
      // once we've crossed the threshold.
      failures += 1;
       
      console.warn("[wpmgr-sse] onerror", { snapshotId, failures, readyState: source.readyState, err });
      // Drop the live flag immediately so polling can resume even on the
      // first hiccup — it'll come back on the next onopen.
      if (markedLive) {
        markedLive = false;
        liveStreams.delete(snapshotId);
        notify();
      }
      setState({ isLive: false, failureCount: failures });
      if (failures >= MAX_FAILURES) {
        teardown();
      }
    };

    return () => {
      teardown();
    };
  }, [snapshotId, queryClient]);

  return state;
}
