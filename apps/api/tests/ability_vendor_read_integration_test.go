// E3-G1: a vendor read through site_ability_run over HTTP with a real bearer
// token, on the wpmgr_app pool with the real audit recorder and the real
// m160 definer. A fake agent answers. The superuser connection only arranges
// site state and reads counts; it never stands in for the application.
// Capped data: at most four tenants, seven sites, one catalogue entry.
package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/abilities"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/govcontext"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// vrAgent refuses every read with read_side_effect_detected.
type vrAgent struct {
	mu    sync.Mutex
	sites []uuid.UUID
}

func (a *vrAgent) AbilityRun(_ context.Context, siteID uuid.UUID, _ string, _ agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sites = append(a.sites, siteID)
	return agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{
		Code: "read_side_effect_detected",
		SideEffects: &agentcmd.AbilityRunSideEffects{
			Options: []string{"vr_option"}, HTTPHosts: []string{}, Blocked: []string{},
		},
	}
}

func (a *vrAgent) calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.sites)
}

type vrWorld struct {
	app, admin *db.Pool
	root       uuid.UUID
	tenant     uuid.UUID
	inScope    []uuid.UUID
	outScope   uuid.UUID
	entry      sqlc.AbilityCatalogue
	agent      *vrAgent
	eng        *gin.Engine
	bearer     string
	repo       *mcp.Repo
}

// vrRefresh stores one vendor inventory row for a site, as the refresh job
// does, in one tenant transaction on the app pool.
func vrRefresh(t *testing.T, pool *db.Pool, tenant, site uuid.UUID, name string) {
	t.Helper()
	ctx := context.Background()
	at := time.Now().UTC()
	if err := pool.RunTenantTx(ctx, acprOrgPrincipal(tenant), func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := q.UpsertSiteAbilityInventory(ctx, sqlc.UpsertSiteAbilityInventoryParams{
			TenantID: tenant, SiteID: site, CheckedAt: at,
			Names: []string{name}, OwnerKinds: []string{"plugin"}, OwnerDirs: []string{"c1-builder"},
			OwnerOks: []string{"true"}, OwnerVersions: []string{"1.5"},
			SchemaStructSha256s: []string{c1SchemaHash}, InputSchemas: []string{`{"type":"object"}`},
			OutputSchemas: []string{""}, Annotations: []string{""}, SiteLabels: []string{"Label"},
			SiteDescriptions: []string{""},
		}); err != nil {
			return err
		}
		return q.UpsertSiteAbilityInventoryRun(ctx, sqlc.UpsertSiteAbilityInventoryRunParams{
			TenantID: tenant, SiteID: site, CheckedAt: at, SnapshotID: uuid.New(), ApiPresent: true, AbilitiesStored: 1,
		})
	}); err != nil {
		t.Fatalf("refresh %s: %v", site, err)
	}
}

func newVRWorld(t *testing.T) *vrWorld {
	t.Helper()
	ctx, app, adm, root := c1World(t)
	r := uuid.NewString()[:8]
	w := &vrWorld{app: app, admin: adm, root: root, agent: &vrAgent{}}
	w.tenant = seedTenant(t, app, "vr-"+r)
	for i := 0; i < 4; i++ {
		site := seedSite(t, app, w.tenant, "https://vr-"+r+"-"+string(rune('a'+i))+".test")
		if i < 3 {
			w.inScope = append(w.inScope, site)
		} else {
			w.outScope = site
		}
	}
	if _, err := adm.Exec(ctx, `UPDATE sites SET connection_state = 'connected', agent_version = $2, wp_version = '7.1' WHERE tenant_id = $1`,
		w.tenant, agentcmd.MinAgentVersionForVendorReads); err != nil {
		t.Fatalf("arrange sites: %v", err)
	}
	name := "vr-" + r + "/read-thing"
	entry, err := c1Upsert(ctx, app, root, c1VendorRead(root, name, "1.0", "2.0"))
	if err != nil {
		t.Fatalf("seed vendor entry: %v", err)
	}
	w.entry = entry
	for _, s := range append(append([]uuid.UUID{}, w.inScope...), w.outScope) {
		vrRefresh(t, app, w.tenant, s, name)
	}

	repo := mcp.NewRepo(app)
	msvc := mcp.NewService(repo).WithAudit(audit.NewRecorder(app, domain.SystemClock{})).
		WithContextResolver(&govcontext.Resolver{Store: govcontext.NewRepo(app)})
	if err := msvc.EnableAbilityTools(repo, w.agent, abilities.SendableEntry, "integration-secret-integration-secret"); err != nil {
		t.Fatalf("enable ability tools: %v", err)
	}
	w.eng = mountLikeProduction(t, msvc, domain.Principal{TenantID: w.tenant, Scope: domain.ScopeOrg})
	w.bearer = e2Grant(t, repo, w.tenant, w.inScope)
	w.repo = repo
	return w
}

// vrQualify makes a tenant count towards the fleet threshold: a paid, active
// plan (m160's "paid or aged").
func vrQualify(t *testing.T, adm *db.Pool, tenant uuid.UUID) {
	t.Helper()
	if _, err := adm.Exec(context.Background(),
		`UPDATE tenants SET plan = 'starter', plan_status = 'active' WHERE id = $1`, tenant); err != nil {
		t.Fatalf("qualify tenant: %v", err)
	}
}

// vrTenant arranges one more qualified tenant with one connected, inventoried
// site and its own grant, on the same engine, and returns its bearer.
func (w *vrWorld) vrTenant(t *testing.T) (tenant, site uuid.UUID, bearer string) {
	t.Helper()
	ctx := context.Background()
	r := uuid.NewString()[:8]
	tenant = seedTenant(t, w.app, "vr-"+r)
	vrQualify(t, w.admin, tenant)
	site = seedSite(t, w.app, tenant, "https://vr-"+r+".test")
	if _, err := w.admin.Exec(ctx, `UPDATE sites SET connection_state = 'connected', agent_version = $2, wp_version = '7.1' WHERE tenant_id = $1`,
		tenant, agentcmd.MinAgentVersionForVendorReads); err != nil {
		t.Fatalf("arrange site: %v", err)
	}
	vrRefresh(t, w.app, tenant, site, w.entry.Name)
	return tenant, site, e2Grant(t, w.repo, tenant, []uuid.UUID{site})
}

func (w *vrWorld) runAs(t *testing.T, bearer string, site uuid.UUID) cpeRPC {
	t.Helper()
	return cpeCall(t, w.eng, bearer, mcp.ToolSiteAbilityRun, map[string]any{
		"site_id": site.String(), "name": w.entry.Name,
	})
}

func vrReason(res cpeRPC) string {
	r, _ := res.data["not_runnable_reason"].(string)
	return r
}

func (w *vrWorld) auditCount(t *testing.T, tenant uuid.UUID, action string) int {
	t.Helper()
	var n int
	if err := w.admin.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3`,
		tenant, action, w.entry.EntryID.String()).Scan(&n); err != nil {
		t.Fatalf("count audit %s: %v", action, err)
	}
	return n
}

func (w *vrWorld) run(t *testing.T, site uuid.UUID) cpeRPC {
	t.Helper()
	return cpeCall(t, w.eng, w.bearer, mcp.ToolSiteAbilityRun, map[string]any{
		"site_id": site.String(), "name": w.entry.Name,
	})
}

// TestAbilityVendorRead_OutOfScopeIdentical: a vendor read for a site of the
// same tenant outside the grant's scope answers exactly what a site that does
// not exist answers (-32007), and the agent is never called.
func TestAbilityVendorRead_OutOfScopeIdentical(t *testing.T) {
	w := newVRWorld(t)
	missing := uuid.New()
	out := w.run(t, w.outScope)
	none := w.run(t, missing)
	out.wantErr(t, "out-of-scope vendor read", -32007, "")
	none.wantErr(t, "nonexistent site vendor read", -32007, "")
	if strings.ReplaceAll(out.raw, w.outScope.String(), "SITE") != strings.ReplaceAll(none.raw, missing.String(), "SITE") {
		t.Fatalf("answers differ:\n out: %s\nnone: %s", out.raw, none.raw)
	}
	if n := w.agent.calls(); n != 0 {
		t.Fatalf("agent called %d times", n)
	}
}

// TestAbilityVendorRead_SideEffectRecordsAndDisables (owner ruling
// 2026-10-02): one qualified tenant with three sites switches the entry off
// for ITSELF at the first report, and its other sites are refused before the
// agent is asked, while the fleet entry stays enabled. Two more qualified
// tenants reporting make three, and the third disables the entry fleet-wide
// with one NULL-actor audit row.
func TestAbilityVendorRead_SideEffectRecordsAndDisables(t *testing.T) {
	w := newVRWorld(t)
	ctx := context.Background()
	vrQualify(t, w.admin, w.tenant)

	first := w.run(t, w.inScope[0])
	if first.code == 0 || first.result != nil {
		t.Fatalf("a side-effecting read returned a result: %s", first.raw)
	}
	if strings.Contains(first.raw, `"vr_option"`) {
		t.Fatalf("unfenced site text: %s", first.raw)
	}
	if !m160DisabledFor(t, ctx, w.app, w.tenant, w.tenant, w.entry.EntryID) {
		t.Fatal("the reporting tenant's switch is not on after its first report")
	}
	m160WantEnabled(t, ctx, w.app, w.root, w.entry.EntryID, "one tenant reported", true)

	for i, site := range w.inScope[1:] {
		res := w.run(t, site)
		if res.code == 0 || vrReason(res) != "disabled_for_your_account" {
			t.Fatalf("site %d: want disabled_for_your_account, got %s", i+1, res.raw)
		}
		if !strings.Contains(res.msg, "turned this tool off for your account") {
			t.Fatalf("site %d: message %q", i+1, res.msg)
		}
	}
	if n := w.agent.calls(); n != 1 {
		t.Fatalf("agent calls = %d, want 1: the tenant switch must refuse before the agent", n)
	}
	m160WantEnabled(t, ctx, w.app, w.root, w.entry.EntryID, "one tenant, three sites", true)
	if n := w.auditCount(t, w.tenant, mcp.ActionAbilityReadSideEffect); n != 1 {
		t.Fatalf("ability.read_side_effect rows = %d, want 1", n)
	}

	// Two more qualified tenants: the third distinct one disables the fleet.
	_, siteB, bearerB := w.vrTenant(t)
	_, siteC, bearerC := w.vrTenant(t)
	if res := w.runAs(t, bearerB, siteB); res.code == 0 {
		t.Fatalf("tenant B's read returned a result: %s", res.raw)
	}
	m160WantEnabled(t, ctx, w.app, w.root, w.entry.EntryID, "two tenants reported", true)
	if res := w.runAs(t, bearerC, siteC); res.code == 0 {
		t.Fatalf("tenant C's read returned a result: %s", res.raw)
	}
	m160WantEnabled(t, ctx, w.app, w.root, w.entry.EntryID, "three tenants reported", false)
	if n := m160SystemDisables(m160Audit(t, ctx, w.app, w.root, w.entry.EntryID)); n != 1 {
		t.Fatalf("system disable audit rows = %d, want 1", n)
	}
	if n := w.agent.calls(); n != 3 {
		t.Fatalf("agent calls = %d, want 3", n)
	}
	// A later call from tenant B: the fleet switch wins, the agent is not asked.
	if res := w.runAs(t, bearerB, siteB); res.code == 0 || vrReason(res) != "disabled" {
		t.Fatalf("fleet-disabled entry: %s", res.raw)
	}
	if n := w.agent.calls(); n != 3 {
		t.Fatalf("a disabled entry reached the agent (calls=%d)", n)
	}
}

// vrReenableEngine mounts the re-enable route as server.New does, on the
// wpmgr_app pool with the real audit recorder, behind a stand-in for session
// auth that carries p.
func vrReenableEngine(w *vrWorld, p domain.Principal, sa abilities.SuperadminChecker) *gin.Engine {
	gin.SetMode(gin.TestMode)
	eng := gin.New()
	g := eng.Group("/api/v1", func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	})
	abilities.NewTenantHandler(abilities.NewTenantRepo(w.app, audit.NewRecorder(w.app, domain.SystemClock{})), sa).Register(g)
	return eng
}

func vrReenable(eng *gin.Engine, entry uuid.UUID) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ai/abilities/"+entry.String()+"/reenable", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	eng.ServeHTTP(rec, req)
	return rec
}

type vrNoSuperadmin struct{}

func (vrNoSuperadmin) IsSuperadmin(context.Context, uuid.UUID) (bool, error) { return false, nil }

// TestAbilityVendorRead_TenantReenable: after a report the tenant is refused
// with disabled_for_your_account. A non-admin of the tenant cannot switch it
// back on, nor can an admin of another tenant; the tenant's admin can, it is
// audited, and the next read reaches the agent again.
func TestAbilityVendorRead_TenantReenable(t *testing.T) {
	w := newVRWorld(t)
	ctx := context.Background()
	r := uuid.NewString()[:8]
	admin := seedUser(t, w.admin, "vr-admin-"+r+"@example.test", "Admin", true)
	operator := seedUser(t, w.admin, "vr-op-"+r+"@example.test", "Operator", true)
	otherTenant := seedTenant(t, w.app, "vr-other-"+r)
	otherAdmin := seedUser(t, w.admin, "vr-other-"+r+"@example.test", "Other", true)

	if res := w.run(t, w.inScope[0]); res.code == 0 {
		t.Fatalf("a side-effecting read returned a result: %s", res.raw)
	}
	if res := w.run(t, w.inScope[1]); vrReason(res) != "disabled_for_your_account" {
		t.Fatalf("want disabled_for_your_account, got %s", res.raw)
	}
	if n := w.agent.calls(); n != 1 {
		t.Fatalf("agent calls = %d, want 1", n)
	}

	user := func(id, tenant uuid.UUID, role string) domain.Principal {
		return domain.Principal{Type: domain.PrincipalUser, UserID: id, TenantID: tenant, Role: role, Scope: domain.ScopeOrg}
	}
	if rec := vrReenable(vrReenableEngine(w, user(operator, w.tenant, "operator"), vrNoSuperadmin{}), w.entry.EntryID); rec.Code != http.StatusForbidden {
		t.Fatalf("operator: %d %s", rec.Code, rec.Body.String())
	}
	if rec := vrReenable(vrReenableEngine(w, user(otherAdmin, otherTenant, "admin"), vrNoSuperadmin{}), w.entry.EntryID); rec.Code != http.StatusNotFound {
		t.Fatalf("another tenant's admin: %d %s", rec.Code, rec.Body.String())
	}
	if !m160DisabledFor(t, ctx, w.app, w.tenant, w.tenant, w.entry.EntryID) {
		t.Fatal("a refused re-enable switched the tool back on")
	}

	adminEng := vrReenableEngine(w, user(admin, w.tenant, "admin"), vrNoSuperadmin{})
	if rec := vrReenable(adminEng, w.entry.EntryID); rec.Code != http.StatusOK {
		t.Fatalf("tenant admin: %d %s", rec.Code, rec.Body.String())
	}
	if m160DisabledFor(t, ctx, w.app, w.tenant, w.tenant, w.entry.EntryID) {
		t.Fatal("the tenant admin's re-enable left the tool off")
	}
	if n := w.auditCount(t, w.tenant, abilities.ActionAbilityTenantReenabled); n != 1 {
		t.Fatalf("ability.tenant_reenabled rows = %d, want 1", n)
	}
	var by uuid.UUID
	if err := w.admin.QueryRow(ctx, `SELECT reenabled_by_user_id FROM ability_tenant_disables WHERE tenant_id = $1 AND entry_id = $2`,
		w.tenant, w.entry.EntryID).Scan(&by); err != nil || by != admin {
		t.Fatalf("reenabled_by_user_id = %s err=%v, want %s", by, err, admin)
	}
	if rec := vrReenable(adminEng, w.entry.EntryID); rec.Code != http.StatusNotFound {
		t.Fatalf("second re-enable: %d %s", rec.Code, rec.Body.String())
	}

	// Back on: the next read reaches the agent again.
	if res := w.run(t, w.inScope[1]); vrReason(res) == "disabled_for_your_account" {
		t.Fatalf("still refused after the re-enable: %s", res.raw)
	}
	if n := w.agent.calls(); n != 2 {
		t.Fatalf("agent calls = %d, want 2", n)
	}
}
