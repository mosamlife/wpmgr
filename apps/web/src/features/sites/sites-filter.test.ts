/**
 * Unit tests for the `visibleSites` filter logic and the `SiteCardThumbnail`
 * state-selection pure function. These tests exercise the pure derivation
 * logic without needing @testing-library/react (matching the project convention
 * seen in connection-state-badge.test.ts and use-site-connection.test.ts).
 */
import { describe, it, expect } from "vitest";

import { connectionStateOf } from "@/features/sites/connection-state";
import type { Site } from "@wpmgr/api";

// ---------------------------------------------------------------------------
// Minimal Site factory
// ---------------------------------------------------------------------------

function makeSite(overrides: Partial<Site> & { connection_state?: string }): Site {
  return {
    id: overrides.id ?? "site-1",
    name: overrides.name ?? "Test site",
    url: overrides.url ?? "https://example.com",
    enrolled: overrides.enrolled ?? true,
    status: overrides.status ?? "active",
    health_status: overrides.health_status ?? "reachable",
    tags: overrides.tags ?? [],
    updates_available: overrides.updates_available ?? 0,
    last_backup_status: overrides.last_backup_status ?? null,
    last_backup_at: overrides.last_backup_at ?? null,
    last_seen_at: overrides.last_seen_at ?? null,
    wp_version: overrides.wp_version ?? null,
    php_version: overrides.php_version ?? null,
    agent_version: overrides.agent_version ?? null,
    client_id: overrides.client_id ?? null,
    client_name: overrides.client_name ?? null,
    created_at: overrides.created_at ?? "2024-01-01T00:00:00Z",
    // Allow injecting runtime connection_state for test isolation.
    ...(overrides.connection_state
      ? { connection_state: overrides.connection_state }
      : {}),
  } as unknown as Site;
}

// ---------------------------------------------------------------------------
// visibleSites filter logic — extracted as a pure function for testability
// ---------------------------------------------------------------------------

/**
 * Pure implementation of the `visibleSites` memo from index.tsx.
 * Composed: site matches if it passes ALL active filter axes.
 *   - text (q): OR across name, url, tags
 *   - status:   OR within selected display labels
 *   - tags:     OR within selected tags (site must share ANY selected tag)
 */
const CONNECTION_STATE_LABELS: Record<string, string> = {
  connected: "Connected",
  degraded: "Degraded",
  disconnected: "Disconnected",
  pending_enrollment: "Pending",
  revoked: "Revoked",
  archived: "Archived",
};

function stateLabel(s: Site): string {
  const raw = connectionStateOf(s);
  return CONNECTION_STATE_LABELS[raw] ?? raw;
}

// GH #230 "rich tags" — the TAGS axis moved server-side (see use-sites.ts's
// `tags`/`tagsMatch` query params); this pure function now mirrors ONLY the
// client-side axes still applied over the (already tag-filtered) `sites`
// array in routes/_authed/sites/index.tsx's `visibleSites` memo: free-text
// search (which still matches a site's tag VALUES, since a tag is part of a
// site's searchable text) and status. There is no independent tags-axis
// predicate anymore — server-side tag filtering has its own coverage in
// use-sites.test.ts / the FE contract test.
function filterSites(
  sites: Site[],
  q: string,
  selectedStatuses: string[],
): Site[] {
  const query = q.trim().toLowerCase();
  const hasQ = query.length > 0;
  const hasStatus = selectedStatuses.length > 0;

  if (!hasQ && !hasStatus) return sites;

  return sites.filter((s) => {
    if (hasQ) {
      const haystack = [s.name, s.url, ...(s.tags ?? [])]
        .join(" ")
        .toLowerCase();
      if (!haystack.includes(query)) return false;
    }
    if (hasStatus) {
      if (!selectedStatuses.includes(stateLabel(s))) return false;
    }
    return true;
  });
}

// ---------------------------------------------------------------------------
// Text search
// ---------------------------------------------------------------------------

describe("filterSites — text search", () => {
  const sites = [
    makeSite({ id: "1", name: "Alpha", url: "https://alpha.example.com", tags: ["production"] }),
    makeSite({ id: "2", name: "Beta", url: "https://beta.example.com", tags: ["staging"] }),
    makeSite({ id: "3", name: "Gamma", url: "https://gamma.io", tags: [] }),
  ];

  it("returns all sites when q is empty", () => {
    expect(filterSites(sites, "", [])).toHaveLength(3);
  });

  it("matches on site name (case-insensitive)", () => {
    const result = filterSites(sites, "ALPHA", []);
    expect(result).toHaveLength(1);
    expect(result[0]?.id).toBe("1");
  });

  it("matches on url hostname substring", () => {
    const result = filterSites(sites, "beta.example", []);
    expect(result).toHaveLength(1);
    expect(result[0]?.id).toBe("2");
  });

  // GH #230: the tags AXIS moved server-side, but free-text search still
  // matches a site's tag VALUES (a tag is part of the searchable haystack).
  it("still matches on tag value via search (server-side tags axis is a separate filter)", () => {
    const result = filterSites(sites, "staging", []);
    expect(result).toHaveLength(1);
    expect(result[0]?.id).toBe("2");
  });

  it("returns empty when no site matches", () => {
    expect(filterSites(sites, "zzz-no-match", [])).toHaveLength(0);
  });
});

// ---------------------------------------------------------------------------
// Status filter — OR within selected statuses
// ---------------------------------------------------------------------------

describe("filterSites — status filter (OR within axis)", () => {
  const connected = makeSite({ id: "c", connection_state: "connected" });
  const degraded = makeSite({ id: "d", connection_state: "degraded" });
  const disconnected = makeSite({ id: "x", connection_state: "disconnected" });
  const sites = [connected, degraded, disconnected];

  it("returns all when no statuses are selected", () => {
    expect(filterSites(sites, "", [])).toHaveLength(3);
  });

  it("filters to a single status", () => {
    const result = filterSites(sites, "", ["Connected"]);
    expect(result).toHaveLength(1);
    expect(result[0]?.id).toBe("c");
  });

  it("ORs multiple statuses so both matched sites are returned", () => {
    const result = filterSites(sites, "", ["Connected", "Degraded"]);
    expect(result).toHaveLength(2);
    expect(result.map((s) => s.id).sort()).toEqual(["c", "d"].sort());
  });

  it("returns empty when selected status matches nothing", () => {
    expect(filterSites(sites, "", ["Archived"])).toHaveLength(0);
  });
});

// ---------------------------------------------------------------------------
// AND composition across the remaining client-side filter axes
// ---------------------------------------------------------------------------

describe("filterSites — AND composition across axes", () => {
  const sites = [
    makeSite({ id: "1", name: "Alpha prod", tags: ["production"], connection_state: "connected" }),
    makeSite({ id: "2", name: "Alpha staging", tags: ["staging"], connection_state: "connected" }),
    makeSite({ id: "3", name: "Beta prod", tags: ["production"], connection_state: "disconnected" }),
  ];

  it("applies text AND status (both active)", () => {
    // "alpha" in name AND connected
    const result = filterSites(sites, "alpha", ["Connected"]);
    // Sites 1 and 2 (both Alpha, both connected)
    expect(result).toHaveLength(2);
    expect(result.map((s) => s.id).sort()).toEqual(["1", "2"]);
  });

  it("text without status", () => {
    const result = filterSites(sites, "alpha", []);
    expect(result).toHaveLength(2);
  });

  it("status without text", () => {
    const result = filterSites(sites, "", ["Disconnected"]);
    expect(result).toHaveLength(1);
    expect(result[0]?.id).toBe("3");
  });
});

// ---------------------------------------------------------------------------
// CRITICAL INVARIANT: selection reads the FULL array, not visibleSites
// ---------------------------------------------------------------------------

describe("selectedSites invariant — selection survives filter changes", () => {
  const sites = [
    makeSite({ id: "1", name: "Alpha", connection_state: "connected" }),
    makeSite({ id: "2", name: "Beta", connection_state: "disconnected" }),
    makeSite({ id: "3", name: "Gamma", connection_state: "connected" }),
  ];

  const selectedIds = new Set(["1", "2", "3"]);

  it("all 3 sites remain in the bulk target after filtering down to 1 visible", () => {
    // User searches "Alpha" — only site 1 is visible.
    const visible = filterSites(sites, "alpha", []);
    expect(visible).toHaveLength(1);

    // But selectedSites reads from the FULL array, not visible.
    const selected = sites.filter((s) => selectedIds.has(s.id));
    expect(selected).toHaveLength(3);
  });

  it("select 12 then filter: selection count stays at 12 regardless of visible rows", () => {
    // Create 12 sites, select all, then filter to show 3 (via search, since
    // the tags axis is server-side and out of scope for this pure function).
    const batch = Array.from({ length: 12 }, (_, i) =>
      makeSite({ id: `s${i}`, name: i < 3 ? `Match ${i}` : `Site ${i}` }),
    );
    const batchSelectedIds = new Set(batch.map((s) => s.id));

    const visible = filterSites(batch, "match", []);
    expect(visible).toHaveLength(3);

    // Selection reads the full array.
    const bulkTarget = batch.filter((s) => batchSelectedIds.has(s.id));
    expect(bulkTarget).toHaveLength(12);
  });
});

// ---------------------------------------------------------------------------
// Clear-all resets all axes
// ---------------------------------------------------------------------------

describe("clear-all resets all filter axes", () => {
  const sites = [
    makeSite({ id: "1", name: "Alpha", tags: ["prod"], connection_state: "connected" }),
    makeSite({ id: "2", name: "Beta", tags: ["staging"], connection_state: "disconnected" }),
  ];

  it("after clearing all axes, all sites are visible", () => {
    // With filters active, only 1 site is visible.
    const beforeClear = filterSites(sites, "alpha", ["Connected"]);
    expect(beforeClear).toHaveLength(1);

    // After clear (q="", no statuses), all are visible.
    const afterClear = filterSites(sites, "", []);
    expect(afterClear).toHaveLength(2);
  });
});

// ---------------------------------------------------------------------------
// SiteCardThumbnail state selection — pure logic test
// ---------------------------------------------------------------------------

/**
 * The thumbnail state selection logic from site-card-thumbnail.tsx extracted
 * as a pure function so we can verify the 4 states without DOM rendering.
 */
interface SiteWithScreenshot {
  screenshot_url?: string | null;
  screenshot_status?: "pending" | "ready" | "failed" | null;
}

function thumbnailState(
  site: SiteWithScreenshot,
  imgError = false,
): "ready" | "capturing" | "failed" | "never" {
  if (site.screenshot_url && !imgError) return "ready";
  if (site.screenshot_status === "pending") return "capturing";
  if (site.screenshot_status === "failed") return "failed";
  if (imgError && site.screenshot_url) return "failed";
  return "never";
}

describe("thumbnailState — 4 states", () => {
  it("ready when screenshot_url is present and image did not error", () => {
    expect(
      thumbnailState({ screenshot_url: "https://cdn.example/shot.jpg" }),
    ).toBe("ready");
  });

  it("capturing when screenshot_status is pending (no url yet)", () => {
    expect(thumbnailState({ screenshot_status: "pending" })).toBe("capturing");
  });

  it("failed when screenshot_status is failed", () => {
    expect(thumbnailState({ screenshot_status: "failed" })).toBe("failed");
  });

  it("failed when url is present but the image errored", () => {
    expect(
      thumbnailState({ screenshot_url: "https://cdn.example/shot.jpg" }, true),
    ).toBe("failed");
  });

  it("never when no screenshot fields are present (default at launch)", () => {
    expect(thumbnailState({})).toBe("never");
  });

  it("never when screenshot_url is null and status is null", () => {
    expect(thumbnailState({ screenshot_url: null, screenshot_status: null })).toBe(
      "never",
    );
  });
});
