// Integration test for the read-only agent-freshness dashboard
// (internal/agentrelease): proves ListSiteAgentVersions enforces the same
// sites_tenant_isolation RLS as every other tenant-scoped fleet rollup,
// against a real Postgres with the production non-superuser role. Requires
// Docker; skips when unavailable (via startPostgres).
package tests

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentplugin"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentrelease"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// TestAgentReleaseFleet_TenantIsolation proves that
// agentrelease.Repo.ListSiteAgentVersions for tenant A never returns tenant
// B's sites, even though both tenants have sites with the same shape
// (matching agent_version values, overlapping names): RLS, not incidental
// data differences, is what isolates the read.
func TestAgentReleaseFleet_TenantIsolation(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	tenantA := seedTenant(t, pool, "agentrel-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, pool, "agentrel-b-"+uuid.NewString()[:8])

	siteRepo := site.NewRepo(pool)
	siteA, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenantA, URL: "https://" + uuid.NewString() + ".example.com", Name: "shared-name",
	})
	if err != nil {
		t.Fatalf("create site A: %v", err)
	}
	siteB, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenantB, URL: "https://" + uuid.NewString() + ".example.com", Name: "shared-name",
	})
	if err != nil {
		t.Fatalf("create site B: %v", err)
	}

	// Both sites report the identical agent_version so a leak cannot be
	// masked or "explained away" by differing data; only the tenant_id must
	// tell them apart.
	setAgentVersion(t, pool, tenantA, siteA.ID, "0.61.90")
	setAgentVersion(t, pool, tenantB, siteB.ID, "0.61.90")

	repo := agentrelease.NewRepo(pool)

	rowsA, err := repo.ListSiteAgentVersions(ctx, tenantA)
	if err != nil {
		t.Fatalf("list tenant A: %v", err)
	}
	if len(rowsA) != 1 || rowsA[0].SiteID != siteA.ID {
		t.Fatalf("tenant A must see exactly its own site, got %+v", rowsA)
	}
	for _, r := range rowsA {
		if r.SiteID == siteB.ID {
			t.Fatalf("tenant A leaked tenant B's site %s", siteB.ID)
		}
	}

	rowsB, err := repo.ListSiteAgentVersions(ctx, tenantB)
	if err != nil {
		t.Fatalf("list tenant B: %v", err)
	}
	if len(rowsB) != 1 || rowsB[0].SiteID != siteB.ID {
		t.Fatalf("tenant B must see exactly its own site, got %+v", rowsB)
	}
	for _, r := range rowsB {
		if r.SiteID == siteA.ID {
			t.Fatalf("tenant B leaked tenant A's site %s", siteA.ID)
		}
	}

	// A third, uninvolved tenant with no sites sees an empty rollup, not an
	// error and not another tenant's rows.
	tenantC := seedTenant(t, pool, "agentrel-c-"+uuid.NewString()[:8])
	rowsC, err := repo.ListSiteAgentVersions(ctx, tenantC)
	if err != nil {
		t.Fatalf("list tenant C: %v", err)
	}
	if len(rowsC) != 0 {
		t.Fatalf("tenant C (no sites) must see zero rows, got %+v", rowsC)
	}
}

// TestAgentReleaseFleet_ArchivedSitesExcluded proves ListSiteAgentVersions
// matches the ListSites/ListAllSiteIDs default (ADR-041): an archived site
// is excluded from the fleet rollup.
func TestAgentReleaseFleet_ArchivedSitesExcluded(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	tenantID := seedTenant(t, pool, "agentrel-arch-"+uuid.NewString()[:8])
	siteRepo := site.NewRepo(pool)
	activeSite, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenantID, URL: "https://" + uuid.NewString() + ".example.com", Name: "active-site",
	})
	if err != nil {
		t.Fatalf("create active site: %v", err)
	}
	archivedSite, err := siteRepo.Create(ctx, site.CreateInput{
		TenantID: tenantID, URL: "https://" + uuid.NewString() + ".example.com", Name: "archived-site",
	})
	if err != nil {
		t.Fatalf("create archived site: %v", err)
	}
	if err := pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE sites SET connection_state = 'archived' WHERE id = $1 AND tenant_id = $2`, archivedSite.ID, tenantID)
		return e
	}); err != nil {
		t.Fatalf("archive site: %v", err)
	}

	repo := agentrelease.NewRepo(pool)
	rows, err := repo.ListSiteAgentVersions(ctx, tenantID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].SiteID != activeSite.ID {
		t.Fatalf("archived site must be excluded, got %+v", rows)
	}
}

// TestAgentReleaseFleet_DistributionFromInventory exercises the part of
// ListSiteAgentVersions no unit test can reach: the plugin_identities JSONB
// projection, run by a real Postgres against a real components document.
//
// It is the whole basis of the "ineligible" classification, so a projection
// that silently returned nothing would leave every plugin-directory site
// reported outdated forever against a release channel it cannot consume, with
// the Go-side unit tests still green. The empty/absent-inventory cases matter
// just as much: jsonb_array_elements errors outright on a non-array, which
// would fail the whole tenant's dashboard rather than one row.
func TestAgentReleaseFleet_DistributionFromInventory(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	tenantID := seedTenant(t, pool, "agentrel-dist-"+uuid.NewString()[:8])
	siteRepo := site.NewRepo(pool)

	cases := []struct {
		name       string
		components string
		want       agentplugin.Distribution
	}{
		{
			name: "directory-build",
			components: `{"plugins":[
				{"slug":"akismet/akismet.php","name":"Akismet Anti-spam","version":"5.3.1"},
				{"slug":"fleet-agent-site-manager/fleet-agent-site-manager.php","name":"Fleet Agent Site Manager","version":"0.61.95"}
			],"themes":[{"slug":"tt4","name":"Twenty Twenty-Four","version":"1.0"}]}`,
			want: agentplugin.DistributionDirectory,
		},
		{
			name: "self-hosted-build",
			components: `{"plugins":[
				{"slug":"wpmgr-agent/wpmgr-agent.php","name":"WPMgr Agent","version":"0.61.95"}
			]}`,
			want: agentplugin.DistributionSelfHosted,
		},
		{
			// The reason the projection carries the name at all: this slug is
			// in no allowlist, so only the plugin header identifies the build.
			name: "directory-build-under-renamed-directory",
			components: `{"plugins":[
				{"slug":"fleet-agent-site-manager-0.61.88/fleet-agent-site-manager.php","name":"Fleet Agent Site Manager","version":"0.61.88"}
			]}`,
			want: agentplugin.DistributionDirectory,
		},
		{
			name:       "no-agent-in-inventory",
			components: `{"plugins":[{"slug":"akismet/akismet.php","name":"Akismet Anti-spam","version":"5.3.1"}]}`,
			want:       agentplugin.DistributionNone,
		},
		{
			name:       "a-plugin-merely-named-like-the-agent",
			components: `{"plugins":[{"slug":"wpmgr-agent-pro/wpmgr-agent-pro.php","name":"WPMgr Agent Pro","version":"2.0.0"}]}`,
			want:       agentplugin.DistributionNone,
		},
		{
			name:       "empty-plugins-array",
			components: `{"plugins":[]}`,
			want:       agentplugin.DistributionNone,
		},
		{
			name:       "inventory-never-reported",
			components: `{}`,
			want:       agentplugin.DistributionNone,
		},
		{
			// The CASE guard: "plugins" present but not an array is exactly
			// what jsonb_array_elements refuses to run on.
			name:       "plugins-key-is-not-an-array",
			components: `{"plugins":{"unexpected":"shape"}}`,
			want:       agentplugin.DistributionNone,
		},
	}

	repo := agentrelease.NewRepo(pool)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := siteRepo.Create(ctx, site.CreateInput{
				TenantID: tenantID,
				URL:      "https://" + uuid.NewString() + ".example.com",
				Name:     tc.name,
			})
			if err != nil {
				t.Fatalf("create site: %v", err)
			}
			setComponents(t, pool, tenantID, s.ID, tc.components)

			rows, err := repo.ListSiteAgentVersions(ctx, tenantID)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			var got agentplugin.Distribution
			found := false
			for _, r := range rows {
				if r.SiteID == s.ID {
					got, found = r.Distribution, true
				}
			}
			if !found {
				t.Fatalf("site %s missing from the rollup", s.ID)
			}
			if got != tc.want {
				t.Fatalf("distribution = %q, want %q", got, tc.want)
			}

			// The classification the dashboard actually reports. Only the
			// plugin-directory build is ineligible; every other site keeps a
			// status the operator can act on.
			status := agentrelease.Classify("0.61.88", "0.61.95", got)
			wantStatus := agentrelease.StatusOutdated
			if tc.want == agentplugin.DistributionDirectory {
				wantStatus = agentrelease.StatusIneligible
			}
			if status != wantStatus {
				t.Fatalf("status = %q, want %q", status, wantStatus)
			}
		})
	}
}

// setComponents writes a site's JSONB inventory directly, standing in for the
// agent's metadata push.
func setComponents(t *testing.T, pool *db.Pool, tenantID, siteID uuid.UUID, components string) {
	t.Helper()
	ctx := context.Background()
	if err := pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE sites SET components = $1::jsonb WHERE id = $2 AND tenant_id = $3`, components, siteID, tenantID)
		return e
	}); err != nil {
		t.Fatalf("set components: %v", err)
	}
}

// setAgentVersion sets a site's agent_version directly (no such helper exists
// on site.Repo/site.CreateInput; agent_version is only ever set by the
// agent's own metadata push in production).
func setAgentVersion(t *testing.T, pool *db.Pool, tenantID, siteID uuid.UUID, version string) {
	t.Helper()
	ctx := context.Background()
	if err := pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE sites SET agent_version = $1 WHERE id = $2 AND tenant_id = $3`, version, siteID, tenantID)
		return e
	}); err != nil {
		t.Fatalf("set agent_version: %v", err)
	}
}
