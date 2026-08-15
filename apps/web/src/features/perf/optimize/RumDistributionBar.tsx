// RumDistributionBar — horizontal stacked rating-band bar for one CWV metric.
//
// Three segments: good (green) / needs-improvement (amber) / poor (red), sized
// by the `*_pct` fields from the summary endpoint's `distribution` object.
// The raw percentages are integers that sum to 100.
//
// Segments below ~8% hide their numeric label (not enough horizontal room) but
// the percentage is still present in the accessible aria-label / title. A min-
// width of 4px keeps every non-zero segment visible.
//
// When distribution is absent (the slice is suppressed or has no data), the
// component renders nothing — the caller is responsible for rendering the
// existing "insufficient samples" card instead of invoking this bar.
//
// `metricLabel` always reaches assistive tech (aria-label / title). It only
// reaches sighted users when a caller opts in with `showMetricLabel` — see
// that prop's doc comment for why this is opt-in rather than the default.

import { AlertCircle } from "lucide-react";
import type { RumDistribution } from "../types";

// Segment thresholds and appearance.
const LABEL_HIDE_BELOW_PCT = 8;
const MIN_SEGMENT_PX = 4;

interface Segment {
  key: "good" | "needs_improvement" | "poor";
  label: string;
  pct: number;
  bgClass: string;
  textClass: string;
}

function buildSegments(dist: RumDistribution): Segment[] {
  return [
    {
      key: "good",
      label: "Good",
      pct: dist.good_pct,
      bgClass: "bg-[var(--color-success)]",
      textClass: "text-[var(--color-success-foreground)]",
    },
    {
      key: "needs_improvement",
      label: "NI",
      pct: dist.needs_improvement_pct,
      bgClass: "bg-[var(--color-warning)]",
      textClass: "text-[var(--color-warning-foreground)]",
    },
    {
      key: "poor",
      label: "Poor",
      pct: dist.poor_pct,
      bgClass: "bg-[var(--color-destructive)]",
      textClass: "text-[var(--color-destructive-foreground)]",
    },
  ];
}

export interface RumDistributionBarProps {
  /** The metric label (e.g. "LCP") for the accessible description. */
  metricLabel: string;
  /**
   * The distribution object from the summary endpoint. Absent (undefined) when
   * the slice is suppressed or has no data; in that case the bar is not rendered.
   */
  distribution: RumDistribution | undefined;
  /**
   * When true, renders the "insufficient samples" affordance instead of the bar.
   * Mirrors the CoreMetricCard suppressed state so callers can pass the same
   * suppressed flag without conditional logic at the call site.
   */
  suppressed?: boolean;
  /** Sample count — used in the suppressed fallback copy. */
  sampleCount?: number;
  /** Required minimum count — used in the suppressed fallback copy. */
  minSampleCount?: number;
  /**
   * Render `metricLabel` as a VISIBLE label next to the bar (and in the
   * suppressed fallback), not just inside the accessible name / title.
   *
   * Opt-in: FleetRumPanel's callers each sit inside a card/row that already
   * names the metric on screen (the CoreMetricCard heading, the "Additional
   * metrics" row label), so turning this on there would duplicate it and
   * change their existing rendered output. The worst-offenders table
   * (performance.tsx) has no other on-screen home for the metric name — three
   * rows can show three different bars for three different metrics with
   * nothing else distinguishing them — so it sets this to true (GH #391).
   */
  showMetricLabel?: boolean;
}

export function RumDistributionBar({
  metricLabel,
  distribution,
  suppressed,
  sampleCount,
  minSampleCount,
  showMetricLabel,
}: RumDistributionBarProps) {
  // If suppressed or no distribution present, render the inline fallback that
  // mirrors the CoreMetricCard suppressed copy. This prevents an empty/0 bar.
  if (suppressed || !distribution) {
    if (suppressed) {
      return (
        <span
          className="mt-0.5 inline-flex items-center gap-1 text-xs text-[var(--color-muted-foreground)]"
          title={`${metricLabel}: insufficient samples`}
        >
          <AlertCircle aria-hidden="true" className="size-3 shrink-0" />
          {showMetricLabel ? (
            <span className="font-semibold text-[var(--color-foreground)]">
              {metricLabel}:
            </span>
          ) : null}
          Insufficient samples
          {sampleCount !== undefined && minSampleCount !== undefined
            ? ` (${String(sampleCount)} of ${String(minSampleCount)})`
            : null}
        </span>
      );
    }
    return null;
  }

  const segments = buildSegments(distribution);

  // Build the full aria-label for screen readers.
  const ariaLabel = [
    `${metricLabel} distribution:`,
    `${String(distribution.good_pct)}% good,`,
    `${String(distribution.needs_improvement_pct)}% needs improvement,`,
    `${String(distribution.poor_pct)}% poor.`,
  ].join(" ");

  return (
    <div className="mt-2 w-full">
      {/* Metric label (opt-in) + stacked bar, one row so no extra height */}
      <div className="flex items-center gap-1.5">
        {showMetricLabel ? (
          <span
            aria-hidden="true"
            className="shrink-0 whitespace-nowrap text-[9px] font-semibold uppercase tracking-wide text-[var(--color-muted-foreground)]"
          >
            {metricLabel}
          </span>
        ) : null}
        <div
          role="img"
          aria-label={ariaLabel}
          title={ariaLabel}
          className="flex h-4 min-w-0 flex-1 overflow-hidden rounded-full"
          style={{ minHeight: "1rem" }}
        >
          {segments.map((seg) => {
            if (seg.pct <= 0) return null;
            return (
              <div
                key={seg.key}
                className={`relative flex items-center justify-center ${seg.bgClass}`}
                style={{
                  width: `${String(seg.pct)}%`,
                  minWidth:
                    seg.pct > 0 ? `${String(MIN_SEGMENT_PX)}px` : undefined,
                }}
              >
                {seg.pct >= LABEL_HIDE_BELOW_PCT ? (
                  <span
                    className={`select-none text-[10px] font-semibold leading-none tabular-nums ${seg.textClass}`}
                    aria-hidden="true"
                  >
                    {String(seg.pct)}%
                  </span>
                ) : null}
              </div>
            );
          })}
        </div>
      </div>

      {/* Legend row */}
      <div
        className="mt-1 flex items-center gap-3 text-[10px] text-[var(--color-muted-foreground)]"
        aria-hidden="true"
      >
        {segments.map((seg) => (
          <span key={seg.key} className="inline-flex items-center gap-1">
            <span
              className={`inline-block size-2 rounded-sm ${seg.bgClass}`}
            />
            {seg.label}
            {": "}
            {String(seg.pct)}%
          </span>
        ))}
      </div>
    </div>
  );
}
