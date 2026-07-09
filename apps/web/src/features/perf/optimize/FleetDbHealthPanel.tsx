// FleetDbHealthPanel — P3.7 Portfolio DB Health.
//
// Displays a tenant-level aggregate of database health across ALL of the
// tenant's sites. Powered by GET /api/v1/perf/db/fleet-health via the
// useFleetDbHealth hook.
//
// This panel is READ-ONLY. No mutations, no delete controls.
// Placement: apps/web/src/routes/_authed/performance.tsx (Insights > Performance),
// which is the natural portfolio/fleet home for cross-site rollups.

import { useState } from "react";
import {
  AlertTriangle,
  ChevronDown,
  Database,
  TrendingDown,
  TrendingUp,
} from "lucide-react";
import { Link } from "@tanstack/react-router";

import { cn } from "@/lib/utils";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";

import { useFleetDbHealth } from "../hooks/useFleetDbHealth";
import { formatBytes, formatCount } from "../format";
import type { FleetDbSiteNeedingReview, FleetDbTopSite } from "../types";

const SITES_NEEDING_REVIEW_LIST_ID = "fleet-sites-needing-review-list";

export function FleetDbHealthPanel() {
  const { data, isPending, isError, error, refetch } = useFleetDbHealth();
  const [needingReviewExpanded, setNeedingReviewExpanded] = useState(false);

  return (
    <section
      aria-labelledby="fleet-db-health-heading"
      className="rounded-xl border border-border bg-card text-card-foreground shadow-sm"
    >
      {/* ── Header ──────────────────────────────────────────────────────── */}
      <div className="flex items-center gap-3 border-b border-border px-5 py-3">
        <Database
          aria-hidden="true"
          className="size-4 shrink-0 text-muted-foreground"
        />
        <div className="min-w-0 flex-1">
          <h2
            id="fleet-db-health-heading"
            className="text-sm font-semibold text-foreground"
          >
            Database health across your sites
          </h2>
          <p className="mt-0.5 text-xs text-muted-foreground">
            Aggregated from the latest per-site scan results — read-only
          </p>
        </div>
      </div>

      {/* ── Body ────────────────────────────────────────────────────────── */}
      {isPending ? (
        <FleetDbHealthSkeleton />
      ) : isError ? (
        <div className="px-5 py-6">
          <PageError
            what="Could not load fleet database health."
            why={error?.message ?? "Unknown error"}
            onRetry={() => void refetch()}
            retryLabel="Reload fleet health"
          />
        </div>
      ) : data.total_sites_scanned === 0 ? (
        <FleetDbHealthEmpty />
      ) : (
        <>
          {/* Stat strip */}
          <div
            aria-label="Fleet database summary"
            className="flex flex-wrap items-center gap-x-8 gap-y-3 border-b border-border px-5 py-4"
          >
            <FleetStat
              label="Sites with scan data"
              value={formatCount(data.total_sites_scanned)}
            />
            <FleetStat
              label="Total DB size"
              value={formatBytes(data.total_db_size_bytes)}
            />
            <FleetStat
              label="Total tables"
              value={formatCount(data.total_table_count)}
            />
            <FleetStat
              label="Total orphaned items"
              value={formatCount(data.total_orphaned_options + data.total_orphaned_cron)}
              hint="Options and scheduled-task entries attributed to uninstalled plugins across all sites"
            />
            {data.sites_with_orphans > 0 && (
              <FleetStat
                label="Sites with items to review"
                value={formatCount(data.sites_with_orphans)}
                highlight="amber"
                expanded={needingReviewExpanded}
                controls={SITES_NEEDING_REVIEW_LIST_ID}
                onToggle={() => setNeedingReviewExpanded((v) => !v)}
              />
            )}
          </div>

          {/* Sites needing review — drill-down disclosure */}
          {needingReviewExpanded && (data.sites_needing_review?.length ?? 0) > 0 && (
            <SitesNeedingReviewList
              id={SITES_NEEDING_REVIEW_LIST_ID}
              sites={data.sites_needing_review ?? []}
            />
          )}

          {/* Top sites table */}
          {data.top_sites.length > 0 && (
            <TopSitesTable sites={data.top_sites} />
          )}
        </>
      )}
    </section>
  );
}

// ---------------------------------------------------------------------------
// Stat tile
// ---------------------------------------------------------------------------

interface FleetStatProps {
  label: string;
  value: string;
  hint?: string;
  highlight?: "amber";
  /** When set (together with `onToggle`), the stat renders as an expandable
   *  disclosure button rather than inert text. */
  expanded?: boolean;
  /** id of the region this stat's disclosure controls, wired via
   *  `aria-controls`. Required alongside `onToggle`. */
  controls?: string;
  onToggle?: () => void;
}

function FleetStat({
  label,
  value,
  hint,
  highlight,
  expanded,
  controls,
  onToggle,
}: FleetStatProps) {
  const valueEl = (
    <span
      className={`tabular-nums text-xl font-semibold leading-none ${
        highlight === "amber"
          ? "text-amber-700 dark:text-amber-400"
          : "text-foreground"
      }`}
    >
      {value}
    </span>
  );

  const labelEl = (
    <span className="text-xs text-muted-foreground" title={hint}>
      {label}
      {hint && (
        <AlertTriangle
          aria-label={hint}
          className="ml-1 inline size-3 cursor-help text-muted-foreground/50"
        />
      )}
    </span>
  );

  if (onToggle) {
    return (
      <button
        type="button"
        onClick={onToggle}
        aria-expanded={expanded ?? false}
        aria-controls={controls}
        className="flex flex-col gap-0.5 rounded-sm text-left transition-colors hover:opacity-80 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
      >
        <span className="inline-flex items-center gap-1">
          {valueEl}
          <ChevronDown
            aria-hidden="true"
            className={cn(
              "size-3.5 shrink-0 text-muted-foreground transition-transform",
              expanded && "rotate-180",
            )}
          />
        </span>
        {labelEl}
      </button>
    );
  }

  return (
    <div className="flex flex-col gap-0.5">
      {valueEl}
      {labelEl}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Sites needing review — drill-down disclosure for the amber stat (GH #197)
// ---------------------------------------------------------------------------

function SitesNeedingReviewList({
  id,
  sites,
}: {
  id: string;
  sites: FleetDbSiteNeedingReview[];
}) {
  return (
    <div
      id={id}
      className="border-b border-border px-5 py-4"
    >
      <p className="mb-3 text-xs font-medium uppercase tracking-[0.02em] text-muted-foreground">
        Sites with items to review
      </p>
      <ul className="space-y-2.5">
        {sites.map((site) => {
          const total = site.orphaned_options_count + site.orphaned_cron_count;
          return (
            <li
              key={site.site_id}
              className="flex items-center justify-between gap-3"
            >
              <Link
                to="/sites/$siteId/optimize"
                params={{ siteId: site.site_id }}
                className="min-w-0 flex-1 truncate text-sm font-medium text-foreground hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
              >
                {site.site_name}
              </Link>
              <span className="shrink-0 text-xs tabular-nums text-amber-700 dark:text-amber-400">
                {formatCount(total)} item{total === 1 ? "" : "s"}
              </span>
            </li>
          );
        })}
      </ul>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Top-N sites table
// ---------------------------------------------------------------------------

function TopSitesTable({ sites }: { sites: FleetDbTopSite[] }) {
  return (
    <div className="px-5 py-4">
      <p className="mb-3 text-xs font-medium uppercase tracking-[0.02em] text-muted-foreground">
        Largest databases
      </p>
      <div
        role="table"
        aria-label="Largest site databases"
        className="w-full overflow-x-auto"
      >
        {/* Header */}
        <div
          role="row"
          className="flex items-center gap-2 border-b border-border pb-2 text-xs font-medium text-muted-foreground"
        >
          <span role="columnheader" className="min-w-0 flex-1">
            Site
          </span>
          <span
            role="columnheader"
            className="w-24 shrink-0 text-right"
          >
            DB size
          </span>
          <span
            role="columnheader"
            className="w-28 shrink-0 text-right"
          >
            90-day growth
          </span>
        </div>

        {/* Rows */}
        <div role="rowgroup">
          {sites.map((site) => (
            <TopSiteRow key={site.site_id} site={site} />
          ))}
        </div>
      </div>
    </div>
  );
}

function TopSiteRow({ site }: { site: FleetDbTopSite }) {
  const hasGrowth = site.growth_bytes !== 0;
  const growing = site.growth_bytes > 0;

  return (
    <div
      role="row"
      className="flex items-center gap-2 border-b border-border py-2.5 last:border-0"
    >
      {/* Site name — link to site's Health tab */}
      <span role="cell" className="min-w-0 flex-1 truncate">
        <Link
          to="/sites/$siteId/optimize"
          params={{ siteId: site.site_id }}
          className="text-sm font-medium text-foreground hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
        >
          {site.site_name}
        </Link>
      </span>

      {/* DB size */}
      <span
        role="cell"
        className="w-24 shrink-0 text-right font-mono text-sm tabular-nums text-foreground"
      >
        {formatBytes(site.db_size_bytes)}
      </span>

      {/* Growth badge */}
      <span role="cell" className="w-28 shrink-0 text-right">
        {hasGrowth ? (
          <span
            className={`inline-flex items-center justify-end gap-1 text-xs tabular-nums ${
              growing
                ? "text-amber-700 dark:text-amber-400"
                : "text-green-700 dark:text-green-400"
            }`}
          >
            {growing ? (
              <TrendingUp aria-hidden="true" className="size-3 shrink-0" />
            ) : (
              <TrendingDown aria-hidden="true" className="size-3 shrink-0" />
            )}
            {growing ? "+" : ""}
            {formatBytes(Math.abs(site.growth_bytes))}
          </span>
        ) : (
          <span className="text-xs text-muted-foreground">No change</span>
        )}
      </span>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Loading skeleton
// ---------------------------------------------------------------------------

function FleetDbHealthSkeleton() {
  return (
    <div
      role="status"
      aria-busy="true"
      aria-label="Loading fleet database health"
      className="space-y-3 px-5 py-4"
    >
      <span className="sr-only">Loading fleet database health</span>
      {/* Stat strip skeleton */}
      <div className="flex flex-wrap gap-x-8 gap-y-3 border-b border-border pb-4">
        {Array.from({ length: 4 }).map((_, i) => (
          <div key={i} className="flex flex-col gap-1">
            <Skeleton className="h-6 w-16 rounded" />
            <Skeleton className="h-3 w-24 rounded" />
          </div>
        ))}
      </div>
      {/* Table skeleton */}
      <div className="space-y-2 pt-1">
        <Skeleton className="h-3 w-28 rounded" />
        {Array.from({ length: 5 }).map((_, i) => (
          <div
            key={i}
            className="flex items-center gap-2 border-b border-border py-2.5 last:border-0"
          >
            <Skeleton className="h-3.5 flex-1 rounded" />
            <Skeleton className="h-3.5 w-20 rounded" />
            <Skeleton className="h-3.5 w-24 rounded" />
          </div>
        ))}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Empty state — no sites have been scanned yet
// ---------------------------------------------------------------------------

function FleetDbHealthEmpty() {
  return (
    <div className="flex flex-col items-center gap-3 px-5 py-10 text-center">
      <Database
        aria-hidden="true"
        strokeWidth={1.5}
        className="size-8 text-muted-foreground/40"
      />
      <p className="max-w-xs text-sm text-muted-foreground">
        No database scans have run yet. Open any site's{" "}
        <Link
          to="/sites"
          className="font-medium text-foreground underline underline-offset-4 hover:text-foreground/80 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
        >
          Optimize
        </Link>{" "}
        tab and run a scan to see data here.
      </p>
    </div>
  );
}
