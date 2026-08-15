import { describe, it, expect } from "vitest";
import { screen } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";
import { RumDistributionBar } from "./RumDistributionBar";

// GH #391 follow-up — RumDistributionBar had zero direct test coverage before
// this file (only exercised indirectly through
// routes/_authed/-performance.distribution.test.tsx, which covers the
// worst-offenders table's opt-in `showMetricLabel` usage).
//
// FleetRumPanel.tsx has no test file of its own (`find apps/web/src -iname
// "*FleetRumPanel*"` under a *.test.* filter returns nothing), and
// portal-month-glance.tsx does not call this component at all — it renders a
// distinct `PortalDistributionRow` (see that file's header comment). So the
// worst-offenders table's opt-in flag can't be regression-tested against
// FleetRumPanel's real usage by running FleetRumPanel's own suite; there
// isn't one. This file instead pins FleetRumPanel's exact call shape (no
// `showMetricLabel`) directly against the shared component, so a future
// change to the default can't silently add a visible label to FleetRumPanel's
// cards without a test going red here.

describe("RumDistributionBar — default (opt-out) rendering, FleetRumPanel's call shape", () => {
  it("renders no visible metric-label text when showMetricLabel is omitted", () => {
    renderWithProviders(
      <RumDistributionBar
        metricLabel="LCP"
        distribution={{
          good: 60,
          needs_improvement: 30,
          poor: 10,
          good_pct: 60,
          needs_improvement_pct: 30,
          poor_pct: 10,
        }}
        suppressed={false}
      />,
    );

    // The bar itself still carries the metric name for assistive tech...
    expect(
      screen.getByRole("img", { name: /^LCP distribution:/ }),
    ).toBeInTheDocument();

    // ...but nothing sighted-only reads "LCP" — FleetRumPanel's CoreMetricCard
    // already prints the metric name as its own heading, so a second, visible
    // "LCP" here would be a duplicate this change must not introduce.
    expect(screen.queryByText("LCP")).not.toBeInTheDocument();
  });

  it("renders the plain 'Insufficient samples' fallback with no metric prefix when showMetricLabel is omitted", () => {
    renderWithProviders(
      <RumDistributionBar
        metricLabel="CLS"
        distribution={undefined}
        suppressed
        sampleCount={12}
        minSampleCount={30}
      />,
    );

    // FleetRumPanel never actually reaches this branch in production (its
    // suppressed slices short-circuit before rendering RumDistributionBar at
    // all — see CoreMetricCard), but the prop contract still needs to hold:
    // opt-out means opt-out.
    expect(
      screen.getByText(/^Insufficient samples \(12 of 30\)$/),
    ).toBeInTheDocument();
    expect(screen.queryByText(/^CLS:/)).not.toBeInTheDocument();
  });

  it("showMetricLabel=true (the worst-offenders table's opt-in) DOES add a visible label — proves the flag actually does something", () => {
    renderWithProviders(
      <RumDistributionBar
        metricLabel="INP"
        distribution={{
          good: 60,
          needs_improvement: 30,
          poor: 10,
          good_pct: 60,
          needs_improvement_pct: 30,
          poor_pct: 10,
        }}
        showMetricLabel
      />,
    );
    expect(screen.getByText("INP")).toBeInTheDocument();
  });
});
