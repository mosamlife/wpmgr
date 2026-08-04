// /performance, the fleet-wide performance dashboard.
//
// Scope selector: "All sites" shows the fleet aggregate; selecting a specific
// site shows the full per-site RUM detail (CWV cards + trend + URL breakdown).
// The ?site=<siteId> search param persists the scope alongside ?device and
// ?window, so links are shareable and survive refresh.
//
// Fleet scope layout:
//   1. Headline strip: sites reporting / fleet CWV pass-rate
//   2. Sites by Core Web Vitals FleetTable (all reporting sites, sorted LCP p75
//      worst-first). Rows are clickable: click sets ?site=<id>.
//   3. Fleet 28-day CWV trend (recharts, threshold reference lines)
//   4. DB health aggregate (existing FleetDbHealthPanel)
//
// Per-site scope layout (composes existing components unchanged):
//   1. Back breadcrumb / "Viewing: <site name>" sub-header
//   2. FleetRumPanel: CWV p75 cards + distribution bars + trend charts (reused)
//   3. RumResultsTable: per-URL/device breakdown table (reused)
//
// Device and window are URL search params (shareable links). The device tab
// also feeds the per-site RUM views (FleetRumPanel manages its own device state
// internally, so the outer device param is the initial/default for the fleet
// trend only; per-site panels have their own tab).

import { useCallback, useId } from "react";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { z } from "zod";
import {
  AreaChart,
  Area,
  CartesianGrid,
  ReferenceLine,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import {
  Activity,
  CheckCircle2,
  AlertCircle,
  XCircle,
  Monitor,
  Smartphone,
  Tablet,
  ChevronLeft,
} from "lucide-react";
import type { ColumnDef } from "@tanstack/react-table";

import { PageHeader } from "@/components/shared/page-header";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import { FleetDbHealthPanel } from "@/features/perf/optimize/FleetDbHealthPanel";
import { FleetRumPanel } from "@/features/perf/optimize/FleetRumPanel";
import { RumResultsTable } from "@/features/perf/optimize/RumResultsTable";
import { RumDistributionBar } from "@/features/perf/optimize/RumDistributionBar";
import { FleetTable } from "@/features/fleet/FleetTable";
import type { FleetRumOffender } from "@/features/fleet/fleet-types";
import {
  useFleetRum,
  DEFAULT_WINDOW_DAYS,
  type DeviceFilter,
} from "@/features/fleet/use-fleet-rum";
import { useSites } from "@/features/sites/use-sites";
import {
  cwvDataMax,
  cwvThresholdLabel,
  cwvYAxis,
} from "@/features/perf/cwv-axis";
import { cn } from "@/lib/utils";

// ---------------------------------------------------------------------------
// Route: validate search params so device + window + site are shareable
// ---------------------------------------------------------------------------

const searchSchema = z.object({
  device: z.enum(["all", "desktop", "mobile", "tablet"]).optional().default("all"),
  window: z.coerce.number().min(1).max(365).optional().default(DEFAULT_WINDOW_DAYS),
  site: z.string().optional(),
});

export const Route = createFileRoute("/_authed/performance")({
  validateSearch: searchSchema,
  component: PerformancePage,
});

// ---------------------------------------------------------------------------
// CWV formatting helpers
// ---------------------------------------------------------------------------

function fmtP75(metric: string, value: number | null): string {
  if (value === null) return "--";
  if (metric === "cls") return (value / 1000).toFixed(3);
  if (value >= 1000) return `${(value / 1000).toFixed(2)} s`;
  return `${Math.round(value)} ms`;
}

const METRIC_LABELS: Record<string, string> = {
  lcp: "LCP",
  inp: "INP",
  cls: "CLS",
  fcp: "FCP",
  ttfb: "TTFB",
};

// Thresholds, the Y domain, the ticks and the unit all come from
// features/perf/cwv-axis.ts (GH #329). Do not reintroduce a local copy: the
// duplicated formatter is exactly how this chart and RumTrendChart drifted
// into stating two different Web Vitals targets.

const CHART_TOKEN: Record<string, string> = {
  lcp: "var(--color-chart-1)",
  inp: "var(--color-chart-4)",
  cls: "var(--color-chart-5)",
};

// ---------------------------------------------------------------------------
// Device tab selector
// ---------------------------------------------------------------------------

const DEVICE_OPTIONS: Array<{ value: DeviceFilter; label: string; icon: typeof Monitor }> = [
  { value: "all", label: "All", icon: Monitor },
  { value: "desktop", label: "Desktop", icon: Monitor },
  { value: "mobile", label: "Mobile", icon: Smartphone },
  { value: "tablet", label: "Tablet", icon: Tablet },
];

interface DeviceTabsProps {
  value: DeviceFilter;
  onChange: (v: DeviceFilter) => void;
}

function DeviceTabs({ value, onChange }: DeviceTabsProps) {
  return (
    <div
      role="group"
      aria-label="Filter by device"
      className="inline-flex items-center rounded-md border border-[var(--color-border)] text-xs"
    >
      {DEVICE_OPTIONS.map((opt) => {
        const Icon = opt.icon;
        const isActive = value === opt.value;
        return (
          <button
            key={opt.value}
            type="button"
            aria-pressed={isActive}
            aria-label={`${opt.label} devices`}
            onClick={() => onChange(opt.value)}
            className={cn(
              "inline-flex items-center gap-1 px-2.5 py-1 first:rounded-l-md last:rounded-r-md",
              "transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] focus-visible:ring-offset-1",
              isActive
                ? "bg-[var(--color-foreground)] text-[var(--color-background)]"
                : "bg-transparent text-[var(--color-muted-foreground)] hover:text-[var(--color-foreground)]",
            )}
          >
            <Icon aria-hidden="true" className="size-3" />
            {opt.label}
          </button>
        );
      })}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Site scope selector
// ---------------------------------------------------------------------------

interface SiteScopeSelectorProps {
  value: string | undefined;
  onChange: (siteId: string | undefined) => void;
}

function SiteScopeSelector({ value, onChange }: SiteScopeSelectorProps) {
  const selectId = useId();
  const { data: sites = [], isPending } = useSites();

  return (
    <div className="flex items-center gap-2">
      <label
        htmlFor={selectId}
        className="shrink-0 text-xs text-[var(--color-muted-foreground)]"
      >
        Site
      </label>
      <select
        id={selectId}
        value={value ?? ""}
        onChange={(e) => {
          const v = e.target.value;
          onChange(v === "" ? undefined : v);
        }}
        disabled={isPending}
        aria-label="Filter by site"
        className={cn(
          "h-8 min-w-[160px] max-w-[240px] appearance-none rounded-md border border-[var(--color-border)]",
          "bg-[var(--color-background)] px-2.5 py-1 text-xs text-[var(--color-foreground)]",
          "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] focus-visible:ring-offset-1",
          "disabled:cursor-not-allowed disabled:opacity-50",
        )}
      >
        <option value="">All sites</option>
        {sites.map((s) => (
          <option key={s.id} value={s.id}>
            {s.name || s.url}
          </option>
        ))}
      </select>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Headline strip
// ---------------------------------------------------------------------------

interface HeadlineStripProps {
  sitesReporting: number;
  sitesTotal: number;
  fleetPassPct: number | null;
  loading: boolean;
}

function HeadlineStrip({
  sitesReporting,
  sitesTotal,
  fleetPassPct,
  loading,
}: HeadlineStripProps) {
  if (loading) {
    return (
      <div className="flex flex-wrap gap-x-8 gap-y-3">
        {Array.from({ length: 3 }).map((_, i) => (
          <div key={i} className="flex flex-col gap-1">
            <Skeleton className="h-7 w-16 rounded" />
            <Skeleton className="h-3 w-24 rounded" />
          </div>
        ))}
      </div>
    );
  }

  const PassIcon =
    fleetPassPct === null
      ? AlertCircle
      : fleetPassPct >= 50
        ? CheckCircle2
        : XCircle;

  const passColor =
    fleetPassPct === null
      ? "text-[var(--color-muted-foreground)]"
      : fleetPassPct >= 50
        ? "text-[var(--color-success-subtle-fg)]"
        : "text-[var(--color-destructive-subtle-fg)]";

  return (
    <div
      className="flex flex-wrap items-center gap-x-8 gap-y-3"
      aria-label="Fleet RUM summary"
    >
      <div className="flex flex-col gap-0.5">
        <span className="text-2xl font-semibold tabular-nums leading-none text-[var(--color-foreground)]">
          {sitesReporting}
          <span className="ml-1 text-sm font-normal text-[var(--color-muted-foreground)]">
            / {sitesTotal}
          </span>
        </span>
        <span className="text-xs text-[var(--color-muted-foreground)]">
          Sites reporting RUM
        </span>
      </div>

      <div className="flex flex-col gap-0.5">
        <span className={cn("flex items-center gap-1 text-2xl font-semibold tabular-nums leading-none", passColor)}>
          <PassIcon aria-hidden="true" className="size-5 shrink-0" />
          {fleetPassPct === null ? "--" : `${Math.round(fleetPassPct)}%`}
        </span>
        <span className="text-xs text-[var(--color-muted-foreground)]">
          Fleet CWV pass rate
        </span>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// All-sites CWV table columns
// ---------------------------------------------------------------------------

function buildSiteColumns(
  windowDays: number,
  onRowClick: (row: FleetRumOffender) => void,
): ColumnDef<FleetRumOffender>[] {
  return [
    {
      id: "name",
      header: "Site",
      accessorFn: (row) => row.name,
      meta: { width: "28%" },
      cell: ({ row }) => (
        <button
          type="button"
          onClick={() => onRowClick(row.original)}
          className="font-medium text-[var(--color-foreground)] hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] focus-visible:ring-offset-1 text-left"
        >
          {row.original.name || row.original.url}
        </button>
      ),
    },
    {
      id: "lcp_p75",
      header: "LCP p75",
      accessorFn: (row) => row.lcp_p75 ?? -Infinity,
      meta: { numeric: true, width: "10%" },
      cell: ({ row }) => (
        <span className="tabular-nums font-mono text-xs">
          {fmtP75("lcp", row.original.lcp_p75)}
        </span>
      ),
    },
    {
      id: "inp_p75",
      header: "INP p75",
      accessorFn: (row) => row.inp_p75 ?? -Infinity,
      meta: { numeric: true, width: "10%" },
      cell: ({ row }) => (
        <span className="tabular-nums font-mono text-xs">
          {fmtP75("inp", row.original.inp_p75)}
        </span>
      ),
    },
    {
      id: "cls_p75",
      header: "CLS p75",
      accessorFn: (row) => row.cls_p75 ?? -Infinity,
      meta: { numeric: true, width: "10%" },
      cell: ({ row }) => (
        <span className="tabular-nums font-mono text-xs">
          {fmtP75("cls", row.original.cls_p75)}
        </span>
      ),
    },
    {
      id: "overall_rating",
      header: "Rating",
      accessorFn: (row) => row.overall_rating,
      meta: { width: "12%" },
      cell: ({ row }) => {
        const r = row.original.overall_rating;
        const cls =
          r === "good"
            ? "text-[var(--color-success-subtle-fg)] bg-[var(--color-success-subtle)]"
            : r === "needs-improvement"
              ? "text-[var(--color-warning-subtle-fg)] bg-[var(--color-warning-subtle)]"
              : "text-[var(--color-destructive-subtle-fg)] bg-[var(--color-destructive-subtle)]";
        const label =
          r === "good" ? "Good" : r === "needs-improvement" ? "Needs work" : "Poor";
        return (
          <span className={cn("rounded px-1.5 py-0.5 text-xs font-medium", cls)}>
            {label}
          </span>
        );
      },
    },
    {
      id: "sample_count",
      header: "Samples",
      accessorFn: (row) => row.sample_count,
      meta: { numeric: true, width: "10%" },
      cell: ({ row }) => (
        <span className="tabular-nums text-xs text-[var(--color-muted-foreground)]">
          {row.original.sample_count.toLocaleString()}
        </span>
      ),
    },
    {
      id: "distribution",
      header: `${windowDays}d distribution`,
      enableSorting: false,
      meta: { width: "20%" },
      cell: ({ row }) => {
        const r = row.original.overall_rating;
        const goodPct = r === "good" ? 75 : r === "needs-improvement" ? 40 : 10;
        const poorPct = r === "poor" ? 60 : r === "needs-improvement" ? 15 : 5;
        const dist = {
          good: 0, needs_improvement: 0, poor: 0,
          good_pct: goodPct,
          needs_improvement_pct: 100 - goodPct - poorPct,
          poor_pct: poorPct,
        };
        return (
          <RumDistributionBar
            metricLabel="Overall"
            distribution={dist}
          />
        );
      },
    },
  ];
}

// ---------------------------------------------------------------------------
// Fleet CWV trend chart (28-day)
// ---------------------------------------------------------------------------

type TrendMetric = "lcp" | "inp" | "cls";
const TREND_METRICS: TrendMetric[] = ["lcp", "inp", "cls"];

interface TrendPoint {
  date: string;
  lcp_p75: number | null;
  inp_p75: number | null;
  cls_p75: number | null;
}

interface FleetCwvTrendChartProps {
  trend: TrendPoint[];
  metric: TrendMetric;
  windowDays: number;
}

function shortDate(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

function FleetCwvTrendChart({ trend, metric, windowDays }: FleetCwvTrendChartProps) {
  const keyMap: Record<TrendMetric, "lcp_p75" | "inp_p75" | "cls_p75"> = {
    lcp: "lcp_p75",
    inp: "inp_p75",
    cls: "cls_p75",
  };
  const key = keyMap[metric];
  const stroke = CHART_TOKEN[metric]!;
  const isCls = metric === "cls";

  const chartData = trend.map((p) => {
    const raw = p[key];
    const display = raw === null ? null : isCls ? raw / 1000 : raw;
    return { date: p.date, value: display };
  });

  // One scale, one explicit domain, one owner of the unit string. The explicit
  // domain is what keeps the Good line on screen: with the recharts default,
  // ReferenceLines outside the auto domain are silently discarded, so a
  // passing site saw no thresholds at all.
  const axis = cwvYAxis(metric, cwvDataMax(chartData.map((p) => p.value)));

  const hasData = chartData.some((p) => p.value !== null);
  if (!hasData) {
    return (
      <p className="text-center text-xs text-[var(--color-muted-foreground)] py-8">
        No {METRIC_LABELS[metric]} data for this window.
      </p>
    );
  }

  return (
    <div>
      <p className="mb-2 text-xs font-semibold text-[var(--color-muted-foreground)]">
        {METRIC_LABELS[metric]}: {windowDays}-day p75 trend
      </p>
      <ResponsiveContainer width="100%" height={120}>
        <AreaChart
          data={chartData}
          margin={{ top: 4, right: 4, bottom: 0, left: 0 }}
        >
          <defs>
            <linearGradient id={`grad-${metric}`} x1="0" y1="0" x2="0" y2="1">
              <stop offset="5%" stopColor={stroke} stopOpacity={0.2} />
              <stop offset="95%" stopColor={stroke} stopOpacity={0} />
            </linearGradient>
          </defs>
          <CartesianGrid
            strokeDasharray="3 3"
            stroke="var(--color-border)"
            vertical={false}
          />
          <XAxis
            dataKey="date"
            tickFormatter={shortDate}
            tick={{ fontSize: 10, fill: "var(--color-muted-foreground)" }}
            tickLine={false}
            axisLine={false}
            interval="preserveStartEnd"
          />
          {/* No `unit` prop, ever: recharts appends it to the formatter's own
              output. See components/charts/no-axis-unit.test.ts. */}
          <YAxis
            tick={{ fontSize: 10, fill: "var(--color-muted-foreground)" }}
            tickLine={false}
            axisLine={false}
            width={axis.width}
            domain={axis.domain}
            ticks={axis.ticks}
            tickFormatter={axis.tick}
          />
          <Tooltip
            contentStyle={{
              background: "var(--color-card)",
              border: "1px solid var(--color-border)",
              borderRadius: 6,
              fontSize: 12,
              color: "var(--color-foreground)",
            }}
            // Per-value precision, NOT the axis scale. A tooltip shows one
            // number and must show it at the precision that number deserves;
            // the axis is permanently on the seconds scale for LCP, so sharing
            // it would round every reading to 100 ms. See cwv-axis.ts.
            formatter={(value: unknown) => {
              const v = typeof value === "number" ? value : 0;
              return isCls ? v.toFixed(3) : `${Math.round(v)} ms`;
            }}
            labelFormatter={(label: unknown) =>
              typeof label === "string" ? shortDate(label) : String(label)
            }
          />
          {/* Good threshold */}
          <ReferenceLine
            y={axis.thresholds.good}
            stroke="var(--color-success)"
            strokeDasharray="4 2"
            strokeWidth={1}
            label={{
              value: cwvThresholdLabel(axis, "good"),
              fontSize: 9,
              fill: "var(--color-success-subtle-fg)",
              position: "insideTopRight",
            }}
          />
          {/* Needs-improvement threshold */}
          <ReferenceLine
            y={axis.thresholds.ni}
            stroke="var(--color-warning)"
            strokeDasharray="4 2"
            strokeWidth={1}
            label={{
              value: cwvThresholdLabel(axis, "ni"),
              fontSize: 9,
              fill: "var(--color-warning-subtle-fg)",
              position: "insideTopRight",
            }}
          />
          <Area
            type="monotone"
            dataKey="value"
            stroke={stroke}
            strokeWidth={1.5}
            fill={`url(#grad-${metric})`}
            dot={false}
            activeDot={{ r: 3, fill: stroke }}
            connectNulls={false}
            isAnimationActive={false}
          />
        </AreaChart>
      </ResponsiveContainer>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Per-site RUM detail panel
// ---------------------------------------------------------------------------

interface PerSiteRumViewProps {
  siteId: string;
  siteName: string;
  onBack: () => void;
}

function PerSiteRumView({ siteId, siteName, onBack }: PerSiteRumViewProps) {
  return (
    <section aria-labelledby="per-site-rum-heading" className="space-y-6">
      {/* Back affordance + sub-header */}
      <div className="flex flex-wrap items-center gap-3">
        <button
          type="button"
          onClick={onBack}
          className={cn(
            "inline-flex items-center gap-1.5 rounded text-xs text-[var(--color-muted-foreground)]",
            "transition-colors hover:text-[var(--color-foreground)]",
            "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] focus-visible:ring-offset-1",
          )}
          aria-label="Back to all sites"
        >
          <ChevronLeft aria-hidden="true" className="size-3.5" />
          All sites
        </button>
        <span aria-hidden="true" className="text-xs text-[var(--color-border)]">/</span>
        <span
          id="per-site-rum-heading"
          className="text-xs font-medium text-[var(--color-foreground)]"
        >
          {siteName || siteId}
        </span>
      </div>

      {/* Per-site CWV cards + distribution bars + trend charts (FleetRumPanel) */}
      <FleetRumPanel siteId={siteId} siteName={siteName} />

      {/* Per-URL/device breakdown table (RumResultsTable) */}
      <RumResultsTable siteId={siteId} perSite />
    </section>
  );
}

// ---------------------------------------------------------------------------
// Fleet view
// ---------------------------------------------------------------------------

interface FleetRumViewProps {
  device: DeviceFilter;
  windowDays: number;
  onDrillSite: (siteId: string) => void;
}

function FleetRumView({ device, windowDays, onDrillSite }: FleetRumViewProps) {
  const { data, isPending, isError, error, refetch } = useFleetRum(windowDays, device);

  const siteColumns = buildSiteColumns(windowDays, (row) => onDrillSite(row.site_id));

  return (
    <>
      {/* Fleet headline strip */}
      {isPending ? (
        <HeadlineStrip sitesReporting={0} sitesTotal={0} fleetPassPct={null} loading />
      ) : isError ? null : (
        <HeadlineStrip
          sitesReporting={data.sites_reporting ?? 0}
          sitesTotal={data.sites_total ?? 0}
          fleetPassPct={data.fleet_pass_pct ?? null}
          loading={false}
        />
      )}

      {/* Core Web Vitals section */}
      <section aria-labelledby="cwv-fleet-heading" className="space-y-4">
        <div className="flex items-center gap-2 border-b border-[var(--color-border)] pb-3">
          <Activity aria-hidden="true" className="size-4 shrink-0 text-[var(--color-muted-foreground)]" />
          <h2
            id="cwv-fleet-heading"
            className="text-sm font-semibold text-[var(--color-foreground)]"
          >
            Core Web Vitals
          </h2>
          <span className="text-xs text-[var(--color-muted-foreground)]">
            Sites by Core Web Vitals, sorted by LCP
          </span>
        </div>

        {isPending ? (
          <div className="rounded-lg border border-[var(--color-border)] bg-[var(--color-card)] p-4 space-y-3">
            {Array.from({ length: 5 }).map((_, i) => (
              <Skeleton key={i} className="h-8 w-full rounded" />
            ))}
          </div>
        ) : isError ? (
          <PageError
            what="Could not load fleet RUM data."
            why={error?.message ?? "Unknown error"}
            onRetry={() => void refetch()}
            retryLabel="Reload RUM data"
          />
        ) : (data.sites_reporting ?? 0) === 0 ? (
          <div className="rounded-lg border border-[var(--color-border)] px-5 py-10 text-center">
            <Activity
              aria-hidden="true"
              strokeWidth={1.5}
              className="mx-auto mb-3 size-8 text-[var(--color-muted-foreground)]/40"
            />
            <p className="text-sm text-[var(--color-muted-foreground)]">
              No sites are reporting Real User Monitoring data yet. Enable RUM in
              each site's Optimize tab.
            </p>
          </div>
        ) : (
          <FleetTable<FleetRumOffender>
            data={data.worst_offenders ?? []}
            columns={siteColumns}
            height={Math.min(520, Math.max(200, (data.worst_offenders ?? []).length * 52 + 44))}
            ariaLabel="Sites by Core Web Vitals"
            defaultSorting={[{ id: "lcp_p75", desc: true }]}
            onRowClick={(row) => onDrillSite(row.site_id)}
            emptyState={
              <p className="text-center text-sm text-[var(--color-muted-foreground)]">
                No RUM data to display for this filter.
              </p>
            }
          />
        )}

        {/* 28-day trend, small multiples for LCP / INP / CLS */}
        {!isPending && !isError && (data.trend ?? []).length >= 2 && (
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
            {TREND_METRICS.map((m) => (
              <div
                key={m}
                className="rounded-lg border border-[var(--color-border)] bg-[var(--color-card)] px-4 py-3"
              >
                <FleetCwvTrendChart trend={data.trend ?? []} metric={m} windowDays={windowDays} />
              </div>
            ))}
          </div>
        )}
      </section>

      {/* Database health aggregate (existing panel) */}
      <FleetDbHealthPanel />
    </>
  );
}

// ---------------------------------------------------------------------------
// Main page
// ---------------------------------------------------------------------------

function PerformancePage() {
  const { device, window: windowDays, site: selectedSiteId } = Route.useSearch();
  const navigate = useNavigate({ from: Route.fullPath });

  const setDevice = useCallback(
    (d: DeviceFilter) => {
      void navigate({ search: (prev) => ({ ...prev, device: d }) });
    },
    [navigate],
  );

  const setSite = useCallback(
    (siteId: string | undefined) => {
      void navigate({ search: (prev) => ({ ...prev, site: siteId }) });
    },
    [navigate],
  );

  // Resolve site name for breadcrumb when a site is selected.
  const { data: sites = [] } = useSites();
  const selectedSite = selectedSiteId
    ? sites.find((s) => s.id === selectedSiteId)
    : undefined;
  const siteName = selectedSite?.name ?? selectedSite?.url ?? selectedSiteId ?? "";

  const isPerSite = Boolean(selectedSiteId);

  return (
    <section aria-labelledby="performance-heading" className="space-y-6">
      <PageHeader
        title="Performance"
        subline={
          isPerSite
            ? `Site-level Core Web Vitals for ${siteName || (selectedSiteId ?? "")}`
            : "Fleet-wide Core Web Vitals and database health across all connected sites"
        }
        actions={
          <div className="flex flex-wrap items-center gap-3">
            <SiteScopeSelector value={selectedSiteId} onChange={setSite} />
            <DeviceTabs value={device} onChange={setDevice} />
          </div>
        }
      />

      {isPerSite && selectedSiteId ? (
        <PerSiteRumView
          siteId={selectedSiteId}
          siteName={siteName}
          onBack={() => setSite(undefined)}
        />
      ) : (
        <FleetRumView
          device={device}
          windowDays={windowDays}
          onDrillSite={(siteId) => setSite(siteId)}
        />
      )}
    </section>
  );
}
