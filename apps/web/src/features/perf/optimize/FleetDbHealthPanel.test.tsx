import { describe, it, expect, vi } from "vitest";
import { fireEvent, screen } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";
import { mockQueryResult } from "@/test/query-mocks";

import { FleetDbHealthPanel } from "./FleetDbHealthPanel";
import { useFleetDbHealth } from "../hooks/useFleetDbHealth";
import type { FleetDbHealth } from "../types";

// GH #197 — the amber "Sites with items to review" stat on the fleet DB
// health panel used to be inert text: a count with no way to see WHICH
// sites it referred to. This proves the stat is now a real disclosure that
// drills through to every flagged site, each linking to that site's
// existing Orphaned-items view (`/sites/$siteId/optimize`).

vi.mock("../hooks/useFleetDbHealth", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("../hooks/useFleetDbHealth")>();
  return { ...actual, useFleetDbHealth: vi.fn() };
});

const mockedUseFleetDbHealth = vi.mocked(useFleetDbHealth);

function buildHealth(overrides: Partial<FleetDbHealth> = {}): FleetDbHealth {
  return {
    total_sites_scanned: 4,
    total_db_size_bytes: 400_000_000,
    total_table_count: 120,
    total_orphaned_options: 30,
    total_orphaned_cron: 5,
    sites_with_orphans: 2,
    top_sites: [],
    sites_needing_review: [
      {
        site_id: "site-aaa",
        site_name: "aaa.example.com",
        orphaned_options_count: 12,
        orphaned_cron_count: 3,
      },
      {
        site_id: "site-bbb",
        site_name: "bbb.example.com",
        orphaned_options_count: 8,
        orphaned_cron_count: 0,
      },
    ],
    ...overrides,
  };
}

function renderPanel(health: FleetDbHealth) {
  mockedUseFleetDbHealth.mockReturnValue(mockQueryResult<FleetDbHealth>({ data: health }));
  return renderWithProviders(<FleetDbHealthPanel />, { withRouter: true });
}

describe("FleetDbHealthPanel — sites-needing-review drill-down (GH #197)", () => {
  it("renders the amber stat as a collapsed, interactive disclosure by default", async () => {
    renderPanel(buildHealth());

    // RouterProvider's first paint is async (see src/test/render.tsx) —
    // the FIRST assertion must be a findBy*, everything after is sync.
    const stat = await screen.findByRole("button", {
      name: /sites with items to review/i,
    });
    expect(stat).toHaveAttribute("aria-expanded", "false");

    // Collapsed: no per-site links are in the DOM yet.
    expect(
      screen.queryByRole("link", { name: "aaa.example.com" }),
    ).not.toBeInTheDocument();
  });

  it("expands to list every flagged site, each linking to its Orphaned-items view with the total orphan count, and toggles aria-expanded", async () => {
    renderPanel(buildHealth());

    const stat = await screen.findByRole("button", {
      name: /sites with items to review/i,
    });

    fireEvent.click(stat);

    expect(stat).toHaveAttribute("aria-expanded", "true");

    // Non-vacuous: one Link per flagged site, pointing at the real route,
    // showing the site name and the SUM of options + cron orphan counts.
    const linkA = screen.getByRole("link", { name: "aaa.example.com" });
    expect(linkA).toHaveAttribute("href", "/sites/site-aaa/optimize");
    const linkB = screen.getByRole("link", { name: "bbb.example.com" });
    expect(linkB).toHaveAttribute("href", "/sites/site-bbb/optimize");

    expect(screen.getByText("15 items")).toBeInTheDocument(); // 12 + 3
    expect(screen.getByText("8 items")).toBeInTheDocument(); // 8 + 0

    // Collapses back on a second click.
    fireEvent.click(stat);
    expect(stat).toHaveAttribute("aria-expanded", "false");
    expect(
      screen.queryByRole("link", { name: "aaa.example.com" }),
    ).not.toBeInTheDocument();
  });

  it("does not render the amber stat at all when no sites have orphans", async () => {
    renderPanel(
      buildHealth({ sites_with_orphans: 0, sites_needing_review: [] }),
    );

    // Assert on something else that IS present first (async router paint),
    // then confirm the amber stat never shows up.
    await screen.findByText("Database health across your sites");
    expect(
      screen.queryByRole("button", { name: /sites with items to review/i }),
    ).not.toBeInTheDocument();
  });

  it("guards against an undefined sites_needing_review (older server response) without crashing", async () => {
    const health = buildHealth();
    delete (health as { sites_needing_review?: unknown }).sites_needing_review;

    renderPanel(health);

    const stat = await screen.findByRole("button", {
      name: /sites with items to review/i,
    });
    fireEvent.click(stat);

    expect(stat).toHaveAttribute("aria-expanded", "true");
    // Nothing to show — no crash, no stray links.
    expect(screen.queryAllByRole("link")).toHaveLength(0);
  });
});
