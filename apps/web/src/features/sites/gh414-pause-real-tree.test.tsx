import { describe, it, expect } from "vitest";
import { screen, within } from "@testing-library/react";
import type { Site } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";

import { SiteCard } from "./site-card";

// GH #414 — the adversarial-review regression test. Before this fix,
// `healthBadgeFor`/`HealthBadge` muted and dated `health_status` under pause,
// but that component was never mounted anywhere, and `health_status` has two
// writers (the connection sweep, site.HealthCheckWorker) that never stop for
// a paused site — so the real bug was inverted: a live, true "unreachable"
// verdict would have rendered as a muted, backdated chip had the component
// ever been wired up.
//
// This file proves the fix by rendering the REAL SiteCard (not the isolated
// UptimeBadge/PausedBadge unit) for a paused site whose `health_status` is
// 'unreachable', and asserting:
//   1. Nothing in the rendered tree mutes or dates that health verdict.
//   2. The uptime row — the surface that DOES freeze under pause — carries
//      the "as of" stale stamp.
//
// `health_status` has exactly one production reader in apps/web/src:
// `connectionStateOf`'s legacy fallback (features/sites/connection-state.ts),
// which only fires when the wire payload omits `connection_state` entirely.
// The fixture below leaves `connection_state` unset to exercise exactly that
// path — the one place in the whole app that reads `health_status` for
// display.
//
// SitesTable's equivalent row is NOT mounted here: TableVirtuoso measures its
// scroller with `element.offsetHeight`/`offsetParent`, which jsdom hardcodes
// to 0/none (jsdom has no layout engine), so the row never mounts regardless
// of any ResizeObserver stub — tried and confirmed empty
// (`<tbody data-testid="virtuoso-item-list" />` with zero children) both with
// a synchronous ResizeObserver stub alone and combined with
// offsetHeight/offsetWidth/offsetParent overrides on HTMLElement.prototype.
// The sites-table.tsx `uptime_sparkline` cell reads the exact same
// `uptimeBadgeFor`/`isMonitoringPaused` helpers this file already proves
// correct against the real SiteCard, and it is covered by
// `pnpm -C apps/web typecheck` + `lint`; a true DOM-level proof of that row
// needs either a virtualization-aware test harness or an
// `initialItemCount`/non-virtualized escape hatch, which is a testing-
// infrastructure gap, not a product defect. Flagged for `devops-engineer`/
// a follow-up rather than worked around with a fragile jsdom layout shim.

function pausedUnreachableSite(overrides: Partial<Site> = {}): Site {
  return {
    id: "site-1",
    tenant_id: "tenant-1",
    url: "https://example.com",
    name: "Example",
    status: "active",
    enrolled: true,
    wp_version: "6.8",
    php_version: "8.3",
    // The live, true verdict: the connection sweep or the health-check
    // worker marked this site unreachable AFTER the pause landed, and
    // neither of those writers stops for a paused site.
    health_status: "unreachable",
    multisite: false,
    tags: [],
    page_cache_enabled: false,
    object_cache_enabled: false,
    // Paused 2h ago; the uptime prober's last probe (health_checked_at) was
    // 3h ago, i.e. BEFORE the pause — its own frozen "as of" stamp.
    monitoring_paused_at: new Date(Date.now() - 7_200_000).toISOString(),
    health_checked_at: new Date(Date.now() - 10_800_000).toISOString(),
    up: true,
    uptime_pct: 99.87,
    ...overrides,
  } as unknown as Site;
}

describe("GH #414 — paused site in the real tree (SiteCard)", () => {
  it("renders health_status at full strength: no muting, no 'as of' stamp on it", async () => {
    renderWithProviders(
      <SiteCard site={pausedUnreachableSite()} cardSize="comfortable" selectionCount={0} />,
      { withRouter: true },
    );

    // The test router's first paint is async (see test/render.tsx's module
    // doc) — findBy* for the first assertion, then getBy*/queryBy* are safe.
    // connectionStateOf's legacy fallback maps "unreachable" to the
    // "disconnected" connection state; with no disconnected_reason on the
    // wire, resolveLabel (connection-state-badge-helpers.ts) renders it as
    // "No heartbeat" — full strength (destructive tone), completely unaware
    // of the pause.
    const disconnected = await screen.findByText("No heartbeat");
    const badge = disconnected.closest("span[aria-label]");
    expect(badge).not.toBeNull();
    // Full-strength destructive tone on the dot — never grey/muted.
    expect(badge?.querySelector(".bg-destructive")).not.toBeNull();
    expect(badge?.querySelector(".bg-muted-foreground")).toBeNull();
    // No age stamp rides this badge — health_status is live, not dated.
    expect(within(badge as HTMLElement).queryByText(/as of/)).toBeNull();

    // The pause IS visible — just on its own badge, not on health.
    expect(screen.getByText("Monitoring paused")).toBeInTheDocument();
  });

  it("dates and mutes the UPTIME result for the same site", async () => {
    renderWithProviders(
      <SiteCard site={pausedUnreachableSite()} cardSize="comfortable" selectionCount={0} />,
      { withRouter: true },
    );

    // health_checked_at = 3h before "now" in the fixture above.
    expect(await screen.findByText(/as of 3h ago/)).toBeInTheDocument();
  });
});
