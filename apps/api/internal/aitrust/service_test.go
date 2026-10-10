package aitrust

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/aipolicy"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// These tests run the refusals that come before any database access. The
// service is built with no repo, so a refusal that let a call through would
// reach the database and panic, which fails the test.

func person(role authz.Role) domain.Principal {
	return domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: uuid.New(), Role: string(role)}
}

func apiKey(role authz.Role) domain.Principal {
	return domain.Principal{Type: domain.PrincipalAPIKey, APIKeyID: uuid.New(), TenantID: uuid.New(), Role: string(role)}
}

func code(t *testing.T, err error) string {
	t.Helper()
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("want a domain error, got %v", err)
	}
	return de.Code
}

func TestSetModeRaiseNeedsASignedInPerson(t *testing.T) {
	svc := NewService(nil, nil, nil)
	_, err := svc.SetMode(context.Background(), apiKey(authz.RoleOwner), uuid.New(), aipolicy.ModeAIDrafts, 3)
	if got := code(t, err); got != CodeSessionRequired {
		t.Fatalf("an API key raising: code %q, want %q", got, CodeSessionRequired)
	}
	if domain.HTTPStatus(err) != http.StatusForbidden {
		t.Fatalf("status %d, want 403", domain.HTTPStatus(err))
	}
}

func TestSetModeRefusesFullAndUnknown(t *testing.T) {
	svc := NewService(nil, nil, nil)
	_, err := svc.SetMode(context.Background(), person(authz.RoleOwner), uuid.New(), aipolicy.ModeFull, 1)
	if got := code(t, err); got != CodeUseFullAutoRoute || domain.HTTPStatus(err) != http.StatusUnprocessableEntity {
		t.Fatalf("full: code %q status %d", got, domain.HTTPStatus(err))
	}
	_, err = svc.SetMode(context.Background(), person(authz.RoleOwner), uuid.New(), aipolicy.Mode("everything"), 1)
	if domain.HTTPStatus(err) != http.StatusUnprocessableEntity {
		t.Fatalf("unknown mode: status %d, want 422", domain.HTTPStatus(err))
	}
	_, err = svc.SetMode(context.Background(), person(authz.RoleOwner), uuid.New(), aipolicy.ModeAsk, -1)
	if domain.HTTPStatus(err) != http.StatusUnprocessableEntity {
		t.Fatalf("negative version: status %d, want 422", domain.HTTPStatus(err))
	}
}

func TestSetConnectionAutoRaiseNeedsASignedInPerson(t *testing.T) {
	svc := NewService(nil, nil, nil)
	_, err := svc.SetConnectionAuto(context.Background(), apiKey(authz.RoleOwner), uuid.New(), aipolicy.AutoSiteSetting)
	if got := code(t, err); got != CodeSessionRequired {
		t.Fatalf("an API key allowing: code %q, want %q", got, CodeSessionRequired)
	}
}

func TestConnectionRoutesRefuseASiteConstrainedPrincipal(t *testing.T) {
	svc := NewService(nil, nil, nil)
	p := person(authz.RoleAdmin)
	p.Scope, p.AllowedSiteIDs = domain.ScopeSite, []uuid.UUID{uuid.New()}
	for _, auto := range []aipolicy.ConnectionAuto{aipolicy.AutoSiteSetting, aipolicy.AutoNever} {
		_, err := svc.SetConnectionAuto(context.Background(), p, uuid.New(), auto)
		if got := code(t, err); got != CodeOrgScopeRequired {
			t.Fatalf("%s by a site collaborator: code %q, want %q", auto, got, CodeOrgScopeRequired)
		}
	}
	_, err := svc.ConnectionUsage(context.Background(), p, uuid.New())
	if got := code(t, err); got != CodeOrgScopeRequired {
		t.Fatalf("usage by a site collaborator: code %q, want %q", got, CodeOrgScopeRequired)
	}
}

func TestModeOptionsReasonOrder(t *testing.T) {
	site := uuid.New()
	ok := modeFacts{agentVersion: MinAgentVersion}
	cases := []struct {
		name   string
		p      domain.Principal
		f      modeFacts
		drafts string
		ask    string
	}{
		{"operator person", person(authz.RoleOperator), ok, "", ""},
		{"viewer", person(authz.RoleViewer), ok, CodeRoleRequired, CodeRoleRequired},
		{"owner key", apiKey(authz.RoleOwner), ok, CodeSessionRequired, ""},
		{"paused", person(authz.RoleOwner), modeFacts{agentVersion: MinAgentVersion, paused: true}, CodePaused, ""},
		{"old plugin", person(authz.RoleOwner), modeFacts{agentVersion: "0.1.0"}, CodeAgentOutdated, ""},
		{"key while paused", apiKey(authz.RoleOwner), modeFacts{paused: true}, CodeSessionRequired, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := modeOptions(c.p, site, c.f)
			if len(opts) != 2 || opts[0].Mode != aipolicy.ModeAsk || opts[1].Mode != aipolicy.ModeAIDrafts {
				t.Fatalf("options: %+v", opts)
			}
			if opts[0].Reason != c.ask || opts[0].Choosable != (c.ask == "") {
				t.Fatalf("ask: %+v, want reason %q", opts[0], c.ask)
			}
			if opts[1].Reason != c.drafts || opts[1].Choosable != (c.drafts == "") {
				t.Fatalf("ai_drafts: %+v, want reason %q", opts[1], c.drafts)
			}
		})
	}
}

func TestChangeKindsFollowTheCatalogue(t *testing.T) {
	entries := []sqlc.AbilityCatalogue{
		{Name: "wpmgr/page-create", Title: "Create a draft page", Class: "write", ChangeClass: "ai_draft"},
		{Name: "vendor/thing", Title: "A vendor write", Class: "write", ChangeClass: "always_ask"},
		{Name: "wpmgr/site-info", Title: "Read the site", Class: "read", ChangeClass: "always_ask"},
	}
	routes := []sqlc.RestRouteCatalogue{
		{RouteID: "wp-v2-pages-update-fields", Title: "Edit a page's title", Class: "write", ChangeClass: "by_target_status"},
	}
	kinds := changeKinds(entries, routes)
	got := map[aipolicy.Class]ChangeKind{}
	var order []aipolicy.Class
	for _, k := range kinds {
		got[k.Class] = k
		order = append(order, k.Class)
	}
	want := []aipolicy.Class{aipolicy.ClassAIDraft, aipolicy.ClassUnpublished, aipolicy.ClassLive, aipolicy.ClassPublish}
	if len(order) != len(want) {
		t.Fatalf("kinds %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("kinds %v, want %v", order, want)
		}
	}
	if n := len(got[aipolicy.ClassAIDraft].Abilities); n != 2 {
		t.Fatalf("ai_draft lists %d tools, want page creation and the route", n)
	}
	for _, k := range kinds {
		if len(k.Decisions) != 2 || k.Decisions[0].Outcome != "ask" {
			t.Fatalf("%s decisions %+v: ask must ask", k.Class, k.Decisions)
		}
		wantDrafts := "ask"
		if k.Class == aipolicy.ClassAIDraft {
			wantDrafts = "auto"
		}
		if k.Decisions[1].Outcome != wantDrafts {
			t.Fatalf("%s under ai_drafts: %q, want %q", k.Class, k.Decisions[1].Outcome, wantDrafts)
		}
	}
}

func TestCursorRoundTripAndRefusal(t *testing.T) {
	c := Cursor{CreatedAt: time.Date(2026, 10, 9, 12, 0, 0, 123456789, time.UTC), ID: uuid.New()}
	back, ok := DecodeCursor(EncodeCursor(c))
	if !ok || !back.CreatedAt.Equal(c.CreatedAt) || back.ID != c.ID {
		t.Fatalf("round trip: %+v %v, want %+v", back, ok, c)
	}
	for _, bad := range []string{"", "!!", "bm90LWEtY3Vyc29y", strings.Repeat("A", 200)} {
		if _, ok := DecodeCursor(bad); ok {
			t.Fatalf("cursor %q accepted", bad)
		}
	}
}

// TestRoutesRefuseThroughTheRouter mounts the handler behind a principal and
// checks the gates and refusals a request meets before the database.
func TestRoutesRefuseThroughTheRouter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	siteID := uuid.New()
	serve := func(p *domain.Principal, method, path, body string) *httptest.ResponseRecorder {
		e := gin.New()
		v1 := e.Group("/api/v1", func(c *gin.Context) {
			if p != nil {
				c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), *p))
			}
			c.Next()
		})
		NewHandler(NewService(nil, nil, nil)).Register(v1)
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		e.ServeHTTP(w, req)
		return w
	}
	key := apiKey(authz.RoleOwner)
	owner := person(authz.RoleOwner)
	viewer := person(authz.RoleViewer)
	cases := []struct {
		name   string
		p      *domain.Principal
		method string
		path   string
		body   string
		status int
		code   string
	}{
		{"key raises the mode", &key, http.MethodPut, "/api/v1/sites/" + siteID.String() + "/ai/mode", `{"mode":"ai_drafts","version":1}`, 403, CodeSessionRequired},
		{"key allows a connection", &key, http.MethodPut, "/api/v1/ai/connections/" + uuid.NewString() + "/auto", `{"ai_auto":"site_setting"}`, 403, CodeSessionRequired},
		{"owner names full", &owner, http.MethodPut, "/api/v1/sites/" + siteID.String() + "/ai/mode", `{"mode":"full","version":1}`, 422, CodeUseFullAutoRoute},
		{"no version", &owner, http.MethodPut, "/api/v1/sites/" + siteID.String() + "/ai/mode", `{"mode":"ask"}`, 422, "invalid_version"},
		{"unknown field", &owner, http.MethodPut, "/api/v1/sites/" + siteID.String() + "/ai/mode", `{"mode":"ask","version":1,"setter":"x"}`, 422, "invalid_body"},
		{"viewer may not write the mode", &viewer, http.MethodPut, "/api/v1/sites/" + siteID.String() + "/ai/mode", `{"mode":"ask","version":1}`, 403, "insufficient_permission"},
		{"no principal", nil, http.MethodPut, "/api/v1/sites/" + siteID.String() + "/ai/mode", `{"mode":"ai_drafts","version":1}`, 401, ""},
		{"bad cursor", &owner, http.MethodGet, "/api/v1/ai/activity?cursor=nope", "", 422, "invalid_cursor"},
		{"bad filter", &owner, http.MethodGet, "/api/v1/ai/activity?filter=everything", "", 422, "invalid_filter"},
		{"bad limit", &owner, http.MethodGet, "/api/v1/ai/activity?limit=101", "", 422, "invalid_limit"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := serve(c.p, c.method, c.path, c.body)
			if w.Code != c.status || (c.code != "" && !strings.Contains(w.Body.String(), `"`+c.code+`"`)) {
				t.Fatalf("got %d %s, want %d %q", w.Code, w.Body.String(), c.status, c.code)
			}
		})
	}
}
