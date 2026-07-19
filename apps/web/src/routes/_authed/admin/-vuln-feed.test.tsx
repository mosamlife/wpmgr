import { describe, it, expect, vi } from "vitest";
import { screen } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";
import { mockMutationResult } from "@/test/query-mocks";

import { FeedStatusCard } from "./vuln-feed";
import { useVulnFeedSync } from "@/features/admin/use-admin-vuln-feed";
import type { VulnFeedStatus, VulnFeedSyncResult } from "@/features/admin/use-admin-vuln-feed";

// GH #245 web: the admin Vulnerability feed status card's degraded-CVSS-
// enrichment sub-line.
//
// `FeedStatusCard` is rendered directly (a plain named export alongside
// `Route`, NOT via `Route.options.component`): the router vite plugin's
// `autoCodeSplitting` extracts this ROUTE's `component:` reference
// (`VulnFeedAdminPage`) into a separately-transformed chunk, and that
// specific split module never resolves its dynamic import under this repo's
// vitest + `@tanstack/router-plugin` combination (confirmed independent of
// this feature's content: a real `vite build` produces a correct
// `vuln-feed-*.js` chunk with no errors, and other never-before-tested
// `admin/*` leaf routes mount fine via `Route.options.component`; the stall
// is specific to this one route file's split boundary). `FeedStatusCard`
// itself is an ordinary function component untouched by that split, so it
// mounts synchronously like any other feature component test.

vi.mock("@/features/admin/use-admin-vuln-feed", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("@/features/admin/use-admin-vuln-feed")>();
  return {
    ...actual,
    useVulnFeedSync: vi.fn(),
  };
});

const mockedUseVulnFeedSync = vi.mocked(useVulnFeedSync);

function stubSync() {
  mockedUseVulnFeedSync.mockReturnValue(
    mockMutationResult<VulnFeedSyncResult, void>({}),
  );
}

describe("FeedStatusCard: degraded-enrichment sub-line (GH #245)", () => {
  it("shows the amber degraded sub-line when feed_ok=true and enrichment_available=false", () => {
    stubSync();
    const status: VulnFeedStatus = {
      configured: true,
      source: "ui",
      feed_ok: true,
      record_count: 12_431,
      last_synced: "2026-07-19T10:00:00Z",
      last_error: "",
      enrichment_available: false,
      last_enrichment_at: "2026-07-10T00:00:00Z",
    };

    renderWithProviders(<FeedStatusCard status={status} />);

    // Header stays "Connected" (green): the scanner IS healthy.
    expect(screen.getByText("Connected")).toBeInTheDocument();

    // The degraded sub-line is an ADDITIONAL amber signal, not a replacement.
    expect(
      screen.getByText(/Last sync was scanner-only\./),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        /CVSS enrichment was unavailable, so severities may be understated\./,
      ),
    ).toBeInTheDocument();
  });

  it("shows the last_enrichment_at relative time when present", () => {
    stubSync();
    const status: VulnFeedStatus = {
      configured: true,
      source: "ui",
      feed_ok: true,
      record_count: 12_431,
      last_synced: "2026-07-19T10:00:00Z",
      last_error: "",
      enrichment_available: false,
      last_enrichment_at: "2026-07-19T09:00:00Z",
    };

    renderWithProviders(<FeedStatusCard status={status} />);

    expect(screen.getByText(/Last enrichment/)).toBeInTheDocument();
  });

  it("does NOT show the degraded sub-line when enrichment_available=true", () => {
    stubSync();
    const status: VulnFeedStatus = {
      configured: true,
      source: "ui",
      feed_ok: true,
      record_count: 12_431,
      last_synced: "2026-07-19T10:00:00Z",
      last_error: "",
      enrichment_available: true,
      last_enrichment_at: "2026-07-19T10:00:00Z",
    };

    renderWithProviders(<FeedStatusCard status={status} />);

    expect(screen.getByText("Connected")).toBeInTheDocument();
    expect(
      screen.queryByText(/Last sync was scanner-only/),
    ).not.toBeInTheDocument();
  });

  it("does NOT show the degraded sub-line for the not-configured state (no false-positive on the amber signal)", () => {
    stubSync();
    const status: VulnFeedStatus = {
      configured: false,
      source: "none",
      feed_ok: false,
      record_count: 0,
      last_synced: null,
      last_error: "",
      enrichment_available: false,
    };

    renderWithProviders(<FeedStatusCard status={status} />);

    expect(screen.getByText("Not configured")).toBeInTheDocument();
    expect(
      screen.queryByText(/Last sync was scanner-only/),
    ).not.toBeInTheDocument();
  });

  it("does NOT show the degraded sub-line for the error state (feed_ok=false takes priority over enrichment)", () => {
    stubSync();
    const status: VulnFeedStatus = {
      configured: true,
      source: "ui",
      feed_ok: false,
      record_count: 0,
      last_synced: null,
      last_error: "feed auth failed: 401 Unauthorized",
      enrichment_available: false,
    };

    renderWithProviders(<FeedStatusCard status={status} />);

    expect(screen.getByText("Error")).toBeInTheDocument();
    expect(screen.getByText(status.last_error)).toBeInTheDocument();
    expect(
      screen.queryByText(/Last sync was scanner-only/),
    ).not.toBeInTheDocument();
  });
});
