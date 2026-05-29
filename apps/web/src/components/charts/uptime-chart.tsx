// 30-day uptime chart for the site detail Health tile. Two series, both in
// chart-1: current period solid, previous period dashed at 50% opacity. A
// 99.9% SLA reference line sits in `--warning-subtle` (the warning-tinted
// background token) so it reads as a budget line, not as a warning event.
//
// Axes follow the operator-grade ruleset: grid is 3-3 dashed in `--border`,
// ticks are small and muted, percent suffix on Y, every-fifth-day on X.
//
// The tooltip is the shared `ChartTooltip` — no per-chart variants — so any
// future addition (CWV, response time, etc.) inherits the same look.

import {
  CartesianGrid,
  Line,
  LineChart,
  ReferenceLine,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import { ChartTooltip } from "./chart-tooltip";
import { ChartEmpty } from "./chart-empty";

export interface UptimePoint {
  /** ISO date string (e.g. `2026-05-01`). */
  date: string;
  /** 0-100 percent uptime for the current period. */
  uptime: number;
  /** Optional 0-100 percent uptime for the previous period (same day offset). */
  previousUptime?: number;
}

export interface UptimeChartProps {
  data: UptimePoint[];
  height?: number;
}

function shortDate(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

function formatPct(value: number): string {
  if (!Number.isFinite(value)) return "—";
  return `${value.toFixed(2)}%`;
}

export function UptimeChart({ data, height = 180 }: UptimeChartProps) {
  // Recharts will happily render a chart from one point, but a single point
  // is not a time series — it's an empty state with one probe. We treat
  // anything under two points as empty so the operator sees a clear "no data
  // yet" instead of a misleading flat line.
  if (!data || data.length < 2) {
    return <ChartEmpty message="No uptime data yet" />;
  }

  const hasPrevious = data.some((d) => typeof d.previousUptime === "number");

  // X tick every ~5 days. Recharts uses `interval` as a number of points to
  // skip between ticks, so for a 30-day series this gives ~6 ticks.
  const interval = Math.max(0, Math.floor(data.length / 6) - 1);

  return (
    <div style={{ width: "100%", height }}>
      <ResponsiveContainer width="100%" height="100%">
        <LineChart
          data={data}
          margin={{ top: 8, right: 12, bottom: 0, left: 0 }}
        >
          <CartesianGrid
            strokeDasharray="3 3"
            stroke="var(--color-border)"
            vertical={false}
          />
          <XAxis
            dataKey="date"
            tickFormatter={shortDate}
            interval={interval}
            tick={{
              fill: "var(--color-muted-foreground)",
              fontSize: 11,
            }}
            stroke="var(--color-border)"
            tickLine={false}
            axisLine={false}
          />
          <YAxis
            domain={[0, 100]}
            tickFormatter={(v: number) => `${v}%`}
            tick={{
              fill: "var(--color-muted-foreground)",
              fontSize: 11,
            }}
            stroke="var(--color-border)"
            tickLine={false}
            axisLine={false}
            width={36}
          />
          <Tooltip
            content={
              <ChartTooltip
                formatter={formatPct}
                labelFormatter={(l) => shortDate(l)}
                showPrevious={hasPrevious}
              />
            }
            cursor={{
              stroke: "var(--color-border)",
              strokeDasharray: "3 3",
            }}
          />
          <ReferenceLine
            y={99.9}
            stroke="var(--color-warning-subtle)"
            strokeWidth={1}
            label={{
              value: "99.9% SLA",
              position: "insideTopRight",
              fill: "var(--color-muted-foreground)",
              fontSize: 11,
            }}
          />
          {hasPrevious ? (
            <Line
              type="monotone"
              dataKey="previousUptime"
              name="Previous"
              stroke="var(--color-chart-1)"
              strokeWidth={1.5}
              strokeDasharray="4 4"
              strokeOpacity={0.5}
              dot={false}
              activeDot={false}
              isAnimationActive={false}
            />
          ) : null}
          <Line
            type="monotone"
            dataKey="uptime"
            name="Uptime"
            stroke="var(--color-chart-1)"
            strokeWidth={1.5}
            dot={false}
            activeDot={{
              r: 3,
              stroke: "var(--color-chart-1)",
              strokeWidth: 1,
              fill: "var(--color-background)",
            }}
            isAnimationActive={false}
          />
        </LineChart>
      </ResponsiveContainer>
    </div>
  );
}
