import { describe, it, expect } from "vitest";

import {
  findAggregateMetric,
  formatLcpP75,
  RUM_RATING_LABEL,
  RUM_RATING_CLASS,
} from "./rum-tile";
import type { RumMetricSummary, RumSummary } from "./types";

// ---------------------------------------------------------------------------
// findAggregateMetric — must match the CP's all-devices sentinel exactly
//
// apps/api/internal/perf/rum_results_handler.go builds an "all-devices" row
// per metric with device:"" (country-collapsed too). A frontend contract
// mismatch here (e.g. expecting device:"all" instead of "") would silently
// drop the aggregate row and make the tile show "No visitor data yet" even
// though the site has plenty of RUM samples — the same class of bug fixed by
// the Go-side "All tab No data" regression test.
//
// The generated SDK type narrows `device` to "desktop" | "mobile" | "tablet"
// (it does not model the "" wire sentinel), the same generated-type gap
// FleetRumPanel works around with its own `RawMetricRow` cast. `wireRow`
// mirrors that same workaround for fixture construction here.
// ---------------------------------------------------------------------------

function wireRow(row: {
  metric: NonNullable<RumMetricSummary["metric"]>;
  device?: string;
  p75_ms: number;
  sample_count: number;
  rating?: NonNullable<RumMetricSummary["rating"]>;
  suppressed: boolean;
}): RumMetricSummary {
  return { ...row, device: row.device as RumMetricSummary["device"] };
}

function makeSummary(metrics: RumSummary["metrics"]): RumSummary {
  return { window_days: 28, min_sample_count: 30, metrics };
}

describe("findAggregateMetric", () => {
  it("finds the device:\"\" aggregate row for the requested metric", () => {
    const summary = makeSummary([
      wireRow({ metric: "lcp", device: "desktop", p75_ms: 1800, sample_count: 40, rating: "good", suppressed: false }),
      wireRow({ metric: "lcp", device: "mobile", p75_ms: 2600, sample_count: 60, rating: "needs_improvement", suppressed: false }),
      wireRow({ metric: "lcp", device: "", p75_ms: 2300, sample_count: 100, rating: "needs_improvement", suppressed: false }),
    ]);
    const row = findAggregateMetric(summary, "lcp");
    expect(row).toBeDefined();
    expect(row?.device).toBe("");
    expect(row?.sample_count).toBe(100);
  });

  it("also matches a row where device is entirely absent (undefined)", () => {
    const summary = makeSummary([
      wireRow({ metric: "lcp", p75_ms: 2100, sample_count: 50, rating: "good", suppressed: false }),
    ]);
    const row = findAggregateMetric(summary, "lcp");
    expect(row).toBeDefined();
    expect(row?.p75_ms).toBe(2100);
  });

  it("does not match a per-device row (desktop/mobile/tablet) as the aggregate", () => {
    const summary = makeSummary([
      wireRow({ metric: "lcp", device: "desktop", p75_ms: 1800, sample_count: 40, rating: "good", suppressed: false }),
    ]);
    expect(findAggregateMetric(summary, "lcp")).toBeUndefined();
  });

  it("returns undefined when the metric has no rows at all", () => {
    const summary = makeSummary([
      wireRow({ metric: "inp", device: "", p75_ms: 150, sample_count: 40, rating: "good", suppressed: false }),
    ]);
    expect(findAggregateMetric(summary, "lcp")).toBeUndefined();
  });

  it("returns undefined when metrics is missing entirely", () => {
    const summary: RumSummary = { window_days: 28, min_sample_count: 30 };
    expect(findAggregateMetric(summary, "lcp")).toBeUndefined();
  });

  it("returns a suppressed row as-is — callers must check .suppressed before showing a p75", () => {
    const summary = makeSummary([
      wireRow({ metric: "lcp", device: "", p75_ms: 0, sample_count: 5, suppressed: true }),
    ]);
    const row = findAggregateMetric(summary, "lcp");
    expect(row).toBeDefined();
    expect(row?.suppressed).toBe(true);
  });

  it("picks the row matching the requested metric among several metrics", () => {
    const summary = makeSummary([
      wireRow({ metric: "lcp", device: "", p75_ms: 2000, sample_count: 80, rating: "good", suppressed: false }),
      wireRow({ metric: "inp", device: "", p75_ms: 150, sample_count: 80, rating: "good", suppressed: false }),
      wireRow({ metric: "cls", device: "", p75_ms: 50, sample_count: 80, rating: "good", suppressed: false }),
    ]);
    expect(findAggregateMetric(summary, "inp")?.p75_ms).toBe(150);
    expect(findAggregateMetric(summary, "cls")?.p75_ms).toBe(50);
  });
});

// ---------------------------------------------------------------------------
// formatLcpP75 — display formatting
// ---------------------------------------------------------------------------

describe("formatLcpP75", () => {
  it("formats sub-1000ms values in milliseconds", () => {
    expect(formatLcpP75(850)).toBe("850 ms");
  });

  it("rounds fractional milliseconds", () => {
    expect(formatLcpP75(849.6)).toBe("850 ms");
  });

  it("formats 1000ms and above in seconds with two decimals", () => {
    expect(formatLcpP75(1000)).toBe("1.00 s");
    expect(formatLcpP75(2456)).toBe("2.46 s");
  });

  it("formats zero as 0 ms (never shown for a suppressed row by the caller, but the formatter itself is total)", () => {
    expect(formatLcpP75(0)).toBe("0 ms");
  });

  it("keeps the two decimals that straddle the Good boundary (GH #329)", () => {
    // The Health tab prints this number colour-coded by its rating band
    // (routes/_authed/sites/$siteId.health.tsx). Dropping to one decimal, as an
    // earlier attempt at the #329 fix proposed, would render 2460 ms (Good,
    // green) and 2540 ms (Needs work, amber) both as "2.5 s", so the number
    // would contradict the colour it is printed in. Do not reduce precision
    // here to match the chart axis: an axis and a single reading have
    // different jobs. See features/perf/cwv-axis.ts.
    expect(formatLcpP75(2460)).toBe("2.46 s");
    expect(formatLcpP75(2540)).toBe("2.54 s");
    expect(formatLcpP75(2460)).not.toBe(formatLcpP75(2540));
  });
});

// ---------------------------------------------------------------------------
// RUM_RATING_LABEL / RUM_RATING_CLASS — every rating band has a label + class
// ---------------------------------------------------------------------------

describe("RUM_RATING_LABEL and RUM_RATING_CLASS", () => {
  it("cover all three CWV rating bands", () => {
    expect(RUM_RATING_LABEL.good).toBe("Good");
    expect(RUM_RATING_LABEL.needs_improvement).toBe("Needs work");
    expect(RUM_RATING_LABEL.poor).toBe("Poor");
  });

  it("every rating has a non-empty color class", () => {
    for (const rating of ["good", "needs_improvement", "poor"] as const) {
      expect(RUM_RATING_CLASS[rating].length).toBeGreaterThan(0);
    }
  });
});
