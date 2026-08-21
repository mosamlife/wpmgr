// PortalHero — 5 KPI tiles with CountUp animations.
//
// Tiles: sites monitored · average uptime · backups completed · updates applied
//        (with failed subline when >0) · site speed rating
//
// KPI numerals use brand --color-primary (via the portal-shell's scoped var).
// Never invent zeros for missing data — show "—" with "No data yet" subline.
//
// Mobile: grid-cols-2; tablet: sm:grid-cols-3; desktop: lg:grid-cols-5

import { Skeleton } from "@/components/ui/skeleton";
import { CountUp } from "@/components/ui/count-up";
import { cn } from "@/lib/utils";
import type { PortalSummaryTotals } from "./use-portal";

// ---------------------------------------------------------------------------
// Vitals rating display
// ---------------------------------------------------------------------------

const VITALS_LABEL: Record<string, string> = {
  good: "Good",
  "needs-improvement": "Fair",
  poor: "Poor",
};

const VITALS_COLOR: Record<string, string> = {
  good: "text-[var(--color-success)]",
  "needs-improvement": "text-[var(--color-warning)]",
  poor: "text-[var(--color-destructive)]",
};

// ---------------------------------------------------------------------------
// KPI tile
// ---------------------------------------------------------------------------

interface KpiTileProps {
  label: string;
  children: React.ReactNode;
  subline?: React.ReactNode;
  className?: string;
}

function KpiTile({ label, children, subline, className }: KpiTileProps) {
  return (
    <div
      className={cn(
        "flex flex-col gap-1 rounded-lg border border-[var(--color-border)] bg-[var(--color-card)] px-4 py-4",
        className,
      )}
    >
      <p className="text-xs text-[var(--color-muted-foreground)]">{label}</p>
      <div className="font-mono text-2xl font-bold tabular-nums text-[var(--color-primary)]">
        {children}
      </div>
      {subline ? (
        <p className="text-xs text-[var(--color-muted-foreground)]">{subline}</p>
      ) : null}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Loading skeleton
// ---------------------------------------------------------------------------

export function PortalHeroSkeleton() {
  return (
    <div className="mb-6">
      <Skeleton className="mb-1 h-6 w-48" />
      <Skeleton className="mb-4 h-4 w-36" />
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
        {[0, 1, 2, 3, 4].map((i) => (
          <Skeleton key={i} className="h-[90px] w-full rounded-lg" />
        ))}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Main component
// ---------------------------------------------------------------------------

export interface PortalHeroProps {
  agencyName: string;
  periodLabel: string;
  totals: PortalSummaryTotals;
  /**
   * Null when the fleet has not collected enough real-user samples to rate
   * Core Web Vitals. That is a different fact from a bad score and is never
   * rendered as one: see the "Site speed" tile below.
   */
  vitalsOverall: "good" | "needs-improvement" | "poor" | null | undefined;
}

export function PortalHero({
  agencyName,
  periodLabel,
  totals,
  vitalsOverall,
}: PortalHeroProps) {
  const hasUptime = totals.avg_uptime_pct != null;
  const vitalsLabel = vitalsOverall ? VITALS_LABEL[vitalsOverall] : null;
  const vitalsColor = vitalsOverall ? VITALS_COLOR[vitalsOverall] : null;

  return (
    <div className="mb-6">
      {agencyName ? (
        <p className="mb-0.5 text-xs text-[var(--color-muted-foreground)]">
          Managed by {agencyName}
        </p>
      ) : null}
      <h2 className="mb-1 text-lg font-semibold text-[var(--color-foreground)]">
        Last 30 days, at a glance
      </h2>
      <p className="mb-4 text-xs text-[var(--color-muted-foreground)]">
        {periodLabel}
      </p>

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
        {/* Sites monitored */}
        <KpiTile label="Sites monitored">
          <CountUp value={totals.site_count} durationMs={900} />
        </KpiTile>

        {/* Average uptime */}
        <KpiTile
          label="Average uptime"
          subline={hasUptime ? undefined : "No data yet"}
        >
          {hasUptime ? (
            <CountUp
              value={totals.avg_uptime_pct!}
              durationMs={1100}
              format={(n) => n.toFixed(2)}
              suffix="%"
            />
          ) : (
            <span className="text-[var(--color-muted-foreground)]">—</span>
          )}
        </KpiTile>

        {/* Backups completed */}
        <KpiTile label="Backups completed">
          <CountUp value={totals.backups_count} durationMs={900} />
        </KpiTile>

        {/* Updates applied */}
        <KpiTile
          label="Updates applied"
          subline={
            totals.updates_failed > 0 ? (
              <span className="text-[var(--color-destructive)]">
                {totals.updates_failed} failed
              </span>
            ) : undefined
          }
        >
          <CountUp value={totals.updates_applied} durationMs={900} />
        </KpiTile>

        {/* Site speed (vitals) */}
        <KpiTile label="Site speed">
          {vitalsLabel ? (
            <span className={cn("font-sans text-xl", vitalsColor ?? "")}>
              {vitalsLabel}
            </span>
          ) : (
            // Absence is stated in words, not as a placeholder glyph. A dash
            // in a slot that otherwise holds a rating reads as a rating.
            <span className="font-sans text-sm font-normal text-[var(--color-muted-foreground)]">
              Not enough data
            </span>
          )}
        </KpiTile>
      </div>
    </div>
  );
}
