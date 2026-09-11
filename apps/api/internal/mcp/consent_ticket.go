package mcp

// consent_ticket.go: the server-side record of what an /authorize call
// actually carried, so that the approval which follows it is measured against
// that request rather than against its own body.
//
// THE PROPERTY THIS FILE EXISTS TO HOLD. A grant's stored scope set is the set
// the authorize call requested and the consent screen displayed. Not a set the
// approval POST names, and not "any set the registry happens to recognise".
//
// WHY A TICKET AND NOT A ROW. Service.Authorize mints nothing -- no grant, no
// code, no row -- and that is deliberate: until a human approves, nothing
// exists to revoke, expire or reconcile, and an unauthenticated caller cannot
// make this server write. Keeping that property means the authorize call's
// output is AUTHENTICATED rather than STORED: the scope set travels through the
// browser with a MAC this server computed, and comes back verifiable. The
// alternative -- a table of pending authorization requests -- reintroduces
// exactly the unauthenticated-write surface the endpoint was built without, and
// costs a migration for state whose whole life is one consent screen.
//
// This is the shape auth/social_handshake.go already uses for the same problem
// one flow over: state that must survive a browser round trip, keyed from the
// instance session secret, with the expiry inside the sealed payload rather
// than in a header the caller controls.
//
// WHY A MAC AND NOT AN AEAD. The handshake cookie encrypts because it carries a
// PKCE verifier and a nonce, which are secrets. Nothing here is: the scope set
// is rendered to the operator on the consent screen and echoed in the approval
// body. What this needs is integrity and origin, which is what HMAC is, and a
// primitive whose failure mode is legible beats one whose payload a reviewer
// cannot read.

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

const (
	// consentTicketTTL is how long an operator has between the authorize call
	// and the approval POST.
	//
	// The consent screen is not a click-through: the operator names the
	// connection, chooses the site scope (all, by tag, or a hand-picked list)
	// and may narrow the capability set, and the site and tag pickers are
	// themselves loaded from the API. Fifteen minutes is long enough for that
	// with a distraction in the middle, and short enough that an abandoned
	// authorization is gone rather than approvable tomorrow from a tab left
	// open.
	//
	// IT IS THE CEILING ON A STALE CONSENT, which is the reason it exists at
	// all rather than being left unbounded. A ticket is the statement "this
	// request was made"; a statement with no expiry outlives the intent behind
	// it, and the client's registration, redirect URIs and the operator's own
	// permissions can all have changed in the meantime. The authorization code
	// minted at the end of this flow lives five minutes for the same reason.
	consentTicketTTL = 15 * time.Minute

	// consentTicketKeyInfo separates this key from every other use of the
	// instance session secret -- the session store, the social handshake codec,
	// the derived age identity. Two purposes sharing one derived key is how a
	// value signed for one becomes valid for the other.
	consentTicketKeyInfo = "wpmgr/mcp/consent-ticket/v1"

	// consentTicketMaxBytes refuses an oversized ticket before any decoding.
	// What this server issues is a few hundred bytes; the bound is generous and
	// exists so a caller cannot make the process do unbounded work by claiming
	// a ticket.
	consentTicketMaxBytes = 4096
)

// ErrCodeConsentTicketInvalid is the refusal for an approval that carries no
// consent ticket this server issued, one that has been altered since, one that
// has expired, or one issued for a different client.
//
// IT IS A DISTINCT CODE FROM THE SCOPE MISMATCH BELOW, deliberately. "Start the
// flow again" and "your body disagrees with your request" are different
// remedies, and a caller that cannot tell them apart retries the wrong one.
const ErrCodeConsentTicketInvalid = "mcp_invalid_consent_ticket"

// ErrCodeScopeNotAuthorized is the refusal for an approval whose scope set is
// not the set the authorize call carried.
//
// The message NAMES THE OFFENDING SCOPE. That is the same stance
// ParseRequestedScopes takes on an unrecognised one: the value is the caller's
// own input so echoing it discloses nothing, and a refusal that does not say
// what it refused leaves an automated client retrying the identical request
// forever.
const ErrCodeScopeNotAuthorized = "mcp_scope_not_authorized"

// consentTicketClaims is what the authorize call leaves behind for the approval
// that follows it. Field names are one letter because this is serialised into
// every consent response, not because anything reads them by hand.
type consentTicketClaims struct {
	// ClientID binds the ticket to the client the operator was shown. Without
	// it a ticket issued for one client's request is a valid ticket for any
	// other's approval, and "the operator approved this for THAT client" stops
	// being something this server can say.
	ClientID string `json:"c"`

	// Scopes is the authorize call's parsed, deduplicated, sorted scope set --
	// the set the consent screen renders and the only set an approval may
	// store.
	Scopes []string `json:"s"`

	// Expires is the authority on freshness, inside the MAC. A deadline the
	// caller can edit is not a deadline.
	Expires int64 `json:"e"`
}

// consentTicketCodec issues and verifies consent tickets.
type consentTicketCodec struct {
	key []byte
}

// newConsentTicketCodec derives the ticket key from the instance session
// secret, which every replica of this process shares and which is already
// required to exist and to be non-trivial before boot
// (config.ValidateSessionSecret).
//
// THE SHARED SECRET IS THE POINT. A ticket must verify on whichever instance
// receives the approval, which is not in general the instance that issued it,
// and must survive a deploy that lands between the two. A per-process key
// satisfies neither.
func newConsentTicketCodec(sessionSecret string) (*consentTicketCodec, error) {
	if len(sessionSecret) < 32 {
		return nil, errors.New("mcp consent ticket: session secret too short to derive a key from")
	}
	// No salt: the input is a high-entropy instance secret, and a random salt
	// would have to be stored for the approval to derive the same key, which is
	// the server-side state this design exists without.
	key, err := hkdf.Key(sha256.New, []byte(sessionSecret), nil, consentTicketKeyInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("mcp consent ticket: derive key: %w", err)
	}
	return &consentTicketCodec{key: key}, nil
}

// newEphemeralConsentTicketCodec keys a codec from fresh entropy.
//
// IT IS THE DEFAULT NewService ARMS, and the reason is the one NewService
// already gives about its rate limiter: an unarmed codec would be a wiring
// failure that presents as a working endpoint. There is no nil codec and
// therefore no branch anywhere that treats "no key" as "skip the check" --
// every Service verifies, always.
//
// A process left on this default is not insecure, it is merely alone: tickets
// it issues verify only against itself, so a multi-instance deployment that
// forgot the wiring refuses approvals with a named error instead of accepting
// unbound ones. cmd/wpmgr calls SetConsentSigningSecret at boot.
func newEphemeralConsentTicketCodec() *consentTicketCodec {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		// Unreachable on any supported platform (crypto/rand does not return a
		// usable error), and a service holding a zero key would issue tickets
		// anyone could forge. Failing construction is the only honest branch.
		panic("mcp: cannot key the consent ticket codec: " + err.Error())
	}
	return &consentTicketCodec{key: key}
}

// issue returns the ticket for an authorize call carrying scopes for clientID.
func (c *consentTicketCodec) issue(clientID string, scopes []Scope, now time.Time) (string, error) {
	names := scopeNames(scopes)
	sort.Strings(names)
	payload, err := json.Marshal(consentTicketClaims{
		ClientID: clientID,
		Scopes:   names,
		Expires:  now.UTC().Add(consentTicketTTL).Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("mcp consent ticket: marshal: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	return body + "." + base64.RawURLEncoding.EncodeToString(c.mac(body)), nil
}

// open returns the claims a ticket carries, or false for anything this server
// did not issue, anything altered since, and anything expired.
//
// One boolean rather than an error, for the reason handshakeCodec.open gives:
// every failure here has the same answer for the caller, and naming which check
// failed invites a branch that treats "altered" as recoverable.
func (c *consentTicketCodec) open(raw string, now time.Time) (consentTicketClaims, bool) {
	if raw == "" || len(raw) > consentTicketMaxBytes {
		return consentTicketClaims{}, false
	}
	body, sig, ok := strings.Cut(raw, ".")
	if !ok {
		return consentTicketClaims{}, false
	}
	presented, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return consentTicketClaims{}, false
	}
	// Constant time, and BEFORE the payload is decoded: nothing inside an
	// unauthenticated blob may steer this function.
	if !hmac.Equal(presented, c.mac(body)) {
		return consentTicketClaims{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return consentTicketClaims{}, false
	}
	var claims consentTicketClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return consentTicketClaims{}, false
	}
	if claims.Expires <= now.UTC().Unix() {
		return consentTicketClaims{}, false
	}
	return claims, true
}

func (c *consentTicketCodec) mac(body string) []byte {
	m := hmac.New(sha256.New, c.key)
	m.Write([]byte(body))
	return m.Sum(nil)
}

// authorizedScopes resolves the scope set an approval may store: the set its
// authorize call carried, having refused any approval that names a different
// one.
//
// WHAT IT RETURNS IS THE TICKET'S SET, not the body's, even though the two are
// equal by the time it returns. The value that is written to the grant and the
// value the capability ceiling is computed from therefore both come from the
// server's own record of the request, and a future loosening of the comparison
// below cannot silently become a loosening of what is stored.
//
// EXACT SET EQUALITY, IN BOTH DIRECTIONS, and the narrowing direction is
// refused on purpose. A body naming LESS than the authorize call is not an
// escalation, but neither is it anybody's decision: the consent screen offers
// no scope-narrowing control, so a narrower body is not an operator choice, it
// is the round trip disagreeing with itself. Accepting it would mean the screen
// said one thing and the grant recorded another -- the same "the two parties
// never learn they disagreed" failure the closed registry refuses one layer up,
// pointing the other way. The capability axis shows what narrowing looks like
// when it is real: an explicit control on the screen, a value carried in its
// own field, and a refusal rather than an intersection when it exceeds the
// ceiling. A scope-narrowing control arrives the same way or not at all.
func (s *Service) authorizedScopes(consent ConsentContext) ([]Scope, error) {
	// The body's own set first, through the same exit gate the authorize call
	// went through, so a malformed or unrecognised scope keeps the precise
	// refusal it already had rather than being answered with "bad ticket".
	requested, err := ParseRequestedScopes(scopesToString(consent.Scopes))
	if err != nil {
		return nil, err
	}

	claims, ok := s.consentTickets.open(consent.ConsentTicket, s.now())
	if !ok {
		return nil, domain.Validation(ErrCodeConsentTicketInvalid,
			"this approval carries no valid consent ticket; the authorization request "+
				"must be started again")
	}
	if claims.ClientID != consent.ClientID {
		return nil, domain.Validation(ErrCodeConsentTicketInvalid,
			"the consent ticket was issued for a different client_id")
	}

	// The authorized set, re-parsed rather than trusted verbatim. The ticket is
	// this server's own, but it was issued by a possibly older build, and a
	// scope that build recognised and this one does not is a grant this build
	// cannot correctly evaluate. Authenticate takes the same stance on a stored
	// scope it does not recognise: refuse, never trim.
	authorized, err := ParseRequestedScopes(strings.Join(claims.Scopes, " "))
	if err != nil {
		return nil, err
	}

	authorizedSet := make(map[Scope]struct{}, len(authorized))
	for _, sc := range authorized {
		authorizedSet[sc] = struct{}{}
	}
	requestedSet := make(map[Scope]struct{}, len(requested))
	for _, sc := range requested {
		requestedSet[sc] = struct{}{}
	}

	for _, sc := range requested {
		if _, ok := authorizedSet[sc]; !ok {
			return nil, domain.Validation(ErrCodeScopeNotAuthorized,
				fmt.Sprintf("scope %q is not part of the authorization request this consent "+
					"was issued for", string(sc))).
				WithDetails(map[string]any{
					"unauthorized_scope": string(sc),
					"authorized_scopes":  claims.Scopes,
				})
		}
	}
	for _, sc := range authorized {
		if _, ok := requestedSet[sc]; !ok {
			return nil, domain.Validation(ErrCodeScopeNotAuthorized,
				fmt.Sprintf("this approval drops scope %q, which the authorization request "+
					"carried and the consent screen showed; approve the request as it was "+
					"made or start a new one", string(sc))).
				WithDetails(map[string]any{
					"missing_scope":     string(sc),
					"authorized_scopes": claims.Scopes,
				})
		}
	}
	return authorized, nil
}
