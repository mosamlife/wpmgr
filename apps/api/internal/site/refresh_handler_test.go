package site

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// fakeRefresher records the last EnqueueRefresh call and lets a test inject an
// error to verify the handler maps it. Mirrors the production siteRefreshAdapter.
type fakeRefresher struct {
	calls   int
	tenant  uuid.UUID
	siteID  uuid.UUID
	siteURL string
	source  string
	err     error
}

func (f *fakeRefresher) EnqueueRefresh(_ context.Context, tenantID, siteID uuid.UUID, siteURL, source string) error {
	f.calls++
	f.tenant = tenantID
	f.siteID = siteID
	f.siteURL = siteURL
	f.source = source
	return f.err
}

// freshRepo returns sites with a heartbeat configurable from the test so the
// handler's staleness check can be exercised without a real DB.
type freshRepo struct {
	fakeRepo
	site     Site
	enrolled bool
	lastSeen *time.Time
}

func (r *freshRepo) Get(_ context.Context, tenantID, id uuid.UUID) (Site, error) {
	s := r.site
	s.TenantID = tenantID
	s.ID = id
	if r.enrolled {
		t := time.Now().Add(-time.Hour)
		s.EnrolledAt = &t
	}
	s.LastSeenAt = r.lastSeen
	return s, nil
}

// buildRefreshEngine wires a gin engine that injects the tenantID on the
// request context (mirrors the auth middleware in the real server) and mounts
// the handler under /api/v1. The test calls r.ServeHTTP — that path drives gin
// through its full middleware/finalize chain, which writes headers to the
// recorder even when gen.Error fails to JSON-encode under its current ogen
// shape (a known minor quirk; status code is what we assert).
func buildRefreshEngine(h *Handler, tenantID uuid.UUID) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithTenantID(c.Request.Context(), tenantID))
		c.Next()
	})
	// Mount without the authz wrapper (we're not testing authz here; the
	// real engine layers PermSiteRead in front).
	r.POST("/api/v1/sites/:siteId/updates/refresh", h.refreshUpdates)
	r.GET("/api/v1/sites/:siteId/updates/available", h.getAvailableUpdates)
	return r
}

func newHandlerWithRefresher(repo Repo, refresher RefreshEnqueuer, staleAfter time.Duration, now time.Time) *Handler {
	svc := NewService(repo, domain.NewValidator(), domain.SystemClock{})
	h := NewHandler(svc, nil, "") // nil audit recorder ⇒ record() no-ops
	h.SetRefreshEnqueuer(refresher, staleAfter)
	h.SetClock(func() time.Time { return now })
	return h
}

func doRefresh(r *gin.Engine, siteID uuid.UUID) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sites/"+siteID.String()+"/updates/refresh", nil)
	r.ServeHTTP(rec, req)
	return rec
}

func TestRefreshUpdatesHappyPath202(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	now := time.Now()
	seen := now.Add(-30 * time.Second)
	repo := &freshRepo{enrolled: true, lastSeen: &seen, site: Site{URL: "https://example.com"}}
	refresher := &fakeRefresher{}

	h := newHandlerWithRefresher(repo, refresher, 5*time.Minute, now)
	rec := doRefresh(buildRefreshEngine(h, tenantID), siteID)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if refresher.calls != 1 || refresher.siteID != siteID || refresher.source != "api" {
		t.Fatalf("refresher not invoked: %+v", refresher)
	}
	if refresher.siteURL != "https://example.com" {
		t.Fatalf("siteURL not forwarded: %q", refresher.siteURL)
	}
}

func TestRefreshUpdatesUnenrolled409(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := &freshRepo{enrolled: false}
	refresher := &fakeRefresher{}

	h := newHandlerWithRefresher(repo, refresher, 5*time.Minute, time.Now())
	rec := doRefresh(buildRefreshEngine(h, tenantID), siteID)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if refresher.calls != 0 {
		t.Fatalf("unenrolled site must NOT enqueue refresh")
	}
}

func TestRefreshUpdatesStaleHeartbeat409(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	now := time.Now()
	seen := now.Add(-10 * time.Minute) // beyond the 5m stale window
	repo := &freshRepo{enrolled: true, lastSeen: &seen, site: Site{URL: "https://example.com"}}
	refresher := &fakeRefresher{}

	h := newHandlerWithRefresher(repo, refresher, 5*time.Minute, now)
	rec := doRefresh(buildRefreshEngine(h, tenantID), siteID)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if refresher.calls != 0 {
		t.Fatalf("stale site must NOT enqueue refresh")
	}
}

func TestRefreshUpdatesNoRefresherReturns500(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := &freshRepo{enrolled: true}
	svc := NewService(repo, domain.NewValidator(), domain.SystemClock{})
	h := NewHandler(svc, nil, "")
	// No SetRefreshEnqueuer call ⇒ disabled; handler returns 500/refresh_disabled.
	rec := doRefresh(buildRefreshEngine(h, tenantID), siteID)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestGetAvailableUpdatesHandlerReturns200(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	inv := map[string]any{
		"plugins": []map[string]any{
			{"slug": "wp-rocket", "name": "WP Rocket", "version": "3.16.1", "active": true,
				"available_update": map[string]any{"new_version": "3.16.2"}},
		},
		"core_update": map[string]any{"new_version": "6.5.2", "current_version": "6.4.3"},
	}
	raw, _ := json.Marshal(inv)
	repo := &freshRepo{site: Site{Components: raw, UpdatedAt: time.Now()}}
	svc := NewService(repo, domain.NewValidator(), domain.SystemClock{})
	h := NewHandler(svc, nil, "")

	r := buildRefreshEngine(h, tenantID)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/sites/"+siteID.String()+"/updates/available", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "wp-rocket") {
		t.Fatalf("response body should list wp-rocket: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "core_update") {
		t.Fatalf("response body should carry core_update: %s", rec.Body.String())
	}
}
