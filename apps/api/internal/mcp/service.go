package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Lifetimes. The code is short-lived because it is a bearer credential in a
// URL; the token's is the connection's own lifetime.
const (
	authorizationCodeTTL = 5 * time.Minute
	connectionTokenTTL   = 90 * 24 * time.Hour
	tokenPrefixLen       = 12
)

// Error codes. These are domain codes; the OAuth wire errors are mapped from
// them in dto.go so that RFC 6749 section 5.2 naming lives at the edge.
const (
	ErrCodeInvalidClient       = "mcp_invalid_client"
	ErrCodeInvalidRedirectURI  = "mcp_invalid_redirect_uri"
	ErrCodeInvalidGrant        = "mcp_invalid_grant"
	ErrCodeInvalidRequest      = "mcp_invalid_request"
	ErrCodeUnsupportedResponse = "mcp_unsupported_response_type"
	ErrCodeRegistrationInvalid = "mcp_invalid_client_metadata"

	// ErrCodeScopeEmpty is the NAMED refusal for a grant whose site scope
	// resolves to no sites at all -- a tag that matches nothing, or a list
	// whose every id was dropped by tenant RLS. It exists because the only
	// alternative is returning an empty success, and an empty result that
	// reads as "nothing to do" is how a scoping bug becomes invisible.
	ErrCodeScopeEmpty = "mcp_scope_empty"
)

// Clock is injectable so expiry behaviour is testable without sleeping.
type Clock func() time.Time

// Service carries the OAuth surface. It holds no plaintext credential beyond
// the response it is building: m124 obligation 6 says the plaintext is returned
// once at creation and never read back, and there is no cache here.
type Service struct {
	store Store
	now   Clock
}

func NewService(store Store) *Service {
	return &Service{store: store, now: time.Now}
}

// WithClock returns a copy using the supplied clock. Test-only in practice.
func (s *Service) WithClock(c Clock) *Service {
	cp := *s
	cp.now = c
	return &cp
}

// ---------------------------------------------------------------------------
// RFC 7591 dynamic client registration
// ---------------------------------------------------------------------------

// RegistrationRequest is the RFC 7591 client metadata a client POSTs. It is
// UNAUTHENTICATED, so every string in here is attacker-controlled and none of
// it may be presented as verified.
type RegistrationRequest struct {
	RedirectURIs            []string
	ClientName              string
	ClientURI               string
	TokenEndpointAuthMethod string
}

// RegisteredClient is what registration returns. ClientSecret is present
// exactly once, here, and is never readable again (m124 obligation 6).
type RegisteredClient struct {
	ClientID                string
	ClientSecret            string
	TokenEndpointAuthMethod string
	RedirectURIs            []string
	ClientName              string
	ClientURI               string
}

// Register implements RFC 7591 dynamic client registration.
func (s *Service) Register(ctx context.Context, req RegistrationRequest) (RegisteredClient, error) {
	if len(req.RedirectURIs) == 0 {
		return RegisteredClient{}, domain.Validation(ErrCodeRegistrationInvalid,
			"redirect_uris is required and must name at least one URI")
	}
	for _, raw := range req.RedirectURIs {
		if err := validateRedirectURI(raw); err != nil {
			return RegisteredClient{}, err
		}
	}

	// RFC 7591 section 2: an omitted token_endpoint_auth_method defaults to
	// client_secret_basic. That default is the RFC's and it is the RESTRICTIVE
	// direction -- it mints a secret that Exchange then REQUIRES the client to
	// present, over HTTP Basic or in the body, and verifies against the stored
	// hash in constant time. 'none' must be asked for explicitly, which is the
	// case that matters, because 'none' is the no-secret PKCE-only path.
	//
	// That claim was false when it was first written: the token endpoint read no
	// secret at all, so the default minted a credential nothing ever asked for
	// and the sentence described a mode that did not exist (review finding H3).
	// The check now exists; if it is ever removed, this comment must go with it.
	method := req.TokenEndpointAuthMethod
	if method == "" {
		method = "client_secret_basic"
	}
	switch method {
	case "none", "client_secret_basic", "client_secret_post":
	default:
		return RegisteredClient{}, domain.Validation(ErrCodeRegistrationInvalid,
			fmt.Sprintf("token_endpoint_auth_method %q is not supported", method))
	}

	clientID, err := randomToken(24)
	if err != nil {
		return RegisteredClient{}, fmt.Errorf("generate client_id: %w", err)
	}

	// 'none' if and only if there is no secret -- the schema's
	// mcp_oauth_clients_secret_matches_method_check makes the incoherent row
	// unrepresentable, and this is the Go side of the same invariant so we
	// never attempt one.
	var (
		secret     string
		secretHash *string
	)
	if method != "none" {
		secret, err = randomToken(32)
		if err != nil {
			return RegisteredClient{}, fmt.Errorf("generate client_secret: %w", err)
		}
		h := hashCredential(secret)
		secretHash = &h
	}

	// THE WRITE. It returns a count, not a row: the register GUC enables no
	// SELECT policy, so RETURNING would raise 42501 and roll the whole
	// registration back. See Repo.RegisterClient.
	affected, err := s.store.RegisterClient(ctx, sqlc.RegisterMCPOAuthClientParams{
		ClientID:                clientID,
		ClientSecretHash:        secretHash,
		TokenEndpointAuthMethod: method,
		RedirectUris:            req.RedirectURIs,
		ClientName:              nullableText(strings.TrimSpace(req.ClientName)),
		ClientUri:               nullableText(strings.TrimSpace(req.ClientURI)),
	})
	if err != nil {
		return RegisteredClient{}, fmt.Errorf("register client: %w", err)
	}
	// A ZERO-ROW WRITE IS A FAILURE, LOUDLY. The query is :execrows rather than
	// :exec precisely so there is something to assert here. An INSERT ... VALUES
	// with no ON CONFLICT writes one row or raises, so 0 should be unreachable
	// -- which is exactly why reaching it must stop the registration rather than
	// be shrugged off. A silent 0 here would hand back a client_id and a secret
	// for a row that does not exist.
	if affected != 1 {
		return RegisteredClient{}, fmt.Errorf(
			"register client %q: wrote %d rows, want exactly 1", clientID, affected)
	}

	// THE READ-BACK, in its own transaction under the lookup GUC. Two round
	// trips is the deliberate cost of not granting the unauthenticated
	// registration endpoint the ability to read this table.
	stored, err := s.store.LookupClient(ctx, clientID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The insert reported one row and the read found none. That is a
			// broken invariant, not a client with no details, and it must NOT
			// be papered over by rebuilding the response from req -- doing so
			// would report a fabricated success on top of a real failure.
			return RegisteredClient{}, fmt.Errorf(
				"register client %q: wrote the registration but could not read it back", clientID)
		}
		return RegisteredClient{}, fmt.Errorf("read back registered client %q: %w", clientID, err)
	}

	// Built from the STORED row, so the response describes what the database
	// actually holds rather than what the caller asked for. The two can differ
	// -- trimming, and the auth method defaulted above -- and the stored value
	// is the true one. ClientSecret is the sole exception: it exists only here,
	// in memory, and there is no column to read it back from (m124 obligation 6).
	return RegisteredClient{
		ClientID:                stored.ClientID,
		ClientSecret:            secret, // once, here, never again
		TokenEndpointAuthMethod: stored.TokenEndpointAuthMethod,
		RedirectURIs:            stored.RedirectUris,
		ClientName:              derefString(stored.ClientName),
		ClientURI:               derefString(stored.ClientUri),
	}, nil
}

// validateRedirectURI refuses anything that is not an absolute URI we are
// willing to send a code to. A fragment is refused because RFC 6749 3.1.2
// forbids one; a non-https non-loopback scheme is refused because the code
// would travel in clear.
func validateRedirectURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return domain.Validation(ErrCodeRegistrationInvalid,
			fmt.Sprintf("redirect_uri %q is not a valid URI", raw))
	}
	if !u.IsAbs() || u.Host == "" && u.Scheme != "http" {
		return domain.Validation(ErrCodeRegistrationInvalid,
			fmt.Sprintf("redirect_uri %q must be absolute", raw))
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return domain.Validation(ErrCodeRegistrationInvalid,
			fmt.Sprintf("redirect_uri %q must not carry a fragment", raw))
	}
	host := u.Hostname()
	isLoopback := host == "localhost" || host == "127.0.0.1" || host == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopback) {
		return domain.Validation(ErrCodeRegistrationInvalid,
			fmt.Sprintf("redirect_uri %q must use https, or http on loopback for a native client", raw))
	}
	return nil
}

// ---------------------------------------------------------------------------
// The authorization endpoint, and the consent screen's server side
// ---------------------------------------------------------------------------

// AuthorizeRequest is the parsed /authorize query.
type AuthorizeRequest struct {
	ResponseType        string
	ClientID            string
	RedirectURI         string
	Scope               string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
}

// ConsentContext is what the consent screen renders. Every field carrying
// client-supplied text is named so the template cannot forget what it is:
// registration is unauthenticated, so ClientNameUnverified is an
// attacker-controlled string and two clients may both claim to be "Claude
// Desktop" (m124 obligation 7). The screen must show it as self-asserted and
// must show RedirectHost, which is the part the user can actually judge.
type ConsentContext struct {
	ClientID             string
	ClientNameUnverified string
	ClientURIUnverified  string
	RedirectURI          string
	RedirectHost         string
	Scopes               []Scope
	State                string

	// CodeChallenge travels through consent so the code can be minted with it
	// on approval. It is public by construction -- already SHA-256 of the
	// verifier -- so carrying it is not a secret leak.
	CodeChallenge       string
	CodeChallengeMethod string
}

// Authorize validates an authorization request and returns what the consent
// screen must render. It mints NOTHING: no grant and no code exist until a
// human approves, which is what makes consent the authorization.
func (s *Service) Authorize(ctx context.Context, req AuthorizeRequest) (ConsentContext, error) {
	if req.ResponseType != "code" {
		return ConsentContext{}, domain.Validation(ErrCodeUnsupportedResponse,
			"response_type must be 'code'")
	}
	if strings.TrimSpace(req.ClientID) == "" {
		return ConsentContext{}, domain.Validation(ErrCodeInvalidRequest, "client_id is required")
	}

	// PKCE is mandatory and S256 only. An absent method must NOT fall back to
	// 'plain' -- m124 DECISION 5 keeps 'plain' out of the schema's closed set
	// for the same reason, and a missing challenge is an absence, not a
	// licence to skip the check.
	if strings.TrimSpace(req.CodeChallenge) == "" {
		return ConsentContext{}, domain.Validation(ErrCodeInvalidRequest,
			"code_challenge is required; this server requires PKCE")
	}
	if req.CodeChallengeMethod != "S256" {
		return ConsentContext{}, domain.Validation(ErrCodeInvalidRequest,
			"code_challenge_method must be 'S256'")
	}

	// THE EXIT GATE. A client requesting no recognised scope is refused here,
	// before any consent screen is drawn, and is never granted a default.
	scopes, err := ParseRequestedScopes(req.Scope)
	if err != nil {
		return ConsentContext{}, err
	}

	client, err := s.store.LookupClient(ctx, req.ClientID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ConsentContext{}, domain.Unauthorized(ErrCodeInvalidClient, "unknown client_id")
		}
		return ConsentContext{}, fmt.Errorf("lookup client: %w", err)
	}

	// EXACT MATCH, in Go, against the stored array. Never a prefix, suffix or
	// host-only comparison -- each of those is an open redirector, and the
	// query takes no redirect_uri parameter precisely so this cannot be pushed
	// into SQL and forgotten.
	if !exactMatchRedirectURI(client.RedirectUris, req.RedirectURI) {
		return ConsentContext{}, domain.Validation(ErrCodeInvalidRedirectURI,
			"redirect_uri does not exactly match a registered redirect URI")
	}

	redirectHost := ""
	if u, perr := url.Parse(req.RedirectURI); perr == nil {
		redirectHost = u.Host
	}

	return ConsentContext{
		ClientID:             client.ClientID,
		ClientNameUnverified: derefString(client.ClientName),
		ClientURIUnverified:  derefString(client.ClientUri),
		RedirectURI:          req.RedirectURI,
		RedirectHost:         redirectHost,
		Scopes:               scopes,
		State:                req.State,
		CodeChallenge:        req.CodeChallenge,
		CodeChallengeMethod:  req.CodeChallengeMethod,
	}, nil
}

// exactMatchRedirectURI compares byte-for-byte against each registered URI.
// Deliberately not url.Parse-and-compare-parts: normalisation is where
// redirector bugs live.
func exactMatchRedirectURI(registered []string, presented string) bool {
	if presented == "" {
		return false
	}
	for _, r := range registered {
		if r == presented {
			return true
		}
	}
	return false
}

// ApprovalRequest is a human's consent, submitted by the authenticated
// operator whose organisation the grant will belong to.
type ApprovalRequest struct {
	TenantID  uuid.UUID
	UserID    uuid.UUID
	Consent   ConsentContext
	GrantName string
	SiteScope SiteScopeRequest
}

// Approval is the result: the code to hand back to the client via the
// redirect, plus the grant it belongs to.
type Approval struct {
	GrantID     uuid.UUID
	Code        string
	RedirectURI string
	State       string
}

// Approve records the human's consent as a grant and mints the single-use PKCE
// code. Grant and code are created in one transaction.
func (s *Service) Approve(ctx context.Context, req ApprovalRequest) (Approval, error) {
	if req.TenantID == uuid.Nil {
		return Approval{}, domain.Validation(ErrCodeInvalidRequest, "an organisation is required")
	}
	if strings.TrimSpace(req.GrantName) == "" {
		return Approval{}, domain.Validation(ErrCodeInvalidRequest, "a connection name is required")
	}

	// Re-run the exit gate on the approval too. The consent screen's scope list
	// arrives back over the wire and a resubmitted form is caller input like
	// any other; re-parsing means a tampered approval cannot widen what the
	// authorize call already refused.
	if _, err := ParseRequestedScopes(scopesToString(req.Consent.Scopes)); err != nil {
		return Approval{}, err
	}
	if err := ValidateSiteScopeRequest(req.SiteScope); err != nil {
		return Approval{}, err
	}
	if req.Consent.CodeChallengeMethod != "S256" || strings.TrimSpace(req.Consent.CodeChallenge) == "" {
		return Approval{}, domain.Validation(ErrCodeInvalidRequest,
			"the approved request carries no S256 PKCE challenge")
	}

	// RE-RESOLVE THE CLIENT AND RE-MATCH THE REDIRECT, AGAINST THE STORED ARRAY.
	//
	// This is the minting path, and it is the one that has to hold. Everything
	// in req.Consent arrives in the approval POST body, so the checks Authorize
	// ran a request earlier are worth nothing here -- a body naming an
	// unregistered client_id and an attacker's redirect_uri would otherwise mint
	// a real code carrying the approving operator's tenant, and Exchange would
	// then honour it because it compares the presented redirect against the one
	// STORED ON THE CODE ROW. There is no foreign key to catch it either:
	// mcp_authorization_codes.client_id deliberately carries none (m124
	// DECISION 12), because the column records a historical fact.
	//
	// The invariant this restores is "this code may only travel to a URI this
	// client registered", not merely "a consent screen was shown once".
	client, err := s.store.LookupClient(ctx, req.Consent.ClientID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Approval{}, domain.Unauthorized(ErrCodeInvalidClient, "unknown client_id")
		}
		return Approval{}, fmt.Errorf("lookup client: %w", err)
	}
	// Exact match, against client.RedirectUris -- never against anything that
	// came in with the request. A prefix, suffix or host-only comparison here is
	// an open redirector, and so is trusting the body's own copy of the array.
	if !exactMatchRedirectURI(client.RedirectUris, req.Consent.RedirectURI) {
		return Approval{}, domain.Validation(ErrCodeInvalidRedirectURI,
			"redirect_uri does not exactly match a registered redirect URI")
	}

	code, err := randomToken(32)
	if err != nil {
		return Approval{}, fmt.Errorf("generate authorization code: %w", err)
	}
	codeHash := hashCredential(code)
	expiresAt := s.now().UTC().Add(authorizationCodeTTL)

	grant, _, err := s.store.CreateGrantWithCode(ctx,
		sqlc.CreateMCPGrantParams{
			TenantID:        req.TenantID,
			Name:            strings.TrimSpace(req.GrantName),
			Status:          string(GrantStatusActive),
			SiteScopeMode:   string(req.SiteScope.Mode),
			ScopeTagIds:     orEmpty(req.SiteScope.TagIDs),
			ScopeSiteIds:    orEmpty(req.SiteScope.SiteIDs),
			// The RESOLVED client id, not the body's copy of it.
			ClientID:        nullableText(client.ClientID),
			CreatedByUserID: uuidToPG(req.UserID),
		},
		func(grantID uuid.UUID) sqlc.CreateMCPAuthorizationCodeParams {
			return sqlc.CreateMCPAuthorizationCodeParams{
				TenantID: req.TenantID,
				GrantID:  grantID,
				ClientID: client.ClientID,
				CodeHash: codeHash,
				CodeChallenge:       req.Consent.CodeChallenge,
				CodeChallengeMethod: req.Consent.CodeChallengeMethod,
				RedirectUri:         req.Consent.RedirectURI,
				ExpiresAt:           expiresAt,
			}
		})
	if err != nil {
		return Approval{}, fmt.Errorf("create grant and code: %w", err)
	}

	return Approval{
		GrantID:     grant.ID,
		Code:        code, // returned once, to the redirect
		RedirectURI: req.Consent.RedirectURI,
		State:       req.Consent.State,
	}, nil
}

// ---------------------------------------------------------------------------
// The token endpoint
// ---------------------------------------------------------------------------

// TokenRequest is the RFC 6749 4.1.3 access token request.
type TokenRequest struct {
	GrantType    string
	Code         string
	RedirectURI  string
	ClientID     string
	ClientSecret string
	CodeVerifier string

	// ClientAuthVia is HOW the credential arrived, not whether it is valid:
	// "client_secret_basic" (Authorization: Basic), "client_secret_post" (body
	// parameters), "none" (no secret presented at all), or AuthViaMultiple when
	// the client used more than one at once.
	//
	// It exists because token_endpoint_auth_method is NOT NULL over a closed set
	// with no default -- the value is always a decision somebody made at
	// registration -- and a stored decision that nothing honours is a field that
	// looks like it governs behaviour and does not. The handler determines the
	// source; only the handler can see the transport.
	ClientAuthVia string
}

// AuthViaMultiple marks a request that presented credentials through more than
// one mechanism. RFC 6749 section 2.3.1: "The client MUST NOT use more than one
// authentication method in each request." Refusing is the only reading that
// does not require guessing which one the client meant.
const AuthViaMultiple = "multiple"

// IssuedToken is the token response. AccessToken is present exactly once.
type IssuedToken struct {
	AccessToken string
	TokenType   string
	ExpiresIn   int
	Scope       string
}

// Exchange redeems a PKCE authorization code for a connection token.
//
// THE ORDER HERE IS THE SECURITY PROPERTY. Everything that can refuse is
// checked first, then the code is consumed by an atomic compare-and-set in its
// own transaction, and ONLY IF that consume actually wrote is a token minted.
// A zero-row consume is a refusal, never a shrug.
func (s *Service) Exchange(ctx context.Context, req TokenRequest) (IssuedToken, error) {
	if req.GrantType != "authorization_code" {
		return IssuedToken{}, domain.Validation(ErrCodeInvalidRequest,
			"grant_type must be 'authorization_code'")
	}
	if strings.TrimSpace(req.Code) == "" {
		return IssuedToken{}, domain.Validation(ErrCodeInvalidRequest, "code is required")
	}
	if strings.TrimSpace(req.CodeVerifier) == "" {
		return IssuedToken{}, domain.Validation(ErrCodeInvalidRequest,
			"code_verifier is required; this server requires PKCE")
	}
	// RFC 6749 section 2.3.1: "The client MUST NOT use more than one
	// authentication method in each request." This is a property of the REQUEST
	// SHAPE, independent of whether the code is valid, so it is refused here
	// with the other shape checks rather than after a credential lookup. A
	// request carrying both a Basic header and body credentials gives no honest
	// answer to "which method did the client use", and picking one by precedence
	// would be a guess.
	if req.ClientAuthVia == AuthViaMultiple {
		return IssuedToken{}, domain.Unauthorized(ErrCodeInvalidClient,
			"present client credentials through exactly one mechanism, not several")
	}

	// 1. READ ONLY. This transaction must not write -- see repo.go.
	row, err := s.store.LookupAuthorizationCode(ctx, hashCredential(req.Code))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return IssuedToken{}, domain.Unauthorized(ErrCodeInvalidGrant,
				"the authorization code is not valid")
		}
		return IssuedToken{}, fmt.Errorf("lookup authorization code: %w", err)
	}

	// 2. Bind the code to the client and the redirect it was issued for
	// (RFC 6749 4.1.3). Both are exact comparisons.
	//
	// client_id IS REQUIRED, unconditionally. It used to be checked only when
	// present, which meant OMITTING it skipped the code-to-client binding
	// entirely -- absence read as permission, which is the shape this whole
	// surface is built to refuse. RFC 6749 4.1.3 requires it whenever the
	// client is not authenticating, and requiring it always is stricter than
	// the RFC rather than looser.
	if strings.TrimSpace(req.ClientID) == "" {
		return IssuedToken{}, domain.Unauthorized(ErrCodeInvalidGrant, "client_id is required")
	}
	if req.ClientID != row.ClientID {
		return IssuedToken{}, domain.Unauthorized(ErrCodeInvalidGrant,
			"the authorization code was not issued to this client")
	}

	// 2b. AUTHENTICATE THE CLIENT WHEN IT REGISTERED A SECRET.
	//
	// A client registered client_secret_basic or client_secret_post has a
	// secret minted, hashed and stored under a CHECK. Never asking for it makes
	// that whole apparatus decorative, and the registration default is
	// client_secret_basic -- so the common case was the unauthenticated one.
	// PKCE already limits the practical exploit; this is the defence in depth
	// the registration path already believes it has.
	client, err := s.store.LookupClient(ctx, req.ClientID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return IssuedToken{}, domain.Unauthorized(ErrCodeInvalidClient, "unknown client_id")
		}
		return IssuedToken{}, fmt.Errorf("lookup client: %w", err)
	}
	if client.TokenEndpointAuthMethod != "none" {
		// THE REGISTERED TRANSPORT IS ENFORCED, NOT MERELY RECORDED.
		//
		// token_endpoint_auth_method is NOT NULL over a closed set with no
		// default precisely so the value is always a decision. Accepting a
		// secret through a mechanism the client did not register would make the
		// column decorative -- the same shape as a column no statement can
		// write, which m126 deleted rather than leave lying around.
		//
		// RFC 6749 2.3.1 requires a server to support Basic and permits the body
		// form; it does not require accepting whichever one shows up. A client
		// that registered client_secret_basic and then posts its secret in a
		// body has either been misconfigured or is not the client it claims to
		// be, and both deserve a refusal rather than a token. The strict reading
		// costs a registered client nothing, because it chose the value.
		if req.ClientAuthVia != client.TokenEndpointAuthMethod {
			return IssuedToken{}, domain.Unauthorized(ErrCodeInvalidClient,
				fmt.Sprintf("this client registered %s; credentials presented via %s are refused",
					client.TokenEndpointAuthMethod, req.ClientAuthVia))
		}
		// The schema's secret_matches_method_check guarantees a non-'none'
		// client HAS a hash, so a nil here is an impossible row rather than a
		// public client. Refuse rather than treat it as "no secret required" --
		// that is the absence-means-permitted reflex m124 DECISION 11 names.
		if client.ClientSecretHash == nil {
			return IssuedToken{}, domain.Unauthorized(ErrCodeInvalidClient,
				"this client is misconfigured and cannot authenticate")
		}
		if strings.TrimSpace(req.ClientSecret) == "" {
			return IssuedToken{}, domain.Unauthorized(ErrCodeInvalidClient,
				"client_secret is required for this client")
		}
		if subtle.ConstantTimeCompare(
			[]byte(hashCredential(req.ClientSecret)),
			[]byte(*client.ClientSecretHash),
		) != 1 {
			return IssuedToken{}, domain.Unauthorized(ErrCodeInvalidClient,
				"client authentication failed")
		}
	} else if req.ClientAuthVia != "" && req.ClientAuthVia != "none" {
		// A PUBLIC CLIENT PRESENTING A SECRET IS REFUSED. 'none' means there is
		// no secret to compare against (m124 DECISION 11 makes the row shape
		// unrepresentable), so accepting a credential here would be accepting
		// one that nothing verifies -- absence of a stored secret read as
		// "anything matches", which is the exact reflex that decision exists to
		// prevent.
		return IssuedToken{}, domain.Unauthorized(ErrCodeInvalidClient,
			"this client registered token_endpoint_auth_method=none and must not present a secret")
	}
	if req.RedirectURI != row.RedirectUri {
		return IssuedToken{}, domain.Unauthorized(ErrCodeInvalidGrant,
			"redirect_uri does not match the one the code was issued for")
	}

	// 3. Replay and expiry, as the database computed them. Reported early so a
	// replay is refused with a clear reason; the authoritative refusal is still
	// the compare-and-set at step 5, because anything checked here is a TOCTOU
	// window.
	if row.IsConsumed {
		return IssuedToken{}, domain.Unauthorized(ErrCodeInvalidGrant,
			"the authorization code has already been redeemed")
	}
	if row.IsExpired || !row.IsRedeemable {
		return IssuedToken{}, domain.Unauthorized(ErrCodeInvalidGrant,
			"the authorization code is expired or not redeemable")
	}

	// 4. PKCE. S256 only, constant-time.
	if row.CodeChallengeMethod != "S256" {
		return IssuedToken{}, domain.Unauthorized(ErrCodeInvalidGrant,
			"the authorization code carries an unsupported challenge method")
	}
	if !verifyPKCE(req.CodeVerifier, row.CodeChallenge) {
		return IssuedToken{}, domain.Unauthorized(ErrCodeInvalidGrant,
			"code_verifier does not match the code_challenge")
	}

	// 5. REDEEM: consume the code and issue the token IN ONE TRANSACTION.
	//
	// The credential is generated before the call because the insert needs its
	// hash, but nothing has been persisted yet -- a failure here leaves no trace
	// and the code stays redeemable.
	secret, err := randomToken(32)
	if err != nil {
		return IssuedToken{}, fmt.Errorf("generate connection token: %w", err)
	}
	expiresAt := s.now().UTC().Add(connectionTokenTTL)

	// ATOMIC BY CONSTRUCTION. These used to be two commits, and the ORDER was
	// right -- burn the code, then mint -- because the reverse risks two tokens
	// from one code. But two commits meant a failure in between left a state
	// that is safe and useless: code permanently consumed, no token issued, and
	// a blameless client unable to retry and forced to restart the whole browser
	// flow with nothing to explain it. One transaction removes the window
	// instead of documenting it.
	//
	// pgx.ErrNoRows means the compare-and-set matched nothing: a racing exchange
	// won, it expired since the lookup, or RLS refused the write. All three mean
	// refuse. Treating "no row" as "already fine" is exactly how single-use
	// becomes multi-use, so there is no such branch.
	if _, err := s.store.RedeemAuthorizationCode(ctx, row.TenantID, row.ID,
		sqlc.CreateMCPConnectionTokenParams{
			TenantID:    row.TenantID,
			GrantID:     row.GrantID,
			TokenPrefix: secret[:tokenPrefixLen],
			TokenHash:   hashCredential(secret),
			Status:      string(GrantStatusActive),
			ExpiresAt:   pgtype.Timestamptz{Time: expiresAt, Valid: true},
		}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return IssuedToken{}, domain.Unauthorized(ErrCodeInvalidGrant,
				"the authorization code could not be redeemed; it was already used or has expired")
		}
		// The consume rolled back with the failure, so the code is still
		// redeemable and the client may retry this exact request.
		return IssuedToken{}, fmt.Errorf("redeem authorization code: %w", err)
	}

	return IssuedToken{
		AccessToken: secret, // once, here, never again
		TokenType:   "Bearer",
		ExpiresIn:   int(connectionTokenTTL.Seconds()),
		Scope:       string(ScopeRead),
	}, nil
}

// ---------------------------------------------------------------------------
// Per-request re-authorization (m124 obligation 4)
// ---------------------------------------------------------------------------

// AuthorizedRequest is a successfully re-checked MCP request.
type AuthorizedRequest struct {
	TenantID uuid.UUID
	GrantID  uuid.UUID
	TokenID  uuid.UUID
	Sites    SiteSet
}

// Authenticate resolves a bearer token and re-checks its grant against CURRENT
// state on EVERY request, so revocation lands on the next request rather than
// at token expiry.
//
// It is TWO queries and not a join: mcp_grants has no lookup policy, so
// `token JOIN grant` inside the token-lookup transaction matches zero rows and
// would fail every request with nothing in any log.
func (s *Service) Authenticate(ctx context.Context, bearer string) (AuthorizedRequest, error) {
	if strings.TrimSpace(bearer) == "" {
		return AuthorizedRequest{}, domain.Unauthorized(ErrCodeInvalidGrant, "a bearer token is required")
	}

	tok, err := s.store.LookupConnectionToken(ctx, hashCredential(bearer))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AuthorizedRequest{}, domain.Unauthorized(ErrCodeInvalidGrant, "the token is not valid")
		}
		return AuthorizedRequest{}, fmt.Errorf("lookup connection token: %w", err)
	}

	// Re-check under the now-known tenant. BRANCH ON Authorized, NOT ON ROW
	// PRESENCE: a revoked grant still returns a row, and reading "I got a row"
	// as "authorized" is how revocation silently stops working.
	chk, err := s.store.ReCheckAuthorization(ctx, tok.TenantID, tok.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AuthorizedRequest{}, domain.Unauthorized(ErrCodeInvalidGrant,
				"the grant behind this token no longer exists")
		}
		return AuthorizedRequest{}, fmt.Errorf("re-check mcp authorization: %w", err)
	}
	if !chk.Authorized {
		return AuthorizedRequest{}, domain.Unauthorized(ErrCodeInvalidGrant,
			"this connection has been revoked or has expired")
	}

	// Resolve the site scope at the one audited chokepoint, inside a tenant
	// transaction so `sites` RLS drops any foreign UUID.
	ids, err := s.store.ResolveScopeSites(ctx, tok.TenantID,
		chk.SiteScopeMode, chk.ScopeTagIds, chk.ScopeSiteIds)
	if err != nil {
		return AuthorizedRequest{}, fmt.Errorf("resolve grant scope: %w", err)
	}

	// An empty resolved set means NO SITES. NewSiteSet's zero value allows
	// nothing, so there is no widening path here even if ids is nil.
	return AuthorizedRequest{
		TenantID: tok.TenantID,
		GrantID:  chk.GrantID,
		TokenID:  chk.TokenID,
		Sites:    NewSiteSet(ids),
	}, nil
}

// ---------------------------------------------------------------------------
// Transport-facing service methods (S6b)
// ---------------------------------------------------------------------------

// RecordConnect persists what the client said about itself at initialize:
// name, version, the protocol header value OR ITS ABSENCE, and the time.
//
// protocolHeader is nil when the client sent no MCP-Protocol-Version header.
// It is passed straight through as NULL and is NEVER defaulted to a string:
// absence is a fact worth storing, and NULL here is what separates "connected
// and sent no header" from "has never connected".
//
// The error is RETURNED, not logged and dropped. The caller refuses the
// session on failure -- a connection the control plane could not attribute is
// worse than a refused one.
func (s *Service) RecordConnect(ctx context.Context, auth AuthorizedRequest, name, version string, protocolHeader *string) error {
	if err := s.store.RecordClientIdentity(ctx, auth.TenantID, auth.GrantID, name, version, protocolHeader); err != nil {
		return fmt.Errorf("record mcp connect: %w", err)
	}
	return nil
}

// ListSitesForModel is the one Phase 1 read tool. It returns the rendered tool
// text, already byte-capped and staleness-stamped.
//
// THE EMPTY-SCOPE BRANCH IS A REFUSAL, NOT AN EMPTY LIST. auth.Sites is the
// resolved set from the audited chokepoint, and an empty one means NO SITES --
// never every site. Returning `{"sites": []}` here would be indistinguishable
// from a healthy organisation that owns nothing, which is exactly how a
// scoping bug stays invisible.
func (s *Service) ListSitesForModel(ctx context.Context, auth AuthorizedRequest) (string, error) {
	if auth.Sites.IsEmpty() {
		return "", domain.Forbidden(ErrCodeScopeEmpty,
			"this connection's site scope resolves to no sites, so there is nothing it may read. "+
				"This is a refusal, not an empty fleet: check the grant's site scope.")
	}

	rows, more, err := s.store.ListSitesForRead(ctx, auth.TenantID, sitesPageBound)
	if err != nil {
		return "", fmt.Errorf("list sites for mcp read: %w", err)
	}

	// Filter to the grant's resolved set. The query is tenant-scoped by RLS;
	// this narrows it further to the sites THIS GRANT may read. SiteSet.Allows
	// returns false for every id on an empty or zero-value set, so there is no
	// widening path even if the set were somehow lost between here and there.
	allowed := make([]sqlc.ListSitesRow, 0, len(rows))
	for _, r := range rows {
		if auth.Sites.Allows(r.ID) {
			allowed = append(allowed, r)
		}
	}

	return buildListSitesResult(allowed, more, s.now())
}

// ---------------------------------------------------------------------------
// Credential helpers
// ---------------------------------------------------------------------------

// hashCredential is lower-case hex SHA-256, matching the '^[0-9a-f]{64}$'
// CHECK on every hash column and the construction used by internal/apikey and
// internal/agent/signature.go.
func hashCredential(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// randomToken returns nBytes of crypto/rand as unpadded base64url. It returns
// an error rather than falling back to anything: a credential built from a
// failed entropy read is the defect class this project is named for.
func randomToken(nBytes int) (string, error) {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// verifyPKCE checks BASE64URL(SHA256(verifier)) == challenge, in constant time.
func verifyPKCE(verifier, challenge string) bool {
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(want), []byte(challenge)) == 1
}

func scopesToString(scopes []Scope) string {
	parts := make([]string, 0, len(scopes))
	for _, s := range scopes {
		parts = append(parts, string(s))
	}
	return strings.Join(parts, " ")
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// orEmpty normalises a nil slice to an empty one so the uuid[] column receives
// '{}' rather than NULL. Empty names nothing and therefore grants nothing,
// which is the restrictive direction; the schema's payload CHECK stops it
// co-existing with a mode that would make it meaningful.
func orEmpty(ids []uuid.UUID) []uuid.UUID {
	if ids == nil {
		return []uuid.UUID{}
	}
	return ids
}
