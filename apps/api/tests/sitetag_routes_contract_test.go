// sitetag_routes_contract_test.go — GH #230 "rich tags" (m100) anti-drift
// contract test: hits the REAL Gin routes (sitetag.Handler + site.Handler)
// via httptest, unmarshals the responses into the ogen-generated OpenAPI
// types, and asserts exact field presence + the documented error codes
// (tag_name_exists, invalid_tag, invalid_color). Also proves GET /api/v1/sites
// accepts repeated ?tags= plus ?tags_match=all end-to-end through the wire,
// and that bulk-apply's per-site allowlist gate (Principal.CanAccessSite)
// yields ok:false for a site outside a site-scoped collaborator's grant
// rather than a global 403. Requires Docker; skips when unavailable.
package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/sitetag"
)

// newTagRoutesEngine mounts both the tag-registry routes and the sites list
// route (for the tags/tags_match query-param contract) on a real Gin engine,
// with a middleware injecting the given principal exactly as production auth
// middleware would (no audit recorder — nil is tolerated).
func newTagRoutesEngine(pool *db.Pool, p domain.Principal) *gin.Engine {
	tagH := sitetag.NewHandler(sitetag.NewService(sitetag.NewRepo(pool)), nil)
	siteH := site.NewHandler(site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{}), nil, "")

	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	})
	v1 := engine.Group("/api/v1")
	tagH.Register(v1)
	siteH.Register(v1)
	return engine
}

func doJSON(t *testing.T, engine *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, path, reader)
	r.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, r)
	return w
}

func decodeInto(t *testing.T, w *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode response (status %d, body %s): %v", w.Code, w.Body.String(), err)
	}
}

func orgPrincipal(tenant uuid.UUID) domain.Principal {
	return domain.Principal{
		Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenant,
		Role: "owner", Scope: domain.ScopeOrg,
	}
}

// TestTagRoutes_CreateListShape_And_ErrorCodes proves the create/list wire
// shape matches the ogen-generated SiteTag/SiteTagList types exactly, and
// that the three documented error codes are returned with the right status.
func TestTagRoutes_CreateListShape_And_ErrorCodes(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "tagroutes-shape-"+uuid.NewString()[:8])
	engine := newTagRoutesEngine(pool, orgPrincipal(tenant))

	// Create.
	w := doJSON(t, engine, http.MethodPost, "/api/v1/tags", map[string]any{"name": "prod", "color": "#ABCDEF"})
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /tags = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var created gen.SiteTag
	decodeInto(t, w, &created)
	if created.ID == uuid.Nil {
		t.Fatal("created tag has zero ID")
	}
	if created.Name != "prod" {
		t.Fatalf("Name = %q, want %q", created.Name, "prod")
	}
	if created.Color != "#abcdef" {
		t.Fatalf("Color = %q, want lowercased %q", created.Color, "#abcdef")
	}
	if created.UsageCount != 0 {
		t.Fatalf("UsageCount = %d, want 0 for a freshly created tag", created.UsageCount)
	}
	if created.CreatedAt.IsZero() {
		t.Fatal("CreatedAt must be set")
	}

	// invalid_tag: blank name.
	w = doJSON(t, engine, http.MethodPost, "/api/v1/tags", map[string]any{"name": "   "})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST /tags blank name = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	var errBody gen.Error
	decodeInto(t, w, &errBody)
	if errBody.Code != "invalid_tag" {
		t.Fatalf("error code = %q, want invalid_tag", errBody.Code)
	}

	// invalid_color: malformed hex.
	w = doJSON(t, engine, http.MethodPost, "/api/v1/tags", map[string]any{"name": "qa", "color": "not-a-color"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST /tags bad color = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	decodeInto(t, w, &errBody)
	if errBody.Code != "invalid_color" {
		t.Fatalf("error code = %q, want invalid_color", errBody.Code)
	}

	// tag_name_exists: exact-case duplicate.
	w = doJSON(t, engine, http.MethodPost, "/api/v1/tags", map[string]any{"name": "prod"})
	if w.Code != http.StatusConflict {
		t.Fatalf("POST /tags duplicate = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	decodeInto(t, w, &errBody)
	if errBody.Code != "tag_name_exists" {
		t.Fatalf("error code = %q, want tag_name_exists", errBody.Code)
	}

	// List shape.
	w = doJSON(t, engine, http.MethodGet, "/api/v1/tags", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /tags = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var list gen.SiteTagList
	decodeInto(t, w, &list)
	if len(list.Items) != 1 {
		t.Fatalf("list has %d items, want 1", len(list.Items))
	}
	if list.Items[0].Name != "prod" {
		t.Fatalf("list item Name = %q, want prod", list.Items[0].Name)
	}
}

// TestTagRoutes_UpdateNotFound_DeleteNoContent covers the 404 + 204 shapes.
func TestTagRoutes_UpdateNotFound_DeleteNoContent(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "tagroutes-404-"+uuid.NewString()[:8])
	engine := newTagRoutesEngine(pool, orgPrincipal(tenant))

	missing := uuid.New()
	w := doJSON(t, engine, http.MethodPatch, "/api/v1/tags/"+missing.String(), map[string]any{"color": "#123456"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("PATCH missing tag = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	var errBody gen.Error
	decodeInto(t, w, &errBody)
	if errBody.Code != "tag_not_found" {
		t.Fatalf("error code = %q, want tag_not_found", errBody.Code)
	}

	w = doJSON(t, engine, http.MethodPost, "/api/v1/tags", map[string]any{"name": "deleteme"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d body=%s", w.Code, w.Body.String())
	}
	var created gen.SiteTag
	decodeInto(t, w, &created)

	w = doJSON(t, engine, http.MethodDelete, "/api/v1/tags/"+created.ID.String(), nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if w.Body.Len() != 0 {
		t.Fatalf("204 body must be empty, got %q", w.Body.String())
	}
}

// TestSitesRoute_RepeatedTagsParam_AnyAndAll proves GET /api/v1/sites accepts
// repeated ?tags=a&tags=b plus ?tags_match=all through the real wire (query
// string parsing, not the Go ListInput struct directly).
func TestSitesRoute_RepeatedTagsParam_AnyAndAll(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "sitesroute-tags-"+uuid.NewString()[:8])
	p := orgPrincipal(tenant)
	engine := newTagRoutesEngine(pool, p)

	siteSvc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	ctx := context.Background()
	sA, err := siteSvc.Create(ctx, site.CreateInput{TenantID: tenant, URL: "https://a.tagsroute.example.com", Name: "a"})
	if err != nil {
		t.Fatalf("create site a: %v", err)
	}
	sB, err := siteSvc.Create(ctx, site.CreateInput{TenantID: tenant, URL: "https://b.tagsroute.example.com", Name: "b"})
	if err != nil {
		t.Fatalf("create site b: %v", err)
	}
	if _, err := siteSvc.SetTags(ctx, site.SetTagsInput{TenantID: tenant, SiteID: sA.ID, Tags: []string{"prod", "eu"}}); err != nil {
		t.Fatalf("SetTags a: %v", err)
	}
	if _, err := siteSvc.SetTags(ctx, site.SetTagsInput{TenantID: tenant, SiteID: sB.ID, Tags: []string{"prod"}}); err != nil {
		t.Fatalf("SetTags b: %v", err)
	}

	// any (default): both sites carry "prod".
	w := doJSON(t, engine, http.MethodGet, "/api/v1/sites?tags=prod", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /sites?tags=prod = %d; body=%s", w.Code, w.Body.String())
	}
	var list gen.SiteList
	decodeInto(t, w, &list)
	if len(list.Items) != 2 {
		t.Fatalf("tags=prod (any, default): got %d items, want 2", len(list.Items))
	}

	// all: only site A carries BOTH prod and eu.
	w = doJSON(t, engine, http.MethodGet, "/api/v1/sites?tags=prod&tags=eu&tags_match=all", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /sites?tags=prod&tags=eu&tags_match=all = %d; body=%s", w.Code, w.Body.String())
	}
	decodeInto(t, w, &list)
	if len(list.Items) != 1 || list.Items[0].ID != sA.ID {
		t.Fatalf("tags_match=all: got %d items (want 1, site A); body=%s", len(list.Items), w.Body.String())
	}
}

// TestBulkApplyTags_UnauthorizedSiteExcluded_NotGlobal403 proves the
// per-site allowlist gate: a site-scoped collaborator's request that
// includes a site outside their grant gets ok:false for that site in the
// results — the whole call still succeeds (200), never a global 403.
func TestBulkApplyTags_UnauthorizedSiteExcluded_NotGlobal403(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "bulkapply-allowlist-"+uuid.NewString()[:8])

	siteSvc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	ctx := context.Background()
	granted, err := siteSvc.Create(ctx, site.CreateInput{TenantID: tenant, URL: "https://granted.bulkallow.example.com", Name: "granted"})
	if err != nil {
		t.Fatalf("create granted site: %v", err)
	}
	foreign, err := siteSvc.Create(ctx, site.CreateInput{TenantID: tenant, URL: "https://foreign.bulkallow.example.com", Name: "foreign"})
	if err != nil {
		t.Fatalf("create foreign site: %v", err)
	}

	scoped := domain.Principal{
		Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenant,
		Role: "operator", Scope: domain.ScopeSite, AllowedSiteIDs: []uuid.UUID{granted.ID},
	}
	engine := newTagRoutesEngine(pool, scoped)

	w := doJSON(t, engine, http.MethodPost, "/api/v1/tags/bulk-apply", map[string]any{
		"site_ids": []string{granted.ID.String(), foreign.ID.String()},
		"add":      []string{"beta"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("bulk-apply with a partially-unauthorized batch = %d, want 200 (never a global 403); body=%s", w.Code, w.Body.String())
	}
	var results gen.BulkResultList
	decodeInto(t, w, &results)
	if len(results.Results) != 2 {
		t.Fatalf("got %d results, want 2", len(results.Results))
	}
	byID := map[string]gen.BulkResult{}
	for _, r := range results.Results {
		byID[r.SiteID] = r
	}
	if !byID[granted.ID.String()].Ok {
		t.Fatalf("granted site must be ok:true, got %+v", byID[granted.ID.String()])
	}
	if byID[foreign.ID.String()].Ok {
		t.Fatalf("foreign (unauthorized) site must be ok:false, got %+v", byID[foreign.ID.String()])
	}
	if byID[foreign.ID.String()].Detail != "forbidden" {
		t.Fatalf("foreign site detail = %q, want forbidden", byID[foreign.ID.String()].Detail)
	}
}

// TestTagRoutes_ScopedCollaborator_Forbidden_OnMutations proves the m100
// security-review follow-up (MEDIUM #3): a site-scoped collaborator (one
// granted PermSiteWrite on a single site, not an org member) is blocked with
// 403 from POST /tags, PATCH /tags/:tagId, and DELETE /tags/:tagId — these
// mutate the tag registry FLEET-WIDE (rewriting sites.tags on every site in
// the tenant on rename/merge/delete), which must never be reachable via a
// single site grant. GET /tags and POST /tags/bulk-apply remain reachable
// (bulk-apply is itself per-site authorized — see
// TestBulkApplyTags_UnauthorizedSiteExcluded_NotGlobal403 above).
func TestTagRoutes_ScopedCollaborator_Forbidden_OnMutations(t *testing.T) {
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "tagroutes-scoped-"+uuid.NewString()[:8])

	// Seed one tag as an org operator so PATCH/DELETE have a real id to target.
	orgEngine := newTagRoutesEngine(pool, orgPrincipal(tenant))
	w := doJSON(t, orgEngine, http.MethodPost, "/api/v1/tags", map[string]any{"name": "seeded"})
	if w.Code != http.StatusCreated {
		t.Fatalf("seed tag (org): %d; body=%s", w.Code, w.Body.String())
	}
	var seeded gen.SiteTag
	decodeInto(t, w, &seeded)

	siteSvc := site.NewService(site.NewRepo(pool), domain.NewValidator(), domain.SystemClock{})
	granted, err := siteSvc.Create(context.Background(), site.CreateInput{TenantID: tenant, URL: "https://scoped-tags.example.com", Name: "granted"})
	if err != nil {
		t.Fatalf("create granted site: %v", err)
	}
	scoped := domain.Principal{
		Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenant,
		Role: "operator", Scope: domain.ScopeSite, AllowedSiteIDs: []uuid.UUID{granted.ID},
	}
	scopedEngine := newTagRoutesEngine(pool, scoped)

	w = doJSON(t, scopedEngine, http.MethodPost, "/api/v1/tags", map[string]any{"name": "scoped-should-fail"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("POST /tags as site-scoped collaborator = %d, want 403; body=%s", w.Code, w.Body.String())
	}

	w = doJSON(t, scopedEngine, http.MethodPatch, "/api/v1/tags/"+seeded.ID.String(), map[string]any{"color": "#123456"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("PATCH /tags/:tagId as site-scoped collaborator = %d, want 403; body=%s", w.Code, w.Body.String())
	}

	w = doJSON(t, scopedEngine, http.MethodDelete, "/api/v1/tags/"+seeded.ID.String(), nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("DELETE /tags/:tagId as site-scoped collaborator = %d, want 403; body=%s", w.Code, w.Body.String())
	}

	// GET /tags remains reachable (tenant-level metadata read).
	w = doJSON(t, scopedEngine, http.MethodGet, "/api/v1/tags", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /tags as site-scoped collaborator = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// The seeded tag must be untouched by the rejected mutations.
	w = doJSON(t, orgEngine, http.MethodGet, "/api/v1/tags", nil)
	var list gen.SiteTagList
	decodeInto(t, w, &list)
	if len(list.Items) != 1 || list.Items[0].Name != "seeded" || list.Items[0].Color != "" {
		t.Fatalf("tag registry after rejected scoped mutations = %+v, want unchanged [seeded, color:'']", list.Items)
	}
}
