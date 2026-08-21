import {
  BarChart,
  Bar,
  XAxis,
  YAxis,
  Tooltip,
  ResponsiveContainer,
  Legend,
} from "recharts";

import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  CardDescription,
} from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback";
import type { EmailStats, EmailStatsByDay, EmailStatsByProvider } from "@wpmgr/api";
import { useEmailStats } from "./use-email";

// ---------------------------------------------------------------------------
// Deliverability stats panel (per-site)
//
// Shows top-line sent/failed/total counts, a per-day sent/failed bar chart,
// and a per-provider breakdown table.
// ---------------------------------------------------------------------------

export interface EmailDeliverabilityProps {
  siteId: string;
}

export function EmailDeliverability({ siteId }: EmailDeliverabilityProps) {
  const stats = useEmailStats(siteId);

  if (stats.isPending) {
    return <StatsSkeletons />;
  }

  if (stats.isError) {
    return (
      <PageError
        what="Could not load email statistics."
        why={stats.error?.message}
        onRetry={() => void stats.refetch()}
        retryLabel="Reload stats"
      />
    );
  }

  const data = stats.data;

  if (!data || (data.total === 0 && data.by_day.length === 0)) {
    return (
      <div className="rounded-lg border border-[var(--color-border)] px-5 py-10 text-center">
        <p className="text-sm text-[var(--color-muted-foreground)]">
          No email activity recorded for this period.
        </p>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      {/* Top-line stat cards */}
      <div className="grid grid-cols-2 gap-4 sm:grid-cols-3">
        <StatCard
          label="Total"
          value={data.total}
        />
        <StatCard
          label="Sent"
          value={data.sent_count}
          variant="success"
        />
        <StatCard
          label="Failed"
          value={data.failed_count}
          variant="destructive"
        />
      </div>

      {/* Per-day bar chart */}
      {data.by_day.length > 0 ? (
        <Card>
          <CardHeader>
            <CardTitle>Daily delivery</CardTitle>
            <CardDescription>Sent and failed emails by day.</CardDescription>
          </CardHeader>
          <CardContent>
            <DailyChart data={data.by_day} />
          </CardContent>
        </Card>
      ) : null}

      {/* Per-provider breakdown */}
      {data.by_provider.length > 0 ? (
        <Card>
          <CardHeader>
            <CardTitle>By provider</CardTitle>
          </CardHeader>
          <CardContent>
            <ProviderBreakdown data={data.by_provider} />
          </CardContent>
        </Card>
      ) : null}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Fleet deliverability stats (org-scope)
// ---------------------------------------------------------------------------

export interface FleetEmailDeliverabilityProps {
  stats: EmailStats;
}

export function FleetEmailDeliverability({ stats }: FleetEmailDeliverabilityProps) {
  if (stats.total === 0 && stats.by_day.length === 0) {
    return (
      <div className="rounded-lg border border-[var(--color-border)] px-5 py-10 text-center">
        <p className="text-sm text-[var(--color-muted-foreground)]">
          No email activity recorded for this period.
        </p>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      {/* Top-line stat cards */}
      <div className="grid grid-cols-2 gap-4 sm:grid-cols-4">
        <StatCard label="Total" value={stats.total} />
        <StatCard label="Sent" value={stats.sent_count} variant="success" />
        <StatCard label="Failed" value={stats.failed_count} variant="destructive" />
        {stats.site_count != null ? (
          <StatCard label="Sites" value={stats.site_count} />
        ) : null}
      </div>

      {stats.by_day.length > 0 ? (
        <Card>
          <CardHeader>
            <CardTitle>Daily delivery</CardTitle>
            <CardDescription>Fleet-wide sent and failed emails by day.</CardDescription>
          </CardHeader>
          <CardContent>
            <DailyChart data={stats.by_day} />
          </CardContent>
        </Card>
      ) : null}

      {stats.by_provider.length > 0 ? (
        <Card>
          <CardHeader>
            <CardTitle>By provider</CardTitle>
          </CardHeader>
          <CardContent>
            <ProviderBreakdown data={stats.by_provider} />
          </CardContent>
        </Card>
      ) : null}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Stat card
// ---------------------------------------------------------------------------

interface StatCardProps {
  label: string;
  value: number;
  variant?: "success" | "destructive";
}

function StatCard({ label, value, variant }: StatCardProps) {
  return (
    <Card>
      <CardContent className="pt-6">
        <p className="text-xs font-medium uppercase tracking-wide text-[var(--color-muted-foreground)]">
          {label}
        </p>
        <p
          className={
            variant === "success"
              ? "mt-1 text-2xl font-bold text-green-600 dark:text-green-400"
              : variant === "destructive"
                ? "mt-1 text-2xl font-bold text-[var(--color-destructive)]"
                : "mt-1 text-2xl font-bold text-[var(--color-foreground)]"
          }
        >
          {value.toLocaleString()}
        </p>
      </CardContent>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// Daily bar chart (recharts)
// ---------------------------------------------------------------------------

function DailyChart({ data }: { data: EmailStatsByDay[] }) {
  const chartData = data.map((d) => ({
    day: d.day.slice(5), // Show MM-DD instead of full YYYY-MM-DD
    sent: d.sent_count,
    failed: d.failed_count,
  }));

  return (
    <ResponsiveContainer width="100%" height={220}>
      <BarChart
        data={chartData}
        margin={{ top: 4, right: 4, left: -16, bottom: 0 }}
      >
        <XAxis
          dataKey="day"
          tick={{ fontSize: 11, fill: "var(--color-muted-foreground)" }}
          axisLine={false}
          tickLine={false}
        />
        <YAxis
          allowDecimals={false}
          tick={{ fontSize: 11, fill: "var(--color-muted-foreground)" }}
          axisLine={false}
          tickLine={false}
        />
        <Tooltip
          contentStyle={{
            background: "var(--color-popover)",
            border: "1px solid var(--color-border)",
            borderRadius: "8px",
            fontSize: 12,
            color: "var(--color-popover-foreground)",
          }}
          cursor={{ fill: "var(--color-accent)" }}
        />
        <Legend
          wrapperStyle={{ fontSize: 12, color: "var(--color-muted-foreground)" }}
        />
        <Bar
          dataKey="sent"
          name="Sent"
          fill="var(--color-primary)"
          radius={[3, 3, 0, 0]}
          maxBarSize={24}
        />
        <Bar
          dataKey="failed"
          name="Failed"
          fill="var(--color-destructive)"
          radius={[3, 3, 0, 0]}
          maxBarSize={24}
        />
      </BarChart>
    </ResponsiveContainer>
  );
}

// ---------------------------------------------------------------------------
// Provider breakdown table
// ---------------------------------------------------------------------------

function ProviderBreakdown({ data }: { data: EmailStatsByProvider[] }) {
  return (
    <div className="space-y-2">
      {data.map((row) => (
        <div
          key={row.provider}
          className="flex items-center justify-between gap-4"
        >
          <Badge variant="outline" className="text-xs font-normal">
            {row.provider}
          </Badge>
          <div className="flex items-center gap-4 text-sm">
            <span className="tabular-nums text-green-600 dark:text-green-400">
              {row.sent_count.toLocaleString()} sent
            </span>
            {row.failed_count > 0 ? (
              <span className="tabular-nums text-[var(--color-destructive)]">
                {row.failed_count.toLocaleString()} failed
              </span>
            ) : null}
            <span className="tabular-nums text-[var(--color-muted-foreground)]">
              {row.total.toLocaleString()} total
            </span>
          </div>
        </div>
      ))}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Loading skeletons
// ---------------------------------------------------------------------------

function StatsSkeletons() {
  return (
    <div className="space-y-4">
      <div className="grid grid-cols-3 gap-4">
        <Skeleton className="h-24 rounded-xl" />
        <Skeleton className="h-24 rounded-xl" />
        <Skeleton className="h-24 rounded-xl" />
      </div>
      <Skeleton className="h-56 rounded-xl" />
    </div>
  );
}
