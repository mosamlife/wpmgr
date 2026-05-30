import { createFileRoute } from "@tanstack/react-router";
import type { ReactNode } from "react";

import { BackupChip } from "@/components/status";
import { Sparkline } from "@/components/charts";
import { useBackups } from "@/features/backups/use-backups";
import { useSiteUptime } from "@/features/monitoring/use-uptime";
import { HealthTab as DiagnosticsHealth } from "@/features/health/health-tab";
import { formatBytes, relativeTime, cn } from "@/lib/utils";

// `/sites/$siteId/health` — the Health tab (ADR-037 Impeccable, Batch 1).
//
// Structure, top to bottom:
//   1. The headline summary band: ONE card split into four tiles
//      (Uptime / Last backup / Vulnerabilities / Performance). Per DESIGN.md
//      ("Don't nest cards.") the tiles share one bordered card, divided by
//      internal borders rather than four sibling cards. The two unwired tiles
//      render an HONEST empty ("Not scanned yet" / "Not measured yet") instead
//      of a fabricated "0 findings".
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
          <VulnerabilitiesTile />
          <PerformanceTile />
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
}: {
  title: string;
  value: ReactNode;
  sub?: ReactNode;
  children?: ReactNode;
}) {
  return (
    <div className="flex min-w-0 flex-col gap-2 p-6">
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
    </div>
  );
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

function VulnerabilitiesTile() {
  // Not yet wired to a scan endpoint. Render an honest empty rather than a
  // fabricated "0 findings" — asserting zero vulnerabilities would be a false
  // fact when no scan has run.
  return <Tile title="Vulnerabilities" value="–" sub="Not scanned yet" />;
}

function PerformanceTile() {
  // Not yet wired to a performance endpoint. Honest empty, same reasoning.
  return <Tile title="Performance" value="–" sub="Not measured yet" />;
}
