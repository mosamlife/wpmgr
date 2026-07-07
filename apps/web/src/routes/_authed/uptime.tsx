// /uptime — fleet-wide uptime overview.
//
// Layout:
//   1. ExceptionSummaryTiles (up/degraded/down/unknown) — filter toggles
//   2. StatusMatrix — at-a-glance hero: one cell per site, fill = status colour
//   3. FleetTable — row-per-site with DayBarStrip (90d), latency SparklineCell,
//      connection_state, TLS expiry
//   4. Incidents panel — open/closed cross-site incidents

import { useState, useMemo } from "react";
import { createFileRoute, Link } from "@tanstack/react-router";
import {
  CheckCircle2,
  AlertTriangle,
  XCircle,
  HelpCircle,
  Globe,
  ShieldCheck,
  AlertCircle,
  Clock,
  Timer,
} from "lucide-react";
import type { ColumnDef } from "@tanstack/react-table";

import { PageHeader } from "@/components/shared/page-header";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import {
  ExceptionSummaryTiles,
  useFilterToggle,
  type TileDefinition,
} from "@/features/fleet/ExceptionSummaryTiles";
import { StatusMatrix, type MatrixCell } from "@/features/fleet/StatusMatrix";
import { FleetTable } from "@/features/fleet/FleetTable";
import {
  DayBarStrip,
  type DayBarCell,
  type DayStatus,
} from "@/features/fleet/DayBarStrip";
import { SparklineCell } from "@/features/fleet/SparklineCell";
import {
  useFleetStatus,
  useFleetIncidents,
} from "@/features/fleet/use-fleet-uptime";
import {
  isIncidentOngoing,
  formatIncidentDuration,
} from "@/features/fleet/incident-format";
import type {
  FleetStatusItem,
  FleetIncident,
  UptimeStatusKind,
} from "@/features/fleet/fleet-types";
import { cn, relativeTime } from "@/lib/utils";
import { useNow } from "@/lib/use-now";

export const Route = createFileRoute("/_authed/uptime")({
  component: UptimePage,
});

// ---------------------------------------------------------------------------
// Status colour helpers — status by colour AND label AND icon (a11y)
// ---------------------------------------------------------------------------

const STATUS_ICON: Record<UptimeStatusKind, typeof CheckCircle2> = {
  up: CheckCircle2,
  degraded: AlertTriangle,
  down: XCircle,
  unknown: HelpCircle,
};

const STATUS_LABEL: Record<UptimeStatusKind, string> = {
  up: "Up",
  degraded: "Degraded",
  down: "Down",
  unknown: "Unknown",
};

const STATUS_COLOR_CLASS: Record<UptimeStatusKind, string> = {
  up: "text-[var(--color-success-subtle-fg)]",
  degraded: "text-[var(--color-warning-subtle-fg)]",
  down: "text-[var(--color-destructive-subtle-fg)]",
  unknown: "text-[var(--color-muted-foreground)]",
};

// ---------------------------------------------------------------------------
// Tile definitions
// ---------------------------------------------------------------------------

function buildTiles(
  up: number,
  degraded: number,
  down: number,
  unknown: number,
): TileDefinition[] {
  return [
    {
      key: "up",
      label: "Up",
      count: up,
      icon: <CheckCircle2 className="size-5" />,
      tone: "success",
    },
    {
      key: "degraded",
      label: "Degraded",
      count: degraded,
      icon: <AlertTriangle className="size-5" />,
      tone: "warning",
    },
    {
      key: "down",
      label: "Down",
      count: down,
      icon: <XCircle className="size-5" />,
      tone: "destructive",
    },
    {
      key: "unknown",
      label: "Unknown",
      count: unknown,
      icon: <HelpCircle className="size-5" />,
      tone: "muted",
    },
  ];
}

// ---------------------------------------------------------------------------
// Derive 90-day bar from uptime_pct_7d (placeholder until the backend provides
// a day-series on this endpoint; maps the scalar to a simplified strip)
// ---------------------------------------------------------------------------

function deriveStubDays(item: FleetStatusItem, count: number): DayBarCell[] {
  // The fleet/status endpoint returns a site status + uptime_pct_7d but not a
  // day-level series. Until the backend exposes /fleet/uptime-history we
  // render a stub strip using the current status and uptime_pct_7d as a
  // heuristic. This is declared in UI copy as "approximate" when heuristic.
  const today = new Date();
  return Array.from({ length: count }, (_, i) => {
    const d = new Date(today);
    d.setDate(d.getDate() - (count - 1 - i));
    const iso = d.toISOString().slice(0, 10);
    // For the most recent day use the current probe status. For prior days use
    // the uptime_pct_7d as a rough guide: ~green if pct >= 95, amber if 70-95,
    // red if < 70, unknown if null.
    const isToday = i === count - 1;
    let status: DayStatus;
    if (isToday) {
      status =
        item.status === "up"
          ? "up"
          : item.status === "degraded"
            ? "degraded"
            : item.status === "down"
              ? "incident"
              : "unknown";
    } else if (item.uptime_pct_7d === null) {
      status = "unknown";
    } else if (item.uptime_pct_7d >= 99) {
      status = "up";
    } else if (item.uptime_pct_7d >= 95) {
      status = i < count - 3 ? "up" : "degraded";
    } else {
      status = "incident";
    }
    return { date: iso, status };
  });
}

// ---------------------------------------------------------------------------
// TLS expiry badge
// ---------------------------------------------------------------------------

function TlsExpiry({ expiry }: { expiry: string | null }) {
  const now = useNow(60_000); // update every minute — TLS expiry doesn't need sub-minute
  if (!expiry) {
    return <span className="text-[var(--color-muted-foreground)]">--</span>;
  }
  const daysLeft = Math.ceil((Date.parse(expiry) - now) / 86_400_000);
  const isWarning = daysLeft <= 30;
  const isDanger = daysLeft <= 7;
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1 text-xs tabular-nums",
        isDanger
          ? "text-[var(--color-destructive-subtle-fg)]"
          : isWarning
            ? "text-[var(--color-warning-subtle-fg)]"
            : "text-[var(--color-muted-foreground)]",
      )}
      title={`TLS expires ${expiry}`}
    >
      <ShieldCheck aria-hidden="true" className="size-3 shrink-0" />
      {daysLeft}d
    </span>
  );
}

// ---------------------------------------------------------------------------
// Fleet table columns
// ---------------------------------------------------------------------------

function buildColumns(): ColumnDef<FleetStatusItem>[] {
  return [
    {
      id: "name",
      header: "Site",
      accessorFn: (row) => row.name,
      meta: { width: "28%" },
      cell: ({ row }) => (
        <Link
          to="/sites/$siteId"
          params={{ siteId: row.original.site_id }}
          className="font-medium text-[var(--color-foreground)] hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
        >
          {row.original.name || row.original.url}
        </Link>
      ),
    },
    {
      id: "status",
      header: "Status",
      accessorFn: (row) => row.status,
      meta: { width: "10%" },
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
      id: "uptime_pct_7d",
      header: "7d uptime",
      accessorFn: (row) => row.uptime_pct_7d ?? -1,
      meta: { numeric: true, width: "10%" },
      cell: ({ row }) => {
        const pct = row.original.uptime_pct_7d;
        if (pct === null)
          return (
            <span className="text-[var(--color-muted-foreground)]">--</span>
          );
        const cls =
          pct >= 99.9
            ? "text-[var(--color-success-subtle-fg)]"
            : pct >= 95
              ? "text-[var(--color-warning-subtle-fg)]"
              : "text-[var(--color-destructive-subtle-fg)]";
        return (
          <span className={cn("tabular-nums text-xs font-medium", cls)}>
            {pct.toFixed(2)}%
          </span>
        );
      },
    },
    {
      id: "latency",
      header: "Latency (avg)",
      accessorFn: (row) => row.avg_latency_ms ?? -1,
      meta: { numeric: true, width: "10%" },
      cell: ({ row }) => {
        const ms = row.original.avg_latency_ms;
        if (ms === null)
          return (
            <span className="text-[var(--color-muted-foreground)]">--</span>
          );
        const tone: "success" | "warning" | "destructive" =
          ms < 500 ? "success" : ms < 2000 ? "warning" : "destructive";
        return (
          <span
            className={cn(
              "tabular-nums text-xs",
              tone === "success"
                ? "text-[var(--color-success-subtle-fg)]"
                : tone === "warning"
                  ? "text-[var(--color-warning-subtle-fg)]"
                  : "text-[var(--color-destructive-subtle-fg)]",
            )}
          >
            {Math.round(ms)} ms
          </span>
        );
      },
    },
    {
      id: "latency_sparkline",
      header: "Latency trend",
      enableSorting: false,
      meta: { width: "10%" },
      cell: ({ row }) => {
        const data = row.original.latency_sparkline ?? [];
        const maxLatency = Math.max(...data.filter((v) => v > 0), 0);
        const tone: "success" | "warning" | "destructive" =
          maxLatency < 500 ? "success" : maxLatency < 2000 ? "warning" : "destructive";
        return (
          <SparklineCell
            data={data}
            tone={tone}
            width={56}
            height={20}
            ariaLabel={`Latency trend for ${row.original.name}`}
          />
        );
      },
    },
    {
      id: "history",
      header: "90d history (approx)",
      enableSorting: false,
      meta: { width: "20%" },
      cell: ({ row }) => {
        const days = deriveStubDays(row.original, 90);
        return <DayBarStrip days={days} cellW={4} cellH={18} />;
      },
    },
    {
      id: "tls_expiry",
      header: "TLS",
      accessorFn: (row) =>
        row.tls_expiry ? Date.parse(row.tls_expiry) : Infinity,
      meta: { numeric: true, width: "8%" },
      cell: ({ row }) => <TlsExpiry expiry={row.original.tls_expiry} />,
    },
    {
      id: "last_probe",
      header: "Last check",
      accessorFn: (row) =>
        row.last_probe_at ? Date.parse(row.last_probe_at) : -1,
      meta: { numeric: true, width: "10%" },
      cell: ({ row }) => (
        <span className="text-xs text-[var(--color-muted-foreground)]">
          {relativeTime(row.original.last_probe_at) ?? "--"}
        </span>
      ),
    },
  ];
}

// ---------------------------------------------------------------------------
// Incidents panel
// ---------------------------------------------------------------------------

function IncidentsPanel({
  loading,
  isError,
  incidents,
}: {
  loading: boolean;
  isError: boolean;
  incidents: FleetIncident[];
}) {
  const open = incidents.filter((i) => isIncidentOngoing(i));
  const closed = incidents.filter((i) => !isIncidentOngoing(i)).slice(0, 10);

  return (
    <section aria-labelledby="incidents-heading" className="space-y-3">
      <div className="flex items-center gap-2 border-b border-[var(--color-border)] pb-3">
        <AlertCircle aria-hidden="true" className="size-4 shrink-0 text-[var(--color-muted-foreground)]" />
        <h2 id="incidents-heading" className="text-sm font-semibold text-[var(--color-foreground)]">
          Incidents
        </h2>
      </div>

      {loading ? (
        <div className="space-y-2">
          {Array.from({ length: 3 }).map((_, i) => (
            <Skeleton key={i} className="h-10 w-full rounded" />
          ))}
        </div>
      ) : isError ? (
        <p className="text-sm text-[var(--color-muted-foreground)]">
          Could not load incident history.
        </p>
      ) : incidents.length === 0 ? (
        <div className="rounded-lg border border-[var(--color-border)] px-5 py-8 text-center">
          <CheckCircle2
            aria-hidden="true"
            strokeWidth={1.5}
            className="mx-auto mb-3 size-8 text-[var(--color-success-subtle-fg)]/60"
          />
          <p className="text-sm text-[var(--color-muted-foreground)]">
            No incidents recorded.
          </p>
        </div>
      ) : (
        <div className="space-y-4">
          {open.length > 0 && (
            <div>
              <p className="mb-2 text-xs font-medium text-[var(--color-destructive-subtle-fg)]">
                Ongoing ({open.length})
              </p>
              <IncidentList incidents={open} />
            </div>
          )}
          {closed.length > 0 && (
            <div>
              <p className="mb-2 text-xs font-medium text-[var(--color-muted-foreground)]">
                Recent resolved
              </p>
              <IncidentList incidents={closed} />
            </div>
          )}
        </div>
      )}
    </section>
  );
}

function IncidentList({ incidents }: { incidents: FleetIncident[] }) {
  return (
    <div
      role="list"
      aria-label="Incident list"
      className="divide-y divide-[var(--color-border)] rounded-lg border border-[var(--color-border)]"
    >
      {incidents.map((inc) => {
        // Ongoing decision is hardened off the reliable `ongoing` boolean
        // (falling back to a finite-duration check), NEVER off
        // `duration_seconds === null` alone — a missing/malformed duration
        // must never render as a fabricated "NaNh" (GH #148).
        const ongoing = isIncidentOngoing(inc);
        const durationLabel = formatIncidentDuration(inc.duration_seconds, ongoing);
        const startedLabel = `started ${relativeTime(inc.started_at) ?? inc.started_at}`;

        return (
          <div
            key={`${inc.site_id}-${inc.started_at}`}
            role="listitem"
            className="flex flex-wrap items-center gap-x-4 gap-y-1 px-4 py-2.5 text-xs"
          >
            <span
              className={cn(
                "inline-flex items-center gap-1 font-medium",
                ongoing
                  ? "text-[var(--color-destructive-subtle-fg)]"
                  : "text-[var(--color-muted-foreground)]",
              )}
            >
              {ongoing ? (
                <XCircle aria-hidden="true" className="size-3.5 shrink-0" />
              ) : (
                <CheckCircle2 aria-hidden="true" className="size-3.5 shrink-0" />
              )}
              {inc.kind === "down" ? "Down" : "Degraded"}
            </span>
            {/* Lightweight drill-in: reuse the same Link-to-detail pattern as
                the fleet Sites table's "Site" column (see buildColumns above)
                so the name/url stays the click target and is keyboard/a11y
                navigable — no modal (there's no persisted incident history
                to show one). */}
            <Link
              to="/sites/$siteId"
              params={{ siteId: inc.site_id }}
              className="font-medium text-[var(--color-foreground)] hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)]"
            >
              {inc.name || inc.url}
            </Link>
            {/* Both time values are labeled so they never read as two bare
                unlabeled timestamps (GH #148): "started X ago[, ongoing]"
                and, for resolved incidents only, "for Xh/Xm/Xs". */}
            <span className="inline-flex items-center gap-1 text-[var(--color-muted-foreground)]">
              <Clock aria-hidden="true" className="size-3 shrink-0" />
              {ongoing ? `${startedLabel}, ongoing` : startedLabel}
            </span>
            {!ongoing && durationLabel ? (
              <span className="ml-auto inline-flex items-center gap-1 tabular-nums text-[var(--color-muted-foreground)]">
                <Timer aria-hidden="true" className="size-3 shrink-0" />
                for {durationLabel}
              </span>
            ) : null}
          </div>
        );
      })}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Page
// ---------------------------------------------------------------------------

const COLUMNS = buildColumns();

function UptimePage() {
  const { activeKeys, toggle } = useFilterToggle();

  const {
    data: statusData,
    isPending: statusPending,
    isError: statusError,
    error: statusErr,
    refetch: statusRefetch,
  } = useFleetStatus();

  const {
    data: incidentsData,
    isPending: incidentsPending,
    isError: incidentsError,
  } = useFleetIncidents();

  const summary = statusData?.summary ?? {
    up: 0,
    degraded: 0,
    down: 0,
    unknown: 0,
  };

  const tiles = buildTiles(
    summary.up,
    summary.degraded,
    summary.down,
    summary.unknown,
  );

  const allItems = useMemo(
    () => statusData?.items ?? [],
    [statusData],
  );

  // Filter items by active tile keys.
  const filteredItems = useMemo(() => {
    if (activeKeys.size === 0) return allItems;
    return allItems.filter((item) => activeKeys.has(item.status));
  }, [allItems, activeKeys]);

  // Matrix cells — always show all sites (unfiltered), highlight selection.
  const [selectedSiteId, setSelectedSiteId] = useState<string | null>(null);

  const matrixCells: MatrixCell[] = useMemo(
    () =>
      allItems.map((item) => ({
        siteId: item.site_id,
        name: item.name,
        url: item.url,
        status: item.status,
        metricLabel:
          item.avg_latency_ms !== null
            ? `${Math.round(item.avg_latency_ms)} ms`
            : undefined,
      })),
    [allItems],
  );

  // When a matrix cell is selected, filter table to that site.
  const tableItems = useMemo(() => {
    if (selectedSiteId) {
      return filteredItems.filter((i) => i.site_id === selectedSiteId);
    }
    return filteredItems;
  }, [filteredItems, selectedSiteId]);

  return (
    <section aria-labelledby="uptime-heading" className="space-y-6">
      <PageHeader
        title="Uptime"
        subline="Real-time connection and probe status across all sites"
      />

      {/* Tile filter row */}
      <ExceptionSummaryTiles
        tiles={tiles}
        activeKeys={activeKeys}
        onToggle={(key) => {
          toggle(key);
          // Clicking a tile also clears matrix selection so filters compose cleanly.
          setSelectedSiteId(null);
        }}
        loading={statusPending}
      />

      {/* Status matrix hero */}
      {statusError ? (
        <PageError
          what="Could not load fleet uptime status."
          why={statusErr?.message ?? "Unknown error"}
          onRetry={() => void statusRefetch()}
          retryLabel="Reload uptime data"
        />
      ) : (
        <section aria-labelledby="matrix-heading" className="space-y-2">
          <div className="flex items-baseline justify-between">
            <h2
              id="matrix-heading"
              className="text-xs font-medium text-[var(--color-muted-foreground)]"
            >
              All sites at a glance
              {selectedSiteId && (
                <button
                  type="button"
                  onClick={() => setSelectedSiteId(null)}
                  className="ml-3 text-[var(--color-primary)] hover:underline focus-visible:outline-none"
                >
                  Clear selection
                </button>
              )}
            </h2>
            <span className="text-xs text-[var(--color-muted-foreground)]">
              <span className="inline-flex items-center gap-1.5">
                <span className="inline-block size-2.5 rounded-sm bg-[var(--color-success)]" />
                Up
              </span>
              <span className="ml-3 inline-flex items-center gap-1.5">
                <span className="inline-block size-2.5 rounded-sm bg-[var(--color-warning)]" />
                Degraded
              </span>
              <span className="ml-3 inline-flex items-center gap-1.5">
                <span className="inline-block size-2.5 rounded-sm bg-[var(--color-destructive)]" />
                Down
              </span>
              <span className="ml-3 inline-flex items-center gap-1.5">
                <span className="inline-block size-2.5 rounded-sm bg-[var(--color-muted-foreground)]/40" />
                Unknown
              </span>
            </span>
          </div>
          <div className="rounded-lg border border-[var(--color-border)] bg-[var(--color-card)] p-4">
            <StatusMatrix
              cells={matrixCells}
              selectedSiteId={selectedSiteId}
              onSelect={(id) =>
                setSelectedSiteId((prev) => (prev === id ? null : id))
              }
              loading={statusPending}
              cellSize={14}
            />
          </div>
        </section>
      )}

      {/* Fleet table */}
      <section aria-labelledby="uptime-table-heading" className="space-y-3">
        <div className="flex items-center gap-2 border-b border-[var(--color-border)] pb-3">
          <Globe aria-hidden="true" className="size-4 shrink-0 text-[var(--color-muted-foreground)]" />
          <h2
            id="uptime-table-heading"
            className="text-sm font-semibold text-[var(--color-foreground)]"
          >
            Sites
            {filteredItems.length !== allItems.length && (
              <span
                aria-live="polite"
                aria-atomic="true"
                className="ml-2 text-xs font-normal text-[var(--color-muted-foreground)]"
              >
                {filteredItems.length} of {allItems.length}
              </span>
            )}
          </h2>
        </div>

        <FleetTable<FleetStatusItem>
          data={tableItems}
          columns={COLUMNS}
          height={Math.min(600, Math.max(200, tableItems.length * 48 + 44))}
          loading={statusPending}
          isError={statusError}
          errorMessage={statusErr?.message}
          onRetry={() => void statusRefetch()}
          ariaLabel="Fleet uptime table"
          defaultSorting={[{ id: "status", desc: true }]}
          emptyState={
            activeKeys.size > 0 || selectedSiteId ? (
              <div className="text-center">
                <p className="text-sm text-[var(--color-muted-foreground)]">
                  No sites match the current filter.
                </p>
                <button
                  type="button"
                  onClick={() => setSelectedSiteId(null)}
                  className="mt-2 text-sm text-[var(--color-primary)] hover:underline"
                >
                  Clear filter
                </button>
              </div>
            ) : (
              <div className="text-center">
                <Globe
                  aria-hidden="true"
                  strokeWidth={1.5}
                  className="mx-auto mb-3 size-8 text-[var(--color-muted-foreground)]/40"
                />
                <p className="text-sm text-[var(--color-muted-foreground)]">
                  No sites connected yet.{" "}
                  <Link to="/sites" className="text-[var(--color-primary)] hover:underline">
                    Add a site
                  </Link>{" "}
                  to start monitoring.
                </p>
              </div>
            )
          }
        />
      </section>

      {/* Incidents */}
      <IncidentsPanel
        loading={incidentsPending}
        isError={incidentsError}
        incidents={incidentsData?.items ?? []}
      />
    </section>
  );
}
