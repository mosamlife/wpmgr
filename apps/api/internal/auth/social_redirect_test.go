package auth

// social_redirect_test.go: the deep link a shared URL carries must survive a
// provider round trip, and must never become an open redirect on the way.
//
// Two halves, both real: safeReturnPath is exercised directly against the
// inputs an attacker would supply, and the handler tests drive the actual
// socialStart/socialFail code against a real SessionManager so that dropping
// the session write, or trusting the query parameter instead, fails here.

//
// It also pins the redirect_uri derivation itself, and the fact that
// .env.example documents the callback an operator has to register with each
// provider. Same file because both are answers to "where does the browser go",
// and an operator who gets either wrong sees the same broken button.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// fakeSocialAdapter stands in for Google/GitHub so socialStart can be driven
// without a network. It returns fixed handshake values.
type fakeSocialAdapter struct{ key string }

func (f fakeSocialAdapter) Key() string { return f.key }

func (f fakeSocialAdapter) AuthCodeURL(_ context.Context, _ string) (string, string, string, string, error) {
	return "https://provider.example/authorize", "state-1", "nonce-1", "verifier-1", nil
}

func (f fakeSocialAdapter) Exchange(_ context.Context, _, _, _, _ string) (SocialIdentity, error) {
	return SocialIdentity{}, nil
}

// newStartContext builds a gin context for GET /auth/social/google/start with a
// primed SCS session, matching what LoadAndSave provides in production.
func newStartContext(t *testing.T, sm *SessionManager, rawQuery string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/auth/social/google/start?"+rawQuery, nil)
	ctx, err := sm.SCS().Load(req.Context(), "")
	if err != nil {
		t.Fatalf("load session context: %v", err)
	}
	req = req.WithContext(ctx)
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Params = gin.Params{{Key: "provider", Value: "google"}}
	return c, w
}

// newStartHandler is the handler the start tests drive: one working provider
// and a handshake key, since a handler without one refuses to start at all.
func newStartHandler(t *testing.T, sm *SessionManager) *Handler {
	t.Helper()
	h := &Handler{
		svc:      &Service{baseURL: "https://manage.example"},
		sessions: sm,
		social: &SocialProviders{
			byKey: map[string]SocialProviderAdapter{"google": fakeSocialAdapter{key: "google"}},
			order: []string{"google"},
		},
	}
	if err := h.SetHandshakeSecret(strings.Repeat("k", 32)); err != nil {
		t.Fatalf("handshake key: %v", err)
	}
	return h
}

// startedHandshake opens the handshake cookie a start response set.
func startedHandshake(t *testing.T, h *Handler, w *httptest.ResponseRecorder) handshake {
	t.Helper()
	for _, ck := range w.Result().Cookies() {
		if ck.Name != handshakeCookieName {
			continue
		}
		hs, ok := h.handshake.open(ck.Value)
		if !ok {
			t.Fatal("the handshake cookie the start issued does not open")
		}
		return hs
	}
	t.Fatal("the start issued no handshake cookie")
	return handshake{}
}

func TestSafeReturnPathAcceptsOnlySameOriginPaths(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain path", "/sites/abc", "/sites/abc"},
		{"path with query", "/sites?tab=backups", "/sites?tab=backups"},
		{"empty", "", ""},
		{"relative", "sites/abc", ""},
		{"absolute url", "https://evil.example/steal", ""},
		{"protocol relative", "//evil.example/steal", ""},
		{"backslash protocol relative", `/\evil.example/steal`, ""},
		{"scheme only", "javascript:alert(1)", ""},
		{"control character", "/sites\n/evil", ""},
		{"absurdly long", "/" + string(make([]byte, 600)), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := safeReturnPath(tc.in); got != tc.want {
				t.Fatalf("safeReturnPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSocialLandingPathFallsBackToSites(t *testing.T) {
	if got := socialLandingPath("/sites/abc/backups"); got != "/sites/abc/backups" {
		t.Fatalf("landing path = %q, want the deep link", got)
	}
	if got := socialLandingPath(""); got != "/sites" {
		t.Fatalf("landing path with no deep link = %q, want /sites", got)
	}
	if got := socialLandingPath("https://evil.example/"); got != "/sites" {
		t.Fatalf("landing path for an off-site target = %q, want /sites", got)
	}
}

// TestSocialStartCarriesDeepLinkThroughTheHandshake is the regression test for
// the dropped ?redirect=: the value must land in the handshake, where the
// callback can read it back, and must not be carried in the authorization URL.
func TestSocialStartCarriesDeepLinkThroughTheHandshake(t *testing.T) {
	sm := newTestSessionManager(t)
	h := newStartHandler(t, sm)

	c, w := newStartContext(t, sm, "redirect="+url.QueryEscape("/sites/abc/backups"))
	h.socialStart(c)

	if w.Code != http.StatusFound {
		t.Fatalf("socialStart status = %d, want 302", w.Code)
	}
	hs := startedHandshake(t, h, w)
	if hs.Provider != "google" || hs.State != "state-1" {
		t.Fatalf("handshake not carried: provider=%q state=%q", hs.Provider, hs.State)
	}
	if hs.Return != "/sites/abc/backups" {
		t.Fatalf("handshake return path = %q, want the deep link", hs.Return)
	}
	// It travels with us, not through the provider: a value handed to the
	// provider comes back as a value an attacker can choose.
	if loc := w.Header().Get("Location"); strings.Contains(loc, "sites") {
		t.Fatalf("the deep link reached the authorization URL: %q", loc)
	}
}

// An off-site ?redirect= must be discarded at the start of the handshake, so it
// can never reach the Location header the callback writes.
func TestSocialStartDropsOffSiteDeepLink(t *testing.T) {
	sm := newTestSessionManager(t)
	h := newStartHandler(t, sm)

	c, w := newStartContext(t, sm, "redirect="+url.QueryEscape("https://evil.example/steal"))
	h.socialStart(c)

	if got := startedHandshake(t, h, w).Return; got != "" {
		t.Fatalf("carried return path = %q, want it discarded", got)
	}
}

// A provider this install has not configured must not answer a browser with
// JSON. It is mid-navigation with nothing else on the page, so the only useful
// answer is the sign-in page and a code it can turn into a sentence.
func TestSocialStartRedirectsForAnUnconfiguredProvider(t *testing.T) {
	sm := newTestSessionManager(t)
	h := newStartHandler(t, sm)

	c, w := newStartContext(t, sm, "")
	c.Params = gin.Params{{Key: "provider", Value: "gitlab"}}
	h.socialStart(c)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 rather than a JSON body", w.Code)
	}
	if got := w.Header().Get("Location"); !strings.Contains(got, "social_error=social_provider_disabled") {
		t.Fatalf("Location = %q, want the sign-in page carrying social_provider_disabled", got)
	}
}

// An unreachable issuer is the other half of the same rule. For Google this is
// discovery failing, which since discovery left the boot path is the ONLY place
// an unreachable issuer shows up; answering it with a JSON 500 left a person
// looking at raw text with no way back to the sign-in page.
func TestSocialStartRedirectsWhenTheProviderCannotBeReached(t *testing.T) {
	sm := newTestSessionManager(t)
	h := newStartHandler(t, sm)
	h.social = &SocialProviders{
		byKey: map[string]SocialProviderAdapter{
			"google": stubAdapter{key: "google", err: errors.New("discovery: dial tcp: i/o timeout")},
		},
		order: []string{"google"},
	}

	c, w := newStartContext(t, sm, "redirect="+url.QueryEscape("/sites/abc"))
	h.socialStart(c)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 rather than a JSON body", w.Code)
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if loc.Path != "/login" {
		t.Fatalf("landed on %q, want the sign-in page", loc.Path)
	}
	if got := loc.Query().Get("social_error"); got != "social_url_failed" {
		t.Fatalf("social_error = %q, want social_url_failed", got)
	}
	// The refusal keeps the deep link, like every other refusal on this flow,
	// so recovering with a password still lands where the person was going.
	if got := loc.Query().Get("redirect"); got != "/sites/abc" {
		t.Fatalf("redirect = %q, want the deep link preserved", got)
	}
}

// A refusal sends the browser back to the sign-in page. It must carry the deep
// link too, or recovering by password loses the link the refusal interrupted.
func TestSocialFailPreservesDeepLink(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/auth/social/google/callback", nil)

	h := &Handler{svc: &Service{baseURL: "https://manage.example"}}
	h.socialFail(c, "email_not_verified", "/sites/abc")

	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if loc.Path != "/login" {
		t.Fatalf("refusal landed on %q, want /login", loc.Path)
	}
	if got := loc.Query().Get("social_error"); got != "email_not_verified" {
		t.Fatalf("social_error = %q", got)
	}
	if got := loc.Query().Get("redirect"); got != "/sites/abc" {
		t.Fatalf("redirect = %q, want the deep link", got)
	}
}

func TestSocialFailRefusesOffSiteDeepLink(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/auth/social/google/callback", nil)

	h := &Handler{svc: &Service{baseURL: "https://manage.example"}}
	h.socialFail(c, "social_cancelled", "https://evil.example/steal")

	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if got := loc.Query().Get("redirect"); got != "" {
		t.Fatalf("redirect = %q, want it dropped", got)
	}
}

// THE SUCCESS PATH, which is the headline behaviour and was the one line no
// test reached: everything above it in socialCallback needs a live provider and
// a database, so socialComplete exists as the seam. Replace socialLandingPath
// with the old hardcoded "/sites" and these fail.
func TestSocialCompleteLandsOnTheDeepLink(t *testing.T) {
	gin.SetMode(gin.TestMode)
	sm := newTestSessionManager(t)
	h := &Handler{svc: &Service{baseURL: "https://manage.example"}, sessions: sm}

	cases := []struct {
		name     string
		returnTo string
		want     string
	}{
		{"deep link", "/sites/abc/backups", "https://manage.example/sites/abc/backups"},
		{"deep link with query", "/sites?tab=backups", "https://manage.example/sites?tab=backups"},
		{"no deep link", "", "https://manage.example/sites"},
		// Defence in depth. Only safeReturnPath can write the session value, so
		// this is unreachable today; it is asserted because socialComplete is
		// what writes a Location header, and that is the wrong place to rely on
		// somebody else's validation still being there next year.
		{"off-site target", "https://evil.example/steal", "https://manage.example/sites"},
		{"protocol relative target", "//evil.example/steal", "https://manage.example/sites"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			req, _ := http.NewRequest(http.MethodGet, "/auth/social/google/callback", nil)
			ctx, err := sm.SCS().Load(req.Context(), "")
			if err != nil {
				t.Fatalf("load session context: %v", err)
			}
			c.Request = req.WithContext(ctx)

			// No second factor, so this is the plain "session issued, now go
			// somewhere" ending.
			h.socialComplete(c, LoginResult{User: User{ID: uuid.New(), Status: "active"}}, tc.returnTo)

			if w.Code != http.StatusFound {
				t.Fatalf("status = %d, want 302", w.Code)
			}
			if got := w.Header().Get("Location"); got != tc.want {
				t.Fatalf("landed on %q, want %q", got, tc.want)
			}
			// And the session really was issued: a redirect to the app with no
			// session behind it would bounce straight back to /login.
			if _, _, ok := sm.Current(c.Request.Context()); !ok {
				t.Fatal("no session was established before the redirect")
			}
		})
	}
}

// The handshake no longer touches the session, so a social flow started after
// an abandoned one cannot inherit anything from it: there is nothing left in
// the session to inherit, and each handshake cookie replaces the last.
func TestASecondStartReplacesTheFirstHandshake(t *testing.T) {
	sm := newTestSessionManager(t)
	h := newStartHandler(t, sm)

	c, first := newStartContext(t, sm, "redirect="+url.QueryEscape("/sites/abc"))
	h.socialStart(c)
	if got := startedHandshake(t, h, first).Return; got != "/sites/abc" {
		t.Fatalf("first handshake return path = %q", got)
	}

	c2, second := newStartContext(t, sm, "")
	h.socialStart(c2)
	if got := startedHandshake(t, h, second).Return; got != "" {
		t.Fatalf("the second handshake inherited the first one's landing page: %q", got)
	}
}

// TestSocialRedirectURL pins the derivation itself, including the two ways an
// operator's value differs from the canonical one.
func TestSocialRedirectURL(t *testing.T) {
	cases := []struct {
		name string
		base string
		want string
	}{
		{"plain origin", "https://manage.example.com", "https://manage.example.com/auth/social/google/callback"},
		{"trailing slash", "https://manage.example.com/", "https://manage.example.com/auth/social/google/callback"},
		{"surrounding whitespace", "  https://manage.example.com  ", "https://manage.example.com/auth/social/google/callback"},
		{"path prefix", "https://example.com/wpmgr", "https://example.com/wpmgr/auth/social/google/callback"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SocialRedirectURL(tc.base, "google"); got != tc.want {
				t.Errorf("SocialRedirectURL(%q) = %q, want %q", tc.base, got, tc.want)
			}
		})
	}
}

// envExampleCallbackLine matches a documented callback URL in .env.example:
// a comment line holding nothing but an absolute /auth/social/<provider>/callback
// URL. That is the shape an operator copies into a provider's console.
var envExampleCallbackLine = regexp.MustCompile(`^#\s+(https?://\S+/auth/social/([a-z]+)/callback)\s*$`)

// TestEnvExampleDocumentsTheDerivedSocialCallback checks the callback URLs
// .env.example tells operators to register against the URL this code actually
// produces, from that same file's own WPMGR_PUBLIC_BASE_URL.
//
// This exists because the two were once written independently, and drifted: the
// file documented a callback on the web container's port while the code derived
// one on the API's, so an operator who followed the instructions to the letter
// got redirect_uri_mismatch from the provider. That failure has no symptom to
// search for, since both URLs look plausible and neither the log nor the
// dashboard mentions either.
//
// So the documentation is not allowed to be a second, hand-maintained copy of
// the rule. It is checked against SocialRedirectURL, the one function the
// running server uses.
func TestEnvExampleDocumentsTheDerivedSocialCallback(t *testing.T) {
	// internal/auth -> internal -> apps/api -> apps -> repo root.
	path := filepath.Join("..", "..", "..", "..", ".env.example")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}

	var base string
	var documented []string
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "WPMGR_PUBLIC_BASE_URL="); ok {
			base = v
			continue
		}
		if m := envExampleCallbackLine.FindStringSubmatch(line); m != nil {
			documented = append(documented, m[1])
		}
	}

	if base == "" {
		t.Fatal(".env.example must set WPMGR_PUBLIC_BASE_URL: the social callback is derived from it, so a file that documents a callback without setting it documents nothing")
	}
	if len(documented) == 0 {
		t.Fatal(".env.example must document the callback URLs to register at the providers: deriving the redirect_uri means the operator cannot work it out from a provider console")
	}

	for _, got := range documented {
		provider := envExampleCallbackLine.FindStringSubmatch("# " + got)[2]
		want := SocialRedirectURL(base, provider)
		if got != want {
			t.Errorf("\n.env.example tells operators to register:\n  %s\nbut with its own WPMGR_PUBLIC_BASE_URL=%s this code derives:\n  %s\nRegistering the documented URL would fail with redirect_uri_mismatch.", got, base, want)
		}
	}
}
