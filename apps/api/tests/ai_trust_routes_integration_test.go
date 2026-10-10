package tests

// ai_trust_routes_integration_test.go: the AI trust settings routes (site
// mode, connection switch) as production serves them.
//
// Every request goes through the real router (server.New) over the pool
// startPostgres returns (wpmgr_app: NOSUPERUSER, NOBYPASSRLS), authenticated
// by a session, an owner-role API key or an MCP bearer token, the way
// production authenticates each. The bootstrap superuser only seeds the
// site's reported agent version, a value the agent normally reports.
//
// NOT RUN BY CI. Run with `make test-integration` from the repository root.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/abilityrequest"
	"github.com/mosamlife/wpmgr/apps/api/internal/aipolicy"
	"github.com/mosamlife/wpmgr/apps/api/internal/aitrust"
	"github.com/mosamlife/wpmgr/apps/api/internal/apikey"
	"github.com/mosamlife/wpmgr/apps/api/internal/assistantrequest"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/config"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/internal/middleware"
	"github.com/mosamlife/wpmgr/apps/api/internal/server"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/tenant"
)

// aitrStack is the loosen stack plus the AI trust handler, wired as
// cmd/wpmgr wires it.
type aitrStack struct {
	*loosenStack
	mcpSvc *mcp.Service
}

func newAITrustStack(t *testing.T) *aitrStack {
	t.Helper()
	pool := startPostgres(t)
	clock := domain.SystemClock{}
	validator := domain.NewValidator()
	rec := audit.NewRecorder(pool, clock)
	authRepo := auth.NewRepo(pool)
	authSvc := auth.NewService(authRepo, rec, validator)
	sessions := auth.NewSessionManagerWithStore(scs.New(), false)
	keys := apikey.NewService(pool)
	authn := middleware.NewAuthenticator(sessions, authSvc, keys, pool)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mcpRepo := mcp.NewRepo(pool)
	mcpSvc := mcp.NewService(mcpRepo).WithAudit(rec)
	abilityReqH := abilityrequest.NewHandler(abilityrequest.NewService(pool, mcpRepo, mcpSvc, rec, logger))
	assistantReqH := assistantrequest.NewHandler(assistantrequest.NewService(
		assistantrequest.NewRepo(pool), mcpRepo, mcpSvc, rec, logger))
	trustSvc := aitrust.NewService(aitrust.NewRepo(pool, rec), authn, logger)
	trustSvc.SetRenderers(abilityReqH, assistantReqH)

	srv := server.New(server.Deps{
		Config:   config.Config{},
		Logger:   logger,
		Pool:     pool,
		Sessions: sessions,
		Auth:     authn,
		AuthH:    auth.NewHandler(authSvc, sessions, nil, nil),
		MembersH: auth.NewMembersHandler(authSvc, nil),
		APIKeyH:  apikey.NewHandler(keys, rec),
		AuditH:   audit.NewHandler(rec),
		TenantH:  tenant.NewHandler(tenant.NewService(tenant.NewRepo(pool), validator, clock), rec),
		SiteH:    site.NewHandler(site.NewService(site.NewRepo(pool), validator, clock), rec, ""),
		AITrustH: aitrust.NewHandler(trustSvc),
	})
	e, ok := srv.Handler().(*gin.Engine)
	if !ok {
		t.Fatalf("server.Handler() is %T, want *gin.Engine", srv.Handler())
	}
	base := &loosenStack{pool: pool, rec: rec, authRepo: authRepo, sessions: sessions, keys: keys, engine: e}
	return &aitrStack{loosenStack: base, mcpSvc: mcpSvc}
}

// aitrWorld is one organisation with an owner, an owner-role API key, a site
// with AI editing on (Auto for AI drafts, chosen by the owner) and a
// connection the owner created.
type aitrWorld struct {
	org, siteID, grantID uuid.UUID
	owner                auth.User
	keyToken, mcpToken   string
	person               context.Context
}

func (s *aitrStack) world(t *testing.T) aitrWorld {
	t.Helper()
	ctx := context.Background()
	sfx := uuid.NewString()[:8]
	w := aitrWorld{org: seedTenant(t, s.pool, "aitr-"+sfx)}
	w.owner = seedUserMembership(t, s.authRepo, "aitr-owner-"+sfx+"@example.com", w.org, authz.RoleOwner)
	key, err := s.keys.Create(ctx, w.org, "aitr-key-"+sfx, authz.RoleOwner)
	if err != nil {
		t.Fatalf("create API key: %v", err)
	}
	w.keyToken = key.Token
	st, err := site.NewRepo(s.pool).Create(ctx, site.CreateInput{
		TenantID: w.org, URL: "https://aitr-" + sfx + ".example.com", Name: "aitr-" + sfx,
	})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}
	w.siteID = st.ID
	admin := connectAdmin(t, s.pool)
	if _, err := admin.Pool.Exec(ctx, `UPDATE sites SET agent_version = $1 WHERE id = $2`,
		aitrust.MinAgentVersion, w.siteID); err != nil {
		t.Fatalf("seed the agent version: %v", err)
	}
	admin.Close()
	if got := m174EnableAI(t, s.pool, w.org, w.siteID, w.owner.ID); got.AiMode != string(aipolicy.ModeAIDrafts) {
		t.Fatalf("enable AI editing: mode %q, want ai_drafts", got.AiMode)
	}
	minted, err := s.mcpSvc.MintConnection(ctx, mcp.MintConnectionRequest{
		Principal: m174UserPrincipal(w.org, w.owner.ID),
		Name:      "aitr connection",
		SiteScope: mcp.SiteScopeRequest{Mode: mcp.SiteScopeModeAll},
	})
	if err != nil {
		t.Fatalf("mint a connection: %v", err)
	}
	w.grantID, w.mcpToken = minted.GrantID, minted.Token
	if err := s.pool.InTenantTx(ctx, w.org, func(tx pgx.Tx) error {
		requireAppRoleInTx(t, ctx, tx)
		return nil
	}); err != nil {
		t.Fatalf("open tenant tx: %v", err)
	}
	w.person = s.session(t, w.owner.ID, w.org)
	return w
}

type aitrMode struct {
	Mode        string     `json:"mode"`
	Source      string     `json:"source"`
	Version     int64      `json:"version"`
	SetByUserID *uuid.UUID `json:"set_by_user_id"`
	SetterValid bool       `json:"setter_valid"`
}

type aitrAuto struct {
	AIAuto          string     `json:"ai_auto"`
	AutoSetByUserID *uuid.UUID `json:"auto_set_by_user_id"`
	AutoSetterValid bool       `json:"auto_setter_valid"`
}

// modeChanges names every field of the site's mode that differs between two
// reads: the mode, its source, its version, its setter and whether that
// setter still holds the authority. An empty result means nothing changed.
//
// Each read decodes its own SetByUserID pointer, so the setter is compared by
// the id it points to. Comparing the structs with == compares the two
// addresses and reports a change whenever a setter is recorded.
func modeChanges(before, after aitrMode) []string {
	var d []string
	if before.Mode != after.Mode {
		d = append(d, fmt.Sprintf("mode %q -> %q", before.Mode, after.Mode))
	}
	if before.Source != after.Source {
		d = append(d, fmt.Sprintf("source %q -> %q", before.Source, after.Source))
	}
	if before.Version != after.Version {
		d = append(d, fmt.Sprintf("version %d -> %d", before.Version, after.Version))
	}
	if !sameUserID(before.SetByUserID, after.SetByUserID) {
		d = append(d, fmt.Sprintf("set_by_user_id %s -> %s", userIDText(before.SetByUserID), userIDText(after.SetByUserID)))
	}
	if before.SetterValid != after.SetterValid {
		d = append(d, fmt.Sprintf("setter_valid %t -> %t", before.SetterValid, after.SetterValid))
	}
	return d
}

// autoChanges is modeChanges for a connection's switch: the switch, its
// setter (compared by id) and whether that setter still holds the authority.
func autoChanges(before, after aitrAuto) []string {
	var d []string
	if before.AIAuto != after.AIAuto {
		d = append(d, fmt.Sprintf("ai_auto %q -> %q", before.AIAuto, after.AIAuto))
	}
	if !sameUserID(before.AutoSetByUserID, after.AutoSetByUserID) {
		d = append(d, fmt.Sprintf("auto_set_by_user_id %s -> %s",
			userIDText(before.AutoSetByUserID), userIDText(after.AutoSetByUserID)))
	}
	if before.AutoSetterValid != after.AutoSetterValid {
		d = append(d, fmt.Sprintf("auto_setter_valid %t -> %t", before.AutoSetterValid, after.AutoSetterValid))
	}
	return d
}

// sameUserID compares two decoded ids by value: both absent, or both present
// and equal.
func sameUserID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func userIDText(id *uuid.UUID) string {
	if id == nil {
		return "null"
	}
	return id.String()
}

func (s *aitrStack) readMode(t *testing.T, w aitrWorld) aitrMode {
	t.Helper()
	r := s.do(w.person, "", http.MethodGet, "/api/v1/sites/"+w.siteID.String()+"/ai/mode", "")
	if r.status != http.StatusOK {
		t.Fatalf("read the mode: got %d %s", r.status, r.body)
	}
	var m aitrMode
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("decode the mode: %v", err)
	}
	return m
}

func (s *aitrStack) readAuto(t *testing.T, w aitrWorld) aitrAuto {
	t.Helper()
	r := s.do(w.person, "", http.MethodGet, "/api/v1/ai/connections/"+w.grantID.String()+"/usage", "")
	if r.status != http.StatusOK {
		t.Fatalf("read the usage: got %d %s", r.status, r.body)
	}
	var a aitrAuto
	if err := json.Unmarshal(r.body, &a); err != nil {
		t.Fatalf("decode the usage: %v", err)
	}
	return a
}

func modeBody(mode string, version int64) string {
	return fmt.Sprintf(`{"mode":%q,"version":%d}`, mode, version)
}

func autoBody(auto string) string { return fmt.Sprintf(`{"ai_auto":%q}`, auto) }

// TestLoosenRoutesRefuseApiKey: an owner-role API key may lower a site to ask
// and set a connection to never, and is refused with 403 session_required
// when it raises either; the setting is then unchanged. A signed-in person
// with the same role may raise both and is recorded as the setter.
//
// Mutation: drop authz.AuthorizeLoosening from SetMode or SetConnectionAuto
// in internal/aitrust/service.go; the key's raise is then not refused.
func TestLoosenRoutesRefuseApiKey(t *testing.T) {
	s := newAITrustStack(t)
	w := s.world(t)
	ctx := context.Background()
	modePath := "/api/v1/sites/" + w.siteID.String() + "/ai/mode"
	autoPath := "/api/v1/ai/connections/" + w.grantID.String() + "/auto"
	asKey := func(method, path, body string) loosenResp { return s.do(ctx, w.keyToken, method, path, body) }
	asPerson := func(method, path, body string) loosenResp { return s.do(w.person, "", method, path, body) }

	t.Run("a key lowers the site to ask, recorded as tightened with no person", func(t *testing.T) {
		before := s.readMode(t, w)
		r := asKey(http.MethodPut, modePath, modeBody("ask", before.Version))
		if r.status != http.StatusOK {
			t.Fatalf("lowering by key: got %d %q %s", r.status, r.code, r.body)
		}
		after := s.readMode(t, w)
		if after.Mode != "ask" || after.Source != aitrust.SourceTightened || after.SetByUserID != nil || after.Version != before.Version+1 {
			t.Fatalf("after lowering by key: %+v (before %+v)", after, before)
		}
	})
	t.Run("a key may not raise the site", func(t *testing.T) {
		before := s.readMode(t, w)
		r := asKey(http.MethodPut, modePath, modeBody("ai_drafts", before.Version))
		if r.status != http.StatusForbidden || r.code != aitrust.CodeSessionRequired {
			t.Fatalf("raising by key: got %d %q %s", r.status, r.code, r.body)
		}
		if after := s.readMode(t, w); len(modeChanges(before, after)) > 0 {
			t.Fatalf("a refused raise changed the setting: %s (before %+v, after %+v)",
				strings.Join(modeChanges(before, after), ", "), before, after)
		}
	})
	t.Run("a person raises the site and is recorded as its setter", func(t *testing.T) {
		before := s.readMode(t, w)
		r := asPerson(http.MethodPut, modePath, modeBody("ai_drafts", before.Version))
		if r.status != http.StatusOK {
			t.Fatalf("raising as a person: got %d %q %s", r.status, r.code, r.body)
		}
		after := s.readMode(t, w)
		if after.Mode != "ai_drafts" || after.Source != aitrust.SourcePerson || after.SetByUserID == nil ||
			*after.SetByUserID != w.owner.ID || !after.SetterValid || after.Version != before.Version+1 {
			t.Fatalf("after raising as a person: %+v", after)
		}
	})
	t.Run("a key sets the connection to never", func(t *testing.T) {
		r := asKey(http.MethodPut, autoPath, autoBody("never"))
		if r.status != http.StatusOK {
			t.Fatalf("never by key: got %d %q %s", r.status, r.code, r.body)
		}
		if a := s.readAuto(t, w); a.AIAuto != "never" || a.AutoSetByUserID != nil {
			t.Fatalf("after never by key: %+v", a)
		}
	})
	t.Run("a key may not allow the connection", func(t *testing.T) {
		r := asKey(http.MethodPut, autoPath, autoBody("site_setting"))
		if r.status != http.StatusForbidden || r.code != aitrust.CodeSessionRequired {
			t.Fatalf("allowing by key: got %d %q %s", r.status, r.code, r.body)
		}
		if a := s.readAuto(t, w); a.AIAuto != "never" {
			t.Fatalf("a refused allow changed the switch: %+v", a)
		}
	})
	t.Run("a person allows the connection and is recorded as its setter", func(t *testing.T) {
		r := asPerson(http.MethodPut, autoPath, autoBody("site_setting"))
		if r.status != http.StatusOK {
			t.Fatalf("allowing as a person: got %d %q %s", r.status, r.code, r.body)
		}
		a := s.readAuto(t, w)
		if a.AIAuto != "site_setting" || a.AutoSetByUserID == nil || *a.AutoSetByUserID != w.owner.ID || !a.AutoSetterValid {
			t.Fatalf("after allowing as a person: %+v", a)
		}
	})
	t.Run("every applied change wrote its audit row", func(t *testing.T) {
		if n := s.auditCount(t, w.org, aitrust.ActionModeChanged); n != 2 {
			t.Fatalf("mode audit rows: got %d, want 2 (one lowering, one raise)", n)
		}
		if n := s.auditCount(t, w.org, aitrust.ActionConnectionAutoChanged); n != 2 {
			t.Fatalf("switch audit rows: got %d, want 2 (never, then allowed)", n)
		}
	})
}

// TestLoosenRoutesRefuseMcpToken: an MCP bearer token is not a credential
// for the settings routes. Every loosening route answers 401 and nothing
// changes. A wrong answer does not stop the test, so a route that accepts the
// token also reports what it changed.
//
// Mutation: none in this package can make it pass by accident; the token is
// a live connection's token, so a router that accepted MCP tokens on /api/v1
// would turn this red.
func TestLoosenRoutesRefuseMcpToken(t *testing.T) {
	s := newAITrustStack(t)
	w := s.world(t)
	ctx := context.Background()
	before := s.readMode(t, w)
	beforeAuto := s.readAuto(t, w)
	for _, c := range []struct{ path, body string }{
		{"/api/v1/sites/" + w.siteID.String() + "/ai/mode", modeBody("ai_drafts", before.Version)},
		{"/api/v1/ai/connections/" + w.grantID.String() + "/auto", autoBody("site_setting")},
	} {
		r := s.do(ctx, w.mcpToken, http.MethodPut, c.path, c.body)
		if r.status != http.StatusUnauthorized {
			t.Errorf("an MCP token on a loosening route %s: got %d %q %s", c.path, r.status, r.code, r.body)
		}
	}
	if after := s.readMode(t, w); len(modeChanges(before, after)) > 0 {
		t.Errorf("the mode changed: %s (before %+v, after %+v)",
			strings.Join(modeChanges(before, after), ", "), before, after)
	}
	if after := s.readAuto(t, w); len(autoChanges(beforeAuto, after)) > 0 {
		t.Errorf("the switch changed: %s (before %+v, after %+v)",
			strings.Join(autoChanges(beforeAuto, after), ", "), beforeAuto, after)
	}
}

// TestGeneralModeRouteRefusesFull: the mode route never sets full auto,
// even for an owner. It answers 422 use_full_auto_route and nothing changes.
//
// Mutation: accept aipolicy.ModeFull in SetMode's switch; the owner's
// request then reaches the database.
func TestGeneralModeRouteRefusesFull(t *testing.T) {
	s := newAITrustStack(t)
	w := s.world(t)
	before := s.readMode(t, w)
	r := s.do(w.person, "", http.MethodPut, "/api/v1/sites/"+w.siteID.String()+"/ai/mode", modeBody("full", before.Version))
	if r.status != http.StatusUnprocessableEntity || r.code != aitrust.CodeUseFullAutoRoute {
		t.Fatalf("full on the general route: got %d %q %s", r.status, r.code, r.body)
	}
	if after := s.readMode(t, w); len(modeChanges(before, after)) > 0 {
		t.Fatalf("the mode changed: %s (before %+v, after %+v)",
			strings.Join(modeChanges(before, after), ", "), before, after)
	}
	// The compare-and-set: a write against a version that moved is refused.
	r = s.do(w.person, "", http.MethodPut, "/api/v1/sites/"+w.siteID.String()+"/ai/mode", modeBody("ask", before.Version+7))
	if r.status != http.StatusConflict || r.code != aitrust.CodeStaleVersion {
		t.Fatalf("a stale version: got %d %q %s", r.status, r.code, r.body)
	}
	if after := s.readMode(t, w); len(modeChanges(before, after)) > 0 {
		t.Fatalf("a stale write changed the mode: %s (before %+v, after %+v)",
			strings.Join(modeChanges(before, after), ", "), before, after)
	}
}

// TestModeLowerSerialisesWithReserve: lowering a site's mode waits for the
// lock the reservation holds while it re-checks the setting that approved a
// change (the site dispatch lock), and for the lock every automatic approval
// holds (the tenant policy lock). Each lock is held here by a wpmgr_app
// transaction through the shipped lock statement, as the reservation and the
// decision take them; the lowering commits only after that transaction ends.
//
// Mutation: drop either lock from aitrust's inSettingTx; the lowering then
// commits while the holder still has the lock.
func TestModeLowerSerialisesWithReserve(t *testing.T) {
	s := newAITrustStack(t)
	w := s.world(t)
	cases := []struct {
		name    string
		key, id string
	}{
		{"the site dispatch lock a reservation holds", aipolicy.AbilitySiteDispatchLockKey, w.siteID.String()},
		{"the tenant policy lock a decision holds", aipolicy.PolicyTenantLockKey, w.org.String()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Start each case from Auto for AI drafts.
			if m := s.readMode(t, w); m.Mode != "ai_drafts" {
				if r := s.do(w.person, "", http.MethodPut, "/api/v1/sites/"+w.siteID.String()+"/ai/mode",
					modeBody("ai_drafts", m.Version)); r.status != http.StatusOK {
					t.Fatalf("reset to ai_drafts: %d %s", r.status, r.body)
				}
			}
			before := s.readMode(t, w)
			held, release, holderDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			go func() {
				holderDone <- s.pool.InTenantTx(context.Background(), w.org, func(tx pgx.Tx) error {
					requireAppRoleInTx(t, context.Background(), tx)
					if err := sqlc.New(tx).TakeAssistantRequestXactLock(context.Background(),
						sqlc.TakeAssistantRequestXactLockParams{LockKey: c.key, LockID: c.id}); err != nil {
						return err
					}
					close(held)
					<-release
					return nil
				})
			}()
			<-held
			lowered := make(chan loosenResp, 1)
			go func() {
				lowered <- s.do(w.person, "", http.MethodPut, "/api/v1/sites/"+w.siteID.String()+"/ai/mode",
					modeBody("ask", before.Version))
			}()
			select {
			case r := <-lowered:
				close(release)
				<-holderDone
				t.Fatalf("the lowering committed while the lock was held: %d %s", r.status, r.body)
			case <-time.After(1500 * time.Millisecond):
			}
			if m := s.readMode(t, w); len(modeChanges(before, m)) > 0 {
				close(release)
				t.Fatalf("the mode moved while the lock was held: %s (before %+v, after %+v)",
					strings.Join(modeChanges(before, m), ", "), before, m)
			}
			close(release)
			if err := <-holderDone; err != nil {
				t.Fatalf("lock holder: %v", err)
			}
			select {
			case r := <-lowered:
				if r.status != http.StatusOK {
					t.Fatalf("the lowering after the lock was released: %d %q %s", r.status, r.code, r.body)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("the lowering did not finish after the lock was released")
			}
			if m := s.readMode(t, w); m.Mode != "ask" || m.Version != before.Version+1 {
				t.Fatalf("after the lowering: %+v (before %+v)", m, before)
			}
		})
	}
}
