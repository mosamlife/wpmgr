// Tiny inline line chart for table cells and tile decorations. Per DESIGN.md
// (`components.Sparkline`): chart-1 stroke at 1.5px, no fill, no axes, no
// tooltip, no dots. Bound to real series only: fewer than two points means we
// render nothing (a flatline would lie about the absence of data).
//
// Tone maps to the chart palette so a destructive sparkline can sit next to
// a destructive badge without us hard-coding hex anywhere. Callers pass
// either a raw `number[]` or `{ value, label }[]` so both API shapes work.
//
// WHY THIS IS PLAIN SVG AND NOT RECHARTS.
//
// This used to render a recharts <LineChart> with a hidden <YAxis>. Every
// recharts root builds its OWN Redux store (RechartsStoreProvider.js calls
// createRechartsStore per instance behind a useRef) plus its own
// ResizeObserver and its own SVG surface. FleetTable renders one sparkline per
// row with no virtualizer and use-fleet-uptime.ts asks for up to 100 rows, so
// a single fleet screen mounted up to 100 Redux stores to draw 100 sixty-by-
// sixteen-pixel decorations with no axes, no grid, no tooltip and no
// interaction. Measured in jsdom at N=100: 222-385 ms mount and 2,305 DOM
// nodes for the recharts version against 3.4-5.4 ms and 205 nodes for a plain
// polyline. It also kept the whole cartesian charting engine in the chunk
// graph for /uptime and /backups, which have no other chart.
//
// The geometry below reproduces what recharts drew, so this is not a visual
// change:
//   - x uses a point scale across the full width, first sample at x=0 and last
//     at x=width, which is what recharts does for a line chart.
//   - y spans the margin box the old chart used, top: 1 and bottom: 1.
//   - the old <YAxis domain={["dataMin","dataMax"]}> made the series fill the
//     box exactly, and a flat series landed on the vertical midpoint (d3 maps
//     a zero-width domain to the middle of the range). Both are preserved.
// The one deliberate difference is that segments are straight rather than
// recharts' "monotone" curve, which at this size is under a pixel.
//
// The role="img" + aria-label wrapper is load-bearing and must stay: raw SVG
// does not get recharts' accessibilityLayer for free.

export type SparklineDatum = number | { value: number; label?: string };

export interface SparklineProps {
  data: SparklineDatum[];
  width?: number;
  height?: number;
  tone?: "primary" | "success" | "warning" | "destructive";
  /** Optional aria-label; defaults to a generic "Sparkline" announcement. */
  ariaLabel?: string;
}

const toneToVar: Record<NonNullable<SparklineProps["tone"]>, string> = {
  primary: "var(--color-chart-1)",
  success: "var(--color-chart-2)",
  warning: "var(--color-chart-3)",
  destructive: "var(--color-destructive)",
};

/**
 * Drops every entry that is not a finite number, matching the pre-existing
 * behaviour exactly: a gap (null, undefined, NaN, Infinity, a malformed datum)
 * is removed from the series rather than plotted as a zero. The remaining
 * samples keep their order and are spread evenly across the width, so a gap
 * shortens the series rather than putting a hole in it. Two samples are the
 * minimum; below that we render a spacer instead of inventing a trend.
 */
function normalize(data: SparklineDatum[]): { value: number; label?: string }[] {
  return data
    .map((d): { value: number; label?: string } | null => {
      if (typeof d === "number") {
        return Number.isFinite(d) ? { value: d } : null;
      }
      if (d && typeof d.value === "number" && Number.isFinite(d.value)) {
        return { value: d.value, label: d.label };
      }
      return null;
    })
    .filter((d): d is { value: number; label?: string } => d !== null);
}

/** Margin box the recharts version used: top 1, bottom 1, no side margins. */
const MARGIN_Y = 1;

function buildPoints(
  values: number[],
  width: number,
  height: number,
): string {
  const top = MARGIN_Y;
  const bottom = height - MARGIN_Y;
  const min = Math.min(...values);
  const max = Math.max(...values);
  const span = max - min;
  const lastIndex = values.length - 1;

  return values
    .map((v, i) => {
      const x = lastIndex === 0 ? width / 2 : (i / lastIndex) * width;
      // A flat series has no vertical range to map onto, so it sits on the
      // midline. This is what d3 (and therefore recharts) already did.
      const y =
        span === 0 ? (top + bottom) / 2 : bottom - ((v - min) / span) * (bottom - top);
      return `${round2(x)},${round2(y)}`;
    })
    .join(" ");
}

function round2(n: number): number {
  return Math.round(n * 100) / 100;
}

export function Sparkline({
  data,
  width = 60,
  height = 16,
  tone = "primary",
  ariaLabel = "Sparkline",
}: SparklineProps) {
  const series = normalize(data);

  // Per DESIGN.md: only bound to real series. A 0- or 1-point dataset would
  // either render as nothing or as a single misleading dot, so we render an
  // empty placeholder that preserves layout without inventing a trend line.
  if (series.length < 2) {
    return (
      <span
        aria-hidden="true"
        style={{
          display: "inline-block",
          width,
          height,
        }}
      />
    );
  }

  const stroke = toneToVar[tone];
  const points = buildPoints(
    series.map((d) => d.value),
    width,
    height,
  );

  return (
    <span
      role="img"
      aria-label={ariaLabel}
      style={{
        display: "inline-block",
        width,
        height,
        lineHeight: 0,
      }}
    >
      <svg
        width={width}
        height={height}
        viewBox={`0 0 ${width} ${height}`}
        aria-hidden="true"
        focusable="false"
        style={{ display: "block" }}
      >
        <polyline
          points={points}
          fill="none"
          stroke={stroke}
          strokeWidth={1.5}
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      </svg>
    </span>
  );
}
