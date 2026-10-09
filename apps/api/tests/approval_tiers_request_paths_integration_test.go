// Approval tiers (ADR-065), the request and grant paths, end to end as
// wpmgr_app: site_ability_run over HTTP with a real bearer token, the
// decision by the site's setting through internal/abilityrequest's Decide
// with the session authenticator's own setter resolver, the dispatch worker
// run at once, the content-editing switch through its service, connections
// minted and consented to through internal/mcp's service, and the request
// list through the real handler. A fake agent answers precheck, write and
// revert. The superuser connection only arranges site state and reads rows.
package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/abilities"
	"github.com/mosamlife/wpmgr/apps/api/internal/abilityrequest"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/aipolicy"
	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/apikey"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/govcontext"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/internal/middleware"
)

const (
	atRoutePages = "wp-v2-pages-update-fields"
	atPublicBase = "https://app.wpmgr.test"
)

// atAgent is a fake site agent for page creation and REST writes. A page
// it creates gets a new post id and reports status draft; status sets what
// a REST precheck reports for a post (publish when unset). Every value is
// ASCII with no '/', '<', '>' or '&', where encoding/json and PHP's
// json_encode produce the same bytes, so its digests are the agent's.
type atAgent struct {
	mu       sync.Mutex
	nextPost int64
	status   map[int64]string
	writes   []agentcmd.AbilityRunCall
}

func newATAgent() *atAgent { return &atAgent{nextPost: 1000, status: map[int64]string{}} }

func (a *atAgent) setStatus(post int64, status string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.status[post] = status
}

func (a *atAgent) writeCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.writes)
}

func (a *atAgent) AbilityRun(_ context.Context, _ uuid.UUID, _ string, call agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rest := call.RouteSHA256 != ""
	switch call.Mode {
	case agentcmd.AbilityRunModePrecheck:
		if rest {
			var in struct {
				RouteID string `json:"route_id"`
				Path    struct {
					ID int64 `json:"id"`
				} `json:"path"`
				Body map[string]string `json:"body"`
			}
			_ = json.Unmarshal(call.Input, &in)
			status, ok := a.status[in.Path.ID]
			if !ok {
				status = "publish"
			}
			base := e2Hex([]byte(fmt.Sprintf("base-%d-%s", in.Path.ID, status)))
			pre := e2Hex(e2Arr(call.EntrySHA256, call.RouteSHA256, e2Hex(call.Input), base))
			tf, _ := json.Marshal(map[string]any{"id": in.Path.ID, "post_type": "page", "status": status,
				"live": status == "publish", "title_before": "Old title", "excerpt_before": ""})
			ch, _ := json.Marshal([]map[string]string{{"key": "title", "before": "Old title",
				"after": in.Body["title"], "stored": in.Body["title"]}})
			undo := true
			return agentcmd.AbilityRunResponse{OK: true, Mode: "precheck", Valid: true, RouteID: in.RouteID,
				BaseFingerprint: base, PrecheckDigest: pre, TargetFacts: tf, Changes: ch, UndoExact: &undo}, nil
		}
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
		pv, _ := json.Marshal(map[string]string{"post_type": in.PostType, "editor": in.Editor, "status": "draft",
			"title": in.Title, "content": content})
		return agentcmd.AbilityRunResponse{OK: true, Mode: "precheck", Valid: true, BaseFingerprint: base,
			PreviewDigest: preview, PrecheckDigest: pre, Preview: pv}, nil
	case agentcmd.AbilityRunModeWrite:
		a.writes = append(a.writes, call)
		if rest {
			var in struct {
				Path struct {
					ID int64 `json:"id"`
				} `json:"path"`
			}
			_ = json.Unmarshal(call.Input, &in)
			return agentcmd.AbilityRunResponse{OK: true, Outcome: "updated", Mode: "write", PostID: in.Path.ID}, nil
		}
		a.nextPost++
		a.status[a.nextPost] = "draft"
		return agentcmd.AbilityRunResponse{OK: true, Outcome: "created", Mode: "write", PostID: a.nextPost}, nil
	case agentcmd.AbilityRunModeRevert:
		return agentcmd.AbilityRunResponse{OK: true, Outcome: "reverted", Mode: "revert", Trashed: true}, nil
	}
	return agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{Code: "bad_mode"}
}

func (a *atAgent) ContentEditingEnable(context.Context, uuid.UUID, string) (agentcmd.ContentEditingEnableResponse, error) {
	return agentcmd.ContentEditingEnableResponse{OK: true, Outcome: "created", UserID: 7}, nil
}

// atSendNow is the dispatch enqueuer: it runs the dispatch worker at once,
// as a River worker would pick the job up.
type atSendNow struct{ svc *abilityrequest.Service }

func (d atSendNow) EnqueueDispatch(ctx context.Context, args abilityrequest.DispatchArgs) error {
	return abilityrequest.NewDispatchWorker(d.svc).Work(ctx, &river.Job[abilityrequest.DispatchArgs]{Args: args})
}

// atWorld is one tenant with connected sites at the page-layout floor, an
// owner, the MCP surface with ability writes on, the ability request service
// with the policy engine wired as main.go wires it, and the request list
// mounted for the owner.
type atWorld struct {
	pool   *db.Pool
	admin  *db.Pool
	tenant uuid.UUID
	sites  []uuid.UUID
	owner  domain.Principal
	agent  *atAgent
	msvc   *mcp.Service
	svc    *abilityrequest.Service
	eng    *gin.Engine
}

func newATWorld(t *testing.T, nSites int) *atWorld {
	t.Helper()
	ctx := context.Background()
	pool := startPostgres(t)
	repo := mcp.NewRepo(pool)
	rec := audit.NewRecorder(pool, domain.SystemClock{})
	r := uuid.NewString()[:8]
	w := &atWorld{pool: pool, agent: newATAgent()}
	w.tenant = seedTenant(t, pool, "at-"+r)
	for i := 0; i < nSites; i++ {
		w.sites = append(w.sites, seedSite(t, pool, w.tenant, fmt.Sprintf("https://at-%s-%d.test", r, i)))
	}
	w.admin = connectAdmin(t, pool)
	t.Cleanup(w.admin.Close)
	if _, err := w.admin.Exec(ctx, `UPDATE sites SET connection_state = 'connected', agent_version = $2 WHERE tenant_id = $1`,
		w.tenant, agentcmd.MinAgentVersionForPageLayout); err != nil {
		t.Fatalf("arrange sites: %v", err)
	}
	for _, s := range w.sites {
		m155Refresh(t, pool, w.tenant, s, mcp.AbilityPageCreate, mcp.AbilityRestWrite)
	}
	if _, err := abilities.StampOwnEntryHashes(ctx, pool, slog.Default()); err != nil {
		t.Fatalf("stamp entries: %v", err)
	}
	if _, err := abilities.StampOwnRouteHashes(ctx, pool, slog.Default()); err != nil {
		t.Fatalf("stamp routes: %v", err)
	}
	owner := seedUserRow(t, w.admin, "at-owner-"+r+"@example.test")
	seedMembershipRow(t, w.admin, owner, w.tenant)
	if _, err := w.admin.Exec(ctx, `UPDATE users SET name = 'Priya' WHERE id = $1`, owner); err != nil {
		t.Fatalf("name the owner: %v", err)
	}
	w.owner = domain.Principal{Type: domain.PrincipalUser, UserID: owner, TenantID: w.tenant, Scope: domain.ScopeOrg, Role: "owner"}

	w.msvc = mcp.NewService(repo).WithAudit(rec).
		WithContextResolver(&govcontext.Resolver{Store: govcontext.NewRepo(pool)})
	if err := w.msvc.EnableAbilityTools(repo, w.agent, abilities.SendableEntry, "integration-secret"); err != nil {
		t.Fatalf("enable ability tools: %v", err)
	}
	w.msvc.SetRouteEncoder(abilities.SendableRoute)
	if err := w.msvc.EnableAbilityWrites(repo); err != nil {
		t.Fatalf("enable ability writes: %v", err)
	}
	if err := w.msvc.SetWriteToolsEnabled(true); err != nil {
		t.Fatalf("write tools: %v", err)
	}
	w.eng = mountLikeProduction(t, w.msvc, w.owner)

	w.svc = abilityrequest.NewService(pool, repo, w.msvc, rec, slog.Default())
	w.svc.SetWriteToolsEnabled(true)
	w.svc.SetSender(w.agent, abilities.SendableEntry, w.msvc)
	w.svc.SetRouteEncoder(abilities.SendableRoute)
	w.svc.SetEnabler(w.agent)

	// As cmd/wpmgr/main.go wires the engine: the setter checks resolve each
	// person through the session authenticator's own code path.
	sessions := auth.NewSessionManagerWithStore(scs.New(), false)
	authSvc, _ := newAuthStack(pool)
	authn := middleware.NewAuthenticator(sessions, authSvc, apikey.NewService(pool), pool)
	w.svc.SetPolicy(abilityrequest.NewPolicyRepo(pool, rec), authn)
	w.svc.SetDispatchEnqueuer(atSendNow{svc: w.svc})
	w.msvc.SetAbilityDecider(w.svc)
	w.msvc.SetPublicBaseURL(atPublicBase)
	return w
}

// enableAI turns AI editing on for a site as the owner, through the service.
func (w *atWorld) enableAI(t *testing.T, site uuid.UUID) {
	t.Helper()
	if _, err := w.svc.EnableContentEditing(context.Background(), w.owner, site); err != nil {
		t.Fatalf("enable AI editing on %s: %v", site, err)
	}
}

// mint creates a connection over every site through MintConnection, as p.
func (w *atWorld) mint(t *testing.T, p domain.Principal, name string) (uuid.UUID, string) {
	t.Helper()
	caps := []mcp.Capability{mcp.CapSitesRead, mcp.CapAbilityRead, mcp.CapAbilityRequest}
	m, err := w.msvc.MintConnection(context.Background(), mcp.MintConnectionRequest{
		Principal: p, Name: name,
		SiteScope:    mcp.SiteScopeRequest{Mode: mcp.SiteScopeModeList, SiteIDs: w.sites},
		Capabilities: &caps,
	})
	if err != nil {
		t.Fatalf("mint %q: %v", name, err)
	}
	return m.GrantID, m.Token
}

type atGrant struct {
	auto      string
	setBy     *uuid.UUID
	createdBy *uuid.UUID
}

func (w *atWorld) grant(t *testing.T, id uuid.UUID) atGrant {
	t.Helper()
	var g atGrant
	if err := w.admin.QueryRow(context.Background(),
		`SELECT ai_auto, ai_auto_set_by, created_by_user_id FROM mcp_grants WHERE id = $1`, id).
		Scan(&g.auto, &g.setBy, &g.createdBy); err != nil {
		t.Fatalf("read grant: %v", err)
	}
	return g
}

type atMode struct {
	mode, source string
	setBy        *uuid.UUID
	version      int64
}

func (w *atWorld) mode(t *testing.T, site uuid.UUID) atMode {
	t.Helper()
	var m atMode
	if err := w.admin.QueryRow(context.Background(),
		`SELECT ai_mode, ai_mode_source, ai_mode_set_by, ai_mode_version FROM sites WHERE id = $1`, site).
		Scan(&m.mode, &m.source, &m.setBy, &m.version); err != nil {
		t.Fatalf("read site mode: %v", err)
	}
	return m
}

const atPageInput = `{"post_type":"page","editor":"wordpress_blocks","title":"%s","outline":[{"type":"paragraph","text":"Body text"}]}`

// askPage runs site_ability_run for a page over HTTP.
func (w *atWorld) askPage(t *testing.T, bearer string, site uuid.UUID, title string) cpeRPC {
	t.Helper()
	var input map[string]any
	_ = json.Unmarshal([]byte(fmt.Sprintf(atPageInput, title)), &input)
	return cpeCall(t, w.eng, bearer, mcp.ToolSiteAbilityRun, map[string]any{
		"site_id": site.String(), "name": mcp.AbilityPageCreate, "input": input,
	})
}

// askRetitle runs site_ability_run for a REST retitle of post over HTTP.
func (w *atWorld) askRetitle(t *testing.T, bearer string, site uuid.UUID, post int64, title string) cpeRPC {
	t.Helper()
	return cpeCall(t, w.eng, bearer, mcp.ToolSiteAbilityRun, map[string]any{
		"site_id": site.String(), "name": mcp.AbilityRestWrite,
		"input": map[string]any{"route_id": atRoutePages, "path": map[string]any{"id": post}, "body": map[string]any{"title": title}},
	})
}

type atRow struct {
	state, approvalSource                     string
	baseClass, class, askReason, targetStatus *string
	modeSource                                *string
	setter, decidedBy                         *uuid.UUID
	createdPost                               *int64
}

func (w *atWorld) row(t *testing.T, id string) atRow {
	t.Helper()
	var r atRow
	if err := w.admin.QueryRow(context.Background(), `
SELECT state, approval_source, base_change_class, change_class, ask_reason, checked_target_status,
       approval_mode_source, approval_setter_user_id, decided_by_user_id, created_post_id
FROM assistant_ability_requests WHERE id = $1`, id).Scan(&r.state, &r.approvalSource, &r.baseClass, &r.class,
		&r.askReason, &r.targetStatus, &r.modeSource, &r.setter, &r.decidedBy, &r.createdPost); err != nil {
		t.Fatalf("read request %s: %v", id, err)
	}
	return r
}

func atStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// wantRanAutomatically fails unless the call's answer says the change ran
// under the site's setting and finished.
func wantRanAutomatically(t *testing.T, what string, res map[string]any) string {
	t.Helper()
	if res["approval"] != "auto" || res["state"] != "done" {
		t.Fatalf("%s: want approval auto and state done, got approval %v state %v ask_reason %v\n%v",
			what, res["approval"], res["state"], res["ask_reason"], res)
	}
	if res["message"] != aipolicy.MessageDoneBySetting {
		t.Fatalf("%s: message = %v, want the done-by-setting message", what, res["message"])
	}
	return res["request_id"].(string)
}

// wantWaits fails unless the call's answer says the change waits for a
// person for reason, with the absolute approval_url.
func wantWaits(t *testing.T, what string, res map[string]any, reason aipolicy.AskReason) string {
	t.Helper()
	id, _ := res["request_id"].(string)
	if res["state"] != "waiting_for_approval" || res["approval"] != "ask" || res["ask_reason"] != string(reason) {
		t.Fatalf("%s: want waiting_for_approval, approval ask, ask_reason %s; got state %v approval %v ask_reason %v\n%v",
			what, reason, res["state"], res["approval"], res["ask_reason"], res)
	}
	if res["approval_url"] != atPublicBase+"/ai/requests?request="+id {
		t.Fatalf("%s: approval_url = %v, want the absolute link to the request", what, res["approval_url"])
	}
	if res["message"] != reason.Message() {
		t.Fatalf("%s: message = %v, want the constant for %s", what, res["message"], reason)
	}
	return id
}

// TestPersonMintedConnectionFirstDraftRuns: a connection a signed-in person
// mints starts on site_setting with that person as its setter, and its
// first draft on a site with AI editing on runs at once under the site's
// setting.
func TestPersonMintedConnectionFirstDraftRuns(t *testing.T) {
	w := newATWorld(t, 1)
	site := w.sites[0]
	w.enableAI(t, site)
	if m := w.mode(t, site); m.mode != "ai_drafts" || m.source != "enable_default" || m.setBy == nil || *m.setBy != w.owner.UserID {
		t.Fatalf("enable default: got %+v, want ai_drafts/enable_default set by the owner", m)
	}
	grantID, bearer := w.mint(t, w.owner, "Priya's laptop")
	if g := w.grant(t, grantID); g.auto != "site_setting" || g.setBy == nil || *g.setBy != w.owner.UserID {
		t.Fatalf("person-minted connection: got %+v, want site_setting set by the owner", g)
	}

	id := wantRanAutomatically(t, "first draft", w.askPage(t, bearer, site, "Spring sale").wantOK(t, "first draft"))
	r := w.row(t, id)
	if r.state != "done" || r.approvalSource != "policy" || r.decidedBy != nil || atStr(r.class) != "ai_draft" ||
		atStr(r.modeSource) != "enable_default" || r.setter == nil || *r.setter != w.owner.UserID || r.targetStatus != nil {
		t.Fatalf("first draft row: %+v class %s mode source %s target status %s", r, atStr(r.class), atStr(r.modeSource), atStr(r.targetStatus))
	}
	if w.agent.writeCount() != 1 {
		t.Fatalf("writes sent = %d, want 1", w.agent.writeCount())
	}
}

// TestKeyMintedConnectionFirstWriteWaits: a connection minted with an API
// key starts on never, and its first draft waits with connection_never_auto
// even on a site whose setting would run it.
func TestKeyMintedConnectionFirstWriteWaits(t *testing.T) {
	w := newATWorld(t, 1)
	site := w.sites[0]
	w.enableAI(t, site)
	key := domain.Principal{Type: domain.PrincipalAPIKey, APIKeyID: uuid.New(), TenantID: w.tenant, Scope: domain.ScopeOrg, Role: "owner"}
	grantID, bearer := w.mint(t, key, "Build server")
	if g := w.grant(t, grantID); g.auto != "never" || g.setBy != nil || g.createdBy != nil {
		t.Fatalf("key-minted connection: got %+v, want never with no setter and no creator", g)
	}

	id := wantWaits(t, "key-minted first draft", w.askPage(t, bearer, site, "Spring sale").wantOK(t, "key-minted first draft"),
		aipolicy.AskConnectionNeverAuto)
	if r := w.row(t, id); r.state != "pending" || atStr(r.askReason) != "connection_never_auto" || atStr(r.class) != "ai_draft" {
		t.Fatalf("key-minted first draft row: %+v class %s ask %s", r, atStr(r.class), atStr(r.askReason))
	}
	if w.agent.writeCount() != 0 {
		t.Fatalf("writes sent = %d, want 0", w.agent.writeCount())
	}
}

// TestConsentedConnectionStartsOnTheCreatorsChoice: the consent path records
// the switch the same way: a signed-in person's consent starts on
// site_setting with that person as setter, a consent approved with an API
// key starts on never.
func TestConsentedConnectionStartsOnTheCreatorsChoice(t *testing.T) {
	ctx := context.Background()
	w := newATWorld(t, 1)
	clientID := "at-client-" + uuid.NewString()
	secretHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if n, err := mcp.NewRepo(w.pool).RegisterClient(ctx, sqlc.RegisterMCPOAuthClientParams{
		ClientID: clientID, ClientSecretHash: &secretHash, TokenEndpointAuthMethod: "client_secret_basic",
		RedirectUris: []string{"https://claude.ai/api/mcp/auth_callback"}, RegisteredScopes: mcp.SupportedScopes(),
	}); err != nil || n != 1 {
		t.Fatalf("register client: n=%d err=%v", n, err)
	}
	key := domain.Principal{Type: domain.PrincipalAPIKey, APIKeyID: uuid.New(), TenantID: w.tenant, Scope: domain.ScopeOrg, Role: "owner"}
	for _, tc := range []struct {
		name      string
		p         domain.Principal
		wantAuto  string
		wantSetBy *uuid.UUID
	}{
		{"signed-in person", w.owner, "site_setting", &w.owner.UserID},
		{"api key", key, "never", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			consent, err := w.msvc.Authorize(ctx, mcp.AuthorizeRequest{
				ResponseType: "code", ClientID: clientID, RedirectURI: "https://claude.ai/api/mcp/auth_callback",
				Scope: string(mcp.ScopeRead), CodeChallenge: "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM",
				CodeChallengeMethod: "S256",
			})
			if err != nil {
				t.Fatalf("authorize: %v", err)
			}
			ap, err := w.msvc.Approve(ctx, mcp.ApprovalRequest{
				Principal: tc.p, GrantName: "Consent " + tc.name,
				SiteScope: mcp.SiteScopeRequest{Mode: mcp.SiteScopeModeAll}, Consent: consent,
			})
			if err != nil {
				t.Fatalf("approve consent: %v", err)
			}
			g := w.grant(t, ap.GrantID)
			if g.auto != tc.wantAuto || (g.setBy == nil) != (tc.wantSetBy == nil) || (g.setBy != nil && *g.setBy != *tc.wantSetBy) {
				t.Fatalf("consented connection: got %+v, want %s set by %v", g, tc.wantAuto, tc.wantSetBy)
			}
		})
	}
}

// TestReEnableKeepsChosenMode: turning AI editing on gives a site the drafts
// default only while no mode was ever set; turning it on again keeps a mode
// a person chose, Ask and Full auto alike.
func TestReEnableKeepsChosenMode(t *testing.T) {
	ctx := context.Background()
	w := newATWorld(t, 1)
	site := w.sites[0]
	if m := w.mode(t, site); m.mode != "ask" || m.source != "unset" || m.version != 0 {
		t.Fatalf("before enabling: %+v, want ask/unset v0", m)
	}
	w.enableAI(t, site)
	if m := w.mode(t, site); m.mode != "ai_drafts" || m.source != "enable_default" || m.setBy == nil ||
		*m.setBy != w.owner.UserID || m.version != 1 {
		t.Fatalf("first enable: %+v, want ai_drafts/enable_default by the owner v1", m)
	}
	var md map[string]any
	if err := w.admin.QueryRow(ctx, `SELECT metadata FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3
		ORDER BY created_at DESC LIMIT 1`, w.tenant, audit.ActionSiteContentEditingEnabled, site.String()).Scan(&md); err != nil {
		t.Fatalf("read enable audit row: %v", err)
	}
	if md["ai_mode"] != "ai_drafts" || md["ai_mode_source"] != "enable_default" {
		t.Fatalf("enable audit metadata = %v, want the mode it set", md)
	}

	// A person chooses a mode through the shipped compare-and-set, in their
	// own tenant transaction, as the settings route runs it.
	set := func(mode, step string, expected int64) {
		t.Helper()
		var stepUp *string
		if step != "" {
			stepUp = &step
		}
		err := w.pool.RunTenantTx(ctx, w.owner, func(tx pgx.Tx) error {
			row, err := sqlc.New(tx).SetSiteAIMode(ctx, sqlc.SetSiteAIModeParams{
				Mode: mode, Source: "person", SetBy: m174UUID(w.owner.UserID), StepUp: stepUp,
				TenantID: w.tenant, SiteID: site, ExpectedVersion: expected,
			})
			if err == nil && !row.Applied {
				err = fmt.Errorf("not applied: stored version %d", row.AiModeVersion)
			}
			return err
		})
		if err != nil {
			t.Fatalf("set %s: %v", mode, err)
		}
	}
	for _, tc := range []struct {
		mode, step string
	}{{"ask", ""}, {"full", "password"}} {
		before := w.mode(t, site)
		set(tc.mode, tc.step, before.version)
		chosen := w.mode(t, site)
		if chosen.mode != tc.mode || chosen.source != "person" {
			t.Fatalf("chosen %s: %+v", tc.mode, chosen)
		}
		w.enableAI(t, site)
		again := w.mode(t, site)
		if again.mode != chosen.mode || again.source != chosen.source || again.version != chosen.version ||
			again.setBy == nil || *again.setBy != w.owner.UserID {
			t.Fatalf("re-enable reset a chosen %s: before %+v after %+v", tc.mode, chosen, again)
		}
	}
}

// TestRestWriteOnAIDraftRunsByTargetStatus: the two page and post retitle
// routes are stored by_target_status. An edit to a draft the AI made on this
// site, whose checked status is exactly draft, runs at once on Auto for AI
// drafts and ends done, approved by the site's setting.
func TestRestWriteOnAIDraftRunsByTargetStatus(t *testing.T) {
	w := newATWorld(t, 1)
	site := w.sites[0]
	w.enableAI(t, site)
	_, bearer := w.mint(t, w.owner, "Priya's laptop")

	pageID := wantRanAutomatically(t, "draft", w.askPage(t, bearer, site, "Spring sale").wantOK(t, "draft"))
	post := w.row(t, pageID).createdPost
	if post == nil {
		t.Fatal("the draft recorded no created post")
	}
	id := wantRanAutomatically(t, "retitle the AI draft",
		w.askRetitle(t, bearer, site, *post, "Spring sale 2").wantOK(t, "retitle the AI draft"))
	r := w.row(t, id)
	if r.state != "done" || r.approvalSource != "policy" || r.decidedBy != nil || atStr(r.baseClass) != "by_target_status" ||
		atStr(r.class) != "ai_draft" || atStr(r.targetStatus) != "draft" {
		t.Fatalf("retitle row: state %s source %s base %s class %s target status %s",
			r.state, r.approvalSource, atStr(r.baseClass), atStr(r.class), atStr(r.targetStatus))
	}
	if w.agent.writeCount() != 2 {
		t.Fatalf("writes sent = %d, want 2", w.agent.writeCount())
	}
}

// TestRestWritePublishedWaitsOnAIDrafts: an edit to a published page is a
// change visitors see; on Auto for AI drafts it waits for a person with
// kind_not_in_mode, and nothing is sent to the site.
func TestRestWritePublishedWaitsOnAIDrafts(t *testing.T) {
	w := newATWorld(t, 1)
	site := w.sites[0]
	w.enableAI(t, site)
	_, bearer := w.mint(t, w.owner, "Priya's laptop")
	w.agent.setStatus(412, "publish")

	id := wantWaits(t, "retitle a published page",
		w.askRetitle(t, bearer, site, 412, "Spring sale").wantOK(t, "retitle a published page"), aipolicy.AskKindNotInMode)
	if r := w.row(t, id); r.state != "pending" || atStr(r.class) != "live" || atStr(r.targetStatus) != "publish" ||
		atStr(r.askReason) != "kind_not_in_mode" {
		t.Fatalf("published retitle row: state %s class %s target status %s ask %s",
			r.state, atStr(r.class), atStr(r.targetStatus), atStr(r.askReason))
	}
	if w.agent.writeCount() != 0 {
		t.Fatalf("writes sent = %d, want 0", w.agent.writeCount())
	}
}

// TestRestWriteClassByStatus: through the real insert path, the raw checked
// status classes a by_target_status write, compared exactly. The site is on
// Auto for AI drafts, so only an AI draft runs; every other class waits.
func TestRestWriteClassByStatus(t *testing.T) {
	ctx := context.Background()
	w := newATWorld(t, 1)
	site := w.sites[0]
	w.enableAI(t, site)
	_, bearer := w.mint(t, w.owner, "Priya's laptop")

	aiDraft := func(title string) int64 {
		t.Helper()
		id := wantRanAutomatically(t, "draft "+title, w.askPage(t, bearer, site, title).wantOK(t, "draft "+title))
		p := w.row(t, id).createdPost
		if p == nil {
			t.Fatalf("draft %s recorded no created post", title)
		}
		return *p
	}
	scheduledAIDraft := aiDraft("Later sale")
	w.agent.setStatus(scheduledAIDraft, "future")

	for i, tc := range []struct {
		name, status string
		post         int64
		class        string // "" when the target state is unknown
		ask          aipolicy.AskReason
	}{
		{"published", "publish", 501, "live", aipolicy.AskKindNotInMode},
		{"private", "private", 502, "live", aipolicy.AskKindNotInMode},
		{"scheduled", "future", 503, "publish", aipolicy.AskKindNotInMode},
		{"scheduled AI draft", "future", scheduledAIDraft, "publish", aipolicy.AskKindNotInMode},
		{"pending review", "pending", 504, "unpublished", aipolicy.AskKindNotInMode},
		{"a person's draft", "draft", 505, "unpublished", aipolicy.AskKindNotInMode},
		{"auto-draft", "auto-draft", 506, "", aipolicy.AskUnknownTargetState},
		{"capitalised Draft", "Draft", 507, "", aipolicy.AskUnknownTargetState},
		{"draft with a trailing space", "draft ", 508, "", aipolicy.AskUnknownTargetState},
		{"empty status", "", 509, "", aipolicy.AskUnknownTargetState},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.post != scheduledAIDraft {
				w.agent.setStatus(tc.post, tc.status)
			}
			title := fmt.Sprintf("Retitle %d", i)
			id := wantWaits(t, tc.name, w.askRetitle(t, bearer, site, tc.post, title).wantOK(t, tc.name), tc.ask)
			r := w.row(t, id)
			if r.targetStatus == nil || *r.targetStatus != tc.status {
				t.Fatalf("checked_target_status = %s, want exactly %q", atStr(r.targetStatus), tc.status)
			}
			wantClass := tc.class
			if wantClass == "" {
				wantClass = "<nil>"
			}
			if r.state != "pending" || atStr(r.baseClass) != "by_target_status" || atStr(r.class) != wantClass ||
				atStr(r.askReason) != string(tc.ask) {
				t.Fatalf("row: state %s base %s class %s ask %s, want pending by_target_status %s %s",
					r.state, atStr(r.baseClass), atStr(r.class), atStr(r.askReason), wantClass, tc.ask)
			}
			// Free the waiting caps for the next case, as a person would.
			if _, err := w.svc.Decline(ctx, w.owner, site, uuid.MustParse(id)); err != nil {
				t.Fatalf("decline: %v", err)
			}
		})
	}

	t.Run("an AI draft", func(t *testing.T) {
		post := aiDraft("Summer sale")
		id := wantRanAutomatically(t, "retitle the AI draft",
			w.askRetitle(t, bearer, site, post, "Summer sale 2").wantOK(t, "retitle the AI draft"))
		if r := w.row(t, id); atStr(r.class) != "ai_draft" || atStr(r.targetStatus) != "draft" || r.approvalSource != "policy" {
			t.Fatalf("AI draft row: class %s target status %s source %s", atStr(r.class), atStr(r.targetStatus), r.approvalSource)
		}
	})
}

// atListBody is the request list's body, each request kept raw so it can
// be read both as the generated contract type and key by key.
type atListBody struct {
	Requests []json.RawMessage `json:"requests"`
}

func (w *atWorld) listAs(t *testing.T, p domain.Principal, path string) map[string]gen.AbilityRequest {
	t.Helper()
	gin.SetMode(gin.TestMode)
	api := gin.New()
	g := api.Group("/api/v1")
	g.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	})
	abilityrequest.NewHandler(w.svc).Register(g)
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
	}
	var body atListBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	out := map[string]gen.AbilityRequest{}
	for _, raw := range body.Requests {
		var keys map[string]json.RawMessage
		_ = json.Unmarshal(raw, &keys)
		for _, k := range []string{"approval", "change_class", "change_kind_name", "ask_reason"} {
			if _, ok := keys[k]; !ok {
				t.Fatalf("GET %s: a request has no %q member: %s", path, k, raw)
			}
		}
		var ar gen.AbilityRequest
		if err := ar.UnmarshalJSON(raw); err != nil {
			t.Fatalf("GET %s: a request does not decode as the contract's AbilityRequest: %v\n%s", path, err, raw)
		}
		if err := ar.Validate(); err != nil {
			t.Fatalf("GET %s: a request breaks the contract's AbilityRequest: %v\n%s", path, err, raw)
		}
		out[ar.ID.String()] = ar
	}
	return out
}

// TestAbilityRequestListSaysHowItWasApproved: the request list, through the
// real handler as wpmgr_app, says how each request was approved, its kind
// and why it waits, per the AbilityRequest contract: a change the site's
// setting ran names that setting as recorded on the request; a person's
// approval names no setting; a waiting change has no approval; and a setter
// whose account was later deleted is reported as such.
func TestAbilityRequestListSaysHowItWasApproved(t *testing.T) {
	ctx := context.Background()
	w := newATWorld(t, 1)
	site := w.sites[0]
	w.enableAI(t, site)
	_, bearer := w.mint(t, w.owner, "Priya's laptop")
	key := domain.Principal{Type: domain.PrincipalAPIKey, APIKeyID: uuid.New(), TenantID: w.tenant, Scope: domain.ScopeOrg, Role: "owner"}
	_, keyBearer := w.mint(t, key, "Build server")

	auto := wantRanAutomatically(t, "draft", w.askPage(t, bearer, site, "Spring sale").wantOK(t, "draft"))
	w.agent.setStatus(412, "publish")
	waiting := wantWaits(t, "published retitle", w.askRetitle(t, bearer, site, 412, "New title").wantOK(t, "published retitle"),
		aipolicy.AskKindNotInMode)
	byPerson := wantWaits(t, "key-minted draft", w.askPage(t, keyBearer, site, "Autumn sale").wantOK(t, "key-minted draft"),
		aipolicy.AskConnectionNeverAuto)
	rows, err := w.svc.List(ctx, w.owner, &site, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var digest string
	for _, r := range rows {
		if r.ID.String() == byPerson {
			digest = r.PresentedDigest
		}
	}
	if _, err := w.svc.Approve(ctx, w.owner, site, uuid.MustParse(byPerson), digest); err != nil {
		t.Fatalf("approve as a person: %v", err)
	}

	// A second owner chooses Auto for AI drafts as a person, and a draft runs
	// under that choice; then that person's account is deleted.
	second := seedUserRow(t, w.admin, "at-second-"+uuid.NewString()[:8]+"@example.test")
	seedMembershipRow(t, w.admin, second, w.tenant)
	secondP := domain.Principal{Type: domain.PrincipalUser, UserID: second, TenantID: w.tenant, Scope: domain.ScopeOrg, Role: "owner"}
	if err := w.pool.RunTenantTx(ctx, secondP, func(tx pgx.Tx) error {
		row, err := sqlc.New(tx).SetSiteAIMode(ctx, sqlc.SetSiteAIModeParams{
			Mode: "ai_drafts", Source: "person", SetBy: m174UUID(second), TenantID: w.tenant, SiteID: site,
			ExpectedVersion: w.mode(t, site).version,
		})
		if err == nil && !row.Applied {
			err = fmt.Errorf("not applied")
		}
		return err
	}); err != nil {
		t.Fatalf("second owner chooses the mode: %v", err)
	}
	underSecond := wantRanAutomatically(t, "draft under the second owner's choice",
		w.askPage(t, bearer, site, "Winter sale").wantOK(t, "draft under the second owner's choice"))
	if _, err := w.admin.Exec(ctx, `DELETE FROM users WHERE id = $1`, second); err != nil {
		t.Fatalf("delete the second owner's account: %v", err)
	}

	for _, path := range []string{
		"/api/v1/sites/" + site.String() + "/ai/ability-requests?limit=100",
		"/api/v1/ai/ability-requests?limit=100",
	} {
		got := w.listAs(t, w.owner, path)

		a, ok := got[auto]
		if !ok {
			t.Fatalf("%s: the automatic change is not listed", path)
		}
		ap, ok := a.Approval.Get()
		if !ok || ap.Source != gen.AbilityRequestApprovalSourcePolicy {
			t.Fatalf("%s: automatic change approval = %+v, want source policy", path, a.Approval)
		}
		st, ok := ap.Setting.Get()
		if !ok || st.Mode != gen.AIApprovalSettingModeAiDrafts || st.Source != gen.AIApprovalSettingSourceEnableDefault ||
			st.SetByUserID.Or(uuid.Nil) != w.owner.UserID || st.SetByName.Or("") != "Priya" || st.SetByAccountDeleted ||
			!st.SetAt.IsSet() || st.SetAt.IsNull() {
			t.Fatalf("%s: automatic change setting = %+v", path, ap.Setting)
		}
		if a.ChangeClass.Or("") != gen.AIChangeClassAiDraft || a.ChangeKindName.Or("") != aipolicy.ClassAIDraft.KindName() ||
			!a.AskReason.IsNull() {
			t.Fatalf("%s: automatic change class %+v kind %+v ask %+v", path, a.ChangeClass, a.ChangeKindName, a.AskReason)
		}

		wt := got[waiting]
		if !wt.Approval.IsNull() || wt.ChangeClass.Or("") != gen.AIChangeClassLive ||
			wt.ChangeKindName.Or("") != aipolicy.ClassLive.KindName() || wt.AskReason.Or("") != gen.AIAskReasonKindNotInMode {
			t.Fatalf("%s: waiting change approval %+v class %+v kind %+v ask %+v", path, wt.Approval, wt.ChangeClass,
				wt.ChangeKindName, wt.AskReason)
		}

		bp := got[byPerson]
		pap, ok := bp.Approval.Get()
		if !ok || pap.Source != gen.AbilityRequestApprovalSourcePerson || !pap.Setting.IsNull() ||
			bp.AskReason.Or("") != gen.AIAskReasonConnectionNeverAuto {
			t.Fatalf("%s: person's approval %+v ask %+v", path, bp.Approval, bp.AskReason)
		}

		us := got[underSecond]
		uap, _ := us.Approval.Get()
		ust, ok := uap.Setting.Get()
		if !ok || ust.Source != gen.AIApprovalSettingSourcePerson || ust.SetByUserID.Or(uuid.Nil) != second ||
			!ust.SetByName.IsNull() || !ust.SetByAccountDeleted {
			t.Fatalf("%s: setting of a deleted setter = %+v", path, uap.Setting)
		}
	}
}

// TestDraftBudget: the creation caps are sized for draft work that runs on
// its own, and the draft budget falls back to Ask instead of refusing.
//
//   - More than the old twelve drafts on one site in an hour each run at
//     once over HTTP, none refused.
//   - With changes on 30 sites in the window, a draft on a 31st site is
//     created and waits with over_site_cap.
//   - With 600 changes in the window, the next draft is created and waits
//     with over_change_budget and says when automatic changes resume.
//
// The transport limits one connection to a burst of tool calls, so the
// changes that fill the window between the HTTP calls are made through the
// same insert and the same Decide a tool call runs, as wpmgr_app.
func TestDraftBudget(t *testing.T) {
	ctx := context.Background()
	w := newATWorld(t, aipolicy.DraftSitesPerConnection+1)
	for _, s := range w.sites {
		w.enableAI(t, s)
	}
	grantID, bearer := w.mint(t, w.owner, "Priya's laptop")
	first := w.sites[0]

	const overOldSiteCap = 13
	var sample string
	for i := 0; i < overOldSiteCap; i++ {
		res := w.askPage(t, bearer, first, fmt.Sprintf("Sale %d", i))
		if res.code != 0 {
			t.Fatalf("draft %d on one site was refused: code %d %q %v", i+1, res.code, res.msg, res.data)
		}
		sample = wantRanAutomatically(t, fmt.Sprintf("draft %d", i+1), res.result)
	}

	// The connection's further changes, through the production statements.
	var entryID uuid.UUID
	var entrySum, perm string
	if err := w.admin.QueryRow(ctx, `SELECT entry_id, entry_sha256, operator_permission FROM assistant_ability_requests WHERE id = $1`,
		sample).Scan(&entryID, &entrySum, &perm); err != nil {
		t.Fatalf("read the page entry: %v", err)
	}
	w.svc.SetDispatchEnqueuer(nil)
	seeded := 0
	approve := func(site uuid.UUID) {
		t.Helper()
		seeded++
		arg := aarParams(w.tenant, site, grantID, fmt.Sprintf("budget-%d", seeded))
		arg.EntryID, arg.EntrySha256, arg.OperatorPermission = entryID, entrySum, perm
		row := aarInsert(t, w.pool, acprSitePrincipal(w.tenant, site), arg)
		res, err := w.svc.Decide(ctx, w.tenant, row.ID)
		if err != nil || res.Outcome != aipolicy.OutcomeAutoBySetting {
			t.Fatalf("seeded change %d on %s: outcome %s ask %s err %v", seeded, site, res.Outcome, res.Ask, err)
		}
	}
	for _, s := range w.sites[1:aipolicy.DraftSitesPerConnection] {
		approve(s)
	}
	w.svc.SetDispatchEnqueuer(atSendNow{svc: w.svc})

	last := w.sites[aipolicy.DraftSitesPerConnection]
	id := wantWaits(t, "a draft on a 31st site", w.askPage(t, bearer, last, "Sale on a new site").wantOK(t, "a draft on a 31st site"),
		aipolicy.AskOverSiteCap)
	if r := w.row(t, id); r.state != "pending" || atStr(r.askReason) != "over_site_cap" {
		t.Fatalf("31st site row: state %s ask %s", r.state, atStr(r.askReason))
	}

	w.svc.SetDispatchEnqueuer(nil)
	counted := overOldSiteCap + aipolicy.DraftSitesPerConnection - 1
	for i := 0; counted < aipolicy.DraftChangesPerConnection; i++ {
		approve(w.sites[i%aipolicy.DraftSitesPerConnection])
		counted++
	}
	w.svc.SetDispatchEnqueuer(atSendNow{svc: w.svc})
	var policy int
	if err := w.admin.QueryRow(ctx, `SELECT count(*) FROM assistant_ability_requests
		WHERE proposed_by_grant_id = $1 AND approval_source = 'policy'`, grantID).Scan(&policy); err != nil {
		t.Fatalf("count: %v", err)
	}
	if policy != aipolicy.DraftChangesPerConnection {
		t.Fatalf("changes approved by the setting = %d, want %d", policy, aipolicy.DraftChangesPerConnection)
	}

	res := w.askPage(t, bearer, w.sites[1], "One more sale").wantOK(t, "the change past the budget")
	id = wantWaits(t, "the change past the budget", res, aipolicy.AskOverChangeBudget)
	resumes, err := time.Parse(time.RFC3339, fmt.Sprint(res["auto_resumes_at"]))
	if err != nil || !resumes.After(time.Now()) || resumes.After(time.Now().Add(aipolicy.BudgetWindow)) {
		t.Fatalf("auto_resumes_at = %v (%v), want within the next hour", res["auto_resumes_at"], err)
	}
	if r := w.row(t, id); r.state != "pending" || atStr(r.askReason) != "over_change_budget" {
		t.Fatalf("budget row: state %s ask %s", r.state, atStr(r.askReason))
	}
	if !strings.Contains(fmt.Sprint(res["message"]), "Do not split the work") {
		t.Fatalf("message = %v", res["message"])
	}
}
