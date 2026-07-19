import { describe, it, expect, vi } from "vitest";
import { screen, fireEvent } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";
import { mockQueryResult } from "@/test/query-mocks";

import { Route } from "./vulnerabilities";
import { useFleetVulnerabilities } from "@/features/security/use-vuln";
import type {
  FleetVulnsResponse,
  FleetVulnFinding,
} from "@/features/security/use-vuln";

// GH #245 web: the fleet Vulnerabilities page's 5th "Unknown" severity tile
// and the degraded-CVSS-enrichment banner.
//
// Rendered via `Route.options.component` (see routes/_authed/settings/-route.test.tsx
// for why: the vite router plugin auto-splits each route's component into its
// own chunk, and a second named export would opt it back out of that split).
// The page reads no route-bound search/params state, so it mounts fine under
// the generic ad hoc test router from `renderWithProviders({ withRouter: true })`.
//
// GOTCHA (src/test/render.tsx): RouterProvider's first paint resolves in a
// microtask, so every test below awaits its FIRST query with `findBy*`
// before making any further synchronous `getBy*`/`queryBy*` assertions.

vi.mock("@/features/security/use-vuln", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/security/use-vuln")>();
  return {
    ...actual,
    useFleetVulnerabilities: vi.fn(),
  };
});

const mockedUseFleetVulnerabilities = vi.mocked(useFleetVulnerabilities);

const VulnerabilitiesPage = Route.options.component!;

const ATTRIBUTION = {
  defiant_notice:
    "Vulnerability data is sourced from the Defiant vulnerability intelligence feed.",
  defiant_license: "Used under the Defiant Vulnerability Database license.",
  mitre_notice:
    "CVE is a registered trademark of The MITRE Corporation, used with permission.",
};

function buildFleetFinding(
  overrides: Partial<FleetVulnFinding["finding"]> & { siteId?: string },
): FleetVulnFinding {
  const { siteId = "11111111-0000-0000-0000-000000000001", ...findingOverrides } =
    overrides;
  return {
    site_id: siteId,
    site_name: "acme.example.com",
    site_url: "https://acme.example.com",
    finding: {
      id: "00000000-0000-0000-0000-000000000001",
      site_id: siteId,
      vuln_id: "22222222-0000-0000-0000-000000000001",
      kind: "plugin",
      slug: "example-plugin",
      name: "Example Plugin",
      installed_version: "1.0.0",
      fixed_version: "1.0.1",
      severity: "low",
      cvss_score: 3.1,
      cve: null,
      cve_link: null,
      title: "Example finding",
      status: "open",
      first_seen: "2026-01-01T00:00:00Z",
      last_seen: "2026-01-02T00:00:00Z",
      references: [],
      ...findingOverrides,
    },
  };
}

describe("Fleet Vulnerabilities page: Unknown severity tile (GH #245)", () => {
  it("renders a 5th Unknown tile with the unknown count, ordered after High and before Medium", async () => {
    const response: FleetVulnsResponse = {
      total_open: 4,
      critical: 1,
      high: 1,
      medium: 1,
      low: 1,
      unknown: 2,
      items: [
        buildFleetFinding({ id: "f1", severity: "critical" }),
        buildFleetFinding({ id: "f2", severity: "high" }),
        buildFleetFinding({ id: "f3", severity: "unknown", cvss_score: null }),
        buildFleetFinding({ id: "f4", severity: "unknown", cvss_score: null }),
        buildFleetFinding({ id: "f5", severity: "medium" }),
        buildFleetFinding({ id: "f6", severity: "low" }),
      ],
      attribution: ATTRIBUTION,
      feed_ok: true,
      feed_synced: "2026-07-19T00:00:00Z",
      enrichment_available: true,
    };
    mockedUseFleetVulnerabilities.mockReturnValue(
      mockQueryResult<FleetVulnsResponse>({ data: response }),
    );

    renderWithProviders(<VulnerabilitiesPage />, { withRouter: true });

    const unknownTile = await screen.findByRole("button", {
      name: "Filter by Unknown: 2 findings",
    });
    expect(unknownTile).toBeInTheDocument();

    const tiles = screen.getAllByRole("button", { name: /Filter by/ });
    expect(tiles).toHaveLength(5);

    // Order: Critical, High, Unknown, Medium, Low. Matches the CP's ORDER
    // BY CASE sort rank (repo.go).
    const order = tiles.map((t) =>
      t.getAttribute("aria-label")?.replace(/^Filter by /, "").split(":")[0],
    );
    expect(order).toEqual(["Critical", "High", "Unknown", "Medium", "Low"]);
  });

  it("clicking the Unknown tile filters the table to unknown-severity findings only", async () => {
    const response: FleetVulnsResponse = {
      total_open: 2,
      critical: 0,
      high: 0,
      medium: 0,
      low: 1,
      unknown: 1,
      items: [
        buildFleetFinding({
          id: "f-unknown",
          name: "Unrated Plugin",
          severity: "unknown",
          cvss_score: null,
        }),
        buildFleetFinding({
          id: "f-low",
          name: "Low Plugin",
          severity: "low",
        }),
      ],
      attribution: ATTRIBUTION,
      feed_ok: true,
      feed_synced: "2026-07-19T00:00:00Z",
      enrichment_available: true,
    };
    mockedUseFleetVulnerabilities.mockReturnValue(
      mockQueryResult<FleetVulnsResponse>({ data: response }),
    );

    renderWithProviders(<VulnerabilitiesPage />, { withRouter: true });

    const unknownTile = await screen.findByRole("button", {
      name: "Filter by Unknown: 1 finding",
    });
    fireEvent.click(unknownTile);

    expect(screen.getByText("Unrated Plugin")).toBeInTheDocument();
    expect(screen.queryByText("Low Plugin")).not.toBeInTheDocument();
  });
});

describe("Fleet Vulnerabilities page: degraded-enrichment banner (GH #245)", () => {
  it("renders the amber banner when feed_ok=true and enrichment_available=false", async () => {
    const response: FleetVulnsResponse = {
      total_open: 1,
      critical: 0,
      high: 0,
      medium: 0,
      low: 0,
      unknown: 1,
      items: [
        buildFleetFinding({ id: "f-unknown", severity: "unknown", cvss_score: null }),
      ],
      attribution: ATTRIBUTION,
      feed_ok: true,
      feed_synced: "2026-07-19T00:00:00Z",
      enrichment_available: false,
    };
    mockedUseFleetVulnerabilities.mockReturnValue(
      mockQueryResult<FleetVulnsResponse>({ data: response }),
    );

    renderWithProviders(<VulnerabilitiesPage />, { withRouter: true });

    expect(
      await screen.findByText(/Severity data unavailable\./),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        /The CVSS enrichment feed was not reachable on the last sync, so some findings may be understated\./,
      ),
    ).toBeInTheDocument();
  });

  it("does NOT render the banner when enrichment_available=true", async () => {
    const response: FleetVulnsResponse = {
      total_open: 1,
      critical: 0,
      high: 0,
      medium: 0,
      low: 1,
      unknown: 0,
      items: [buildFleetFinding({ id: "f-low", severity: "low" })],
      attribution: ATTRIBUTION,
      feed_ok: true,
      feed_synced: "2026-07-19T00:00:00Z",
      enrichment_available: true,
    };
    mockedUseFleetVulnerabilities.mockReturnValue(
      mockQueryResult<FleetVulnsResponse>({ data: response }),
    );

    renderWithProviders(<VulnerabilitiesPage />, { withRouter: true });

    // Await a tile that IS expected to render, to clear the router's
    // async first-paint, before asserting the banner's absence.
    await screen.findByRole("button", { name: "Filter by Low: 1 finding" });

    expect(
      screen.queryByText(/Severity data unavailable/i),
    ).not.toBeInTheDocument();
  });

  it("does NOT render the banner when the feed is not configured (feed_ok=false takes priority)", async () => {
    const response: FleetVulnsResponse = {
      total_open: 0,
      critical: 0,
      high: 0,
      medium: 0,
      low: 0,
      unknown: 0,
      items: [],
      attribution: { defiant_notice: "", defiant_license: "", mitre_notice: "" },
      feed_ok: false,
      feed_synced: null,
      enrichment_available: false,
    };
    mockedUseFleetVulnerabilities.mockReturnValue(
      mockQueryResult<FleetVulnsResponse>({ data: response }),
    );

    renderWithProviders(<VulnerabilitiesPage />, { withRouter: true });

    expect(
      await screen.findByText("Vulnerability feed not configured yet"),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(/Severity data unavailable/i),
    ).not.toBeInTheDocument();
  });
});
