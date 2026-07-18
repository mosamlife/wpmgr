// CacheHitRatioTrendCard — surfaces the cache hit-ratio trend chart inside
// CacheTab. Mirrors DBSizeTrendCard conventions: standalone card, owns its own
// loading/error/empty states, calls useCacheHealth directly.
//
// Layout:
//   - Header: title + subtitle + average ratio badge + WindowToggle (7/30/90d)
//     (+ a "served at the web server level" callout when htaccessManaged)
//   - Body:   isFetching skeleton  |  error message  |  area chart or empty state
//
// The WindowToggle drives the days prop of useCacheHealth. isFetching (not
// isPending) drives the spinner so window switches show a refetch indicator
// while the previous window's data remains visible.
//
// The avg_ratio_pct stat is shown in the card header alongside the window
// selector. It updates as the selected window changes.
//
// Empty state is the normal first-paint: the endpoint returns no points until
// the agent begins reporting hit/miss traffic.
//
// GH #243 (honest hit-ratio semantics) — with the agent-managed .htaccess
// active, Apache serves cache hits statically without ever running PHP, so
// the PHP-side tally only ever sees misses. The stored series is real data;
// it is just measuring the PHP layer, not the whole site. When
// `htaccessManaged` is true we say so plainly (title becomes "PHP-layer hit
// ratio", plus a success-toned callout explaining where the fast-path hits
// went) instead of letting a near-0% number read as "caching is broken".
// When false (nginx, or .htaccess not managed) render exactly as before.

import { useState } from "react";
import { Loader2, Zap } from "lucide-react";

import { Skeleton } from "@/components/ui/skeleton";
import { CacheHitRatioChart } from "@/components/charts/cache-hit-ratio-chart";
import { useCacheHealth } from "../hooks/useCacheHealth";

/** Selectable lookback window (numeric days, matches the endpoint query param). */
type CacheWindow = 7 | 30 | 90;

const CACHE_WINDOWS: ReadonlyArray<{ value: CacheWindow; label: string }> = [
  { value: 7, label: "7d" },
  { value: 30, label: "30d" },
  { value: 90, label: "90d" },
];

export interface CacheHitRatioTrendCardProps {
  siteId: string;
  /**
   * GH #243 — true when the agent manages the site's .htaccess (Apache
   * serves cached pages before PHP runs, so the PHP-layer tally undercounts
   * hits). Drives the title relabel + the honest-semantics callout.
   */
  htaccessManaged: boolean;
  /** Chart height passed to CacheHitRatioChart (default 160). */
  chartHeight?: number;
}

export function CacheHitRatioTrendCard({
  siteId,
  htaccessManaged,
  chartHeight = 160,
}: CacheHitRatioTrendCardProps) {
  const [days, setDays] = useState<CacheWindow>(7);
  const { data, isLoading, isError, isFetching } = useCacheHealth(siteId, days);

  const hasPoints = (data?.points.length ?? 0) >= 1;
  const title = htaccessManaged ? "PHP-layer hit ratio" : "Cache hit ratio";

  return (
    <section
      aria-label={title}
      className="rounded-xl border border-border bg-card text-card-foreground shadow-sm"
    >
      {/* Card header */}
      <div className="space-y-3 border-b border-border px-5 py-3">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div className="min-w-0">
            <h3 className="text-sm font-semibold text-foreground">
              {title}
            </h3>
            <p className="mt-0.5 text-xs text-muted-foreground">
              Hit percentage over the selected window — one point per sample
            </p>
          </div>

          <div className="flex items-center gap-3">
            {/* Average ratio badge — only when we have data */}
            {hasPoints && data ? (
              <AvgRatioBadge avgRatioPct={data.avg_ratio_pct} busy={isFetching} />
            ) : null}

            <CacheWindowToggle value={days} onChange={setDays} busy={isFetching} />
          </div>
        </div>

        {htaccessManaged ? <ServedAtWebServerCallout /> : null}
      </div>

      {/* Chart body */}
      <div className="px-2 py-3">
        {isLoading ? (
          <div className="flex flex-col gap-2 px-3">
            <Skeleton className="h-3 w-24 rounded" />
            <Skeleton style={{ height: chartHeight }} className="w-full rounded-md" />
          </div>
        ) : isError ? (
          <div className="flex items-center justify-center py-10 text-xs text-muted-foreground">
            Could not load hit-ratio history.
          </div>
        ) : (
          <CacheHitRatioChart
            points={data?.points ?? []}
            height={chartHeight}
          />
        )}
      </div>
    </section>
  );
}

// ---------------------------------------------------------------------------
// "Served at the web server level" callout (GH #243)
// ---------------------------------------------------------------------------
// A calm SUCCESS-toned state, not a warning — the site is doing the fast
// thing. Explains why the PHP-layer ratio below can read low even when the
// page cache is working perfectly, and gives the operator a way to verify it
// independently (the x-wpmgr-source response header).

function ServedAtWebServerCallout() {
  return (
    <div
      role="status"
      className="flex items-start gap-2.5 rounded-lg border border-[var(--color-success)]/25 bg-[var(--color-success-subtle)] px-3.5 py-3 text-[var(--color-success-subtle-fg)]"
    >
      <Zap aria-hidden="true" className="mt-0.5 size-4 shrink-0" />
      <div className="min-w-0 space-y-1">
        <p className="text-sm font-medium">Served at the web-server level</p>
        <p className="text-xs">
          Cached pages on this site are served directly by the web server
          (the fastest path), before PHP runs. Those hits cannot be counted
          at the PHP layer, so the ratio below reflects only requests that
          reached PHP.
        </p>
        <p className="text-xs opacity-90">
          To verify caching, check the{" "}
          <span className="font-mono">x-wpmgr-source</span> response header
          on an anonymous page view:{" "}
          <span className="font-medium">Web Server</span> means the fast
          path served it.
        </p>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Average ratio badge
// ---------------------------------------------------------------------------

interface AvgRatioBadgeProps {
  avgRatioPct: number;
  busy: boolean;
}

function AvgRatioBadge({ avgRatioPct, busy }: AvgRatioBadgeProps) {
  const display = `${avgRatioPct.toFixed(1)}% avg`;

  return (
    <span className="inline-flex items-center gap-1.5 rounded-full bg-muted px-2.5 py-0.5 text-xs font-medium tabular-nums text-muted-foreground ring-1 ring-border">
      {busy ? (
        <Loader2 aria-hidden="true" className="size-3 animate-spin" />
      ) : null}
      {display}
      {busy ? <span className="sr-only">(refreshing)</span> : null}
    </span>
  );
}

// ---------------------------------------------------------------------------
// Window toggle
// ---------------------------------------------------------------------------

interface CacheWindowToggleProps {
  value: CacheWindow;
  onChange: (w: CacheWindow) => void;
  busy: boolean;
}

function CacheWindowToggle({ value, onChange, busy }: CacheWindowToggleProps) {
  return (
    <div
      role="group"
      aria-label="Cache hit-ratio window"
      className="inline-flex rounded-md border border-border"
    >
      {CACHE_WINDOWS.map((w) => {
        const active = w.value === value;
        return (
          <button
            key={w.value}
            type="button"
            aria-pressed={active}
            onClick={() => onChange(w.value)}
            className={
              "px-3 py-1.5 text-sm font-medium transition-colors first:rounded-l-md last:rounded-r-md focus-visible:ring-2 focus-visible:ring-[var(--color-ring)] focus-visible:outline-none " +
              (active
                ? "bg-[var(--color-primary)] text-[var(--color-primary-foreground)]"
                : "hover:bg-[var(--color-accent)]")
            }
          >
            {w.label}
            {active && busy ? (
              <span className="sr-only"> (refreshing)</span>
            ) : null}
          </button>
        );
      })}
    </div>
  );
}
