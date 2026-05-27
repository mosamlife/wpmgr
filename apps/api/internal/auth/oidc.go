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
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// NewOIDCProvider builds an OIDCProvider from config, discovering the issuer's
// endpoints. Returns (nil, nil) when OIDC is disabled (issuer unset).
func NewOIDCProvider(ctx context.Context, cfg config.OIDCConfig) (*OIDCProvider, error) {
	if !cfg.Enabled() {
		return nil, nil
	}
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery for issuer %q: %w", cfg.Issuer, err)
	}
	return &OIDCProvider{
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
	}, nil
}

// Enabled reports whether OIDC is configured (provider may be nil).
func (p *OIDCProvider) Enabled() bool { return p != nil }

// AuthCodeURL builds the provider authorization URL with PKCE, state and nonce.
// The returned verifier must be persisted (in the session) for the callback.
func (p *OIDCProvider) AuthCodeURL() (url, state, nonce, verifier string, err error) {
	state, err = randString()
	if err != nil {
		return "", "", "", "", err
	}
	nonce, err = randString()
	if err != nil {
		return "", "", "", "", err
	}
	verifier = oauth2.GenerateVerifier()
	url = p.oauth.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	)
	return url, state, nonce, verifier, nil
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
	tok, err := p.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return OIDCClaims{}, fmt.Errorf("oidc token exchange: %w", err)
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return OIDCClaims{}, fmt.Errorf("oidc response missing id_token")
	}
	idToken, err := p.verifier.Verify(ctx, rawID)
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
