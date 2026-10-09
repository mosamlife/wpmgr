// E2, AI creates a page: the write branch of site_ability_run over HTTP with
// a real bearer token, then approve, the dispatch worker, undo and the
// content-editing switch through internal/abilityrequest's service, all on
// the wpmgr_app pool with the real audit recorder. A fake agent answers
// precheck, write and revert. The superuser connection only arranges site
// state and reads counts; it never stands in for the application.
package tests

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/abilities"
	"github.com/mosamlife/wpmgr/apps/api/internal/abilityrequest"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/govcontext"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// e2Agent is a fake site agent for wpmgr/page-create. Inputs and content are
// ASCII with no '/', '<', '>' or '&', where encoding/json and PHP's
// json_encode produce the same bytes, so its digests are the agent's.
type e2Agent struct {
	mu      sync.Mutex
	modes   []string
	last    agentcmd.AbilityRunCall
	enabled int
	// override, when set and returning handled, answers a call in place of
	// the defaults below (GH #824, #826 tests).
	override func(call agentcmd.AbilityRunCall) (resp agentcmd.AbilityRunResponse, err error, handled bool)
}

func (a *e2Agent) setOverride(f func(call agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error, bool)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.override = f
}

func e2Arr(items ...string) []byte {
	b, _ := json.Marshal(items)
	return b
}

func e2Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (a *e2Agent) AbilityRun(_ context.Context, _ uuid.UUID, _ string, call agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.modes = append(a.modes, call.Mode)
	a.last = call
	if a.override != nil {
		if resp, err, handled := a.override(call); handled {
			return resp, err
		}
	}
	switch call.Mode {
	case agentcmd.AbilityRunModePrecheck:
		var in struct {
			PostType string `json:"post_type"`
			Editor   string `json:"editor"`
			Title    string `json:"title"`
		}
		_ = json.Unmarshal(call.Input, &in)
		content := "Body text"
		preview := e2Hex(e2Arr(in.Editor, in.PostType, "draft", in.Title, content))
		base := e2Hex(e2Arr("new_post", in.PostType))
		pre := e2Hex(e2Arr(call.EntrySHA256, e2Hex(call.Input), base, preview))
		pv, _ := json.Marshal(map[string]string{"post_type": in.PostType, "editor": in.Editor, "status": "draft", "title": in.Title, "content": content})
		return agentcmd.AbilityRunResponse{OK: true, Mode: "precheck", Valid: true, BaseFingerprint: base,
			PreviewDigest: preview, PrecheckDigest: pre, Preview: pv}, nil
	case agentcmd.AbilityRunModeWrite:
		return agentcmd.AbilityRunResponse{OK: true, Outcome: "created", Mode: "write", PostID: 42}, nil
	case agentcmd.AbilityRunModeRevert:
		return agentcmd.AbilityRunResponse{OK: true, Outcome: "reverted", Mode: "revert", PostID: 42, Trashed: true}, nil
	}
	return agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{Code: "bad_mode"}
}

func (a *e2Agent) ContentEditingEnable(context.Context, uuid.UUID, string) (agentcmd.ContentEditingEnableResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.enabled++
	return agentcmd.ContentEditingEnableResponse{OK: true, Outcome: "created", UserID: 7}, nil
}

func (a *e2Agent) sent(mode string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, m := range a.modes {
		if m == mode {
			n++
		}
	}
	return n
}

const e2PageInput = `{"post_type":"page","editor":"wordpress_blocks","title":"Spring sale","outline":[{"type":"paragraph","text":"Body text"}]}`

// e2World is one tenant, one connected site at the page-create floor, the
// MCP surface with ability writes on, and the ability request service.
type e2World struct {
	pool   *db.Pool
	admin  *db.Pool
	tenant uuid.UUID
	site   uuid.UUID
	person domain.Principal
	agent  *e2Agent
	eng    *gin.Engine
	bearer string
	svc    *abilityrequest.Service
}

func newE2World(t *testing.T, contentEditingOn bool) *e2World {
	t.Helper()
	ctx := context.Background()
	pool := startPostgres(t)
	repo := mcp.NewRepo(pool)
	rec := audit.NewRecorder(pool, domain.SystemClock{})
	r := uuid.NewString()[:8]
	w := &e2World{pool: pool, agent: &e2Agent{}}
	w.tenant = seedTenant(t, pool, "e2-"+r)
	w.site = seedSite(t, pool, w.tenant, "https://e2-"+r+".test")
	w.admin = connectAdmin(t, pool)
	t.Cleanup(w.admin.Close)
	if _, err := w.admin.Exec(ctx, `UPDATE sites SET connection_state = 'connected', agent_version = $2 WHERE tenant_id = $1`,
		w.tenant, agentcmd.MinAgentVersionForPageCreate); err != nil {
		t.Fatalf("arrange site: %v", err)
	}
	if contentEditingOn {
		if _, err := w.admin.Exec(ctx, `UPDATE sites SET content_editing_enabled_at = now(), content_editing_principal_user_id = 7 WHERE id = $1`, w.site); err != nil {
			t.Fatalf("arrange content editing: %v", err)
		}
	}
	m155Refresh(t, pool, w.tenant, w.site, mcp.AbilityPageCreate)
	if _, err := abilities.StampOwnEntryHashes(ctx, pool, slog.Default()); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	user := seedUserRow(t, w.admin, "e2-"+r+"@example.test")
	seedMembershipRow(t, w.admin, user, w.tenant)
	w.person = domain.Principal{Type: domain.PrincipalUser, UserID: user, TenantID: w.tenant, Scope: domain.ScopeOrg, Role: "owner"}

	msvc := mcp.NewService(repo).WithAudit(rec).
		WithContextResolver(&govcontext.Resolver{Store: govcontext.NewRepo(pool)})
	if err := msvc.EnableAbilityTools(repo, w.agent, abilities.SendableEntry, "integration-secret"); err != nil {
		t.Fatalf("enable ability tools: %v", err)
	}
	if err := msvc.EnableAbilityWrites(repo); err != nil {
		t.Fatalf("enable ability writes: %v", err)
	}
	if err := msvc.SetWriteToolsEnabled(true); err != nil {
		t.Fatalf("write tools: %v", err)
	}
	w.eng = mountLikeProduction(t, msvc, domain.Principal{TenantID: w.tenant, Scope: domain.ScopeOrg})
	w.bearer = e2Grant(t, repo, w.tenant, []uuid.UUID{w.site})

	w.svc = abilityrequest.NewService(pool, repo, msvc, rec, slog.Default())
	w.svc.SetWriteToolsEnabled(true)
	w.svc.SetSender(w.agent, abilities.SendableEntry, msvc)
	w.svc.SetEnabler(w.agent)
	return w
}

// e2Grant seeds a live connection holding mcp.ability.request over sites.
func e2Grant(t *testing.T, repo *mcp.Repo, tenantID uuid.UUID, sites []uuid.UUID) string {
	t.Helper()
	ctx := context.Background()
	clientID := "e2-client-" + uuid.NewString()
	secretHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if n, err := repo.RegisterClient(ctx, sqlc.RegisterMCPOAuthClientParams{
		ClientID: clientID, ClientSecretHash: &secretHash, TokenEndpointAuthMethod: "client_secret_basic",
		RedirectUris: []string{"https://claude.ai/api/mcp/auth_callback"}, RegisteredScopes: mcp.SupportedScopes(),
	}); err != nil || n != 1 {
		t.Fatalf("register client: n=%d err=%v", n, err)
	}
	codeSum := sha256.Sum256([]byte("e2-code-" + uuid.NewString()))
	g, code, err := repo.CreateGrantWithCode(ctx, domain.Principal{TenantID: tenantID, Scope: domain.ScopeOrg}, sqlc.CreateMCPGrantParams{
		TenantID: tenantID, Name: "Page laptop", Status: "active", SiteScopeMode: "list",
		ScopeTagIds: []uuid.UUID{}, ScopeSiteIds: sites, ClientID: &clientID,
		Capabilities: []string{"mcp.sites.read", "mcp.ability.read", "mcp.ability.request"},
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
	bearer := "e2-bearer-" + uuid.NewString()
	sum := sha256.Sum256([]byte(bearer))
	if _, err := repo.RedeemAuthorizationCode(ctx, tenantID, code.ID, sqlc.CreateMCPConnectionTokenParams{
		TenantID: tenantID, GrantID: g.ID, TokenPrefix: "e2test", TokenHash: hex.EncodeToString(sum[:]), Status: "active",
	}); err != nil {
		t.Fatalf("redeem token: %v", err)
	}
	return bearer
}

// ask runs site_ability_run for a page and returns the request id.
func (w *e2World) ask(t *testing.T) uuid.UUID {
	t.Helper()
	var input map[string]any
	_ = json.Unmarshal([]byte(e2PageInput), &input)
	res := cpeCall(t, w.eng, w.bearer, mcp.ToolSiteAbilityRun, map[string]any{
		"site_id": w.site.String(), "name": mcp.AbilityPageCreate, "input": input,
	}).wantOK(t, "ask for a page")
	if res["state"] != "waiting_for_approval" {
		t.Fatalf("state = %v, want waiting_for_approval", res["state"])
	}
	id, err := uuid.Parse(res["request_id"].(string))
	if err != nil {
		t.Fatalf("request_id: %v", err)
	}
	return id
}

func (w *e2World) digest(t *testing.T, id uuid.UUID) string {
	t.Helper()
	rows, err := w.svc.List(context.Background(), w.person, &w.site, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, r := range rows {
		if r.ID == id {
			return r.PresentedDigest
		}
	}
	t.Fatalf("request %s not in the queue", id)
	return ""
}

func (w *e2World) dispatch(t *testing.T, id uuid.UUID) {
	t.Helper()
	grant := w.grantOf(t, id)
	job := &river.Job[abilityrequest.DispatchArgs]{Args: abilityrequest.DispatchArgs{
		TenantID: w.tenant, RequestID: id, SiteID: w.site, GrantID: grant,
	}}
	if err := abilityrequest.NewDispatchWorker(w.svc).Work(context.Background(), job); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
}

func (w *e2World) grantOf(t *testing.T, id uuid.UUID) uuid.UUID {
	t.Helper()
	var g uuid.UUID
	if err := w.admin.QueryRow(context.Background(),
		`SELECT proposed_by_grant_id FROM assistant_ability_requests WHERE id = $1`, id).Scan(&g); err != nil {
		t.Fatalf("read grant: %v", err)
	}
	return g
}

func (w *e2World) row(t *testing.T, id uuid.UUID) (state string, outcome, notSent, undo *string, post *int64) {
	t.Helper()
	if err := w.admin.QueryRow(context.Background(),
		`SELECT state, outcome, not_sent_reason, undo_state, created_post_id FROM assistant_ability_requests WHERE id = $1`, id).
		Scan(&state, &outcome, &notSent, &undo, &post); err != nil {
		t.Fatalf("read row: %v", err)
	}
	return
}

// TestE2PageCreateApproveDispatchCreatedAsAppRole: ask, approve, the worker
// sends write with expected{} bound to the precheck, outcome created with
// the undo window open. The request is stored under the request id its
// precheck was sent with, and the write is sent under that same id (BF-C F1).
func TestE2PageCreateApproveDispatchCreatedAsAppRole(t *testing.T) {
	w := newE2World(t, true)
	id := w.ask(t)
	if w.agent.sent(agentcmd.AbilityRunModePrecheck) != 1 || w.agent.last.Mode != agentcmd.AbilityRunModePrecheck ||
		w.agent.last.RequestID != id {
		t.Fatalf("stored as request %s; the precheck was sent as %s (%s), want the same id", id, w.agent.last.RequestID, w.agent.last.Mode)
	}
	if _, err := w.svc.Approve(context.Background(), w.person, w.site, id, w.digest(t, id)); err != nil {
		t.Fatalf("approve: %v", err)
	}
	w.dispatch(t, id)
	if w.agent.sent(agentcmd.AbilityRunModeWrite) != 1 {
		t.Fatal("the write was not sent exactly once")
	}
	if w.agent.last.Expected == nil || w.agent.last.RequestID != id {
		t.Fatalf("write not bound to the request: %+v", w.agent.last)
	}
	state, outcome, _, undo, post := w.row(t, id)
	if state != "done" || outcome == nil || *outcome != "created" || post == nil || *post != 42 || undo == nil || *undo != "available" {
		t.Fatalf("row after dispatch: state=%s outcome=%v post=%v undo=%v", state, outcome, post, undo)
	}
	// A second dispatch of the same row sends nothing.
	w.dispatch(t, id)
	if w.agent.sent(agentcmd.AbilityRunModeWrite) != 1 {
		t.Fatal("a done request was sent again")
	}
}

// TestE2StaleDigestApproveRefusedAsAppRole: an approve with a digest other
// than the stored one is a 409 and approves nothing.
func TestE2StaleDigestApproveRefusedAsAppRole(t *testing.T) {
	w := newE2World(t, true)
	id := w.ask(t)
	_, err := w.svc.Approve(context.Background(), w.person, w.site, id, strings.Repeat("0", 64))
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != abilityrequest.CodeRequestChanged {
		t.Fatalf("stale digest: got %v, want %s", err, abilityrequest.CodeRequestChanged)
	}
	if state, _, _, _, _ := w.row(t, id); state != "pending" {
		t.Fatalf("state after a refused approve = %s, want pending", state)
	}
}

// TestE2EntryChangedAfterApproveClosesAsAppRole (W1): the catalogue entry
// changes between approval and dispatch; nothing is sent and the row closes
// not_sent / entry_changed.
func TestE2EntryChangedAfterApproveClosesAsAppRole(t *testing.T) {
	w := newE2World(t, true)
	id := w.ask(t)
	if _, err := w.svc.Approve(context.Background(), w.person, w.site, id, w.digest(t, id)); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// The superuser stands in for a superadmin catalogue edit that moved the
	// stamped hash.
	if _, err := w.admin.Exec(context.Background(),
		`UPDATE ability_catalogue SET entry_sha256 = $2 WHERE name = $1`, mcp.AbilityPageCreate, strings.Repeat("a", 64)); err != nil {
		t.Fatalf("change entry: %v", err)
	}
	w.dispatch(t, id)
	if w.agent.sent(agentcmd.AbilityRunModeWrite) != 0 {
		t.Fatal("a write was sent against a changed entry")
	}
	state, _, notSent, _, _ := w.row(t, id)
	if state != "not_sent" || notSent == nil || *notSent != abilityrequest.ReasonEntryChanged {
		t.Fatalf("row: state=%s not_sent_reason=%v, want not_sent/entry_changed", state, notSent)
	}
}

// TestE2UndoTrashesAsAppRole: a person's undo sends revert with no input
// (W3) and records undone.
func TestE2UndoTrashesAsAppRole(t *testing.T) {
	w := newE2World(t, true)
	id := w.ask(t)
	if _, err := w.svc.Approve(context.Background(), w.person, w.site, id, w.digest(t, id)); err != nil {
		t.Fatalf("approve: %v", err)
	}
	w.dispatch(t, id)
	got, err := w.svc.Undo(context.Background(), w.person, w.site, id)
	if err != nil {
		t.Fatalf("undo: %v", err)
	}
	if got.UndoState == nil || *got.UndoState != abilityrequest.UndoDone {
		t.Fatalf("undo_state = %v, want undone", got.UndoState)
	}
	if len(w.agent.last.Input) != 0 || w.agent.last.Mode != agentcmd.AbilityRunModeRevert {
		t.Fatalf("revert carried input or wrong mode: %+v", w.agent.last)
	}
	if _, err := w.svc.Undo(context.Background(), w.person, w.site, id); err == nil {
		t.Fatal("a second undo was accepted")
	}
}

// TestE2WritesRefusedUntilContentEditingEnabledAsAppRole: before enable the
// run is refused and no request row exists; after enable the same run makes
// one.
func TestE2WritesRefusedUntilContentEditingEnabledAsAppRole(t *testing.T) {
	w := newE2World(t, false)
	var input map[string]any
	_ = json.Unmarshal([]byte(e2PageInput), &input)
	before := cpeCall(t, w.eng, w.bearer, mcp.ToolSiteAbilityRun, map[string]any{
		"site_id": w.site.String(), "name": mcp.AbilityPageCreate, "input": input,
	})
	if before.code == 0 || !strings.Contains(before.msg, "Content editing is not enabled") {
		t.Fatalf("write before enable: want the content-editing refusal, got code=%d msg=%q", before.code, before.msg)
	}
	if w.agent.sent(agentcmd.AbilityRunModePrecheck) != 0 {
		t.Fatal("the site was asked to precheck before content editing was enabled")
	}
	var n int
	if err := w.admin.QueryRow(context.Background(),
		`SELECT count(*) FROM assistant_ability_requests WHERE site_id = $1`, w.site).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows before enable = %d (err %v), want 0", n, err)
	}
	st, err := w.svc.EnableContentEditing(context.Background(), w.person, w.site)
	if err != nil || !st.Enabled || st.PrincipalUserID == nil || *st.PrincipalUserID != 7 {
		t.Fatalf("enable: %+v %v", st, err)
	}
	got, err := w.svc.ContentEditing(context.Background(), w.person, w.site)
	if err != nil || !got.Enabled || got.EnabledBy == nil || *got.EnabledBy != w.person.UserID {
		t.Fatalf("state after enable: %+v %v", got, err)
	}
	w.ask(t)
}
