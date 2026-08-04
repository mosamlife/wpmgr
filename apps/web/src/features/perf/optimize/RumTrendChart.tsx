// RumTrendChart, daily p75 line/area chart for one CWV metric.
//
// Mirrors cache-hit-ratio-chart.tsx conventions exactly:
//   - ChartEmpty when fewer than 2 non-suppressed points.
//   - ResponsiveContainer + AreaChart.
//   - var(--color-chart-*) stroke/fill.
//   - ChartTooltip for the tooltip.
//   - ~6 X-axis ticks, short-date "Mon D" labels.
//   - isAnimationActive={false}.
//
// Two ReferenceLine overlays at the metric's good and needs-improvement
// thresholds (green / amber) so the operator sees where the series sits
// relative to the CWV pass/fail bands, exactly as Google's PageSpeed Insights.
//
// Suppressed days (p75_ms=0, suppressed=true) are mapped to null for the Y
// value. connectNulls={false} causes Recharts to break the line at those days
// rather than interpolating through them, preserving data honesty.
//
// CLS display: divide p75_ms by 1000 before rendering.
//
// Tooltip: shows the day, the p75 formatted by rum-trend-format.ts (CLS at 3
// decimals, everything else in ms to one decimal, or seconds once a reading
// reaches 1s), and the sample_count.
//
// GH #329: the Y axis and the threshold labels are owned entirely by
// features/perf/cwv-axis.ts. Read the header of that file before changing
// anything below about ticks, units, the domain or the threshold lines; in
// particular the tooltip formatter here deliberately does NOT share the axis
// scale, and the reason is measured and written down there.

import type { ReactNode } from "react";
import {
  Area,
  AreaChart,
  CartesianGrid,
  ReferenceLine,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";

import { ChartEmpty } from "@/components/charts/chart-empty";
import {
  cwvDataMax,
  cwvThresholdLabel,
  cwvYAxis,
  type CwvAxis,
  type CwvMetric,
} from "../cwv-axis";
import { formatTrendValue } from "./rum-trend-format";
import type { RumTrendPoint } from "../types";

// ---------------------------------------------------------------------------
// Display constants
// ---------------------------------------------------------------------------

export type MetricName = CwvMetric;

const METRIC_LABELS: Record<MetricName, string> = {
  lcp: "LCP",
  inp: "INP",
  cls: "CLS",
  fcp: "FCP",
  ttfb: "TTFB",
};

// Chart-token per metric to vary the line color across small multiples.
//
// DELIBERATELY AVOIDS --chart-2 AND --chart-3. Those two are defined at
// oklch(60% 0.14 155) and oklch(62% 0.16 75), which are the SAME hue and
// nearly the same chroma as --success (58% 0.14 155) and --warning
// (70% 0.14 75). Since the Good and Needs-Improvement reference lines carry
// those semantic colors, using chart-2 for FCP or chart-3 for TTFB drew the
// series in the same green, or the same amber, as its own threshold line
// (measured OKLab dE 0.0200 for FCP, 0.0825 for TTFB), and the series paints
// over the line. The threshold colors are semantic and must not move, so the
// decorative series colors move instead.
//
// Repeats across metrics are fine and intentional: every metric renders in
// its own card, so the only separation that has to hold is series versus
// threshold WITHIN one chart. The three hues used here (195, 235, 320) all
// sit far from the semantic 155 and 75.
const CHART_TOKEN: Record<MetricName, string> = {
  lcp: "var(--color-chart-1)",
  inp: "var(--color-chart-4)",
  cls: "var(--color-chart-5)",
  fcp: "var(--color-chart-5)",
  ttfb: "var(--color-chart-4)",
};

// Gradient IDs must be unique per metric to avoid cross-contamination.
const GRADIENT_ID: Record<MetricName, string> = {
  lcp: "rumTrendFill_lcp",
  inp: "rumTrendFill_inp",
  cls: "rumTrendFill_cls",
  fcp: "rumTrendFill_fcp",
  ttfb: "rumTrendFill_ttfb",
};

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function shortDate(day: string): string {
  // day is "YYYY-MM-DD", parsed as UTC noon to avoid a timezone off-by-one.
  const d = new Date(`${day}T12:00:00Z`);
  if (Number.isNaN(d.getTime())) return day;
  return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

/** Convert raw p75_ms from the API to the display value for a given metric. */
function toDisplayValue(metric: MetricName, p75_ms: number): number {
  return metric === "cls" ? p75_ms / 1000 : p75_ms;
}

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

/** Internal chart datum. Y is null for suppressed days so the line breaks. */
interface ChartDatum {
  day: string;
  y: number | null;
  sample_count: number;
  suppressed: boolean;
}

// ---------------------------------------------------------------------------
// Custom tooltip
// ---------------------------------------------------------------------------

interface TrendTooltipProps {
  active?: boolean;
  payload?: Array<{
    value?: number | null;
    payload?: ChartDatum;
    color?: string;
    stroke?: string;
  }>;
  label?: string;
  metric: MetricName;
}

function TrendTooltip({ active, payload, label, metric }: TrendTooltipProps) {
  if (!active || !payload || payload.length === 0) return null;
  const entry = payload[0];
  const datum = entry?.payload;
  const dayLabel = label ? shortDate(label) : "";
  const value = entry?.value;
  const swatch = entry?.color ?? entry?.stroke ?? CHART_TOKEN[metric];

  return (
    <div
      role="tooltip"
      className="rounded-md border border-[var(--color-border)] bg-[var(--color-popover)] px-3 py-2 text-sm shadow-md"
    >
      <div className="mb-1 text-xs text-[var(--color-muted-foreground)]">
        {dayLabel}
      </div>
      <dl className="flex flex-col gap-1">
        <div className="flex items-center gap-2">
          <span
            aria-hidden="true"
            className="inline-block h-2 w-2 rounded-full"
            style={{ backgroundColor: swatch }}
          />
          <dt className="text-[var(--color-muted-foreground)]">
            {METRIC_LABELS[metric]} p75
          </dt>
          <dd className="ml-auto font-mono tabular-nums text-[var(--color-foreground)]">
            {value !== null && value !== undefined
              ? formatTrendValue(metric, value)
              : "Insufficient samples"}
          </dd>
        </div>
        {datum && (
          <div className="flex items-center gap-2 text-xs">
            <span className="text-[var(--color-muted-foreground)]">
              Samples
            </span>
            <span className="ml-auto font-mono tabular-nums text-[var(--color-foreground)]">
              {datum.sample_count.toLocaleString()}
            </span>
          </div>
        )}
      </dl>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Main component
// ---------------------------------------------------------------------------

export interface RumTrendChartProps {
  /** Metric this chart visualises. */
  metric: MetricName;
  /** Daily trend series from the trend endpoint. */
  points: RumTrendPoint[];
  height?: number;
  /** Optional title rendered above the chart (e.g. "LCP - 28d p75 trend"). */
  title?: ReactNode;
}

export function RumTrendChart({
  metric,
  points,
  height = 160,
  title,
}: RumTrendChartProps) {
  const chartToken = CHART_TOKEN[metric];
  const gradientId = GRADIENT_ID[metric];

  // Map API points to chart datums. Suppressed days become y=null so the line
  // breaks (connectNulls={false}). Non-suppressed days are converted to display
  // units.
  const chartData: ChartDatum[] = points.map((pt) => ({
    day: pt.day,
    y: pt.suppressed ? null : toDisplayValue(metric, pt.p75_ms),
    sample_count: pt.sample_count,
    suppressed: pt.suppressed,
  }));

  // One scale for the whole axis, resolved once from the data maximum (D3).
  const axis = cwvYAxis(
    metric,
    cwvDataMax(chartData.map((d) => d.y)),
  );

  // Count non-suppressed points. Show ChartEmpty when fewer than 2.
  const unsuppressedCount = chartData.filter((d) => d.y !== null).length;

  const content =
    unsuppressedCount < 2 ? (
      <ChartEmpty message="Not enough data yet to show a trend" />
    ) : (
      renderChart(chartData, metric, axis, chartToken, gradientId, height)
    );

  if (!title) return <>{content}</>;

  return (
    <div>
      <p className="mb-1 text-xs font-medium text-muted-foreground">{title}</p>
      {content}
    </div>
  );
}

function renderChart(
  chartData: ChartDatum[],
  metric: MetricName,
  axis: CwvAxis,
  chartToken: string,
  gradientId: string,
  height: number,
): ReactNode {
  const interval = Math.max(0, Math.floor(chartData.length / 6) - 1);

  return (
    <div style={{ width: "100%", height }}>
      <ResponsiveContainer width="100%" height="100%">
        <AreaChart
          data={chartData}
          margin={{ top: 8, right: 12, bottom: 0, left: 0 }}
        >
          <defs>
            <linearGradient id={gradientId} x1="0" y1="0" x2="0" y2="1">
              <stop
                offset="5%"
                stopColor={chartToken}
                stopOpacity={0.18}
              />
              <stop
                offset="95%"
                stopColor={chartToken}
                stopOpacity={0.03}
              />
            </linearGradient>
          </defs>

          <CartesianGrid
            strokeDasharray="3 3"
            stroke="var(--color-border)"
            vertical={false}
          />

          <XAxis
            dataKey="day"
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

          {/*
            No `unit` prop. Recharts concatenates it onto whatever
            tickFormatter returns (CartesianAxis.js:353), which is what
            produced "3sms". The formatter owns the unit; the ban is enforced
            by components/charts/no-axis-unit.test.ts.
          */}
          <YAxis
            dataKey="y"
            domain={axis.domain}
            ticks={axis.ticks}
            tickFormatter={axis.tick}
            tick={{
              fill: "var(--color-muted-foreground)",
              fontSize: 11,
            }}
            stroke="var(--color-border)"
            tickLine={false}
            axisLine={false}
            width={axis.width}
          />

          <Tooltip
            content={(props) => (
              <TrendTooltip
                active={props.active}
                payload={props.payload as unknown as TrendTooltipProps["payload"]}
                label={typeof props.label === "string" ? props.label : undefined}
                metric={metric}
              />
            )}
            cursor={{
              stroke: "var(--color-border)",
              strokeDasharray: "3 3",
            }}
          />

          {/*
            Threshold lines use --color-success / --color-warning, matching the
            fleet chart on /performance. The old --chart-1 / --chart-4 tokens
            were the SAME colour as the LCP and INP series lines, and the
            --chart-* family has no .dark override at all, so the lines were
            both indistinguishable from the data and unverified in dark mode.
          */}
          <Area
            type="monotone"
            dataKey="y"
            name={`${METRIC_LABELS[metric]} p75`}
            stroke={chartToken}
            strokeWidth={1.5}
            fill={`url(#${gradientId})`}
            dot={false}
            activeDot={{
              r: 3,
              stroke: chartToken,
              strokeWidth: 1,
              fill: "var(--color-background)",
            }}
            isAnimationActive={false}
            connectNulls={false}
          />

          {/*
            The two thresholds render AFTER the Area on purpose. Recharts
            paints children in document order, so with the Area last the
            series and its gradient fill covered the reference lines, which is
            the one thing a pass or fail chart cannot afford to hide. Putting
            them after means the target a site is measured against is always
            legible on top of the data.
          */}
          {/* Good threshold */}
          <ReferenceLine
            y={axis.thresholds.good}
            stroke="var(--color-success)"
            strokeDasharray="4 3"
            strokeWidth={1}
            label={{
              value: cwvThresholdLabel(axis, "good"),
              position: "insideTopRight",
              fontSize: 9,
              fill: "var(--color-success-subtle-fg)",
            }}
          />

          {/* Needs-improvement threshold */}
          <ReferenceLine
            y={axis.thresholds.ni}
            stroke="var(--color-warning)"
            strokeDasharray="4 3"
            strokeWidth={1}
            label={{
              value: cwvThresholdLabel(axis, "ni"),
              position: "insideTopRight",
              fontSize: 9,
              fill: "var(--color-warning-subtle-fg)",
            }}
          />
        </AreaChart>
      </ResponsiveContainer>
    </div>
  );
}
