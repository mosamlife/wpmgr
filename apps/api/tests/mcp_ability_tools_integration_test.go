// The ability engine's MCP tools over HTTP with a real bearer token, through
// the mounted transport, Authenticate, the shipped mcp.Repo (whose ability
// reads run in RunTenantTx under the connection's single-site principal) and
// the real audit recorder, on the wpmgr_app pool. The superuser connection
// only arranges the site's agent version; it never reads an answer.
package tests

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/abilities"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/govcontext"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

const abilityIntegrationCanary = "zzz-ability-int-canary"

type abilityIntAgent struct {
	calls int
	last  agentcmd.AbilityRunCall
}

func (a *abilityIntAgent) AbilityRun(_ context.Context, _ uuid.UUID, _ string, call agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error) {
	a.calls++
	a.last = call
	return agentcmd.AbilityRunResponse{OK: true, Mode: "read", Ability: "wpmgr/site-facts",
		EntrySHA256: call.EntrySHA256, Output: json.RawMessage(`{"wp_version":"` + abilityIntegrationCanary + `"}`)}, nil
}

// abilityGrant seeds a live connection holding mcp.ability.read over sites.
func abilityGrant(t *testing.T, repo *mcp.Repo, tenantID uuid.UUID, sites []uuid.UUID) string {
	t.Helper()
	ctx := context.Background()
	clientID := "abl-client-" + uuid.NewString()
	secretHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if n, err := repo.RegisterClient(ctx, sqlc.RegisterMCPOAuthClientParams{
		ClientID: clientID, ClientSecretHash: &secretHash, TokenEndpointAuthMethod: "client_secret_basic",
		RedirectUris: []string{"https://claude.ai/api/mcp/auth_callback"}, RegisteredScopes: mcp.SupportedScopes(),
	}); err != nil || n != 1 {
		t.Fatalf("register client: n=%d err=%v", n, err)
	}
	codeSum := sha256.Sum256([]byte("abl-code-" + uuid.NewString()))
	g, code, err := repo.CreateGrantWithCode(ctx, domain.Principal{TenantID: tenantID, Scope: domain.ScopeOrg}, sqlc.CreateMCPGrantParams{
		TenantID: tenantID, Name: "Ability laptop", Status: "active", SiteScopeMode: "list",
		ScopeTagIds: []uuid.UUID{}, ScopeSiteIds: sites, ClientID: &clientID,
		Capabilities: []string{"mcp.sites.read", "mcp.ability.read"},
		OauthScopes:  []string{"mcp:read", "mcp:site"},
		ExpiresAt:    time.Now().UTC().Add(90 * 24 * time.Hour),
	}, func(grantID uuid.UUID) sqlc.CreateMCPAuthorizationCodeParams {
		return sqlc.CreateMCPAuthorizationCodeParams{
			TenantID: tenantID, GrantID: grantID, ClientID: clientID, CodeHash: hex.EncodeToString(codeSum[:]),
			CodeChallenge: "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM", CodeChallengeMethod: "S256",
			RedirectUri: "https://claude.ai/api/mcp/auth_callback", ExpiresAt: time.Now().UTC().Add(5 * time.Minute),
		}
	}, nil)
	if err != nil {
		t.Fatalf("create grant: %v", err)
	}
	bearer := "abl-bearer-" + uuid.NewString()
	sum := sha256.Sum256([]byte(bearer))
	if _, err := repo.RedeemAuthorizationCode(ctx, tenantID, code.ID, sqlc.CreateMCPConnectionTokenParams{
		TenantID: tenantID, GrantID: g.ID, TokenPrefix: "abltest", TokenHash: hex.EncodeToString(sum[:]), Status: "active",
	}); err != nil {
		t.Fatalf("redeem token: %v", err)
	}
	return bearer
}

// TestAbilityToolsSiteScopeAndAuditAsAppRole: discover, describe and run
// answer for the connection's own site; a site of the same tenant outside
// the grant, a site of another tenant (which also has an inventory) and a
// nonexistent id all answer the identical -32007 body. A run's text is
// served only after its mcp.tool.called row is committed (R1).
func TestAbilityToolsSiteScopeAndAuditAsAppRole(t *testing.T) {
	ctx := context.Background()
	pool := startPostgres(t)
	repo := mcp.NewRepo(pool)
	rec := audit.NewRecorder(pool, domain.SystemClock{})
	agent := &abilityIntAgent{}

	r := uuid.NewString()[:8]
	tenant := seedTenant(t, pool, "abl-"+r)
	other := seedTenant(t, pool, "ablo-"+r)
	mine := seedSite(t, pool, tenant, "https://abl-"+r+".test")
	sibling := seedSite(t, pool, tenant, "https://abls-"+r+".test")
	foreign := seedSite(t, pool, other, "https://ablf-"+r+".test")

	admin := connectAdmin(t, pool)
	defer admin.Close()
	if _, err := admin.Exec(ctx, `UPDATE sites SET connection_state = 'connected', agent_version = $3 WHERE tenant_id IN ($1, $2)`,
		tenant, other, agentcmd.MinAgentVersionForAbilityEngine); err != nil {
		t.Fatalf("arrange: %v", err)
	}
	for _, s := range []struct{ tenant, site uuid.UUID }{{tenant, mine}, {tenant, sibling}, {other, foreign}} {
		m155Refresh(t, pool, s.tenant, s.site, "wpmgr/site-facts", "vendor-"+r+"/tool")
	}

	svc := mcp.NewService(repo).WithAudit(rec).
		WithContextResolver(&govcontext.Resolver{Store: govcontext.NewRepo(pool)})
	if err := svc.EnableAbilityTools(repo, agent, abilities.SendableEntry, "integration-secret"); err != nil {
		t.Fatalf("enable ability tools: %v", err)
	}
	eng := mountLikeProduction(t, svc, domain.Principal{TenantID: tenant, Scope: domain.ScopeOrg})
	bearer := abilityGrant(t, repo, tenant, []uuid.UUID{mine})

	// Own site: every tool answers.
	disc := cpeCall(t, eng, bearer, mcp.ToolSiteAbilitiesDiscover, map[string]any{"site_id": mine.String()}).wantOK(t, "discover own site")
	list, _ := disc["abilities"].([]any)
	if len(list) != 2 {
		t.Fatalf("discover listed %d abilities, want 2: %v", len(list), disc)
	}
	cpeCall(t, eng, bearer, mcp.ToolSiteAbilityDescribe, map[string]any{"site_id": mine.String(), "name": "wpmgr/site-facts"}).wantOK(t, "describe own site")

	before := abilityToolCalledRows(t, admin, tenant)
	run := cpeCall(t, eng, bearer, mcp.ToolSiteAbilityRun, map[string]any{"site_id": mine.String(), "name": "wpmgr/site-facts"})
	run.wantOK(t, "run own site")
	if !strings.Contains(run.text, abilityIntegrationCanary) {
		t.Fatalf("run text lacks the output: %s", run.text)
	}
	if got := abilityToolCalledRows(t, admin, tenant); got != before+1 {
		t.Fatalf("mcp.tool.called rows for the run = %d, want %d (exactly one, written by the transport)", got-before, 1)
	}
	if agentcmd.SHA256Hex(agent.last.Entry) != agent.last.EntrySHA256 {
		t.Fatal("the run sent an entry whose hash does not match its bytes")
	}
	callsBefore := agent.calls

	// Vendor entries are listed, never run.
	cpeCall(t, eng, bearer, mcp.ToolSiteAbilityRun, map[string]any{"site_id": mine.String(), "name": "vendor-" + r + "/tool"}).
		wantErr(t, "run a vendor ability", -32003, "")

	// Out of scope: identical bodies across tools and ids.
	for _, tool := range []string{mcp.ToolSiteAbilitiesDiscover, mcp.ToolSiteAbilityDescribe, mcp.ToolSiteAbilityRun} {
		var bodies []string
		for _, id := range []uuid.UUID{sibling, foreign, uuid.New()} {
			args := map[string]any{"site_id": id.String()}
			if tool != mcp.ToolSiteAbilitiesDiscover {
				args["name"] = "wpmgr/site-facts"
			}
			res := cpeCall(t, eng, bearer, tool, args)
			res.wantErr(t, tool+" out of scope", -32007, "")
			bodies = append(bodies, res.raw)
		}
		for _, b := range bodies[1:] {
			if b != bodies[0] {
				t.Fatalf("%s: out-of-scope bodies differ:\n%s\n%s", tool, bodies[0], b)
			}
		}
	}
	if agent.calls != callsBefore {
		t.Fatal("the agent was called for a site outside the grant, or for a vendor entry")
	}
}

func abilityToolCalledRows(t *testing.T, admin *db.Pool, tenant uuid.UUID) int {
	t.Helper()
	var n int
	if err := admin.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3`,
		tenant, audit.ActionMCPToolCalled, mcp.ToolSiteAbilityRun).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}
