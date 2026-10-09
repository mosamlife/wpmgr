package tests

// loosening_ai_control_integration_test.go: loosening an AI control needs a
// signed-in person. Tightening one stays open to an API key.
//
// Every request goes through the real router (server.New) over the pool
// startPostgres returns (wpmgr_app: NOSUPERUSER, NOBYPASSRLS), authenticated
// either by a session or by an owner-role API key, the way production
// authenticates both. State is read back through the same router as the
// signed-in owner, and the audit log through audit.Recorder over the same
// pool, so every read is one production makes.
//
// NOT RUN BY CI. Run with `make test-integration` from the repository root.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/apikey"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/config"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/govcontext"
	"github.com/mosamlife/wpmgr/apps/api/internal/middleware"
	"github.com/mosamlife/wpmgr/apps/api/internal/server"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	"github.com/mosamlife/wpmgr/apps/api/internal/tenant"
)

type loosenStack struct {
	pool     *db.Pool
	rec      *audit.Recorder
	authRepo *auth.Repo
	sessions *auth.SessionManager
	keys     *apikey.Service
	engine   *gin.Engine
}

// newLoosenStack builds the production router with server.New, wiring the
// handlers server.New registers unconditionally plus the context handler, the
// way cmd/wpmgr wires them. Every other optional handler is left nil.
func newLoosenStack(t *testing.T) *loosenStack {
	t.Helper()
	pool := startPostgres(t)
	gin.SetMode(gin.TestMode)
	clock := domain.SystemClock{}
	validator := domain.NewValidator()
	rec := audit.NewRecorder(pool, clock)
	authRepo := auth.NewRepo(pool)
	authSvc := auth.NewService(authRepo, rec, validator)
	sessions := auth.NewSessionManagerWithStore(scs.New(), false)
	keys := apikey.NewService(pool)
	govRepo := govcontext.NewRepo(pool)

	srv := server.New(server.Deps{
		Config:      config.Config{},
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Pool:        pool,
		Sessions:    sessions,
		Auth:        middleware.NewAuthenticator(sessions, authSvc, keys, pool),
		AuthH:       auth.NewHandler(authSvc, sessions, nil, nil),
		MembersH:    auth.NewMembersHandler(authSvc, nil),
		APIKeyH:     apikey.NewHandler(keys, rec),
		AuditH:      audit.NewHandler(rec),
		TenantH:     tenant.NewHandler(tenant.NewService(tenant.NewRepo(pool), validator, clock), rec),
		SiteH:       site.NewHandler(site.NewService(site.NewRepo(pool), validator, clock), rec, ""),
		GovContextH: govcontext.NewHandler(govcontext.NewService(govRepo, rec, &govcontext.Resolver{Store: govRepo})),
	})
	e, ok := srv.Handler().(*gin.Engine)
	if !ok {
		t.Fatalf("server.Handler() is %T, want *gin.Engine", srv.Handler())
	}
	return &loosenStack{pool: pool, rec: rec, authRepo: authRepo, sessions: sessions, keys: keys, engine: e}
}

func (s *loosenStack) session(t *testing.T, userID, tenantID uuid.UUID) context.Context {
	t.Helper()
	ctx, err := s.sessions.SCS().Load(context.Background(), "")
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if err := s.sessions.Login(ctx, userID, tenantID); err != nil {
		t.Fatalf("login: %v", err)
	}
	return ctx
}

type loosenResp struct {
	status int
	code   string
	body   []byte
}

// do sends one request through the router. ctx carries a session when the
// caller is a person; token, when set, authenticates an API key instead.
func (s *loosenStack) do(ctx context.Context, token, method, path, body string) loosenResp {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr).WithContext(ctx)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, req)
	var env struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	return loosenResp{status: w.Code, code: env.Code, body: w.Body.Bytes()}
}

func (s *loosenStack) auditCount(t *testing.T, tenantID uuid.UUID, action string) int {
	t.Helper()
	entries, err := s.rec.List(context.Background(), tenantID, 200, 0)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if action == "" {
		return len(entries)
	}
	n := 0
	for _, e := range entries {
		if e.Action == action {
			n++
		}
	}
	return n
}

func loosenBody(base int64, tools, domains, topics []string) string {
	b, _ := json.Marshal(map[string]any{
		"base_version": base,
		"restrictions": govcontext.RestrictionSet{
			ForbiddenTools: tools, ForbiddenDomains: domains, ForbiddenTopics: topics,
		},
	})
	return string(b)
}

func TestLooseningAnAIControlNeedsASignedInPerson(t *testing.T) {
	s := newLoosenStack(t)
	ctx := context.Background()
	sfx := uuid.NewString()[:8]

	org := seedTenant(t, s.pool, "loosen-"+sfx)
	owner := seedUserMembership(t, s.authRepo, "loosen-owner-"+sfx+"@example.com", org, authz.RoleOwner)
	key, err := s.keys.Create(ctx, org, "loosen-key-"+sfx, authz.RoleOwner)
	if err != nil {
		t.Fatalf("create API key: %v", err)
	}
	st, err := site.NewRepo(s.pool).Create(ctx, site.CreateInput{
		TenantID: org, URL: "https://loosen-" + sfx + ".example.com", Name: "loosen-" + sfx,
	})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}
	if err := s.pool.InTenantTx(ctx, org, func(tx pgx.Tx) error {
		requireAppRoleInTx(t, ctx, tx)
		return nil
	}); err != nil {
		t.Fatalf("open tenant tx: %v", err)
	}

	person := s.session(t, owner.ID, org)
	asKey := func(method, path, body string) loosenResp { return s.do(ctx, key.Token, method, path, body) }
	asPerson := func(method, path, body string) loosenResp { return s.do(person, "", method, path, body) }

	n := 0
	step := func(fn func(t *testing.T)) {
		n++
		t.Run(fmt.Sprintf("case_%d", n), fn)
	}
	refused := func(t *testing.T, r loosenResp) {
		t.Helper()
		if r.status != http.StatusForbidden || r.code != "session_required" {
			t.Fatalf("a credential that is not a person loosened a control: got %d %q %s", r.status, r.code, r.body)
		}
	}
	admitted := func(t *testing.T, r loosenResp) {
		t.Helper()
		if r.status != http.StatusOK {
			t.Fatalf("an allowed request was refused: got %d %q %s", r.status, r.code, r.body)
		}
	}
	readContext := func(t *testing.T, path string) (int64, govcontext.RestrictionSet) {
		t.Helper()
		r := asPerson(http.MethodGet, path, "")
		if r.status != http.StatusOK {
			t.Fatalf("read back: got %d %s", r.status, r.body)
		}
		var out struct {
			Version      int64                     `json:"version"`
			Restrictions govcontext.RestrictionSet `json:"restrictions"`
		}
		if err := json.Unmarshal(r.body, &out); err != nil {
			t.Fatalf("decode read back: %v", err)
		}
		return out.Version, out.Restrictions
	}
	versionID := func(t *testing.T, path string, version int64) string {
		t.Helper()
		r := asPerson(http.MethodGet, path+"/versions?limit=200", "")
		if r.status != http.StatusOK {
			t.Fatalf("list versions: got %d %s", r.status, r.body)
		}
		var out struct {
			Items []struct {
				ID      string `json:"id"`
				Version int64  `json:"version"`
			} `json:"items"`
		}
		if err := json.Unmarshal(r.body, &out); err != nil {
			t.Fatalf("decode versions: %v", err)
		}
		for _, it := range out.Items {
			if it.Version == version {
				return it.ID
			}
		}
		t.Fatalf("version %d not found", version)
		return ""
	}
	// refusedAndUnchanged sends a request the key must not be allowed to
	// make, then reads the layer back and counts the audit rows.
	refusedAndUnchanged := func(t *testing.T, path string, send func() loosenResp) {
		t.Helper()
		v0, r0 := readContext(t, path)
		a0 := s.auditCount(t, org, "")
		refused(t, send())
		v1, r1 := readContext(t, path)
		if v1 != v0 || !reflect.DeepEqual(r1, r0) {
			t.Fatalf("a refused request changed state: version %d -> %d, %+v -> %+v", v0, v1, r0, r1)
		}
		if a1 := s.auditCount(t, org, ""); a1 != a0 {
			t.Fatalf("a refused request wrote %d audit rows", a1-a0)
		}
	}

	// The same sequence on both writable layers. The site layer runs first,
	// while the organisation layer above it is still empty.
	for _, layer := range []struct{ path, p string }{
		{fmt.Sprintf("/api/v1/sites/%s/context", st.ID), "site"},
		{fmt.Sprintf("/api/v1/orgs/%s/context", org), "org"},
	} {
		path := layer.path
		a := func(kind string) string { return layer.p + "-" + kind + "-a" }
		b := func(kind string) string { return layer.p + "-" + kind + "-b" }
		ab := func(kind string) []string { return []string{a(kind), b(kind)} }
		ba := func(kind string) []string { return []string{b(kind), a(kind)} }
		only := func(v string) []string { return []string{v} }
		base := func(t *testing.T) int64 {
			t.Helper()
			v, _ := readContext(t, path)
			return v
		}

		step(func(t *testing.T) {
			admitted(t, asKey(http.MethodPatch, path,
				loosenBody(base(t), only(a("tool")), only(a("domain")), only(a("topic")))))
		})
		step(func(t *testing.T) {
			admitted(t, asKey(http.MethodPatch, path,
				fmt.Sprintf(`{"base_version":%d,"guidance":{"brand_voice":"plain"}}`, base(t))))
		})
		step(func(t *testing.T) {
			admitted(t, asKey(http.MethodPatch, path, loosenBody(base(t), ab("tool"), ab("domain"), ab("topic"))))
		})
		step(func(t *testing.T) {
			admitted(t, asKey(http.MethodPatch, path, loosenBody(base(t), ba("tool"), ba("domain"), ba("topic"))))
		})
		step(func(t *testing.T) {
			v := base(t)
			refusedAndUnchanged(t, path, func() loosenResp {
				return asKey(http.MethodPatch, path, loosenBody(v, only(b("tool")), ba("domain"), ba("topic")))
			})
		})
		step(func(t *testing.T) {
			v := base(t)
			refusedAndUnchanged(t, path, func() loosenResp {
				return asKey(http.MethodPatch, path, loosenBody(v, ba("tool"), only(b("domain")), ba("topic")))
			})
		})
		step(func(t *testing.T) {
			v := base(t)
			refusedAndUnchanged(t, path, func() loosenResp {
				return asKey(http.MethodPatch, path, loosenBody(v, ba("tool"), ba("domain"), only(b("topic"))))
			})
		})
		step(func(t *testing.T) {
			id := versionID(t, path, 1)
			refusedAndUnchanged(t, path, func() loosenResp {
				return asKey(http.MethodPost, path+"/versions/"+id+"/restore", "")
			})
		})
		step(func(t *testing.T) {
			admitted(t, asKey(http.MethodPost, path+"/versions/"+versionID(t, path, 3)+"/restore", ""))
		})
		step(func(t *testing.T) {
			admitted(t, asPerson(http.MethodPatch, path,
				loosenBody(base(t), only(b("tool")), only(b("domain")), only(b("topic")))))
			if _, got := readContext(t, path); !reflect.DeepEqual(got.ForbiddenTools, only(b("tool"))) {
				t.Fatalf("an allowed request did not land: %+v", got)
			}
		})
		step(func(t *testing.T) {
			admitted(t, asPerson(http.MethodPost, path+"/versions/"+versionID(t, path, 1)+"/restore", ""))
			if _, got := readContext(t, path); !reflect.DeepEqual(got.ForbiddenTools, only(a("tool"))) {
				t.Fatalf("an allowed request did not land: %+v", got)
			}
		})
	}

	assistant := fmt.Sprintf("/api/v1/tenants/%s/assistant", org)
	paused := func(t *testing.T) bool {
		t.Helper()
		r := asPerson(http.MethodGet, assistant, "")
		if r.status != http.StatusOK {
			t.Fatalf("read back: got %d %s", r.status, r.body)
		}
		var out struct {
			Paused bool `json:"paused"`
		}
		if err := json.Unmarshal(r.body, &out); err != nil {
			t.Fatalf("decode read back: %v", err)
		}
		return out.Paused
	}
	step(func(t *testing.T) {
		admitted(t, asKey(http.MethodPost, assistant+"/pause", `{"reason":"loosen"}`))
		if !paused(t) {
			t.Fatal("an allowed request did not land")
		}
	})
	step(func(t *testing.T) {
		a0 := s.auditCount(t, org, "")
		refused(t, asKey(http.MethodPost, assistant+"/resume", ""))
		if !paused(t) {
			t.Fatal("a refused request changed state")
		}
		if a1 := s.auditCount(t, org, ""); a1 != a0 {
			t.Fatalf("a refused request wrote %d audit rows", a1-a0)
		}
	})
	step(func(t *testing.T) {
		admitted(t, asPerson(http.MethodPost, assistant+"/resume", ""))
		if paused(t) {
			t.Fatal("an allowed request did not land")
		}
	})

	const rebaseline = "/api/v1/audit/integrity/rebaseline"
	baseline := func(t *testing.T) *audit.Baseline {
		t.Helper()
		b, err := s.rec.GetBaseline(ctx, org)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		return b
	}
	step(func(t *testing.T) {
		a0 := s.auditCount(t, org, "")
		refused(t, asKey(http.MethodPost, rebaseline, ""))
		if baseline(t) != nil {
			t.Fatal("a refused request changed state")
		}
		if a1 := s.auditCount(t, org, ""); a1 != a0 {
			t.Fatalf("a refused request wrote %d audit rows", a1-a0)
		}
	})
	step(func(t *testing.T) {
		admitted(t, asPerson(http.MethodPost, rebaseline, ""))
		if b := baseline(t); b == nil || b.SetBy == nil || *b.SetBy != owner.ID {
			t.Fatalf("an allowed request did not land: %+v", b)
		}
	})
}
