import { describe, it, expect, vi } from "vitest";
import { screen } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";
import { mockQueryResult } from "@/test/query-mocks";

import { VulnPanel } from "./vuln-panel";
import { useSiteVulnerabilities } from "./use-vuln";
import type { SiteVulnsResponse, VulnFinding } from "./use-vuln";

// P0 outcome test — GH #170 Wave 4.
//
// This is the exact bug class the audit called out: a comment in
// `vuln-panel.tsx` (~:320-325) warns "do NOT render 'No vulnerabilities
// found' when feed_ok is false — that would be actively misleading" (the
// operator would read it as "you're safe" when the truth is "we never
// checked"). Before this file, nothing rendered VulnPanel at all, so a
// regression that swapped the feed-not-configured branch for the "no vulns"
// copy would pass every existing test (all of which test extracted pure
// functions, never the component). Test 1 below fails if that swap happens
// — see the "non-vacuous" note above it.
//
// Only `useSiteVulnerabilities` is mocked. `useRescanVulns` /
// `useDismissVuln` / `useRestoreVuln` / `useRemediateVuln` are left as their
// real `useMutation`-based implementations — they never touch the network
// on mount, only when `.mutate(...)` is called, which these tests never do.

vi.mock("./use-vuln", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-vuln")>();
  return {
    ...actual,
    useSiteVulnerabilities: vi.fn(),
  };
});

const mockedUseSiteVulnerabilities = vi.mocked(useSiteVulnerabilities);

function buildFinding(overrides: Partial<VulnFinding>): VulnFinding {
  return {
    id: "00000000-0000-0000-0000-000000000001",
    site_id: "11111111-0000-0000-0000-000000000001",
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
    ...overrides,
  };
}

const ATTRIBUTION = {
  defiant_notice:
    "Vulnerability data is sourced from the Defiant vulnerability intelligence feed.",
  defiant_license: "Used under the Defiant Vulnerability Database license.",
  mitre_notice:
    "CVE is a registered trademark of The MITRE Corporation, used with permission.",
};

describe("VulnPanel — feed-not-configured state (security-misleading regression guard)", () => {
  it("renders the feed-not-configured state and NEVER the 'no vulnerabilities' copy when feed_ok is false", () => {
    const response: SiteVulnsResponse = {
      items: [],
      attribution: ATTRIBUTION,
      feed_ok: false,
      feed_synced: null,
    };
    mockedUseSiteVulnerabilities.mockReturnValue(
      mockQueryResult<SiteVulnsResponse>({ data: response }),
    );

    renderWithProviders(<VulnPanel siteId="site-1" />);

    // The honest state: tell the operator the feed isn't set up.
    expect(
      screen.getByText("Vulnerability feed not configured yet"),
    ).toBeInTheDocument();

    // The misleading state must be ABSENT. Both copy variants are checked:
    // the exact string this component uses ("No known vulnerabilities") and
    // the generic phrase the audit named ("No vulnerabilities found") in
    // case a future edit rewords it without changing the underlying bug.
    expect(screen.queryByText(/no known vulnerabilities/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/no vulnerabilities found/i)).not.toBeInTheDocument();
  });
});

describe("VulnPanel — findings table + Gate 0 attribution", () => {
  it("shows the correct high-risk count and the legally-required attribution strings", () => {
    const findings: VulnFinding[] = [
      buildFinding({
        id: "f-critical",
        slug: "critical-plugin",
        name: "Critical Plugin",
        severity: "critical",
        cve: "CVE-2026-00001",
        cve_link: "https://www.cve.org/CVERecord?id=CVE-2026-00001",
      }),
      buildFinding({
        id: "f-high",
        slug: "high-theme",
        name: "High Theme",
        kind: "theme",
        severity: "high",
      }),
      buildFinding({
        id: "f-medium",
        slug: "medium-plugin",
        name: "Medium Plugin",
        severity: "medium",
      }),
      buildFinding({
        id: "f-low",
        slug: "low-plugin",
        name: "Low Plugin",
        severity: "low",
      }),
    ];
    const response: SiteVulnsResponse = {
      items: findings,
      attribution: ATTRIBUTION,
      feed_ok: true,
      feed_synced: "2026-07-06T00:00:00Z",
    };
    mockedUseSiteVulnerabilities.mockReturnValue(
      mockQueryResult<SiteVulnsResponse>({ data: response }),
    );

    renderWithProviders(<VulnPanel siteId="site-1" />);

    // 4 findings surfaced, all open. Matched as a substring (regex, not an
    // exact string) because the same <p> also renders the "Feed synced ..."
    // suffix inline.
    expect(screen.getByText(/4 open findings/)).toBeInTheDocument();

    // High-risk count: exactly the critical + high rows (2 of the 4) render
    // a "Critical"/"High" severity chip. A regression that mislabels
    // severity (e.g. always "Medium") or drops a row changes this count.
    const highRiskChips = [
      ...screen.getAllByText("Critical"),
      ...screen.getAllByText("High"),
    ];
    expect(highRiskChips).toHaveLength(2);
    // And the non-high-risk rows are still present, just not counted above.
    expect(screen.getByText("Medium")).toBeInTheDocument();
    expect(screen.getByText("Low")).toBeInTheDocument();

    // Gate 0 — legally required attribution. Must appear verbatim.
    expect(screen.getByText(ATTRIBUTION.defiant_notice)).toBeInTheDocument();
    expect(screen.getByText(ATTRIBUTION.defiant_license)).toBeInTheDocument();
    expect(screen.getByText(ATTRIBUTION.mitre_notice)).toBeInTheDocument();
    expect(screen.getByText("Wordfence Intelligence")).toBeInTheDocument();
  });
});
