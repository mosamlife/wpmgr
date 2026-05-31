import { describe, it, expect, vi } from "vitest";

import type { SiteEvent } from "@/features/sites/use-site-events";

import {
  mediaEventReducer,
  patchAssetStatus,
  type MediaEventDeps,
} from "./hooks/useMediaEvents";
import type { LiveJobRow } from "./jobs-store";
import type { ListAssetsResponse, MediaAsset } from "./types";

const SITE_ID = "33333333-3333-3333-3333-333333333333";

function makeAsset(overrides: Partial<MediaAsset> = {}): MediaAsset {
  return {
    id: "a1",
    site_id: SITE_ID,
    wp_attachment_id: 123,
    title: "hero.jpg",
    original_url: "https://site/hero.jpg",
    original_mime: "image/jpeg",
    original_size_bytes: 1000,
    current_format: "original",
    current_size_bytes: 1000,
    status: "pending",
    generation: 0,
    ...overrides,
  };
}

function makePage(items: MediaAsset[]): ListAssetsResponse {
  const optimized = items.filter(
    (a) => a.status === "optimized" || a.status === "originals_deleted",
  ).length;
  const pending = items.filter(
    (a) => a.status === "pending" || a.status === "optimizing",
  ).length;
  const failed = items.filter((a) => a.status === "failed").length;
  return {
    items,
    total_count: items.length,
    summary: {
      total: items.length,
      optimized,
      pending,
      failed,
      bytes_saved: 0,
    },
  };
}

function makeEvent(
  type: SiteEvent["type"],
  data: Record<string, unknown>,
  siteId = SITE_ID,
): SiteEvent {
  return {
    id: "01HXEVENT",
    type,
    site_id: siteId,
    ts: "2026-05-31T00:00:00Z",
    data,
  };
}

type SetQueriesDataMock = ReturnType<typeof vi.fn>;
type UpsertRowMock = ReturnType<typeof vi.fn<(row: LiveJobRow) => void>>;

type PageUpdater = (
  p: ListAssetsResponse | undefined,
) => ListAssetsResponse | undefined;

/** Pull the page-updater fn passed as the 2nd arg of the first setQueriesData call. */
function firstUpdater(mock: SetQueriesDataMock): PageUpdater {
  const call = mock.mock.calls[0];
  if (!call) throw new Error("setQueriesData was not called");
  return call[1] as PageUpdater;
}

/** Pull the LiveJobRow passed to the first upsertRow call. */
function firstRow(mock: UpsertRowMock): LiveJobRow {
  const call = mock.mock.calls[0];
  if (!call) throw new Error("upsertRow was not called");
  return call[0];
}

// A minimal fake of the slice of QueryClient the reducer uses.
function fakeDeps() {
  const setQueriesData = vi.fn();
  const invalidateQueries = vi.fn();
  const upsertRow = vi.fn<(row: LiveJobRow) => void>();
  const openDrawer = vi.fn();
  const deps: MediaEventDeps = {
    siteId: SITE_ID,
    queryClient: { setQueriesData, invalidateQueries },
    upsertRow,
    openDrawer,
  };
  return { deps, setQueriesData, invalidateQueries, upsertRow, openDrawer };
}

describe("patchAssetStatus", () => {
  it("flips the matching row by wp_attachment_id and recomputes the summary", () => {
    const page = makePage([
      makeAsset({ id: "a1", wp_attachment_id: 123, status: "optimizing" }),
      makeAsset({ id: "a2", wp_attachment_id: 456, status: "pending" }),
    ]);
    const next = patchAssetStatus(page, 123, "optimized");
    expect(next.items.find((a) => a.wp_attachment_id === 123)?.status).toBe(
      "optimized",
    );
    // a2 is untouched.
    expect(next.items.find((a) => a.wp_attachment_id === 456)?.status).toBe(
      "pending",
    );
    // summary recomputed: 1 optimized, 1 pending, 0 failed.
    expect(next.summary.optimized).toBe(1);
    expect(next.summary.pending).toBe(1);
    expect(next.summary.failed).toBe(0);
  });

  it("returns the same reference when nothing matches (no needless re-render)", () => {
    const page = makePage([makeAsset({ wp_attachment_id: 123 })]);
    expect(patchAssetStatus(page, 999, "optimized")).toBe(page);
  });

  it("holds total and bytes_saved from the server", () => {
    const page = makePage([makeAsset({ wp_attachment_id: 123 })]);
    page.summary.bytes_saved = 5000;
    const next = patchAssetStatus(page, 123, "optimized");
    expect(next.summary.total).toBe(1);
    expect(next.summary.bytes_saved).toBe(5000);
  });
});

describe("mediaEventReducer", () => {
  it("ignores events for other event families", () => {
    const { deps, setQueriesData, upsertRow } = fakeDeps();
    mediaEventReducer(makeEvent("site.heartbeat", {}), deps);
    expect(setQueriesData).not.toHaveBeenCalled();
    expect(upsertRow).not.toHaveBeenCalled();
  });

  it("optimize.asset_done patches the row but does NOT invalidate (cheap path)", () => {
    const { deps, setQueriesData, invalidateQueries } = fakeDeps();
    mediaEventReducer(
      makeEvent("media.optimize.asset_done", {
        job_id: "01JOB",
        wp_attachment_id: 123,
        applied: 4,
      }),
      deps,
    );
    expect(setQueriesData).toHaveBeenCalledTimes(1);
    expect(invalidateQueries).not.toHaveBeenCalled();
  });

  it("the asset_done patcher resolves to 'optimized'", () => {
    const { deps, setQueriesData } = fakeDeps();
    mediaEventReducer(
      makeEvent("media.optimize.asset_done", {
        job_id: "01JOB",
        wp_attachment_id: 123,
      }),
      deps,
    );
    const updater = firstUpdater(setQueriesData);
    const page = makePage([makeAsset({ wp_attachment_id: 123, status: "optimizing" })]);
    const result = updater(page);
    expect(result?.items[0]?.status).toBe("optimized");
  });

  it("restore.asset_done patches the row to 'restored'", () => {
    const { deps, setQueriesData } = fakeDeps();
    mediaEventReducer(
      makeEvent("media.restore.asset_done", {
        job_id: "01JOB",
        wp_attachment_id: 123,
      }),
      deps,
    );
    const updater = firstUpdater(setQueriesData);
    const page = makePage([makeAsset({ wp_attachment_id: 123, status: "restoring" })]);
    expect(updater(page)?.items[0]?.status).toBe("restored");
  });

  it("optimize.completed invalidates assets + jobs (authoritative re-read)", () => {
    const { deps, invalidateQueries } = fakeDeps();
    mediaEventReducer(
      makeEvent("media.optimize.completed", {
        job_id: "01JOB",
        state: "succeeded",
      }),
      deps,
    );
    expect(invalidateQueries).toHaveBeenCalledTimes(2);
  });

  it("optimize.started upserts a live row and opens the drawer", () => {
    const { deps, upsertRow, openDrawer } = fakeDeps();
    mediaEventReducer(
      makeEvent("media.optimize.started", {
        batch_job_id: "01BATCH",
        queued_count: 5,
        target_format: "avif",
      }),
      deps,
    );
    expect(upsertRow).toHaveBeenCalledTimes(1);
    const row = firstRow(upsertRow);
    expect(row.jobId).toBe("01BATCH");
    expect(row.kind).toBe("optimize");
    expect(row.state).toBe("in_progress");
    expect(openDrawer).toHaveBeenCalledTimes(1);
  });

  it("optimize.progress upserts a row with a derived percentage", () => {
    const { deps, upsertRow } = fakeDeps();
    mediaEventReducer(
      makeEvent("media.optimize.progress", {
        job_id: "01JOB",
        variant: "thumbnail",
        phase: "encoded",
      }),
      deps,
    );
    const row = firstRow(upsertRow);
    expect(row.progress).toBe(75);
    expect(row.state).toBe("in_progress");
  });

  it("job.failed marks the asset failed AND records the reason on the row", () => {
    const { deps, setQueriesData, upsertRow } = fakeDeps();
    mediaEventReducer(
      makeEvent("media.job.failed", {
        job_id: "01JOB",
        wp_attachment_id: 123,
        reason: "encode timed out",
      }),
      deps,
    );
    // asset row patched to failed
    const updater = firstUpdater(setQueriesData);
    const page = makePage([makeAsset({ wp_attachment_id: 123, status: "optimizing" })]);
    expect(updater(page)?.items[0]?.status).toBe("failed");
    // live row marked failed with reason
    const row = firstRow(upsertRow);
    expect(row.state).toBe("failed");
    expect(row.reason).toBe("encode timed out");
  });
});
