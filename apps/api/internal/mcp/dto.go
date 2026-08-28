package mcp

import (
	"errors"
	"net/http"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// Wire shapes. RFC 7591 registration and RFC 6749 token responses are
// snake_case by specification, so these are hand-shaped rather than reusing a
// house DTO convention.
// ---------------------------------------------------------------------------

type registrationRequestDTO struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name"`
	ClientURI               string   `json:"client_uri"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

type registrationResponseDTO struct {
	ClientID                string   `json:"client_id"`
	ClientSecret            string   `json:"client_secret,omitempty"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name,omitempty"`
	ClientURI               string   `json:"client_uri,omitempty"`
}

func toRegistrationResponse(c RegisteredClient) registrationResponseDTO {
	return registrationResponseDTO{
		ClientID:                c.ClientID,
		ClientSecret:            c.ClientSecret,
		TokenEndpointAuthMethod: c.TokenEndpointAuthMethod,
		RedirectURIs:            c.RedirectURIs,
		ClientName:              c.ClientName,
		ClientURI:               c.ClientURI,
	}
}

// consentResponseDTO is what the dashboard renders as the consent screen.
//
// THE FIELD NAMES CARRY THE OBLIGATION. m124 obligation 7: registration is
// unauthenticated, so client_name is an attacker-controlled string and two
// registrations may both claim to be "Claude Desktop" with identical redirect
// URIs -- the database cannot tell them apart and no constraint can fix it. The
// `_unverified` suffix is here so a template author cannot render it as a
// verified identity by accident, and redirect_host is supplied because the host
// is the part of the request a human can actually judge.
type consentResponseDTO struct {
	ClientID             string   `json:"client_id"`
	ClientNameUnverified string   `json:"client_name_unverified"`
	ClientURIUnverified  string   `json:"client_uri_unverified"`
	IdentityVerified     bool     `json:"identity_verified"`
	RedirectURI          string   `json:"redirect_uri"`
	RedirectHost         string   `json:"redirect_host"`
	Scopes               []string `json:"scopes"`
	State                string   `json:"state"`
	CodeChallenge        string   `json:"code_challenge"`
	CodeChallengeMethod  string   `json:"code_challenge_method"`
}

func toConsentResponse(c ConsentContext) consentResponseDTO {
	scopes := make([]string, 0, len(c.Scopes))
	for _, s := range c.Scopes {
		scopes = append(scopes, string(s))
	}
	return consentResponseDTO{
		ClientID:             c.ClientID,
		ClientNameUnverified: c.ClientNameUnverified,
		ClientURIUnverified:  c.ClientURIUnverified,
		// Always false, and it is a literal rather than a field on
		// ConsentContext so there is no code path that can set it true.
		// Registration is unauthenticated; nothing about this identity is
		// verified, ever.
		IdentityVerified:    false,
		RedirectURI:         c.RedirectURI,
		RedirectHost:        c.RedirectHost,
		Scopes:              scopes,
		State:               c.State,
		CodeChallenge:       c.CodeChallenge,
		CodeChallengeMethod: c.CodeChallengeMethod,
	}
}

type approvalRequestDTO struct {
	ClientID            string   `json:"client_id"`
	RedirectURI         string   `json:"redirect_uri"`
	Scopes              []string `json:"scopes"`
	State               string   `json:"state"`
	CodeChallenge       string   `json:"code_challenge"`
	CodeChallengeMethod string   `json:"code_challenge_method"`
	GrantName           string   `json:"name"`
	SiteScopeMode       string   `json:"site_scope_mode"`
	ScopeTagIDs         []string `json:"scope_tag_ids"`
	ScopeSiteIDs        []string `json:"scope_site_ids"`
}

type approvalResponseDTO struct {
	GrantID     string `json:"grant_id"`
	Code        string `json:"code"`
	RedirectURI string `json:"redirect_uri"`
	State       string `json:"state,omitempty"`
}

type tokenRequestDTO struct {
	GrantType    string `json:"grant_type" form:"grant_type"`
	Code         string `json:"code" form:"code"`
	RedirectURI  string `json:"redirect_uri" form:"redirect_uri"`
	ClientID     string `json:"client_id" form:"client_id"`
	ClientSecret string `json:"client_secret" form:"client_secret"`
	CodeVerifier string `json:"code_verifier" form:"code_verifier"`
}

type tokenResponseDTO struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope"`
}

// oauthErrorDTO is the RFC 6749 section 5.2 error envelope. The OAuth
// endpoints must answer in this shape rather than the house error envelope,
// because a client library parses it.
type oauthErrorDTO struct {
	Err     string `json:"error"`
	ErrDesc string `json:"error_description,omitempty"`
}

// oauthError maps a domain error onto the RFC 6749 wire vocabulary and status.
//
// The mapping is EXPLICIT AND CLOSED. An unrecognised domain code falls to
// invalid_request with a 400 rather than to a permissive default, and an error
// that is not a domain error at all is a 500 that says nothing -- an infra
// failure must never be reported to a client as a successful-but-empty grant.
func oauthError(err error) (int, oauthErrorDTO) {
	var domErr *domain.Error
	if !errors.As(err, &domErr) {
		return http.StatusInternalServerError, oauthErrorDTO{Err: "server_error"}
	}

	switch domErr.Code {
	case ErrCodeInvalidScope:
		return http.StatusBadRequest, oauthErrorDTO{Err: "invalid_scope", ErrDesc: domErr.Message}
	case ErrCodeInvalidClient:
		return http.StatusUnauthorized, oauthErrorDTO{Err: "invalid_client", ErrDesc: domErr.Message}
	case ErrCodeInvalidGrant:
		return http.StatusBadRequest, oauthErrorDTO{Err: "invalid_grant", ErrDesc: domErr.Message}
	case ErrCodeUnsupportedResponse:
		return http.StatusBadRequest, oauthErrorDTO{Err: "unsupported_response_type", ErrDesc: domErr.Message}
	case ErrCodeRegistrationInvalid:
		return http.StatusBadRequest, oauthErrorDTO{Err: "invalid_client_metadata", ErrDesc: domErr.Message}
	case ErrCodeInvalidRedirectURI:
		return http.StatusBadRequest, oauthErrorDTO{Err: "invalid_request", ErrDesc: domErr.Message}
	case ErrCodeInvalidSiteScope, ErrCodeInvalidRequest:
		return http.StatusBadRequest, oauthErrorDTO{Err: "invalid_request", ErrDesc: domErr.Message}
	default:
		return http.StatusBadRequest, oauthErrorDTO{Err: "invalid_request", ErrDesc: domErr.Message}
	}
}
