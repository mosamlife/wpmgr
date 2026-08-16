// gh414_monitoring_guards_test.go — GH #414 phase 1, the guards that need a
// real database.
//
// Companion to internal/site/monitoring_guards_test.go, which pins the guards
// that need no DB. Every assertion here reaches Postgres through the SAME tx
// helper the request uses, as the non-superuser wpmgr_app role, so the RLS
// policies are live. A test that opened its own connection would leave them
// inert and pass over a broken boundary — which is exactly what happened to
// m112's proofs.
package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// TestMonitoringGuards_Phase1DB runs every database-backed guard against one
// container: startPostgres is the expensive part and each subtest owns its own
// site rows, so they neither share nor order-depend on state.
func TestMonitoringGuards_Phase1DB(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "gh414-guards-"+uuid.NewString()[:8])
	actor := seedMonitoringUser(t, pool, "gh414-guards-"+uuid.NewString()[:8]+"@example.com")
	engine := newMonitoringEngine(t, pool, monitoringPrincipal(tenant, actor))
	ctx := context.Background()

	// ---------------------------------------------------------------------
	// FIX 1, PROVED BY EXECUTION. The handler's `if !p.CanAccessSite(id)` is
	// bypassed entirely here — the repo is called directly with a site-scoped
	// principal — so the ONLY thing that can refuse the write is the
	// RESTRICTIVE sites_site_scope policy, which only engages because
	// PauseMonitoring now runs through pool.RunTenantTx.
	//
	// DELIBERATELY NOT AN HTTP-ROUTE TEST, and not a gap to close by adding
	// one: going through POST /api/v1/sites/monitoring/pause would hit the
	// handler's CanAccessSite gate FIRST, which would refuse the out-of-scope
	// site before the request ever reached this repo call — proving the Go
	// gate, and saying nothing about whether the RESTRICTIVE policy would
	// have caught the write on its own. Calling repo.PauseMonitoring directly
	// is the only way to isolate the database as the sole remaining guard,
	// which is the second layer this test exists to prove (see the file
	// header, m112). It still goes through pool.RunTenantTx and the real
	// wpmgr_app role — nothing here opens its own connection.
	//
	// Mutation: change monitoringTx back to
	// r.pool.InTenantTxAsUser(ctx, in.TenantID, in.ActorUserID, fn). The
	// policy goes inert, the UPDATE writes the out-of-scope row, and this
	// test fails on both the row count and the persisted paused_at.
	// ---------------------------------------------------------------------
	t.Run("the DATABASE refuses an out-of-scope pause with the Go gate bypassed", func(t *testing.T) {
		granted := seedSite(t, pool, tenant, "https://gh414-granted-"+uuid.NewString()[:8]+".example.com")
		outside := seedSite(t, pool, tenant, "https://gh414-outside-"+uuid.NewString()[:8]+".example.com")

		scoped := domain.Principal{
			Type: domain.PrincipalUser, UserID: actor, TenantID: tenant,
			Role: "member", Scope: domain.ScopeSite,
			AllowedSiteIDs: []uuid.UUID{granted},
		}
		repo := site.NewRepo(pool)

		// The site OUTSIDE the grant: no Go gate in the way at all.
		states, err := repo.PauseMonitoring(ctx, site.PauseMonitoringInput{
			TenantID: tenant, ActorUserID: actor, Principal: scoped,
			SiteIDs: []uuid.UUID{outside}, Reason: "out of scope",
		})
		if err != nil {
			t.Fatalf("out-of-scope pause returned an infra error: %v", err)
		}
		if len(states) != 0 {
			t.Fatalf("the RESTRICTIVE site-scope policy must exclude the out-of-scope row, got %d rows: %+v", len(states), states)
		}
		if pausedAt, _, _, _, found := readPauseRow(t, pool, tenant, outside); !found || pausedAt != nil {
			t.Fatalf("the out-of-scope site must still be unpaused, got paused_at=%v (found=%v)", pausedAt, found)
		}

		// CONTROL: the same principal, the same call, its OWN site. Without
		// this the test would pass if every scoped write failed.
		states, err = repo.PauseMonitoring(ctx, site.PauseMonitoringInput{
			TenantID: tenant, ActorUserID: actor, Principal: scoped,
			SiteIDs: []uuid.UUID{granted}, Reason: "in scope",
		})
		if err != nil {
			t.Fatalf("in-scope pause failed: %v", err)
		}
		if len(states) != 1 || !states[0].Changed {
			t.Fatalf("a site-scoped principal must still pause its OWN site, got %+v", states)
		}
		if pausedAt, _, _, _, _ := readPauseRow(t, pool, tenant, granted); pausedAt == nil {
			t.Fatalf("the in-scope site must be paused in the database")
		}
	})

	// ---------------------------------------------------------------------
	// GUARD — the FOR UPDATE in pauseMonitoringSQL's prior CTE.
	//
	// Two pauses of the SAME row are forced to overlap by holding the row lock
	// on a third connection until both are queued behind it. With FOR UPDATE
	// the second request's prior read waits for the first to commit and sees
	// the row already paused, so exactly one request reports changed and
	// exactly one audit event exists.
	//
	// Mutation: delete `FOR UPDATE` from the prior CTE. Both prior reads then
	// complete before either UPDATE runs, both report was_paused=false, and the
	// route records a second operator action that never happened.
	// ---------------------------------------------------------------------
	t.Run("concurrent pauses of one site report exactly one change", func(t *testing.T) {
		siteID := seedSite(t, pool, tenant, "https://gh414-lock-pause-"+uuid.NewString()[:8]+".example.com")
		changed := concurrentMonitoringRequests(t, pool, engine,
			"/api/v1/sites/monitoring/pause", siteID,
			map[string]any{"site_ids": []string{siteID.String()}, "reason": "race"})

		if changed != 1 {
			t.Fatalf("exactly one concurrent pause may report changed, got %d", changed)
		}
		if n := countAuditFor(t, pool, tenant, audit.ActionSiteMonitoringPaused, siteID.String()); n != 1 {
			t.Fatalf("exactly one audit event may be recorded for one real pause, got %d", n)
		}
	})

	// ---------------------------------------------------------------------
	// GUARD — the FOR UPDATE in resumeMonitoringSQL's prior subquery. Same
	// shape, same mutation, on the resume path.
	// ---------------------------------------------------------------------
	t.Run("concurrent resumes of one site report exactly one change", func(t *testing.T) {
		siteID := seedSite(t, pool, tenant, "https://gh414-lock-resume-"+uuid.NewString()[:8]+".example.com")
		pauseVia(t, engine, map[string]any{"site_ids": []string{siteID.String()}, "reason": "seed"})

		changed := concurrentMonitoringRequests(t, pool, engine,
			"/api/v1/sites/monitoring/resume", siteID,
			map[string]any{"site_ids": []string{siteID.String()}})

		if changed != 1 {
			t.Fatalf("exactly one concurrent resume may report changed, got %d", changed)
		}
		if n := countAuditFor(t, pool, tenant, audit.ActionSiteMonitoringResumed, siteID.String()); n != 1 {
			t.Fatalf("exactly one audit event may be recorded for one real resume, got %d", n)
		}
	})

	// ---------------------------------------------------------------------
	// GUARD — the audit metadata. The event is useless for an investigation if
	// it records that a pause happened without the reason or the scheduled
	// resume instant.
	//
	// Mutation: drop the reason/resume_at keys from recordMonitoringEvent (or
	// replace meta with an empty map). The event still exists and the older
	// per-site count assertions still pass; only this test notices.
	// ---------------------------------------------------------------------
	t.Run("the audit event carries the reason and the resume instant", func(t *testing.T) {
		siteID := seedSite(t, pool, tenant, "https://gh414-meta-"+uuid.NewString()[:8]+".example.com")
		resumeAt := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
		reason := "maintenance window " + uuid.NewString()[:8]
		pauseVia(t, engine, map[string]any{
			"site_ids":  []string{siteID.String()},
			"reason":    reason,
			"resume_at": resumeAt.Format(time.RFC3339),
		})

		meta := readAuditMeta(t, pool, tenant, audit.ActionSiteMonitoringPaused, siteID.String())
		if meta["reason"] != reason {
			t.Fatalf("audit metadata reason = %v, want %q", meta["reason"], reason)
		}
		if got, _ := meta["resume_at"].(string); got != resumeAt.Format(time.RFC3339) {
			t.Fatalf("audit metadata resume_at = %v, want %s", meta["resume_at"], resumeAt.Format(time.RFC3339))
		}
		if bulk, _ := meta["bulk"].(bool); !bulk {
			t.Fatalf("audit metadata must mark the event as bulk, got %v", meta["bulk"])
		}
	})

	// ---------------------------------------------------------------------
	// GUARD — nullableUUID's API-key path, end to end.
	//
	// Mutation: make nullableUUID return &u unconditionally. The all-zero uuid
	// is not a users(id) row, the FK rejects it, and every API-key pause 500s.
	// ---------------------------------------------------------------------
	t.Run("an API-key principal pauses with paused_by NULL", func(t *testing.T) {
		siteID := seedSite(t, pool, tenant, "https://gh414-apikey-"+uuid.NewString()[:8]+".example.com")
		keyEngine := newMonitoringEngine(t, pool, domain.Principal{
			Type: domain.PrincipalAPIKey, APIKeyID: uuid.New(), UserID: uuid.Nil,
			TenantID: tenant, Role: "owner", Scope: domain.ScopeOrg,
		})
		rec := postJSON(t, keyEngine, "/api/v1/sites/monitoring/pause",
			map[string]any{"site_ids": []string{siteID.String()}, "reason": "api key pause"})
		if rec.Code != http.StatusOK {
			t.Fatalf("an API-key pause must succeed, got %d: %s", rec.Code, rec.Body.String())
		}
		got := resultFor(t, decodeMonitoring(t, rec.Body.Bytes(), rec.Code), siteID.String())
		if !got.OK || !got.Changed {
			t.Fatalf("the API-key pause must have taken effect, got %+v", got)
		}
		pausedAt, pausedBy, _, _, _ := readPauseRow(t, pool, tenant, siteID)
		if pausedAt == nil {
			t.Fatalf("the site must be paused in the database")
		}
		if pausedBy != nil {
			t.Fatalf("an API-key actor has no users(id) row: paused_by must be NULL, got %v", *pausedBy)
		}
	})

	// ---------------------------------------------------------------------
	// GUARD — archived and revoked sites refuse a pause.
	//
	// Mutation: delete `AND NOT (prior.connection_state::text = ANY($6::text[]))`
	// from the UPDATE in pauseMonitoringSQL. Both rows are then paused and
	// reported ok:true — and the archived one, hidden from the default sites
	// list, could never be resumed from the interface.
	// ---------------------------------------------------------------------
	t.Run("an archived or revoked site refuses a pause and says which", func(t *testing.T) {
		for _, tc := range []struct{ state, detail string }{
			{"archived", "site_archived"},
			{"revoked", "site_revoked"},
		} {
			siteID := seedSite(t, pool, tenant, "https://gh414-"+tc.state+"-"+uuid.NewString()[:8]+".example.com")
			setConnectionState(t, pool, siteID, tc.state)

			rec := postJSON(t, engine, "/api/v1/sites/monitoring/pause",
				map[string]any{"site_ids": []string{siteID.String()}, "reason": "should not stick"})
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: the request itself is valid, expected 200, got %d: %s", tc.state, rec.Code, rec.Body.String())
			}
			got := resultFor(t, decodeMonitoring(t, rec.Body.Bytes(), rec.Code), siteID.String())
			if got.OK || got.Changed {
				t.Fatalf("%s: a %s site must not be paused, got %+v", tc.state, tc.state, got)
			}
			if got.Detail != tc.detail {
				t.Fatalf("%s: expected detail %q, got %q", tc.state, tc.detail, got.Detail)
			}
			if pausedAt, _, _, _, _ := readPauseRow(t, pool, tenant, siteID); pausedAt != nil {
				t.Fatalf("%s: the row must be untouched in the database, got paused_at=%v", tc.state, pausedAt)
			}
			if n := countAuditFor(t, pool, tenant, audit.ActionSiteMonitoringPaused, siteID.String()); n != 0 {
				t.Fatalf("%s: a refused pause must not be audited, got %d events", tc.state, n)
			}
		}
	})

	// A connected site is the honest case the lifecycle guard must not block.
	t.Run("a connected site still pauses", func(t *testing.T) {
		siteID := seedSite(t, pool, tenant, "https://gh414-connected-"+uuid.NewString()[:8]+".example.com")
		setConnectionState(t, pool, siteID, "connected")
		rec := postJSON(t, engine, "/api/v1/sites/monitoring/pause",
			map[string]any{"site_ids": []string{siteID.String()}, "reason": "ordinary"})
		got := resultFor(t, decodeMonitoring(t, rec.Body.Bytes(), rec.Code), siteID.String())
		if !got.OK || !got.Changed || got.Detail != "paused" {
			t.Fatalf("a connected site must pause normally, got %+v", got)
		}
	})

	// ---------------------------------------------------------------------
	// FIX 2 — a pause must NOT move sites.updated_at. That column is served as
	// `as_of` on GET /sites/{id}/updates, the freshness stamp of the
	// plugin/theme inventory; bumping it on a pause claims an inventory refresh
	// that never happened.
	//
	// Mutation: restore `updated_at = CASE WHEN ... THEN now() ...` to either
	// statement. This test fails on the timestamp it reads back.
	// ---------------------------------------------------------------------
	t.Run("a pause and a resume leave updated_at alone", func(t *testing.T) {
		siteID := seedSite(t, pool, tenant, "https://gh414-updatedat-"+uuid.NewString()[:8]+".example.com")
		before := readUpdatedAt(t, pool, tenant, siteID)

		pauseVia(t, engine, map[string]any{"site_ids": []string{siteID.String()}, "reason": "quiet"})
		afterPause := readUpdatedAt(t, pool, tenant, siteID)
		if !afterPause.Equal(before) {
			t.Fatalf("a pause must not move updated_at: before=%s after=%s", before, afterPause)
		}

		rec := postJSON(t, engine, "/api/v1/sites/monitoring/resume",
			map[string]any{"site_ids": []string{siteID.String()}})
		if rec.Code != http.StatusOK {
			t.Fatalf("resume failed: %d %s", rec.Code, rec.Body.String())
		}
		afterResume := readUpdatedAt(t, pool, tenant, siteID)
		if !afterResume.Equal(before) {
			t.Fatalf("a resume must not move updated_at: before=%s after=%s", before, afterResume)
		}

		// The pause DID happen — otherwise this test would pass by doing
		// nothing at all.
		if _, _, _, _, found := readPauseRow(t, pool, tenant, siteID); !found {
			t.Fatalf("the site row vanished")
		}
	})

	// ---------------------------------------------------------------------
	// FIX 3 — the 888 KB junk body that answered 200 with a 7.5 MB response.
	// ---------------------------------------------------------------------
	t.Run("an oversized junk body is refused small", func(t *testing.T) {
		junk := make([]string, 100_000)
		for i := range junk {
			junk[i] = "not-a-uuid-not-a-uuid-not-a-uuid"
		}
		body, err := json.Marshal(map[string]any{"site_ids": junk})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/sites/monitoring/pause", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(rec, req)

		t.Logf("request %d bytes -> status %d, response %d bytes", len(body), rec.Code, rec.Body.Len())
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
		}
		if rec.Body.Len() > 4096 {
			t.Fatalf("the refusal must be small, got %d bytes", rec.Body.Len())
		}
	})
}

// --- helpers -------------------------------------------------------------

func postJSON(t *testing.T, engine http.Handler, path string, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(rec, req)
	return rec
}

// pauseVia posts a pause and fails unless it was accepted.
func pauseVia(t *testing.T, engine http.Handler, payload map[string]any) {
	t.Helper()
	rec := postJSON(t, engine, "/api/v1/sites/monitoring/pause", payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("pause failed: %d %s", rec.Code, rec.Body.String())
	}
}

// concurrentMonitoringRequests forces two requests for the same site to overlap
// by holding that row's lock on a separate admin connection until both are
// queued behind it, then releasing it. It returns how many of the two reported
// changed_count > 0.
//
// The deliberate lock-then-release is what makes this deterministic rather than
// a hopeful sleep: neither request can commit before the holder releases, so
// both are certainly in flight at the same time.
func concurrentMonitoringRequests(t *testing.T, pool *db.Pool, engine http.Handler, path string, siteID uuid.UUID, payload map[string]any) int {
	t.Helper()
	ctx := context.Background()

	admin := connectAdmin(t, pool)
	defer admin.Close()
	holder, err := admin.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock holder: %v", err)
	}
	if _, err := holder.Exec(ctx, "SELECT id FROM sites WHERE id = $1 FOR UPDATE", siteID); err != nil {
		t.Fatalf("hold row lock: %v", err)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		changed int
	)
	// fire runs on its OWN goroutine (via `go fire()` below), so it must never
	// reach a t.Fatalf, directly or through a helper: t.Fatalf calls
	// runtime.Goexit on the CALLING goroutine, not the test's own goroutine, so
	// it would unwind fire (running its deferred wg.Done()) while
	// concurrentMonitoringRequests keeps executing past wg.Wait() as if nothing
	// had failed, asserting on a `changed` count that never got incremented for
	// the failed request and losing the real failure's message from the
	// subtest's actual failure path. postJSON and decodeMonitoring both call
	// t.Fatalf internally, so fire is deliberately inlined below instead of
	// calling them, using t.Errorf + return so a failure here marks the test
	// failed without killing a goroutine other than the one still running the
	// test body.
	fire := func() {
		defer wg.Done()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Errorf("marshal: %v", err)
			return
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("concurrent request failed: %d %s", rec.Code, rec.Body.String())
			return
		}
		var got monitoringBulkResult
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Errorf("decode monitoring response (status %d, body %s): %v", rec.Code, rec.Body.String(), err)
			return
		}
		mu.Lock()
		changed += got.ChangedCount
		mu.Unlock()
	}

	wg.Add(1)
	go fire()
	time.Sleep(400 * time.Millisecond) // the first request is now blocked on the lock
	wg.Add(1)
	go fire()
	time.Sleep(400 * time.Millisecond) // and so is the second

	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release lock: %v", err)
	}
	wg.Wait()
	return changed
}

// readAuditMeta returns the metadata of the single audit_log row for one action
// against one site, failing unless there is exactly one.
func readAuditMeta(t *testing.T, pool *db.Pool, tenant uuid.UUID, action, targetID string) map[string]any {
	t.Helper()
	var raw []byte
	err := pool.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `
			SELECT metadata FROM audit_log
			 WHERE tenant_id = $1 AND action = $2
			   AND target_type = 'site' AND target_id = $3`,
			tenant, action, targetID).Scan(&raw)
	})
	if err != nil {
		t.Fatalf("read audit metadata (action %s, site %s): %v", action, targetID, err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal audit metadata %s: %v", raw, err)
	}
	return out
}

// readUpdatedAt reads sites.updated_at through the same tenant tx helper the
// request uses.
func readUpdatedAt(t *testing.T, pool *db.Pool, tenant, siteID uuid.UUID) time.Time {
	t.Helper()
	var out time.Time
	err := pool.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT updated_at FROM sites WHERE tenant_id = $1 AND id = $2`, tenant, siteID).Scan(&out)
	})
	if err != nil {
		t.Fatalf("read updated_at: %v", err)
	}
	return out
}

// setConnectionState puts a seeded site into a lifecycle state for the test.
// This is SETUP, so it runs on the admin connection; every assertion in the
// tests above still goes through the app role.
func setConnectionState(t *testing.T, pool *db.Pool, siteID uuid.UUID, state string) {
	t.Helper()
	ctx := context.Background()
	admin := connectAdmin(t, pool)
	defer admin.Close()
	if _, err := admin.Exec(ctx,
		`UPDATE sites SET connection_state = $2 WHERE id = $1`, siteID, state); err != nil {
		t.Fatalf("set connection_state=%s: %v", state, err)
	}
}
