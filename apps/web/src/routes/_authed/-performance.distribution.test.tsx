import { describe, it, expect, vi } from "vitest";
import { screen } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";
import { FleetTable } from "@/features/fleet/FleetTable";
import type { FleetRumOffender } from "@/features/fleet/fleet-types";

import { buildSiteColumns } from "./performance";

// GH #391 — the worst-offenders "distribution" cell used to fabricate its
// bar from nothing but `overall_rating`'s one-word band (a hardcoded
// 75/40/10-style split keyed on the rating string, never on real sample
// data): every row sharing a rating rendered the IDENTICAL bar, labelled
// "Overall" even though no such derivable distribution exists (rollups are
// per-metric histograms with no cross-metric join key — see the design
// note in the route's own worklog). Before this file, no test under
// apps/web/src referenced RumDistributionBar or worst_offenders (verify with
// `git grep -l 'RumDistributionBar\|worst_offenders' -- apps/web/src
// '*.test.*'` against the commit before this one), so this defect had zero
// coverage.
//
// This file renders the real `buildSiteColumns` cell defs through the real
// `FleetTable` (not a mock) — the same code path production uses — so a
// regression that reintroduces the fabrication is caught at the render
// layer, not just typechecked.

function buildOffender(overrides: Partial<FleetRumOffender>): FleetRumOffender {
  return {
    site_id: "site-1",
    name: "example.com",
    url: "https://example.com",
    lcp_p75: 4200,
    inp_p75: 180,
    cls_p75: 0.05,
    overall_rating: "needs-improvement",
    sample_count: 200,
    distribution_metric: "lcp",
    distribution: {
      good: 5,
      needs_improvement: 80,
      poor: 15,
      good_pct: 5,
      needs_improvement_pct: 80,
      poor_pct: 15,
    },
    distribution_suppressed: false,
    distribution_sample_count: 100,
    distribution_sample_floor: 30,
    ...overrides,
  };
}

describe("performance.tsx worst-offenders distribution cell (GH #391)", () => {
  it("renders DIFFERENT bars for two rows sharing the SAME overall_rating but different real per-metric distributions", () => {
    // Both rows are "needs-improvement" — the exact case the fabricated
    // ternary could not distinguish (it read only overall_rating and always
    // produced good=40/ni=45/poor=15 for this band, regardless of the site).
    const lcpDriven = buildOffender({
      site_id: "site-lcp",
      name: "lcp-site.example",
      overall_rating: "needs-improvement",
      distribution_metric: "lcp",
      distribution: {
        good: 5,
        needs_improvement: 80,
        poor: 15,
        good_pct: 5,
        needs_improvement_pct: 80,
        poor_pct: 15,
      },
    });
    const inpDriven = buildOffender({
      site_id: "site-inp",
      name: "inp-site.example",
      overall_rating: "needs-improvement",
      distribution_metric: "inp",
      distribution: {
        good: 60,
        needs_improvement: 30,
        poor: 10,
        good_pct: 60,
        needs_improvement_pct: 30,
        poor_pct: 10,
      },
    });

    const columns = buildSiteColumns(28, vi.fn());
    renderWithProviders(
      <FleetTable<FleetRumOffender>
        data={[lcpDriven, inpDriven]}
        columns={columns}
        ariaLabel="Sites"
      />,
    );

    // Real, DIFFERENT numbers for each row — this is the regression the
    // fabricated ternary would have failed: it could only ever emit
    // 40% good, 45% needs improvement, 15% poor for a needs-improvement row.
    expect(
      screen.getByRole("img", {
        name: "LCP distribution: 5% good, 80% needs improvement, 15% poor.",
      }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("img", {
        name: "INP distribution: 60% good, 30% needs improvement, 10% poor.",
      }),
    ).toBeInTheDocument();

    // The fabricated split for "needs-improvement" never appears anywhere.
    // This text lives only inside the bar's `title` attribute (and its
    // identical aria-label) — `queryByText` searches rendered text content
    // and can never see an attribute, so it must be `queryByTitle` here, not
    // `queryByText`, or this assertion passes no matter what the code does.
    expect(
      screen.queryByTitle(/40% good, 45% needs improvement, 15% poor/),
    ).not.toBeInTheDocument();
  });

  it("renders the insufficient-samples affordance, not a confident bar, for a row below the sample floor", () => {
    const suppressed = buildOffender({
      site_id: "site-suppressed",
      name: "low-traffic.example",
      overall_rating: "poor",
      distribution_metric: "cls",
      distribution: undefined,
      distribution_suppressed: true,
      distribution_sample_count: 12,
      distribution_sample_floor: 30,
    });

    const columns = buildSiteColumns(28, vi.fn());
    renderWithProviders(
      <FleetTable<FleetRumOffender>
        data={[suppressed]}
        columns={columns}
        ariaLabel="Sites"
      />,
    );

    expect(
      screen.getByText(/Insufficient samples \(12 of 30\)/),
    ).toBeInTheDocument();

    // No stacked-bar role="img" for this row — the suppressed branch never
    // reaches buildSegments/the percentage bar.
    expect(screen.queryByRole("img")).not.toBeInTheDocument();
  });

  it("labels the bar with the metric that actually drove overall_rating, never the fabricated 'Overall'", () => {
    const clsDriven = buildOffender({
      site_id: "site-cls",
      name: "cls-site.example",
      overall_rating: "poor",
      distribution_metric: "cls",
      distribution: {
        good: 0,
        needs_improvement: 10,
        poor: 90,
        good_pct: 0,
        needs_improvement_pct: 10,
        poor_pct: 90,
      },
    });

    const columns = buildSiteColumns(28, vi.fn());
    renderWithProviders(
      <FleetTable<FleetRumOffender>
        data={[clsDriven]}
        columns={columns}
        ariaLabel="Sites"
      />,
    );

    expect(
      screen.getByRole("img", { name: /^CLS distribution:/ }),
    ).toBeInTheDocument();
    // Same defect as above: "Overall distribution:" (the old fabricated
    // label) lives only in the `title` attribute, never in text content, so
    // this must query the attribute directly rather than `queryByText`.
    expect(
      screen.queryByTitle(/^Overall distribution:/),
    ).not.toBeInTheDocument();
    expect(screen.queryByRole("img", { name: /^Overall/ })).not.toBeInTheDocument();
  });
});
