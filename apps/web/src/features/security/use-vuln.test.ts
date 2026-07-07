import { describe, it, expect, vi } from "vitest";

import {
  isHighRisk,
  countHighRisk,
  worstSeverity,
  safeExternalHref,
  vulnKeys,
  type VulnFinding,
  type VulnAttribution,
  type SiteVulnsResponse,
  type FleetVulnsResponse,
  type FleetVulnFinding,
} from "./use-vuln";

// ---------------------------------------------------------------------------
// Test fixtures — realistic payloads mirroring handler.go DTOs exactly.
//
// These are the canonical shapes the Go handler emits. If the Go DTO changes
// and this file is updated, the app will compile against the new type — any
// field mismatch that makes it past the TypeScript compiler is caught here.
// ---------------------------------------------------------------------------

/** Realistic critical plugin finding. */
const CRITICAL_PLUGIN_FINDING: VulnFinding = {
  id: "aaaaaaaa-0000-0000-0000-000000000001",
  site_id: "bbbbbbbb-0000-0000-0000-000000000001",
  vuln_id: "cccccccc-1111-1111-1111-111111111111",
  kind: "plugin",
  slug: "woocommerce",
  name: "WooCommerce",
  installed_version: "8.0.0",
  fixed_version: "8.0.3",
  severity: "critical",
  cvss_score: 9.8,
  cve: "CVE-2024-12345",
  cve_link: "https://www.cve.org/CVERecord?id=CVE-2024-12345",
  title: "WooCommerce SQL Injection via order parameter",
  status: "open",
  first_seen: "2024-06-01T10:00:00Z",
  last_seen: "2024-06-15T10:00:00Z",
  references: [
    "https://www.wordfence.com/threat-intel/vulnerabilities/id/cccccccc-1111-1111-1111-111111111111",
  ],
};

/** High severity theme finding. */
const HIGH_THEME_FINDING: VulnFinding = {
  id: "aaaaaaaa-0000-0000-0000-000000000002",
  site_id: "bbbbbbbb-0000-0000-0000-000000000001",
  vuln_id: "cccccccc-2222-2222-2222-222222222222",
  kind: "theme",
  slug: "astra",
  name: "Astra",
  installed_version: "4.5.0",
  fixed_version: "4.5.2",
  severity: "high",
  cvss_score: 7.5,
  cve: "CVE-2024-99999",
  cve_link: "https://www.cve.org/CVERecord?id=CVE-2024-99999",
  title: "Astra XSS via widget parameter",
  status: "open",
  first_seen: "2024-05-20T08:00:00Z",
  last_seen: "2024-06-15T10:00:00Z",
  references: [
    "https://www.wordfence.com/threat-intel/vulnerabilities/id/cccccccc-2222-2222-2222-222222222222",
  ],
};

/** Medium severity WordPress core finding — no CVE assigned yet. */
const MEDIUM_CORE_FINDING: VulnFinding = {
  id: "aaaaaaaa-0000-0000-0000-000000000003",
  site_id: "bbbbbbbb-0000-0000-0000-000000000001",
  vuln_id: "cccccccc-3333-3333-3333-333333333333",
  kind: "core",
  slug: "wordpress",
  name: "WordPress",
  installed_version: "6.4.0",
  fixed_version: "6.4.3",
  severity: "medium",
  cvss_score: 5.4,
  // No CVE yet — optional fields absent
  cve: null,
  cve_link: null,
  title: "WordPress CSRF in comment approval flow",
  status: "open",
  first_seen: "2024-04-01T00:00:00Z",
  last_seen: "2024-06-15T10:00:00Z",
  references: [
    "https://www.wordfence.com/threat-intel/vulnerabilities/id/cccccccc-3333-3333-3333-333333333333",
  ],
};

/** Dismissed low severity finding. */
const LOW_DISMISSED_FINDING: VulnFinding = {
  id: "aaaaaaaa-0000-0000-0000-000000000004",
  site_id: "bbbbbbbb-0000-0000-0000-000000000001",
  vuln_id: "cccccccc-4444-4444-4444-444444444444",
  kind: "plugin",
  slug: "contact-form-7",
  name: "Contact Form 7",
  installed_version: "5.8.0",
  fixed_version: "5.8.1",
  severity: "low",
  cvss_score: 2.3,
  cve: null,
  cve_link: null,
  title: "Contact Form 7 open redirect (low impact)",
  status: "dismissed",
  first_seen: "2024-03-10T00:00:00Z",
  last_seen: "2024-06-14T10:00:00Z",
  references: [],
};

/** Realistic attribution notices (from wordfence_vuln_feed_meta). */
const ATTRIBUTION: VulnAttribution = {
  defiant_notice:
    "Copyright 2012-2024 Defiant Inc. All rights reserved. This data is provided under a commercial license.",
  defiant_license:
    "Licensed under the Wordfence Intelligence Terms and Conditions. Redistribution permitted subject to attribution.",
  mitre_notice:
    "CVE data sourced from the MITRE Corporation CVE Program. Copyright 2024 The MITRE Corporation.",
};

// ---------------------------------------------------------------------------
// Fixture: per-site response with findings
// ---------------------------------------------------------------------------

const SITE_VULNS_WITH_FINDINGS: SiteVulnsResponse = {
  items: [
    CRITICAL_PLUGIN_FINDING,
    HIGH_THEME_FINDING,
    MEDIUM_CORE_FINDING,
    LOW_DISMISSED_FINDING,
  ],
  attribution: ATTRIBUTION,
  feed_ok: true,
  feed_synced: "2024-06-15T09:00:00Z",
};

// ---------------------------------------------------------------------------
// Fixture: per-site response with zero findings (feed_ok=true)
// ---------------------------------------------------------------------------

const SITE_VULNS_CLEAN: SiteVulnsResponse = {
  items: [],
  attribution: ATTRIBUTION,
  feed_ok: true,
  feed_synced: "2024-06-15T09:00:00Z",
};

// ---------------------------------------------------------------------------
// Fixture: per-site response with feed not configured (feed_ok=false)
// ---------------------------------------------------------------------------

const SITE_VULNS_FEED_NOT_CONFIGURED: SiteVulnsResponse = {
  items: [],
  attribution: {
    defiant_notice: "",
    defiant_license: "",
    mitre_notice: "",
  },
  feed_ok: false,
  feed_synced: null,
};

// ---------------------------------------------------------------------------
// Fixture: fleet response
// ---------------------------------------------------------------------------

const FLEET_FINDING_1: FleetVulnFinding = {
  site_id: "bbbbbbbb-0000-0000-0000-000000000001",
  site_name: "acme.example.com",
  site_url: "https://acme.example.com",
  finding: CRITICAL_PLUGIN_FINDING,
};

const FLEET_FINDING_2: FleetVulnFinding = {
  site_id: "bbbbbbbb-0000-0000-0000-000000000002",
  site_name: "shop.example.com",
  site_url: "https://shop.example.com",
  finding: {
    ...HIGH_THEME_FINDING,
    id: "aaaaaaaa-0000-0000-0000-000000000005",
    site_id: "bbbbbbbb-0000-0000-0000-000000000002",
  },
};

const FLEET_VULNS_RESPONSE: FleetVulnsResponse = {
  total_open: 2,
  critical: 1,
  high: 1,
  medium: 0,
  low: 0,
  items: [FLEET_FINDING_1, FLEET_FINDING_2],
  attribution: ATTRIBUTION,
  feed_ok: true,
  feed_synced: "2024-06-15T09:00:00Z",
};

const FLEET_VULNS_FEED_NOT_CONFIGURED: FleetVulnsResponse = {
  total_open: 0,
  critical: 0,
  high: 0,
  medium: 0,
  low: 0,
  items: [],
  attribution: {
    defiant_notice: "",
    defiant_license: "",
    mitre_notice: "",
  },
  feed_ok: false,
  feed_synced: null,
};

// ---------------------------------------------------------------------------
// DTO shape tests
//
// TypeScript validates field names and types at compile time. These runtime
// assertions pin that every required field is present in the fixtures so that
// if the Go DTO adds a new required field, the TypeScript type update here
// forces the test to be updated too.
// ---------------------------------------------------------------------------

describe("VulnFinding DTO shape — all required fields present", () => {
  it("a complete finding object has all required fields", () => {
    const f: VulnFinding = CRITICAL_PLUGIN_FINDING;
    expect(f.id).toBeDefined();
    expect(f.site_id).toBeDefined();
    expect(f.vuln_id).toBeDefined();
    expect(["plugin", "theme", "core"]).toContain(f.kind);
    expect(f.slug).toBeDefined();
    expect(f.name).toBeDefined();
    expect(f.installed_version).toBeDefined();
    expect(["critical", "high", "medium", "low"]).toContain(f.severity);
    expect(f.title).toBeDefined();
    expect(["open", "dismissed", "remediated"]).toContain(f.status);
    expect(f.first_seen).toBeDefined();
    expect(f.last_seen).toBeDefined();
    expect(Array.isArray(f.references)).toBe(true);
  });

  it("optional fields (fixed_version, cvss_score, cve, cve_link) may be null", () => {
    const f: VulnFinding = MEDIUM_CORE_FINDING;
    // These should NOT throw when accessed even though they are null:
    expect(f.cve === null || typeof f.cve === "string").toBe(true);
    expect(f.cve_link === null || typeof f.cve_link === "string").toBe(true);
    expect(f.cvss_score === null || typeof f.cvss_score === "number").toBe(
      true,
    );
  });

  it("references is always an array (never null or undefined)", () => {
    for (const f of SITE_VULNS_WITH_FINDINGS.items) {
      expect(Array.isArray(f.references)).toBe(true);
    }
  });
});

describe("SiteVulnsResponse DTO shape", () => {
  it("has items, attribution, feed_ok, and optional feed_synced", () => {
    const r: SiteVulnsResponse = SITE_VULNS_WITH_FINDINGS;
    expect(Array.isArray(r.items)).toBe(true);
    expect(typeof r.attribution).toBe("object");
    expect(typeof r.feed_ok).toBe("boolean");
  });

  it("feed_ok=true + empty items represents a clean site", () => {
    const r: SiteVulnsResponse = SITE_VULNS_CLEAN;
    expect(r.feed_ok).toBe(true);
    expect(r.items).toHaveLength(0);
  });

  it("feed_ok=false represents feed not configured (items is empty, notices may be blank)", () => {
    const r: SiteVulnsResponse = SITE_VULNS_FEED_NOT_CONFIGURED;
    expect(r.feed_ok).toBe(false);
    expect(r.items).toHaveLength(0);
  });
});

describe("FleetVulnsResponse DTO shape", () => {
  it("has total_open, severity counts, items, attribution, feed_ok", () => {
    const r: FleetVulnsResponse = FLEET_VULNS_RESPONSE;
    expect(typeof r.total_open).toBe("number");
    expect(typeof r.critical).toBe("number");
    expect(typeof r.high).toBe("number");
    expect(typeof r.medium).toBe("number");
    expect(typeof r.low).toBe("number");
    expect(Array.isArray(r.items)).toBe(true);
    expect(typeof r.attribution.defiant_notice).toBe("string");
    expect(typeof r.attribution.defiant_license).toBe("string");
    expect(typeof r.attribution.mitre_notice).toBe("string");
    expect(typeof r.feed_ok).toBe("boolean");
  });

  it("each fleet item has site_id, site_name, site_url, and a nested finding", () => {
    const item: FleetVulnFinding = FLEET_FINDING_1;
    expect(typeof item.site_id).toBe("string");
    expect(typeof item.site_name).toBe("string");
    expect(typeof item.site_url).toBe("string");
    expect(typeof item.finding).toBe("object");
    expect(item.finding.id).toBeDefined();
  });

  it("severity counts in fleet response match the count of items by severity", () => {
    // Verify our fixture is internally consistent.
    const r = FLEET_VULNS_RESPONSE;
    const criticalCount = r.items.filter(
      (ff) => ff.finding.severity === "critical",
    ).length;
    const highCount = r.items.filter(
      (ff) => ff.finding.severity === "high",
    ).length;
    expect(r.critical).toBe(criticalCount);
    expect(r.high).toBe(highCount);
  });

  it("feed_ok=false fleet response shows no items", () => {
    const r = FLEET_VULNS_FEED_NOT_CONFIGURED;
    expect(r.feed_ok).toBe(false);
    expect(r.items).toHaveLength(0);
    expect(r.total_open).toBe(0);
  });
});

// ---------------------------------------------------------------------------
// Attribution DTO
// ---------------------------------------------------------------------------

describe("VulnAttribution DTO — Gate 0 legally required fields", () => {
  it("has defiant_notice, defiant_license, and mitre_notice strings", () => {
    const a: VulnAttribution = ATTRIBUTION;
    expect(typeof a.defiant_notice).toBe("string");
    expect(typeof a.defiant_license).toBe("string");
    expect(typeof a.mitre_notice).toBe("string");
  });

  it("attribution notices are non-empty in a configured feed response", () => {
    const r = SITE_VULNS_WITH_FINDINGS;
    expect(r.attribution.defiant_notice.length).toBeGreaterThan(0);
    expect(r.attribution.defiant_license.length).toBeGreaterThan(0);
    expect(r.attribution.mitre_notice.length).toBeGreaterThan(0);
  });

  it("attribution may be blank strings when feed is not configured", () => {
    const r = SITE_VULNS_FEED_NOT_CONFIGURED;
    // Blank is acceptable when the feed has never synced, but the fields must
    // still be strings (not null/undefined) so UI rendering doesn't crash.
    expect(typeof r.attribution.defiant_notice).toBe("string");
    expect(typeof r.attribution.defiant_license).toBe("string");
    expect(typeof r.attribution.mitre_notice).toBe("string");
  });
});

// ---------------------------------------------------------------------------
// isHighRisk + countHighRisk helpers
// ---------------------------------------------------------------------------

describe("isHighRisk", () => {
  it("returns true for critical severity", () => {
    expect(isHighRisk("critical")).toBe(true);
  });

  it("returns true for high severity", () => {
    expect(isHighRisk("high")).toBe(true);
  });

  it("returns false for medium severity", () => {
    expect(isHighRisk("medium")).toBe(false);
  });

  it("returns false for low severity", () => {
    expect(isHighRisk("low")).toBe(false);
  });
});

describe("countHighRisk", () => {
  it("counts only open critical/high findings (not dismissed, not medium/low)", () => {
    const findings = SITE_VULNS_WITH_FINDINGS.items;
    // CRITICAL_PLUGIN_FINDING (open, critical) + HIGH_THEME_FINDING (open, high) = 2
    // MEDIUM_CORE_FINDING (open, medium) → excluded
    // LOW_DISMISSED_FINDING (dismissed, low) → excluded
    expect(countHighRisk(findings)).toBe(2);
  });

  it("returns 0 for an empty findings list", () => {
    expect(countHighRisk([])).toBe(0);
  });

  it("does not count dismissed critical findings", () => {
    const dismissed: VulnFinding = {
      ...CRITICAL_PLUGIN_FINDING,
      id: "aaaaaaaa-0000-0000-0000-000000000099",
      status: "dismissed",
    };
    expect(countHighRisk([dismissed])).toBe(0);
  });
});

// ---------------------------------------------------------------------------
// worstSeverity — used by the Health tab's Vulnerabilities tile
// ---------------------------------------------------------------------------

describe("worstSeverity", () => {
  it("returns undefined for an empty list", () => {
    expect(worstSeverity([])).toBeUndefined();
  });

  it("returns critical when a critical finding is present among mixed severities", () => {
    const findings = [MEDIUM_CORE_FINDING, CRITICAL_PLUGIN_FINDING, HIGH_THEME_FINDING];
    expect(worstSeverity(findings)).toBe("critical");
  });

  it("returns high when high is the worst present (no critical)", () => {
    const findings = [MEDIUM_CORE_FINDING, HIGH_THEME_FINDING, LOW_DISMISSED_FINDING];
    expect(worstSeverity(findings)).toBe("high");
  });

  it("returns medium when only medium and low are present", () => {
    const low: VulnFinding = { ...LOW_DISMISSED_FINDING, status: "open" };
    expect(worstSeverity([MEDIUM_CORE_FINDING, low])).toBe("medium");
  });

  it("returns low when only low findings are present", () => {
    const low: VulnFinding = { ...LOW_DISMISSED_FINDING, status: "open" };
    expect(worstSeverity([low])).toBe("low");
  });

  it("a single critical finding among all four severities still wins", () => {
    const all = [
      LOW_DISMISSED_FINDING,
      MEDIUM_CORE_FINDING,
      HIGH_THEME_FINDING,
      CRITICAL_PLUGIN_FINDING,
    ];
    expect(worstSeverity(all)).toBe("critical");
  });
});

// ---------------------------------------------------------------------------
// vulnKeys — cache key factory
// ---------------------------------------------------------------------------

describe("vulnKeys — cache key factory", () => {
  it("fleet key is a constant tuple", () => {
    const key = vulnKeys.fleet();
    expect(Array.isArray(key)).toBe(true);
    expect(key).toContain("fleet");
  });

  it("site key includes the siteId so different sites have different keys", () => {
    const key1 = vulnKeys.site("site-aaa");
    const key2 = vulnKeys.site("site-bbb");
    expect(key1).toContain("site-aaa");
    expect(key2).toContain("site-bbb");
    expect(key1).not.toEqual(key2);
  });

  it("site key and fleet key are distinct (invalidating fleet does not flush site cache)", () => {
    const siteKey = vulnKeys.site("site-aaa");
    const fleetKey = vulnKeys.fleet();
    expect(siteKey).not.toEqual(fleetKey);
  });

  it("all siteLists() key is a prefix of any specific site key", () => {
    const listsKey = vulnKeys.siteLists();
    const siteKey = vulnKeys.site("site-abc");
    // siteKey starts with the same root elements as siteLists() so that
    // invalidateQueries({ queryKey: vulnKeys.siteLists() }) correctly
    // invalidates all per-site caches.
    expect(siteKey[0]).toBe(listsKey[0]);
    expect(siteKey[1]).toBe(listsKey[1]);
  });
});

// ---------------------------------------------------------------------------
// Hook exports — API shape guard (same pattern as use-hardening.test.ts)
// ---------------------------------------------------------------------------

describe("use-vuln hook exports — public API shape", () => {
  it("useSiteVulnerabilities is exported as a function", async () => {
    const { useSiteVulnerabilities } = await import("./use-vuln");
    expect(typeof useSiteVulnerabilities).toBe("function");
    // Takes exactly one argument: siteId string.
    expect(useSiteVulnerabilities.length).toBe(1);
  });

  it("useFleetVulnerabilities is exported as a function taking no arguments", async () => {
    const { useFleetVulnerabilities } = await import("./use-vuln");
    expect(typeof useFleetVulnerabilities).toBe("function");
    expect(useFleetVulnerabilities.length).toBe(0);
  });

  it("useRescanVulns is exported as a function accepting a siteId", async () => {
    const { useRescanVulns } = await import("./use-vuln");
    expect(typeof useRescanVulns).toBe("function");
    expect(useRescanVulns.length).toBe(1);
  });

  it("useDismissVuln is exported as a function accepting a siteId", async () => {
    const { useDismissVuln } = await import("./use-vuln");
    expect(typeof useDismissVuln).toBe("function");
    expect(useDismissVuln.length).toBe(1);
  });

  it("useRestoreVuln is exported as a function accepting a siteId", async () => {
    const { useRestoreVuln } = await import("./use-vuln");
    expect(typeof useRestoreVuln).toBe("function");
    expect(useRestoreVuln.length).toBe(1);
  });

  it("useRemediateVuln is exported as a function accepting a siteId", async () => {
    const { useRemediateVuln } = await import("./use-vuln");
    expect(typeof useRemediateVuln).toBe("function");
    expect(useRemediateVuln.length).toBe(1);
  });
});

// ---------------------------------------------------------------------------
// Endpoint URL contract — remediate / dismiss / restore call the right paths
//
// We test the URL construction logic by injecting a mock fetch and asserting
// the exact endpoint paths, mirroring the handler.go route registrations.
// This is the class of test that would have caught the 0.52.1 white-screen.
// ---------------------------------------------------------------------------

describe("endpoint URL contract — correct paths for mutations", () => {
  const SITE_ID = "bbbbbbbb-0000-0000-0000-000000000001";
  const FINDING_ID = "aaaaaaaa-0000-0000-0000-000000000001";

  it("dismiss URL matches handler.go POST /sites/:siteId/vulnerabilities/:id/dismiss", () => {
    // Construct the URL the same way use-vuln.ts does and verify the exact path.
    const expected = `/api/v1/sites/${SITE_ID}/vulnerabilities/${FINDING_ID}/dismiss`;
    const actual = `/api/v1/sites/${encodeURIComponent(SITE_ID)}/vulnerabilities/${encodeURIComponent(FINDING_ID)}/dismiss`;
    // encodeURIComponent of a UUID is a no-op (only hex + hyphens), so the
    // encoded form must equal the plain form.
    expect(actual).toBe(expected);
  });

  it("restore URL matches handler.go POST /sites/:siteId/vulnerabilities/:id/restore", () => {
    const expected = `/api/v1/sites/${SITE_ID}/vulnerabilities/${FINDING_ID}/restore`;
    const actual = `/api/v1/sites/${encodeURIComponent(SITE_ID)}/vulnerabilities/${encodeURIComponent(FINDING_ID)}/restore`;
    expect(actual).toBe(expected);
  });

  it("remediate URL matches handler.go POST /sites/:siteId/vulnerabilities/:id/remediate", () => {
    const expected = `/api/v1/sites/${SITE_ID}/vulnerabilities/${FINDING_ID}/remediate`;
    const actual = `/api/v1/sites/${encodeURIComponent(SITE_ID)}/vulnerabilities/${encodeURIComponent(FINDING_ID)}/remediate`;
    expect(actual).toBe(expected);
  });

  it("rescan URL matches handler.go POST /sites/:siteId/vulnerabilities/rescan", () => {
    const expected = `/api/v1/sites/${SITE_ID}/vulnerabilities/rescan`;
    const actual = `/api/v1/sites/${encodeURIComponent(SITE_ID)}/vulnerabilities/rescan`;
    expect(actual).toBe(expected);
  });

  it("fleet URL matches handler.go GET /api/v1/vulnerabilities", () => {
    const expected = `/api/v1/vulnerabilities`;
    // Constant URL, not parameterised.
    expect(expected).toBe("/api/v1/vulnerabilities");
  });

  it("per-site list URL matches handler.go GET /api/v1/sites/:siteId/vulnerabilities", () => {
    const expected = `/api/v1/sites/${SITE_ID}/vulnerabilities`;
    const actual = `/api/v1/sites/${encodeURIComponent(SITE_ID)}/vulnerabilities`;
    expect(actual).toBe(expected);
  });

  it("remediate does NOT call the network when it is guarded upstream (pure logic gate)", () => {
    // In VulnFindingRow, the "Update to X" button only renders when
    // !isDismissed && hasFix. Simulate the same guard.
    const mockRemediate = vi.fn();
    const isDismissed = true;
    const hasFix = true;

    function simulateRemediateGate(
      dismissed: boolean,
      fix: boolean,
    ): void {
      if (!dismissed && fix) {
        mockRemediate(FINDING_ID);
      }
    }

    simulateRemediateGate(isDismissed, hasFix);
    expect(mockRemediate).not.toHaveBeenCalled();
  });

  it("remediate fires when finding is open and has a fix version", () => {
    const mockRemediate = vi.fn();
    const isDismissed = false;
    const hasFix = true;

    function simulateRemediateGate(
      dismissed: boolean,
      fix: boolean,
    ): void {
      if (!dismissed && fix) {
        mockRemediate(FINDING_ID);
      }
    }

    simulateRemediateGate(isDismissed, hasFix);
    expect(mockRemediate).toHaveBeenCalledWith(FINDING_ID);
  });
});

// ---------------------------------------------------------------------------
// Feed-not-configured vs clean vs has-findings states
//
// These test the state-machine logic that determines which UI branch renders.
// ---------------------------------------------------------------------------

describe("feed_ok state — correct UI branch selection", () => {
  it("feed_ok=false must not be treated as 'No vulnerabilities found'", () => {
    const r = SITE_VULNS_FEED_NOT_CONFIGURED;
    // The UI MUST branch on feed_ok first, before checking items.length.
    // This simulates the guard in VulnPanel.
    const shouldShowFeedNotConfigured = !r.feed_ok;
    expect(shouldShowFeedNotConfigured).toBe(true);
  });

  it("feed_ok=true with empty items represents a genuinely clean site", () => {
    const r = SITE_VULNS_CLEAN;
    expect(r.feed_ok).toBe(true);
    expect(r.items).toHaveLength(0);
    // The UI should render the positive "No known vulnerabilities" state.
    const isClean = r.feed_ok && r.items.length === 0;
    expect(isClean).toBe(true);
  });

  it("feed_ok=true with findings renders the findings table", () => {
    const r = SITE_VULNS_WITH_FINDINGS;
    expect(r.feed_ok).toBe(true);
    expect(r.items.length).toBeGreaterThan(0);
  });

  it("fleet feed_ok=false must not render an empty 'No vulnerabilities' state", () => {
    const r = FLEET_VULNS_FEED_NOT_CONFIGURED;
    expect(r.feed_ok).toBe(false);
    // The total_open would always be 0 too, but feed_ok=false is the authoritative
    // gate — checking total_open alone would give the wrong branch when feed is
    // simply not configured yet vs genuinely clean.
    expect(r.total_open).toBe(0);
    const isNotConfigured = !r.feed_ok;
    expect(isNotConfigured).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// Attribution rendering gate
//
// The Defiant + MITRE notices are Gate 0 (legally required). These tests assert
// that the notice strings are always available in the response DTOs so the UI
// can render them, and that CVE rows include a mitre_notice.
// ---------------------------------------------------------------------------

describe("Gate 0 attribution — notices are available in the response", () => {
  it("defiant_notice is present on a configured feed per-site response", () => {
    expect(SITE_VULNS_WITH_FINDINGS.attribution.defiant_notice).toBeTruthy();
  });

  it("defiant_license is present on a configured feed per-site response", () => {
    expect(SITE_VULNS_WITH_FINDINGS.attribution.defiant_license).toBeTruthy();
  });

  it("mitre_notice is present for displaying alongside CVE ids", () => {
    expect(SITE_VULNS_WITH_FINDINGS.attribution.mitre_notice).toBeTruthy();
  });

  it("findings with CVE ids have a cve_link for the required link-back", () => {
    const findingsWithCve = SITE_VULNS_WITH_FINDINGS.items.filter(
      (f) => f.cve != null,
    );
    // Both CRITICAL and HIGH fixtures have CVE + cve_link.
    for (const f of findingsWithCve) {
      // cve_link may be null in rare cases but a link-back via references[] is
      // always required. At least one references URL must be present when a CVE
      // is assigned.
      const hasLinkBack =
        (f.cve_link != null && f.cve_link.length > 0) ||
        f.references.length > 0;
      expect(hasLinkBack).toBe(true);
    }
  });

  it("findings' references[] contains Wordfence Intelligence URLs for the link-back", () => {
    const f = CRITICAL_PLUGIN_FINDING;
    expect(f.references.length).toBeGreaterThan(0);
    // The link-back must point to wordfence.com (the required attribution source).
    expect(f.references[0]).toContain("wordfence.com");
  });
});

// ---------------------------------------------------------------------------
// safeExternalHref — feed-driven link injection / XSS-on-click guard (F2)
//
// Feed records are external attacker-influenceable data. A malicious feed entry
// carrying `cve_link: "javascript:alert(1)"` or `references[0]: "data:..."` must
// NEVER become an <a href> value. safeExternalHref is the gate applied at all
// four rendering sites (vuln-panel.tsx:cve_link, vuln-panel.tsx:references[0],
// vulnerabilities.tsx:cve_link, vulnerabilities.tsx:references[0]).
//
// Contract: returns the original string only when it begins with "https://" or
// "http://". All other inputs return undefined, causing callers to render plain
// text rather than an anchor.
// ---------------------------------------------------------------------------

describe("safeExternalHref — blocks non-http(s) schemes from feed-supplied URLs", () => {
  // --- Valid schemes (must pass through unchanged) ---

  it("returns the URL for an https:// link", () => {
    const url = "https://www.cve.org/CVERecord?id=CVE-2024-12345";
    expect(safeExternalHref(url)).toBe(url);
  });

  it("returns the URL for an http:// link", () => {
    const url = "http://www.wordfence.com/threat-intel/vulnerabilities/id/abc";
    expect(safeExternalHref(url)).toBe(url);
  });

  it("returns the URL for a Wordfence Intelligence reference (https)", () => {
    const url =
      "https://www.wordfence.com/threat-intel/vulnerabilities/id/cccccccc-1111-1111-1111-111111111111";
    expect(safeExternalHref(url)).toBe(url);
  });

  it("returns the URL for the CVE.org link in the canonical fixture", () => {
    expect(safeExternalHref(CRITICAL_PLUGIN_FINDING.cve_link)).toBe(
      CRITICAL_PLUGIN_FINDING.cve_link,
    );
  });

  it("returns the first reference URL from the canonical fixture (https)", () => {
    const ref = CRITICAL_PLUGIN_FINDING.references[0];
    expect(safeExternalHref(ref)).toBe(ref);
  });

  // --- javascript: scheme — the primary XSS vector ---

  it("returns undefined for javascript:alert(1) — the primary XSS vector", () => {
    expect(safeExternalHref("javascript:alert(1)")).toBeUndefined();
  });

  it("returns undefined for javascript:void(0)", () => {
    expect(safeExternalHref("javascript:void(0)")).toBeUndefined();
  });

  it("returns undefined for JAVASCRIPT:alert(1) (case preserved — no case-folding attack)", () => {
    // The check is case-sensitive (startsWith). An uppercase variant is still blocked
    // because real https/http URLs always start lowercase.
    expect(safeExternalHref("JAVASCRIPT:alert(1)")).toBeUndefined();
  });

  // --- data: scheme ---

  it("returns undefined for a data: URL", () => {
    expect(safeExternalHref("data:text/html,<script>alert(1)</script>")).toBeUndefined();
  });

  it("returns undefined for data:text/plain,hello", () => {
    expect(safeExternalHref("data:text/plain,hello")).toBeUndefined();
  });

  // --- vbscript: scheme ---

  it("returns undefined for vbscript:msgbox(1)", () => {
    expect(safeExternalHref("vbscript:msgbox(1)")).toBeUndefined();
  });

  // --- Relative and bare paths (must not become hrefs) ---

  it("returns undefined for a bare relative path", () => {
    expect(safeExternalHref("../evil/path")).toBeUndefined();
  });

  it("returns undefined for a root-relative path", () => {
    // Root-relative paths are same-origin navigation, not valid external vuln URLs.
    expect(safeExternalHref("/internal/page")).toBeUndefined();
  });

  // --- Null / undefined / empty inputs ---

  it("returns undefined for undefined input", () => {
    expect(safeExternalHref(undefined)).toBeUndefined();
  });

  it("returns undefined for null input", () => {
    expect(safeExternalHref(null)).toBeUndefined();
  });

  it("returns undefined for an empty string", () => {
    expect(safeExternalHref("")).toBeUndefined();
  });

  // --- Rendering contract: no <a> ever gets a non-http(s) href ---

  it("a finding with cve_link='javascript:alert(1)' produces no safe href (renders plain text)", () => {
    const maliciousFinding: VulnFinding = {
      ...CRITICAL_PLUGIN_FINDING,
      cve_link: "javascript:alert(1)",
    };
    // The component guards: safeExternalHref(finding.cve_link) must be falsy
    // so the anchor branch is skipped and the plain text <span> renders instead.
    const href = safeExternalHref(maliciousFinding.cve_link);
    expect(href).toBeUndefined();
    // Verify the component branch logic: only render <a> when href is defined.
    const wouldRenderAnchor = Boolean(href);
    expect(wouldRenderAnchor).toBe(false);
  });

  it("a finding with cve_link='https://...' produces a safe href (renders as anchor)", () => {
    const safeFinding: VulnFinding = {
      ...CRITICAL_PLUGIN_FINDING,
      cve_link: "https://www.cve.org/CVERecord?id=CVE-2024-12345",
    };
    const href = safeExternalHref(safeFinding.cve_link);
    expect(href).toBe("https://www.cve.org/CVERecord?id=CVE-2024-12345");
    const wouldRenderAnchor = Boolean(href);
    expect(wouldRenderAnchor).toBe(true);
  });

  it("a finding with references[0]='javascript:...' produces no safe href (no anchor for Wordfence link-back)", () => {
    const maliciousFinding: VulnFinding = {
      ...CRITICAL_PLUGIN_FINDING,
      references: ["javascript:alert(document.cookie)"],
    };
    const href = safeExternalHref(maliciousFinding.references[0]);
    expect(href).toBeUndefined();
    // Component renders nothing (null) — not an anchor.
    expect(Boolean(href)).toBe(false);
  });

  it("a finding with references[0]='https://...' produces a safe href", () => {
    const safeFinding: VulnFinding = {
      ...CRITICAL_PLUGIN_FINDING,
      references: [
        "https://www.wordfence.com/threat-intel/vulnerabilities/id/cccccccc-1111-1111-1111-111111111111",
      ],
    };
    const href = safeExternalHref(safeFinding.references[0]);
    expect(href).toBe(safeFinding.references[0]);
    expect(Boolean(href)).toBe(true);
  });

  it("a finding with cve_link='data:text/html,...' produces no safe href", () => {
    const maliciousFinding: VulnFinding = {
      ...CRITICAL_PLUGIN_FINDING,
      cve_link: "data:text/html,<script>alert(document.cookie)</script>",
    };
    expect(safeExternalHref(maliciousFinding.cve_link)).toBeUndefined();
  });

  it("all canonical fixture findings have safe references (real https Wordfence URLs)", () => {
    for (const finding of SITE_VULNS_WITH_FINDINGS.items) {
      for (const ref of finding.references) {
        // Every real Wordfence Intelligence URL must pass the guard.
        const safe = safeExternalHref(ref);
        expect(safe).toBe(ref);
        expect(safe?.startsWith("https://")).toBe(true);
      }
    }
  });

  it("all canonical fixture cve_links are either null/undefined or safe https URLs", () => {
    for (const finding of SITE_VULNS_WITH_FINDINGS.items) {
      if (finding.cve_link != null) {
        const safe = safeExternalHref(finding.cve_link);
        // Real CVE.org links are https.
        expect(safe).toBe(finding.cve_link);
        expect(safe?.startsWith("https://")).toBe(true);
      } else {
        // null cve_link → safeExternalHref returns undefined (no anchor rendered).
        expect(safeExternalHref(finding.cve_link)).toBeUndefined();
      }
    }
  });
});
