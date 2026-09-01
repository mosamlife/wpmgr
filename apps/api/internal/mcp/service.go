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
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Lifetimes. The code is short-lived because it is a bearer credential in a
// URL; the token's is the connection's own lifetime.
const (
	authorizationCodeTTL = 5 * time.Minute
	connectionTokenTTL   = 90 * 24 * time.Hour
	tokenPrefixLen       = 12

	// grantAbsoluteTTL is the absolute lifetime stamped onto a new grant's
	// mcp_grants.expires_at.
	//
	// IT IS A GO CONSTANT AND NOT A COLUMN DEFAULT, AND THE DIFFERENCE IS THE
	// WHOLE OF m127 DECISION 2. The column is NOT NULL with no default, so this
	// call site cannot omit the term and get one silently -- it gets 23502. The
	// value is chosen here, once, where it is reviewable, rather than in the
	// schema where it would apply to every future writer including one that
	// meant to ask the operator.
	//
	// 90 days matches the consent wireframe's pre-selected option and the value
	// m127 backfilled existing rows with. When Step 5's control ships, the
	// operator's choice arrives on ApprovalRequest and REPLACES this default at
	// this line; it does not become a second default anywhere else.
	grantAbsoluteTTL = 90 * 24 * time.Hour
)

// grantLifetimeDays is grantAbsoluteTTL in whole days, and it is the ONLY
// lifetime figure the consent screen is given.
//
// A TERM AND NOT A DATE, because at consent time no grant exists. expires_at is
// stamped at approval from s.now(), which is later than the moment this
// response was built by however long the human spends reading the screen, so a
// timestamp computed here would be a date the row never holds. The term is
// exact at both moments: approve it and it expires this many days later.
//
// DERIVED, NEVER RETYPED. The screen's sentence and the column's value now have
// one source, so a change to grantAbsoluteTTL cannot leave the consent copy
// stating the old term.
func grantLifetimeDays() int {
	return int(grantAbsoluteTTL / (24 * time.Hour))
}

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

	// ErrCodeConnectionNotFound is the answer for a grant id this principal
	// cannot see. It carries NO information about whether the id exists
	// elsewhere: an id from another organisation and an id that never existed
	// produce the identical response, which is the same non-oracle stance
	// authz.RequireSiteAccess takes when it 404s instead of 403ing.
	ErrCodeConnectionNotFound = "mcp_connection_not_found"

	// ErrCodeOrgScopeRequired refuses a site-constrained principal at the
	// connections surface. It mirrors the authz middleware's own
	// "org_scope_required" and exists as a SECOND, independent refusal -- see
	// requireOrgScopedPrincipal for why one is not enough.
	//
	// Distinct from ErrCodeAccessDenied below, and the split is deliberate: that
	// one is the OAuth-envelope refusal for the CONSENT routes, which a browser
	// screen parses as RFC 6749 {error, error_description}. This one travels in
	// the house {code, message} envelope because the connections routes are read
	// by the dashboard's own data layer. Same predicate
	// (domain.Principal.IsSiteConstrained), two envelopes, because two different
	// consumers parse them.
	ErrCodeOrgScopeRequired = "mcp_org_scope_required"

	// ErrCodeAccessDenied is the refusal for a principal that is authenticated
	// and carries a tenant but may not authorize an ORG-LEVEL object. It maps
	// to RFC 6749 section 4.1.2.1 "access_denied", which is the spec's own name
	// for "the resource owner refused", and 403 rather than 401: re-presenting
	// the same credential will not help.
	ErrCodeAccessDenied = "mcp_access_denied"
)

// The OAuth vocabulary this server implements, named once.
//
// THE VALIDATORS BELOW COMPARE AGAINST THESE, AND discovery.go ADVERTISES
// THESE. That is the entire point: a discovery document is a promise, and the
// way a server comes to advertise a grant type or a PKCE method it refuses is
// by keeping two lists that agree on the day they are written. There is one
// list, and TestDiscoveryVocabularyMatchesTheValidators drives the validators
// with every plausible neighbouring value to prove the list is the set the code
// actually accepts.
const (
	// ResponseTypeCode is the only response_type Service.Authorize accepts.
	ResponseTypeCode = "code"
	// GrantTypeAuthorizationCode is the only grant_type Service.Exchange
	// accepts. There is no refresh_token grant: the connection token's lifetime
	// is the connection's, and nothing here mints a refresh token.
	GrantTypeAuthorizationCode = "authorization_code"
	// CodeChallengeMethodS256 is the only PKCE method accepted, at /authorize
	// and again at /token. "plain" is deliberately absent here, absent from the
	// schema's closed set, and must stay absent from the discovery document.
	CodeChallengeMethodS256 = "S256"
	// TokenEndpointAuthMethodNone is the no-secret PKCE-only client type. It
	// must be asked for explicitly; RFC 7591's default is the restrictive one.
	TokenEndpointAuthMethodNone = "none"
)

// supportedTokenEndpointAuthMethods is the closed set Register accepts, in the
// order the discovery document advertises them.
var supportedTokenEndpointAuthMethods = []string{
	TokenEndpointAuthMethodNone,
	"client_secret_basic",
	"client_secret_post",
}

// SupportedResponseTypes reports the response_type values Service.Authorize
// accepts.
func SupportedResponseTypes() []string { return []string{ResponseTypeCode} }

// SupportedGrantTypes reports the grant_type values Service.Exchange accepts.
func SupportedGrantTypes() []string { return []string{GrantTypeAuthorizationCode} }

// SupportedCodeChallengeMethods reports the PKCE methods this server accepts.
//
// RFC 8414 section 2 makes this field how a client learns PKCE is available at
// all, and the MCP specification has clients REFUSE to proceed when it is
// absent. It must therefore be present and it must be exactly true.
func SupportedCodeChallengeMethods() []string { return []string{CodeChallengeMethodS256} }

// SupportedTokenEndpointAuthMethods reports the client authentication methods
// Service.Register accepts.
func SupportedTokenEndpointAuthMethods() []string {
	return append([]string(nil), supportedTokenEndpointAuthMethods...)
}

// validateCodeChallengeMethod is the one place a PKCE method is judged at
// authorization time. Extracted from Authorize so the discovery test can drive
// it directly with every neighbouring value ("plain", "s256", "") and assert
// the advertised list is exactly the set it accepts.
func validateCodeChallengeMethod(method string) error {
	if method != CodeChallengeMethodS256 {
		return domain.Validation(ErrCodeInvalidRequest,
			"code_challenge_method must be '"+CodeChallengeMethodS256+"'")
	}
	return nil
}

// validateResponseType and validateGrantType exist for the same reason.
func validateResponseType(responseType string) error {
	if responseType != ResponseTypeCode {
		return domain.Validation(ErrCodeUnsupportedResponse,
			"response_type must be '"+ResponseTypeCode+"'")
	}
	return nil
}

func validateGrantType(grantType string) error {
	if grantType != GrantTypeAuthorizationCode {
		return domain.Validation(ErrCodeInvalidRequest,
			"grant_type must be '"+GrantTypeAuthorizationCode+"'")
	}
	return nil
}

// validateTokenEndpointAuthMethod judges a registration's requested client
// authentication method against the closed set above.
func validateTokenEndpointAuthMethod(method string) error {
	if !slices.Contains(supportedTokenEndpointAuthMethods, method) {
		return domain.Validation(ErrCodeRegistrationInvalid,
			fmt.Sprintf("token_endpoint_auth_method %q is not supported", method))
	}
	return nil
}

// Clock is injectable so expiry behaviour is testable without sleeping.
type Clock func() time.Time

// auditRecorder is the slice of *audit.Recorder this package actually uses. It
// exists to make the append-FAILED branch testable without a broken database;
// production passes the concrete recorder through WithAudit and nothing else
// implements it outside tests.
type auditRecorder interface {
	Record(ctx context.Context, e audit.Event) (audit.Entry, error)
	RecordInTx(ctx context.Context, tx pgx.Tx, e audit.Event) (audit.Entry, error)
}

// Service carries the OAuth surface. It holds no plaintext credential beyond
// the response it is building: m124 obligation 6 says the plaintext is returned
// once at creation and never read back, and there is no cache here.
type Service struct {
	store Store
	now   Clock
	// audit is nil unless WithAudit is called. Every audit write in this
	// package guards on it being nil first -- see Approve, RevokeConnection,
	// and RecordToolCall -- so a Service built by the many unit tests in this
	// package (fakeStore, no real Postgres pool to back an audit.Recorder)
	// keeps working unaudited. Production wiring (cmd/wpmgr/main.go) always
	// calls WithAudit; a Service that reaches a request path without it is a
	// wiring bug, not a supported configuration, and the finding to raise if
	// one is ever found is "this deploy path is unattributable", not "make the
	// nil check quieter".
	//
	// The type is the two-method auditRecorder rather than *audit.Recorder so
	// that the FAILURE path is reachable from a unit test. What happens when
	// an append fails is a decision this package makes deliberately (see
	// TransportHandler.auditGap), and a decision whose only implementation is
	// unreachable without a broken Postgres is a decision nothing checks.
	audit auditRecorder

	// mintLimit bounds POST /api/v1/mcp/connections. It lives on the SERVICE
	// and not on the Handler, unlike Handler.regLimit, and the placement is
	// deliberate: /register is unauthenticated so its limiter can only key on
	// the transport (RemoteIP), which only the handler can see, whereas this
	// one keys on the authenticated operator and is therefore expressible at
	// the service layer -- where every mount of MintConnection is covered,
	// including a test harness that forgot the middleware.
	//
	// Never nil after NewService. registrationLimiter.allow refuses on a nil
	// receiver anyway, so a Service built by a struct literal that skipped it
	// mints NOTHING rather than minting unlimited.
	mintLimit *registrationLimiter
}

func NewService(store Store) *Service {
	return &Service{
		store: store,
		now:   time.Now,
		// Armed HERE rather than injected with a nil default, for the reason
		// NewHandler gives about its own limiter: an unarmed limiter would be a
		// wiring failure that presents as a working endpoint.
		mintLimit: newMintLimiter(),
	}
}

// WithClock returns a copy using the supplied clock. Test-only in practice.
func (s *Service) WithClock(c Clock) *Service {
	cp := *s
	cp.now = c
	return &cp
}

// WithAudit returns a copy that appends ActionMCPGrantCreated,
// ActionMCPGrantRevoked and ActionMCPToolCalled through rec. Mirrors
// WithClock's copy-and-return shape. cmd/wpmgr/main.go calls this on the one
// Service the process constructs; a Service left without it (as most of this
// package's unit tests are, deliberately, since they drive a fakeStore with no
// backing Postgres pool) simply does not audit -- see the field doc on audit.
// It keeps the CONCRETE parameter type on purpose even though the field is an
// interface: a nil *audit.Recorder stored straight into an interface is a
// non-nil interface holding a nil pointer, which would sail past every
// `s.audit == nil` guard in this file and panic on the first append instead of
// skipping it. The explicit nil check below is what keeps "unaudited" meaning
// unaudited.
func (s *Service) WithAudit(rec *audit.Recorder) *Service {
	cp := *s
	if rec == nil {
		cp.audit = nil
		return &cp
	}
	cp.audit = rec
	return &cp
}

// withAuditRecorder is the test-only door onto the same field, so a fake can
// drive the append-failed path. Unexported: production wiring goes through
// WithAudit.
func (s *Service) withAuditRecorder(rec auditRecorder) *Service {
	cp := *s
	cp.audit = rec
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
	if err := validateTokenEndpointAuthMethod(method); err != nil {
		return RegisteredClient{}, err
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
	if err := validateResponseType(req.ResponseType); err != nil {
		return ConsentContext{}, err
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
	if err := validateCodeChallengeMethod(req.CodeChallengeMethod); err != nil {
		return ConsentContext{}, err
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
//
// Principal is the WHOLE principal and not the (TenantID, UserID) pair it
// replaced. The pair could be filled in correctly while Scope and
// AllowedSiteIDs were silently dropped, and dropping them is precisely what
// makes the site-scope RLS inert on the write below -- a caller that populates
// two of four fields would still compile and would still escalate. One field
// cannot be half-populated.
type ApprovalRequest struct {
	Principal domain.Principal
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
	if req.Principal.TenantID == uuid.Nil {
		return Approval{}, domain.Validation(ErrCodeInvalidRequest, "an organisation is required")
	}

	// A grant is org-level: site_scope_mode 'all' resolves to every site in the
	// organisation. authz.RequireOrgScope on the route is the primary refusal
	// and this is the service-layer restatement of it, so a future caller that
	// reaches Approve without passing through that middleware is refused too.
	// The repo's RunTenantTx is the third layer, in the database.
	if req.Principal.IsSiteConstrained() {
		return Approval{}, domain.Forbidden(ErrCodeAccessDenied,
			"site-scoped access cannot authorize an organisation-level connection")
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
	if req.Consent.CodeChallengeMethod != CodeChallengeMethodS256 || strings.TrimSpace(req.Consent.CodeChallenge) == "" {
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

	// req.Principal, not a tenant id: it is what routes the write to the correct
	// tx helper, and therefore what decides whether the site-scope RLS is live.
	grant, _, err := s.store.CreateGrantWithCode(ctx, req.Principal,
		sqlc.CreateMCPGrantParams{
			TenantID:      req.Principal.TenantID,
			Name:          strings.TrimSpace(req.GrantName),
			Status:        string(GrantStatusActive),
			SiteScopeMode: string(req.SiteScope.Mode),
			ScopeTagIds:   orEmpty(req.SiteScope.TagIDs),
			ScopeSiteIds:  orEmpty(req.SiteScope.SiteIDs),
			// The RESOLVED client id, not the body's copy of it.
			ClientID:        nullableText(client.ClientID),
			CreatedByUserID: uuidToPG(req.Principal.UserID),

			// m127's three columns, ALL SUPPLIED EXPLICITLY. Two of them are
			// NOT NULL with no default precisely so that this literal cannot
			// forget one: a missing field is the zero value, which is
			// nil/invalid, which is NULL, which is 23502 at the INSERT rather
			// than an unrestricted or never-expiring connection.
			//
			// Capabilities is the STORED per-connection capability set, and
			// from here on it is the authority Authenticate reads -- not a
			// value recomputed from the scope registry at request time. It is
			// written as the org default because Phase 1's consent screen
			// offers no narrowing control; the moment it does, the operator's
			// choice arrives here and NarrowTo is what applies it.
			Capabilities: capabilityNames(DefaultGrantCapabilities()),
			ExpiresAt:    s.now().UTC().Add(grantAbsoluteTTL),

			// IDLE EXPIRY IS WRITTEN NULL, AND NULL IS THE ANSWER, NOT A
			// PLACEHOLDER. NULL means "never idle-expire" (m127 DECISION 4).
			//
			// A non-NULL value here is only safe once mcp_grants.last_used_at
			// is actually written, because the deadline is
			// coalesce(last_used_at, created_at) + N days: with the stamp
			// unwired that collapses to created_at + N and every connection
			// dies N days after creation however heavily it is used. The stamp
			// IS wired now (RecordActivity, called from the transport's
			// tools/list and tools/call arms), so a non-NULL window has become
			// REPRESENTABLE -- but nothing yet asks the operator for one, and
			// inventing a window nobody chose is exactly the "credential nobody
			// chose the terms of" this column refuses to default.
			//
			// So: NULL until Step 5's second control ships and supplies a
			// chosen value. The prerequisite is now met; the input is not.
			IdleExpireAfterDays: nil,

			// m128. NULL, AND NULL IS THE ANSWER RATHER THAN 'generic'.
			//
			// THE CONSENT PATH NEVER ASKS. An OAuth grant is authorised by a
			// consent screen that shows the client's own unverified claim about
			// itself; it never puts the nine-card chooser in front of the
			// operator, so no operator choice exists to record. NULL is the
			// column's word for "nobody asked" and it is the truth here.
			//
			// 'generic' WOULD BE A LIE, not a harmless default. It asserts the
			// operator saw nine cards and picked "Other MCP client", and S31's
			// filter and S29 step 9 both read that as a stated choice: every
			// OAuth connection would then answer a filter chip no operator ever
			// selected. m128 DECISION 2(b) refuses the schema default for this
			// reason, and a Go-side default would reintroduce it one layer up.
			//
			// Do NOT derive it from client.ClientName either. That is the
			// client's self-report, m128 DECISION 2(c), and inferring a choice
			// from it manufactures a fact the operator never stated -- and
			// makes an inferred row indistinguishable from a chosen one at
			// exactly the screen that exists to tell them apart.
			SetupClient: nil,
		},
		func(grantID uuid.UUID) sqlc.CreateMCPAuthorizationCodeParams {
			return sqlc.CreateMCPAuthorizationCodeParams{
				TenantID:            req.Principal.TenantID,
				GrantID:             grantID,
				ClientID:            client.ClientID,
				CodeHash:            codeHash,
				CodeChallenge:       req.Consent.CodeChallenge,
				CodeChallengeMethod: req.Consent.CodeChallengeMethod,
				RedirectUri:         req.Consent.RedirectURI,
				ExpiresAt:           expiresAt,
			}
		},
		// ActionMCPGrantCreated, in the SAME transaction as both inserts above.
		// This is "the row written to the audit log" the operator-facing
		// consent flow promises: if the grant or the code fails to insert, or
		// this append fails, the whole approval rolls back and there is no
		// mcp.grant.created row for a grant that does not exist.
		func(tx pgx.Tx, gr sqlc.McpGrant) error {
			if s.audit == nil {
				return nil
			}
			// THE ACTOR IS WHICHEVER CREDENTIAL AUTHENTICATED, resolved by
			// audit.ActorFor rather than hardcoded.
			//
			// "CONSENT IS BROWSER-ONLY" IS A CONVENTION, NOT A CONSTRAINT, and
			// an audit row may not rest on one. /consent is mounted on v1
			// (server.go's Register(v1), where v1 derives from
			// sessionAuthGroup and therefore carries Auth.Authenticate()) --
			// the same Bearer-accepting group as the mint and the revoke. An
			// API-key holder can drive the OAuth flow headlessly, and then
			// ActorUser over req.Principal.UserID recorded uuid.Nil against a
			// grant that really was created.
			//
			// CreatedByUserID above stays as it is and stays NULL for a key:
			// that column means "the human who created it", and "none" is the
			// honest answer where this pair is the complete one.
			actorType, actorID := audit.ActorFor(req.Principal)
			_, aerr := s.audit.RecordInTx(ctx, tx, audit.Event{
				TenantID:   req.Principal.TenantID,
				ActorType:  actorType,
				ActorID:    actorID,
				Action:     audit.ActionMCPGrantCreated,
				TargetType: "mcp_grant",
				TargetID:   gr.ID.String(),
				Metadata: map[string]any{
					"grant_name":      gr.Name,
					"site_scope_mode": gr.SiteScopeMode,
				},
			})
			return aerr
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
	if err := validateGrantType(req.GrantType); err != nil {
		return IssuedToken{}, err
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
	if row.CodeChallengeMethod != CodeChallengeMethodS256 {
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
	// GrantName is the grant's operator-assigned name (mcp_grants.name), read
	// at the same ReCheckAuthorization call that resolves GrantID -- no extra
	// query. It exists so RecordToolCall can stamp a human-readable label onto
	// an ActionMCPToolCalled event without a database join: ActorName
	// resolution (ListAuditEntries) has no arm for ActorAssistant today, so
	// this is how the label travels until it does. Never used for
	// authorization, only for display.
	GrantName string

	// Sites is the resolved site scope: the PER-CONNECTION narrowing on the
	// site axis. Zero value allows nothing.
	Sites SiteSet

	// Capabilities is the resolved capability set: which tools this connection
	// may reach. Zero value allows nothing, so an AuthorizedRequest built by a
	// literal in a test -- or by any future code path that forgets to set it --
	// reaches NO tool rather than every tool. That is why it is a CapabilitySet
	// and not a []string.
	Capabilities CapabilitySet

	// OrgCeiling is the ORGANISATION's widest capability set -- what
	// OrgDefaultCapabilities resolved for this grant's scopes, BEFORE the
	// per-connection narrowing that produced Capabilities. Capabilities is
	// always a subset of it.
	//
	// IT IS CARRIED, NOT RECOMPUTED. Authenticate already resolves the ceiling
	// to intersect it with the stored set, and a second OrgDefaultCapabilities
	// call inside the registry would be a second source of truth that can
	// disagree with this one -- and would have to discard the error that
	// function returns for exactly the misconfiguration it exists to catch.
	//
	// IT EXISTS BECAUSE THE TWO BOUNDARIES DISCLOSE DIFFERENTLY. A tool inside
	// the ceiling that this grant does not hold is LISTED and refuses by name,
	// so that unticking a permission produces an explicable refusal rather than
	// a tool that silently vanishes. A tool outside the ceiling is omitted from
	// tools/list and refuses as unregistered, so a token holder cannot
	// enumerate the capabilities their organisation deliberately switched off.
	// Without this field the registry can only see Capabilities and cannot tell
	// the two apart.
	//
	// Zero value allows nothing, and that is the same fail-closed choice
	// Capabilities makes for the same reason: a literal that forgets it lists
	// NO tool rather than every tool.
	OrgCeiling CapabilitySet
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
	//
	// THIS CALL SITE IS DELIBERATELY TENANT-SCOPED AND NOT SITE-SCOPED, AND IT
	// IS THE ONE EXCEPTION TO ADR-061 A11 ITEM 2. Read bootstrapTenantPrincipal
	// before changing it. In one line: this call is what PRODUCES the allowlist,
	// so there is no allowlist to scope it by.
	ids, err := s.store.ResolveScopeSites(ctx, bootstrapTenantPrincipal(tok.TenantID),
		chk.SiteScopeMode, chk.ScopeTagIds, chk.ScopeSiteIds)
	if err != nil {
		return AuthorizedRequest{}, fmt.Errorf("resolve grant scope: %w", err)
	}

	// Resolve the CAPABILITY set (S7). Two axes narrow an MCP connection and
	// they are independent: the site axis above says WHICH SITES, this one says
	// WHICH TOOLS.
	//
	// THE STORED COLUMN IS THE AUTHORITY, AND THAT IS WHAT m127 CHANGED.
	// mcp_grants.capabilities now exists -- NOT NULL, no default, CHECKed
	// against the same closed vocabulary this package holds -- and it is read
	// off chk.GrantCapabilities, THE SAME ROW AND THE SAME TRANSACTION as the
	// `authorized` verdict this request was admitted under.
	//
	// Recomputing the set from the scope registry instead, as this line did
	// before m127, is a GUESS: it agrees with the row today only because the
	// vocabulary holds one name, and it diverges the instant a grant is created
	// holding anything else. It also answers "full capabilities" for a grant
	// whose stored set is EMPTY, which is the case where the two disagree by
	// everything.
	//
	// THE ORG DEFAULT REMAINS THE CEILING; the stored value NARROWS it and
	// never replaces it. That ordering is the only thing standing between a row
	// and a capability the organisation's scopes do not confer, so a capability
	// in the column that the default does not hold is REFUSED here -- not
	// honoured, and not quietly dropped. Dropping would be fail-closed and
	// still wrong, for the reason written on NarrowTo.
	//
	// ONE GAP SURVIVES m127, AND IT IS A LATENT WIDENING RATHER THAN A MISSING
	// NARROWING: the scope list below is a CONSTANT, not the grant's own
	// scopes. mcp_grants still has no scopes column, so there is nothing
	// per-grant to read; grantScopes() returns the one scope every live grant
	// holds by construction. That is exact TODAY and only because
	// recognisedScopes holds exactly one entry. The day a second scope joins
	// it, a connection granted ONLY that scope would still be handed
	// ScopeRead's capabilities as its CEILING here -- a widening, and one no
	// existing test catches, because
	// TestEveryRecognisedScopeHasACapabilityMapping pins the map's totality and
	// not this function's input.
	//
	// TestGrantScopesIsExactOnlyWhileOneScopeExists goes red the moment
	// recognisedScopes grows, so whoever adds the second scope is forced to
	// come here and read a grant's scopes instead of a constant.
	ceiling, err := OrgDefaultCapabilities(grantScopes())
	if err != nil {
		// A capability set that cannot be resolved is a REFUSAL, never an
		// empty-but-proceeding one. An AuthorizedRequest with a zero-value
		// CapabilitySet reaches no tool, so proceeding would produce a
		// connection that authenticates and then refuses everything with no
		// explanation anywhere.
		return AuthorizedRequest{}, fmt.Errorf("resolve grant capabilities: %w", err)
	}

	// AN EMPTY STORED SET IS REFUSED BY NAME, not carried forward as an empty
	// CapabilitySet. The column's shape CHECK admits '{}' -- the restrictive
	// value passes the vocabulary containment test -- so the state is
	// representable in the database even though no write path in this package
	// mints it. Carrying it forward would authenticate a connection that then
	// answers every tools/call with "not available" and every tools/list with
	// nothing, which is precisely the half-working connection m127 DECISION 3
	// says this boundary must not be able to produce.
	//
	// IT IS 403, NOT 401, and the distinction is the client's control flow
	// rather than a matter of taste. Authenticate's refusals reach
	// TransportHandler.writeUnauthorized, and an MCP client that receives 401
	// re-runs the OAuth handshake -- which cannot change a stored capability
	// set, so the client loops and the operator is sent to rotate a credential
	// that was never the problem. An empty capabilities column is a
	// CONFIGURATION state: the credential is valid and the grant is not
	// permitted. That is what 403 says, and it is what every other producer of
	// ErrCodeCapabilityUnmapped already returns (all three in
	// OrgDefaultCapabilities).
	stored := capabilitiesFromColumn(chk.GrantCapabilities)
	if len(stored) == 0 {
		return AuthorizedRequest{}, domain.Forbidden(ErrCodeCapabilityUnmapped,
			"this connection holds no capability, so it can reach no tool")
	}

	caps, err := ceiling.NarrowTo(stored)
	if err != nil {
		// Same refusal, same reason: a stored set that cannot be reconciled
		// with the organisation's ceiling is not silently reduced to the
		// intersection. See NarrowTo.
		return AuthorizedRequest{}, fmt.Errorf("resolve grant capabilities: %w", err)
	}

	// An empty resolved set means NO SITES. NewSiteSet's zero value allows
	// nothing, so there is no widening path here even if ids is nil.
	return AuthorizedRequest{
		TenantID:     tok.TenantID,
		GrantID:      chk.GrantID,
		GrantName:    chk.GrantName,
		TokenID:      chk.TokenID,
		Sites:        NewSiteSet(ids),
		Capabilities: caps,
		// The ceiling resolved above, carried rather than recomputed. caps is
		// ceiling.NarrowTo(stored), so this is always a superset of
		// Capabilities and the registry can tell "your grant lacks it" from
		// "your organisation switched it off". See AuthorizedRequest.OrgCeiling.
		OrgCeiling: ceiling,
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
//
// ============================================================================
// IT MUST NEVER WRITE mcp_grants.setup_client. THIS IS LOAD-BEARING (m128).
// ============================================================================
//
// The tidy-looking change is to notice that this function already writes the
// client's name and to "keep setup_client in sync" from it. That destroys the
// column. setup_client is THE OPERATOR'S CHOICE at wizard step 2; name and
// version are THE CLIENT'S SELF-REPORT at initialize. They are different facts
// about different actors and they DISAGREE LEGITIMATELY AND PERMANENTLY -- set
// up for Claude Desktop, URL pasted into Cursor. Neither is the other's stale
// copy, so neither may overwrite the other.
//
// The damage is not hypothetical and it is silent. S29 step 9's failure state
// reads setup_client precisely when NOTHING HAS EVER CONNECTED, which is by
// definition the state in which this function has never run and both reported
// columns are NULL; and S31 filters on "was set up for", not "last connected
// as". A sync here would overwrite the operator's stated choice with an
// observation on first connect, and every screen would keep rendering, wrong.
//
// The write path for that column is CreateGrant/MintConnection, once, at
// creation. There is no second one. TestRecordConnectLeavesSetupClientAlone
// and the integration proof of the same name are what make removing this
// paragraph go red rather than green.
func (s *Service) RecordConnect(ctx context.Context, auth AuthorizedRequest, name, version string, protocolHeader *string) error {
	if err := s.store.RecordClientIdentity(ctx, auth.TenantID, auth.GrantID, name, version, protocolHeader); err != nil {
		return fmt.Errorf("record mcp connect: %w", err)
	}
	return nil
}

// RecordActivity stamps mcp_grants.last_used_at (and the token's) for one
// request that used this connection. GH #605.
//
// ============================================================================
// WHAT COUNTS AS "USED", DECIDED EXPLICITLY: EVERY TOOL CALL, NOT CONNECT.
// ============================================================================
//
// The caller is TransportHandler.dispatch, from the tools/list and tools/call
// arms only. initialize, ping and notifications/initialized do NOT stamp.
//
// STAMPING ONCE PER SESSION AT CONNECT WAS THE ALTERNATIVE AND IT IS REJECTED,
// for two reasons and not one:
//
//  1. IT WOULD MAKE THE COLUMN MEAN "last connected", which is a weaker claim
//     than its name and than the connections list's "Last used" label. Renaming
//     either to match would then be part of the change.
//
//  2. MORE IMPORTANTLY IT WOULD NOT BE SAFE TO IDLE-EXPIRE AGAINST. Streamable
//     HTTP sessions are long-lived and initialize happens once at the start of
//     one. A client that initialized in January and has called tools every day
//     since would carry a January stamp, so a 30-day idle window would kill a
//     demonstrably active connection in February. That is a smaller, quieter
//     version of exactly the fleet outage m127 DECISION 4 describes, and the
//     whole point of wiring this stamp is to make that outage impossible rather
//     than rare.
//
// ping and notifications/initialized are excluded for the opposite reason: they
// are transport keepalives that a stuck background process emits forever
// without anyone using anything. Letting them refresh the deadline would make
// "unused for 30 days" unfalsifiable and the feature decorative. Connecting is
// still recorded -- RecordConnect stamps client_identity_recorded_at, which is
// the separate fact "this client has connected at least once".
//
// THE ERROR IS RETURNED AND THE CALLER REFUSES THE REQUEST, deliberately unlike
// RecordToolCall's best-effort audit append one function below. last_used_at is
// an INPUT TO AN AUTHORIZATION DEADLINE, not an observation of one: a stamp
// that silently fails leaves the connection's idle clock running while it is
// being used, and the cost lands N days later as an expiry nobody can explain.
// Refusing is loud, immediate, and local to the request that could not be
// recorded -- and it costs nothing in practice, because a database that cannot
// take this write also could not have served the two reads Authenticate just
// made.
func (s *Service) RecordActivity(ctx context.Context, auth AuthorizedRequest) error {
	if _, err := s.store.TouchActivity(ctx, auth.TenantID, auth.GrantID, auth.TokenID); err != nil {
		return fmt.Errorf("record mcp activity: %w", err)
	}
	return nil
}

// RecordToolCall appends the ActionMCPToolCalled audit row for one
// successfully executed tool call. The caller (TransportHandler.callTool)
// invokes this AFTER entry.invoke returns without error. A REFUSED call is
// recorded separately, by RecordToolDenied, under ActionMCPToolDenied: until
// ADR-061 A10 was addressed this comment claimed a refusal earned no row
// because it "never reaches a tenant's data and is already in the operator
// log", and both halves of that were true while the conclusion was not -- see
// the argument on RecordToolDenied.
//
// ActorType is ActorAssistant, the one actor kind in this whole log that
// attributes the row to the MODEL rather than to the human who granted it
// access: auth.GrantID identifies WHICH connection acted, which is the fact an
// operator reviewing "what did this MCP client touch" actually wants, and it
// is a fact ActorUser (the approving human, possibly long gone from the
// account) cannot carry. operatorPermission and toolName both come from the
// registry entry AuthorizeTool already resolved -- see the OperatorPermission
// doc on ToolPolicy for why that field lands here.
//
// BEST-EFFORT, deliberately unlike Approve/RevokeConnection's RecordInTx:
// there is no companion write in the same transaction to roll back alongside a
// failed append (a tool call in this phase is a read), so failing the whole
// tool call because its own audit trail could not be appended would let a
// purely observational feature take down the read path it exists to observe.
// The error is returned so the caller can log it, never to be treated as a
// reason to withhold the tool's answer.
func (s *Service) RecordToolCall(ctx context.Context, auth AuthorizedRequest, toolName string, operatorPermission string) error {
	if s.audit == nil {
		return nil
	}
	_, err := s.audit.Record(ctx, audit.Event{
		TenantID:   auth.TenantID,
		ActorType:  audit.ActorAssistant,
		ActorID:    auth.GrantID.String(),
		Action:     audit.ActionMCPToolCalled,
		TargetType: "mcp_tool",
		TargetID:   toolName,
		Metadata: map[string]any{
			"grant_name":          auth.GrantName,
			"operator_permission": operatorPermission,
		},
	})
	return err
}

// RecordToolDenied appends the ActionMCPToolDenied audit row for one REFUSED
// tools/call. The caller (TransportHandler.callTool) invokes it on the
// AuthorizeTool refusal branch, alongside the operator log line and before the
// wire answer is chosen.
//
// WHY A REFUSAL EARNS A ROW WHEN THE OLD COMMENT ON RecordToolCall SAID IT DID
// NOT. That comment argued a refusal "never reached a tenant's data, and is
// already covered by the WarnContext log at the refusal site", and both halves
// are true and neither is sufficient. Never reaching the data is exactly what
// has to be PROVEN rather than assumed -- it is the boundary's own claim about
// itself -- and an slog line is not the artifact that proves it: it is not
// hash-chained, not tenant-scoped, not readable through the audit endpoint a
// customer's auditor is given, and it is subject to a retention the log
// pipeline chooses. "Who was denied what" was therefore answerable only by an
// operator with production log access, which is the wrong audience and the
// wrong durability for the record that a security boundary functioned.
//
// The actor pair is ActorAssistant over auth.GrantID, identical to
// RecordToolCall's, so a single query on actor_id returns everything one
// connection did AND everything it was refused -- which is the shape an
// investigation actually wants, and it is why this is not modelled as a
// separate actor kind.
//
// reason is the OPERATOR-FACING refusalReason, deliberately the value the wire
// blurs: the whole disclosure decision on AuthorizeTool is that "no such tool"
// and "not yours" are indistinguishable to the CALLER, and it holds only
// because the operator gets the precise reason somewhere else. This row is now
// that somewhere else. Taking the typed refusalReason rather than a string is
// what stops a caller composing a reason by hand and drifting from the log line
// two lines above it.
//
// BEST-EFFORT, and that is a deliberate divergence from ADR-061 A10's
// fail-closed language rather than an oversight. See TransportHandler.auditGap.
func (s *Service) RecordToolDenied(ctx context.Context, auth AuthorizedRequest, toolName string, reason refusalReason) error {
	// NOTE: a Service with no recorder records NOTHING and reports no error,
	// the same as RecordToolCall. That is the unit-test configuration (see the
	// audit field doc); in production WithAudit has always been called, and a
	// request path reached without it is a wiring bug whose finding is "this
	// deploy is unattributable", not "make this quieter".
	if s.audit == nil {
		return nil
	}
	// BOUNDED, because this is attacker-chosen input on a path the attacker can
	// reach WITHOUT holding the tool. toolName is whatever arrived in params,
	// up to maxRequestBytes (256 KiB), and every refusal would otherwise put all
	// of it through the per-tenant audit advisory lock. Being denied is not a
	// barrier to writing, so a caller who can use nothing on this surface could
	// still drive the serialised ledger writer.
	target, truncated, sanitized, origLen := audit.SafeTargetID(toolName)

	meta := map[string]any{
		"grant_name":        auth.GrantName,
		"refusal_reason":    string(reason),
		"held_capabilities": auth.Capabilities.Len(),
		"scoped_sites":      auth.Sites.Len(),
	}
	if truncated {
		// The row SAYS it is a prefix. Without this an auditor reads the
		// shortened value as the name the caller actually sent, and an
		// oversized-input probe -- which is itself the signal worth seeing --
		// becomes indistinguishable from an ordinary typo.
		meta["target_truncated"] = true
		meta["target_original_len"] = origLen
	}
	if sanitized {
		// Invalid UTF-8 in a tool name is not something a working client
		// produces, so this flag is the signal that someone was probing the
		// encoding boundary rather than mistyping.
		meta["target_sanitized"] = true
	}

	_, err := s.audit.Record(ctx, audit.Event{
		TenantID:  auth.TenantID,
		ActorType: audit.ActorAssistant,
		ActorID:   auth.GrantID.String(),
		Action:    audit.ActionMCPToolDenied,
		// The name AS SPELLED BY THE CALLER, not a registry name: on the
		// unregistered branch there is no registry entry, and the string the
		// caller guessed is the entire content of the evidence.
		TargetType: "mcp_tool",
		TargetID:   target,
		Metadata:   meta,
	})
	return err
}

// RecordProtocolDenied appends the ActionMCPProtocolDenied audit row for an
// authenticated request refused at revision negotiation.
//
// It is called from TransportHandler.writeProtocolRefusal, which is the ONE
// function both refusal sites go through (the per-request header in serve, and
// the initialize params). Putting the append there rather than at the two call
// sites is what makes "every protocol refusal is recorded" structurally true
// instead of true by convention -- a third refusal site added later inherits it.
//
// phase names which of the two negotiations refused, because they mean
// different things operationally: a params refusal is a client that cannot
// connect at all, a header refusal is a client that connected and then sent
// something else, and only the second can indicate a proxy rewriting headers.
func (s *Service) RecordProtocolDenied(ctx context.Context, auth AuthorizedRequest, neg Negotiation, phase string) error {
	if s.audit == nil {
		return nil
	}
	reason := neg.RefusalReason()

	// BOUNDED for the same reason as RecordToolDenied, and reachable the same
	// way: on the initialize_params phase neg.Raw is a JSON string from the
	// body, not a header, so it is bounded only by maxRequestBytes. An
	// unsupported revision is refused, and the refusal writes the row.
	target, truncated, sanitized, origLen := audit.SafeTargetID(neg.Raw)

	meta := map[string]any{
		"grant_name":     auth.GrantName,
		"refusal_reason": reason,
		"phase":          phase,
		// Recorded per row rather than left to be looked up: these are
		// compile-time constants that WILL move, and a row that says only
		// "2024-11-05 was refused" stops being interpretable the moment
		// they do.
		"floor":  ProtocolFloor,
		"target": ProtocolTarget,
	}
	if truncated {
		meta["target_truncated"] = true
		meta["target_original_len"] = origLen
	}
	if sanitized {
		// Worth its own field: invalid UTF-8 in a tool name or a revision
		// string is not something a working client produces, so this flag is
		// the signal that someone was probing the encoding boundary.
		meta["target_sanitized"] = true
	}

	_, err := s.audit.Record(ctx, audit.Event{
		TenantID:   auth.TenantID,
		ActorType:  audit.ActorAssistant,
		ActorID:    auth.GrantID.String(),
		Action:     audit.ActionMCPProtocolDenied,
		TargetType: "mcp_protocol",
		TargetID:   target,
		Metadata:   meta,
	})
	return err
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

	// THE SECOND RETURN OF ListSitesForRead IS DELIBERATELY DISCARDED, AND
	// DISCARDING IT IS A FIX RATHER THAN AN OVERSIGHT.
	//
	// That value is `more`: "the bounded page was filled and this TENANT has
	// further site rows". It was previously passed straight through into the
	// rendered result, which reported it to the caller as a truncation notice.
	// For a connection scoped to two sites in a six-hundred-site tenant that
	// notice was both false and disclosing -- the caller received every site
	// it may read, was told further sites had been withheld, and could infer
	// from a flag it had no business seeing that the tenant holds more than
	// sitesPageBound sites.
	//
	// A count taken over the TENANT can never be reported to a SITE-SCOPED
	// caller, however harmless it looks as a completeness flag. Completeness
	// is recomputed below over the caller's own scope instead, where both
	// terms are numbers the caller already knows.
	rows, _, err := s.store.ListSitesForRead(ctx, auth.TenantID, sitesPageBound)
	if err != nil {
		return "", fmt.Errorf("list sites for mcp read: %w", err)
	}

	// Filter to the grant's resolved set. The query is tenant-scoped by RLS;
	// this narrows it further to the sites THIS GRANT may read. SiteSet.Allows
	// returns false for every id on an empty or zero-value set, so there is no
	// widening path even if the set were somehow lost between here and there.
	//
	// THIS IS LAYER 3, AND LAYER 3 HIDES. A row dropped here is not refused,
	// not counted and not mentioned: it leaves no trace in the envelope built
	// below, because `asked` is closed over auth.Sites and an out-of-scope row
	// was never in auth.Sites to begin with.
	allowed := make([]sqlc.ListSitesRow, 0, len(rows))
	seen := make(map[uuid.UUID]struct{}, len(rows))
	for _, r := range rows {
		if auth.Sites.Allows(r.ID) {
			allowed = append(allowed, r)
			seen[r.ID] = struct{}{}
		}
	}

	// SCOPE-RELATIVE COMPLETENESS. `asked` is the caller's own scope
	// cardinality and nothing else; every site it counts is one the caller
	// already knows it has. An in-scope site the bounded page did not contain
	// is reported as an explicit site_unread refusal rather than being left
	// silently absent, so ok+refused balances against asked and the caller is
	// never handed a short list that reads as a complete fleet.
	refusals := make([]Refusal, 0)
	for _, id := range auth.Sites.Sorted() {
		if _, found := seen[id]; !found {
			refusals = append(refusals, Refusal{
				SiteID: id.String(),
				Code:   RefusalSiteUnread,
				Detail: "this site is in scope for this connection but was not returned by the " +
					"bounded page this call reads; retry, and report it if it persists",
			})
		}
	}

	env, err := NewEnvelope(auth.Sites.Len(), len(allowed), refusals)
	if err != nil {
		// An unbalanced envelope means a site was counted in scope and then
		// lost without being accounted for. That is the leak shape, so it
		// fails the call rather than rendering a result with a residual in it.
		return "", fmt.Errorf("build fleet_sites_list envelope: %w", err)
	}

	return buildListSitesResult(allowed, env, s.now())
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

// ---------------------------------------------------------------------------
// Operator-facing connection management (S16 -- design Step 10, list + revoke)
// ---------------------------------------------------------------------------

// bootstrapTenantPrincipal builds the org-scoped principal that Authenticate
// hands the scope-resolution chokepoint. It exists so that the ONE place on
// this surface that must not run under app.site_scope says so by name, in code,
// instead of being indistinguishable from a call site that forgot.
//
// WHY THIS IS NOT A SITE-SCOPED PRINCIPAL. Authenticate has no domain.Principal
// at all -- it is authenticating a bearer token, and the only identity in hand
// is the grant's own stored SiteScopeMode / ScopeTagIds / ScopeSiteIds.
// Resolving those three columns into concrete site ids is the step that
// COMPUTES this connection's allowlist. Scoping that step by the allowlist it
// is computing is circular, and it breaks differently in each of the three
// modes:
//
//   - 'tags'  -- the site set is not known until this query runs. There is no
//     allowlist to supply. Passing the empty one resolves to ZERO SITES, and
//     because an empty result correctly means "no sites" and never "no filter",
//     every tag-scoped connection would authenticate to nothing. That is
//     fail-closed and still a total outage of the mode.
//
//   - 'all'   -- names no ids. Org-wide IS the intended meaning; a site-scoped
//     principal here contradicts the stored grant.
//
//   - 'list'  -- the allowlist would be exactly the requested ids, so the
//     policy would intersect the set with itself and could refuse nothing the
//     query does not already refuse. Zero security gain, and a second copy of
//     the filter to keep in step.
//
//     THAT ARM IS A CONDITION AND NOT A STANDING FACT, so read it as one before
//     relying on it. It holds only while sites_site_scope's predicate
//     (`sites.id = ANY(<app.allowed_site_ids>)`, m19) and this query's own
//     predicate (`s.id = ANY($3::uuid[])`, ResolveMCPGrantScopeSitesInTenantTx)
//     range over THE SAME RELATION AND THE SAME COLUMN. Nothing binds them
//     together. If either grows a second term, or 'list' starts resolving
//     through a join rather than off `sites.id`, the intersection stops being
//     the identity and this arm needs deciding again from scratch.
//
// The boundary that IS load-bearing here is tenant isolation, and it holds:
// RunTenantTx dispatches this principal to InTenantTx, the join through `sites`
// runs under the tenant policies, and every foreign or nonexistent UUID in the
// uuid[] column is dropped. What the caller does with the resolved set is where
// the site boundary lives -- see NewSiteSet, whose zero value allows nothing.
//
// It returns a domain.Principal with Scope and UserID unset ON PURPOSE. That is
// the org-scoped, no-user shape, and it is what the dispatch reads.
//
// THE NAME AND THIS COMMENT ARE THE ONLY STRUCTURAL BARRIER, AND THAT IS WORTH
// SAYING OUT LOUD. dispatchTenantTx reads exactly three fields -- Scope, UserID,
// AllowedSiteIDs -- so the value returned here is INDISTINGUISHABLE at the
// dispatch from any other org-scoped, no-user principal. Nothing in the type
// system marks this one as the deliberate exception. What guards it is executed
// rather than structural: TestAuthenticateHandsTheChokepointAnUnscopedPrincipal
// asserts that the principal Authenticate hands the chokepoint is NOT
// site-constrained, and it is what goes red, at unit speed, if someone applies
// ADR-061 A11 item 2 literally here.
func bootstrapTenantPrincipal(tenantID uuid.UUID) domain.Principal {
	return domain.Principal{TenantID: tenantID}
}

// requireOrgScopedPrincipal is the site-scope refusal, and it is the SECOND of
// three independent layers rather than the only one.
//
// The three, outermost first:
//
//  1. authz.RequirePermission(PermAPIKeyRead / PermAPIKeyManage) in the route.
//     Both are members of authz.orgLevelPerms, so a site-constrained principal
//     is refused 403 there and never reaches this service.
//  2. THIS FUNCTION.
//  3. mcp_grants_site_scope_select / _update, RESTRICTIVE policies keyed on the
//     app.site_scope GUC that Repo's RunTenantTx sets.
//
// LAYER 2 IS NOT REDUNDANT WITH LAYER 3, AND THAT IS THE ENTIRE POINT. Layer 3
// refuses by returning ZERO ROWS WITH NO ERROR. On the list path that is
// indistinguishable at the Go layer from an organisation that has minted no
// connections, so a service relying on RLS alone would answer a refused
// collaborator with `{"connections": []}` and the UI would render "you have no
// connections" -- a false statement produced by a security control working
// correctly. Refusing here is what makes the refusal SAYABLE.
//
// It is not redundant with layer 1 either: layer 1 lives in the route
// registration, and a route mounted without its middleware is a wiring mistake
// that presents as a working endpoint. This function is inside the call.
func requireOrgScopedPrincipal(p domain.Principal) error {
	if p.TenantID == uuid.Nil {
		return domain.Validation(ErrCodeInvalidRequest, "an organisation is required")
	}
	if p.IsSiteConstrained() {
		return domain.Forbidden(ErrCodeOrgScopeRequired,
			"an AI connection is an organisation-wide credential, so listing or revoking one "+
				"requires full organisation membership. This is a refusal, not an empty list.")
	}
	return nil
}

// ListConnections returns every grant in the principal's organisation, revoked
// ones included.
//
// REVOKED GRANTS ARE RETURNED ON PURPOSE (m124 Decision 2). The row is kept
// after revocation precisely so last_used_at and revoked_at stay readable: what
// the credential did while it was live is the thing an operator reviews after
// revoking it. The caller renders the status column and must never infer
// liveness from presence in this list.
//
// The returned slice is nil on error and non-nil on success, including for a
// genuinely empty organisation. That asymmetry is deliberate and is what lets
// the layer above tell "we did not read the list" from "the list is empty".
func (s *Service) ListConnections(ctx context.Context, p domain.Principal) ([]Connection, error) {
	if err := requireOrgScopedPrincipal(p); err != nil {
		return nil, err
	}

	rows, err := s.store.ListGrants(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("list mcp connections: %w", err)
	}

	out := make([]Connection, 0, len(rows))
	for _, r := range rows {
		out = append(out, connectionFromGrant(r))
	}
	return out, nil
}

// RevokeOutcome is what a revoke actually did. It is a struct of counts rather
// than a bool because the query distinguishes four outcomes and three of them
// are successes that differ in what was left to do -- see the query comment on
// RevokeMCPGrantWithTokensInTenantTx.
type RevokeOutcome struct {
	// GrantsRevoked is 1 when this call flipped the grant, 0 when it was
	// already revoked.
	GrantsRevoked int64
	// TokensRevoked is how many live bearer tokens this call killed. It can be
	// non-zero while GrantsRevoked is 0: that is the REPAIR of a half-revoked
	// grant, and it is the state a security review of this stack actually
	// observed in the database.
	TokensRevoked int64
	// AlreadyRevoked reports that the grant was not active when this call ran.
	// The request still succeeded -- the end state the caller asked for holds.
	AlreadyRevoked bool
}

// RevokeConnection revokes a grant AND every live token beneath it.
//
// THE CASCADE IS THE POINT OF THIS METHOD. A revoke that flips only the grant
// leaves a live bearer token in a client's config file, and this stack has
// already been observed in exactly that state (`grant_status revoked /
// token_status active`). The Store interface deliberately offers no grant-only
// revoke, so there is no way to express the half here.
//
// FOUR OUTCOMES, AND ONLY ONE OF THEM IS A FAILURE:
//
//	pgx.ErrNoRows       the grant is not visible to this principal -- absent,
//	                    another organisation's, or refused by RLS. 404. NOTHING
//	                    WAS WRITTEN, and this is the ONLY reading of "matched no
//	                    rows" that means failure.
//	grants=1            flipped now, with TokensRevoked tokens killed.
//	grants=0, tokens>0  the grant was already revoked and its tokens were not.
//	                    This call CONVERGED that half-revoked state. Success.
//	grants=0, tokens=0  already fully revoked. The requested end state holds, so
//	                    this is an idempotent retry and it SUCCEEDED.
//
// THE LAST ONE IS THE TRAP AND IT IS WHY THIS COMMENT IS LONG. Two zeroes look
// like "nothing happened, therefore something went wrong". Mapping them to 404
// or 500 would report a correctly revoked credential as a failure, which invites
// an operator to retry or -- much worse -- to believe the credential is still
// live and go hunting for it. What separates the two cases is whether a ROW CAME
// BACK AT ALL, never what the counts say.
func (s *Service) RevokeConnection(ctx context.Context, p domain.Principal, grantID uuid.UUID) (RevokeOutcome, error) {
	if err := requireOrgScopedPrincipal(p); err != nil {
		return RevokeOutcome{}, err
	}
	if grantID == uuid.Nil {
		// uuid.Nil is what a failed parse decays to. Refusing it by name stops
		// a malformed id being sent to the database as a real-looking key.
		return RevokeOutcome{}, domain.Validation(ErrCodeInvalidRequest,
			"a connection id is required")
	}

	row, err := s.store.RevokeGrantWithTokens(ctx, p, grantID,
		// ActionMCPGrantRevoked, in the SAME transaction as the revoke
		// statement. Runs on all three non-error outcomes (freshly revoked,
		// half-revoked repair, idempotent no-op): the operator explicitly
		// asked for this credential to be revoked and the request is what is
		// being attributed, not merely a state transition. It never runs
		// alongside pgx.ErrNoRows -- see onRevoked's doc on the interface.
		func(tx pgx.Tx, row sqlc.RevokeMCPGrantWithTokensInTenantTxRow) error {
			if s.audit == nil {
				return nil
			}
			// THE ACTOR IS WHICHEVER CREDENTIAL AUTHENTICATED, resolved by
			// audit.ActorFor rather than hardcoded.
			//
			// THIS ROUTE IS MOUNTED BY THE SAME CALL AS THE HEADLESS MINT --
			// server.go's RegisterConnections(v1), one nil check below
			// Register(v1) -- so an API key that can mint a connection can
			// revoke one, today, with nothing in between. ActorUser over
			// p.UserID therefore wrote uuid.Nil for exactly the caller class
			// this surface was built for, and it wrote it on THE ROW AN
			// AUDITOR REACHES FOR AFTER AN INCIDENT: "who killed this
			// credential, and when" answered with a user id that resolves to
			// no user and, because the name join is gated on actor_type, to no
			// name either.
			actorType, actorID := audit.ActorFor(p)
			_, aerr := s.audit.RecordInTx(ctx, tx, audit.Event{
				TenantID:   p.TenantID,
				ActorType:  actorType,
				ActorID:    actorID,
				Action:     audit.ActionMCPGrantRevoked,
				TargetType: "mcp_grant",
				TargetID:   grantID.String(),
				Metadata: map[string]any{
					"grants_revoked":  row.GrantsRevoked,
					"tokens_revoked":  row.TokensRevoked,
					"already_revoked": row.GrantsRevoked == 0,
				},
			})
			return aerr
		})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RevokeOutcome{}, domain.NotFound(ErrCodeConnectionNotFound,
				"no such connection")
		}
		return RevokeOutcome{}, fmt.Errorf("revoke mcp connection: %w", err)
	}

	return RevokeOutcome{
		GrantsRevoked: row.GrantsRevoked,
		TokensRevoked: row.TokensRevoked,
		// Derived from the count and NOT from a second read: "the grant was not
		// active when this statement ran" is exactly grants_revoked = 0, and
		// asking the database again would be asking a different moment.
		AlreadyRevoked: row.GrantsRevoked == 0,
	}, nil
}

// connectionFromGrant maps one stored row onto the operator-facing shape.
//
// EVERY NULLABLE COLUMN STAYS NULLABLE. pgtype's Valid flag is consulted for
// each timestamp rather than reading .Time unconditionally: an invalid
// pgtype.Timestamptz carries the Go zero time, which serialises as a real
// date in the year 1 and would render as "last used 1 Jan 0001" for a
// connection that has never been used.
//
// Scopes is DERIVED, not read. mcp_grants has no scopes column (m124 Decision
// 1 declines to add one, so that a write capability cannot appear without a
// migration), and grantScopes() returns the one scope every live grant holds by
// construction. That is exact only while recognisedScopes has one entry, which
// is what TestGrantScopesIsExactOnlyWhileOneScopeExists pins -- when a second
// scope is added, that test goes red and this line has to start reading the
// grant instead of a constant.
func connectionFromGrant(g sqlc.McpGrant) Connection {
	return Connection{
		ID:            g.ID,
		Name:          g.Name,
		Status:        GrantStatus(g.Status),
		SiteScopeMode: SiteScopeMode(g.SiteScopeMode),
		Scopes:        grantScopes(),
		// Read straight off the row via capabilitiesFromColumn -- the same
		// reader Authenticate uses on chk.GrantCapabilities -- and never
		// recomputed. See Connection.Capabilities.
		Capabilities:          capabilitiesFromColumn(g.Capabilities),
		CreatedAt:             g.CreatedAt,
		ReportedClientName:    g.ClientName,
		ReportedClientVersion: g.ClientVersion,
		// The OPERATOR's choice, read straight off the row and never
		// substituted from ClientName above. The two fields sit adjacent here
		// on purpose: they answer different questions and either may be nil
		// while the other is set. See Connection.SetupClient.
		SetupClient: g.SetupClient,
		Protocol: ClassifyStoredProtocol(
			timestamptzTimeOrNil(g.ClientIdentityRecordedAt), g.ProtocolVersion),
		LastUsedAt: timestamptzTimeOrNil(g.LastUsedAt),
		RevokedAt:  timestamptzTimeOrNil(g.RevokedAt),
	}
}

// timestamptzTimeOrNil converts a pgtype.Timestamptz to *time.Time, mapping SQL
// NULL to nil rather than to the Go zero time.
//
// It is the single line that keeps "never" distinguishable from "at
// 0001-01-01", so it exists once and is called everywhere rather than being
// restated per field.
//
// Distinct from tools.go's timestampOrNil, which renders straight to *string for
// the model-facing tool output. This one stays in the time domain because the
// DOMAIN type holds *time.Time; the string rendering happens later, in dto.go,
// where the wire shape is decided. Two functions, because the two layers answer
// to different consumers and collapsing them would make one of them render at
// the wrong layer.
func timestamptzTimeOrNil(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}
