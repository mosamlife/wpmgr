// gh414_monitoring_pause_routes_test.go — GH #414 phase 1 route coverage for
// "pause monitoring".
//
// Reaches the handlers through the SAME path production uses: a real Gin
// engine with site.Handler.Register mounted on /api/v1, so the real
// authz.RequirePermission middleware and the real Gin router matching run;
// the service and repo are the real ones; and every statement lands on the
// testcontainers Postgres through pool.InTenantTxAsUser as the non-superuser
// wpmgr_app role, so the sites RLS policies are LIVE for every assertion.
// A test that opened its own connection would leave those policies inert —
// that is exactly how m112's proofs passed over a cross-site-readable table.
package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// monitoringResult mirrors the handler's per-site DTO on the wire.
type monitoringResult struct {
	SiteID                 string     `json:"site_id"`
	OK                     bool       `json:"ok"`
	Changed                bool       `json:"changed"`
	Detail                 string     `json:"detail"`
	MonitoringPausedAt     *time.Time `json:"monitoring_paused_at"`
	MonitoringPausedReason string     `json:"monitoring_paused_reason"`
	MonitoringResumeAt     *time.Time `json:"monitoring_resume_at"`
}

type monitoringBulkResult struct {
	Results      []monitoringResult `json:"results"`
	ChangedCount int                `json:"changed_count"`
}

// newMonitoringEngine mounts the REAL site routes (including the real
// authz.RequirePermission middleware baked into Register) behind a middleware
// that injects the principal exactly as production auth middleware does. The
// audit recorder is the real one so the per-site audit assertions exercise the
// production write path, not a stub.
func newMonitoringEngine(t *testing.T, pool *db.Pool, p domain.Principal) *gin.Engine {
	t.Helper()
	svc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	h := site.NewHandler(svc, audit.NewRecorder(pool, domain.SystemClock{}), "")

	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	})
	v1 := engine.Group("/api/v1")
	h.Register(v1)
	return engine
}

func monitoringPrincipal(tenant, user uuid.UUID) domain.Principal {
	return domain.Principal{
		Type: domain.PrincipalUser, UserID: user, TenantID: tenant,
		Role: "owner", Scope: domain.ScopeOrg,
	}
}

func decodeMonitoring(t *testing.T, body []byte, code int) monitoringBulkResult {
	t.Helper()
	var out monitoringBulkResult
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode monitoring response (status %d, body %s): %v", code, string(body), err)
	}
	return out
}

// resultFor finds the result row for a site id, failing when absent — every
// requested id must appear exactly once.
func resultFor(t *testing.T, got monitoringBulkResult, id string) monitoringResult {
	t.Helper()
	var found *monitoringResult
	for i := range got.Results {
		if got.Results[i].SiteID == id {
			if found != nil {
				t.Fatalf("site %s appears more than once in results: %+v", id, got.Results)
			}
			found = &got.Results[i]
		}
	}
	if found == nil {
		t.Fatalf("site %s missing from results: %+v", id, got.Results)
	}
	return *found
}

// readPauseRow reads the persisted pause columns through the SAME tenant tx
// helper the request uses, as wpmgr_app, so RLS is in force for the read too.
func readPauseRow(t *testing.T, pool *db.Pool, tenant, siteID uuid.UUID) (pausedAt *time.Time, pausedBy *uuid.UUID, reason string, resumeAt *time.Time, found bool) {
	t.Helper()
	err := pool.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `
			SELECT monitoring_paused_at, monitoring_paused_by,
			       monitoring_paused_reason, monitoring_resume_at
			  FROM sites WHERE tenant_id = $1 AND id = $2`, tenant, siteID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			found = true
			if err := rows.Scan(&pausedAt, &pausedBy, &reason, &resumeAt); err != nil {
				return err
			}
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("read pause row: %v", err)
	}
	return pausedAt, pausedBy, reason, resumeAt, found
}

// countAuditFor counts audit_log rows for one action against ONE site's
// target_id — the query an operator auditing a single site actually runs. A
// request-level event carrying a site_ids array in its metadata is invisible
// here, which is the whole point of auditing per site.
func countAuditFor(t *testing.T, pool *db.Pool, tenant uuid.UUID, action, targetID string) int {
	t.Helper()
	n := 0
	err := pool.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `
			SELECT count(*) FROM audit_log
			 WHERE tenant_id = $1 AND action = $2
			   AND target_type = 'site' AND target_id = $3`,
			tenant, action, targetID).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

// seedMonitoringUser inserts a users row so monitoring_paused_by's FK to
// users(id) is satisfiable, and returns its id.
func seedMonitoringUser(t *testing.T, pool *db.Pool, email string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.InAgentTx(context.Background(), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(),
			`INSERT INTO users (id, email, name) VALUES ($1, $2, 'GH414 Tester')`, id, email)
		return err
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// TestMonitoringPauseRoutes_Phase1 covers the whole phase-1 contract in one
// container: pause one, pause many, a re-pause that must not overwrite the
// reason, resume, resume-when-already-active, a cross-tenant id, an empty
// list, a past resume_at, and one audit event PER SITE.
//
// One test function on purpose: startPostgres is the expensive part, and every
// subtest below asserts against independent site rows, so they neither share
// nor order-depend on each other's state.
func TestMonitoringPauseRoutes_Phase1(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "gh414-pause-"+uuid.NewString()[:8])
	actor := seedMonitoringUser(t, pool, "gh414-"+uuid.NewString()[:8]+"@example.com")
	engine := newMonitoringEngine(t, pool, monitoringPrincipal(tenant, actor))

	t.Run("pause one site persists paused_at, by, reason and resume_at", func(t *testing.T) {
		siteID := seedSite(t, pool, tenant, "https://gh414-one-"+uuid.NewString()[:8]+".example.com")
		resumeAt := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)

		w := doJSON(t, engine, http.MethodPost, "/api/v1/sites/monitoring/pause", map[string]any{
			"site_ids":  []string{siteID.String()},
			"reason":    "noisy staging box",
			"resume_at": resumeAt.Format(time.RFC3339),
		})
		if w.Code != http.StatusOK {
			t.Fatalf("pause = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		got := decodeMonitoring(t, w.Body.Bytes(), w.Code)
		if got.ChangedCount != 1 {
			t.Fatalf("changed_count = %d, want 1; body=%s", got.ChangedCount, w.Body.String())
		}
		res := resultFor(t, got, siteID.String())
		if !res.OK || !res.Changed || res.Detail != "paused" {
			t.Fatalf("result = %+v, want ok=true changed=true detail=paused", res)
		}
		if res.MonitoringPausedAt == nil {
			t.Fatal("response omitted monitoring_paused_at for a freshly paused site")
		}

		pausedAt, pausedBy, reason, storedResume, found := readPauseRow(t, pool, tenant, siteID)
		if !found {
			t.Fatal("site row missing after pause")
		}
		if pausedAt == nil {
			t.Fatal("monitoring_paused_at is NULL after a successful pause")
		}
		if reason != "noisy staging box" {
			t.Fatalf("monitoring_paused_reason = %q, want %q", reason, "noisy staging box")
		}
		// paused_by comes from the PRINCIPAL, never from request input: the FK
		// is to users(id) alone and the database cannot check that the
		// referenced user belongs to this site's tenant.
		if pausedBy == nil || *pausedBy != actor {
			t.Fatalf("monitoring_paused_by = %v, want the authenticated actor %v", pausedBy, actor)
		}
		if storedResume == nil || !storedResume.UTC().Truncate(time.Second).Equal(resumeAt) {
			t.Fatalf("monitoring_resume_at = %v, want %v", storedResume, resumeAt)
		}
	})

	t.Run("pause many sites pauses every one of them", func(t *testing.T) {
		ids := []uuid.UUID{
			seedSite(t, pool, tenant, "https://gh414-many-a-"+uuid.NewString()[:8]+".example.com"),
			seedSite(t, pool, tenant, "https://gh414-many-b-"+uuid.NewString()[:8]+".example.com"),
			seedSite(t, pool, tenant, "https://gh414-many-c-"+uuid.NewString()[:8]+".example.com"),
		}
		raw := make([]string, len(ids))
		for i, id := range ids {
			raw[i] = id.String()
		}
		w := doJSON(t, engine, http.MethodPost, "/api/v1/sites/monitoring/pause", map[string]any{
			"site_ids": raw, "reason": "fleet maintenance window",
		})
		if w.Code != http.StatusOK {
			t.Fatalf("bulk pause = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		got := decodeMonitoring(t, w.Body.Bytes(), w.Code)
		if got.ChangedCount != len(ids) {
			t.Fatalf("changed_count = %d, want %d; body=%s", got.ChangedCount, len(ids), w.Body.String())
		}
		for _, id := range ids {
			res := resultFor(t, got, id.String())
			if !res.OK || !res.Changed || res.Detail != "paused" {
				t.Fatalf("site %s result = %+v, want ok/changed/paused", id, res)
			}
			pausedAt, _, reason, _, _ := readPauseRow(t, pool, tenant, id)
			if pausedAt == nil {
				t.Fatalf("site %s: monitoring_paused_at is NULL after bulk pause", id)
			}
			if reason != "fleet maintenance window" {
				t.Fatalf("site %s: reason = %q", id, reason)
			}
		}
	})

	t.Run("re-pausing an already-paused site does NOT overwrite its reason or paused_at", func(t *testing.T) {
		siteID := seedSite(t, pool, tenant, "https://gh414-idem-"+uuid.NewString()[:8]+".example.com")
		firstResume := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)

		w := doJSON(t, engine, http.MethodPost, "/api/v1/sites/monitoring/pause", map[string]any{
			"site_ids": []string{siteID.String()},
			"reason":   "the reason someone actually typed",
			// A resume instant that the retry must also leave alone.
			"resume_at": firstResume.Format(time.RFC3339),
		})
		if w.Code != http.StatusOK {
			t.Fatalf("first pause = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		firstPausedAt, _, firstReason, firstStoredResume, _ := readPauseRow(t, pool, tenant, siteID)
		if firstPausedAt == nil {
			t.Fatal("first pause did not set monitoring_paused_at")
		}

		// The retry a normal client sends after a timeout, carrying a
		// DIFFERENT reason and resume_at. Both must be ignored.
		w = doJSON(t, engine, http.MethodPost, "/api/v1/sites/monitoring/pause", map[string]any{
			"site_ids":  []string{siteID.String()},
			"reason":    "CLOBBERED BY A RETRY",
			"resume_at": time.Now().Add(999 * time.Hour).UTC().Format(time.RFC3339),
		})
		if w.Code != http.StatusOK {
			t.Fatalf("re-pause = %d, want 200 (idempotent success); body=%s", w.Code, w.Body.String())
		}
		got := decodeMonitoring(t, w.Body.Bytes(), w.Code)
		if got.ChangedCount != 0 {
			t.Fatalf("changed_count = %d on a re-pause, want 0; body=%s", got.ChangedCount, w.Body.String())
		}
		res := resultFor(t, got, siteID.String())
		if !res.OK {
			t.Fatalf("re-pause result = %+v, want ok=true (idempotent, not an error)", res)
		}
		if res.Changed {
			t.Fatalf("re-pause reported changed=true; a retry must report changed=false: %+v", res)
		}
		if res.Detail != "already_paused" {
			t.Fatalf("re-pause detail = %q, want %q", res.Detail, "already_paused")
		}

		pausedAt, pausedBy, reason, storedResume, _ := readPauseRow(t, pool, tenant, siteID)
		if reason != firstReason {
			t.Fatalf("REGRESSION: a retry overwrote the stored reason: %q -> %q", firstReason, reason)
		}
		if pausedAt == nil || !pausedAt.Equal(*firstPausedAt) {
			t.Fatalf("REGRESSION: a retry moved monitoring_paused_at: %v -> %v", firstPausedAt, pausedAt)
		}
		if storedResume == nil || firstStoredResume == nil || !storedResume.Equal(*firstStoredResume) {
			t.Fatalf("REGRESSION: a retry moved monitoring_resume_at: %v -> %v", firstStoredResume, storedResume)
		}
		if pausedBy == nil || *pausedBy != actor {
			t.Fatalf("monitoring_paused_by = %v after retry, want %v", pausedBy, actor)
		}
	})

	t.Run("resume clears paused_at, by, reason and resume_at together", func(t *testing.T) {
		siteID := seedSite(t, pool, tenant, "https://gh414-resume-"+uuid.NewString()[:8]+".example.com")
		w := doJSON(t, engine, http.MethodPost, "/api/v1/sites/monitoring/pause", map[string]any{
			"site_ids":  []string{siteID.String()},
			"reason":    "before the migration",
			"resume_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
		})
		if w.Code != http.StatusOK {
			t.Fatalf("pause before resume = %d; body=%s", w.Code, w.Body.String())
		}

		w = doJSON(t, engine, http.MethodPost, "/api/v1/sites/monitoring/resume", map[string]any{
			"site_ids": []string{siteID.String()},
		})
		if w.Code != http.StatusOK {
			t.Fatalf("resume = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		got := decodeMonitoring(t, w.Body.Bytes(), w.Code)
		if got.ChangedCount != 1 {
			t.Fatalf("resume changed_count = %d, want 1; body=%s", got.ChangedCount, w.Body.String())
		}
		res := resultFor(t, got, siteID.String())
		if !res.OK || !res.Changed || res.Detail != "resumed" {
			t.Fatalf("resume result = %+v, want ok/changed/resumed", res)
		}

		// All four columns must clear in the same UPDATE: leaving
		// monitoring_resume_at behind would raise 23514 on
		// sites_monitoring_resume_requires_pause_check, so a green assertion
		// here is also proof the constraint was satisfied.
		pausedAt, pausedBy, reason, storedResume, _ := readPauseRow(t, pool, tenant, siteID)
		if pausedAt != nil {
			t.Fatalf("monitoring_paused_at = %v after resume, want NULL", pausedAt)
		}
		if pausedBy != nil {
			t.Fatalf("monitoring_paused_by = %v after resume, want NULL", pausedBy)
		}
		if reason != "" {
			t.Fatalf("monitoring_paused_reason = %q after resume, want empty", reason)
		}
		if storedResume != nil {
			t.Fatalf("monitoring_resume_at = %v after resume, want NULL", storedResume)
		}
	})

	t.Run("resuming an already-active site succeeds with changed=false", func(t *testing.T) {
		siteID := seedSite(t, pool, tenant, "https://gh414-noop-"+uuid.NewString()[:8]+".example.com")
		w := doJSON(t, engine, http.MethodPost, "/api/v1/sites/monitoring/resume", map[string]any{
			"site_ids": []string{siteID.String()},
		})
		if w.Code != http.StatusOK {
			t.Fatalf("resume of an active site = %d, want 200 (not a 409); body=%s", w.Code, w.Body.String())
		}
		got := decodeMonitoring(t, w.Body.Bytes(), w.Code)
		if got.ChangedCount != 0 {
			t.Fatalf("changed_count = %d, want 0; body=%s", got.ChangedCount, w.Body.String())
		}
		res := resultFor(t, got, siteID.String())
		if !res.OK || res.Changed || res.Detail != "already_active" {
			t.Fatalf("result = %+v, want ok=true changed=false detail=already_active", res)
		}
	})

	t.Run("a site_id from another tenant is never paused", func(t *testing.T) {
		otherTenant := seedTenant(t, pool, "gh414-other-"+uuid.NewString()[:8])
		foreignSite := seedSite(t, pool, otherTenant, "https://gh414-foreign-"+uuid.NewString()[:8]+".example.com")
		mine := seedSite(t, pool, tenant, "https://gh414-mine-"+uuid.NewString()[:8]+".example.com")

		w := doJSON(t, engine, http.MethodPost, "/api/v1/sites/monitoring/pause", map[string]any{
			"site_ids": []string{mine.String(), foreignSite.String()},
			"reason":   "cross-tenant attempt",
		})
		if w.Code != http.StatusOK {
			t.Fatalf("mixed-tenant pause = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		got := decodeMonitoring(t, w.Body.Bytes(), w.Code)
		if got.ChangedCount != 1 {
			t.Fatalf("changed_count = %d, want 1 (only the caller's own site); body=%s", got.ChangedCount, w.Body.String())
		}
		foreign := resultFor(t, got, foreignSite.String())
		if foreign.OK {
			t.Fatalf("CROSS-TENANT LEAK: foreign site reported ok=true: %+v", foreign)
		}
		// site_not_found, not a distinct "wrong tenant" code: telling the
		// caller which is which turns the route into an existence oracle.
		if foreign.Detail != "site_not_found" {
			t.Fatalf("foreign site detail = %q, want %q", foreign.Detail, "site_not_found")
		}
		// The authoritative check: the foreign row itself, read in ITS OWN
		// tenant scope, is still unpaused.
		pausedAt, _, _, _, found := readPauseRow(t, pool, otherTenant, foreignSite)
		if !found {
			t.Fatal("foreign site row vanished")
		}
		if pausedAt != nil {
			t.Fatalf("CROSS-TENANT WRITE: foreign site was paused at %v", pausedAt)
		}
		if mineRes := resultFor(t, got, mine.String()); !mineRes.OK || !mineRes.Changed {
			t.Fatalf("caller's own site was not paused: %+v", mineRes)
		}
	})

	t.Run("an empty site_ids list is a 422, not a silent success", func(t *testing.T) {
		for _, path := range []string{"/api/v1/sites/monitoring/pause", "/api/v1/sites/monitoring/resume"} {
			w := doJSON(t, engine, http.MethodPost, path, map[string]any{"site_ids": []string{}})
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("%s with empty site_ids = %d, want 422; body=%s", path, w.Code, w.Body.String())
			}
			var errBody struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if errBody.Code != "site_ids_required" {
				t.Fatalf("%s error code = %q, want %q", path, errBody.Code, "site_ids_required")
			}
		}
	})

	t.Run("a resume_at in the past is a validation error, not a pause that un-pauses", func(t *testing.T) {
		siteID := seedSite(t, pool, tenant, "https://gh414-past-"+uuid.NewString()[:8]+".example.com")
		w := doJSON(t, engine, http.MethodPost, "/api/v1/sites/monitoring/pause", map[string]any{
			"site_ids":  []string{siteID.String()},
			"reason":    "typo in the date",
			"resume_at": time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339),
		})
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("past resume_at = %d, want 422; body=%s", w.Code, w.Body.String())
		}
		var errBody struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil {
			t.Fatalf("decode error body: %v", err)
		}
		if errBody.Code != "resume_at_in_past" {
			t.Fatalf("error code = %q, want %q", errBody.Code, "resume_at_in_past")
		}
		// And nothing was written: a rejected request must not half-pause.
		pausedAt, _, _, _, _ := readPauseRow(t, pool, tenant, siteID)
		if pausedAt != nil {
			t.Fatalf("site was paused at %v despite the 422", pausedAt)
		}
	})

	t.Run("one audit event per site, findable by that site alone", func(t *testing.T) {
		a := seedSite(t, pool, tenant, "https://gh414-audit-a-"+uuid.NewString()[:8]+".example.com")
		b := seedSite(t, pool, tenant, "https://gh414-audit-b-"+uuid.NewString()[:8]+".example.com")

		w := doJSON(t, engine, http.MethodPost, "/api/v1/sites/monitoring/pause", map[string]any{
			"site_ids": []string{a.String(), b.String()}, "reason": "audited pause",
		})
		if w.Code != http.StatusOK {
			t.Fatalf("bulk pause = %d; body=%s", w.Code, w.Body.String())
		}
		for _, id := range []uuid.UUID{a, b} {
			if n := countAuditFor(t, pool, tenant, audit.ActionSiteMonitoringPaused, id.String()); n != 1 {
				t.Fatalf("site %s has %d %q audit rows, want exactly 1 — someone auditing this ONE site must find the bulk pause",
					id, n, audit.ActionSiteMonitoringPaused)
			}
		}

		// A retry writes NO second event: an accepted retry is not a second
		// operator action, and auditing it would misreport a timeout as one.
		w = doJSON(t, engine, http.MethodPost, "/api/v1/sites/monitoring/pause", map[string]any{
			"site_ids": []string{a.String(), b.String()}, "reason": "audited pause",
		})
		if w.Code != http.StatusOK {
			t.Fatalf("re-pause = %d; body=%s", w.Code, w.Body.String())
		}
		if n := countAuditFor(t, pool, tenant, audit.ActionSiteMonitoringPaused, a.String()); n != 1 {
			t.Fatalf("after an idempotent retry site %s has %d pause audit rows, want still 1", a, n)
		}

		// Resume writes its own per-site event under its own action.
		w = doJSON(t, engine, http.MethodPost, "/api/v1/sites/monitoring/resume", map[string]any{
			"site_ids": []string{a.String(), b.String()},
		})
		if w.Code != http.StatusOK {
			t.Fatalf("bulk resume = %d; body=%s", w.Code, w.Body.String())
		}
		for _, id := range []uuid.UUID{a, b} {
			if n := countAuditFor(t, pool, tenant, audit.ActionSiteMonitoringResumed, id.String()); n != 1 {
				t.Fatalf("site %s has %d %q audit rows, want exactly 1", id, n, audit.ActionSiteMonitoringResumed)
			}
		}
	})
}
