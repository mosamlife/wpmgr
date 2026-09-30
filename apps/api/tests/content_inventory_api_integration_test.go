// Track B S1 API proofs, through the production path: the content handler and
// service over a wpmgr_app pool (RLS in force), and the admin routes behind
// the real requireSuperadmin gate.
//
// What is proven here and cannot be proven by the unit tests:
//   - the inventory of one tenant's site is invisible to another tenant, and
//     to a site-scoped principal not granted that site;
//   - page titles reach only a caller holding site.content.read;
//   - a second refresh request inside the window is rate limited;
//   - the allowlist writer and the fleet report refuse a non-superadmin, the
//     actor of a write is the session and never the body, and a body that
//     names an actor is refused with nothing written.
//
// Run one test at a time:
//
//	go test -run '^TestContentInventoryAPIIsolation$' ./tests/
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

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/admin"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/content"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// contentFakeProbe answers list mode with fixed rows.
type contentFakeProbe struct{}

func (contentFakeProbe) ContentProbeList(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.ContentProbeRequest) (agentcmd.ContentProbeListResponse, error) {
	title := func(s string) *string { return &s }
	ver := "3.21.0"
	return agentcmd.ContentProbeListResponse{
		OK: true, ProbeVersion: 2, Mode: "list",
		Rows: []agentcmd.ContentProbeRow{
			{Post: 10, Type: "page", Status: "publish", Verdict: "builder",
				Route: agentcmd.ContentProbeRoute{Number: 3, Reason: "builder_not_supported"},
				Owner: &agentcmd.ContentProbeOwner{IntegrationID: "elementor", Version: &ver}, Title: title("Spring sale")},
			{Post: 11, Type: "page", Status: "publish", Verdict: "classic",
				Route: agentcmd.ContentProbeRoute{Number: 1, Reason: "content_column"}, Title: title("About us")},
		},
	}, nil
}

type contentFakeEnq struct {
	mu   sync.Mutex
	seen map[uuid.UUID]bool
}

func (f *contentFakeEnq) EnqueueRefresh(_ context.Context, a content.RefreshArgs, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seen == nil {
		f.seen = map[uuid.UUID]bool{}
	}
	if f.seen[a.SiteID] {
		return false, nil
	}
	f.seen[a.SiteID] = true
	return true, nil
}

func contentConnect(t *testing.T, admin *db.Pool, site uuid.UUID) {
	t.Helper()
	if _, err := admin.Exec(context.Background(),
		`UPDATE sites SET agent_version = '0.61.154', connection_state = 'connected', enrolled_at = now() WHERE id = $1`, site); err != nil {
		t.Fatalf("connect site: %v", err)
	}
}

func contentEngine(t *testing.T, h *content.Handler, p domain.Principal) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	})
	h.Register(engine.Group("/api/v1"))
	return engine
}

func contentDo(engine *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	engine.ServeHTTP(w, req)
	return w
}

type contentPageBody struct {
	State           string `json:"state"`
	TitlesIncluded  bool   `json:"titles_included"`
	NextAfterPostID *int64 `json:"next_after_post_id"`
	Pages           []struct {
		PostID int64   `json:"post_id"`
		Title  *string `json:"title"`
		Editor *struct {
			IntegrationID string  `json:"integration_id"`
			DisplayName   *string `json:"display_name"`
		} `json:"editor"`
	} `json:"pages"`
}

func contentDecode(t *testing.T, w *httptest.ResponseRecorder) contentPageBody {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var out contentPageBody
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body.String())
	}
	return out
}

// Mutation, planted and watched: removing site_content_inventory_tenant_isolation
// from m153 fires "TENANCY LEAK"; removing the site_scope policy fires
// "SITE-SCOPE LEAK".
func TestContentInventoryAPIIsolation(t *testing.T) {
	ctx := context.Background()
	app := startPostgres(t)
	adm := connectAdmin(t, app)

	tenantA := seedTenant(t, app, "content-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, app, "content-b-"+uuid.NewString()[:8])
	siteA1 := seedSite(t, app, tenantA, "")
	siteA2 := seedSite(t, app, tenantA, "")
	siteB := seedSite(t, app, tenantB, "")
	for _, s := range []uuid.UUID{siteA1, siteA2, siteB} {
		contentConnect(t, adm, s)
	}

	repo := content.NewRepo(app)
	svc := content.NewService(repo, contentFakeProbe{}, nil)
	enq := &contentFakeEnq{}
	h := content.NewHandler(svc)
	h.SetEnqueuer(enq)

	// The refresh path stores through the production tenant transaction.
	res, err := svc.Refresh(ctx, tenantA, siteA1, false)
	if err != nil || res.Stored != 2 {
		t.Fatalf("refresh: %+v %v", res, err)
	}

	owner := domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenantA, Role: "owner", Scope: domain.ScopeOrg}
	viewer := owner
	viewer.Role = "viewer"

	path := "/api/v1/sites/" + siteA1.String() + "/content/inventory"

	// An operator-or-above caller sees titles and the editor's display name.
	page := contentDecode(t, contentDo(contentEngine(t, h, owner), http.MethodGet, path, ""))
	if page.State != "ok" || !page.TitlesIncluded || len(page.Pages) != 2 {
		t.Fatalf("owner page = %+v", page)
	}
	if page.Pages[0].Title == nil || *page.Pages[0].Title != "Spring sale" {
		t.Errorf("owner did not get the title: %+v", page.Pages[0])
	}
	if page.Pages[0].Editor == nil || page.Pages[0].Editor.IntegrationID != "elementor" ||
		page.Pages[0].Editor.DisplayName == nil || *page.Pages[0].Editor.DisplayName != "Elementor" {
		t.Errorf("editor = %+v (the display name must come from the allowlist)", page.Pages[0].Editor)
	}

	// A viewer reads the route facts but never a title.
	w := contentDo(contentEngine(t, h, viewer), http.MethodGet, path, "")
	vpage := contentDecode(t, w)
	if vpage.TitlesIncluded || len(vpage.Pages) != 2 {
		t.Fatalf("viewer page = %+v", vpage)
	}
	for _, p := range vpage.Pages {
		if p.Title != nil {
			t.Fatalf("TITLE LEAK: a viewer received a page title")
		}
	}
	if strings.Contains(w.Body.String(), "Spring sale") || strings.Contains(w.Body.String(), "About us") {
		t.Fatalf("TITLE LEAK: a title is in the viewer's response body")
	}

	// Filter by editor, and keyset paging by post id.
	if p := contentDecode(t, contentDo(contentEngine(t, h, owner), http.MethodGet, path+"?editor=elementor", "")); len(p.Pages) != 1 || p.Pages[0].PostID != 10 {
		t.Errorf("editor=elementor: %+v", p)
	}
	if p := contentDecode(t, contentDo(contentEngine(t, h, owner), http.MethodGet, path+"?editor=classic", "")); len(p.Pages) != 1 || p.Pages[0].PostID != 11 {
		t.Errorf("editor=classic: %+v", p)
	}
	first := contentDecode(t, contentDo(contentEngine(t, h, owner), http.MethodGet, path+"?limit=1", ""))
	if len(first.Pages) != 1 || first.NextAfterPostID == nil || *first.NextAfterPostID != 10 {
		t.Fatalf("first page = %+v", first)
	}
	second := contentDecode(t, contentDo(contentEngine(t, h, owner), http.MethodGet, path+"?limit=1&after_post_id=10", ""))
	if len(second.Pages) != 1 || second.Pages[0].PostID != 11 || second.NextAfterPostID != nil {
		t.Fatalf("second page = %+v", second)
	}
	if w := contentDo(contentEngine(t, h, owner), http.MethodGet, path+"?editor=Bad%20Value", ""); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("a malformed editor filter answered %d, want 422", w.Code)
	}

	// Tenant B asks for tenant A's site: not found, and nothing of it.
	ownerB := domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenantB, Role: "owner", Scope: domain.ScopeOrg}
	wb := contentDo(contentEngine(t, h, ownerB), http.MethodGet, path, "")
	if wb.Code != http.StatusNotFound {
		t.Fatalf("TENANCY LEAK: tenant B reading tenant A's site inventory answered %d: %s", wb.Code, wb.Body.String())
	}
	if strings.Contains(wb.Body.String(), "Spring sale") {
		t.Fatalf("TENANCY LEAK: tenant A's title is in tenant B's response")
	}
	// The same read at the repo, as tenant B naming tenant A's ids, returns nothing.
	rows, err := repo.ListInventory(ctx, ownerB, siteA1, 0, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("TENANCY LEAK: repo.ListInventory as tenant B returned %d rows of tenant A's site", len(rows))
	}
	// Tenant B's own site is fine: the agent update needed state is not for it (0.61.154 meets the floor).
	if p := contentDecode(t, contentDo(contentEngine(t, h, ownerB), http.MethodGet, "/api/v1/sites/"+siteB.String()+"/content/inventory", "")); len(p.Pages) != 0 || p.State != "ok" {
		t.Errorf("tenant B's own empty site = %+v", p)
	}

	// A site-scoped collaborator granted only siteA2 cannot reach siteA1 at the
	// route (RequireSiteAccess) or at the row level (the RESTRICTIVE policy).
	scoped := domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenantA, Role: "operator", Scope: domain.ScopeSite, AllowedSiteIDs: []uuid.UUID{siteA2}}
	if w := contentDo(contentEngine(t, h, scoped), http.MethodGet, path, ""); w.Code != http.StatusNotFound {
		t.Fatalf("SITE-SCOPE LEAK: a collaborator without siteA1 got %d from the route", w.Code)
	}
	srows, err := repo.ListInventory(ctx, scoped, siteA1, 0, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(srows) != 0 {
		t.Fatalf("SITE-SCOPE LEAK: repo.ListInventory as a collaborator without siteA1 returned %d rows", len(srows))
	}
	granted := scoped
	granted.AllowedSiteIDs = []uuid.UUID{siteA1}
	if p := contentDecode(t, contentDo(contentEngine(t, h, granted), http.MethodGet, path, "")); len(p.Pages) != 2 {
		t.Errorf("a collaborator granted siteA1 sees %d pages, want 2", len(p.Pages))
	}

	// The test really ran as the app role, with RLS.
	if err := app.InTenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (content isolation)")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Refresh: operator+ only, and rate limited per site.
	refresh := "/api/v1/sites/" + siteA1.String() + "/content/inventory/refresh"
	if w := contentDo(contentEngine(t, h, viewer), http.MethodPost, refresh, ""); w.Code != http.StatusForbidden {
		t.Errorf("a viewer's refresh answered %d, want 403", w.Code)
	}
	if w := contentDo(contentEngine(t, h, ownerB), http.MethodPost, refresh, ""); w.Code != http.StatusNotFound {
		t.Errorf("TENANCY LEAK: tenant B queued a refresh for tenant A's site: %d", w.Code)
	}
	if enq.seen[siteA1] {
		t.Fatalf("TENANCY LEAK: a refresh was queued for a site the caller does not own")
	}
	if w := contentDo(contentEngine(t, h, owner), http.MethodPost, refresh, ""); w.Code != http.StatusAccepted {
		t.Fatalf("first refresh answered %d: %s", w.Code, w.Body.String())
	}
	w2 := contentDo(contentEngine(t, h, owner), http.MethodPost, refresh, "")
	if w2.Code != http.StatusTooManyRequests || w2.Header().Get("Retry-After") == "" {
		t.Errorf("second refresh answered %d (Retry-After %q), want 429 with Retry-After", w2.Code, w2.Header().Get("Retry-After"))
	}

	// A site below the agent floor reads as "agent update needed", not an error.
	if _, err := adm.Exec(ctx, `UPDATE sites SET agent_version = '0.61.153' WHERE id = $1`, siteA2); err != nil {
		t.Fatal(err)
	}
	if p := contentDecode(t, contentDo(contentEngine(t, h, owner), http.MethodGet, "/api/v1/sites/"+siteA2.String()+"/content/inventory", "")); p.State != "agent_update_needed" || len(p.Pages) != 0 {
		t.Errorf("below the floor: %+v", p)
	}
}

func contentAdminEngine(t *testing.T, app *db.Pool, h *content.Handler, p domain.Principal) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ah := admin.NewHandler(nil, app)
	ah.SetContentRoutes(h.RegisterAdmin)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	})
	ah.Register(engine.Group("/api/v1"))
	return engine
}

// Mutation, planted and watched: mounting the content routes on a group
// outside requireSuperadmin fires "GATE LEAK"; taking the actor from the body
// fires "ACTOR FROM BODY".
func TestContentAdminGateAndActor(t *testing.T) {
	ctx := context.Background()
	app := startPostgres(t)
	adm := connectAdmin(t, app)
	tenant := seedTenant(t, app, "content-admin-"+uuid.NewString()[:8])
	site := seedSite(t, app, tenant, "")
	contentConnect(t, adm, site)

	svc := content.NewService(content.NewRepo(app), contentFakeProbe{}, nil)
	h := content.NewHandler(svc)
	if _, err := svc.Refresh(ctx, tenant, site, false); err != nil {
		t.Fatal(err)
	}

	regular := seedUserRow(t, adm, "regular-"+uuid.NewString()[:8]+"@example.com")
	root := seedUserRow(t, adm, "root-"+uuid.NewString()[:8]+"@example.com")
	other := seedUserRow(t, adm, "other-"+uuid.NewString()[:8]+"@example.com")
	markSuperadmin(t, adm, root)
	markSuperadmin(t, adm, other)

	pReg := domain.Principal{Type: domain.PrincipalUser, UserID: regular, TenantID: tenant, Role: "owner", Scope: domain.ScopeOrg}
	pRoot := domain.Principal{Type: domain.PrincipalUser, UserID: root, TenantID: tenant, Role: "owner", Scope: domain.ScopeOrg}

	put := "/api/v1/admin/content/integrations/test-builder"
	body := `{"display_name":"Test Builder","enabled":true,"status":"detect_only","descriptor":{"mode_flag":{"meta_key":"_tb_on","on_values":["1"]},"payload_keys":["_tb_data"]}}`

	auditCount := func() int {
		var n int
		if err := adm.QueryRow(ctx, `SELECT count(*) FROM content_integrations_audit WHERE integration_id = 'test-builder'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// A non-superadmin is refused on every content admin route, and writes nothing.
	engReg := contentAdminEngine(t, app, h, pReg)
	for _, r := range []struct{ method, path, body string }{
		{http.MethodPut, put, body},
		{http.MethodGet, "/api/v1/admin/content/integrations", ""},
		{http.MethodGet, "/api/v1/admin/content/fleet-report", ""},
	} {
		w := contentDo(engReg, r.method, r.path, r.body)
		if w.Code != http.StatusForbidden {
			t.Fatalf("GATE LEAK: a non-superadmin got %d from %s %s: %s", w.Code, r.method, r.path, w.Body.String())
		}
	}
	if n := auditCount(); n != 0 {
		t.Fatalf("GATE LEAK: %d audit rows after a refused non-superadmin write", n)
	}

	engRoot := contentAdminEngine(t, app, h, pRoot)

	// A body that names an actor is refused and nothing is written.
	spoofed := strings.TrimSuffix(body, "}") + `,"actor_user_id":"` + other.String() + `"}`
	if w := contentDo(engRoot, http.MethodPut, put, spoofed); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("ACTOR FROM BODY: a body naming an actor answered %d, want 422: %s", w.Code, w.Body.String())
	}
	if n := auditCount(); n != 0 {
		t.Fatalf("ACTOR FROM BODY: %d audit rows after a refused body", n)
	}

	// The honest write: the actor is the session, the hash is server-computed.
	w := contentDo(engRoot, http.MethodPut, put, body)
	if w.Code != http.StatusOK {
		t.Fatalf("superadmin write answered %d: %s", w.Code, w.Body.String())
	}
	var rec struct {
		IntegrationID          string  `json:"integration_id"`
		IntegrationEntrySHA256 *string `json:"integration_entry_sha256"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.IntegrationID != "test-builder" || rec.IntegrationEntrySHA256 == nil || len(*rec.IntegrationEntrySHA256) != 64 {
		t.Fatalf("stored record = %s", w.Body.String())
	}
	var actor uuid.UUID
	if err := adm.QueryRow(ctx, `SELECT actor_user_id FROM content_integrations_audit WHERE integration_id = 'test-builder'`).Scan(&actor); err != nil {
		t.Fatalf("no audit row after a superadmin write: %v", err)
	}
	if actor != root {
		t.Fatalf("ACTOR FROM BODY: the audit names %s, want the session user %s", actor, root)
	}

	// Refused values never reach the database.
	for name, b := range map[string]string{
		"admitted":    `{"display_name":"X","enabled":true,"status":"admitted","descriptor":{}}`,
		"callable":    `{"display_name":"X","enabled":true,"status":"detect_only","descriptor":{"callback":"evil"}}`,
		"blank name":  `{"display_name":"  ","enabled":true,"status":"detect_only","descriptor":{}}`,
		"unknown key": `{"display_name":"X","enabled":true,"status":"detect_only","extra":1}`,
	} {
		if w := contentDo(engRoot, http.MethodPut, put, b); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s answered %d, want 422: %s", name, w.Code, w.Body.String())
		}
	}
	if w := contentDo(engRoot, http.MethodPut, "/api/v1/admin/content/integrations/Bad_Id", body); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("a malformed integration id answered %d, want 422", w.Code)
	}

	// Fleet report: counts across tenants, and no title or tenant id in it.
	wf := contentDo(engRoot, http.MethodGet, "/api/v1/admin/content/fleet-report", "")
	if wf.Code != http.StatusOK {
		t.Fatalf("fleet report answered %d: %s", wf.Code, wf.Body.String())
	}
	var fleet struct {
		Pages     int64 `json:"pages"`
		ByVerdict []struct {
			Verdict string `json:"verdict"`
			Pages   int64  `json:"pages"`
			Sites   int64  `json:"sites"`
		} `json:"by_verdict"`
		ByBuilder []struct {
			IntegrationID string `json:"integration_id"`
			Pages         int64  `json:"pages"`
		} `json:"by_builder"`
	}
	if err := json.Unmarshal(wf.Body.Bytes(), &fleet); err != nil {
		t.Fatal(err)
	}
	if fleet.Pages < 2 || len(fleet.ByVerdict) < 2 || len(fleet.ByBuilder) < 1 {
		t.Errorf("fleet report = %s", wf.Body.String())
	}
	if strings.Contains(wf.Body.String(), "Spring sale") || strings.Contains(wf.Body.String(), tenant.String()) {
		t.Errorf("fleet report carries a title or a tenant id: %s", wf.Body.String())
	}
}
