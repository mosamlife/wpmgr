import { describe, it, expect, vi } from "vitest";
import { screen } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";
import { mockQueryResult } from "@/test/query-mocks";

import { RumResultsTable } from "./RumResultsTable";
import { useRumResults } from "../hooks/useRumResults";
import type { RumResult } from "../types";

// P1 outcome test — GH #170 Wave 5.
//
// The invariant this guards (RumResultsTable.tsx ~:21-24): a row with
// `suppressed: true` MUST NEVER show a p75 number — the server withheld the
// estimate because sample_count < min_sample_count, and rendering any number
// (even "0 ms") would report noise as a real metric. Before this file,
// `rum-tile.test.ts` covered only the pure `formatLcpP75`/rating-band helpers
// in isolation; nothing ever rendered the table, so a regression that dropped
// the `suppressed` guard (e.g. rendered `formatP75(...)` unconditionally)
// would pass every existing test.

vi.mock("../hooks/useRumResults", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../hooks/useRumResults")>();
  return {
    ...actual,
    useRumResults: vi.fn(),
  };
});

const mockedUseRumResults = vi.mocked(useRumResults);

function buildResult(overrides: Partial<RumResult>): RumResult {
  return {
    url_pattern: "/",
    metric: "lcp",
    device: "desktop",
    country: "us",
    p75_ms: 1800,
    sample_count: 500,
    rating: "good",
    suppressed: false,
    ...overrides,
  };
}

describe("RumResultsTable — suppressed rows never show a p75 (data-integrity invariant)", () => {
  it("hides the p75 and sample-count-derived message for a suppressed row, and shows the formatted p75 + rating for a healthy row", () => {
    const suppressedRow = buildResult({
      url_pattern: "/rarely-visited",
      metric: "inp",
      device: "mobile",
      p75_ms: 0,
      sample_count: 4,
      rating: undefined,
      suppressed: true,
    });
    const healthyRow = buildResult({
      url_pattern: "/",
      metric: "lcp",
      device: "desktop",
      p75_ms: 1800,
      sample_count: 500,
      rating: "good",
      suppressed: false,
    });

    mockedUseRumResults.mockReturnValue(
      mockQueryResult<RumResult[]>({ data: [suppressedRow, healthyRow] }),
    );

    renderWithProviders(<RumResultsTable siteId="site-1" />);

    // Healthy row: the REAL formatted p75 (1800ms -> "1.80 s") and rating.
    expect(screen.getByText("1.80 s")).toBeInTheDocument();
    expect(screen.getByText("Good")).toBeInTheDocument();

    // Suppressed row: the honest "insufficient samples" notice, carrying the
    // real sample count (4)...
    expect(
      screen.getByText(/Insufficient samples \(4 of/),
    ).toBeInTheDocument();

    // ...and NEVER a p75 number for that row. p75_ms=0 would format as
    // "0 ms" if the suppression guard were dropped — assert that string is
    // absent anywhere in the table (the only row that could produce it is
    // the suppressed one, since the healthy row's real value is 1800ms).
    expect(screen.queryByText("0 ms")).not.toBeInTheDocument();
    expect(screen.queryByText("0.000")).not.toBeInTheDocument();
  });

  it("never renders a p75 for ANY suppressed row even when its p75_ms happens to be non-zero (defense in depth)", () => {
    // The server always sends p75_ms: 0 for a suppressed row in practice, but
    // the UI's own guard is `r.suppressed || ...`, independent of the value —
    // assert that independence directly so a future refactor that keys off
    // `p75_ms === 0` instead of the `suppressed` flag is caught.
    const suppressedButNonZero = buildResult({
      url_pattern: "/edge-case",
      metric: "cls",
      p75_ms: 150,
      sample_count: 2,
      rating: undefined,
      suppressed: true,
    });

    mockedUseRumResults.mockReturnValue(
      mockQueryResult<RumResult[]>({ data: [suppressedButNonZero] }),
    );

    renderWithProviders(<RumResultsTable siteId="site-1" />);

    expect(screen.queryByText("0.150")).not.toBeInTheDocument();
    expect(
      screen.getByText(/Insufficient samples \(2 of/),
    ).toBeInTheDocument();
  });
});
