package tests

// GH #755 — a site-bound enrollment (first connect, or Reconnect) adopts the
// address the agent reports only when it differs from the stored address by a
// leading "www." and/or an http to https upgrade, on the same port and path.
// Every other difference keeps the stored address and is flagged in the audit
// log, and an address another site in the tenant already holds never fails the
// enrollment.
//
// Every request here goes through the mounted routes (POST /sites, POST
// /sites/:siteId/enrollment-codes with their real authz middleware, and the
// public POST /enroll), with the production site service, connection service
// and audit recorder, against a database reached as wpmgr_app (NOSUPERUSER,
// NOBYPASSRLS), which gh755AssertAppRole checks rather than assumes.

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

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// gh755Env is one control plane mounted the way server.go mounts it: the
// public /enroll on the root engine, the authed site routes under /api/v1
// behind a middleware that injects the principal the way the auth chain does.
type gh755Env struct {
	pool *db.Pool
	eng  *gin.Engine
	svc  *site.Service
	rec  *audit.Recorder
	as   domain.Principal // the principal the next /api/v1 request carries
}

func newGH755Env(t *testing.T, pool *db.Pool) *gh755Env {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repo := site.NewRepo(pool)
	rec := audit.NewRecorder(pool, domain.SystemClock{})
	conn := site.NewConnectionService(repo, domain.NewValidator(), rec, nil, domain.SystemClock{}, nil)
	svc := site.NewService(repo, domain.NewValidator(), domain.SystemClock{})
	svc.SetConnectionService(conn)
	h := site.NewHandler(svc, rec, "")
	h.SetConnectionService(conn)

	env := &gh755Env{pool: pool, svc: svc, rec: rec}
	eng := gin.New()
	h.RegisterPublic(eng)
	v1 := eng.Group("/api/v1")
	v1.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), env.as))
		c.Next()
	})
	h.Register(v1)
	env.eng = eng
	return env
}

func (e *gh755Env) do(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.9:4444"
	w := httptest.NewRecorder()
	e.eng.ServeHTTP(w, req)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// addSite runs the dashboard's "Add site" (POST /sites) and returns the new
// site id and its enrollment code.
func (e *gh755Env) addSite(t *testing.T, url string) (uuid.UUID, string) {
	t.Helper()
	code, body := e.do(t, http.MethodPost, "/api/v1/sites", map[string]any{"url": url, "name": url})
	if code != http.StatusCreated {
		t.Fatalf("POST /sites %s answered %d, want 201: %v", url, code, body)
	}
	id, err := uuid.Parse(body["site_id"].(string))
	if err != nil {
		t.Fatalf("site_id: %v", err)
	}
	return id, body["enrollment_code"].(string)
}

// reconnect runs Reconnect (POST /sites/:siteId/enrollment-codes) and returns
// the fresh enrollment code.
func (e *gh755Env) reconnect(t *testing.T, siteID uuid.UUID) string {
	t.Helper()
	code, body := e.do(t, http.MethodPost, "/api/v1/sites/"+siteID.String()+"/enrollment-codes", nil)
	if code != http.StatusCreated {
		t.Fatalf("reconnect answered %d, want 201: %v", code, body)
	}
	return body["enrollment_code"].(string)
}

// enroll is the agent's POST /enroll, reporting reportedURL as its home_url.
func (e *gh755Env) enroll(t *testing.T, pairingCode, reportedURL string) (int, map[string]any) {
	t.Helper()
	_, _, pub := genKey(t)
	return e.do(t, http.MethodPost, "/enroll", map[string]any{
		"pairing_code":     pairingCode,
		"site_url":         reportedURL,
		"agent_public_key": pub,
		"wp_version":       "6.6",
		"php_version":      "8.3",
	})
}

func (e *gh755Env) mustEnroll(t *testing.T, pairingCode, reportedURL string, want uuid.UUID) {
	t.Helper()
	code, body := e.enroll(t, pairingCode, reportedURL)
	if code != http.StatusOK {
		t.Fatalf("POST /enroll reporting %s answered %d, want 200: %v", reportedURL, code, body)
	}
	if body["site_id"] != want.String() {
		t.Fatalf("enroll attached to %v, want %s", body["site_id"], want)
	}
}

func (e *gh755Env) site(t *testing.T, tenant, id uuid.UUID) site.Site {
	t.Helper()
	s, err := e.svc.Get(context.Background(), tenant, id)
	if err != nil {
		t.Fatalf("get site %s: %v", id, err)
	}
	return s
}

// urlAudits returns the site.url_changed and site.url_mismatch rows for one
// site, read through the recorder's own tenant-scoped List.
func (e *gh755Env) urlAudits(t *testing.T, tenant, siteID uuid.UUID) (changed, mismatch []audit.Entry) {
	t.Helper()
	entries, err := e.rec.List(context.Background(), tenant, 500, 0)
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	for _, en := range entries {
		if en.TargetType != "site" || en.TargetID != siteID.String() {
			continue
		}
		switch en.Action {
		case audit.ActionSiteURLChanged:
			changed = append(changed, en)
		case audit.ActionSiteURLMismatch:
			mismatch = append(mismatch, en)
		}
	}
	return changed, mismatch
}

func gh755Owner(tenant, user uuid.UUID) domain.Principal {
	return domain.Principal{Type: domain.PrincipalUser, UserID: user, TenantID: tenant, Role: "owner", Scope: domain.ScopeOrg}
}

func gh755SeedUser(t *testing.T, pool *db.Pool, email string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.InAgentTx(context.Background(), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `INSERT INTO users (id, email, name) VALUES ($1, $2, 'GH755')`, id, email)
		return err
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// gh755AssertAppRole fails unless the pool connects as wpmgr_app with neither
// SUPERUSER nor BYPASSRLS, so the RLS policies these proofs rely on are live.
func gh755AssertAppRole(t *testing.T, pool *db.Pool, tenant uuid.UUID) {
	t.Helper()
	var who string
	var super, bypass bool
	err := pool.InTenantTx(context.Background(), tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT current_user, r.rolsuper, r.rolbypassrls FROM pg_roles r WHERE r.rolname = current_user`).Scan(&who, &super, &bypass)
	})
	if err != nil {
		t.Fatalf("role check: %v", err)
	}
	if who != "wpmgr_app" || super || bypass {
		t.Fatalf("running as %s (super=%v bypassrls=%v), want wpmgr_app with neither", who, super, bypass)
	}
}

func gh755Setup(t *testing.T, slug string) (*gh755Env, uuid.UUID) {
	t.Helper()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, slug)
	gh755AssertAppRole(t, pool, tenant)
	env := newGH755Env(t, pool)
	env.as = gh755Owner(tenant, gh755SeedUser(t, pool, slug+"@example.test"))
	return env, tenant
}

func assertURLChanged(t *testing.T, got []audit.Entry, from, to string) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("site.url_changed rows = %d, want 1", len(got))
	}
	m := got[0].Metadata
	if m["from"] != from || m["to"] != to || m["source"] != "agent_enrollment" {
		t.Fatalf("site.url_changed metadata = %v, want from=%s to=%s source=agent_enrollment", m, from, to)
	}
	if got[0].ActorType != audit.ActorSystem {
		t.Fatalf("site.url_changed actor = %s, want %s", got[0].ActorType, audit.ActorSystem)
	}
}

func assertURLMismatch(t *testing.T, got []audit.Entry, stored, reported, reason string) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("site.url_mismatch rows = %d, want 1", len(got))
	}
	m := got[0].Metadata
	if m["stored"] != stored || m["agent_reported"] != reported || m["source"] != "agent_enrollment" || m["reason"] != reason {
		t.Fatalf("site.url_mismatch metadata = %v, want stored=%s agent_reported=%s reason=%s", m, stored, reported, reason)
	}
}

// TestSiteFirstEnroll_AdoptsEquivalentAgentAddress: the first connect of a site
// saved as the apex, whose agent reports the www address, stores the www
// address; an http site whose agent reports https is upgraded. The same address
// held by a site in ANOTHER tenant does not block either, and that tenant's
// site is untouched.
func TestSiteFirstEnroll_AdoptsEquivalentAgentAddress(t *testing.T) {
	env, tenant := gh755Setup(t, "gh755-adopt")

	// Another tenant already holds the www address the agent will report.
	other := seedTenant(t, env.pool, "gh755-adopt-other")
	otherSite, err := site.NewRepo(env.pool).CreatePending(context.Background(), other, "https://www.adopt755.example.com", "other", nil)
	if err != nil {
		t.Fatalf("seed other tenant's site: %v", err)
	}

	t.Run("apex saved, agent reports www", func(t *testing.T) {
		id, code := env.addSite(t, "https://adopt755.example.com")
		env.mustEnroll(t, code, "https://www.adopt755.example.com/", id)

		s := env.site(t, tenant, id)
		if s.URL != "https://www.adopt755.example.com" || s.ConnectionState != site.StateConnected {
			t.Fatalf("site url=%q state=%s, want the www address and connected", s.URL, s.ConnectionState)
		}
		changed, mismatch := env.urlAudits(t, tenant, id)
		assertURLChanged(t, changed, "https://adopt755.example.com", "https://www.adopt755.example.com")
		if len(mismatch) != 0 {
			t.Fatalf("unexpected site.url_mismatch rows: %v", mismatch)
		}
	})

	t.Run("http saved, agent reports https", func(t *testing.T) {
		id, code := env.addSite(t, "http://upgrade755.example.com")
		env.mustEnroll(t, code, "https://upgrade755.example.com", id)

		if s := env.site(t, tenant, id); s.URL != "https://upgrade755.example.com" {
			t.Fatalf("site url=%q, want the https address", s.URL)
		}
		changed, _ := env.urlAudits(t, tenant, id)
		assertURLChanged(t, changed, "http://upgrade755.example.com", "https://upgrade755.example.com")
	})

	t.Run("equal address records nothing", func(t *testing.T) {
		id, code := env.addSite(t, "https://same755.example.com")
		env.mustEnroll(t, code, "https://SAME755.example.com:443/", id)

		if s := env.site(t, tenant, id); s.URL != "https://same755.example.com" {
			t.Fatalf("site url=%q, want it unchanged", s.URL)
		}
		changed, mismatch := env.urlAudits(t, tenant, id)
		if len(changed)+len(mismatch) != 0 {
			t.Fatalf("an equal address was audited: changed=%v mismatch=%v", changed, mismatch)
		}
	})

	// The other tenant's site kept its address and was not attached.
	var url, state string
	err = env.pool.InTenantTx(context.Background(), other, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT url, connection_state FROM sites WHERE tenant_id = $1 AND id = $2`, other, otherSite.ID).Scan(&url, &state)
	})
	if err != nil || url != "https://www.adopt755.example.com" || state != string(site.StatePendingEnrollment) {
		t.Fatalf("other tenant's site: err=%v url=%q state=%q", err, url, state)
	}
}

// TestReEnroll_AdoptsAgentWwwAddress_KeepsSiteID is the remedy for a site that
// is already connected under the apex while WordPress answers on www: Reconnect
// stores the www address on the same site.
func TestReEnroll_AdoptsAgentWwwAddress_KeepsSiteID(t *testing.T) {
	env, tenant := gh755Setup(t, "gh755-reconnect")

	id, code := env.addSite(t, "https://reconnect755.example.com")
	env.mustEnroll(t, code, "https://reconnect755.example.com", id) // saved and reported as the apex
	if s := env.site(t, tenant, id); s.URL != "https://reconnect755.example.com" {
		t.Fatalf("after first connect url=%q", s.URL)
	}

	code = env.reconnect(t, id)
	env.mustEnroll(t, code, "https://www.reconnect755.example.com", id)

	s := env.site(t, tenant, id)
	if s.ID != id || s.URL != "https://www.reconnect755.example.com" || s.ConnectionState != site.StateConnected {
		t.Fatalf("after reconnect id=%s url=%q state=%s", s.ID, s.URL, s.ConnectionState)
	}
	list, err := env.svc.List(context.Background(), site.ListInput{TenantID: tenant})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("reconnect left %d sites, want 1", len(list))
	}
	changed, _ := env.urlAudits(t, tenant, id)
	assertURLChanged(t, changed, "https://reconnect755.example.com", "https://www.reconnect755.example.com")
}

// TestSiteFirstEnroll_KeepsStoredURLForNonEquivalentAddress: a downgrade, a
// different host, a different subdomain and a different path are never
// adopted. The enrollment still completes and the difference is audited.
func TestSiteFirstEnroll_KeepsStoredURLForNonEquivalentAddress(t *testing.T) {
	env, tenant := gh755Setup(t, "gh755-keep")

	cases := []struct {
		name, stored, reported string
	}{
		{"https to http downgrade", "https://down755.example.com", "http://down755.example.com"},
		{"https www to http apex", "https://www.downwww755.example.com", "http://downwww755.example.com"},
		{"other host", "https://host755.example.com", "https://attacker755.example.net"},
		{"other subdomain", "https://sub755.example.com", "https://shop.sub755.example.com"},
		{"other path", "https://path755.example.com", "https://www.path755.example.com/blog"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, code := env.addSite(t, c.stored)
			env.mustEnroll(t, code, c.reported, id)

			s := env.site(t, tenant, id)
			if s.URL != c.stored || s.ConnectionState != site.StateConnected {
				t.Fatalf("site url=%q state=%s, want %q and connected", s.URL, s.ConnectionState, c.stored)
			}
			changed, mismatch := env.urlAudits(t, tenant, id)
			if len(changed) != 0 {
				t.Fatalf("a non-equivalent address was recorded as adopted: %v", changed)
			}
			assertURLMismatch(t, mismatch, c.stored, c.reported, "not_equivalent")
		})
	}
}

// TestSiteFirstEnroll_AdoptConflictKeepsStoredURL: when another site in the
// same tenant holds the adoptable address, the stored address is kept, the
// enrollment completes, and the conflict is audited. Both when the holder is
// already committed and when it commits while the enrollment is in flight.
func TestSiteFirstEnroll_AdoptConflictKeepsStoredURL(t *testing.T) {
	env, tenant := gh755Setup(t, "gh755-conflict")
	ctx := context.Background()
	repo := site.NewRepo(env.pool)

	t.Run("holder already committed", func(t *testing.T) {
		id, code := env.addSite(t, "https://held755.example.com")
		env.mustEnroll(t, code, "https://held755.example.com", id)
		// A second site under the www spelling, as a tenant could hold from
		// before the add-site check covered equivalent spellings.
		holder, err := repo.CreatePending(ctx, tenant, "https://www.held755.example.com", "holder", nil)
		if err != nil {
			t.Fatalf("seed holder: %v", err)
		}

		code = env.reconnect(t, id)
		env.mustEnroll(t, code, "https://www.held755.example.com", id)

		if s := env.site(t, tenant, id); s.URL != "https://held755.example.com" || s.ConnectionState != site.StateConnected {
			t.Fatalf("site url=%q state=%s, want the stored address and connected", s.URL, s.ConnectionState)
		}
		if h := env.site(t, tenant, holder.ID); h.URL != "https://www.held755.example.com" {
			t.Fatalf("holder url=%q, want it unchanged", h.URL)
		}
		changed, mismatch := env.urlAudits(t, tenant, id)
		if len(changed) != 0 {
			t.Fatalf("conflict recorded as adopted: %v", changed)
		}
		assertURLMismatch(t, mismatch, "https://held755.example.com", "https://www.held755.example.com", "address_in_use")
	})

	t.Run("holder commits during the enrollment", func(t *testing.T) {
		id, code := env.addSite(t, "https://race755.example.com")
		env.mustEnroll(t, code, "https://race755.example.com", id)
		code = env.reconnect(t, id)

		// Insert the holder in an open transaction, so the enrollment's own
		// conflict check cannot see it and its write waits on the unique index.
		inserted := make(chan struct{})
		release := make(chan struct{})
		holderErr := make(chan error, 1)
		go func() {
			holderErr <- env.pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
				if _, err := sqlc.New(tx).CreatePendingSite(ctx, sqlc.CreatePendingSiteParams{
					TenantID: tenant, Url: "https://www.race755.example.com", Name: "holder", Tags: []string{},
				}); err != nil {
					close(inserted)
					return err
				}
				close(inserted)
				<-release
				return nil
			})
		}()
		<-inserted

		var wg sync.WaitGroup
		var status int
		var body map[string]any
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, body = env.enroll(t, code, "https://www.race755.example.com")
		}()

		// Bounded wait for the enrollment to block on the holder's transaction.
		blocked := false
		for i := 0; i < 100 && !blocked; i++ {
			var n int
			if err := env.pool.QueryRow(ctx,
				`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query ILIKE '%UPDATE sites%'`).Scan(&n); err != nil {
				t.Fatalf("pg_stat_activity: %v", err)
			}
			blocked = n > 0
			if !blocked {
				time.Sleep(50 * time.Millisecond)
			}
		}
		close(release)
		wg.Wait()
		if err := <-holderErr; err != nil {
			t.Fatalf("holder insert: %v", err)
		}
		if !blocked {
			t.Fatal("the enrollment never waited on the holder's transaction, so this case did not exercise a conflict its check could not see")
		}

		if status != http.StatusOK {
			t.Fatalf("POST /enroll answered %d, want 200: %v", status, body)
		}
		if s := env.site(t, tenant, id); s.URL != "https://race755.example.com" || s.ConnectionState != site.StateConnected {
			t.Fatalf("site url=%q state=%s, want the stored address and connected", s.URL, s.ConnectionState)
		}
		changed, mismatch := env.urlAudits(t, tenant, id)
		if len(changed) != 0 {
			t.Fatalf("conflict recorded as adopted: %v", changed)
		}
		assertURLMismatch(t, mismatch, "https://race755.example.com", "https://www.race755.example.com", "address_in_use")
	})
}

// TestReEnrollAddressGate_RequiresOrgScopedSiteWriter: the address changes
// only through an enrollment code, and a code for an existing site is minted
// only by Reconnect, which needs site:write, org scope and access to the site.
// A site-scoped collaborator (with or without a share of this site), an org
// viewer, and an owner of another tenant all fail to mint one, and the site is
// left connected under its address.
func TestReEnrollAddressGate_RequiresOrgScopedSiteWriter(t *testing.T) {
	env, tenant := gh755Setup(t, "gh755-gate")
	owner := env.as

	id, code := env.addSite(t, "https://gate755.example.com")
	env.mustEnroll(t, code, "https://gate755.example.com", id)
	otherSite, _ := env.addSite(t, "https://gate-other755.example.com")

	otherTenant := seedTenant(t, env.pool, "gh755-gate-other")
	callers := []struct {
		name string
		p    domain.Principal
		want int
	}{
		{"site-scoped collaborator on another site", domain.Principal{
			Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenant, Role: "operator",
			Scope: domain.ScopeSite, AllowedSiteIDs: []uuid.UUID{otherSite}}, http.StatusForbidden},
		{"site-scoped collaborator shared this site", domain.Principal{
			Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenant, Role: "operator",
			Scope: domain.ScopeSite, AllowedSiteIDs: []uuid.UUID{id}}, http.StatusForbidden},
		{"org viewer", domain.Principal{
			Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenant, Role: "viewer",
			Scope: domain.ScopeOrg}, http.StatusForbidden},
		{"owner of another tenant", gh755Owner(otherTenant, gh755SeedUser(t, env.pool, "gate-other@example.test")), http.StatusNotFound},
	}
	for _, c := range callers {
		t.Run(c.name, func(t *testing.T) {
			env.as = c.p
			status, body := env.do(t, http.MethodPost, "/api/v1/sites/"+id.String()+"/enrollment-codes", nil)
			if status != c.want {
				t.Fatalf("answered %d, want %d: %v", status, c.want, body)
			}
			if _, ok := body["enrollment_code"]; ok {
				t.Fatalf("an enrollment code was returned: %v", body)
			}
		})
	}

	env.as = owner
	s := env.site(t, tenant, id)
	if s.URL != "https://gate755.example.com" || s.ConnectionState != site.StateConnected {
		t.Fatalf("site url=%q state=%s, want it untouched and connected", s.URL, s.ConnectionState)
	}
}

// TestSiteMint_RefusesEquivalentAddress: "Add site" answers 409
// site_url_exists for an http/https, www or trailing-slash spelling of a site
// the tenant already has, naming the stored address, and still adds an
// unrelated address.
func TestSiteMint_RefusesEquivalentAddress(t *testing.T) {
	env, _ := gh755Setup(t, "gh755-mint")

	id, _ := env.addSite(t, "https://mint755.example.com")
	for _, spelling := range []string{
		"https://mint755.example.com",
		"https://www.mint755.example.com",
		"http://mint755.example.com/",
		"http://www.mint755.example.com",
	} {
		status, body := env.do(t, http.MethodPost, "/api/v1/sites", map[string]any{"url": spelling, "name": "dup"})
		if status != http.StatusConflict {
			t.Fatalf("%s answered %d, want 409: %v", spelling, status, body)
		}
		details, _ := body["details"].(map[string]any)
		if body["code"] != "site_url_exists" || details["site_id"] != id.String() || details["url"] != "https://mint755.example.com" {
			t.Fatalf("%s: body %v, want site_url_exists naming %s at https://mint755.example.com", spelling, body, id)
		}
	}
	for _, distinct := range []string{"https://shop.mint755.example.com", "https://mint755.example.com/blog"} {
		if status, body := env.do(t, http.MethodPost, "/api/v1/sites", map[string]any{"url": distinct, "name": "distinct"}); status != http.StatusCreated {
			t.Fatalf("%s answered %d, want 201: %v", distinct, status, body)
		}
	}
}
