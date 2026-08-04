// GH #329. The LCP trend chart printed "3sms" where the Web Vitals Good
// threshold is 2500 ms, and the axis rendered two different scales at once.
// These tests pin the invariant that replaced it: ONE SCALE PER AXIS, and one
// owner of the unit string.
//
// Every assertion here is written to fail against the pre-fix code. The old
// tick formatter was:
//
//   metric === "cls" ? v.toFixed(2)
//                    : v >= 1000 ? `${(v / 1000).toFixed(0)}s` : `${v}`
//
// plus a recharts `unit="ms"` prop that recharts appended to that output.

import { describe, it, expect } from "vitest";

import {
  CWV_THRESHOLDS,
  CWV_TICK_COUNT,
  cwvDataMax,
  cwvThresholdLabel,
  cwvYAxis,
  isCwvMetric,
  type CwvMetric,
} from "./cwv-axis";

const METRICS: CwvMetric[] = ["lcp", "inp", "cls", "fcp", "ttfb"];

/** Reads a rendered tick back into the axis's own display units. */
function parseTick(text: string, unit: string): number {
  const numeric = Number.parseFloat(text);
  return unit === "s" ? numeric * 1000 : numeric;
}

/** A spread of plausible data maxima per metric, from "no data" to "awful". */
function sweep(metric: CwvMetric): number[] {
  const { good, ni } = CWV_THRESHOLDS[metric];
  return [
    0,
    good * 0.01,
    good * 0.4,
    good * 0.99,
    good,
    good * 1.01,
    ni * 0.9,
    ni,
    ni * 1.5,
    ni * 3,
    ni * 10,
  ];
}

describe("cwvYAxis -- the reported defect (GH #329)", () => {
  it("renders the LCP Good threshold as 2.5s, never 3sms and never 3s", () => {
    const axis = cwvYAxis("lcp", 2100);

    expect(axis.tick(2500)).toBe("2.5s");
    expect(cwvThresholdLabel(axis, "good")).toBe("Good 2.5s");
    expect(cwvThresholdLabel(axis, "ni")).toBe("NI 4.0s");

    // The exact strings from the reporter's screenshot.
    expect(cwvThresholdLabel(axis, "good")).not.toBe("Good 3sms");
    expect(cwvThresholdLabel(axis, "ni")).not.toBe("NI 4sms");
  });

  it("never emits a doubled unit on any tick of any metric (D1)", () => {
    for (const metric of METRICS) {
      for (const dataMax of sweep(metric)) {
        const axis = cwvYAxis(metric, dataMax);
        for (const t of axis.ticks) {
          expect(axis.tick(t)).not.toMatch(/sms|msms|ss$/);
        }
      }
    }
  });

  it("states every one of the ten thresholds exactly (D2)", () => {
    for (const metric of METRICS) {
      const { good, ni } = CWV_THRESHOLDS[metric];
      for (const dataMax of sweep(metric)) {
        const axis = cwvYAxis(metric, dataMax);
        const tolerance = metric === "cls" ? 0.0005 : 0.5;
        // The pre-fix formatter turned LCP 2500 into "3s", an error of 500 ms.
        expect(Math.abs(parseTick(axis.tick(good), axis.unit) - good)).toBeLessThan(
          tolerance,
        );
        expect(Math.abs(parseTick(axis.tick(ni), axis.unit) - ni)).toBeLessThan(
          tolerance,
        );
      }
    }
  });

  it("puts every tick of one axis on the same scale (D3)", () => {
    for (const metric of METRICS) {
      for (const dataMax of sweep(metric)) {
        const axis = cwvYAxis(metric, dataMax);
        const suffixes = new Set(
          axis.ticks.map((t) => axis.tick(t).replace(/^[\d.]+/, "")),
        );
        expect(suffixes.size, `${metric} @ ${dataMax}`).toBe(1);
        expect([...suffixes][0]).toBe(axis.unit);
      }
    }
  });

  it("puts 650 and 2500 on one LCP axis in the same scale (D3)", () => {
    const axis = cwvYAxis("lcp", 2600);
    // Pre-fix this was ["650", "2s"] with a stray "ms" appended to each.
    expect([650, 2000, 2500].map(axis.tick)).toEqual(["0.7s", "2.0s", "2.5s"]);
  });

  it("never renders two distinct ticks identically (D3b)", () => {
    for (const metric of METRICS) {
      for (const dataMax of sweep(metric)) {
        const axis = cwvYAxis(metric, dataMax);
        const rendered = axis.ticks.map(axis.tick);
        expect(new Set(rendered).size, `${metric} @ ${dataMax}`).toBe(
          new Set(axis.ticks).size,
        );
        expect(rendered).toHaveLength(CWV_TICK_COUNT);
      }
    }
  });

  it("keeps the Good threshold inside the domain for every input (D4)", () => {
    for (const metric of METRICS) {
      const { good } = CWV_THRESHOLDS[metric];
      // Including hostile inputs: no data at all, NaN, negatives.
      for (const dataMax of [...sweep(metric), Number.NaN, -1, Infinity]) {
        const axis = cwvYAxis(metric, dataMax);
        expect(axis.domain[0], `${metric} @ ${dataMax}`).toBe(0);
        expect(axis.domain[1], `${metric} @ ${dataMax}`).toBeGreaterThanOrEqual(
          good,
        );
      }
    }
  });

  it("keeps the NI threshold in frame once the data reaches it (D4)", () => {
    for (const metric of METRICS) {
      const { ni } = CWV_THRESHOLDS[metric];
      const axis = cwvYAxis(metric, ni);
      expect(axis.domain[1], metric).toBeGreaterThanOrEqual(ni);
    }
  });
});

describe("cwvYAxis -- axis mechanics", () => {
  it("keeps every plotted value inside the domain", () => {
    for (const metric of METRICS) {
      for (const dataMax of sweep(metric)) {
        const axis = cwvYAxis(metric, dataMax);
        expect(axis.domain[1], `${metric} @ ${dataMax}`).toBeGreaterThanOrEqual(
          dataMax,
        );
      }
    }
  });

  it("renders every tick exactly, losing no precision to the formatter", () => {
    for (const metric of METRICS) {
      for (const dataMax of sweep(metric)) {
        const axis = cwvYAxis(metric, dataMax);
        for (const t of axis.ticks) {
          const back = parseTick(axis.tick(t), axis.unit);
          expect(back, `${metric} @ ${dataMax} tick ${t}`).toBeCloseTo(t, 6);
        }
      }
    }
  });

  it("spaces ticks evenly from zero to the domain maximum", () => {
    for (const metric of METRICS) {
      for (const dataMax of sweep(metric)) {
        const axis = cwvYAxis(metric, dataMax);
        expect(axis.ticks[0]).toBe(0);
        expect(axis.ticks[axis.ticks.length - 1]).toBe(axis.domain[1]);
        const step = axis.domain[1] / (CWV_TICK_COUNT - 1);
        axis.ticks.forEach((t, i) => {
          expect(t).toBeCloseTo(step * i, 6);
        });
      }
    }
  });

  it("uses only ms, s or the empty unit, and never a unit for CLS", () => {
    for (const metric of METRICS) {
      for (const dataMax of sweep(metric)) {
        const axis = cwvYAxis(metric, dataMax);
        if (metric === "cls") {
          expect(axis.unit).toBe("");
        } else {
          expect(["ms", "s"]).toContain(axis.unit);
        }
      }
    }
  });

  it("sizes the axis to its widest label", () => {
    for (const metric of METRICS) {
      for (const dataMax of sweep(metric)) {
        const axis = cwvYAxis(metric, dataMax);
        const longest = Math.max(...axis.ticks.map((t) => axis.tick(t).length));
        expect(axis.width).toBeGreaterThanOrEqual(longest * 6.5);
        expect(axis.width).toBeLessThanOrEqual(56);
      }
    }
  });

  it("resolves the documented axis for each headline scenario", () => {
    expect(cwvYAxis("lcp", 1250).ticks.map(cwvYAxis("lcp", 1250).tick)).toEqual([
      "0.0s",
      "1.0s",
      "2.0s",
      "3.0s",
      "4.0s",
    ]);

    const inpFast = cwvYAxis("inp", 180);
    expect(inpFast.ticks.map(inpFast.tick)).toEqual([
      "0ms",
      "100ms",
      "200ms",
      "300ms",
      "400ms",
    ]);
    expect(cwvThresholdLabel(inpFast, "good")).toBe("Good 200ms");

    const clsFast = cwvYAxis("cls", 0.09);
    expect(clsFast.ticks.map(clsFast.tick)).toEqual([
      "0.00",
      "0.05",
      "0.10",
      "0.15",
      "0.20",
    ]);
    expect(cwvThresholdLabel(clsFast, "good")).toBe("Good 0.10");
    expect(cwvThresholdLabel(clsFast, "ni")).toBe("NI 0.25");

    const ttfbFast = cwvYAxis("ttfb", 720);
    expect(cwvThresholdLabel(ttfbFast, "good")).toBe("Good 800ms");

    const ttfbSlow = cwvYAxis("ttfb", 1620);
    expect(cwvThresholdLabel(ttfbSlow, "good")).toBe("Good 0.8s");
    expect(cwvThresholdLabel(ttfbSlow, "ni")).toBe("NI 1.8s");

    const fcp = cwvYAxis("fcp", 1620);
    expect(cwvThresholdLabel(fcp, "good")).toBe("Good 1.8s");
  });
});

describe("cwvDataMax", () => {
  it("ignores gaps and non-finite values", () => {
    expect(cwvDataMax([100, null, 900, null])).toBe(900);
    expect(cwvDataMax([null, null])).toBe(0);
    expect(cwvDataMax([])).toBe(0);
    expect(cwvDataMax([Number.NaN, 5])).toBe(5);
  });
});

describe("isCwvMetric", () => {
  it("accepts the five CWV metrics and nothing else", () => {
    for (const m of METRICS) expect(isCwvMetric(m)).toBe(true);
    expect(isCwvMetric("fid")).toBe(false);
    expect(isCwvMetric("")).toBe(false);
  });
});
