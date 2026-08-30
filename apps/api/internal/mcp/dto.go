package mcp

import (
	"errors"
	"net/http"
	"time"

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

// ---------------------------------------------------------------------------
// The operator-facing connections surface (S16). These are HOUSE routes under
// /api/v1 read by the dashboard, NOT OAuth endpoints, so they answer in the
// house error envelope via httpx.Error rather than in oauthErrorDTO.
// ---------------------------------------------------------------------------

// protocolReportDTO carries the four-state classification onto the wire WITHOUT
// flattening it into a nullable string.
//
// State is always present and is the field to switch on. Version is a *string
// and is null for `never_connected` and for `absent` -- those two states have no
// version because the client named none, and emitting the negotiated floor
// there would put a claim in the client's mouth. A consumer that reads Version
// and ignores State gets null, which is the fail-visible direction.
type protocolReportDTO struct {
	State   string  `json:"state"`
	Version *string `json:"version"`
}

// connectionDTO is one row of the connections list.
//
// EVERY NULLABLE FIELD IS A POINTER AND CARRIES NO `omitempty`. That is
// deliberate on both counts: `omitempty` would DROP the key entirely for a
// never-used connection, and a missing key is a third state the consumer has to
// guess at, whereas an explicit `"last_used_at": null` says "never" out loud.
// The frontend model (apps/web/src/features/ai-connections/connection-model.ts)
// maps null onto its own `{kind: "never"}` tag.
type connectionDTO struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Status        string   `json:"status"`
	SiteScopeMode string   `json:"site_scope_mode"`
	Scopes        []string `json:"scopes"`
	CreatedAt     string   `json:"created_at"`

	// The client's OWN unverified claims. Never defaulted to Name: one is the
	// client's assertion and the other is the operator's, and the `reported_`
	// prefix is here so a template author cannot render the first as the second.
	ReportedClientName    *string `json:"reported_client_name"`
	ReportedClientVersion *string `json:"reported_client_version"`

	Protocol protocolReportDTO `json:"protocol"`

	// null means NEVER USED. It is not omitted and it is never a zero date.
	LastUsedAt *string `json:"last_used_at"`
	// null means not revoked.
	RevokedAt *string `json:"revoked_at"`
}

// connectionListDTO wraps the list in an OBJECT rather than returning a bare
// JSON array, and the wrapper is a safety property rather than a style
// preference.
//
// A bare `[]` response body and an error body are both valid JSON, and a client
// that forgets to check the status code can decode an error into a slice and
// get length zero -- "you have no connections" rendered from a failure. The
// house error envelope is `{"code":…,"message":…}`, which cannot decode into
// this struct's `connections` array at all, so the two shapes are
// distinguishable even by a caller that ignores the status.
type connectionListDTO struct {
	// Always non-nil on a 200. A nil slice would marshal as `null`, which a
	// consumer would have to special-case into a third state.
	Connections []connectionDTO `json:"connections"`
}

// revokeResponseDTO reports WHAT THE REVOKE DID, not merely that it returned.
//
// The counts are on the wire because three different successes are possible and
// they are not interchangeable to a human: a first revoke that killed two live
// tokens, an idempotent retry that had nothing left to do, and the repair of a
// half-revoked grant whose tokens were still live. An operator reading
// "revoked, 0 tokens" after a first revoke has learned something worth knowing.
type revokeResponseDTO struct {
	// Status is the END STATE, which is 'revoked' for every success here --
	// including the idempotent retry, because the grant IS revoked.
	Status string `json:"status"`
	// GrantsRevoked is 1 when this call flipped the grant, 0 when it was
	// already revoked.
	GrantsRevoked int64 `json:"grants_revoked"`
	// TokensRevoked is how many live bearer tokens this call killed.
	TokensRevoked int64 `json:"tokens_revoked"`
	// AlreadyRevoked is true when the grant was not active when the statement
	// ran. The request still succeeded.
	AlreadyRevoked bool `json:"already_revoked"`
}

// toConnectionDTO maps one domain Connection onto the wire.
//
// Timestamps become RFC 3339 STRINGS through a nil-preserving helper rather
// than being handed to encoding/json as *time.Time. Both marshal identically
// today; the string form is used so that the nil check is written once, here,
// where it is visible, instead of relying on a marshaller detail to keep
// "never" out of the year 1.
func toConnectionDTO(c Connection) connectionDTO {
	scopes := make([]string, 0, len(c.Scopes))
	for _, s := range c.Scopes {
		scopes = append(scopes, string(s))
	}

	proto := protocolReportDTO{State: string(c.Protocol.State)}
	// Version travels ONLY for the two states that have one. Guarding on the
	// state rather than on the string being non-empty is what stops a future
	// classifier bug from leaking a version onto `absent`.
	if c.Protocol.State == ClientProtocolRecognised || c.Protocol.State == ClientProtocolUnrecognised {
		v := c.Protocol.Version
		proto.Version = &v
	}

	return connectionDTO{
		ID:                    c.ID.String(),
		Name:                  c.Name,
		Status:                string(c.Status),
		SiteScopeMode:         string(c.SiteScopeMode),
		Scopes:                scopes,
		CreatedAt:             c.CreatedAt.UTC().Format(time.RFC3339Nano),
		ReportedClientName:    c.ReportedClientName,
		ReportedClientVersion: c.ReportedClientVersion,
		Protocol:              proto,
		LastUsedAt:            isoOrNil(c.LastUsedAt),
		RevokedAt:             isoOrNil(c.RevokedAt),
	}
}

// toConnectionListDTO maps the whole list, guaranteeing a non-nil slice so a
// genuinely empty organisation serialises as `[]` and never as `null`.
func toConnectionListDTO(cs []Connection) connectionListDTO {
	out := make([]connectionDTO, 0, len(cs))
	for _, c := range cs {
		out = append(out, toConnectionDTO(c))
	}
	return connectionListDTO{Connections: out}
}

// isoOrNil renders a time as RFC 3339, preserving nil as nil. It is the one
// place "never" is kept out of the year 1.
func isoOrNil(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
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
