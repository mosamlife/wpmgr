import { describe, it, expect, vi } from "vitest";
import { screen } from "@testing-library/react";
import type { Site, SiteEmailLogEntry } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { mockQueryResult } from "@/test/query-mocks";

// GH #251: the fleet Email log's "Site" column resolved `site_id` to a
// truncated raw UUID instead of the site's name. Fixed on the frontend by
// resolving `site_id` -> name through the shared `useSites` hook (the same
// cache the Sites list + capability dots already read).

const { useSitesMock, useFleetEmailLogMock } = vi.hoisted(() => ({
  useSitesMock: vi.fn(),
  useFleetEmailLogMock: vi.fn(),
}));

vi.mock("@/features/sites/use-sites", () => ({ useSites: useSitesMock }));
vi.mock("./use-email", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-email")>();
  return { ...actual, useFleetEmailLog: useFleetEmailLogMock };
});

import { FleetEmailLogTable } from "./fleet-email-log-table";

function buildSite(overrides: Partial<Site> = {}): Site {
  return {
    id: "11111111-0000-0000-0000-000000000001",
    tenant_id: "tenant-1",
    url: "https://acme.example.com",
    name: "Acme Blog",
    status: "active",
    wp_version: "6.8",
    php_version: "8.3",
    health_status: "healthy",
    multisite: false,
    tags: [],
    ...overrides,
  } as unknown as Site;
}

function buildLogEntry(
  overrides: Partial<SiteEmailLogEntry> = {},
): SiteEmailLogEntry {
  return {
    id: "log-1",
    tenant_id: "tenant-1",
    site_id: "11111111-0000-0000-0000-000000000001",
    to_addresses: ["ops@example.com"],
    from_address: "wp@example.com",
    subject: "Test subject",
    provider: "smtp",
    status: "sent",
    response: {},
    error: "",
    retries: 0,
    resent_count: 0,
    body_stored: false,
    created_at: "2026-07-19T00:00:00Z",
    updated_at: "2026-07-19T00:00:00Z",
    ...overrides,
  };
}

function mockFleetEmailLog(entries: SiteEmailLogEntry[]) {
  useFleetEmailLogMock.mockReturnValue({
    entries,
    fetchNextPage: vi.fn(),
    hasNextPage: false,
    isFetchingNextPage: false,
    isPending: false,
    isError: false,
    error: null,
    refetch: vi.fn(),
  });
}

describe("FleetEmailLogTable site name resolution (GH #251)", () => {
  it("renders the resolved site name instead of the raw id when the site is in the sites list", async () => {
    useSitesMock.mockReturnValue(
      mockQueryResult<Site[]>({ data: [buildSite()] }),
    );
    mockFleetEmailLog([
      buildLogEntry({ site_id: "11111111-0000-0000-0000-000000000001" }),
    ]);

    renderWithProviders(<FleetEmailLogTable />, { withRouter: true });

    const link = await screen.findByRole("link", { name: "Acme Blog" });
    expect(link).toHaveAttribute(
      "href",
      "/sites/11111111-0000-0000-0000-000000000001/email",
    );
    expect(screen.queryByText(/11111111…/)).not.toBeInTheDocument();
  });

  it("falls back to the truncated id when the site is not in the sites list (archived/removed)", async () => {
    // The sites list has loaded but does not contain this entry's site_id.
    useSitesMock.mockReturnValue(mockQueryResult<Site[]>({ data: [] }));
    mockFleetEmailLog([
      buildLogEntry({ site_id: "22222222-aaaa-0000-0000-000000000002" }),
    ]);

    renderWithProviders(<FleetEmailLogTable />, { withRouter: true });

    const link = await screen.findByRole("link", { name: "22222222…" });
    expect(link).toHaveAttribute(
      "href",
      "/sites/22222222-aaaa-0000-0000-000000000002/email",
    );
  });

  it("falls back to the truncated id while the sites list is still loading", async () => {
    useSitesMock.mockReturnValue(
      mockQueryResult<Site[]>({ data: undefined, isPending: true }),
    );
    mockFleetEmailLog([
      buildLogEntry({ site_id: "33333333-bbbb-0000-0000-000000000003" }),
    ]);

    renderWithProviders(<FleetEmailLogTable />, { withRouter: true });

    const link = await screen.findByRole("link", { name: "33333333…" });
    expect(link).toBeInTheDocument();
  });
});
