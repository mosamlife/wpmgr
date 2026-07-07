// /backups — fleet-wide backup browser.
//
// Layout:
//   1. ExceptionSummaryTiles (Protected / Stale / Failed / Unprotected)
//      driven by GET /api/v1/backups/health
//   2. FleetTable ROW-PER-SITE (not per-snapshot) with:
//      - Last good backup (age, colour-coded)
//      - Next scheduled run
//      - Latest size
//      - Size-trend sparkline (placeholder until fleet API provides series)
//      - Status badge
//      - Row actions: Run backup | Browse snapshots | Restore
//   NOTE: No download button — deferred per the spec.

import { useMemo } from "react";
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { z } from "zod";
import {
  CheckCircle2,
  AlertTriangle,
  XCircle,
  ShieldOff,
  Database,
  Play,
  List,
  RefreshCw,
  RotateCw,
  MoreHorizontal,
} from "lucide-react";
import type { ColumnDef } from "@tanstack/react-table";

import { PageHeader } from "@/components/shared/page-header";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  ExceptionSummaryTiles,
  useFilterToggle,
  type TileDefinition,
} from "@/features/fleet/ExceptionSummaryTiles";
import { FleetTable } from "@/features/fleet/FleetTable";
import { SparklineCell } from "@/features/fleet/SparklineCell";
import {
  useBackupHealth,
} from "@/features/fleet/use-fleet-backups";
import type {
  BackupHealthItem,
  BackupHealthStatus,
} from "@/features/fleet/fleet-types";
import { useCreateBackup } from "@/features/backups/use-backups";
import { toast } from "@/components/toast";
import { cn, relativeTime, formatBytes } from "@/lib/utils";
import { ByoDestinationRequiredError } from "@/lib/api";

// ---------------------------------------------------------------------------
// Route
// ---------------------------------------------------------------------------

const searchSchema = z.object({
  status: z
    .enum(["protected", "stale", "failed", "unprotected", "in_flight"])
    .optional(),
});

export const Route = createFileRoute("/_authed/backups/")({
  validateSearch: searchSchema,
  component: BackupsIndexPage,
});

// ---------------------------------------------------------------------------
// Status helpers
// ---------------------------------------------------------------------------

const STATUS_ICON: Record<BackupHealthStatus, typeof CheckCircle2> = {
  protected: CheckCircle2,
  stale: AlertTriangle,
  failed: XCircle,
  unprotected: ShieldOff,
  in_flight: RefreshCw,
};

const STATUS_LABEL: Record<BackupHealthStatus, string> = {
  protected: "Protected",
  stale: "Stale",
  failed: "Failed",
  unprotected: "Unprotected",
  in_flight: "In progress",
};

const STATUS_COLOR_CLASS: Record<BackupHealthStatus, string> = {
  protected: "text-[var(--color-success-subtle-fg)]",
  stale: "text-[var(--color-warning-subtle-fg)]",
  failed: "text-[var(--color-destructive-subtle-fg)]",
  unprotected: "text-[var(--color-muted-foreground)]",
  in_flight: "text-[var(--color-info-subtle-fg)]",
};

// Age colour for last_completed_at column.
function ageColor(iso: string | null): string {
  if (!iso) return "text-[var(--color-muted-foreground)]";
  const hours = (Date.now() - Date.parse(iso)) / 3_600_000;
  if (hours <= 26) return "text-[var(--color-success-subtle-fg)]";
  if (hours <= 72) return "text-[var(--color-warning-subtle-fg)]";
  return "text-[var(--color-destructive-subtle-fg)]";
}

// ---------------------------------------------------------------------------
// Tile definitions
// ---------------------------------------------------------------------------

function buildTiles(counts: Record<BackupHealthStatus, number>): TileDefinition[] {
  return [
    {
      key: "protected",
      label: "Protected",
      count: counts.protected,
      icon: <CheckCircle2 className="size-5" />,
      tone: "success",
    },
    {
      key: "stale",
      label: "Stale",
      count: counts.stale,
      icon: <AlertTriangle className="size-5" />,
      tone: "warning",
    },
    {
      key: "failed",
      label: "Failed",
      count: counts.failed,
      icon: <XCircle className="size-5" />,
      tone: "destructive",
    },
    {
      key: "unprotected",
      label: "Unprotected",
      count: counts.unprotected,
      icon: <ShieldOff className="size-5" />,
      tone: "muted",
    },
  ];
}

function countByStatus(items: BackupHealthItem[]): Record<BackupHealthStatus, number> {
  const counts: Record<BackupHealthStatus, number> = {
    protected: 0,
    stale: 0,
    failed: 0,
    unprotected: 0,
    in_flight: 0,
  };
  for (const item of items) {
    counts[item.status]++;
  }
  return counts;
}

// ---------------------------------------------------------------------------
// Run backup cell action
// ---------------------------------------------------------------------------

function RunBackupAction({ siteId }: { siteId: string }) {
  const mutation = useCreateBackup(siteId);
  const navigate = useNavigate();
  return (
    <button
      type="button"
      disabled={mutation.isPending}
      onClick={(e) => {
        e.stopPropagation();
        mutation.mutate(
          { kind: "full" },
          {
            onSuccess: () => toast.success("Backup started"),
            onError: (err) => {
              // 402 byo_destination_required (M16 Phase B backup-destinations
              // Phase 2): point straight at Destinations instead of a bare
              // error string — there's no room for the full
              // BackupDestinationRequiredPrompt in a table row action.
              if (err instanceof ByoDestinationRequiredError) {
                toast.error("This plan needs your own backup storage", {
                  description: err.message,
                  action: {
                    label: "Add destination",
                    onClick: () => void navigate({ to: "/destinations" }),
                  },
                });
                return;
              }
              toast.error(err.message);
            },
          },
        );
      }}
      className="inline-flex items-center gap-1 text-xs text-[var(--color-primary)] hover:underline disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
      aria-label="Run backup for this site"
    >
      <Play aria-hidden="true" className="size-3 shrink-0" />
      Run backup
    </button>
  );
}

// ---------------------------------------------------------------------------
// Row actions dropdown
// ---------------------------------------------------------------------------

function RowActions({ item }: { item: BackupHealthItem }) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7"
          aria-label={`Actions for ${item.site_name}`}
          onClick={(e) => e.stopPropagation()}
        >
          <MoreHorizontal aria-hidden="true" className="size-4" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuItem asChild>
          <Link
            to="/sites/$siteId/backups"
            params={{ siteId: item.site_id }}
            className="flex items-center gap-2"
          >
            <List aria-hidden="true" className="size-4" />
            Browse snapshots
          </Link>
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem
          onSelect={(e) => {
            e.preventDefault();
          }}
          className="flex items-center gap-2"
          disabled
        >
          <RotateCw aria-hidden="true" className="size-4" />
          Restore site
          <span className="ml-auto text-[10px] text-[var(--color-muted-foreground)]">
            Select snapshot first
          </span>
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

// ---------------------------------------------------------------------------
// Table columns
// ---------------------------------------------------------------------------

function buildColumns(): ColumnDef<BackupHealthItem>[] {
  return [
    {
      id: "site_name",
      header: "Site",
      accessorFn: (row) => row.site_name,
      meta: { width: "24%" },
      cell: ({ row }) => (
        <Link
          to="/sites/$siteId/backups"
          params={{ siteId: row.original.site_id }}
          className="font-medium text-[var(--color-foreground)] hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
        >
          {row.original.site_name || row.original.site_url}
        </Link>
      ),
    },
    {
      id: "status",
      header: "Status",
      accessorFn: (row) => row.status,
      meta: { width: "11%" },
      cell: ({ row }) => {
        const s = row.original.status;
        const Icon = STATUS_ICON[s];
        return (
          <span
            className={cn(
              "inline-flex items-center gap-1 text-xs",
              STATUS_COLOR_CLASS[s],
            )}
          >
            <Icon aria-hidden="true" className="size-3.5 shrink-0" />
            {STATUS_LABEL[s]}
          </span>
        );
      },
    },
    {
      id: "last_completed_at",
      header: "Last good backup",
      accessorFn: (row) =>
        row.last_completed_at ? Date.parse(row.last_completed_at) : -1,
      meta: { numeric: false, width: "15%" },
      cell: ({ row }) => {
        const iso = row.original.last_completed_at;
        if (!iso)
          return (
            <span className="text-xs text-[var(--color-muted-foreground)]">
              Never
            </span>
          );
        return (
          <span
            className={cn("text-xs font-medium tabular-nums", ageColor(iso))}
            title={new Date(iso).toLocaleString()}
          >
            {relativeTime(iso) ?? "--"}
          </span>
        );
      },
    },
    {
      id: "next_run_at",
      header: "Next scheduled",
      accessorFn: (row) =>
        row.next_run_at ? Date.parse(row.next_run_at) : Infinity,
      meta: { width: "14%" },
      cell: ({ row }) => {
        const iso = row.original.next_run_at;
        if (!iso)
          return (
            <span className="text-xs text-[var(--color-muted-foreground)]">
              Not scheduled
            </span>
          );
        return (
          <span
            className="text-xs text-[var(--color-muted-foreground)] tabular-nums"
            title={new Date(iso).toLocaleString()}
          >
            {relativeTime(iso) ?? "--"}
          </span>
        );
      },
    },
    {
      id: "latest_size_bytes",
      header: "Latest size",
      accessorFn: (row) => row.latest_size_bytes ?? -1,
      meta: { numeric: true, width: "10%" },
      cell: ({ row }) => (
        <span className="tabular-nums text-xs font-mono text-[var(--color-foreground)]">
          {formatBytes(row.original.latest_size_bytes)}
        </span>
      ),
    },
    {
      id: "size_trend",
      header: "Size trend",
      enableSorting: false,
      meta: { width: "10%" },
      cell: ({ row }) => {
        // The health endpoint does not yet provide a size series; render a
        // stub sparkline that shows a flat line for the single data point.
        // When the API adds size_history[] this will be wired up.
        const size = row.original.latest_size_bytes;
        const data = size !== null ? [size, size] : [];
        return (
          <SparklineCell
            data={data}
            tone="primary"
            width={48}
            height={18}
            ariaLabel={`Size trend for ${row.original.site_name}`}
          />
        );
      },
    },
    {
      id: "schedule_cadence",
      header: "Schedule",
      accessorFn: (row) => row.schedule_cadence ?? "",
      meta: { width: "8%" },
      cell: ({ row }) => {
        const c = row.original.schedule_cadence;
        return (
          <span className="text-xs text-[var(--color-muted-foreground)] capitalize">
            {c ?? "None"}
          </span>
        );
      },
    },
    {
      id: "actions",
      header: "",
      enableSorting: false,
      meta: { width: "8%" },
      cell: ({ row }) => (
        <div className="flex items-center justify-end gap-1">
          <RunBackupAction siteId={row.original.site_id} />
          <RowActions item={row.original} />
        </div>
      ),
    },
  ];
}

// ---------------------------------------------------------------------------
// Page
// ---------------------------------------------------------------------------

const COLUMNS = buildColumns();

function BackupsIndexPage() {
  const { activeKeys, toggle } = useFilterToggle();

  const {
    data,
    isPending,
    isError,
    error,
    refetch,
  } = useBackupHealth();

  const items = useMemo(() => data?.items ?? [], [data]);
  const counts = useMemo(() => countByStatus(items), [items]);
  const tiles = buildTiles(counts);

  // Filter by active tile keys.
  const filteredItems = useMemo(() => {
    if (activeKeys.size === 0) return items;
    return items.filter((item) => activeKeys.has(item.status));
  }, [items, activeKeys]);

  return (
    <section aria-labelledby="backups-heading" className="space-y-6">
      <PageHeader
        title="Backups"
        subline="Protection status and snapshot history across all connected sites"
      />

      {/* Status tiles */}
      <ExceptionSummaryTiles
        tiles={tiles}
        activeKeys={activeKeys}
        onToggle={toggle}
        loading={isPending}
      />

      {/* Site-level fleet table */}
      <section aria-labelledby="backups-table-heading" className="space-y-3">
        <div className="flex items-center gap-2 border-b border-[var(--color-border)] pb-3">
          <Database aria-hidden="true" className="size-4 shrink-0 text-[var(--color-muted-foreground)]" />
          <h2
            id="backups-table-heading"
            className="text-sm font-semibold text-[var(--color-foreground)]"
          >
            Sites
            {filteredItems.length !== items.length && (
              <span
                aria-live="polite"
                aria-atomic="true"
                className="ml-2 text-xs font-normal text-[var(--color-muted-foreground)]"
              >
                {filteredItems.length} of {items.length}
              </span>
            )}
          </h2>
        </div>

        <FleetTable<BackupHealthItem>
          data={filteredItems}
          columns={COLUMNS}
          height={Math.min(620, Math.max(200, filteredItems.length * 48 + 44))}
          loading={isPending}
          isError={isError}
          errorMessage={error?.message}
          onRetry={() => void refetch()}
          ariaLabel="Fleet backup status table"
          defaultSorting={[{ id: "last_completed_at", desc: false }]}
          emptyState={
            activeKeys.size > 0 ? (
              <div className="text-center">
                <p className="text-sm text-[var(--color-muted-foreground)]">
                  No sites match the selected filter.
                </p>
                <button
                  type="button"
                  onClick={() => activeKeys.forEach((k) => toggle(k))}
                  className="mt-2 text-sm text-[var(--color-primary)] hover:underline"
                >
                  Clear filter
                </button>
              </div>
            ) : (
              <div className="text-center">
                <Database
                  aria-hidden="true"
                  strokeWidth={1.5}
                  className="mx-auto mb-3 size-8 text-[var(--color-muted-foreground)]/40"
                />
                <p className="text-sm text-[var(--color-muted-foreground)]">
                  No sites connected yet.{" "}
                  <Link
                    to="/sites"
                    className="text-[var(--color-primary)] hover:underline"
                  >
                    Connect a site
                  </Link>{" "}
                  to manage backups.
                </p>
              </div>
            )
          }
        />
      </section>
    </section>
  );
}
