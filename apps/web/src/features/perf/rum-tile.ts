import type { RumMetricSummary, RumSummary } from "./types";

// Pure helpers for a single-metric CWV headline (the Health tab's Performance
// tile). LCP is the headline metric — first among the three core CWV metrics
// in FleetRumPanel / RumResultsTable, and the metric most commonly cited as
// the primary "loading experience" signal.
//
// The all-devices aggregate row is device:"" per the CP handler
// (apps/api/internal/perf/rum_results_handler.go — see
// "All-devices aggregate (device=\"\" sentinel)"), mirrored here exactly the
// same way FleetRumPanel.extractSlice matches it.

/**
 * Finds the all-devices aggregate row for the given metric in a RUM summary.
 * Returns undefined when no row exists for that metric at all (not just when
 * suppressed — callers must check `.suppressed` separately since a suppressed
 * row is still a real row that must not show a p75 value).
 */
export function findAggregateMetric(
  summary: RumSummary,
  metric: NonNullable<RumMetricSummary["metric"]>,
): RumMetricSummary | undefined {
  return (summary.metrics ?? []).find(
    (m) => m.metric === metric && (!m.device || m.device === ("" as string)),
  );
}

export const RUM_RATING_LABEL: Record<
  NonNullable<RumMetricSummary["rating"]>,
  string
> = {
  good: "Good",
  needs_improvement: "Needs work",
  poor: "Poor",
};

export const RUM_RATING_CLASS: Record<
  NonNullable<RumMetricSummary["rating"]>,
  string
> = {
  good: "text-green-700 dark:text-green-400",
  needs_improvement: "text-amber-700 dark:text-amber-400",
  poor: "text-red-700 dark:text-red-400",
};

/** Formats a p75 value in milliseconds for display (LCP/INP/FCP/TTFB units). */
export function formatLcpP75(p75Ms: number): string {
  return p75Ms >= 1000
    ? `${(p75Ms / 1000).toFixed(2)} s`
    : `${Math.round(p75Ms)} ms`;
}
