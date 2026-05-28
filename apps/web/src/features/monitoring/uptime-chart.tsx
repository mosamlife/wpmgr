import { useId } from "react";
import type { UptimePoint } from "@wpmgr/api";

// Dependency-light latency/uptime chart rendered as inline SVG (ADR-018 chose
// Tremor/Recharts, but those are not installed; an inline SVG keeps the M5
// bundle small and fully controllable). It draws an area sparkline of per-bucket
// average latency (`avg_latency_ms`) and marks any bucket with at least one
// down sample (`up_checks < checks`) with a red tick along the baseline. The
// chart is decorative (aria-hidden via role="img") and ships with a real text
// alternative (a visually-hidden table) so screen-reader users get the data.

const WIDTH = 600;
const HEIGHT = 120;
const PAD = 4;

function formatTs(ts: string): string {
  const d = new Date(ts);
  return Number.isNaN(d.getTime()) ? ts : d.toLocaleString();
}

/** A bucket has downtime when at least one probe within it was not up. */
function hasDowntime(p: UptimePoint): boolean {
  return p.checks > 0 && p.up_checks < p.checks;
}

/** Up-ratio for a bucket as a percentage (0 when there were no checks). */
function upRatioPct(p: UptimePoint): number {
  if (p.checks <= 0) return 0;
  return (p.up_checks / p.checks) * 100;
}

export function UptimeChart({ series }: { series: UptimePoint[] }) {
  const gradientId = useId();

  if (series.length === 0) {
    return (
      <p className="text-sm text-[var(--color-muted-foreground)]">
        No probe data in this window.
      </p>
    );
  }

  const latencies = series.map((p) => p.avg_latency_ms);
  const maxLatency = Math.max(...latencies, 1);
  const innerW = WIDTH - PAD * 2;
  const innerH = HEIGHT - PAD * 2;
  const stepX = series.length > 1 ? innerW / (series.length - 1) : 0;

  const x = (i: number) => PAD + (series.length > 1 ? i * stepX : innerW / 2);
  const y = (ms: number) => PAD + innerH - (ms / maxLatency) * innerH;

  const linePoints = series
    .map((p, i) => `${x(i)},${y(p.avg_latency_ms)}`)
    .join(" ");
  const areaPath =
    `M ${x(0)},${PAD + innerH} ` +
    series.map((p, i) => `L ${x(i)},${y(p.avg_latency_ms)}`).join(" ") +
    ` L ${x(series.length - 1)},${PAD + innerH} Z`;

  const downBuckets = series.filter(hasDowntime).length;

  return (
    <figure className="space-y-2">
      <svg
        viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
        className="h-32 w-full"
        preserveAspectRatio="none"
        role="img"
        aria-label={`Latency over time: ${series.length} buckets, peak ${maxLatency} ms${
          downBuckets > 0 ? `, ${downBuckets} with downtime` : ""
        }`}
        data-testid="uptime-chart"
      >
        <defs>
          <linearGradient id={gradientId} x1="0" y1="0" x2="0" y2="1">
            <stop
              offset="0%"
              stopColor="var(--color-primary)"
              stopOpacity="0.35"
            />
            <stop
              offset="100%"
              stopColor="var(--color-primary)"
              stopOpacity="0"
            />
          </linearGradient>
        </defs>
        <path d={areaPath} fill={`url(#${gradientId})`} stroke="none" />
        <polyline
          points={linePoints}
          fill="none"
          stroke="var(--color-primary)"
          strokeWidth={1.5}
          vectorEffect="non-scaling-stroke"
        />
        {series.map((p, i) =>
          hasDowntime(p) ? (
            <line
              key={p.bucket}
              x1={x(i)}
              x2={x(i)}
              y1={PAD + innerH - 6}
              y2={PAD + innerH}
              stroke="var(--color-destructive)"
              strokeWidth={2}
              vectorEffect="non-scaling-stroke"
            />
          ) : null,
        )}
      </svg>

      {/* Accessible text alternative for the chart. */}
      <figcaption className="sr-only">
        <table>
          <caption>Probe results over the selected window</caption>
          <thead>
            <tr>
              <th scope="col">Bucket start</th>
              <th scope="col">Checks</th>
              <th scope="col">Up checks</th>
              <th scope="col">Up %</th>
              <th scope="col">Avg latency (ms)</th>
            </tr>
          </thead>
          <tbody>
            {series.map((p) => (
              <tr key={p.bucket}>
                <td>{formatTs(p.bucket)}</td>
                <td>{p.checks}</td>
                <td>{p.up_checks}</td>
                <td>{upRatioPct(p).toFixed(1)}%</td>
                <td>{p.avg_latency_ms}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </figcaption>
    </figure>
  );
}
