// gh969_report_update_items_integration_test.go — GH #969, the database half.
//
// Client reports gain a table of the plugin, theme and core version changes in
// the report window (ListReportUpdateItems), and the per-type counts
// (GetUpdateReportStats) stop counting the agent's own self-update tasks. Both
// queries run in production under db.Pool.InAgentTx (the report aggregator's
// wiring in cmd/wpmgr/main.go), where update_tasks_agent admits every tenant's
// rows, so the explicit tenant_id predicate is the tenant boundary. This file
// proves that predicate holds instead of assuming RLS supplies it.
//
// Every read goes through pool.InAgentTx and the generated sqlc method, on the
// pool startPostgres returns, which connects as wpmgr_app (NOSUPERUSER,
// NOBYPASSRLS), the role every install runs as. Rows are seeded through
// InTenantTx, so update_tasks_tenant_isolation's WITH CHECK applies to them.
//
// Assertions use t.Errorf per excluded category, so one run against a query
// with its filters removed names every filter the test depends on.
package tests

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

type gh969Task struct {
	site       uuid.UUID
	targetType string
	slug       string
	from, to   string
	status     string
	finishedAt time.Time
}

func gh969SeedRun(t *testing.T, pool *db.Pool, tenant uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO update_runs (tenant_id, status, dry_run)
			VALUES ($1, 'completed', false)
			RETURNING id`, tenant).Scan(&id)
	}); err != nil {
		t.Fatalf("seed update run: %v", err)
	}
	return id
}

func gh969SeedTask(t *testing.T, pool *db.Pool, tenant, run uuid.UUID, task gh969Task) {
	t.Helper()
	ctx := context.Background()
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO update_tasks
				(run_id, tenant_id, site_id, target_type, target_slug,
				 from_version, to_version, status, started_at, finished_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)`,
			run, tenant, task.site, task.targetType, task.slug,
			task.from, task.to, task.status, task.finishedAt)
		return err
	}); err != nil {
		t.Fatalf("seed update task %s/%s: %v", task.targetType, task.slug, err)
	}
}

func gh969Listed(t *testing.T, pool *db.Pool, arg sqlc.ListReportUpdateItemsParams) []sqlc.ListReportUpdateItemsRow {
	t.Helper()
	ctx := context.Background()
	var rows []sqlc.ListReportUpdateItemsRow
	if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		var qerr error
		rows, qerr = sqlc.New(tx).ListReportUpdateItems(ctx, arg)
		return qerr
	}); err != nil {
		t.Fatalf("ListReportUpdateItems under InAgentTx: %v", err)
	}
	return rows
}

func gh969Describe(rows []sqlc.ListReportUpdateItemsRow) string {
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, fmt.Sprintf("%s/%s %q->%q %s @%s",
			r.TargetType, r.TargetSlug, r.FromVersion, r.ToVersion, r.Status,
			r.FinishedAt.Time.UTC().Format(time.RFC3339)))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// gh969AgentCount counts rows under InAgentTx with no tenant predicate. It is
// the positive control for each exclusion: the row is reachable on the
// production path, so its absence from a query result is that query's filter.
func gh969AgentCount(t *testing.T, pool *db.Pool, where string, args ...any) int {
	t.Helper()
	ctx := context.Background()
	var n int
	if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM update_tasks WHERE "+where, args...).Scan(&n)
	}); err != nil {
		t.Fatalf("positive-control count (%s): %v", where, err)
	}
	return n
}

// TestGH969ReportUpdateQueriesAsAppRole seeds one fixture and proves both
// report queries against it.
func TestGH969ReportUpdateQueriesAsAppRole(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	// The premise: the production path, as the production role. If either
	// attribute read true, RLS would be bypassed and nothing below would mean
	// anything.
	var role string
	var super, bypass bool
	if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			"SELECT current_user, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user").
			Scan(&role, &super, &bypass)
	}); err != nil {
		t.Fatalf("read the connected role: %v", err)
	}
	if role != "wpmgr_app" || super || bypass {
		t.Fatalf("connected as %q rolsuper=%t rolbypassrls=%t; want wpmgr_app, false, false", role, super, bypass)
	}

	tenantA := seedTenant(t, pool, "gh969-tenant-a")
	tenantB := seedTenant(t, pool, "gh969-tenant-b")
	siteA1 := seedSite(t, pool, tenantA, "")
	siteA2 := seedSite(t, pool, tenantA, "")
	siteA3 := seedSite(t, pool, tenantA, "") // tenant A's, but never asked for
	siteB := seedSite(t, pool, tenantB, "")
	runA := gh969SeedRun(t, pool, tenantA)
	runB := gh969SeedRun(t, pool, tenantB)

	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)

	// In the report: plugin, theme and core tasks that finished
	// succeeded, failed or rolled_back inside [from, to) on a listed site.
	want := []gh969Task{
		{siteA1, "plugin", "at-window-start", "1.0", "1.1", "succeeded", from},
		{siteA1, "plugin", "akismet", "5.0", "5.1", "succeeded", from.Add(1 * time.Hour)},
		{siteA1, "theme", "twentytwentyfour", "1.0", "1.1", "failed", from.Add(2 * time.Hour)},
		// A rollback records from = to.
		{siteA1, "core", "core", "6.6", "6.6", "rolled_back", from.Add(3 * time.Hour)},
		// An unknown starting version is the empty string.
		{siteA2, "plugin", "woocommerce", "", "8.0", "succeeded", from.Add(30 * time.Minute)},
	}
	for _, task := range want {
		gh969SeedTask(t, pool, tenantA, runA, task)
	}

	// Not in the report, one row per exclusion.
	agentSelfUpdate := gh969Task{siteA1, "agent", "fleet-agent-site-manager", "0.61.1", "0.61.2", "failed", from.Add(4 * time.Hour)}
	skipped := gh969Task{siteA1, "plugin", "skipped-plugin", "1.0", "1.0", "skipped", from.Add(5 * time.Hour)}
	cancelled := gh969Task{siteA1, "plugin", "cancelled-plugin", "1.0", "1.0", "cancelled", from.Add(5*time.Hour + time.Minute)}
	beforeWindow := gh969Task{siteA1, "plugin", "before-window", "1.0", "1.1", "succeeded", from.Add(-time.Second)}
	atWindowEnd := gh969Task{siteA1, "plugin", "at-window-end", "1.0", "1.1", "succeeded", to}
	unlistedSite := gh969Task{siteA3, "plugin", "unlisted-site", "1.0", "1.1", "succeeded", from.Add(time.Hour)}
	for _, task := range []gh969Task{agentSelfUpdate, skipped, cancelled, beforeWindow, atWindowEnd, unlistedSite} {
		gh969SeedTask(t, pool, tenantA, runA, task)
	}
	foreign := gh969Task{siteB, "plugin", "foreign-tenant", "1.0", "1.1", "succeeded", from.Add(time.Hour)}
	gh969SeedTask(t, pool, tenantB, runB, foreign)

	// Positive controls: InAgentTx reaches the rows the queries must exclude.
	if n := gh969AgentCount(t, pool, "site_id = $1", siteB); n != 1 {
		t.Fatalf("positive control: InAgentTx sees %d rows on tenant B's site, want 1; the cross-tenant case below would prove nothing", n)
	}
	if n := gh969AgentCount(t, pool, "site_id = $1 AND target_type = 'agent' AND status = 'failed' AND finished_at >= $2 AND finished_at < $3",
		siteA1, from, to); n != 1 {
		t.Fatalf("positive control: InAgentTx sees %d failed agent self-update rows in the window, want 1", n)
	}

	window := func(tenant uuid.UUID, sites []uuid.UUID, limit int32) sqlc.ListReportUpdateItemsParams {
		return sqlc.ListReportUpdateItemsParams{
			TenantID: tenant,
			SiteIds:  sites,
			FromTime: pgtype.Timestamptz{Time: from, Valid: true},
			ToTime:   pgtype.Timestamptz{Time: to, Valid: true},
			RowLimit: limit,
		}
	}

	// Postgres orders uuid by its 16 bytes, so bytes.Compare gives the same
	// site order the query's ORDER BY site_id, finished_at, id does.
	sort.SliceStable(want, func(i, j int) bool {
		if c := bytes.Compare(want[i].site[:], want[j].site[:]); c != 0 {
			return c < 0
		}
		return want[i].finishedAt.Before(want[j].finishedAt)
	})

	t.Run("ListReportUpdateItems returns the window's rows for the tenant's listed sites, in order", func(t *testing.T) {
		// siteB is tenant B's, passed in tenant A's list as a guessed id.
		got := gh969Listed(t, pool, window(tenantA, []uuid.UUID{siteA1, siteA2, siteB}, 100))

		for _, r := range got {
			switch {
			case r.SiteID == siteB:
				t.Errorf("returned tenant B's row %s/%s for tenant A: the tenant_id predicate is the only tenant boundary under InAgentTx", r.TargetType, r.TargetSlug)
			case r.TargetType == "agent":
				t.Errorf("returned the agent self-update row %s", r.TargetSlug)
			case r.Status != "succeeded" && r.Status != "failed" && r.Status != "rolled_back":
				t.Errorf("returned %s/%s with status %q, outside succeeded, failed, rolled_back", r.TargetType, r.TargetSlug, r.Status)
			case r.SiteID == siteA3:
				t.Errorf("returned %s/%s from a site not in site_ids", r.TargetType, r.TargetSlug)
			case !r.FinishedAt.Valid || r.FinishedAt.Time.Before(from) || !r.FinishedAt.Time.Before(to):
				t.Errorf("returned %s/%s finished at %v, outside [%s, %s)", r.TargetType, r.TargetSlug, r.FinishedAt, from, to)
			}
		}

		if len(got) != len(want) {
			t.Fatalf("got %d rows, want %d\n got: %s", len(got), len(want), gh969Describe(got))
		}
		for i, w := range want {
			g := got[i]
			if g.SiteID != w.site || g.TargetType != w.targetType || g.TargetSlug != w.slug ||
				g.FromVersion != w.from || g.ToVersion != w.to || g.Status != w.status ||
				!g.FinishedAt.Valid || !g.FinishedAt.Time.Equal(w.finishedAt) {
				t.Errorf("row %d = %s %q->%q %s @%v on %s, want %s/%s %q->%q %s @%s on %s",
					i, g.TargetType+"/"+g.TargetSlug, g.FromVersion, g.ToVersion, g.Status, g.FinishedAt.Time, g.SiteID,
					w.targetType, w.slug, w.from, w.to, w.status, w.finishedAt, w.site)
			}
		}
	})

	t.Run("ListReportUpdateItems honours the row limit", func(t *testing.T) {
		if len(want) <= 2 {
			t.Fatalf("fixture has %d reportable rows; the limit case needs more than 2", len(want))
		}
		got := gh969Listed(t, pool, window(tenantA, []uuid.UUID{siteA1, siteA2}, 2))
		if len(got) != 2 {
			t.Fatalf("row_limit 2 returned %d rows: %s", len(got), gh969Describe(got))
		}
		for i := range got {
			if got[i].SiteID != want[i].site || got[i].TargetSlug != want[i].slug {
				t.Errorf("limited row %d = %s on %s, want %s on %s (the first rows of the full ordering)",
					i, got[i].TargetSlug, got[i].SiteID, want[i].slug, want[i].site)
			}
		}
	})

	t.Run("ListReportUpdateItems returns nothing for another tenant's guessed site ids", func(t *testing.T) {
		got := gh969Listed(t, pool, window(tenantB, []uuid.UUID{siteA1, siteA2, siteA3}, 100))
		if len(got) != 0 {
			t.Errorf("tenant B asking for tenant A's site ids got %d rows: %s", len(got), gh969Describe(got))
		}
		// Positive control: the same call reaches tenant B's own row.
		own := gh969Listed(t, pool, window(tenantB, []uuid.UUID{siteB, siteA1}, 100))
		if len(own) != 1 || own[0].TargetSlug != foreign.slug {
			t.Errorf("tenant B's own site returned %s, want exactly %s", gh969Describe(own), foreign.slug)
		}
	})

	t.Run("GetUpdateReportStats does not count the agent self-update", func(t *testing.T) {
		var rows []sqlc.GetUpdateReportStatsRow
		if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
			var qerr error
			rows, qerr = sqlc.New(tx).GetUpdateReportStats(ctx, sqlc.GetUpdateReportStatsParams{
				TenantID: tenantA,
				SiteID:   siteA1,
				FromTime: pgtype.Timestamptz{Time: from, Valid: true},
				ToTime:   pgtype.Timestamptz{Time: to, Valid: true},
			})
			return qerr
		}); err != nil {
			t.Fatalf("GetUpdateReportStats under InAgentTx: %v", err)
		}

		type counts struct{ succeeded, failed int64 }
		got := map[string]counts{}
		var totalFailed int64
		for _, r := range rows {
			got[r.TargetType] = counts{r.Succeeded, r.Failed}
			totalFailed += r.Failed
		}
		if c, ok := got["agent"]; ok {
			t.Errorf("counted agent self-updates as a report row: succeeded=%d failed=%d", c.succeeded, c.failed)
		}
		if totalFailed != 2 {
			t.Errorf("failed across all types = %d, want 2 (the theme failure and the core rollback)", totalFailed)
		}
		wantCounts := map[string]counts{
			"plugin": {2, 0}, // at-window-start and akismet; skipped and cancelled count in neither column
			"theme":  {0, 1},
			"core":   {0, 1},
		}
		for typ, w := range wantCounts {
			if g, ok := got[typ]; !ok || g != w {
				t.Errorf("%s = %+v (present=%t), want %+v", typ, g, ok, w)
			}
		}
		if len(got) != len(wantCounts) {
			t.Errorf("got %d target types %v, want exactly plugin, theme, core", len(got), got)
		}
	})
}
