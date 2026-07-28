package agentrelease_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentplugin"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentrelease"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// site builds one fleet row with an unidentified distribution, i.e. an
// ordinary site that CAN consume the release channel.
func site(version string) agentrelease.SiteAgentVersion {
	return agentrelease.SiteAgentVersion{
		SiteID:       uuid.New(),
		SiteName:     "site-" + version,
		AgentVersion: version,
	}
}

// rollupFor runs FleetRollup for a one-tenant fleet against the given
// published version ("" = manifest unreadable, the self-hosted steady state).
func rollupFor(t *testing.T, published string, rows []agentrelease.SiteAgentVersion) agentrelease.FleetSummary {
	t.Helper()
	tenantID := uuid.New()
	repo := &fakeSiteLister{byTenant: map[uuid.UUID][]agentrelease.SiteAgentVersion{tenantID: rows}}
	svc := agentrelease.NewService(repo, &fakeVersionReader{version: published})
	summary, err := svc.FleetRollup(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("FleetRollup: %v", err)
	}
	return summary
}

func assertSummary(t *testing.T, got agentrelease.FleetSummary, wantVersion string, wantSource agentrelease.ReferenceSource, wantCounts agentrelease.Counts) {
	t.Helper()
	if got.ReferenceVersion != wantVersion {
		t.Errorf("ReferenceVersion = %q; want %q", got.ReferenceVersion, wantVersion)
	}
	if got.ReferenceSource != wantSource {
		t.Errorf("ReferenceSource = %q; want %q", got.ReferenceSource, wantSource)
	}
	if got.Counts != wantCounts {
		t.Errorf("Counts = %+v; want %+v", got.Counts, wantCounts)
	}
}

// TestFleetRollup_PublishedManifestUnchanged is the no-regression pin: when
// the manifest reads, the published version is the reference and the counts
// are exactly what they were before the fleet-derived fallback existed. In
// particular the newest version PRESENT in the fleet (0.62.0 here) must not
// displace the published one.
func TestFleetRollup_PublishedManifestUnchanged(t *testing.T) {
	summary := rollupFor(t, "0.61.95", []agentrelease.SiteAgentVersion{
		site("0.61.90"),
		site("0.61.95"),
		site("0.62.0"),
		site(""),
	})
	assertSummary(t, summary, "0.61.95", agentrelease.ReferenceSourcePublished,
		agentrelease.Counts{Current: 2, Outdated: 1, Unknown: 1})
}

// TestFleetRollup_NoManifestDerivesNewestInFleet is the reported bug (GH
// #255): a self-hosted install has no published manifest, and before the
// fallback every site classified "unknown" no matter what it reported. The
// newest well-formed version in the tenant's own fleet is now the reference,
// so the laggards read "outdated" and the operator's actual question is
// answered.
func TestFleetRollup_NoManifestDerivesNewestInFleet(t *testing.T) {
	summary := rollupFor(t, "", []agentrelease.SiteAgentVersion{
		site("0.61.90"),
		site("0.61.97"),
		site("0.61.95"),
	})
	assertSummary(t, summary, "0.61.97", agentrelease.ReferenceSourceFleet,
		agentrelease.Counts{Current: 1, Outdated: 2})

	byVersion := map[string]agentrelease.Status{}
	for _, row := range summary.Sites {
		byVersion[row.AgentVersion] = row.Status
	}
	for version, want := range map[string]agentrelease.Status{
		"0.61.90": agentrelease.StatusOutdated,
		"0.61.95": agentrelease.StatusOutdated,
		"0.61.97": agentrelease.StatusCurrent,
	} {
		if got := byVersion[version]; got != want {
			t.Errorf("site on %s = %q; want %q", version, got, want)
		}
	}
}

// TestFleetRollup_NoManifestAllEqualInventsNoUpdate: with every site on the
// same version, the newest present IS that version, so the honest answer is
// "all current". The fallback must never manufacture an update that does not
// exist.
func TestFleetRollup_NoManifestAllEqualInventsNoUpdate(t *testing.T) {
	summary := rollupFor(t, "", []agentrelease.SiteAgentVersion{
		site("0.61.97"),
		site("0.61.97"),
		site("0.61.97"),
	})
	assertSummary(t, summary, "0.61.97", agentrelease.ReferenceSourceFleet,
		agentrelease.Counts{Current: 3})
}

// TestFleetRollup_NoManifestNoUsableVersions: no manifest AND nothing
// well-formed anywhere leaves nothing safe to compare against. Source "none"
// plus every site "unknown" is the truthful answer, not a fabricated
// reference.
func TestFleetRollup_NoManifestNoUsableVersions(t *testing.T) {
	summary := rollupFor(t, "", []agentrelease.SiteAgentVersion{
		site(""),
		site("not-a-version"),
		site("5"),
	})
	assertSummary(t, summary, "", agentrelease.ReferenceSourceNone,
		agentrelease.Counts{Unknown: 3})
}

// TestFleetRollup_MalformedManifestFallsBackToFleet: a manifest that reads
// but carries garbage is no more usable than an absent one, and must take the
// same honest fallback rather than poisoning every comparison.
func TestFleetRollup_MalformedManifestFallsBackToFleet(t *testing.T) {
	summary := rollupFor(t, "unknown", []agentrelease.SiteAgentVersion{
		site("0.61.90"),
		site("0.61.97"),
	})
	assertSummary(t, summary, "0.61.97", agentrelease.ReferenceSourceFleet,
		agentrelease.Counts{Current: 1, Outdated: 1})
}

// TestFleetRollup_EmptyFleetIsSourceNone: a tenant with no sites at all has
// no reference to derive and reports none, not an empty-string reference the
// caller would have to interpret.
func TestFleetRollup_EmptyFleetIsSourceNone(t *testing.T) {
	summary := rollupFor(t, "", nil)
	assertSummary(t, summary, "", agentrelease.ReferenceSourceNone, agentrelease.Counts{})
	if len(summary.Sites) != 0 {
		t.Errorf("Sites = %d; want 0", len(summary.Sites))
	}
}

// TestFleetRollup_FleetReferenceKeepsDirectoryBuildIneligible: the
// plugin-directory build has no self-updater, so it stays ineligible under a
// fleet-derived reference exactly as it does under a published one. Its
// version still counts toward the reference, because it is a real agent
// version somebody in this fleet is running.
func TestFleetRollup_FleetReferenceKeepsDirectoryBuildIneligible(t *testing.T) {
	directory := site("0.61.97")
	directory.Distribution = agentplugin.DistributionDirectory
	summary := rollupFor(t, "", []agentrelease.SiteAgentVersion{
		site("0.61.90"),
		directory,
	})
	assertSummary(t, summary, "0.61.97", agentrelease.ReferenceSourceFleet,
		agentrelease.Counts{Outdated: 1, Ineligible: 1})
}

// TestFleetRollup_FleetReferenceNeverCrossesTenants is the tenancy pin. Two
// tenants share one repo; tenant B runs a newer agent than anything tenant A
// has. With no published manifest, tenant A's reference must be derived from
// tenant A's OWN fleet only: its sites are current against their own newest,
// and B's 0.61.99 must never leak in to mark them outdated (nor appear as A's
// reference version).
func TestFleetRollup_FleetReferenceNeverCrossesTenants(t *testing.T) {
	tenantA := uuid.New()
	tenantB := uuid.New()

	repo := &fakeSiteLister{byTenant: map[uuid.UUID][]agentrelease.SiteAgentVersion{
		tenantA: {site("0.61.90"), site("0.61.90")},
		tenantB: {site("0.61.99")},
	}}
	svc := agentrelease.NewService(repo, &fakeVersionReader{version: ""})

	summaryA, err := svc.FleetRollup(context.Background(), tenantA)
	if err != nil {
		t.Fatalf("FleetRollup(tenant A): %v", err)
	}
	assertSummary(t, summaryA, "0.61.90", agentrelease.ReferenceSourceFleet,
		agentrelease.Counts{Current: 2})
	if summaryA.ReferenceVersion == "0.61.99" {
		t.Fatalf("tenant A's reference leaked tenant B's version 0.61.99")
	}

	summaryB, err := svc.FleetRollup(context.Background(), tenantB)
	if err != nil {
		t.Fatalf("FleetRollup(tenant B): %v", err)
	}
	assertSummary(t, summaryB, "0.61.99", agentrelease.ReferenceSourceFleet,
		agentrelease.Counts{Current: 1})
}

// TestFleetRollup_OneSiteUpdatedFirstOutdatesTheRest is the case that must
// never regress. One site updated first is the NORMAL shape of a rollout in
// progress, not an edge case.
//
// A previous revision required two distinct sites to report a version before it
// could become the reference. Under that rule this exact fleet answered
// reference 0.61.97 with "25 current, 0 outdated": a confident all-clear while
// 24 sites were genuinely behind an agent that demonstrably exists in this very
// fleet. Suppressing a real update is a far worse failure than surfacing an
// unusual one, and it is the same "everything looks current while things are
// behind" defect that was already fixed on the published path.
func TestFleetRollup_OneSiteUpdatedFirstOutdatesTheRest(t *testing.T) {
	rows := make([]agentrelease.SiteAgentVersion, 0, 25)
	for i := 0; i < 24; i++ {
		rows = append(rows, site("0.61.97"))
	}
	rows = append(rows, site("0.62.0"))

	summary := rollupFor(t, "", rows)
	assertSummary(t, summary, "0.62.0", agentrelease.ReferenceSourceFleet,
		agentrelease.Counts{Current: 1, Outdated: 24})
}

// TestFleetRollup_LoneNewestVersionIsTheReference pins the deliberate other
// side of that trade. A single site reporting a version nobody else has (a
// locally built agent, a sideloaded dev build) DOES become the reference, and
// the rest of the fleet reads outdated behind it.
//
// That is accepted on purpose. The claim this reference carries on the wire is
// literally "the newest agent version seen in this fleet": if one site reports
// 9.9.9 then 9.9.9 genuinely is the newest seen here, the statement stays true,
// and the outlier site is right there in the list the operator is reading. A
// version this rule cannot become is checked separately, see
// TestFleetRollup_PreReleaseNeverBecomesTheReference.
func TestFleetRollup_LoneNewestVersionIsTheReference(t *testing.T) {
	rows := make([]agentrelease.SiteAgentVersion, 0, 25)
	for i := 0; i < 24; i++ {
		rows = append(rows, site("0.61.97"))
	}
	rows = append(rows, site("9.9.9"))

	summary := rollupFor(t, "", rows)
	assertSummary(t, summary, "9.9.9", agentrelease.ReferenceSourceFleet,
		agentrelease.Counts{Current: 1, Outdated: 24})
}

// TestFleetRollup_PreReleaseNeverBecomesTheReference: a pre-release or
// build-tagged version is by definition not the release the fleet is expected
// to be on, so it is never eligible to BE the reference (the site running it
// is still classified, and comes out current because it is ahead). Unlike a
// corroboration count, this exclusion cannot suppress a legitimate stable
// rollout, because a stable release carries no such suffix.
func TestFleetRollup_PreReleaseNeverBecomesTheReference(t *testing.T) {
	for _, outlier := range []string{"0.62.0-rc.1", "0.62.0+build.7"} {
		t.Run(outlier, func(t *testing.T) {
			// Exactly one site on the stable version, so this also proves the
			// exclusion is not doing its job by accident via a headcount: the
			// lone stable 0.61.97 wins over the pre-release above it.
			rows := []agentrelease.SiteAgentVersion{site("0.61.97"), site(outlier)}
			summary := rollupFor(t, "", rows)
			assertSummary(t, summary, "0.61.97", agentrelease.ReferenceSourceFleet,
				agentrelease.Counts{Current: 2})
		})
	}
}

// TestFleetRollup_OnlyPreReleasesIsSourceNone: with nothing but pre-releases in
// the fleet there is no version anyone should be told to move to, so the honest
// answer is no reference at all rather than promoting a release candidate.
func TestFleetRollup_OnlyPreReleasesIsSourceNone(t *testing.T) {
	summary := rollupFor(t, "", []agentrelease.SiteAgentVersion{
		site("0.62.0-rc.1"), site("0.62.0-rc.2"),
	})
	assertSummary(t, summary, "", agentrelease.ReferenceSourceNone,
		agentrelease.Counts{Unknown: 2})
}

// TestFleetAgents_WireShapeCarriesReferenceSource proves the discriminator
// reaches the wire on both paths, so the UI never has to infer the source
// from an empty or sentinel version string. This is the exact payload GH
// #255's reporter would now receive: latest_version "unknown" became a real
// version, with reference_source saying where it came from.
func TestFleetAgents_WireShapeCarriesReferenceSource(t *testing.T) {
	type fleetResponse struct {
		LatestVersion   string `json:"latest_version"`
		ReferenceSource string `json:"reference_source"`
		Counts          struct {
			Current    int `json:"current"`
			Outdated   int `json:"outdated"`
			Unknown    int `json:"unknown"`
			Ineligible int `json:"ineligible"`
		} `json:"counts"`
	}

	cases := []struct {
		name       string
		published  string
		rows       []agentrelease.SiteAgentVersion
		wantLatest string
		wantSource string
		wantCounts [4]int // current, outdated, unknown, ineligible
	}{
		{
			name:       "published manifest",
			published:  "0.61.97",
			rows:       []agentrelease.SiteAgentVersion{site("0.61.90"), site("0.61.97")},
			wantLatest: "0.61.97",
			wantSource: "published",
			wantCounts: [4]int{1, 1, 0, 0},
		},
		{
			name:       "self hosted fallback",
			published:  "",
			rows:       []agentrelease.SiteAgentVersion{site("0.61.90"), site("0.61.97")},
			wantLatest: "0.61.97",
			wantSource: "fleet",
			wantCounts: [4]int{1, 1, 0, 0},
		},
		{
			name:       "nothing to compare against",
			published:  "",
			rows:       []agentrelease.SiteAgentVersion{site(""), site("")},
			wantLatest: "unknown",
			wantSource: "none",
			wantCounts: [4]int{0, 0, 2, 0},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tenantID := uuid.New()
			repo := &fakeSiteLister{byTenant: map[uuid.UUID][]agentrelease.SiteAgentVersion{tenantID: tc.rows}}
			svc := agentrelease.NewService(repo, &fakeVersionReader{version: tc.published})
			engine := newTestEngine(agentrelease.NewHandler(svc, false))
			p := domain.Principal{
				TenantID: tenantID,
				Type:     domain.PrincipalUser,
				UserID:   uuid.New(),
				Role:     string(authz.RoleViewer),
			}

			w := doRequest(engine, http.MethodGet, "/api/v1/fleet/agents", p)
			if w.Code != http.StatusOK {
				t.Fatalf("GET /fleet/agents = %d; want 200, body=%s", w.Code, w.Body.String())
			}
			var resp fleetResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.LatestVersion != tc.wantLatest {
				t.Errorf("latest_version = %q; want %q", resp.LatestVersion, tc.wantLatest)
			}
			if resp.ReferenceSource != tc.wantSource {
				t.Errorf("reference_source = %q; want %q", resp.ReferenceSource, tc.wantSource)
			}
			got := [4]int{resp.Counts.Current, resp.Counts.Outdated, resp.Counts.Unknown, resp.Counts.Ineligible}
			if got != tc.wantCounts {
				t.Errorf("counts = %v; want %v", got, tc.wantCounts)
			}
		})
	}
}
