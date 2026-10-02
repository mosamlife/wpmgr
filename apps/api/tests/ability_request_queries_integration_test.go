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

// ---------------------------------------------------------------------------
// GH #826: a recovery undo of the draft a failed or given-up write left.
// ---------------------------------------------------------------------------

// arqSent inserts a request on site, approves it through the shipped
// statement, and moves it to 'dispatched' (the reservation's catalogue join
// is not under test here).
func arqSent(t *testing.T, pool *db.Pool, tenant, site uuid.UUID, seed string) sqlc.AssistantAbilityRequest {
	t.Helper()
	row := aarInsert(t, pool, acprSitePrincipal(tenant, site), aarParams(tenant, site, uuid.New(), seed))
	if _, err := aarApprove(t, pool, row, row.PresentedDigest); err != nil {
		t.Fatalf("approve %s: %v", row.ID, err)
	}
	arqExec(t, pool, tenant, `UPDATE assistant_ability_requests
		SET state = 'dispatched', claimed_at = now()
		WHERE tenant_id = $1 AND id = $2 AND state = 'approved'`, tenant, row.ID)
	return row
}

// arqOutcome records outcome on a row through RecordAbilityRequestOutcome
// and returns the rows it matched.
func arqOutcome(t *testing.T, pool *db.Pool, tenant, id uuid.UUID, outcome string, post *int64, undoUntil pgtype.Timestamptz) int64 {
	t.Helper()
	var n int64
	if err := pool.RunTenantTx(context.Background(), acprOrgPrincipal(tenant), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (record outcome)")
		var err error
		n, err = sqlc.New(tx).RecordAbilityRequestOutcome(context.Background(), sqlc.RecordAbilityRequestOutcomeParams{
			Outcome: outcome, CreatedPostID: post, UndoAvailableUntil: undoUntil, TenantID: tenant, ID: id,
		})
		return err
	}); err != nil {
		t.Fatalf("record outcome %q on %s: %v", outcome, id, err)
	}
	return n
}

// arqFailed walks a request to state 'failed', outcome 'verify_mismatch',
// naming post when it is not nil.
func arqFailed(t *testing.T, pool *db.Pool, tenant, site uuid.UUID, seed string, post *int64) sqlc.AssistantAbilityRequest {
	t.Helper()
	row := arqSent(t, pool, tenant, site, seed)
	if n := arqOutcome(t, pool, tenant, row.ID, "verify_mismatch", post, pgtype.Timestamptz{}); n != 1 {
		t.Fatalf("record failed outcome on %s matched %d rows", row.ID, n)
	}
	return row
}

func arqRecoveryBegin(t *testing.T, pool *db.Pool, tenant, site, id uuid.UUID, window int32) int64 {
	t.Helper()
	var n int64
	if err := pool.RunTenantTx(context.Background(), acprOrgPrincipal(tenant), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (recovery undo begin)")
		var err error
		n, err = sqlc.New(tx).BeginAbilityRequestRecoveryUndo(context.Background(), sqlc.BeginAbilityRequestRecoveryUndoParams{
			UndoByUserID: uuid.New(), WindowSeconds: window, TenantID: tenant, ID: id, SiteID: site,
		})
		return err
	}); err != nil {
		t.Fatalf("recovery undo begin on %s: %v", id, err)
	}
	return n
}

func arqUndoWindow(t *testing.T, pool *db.Pool, tenant, id uuid.UUID) pgtype.Timestamptz {
	t.Helper()
	var w pgtype.Timestamptz
	if err := pool.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT undo_available_until
			FROM assistant_ability_requests WHERE id = $1`, id).Scan(&w)
	}); err != nil {
		t.Fatalf("read undo window of %s: %v", id, err)
	}
	return w
}

// TestAbilityRequestRecoveryUndoBeginAsAppRole proves the recovery begin
// starts an undo on a failed row that names its post and on a given-up
// outcome_unknown row that does, and refuses a row with no post, a pending
// row, a done row, a resolving outcome_unknown row, a second begin, a zero
// window, and another tenant.
//
// Mutation: dropping "AND outcome IS NOT NULL" from
// BeginAbilityRequestRecoveryUndo fires "BEGAN ON A RESOLVING ROW".
func TestAbilityRequestRecoveryUndoBeginAsAppRole(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "arq-rec-"+uuid.NewString()[:8])
	site := seedSite(t, pool, tenant, "")
	post := int64(42)

	f := arqFailed(t, pool, tenant, site, "rec-1", &post)
	if n := arqRecoveryBegin(t, pool, tenant, site, f.ID, 600); n != 1 {
		t.Fatalf("recovery begin on a failed row with a post matched %d rows, want 1", n)
	}
	st, by, started, finished := arqUndo(t, pool, tenant, f.ID)
	if st == nil || *st != "in_progress" || by == nil || !started.Valid || finished.Valid {
		t.Fatalf("after recovery begin: undo_state=%v by=%v started=%v finished=%v; want in_progress, starter and start set",
			st, by, started.Valid, finished.Valid)
	}
	if w := arqUndoWindow(t, pool, tenant, f.ID); !w.Valid || !w.Time.After(time.Now().Add(5*time.Minute)) {
		t.Fatalf("recovery begin window = %v, want about ten minutes out", w)
	}
	if n := arqRecoveryBegin(t, pool, tenant, site, f.ID, 600); n != 0 {
		t.Fatalf("BEGAN TWICE: a second recovery begin matched %d rows", n)
	}

	noPost := arqFailed(t, pool, tenant, site, "rec-2", nil)
	if n := arqRecoveryBegin(t, pool, tenant, site, noPost.ID, 600); n != 0 {
		t.Fatalf("BEGAN WITHOUT A POST: recovery begin matched %d rows on a failed row with no post", n)
	}

	pending := aarInsert(t, pool, acprSitePrincipal(tenant, site), aarParams(tenant, site, uuid.New(), "rec-3"))
	if n := arqRecoveryBegin(t, pool, tenant, site, pending.ID, 600); n != 0 {
		t.Fatalf("BEGAN ON PENDING: recovery begin matched %d rows", n)
	}

	done := arqSent(t, pool, tenant, site, "rec-4")
	if n := arqOutcome(t, pool, tenant, done.ID, "created", &post, pgtype.Timestamptz{}); n != 1 {
		t.Fatalf("record created on %s matched %d rows", done.ID, n)
	}
	if n := arqRecoveryBegin(t, pool, tenant, site, done.ID, 600); n != 0 {
		t.Fatalf("BEGAN ON DONE: recovery begin matched %d rows on a done row", n)
	}

	// Resolving: outcome_unknown, outcome NULL, the post already known.
	resolving := arqSent(t, pool, tenant, site, "rec-5")
	arqExec(t, pool, tenant, `UPDATE assistant_ability_requests
		SET state = 'outcome_unknown', unknown_since = now(), created_post_id = 42
		WHERE id = $1`, resolving.ID)
	if n := arqRecoveryBegin(t, pool, tenant, site, resolving.ID, 600); n != 0 {
		t.Fatalf("BEGAN ON A RESOLVING ROW: recovery begin matched %d rows with outcome NULL", n)
	}

	// Given up: outcome_unknown with a final outcome and the post.
	gaveUp := arqSent(t, pool, tenant, site, "rec-6")
	if n := arqOutcome(t, pool, tenant, gaveUp.ID, "outcome_unknown", &post, pgtype.Timestamptz{}); n != 1 {
		t.Fatalf("record outcome_unknown on %s matched %d rows", gaveUp.ID, n)
	}
	if n := arqRecoveryBegin(t, pool, tenant, site, gaveUp.ID, 0); n != 0 {
		t.Fatalf("BEGAN WITH NO WINDOW: recovery begin with window 0 matched %d rows", n)
	}
	other := seedTenant(t, pool, "arq-rec-x-"+uuid.NewString()[:8])
	if n := arqRecoveryBegin(t, pool, other, site, gaveUp.ID, 600); n != 0 {
		t.Fatalf("TENANCY LEAK: a foreign tenant began %d recovery undos", n)
	}
	if n := arqRecoveryBegin(t, pool, tenant, site, gaveUp.ID, 600); n != 1 {
		t.Fatalf("recovery begin on a given-up outcome_unknown row with a post matched %d rows, want 1", n)
	}
}

// TestAbilityRequestRecoveryUndoFinishReleaseAsAppRole proves Finish, Release
// and the stuck scan reach a recovery undo, and that a released recovery undo
// returns to no undo at all and can be begun again.
//
// Mutation: restoring "AND state = 'done'" in FinishAbilityRequestUndo fires
// "FINISH MISSED A RECOVERY UNDO".
func TestAbilityRequestRecoveryUndoFinishReleaseAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "arq-rfr-"+uuid.NewString()[:8])
	site := seedSite(t, pool, tenant, "")
	post := int64(42)

	fin := arqFailed(t, pool, tenant, site, "rfr-1", &post)
	if n := arqRecoveryBegin(t, pool, tenant, site, fin.ID, 600); n != 1 {
		t.Fatalf("recovery begin matched %d rows", n)
	}
	if err := pool.RunTenantTx(ctx, acprOrgPrincipal(tenant), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (recovery undo finish)")
		n, err := sqlc.New(tx).FinishAbilityRequestUndo(ctx, sqlc.FinishAbilityRequestUndoParams{
			UndoResult: "undone", TenantID: tenant, ID: fin.ID,
		})
		if err == nil && n != 1 {
			t.Fatalf("FINISH MISSED A RECOVERY UNDO: finish matched %d rows", n)
		}
		return err
	}); err != nil {
		t.Fatalf("finish recovery undo: %v", err)
	}
	if st, _, _, finished := arqUndo(t, pool, tenant, fin.ID); st == nil || *st != "undone" || !finished.Valid {
		t.Fatalf("after finish: undo_state=%v finished=%v, want undone with a finish time", st, finished.Valid)
	}

	rel := arqFailed(t, pool, tenant, site, "rfr-2", &post)
	if n := arqRecoveryBegin(t, pool, tenant, site, rel.ID, 600); n != 1 {
		t.Fatalf("recovery begin matched %d rows", n)
	}
	if n := arqRelease(t, pool, tenant, rel.ID); n != 1 {
		t.Fatalf("RELEASE MISSED A RECOVERY UNDO: release matched %d rows", n)
	}
	st, by, started, finished := arqUndo(t, pool, tenant, rel.ID)
	if st != nil || by != nil || started.Valid || finished.Valid || arqUndoWindow(t, pool, tenant, rel.ID).Valid {
		t.Fatalf("after recovery release: undo_state=%v by=%v started=%v finished=%v; want every undo column NULL",
			st, by, started.Valid, finished.Valid)
	}
	if n := arqRecoveryBegin(t, pool, tenant, site, rel.ID, 600); n != 1 {
		t.Fatalf("a released recovery undo could not be begun again: matched %d rows", n)
	}

	// The stuck scan lists an aged recovery undo.
	arqExec(t, pool, tenant, `UPDATE assistant_ability_requests
		SET undo_started_at = now() - interval '10 minutes' WHERE id = $1`, rel.ID)
	found := false
	if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InAgentTx (stuck recovery undo scan)")
		rows, err := sqlc.New(tx).ListStuckAbilityRequestUndos(ctx, sqlc.ListStuckAbilityRequestUndosParams{
			StaleAfterSeconds: 300, RowLimit: 1000,
		})
		for _, r := range rows {
			if r.ID == rel.ID {
				found = r.TenantID == tenant && r.SiteID == site && r.UndoAvailableUntil.Valid
			}
		}
		return err
	}); err != nil {
		t.Fatalf("stuck undo scan: %v", err)
	}
	if !found {
		t.Fatalf("STUCK SCAN MISSED A RECOVERY UNDO: %s absent or returned with the wrong tenant, site or window", rel.ID)
	}
}

// TestAbilityRequestLateOutcomeDoesNotClobberRecoveryUndo proves
// RecordAbilityRequestOutcome never rewrites a row whose undo has started.
// The second case is a row with outcome NULL and an undo in progress, which
// the CHECKs admit; only the undo_state guard keeps the outcome statement off
// it. The third is the over-fire control.
//
// Mutation: dropping "AND undo_state IS NULL" from RecordAbilityRequestOutcome
// fires "LATE OUTCOME CLOBBERED AN UNDO".
func TestAbilityRequestLateOutcomeDoesNotClobberRecoveryUndo(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "arq-clb-"+uuid.NewString()[:8])
	site := seedSite(t, pool, tenant, "")
	post := int64(42)
	later := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}

	f := arqFailed(t, pool, tenant, site, "clb-1", &post)
	if n := arqRecoveryBegin(t, pool, tenant, site, f.ID, 600); n != 1 {
		t.Fatalf("recovery begin matched %d rows", n)
	}
	if n := arqOutcome(t, pool, tenant, f.ID, "created", &post, later); n != 0 {
		t.Fatalf("LATE OUTCOME CLOBBERED A RECOVERY UNDO: outcome record matched %d rows", n)
	}
	if st, _, _, _ := arqUndo(t, pool, tenant, f.ID); st == nil || *st != "in_progress" {
		t.Fatalf("recovery undo moved to %v after a late outcome", st)
	}

	r := arqSent(t, pool, tenant, site, "clb-2")
	arqExec(t, pool, tenant, `UPDATE assistant_ability_requests
		SET state = 'outcome_unknown', unknown_since = now(), created_post_id = 42,
		    undo_state = 'in_progress', undo_available_until = now() + interval '10 minutes',
		    undo_by_user_id = $2, undo_started_at = now()
		WHERE id = $1`, r.ID, uuid.New())
	if n := arqOutcome(t, pool, tenant, r.ID, "created", &post, later); n != 0 {
		t.Fatalf("LATE OUTCOME CLOBBERED AN UNDO: outcome record matched %d rows on a row whose undo is in progress", n)
	}
	if st, _, _, _ := arqUndo(t, pool, tenant, r.ID); st == nil || *st != "in_progress" {
		t.Fatalf("undo moved to %v after a late outcome", st)
	}

	// Over-fire control: the guard leaves a resolving row with no undo alone.
	ok := arqSent(t, pool, tenant, site, "clb-3")
	arqExec(t, pool, tenant, `UPDATE assistant_ability_requests
		SET state = 'outcome_unknown', unknown_since = now() WHERE id = $1`, ok.ID)
	if n := arqOutcome(t, pool, tenant, ok.ID, "created", &post, later); n != 1 {
		t.Fatalf("OVER-FIRING: the ledger's answer on a resolving row with no undo matched %d rows, want 1", n)
	}
}

// TestAbilityRequestM158UndoCheckAsAppRole proves the m158 CHECK refuses an
// undo on a failed row that names no post, and admits one that does.
//
// Mutation: dropping "AND created_post_id IS NOT NULL" from the m158 CHECK
// fires "the statement SUCCEEDED".
func TestAbilityRequestM158UndoCheckAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "arq-chk-"+uuid.NewString()[:8])
	site := seedSite(t, pool, tenant, "")
	post := int64(42)

	noPost := arqFailed(t, pool, tenant, site, "chk-1", nil)
	withPost := arqFailed(t, pool, tenant, site, "chk-2", &post)
	const set = `UPDATE assistant_ability_requests
		SET undo_state = 'in_progress', undo_available_until = now() + interval '10 minutes',
		    undo_by_user_id = $2, undo_started_at = now()
		WHERE id = $1`
	if err := pool.RunTenantTx(ctx, acprOrgPrincipal(tenant), func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "RunTenantTx (m158 CHECK)")
		acprExpectConstraint(t, tx, "undo on a failed row with no post", "23514",
			"assistant_ability_requests_undo_only_when_done_check", set, noPost.ID, uuid.New())
		tag, err := tx.Exec(ctx, set, withPost.ID, uuid.New())
		if err != nil {
			t.Fatalf("OVER-FIRING: the CHECK refused an undo on a failed row with a post: %v", err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("undo on a failed row with a post updated %d rows, want 1", tag.RowsAffected())
		}
		return nil
	}); err != nil {
		t.Fatalf("m158 CHECK proof: %v", err)
	}
}
