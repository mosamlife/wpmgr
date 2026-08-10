package auth

// social_handshake_test.go: what an unauthenticated caller can make the server
// store, and what the callback will accept back.
//
// The first test is the reproduction of the finding: GET
// /auth/social/:provider/start needs no session, no CSRF token and no
// credential of any kind, and it used to write a session record per request,
// each one held for the session store's idle lifetime (7 days by default). One
// laptop could therefore fill the shared Redis that every live session lives
// in. The rest of the file pins the replacement: a signed, short-lived cookie
// that the server does not store at all.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/alexedwards/scs/v2/memstore"
	"github.com/gin-gonic/gin"
)

// countingStore is a session store that records how many records were written
// to it. It is the whole point of these tests: "did this request write to the
// shared session store" is the question, and no assertion about handlers or
// cookies answers it.
type countingStore struct {
	scs.Store
	mu      sync.Mutex
	commits int
}

func (s *countingStore) Commit(token string, b []byte, expiry time.Time) error {
	s.mu.Lock()
	s.commits++
	s.mu.Unlock()
	return s.Store.Commit(token, b, expiry)
}

func (s *countingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commits
}

// newCountingSessionManager builds a SessionManager whose writes are counted,
// with the production cookie policy.
func newCountingSessionManager(t *testing.T) (*SessionManager, *countingStore) {
	t.Helper()
	store := &countingStore{Store: memstore.New()}
	m := scs.New()
	m.Store = store
	// The shipped defaults, so the record lifetime under test is the real one.
	m.IdleTimeout = 168 * time.Hour
	m.Lifetime = 720 * time.Hour
	return NewSessionManagerWithStore(m, false), store
}

// newSocialTestHandler builds a handler with one working provider and a keyed
// handshake codec, mounted on a real engine with the session middleware, which
// is what makes the store writes happen (or not).
func newSocialTestHandler(t *testing.T, sm *SessionManager) (*gin.Engine, *Handler) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := &Handler{
		svc:      &Service{baseURL: "https://manage.example"},
		sessions: sm,
		social: &SocialProviders{
			byKey: map[string]SocialProviderAdapter{"google": fakeSocialAdapter{key: "google"}},
			order: []string{"google"},
		},
	}
	h.SetHandshakeSecret(strings.Repeat("k", 32))
	r := gin.New()
	r.Use(sm.LoadAndSave())
	r.GET("/auth/social/:provider/start", h.socialStart)
	return r, h
}

// THE FINDING. An unauthenticated flood of the start endpoint must not be able
// to make the server store anything at all. Before the handshake moved into a
// signed cookie this wrote one session record per request, each one kept for
// the store's idle lifetime, which is a way to exhaust the Redis that holds
// every live session on the instance.
//
// Deliberately counts STORE WRITES rather than asserting a rate limit. A rate
// limit rations the resource and has to be told apart from legitimate traffic;
// this asserts the resource is not consumed, which needs no such judgement.
func TestSocialStartWritesNothingToTheSessionStore(t *testing.T) {
	sm, store := newCountingSessionManager(t)
	r, _ := newSocialTestHandler(t, sm)

	const floods = 50
	for i := 0; i < floods; i++ {
		w := httptest.NewRecorder()
		// No cookie, exactly like an attacker who discards every response.
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/auth/social/google/start", nil))
		if w.Code != http.StatusFound {
			t.Fatalf("start %d: status = %d, want 302", i, w.Code)
		}
	}

	if got := store.count(); got != 0 {
		t.Fatalf("%d unauthenticated starts wrote %d session records; want 0. "+
			"Each record is held for the session idle lifetime, so this is an "+
			"unauthenticated way to fill the store that every live session shares.", floods, got)
	}
}

// The handshake still has to reach the callback, so the cookie is not merely
// absent state: it carries provider, state, nonce, verifier and the deep link,
// and the callback gets exactly what the start put in.
func TestSocialHandshakeCookieRoundTrip(t *testing.T) {
	sm, store := newCountingSessionManager(t)
	r, h := newSocialTestHandler(t, sm)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/auth/social/google/start?redirect="+url.QueryEscape("/sites/abc/backups"), nil))

	ck := findCookie(t, w.Result().Cookies(), handshakeCookieName)
	got, ok := h.handshake.open(ck.Value)
	if !ok {
		t.Fatal("the handshake cookie the start issued does not open")
	}
	if got.Provider != "google" || got.State != "state-1" || got.Nonce != "nonce-1" || got.Verifier != "verifier-1" {
		t.Fatalf("handshake = %+v, want the adapter's values", got)
	}
	if got.Return != "/sites/abc/backups" {
		t.Fatalf("handshake return path = %q, want the deep link", got.Return)
	}
	if store.count() != 0 {
		t.Fatalf("the round trip wrote %d session records, want 0", store.count())
	}
}

// The cookie policy is load-bearing and every attribute answers something:
//
//   - HttpOnly: script must not be able to read the verifier.
//   - SameSite=Lax: the provider sends the browser back with a top-level GET,
//     which Lax allows and Strict does not. Strict here is not "safer", it is a
//     handshake that never completes.
//   - host-only (no Domain): a sibling host on the registrable domain must not
//     be able to plant or read it.
//   - a short Max-Age: the value is worthless after the callback, and an
//     abandoned handshake should not sit in the browser for a week.
func TestSocialHandshakeCookiePolicy(t *testing.T) {
	sm, _ := newCountingSessionManager(t)
	r, _ := newSocialTestHandler(t, sm)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/auth/social/google/start", nil))
	ck := findCookie(t, w.Result().Cookies(), handshakeCookieName)

	if !ck.HttpOnly {
		t.Error("the handshake cookie must be HttpOnly: it carries the PKCE verifier")
	}
	if ck.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax: the provider returns via a top-level navigation", ck.SameSite)
	}
	if ck.Domain != "" {
		t.Errorf("Domain = %q, want host-only", ck.Domain)
	}
	if ck.MaxAge <= 0 || time.Duration(ck.MaxAge)*time.Second > handshakeTTL {
		t.Errorf("Max-Age = %d, want a positive value no longer than the handshake TTL", ck.MaxAge)
	}
	if ck.Path != handshakeCookiePath {
		t.Errorf("Path = %q, want %q so it is not sent on ordinary app requests", ck.Path, handshakeCookiePath)
	}
}

// Secure follows the same switch as every other cookie this handler sets, so a
// production instance never emits the handshake over plain HTTP.
func TestSocialHandshakeCookieIsSecureInProduction(t *testing.T) {
	sm, _ := newCountingSessionManager(t)
	r, h := newSocialTestHandler(t, sm)
	h.SetSecureCookies(true)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/auth/social/google/start", nil))
	if ck := findCookie(t, w.Result().Cookies(), handshakeCookieName); !ck.Secure {
		t.Error("the handshake cookie must be Secure when the instance is production")
	}
}

// A cookie the server did not sign is not a handshake. Without this the
// provider, the deep link and the expiry would all be caller-chosen.
func TestSocialHandshakeRefusesTamperedCookies(t *testing.T) {
	c, err := newHandshakeCodec(strings.Repeat("k", 32))
	if err != nil {
		t.Fatalf("codec: %v", err)
	}
	sealed, err := c.seal(handshake{Provider: "google", State: "s", Nonce: "n", Verifier: "v"}, handshakeTTL)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if _, ok := c.open(sealed); !ok {
		t.Fatal("an untouched cookie must open")
	}

	cases := map[string]string{
		"flipped byte":     flipLastByte(sealed),
		"truncated":        sealed[:len(sealed)/2],
		"empty":            "",
		"not base64":       "not-a-cookie!!",
		"appended garbage": sealed + "AAAA",
	}
	for name, raw := range cases {
		if _, ok := c.open(raw); ok {
			t.Errorf("%s: opened, want refused", name)
		}
	}

	// A different instance secret must not open it either, which is what stops
	// one install's handshake being replayed at another.
	other, err := newHandshakeCodec(strings.Repeat("j", 32))
	if err != nil {
		t.Fatalf("codec: %v", err)
	}
	if _, ok := other.open(sealed); ok {
		t.Error("a handshake sealed with a different secret opened")
	}
}

// The browser holds this cookie, so its own Max-Age proves nothing. The expiry
// travels inside the sealed payload and is checked here.
func TestSocialHandshakeRefusesExpiredCookies(t *testing.T) {
	c, err := newHandshakeCodec(strings.Repeat("k", 32))
	if err != nil {
		t.Fatalf("codec: %v", err)
	}
	sealed, err := c.seal(handshake{Provider: "google", State: "s"}, -time.Second)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, ok := c.open(sealed); ok {
		t.Fatal("an expired handshake opened; the cookie's own Max-Age is the caller's to ignore")
	}
}

// An unkeyed handler must refuse to start a handshake rather than issue one
// nobody signed. Fail closed: an unsigned cookie is a handshake whose provider
// and landing page the caller writes.
func TestSocialStartRefusesWithoutAHandshakeKey(t *testing.T) {
	sm, _ := newCountingSessionManager(t)
	gin.SetMode(gin.TestMode)
	h := &Handler{
		svc:      &Service{baseURL: "https://manage.example"},
		sessions: sm,
		social: &SocialProviders{
			byKey: map[string]SocialProviderAdapter{"google": fakeSocialAdapter{key: "google"}},
			order: []string{"google"},
		},
	}
	r := gin.New()
	r.Use(sm.LoadAndSave())
	r.GET("/auth/social/:provider/start", h.socialStart)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/auth/social/google/start", nil))

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want a 302 back to the sign-in page", w.Code)
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "social_error=social_start_failed") {
		t.Fatalf("Location = %q, want the sign-in page carrying a refusal code", loc)
	}
	for _, ck := range w.Result().Cookies() {
		if ck.Name == handshakeCookieName && ck.Value != "" {
			t.Fatal("an unkeyed handler issued a handshake cookie")
		}
	}
}

// newCallbackRequest drives one callback with the handshake hs in its cookie.
func newCallbackRequest(t *testing.T, h *Handler, sm *SessionManager, provider, query string, hs handshake) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := withHandshake(t, h,
		httptest.NewRequest(http.MethodGet, "/auth/social/"+provider+"/callback?"+query, nil), hs)
	ctx, err := sm.SCS().Load(req.Context(), "")
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	c.Request = req.WithContext(ctx)
	c.Params = gin.Params{{Key: "provider", Value: provider}}
	h.socialCallback(c)
	return w
}

// The handshake names the provider it was started for, and the callback checks
// it. Without that check a code obtained at one provider could be presented to
// another provider's callback, which is now a property of a value the browser
// carries rather than one the server remembered, so it is worth its own test.
func TestSocialCallbackRefusesAHandshakeStartedForAnotherProvider(t *testing.T) {
	sm, _ := newCountingSessionManager(t)
	_, h := newSocialTestHandler(t, sm)
	h.social = &SocialProviders{
		byKey: map[string]SocialProviderAdapter{
			"google": fakeSocialAdapter{key: "google"},
			"github": fakeSocialAdapter{key: "github"},
		},
		order: []string{"google", "github"},
	}

	w := newCallbackRequest(t, h, sm, "github", "state=state-1&code=abc",
		handshake{Provider: "google", State: "state-1", Nonce: "n", Verifier: "v"})

	if got := w.Header().Get("Location"); !strings.Contains(got, "social_error=social_state_mismatch") {
		t.Fatalf("Location = %q, want a refusal: the handshake was started for another provider", got)
	}
}

// A handshake is single-use however the callback ends, so the cookie has to go
// even on a refusal. Otherwise a failed callback leaves a live handshake in the
// browser for its full lifetime, and the state check is the only thing standing
// between that and a replay.
func TestSocialCallbackClearsTheHandshakeCookie(t *testing.T) {
	sm, _ := newCountingSessionManager(t)
	_, h := newSocialTestHandler(t, sm)

	w := newCallbackRequest(t, h, sm, "google", "state=wrong&code=abc",
		handshake{Provider: "google", State: "state-1", Nonce: "n", Verifier: "v"})

	ck := findCookie(t, w.Result().Cookies(), handshakeCookieName)
	if ck.Value != "" || ck.MaxAge >= 0 {
		t.Fatalf("handshake cookie after a refused callback = %+v, want it expired", ck)
	}
	if ck.Path != handshakeCookiePath {
		t.Errorf("deletion Path = %q, want %q, or the browser keeps the original alongside it", ck.Path, handshakeCookiePath)
	}
}

// TestDisabledProviderCallbackKeepsTheDeepLink covers the one callback branch
// that threw the return path away.
//
// The window is narrow, a provider switched off between the start and the
// callback, and the consequence is pure UX: the person is dropped on a bare
// sign-in page having lost the deep link they followed in the first place.
// Every other failure branch on this handler hands the return path to
// socialFail, so this one was simply an omission, and the sealed handshake is
// the only carrier of that link. Reading it before failing costs nothing:
// takeHandshake clears the cookie either way, so the handshake stays
// single-use, and socialFail re-validates the path through safeReturnPath.
func TestDisabledProviderCallbackKeepsTheDeepLink(t *testing.T) {
	sm, _ := newCountingSessionManager(t)
	_, h := newSocialTestHandler(t, sm)

	// github was never configured on this handler, which is what a provider
	// switched off mid-flow looks like at the callback.
	w := newCallbackRequest(t, h, sm, "github", "state=state-1&code=abc",
		handshake{Provider: "github", State: "state-1", Nonce: "n", Verifier: "v", Return: "/sites/abc"})

	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if got := loc.Query().Get("social_error"); got != "social_provider_disabled" {
		t.Fatalf("social_error = %q, want social_provider_disabled", got)
	}
	if got := loc.Query().Get("redirect"); got != "/sites/abc" {
		t.Fatalf("redirect = %q, want the deep link the handshake was carrying", got)
	}

	// Still single-use: the cookie has to go however this branch ends.
	ck := findCookie(t, w.Result().Cookies(), handshakeCookieName)
	if ck.Value != "" || ck.MaxAge >= 0 {
		t.Fatalf("handshake cookie = %+v, want it expired", ck)
	}
}

// TestDisabledProviderCallbackStillRefusesAnOffSiteReturn is the guard on the
// test above. The handshake is sealed, so its contents are authentic, but
// authentic is not the same as safe: this branch must go through the same
// same-origin check as every other, or a value that got into a handshake would
// become an open redirect on the way out.
func TestDisabledProviderCallbackStillRefusesAnOffSiteReturn(t *testing.T) {
	sm, _ := newCountingSessionManager(t)
	_, h := newSocialTestHandler(t, sm)

	w := newCallbackRequest(t, h, sm, "github", "state=state-1&code=abc",
		handshake{Provider: "github", State: "state-1", Nonce: "n", Verifier: "v", Return: "https://evil.example/steal"})

	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if got := loc.Query().Get("redirect"); got != "" {
		t.Fatalf("redirect = %q, want it dropped", got)
	}
}

// withHandshake attaches a handshake this handler sealed to req, which is what
// a browser returning from a provider carries. Shared by the callback tests.
func withHandshake(t *testing.T, h *Handler, req *http.Request, hs handshake) *http.Request {
	t.Helper()
	if h.handshake == nil {
		t.Fatal("handler has no handshake key")
	}
	sealed, err := h.handshake.seal(hs, handshakeTTL)
	if err != nil {
		t.Fatalf("seal handshake: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: handshakeCookieName, Value: sealed})
	return req
}

func findCookie(t *testing.T, cookies []*http.Cookie, name string) *http.Cookie {
	t.Helper()
	for _, ck := range cookies {
		if ck.Name == name {
			return ck
		}
	}
	t.Fatalf("no %q cookie in the response", name)
	return nil
}

func flipLastByte(s string) string {
	if s == "" {
		return "x"
	}
	b := []byte(s)
	if b[len(b)-1] == 'A' {
		b[len(b)-1] = 'B'
	} else {
		b[len(b)-1] = 'A'
	}
	return string(b)
}
