import { describe, it, expect, vi } from "vitest";
import { screen } from "@testing-library/react";
import type { FleetAgentVersions } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { mockQueryResult } from "@/test/query-mocks";

import { AgentFleetSummaryCard } from "./AgentFleetSummaryCard";
import { useFleetAgentVersions } from "./use-fleet-agents";

// GH #255 (reported against 0.61.97, self-hosted, 24 sites): the card used to
// interpolate the literal string "unknown" into the summary sentence as if
// it were a version ("0 of 24 sites on unknown, 24 unknown"), which is not a
// sentence an operator can act on. These tests pin the three
// `reference_source` cases the control plane now distinguishes, and that a
// version placeholder is never rendered as though it were a real version.
//
// Most cases below keep `outdated: 0` so the card never mounts its `<Link>`
// (see `src/test/render.tsx`'s `withRouter` doc: a mounted `<Link>` needs a
// router context, and `RouterProvider`'s first paint is async), that keeps
// these plain synchronous `getByText` assertions. The one case that DOES
// need the link (`reference_source: "fleet"` with an outdated site) opts
// into `withRouter` and awaits the first query with `findByText`.

vi.mock("./use-fleet-agents", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-fleet-agents")>();
  return { ...actual, useFleetAgentVersions: vi.fn() };
});

const mockedUseFleetAgentVersions = vi.mocked(useFleetAgentVersions);

function buildData(overrides: Partial<FleetAgentVersions> = {}): FleetAgentVersions {
  return {
    latest_version: "0.61.97",
    reference_source: "published",
    counts: { current: 20, outdated: 0, unknown: 1, ineligible: 0 },
    sites: [],
    ...overrides,
  };
}

function mock(data: FleetAgentVersions) {
  mockedUseFleetAgentVersions.mockReturnValue(
    mockQueryResult<FleetAgentVersions>({ data }),
  );
}

describe("AgentFleetSummaryCard", () => {
  it("renders the plain summary sentence with no qualifier when the reference is a published release", () => {
    mock(buildData({ reference_source: "published", latest_version: "0.61.97" }));
    renderWithProviders(<AgentFleetSummaryCard />);

    expect(screen.getByText("0.61.97")).toBeInTheDocument();
    expect(screen.queryByText(/newest seen in this fleet/)).not.toBeInTheDocument();
  });

  it("adds a short fleet-derived qualifier and never claims 'current' means up to date with a release", async () => {
    mock(
      buildData({
        reference_source: "fleet",
        latest_version: "0.61.90",
        counts: { current: 23, outdated: 1, unknown: 0, ineligible: 0 },
      }),
    );
    renderWithProviders(<AgentFleetSummaryCard />, { withRouter: true });

    expect(await screen.findByText("0.61.90")).toBeInTheDocument();
    expect(
      screen.getByText(/newest seen in this fleet, not a published release/),
    ).toBeInTheDocument();
  });

  it("never prints 'unknown' as though it were a version, and explains the consequence, when there is no reference at all", () => {
    mock(
      buildData({
        reference_source: "none",
        latest_version: "unknown",
        counts: { current: 0, outdated: 0, unknown: 24, ineligible: 0 },
      }),
    );
    renderWithProviders(<AgentFleetSummaryCard />);

    // The exact reported string must never appear again.
    expect(
      screen.queryByText((text) => text.includes("sites on unknown")),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByText("unknown", { selector: "span.font-mono" }),
    ).not.toBeInTheDocument();

    expect(
      screen.getByText(/WPMgr has no reference agent version for this install/),
    ).toBeInTheDocument();
    expect(
      screen.getByText((text) => text.includes("cannot tell which of your")),
    ).toBeInTheDocument();
    expect(screen.getByText("24")).toBeInTheDocument();
  });

  it("still names ineligible (not self-updating) sites when there is no reference version", () => {
    mock(
      buildData({
        reference_source: "none",
        latest_version: "unknown",
        counts: { current: 0, outdated: 0, unknown: 20, ineligible: 4 },
      }),
    );
    renderWithProviders(<AgentFleetSummaryCard />);

    expect(
      screen.getByText((text) => text.includes("run a build that cannot")),
    ).toBeInTheDocument();
  });

  it("never offers a 'View outdated sites' link when nothing is outdated", () => {
    mock(
      buildData({
        reference_source: "none",
        latest_version: "unknown",
        counts: { current: 0, outdated: 0, unknown: 24, ineligible: 0 },
      }),
    );
    renderWithProviders(<AgentFleetSummaryCard />);

    expect(screen.queryByText("View outdated sites")).not.toBeInTheDocument();
  });

  it("renders nothing when the fleet has zero sites in every bucket", () => {
    mock(buildData({ counts: { current: 0, outdated: 0, unknown: 0, ineligible: 0 } }));
    const { container } = renderWithProviders(<AgentFleetSummaryCard />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing on error (best-effort for a site-scoped collaborator's 403)", () => {
    mockedUseFleetAgentVersions.mockReturnValue(
      mockQueryResult<FleetAgentVersions>({
        data: undefined,
        isError: true,
        error: new Error("forbidden"),
        isSuccess: false,
      }),
    );
    const { container } = renderWithProviders(<AgentFleetSummaryCard />);
    expect(container).toBeEmptyDOMElement();
  });
});
