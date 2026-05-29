// Tiny inline line chart for table cells and tile decorations. Per DESIGN.md
// (`components.Sparkline`): chart-1 stroke at 1.5px, no fill, no axes, no
// tooltip, no dots. Bound to real series only — fewer than two points means
// we render nothing (a flatline would lie about the absence of data).
//
// Tone maps to the chart palette so a destructive sparkline can sit next to
// a destructive badge without us hard-coding hex anywhere. Callers pass
// either a raw `number[]` or `{ value, label }[]` so both API shapes work.

import { LineChart, Line, ResponsiveContainer, YAxis } from "recharts";

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
      <ResponsiveContainer width={width} height={height}>
        <LineChart
          data={series}
          margin={{ top: 1, right: 0, bottom: 1, left: 0 }}
        >
          {/* YAxis hidden but configured so the line uses the full vertical
              range — without it Recharts pads the domain and the spark looks
              squashed in a 16px box. */}
          <YAxis hide domain={["dataMin", "dataMax"]} />
          <Line
            type="monotone"
            dataKey="value"
            stroke={stroke}
            strokeWidth={1.5}
            dot={false}
            activeDot={false}
            isAnimationActive={false}
          />
        </LineChart>
      </ResponsiveContainer>
    </span>
  );
}
