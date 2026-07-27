// vuln_plugin_slug_match_integration_test.go: the ship-blocking regression
// fixture for the plugin-vuln-matching production bug.
//
// Root cause: the agent reports a plugin's installed slug as the
// get_plugins() ARRAY KEY (apps/agent/includes/commands/class-metadata-command.php:322,
// "'slug' => $file"), which for an ordinary directory plugin is a FILE PATH
// like "woocommerce/woocommerce.php", not the bare canonical slug
// ("woocommerce") the Wordfence feed stores in wordfence_vuln_software.slug.
// Before the normSlug directory-cut fix, LookupSoftware's WHERE clause
// compared the untouched (only lower-cased) agent slug against the stored
// canonical slug and NEVER matched, so no plugin vulnerability ever surfaced
// in production, even though themes and core matched fine (their raw and
// canonical forms are already identical).
//
// This test seeds a real Wordfence-shaped feed record through the actual
// ingest path (Repo.UpsertFeedRecord, canonical slug "woocommerce", exactly
// as Wordfence sends it) and proves, against a real Postgres with the
// production non-superuser role and RLS applied:
//
//   - Layer 1 (repo.LookupSoftware): the agent-form slug
//     "woocommerce/woocommerce.php" resolves the seeded record.
//   - Layer 2 (Service.RescanSite end to end): a site whose reported
//     inventory carries the agent-form slug produces an open
//     site_vulnerabilities finding, and the STORED slug column is the RAW
//     agent-inventory form ("woocommerce/woocommerce.php"), not the
//     canonical Wordfence slug: normSlug is used ONLY to derive the
//     LookupSoftware match, never to decide what gets persisted on the
//     finding. Storing the raw slug is what lets Remediate hand it straight
//     to the update domain (which indexes installed components by that same
//     raw form) with no resolution step, and keeps the finding's
//     UNIQUE(site_id, vuln_id, kind, slug) key stable for a mixed-case raw
//     slug across rescans (see service.go RescanSite's Slug comment).
//
// Requires Docker; skips when unavailable (via startPostgres).
package tests

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/update"
	"github.com/mosamlife/wpmgr/apps/api/internal/vuln"
)

// fakeVulnSiteLoaderFixedSnapshot satisfies vuln.SiteLoader by returning a
// single fixed SiteSnapshot regardless of the (tenantID, siteID) requested.
// Sufficient for a single-site RescanSite drive.
type fakeVulnSiteLoaderFixedSnapshot struct {
	snap vuln.SiteSnapshot
}

func (f *fakeVulnSiteLoaderFixedSnapshot) GetSiteForVuln(_ context.Context, _, _ uuid.UUID) (vuln.SiteSnapshot, error) {
	return f.snap, nil
}

func (f *fakeVulnSiteLoaderFixedSnapshot) ListAllSiteIDs(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

// fakeUpdateCreator satisfies vuln.UpdateCreator and records the
// update.CreateRunInput it was called with, so a test can assert exactly
// which item.Slug Service.Remediate passed through.
type fakeUpdateCreator struct {
	called    bool
	lastInput update.CreateRunInput
}

func (f *fakeUpdateCreator) CreateRun(_ context.Context, in update.CreateRunInput) (update.Run, []update.Task, error) {
	f.called = true
	f.lastInput = in
	return update.Run{ID: uuid.New()}, nil, nil
}

func TestVulnPluginSlugMatch_EndToEnd(t *testing.T) {
	pool := startPostgres(t) // app-role pool, RLS enforced, skips if Docker unavailable
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()
	repo := vuln.NewRepo(pool)

	// --- Seed a real-shaped Wordfence feed record via the real ingest path. ---
	// Wordfence's own canonical slug is bare, no slash: "woocommerce".
	affectedVersions, _ := json.Marshal(map[string]any{
		"* - 8.0.2": map[string]any{
			"from_version": "*", "from_inclusive": true,
			"to_version": "8.0.2", "to_inclusive": true,
		},
	})
	patchedVersions, _ := json.Marshal([]string{"8.0.3"})
	refURLs, _ := json.Marshal([]string{
		"https://www.wordfence.com/threat-intel/vulnerabilities/id/dddddddd-0000-0000-0000-000000000001",
	})
	score := 9.8
	rec := vuln.FeedRecord{
		VulnID:     "dddddddd-0000-0000-0000-000000000001",
		Title:      "WooCommerce <= 8.0.2 - SQL Injection",
		CVE:        "CVE-2026-30001",
		CVELink:    "https://www.cve.org/CVERecord?id=CVE-2026-30001",
		CVSSScore:  &score,
		CVSSRating: "Critical",
		References: refURLs,
		Raw:        []byte(`{"_test":true}`),
		Software: []vuln.SoftwareRow{
			{
				Kind:             vuln.KindPlugin,
				Slug:             "woocommerce", // canonical, exactly as Wordfence sends it
				AffectedVersions: affectedVersions,
				Patched:          true,
				PatchedVersions:  patchedVersions,
			},
		},
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed tx: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.agent','on',true)"); err != nil {
		t.Fatalf("set agent guc: %v", err)
	}
	if err := repo.UpsertFeedRecord(ctx, tx, rec); err != nil {
		t.Fatalf("seed feed record: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	// --- Layer 1: LookupSoftware with the AGENT-form slug must match. ---
	t.Run("lookup_software_matches_agent_form_slug", func(t *testing.T) {
		rows, err := repo.LookupSoftware(ctx, vuln.KindPlugin, "woocommerce/woocommerce.php")
		if err != nil {
			t.Fatalf("LookupSoftware: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf(
				"LookupSoftware(plugin, %q) returned %d rows; want 1 "+
					"(this fails against the pre-fix normSlug, which only lower-cases "+
					"and never strips the /file.php suffix, so it never matches the "+
					"feed's stored canonical slug)",
				"woocommerce/woocommerce.php", len(rows),
			)
		}
		if rows[0].VulnID != rec.VulnID {
			t.Errorf("VulnID = %q; want %q", rows[0].VulnID, rec.VulnID)
		}
		if rows[0].Slug != "woocommerce" {
			t.Errorf("stored wordfence_vuln_software.slug = %q; want canonical %q", rows[0].Slug, "woocommerce")
		}
	})

	// --- Layer 2: end-to-end RescanSite must produce a finding, with the
	// RAW agent-inventory slug stored on site_vulnerabilities. ---
	t.Run("rescan_site_produces_finding_with_raw_slug", func(t *testing.T) {
		tenantID := seedTenant(t, pool, "vuln-slug-"+uuid.NewString()[:8])
		siteID := seedSiteFor(t, admin, tenantID, "https://vuln-slug-site.example.com")

		// RescanSite gates on feed meta OK. The m79 migration seeds the
		// wordfence_vuln_feed_meta singleton row (id=1, ok=false by default);
		// flip it to ok=true for this test via the superuser pool (no RLS on
		// this table, but the app pool has no special grant assumption to rely
		// on here, so use admin for consistency with the rest of the fixture).
		if _, err := admin.Exec(ctx,
			`UPDATE wordfence_vuln_feed_meta SET ok = true, fetched_at = now(), record_count = 1 WHERE id = 1`,
		); err != nil {
			t.Fatalf("stamp feed meta ok: %v", err)
		}

		sites := &fakeVulnSiteLoaderFixedSnapshot{
			snap: vuln.SiteSnapshot{
				ID:       siteID,
				TenantID: tenantID,
				Name:     "vuln-slug-site",
				URL:      "https://vuln-slug-site.example.com",
				Plugins: []vuln.ComponentSnapshot{
					// The RAW agent-inventory slug: a get_plugins() file path.
					{Slug: "woocommerce/woocommerce.php", Name: "WooCommerce", Version: "8.0.0"},
				},
			},
		}
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		svc := vuln.NewService(repo, pool, sites, nil, nil, logger)

		if err := svc.RescanSite(ctx, tenantID, siteID); err != nil {
			t.Fatalf("RescanSite: %v", err)
		}

		// Read back through the superuser pool: site_vulnerabilities has
		// FORCE ROW LEVEL SECURITY, and this bare read carries no app.tenant_id
		// GUC, so an app-pool read here would be silently filtered to zero rows
		// by RLS rather than proving anything about the write RescanSite itself
		// performed (correctly, via InTenantTx).
		var storedSlug, storedVulnID, storedStatus string
		err := admin.QueryRow(ctx,
			`SELECT slug, vuln_id, status FROM site_vulnerabilities
			 WHERE tenant_id = $1 AND site_id = $2 AND kind = 'plugin'`,
			tenantID, siteID,
		).Scan(&storedSlug, &storedVulnID, &storedStatus)
		if err != nil {
			t.Fatalf("expected a matched plugin finding row, got none: %v", err)
		}
		if storedVulnID != rec.VulnID {
			t.Errorf("stored vuln_id = %q; want %q", storedVulnID, rec.VulnID)
		}
		if storedStatus != vuln.StatusOpen {
			t.Errorf("stored status = %q; want %q", storedStatus, vuln.StatusOpen)
		}
		if storedSlug != "woocommerce/woocommerce.php" {
			t.Errorf("stored site_vulnerabilities.slug = %q; want the RAW agent-inventory form %q (not the canonical Wordfence slug: the match must happen via normSlug, but the finding must persist the raw slug the update domain indexes by)", storedSlug, "woocommerce/woocommerce.php")
		}
	})
}

// TestVulnRemediate_PassesRawSlugThrough proves Service.Remediate hands a
// plugin finding's stored slug straight through to update.Service.CreateRun
// with no transformation. RescanSite now stores the RAW agent-inventory slug
// on site_vulnerabilities.slug (a get_plugins() file path for a plugin, e.g.
// "woocommerce/woocommerce.php") rather than the canonical Wordfence slug —
// that is exactly the form the update domain indexes a site's installed
// components by (update.Component.Slug is a straight passthrough of
// site.Component.Slug; see cmd/wpmgr/siteadapter.go toUpdateComponent), so
// no resolution step is needed or performed. This test seeds a plugin
// finding the way RescanSite now writes it (raw slug) and asserts the
// update.Item Remediate builds carries that exact same raw slug, unchanged.
func TestVulnRemediate_PassesRawSlugThrough(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	defer admin.Close()
	ctx := context.Background()
	repo := vuln.NewRepo(pool)

	tenantID := seedTenant(t, pool, "vuln-remediate-"+uuid.NewString()[:8])
	siteID := seedSiteFor(t, admin, tenantID, "https://vuln-remediate-site.example.com")

	const rawSlug = "woocommerce/woocommerce.php"

	// Seed a finding exactly the way RescanSite now writes it: the RAW
	// agent-inventory slug, not a canonical Wordfence slug.
	if err := pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		return repo.UpsertFinding(ctx, tx, vuln.FindingUpsert{
			TenantID:         tenantID,
			SiteID:           siteID,
			VulnID:           "remediate-test-vuln-1",
			Kind:             vuln.KindPlugin,
			Slug:             rawSlug,
			Name:             "WooCommerce",
			InstalledVersion: "8.0.0",
			FixedVersion:     "8.0.3",
			Severity:         "critical",
		})
	}); err != nil {
		t.Fatalf("seed finding: %v", err)
	}

	var findingID uuid.UUID
	if err := admin.QueryRow(ctx,
		`SELECT id FROM site_vulnerabilities WHERE tenant_id = $1 AND site_id = $2 AND vuln_id = $3`,
		tenantID, siteID, "remediate-test-vuln-1",
	).Scan(&findingID); err != nil {
		t.Fatalf("read back finding id: %v", err)
	}

	// sites is a valid, non-nil SiteLoader input (required by NewService) but
	// is not consulted by Remediate itself: Remediate now passes f.Slug
	// straight through with no re-loading of the site's live inventory.
	sites := &fakeVulnSiteLoaderFixedSnapshot{
		snap: vuln.SiteSnapshot{
			ID:       siteID,
			TenantID: tenantID,
			Plugins: []vuln.ComponentSnapshot{
				{Slug: rawSlug, Name: "WooCommerce", Version: "8.0.0"},
			},
		},
	}
	updates := &fakeUpdateCreator{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := vuln.NewService(repo, pool, sites, updates, nil, logger)

	if _, _, err := svc.Remediate(ctx, tenantID, siteID, findingID, uuid.New()); err != nil {
		t.Fatalf("Remediate: %v", err)
	}
	if !updates.called {
		t.Fatal("update.Service.CreateRun was never called")
	}
	if len(updates.lastInput.Items) != 1 {
		t.Fatalf("CreateRun Items = %d; want 1", len(updates.lastInput.Items))
	}
	gotSlug := updates.lastInput.Items[0].Slug
	if gotSlug != rawSlug {
		t.Errorf(
			"update.Item.Slug = %q; want the stored finding slug %q passed "+
				"straight through unchanged (Remediate must not transform the "+
				"slug it read off the finding)",
			gotSlug, rawSlug,
		)
	}
}
