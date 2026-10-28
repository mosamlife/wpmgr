// gh970_report_all_client_sites_integration_test.go — GH #970 and the GH #969
// version table, end to end against Postgres.
//
// A client report enumerated its sites through site.Service.List, which caps a
// call at one page, so a client with more than 50 sites got a report covering
// 50. This builds the report data through the production sources
// (site.Service.ListAllForClient, report.UpdateReportStatsSource and
// report.ListReportUpdateItemsSource, the functions cmd/wpmgr/main.go wires),
// on the pool startPostgres returns, which connects as wpmgr_app
// (NOSUPERUSER, NOBYPASSRLS). Every row is seeded through InTenantTx.
package tests

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/report"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

func gh970SeedClient(t *testing.T, pool *db.Pool, tenant uuid.UUID, name string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO clients (id, tenant_id, name) VALUES ($1, $2, $3)", id, tenant, name)
		return err
	}); err != nil {
		t.Fatalf("seed client %s: %v", name, err)
	}
	return id
}

func gh970SeedSite(t *testing.T, pool *db.Pool, tenant uuid.UUID, client *uuid.UUID, url, components string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			"INSERT INTO sites (id, tenant_id, url, name, client_id, components) VALUES ($1, $2, $3, $3, $4, $5::jsonb)",
			id, tenant, url, client, components)
		return err
	}); err != nil {
		t.Fatalf("seed site %s: %v", url, err)
	}
	return id
}

func TestGH970ReportCoversEveryClientSite(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	tenant := seedTenant(t, pool, "gh970-agency")
	otherTenant := seedTenant(t, pool, "gh970-other")
	client := gh970SeedClient(t, pool, tenant, "Big Client")
	otherClient := gh970SeedClient(t, pool, tenant, "Other Client")

	const n = 60
	want := make(map[uuid.UUID]bool, n)
	var first uuid.UUID
	for i := 0; i < n; i++ {
		comps := `{}`
		if i == 0 {
			comps = `{"plugins":[{"slug":"akismet","name":"Akismet Anti-spam","version":"5.3"}]}`
		}
		id := gh970SeedSite(t, pool, tenant, &client, fmt.Sprintf("https://c%02d.gh970.example", i), comps)
		want[id] = true
		if i == 0 {
			first = id
		}
	}
	// Sites the report must NOT include: another client's, an unassigned one,
	// and another tenant's site pointed at nothing of ours.
	gh970SeedSite(t, pool, tenant, &otherClient, "https://other-client.gh970.example", `{}`)
	gh970SeedSite(t, pool, tenant, nil, "https://unassigned.gh970.example", `{}`)
	gh970SeedSite(t, pool, otherTenant, nil, "https://other-tenant.gh970.example", `{}`)

	// Two successful updates of one plugin and one failure on the first site.
	from := time.Now().UTC().Add(-48 * time.Hour)
	run := gh969SeedRun(t, pool, tenant)
	for _, task := range []gh969Task{
		{site: first, targetType: "plugin", slug: "akismet", from: "5.1", to: "5.2", status: "succeeded", finishedAt: from.Add(1 * time.Hour)},
		{site: first, targetType: "plugin", slug: "akismet", from: "5.2", to: "5.3", status: "succeeded", finishedAt: from.Add(2 * time.Hour)},
		{site: first, targetType: "theme", slug: "astra", from: "", to: "4.1", status: "failed", finishedAt: from.Add(3 * time.Hour)},
	} {
		gh969SeedTask(t, pool, tenant, run, task)
	}

	siteSvc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})

	// The pre-#970 enumeration, as the positive control for the premise.
	page, err := siteSvc.List(ctx, site.ListInput{TenantID: tenant, ClientID: &client})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page) != 50 {
		t.Fatalf("premise: List's default page returned %d of %d sites, expected 50", len(page), n)
	}

	src := report.Sources{
		ListClientSites:       siteSvc.ListAllForClient,
		GetUpdateReportStats:  report.UpdateReportStatsSource(pool),
		ListReportUpdateItems: report.ListReportUpdateItemsSource(pool),
	}
	rd, err := report.BuildReportData(ctx, src, report.BuildInput{
		TenantID:    tenant,
		ClientID:    client,
		PeriodStart: from,
		PeriodEnd:   time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("BuildReportData: %v", err)
	}

	if rd.Totals.SiteCount != n || len(rd.Sites) != n {
		t.Fatalf("report covers %d sites (totals say %d), want all %d of the client's", len(rd.Sites), rd.Totals.SiteCount, n)
	}
	for _, s := range rd.Sites {
		if !want[s.SiteID] {
			t.Errorf("report includes site %s (%s), which is not this client's", s.SiteID, s.URL)
		}
	}

	var firstUpdates *report.UpdateSection
	for _, s := range rd.Sites {
		if s.SiteID == first {
			firstUpdates = s.Updates
		}
	}
	if firstUpdates == nil {
		t.Fatal("the seeded site has no Updates section")
	}
	if firstUpdates.Plugins != 2 || firstUpdates.Failed != 1 {
		t.Errorf("counts: plugins=%d failed=%d, want 2 and 1", firstUpdates.Plugins, firstUpdates.Failed)
	}
	if len(firstUpdates.Items) != 2 {
		t.Fatalf("version table has %d rows, want 2 (one folded success, one failure): %+v", len(firstUpdates.Items), firstUpdates.Items)
	}
	ok, failed := firstUpdates.Items[0], firstUpdates.Items[1]
	if ok.Name != "Akismet Anti-spam" || ok.FromVersion != "5.1" || ok.ToVersion != "5.3" || ok.Status != "succeeded" {
		t.Errorf("folded row %+v, want Akismet Anti-spam 5.1 -> 5.3 succeeded", ok)
	}
	if failed.Name != "astra" || failed.FromLabel() != "unknown" || failed.Status != "failed" {
		t.Errorf("failed row %+v, want astra (slug fallback) unknown -> 4.1 failed", failed)
	}
	for _, s := range rd.Sites {
		if s.SiteID != first && s.Updates != nil && len(s.Updates.Items) != 0 {
			t.Errorf("site %s shows another site's update rows: %+v", s.URL, s.Updates.Items)
		}
	}
}
