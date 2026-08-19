package tests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
)

// firstRunHTTP stands the register endpoint up over the real Gin router, with
// the real session middleware, so the request travels the path an operator's
// request actually travels.
//
// THE SERVICE-LEVEL TESTS CANNOT COVER THIS. Every other test in this change
// calls svc.Bootstrap directly and hands it a claim string, which means the one
// piece of code that decides where that string comes from — bootstrapClaim(c),
// reading X-Wpmgr-Bootstrap-Claim off the request — was never executed by
// anything. A header typo, a wrong constant or a handler that read the body
// instead would have left all eight of them green while the only route an
// operator has was broken.
func firstRunHTTP(t *testing.T, claimSecret string) (*gin.Engine, *auth.Service) {
	t.Helper()
	pool := startPostgres(t)
	svc, _ := newAuthStack(pool)
	if claimSecret != "" {
		svc.SetBootstrapClaimSecret(claimSecret)
	}

	sm := auth.NewSessionManagerWithStore(scs.New(), false)
	h := auth.NewHandler(svc, sm, nil, makeCreateTenant(t, pool))
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(sm.LoadAndSave())
	h.Register(r)
	return r, svc
}

// postRegister issues the request an installer makes. A header value of "" is
// sent as no header at all, which is the case an ordinary visitor produces.
func postRegister(t *testing.T, r *gin.Engine, header, email string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"email":    email,
		"password": "a-very-strong-password",
		"name":     "Owner",
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/auth/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if header != "" {
		req.Header.Set("X-Wpmgr-Bootstrap-Claim", header)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestFirstRunHeader_CorrectHeaderEstablishesOwnership is the does-not-over-fire
// half at HTTP level: the header an installer sets reaches the claim check, and
// the request completes with a created owner and a session cookie.
//
// To watch it go red: change the header name in either BootstrapClaimHeader
// (internal/auth/bootstrap_claim.go) or in postRegister above, so the two stop
// agreeing.
func TestFirstRunHeader_CorrectHeaderEstablishesOwnership(t *testing.T) {
	r, _ := firstRunHTTP(t, testClaim)

	w := postRegister(t, r, testClaim, "owner@example.com")
	if w.Code != http.StatusCreated {
		t.Fatalf("correct header: status = %d, want %d; body = %s", w.Code, http.StatusCreated, w.Body.String())
	}

	// The session is what makes this the whole round trip rather than a 201
	// with nothing behind it: bootstrap issues one in the same request.
	var sawSession bool
	for _, c := range w.Result().Cookies() {
		if c.Name == "wpmgr_session" && c.Value != "" {
			sawSession = true
		}
	}
	if !sawSession {
		t.Fatalf("correct header did not issue a session cookie; got %v", w.Result().Cookies())
	}

	// And the response describes the owner, not a pending self-serve signup:
	// the Me payload carries the account and its one owner membership.
	var me struct {
		User struct {
			Email string `json:"email"`
		} `json:"user"`
		Memberships []struct {
			Role string `json:"role"`
		} `json:"memberships"`
		ActiveTenantID string `json:"active_tenant_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
		t.Fatalf("decode body: %v (%s)", err, w.Body.String())
	}
	if me.User.Email != "owner@example.com" {
		t.Fatalf("response user.email = %q, want owner@example.com", me.User.Email)
	}
	if len(me.Memberships) != 1 || me.Memberships[0].Role != "owner" {
		t.Fatalf("response memberships = %+v, want exactly one owner", me.Memberships)
	}
	if me.ActiveTenantID == "" {
		t.Fatal("response carried no active tenant")
	}
}

// TestFirstRunHeader_WrongAndAbsentHeadersAreRefusedIdentically is the fires
// half, and the indistinguishability check where it actually matters: on the
// wire, including the status code.
//
// To watch it go red: give the no-claim-configured or wrong-claim branch its own
// status or error code in Service.Bootstrap.
func TestFirstRunHeader_WrongAndAbsentHeadersAreRefusedIdentically(t *testing.T) {
	// A configured install, still unowned.
	configured, _ := firstRunHTTP(t, testClaim)
	wrong := postRegister(t, configured, "not-the-claim-value-at-all", "a@example.com")

	// An install with no claim configured at all.
	unconfigured, _ := firstRunHTTP(t, "")
	unset := postRegister(t, unconfigured, testClaim, "b@example.com")

	for name, w := range map[string]*httptest.ResponseRecorder{
		"wrong header":        wrong,
		"no claim configured": unset,
	} {
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want %d; body = %s", name, w.Code, http.StatusForbidden, w.Body.String())
		}
		if code := errorCodeOf(t, w); code != "registration_closed" {
			t.Fatalf("%s: error code = %q, want registration_closed", name, code)
		}
		for _, c := range w.Result().Cookies() {
			if c.Name == "wpmgr_session" && c.Value != "" {
				t.Fatalf("%s: a refusal issued a session cookie", name)
			}
		}
	}

	// Byte-identical, not merely same-coded.
	if wrong.Body.String() != unset.Body.String() {
		t.Fatalf("refusal bodies differ:\n wrong header:        %s\n no claim configured: %s",
			wrong.Body.String(), unset.Body.String())
	}

	// The claim must never appear in a response.
	if containsSecret(unset.Body.String()) {
		t.Fatalf("a refusal echoed the provisioning claim: %s", unset.Body.String())
	}
}

// TestFirstRunHeader_AbsentHeaderTakesTheSelfServePath proves the routing half:
// no header is not a refusal, it is "this is an ordinary registration". On an
// unowned install that writes nothing and answers generically, so the
// first-account slot is still there for the installer afterwards.
//
// To watch it go red: make the handler treat an absent header as a bootstrap
// attempt (drop the `claim != ""` condition in register).
func TestFirstRunHeader_AbsentHeaderTakesTheSelfServePath(t *testing.T) {
	r, svc := firstRunHTTP(t, testClaim)

	w := postRegister(t, r, "", "visitor@example.com")
	if w.Code == http.StatusCreated {
		t.Fatalf("a request with no claim header established ownership: %s", w.Body.String())
	}
	if w.Code == http.StatusForbidden {
		t.Fatalf("a request with no claim header was refused rather than routed to self-serve; "+
			"that answers 'is this install unowned?' to anyone who asks: %s", w.Body.String())
	}

	// And the installer can still claim it afterwards, over HTTP.
	after := postRegister(t, r, testClaim, "owner@example.com")
	if after.Code != http.StatusCreated {
		t.Fatalf("the claim must still work after an anonymous registration attempt: status %d, body %s",
			after.Code, after.Body.String())
	}
	_ = svc
}

func errorCodeOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, w.Body.String())
	}
	if body.Error.Code != "" {
		return body.Error.Code
	}
	return body.Code
}
