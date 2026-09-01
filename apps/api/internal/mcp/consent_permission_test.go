package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// ONE OBJECT, ONE PERMISSION.
//
// POST /api/v1/mcp/connections creates a connection behind
// authz.RequirePermission(authz.PermAPIKeyManage). GET /oauth/mcp/authorize and
// POST /oauth/mcp/consent create the same object -- a grant with a capability
// set and an organisation-wide site scope -- by the other route. These tests
// prove the two routes agree on what that costs, and that the refusal arrives
// in the envelope the consent screen can read rather than the house one.
//
// Both arms are here on purpose. The refusal arm alone would pass for a guard
// that refuses everyone, which is not a fix; the admit arm is what makes the
// refusal mean something.
// ---------------------------------------------------------------------------

// consentRouterAs mounts the REAL Register group -- the same middleware chain
// server.New mounts -- behind a principal of the test's choosing. It stands in
// for session auth + authz.RequireAuth + authz.RequireTenant, and deliberately
// applies no authorization of its own: everything under test belongs to
// Handler.Register.
func consentRouterAs(t *testing.T, svc *Service, p domain.Principal) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	})
	NewHandler(svc).Register(r.Group("/api/v1"))
	return r
}

// orgPrincipalWithRole is an ORG-scoped operator -- not site-constrained, so
// requireOrgScope admits it and the role is the only thing left to decide.
func orgPrincipalWithRole(role string) domain.Principal {
	return domain.Principal{
		Type:     domain.PrincipalUser,
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Scope:    domain.ScopeOrg,
		Role:     role,
	}
}

func authorizeQuery() url.Values {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", registeredClientID)
	q.Set("redirect_uri", registeredRedirect)
	q.Set("scope", string(ScopeRead))
	q.Set("state", "opaque-state")
	q.Set("code_challenge", "a-real-challenge-value")
	q.Set("code_challenge_method", "S256")
	return q
}

func consentBody(t *testing.T) string {
	t.Helper()
	raw, err := json.Marshal(approvalRequestDTO{
		ClientID:            registeredClientID,
		RedirectURI:         registeredRedirect,
		Scopes:              []string{string(ScopeRead)},
		State:               "opaque-state",
		CodeChallenge:       "a-real-challenge-value",
		CodeChallengeMethod: "S256",
		GrantName:           "Claude Desktop on my laptop",
		SiteScopeMode:       string(SiteScopeModeAll),
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func postConsent(t *testing.T, r *gin.Engine) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/mcp/consent",
		strings.NewReader(consentBody(t)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func getAuthorize(t *testing.T, r *gin.Engine) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/api/v1/oauth/mcp/authorize?"+authorizeQuery().Encode(), nil))
	return w
}

// TestConsentRoutesRequireTheSamePermissionAsMinting is the refusal arm.
//
// Every role below is org-scoped, so requireOrgScope admits all of them: the
// only thing that can refuse here is the permission gate. The roles chosen are
// the ones authz.minRoleFor places BELOW PermAPIKeyManage, plus the empty role,
// which a hand-built principal carries and which must fail closed.
func TestConsentRoutesRequireTheSamePermissionAsMinting(t *testing.T) {
	for _, role := range []string{"viewer", "operator", "client", ""} {
		name := role
		if name == "" {
			name = "no role at all"
		}
		t.Run(name, func(t *testing.T) {
			p := orgPrincipalWithRole(role)
			if p.IsSiteConstrained() {
				t.Fatal("the principal under test is site-constrained; requireOrgScope " +
					"would refuse it and this test would prove nothing about the permission")
			}
			if authz.PrincipalAllows(p, authz.PermAPIKeyManage) {
				t.Fatalf("role %q holds PermAPIKeyManage; it cannot demonstrate a refusal", role)
			}

			store := approvalStore()
			r := consentRouterAs(t, auditedService(store), p)

			// The write first: it is the one that mints.
			w := postConsent(t, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("POST /consent = %d, want 403; body = %s\n"+
					"POST /api/v1/mcp/connections requires PermAPIKeyManage to create "+
					"the same object", w.Code, w.Body.String())
			}
			for _, c := range store.calls {
				if c == "CreateGrantWithCode" {
					t.Fatal("a refused consent still created a grant; a minted grant is " +
						"not undone by refusing afterwards")
				}
			}
			if strings.Contains(w.Body.String(), `"code"`) {
				t.Fatalf("a refused consent leaked a code: %s", w.Body.String())
			}

			// THE ENVELOPE IS PART OF THE ASSERTION. The consent screen reads
			// `error` and `error_description`; the house {code, message} shape
			// arrives there as an unexplained server fault.
			var body oauthErrorDTO
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("consent refusal is not the RFC 6749 error envelope: %v (%s)",
					err, w.Body.String())
			}
			if body.Err != "access_denied" {
				t.Errorf("error = %q, want access_denied (body %s)", body.Err, w.Body.String())
			}
			if body.ErrDesc == "" {
				t.Error("the refusal carries no error_description; the screen has nothing to show")
			}

			// And the read half of the pair, which is where the operator starts.
			a := getAuthorize(t, r)
			if a.Code != http.StatusForbidden {
				t.Fatalf("GET /authorize = %d, want 403; body = %s", a.Code, a.Body.String())
			}
			var authBody oauthErrorDTO
			if err := json.Unmarshal(a.Body.Bytes(), &authBody); err != nil {
				t.Fatalf("authorize refusal is not the RFC 6749 error envelope: %v (%s)",
					err, a.Body.String())
			}
			if authBody.Err != "access_denied" {
				t.Errorf("error = %q, want access_denied (body %s)", authBody.Err, a.Body.String())
			}
		})
	}
}

// TestConsentStillAdmitsAPrincipalThatHoldsThePermission is the over-fire arm.
// A gate that refuses everyone is not a fix -- it is an outage with a
// justification, and it gets switched off.
func TestConsentStillAdmitsAPrincipalThatHoldsThePermission(t *testing.T) {
	for _, role := range []string{"admin", "owner"} {
		t.Run(role, func(t *testing.T) {
			p := orgPrincipalWithRole(role)
			if !authz.PrincipalAllows(p, authz.PermAPIKeyManage) {
				t.Fatalf("role %q does not hold PermAPIKeyManage; this arm would pass "+
					"for the wrong reason", role)
			}

			store := approvalStore()
			r := consentRouterAs(t, auditedService(store), p)

			if a := getAuthorize(t, r); a.Code != http.StatusOK {
				t.Fatalf("OVER-FIRE: GET /authorize = %d for %s, want 200; body = %s",
					a.Code, role, a.Body.String())
			}

			w := postConsent(t, r)
			if w.Code != http.StatusOK {
				t.Fatalf("OVER-FIRE: POST /consent = %d for %s, want 200; body = %s\n"+
					"the gate refuses the people the feature is for", w.Code, role, w.Body.String())
			}
			var out approvalResponseDTO
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode: %v (%s)", err, w.Body.String())
			}
			if out.Code == "" || out.GrantID == "" {
				t.Fatalf("OVER-FIRE: /consent answered 200 with grant_id=%q code=%q; "+
					"a consent that mints nothing is not a success", out.GrantID, out.Code)
			}
			var minted bool
			for _, c := range store.calls {
				if c == "CreateGrantWithCode" {
					minted = true
				}
			}
			if !minted {
				t.Fatal("a 200 consent never reached CreateGrantWithCode")
			}
		})
	}
}

// TestConsentPermissionGateResolvesTheRoleTheSameWay pins the MECHANISM rather
// than the outcome. The gate asks authz.PrincipalAllows, which is what
// authz.RequirePermission asks on the minting route, so a capability-scoped
// principal is held to its EXPLICIT capability set and never to the role it
// happens to carry (m120/#510). A gate that compared Role directly would admit
// the principal below, which the minting route refuses -- the two routes would
// have drifted apart again in a way no role-based test would show.
func TestConsentPermissionGateResolvesTheRoleTheSameWay(t *testing.T) {
	key := domain.Principal{
		Type:         domain.PrincipalAPIKey,
		APIKeyID:     uuid.New(),
		TenantID:     uuid.New(),
		Scope:        domain.ScopeOrg,
		AuthModel:    domain.AuthModelCapability,
		Role:         "owner", // the role says yes ...
		Capabilities: []string{string(authz.PermSiteRead)}, // ... the capability set says no
	}
	if !key.IsCapabilityScoped() {
		t.Fatal("the fixture is not capability-scoped; it would exercise the role path")
	}
	if authz.PrincipalAllows(key, authz.PermAPIKeyManage) {
		t.Fatal("authz.PrincipalAllows admits this principal; the fixture no longer " +
			"describes what it claims to")
	}

	store := approvalStore()
	r := consentRouterAs(t, auditedService(store), key)
	w := postConsent(t, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("POST /consent = %d, want 403; body = %s\n"+
			"the role was consulted instead of the explicit capability set",
			w.Code, w.Body.String())
	}
	for _, c := range store.calls {
		if c == "CreateGrantWithCode" {
			t.Fatal("a refused consent still created a grant")
		}
	}
}
