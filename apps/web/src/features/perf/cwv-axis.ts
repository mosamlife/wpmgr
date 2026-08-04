// Y-axis geometry and tick rendering for the Core Web Vitals trend charts
// (features/perf/optimize/RumTrendChart.tsx and the fleet trend chart in
// routes/_authed/performance.tsx).
//
// THE INVARIANT THIS MODULE OWNS:
//   ONE SCALE PER AXIS, and ONE OWNER OF THE UNIT STRING.
//
// GH #329. The LCP axis printed "5sms" / "3sms" and the Good threshold label
// read "Good 3sms" where the real Web Vitals target is 2500 ms. Five separate
// causes, all closed structurally here rather than patched at a call site:
//
//  D1 DOUBLE UNIT. Recharts appends the YAxis `unit` prop to whatever
//     tickFormatter returns, unconditionally
//     (recharts/es6/cartesian/CartesianAxis.js:353). The old code both set
//     unit="ms" and returned a "s" suffix from the formatter, so every tick
//     got two units. The tick formatter here is the ONLY owner of the unit
//     string and no chart may ever set the recharts `unit` prop again. That
//     rule is enforced by src/components/charts/no-axis-unit.test.ts, which
//     scans source so the defect cannot come back at a new call site.
//
//  D2 A WRONG THRESHOLD VALUE, the serious one. toFixed(0) on seconds turned
//     2500 into "3s", so the chart stated a Web Vitals target that does not
//     exist (and 1800 into "2s" for FCP and TTFB). Every CWV threshold is a
//     whole multiple of 100 ms, so one decimal of seconds states all ten of
//     them exactly. cwv-axis.test.ts round-trips every threshold back through
//     its own formatter to prove no label can lie again.
//
//  D3 TWO SCALES ON ONE AXIS. The old formatter chose ms or s per tick, so
//     650 and 2000 landed on the same axis as "650ms" and "2sms". A scale is a
//     property of the AXIS, not of a tick, because ticks are read against each
//     other. It is resolved once, in cwvYAxis, from the axis maximum.
//
//  D3b A NON-INJECTIVE AXIS. Following from D2, on a 0..4000 LCP axis the
//     values 1500 / 1600 / 2000 / 2400 all rendered "2sms": four different
//     heights, one identical string. Ticks are returned explicitly and are
//     always exact in the chosen scale, so two distinct ticks can never render
//     the same text.
//
//  D4 SILENTLY DISCARDED THRESHOLD LINES. With domain ["auto","auto"] recharts
//     drops a ReferenceLine whose y falls outside the domain (ifOverflow
//     defaults to "discard": ReferenceLine.js:223 and :79) and only widens a
//     domain for reference elements marked "extendDomain"
//     (axisSelectors.js:673). A site comfortably inside the Good band
//     therefore rendered NEITHER threshold line, and an operator could not
//     tell "you are passing" from "no thresholds are configured". cwvYAxis
//     returns an explicit domain that always contains the Good threshold.
//
//  D5 is fixed at the call sites: the threshold lines use --color-success and
//     --color-warning (both overridden in .dark) rather than the --chart-*
//     tokens, which collided with the series colour on LCP and INP and have no
//     .dark override at all.
//
// WHY THE TOOLTIP DOES NOT USE THIS SCALE. Deliberate. Do not "helpfully"
// unify them.
//
// An axis and a tooltip have different jobs. An axis needs a single scale
// across all of its ticks because the ticks are read against each other. A
// tooltip shows ONE value and must show it at the precision that value
// deserves. #329 was the axis disagreeing with ITSELF and the unit being
// appended twice; it was never the axis disagreeing with the tooltip.
//
// An earlier version of this fix routed the tooltip through the axis scale and
// was refuted by measurement. Because the domain must always contain the Good
// threshold, an FCP axis maximum is never below 2000 ms and is therefore
// permanently on the seconds scale. On real fast-site data the five distinct
// FCP p75 readings 780 / 812 / 795 / 840 / 879 collapsed to two strings
// ("0.8 s" four times, then "0.9 s") where the tooltip renders five distinct
// strings today. That is D3b relocated out of the axis and into the tooltip.
// The same unification made the Health tile ambiguous at the band boundary:
// 2460 ms (Good, green) and 2540 ms (Needs work, amber) would both read
// "2.5 s", so the number would contradict the colour it is printed in.
//
// RumTrendChart.formatTrendValue keeps the per-value rule and is pinned by
// RumTrendChart.test.tsx; features/perf/rum-tile.ts formatLcpP75 keeps its two
// decimals for the same reason.

export type CwvMetric = "lcp" | "inp" | "cls" | "fcp" | "ttfb";

const CWV_METRICS: readonly string[] = ["lcp", "inp", "cls", "fcp", "ttfb"];

export function isCwvMetric(value: string): value is CwvMetric {
  return CWV_METRICS.includes(value);
}

/**
 * Official CWV thresholds in DISPLAY units: milliseconds for the timing
 * metrics, a unitless score for CLS. Callers that hold raw wire values must
 * convert CLS (sent in milli-units) before comparing against these.
 */
export const CWV_THRESHOLDS: Record<CwvMetric, { good: number; ni: number }> = {
  lcp: { good: 2500, ni: 4000 },
  inp: { good: 200, ni: 500 },
  cls: { good: 0.1, ni: 0.25 },
  fcp: { good: 1800, ni: 3000 },
  ttfb: { good: 800, ni: 1800 },
};

/** Ticks per axis, including both endpoints. */
export const CWV_TICK_COUNT = 5;

/**
 * Multiplier applied to whichever is larger, the data maximum or the Good
 * threshold. It buys the topmost line enough clearance that its label is not
 * pinned against the plot edge.
 */
const HEADROOM = 1.08;

/**
 * Axis maxima at or below this render in milliseconds; above it, in seconds.
 * The switch is per AXIS, never per tick (D3).
 */
const MS_AXIS_LIMIT = 1000;

/**
 * Step mantissas, in the order a human would pick them. Combined with the
 * Good-threshold floor below, every reachable step is exact in the scale the
 * axis ends up on: the smallest possible timing step is 100 ms (from INP,
 * whose Good threshold is 200), and every step at or above 500 ms is a whole
 * multiple of 100 ms, which one decimal of seconds represents exactly.
 * cwv-axis.test.ts sweeps this rather than trusting the argument.
 */
const STEP_MANTISSAS: readonly number[] = [1, 2, 2.5, 5];

function round6(n: number): number {
  return Number(n.toFixed(6));
}

/** Smallest mantissa-times-power-of-ten step that is at least `minStep`. */
function niceStep(minStep: number): number {
  const target = minStep > 0 ? minStep : 1;
  const exp = Math.floor(Math.log10(target));
  for (let k = exp; k <= exp + 3; k += 1) {
    for (const mantissa of STEP_MANTISSAS) {
      const step = mantissa * 10 ** k;
      if (step >= target - 1e-9) return round6(step);
    }
  }
  return round6(10 ** (exp + 4));
}

export interface CwvAxis {
  /** Explicit numeric domain. Always contains the Good threshold (D4). */
  domain: [number, number];
  /**
   * Explicit tick values. Recharts picks its own step from a numeric domain
   * (axisSelectors.js:787-815 getTickValuesFixedDomain), and a 750 ms tick on
   * a one-decimal seconds axis would print "0.8", which is D2 at a smaller
   * magnitude. Exactness is ours to guarantee, not the library's.
   */
  ticks: number[];
  /** "ms", "s", or "" for CLS. ONE unit for the whole axis. */
  unit: string;
  /** Renders a value in this axis's single scale, unit included. */
  tick: (value: number) => string;
  /** YAxis width in px, sized to the widest label this axis will render. */
  width: number;
  /** The metric's thresholds in display units, for the ReferenceLines. */
  thresholds: { good: number; ni: number };
}

function makeTickFormatter(unit: string): (value: number) => string {
  if (unit === "s") return (value) => `${(value / 1000).toFixed(1)}s`;
  if (unit === "ms") return (value) => `${Math.round(value)}ms`;
  // CLS is unitless. Two decimals states both thresholds (0.10 and 0.25)
  // exactly and every reachable tick step is a whole multiple of 0.01.
  return (value) => value.toFixed(2);
}

/**
 * Resolves the whole Y axis for one CWV chart in a single call: domain, ticks,
 * unit, formatter and width. `dataMax` is the largest plotted value in DISPLAY
 * units (CLS already divided by 1000).
 */
export function cwvYAxis(metric: CwvMetric, dataMax: number): CwvAxis {
  const thresholds = CWV_THRESHOLDS[metric];
  const observed = Number.isFinite(dataMax) && dataMax > 0 ? dataMax : 0;

  // The Good line is always in frame. A pass-or-fail chart that hides the
  // target it measures against is not a pass-or-fail chart. The NI line is
  // allowed to fall outside: a site nowhere near it does not need it drawn.
  const needed = Math.max(observed, thresholds.good) * HEADROOM;

  const step = niceStep(needed / (CWV_TICK_COUNT - 1));
  const max = round6(step * (CWV_TICK_COUNT - 1));
  const ticks = Array.from({ length: CWV_TICK_COUNT }, (_, i) =>
    round6(step * i),
  );

  const unit = metric === "cls" ? "" : max > MS_AXIS_LIMIT ? "s" : "ms";
  const tick = makeTickFormatter(unit);

  const longest = ticks.reduce((n, t) => Math.max(n, tick(t).length), 0);
  const width = Math.min(56, Math.max(34, Math.round(10 + longest * 6.5)));

  return { domain: [0, max], ticks, unit, tick, width, thresholds };
}

/**
 * The text for a threshold ReferenceLine. Rendered through the axis formatter
 * so the label can never disagree with the ticks it sits between, and so it
 * can never restate the threshold as a number Google never published.
 */
export function cwvThresholdLabel(axis: CwvAxis, band: "good" | "ni"): string {
  const prefix = band === "good" ? "Good" : "NI";
  return `${prefix} ${axis.tick(axis.thresholds[band])}`;
}

/** Largest plotted value across a series, ignoring gaps. 0 when all are gaps. */
export function cwvDataMax(values: Array<number | null>): number {
  return values.reduce<number>(
    (max, v) => (v !== null && Number.isFinite(v) && v > max ? v : max),
    0,
  );
}
