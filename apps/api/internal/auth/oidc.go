package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/mosamlife/wpmgr/apps/api/internal/config"
)

// OIDCProvider is the OpenID Connect relying-party wrapper (coreos/go-oidc v3 +
// golang.org/x/oauth2). It is nil when OIDC is not configured; callers must
// check Enabled before using it so the routes can return a clean 501.
type OIDCProvider struct {
	cfg config.OIDCConfig
	// disc holds the issuer metadata, resolved on first use. See
	// oidc_discovery.go, and NewOIDCProvider for why it is not resolved here.
	disc *oidcDiscovery
}

// NewOIDCProvider builds an OIDCProvider from config. Returns nil when OIDC is
// disabled (issuer unset).
//
// This performs NO network I/O and cannot fail. It used to call
// oidc.NewProvider inline and main treated the error as fatal, so an operator
// whose identity provider was unreachable, or merely slow to answer at the
// moment the control plane restarted, could not start the control plane at all:
// backups, updates, uptime and every dashboard down, because the thing that
// serves ONE sign-in button could not be contacted. The same defect was fixed
// for the consumer providers and left here.
//
// Discovery now happens on the first sign-in that needs it, which also means a
// transient failure clears itself. Under the old shape it needed a redeploy.
func NewOIDCProvider(cfg config.OIDCConfig) *OIDCProvider {
	if !cfg.Enabled() {
		return nil
	}
	return &OIDCProvider{cfg: cfg, disc: newOIDCDiscovery(cfg.Issuer)}
}

// Enabled reports whether OIDC is configured (provider may be nil).
//
// It answers from configuration alone, NOT from whether discovery has
// succeeded. The sign-in page uses this to decide whether to render the SSO
// button, and an operator who has configured an issuer wants the button plus a
// legible failure, not a button that silently disappears whenever their identity
// provider hiccups.
func (p *OIDCProvider) Enabled() bool { return p != nil }

// AuthCodeURL builds the provider authorization URL with PKCE, state and nonce.
// The returned verifier must be persisted (in the session) for the callback.
//
// It takes a context because this is where discovery now happens, so this is
// the first call that can fail because the issuer is unreachable.
func (p *OIDCProvider) AuthCodeURL(ctx context.Context) (url, state, nonce, verifier string, err error) {
	provider, err := p.disc.get(ctx)
	if err != nil {
		return "", "", "", "", err
	}
	state, err = randString()
	if err != nil {
		return "", "", "", "", err
	}
	nonce, err = randString()
	if err != nil {
		return "", "", "", "", err
	}
	verifier = oauth2.GenerateVerifier()
	url = p.oauth(provider.Endpoint()).AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	)
	return url, state, nonce, verifier, nil
}

func (p *OIDCProvider) oauth(endpoint oauth2.Endpoint) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.cfg.ClientID,
		ClientSecret: p.cfg.ClientSecret,
		RedirectURL:  p.cfg.RedirectURL,
		Endpoint:     endpoint,
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}
}

// OIDCClaims are the standard claims we read from a verified ID token.
type OIDCClaims struct {
	Subject       string
	Issuer        string
	Email         string
	EmailVerified bool
	Name          string
}

// Exchange completes the code exchange and verifies the ID token (signature,
// audience, expiry, and the nonce). It returns the verified claims.
func (p *OIDCProvider) Exchange(ctx context.Context, code, verifier, expectedNonce string) (OIDCClaims, error) {
	provider, err := p.disc.get(ctx)
	if err != nil {
		return OIDCClaims{}, err
	}
	tok, err := p.oauth(provider.Endpoint()).Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return OIDCClaims{}, fmt.Errorf("oidc token exchange: %w", err)
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return OIDCClaims{}, fmt.Errorf("oidc response missing id_token")
	}
	idToken, err := provider.Verifier(&oidc.Config{ClientID: p.cfg.ClientID}).Verify(ctx, rawID)
	if err != nil {
		return OIDCClaims{}, fmt.Errorf("verify id_token: %w", err)
	}
	if idToken.Nonce != expectedNonce {
		return OIDCClaims{}, fmt.Errorf("oidc nonce mismatch")
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return OIDCClaims{}, fmt.Errorf("decode id_token claims: %w", err)
	}
	// `sub` IS THE IDENTITY, so an empty one is not an anonymous sign-in, it is
	// a shared bucket: every user of a provider that omits the claim would key
	// to the same (provider, "", issuer) row and sign into whichever account
	// reached it first. The verifier does not require the claim (it checks
	// signature, audience, expiry and nonce), so this is checked here, at the
	// boundary where an ID token becomes an identity.
	if idToken.Subject == "" {
		return OIDCClaims{}, fmt.Errorf("id_token has no sub claim")
	}
	return OIDCClaims{
		Subject:       idToken.Subject,
		Issuer:        idToken.Issuer,
		Email:         claims.Email,
		EmailVerified: claims.EmailVerified,
		Name:          claims.Name,
	}, nil
}

func randString() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
