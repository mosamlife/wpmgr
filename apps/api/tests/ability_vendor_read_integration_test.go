// E3-G1: a vendor read through site_ability_run over HTTP with a real bearer
// token, on the wpmgr_app pool with the real audit recorder and the real
// m160 definer. A fake agent answers. The superuser connection only arranges
// site state and reads counts; it never stands in for the application.
// Capped data: one tenant, four sites, one catalogue entry.
package tests

import (
	"context"
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
	return w
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

// TestAbilityVendorRead_SideEffectRecordsAndDisables: three distinct sites
// report read_side_effect_detected; each call is refused and audited, and
// the third disables the entry fleet-wide with one NULL-actor audit row.
func TestAbilityVendorRead_SideEffectRecordsAndDisables(t *testing.T) {
	w := newVRWorld(t)
	ctx := context.Background()
	for i, site := range w.inScope {
		res := w.run(t, site)
		if res.code == 0 || res.result != nil {
			t.Fatalf("site %d: a side-effecting read returned a result: %s", i, res.raw)
		}
		if strings.Contains(res.raw, `"vr_option"`) {
			t.Fatalf("site %d: unfenced site text: %s", i, res.raw)
		}
		enabled := m160Entry(t, ctx, w.app, w.root, w.entry.EntryID).Enabled
		if want := i < 2; enabled != want {
			t.Fatalf("after site %d: enabled=%v, want %v", i, enabled, want)
		}
	}
	if n := w.agent.calls(); n != 3 {
		t.Fatalf("agent calls = %d, want 3", n)
	}
	if n := m160SystemDisables(m160Audit(t, ctx, w.app, w.root, w.entry.EntryID)); n != 1 {
		t.Fatalf("system disable audit rows = %d, want 1", n)
	}
	var rows int
	if err := w.admin.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3`,
		w.tenant, mcp.ActionAbilityReadSideEffect, w.entry.EntryID.String()).Scan(&rows); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if rows != 3 {
		t.Fatalf("ability.read_side_effect rows = %d, want 3", rows)
	}
	// A fourth call: the entry is disabled, so the agent is not asked.
	res := w.run(t, w.inScope[0])
	if res.code == 0 {
		t.Fatalf("disabled entry ran: %s", res.raw)
	}
	if n := w.agent.calls(); n != 3 {
		t.Fatalf("a disabled entry reached the agent (calls=%d)", n)
	}
}
