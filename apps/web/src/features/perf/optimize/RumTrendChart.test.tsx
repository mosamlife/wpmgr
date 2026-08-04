// GH #329, rendered. cwv-axis.test.ts pins the arithmetic; this file proves
// the arithmetic actually reaches the SVG, which is where the defect was
// visible: the reporter's screenshot showed axis labels "5sms" / "3sms" and a
// threshold label reading "Good 3sms" on a chart whose Web Vitals Good target
// is 2500 ms.
//
// jsdom gives ResponsiveContainer a 0x0 box, so a naive render asserts against
// an empty tree and passes vacuously. Wrapping in an OUTER ResponsiveContainer
// with fixed numeric dimensions makes recharts skip size detection entirely
// (ResponsiveContainer.js: fixed width and height short-circuit to
// ResponsiveContainerContextProvider) and makes the inner container return its
// children as-is. The tick-count assertions below are what keeps a future
// vacuous pass honest.

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { ResponsiveContainer } from "recharts";

import { RumTrendChart } from "./RumTrendChart";
import { formatTrendValue } from "./rum-trend-format";
import type { RumTrendPoint } from "../types";

function points(values: number[]): RumTrendPoint[] {
  return values.map((v, i) => ({
    day: `2026-07-${String(i + 1).padStart(2, "0")}`,
    p75_ms: v,
    sample_count: 500,
    rating: "good" as const,
    suppressed: false,
  }));
}

function renderChart(ui: React.ReactNode) {
  return render(
    <ResponsiveContainer width={640} height={220}>
      {ui}
    </ResponsiveContainer>,
  );
}

// Recharts renders tick LABELS into their own z-index layer, a sibling of the
// axis group rather than a child of it, so the selector has to target
// `.recharts-yAxis-tick-labels` and not `.recharts-yAxis`. Every caller below
// asserts a tick count first; a selector that silently matched nothing would
// otherwise turn every "all ticks are well formed" assertion into a pass.
function yTicks(): string[] {
  return [
    ...document.querySelectorAll(
      ".recharts-yAxis-tick-labels .recharts-cartesian-axis-tick-value",
    ),
  ].map((n) => n.textContent ?? "");
}

describe("RumTrendChart -- Y axis (GH #329)", () => {
  it("labels the LCP Good threshold with its true value, not 3sms", () => {
    renderChart(<RumTrendChart metric="lcp" points={points([2100, 2200, 2050])} />);

    expect(screen.getByText("Good 2.5s")).toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/sms/);
    expect(screen.queryByText("Good 3sms")).toBeNull();
    expect(screen.queryByText("Good 3s")).toBeNull();
  });

  it("renders every Y tick on one scale with exactly one unit", () => {
    renderChart(<RumTrendChart metric="lcp" points={points([2100, 2200, 2050])} />);

    const ticks = yTicks();
    expect(ticks).toEqual(["0.0s", "1.0s", "2.0s", "3.0s", "4.0s"]);
    // Every tick is "<one decimal of seconds>s". Pre-fix this axis mixed bare
    // milliseconds ("650") with doubled-unit seconds ("2sms").
    expect(ticks.every((t) => /^\d+\.\ds$/.test(t))).toBe(true);
  });

  it("renders no two Y ticks with the same text", () => {
    renderChart(<RumTrendChart metric="lcp" points={points([2100, 2200, 2050])} />);

    const ticks = yTicks();
    expect(ticks.length).toBeGreaterThan(2);
    // Pre-fix, 1500 / 1600 / 2000 / 2400 all rendered "2sms" on this axis.
    expect(new Set(ticks).size).toBe(ticks.length);
  });

  it("keeps both threshold lines in frame on a comfortably passing site", () => {
    // All p75 well inside the Good band. With domain ["auto","auto"] recharts
    // discarded both ReferenceLines here, so the operator could not tell
    // "you are passing" from "no thresholds configured".
    renderChart(<RumTrendChart metric="lcp" points={points([780, 812, 795, 840])} />);

    expect(screen.getByText("Good 2.5s")).toBeInTheDocument();
    expect(screen.getByText("NI 4.0s")).toBeInTheDocument();
  });

  it("draws the threshold lines in the success and warning tokens, not the series token", () => {
    const { container } = renderChart(
      <RumTrendChart metric="lcp" points={points([2100, 2200, 2050])} />,
    );

    const strokes = [
      ...container.querySelectorAll(".recharts-reference-line-line"),
    ].map((n) => n.getAttribute("stroke"));

    expect(strokes).toContain("var(--color-success)");
    expect(strokes).toContain("var(--color-warning)");
    // --chart-1 is the LCP series colour AND has no .dark override.
    expect(strokes).not.toContain("var(--color-chart-1)");
    expect(strokes).not.toContain("var(--color-chart-4)");
  });

  it("never sets the recharts unit prop on its Y axis", () => {
    renderChart(<RumTrendChart metric="ttfb" points={points([600, 640, 610])} />);

    const ticks = yTicks();
    expect(ticks).toEqual(["0ms", "250ms", "500ms", "750ms", "1000ms"]);
    // Exactly one unit per tick. The recharts `unit` prop would have made
    // these "0msms" ... "1000msms".
    expect(ticks.every((t) => /^\d+ms$/.test(t))).toBe(true);
  });

  it("renders CLS ticks unitless and with a true 0.10 Good label", () => {
    // The API sends CLS in milli-units.
    renderChart(<RumTrendChart metric="cls" points={points([80, 90, 85])} />);

    expect(screen.getByText("Good 0.10")).toBeInTheDocument();
    const ticks = yTicks();
    expect(ticks).toEqual(["0.00", "0.05", "0.10", "0.15", "0.20"]);
  });

  it("still shows the empty state below two unsuppressed days", () => {
    render(
      <RumTrendChart
        metric="lcp"
        points={[
          {
            day: "2026-07-01",
            p75_ms: 0,
            sample_count: 2,
            rating: "",
            suppressed: true,
          },
        ]}
      />,
    );

    expect(
      screen.getByText("Not enough data yet to show a trend"),
    ).toBeInTheDocument();
  });
});

describe("RumTrendChart -- tooltip precision is NOT the axis scale", () => {
  // The refutation that killed the first fix attempt. Routing the tooltip
  // through the axis scale would have collapsed these readings, because an FCP
  // or LCP axis is permanently on the seconds scale (its domain must contain
  // the Good threshold). These are the measured values from that refutation.
  it("keeps five distinct FCP readings distinct", () => {
    const readings = [780, 812, 795, 840, 879].map((v) =>
      formatTrendValue("fcp", v),
    );

    expect(readings).toEqual([
      "780.0 ms",
      "812.0 ms",
      "795.0 ms",
      "840.0 ms",
      "879.0 ms",
    ]);
    // The axis-scale version produced "0.8 s" four times, then "0.9 s".
    expect(new Set(readings).size).toBe(5);
  });

  it("keeps five distinct sub-second LCP readings distinct", () => {
    const readings = [820, 867, 905, 948, 999].map((v) =>
      formatTrendValue("lcp", v),
    );

    expect(new Set(readings).size).toBe(5);
    expect(readings[0]).toBe("820.0 ms");
  });

  it("switches to seconds only once a reading is at least one second", () => {
    expect(formatTrendValue("lcp", 999)).toBe("999.0 ms");
    expect(formatTrendValue("lcp", 1000)).toBe("1.0 s");
    expect(formatTrendValue("lcp", 2500)).toBe("2.5 s");
  });

  it("keeps CLS at three decimals", () => {
    expect(formatTrendValue("cls", 0.083)).toBe("0.083");
    expect(formatTrendValue("cls", 0.1)).toBe("0.100");
  });
});
