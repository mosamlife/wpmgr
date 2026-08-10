package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	githuboauth "golang.org/x/oauth2/github"

	"github.com/mosamlife/wpmgr/apps/api/internal/config"
)

// A SocialProviderAdapter turns a provider's response into the one shape the
// policy understands. Everything provider-specific lives behind this interface,
// so decideSocial never learns that GitHub is not an OpenID Connect provider.
//
// That containment is the point. The verified-email question is answered
// completely differently by the two providers, and it is the question the
// entire security model rests on, so each adapter answers it once, here, rather
// than the policy growing a branch per vendor.
type SocialProviderAdapter interface {
	// Key is the stable provider identifier stored in user_identities.provider.
	Key() string
	// AuthCodeURL builds the redirect, returning the state, nonce and PKCE
	// verifier the callback must present. nonce is empty for providers with no
	// ID token to bind it to.
	AuthCodeURL(ctx context.Context, redirectURL string) (url, state, nonce, verifier string, err error)
	// Exchange completes the code exchange and returns a verified identity.
	Exchange(ctx context.Context, redirectURL, code, verifier, nonce string) (SocialIdentity, error)
}

// SocialProviders is the set configured on this install, keyed by provider.
type SocialProviders struct {
	byKey map[string]SocialProviderAdapter
	order []string
}

// NewSocialProviders builds the adapters for whatever is configured. A provider
// with no credentials is simply absent: an unconfigured provider must never
// render a button that leads to a provider error page.
//
// This performs NO network I/O and cannot fail. Google's discovery call happens
// on first use instead: see oidc_discovery.go for why it must not be on the
// boot path, and for what a lazy call has to get right that a boot-time one
// never faced.
func NewSocialProviders(cfg config.SocialConfig) *SocialProviders {
	sp := &SocialProviders{byKey: map[string]SocialProviderAdapter{}}
	if cfg.Google.Enabled() {
		sp.byKey["google"] = newGoogleAdapter(cfg.Google)
		sp.order = append(sp.order, "google")
	}
	if cfg.GitHub.Enabled() {
		sp.byKey["github"] = newGitHubAdapter(cfg.GitHub)
		sp.order = append(sp.order, "github")
	}
	return sp
}

// Get returns the adapter for a provider key, or nil.
func (s *SocialProviders) Get(key string) SocialProviderAdapter {
	if s == nil {
		return nil
	}
	return s.byKey[key]
}

// Enabled lists the configured providers, in a stable order, so the sign-in
// page can render exactly the buttons that will work.
func (s *SocialProviders) Enabled() []string {
	if s == nil {
		return nil
	}
	return append([]string(nil), s.order...)
}

// ---------------------------------------------------------------------------
// Google: standard OpenID Connect
// ---------------------------------------------------------------------------

type googleAdapter struct {
	cfg config.GoogleConfig

	// disc holds the issuer metadata, resolved on first use rather than at boot,
	// bounded and shared between concurrent sign-ins. See oidc_discovery.go for
	// why each of those three words is load-bearing.
	disc *oidcDiscovery
}

const googleIssuer = "https://accounts.google.com"

func newGoogleAdapter(cfg config.GoogleConfig) *googleAdapter {
	return &googleAdapter{cfg: cfg, disc: newOIDCDiscovery(googleIssuer)}
}

func (g *googleAdapter) Key() string { return "google" }

func (g *googleAdapter) oauth(redirectURL string, endpoint oauth2.Endpoint) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     g.cfg.ClientID,
		ClientSecret: g.cfg.ClientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     endpoint,
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}
}

func (g *googleAdapter) AuthCodeURL(ctx context.Context, redirectURL string) (string, string, string, string, error) {
	provider, err := g.disc.get(ctx)
	if err != nil {
		return "", "", "", "", err
	}
	state, err := randString()
	if err != nil {
		return "", "", "", "", err
	}
	nonce, err := randString()
	if err != nil {
		return "", "", "", "", err
	}
	verifier := oauth2.GenerateVerifier()
	url := g.oauth(redirectURL, provider.Endpoint()).AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	)
	return url, state, nonce, verifier, nil
}

func (g *googleAdapter) Exchange(ctx context.Context, redirectURL, code, verifier, nonce string) (SocialIdentity, error) {
	provider, err := g.disc.get(ctx)
	if err != nil {
		return SocialIdentity{}, err
	}
	tok, err := g.oauth(redirectURL, provider.Endpoint()).Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return SocialIdentity{}, fmt.Errorf("code exchange: %w", err)
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return SocialIdentity{}, fmt.Errorf("no id_token in google response")
	}
	// Verifies signature, issuer, audience and expiry. The verifier is built per
	// call rather than memoised: it is a thin wrapper, and the JWKS cache that
	// actually matters lives on the shared provider behind it.
	idTok, err := provider.Verifier(&oidc.Config{ClientID: g.cfg.ClientID}).Verify(ctx, rawID)
	if err != nil {
		return SocialIdentity{}, fmt.Errorf("id token verification: %w", err)
	}
	if idTok.Nonce != nonce {
		// Binds this token to the browser that started the flow.
		return SocialIdentity{}, fmt.Errorf("nonce mismatch")
	}
	var claims struct {
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := idTok.Claims(&claims); err != nil {
		return SocialIdentity{}, fmt.Errorf("claims: %w", err)
	}
	if claims.Sub == "" {
		// The subject IS the identity. go-oidc does not require it to be
		// present, and GitHub's adapter has the equivalent guard (user.ID == 0),
		// so the asymmetry is worth closing even though a real Google token
		// always carries one: an empty subject would key an identity row on "".
		return SocialIdentity{}, fmt.Errorf("google id token has no subject")
	}
	// email_verified is passed straight through, NOT assumed. A Google
	// Workspace administrator can create an account bearing an address the
	// organisation does not own, and the resulting ID token is validly signed,
	// so the signature proves Google issued it and says nothing about whether
	// the address belongs to the holder. That distinction is exactly what the
	// claim exists to express.
	return SocialIdentity{
		Provider:      "google",
		Subject:       claims.Sub,
		Issuer:        googleIssuer,
		Email:         claims.Email,
		EmailVerified: claims.EmailVerified,
		Name:          claims.Name,
	}, nil
}

// ---------------------------------------------------------------------------
// GitHub: OAuth 2.0, and no OpenID Connect anywhere in sight
// ---------------------------------------------------------------------------

type githubAdapter struct {
	cfg config.GitHubConfig

	// Test seam. Every outbound detail of this adapter is a field rather than a
	// constant so the failures that matter (a rate limit, a revoked token, a
	// token endpoint that accepts the connection and never answers) can be
	// reproduced against a local server. newGitHubAdapter fills all three with
	// the real values and nothing outside a test changes them.
	apiBase     string
	endpoint    oauth2.Endpoint
	httpTimeout time.Duration
}

// githubAPIBase is where the real thing lives.
const githubAPIBase = "https://api.github.com"

// githubHTTPTimeout bounds every outbound call this adapter makes, INCLUDING
// the code exchange. The exchange used to be the one call with no bound at all:
// it ran on http.DefaultClient, which has no timeout, under whatever context
// the inbound request carried, so a GitHub that accepted the connection and
// then stopped talking held a request handler open indefinitely.
const githubHTTPTimeout = 15 * time.Second

func newGitHubAdapter(cfg config.GitHubConfig) *githubAdapter {
	return &githubAdapter{
		cfg:         cfg,
		apiBase:     githubAPIBase,
		endpoint:    githuboauth.Endpoint,
		httpTimeout: githubHTTPTimeout,
	}
}

func (g *githubAdapter) Key() string { return "github" }

func (g *githubAdapter) oauth(redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     g.cfg.ClientID,
		ClientSecret: g.cfg.ClientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     g.endpoint,
		// user:email ALONE. It is what reaches /user/emails, which is the only
		// source of a verified address, and GET /user answers unscoped. read:user
		// was granting the whole profile for nothing.
		Scopes: []string{"user:email"},
	}
}

func (g *githubAdapter) AuthCodeURL(_ context.Context, redirectURL string) (string, string, string, string, error) {
	state, err := randString()
	if err != nil {
		return "", "", "", "", err
	}
	verifier := oauth2.GenerateVerifier()
	// PKCE, even though GitHub is a confidential client that does not require
	// it. It costs nothing and removes a class of code-interception bug.
	// No nonce: there is no ID token to bind one to, so state carries the
	// whole CSRF burden here.
	url := g.oauth(redirectURL).AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
	return url, state, "", verifier, nil
}

type githubUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type githubEmail struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

func (g *githubAdapter) Exchange(ctx context.Context, redirectURL, code, verifier, _ string) (SocialIdentity, error) {
	// One bounded client for the whole handshake, handed to the exchange through
	// the context because that is the only way oauth2 lets a caller replace the
	// client it would otherwise take from http.DefaultClient. See
	// githubHTTPTimeout for what was wrong without it.
	httpClient := &http.Client{Timeout: g.httpTimeout}
	cctx := context.WithValue(ctx, oauth2.HTTPClient, httpClient)
	xctx, cancel := context.WithTimeout(cctx, g.httpTimeout)
	defer cancel()

	tok, err := g.oauth(redirectURL).Exchange(xctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return SocialIdentity{}, fmt.Errorf("code exchange: %w", err)
	}
	client := g.oauth(redirectURL).Client(cctx, tok)
	client.Timeout = g.httpTimeout

	return g.identity(ctx, client)
}

// identity turns an authorized client into a verified identity. Split from
// Exchange so the part that decides what GitHub told us about a person can be
// tested against a local server, without an OAuth dance in the way.
func (g *githubAdapter) identity(ctx context.Context, client *http.Client) (SocialIdentity, error) {
	var user githubUser
	if err := githubGet(ctx, client, g.apiBase+"/user", &user); err != nil {
		return SocialIdentity{}, fmt.Errorf("fetch user: %w", err)
	}
	if user.ID == 0 {
		return SocialIdentity{}, fmt.Errorf("github user has no id")
	}

	// THE PART THAT MATTERS. /user returns an `email` field that is the
	// PUBLIC profile email, which is null for most accounts and, when present,
	// carries no verification status whatsoever. Trusting it is the standard
	// way this integration is written wrong.
	//
	// The verified address only comes from /user/emails, where each entry has
	// its own primary and verified flags. A GitHub account may hold several
	// addresses at different verification states, so the only one safe to act
	// on is the entry that is both primary and verified.
	var emails []githubEmail
	if err := githubGet(ctx, client, g.apiBase+"/user/emails", &emails); err != nil {
		// Cannot establish a verified address, so under this policy the sign-in
		// fails rather than proceeding with an unverified one.
		return SocialIdentity{}, fmt.Errorf("fetch emails: %w", err)
	}
	email, verified := primaryVerifiedEmail(emails)

	name := user.Name
	if strings.TrimSpace(name) == "" {
		name = user.Login
	}
	return SocialIdentity{
		Provider: "github",
		// Numeric id, not the login: a login can be changed and reused by
		// somebody else, which would silently move an account to a new person.
		Subject:       fmt.Sprintf("%d", user.ID),
		Email:         email,
		EmailVerified: verified,
		// GitHub's privacy address is genuinely primary and genuinely verified,
		// and GitHub discards everything sent to it. See isGitHubNoreply.
		EmailUnreachable: verified && isGitHubNoreply(email),
		Name:             name,
	}, nil
}

// GitHub's privacy addresses. Turning on "Keep my email addresses private"
// makes an outbound-only ID+login@users.noreply.github.com address the account's
// primary, and GitHub reports it through /user/emails as primary and verified
// like any other entry. The old form has no users. prefix.
const (
	githubNoreplyDomain       = "@users.noreply.github.com"
	githubLegacyNoreplyDomain = "@noreply.github.com"
)

// isGitHubNoreply reports whether an address is one GitHub will never deliver
// to.
//
// Accepting one is not a verification failure, it is a DURABILITY failure, and
// that is why it needs its own answer. The address is real and the person is
// who they say they are; nothing we ever send them arrives. Taken as an
// account's email it produces a user who cannot be sent a verification link, a
// password reset, a backup failure or an invitation, and because the GitHub id
// is pinned to that new account by (provider, subject) on first sign-in, coming
// back later with a reachable address does not undo it: the identity resolves
// to the same unreachable account forever. Refusing the FIRST sign-in costs one
// settings toggle and leaves the account reachable for its whole life.
func isGitHubNoreply(email string) bool {
	e := strings.ToLower(strings.TrimSpace(email))
	return strings.HasSuffix(e, githubNoreplyDomain) || strings.HasSuffix(e, githubLegacyNoreplyDomain)
}

// primaryVerifiedEmail returns the address that is both primary and verified.
//
// It deliberately does NOT fall back to the first verified non-primary address.
// A user's primary address is the one they consider theirs, and quietly signing
// them in under a different one would create an account they do not recognise
// and would not find again.
func primaryVerifiedEmail(emails []githubEmail) (string, bool) {
	for _, e := range emails {
		if e.Primary && e.Verified {
			return strings.ToLower(strings.TrimSpace(e.Email)), true
		}
	}
	return "", false
}

func githubGet(ctx context.Context, client *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := client.Do(req)
	if err != nil {
		// Transport level: DNS, TLS, a timeout. Deliberately NOT a
		// githubAPIError, because there is no status to classify and an operator
		// reading the log needs to see that GitHub never answered at all.
		return fmt.Errorf("github %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return newGitHubAPIError(url, resp, time.Now())
	}
	// Bounded: this decodes whatever the far end sends into memory, and the far
	// end is not something this process controls.
	if err := json.NewDecoder(io.LimitReader(resp.Body, githubMaxBodyBytes)).Decode(out); err != nil {
		return fmt.Errorf("github %s: decode response: %w", url, err)
	}
	return nil
}

const (
	// githubMaxBodyBytes caps a successful response. /user and /user/emails are
	// a few hundred bytes; a megabyte is far past any honest answer.
	githubMaxBodyBytes = 1 << 20
	// githubMaxErrorBodyBytes caps the error body read for its message field.
	githubMaxErrorBodyBytes = 8 << 10
)

// githubFailure names WHY a GitHub call failed, which is the distinction that
// was missing.
//
// Every non-200 used to collapse into one fmt.Errorf("status %d"), so a spent
// rate budget, a token the user revoked from their GitHub settings, an OAuth
// app an organisation has not approved for SSO, and GitHub being down all
// reached the operator as the same unactionable line. They need four different
// responses: wait, ask the user to sign in again, approve the app, and do
// nothing.
type githubFailure string

const (
	// githubFailureRateLimited: the budget is spent. RetryAfter says how long.
	githubFailureRateLimited githubFailure = "rate_limited"
	// githubFailureTokenInvalid: 401. The access token was revoked or expired,
	// or the app's credentials are wrong.
	githubFailureTokenInvalid githubFailure = "token_invalid"
	// githubFailureForbidden: a 403 that is not a rate limit. Missing scope, an
	// org's SAML SSO not authorised for this token, or a suspended install.
	githubFailureForbidden githubFailure = "forbidden"
	// githubFailureNotFound: 404. GitHub also answers 404 rather than 403 for
	// resources a token may not see, so this can mean "scope missing" too.
	githubFailureNotFound githubFailure = "not_found"
	// githubFailureUnavailable: 5xx. Their problem, and it will pass.
	githubFailureUnavailable githubFailure = "unavailable"
	// githubFailureUnexpected: anything else.
	githubFailureUnexpected githubFailure = "unexpected"
)

// githubAPIError is a non-200 from GitHub, classified, with whatever wait
// GitHub asked for preserved.
type githubAPIError struct {
	URL     string
	Status  int
	Failure githubFailure
	// Message is GitHub's own "message" field, which usually names the cause
	// exactly ("Bad credentials", "API rate limit exceeded for ...").
	Message string
	// RetryAfter is how long GitHub asked us to wait. Zero when it said
	// nothing. Dropping it, which is what this code did, left an operator
	// unable to tell a sixty second blip from an hour-long lockout.
	RetryAfter time.Duration
}

func (e *githubAPIError) Error() string {
	s := fmt.Sprintf("github %s: %s (status %d)", e.URL, e.Failure, e.Status)
	if e.RetryAfter > 0 {
		s += fmt.Sprintf(", retry after %s", e.RetryAfter.Round(time.Second))
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

func newGitHubAPIError(url string, resp *http.Response, now time.Time) *githubAPIError {
	e := &githubAPIError{
		URL:        url,
		Status:     resp.StatusCode,
		Failure:    classifyGitHubStatus(resp),
		RetryAfter: githubRetryAfter(resp, now),
	}
	var body struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, githubMaxErrorBodyBytes)).Decode(&body); err == nil {
		e.Message = strings.TrimSpace(body.Message)
	}
	return e
}

func classifyGitHubStatus(resp *http.Response) githubFailure {
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return githubFailureTokenInvalid
	case resp.StatusCode == http.StatusTooManyRequests:
		return githubFailureRateLimited
	case resp.StatusCode == http.StatusForbidden:
		// GitHub answers 403 for BOTH a spent rate budget and a genuine
		// authorization failure, so the status alone cannot separate "wait" from
		// "this will never work". The headers can: a primary limit zeroes the
		// remaining budget, a secondary limit sends Retry-After.
		if resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "" {
			return githubFailureRateLimited
		}
		return githubFailureForbidden
	case resp.StatusCode == http.StatusNotFound:
		return githubFailureNotFound
	case resp.StatusCode >= 500:
		return githubFailureUnavailable
	}
	return githubFailureUnexpected
}

// githubRetryAfter reads whatever wait the response asked for. Secondary rate
// limits send Retry-After (delta seconds, or an HTTP date); primary limits send
// x-ratelimit-reset as a Unix epoch instead.
func githubRetryAfter(resp *http.Response, now time.Time) time.Duration {
	if v := strings.TrimSpace(resp.Header.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
		if at, err := http.ParseTime(v); err == nil {
			if d := at.Sub(now); d > 0 {
				return d
			}
		}
	}
	if v := strings.TrimSpace(resp.Header.Get("X-RateLimit-Reset")); v != "" {
		if epoch, err := strconv.ParseInt(v, 10, 64); err == nil {
			if d := time.Unix(epoch, 0).Sub(now); d > 0 {
				return d
			}
		}
	}
	return 0
}
