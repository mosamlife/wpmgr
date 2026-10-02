// Proofs for the GH #824 and #828 statements over assistant_ability_requests:
// ReleaseAbilityRequestUndo, ListStuckAbilityRequestUndos and
// ListOrgAbilityRequests.
//
// As assistant_ability_requests_m156_rls_test.go: every read and write goes
// through the production dispatch (RunTenantTx / InTenantTx / InAgentTx) on
// the wpmgr_app pool, every transaction asserts it is wpmgr_app with neither
// SUPERUSER nor BYPASSRLS, and each statement under test is the generated
// sqlc method. A row reaches 'done' through the shipped approve and outcome
// statements; the one step the shipped path takes elsewhere (the catalogue
// reservation) and the clock moves are plain column updates, still as
// wpmgr_app under its column grants.
package tests

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// arqDoneInUndo inserts a request on site and walks it to state 'done' with
// undo 'in_progress', started now, the undo window an hour out.
func arqDoneInUndo(t *testing.T, pool *db.Pool, tenant, site uuid.UUID, seed string) sqlc.AssistantAbilityRequest {
	t.Helper()
	ctx := context.Background()
	row := aarInsert(t, pool, acprSitePrincipal(tenant, site), aarParams(tenant, site, uuid.New(), seed))
	if _, err := aarApprove(t, pool, row, row.PresentedDigest); err != nil {
		t.Fatalf("approve %s: %v", row.ID, err)
	}
	if err := pool.RunTenantTx(ctx, acprOrgPrincipal(tenant), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (arq walk to done)")
		// The reservation's catalogue join is not under test here.
		if _, err := tx.Exec(ctx, `UPDATE assistant_ability_requests
			SET state = 'dispatched', claimed_at = now()
			WHERE tenant_id = $1 AND id = $2 AND state = 'approved'`, tenant, row.ID); err != nil {
			return err
		}
		q := sqlc.New(tx)
		post := int64(42)
		n, err := q.RecordAbilityRequestOutcome(ctx, sqlc.RecordAbilityRequestOutcomeParams{
			Outcome:            "created",
			CreatedPostID:      &post,
			UndoAvailableUntil: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
			TenantID:           tenant,
			ID:                 row.ID,
		})
		if err != nil || n != 1 {
			t.Fatalf("record outcome %s: n=%d err=%v", row.ID, n, err)
		}
		n, err = q.BeginAbilityRequestUndo(ctx, sqlc.BeginAbilityRequestUndoParams{
			UndoByUserID: uuid.New(), TenantID: tenant, ID: row.ID, SiteID: site,
		})
		if err != nil || n != 1 {
			t.Fatalf("begin undo %s: n=%d err=%v", row.ID, n, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s to done/in_progress: %v", row.ID, err)
	}
	return row
}

func arqUndo(t *testing.T, pool *db.Pool, tenant, id uuid.UUID) (state *string, by *uuid.UUID, started, finished pgtype.Timestamptz) {
	t.Helper()
	if err := pool.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT undo_state, undo_by_user_id, undo_started_at, undo_finished_at
			FROM assistant_ability_requests WHERE id = $1`, id).Scan(&state, &by, &started, &finished)
	}); err != nil {
		t.Fatalf("read undo columns of %s: %v", id, err)
	}
	return
}

func arqRelease(t *testing.T, pool *db.Pool, tenant, id uuid.UUID) int64 {
	t.Helper()
	var n int64
	if err := pool.RunTenantTx(context.Background(), acprOrgPrincipal(tenant), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (release undo)")
		var err error
		n, err = sqlc.New(tx).ReleaseAbilityRequestUndo(context.Background(),
			sqlc.ReleaseAbilityRequestUndoParams{TenantID: tenant, ID: id})
		return err
	}); err != nil {
		t.Fatalf("release undo %s: %v", id, err)
	}
	return n
}

func arqExec(t *testing.T, pool *db.Pool, tenant uuid.UUID, sql string, args ...any) {
	t.Helper()
	if err := pool.RunTenantTx(context.Background(), acprOrgPrincipal(tenant), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (arq arrange)")
		_, err := tx.Exec(context.Background(), sql, args...)
		return err
	}); err != nil {
		t.Fatalf("arrange %q: %v", sql, err)
	}
}

// TestAbilityRequestUndoReleaseOnlyFromInProgressAsAppRole proves release
// moves in_progress back to available with the starter and start time
// cleared, and matches nothing from available, from a finished undo, or past
// the window.
//
// Mutation, planted and watched: dropping "AND undo_state = 'in_progress'"
// from ReleaseAbilityRequestUndo fires "RELEASED FROM A FINISHED UNDO".
func TestAbilityRequestUndoReleaseOnlyFromInProgressAsAppRole(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "arq-rel-"+uuid.NewString()[:8])
	site := seedSite(t, pool, tenant, "")

	r := arqDoneInUndo(t, pool, tenant, site, "rel-1")
	if n := arqRelease(t, pool, tenant, r.ID); n != 1 {
		t.Fatalf("release from in_progress matched %d rows, want 1", n)
	}
	st, by, started, finished := arqUndo(t, pool, tenant, r.ID)
	if st == nil || *st != "available" || by != nil || started.Valid || finished.Valid {
		t.Fatalf("after release: undo_state=%v by=%v started=%v finished=%v; want available with all three NULL",
			st, by, started.Valid, finished.Valid)
	}
	if n := arqRelease(t, pool, tenant, r.ID); n != 0 {
		t.Fatalf("RELEASED FROM AVAILABLE: a second release matched %d rows", n)
	}

	// A finished undo stays finished.
	f := arqDoneInUndo(t, pool, tenant, site, "rel-2")
	if err := pool.RunTenantTx(context.Background(), acprOrgPrincipal(tenant), func(tx pgx.Tx) error {
		n, err := sqlc.New(tx).FinishAbilityRequestUndo(context.Background(), sqlc.FinishAbilityRequestUndoParams{
			UndoResult: "undone", TenantID: tenant, ID: f.ID,
		})
		if err == nil && n != 1 {
			t.Fatalf("finish undo matched %d rows", n)
		}
		return err
	}); err != nil {
		t.Fatalf("finish undo: %v", err)
	}
	if n := arqRelease(t, pool, tenant, f.ID); n != 0 {
		t.Fatalf("RELEASED FROM A FINISHED UNDO: release matched %d rows on an undone request", n)
	}
	if st, _, _, _ := arqUndo(t, pool, tenant, f.ID); st == nil || *st != "undone" {
		t.Fatalf("finished undo moved to %v", st)
	}

	// Past the window, release matches nothing; the caller finishes as failed.
	w := arqDoneInUndo(t, pool, tenant, site, "rel-3")
	arqExec(t, pool, tenant, `UPDATE assistant_ability_requests
		SET undo_available_until = now() - interval '1 second' WHERE id = $1`, w.ID)
	if n := arqRelease(t, pool, tenant, w.ID); n != 0 {
		t.Fatalf("RELEASED PAST THE WINDOW: release matched %d rows", n)
	}

	// Another tenant cannot release this tenant's row.
	other := seedTenant(t, pool, "arq-rel-x-"+uuid.NewString()[:8])
	x := arqDoneInUndo(t, pool, tenant, site, "rel-4")
	if n := arqRelease(t, pool, other, x.ID); n != 0 {
		t.Fatalf("TENANCY LEAK: a foreign tenant released %d rows", n)
	}
}

// TestAbilityRequestStuckUndoScanAsAppRole proves the agent scan returns an
// in_progress undo older than the threshold across tenants and omits a fresh
// one and a released one.
func TestAbilityRequestStuckUndoScanAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tA := seedTenant(t, pool, "arq-stk-a-"+uuid.NewString()[:8])
	tB := seedTenant(t, pool, "arq-stk-b-"+uuid.NewString()[:8])
	sA := seedSite(t, pool, tA, "")
	sB := seedSite(t, pool, tB, "")

	oldA := arqDoneInUndo(t, pool, tA, sA, "stk-old-a")
	oldB := arqDoneInUndo(t, pool, tB, sB, "stk-old-b")
	fresh := arqDoneInUndo(t, pool, tA, sA, "stk-fresh")
	released := arqDoneInUndo(t, pool, tA, sA, "stk-rel")
	arqExec(t, pool, tA, `UPDATE assistant_ability_requests
		SET undo_started_at = now() - interval '10 minutes' WHERE id = ANY($1)`, []uuid.UUID{oldA.ID, released.ID})
	arqExec(t, pool, tB, `UPDATE assistant_ability_requests
		SET undo_started_at = now() - interval '10 minutes' WHERE id = $1`, oldB.ID)
	if n := arqRelease(t, pool, tA, released.ID); n != 1 {
		t.Fatalf("release %s matched %d rows", released.ID, n)
	}

	got := map[uuid.UUID]sqlc.ListStuckAbilityRequestUndosRow{}
	if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InAgentTx (stuck undo scan)")
		rows, err := sqlc.New(tx).ListStuckAbilityRequestUndos(ctx, sqlc.ListStuckAbilityRequestUndosParams{
			StaleAfterSeconds: 300, RowLimit: 1000,
		})
		for _, r := range rows {
			got[r.ID] = r
		}
		return err
	}); err != nil {
		t.Fatalf("stuck undo scan: %v", err)
	}
	for _, want := range []sqlc.AssistantAbilityRequest{oldA, oldB} {
		r, ok := got[want.ID]
		if !ok {
			t.Fatalf("stuck undo %s (tenant %s) missing from the agent scan", want.ID, want.TenantID)
		}
		if r.TenantID != want.TenantID || r.SiteID != want.SiteID {
			t.Fatalf("stuck undo %s returned tenant=%s site=%s, want %s/%s", want.ID, r.TenantID, r.SiteID, want.TenantID, want.SiteID)
		}
	}
	if _, ok := got[fresh.ID]; ok {
		t.Fatalf("AGE THRESHOLD IGNORED: an undo started just now is listed as stuck")
	}
	if _, ok := got[released.ID]; ok {
		t.Fatalf("a released (available) undo is listed as stuck")
	}

	// Outside the agent context the scan sees only its own tenant.
	if err := pool.InTenantTx(ctx, tB, func(tx pgx.Tx) error {
		rows, err := sqlc.New(tx).ListStuckAbilityRequestUndos(ctx, sqlc.ListStuckAbilityRequestUndosParams{
			StaleAfterSeconds: 300, RowLimit: 1000,
		})
		for _, r := range rows {
			if r.TenantID != tB {
				t.Fatalf("TENANCY LEAK: tenant B's transaction scanned tenant %s's undo %s", r.TenantID, r.ID)
			}
		}
		return err
	}); err != nil {
		t.Fatalf("tenant-scoped scan: %v", err)
	}
}

// TestAbilityRequestOrgListAsAppRole proves the org list returns this
// tenant's rows only, newest first, narrowed by state, and that a site-scoped
// principal sees only its own sites through it.
func TestAbilityRequestOrgListAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tA := seedTenant(t, pool, "arq-org-a-"+uuid.NewString()[:8])
	tB := seedTenant(t, pool, "arq-org-b-"+uuid.NewString()[:8])
	s1 := seedSite(t, pool, tA, "")
	s2 := seedSite(t, pool, tA, "")
	sB := seedSite(t, pool, tB, "")

	p1 := aarInsert(t, pool, acprSitePrincipal(tA, s1), aarParams(tA, s1, uuid.New(), "org-p1"))
	p2 := aarInsert(t, pool, acprSitePrincipal(tA, s2), aarParams(tA, s2, uuid.New(), "org-p2"))
	d1 := arqDoneInUndo(t, pool, tA, s1, "org-d1")
	foreign := aarInsert(t, pool, acprSitePrincipal(tB, sB), aarParams(tB, sB, uuid.New(), "org-b"))

	list := func(where string, run func(fn func(tx pgx.Tx) error) error, tenant uuid.UUID, state *string) []sqlc.AssistantAbilityRequest {
		t.Helper()
		var out []sqlc.AssistantAbilityRequest
		if err := run(func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, where)
			var err error
			out, err = sqlc.New(tx).ListOrgAbilityRequests(ctx, sqlc.ListOrgAbilityRequestsParams{
				TenantID: tenant, StateFilter: state, RowLimit: 100, RowOffset: 0,
			})
			return err
		}); err != nil {
			t.Fatalf("%s: %v", where, err)
		}
		return out
	}
	ids := func(rows []sqlc.AssistantAbilityRequest) map[uuid.UUID]bool {
		m := map[uuid.UUID]bool{}
		for _, r := range rows {
			m[r.ID] = true
		}
		return m
	}
	orgA := func(fn func(tx pgx.Tx) error) error { return pool.RunTenantTx(ctx, acprOrgPrincipal(tA), fn) }

	all := list("RunTenantTx (org list, all)", orgA, tA, nil)
	got := ids(all)
	if got[foreign.ID] {
		t.Fatalf("TENANCY LEAK: tenant A's org list returned tenant B's request %s", foreign.ID)
	}
	if len(all) != 3 || !got[p1.ID] || !got[p2.ID] || !got[d1.ID] {
		t.Fatalf("org list returned %d rows %v, want exactly p1, p2, d1", len(all), got)
	}
	for i := 1; i < len(all); i++ {
		if all[i].CreatedAt.After(all[i-1].CreatedAt) {
			t.Fatalf("org list not newest first at index %d", i)
		}
	}

	pending := "pending"
	pend := ids(list("RunTenantTx (org list, pending)", orgA, tA, &pending))
	if len(pend) != 2 || !pend[p1.ID] || !pend[p2.ID] {
		t.Fatalf("state filter 'pending' returned %v, want p1 and p2", pend)
	}
	done := "done"
	dn := ids(list("RunTenantTx (org list, done)", orgA, tA, &done))
	if len(dn) != 1 || !dn[d1.ID] {
		t.Fatalf("state filter 'done' returned %v, want d1", dn)
	}

	// Cross-tenant: tenant B's transaction naming tenant A returns nothing.
	orgB := func(fn func(tx pgx.Tx) error) error { return pool.RunTenantTx(ctx, acprOrgPrincipal(tB), fn) }
	if leak := list("RunTenantTx (tenant B asks for A)", orgB, tA, nil); len(leak) != 0 {
		t.Fatalf("TENANCY LEAK: tenant B's transaction listed %d of tenant A's requests", len(leak))
	}

	// Site scope: a collaborator on s1 sees s1's rows and never s2's.
	s1Only := func(fn func(tx pgx.Tx) error) error { return pool.RunTenantTx(ctx, acprSitePrincipal(tA, s1), fn) }
	sc := ids(list("RunTenantTx (site-scoped org list)", s1Only, tA, nil))
	if sc[p2.ID] {
		t.Fatalf("SITE-SCOPE LEAK: a principal scoped to site %s listed request %s on site %s", s1, p2.ID, s2)
	}
	if len(sc) != 2 || !sc[p1.ID] || !sc[d1.ID] {
		t.Fatalf("OVER-FIRING: site-scoped org list returned %v, want p1 and d1", sc)
	}

	// The badge count the page pairs with this list.
	if err := orgA(func(tx pgx.Tx) error {
		n, err := sqlc.New(tx).CountLivePendingAbilityRequests(ctx, tA)
		if err == nil && n != 2 {
			t.Fatalf("pending badge count = %d, want 2", n)
		}
		return err
	}); err != nil {
		t.Fatalf("pending count: %v", err)
	}
}
