import { describe, it, expect } from "vitest";
import { screen } from "@testing-library/react";
import type { Site } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";

import { UptimeBadge, PausedBadge } from "./site-badges";

// GH #414 phase 4b — the rendered halves of the two badges. The decision logic
// itself is pinned in monitoring-pause.test.ts; this file proves the components
// actually put it on screen, including the hover text.

function buildSite(overrides: Partial<Site> = {}): Site {
  return {
    id: "site-1",
    tenant_id: "tenant-1",
    url: "https://example.com",
    name: "Example",
    status: "active",
    wp_version: "6.8",
    php_version: "8.3",
    health_status: "healthy",
    multisite: false,
    tags: [],
    ...overrides,
  } as unknown as Site;
}

describe("PausedBadge", () => {
  it("renders nothing at all for an active site", () => {
    const { container } = renderWithProviders(
      <PausedBadge site={buildSite()} />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it("renders the badge with its reason reachable on hover", () => {
    renderWithProviders(
      <PausedBadge
        site={buildSite({
          monitoring_paused_at: new Date(Date.now() - 3_600_000).toISOString(),
          monitoring_paused_by: "user-7",
          monitoring_paused_reason: "Migrating to the new host",
        })}
        resolveActor={(id) => (id === "user-7" ? "Dana Ruiz" : null)}
      />,
    );

    const badge = screen.getByText("Monitoring paused");
    expect(badge).toBeInTheDocument();

    // The reason and the actor live in the hover text rather than the chip, so
    // the row stays readable while the detail is one hover away.
    const labelled = screen.getByLabelText(/Reason: Migrating to the new host/);
    expect(labelled).toHaveAttribute(
      "title",
      expect.stringContaining("by Dana Ruiz"),
    );
    expect(labelled.getAttribute("title")).toContain(
      "Migrating to the new host",
    );
  });

  it("puts the scope sentence in reach of the operator", () => {
    renderWithProviders(
      <PausedBadge
        site={buildSite({ monitoring_paused_at: new Date().toISOString() })}
      />,
    );
    const badge = screen.getByText("Monitoring paused").closest("[title]");
    expect(badge?.getAttribute("title")).toContain("Backups");
  });
});

describe("UptimeBadge", () => {
  it("shows a confident Up while monitoring is active", () => {
    renderWithProviders(<UptimeBadge site={buildSite({ up: true })} />);
    expect(screen.getByText("Up")).toBeInTheDocument();
    expect(screen.queryByText(/as of/)).not.toBeInTheDocument();
  });

  it("does NOT show a confident up state for a paused site", () => {
    renderWithProviders(
      <UptimeBadge
        site={buildSite({
          up: true,
          monitoring_paused_at: new Date(Date.now() - 7_200_000).toISOString(),
          health_checked_at: new Date(Date.now() - 10_800_000).toISOString(),
        })}
      />,
    );
    // The word survives, but it is dated and drained of colour: the prober
    // stopped confirming it the moment the pause landed.
    expect(screen.getByText(/as of 3h ago/)).toBeInTheDocument();
  });

  it("says Not checked, never Up, for a paused site never probed", () => {
    renderWithProviders(
      <UptimeBadge
        site={buildSite({
          monitoring_paused_at: new Date().toISOString(),
        })}
      />,
    );
    expect(screen.getByText("Not checked")).toBeInTheDocument();
    expect(screen.queryByText("Up")).not.toBeInTheDocument();
  });
});
