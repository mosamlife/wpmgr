package vuln_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/vuln"
)

func claimedFinding(siteID uuid.UUID, siteName, kind, name, severity string, firstSeen time.Time) vuln.ClaimedFinding {
	return vuln.ClaimedFinding{
		Finding: vuln.Finding{
			ID:               uuid.New(),
			SiteID:           siteID,
			Kind:             kind,
			Name:             name,
			InstalledVersion: "1.0.0",
			Severity:         severity,
			FirstSeen:        firstSeen,
		},
		SiteName: siteName,
		SiteURL:  "https://" + siteName,
	}
}

// ---------------------------------------------------------------------------
// Severity threshold filter
// ---------------------------------------------------------------------------

func TestPassesSeverityThreshold(t *testing.T) {
	cases := []struct {
		severity, minSeverity string
		want                  bool
	}{
		// Unknown ALWAYS passes, regardless of how high the threshold is —
		// the whole point (GH #247): an un-enriched Scanner-only finding must
		// never be excluded from its first-ever alert opportunity.
		{vuln.SeverityUnknown, vuln.SeverityCritical, true},
		{vuln.SeverityUnknown, vuln.SeverityLow, true},

		{vuln.SeverityCritical, vuln.SeverityHigh, true},
		{vuln.SeverityHigh, vuln.SeverityHigh, true},
		{vuln.SeverityMedium, vuln.SeverityHigh, false},
		{vuln.SeverityLow, vuln.SeverityHigh, false},

		{vuln.SeverityLow, vuln.SeverityLow, true},
		{vuln.SeverityMedium, vuln.SeverityLow, true},
	}
	for _, tc := range cases {
		got := vuln.PassesSeverityThreshold(tc.severity, tc.minSeverity)
		if got != tc.want {
			t.Errorf("PassesSeverityThreshold(%q, %q) = %v, want %v", tc.severity, tc.minSeverity, got, tc.want)
		}
	}
}

func TestFilterBySeverity_UnknownIncludedBelowThreshold(t *testing.T) {
	site := uuid.New()
	now := time.Now()
	claimed := []vuln.ClaimedFinding{
		claimedFinding(site, "a.example.com", vuln.KindPlugin, "P1", vuln.SeverityCritical, now),
		claimedFinding(site, "a.example.com", vuln.KindPlugin, "P2", vuln.SeverityMedium, now),  // below "high" threshold
		claimedFinding(site, "a.example.com", vuln.KindPlugin, "P3", vuln.SeverityUnknown, now), // always included
		claimedFinding(site, "a.example.com", vuln.KindPlugin, "P4", vuln.SeverityLow, now),     // below threshold
	}
	got := vuln.FilterBySeverity(claimed, vuln.SeverityHigh)
	if len(got) != 2 {
		t.Fatalf("expected 2 findings to pass (critical + unknown), got %d: %+v", len(got), got)
	}
	names := map[string]bool{}
	for _, c := range got {
		names[c.Finding.Name] = true
	}
	if !names["P1"] || !names["P3"] {
		t.Errorf("expected P1 (critical) and P3 (unknown) to pass, got %v", names)
	}
}

// ---------------------------------------------------------------------------
// Email data builder
// ---------------------------------------------------------------------------

func TestBuildVulnAlertEmailData_ShapeAndFixedVersionDisplay(t *testing.T) {
	siteA := uuid.New()
	now := time.Now()
	f := vuln.ClaimedFinding{
		Finding: vuln.Finding{
			ID:               uuid.New(),
			SiteID:           siteA,
			Kind:             vuln.KindPlugin,
			Name:             "Rank Math SEO",
			InstalledVersion: "1.0.98",
			FixedVersion:     "", // must render as "no fixed version yet"
			Severity:         vuln.SeverityCritical,
			CVE:              "CVE-2024-12345",
			FirstSeen:        now,
		},
		SiteName: "example.com",
		SiteURL:  "https://example.com",
	}

	data := vuln.BuildVulnAlertEmailData([]vuln.ClaimedFinding{f}, "https://manage.wpmgr.app")

	if data["NewCount"] != 1 {
		t.Errorf("NewCount = %v, want 1", data["NewCount"])
	}
	if data["SiteCount"] != 1 {
		t.Errorf("SiteCount = %v, want 1", data["SiteCount"])
	}
	if data["OverflowCount"] != 0 {
		t.Errorf("OverflowCount = %v, want 0", data["OverflowCount"])
	}
	if data["DashboardURL"] != "https://manage.wpmgr.app/vulnerabilities" {
		t.Errorf("DashboardURL = %v", data["DashboardURL"])
	}

	sites, ok := data["Sites"].([]map[string]any)
	if !ok || len(sites) != 1 {
		t.Fatalf("expected 1 site group, got %v", data["Sites"])
	}
	if sites[0]["SiteName"] != "example.com" {
		t.Errorf("SiteName = %v", sites[0]["SiteName"])
	}
	findings, ok := sites[0]["Findings"].([]map[string]any)
	if !ok || len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %v", sites[0]["Findings"])
	}
	if findings[0]["Component"] != "Plugin: Rank Math SEO" {
		t.Errorf("Component = %v, want %q", findings[0]["Component"], "Plugin: Rank Math SEO")
	}
	if findings[0]["FixedVersion"] != "no fixed version yet" {
		t.Errorf("FixedVersion = %v, want %q", findings[0]["FixedVersion"], "no fixed version yet")
	}
	if findings[0]["Severity"] != "Critical" {
		t.Errorf("Severity = %v, want %q", findings[0]["Severity"], "Critical")
	}
}

// TestBuildVulnAlertEmailData_CapAndOverflow proves the 20-item email display
// cap and its "+N more" overflow count, and that NewCount/SiteCount reflect
// the FULL (uncapped) set, not just what's shown.
func TestBuildVulnAlertEmailData_CapAndOverflow(t *testing.T) {
	site := uuid.New()
	now := time.Now()
	var claimed []vuln.ClaimedFinding
	for i := 0; i < 25; i++ {
		claimed = append(claimed, claimedFinding(site, "example.com", vuln.KindPlugin, "P", vuln.SeverityHigh, now))
	}

	data := vuln.BuildVulnAlertEmailData(claimed, "https://manage.wpmgr.app")

	if data["NewCount"] != 25 {
		t.Errorf("NewCount = %v, want 25 (uncapped)", data["NewCount"])
	}
	if data["OverflowCount"] != 5 {
		t.Errorf("OverflowCount = %v, want 5", data["OverflowCount"])
	}
	sites := data["Sites"].([]map[string]any)
	total := 0
	for _, s := range sites {
		total += len(s["Findings"].([]map[string]any))
	}
	if total != 20 {
		t.Errorf("total rendered findings = %d, want 20 (the cap)", total)
	}
}

// TestBuildVulnAlertEmailData_SeverityGrouping proves the display order:
// each site's most-severe finding first, and within a site,
// critical > high > unknown > medium > low.
func TestBuildVulnAlertEmailData_SeverityGrouping(t *testing.T) {
	site := uuid.New()
	now := time.Now()
	claimed := []vuln.ClaimedFinding{
		claimedFinding(site, "example.com", vuln.KindPlugin, "Low1", vuln.SeverityLow, now),
		claimedFinding(site, "example.com", vuln.KindPlugin, "Crit1", vuln.SeverityCritical, now),
		claimedFinding(site, "example.com", vuln.KindPlugin, "Unk1", vuln.SeverityUnknown, now),
		claimedFinding(site, "example.com", vuln.KindPlugin, "Med1", vuln.SeverityMedium, now),
		claimedFinding(site, "example.com", vuln.KindPlugin, "High1", vuln.SeverityHigh, now),
	}
	data := vuln.BuildVulnAlertEmailData(claimed, "")
	sites := data["Sites"].([]map[string]any)
	findings := sites[0]["Findings"].([]map[string]any)
	wantOrder := []string{"Critical", "High", "Unknown (no CVSS yet)", "Medium", "Low"}
	if len(findings) != len(wantOrder) {
		t.Fatalf("expected %d findings, got %d", len(wantOrder), len(findings))
	}
	for i, want := range wantOrder {
		if findings[i]["Severity"] != want {
			t.Errorf("position %d: severity = %v, want %q", i, findings[i]["Severity"], want)
		}
	}
}

// ---------------------------------------------------------------------------
// Webhook payload builder
// ---------------------------------------------------------------------------

func TestBuildVulnAlertWebhookPayload_Uncapped(t *testing.T) {
	site := uuid.New()
	now := time.Now()
	var claimed []vuln.ClaimedFinding
	for i := 0; i < 25; i++ {
		claimed = append(claimed, claimedFinding(site, "example.com", vuln.KindPlugin, "P", vuln.SeverityHigh, now))
	}
	tenantID := uuid.New()

	payload := vuln.BuildVulnAlertWebhookPayload(tenantID, claimed)

	if payload.Event != vuln.VulnAlertEvent {
		t.Errorf("Event = %q, want %q", payload.Event, vuln.VulnAlertEvent)
	}
	if payload.TenantID != tenantID.String() {
		t.Errorf("TenantID = %q, want %q", payload.TenantID, tenantID.String())
	}
	if payload.NewCount != 25 {
		t.Errorf("NewCount = %d, want 25 (webhook is never capped)", payload.NewCount)
	}
	if payload.SiteCount != 1 {
		t.Errorf("SiteCount = %d, want 1", payload.SiteCount)
	}
	if len(payload.Sites) != 1 || len(payload.Sites[0].Findings) != 25 {
		t.Fatalf("expected 1 site with 25 findings, got %+v", payload.Sites)
	}
}
