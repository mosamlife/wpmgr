package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	AuthCodeURL(redirectURL string) (url, state, nonce, verifier string, err error)
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
// Google discovery is a network call, so a failure here is returned rather than
// swallowed. Booting with a half-built Google adapter would surface as a broken
// button at sign-in time, long after anyone was watching the logs.
func NewSocialProviders(ctx context.Context, cfg config.SocialConfig) (*SocialProviders, error) {
	sp := &SocialProviders{byKey: map[string]SocialProviderAdapter{}}
	if cfg.Google.Enabled() {
		g, err := newGoogleAdapter(ctx, cfg.Google)
		if err != nil {
			return nil, fmt.Errorf("google sign-in: %w", err)
		}
		sp.byKey["google"] = g
		sp.order = append(sp.order, "google")
	}
	if cfg.GitHub.Enabled() {
		sp.byKey["github"] = newGitHubAdapter(cfg.GitHub)
		sp.order = append(sp.order, "github")
	}
	return sp, nil
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
	cfg      config.GoogleConfig
	endpoint oauth2.Endpoint
	verifier *oidc.IDTokenVerifier
}

const googleIssuer = "https://accounts.google.com"

func newGoogleAdapter(ctx context.Context, cfg config.GoogleConfig) (*googleAdapter, error) {
	provider, err := oidc.NewProvider(ctx, googleIssuer)
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	return &googleAdapter{
		cfg:      cfg,
		endpoint: provider.Endpoint(),
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
	}, nil
}

func (g *googleAdapter) Key() string { return "google" }

func (g *googleAdapter) oauth(redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     g.cfg.ClientID,
		ClientSecret: g.cfg.ClientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     g.endpoint,
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}
}

func (g *googleAdapter) AuthCodeURL(redirectURL string) (string, string, string, string, error) {
	state, err := randString()
	if err != nil {
		return "", "", "", "", err
	}
	nonce, err := randString()
	if err != nil {
		return "", "", "", "", err
	}
	verifier := oauth2.GenerateVerifier()
	url := g.oauth(redirectURL).AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	)
	return url, state, nonce, verifier, nil
}

func (g *googleAdapter) Exchange(ctx context.Context, redirectURL, code, verifier, nonce string) (SocialIdentity, error) {
	tok, err := g.oauth(redirectURL).Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return SocialIdentity{}, fmt.Errorf("code exchange: %w", err)
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return SocialIdentity{}, fmt.Errorf("no id_token in google response")
	}
	// Verifies signature, issuer, audience and expiry.
	idTok, err := g.verifier.Verify(ctx, rawID)
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
}

func newGitHubAdapter(cfg config.GitHubConfig) *githubAdapter {
	return &githubAdapter{cfg: cfg}
}

func (g *githubAdapter) Key() string { return "github" }

func (g *githubAdapter) oauth(redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     g.cfg.ClientID,
		ClientSecret: g.cfg.ClientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     githuboauth.Endpoint,
		// user:email ALONE. It is what reaches /user/emails, which is the only
		// source of a verified address, and GET /user answers unscoped. read:user
		// was granting the whole profile for nothing.
		Scopes: []string{"user:email"},
	}
}

func (g *githubAdapter) AuthCodeURL(redirectURL string) (string, string, string, string, error) {
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
	tok, err := g.oauth(redirectURL).Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return SocialIdentity{}, fmt.Errorf("code exchange: %w", err)
	}
	client := g.oauth(redirectURL).Client(ctx, tok)
	client.Timeout = 15 * time.Second

	var user githubUser
	if err := githubGet(ctx, client, "https://api.github.com/user", &user); err != nil {
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
	if err := githubGet(ctx, client, "https://api.github.com/user/emails", &emails); err != nil {
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
		Name:          name,
	}, nil
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
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github %s: status %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
