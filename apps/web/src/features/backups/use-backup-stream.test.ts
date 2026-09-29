import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement, type ReactNode } from "react";
import type { BackupSnapshotDetail } from "@wpmgr/api";

import { backupEventSchema, isStallHintPhase, useBackupStream } from "./use-backup-stream";
import { backupsKeys } from "./use-backups";
import { scheduleKeys } from "./use-schedule-runs";

// GH #279 — the CP two-tier watchdog publishes "stalled"/"resumed" SSE
// frames on the existing backup Hub (see hub.go / service.go). These are
// hints, not pipeline phases: unlike `use-updates.ts`'s `applyEvent` reducer
// (which patches every field it's given), the backup stream must NOT let a
// hint frame clobber the cached `progress.phase` — the pull-refetch it
// triggers instead is exercised in the render-level indicator tests
// (`stalled-hint.test.ts` covers the gating; the SSE wiring is asserted here
// at the two decidable units: schema acceptance and phase classification).

describe("backupEventSchema — GH #279 stall hints", () => {
  it("accepts a 'stalled' frame (status stays running) instead of dropping it as malformed", () => {
    const frame = {
      snapshot_id: "snap-1",
      phase: "stalled",
      phase_detail: {},
      status: "running",
      ts: "2026-07-23T00:00:00Z",
    };
    expect(() => backupEventSchema.parse(frame)).not.toThrow();
  });

  it("accepts a 'resumed' frame (status stays running) instead of dropping it as malformed", () => {
    const frame = {
      snapshot_id: "snap-1",
      phase: "resumed",
      phase_detail: {},
      status: "running",
      ts: "2026-07-23T00:01:00Z",
    };
    expect(() => backupEventSchema.parse(frame)).not.toThrow();
  });
});

describe("isStallHintPhase", () => {
  it("classifies 'stalled' and 'resumed' as hints", () => {
    expect(isStallHintPhase("stalled")).toBe(true);
    expect(isStallHintPhase("resumed")).toBe(true);
  });

  it("classifies real pipeline phases as NOT hints, so they still patch the cache", () => {
    expect(isStallHintPhase("archiving_files")).toBe(false);
    expect(isStallHintPhase("encrypting_uploading")).toBe(false);
    expect(isStallHintPhase("completed")).toBe(false);
    expect(isStallHintPhase("failed")).toBe(false);
  });

  // GH #791 — "retrying" is a CP hint like stalled/resumed (no phase_detail
  // counters, not a real pipeline phase), but unlike those two it widens the
  // refetch beyond this snapshot's own detail — see the dedicated `describe`
  // block below. It must still accept as a valid frame shape.
  it("accepts a 'retrying' frame (status stays running)", () => {
    const frame = {
      snapshot_id: "snap-1",
      phase: "retrying",
      phase_detail: {},
      status: "running",
      ts: "2026-09-29T00:00:00Z",
    };
    expect(() => backupEventSchema.parse(frame)).not.toThrow();
  });

  it("does NOT classify 'retrying' as a stall hint — it takes the wider GH #791 path instead", () => {
    expect(isStallHintPhase("retrying")).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// GH #791 — wide-refetch SSE frames ("retrying" hint, "failed" terminal)
//
// A command-failure classification (see the #791 design) can end a backup
// on its FIRST attempt, with no retry: the linked schedule run finalises
// alongside the snapshot, and the site's snapshot list row changes too. A
// "retrying" frame means the opposite is still true (still bouncing off the
// initial dispatch) but is equally a signal that the schedule run and list
// are stale. Both frames must therefore invalidate the detail, list AND
// schedule-run query families — not just the detail cache the "stalled"/
// "resumed" hints handle.
//
// jsdom has no EventSource, so a minimal fake stands in — it only needs to
// let `useBackupStream` register `addEventListener("progress", ...)` and
// then let the test fire a frame through it synchronously.
// ---------------------------------------------------------------------------

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  onopen: (() => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  readyState = 0;
  private listeners = new Map<string, Set<EventListener>>();

  constructor(
    public url: string,
    public opts?: { withCredentials?: boolean },
  ) {
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, cb: EventListener): void {
    if (!this.listeners.has(type)) this.listeners.set(type, new Set());
    this.listeners.get(type)?.add(cb);
  }

  removeEventListener(type: string, cb: EventListener): void {
    this.listeners.get(type)?.delete(cb);
  }

  close(): void {
    this.readyState = 2;
  }

  /** Fire a named SSE frame as `useBackupStream`'s onProgress listener sees it. */
  emit(type: string, data: unknown): void {
    const event = { data: JSON.stringify(data) } as MessageEvent;
    this.listeners.get(type)?.forEach((cb) => cb(event));
  }
}

function makeQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
}

function wrapperFor(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return createElement(QueryClientProvider, { client: qc }, children);
  };
}

function seedDetail(qc: QueryClient, snapshotId: string): void {
  const detail: BackupSnapshotDetail = {
    snapshot: {
      id: snapshotId,
      tenant_id: "tenant-1",
      site_id: "site-42",
      kind: "full",
      status: "running",
      created_at: "2026-09-29T00:00:00Z",
      updated_at: "2026-09-29T00:00:00Z",
      progress: {},
    },
    entries: [],
  };
  qc.setQueryData(backupsKeys.detail(snapshotId), detail);
}

let originalEventSource: typeof EventSource | undefined;

beforeEach(() => {
  originalEventSource = globalThis.EventSource;
  FakeEventSource.instances = [];
  // @ts-expect-error — jsdom has no real EventSource; this test double only
  // needs the shape useBackupStream actually calls.
  globalThis.EventSource = FakeEventSource;
});

afterEach(() => {
  globalThis.EventSource = originalEventSource as typeof EventSource;
});

describe("useBackupStream — GH #791 wide-refetch frames", () => {
  it("a 'retrying' frame invalidates the detail, list and schedule-run queries (no cache patch)", () => {
    const snapshotId = "snap-retry";
    const qc = makeQueryClient();
    seedDetail(qc, snapshotId);
    const invalidateSpy = vi.spyOn(qc, "invalidateQueries");

    renderHook(() => useBackupStream(snapshotId), { wrapper: wrapperFor(qc) });
    const source = FakeEventSource.instances[0]!;
    expect(source).toBeDefined();

    source.emit("progress", {
      snapshot_id: snapshotId,
      phase: "retrying",
      phase_detail: {},
      status: "running",
      ts: "2026-09-29T00:01:00Z",
    });

    expect(invalidateSpy).toHaveBeenCalledWith(
      expect.objectContaining({ queryKey: backupsKeys.detail(snapshotId) }),
    );
    // GH #791 adv-review nit 10 — scoped to THIS snapshot's site list (read
    // off the cached detail: seedDetail sets site_id "site-42"), not every
    // ["backups"] query (which would also refetch every other site's list
    // and the unrelated backup-settings queries).
    expect(invalidateSpy).toHaveBeenCalledWith(
      expect.objectContaining({ queryKey: backupsKeys.listFor("site-42") }),
    );
    expect(invalidateSpy).not.toHaveBeenCalledWith(
      expect.objectContaining({ queryKey: backupsKeys.all }),
    );
    expect(invalidateSpy).toHaveBeenCalledWith(
      expect.objectContaining({ queryKey: scheduleKeys.all }),
    );
    // A hint carries no real phase to patch — the cached phase must be left
    // exactly as it was (still the seeded value, not "retrying").
    const cached = qc.getQueryData<BackupSnapshotDetail>(backupsKeys.detail(snapshotId));
    expect(cached?.snapshot.progress).toEqual({});
  });

  it("a 'failed' frame patches the snapshot detail cache AND invalidates the list and schedule-run queries", () => {
    const snapshotId = "snap-failed";
    const qc = makeQueryClient();
    seedDetail(qc, snapshotId);
    const invalidateSpy = vi.spyOn(qc, "invalidateQueries");

    renderHook(() => useBackupStream(snapshotId), { wrapper: wrapperFor(qc) });
    const source = FakeEventSource.instances[0]!;
    expect(source).toBeDefined();

    source.emit("progress", {
      snapshot_id: snapshotId,
      phase: "failed",
      phase_detail: { error: "Agent error: RuntimeException" },
      status: "failed",
      ts: "2026-09-29T00:02:00Z",
    });

    // The existing per-frame patch still fires immediately.
    const cached = qc.getQueryData<BackupSnapshotDetail>(backupsKeys.detail(snapshotId));
    expect(cached?.snapshot.status).toBe("failed");
    expect(cached?.snapshot.progress).toEqual({
      phase: "failed",
      phase_detail: { error: "Agent error: RuntimeException" },
    });
    // GH #791 — plus the wider refetch, because a command failure can end a
    // run (and its linked schedule run) on the very first attempt. Scoped
    // (nit 10) to this snapshot's own site list, not every ["backups"] query.
    expect(invalidateSpy).toHaveBeenCalledWith(
      expect.objectContaining({ queryKey: backupsKeys.listFor("site-42") }),
    );
    expect(invalidateSpy).not.toHaveBeenCalledWith(
      expect.objectContaining({ queryKey: backupsKeys.all }),
    );
    expect(invalidateSpy).toHaveBeenCalledWith(
      expect.objectContaining({ queryKey: scheduleKeys.all }),
    );
  });

  it("a real pipeline phase (e.g. 'archiving_files') patches the cache and does NOT trigger the wide refetch", () => {
    const snapshotId = "snap-progress";
    const qc = makeQueryClient();
    seedDetail(qc, snapshotId);
    const invalidateSpy = vi.spyOn(qc, "invalidateQueries");

    renderHook(() => useBackupStream(snapshotId), { wrapper: wrapperFor(qc) });
    const source = FakeEventSource.instances[0]!;

    source.emit("progress", {
      snapshot_id: snapshotId,
      phase: "archiving_files",
      phase_detail: { files_done: 3, files_total: 10 },
      status: "running",
      ts: "2026-09-29T00:00:30Z",
    });

    const cached = qc.getQueryData<BackupSnapshotDetail>(backupsKeys.detail(snapshotId));
    expect(cached?.snapshot.progress).toEqual({
      phase: "archiving_files",
      phase_detail: { files_done: 3, files_total: 10 },
    });
    expect(invalidateSpy).not.toHaveBeenCalledWith(
      expect.objectContaining({ queryKey: scheduleKeys.all }),
    );
  });
});
