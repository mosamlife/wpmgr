// AI readiness API proofs, through the production path: the readiness handler
// registered on a gin group named /api/v1 the way server.go mounts it, its
// service and Postgres repo over a wpmgr_app pool (RLS in force), and the real
// audit recorder. Only the two enqueuers are fakes, so a queued job can be
// counted.
//
// What is proven here and cannot be proven by the unit tests:
//   - another tenant's GET and POST on a site answer 404 site_not_found, queue
//     nothing and write no audit row, while the same POST from the owning
//     tenant queues both reads and writes one;
//   - a site collaborator gets 404 for a site outside its scope, its fleet
//     holds only its own site, and that site reads exactly as an org member
//     reads it: the site-scope policies on the inventory tables the facts
//     join do not strip the collaborator's own rows;
//   - a viewer reads the checklist but cannot ask the site to report again.
//
// The query-level proofs of the same tables are TestAIReadinessQueriesAsAppRole;
// its seeding helpers are reused here.
//
// Run one test at a time:
//
//	go test -count=1 -run '^TestAIReadinessAPIIsolation$' ./tests/
package tests

import (
	"bytes"
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

	"github.com/mosamlife/wpmgr/apps/api/internal/aireadiness"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
)

// rdyAPIEnqueued records the site of every metadata refresh and tool-list
// read the service asks for.
type rdyAPIEnqueued struct {
	mu        sync.Mutex
	metadata  []uuid.UUID
	inventory []uuid.UUID
}

func (e *rdyAPIEnqueued) EnqueueRefresh(_ context.Context, _, siteID uuid.UUID, _, _ string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.metadata = append(e.metadata, siteID)
	return nil
}

func (e *rdyAPIEnqueued) EnqueueInventoryRefresh(_ context.Context, _, siteID uuid.UUID) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inventory = append(e.inventory, siteID)
	return true, nil
}

func (e *rdyAPIEnqueued) snapshot() (metadata, inventory []uuid.UUID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]uuid.UUID(nil), e.metadata...), append([]uuid.UUID(nil), e.inventory...)
}

func rdyAPIEngine(t *testing.T, h *aireadiness.Handler, p domain.Principal) *gin.Engine {
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

// rdyAPIDo sends one request. A POST carries an empty JSON object with the
// JSON media type, as the dashboard sends it.
func rdyAPIDo(t *testing.T, h *aireadiness.Handler, p domain.Principal, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if method == http.MethodPost {
		req = httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	rdyAPIEngine(t, h, p).ServeHTTP(w, req)
	return w
}

func rdyAPIWantError(t *testing.T, label string, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != status || body.Code != code {
		t.Fatalf("%s: answered %d %q, want %d %q (body %s)", label, w.Code, body.Code, status, code, w.Body.String())
	}
}

type rdyAPISite struct {
	SiteID        uuid.UUID  `json:"site_id"`
	Status        string     `json:"status"`
	FixCount      int        `json:"fix_count"`
	AbilitiesAsOf *time.Time `json:"abilities_as_of"`
	Warnings      []struct {
		Code string `json:"code"`
	} `json:"warnings"`
}

func rdyAPIDecodeSite(t *testing.T, label string, w *httptest.ResponseRecorder) rdyAPISite {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("%s: answered %d, want 200 (body %s)", label, w.Code, w.Body.String())
	}
	var out rdyAPISite
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: decode: %v (body %s)", label, err, w.Body.String())
	}
	return out
}

type rdyAPIFleetRow struct {
	SiteID   uuid.UUID `json:"site_id"`
	Status   string    `json:"status"`
	FixCount int       `json:"fix_count"`
	Failing  []string  `json:"failing"`
	Warnings []string  `json:"warnings"`
}

func rdyAPIDecodeFleet(t *testing.T, label string, w *httptest.ResponseRecorder) map[uuid.UUID]rdyAPIFleetRow {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("%s: answered %d, want 200 (body %s)", label, w.Code, w.Body.String())
	}
	var body struct {
		Sites []rdyAPIFleetRow `json:"sites"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: decode: %v (body %s)", label, err, w.Body.String())
	}
	out := make(map[uuid.UUID]rdyAPIFleetRow, len(body.Sites))
	for _, r := range body.Sites {
		if _, dup := out[r.SiteID]; dup {
			t.Fatalf("%s: site %s listed twice", label, r.SiteID)
		}
		out[r.SiteID] = r
	}
	return out
}

// rdyAPIAuditRows counts refresh audit rows in every tenant. It reads through
// the superuser pool on purpose: a row written under the wrong tenant must be
// counted too.
func rdyAPIAuditRows(t *testing.T, adm *db.Pool) int {
	t.Helper()
	var n int
	if err := adm.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE action = $1`, audit.ActionAIReadinessRefreshRequested).Scan(&n); err != nil {
		t.Fatalf("count refresh audit rows: %v", err)
	}
	return n
}

func TestAIReadinessAPIIsolation(t *testing.T) {
	ctx := context.Background()
	app := startPostgres(t)
	adm := connectAdmin(t, app)
	defer adm.Close()

	// Every row of the checklist passes for this site, and two of them only
	// when the reader sees both the site's inventory run and the ability
	// Elementor registered: without the run the AI tools row is unknown, and
	// without the ability it fails.
	fl := aireadiness.DefaultFloors()
	agent := fl.Agent
	for _, v := range []string{fl.FactsAgent, fl.EngineAgent} {
		if wpversion.Compare(v, agent) > 0 {
			agent = v
		}
	}
	ready := rdySiteSeed{
		enrolled: true, state: "connected", wp: fl.WP, agent: agent, editing: true,
		components: `{"plugins":[{"slug":"elementor/elementor.php","name":"x","version":"` + fl.Elementor + `","active":true}],` +
			`"themes":[],"builder_facts":{"v":1,"elementor":{"atomic_editor":true}}}`,
	}
	elementorAbility := rdyAbility{"elementor/build-page", "plugin", "elementor", "true"}

	tenantA := seedTenant(t, app, "rdy-api-a-"+uuid.NewString()[:8])
	tenantB := seedTenant(t, app, "rdy-api-b-"+uuid.NewString()[:8])
	siteX := rdySeedSite(t, adm, tenantA, ready)
	siteY := rdySeedSite(t, adm, tenantA, ready)
	siteB := rdySeedSite(t, adm, tenantB, ready)
	rdySeedInventory(t, app, tenantA, siteX, elementorAbility)
	rdySeedInventory(t, app, tenantA, siteY, elementorAbility)
	rdySeedInventory(t, app, tenantB, siteB, elementorAbility)

	enq := &rdyAPIEnqueued{}
	svc := aireadiness.NewService(aireadiness.NewRepo(app), audit.NewRecorder(app, domain.SystemClock{}), nil)
	// Zero turns off the heartbeat gate: the seeded sites never sent one.
	svc.SetRefreshers(enq, enq, 0)
	h := aireadiness.NewHandler(svc)

	ownerA := domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenantA, Role: "owner", Scope: domain.ScopeOrg}
	viewerA := ownerA
	viewerA.UserID, viewerA.Role = uuid.New(), "viewer"
	ownerB := domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenantB, Role: "owner", Scope: domain.ScopeOrg}
	collabX := domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenantA, Role: "operator",
		Scope: domain.ScopeSite, AllowedSiteIDs: []uuid.UUID{siteX}}

	readiness := func(site uuid.UUID) string { return "/api/v1/sites/" + site.String() + "/ai/readiness" }
	refresh := func(site uuid.UUID) string { return "/api/v1/sites/" + site.String() + "/ai/readiness/refresh" }
	const fleet = "/api/v1/fleet/ai-readiness"

	// wantQueued holds the running totals: n metadata refreshes, n tool-list
	// reads and n refresh audit rows so far.
	wantQueued := func(label string, n int) {
		t.Helper()
		metadata, inventory := enq.snapshot()
		if len(metadata) != n || len(inventory) != n {
			t.Fatalf("%s: metadata refreshes %v, tool-list reads %v, want %d of each", label, metadata, inventory, n)
		}
		if got := rdyAPIAuditRows(t, adm); got != n {
			t.Fatalf("%s: %d refresh audit rows, want %d", label, got, n)
		}
	}

	// The reads really run as the app role, with RLS, on both dispatch paths
	// the requests below take.
	for label, p := range map[string]domain.Principal{"tenant B owner": ownerB, "collaborator on X": collabX} {
		if err := app.RunTenantTx(ctx, p, func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "RunTenantTx ("+label+")")
			return nil
		}); err != nil {
			t.Fatalf("role check (%s): %v", label, err)
		}
	}

	// Tenant B names tenant A's site: not found on both routes, nothing
	// queued, nothing audited, and nothing of the site in the answer.
	for _, req := range []struct{ method, path string }{
		{http.MethodGet, readiness(siteX)},
		{http.MethodPost, refresh(siteX)},
	} {
		w := rdyAPIDo(t, h, ownerB, req.method, req.path)
		rdyAPIWantError(t, "TENANCY LEAK: tenant B "+req.method+" on tenant A's site", w, http.StatusNotFound, "site_not_found")
		if strings.Contains(w.Body.String(), siteX.String()) {
			t.Fatalf("TENANCY LEAK: tenant A's site id is in tenant B's %s answer: %s", req.method, w.Body.String())
		}
	}
	wantQueued("TENANCY LEAK: tenant B's refresh of tenant A's site", 0)
	if rows := rdyAPIDecodeFleet(t, "tenant B fleet", rdyAPIDo(t, h, ownerB, http.MethodGet, fleet)); len(rows) != 1 || rows[siteB].SiteID != siteB {
		t.Fatalf("TENANCY LEAK: tenant B's fleet = %+v, want its own site %s only", rows, siteB)
	}
	// Over-firing control: tenant B reads its own site.
	if got := rdyAPIDecodeSite(t, "tenant B own site", rdyAPIDo(t, h, ownerB, http.MethodGet, readiness(siteB))); got.SiteID != siteB {
		t.Fatalf("tenant B's own site answered site_id %s, want %s", got.SiteID, siteB)
	}

	// Positive control: the owning tenant reads the site, and the premise of
	// the collaborator comparison below holds: the site is ready, and its AI
	// tools row passed (the warning is raised only then).
	ownerGet := rdyAPIDo(t, h, ownerA, http.MethodGet, readiness(siteX))
	ownerX := rdyAPIDecodeSite(t, "tenant A owner on X", ownerGet)
	if ownerX.SiteID != siteX {
		t.Fatalf("tenant A's owner got site_id %s, want %s", ownerX.SiteID, siteX)
	}
	if ownerX.Status != "ready" || ownerX.FixCount != 0 || ownerX.AbilitiesAsOf == nil ||
		len(ownerX.Warnings) != 1 || ownerX.Warnings[0].Code != "elementor_mcp_endpoint_open" {
		t.Fatalf("PREMISE LOST: the owner's view of X is %+v, want ready, 0 fixes, an inventory time and the "+
			"elementor_mcp_endpoint_open warning; the facts comparison below would prove nothing (body %s)",
			ownerX, ownerGet.Body.String())
	}
	ownerFleet := rdyAPIDecodeFleet(t, "tenant A owner fleet", rdyAPIDo(t, h, ownerA, http.MethodGet, fleet))
	if len(ownerFleet) != 2 || ownerFleet[siteX].SiteID != siteX || ownerFleet[siteY].SiteID != siteY {
		t.Fatalf("PREMISE LOST: tenant A's owner fleet = %+v, want X %s and Y %s; "+
			"the collaborator's fleet below would prove nothing", ownerFleet, siteX, siteY)
	}

	// The same refresh from the owning tenant is accepted, queues both reads
	// and writes one audit row, so the zero counts above are not a counter
	// that cannot move.
	if w := rdyAPIDo(t, h, ownerA, http.MethodPost, refresh(siteX)); w.Code != http.StatusAccepted {
		t.Fatalf("tenant A owner's refresh answered %d, want 202 (body %s)", w.Code, w.Body.String())
	}
	wantQueued("tenant A owner's refresh", 1)
	if metadata, _ := enq.snapshot(); metadata[0] != siteX {
		t.Fatalf("tenant A owner's refresh queued site %s, want %s", metadata[0], siteX)
	}

	// A collaborator scoped to X: Y is not found on either route and nothing
	// is queued for it.
	rdyAPIWantError(t, "SITE-SCOPE LEAK: collaborator GET on Y", rdyAPIDo(t, h, collabX, http.MethodGet, readiness(siteY)),
		http.StatusNotFound, "site_not_found")
	rdyAPIWantError(t, "SITE-SCOPE LEAK: collaborator POST on Y", rdyAPIDo(t, h, collabX, http.MethodPost, refresh(siteY)),
		http.StatusNotFound, "site_not_found")
	wantQueued("SITE-SCOPE LEAK: the collaborator's refresh of Y", 1)

	// Its fleet is X alone, and X's row is the owner's row.
	collabFleet := rdyAPIDecodeFleet(t, "collaborator fleet", rdyAPIDo(t, h, collabX, http.MethodGet, fleet))
	if _, leaked := collabFleet[siteY]; leaked {
		t.Fatalf("SITE-SCOPE LEAK: the collaborator's fleet holds Y %s", siteY)
	}
	if len(collabFleet) != 1 {
		t.Fatalf("SITE-SCOPE LEAK: the collaborator's fleet = %+v, want X %s only", collabFleet, siteX)
	}
	gotX, ok := collabFleet[siteX]
	if !ok {
		t.Fatalf("OVER-FIRING: the collaborator's fleet lacks its own site X %s", siteX)
	}
	wantX := ownerFleet[siteX]
	if gotX.Status != wantX.Status || gotX.FixCount != wantX.FixCount ||
		strings.Join(gotX.Failing, ",") != strings.Join(wantX.Failing, ",") ||
		strings.Join(gotX.Warnings, ",") != strings.Join(wantX.Warnings, ",") {
		t.Fatalf("FACTS STRIPPED: the collaborator's fleet row for X is %+v, the owner's is %+v", gotX, wantX)
	}
	// And X's full checklist is byte for byte the owner's.
	collabGet := rdyAPIDo(t, h, collabX, http.MethodGet, readiness(siteX))
	if collabGet.Code != http.StatusOK || !bytes.Equal(collabGet.Body.Bytes(), ownerGet.Body.Bytes()) {
		t.Fatalf("FACTS STRIPPED: the collaborator's checklist for X differs from the owner's\n"+
			"collaborator (%d): %s\nowner: %s", collabGet.Code, collabGet.Body.String(), ownerGet.Body.String())
	}

	// A viewer reads the checklist and cannot ask for a new one.
	if got := rdyAPIDecodeSite(t, "viewer GET on X", rdyAPIDo(t, h, viewerA, http.MethodGet, readiness(siteX))); got.SiteID != siteX {
		t.Fatalf("the viewer got site_id %s, want %s", got.SiteID, siteX)
	}
	rdyAPIWantError(t, "a viewer's refresh", rdyAPIDo(t, h, viewerA, http.MethodPost, refresh(siteX)),
		http.StatusForbidden, "insufficient_permission")
	wantQueued("a viewer's refresh", 1)
}
