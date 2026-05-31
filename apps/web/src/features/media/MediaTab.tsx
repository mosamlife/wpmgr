import { useCallback, useMemo, useState } from "react";
import { RefreshCw, Search, XCircle } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import { toast } from "@/components/toast";

import { MediaOverview } from "./MediaOverview";
import { AssetsTable } from "./AssetsTable";
import { BulkActionBar } from "./BulkActionBar";
import { OptimizeDialog } from "./OptimizeDialog";
import { RestoreDialog } from "./RestoreDialog";
import { DeleteOriginalsDialog } from "./DeleteOriginalsDialog";
import { AssetDetailDrawer } from "./AssetDetailDrawer";
import { JobsDrawer } from "./JobsDrawer";
import { MediaEmptyState } from "./EmptyState";
import {
  useMediaAssets,
  useSyncMedia,
  useOptimizeMedia,
  useRestoreMedia,
  useDeleteOriginals,
  useCancelMedia,
  type AssetFilters,
} from "./hooks/useMediaAssets";
import { useMediaEvents } from "./hooks/useMediaEvents";
import { selectSiteRows, useJobsStore } from "./jobs-store";
import type { MediaAsset, TargetFormat, TargetQuality } from "./types";

// MediaTab — the Media Optimizer entry surface. Overview tiles + the assets
// table, with the bulk-action toolbar, the three action dialogs, the live jobs
// drawer, and the row-click asset detail drawer. SSE is wired here via
// useMediaEvents so the tab patches its own caches live (no polling).

export interface MediaTabProps {
  siteId: string;
  hostname: string;
  canOperate: boolean;
  canManage: boolean;
}

type DialogKind = "optimize" | "restore" | "delete" | null;

export function MediaTab({
  siteId,
  hostname,
  canOperate,
  canManage,
}: MediaTabProps) {
  // SSE → caches + jobs store. The media.* types are registered in
  // use-site-events.ts; without that registration these frames are dropped.
  useMediaEvents(siteId);

  const [statusFilter, setStatusFilter] = useState("");
  const [formatFilter, setFormatFilter] = useState("");
  const [search, setSearch] = useState("");
  const filters: AssetFilters = useMemo(
    () => ({
      status: statusFilter || undefined,
      format: formatFilter || undefined,
      search: search.trim() || undefined,
    }),
    [statusFilter, formatFilter, search],
  );

  const { data, isPending, isError, error, refetch, isFetching } =
    useMediaAssets(siteId, filters);

  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [detailAsset, setDetailAsset] = useState<MediaAsset | null>(null);
  const [dialog, setDialog] = useState<DialogKind>(null);
  // When a single-asset action is initiated from the detail drawer, we scope
  // the dialog to just that asset; otherwise it acts on the selection.
  const [scopedAsset, setScopedAsset] = useState<MediaAsset | null>(null);

  const sync = useSyncMedia(siteId);
  const optimize = useOptimizeMedia(siteId);
  const restore = useRestoreMedia(siteId);
  const deleteOriginals = useDeleteOriginals(siteId);
  const cancel = useCancelMedia(siteId);

  const liveRows = useJobsStore((s) => selectSiteRows(s, siteId));
  const hasLiveJobs = Object.keys(liveRows).length > 0;

  const toggle = useCallback((id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const toggleMany = useCallback((ids: string[], on: boolean) => {
    setSelected((prev) => {
      const next = new Set(prev);
      for (const id of ids) {
        if (on) next.add(id);
        else next.delete(id);
      }
      return next;
    });
  }, []);

  const clearSelection = useCallback(() => setSelected(new Set()), []);

  // Resolve the asset ids the current dialog acts on (scoped or selected).
  const actingIds = useMemo(
    () => (scopedAsset ? [scopedAsset.id] : [...selected]),
    [scopedAsset, selected],
  );
  const actingCount = actingIds.length;

  // For optimize: "all pending" when no explicit selection AND no scoped asset.
  const summary = data?.summary;
  const pendingCount = summary?.pending ?? 0;

  function openDialog(kind: DialogKind, asset?: MediaAsset) {
    setScopedAsset(asset ?? null);
    setDialog(kind);
  }
  function closeDialog() {
    setDialog(null);
    setScopedAsset(null);
  }

  function runSync() {
    sync.mutate(undefined, {
      onSuccess: () =>
        toast.success("Sync started.", {
          description: "We're enumerating this site's media library.",
        }),
      onError: (err) =>
        toast.error("Could not start sync.", { description: err.message }),
    });
  }

  function confirmOptimize(input: {
    targetFormat: TargetFormat;
    targetQuality: TargetQuality;
    allPending: boolean;
  }) {
    const allPending = scopedAsset ? false : actingCount === 0;
    optimize.mutate(
      {
        asset_ids: allPending ? undefined : actingIds,
        all_pending: allPending || undefined,
        target_format: input.targetFormat,
        target_quality: input.targetQuality,
      },
      {
        onSuccess: (res) => {
          closeDialog();
          clearSelection();
          toast.success(`Optimizing ${res.queued_count.toLocaleString()} assets.`, {
            description: "Track progress in the jobs drawer.",
          });
        },
        onError: (err) =>
          toast.error("Could not start optimization.", {
            description: err.message,
          }),
      },
    );
  }

  function confirmRestore() {
    restore.mutate(
      { asset_ids: actingIds },
      {
        onSuccess: (res) => {
          closeDialog();
          clearSelection();
          toast.success(`Restoring ${res.queued_count.toLocaleString()} attachments.`);
        },
        onError: (err) =>
          toast.error("Could not start restore.", { description: err.message }),
      },
    );
  }

  function confirmDelete() {
    deleteOriginals.mutate(
      { asset_ids: actingIds },
      {
        onSuccess: (res) => {
          closeDialog();
          clearSelection();
          toast.destructive(
            `Deleted originals for ${res.queued_count.toLocaleString()} attachments.`,
            {
              description: "This cannot be undone. Restore is no longer possible.",
              action: { label: "View jobs", onClick: () => useJobsStore.getState().setOpen(siteId, true) },
            },
          );
        },
        onError: (err) =>
          toast.error("Could not delete originals.", {
            description: err.message,
          }),
      },
    );
  }

  function cancelAll() {
    cancel.mutate(undefined, {
      onSuccess: (res) => {
        useJobsStore.getState().clearSite(siteId);
        toast.success(`Cancelled ${res.cancelled_count.toLocaleString()} jobs.`);
      },
      onError: (err) =>
        toast.error("Could not cancel jobs.", { description: err.message }),
    });
  }

  // ── Render ────────────────────────────────────────────────────────────────

  if (isPending) {
    return <MediaTabSkeleton />;
  }

  if (isError) {
    return (
      <PageError
        what="Could not load this site's media."
        why={error.message}
        onRetry={() => void refetch()}
        retryLabel="Reload media"
      />
    );
  }

  const assets = data.items;
  const hasFilters = Boolean(filters.status || filters.format || filters.search);
  const isEmptyLibrary = data.total_count === 0 && !hasFilters;

  return (
    <div className="space-y-4">
      <MediaOverview summary={data.summary} />

      {/* Toolbar */}
      <div className="flex flex-wrap items-center gap-2">
        <div className="relative min-w-[200px] flex-1">
          <Search
            aria-hidden="true"
            className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-[var(--color-muted-foreground)]"
          />
          <Input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="Search by title"
            aria-label="Search media by title"
            className="pl-8"
          />
        </div>
        <Select
          value={statusFilter}
          onChange={(e) => setStatusFilter(e.target.value)}
          aria-label="Filter by status"
          className="w-auto min-w-[140px]"
        >
          <option value="">All statuses</option>
          <option value="pending">Pending</option>
          <option value="optimized">Optimized</option>
          <option value="failed">Failed</option>
          <option value="excluded">Excluded</option>
          <option value="originals_deleted">Originals deleted</option>
        </Select>
        <Select
          value={formatFilter}
          onChange={(e) => setFormatFilter(e.target.value)}
          aria-label="Filter by format"
          className="w-auto min-w-[120px]"
        >
          <option value="">All formats</option>
          <option value="original">Original</option>
          <option value="webp">WebP</option>
          <option value="avif">AVIF</option>
        </Select>

        <div className="ml-auto flex items-center gap-2">
          {hasLiveJobs && canOperate ? (
            <Button
              type="button"
              variant="ghost"
              size="sm"
              onClick={cancelAll}
              disabled={cancel.isPending}
            >
              <XCircle aria-hidden="true" className="size-4" />
              Cancel all
            </Button>
          ) : null}
          {canOperate ? (
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={runSync}
              disabled={sync.isPending}
              aria-label="Sync media library"
            >
              <RefreshCw
                aria-hidden="true"
                className={isFetching || sync.isPending ? "size-4 animate-spin" : "size-4"}
              />
              Sync
            </Button>
          ) : null}
        </div>
      </div>

      {/* Body */}
      {isEmptyLibrary ? (
        <MediaEmptyState
          onSync={runSync}
          isPending={sync.isPending}
          canOperate={canOperate}
        />
      ) : assets.length === 0 ? (
        <div className="flex flex-col items-center gap-2 rounded-lg border border-[var(--color-border)] bg-[var(--color-card)] px-6 py-12 text-center">
          <p className="text-sm font-medium text-[var(--color-foreground)]">
            No assets match these filters.
          </p>
          <Button
            type="button"
            variant="ghost"
            size="sm"
            onClick={() => {
              setStatusFilter("");
              setFormatFilter("");
              setSearch("");
            }}
          >
            Clear filters
          </Button>
        </div>
      ) : (
        <AssetsTable
          assets={assets}
          selected={selected}
          onToggle={toggle}
          onToggleMany={toggleMany}
          onRowClick={(a) => setDetailAsset(a)}
          onReasonClick={(a) => setDetailAsset(a)}
        />
      )}

      {/* Bulk actions */}
      <BulkActionBar
        selectedCount={selected.size}
        canOperate={canOperate}
        canDelete={canManage}
        onClear={clearSelection}
        onOptimize={() => openDialog("optimize")}
        onRestore={() => openDialog("restore")}
        onDeleteOriginals={() => openDialog("delete")}
      />

      {/* Dialogs */}
      <OptimizeDialog
        open={dialog === "optimize"}
        onClose={closeDialog}
        selectedCount={scopedAsset ? 1 : selected.size}
        pendingCount={pendingCount}
        onConfirm={confirmOptimize}
        isPending={optimize.isPending}
        errorMessage={optimize.isError ? optimize.error.message : null}
      />
      <RestoreDialog
        open={dialog === "restore"}
        onClose={closeDialog}
        count={actingCount}
        onConfirm={confirmRestore}
        isPending={restore.isPending}
        errorMessage={restore.isError ? restore.error.message : null}
      />
      <DeleteOriginalsDialog
        open={dialog === "delete"}
        onClose={closeDialog}
        hostname={hostname}
        count={actingCount}
        onConfirm={confirmDelete}
        isPending={deleteOriginals.isPending}
        errorMessage={deleteOriginals.isError ? deleteOriginals.error.message : null}
      />

      {/* Row-click detail drawer */}
      <AssetDetailDrawer
        siteId={siteId}
        asset={detailAsset}
        jobId={null}
        onClose={() => setDetailAsset(null)}
        canOperate={canOperate}
        onReoptimize={(a) => {
          setDetailAsset(null);
          openDialog("optimize", a);
        }}
        onRestore={(a) => {
          setDetailAsset(null);
          openDialog("restore", a);
        }}
      />

      {/* Live jobs drawer (fixed; self-hides when no rows) */}
      <JobsDrawer siteId={siteId} />
    </div>
  );
}

function MediaTabSkeleton() {
  return (
    <div role="status" aria-busy="true" aria-label="Loading media" className="space-y-4">
      <span className="sr-only">Loading media</span>
      <div className="grid grid-cols-2 gap-px overflow-hidden rounded-lg border border-[var(--color-border)] bg-[var(--color-border)] sm:grid-cols-4">
        {Array.from({ length: 4 }).map((_, i) => (
          <div key={i} className="space-y-2 bg-[var(--color-card)] p-4">
            <Skeleton className="h-3 w-20" />
            <Skeleton className="h-7 w-16" />
          </div>
        ))}
      </div>
      <div className="flex gap-2">
        <Skeleton className="h-9 flex-1" />
        <Skeleton className="h-9 w-36" />
        <Skeleton className="h-9 w-28" />
      </div>
      <div className="overflow-hidden rounded-lg border border-[var(--color-border)] bg-[var(--color-card)]">
        {Array.from({ length: 6 }).map((_, i) => (
          <div
            key={i}
            className="flex items-center gap-4 border-b border-[var(--color-border)] px-4 py-3.5 last:border-0"
          >
            <Skeleton className="size-4 rounded" />
            <Skeleton className="size-9 rounded" />
            <Skeleton className="h-4 flex-1" />
            <Skeleton className="h-4 w-24" />
            <Skeleton className="h-4 w-16" />
          </div>
        ))}
      </div>
    </div>
  );
}
