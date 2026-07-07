// Integration tests for GH #152 part 2 — owner-facing organisation deletion
// (soft-delete + grace-window purge) against a real Postgres with the
// production role topology (see startPostgres in rls_integration_test.go).
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

	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/apikey"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/middleware"
	"github.com/mosamlife/wpmgr/apps/api/internal/org"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// ---------------------------------------------------------------------------
// local seed helpers (distinct names to avoid colliding with the ones already
// defined alongside startPostgres/admin-orphan-cleanup tests in this package)
// ---------------------------------------------------------------------------

func odSeedMembership(t *testing.T, admin *db.Pool, user, tenant uuid.UUID, role string) {
	t.Helper()
	if _, err := admin.Exec(context.Background(),
		`INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, $3)`,
		tenant, user, role); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
}

func odSeedSite(t *testing.T, admin *db.Pool, tenant uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := admin.QueryRow(context.Background(),
		`INSERT INTO sites (tenant_id, url, name) VALUES ($1, $2, 'seed') RETURNING id`,
		tenant, "https://"+uuid.NewString()+".example.com").Scan(&id); err != nil {
		t.Fatalf("seed site: %v", err)
	}
	return id
}

func odSeedRestoreRun(t *testing.T, admin *db.Pool, tenant, siteID uuid.UUID, status string) {
	t.Helper()
	if _, err := admin.Exec(context.Background(),
		`INSERT INTO restore_runs (tenant_id, site_id, snapshot_id, status) VALUES ($1, $2, $3, $4)`,
		tenant, siteID, uuid.New(), status); err != nil {
		t.Fatalf("seed restore run: %v", err)
	}
}

func odTenantDeletedAt(t *testing.T, admin *db.Pool, tenant uuid.UUID) *time.Time {
	t.Helper()
	var ts *time.Time
	if err := admin.QueryRow(context.Background(),
		`SELECT deleted_at FROM tenants WHERE id = $1`, tenant).Scan(&ts); err != nil {
		if err == pgx.ErrNoRows {
			return nil
		}
		t.Fatalf("read deleted_at: %v", err)
	}
	return ts
}

func odBackdateDeletedAt(t *testing.T, admin *db.Pool, tenant uuid.UUID, age time.Duration) {
	t.Helper()
	if _, err := admin.Exec(context.Background(),
		`UPDATE tenants SET deleted_at = now() - $2::interval WHERE id = $1`,
		tenant, age.String()); err != nil {
		t.Fatalf("backdate deleted_at: %v", err)
	}
}

// buildOrgEngine mounts the real org.Handler on a gin engine with a fake
// context-injection middleware standing in for session auth (mirrors
// buildAdminEngine in admin_billing_panel_integration_test.go).
func buildOrgEngine(t *testing.T, pool *db.Pool, authSvc *auth.Service, rec *audit.Recorder, hosted bool, p domain.Principal) *gin.Engine {
	t.Helper()
	return buildOrgEngineWithSessions(t, pool, authSvc, rec, hosted, p, nil)
}

// buildOrgEngineWithSessions is buildOrgEngine plus a REAL org.SessionManager
// (rather than nil). Needed only by tests that exercise DELETE's post-commit
// session-reassignment (h.sessions.SetActiveTenant) — every other guard test
// never deletes the caller's own active org, so SetActiveTenant is never
// called and a nil SessionManager is safe there.
func buildOrgEngineWithSessions(t *testing.T, pool *db.Pool, authSvc *auth.Service, rec *audit.Recorder, hosted bool, p domain.Principal, sessions org.SessionManager) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := org.NewHandler(pool, nil, sessions, authSvc, rec)
	h.SetHosted(hosted)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	})
	v1Auth := engine.Group("/api/v1")
	h.Register(v1Auth)
	return engine
}

func odDo(engine *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)
	return w
}

// odDoWithContext is odDo but lets the caller supply the request's base
// context (e.g. a real SCS-session-loaded context) instead of a bare one —
// needed to observe h.sessions.SetActiveTenant's effect on a real session.
func odDoWithContext(ctx context.Context, engine *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)
	return w
}

func odNewAuthSvc(pool *db.Pool) (*auth.Service, *audit.Recorder) {
	rec := audit.NewRecorder(pool, domain.SystemClock{})
	return auth.NewService(auth.NewRepo(pool), rec, domain.NewValidator()), rec
}

// ---------------------------------------------------------------------------
// DELETE /api/v1/orgs/{orgId}
// ---------------------------------------------------------------------------

// TestOrgDelete_OwnerEmptyOrgHardDeletes covers "owner can delete" AND
// "empty org -> hard delete": an owner deleting a zero-site, zero-other-member
// org gets Lane A (immediate hard delete via admin_delete_empty_tenant); the
// tenants row is gone afterward.
func TestOrgDelete_OwnerEmptyOrgHardDeletes(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	authSvc, rec := odNewAuthSvc(pool)

	slug := "od-empty-" + uuid.NewString()[:8]
	target := seedTenant(t, pool, slug)
	owner := seedUserRow(t, admin, "od-owner-"+uuid.NewString()[:8]+"@example.com")
	odSeedMembership(t, admin, owner, target, "owner")

	// The caller's ACTIVE org must differ from the target (the delete guard
	// refuses deleting the currently-active org).
	home := seedTenant(t, pool, "od-home-"+uuid.NewString()[:8])
	odSeedMembership(t, admin, owner, home, "owner")

	p := domain.Principal{Type: domain.PrincipalUser, UserID: owner, TenantID: home, Role: "owner", Scope: domain.ScopeOrg}
	engine := buildOrgEngine(t, pool, authSvc, rec, false, p)

	w := odDo(engine, http.MethodDelete, "/api/v1/orgs/"+target.String(), `{"confirm_name":"`+slug+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		ID   string `json:"id"`
		Lane string `json:"lane"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Lane != "hard" {
		t.Fatalf("lane = %q, want %q", resp.Lane, "hard")
	}
	if tenantExists(t, admin, target) {
		t.Fatal("empty org should be hard-deleted (tenants row gone), but it still exists")
	}
}

// TestOrgDelete_NonOwnerForbidden covers "non-owner 403": an admin-role
// member (below owner) cannot delete the org.
func TestOrgDelete_NonOwnerForbidden(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	authSvc, rec := odNewAuthSvc(pool)

	slug := "od-nonowner-" + uuid.NewString()[:8]
	target := seedTenant(t, pool, slug)
	member := seedUserRow(t, admin, "od-admin-"+uuid.NewString()[:8]+"@example.com")
	odSeedMembership(t, admin, member, target, "admin")

	home := seedTenant(t, pool, "od-home2-"+uuid.NewString()[:8])
	odSeedMembership(t, admin, member, home, "admin")

	p := domain.Principal{Type: domain.PrincipalUser, UserID: member, TenantID: home, Role: "admin", Scope: domain.ScopeOrg}
	engine := buildOrgEngine(t, pool, authSvc, rec, false, p)

	w := odDo(engine, http.MethodDelete, "/api/v1/orgs/"+target.String(), `{"confirm_name":"`+slug+`"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", w.Code, w.Body.String())
	}
	if !tenantExists(t, admin, target) {
		t.Fatal("org must not be deleted when the caller is not the owner")
	}
}

// TestOrgDelete_ConfirmNameMismatch422 covers "confirm_name mismatch 422".
func TestOrgDelete_ConfirmNameMismatch422(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	authSvc, rec := odNewAuthSvc(pool)

	slug := "od-mismatch-" + uuid.NewString()[:8]
	target := seedTenant(t, pool, slug)
	owner := seedUserRow(t, admin, "od-owner2-"+uuid.NewString()[:8]+"@example.com")
	odSeedMembership(t, admin, owner, target, "owner")

	home := seedTenant(t, pool, "od-home3-"+uuid.NewString()[:8])
	odSeedMembership(t, admin, owner, home, "owner")

	p := domain.Principal{Type: domain.PrincipalUser, UserID: owner, TenantID: home, Role: "owner", Scope: domain.ScopeOrg}
	engine := buildOrgEngine(t, pool, authSvc, rec, false, p)

	w := odDo(engine, http.MethodDelete, "/api/v1/orgs/"+target.String(), `{"confirm_name":"totally-wrong-name"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", w.Code, w.Body.String())
	}
	if !tenantExists(t, admin, target) {
		t.Fatal("org must not be deleted on a confirm_name mismatch")
	}
}

// TestOrgDelete_ActiveOrgRefused covers "active-org refused": the caller
// cannot delete the org they are currently active in.
// TestOrgDelete_ActiveOrgSucceedsAndReassignsSession covers the design
// reconciliation: deleting the caller's own CURRENTLY ACTIVE org is now
// ALLOWED (the Danger Zone UI only ever lives on Settings for the active org,
// so refusing this made the feature unreachable, and also made deleting a
// user's last org impossible). The delete must succeed, and the caller's
// session must never be left pointing at the now-deleted tenant: it is
// reassigned to another live org when one exists, or cleared (uuid.Nil) when
// the deleted org was the caller's last one.
func TestOrgDelete_ActiveOrgSucceedsAndReassignsSession(t *testing.T) {
	t.Run("reassigns_to_another_live_org", func(t *testing.T) {
		pool := startPostgres(t)
		admin := connectAdmin(t, pool)
		authSvc, rec := odNewAuthSvc(pool)

		slug := "od-active-" + uuid.NewString()[:8]
		target := seedTenant(t, pool, slug)
		owner := seedUserRow(t, admin, "od-owner3-"+uuid.NewString()[:8]+"@example.com")
		odSeedMembership(t, admin, owner, target, "owner")
		odSeedSite(t, admin, target) // non-empty -> Lane B (soft delete)
		other := seedTenant(t, pool, "od-other-"+uuid.NewString()[:8])
		odSeedMembership(t, admin, owner, other, "owner")

		sessions := auth.NewSessionManagerWithStore(scs.New(), false)
		sessCtx, err := sessions.SCS().Load(context.Background(), "")
		if err != nil {
			t.Fatalf("load session: %v", err)
		}
		if err := sessions.Login(sessCtx, owner, target); err != nil {
			t.Fatalf("login: %v", err)
		}

		p := domain.Principal{Type: domain.PrincipalUser, UserID: owner, TenantID: target, Role: "owner", Scope: domain.ScopeOrg}
		engine := buildOrgEngineWithSessions(t, pool, authSvc, rec, false, p, sessions)

		w := odDoWithContext(sessCtx, engine, http.MethodDelete, "/api/v1/orgs/"+target.String(), `{"confirm_name":"`+slug+`"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (active-org delete is now allowed); body = %s", w.Code, w.Body.String())
		}
		var resp struct {
			Lane           string  `json:"lane"`
			ActiveTenantID *string `json:"active_tenant_id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if resp.Lane != "soft" {
			t.Fatalf("lane = %q, want %q (target has a site)", resp.Lane, "soft")
		}
		if resp.ActiveTenantID == nil || *resp.ActiveTenantID != other.String() {
			t.Fatalf("response active_tenant_id = %v, want %s", resp.ActiveTenantID, other)
		}

		_, gotTenant, ok := sessions.Current(sessCtx)
		if !ok {
			t.Fatal("session should still be authenticated")
		}
		if gotTenant != other {
			t.Fatalf("session active tenant = %s, want reassigned to %s", gotTenant, other)
		}
		if !tenantExists(t, admin, target) {
			t.Fatal("a populated org must be SOFT-deleted, not hard-deleted — the tenants row must still exist")
		}
		if odTenantDeletedAt(t, admin, target) == nil {
			t.Fatal("tenants.deleted_at must be set after the delete")
		}
	})

	t.Run("clears_when_it_was_the_last_org", func(t *testing.T) {
		pool := startPostgres(t)
		admin := connectAdmin(t, pool)
		authSvc, rec := odNewAuthSvc(pool)

		slug := "od-lastorg-" + uuid.NewString()[:8]
		target := seedTenant(t, pool, slug)
		owner := seedUserRow(t, admin, "od-owner3b-"+uuid.NewString()[:8]+"@example.com")
		odSeedMembership(t, admin, owner, target, "owner")
		// No other org, no sites: this is the caller's ONLY org (Lane A, hard delete).

		sessions := auth.NewSessionManagerWithStore(scs.New(), false)
		sessCtx, err := sessions.SCS().Load(context.Background(), "")
		if err != nil {
			t.Fatalf("load session: %v", err)
		}
		if err := sessions.Login(sessCtx, owner, target); err != nil {
			t.Fatalf("login: %v", err)
		}

		p := domain.Principal{Type: domain.PrincipalUser, UserID: owner, TenantID: target, Role: "owner", Scope: domain.ScopeOrg}
		engine := buildOrgEngineWithSessions(t, pool, authSvc, rec, false, p, sessions)

		w := odDoWithContext(sessCtx, engine, http.MethodDelete, "/api/v1/orgs/"+target.String(), `{"confirm_name":"`+slug+`"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		var resp struct {
			Lane           string  `json:"lane"`
			ActiveTenantID *string `json:"active_tenant_id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if resp.Lane != "hard" {
			t.Fatalf("lane = %q, want %q (the caller's only org, zero sites, zero other members)", resp.Lane, "hard")
		}
		if resp.ActiveTenantID != nil {
			t.Fatalf("response active_tenant_id = %v, want nil (no org left)", *resp.ActiveTenantID)
		}

		_, gotTenant, ok := sessions.Current(sessCtx)
		if !ok {
			t.Fatal("session should still be authenticated (UserID must survive)")
		}
		if gotTenant != uuid.Nil {
			t.Fatalf("session active tenant = %s, want uuid.Nil (deleted the caller's last org)", gotTenant)
		}
		if tenantExists(t, admin, target) {
			t.Fatal("the caller's only, empty org should be hard-deleted (Lane A)")
		}
	})
}

// TestOrgDelete_RestoreInProgressRefused covers "restore-in-progress
// refused": a site in the org has a queued/running restore_runs row.
func TestOrgDelete_RestoreInProgressRefused(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	authSvc, rec := odNewAuthSvc(pool)

	slug := "od-restoring-" + uuid.NewString()[:8]
	target := seedTenant(t, pool, slug)
	owner := seedUserRow(t, admin, "od-owner4-"+uuid.NewString()[:8]+"@example.com")
	odSeedMembership(t, admin, owner, target, "owner")
	siteID := odSeedSite(t, admin, target)
	odSeedRestoreRun(t, admin, target, siteID, "running")

	home := seedTenant(t, pool, "od-home4-"+uuid.NewString()[:8])
	odSeedMembership(t, admin, owner, home, "owner")

	p := domain.Principal{Type: domain.PrincipalUser, UserID: owner, TenantID: home, Role: "owner", Scope: domain.ScopeOrg}
	engine := buildOrgEngine(t, pool, authSvc, rec, false, p)

	w := odDo(engine, http.MethodDelete, "/api/v1/orgs/"+target.String(), `{"confirm_name":"`+slug+`"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", w.Code, w.Body.String())
	}
	if !tenantExists(t, admin, target) {
		t.Fatal("org must not be deleted while a restore is in progress")
	}
}

// TestOrgDelete_PopulatedOrgSoftDeletes covers "populated org -> deleted_at
// set + hidden from the org list": a org with a site is soft-deleted (Lane
// B), the tenants row survives with deleted_at set, and it vanishes from
// GET /orgs for the same owner.
func TestOrgDelete_PopulatedOrgSoftDeletes(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	authSvc, rec := odNewAuthSvc(pool)

	slug := "od-populated-" + uuid.NewString()[:8]
	target := seedTenant(t, pool, slug)
	owner := seedUserRow(t, admin, "od-owner5-"+uuid.NewString()[:8]+"@example.com")
	odSeedMembership(t, admin, owner, target, "owner")
	odSeedSite(t, admin, target) // makes the org non-empty -> Lane B

	home := seedTenant(t, pool, "od-home5-"+uuid.NewString()[:8])
	odSeedMembership(t, admin, owner, home, "owner")

	p := domain.Principal{Type: domain.PrincipalUser, UserID: owner, TenantID: home, Role: "owner", Scope: domain.ScopeOrg}
	engine := buildOrgEngine(t, pool, authSvc, rec, false, p)

	// Sanity: the target org is visible in the list BEFORE deletion.
	wList := odDo(engine, http.MethodGet, "/api/v1/orgs", "")
	if wList.Code != http.StatusOK {
		t.Fatalf("pre-delete list status = %d, body = %s", wList.Code, wList.Body.String())
	}
	if !strings.Contains(wList.Body.String(), target.String()) {
		t.Fatalf("target org missing from the pre-delete org list: %s", wList.Body.String())
	}

	w := odDo(engine, http.MethodDelete, "/api/v1/orgs/"+target.String(), `{"confirm_name":"`+slug+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Lane string `json:"lane"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Lane != "soft" {
		t.Fatalf("lane = %q, want %q", resp.Lane, "soft")
	}
	if !tenantExists(t, admin, target) {
		t.Fatal("a populated org must be SOFT-deleted, not hard-deleted — the tenants row must still exist")
	}
	if odTenantDeletedAt(t, admin, target) == nil {
		t.Fatal("tenants.deleted_at must be set after a Lane-B delete")
	}

	wList2 := odDo(engine, http.MethodGet, "/api/v1/orgs", "")
	if wList2.Code != http.StatusOK {
		t.Fatalf("post-delete list status = %d, body = %s", wList2.Code, wList2.Body.String())
	}
	if strings.Contains(wList2.Body.String(), target.String()) {
		t.Fatalf("soft-deleted org must be hidden from the org list, got: %s", wList2.Body.String())
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/orgs/{orgId}/restore
// ---------------------------------------------------------------------------

// TestOrgRestore_ClearsDeletedAt covers "undelete clears deleted_at": an
// owner restores a soft-deleted org within the grace window; it reappears in
// GET /orgs. Also covers the 409 (not deleted) and 404 (already purged) edges.
func TestOrgRestore_ClearsDeletedAt(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	authSvc, rec := odNewAuthSvc(pool)

	slug := "od-restore-" + uuid.NewString()[:8]
	target := seedTenant(t, pool, slug)
	owner := seedUserRow(t, admin, "od-owner6-"+uuid.NewString()[:8]+"@example.com")
	odSeedMembership(t, admin, owner, target, "owner")
	odSeedSite(t, admin, target)

	// Soft-delete directly (bypassing the endpoint) to isolate the restore
	// path under test.
	if _, err := admin.Exec(context.Background(), `UPDATE tenants SET deleted_at = now() WHERE id = $1`, target); err != nil {
		t.Fatalf("seed soft-delete: %v", err)
	}

	// The principal's active tenant is deliberately the (now soft-deleted)
	// target itself: restore must work via the direct, unfiltered membership
	// check (sqlc.GetMembership), NOT authSvc.RoleInTenant (which would say
	// "not a member" for a soft-deleted tenant).
	p := domain.Principal{Type: domain.PrincipalUser, UserID: owner, TenantID: target, Role: "owner", Scope: domain.ScopeOrg}
	engine := buildOrgEngine(t, pool, authSvc, rec, false, p)

	// 409: restoring a LIVE (never-deleted) org.
	liveTenant := seedTenant(t, pool, "od-live-"+uuid.NewString()[:8])
	odSeedMembership(t, admin, owner, liveTenant, "owner")
	wLive := odDo(engine, http.MethodPost, "/api/v1/orgs/"+liveTenant.String()+"/restore", "")
	if wLive.Code != http.StatusConflict {
		t.Fatalf("restore-live status = %d, want 409; body = %s", wLive.Code, wLive.Body.String())
	}

	// 200: the actual restore.
	w := odDo(engine, http.MethodPost, "/api/v1/orgs/"+target.String()+"/restore", "")
	if w.Code != http.StatusOK {
		t.Fatalf("restore status = %d, body = %s", w.Code, w.Body.String())
	}
	if odTenantDeletedAt(t, admin, target) != nil {
		t.Fatal("deleted_at must be cleared after restore")
	}

	// Visible again in the org list.
	wList := odDo(engine, http.MethodGet, "/api/v1/orgs", "")
	if !strings.Contains(wList.Body.String(), target.String()) {
		t.Fatalf("restored org must reappear in the org list, got: %s", wList.Body.String())
	}

	// 409 purge_in_progress (adversarial-review fast-follow M2): once the
	// PurgeWorker's point-of-no-return marker is set, restore must be refused
	// even though the tenant row (and its membership) still exists.
	purgingTenant := seedTenant(t, pool, "od-purging-"+uuid.NewString()[:8])
	odSeedMembership(t, admin, owner, purgingTenant, "owner")
	if _, err := admin.Exec(context.Background(),
		`UPDATE tenants SET deleted_at = now(), purge_started_at = now() WHERE id = $1`, purgingTenant); err != nil {
		t.Fatalf("seed purge-in-progress: %v", err)
	}
	pPurging := domain.Principal{Type: domain.PrincipalUser, UserID: owner, TenantID: purgingTenant, Role: "owner", Scope: domain.ScopeOrg}
	enginePurging := buildOrgEngine(t, pool, authSvc, rec, false, pPurging)
	wPurging := odDo(enginePurging, http.MethodPost, "/api/v1/orgs/"+purgingTenant.String()+"/restore", "")
	if wPurging.Code != http.StatusConflict {
		t.Fatalf("restore-purge-in-progress status = %d, want 409; body = %s", wPurging.Code, wPurging.Body.String())
	}
	if !strings.Contains(wPurging.Body.String(), "purge_in_progress") {
		t.Fatalf("expected code purge_in_progress, got body: %s", wPurging.Body.String())
	}
	if odTenantDeletedAt(t, admin, purgingTenant) == nil {
		t.Fatal("a purge-in-progress tenant must remain soft-deleted (restore refused)")
	}

	// Restoring an already hard-purged org: admin_purge_tenant's cascade
	// removes the memberships row along with everything else, so the caller
	// fails the SAME membership check a genuine non-member/unknown-org caller
	// fails — 403 not_a_member, not a distinguishing 404. This is deliberate:
	// it mirrors this codebase's existing non-disclosure convention
	// (tenant.Repo.GetForUser: "Non-member or unknown tenant: do not disclose
	// existence") — the restore endpoint must not become an oracle that lets
	// an arbitrary caller learn whether an orgId was ever purged.
	purgedTenant := seedTenant(t, pool, "od-purged-"+uuid.NewString()[:8])
	odSeedMembership(t, admin, owner, purgedTenant, "owner")
	if _, err := admin.Exec(context.Background(), `UPDATE tenants SET deleted_at = now() WHERE id = $1`, purgedTenant); err != nil {
		t.Fatalf("seed soft-delete: %v", err)
	}
	var purged bool
	if err := admin.QueryRow(context.Background(), `SELECT admin_purge_tenant($1)`, purgedTenant).Scan(&purged); err != nil {
		t.Fatalf("admin_purge_tenant: %v", err)
	}
	if !purged {
		t.Fatal("admin_purge_tenant should have purged the soft-deleted tenant")
	}
	pPurged := domain.Principal{Type: domain.PrincipalUser, UserID: owner, TenantID: purgedTenant, Role: "owner", Scope: domain.ScopeOrg}
	enginePurged := buildOrgEngine(t, pool, authSvc, rec, false, pPurged)
	wPurged := odDo(enginePurged, http.MethodPost, "/api/v1/orgs/"+purgedTenant.String()+"/restore", "")
	if wPurged.Code != http.StatusForbidden {
		t.Fatalf("restore-purged status = %d, want 403; body = %s", wPurged.Code, wPurged.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Auth middleware
// ---------------------------------------------------------------------------

// TestOrgAuthMiddleware_RejectsSoftDeletedActiveTenant covers "auth middleware
// rejects a session pinned to it": a session whose active_tenant_id points at
// a now-soft-deleted org must resolve to TenantID==uuid.Nil (RequireTenant
// would then 403 it), even though the underlying membership row still
// physically exists in the DB.
func TestOrgAuthMiddleware_RejectsSoftDeletedActiveTenant(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)
	authSvc, rec := odNewAuthSvc(pool)
	_ = rec

	target := seedTenant(t, pool, "od-mw-"+uuid.NewString()[:8])
	owner := seedUserRow(t, admin, "od-mwowner-"+uuid.NewString()[:8]+"@example.com")
	odSeedMembership(t, admin, owner, target, "owner")

	sessions := auth.NewSessionManagerWithStore(scs.New(), false)
	keys := apikey.NewService(pool)
	authn := middleware.NewAuthenticator(sessions, authSvc, keys, pool)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(authn.Authenticate())
	var gotPrincipal domain.Principal
	var gotOK bool
	engine.GET("/whoami", func(c *gin.Context) {
		gotPrincipal, gotOK = domain.PrincipalFromContext(c.Request.Context())
		c.Status(http.StatusOK)
	})

	sessCtx, err := sessions.SCS().Load(context.Background(), "")
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if err := sessions.Login(sessCtx, owner, target); err != nil {
		t.Fatalf("login: %v", err)
	}

	// Before soft-delete: full org membership resolves normally.
	req := httptest.NewRequest(http.MethodGet, "/whoami", nil).WithContext(sessCtx)
	engine.ServeHTTP(httptest.NewRecorder(), req)
	if !gotOK || gotPrincipal.TenantID != target || gotPrincipal.Scope != domain.ScopeOrg {
		t.Fatalf("pre-delete principal = %+v (ok=%v), want TenantID=%s Scope=org", gotPrincipal, gotOK, target)
	}

	// Soft-delete the active tenant.
	if _, err := admin.Exec(context.Background(), `UPDATE tenants SET deleted_at = now() WHERE id = $1`, target); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/whoami", nil).WithContext(sessCtx)
	engine.ServeHTTP(httptest.NewRecorder(), req2)
	if !gotOK {
		t.Fatal("principal should still be attached (UserID must survive so /auth/me works), just with TenantID cleared")
	}
	if gotPrincipal.TenantID != uuid.Nil {
		t.Fatalf("a session pinned to a soft-deleted active tenant must resolve TenantID=uuid.Nil, got %s", gotPrincipal.TenantID)
	}
}

// ---------------------------------------------------------------------------
// PurgeWorker
// ---------------------------------------------------------------------------

type odOrderRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *odOrderRecorder) add(e string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *odOrderRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	copy(out, r.events)
	return out
}

type odFakeSiteLister struct{ ids []uuid.UUID }

func (f *odFakeSiteLister) ListAllSiteIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return f.ids, nil
}

type odFakeRevoker struct{ rec *odOrderRecorder }

func (f *odFakeRevoker) Revoke(_ context.Context, in site.ActorSiteInput) (site.Site, error) {
	f.rec.add("revoke:" + in.SiteID.String())
	return site.Site{}, nil
}

type odFakeStore struct {
	rec  *odOrderRecorder
	keys map[string][]string
}

func (f *odFakeStore) List(_ context.Context, prefix string) ([]string, error) {
	f.rec.add("list:" + prefix)
	return f.keys[prefix], nil
}

func (f *odFakeStore) Delete(_ context.Context, key string) error {
	f.rec.add("delete:" + key)
	return nil
}

// TestOrgPurgeWorker_RevokeThenObjectsThenHardDelete_Idempotent covers "the
// purge worker revokes -> purges-objects -> hard-deletes in order and is
// idempotent" using a fake storage + fake revoke, exactly as specified.
func TestOrgPurgeWorker_RevokeThenObjectsThenHardDelete_Idempotent(t *testing.T) {
	pool := startPostgres(t)
	admin := connectAdmin(t, pool)

	target := seedTenant(t, pool, "od-purge-"+uuid.NewString()[:8])
	owner := seedUserRow(t, admin, "od-purgeowner-"+uuid.NewString()[:8]+"@example.com")
	odSeedMembership(t, admin, owner, target, "owner")
	odSeedSite(t, admin, target)

	// Past the grace window (worker uses a 1h grace; back-date well beyond it).
	odBackdateDeletedAt(t, admin, target, 30*24*time.Hour)

	fakeSiteID := uuid.New()
	rec := &odOrderRecorder{}
	lister := &odFakeSiteLister{ids: []uuid.UUID{fakeSiteID}}
	revoker := &odFakeRevoker{rec: rec}
	chunkKey := "chunks/" + target.String() + "/deadbeef"
	tenantKey := "tenant/" + target.String() + "/site/x/backup/y/manifest.json"
	store := &odFakeStore{rec: rec, keys: map[string][]string{
		"chunks/" + target.String() + "/": {chunkKey},
		"tenant/" + target.String() + "/": {tenantKey},
	}}

	worker := org.NewPurgeWorker(pool, lister, revoker, store, time.Hour, nil)

	if err := worker.Work(context.Background(), nil); err != nil {
		t.Fatalf("first sweep: %v", err)
	}

	events := rec.snapshot()
	if len(events) == 0 {
		t.Fatal("purge worker recorded no events at all")
	}
	// Order: revoke must precede both object-storage list calls.
	revokeIdx, chunksListIdx, tenantListIdx, chunkDeleteIdx := -1, -1, -1, -1
	for i, e := range events {
		switch {
		case e == "revoke:"+fakeSiteID.String() && revokeIdx == -1:
			revokeIdx = i
		case e == "list:chunks/"+target.String()+"/" && chunksListIdx == -1:
			chunksListIdx = i
		case e == "list:tenant/"+target.String()+"/" && tenantListIdx == -1:
			tenantListIdx = i
		case e == "delete:"+chunkKey && chunkDeleteIdx == -1:
			chunkDeleteIdx = i
		}
	}
	if revokeIdx == -1 || chunksListIdx == -1 || tenantListIdx == -1 || chunkDeleteIdx == -1 {
		t.Fatalf("missing expected events: %v", events)
	}
	if !(revokeIdx < chunksListIdx && chunksListIdx < tenantListIdx) {
		t.Fatalf("expected revoke -> chunks-list -> tenant-list order, got: %v", events)
	}
	if chunkDeleteIdx < chunksListIdx {
		t.Fatalf("delete must come after its list, got: %v", events)
	}

	// The hard delete (admin_purge_tenant) happens LAST, after every recorded
	// event above — proven by the tenants row now being fully gone.
	if tenantExists(t, admin, target) {
		t.Fatal("tenant should be hard-deleted after the purge sweep")
	}

	// Idempotency: a second sweep must be a clean no-op (tenant no longer in
	// ListTenantsPendingPurge), no error, no new events.
	before := len(rec.snapshot())
	if err := worker.Work(context.Background(), nil); err != nil {
		t.Fatalf("second sweep (idempotency check): %v", err)
	}
	after := len(rec.snapshot())
	if after != before {
		t.Fatalf("second sweep should not touch the already-purged tenant again: before=%d after=%d events=%v",
			before, after, rec.snapshot())
	}
}

// TestAdminPurgeTenant_CascadesPopulatedTenantAndDoesNotLeakTenantGUC proves
// two things about the SECURITY DEFINER admin_purge_tenant (m93):
//  1. It actually cascades a POPULATED tenant's rows (site, membership,
//     audit_log) — the design requires app.tenant_id to stay set THROUGH the
//     final `DELETE FROM tenants` (unlike admin_delete_empty_tenant, which
//     blanks it early because it only ever purges an empty tenant).
//  2. It restores the caller's prior app.tenant_id exactly once on its single
//     return path (the M91 Finding A GUC-leak lesson), so it can never leak
//     'the just-purged tenant id' into the rest of the caller's transaction.
func TestAdminPurgeTenant_CascadesPopulatedTenantAndDoesNotLeakTenantGUC(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	admin := connectAdmin(t, pool)

	tenant := seedTenant(t, pool, "od-purge-cascade-"+uuid.NewString()[:8])
	user := seedUserRow(t, admin, "od-cascade-"+uuid.NewString()[:8]+"@example.com")
	odSeedMembership(t, admin, user, tenant, "owner")
	odSeedSite(t, admin, tenant)
	if _, err := admin.Exec(ctx,
		`INSERT INTO audit_log (tenant_id, actor_type, action, hash) VALUES ($1, 'user', 'register', 'seed-hash')`,
		tenant); err != nil {
		t.Fatalf("seed audit row: %v", err)
	}

	var before, after string
	var purged bool
	err := pgx.BeginFunc(ctx, pool.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "SELECT coalesce(current_setting('app.tenant_id', true), '')").Scan(&before); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, "SELECT admin_purge_tenant($1)", tenant).Scan(&purged); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "SELECT coalesce(current_setting('app.tenant_id', true), '')").Scan(&after)
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
	if !purged {
		t.Fatal("admin_purge_tenant should purge a populated tenant (no emptiness guard)")
	}
	if before != "" {
		t.Fatalf("test setup invariant broken: app.tenant_id was already %q", before)
	}
	if after != before {
		t.Fatalf("admin_purge_tenant leaked app.tenant_id into the caller's transaction: before=%q after=%q",
			before, after)
	}

	if tenantExists(t, admin, tenant) {
		t.Fatal("tenants row should be gone after admin_purge_tenant")
	}
	var siteCount, membershipCount, auditCount int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM sites WHERE tenant_id = $1`, tenant).Scan(&siteCount); err != nil {
		t.Fatalf("count sites: %v", err)
	}
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE tenant_id = $1`, tenant).Scan(&membershipCount); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1`, tenant).Scan(&auditCount); err != nil {
		t.Fatalf("count audit_log: %v", err)
	}
	if siteCount != 0 || membershipCount != 0 || auditCount != 0 {
		t.Fatalf("admin_purge_tenant must cascade every child row: sites=%d memberships=%d audit_log=%d",
			siteCount, membershipCount, auditCount)
	}
}
