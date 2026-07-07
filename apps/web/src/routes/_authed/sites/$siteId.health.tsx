import { createFileRoute, Link, type LinkProps } from "@tanstack/react-router";
import type { ReactNode } from "react";

import { BackupChip, VulnSeverityChip } from "@/components/status";
import { Sparkline } from "@/components/charts";
import { useBackups } from "@/features/backups/use-backups";
import { useSiteUptime } from "@/features/monitoring/use-uptime";
import { HealthTab as DiagnosticsHealth } from "@/features/health/health-tab";
import {
  useSiteVulnerabilities,
  worstSeverity,
} from "@/features/security/use-vuln";
import { useRumSummary } from "@/features/perf/hooks/useRumSummary";
import {
  findAggregateMetric,
  formatLcpP75,
  RUM_RATING_CLASS,
  RUM_RATING_LABEL,
} from "@/features/perf/rum-tile";
import { formatBytes, relativeTime, cn } from "@/lib/utils";

// `/sites/$siteId/health` — the Health tab (ADR-037 Impeccable, Batch 1).
//
// Structure, top to bottom:
//   1. The headline summary band: ONE card split into four tiles
//      (Uptime / Last backup / Vulnerabilities / Performance). Per DESIGN.md
//      ("Don't nest cards.") the tiles share one bordered card, divided by
//      internal borders rather than four sibling cards. Vulnerabilities reads
//      the Security Suite vuln scanner (useSiteVulnerabilities) and
//      Performance reads the RUM Core Web Vitals summary (useRumSummary) —
//      both tiles render an HONEST empty when there is genuinely no data yet
//      (feed not configured / no visitor samples) rather than a fabricated
//      "0 findings" or score. Both tiles link out to the page that owns their
//      action (Rescan / the Performance dashboard), since the Health tab
//      itself has no trigger for either.
//   2. The diagnostics surface (HealthTab from features/health): a header
//      ribbon + grouped sections. The ribbon owns the page <h2>, the host
//      identity, and the single "Re-run all checks" action.

export const Route = createFileRoute("/_authed/sites/$siteId/health")({
  component: HealthTab,
});

function HealthTab() {
  const { siteId } = Route.useParams();

  return (
    <section className="space-y-6 px-4 pb-8 pt-6 sm:px-6">
      <div className="rounded-lg border border-border bg-card">
        <div
          className={cn(
            "grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4",
            "divide-y divide-border sm:divide-x sm:divide-y-0",
          )}
        >
          <UptimeTile siteId={siteId} />
          <LastBackupTile siteId={siteId} />
          <VulnerabilitiesTile siteId={siteId} />
          <PerformanceTile siteId={siteId} />
        </div>
      </div>

      <DiagnosticsHealth siteId={siteId} />
    </section>
  );
}

// ── Tile primitive ───────────────────────────────────────────────────────────

function Tile({
  title,
  value,
  sub,
  children,
  linkTo,
}: {
  title: string;
  value: ReactNode;
  sub?: ReactNode;
  children?: ReactNode;
  /**
   * When present, the whole tile becomes a typed router Link to the page that
   * owns the action for this tile (the Health tab itself has no trigger for
   * a vuln rescan or a Performance drill-down).
   */
  linkTo?: Pick<LinkProps, "to" | "params" | "search">;
}) {
  const body = (
    <>
      <div className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
        {title}
      </div>
      <div className="text-2xl font-semibold tabular-nums text-foreground">
        {value}
      </div>
      {sub ? (
        <div className="text-xs tabular-nums text-muted-foreground">{sub}</div>
      ) : null}
      {children ? <div className="pt-1">{children}</div> : null}
    </>
  );

  if (linkTo) {
    return (
      <Link
        to={linkTo.to}
        params={linkTo.params}
        search={linkTo.search}
        className="flex min-w-0 flex-col gap-2 p-6 text-left transition-colors hover:bg-muted/50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-inset"
      >
        {body}
      </Link>
    );
  }

  return <div className="flex min-w-0 flex-col gap-2 p-6">{body}</div>;
}

function UptimeTile({ siteId }: { siteId: string }) {
  const { data, isPending } = useSiteUptime(siteId, "30d");

  if (isPending) {
    return <Tile title="Uptime 30d" value="…" sub="Loading" />;
  }
  if (!data) {
    return <Tile title="Uptime 30d" value="–" sub="No probes yet" />;
  }
  const pct = data.uptime_pct;
  const latencyValues = data.series
    .map((s) => s.avg_latency_ms)
    .filter((v): v is number => typeof v === "number" && Number.isFinite(v));

  return (
    <Tile
      title="Uptime 30d"
      value={`${pct.toFixed(2)}%`}
      sub={`avg ${data.avg_latency_ms} ms`}
    >
      <Sparkline
        data={latencyValues}
        width={120}
        height={24}
        ariaLabel="Latency sparkline"
      />
    </Tile>
  );
}

function LastBackupTile({ siteId }: { siteId: string }) {
  const { data, isPending } = useBackups(siteId);

  if (isPending) {
    return <Tile title="Last backup" value="…" sub="Loading" />;
  }
  const completed = (data ?? []).find((s) => s.status === "completed");
  if (!completed) {
    return <Tile title="Last backup" value="None" sub="Run a backup" />;
  }
  const when = relativeTime(completed.finished_at ?? completed.created_at);
  return (
    <Tile
      title="Last backup"
      value={when ?? "Recent"}
      sub={formatBytes(completed.total_size)}
    >
      <BackupChip status="success" time={when ?? undefined} />
    </Tile>
  );
}

// ── Vulnerabilities tile ─────────────────────────────────────────────────────
//
// Reads the Security Suite vuln scanner (same data as the Security tab's
// VulnPanel / vulnStatus()). SiteVulnsResponse has no aggregate count field,
// so the open-finding count and worst severity are derived from `items[]`
// here, exactly like vulnStatus() does in $siteId.security.tsx. worstSeverity
// is exported from use-vuln.ts (next to isHighRisk/countHighRisk) and tested
// in use-vuln.test.ts.

function VulnerabilitiesTile({ siteId }: { siteId: string }) {
  const { data, isPending, isError } = useSiteVulnerabilities(siteId);
  // The Health tab has no rescan trigger of its own — the whole tile links to
  // the Security tab, where "Rescan now" and the findings table live.
  const linkTo: Pick<LinkProps, "to" | "params"> = {
    to: "/sites/$siteId/security",
    params: { siteId },
  };

  if (isPending) {
    return (
      <Tile title="Vulnerabilities" value="…" sub="Loading" linkTo={linkTo} />
    );
  }
  if (isError || !data) {
    return (
      <Tile
        title="Vulnerabilities"
        value="–"
        sub="Could not load scan results"
        linkTo={linkTo}
      />
    );
  }
  // The instance has no Wordfence Intelligence feed connected. Render a
  // neutral "not configured" state — never imply the site was scanned and
  // found clean when no scan can run at all.
  if (!data.feed_ok) {
    return (
      <Tile
        title="Vulnerabilities"
        value="–"
        sub="Feed not configured"
        linkTo={linkTo}
      />
    );
  }

  const openFindings = (data.items ?? []).filter((f) => f.status === "open");
  if (openFindings.length === 0) {
    return (
      <Tile
        title="Vulnerabilities"
        value="0"
        sub="No known vulnerabilities"
        linkTo={linkTo}
      />
    );
  }

  const worst = worstSeverity(openFindings);
  return (
    <Tile
      title="Vulnerabilities"
      value={String(openFindings.length)}
      sub="Open vulnerabilities"
      linkTo={linkTo}
    >
      {worst ? <VulnSeverityChip severity={worst} /> : null}
    </Tile>
  );
}

// ── Performance tile ─────────────────────────────────────────────────────────
//
// Reads the RUM Core Web Vitals summary (same endpoint as the fleet
// Performance dashboard's FleetRumPanel). LCP is the headline metric — first
// among the three core CWV metrics in FleetRumPanel / RumResultsTable.
// findAggregateMetric / formatLcpP75 / RUM_RATING_* live in
// features/perf/rum-tile.ts (tested in rum-tile.test.ts), where the device:""
// aggregate-row contract with apps/api/internal/perf/rum_results_handler.go
// is pinned.

function PerformanceTile({ siteId }: { siteId: string }) {
  const { data, isPending, isError } = useRumSummary(siteId);
  // The Health tab has no drill-down of its own — the whole tile links to the
  // fleet Performance dashboard, pre-scoped to this site.
  const linkTo: Pick<LinkProps, "to" | "search"> = {
    to: "/performance",
    search: { site: siteId },
  };

  if (isPending) {
    return <Tile title="Performance" value="…" sub="Loading" linkTo={linkTo} />;
  }
  if (isError || !data) {
    return (
      <Tile
        title="Performance"
        value="–"
        sub="Could not load Core Web Vitals"
        linkTo={linkTo}
      />
    );
  }

  const hasSamples = (data.metrics ?? []).length > 0;
  if (!hasSamples) {
    // RUM is off by default and stays passive even when enabled — a fresh or
    // low-traffic site has no visitor measurements yet. Expected, not an error.
    return (
      <Tile
        title="Performance"
        value="–"
        sub="No visitor data yet"
        linkTo={linkTo}
      />
    );
  }

  const lcp = findAggregateMetric(data, "lcp");
  if (!lcp || lcp.suppressed || !lcp.rating || lcp.p75_ms === undefined) {
    // Some other metric has samples but LCP itself is below the site's
    // min_sample_count floor. Never show a p75 for a suppressed slice.
    return (
      <Tile
        title="Performance"
        value="–"
        sub="Not enough samples yet"
        linkTo={linkTo}
      />
    );
  }

  return (
    <Tile
      title="Performance"
      value={
        <span className={RUM_RATING_CLASS[lcp.rating]}>
          {formatLcpP75(lcp.p75_ms)}
        </span>
      }
      sub={`LCP p75 · ${RUM_RATING_LABEL[lcp.rating]}`}
      linkTo={linkTo}
    />
  );
}
