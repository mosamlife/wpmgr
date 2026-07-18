import { describe, it, expect, vi } from "vitest";
import { screen } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";
import { mockQueryResult } from "@/test/query-mocks";

import { CacheHitRatioTrendCard } from "./CacheHitRatioTrendCard";
import { useCacheHealth } from "../hooks/useCacheHealth";
import type { CacheHealthResponse } from "../types";

// GH #243 — honest hit-ratio semantics. With the agent-managed .htaccess
// active, Apache serves cache hits statically without ever running PHP, so
// the PHP-side tally only ever sees misses. This card must say so plainly
// (relabeled title + a success-toned callout) rather than let a near-0%
// number read as "caching is broken" -- and must render exactly as before
// when .htaccess is not managed (nginx, or not yet reconciled).

vi.mock("../hooks/useCacheHealth", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../hooks/useCacheHealth")>();
  return { ...actual, useCacheHealth: vi.fn() };
});

const mockedUseCacheHealth = vi.mocked(useCacheHealth);

function stubHealth(overrides: Partial<CacheHealthResponse> = {}) {
  mockedUseCacheHealth.mockReturnValue(
    mockQueryResult<CacheHealthResponse>({
      data: {
        points: [
          { hit_count: 10, miss_count: 90, ratio_pct: 10, sampled_at: "2026-07-01T00:00:00Z" },
          { hit_count: 12, miss_count: 88, ratio_pct: 12, sampled_at: "2026-07-02T00:00:00Z" },
        ],
        avg_ratio_pct: 11,
        ...overrides,
      },
    }),
  );
}

describe("CacheHitRatioTrendCard -- honest hit-ratio semantics (GH #243)", () => {
  it("renders the plain 'Cache hit ratio' title with no callout when htaccess is not managed", () => {
    stubHealth();

    renderWithProviders(
      <CacheHitRatioTrendCard siteId="site-1" htaccessManaged={false} />,
    );

    expect(
      screen.getByRole("heading", { name: "Cache hit ratio" }),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(/Served at the web-server level/i),
    ).not.toBeInTheDocument();
    expect(screen.queryByText(/x-wpmgr-source/i)).not.toBeInTheDocument();
  });

  it("relabels to 'PHP-layer hit ratio' and shows the web-server-level callout when htaccess is managed", () => {
    stubHealth();

    renderWithProviders(
      <CacheHitRatioTrendCard siteId="site-1" htaccessManaged={true} />,
    );

    expect(
      screen.getByRole("heading", { name: "PHP-layer hit ratio" }),
    ).toBeInTheDocument();
    expect(
      screen.getByText("Served at the web-server level"),
    ).toBeInTheDocument();
    expect(screen.getByText(/x-wpmgr-source/i)).toBeInTheDocument();
    // The sample-window subtitle is unchanged either way.
    expect(
      screen.getByText(/Hit percentage over the selected window/i),
    ).toBeInTheDocument();
  });

  it("does not alter the underlying chart data -- same points render either way", () => {
    stubHealth();

    renderWithProviders(
      <CacheHitRatioTrendCard siteId="site-1" htaccessManaged={true} />,
    );

    // The avg-ratio badge reflects the real (unmodified) average.
    expect(screen.getByText("11.0% avg")).toBeInTheDocument();
  });
});
