package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// These cover the two social HTTP endpoints as endpoints: what they consume
// from the session, and what they say to a browser. The policy matrix lives in
// social_policy_test.go; nothing here re-asserts it.

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type fakeSocialAdapter struct {
	key      string
	state    string
	urlErr   error
	identity SocialIdentity
	exchErr  error
}

func (f *fakeSocialAdapter) Key() string { return f.key }

func (f *fakeSocialAdapter) AuthCodeURL(_ context.Context, _ string) (string, string, string, string, error) {
	if f.urlErr != nil {
		return "", "", "", "", f.urlErr
	}
	return "https://" + f.key + ".test/authorize?state=" + f.state,
		f.state, "nonce-" + f.key, "verifier-" + f.key, nil
}

func (f *fakeSocialAdapter) Exchange(_ context.Context, _, _, _, _ string) (SocialIdentity, error) {
	if f.exchErr != nil {
		return SocialIdentity{}, f.exchErr
	}
	return f.identity, nil
}

func newSocialProviders(adapters ...*fakeSocialAdapter) *SocialProviders {
	sp := &SocialProviders{byKey: map[string]SocialProviderAdapter{}}
	for _, a := range adapters {
		sp.byKey[a.key] = a
		sp.order = append(sp.order, a.key)
	}
	return sp
}

// socialTestClient drives the two endpoints while carrying the session cookie
// forward exactly as a browser would, which is the whole point: the bug under
// test is about what one request leaves behind for the next one.
type socialTestClient struct {
	engine *gin.Engine
	cookie *http.Cookie
}

func newSocialTestClient(sm *SessionManager, sp *SocialProviders, svc *Service) *socialTestClient {
	gin.SetMode(gin.TestMode)
	h := &Handler{svc: svc, sessions: sm, social: sp}
	r := gin.New()
	r.Use(sm.LoadAndSave())
	r.GET("/auth/social/:provider/start", h.socialStart)
	r.GET("/auth/social/:provider/callback", h.socialCallback)
	return &socialTestClient{engine: r}
}

func (c *socialTestClient) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}
	w := httptest.NewRecorder()
	c.engine.ServeHTTP(w, req)
	for _, ck := range w.Result().Cookies() {
		if ck.Name == "wpmgr_session" && ck.Value != "" {
			c.cookie = ck
		}
	}
	return w
}

// socialErrorOf reads the code the endpoint sent the browser back with, or ""
// when the redirect was not a return to the sign-in page.
func socialErrorOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	if w.Code != http.StatusFound {
		t.Fatalf("expected a 302 redirect, got %d with body %q", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	i := strings.Index(loc, "social_error=")
	if i < 0 {
		return ""
	}
	return loc[i+len("social_error="):]
}

func newTestService() *Service { return &Service{baseURL: "https://app.test"} }

// ---------------------------------------------------------------------------
// 2.24 - a stray callback must not consume somebody else's handshake
// ---------------------------------------------------------------------------

// The handshake lives in one set of session keys shared by every provider, and
// the callback used to pop it before establishing the callback was even for the
// right provider. One request to the other provider's callback (a stale tab, a
// bookmark, or a cross-site top-level navigation, which SameSite=Lax still
// sends the cookie on) therefore emptied the session, and the real callback
// arrived to find nothing.
func TestSocialCallback_MismatchedProviderDoesNotConsumeTheHandshake(t *testing.T) {
	google := &fakeSocialAdapter{key: "google", state: "google-state", exchErr: errors.New("exchange refused")}
	github := &fakeSocialAdapter{key: "github", state: "github-state"}
	c := newSocialTestClient(NewSessionManagerWithStore(scs.New(), false), newSocialProviders(google, github), newTestService())

	if w := c.get(t, "/auth/social/google/start"); w.Code != http.StatusFound {
		t.Fatalf("start: expected a redirect to the provider, got %d", w.Code)
	}

	// The stray callback. It is refused, which is correct.
	if code := socialErrorOf(t, c.get(t, "/auth/social/github/callback?state=whatever&code=x")); code != "social_state_mismatch" {
		t.Fatalf("a callback for the wrong provider should be refused, got %q", code)
	}

	// The legitimate callback must still find its handshake. Reaching the code
	// exchange (which this adapter fails) proves the state check passed; before
	// the fix this reported social_state_mismatch because the handshake had
	// already been popped by the request above.
	if code := socialErrorOf(t, c.get(t, "/auth/social/google/callback?state=google-state&code=abc")); code != "social_exchange_failed" {
		t.Fatalf("the in-flight Google handshake was destroyed by a stray GitHub callback: got %q, want social_exchange_failed", code)
	}
}

// The same argument for a callback that names the right provider but carries
// the wrong state. It is equally not this handshake, and burning the state
// would hand anyone who can cause a navigation a way to break an in-flight
// sign-in.
func TestSocialCallback_WrongStateLeavesTheHandshakeIntact(t *testing.T) {
	google := &fakeSocialAdapter{key: "google", state: "google-state", exchErr: errors.New("exchange refused")}
	c := newSocialTestClient(NewSessionManagerWithStore(scs.New(), false), newSocialProviders(google), newTestService())

	c.get(t, "/auth/social/google/start")

	if code := socialErrorOf(t, c.get(t, "/auth/social/google/callback?state=forged&code=x")); code != "social_state_mismatch" {
		t.Fatalf("a forged state must be refused, got %q", code)
	}
	if code := socialErrorOf(t, c.get(t, "/auth/social/google/callback?state=google-state&code=abc")); code != "social_exchange_failed" {
		t.Fatalf("the real callback should still complete its handshake: got %q", code)
	}
}

// Single use is the other half of the invariant: the handshake that IS consumed
// must not be replayable.
func TestSocialCallback_ConsumesTheHandshakeItMatches(t *testing.T) {
	google := &fakeSocialAdapter{key: "google", state: "google-state", exchErr: errors.New("exchange refused")}
	c := newSocialTestClient(NewSessionManagerWithStore(scs.New(), false), newSocialProviders(google), newTestService())

	c.get(t, "/auth/social/google/start")
	c.get(t, "/auth/social/google/callback?state=google-state&code=abc")

	if code := socialErrorOf(t, c.get(t, "/auth/social/google/callback?state=google-state&code=abc")); code != "social_state_mismatch" {
		t.Fatalf("a matched handshake must be single use: got %q on replay", code)
	}
}

// ---------------------------------------------------------------------------
// 2.28 - /start answers a browser, so it redirects
// ---------------------------------------------------------------------------

func TestSocialStart_UnconfiguredProviderRedirectsRatherThanRenderingJSON(t *testing.T) {
	c := newSocialTestClient(NewSessionManagerWithStore(scs.New(), false), newSocialProviders(&fakeSocialAdapter{key: "google", state: "s"}), newTestService())

	w := c.get(t, "/auth/social/apple/start")
	if w.Code != http.StatusFound {
		t.Fatalf("a browser following a sign-in button must be redirected, not handed a %d JSON body: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Location"); got != "https://app.test/login?social_error=social_provider_disabled" {
		t.Fatalf("unexpected redirect target %q", got)
	}
}

func TestSocialStart_ProviderFailureRedirectsRatherThanRenderingJSON(t *testing.T) {
	broken := &fakeSocialAdapter{key: "google", urlErr: errors.New("discovery unreachable")}
	c := newSocialTestClient(NewSessionManagerWithStore(scs.New(), false), newSocialProviders(broken), newTestService())

	w := c.get(t, "/auth/social/google/start")
	if w.Code != http.StatusFound {
		t.Fatalf("expected a redirect, got %d: %s", w.Code, w.Body.String())
	}
	if code := socialErrorOf(t, w); code != "social_start_failed" {
		t.Fatalf("got code %q, want social_start_failed", code)
	}
}

// ---------------------------------------------------------------------------
// 2.25 - an anonymous GET must not be free
// ---------------------------------------------------------------------------

type stubLimiter struct {
	mu      sync.Mutex
	budget  int
	seen    []string
	perMin  int
	granted int
}

func (s *stubLimiter) Allow(_ context.Context, key string, limitPerMinute int) (bool, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, key)
	s.perMin = limitPerMinute
	if s.granted >= s.budget {
		return false, time.Minute
	}
	s.granted++
	return true, 0
}

func TestSocialStart_IsRateLimitedPerClient(t *testing.T) {
	lim := &stubLimiter{budget: 2}
	svc := newTestService()
	svc.limiter = lim
	c := newSocialTestClient(NewSessionManagerWithStore(scs.New(), false), newSocialProviders(&fakeSocialAdapter{key: "google", state: "s"}), svc)

	for i := 0; i < 2; i++ {
		w := c.get(t, "/auth/social/google/start")
		if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "https://google.test/") {
			t.Fatalf("request %d should have reached the provider, went to %q", i, loc)
		}
	}

	if code := socialErrorOf(t, c.get(t, "/auth/social/google/start")); code != "social_rate_limited" {
		t.Fatalf("an unauthenticated start endpoint with no budget mints a session per request: got %q", code)
	}
	if lim.perMin != socialStartPerMinute {
		t.Errorf("budget passed to the limiter is %d, want socialStartPerMinute (%d)", lim.perMin, socialStartPerMinute)
	}
	// Keyed per client, or one visitor's retries would throttle the whole world.
	for _, k := range lim.seen {
		if !strings.HasPrefix(k, "social-start:192.0.2.1") {
			t.Fatalf("limiter key %q is not scoped to the client address", k)
		}
	}
}

// A limiter that was never wired must not become an outage: self-hosted installs
// construct the service before the limiter exists.
func TestSocialStart_WithoutALimiterStillWorks(t *testing.T) {
	c := newSocialTestClient(NewSessionManagerWithStore(scs.New(), false), newSocialProviders(&fakeSocialAdapter{key: "google", state: "s"}), newTestService())
	if w := c.get(t, "/auth/social/google/start"); w.Code != http.StatusFound || !strings.HasPrefix(w.Header().Get("Location"), "https://google.test/") {
		t.Fatalf("start should still redirect to the provider: %d %q", w.Code, w.Header().Get("Location"))
	}
}

// The other half of the same finding: what the request LEAVES BEHIND. An
// anonymous handshake used to inherit the full signed-in session lifetime, so
// one GET bought a store record for as long as a real session.
func TestPutSocial_AnonymousHandshakeExpiresInMinutes(t *testing.T) {
	m := NewSessionManagerWithStore(scs.New(), false)
	ctx := loadCtx(t, m)

	m.putSocial(ctx, "google", "state-1", "nonce-1", "verifier-1")

	deadline := m.SCS().Deadline(ctx)
	if until := time.Until(deadline); until > socialHandshakeTTL+time.Minute || until <= 0 {
		t.Fatalf("an abandoned handshake expires in %v; it should be bounded by socialHandshakeTTL (%v)", until, socialHandshakeTTL)
	}
}

// ... and it must only shorten the anonymous case. Someone already signed in who
// connects a second provider keeps the deadline their own sign-in earned.
func TestPutSocial_DoesNotShortenAnAuthenticatedSession(t *testing.T) {
	m := NewSessionManagerWithStore(scs.New(), false)
	ctx := loadCtx(t, m)
	if err := m.Login(ctx, uuid.New(), uuid.New()); err != nil {
		t.Fatalf("login: %v", err)
	}
	before := m.SCS().Deadline(ctx)

	m.putSocial(ctx, "google", "state-1", "nonce-1", "verifier-1")

	if after := m.SCS().Deadline(ctx); !after.Equal(before) {
		t.Fatalf("connecting a provider shortened a signed-in session from %v to %v", before, after)
	}
}

// ---------------------------------------------------------------------------
// 2.10 - a second account at the same provider
// ---------------------------------------------------------------------------

// A Workspace address is reassigned, or an account is deleted and recreated, so
// the provider issues a new subject for an address this install already knows.
// The link then trips the one-identity-per-provider index, and the sign-in page
// used to show "Sign-in failed. Please try again." forever, because trying
// again reproduces it exactly.
func TestLinkIdentityError_NamesASecondAccountAtTheSameProvider(t *testing.T) {
	err := linkIdentityError(domain.Internal("identity_create_failed", "failed to link identity").
		WithCause(&pgconn.PgError{Code: "23505", ConstraintName: identityUserProviderIndex}))

	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("expected a domain error, got %v", err)
	}
	if de.Code != "social_provider_already_linked" {
		t.Fatalf("got code %q, want social_provider_already_linked", de.Code)
	}
	if de.Message == "" {
		t.Fatal("the refusal must say what happened")
	}
	// A code the callback will not pass through is a generic failure by another
	// name, so the allowlist is part of this fix, not a detail of it.
	if !actionableSocialCodes[de.Code] {
		t.Fatal("social_provider_already_linked must reach the sign-in page, or the user still sees a generic failure")
	}
}

// The other unique index is two callbacks racing to create the SAME identity.
// The caller can neither cause nor fix that, so it stays generic.
func TestLinkIdentityError_LeavesEveryOtherFailureAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   error
	}{
		{"the identity race", domain.Internal("identity_create_failed", "failed").
			WithCause(&pgconn.PgError{Code: "23505", ConstraintName: "user_identities_provider_subject_key"})},
		{"an unrelated constraint", domain.Internal("identity_create_failed", "failed").
			WithCause(&pgconn.PgError{Code: "23503", ConstraintName: identityUserProviderIndex})},
		{"a plain failure", domain.Internal("identity_create_failed", "failed").WithCause(errors.New("connection reset"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			de, _ := domain.AsDomain(linkIdentityError(tc.in))
			if de == nil || de.Code != "identity_create_failed" {
				t.Fatalf("expected the original error to pass through, got %v", de)
			}
		})
	}
}
