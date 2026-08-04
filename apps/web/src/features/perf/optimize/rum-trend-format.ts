// The tooltip formatter for RumTrendChart.
//
// This lives OUTSIDE features/perf/cwv-axis.ts on purpose, and the separation
// is the point rather than an accident of file layout.
//
// An axis and a tooltip have different jobs. An axis needs ONE scale across
// all of its ticks because the ticks are read against each other, so cwv-axis
// resolves a scale once from the axis maximum. A tooltip shows ONE value and
// must show it at the precision that value deserves, so the scale here is
// chosen from the value.
//
// GH #329 was the axis disagreeing with ITSELF (two scales on one axis, a unit
// appended twice, a threshold restated as a number Google never published). It
// was never the axis disagreeing with the tooltip, and the first attempt at
// the fix, which routed both through a single axis-derived scale, was refuted
// by measurement: because the axis domain must always contain the Good
// threshold, an LCP or FCP axis is permanently on the seconds scale, so the
// five real fast-site FCP readings 780 / 812 / 795 / 840 / 879 collapsed to
// two strings ("0.8 s" four times, then "0.9 s") where they render as five
// distinct strings here. That is the non-injectivity defect relocated out of
// the axis and into the tooltip.
//
// If you are here to "unify the formatters", read that paragraph again and
// then read RumTrendChart.test.tsx, which pins those five readings.

import type { CwvMetric } from "../cwv-axis";

/**
 * Formats one p75 reading, already converted to display units, for a tooltip.
 * Behaviour is byte-identical to what shipped before #329.
 */
export function formatTrendValue(metric: CwvMetric, value: number): string {
  if (metric === "cls") {
    return value.toFixed(3);
  }
  if (value >= 1000) {
    return `${(value / 1000).toFixed(1)} s`;
  }
  return `${value.toFixed(1)} ms`;
}
