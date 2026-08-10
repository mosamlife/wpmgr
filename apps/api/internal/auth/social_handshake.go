package auth

// social_handshake.go: the state a social sign-in start hands to its own
// callback, carried in a sealed cookie instead of a server-side session record.
//
// WHY IT IS NOT IN THE SESSION ANY MORE. GET /auth/social/:provider/start needs
// no session, no credential and no CSRF token, by construction: it is the first
// thing a visitor who has never signed in clicks. It used to put the handshake
// on the SCS session, which marked that session Modified, which committed a NEW
// record to Redis on every request, held for the session store's idle lifetime
// (7 days on the shipped defaults). One unauthenticated client that discards its cookies could
// therefore write records without bound into the Redis that holds every live
// session on the instance: under allkeys-lru that evicts signed-in people, under
// noeviction it fails the next commit, and either way it is an outage for
// everyone rather than for the caller.
//
// RATIONING THAT RESOURCE WAS THE WRONG ANSWER TWICE. A shared instance-wide
// ceiling denies sign-in to everybody the moment one host is busy. A per-client
// ceiling cannot be keyed on anything the client does not choose: this install
// sets no trusted-proxy list, so X-Forwarded-For is caller-supplied, which makes
// a per-IP key both evadable by rotating the header and usable to burn a named
// victim's allowance. So the resource is removed instead. The start endpoint now
// writes no server-side state at all, and there is nothing left to flood.
//
// WHAT REPLACES IT. The four values a handshake needs (provider, state, nonce,
// PKCE verifier) plus the deep link travel in one cookie, sealed with a key
// derived from the instance session secret. The server keeps nothing, so the
// cost of a start request is a signature and a 302.
//
// WHAT THIS DOES NOT CLAIM, because a comment that overstates its bound is worse
// than none:
//
//   - It is not a rate limit. A flood still costs CPU, a TLS handshake and an
//     AES seal per request, and that is what the reverse proxy and the platform
//     in front of this process are for. What it removes is the part that
//     outlived the request and was shared with every other user.
//   - The callback still performs an outbound token exchange for anyone who
//     presents a handshake this server sealed, so it can be used to make the
//     process talk to Google or GitHub. That is bounded by the providers' own
//     limits, not by anything here.
//   - A signature stops a cookie being EDITED, not one being replayed by
//     whoever legitimately obtained it. It is the same-origin, host-only cookie
//     plus the state check that binds a callback to the browser that started it.
//   - GET /auth/oidc/login, the operator's generic SSO issuer, still stores its
//     handshake in the session and still has the shape described above. It is a
//     pre-existing path, live only where an operator configured an issuer, and
//     it is deliberately untouched here; the same treatment applies to it
//     unchanged.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	// handshakeCookieName is deliberately not the session cookie's name: the two
	// have different lifetimes, different paths and different contents, and one
	// must never be mistaken for the other.
	handshakeCookieName = "wpmgr_social"

	// handshakeCookiePath scopes the cookie to the routes that use it, so it is
	// not attached to every API request the app makes afterwards.
	handshakeCookiePath = "/auth/social"

	// handshakeTTL is how long a person has to get through a provider's consent
	// screen. Long enough for a password manager, an account chooser and a
	// second factor at the provider; short enough that an abandoned handshake is
	// gone rather than resumable next week.
	handshakeTTL = 10 * time.Minute

	// handshakeKeyInfo separates this key from every other use of the session
	// secret (the derived age identity, above all). Two purposes sharing one
	// derived key is how a value signed for one becomes valid for the other.
	handshakeKeyInfo = "wpmgr/social-handshake/v1"

	// handshakeMaxCookieBytes refuses an oversized cookie before decrypting it.
	// Nothing this server seals comes close: the largest field is the return
	// path, capped at 512 bytes by safeReturnPath.
	handshakeMaxCookieBytes = 4096
)

// handshake is what a start must tell its own callback.
//
// The field names are short because this is serialised into a cookie on every
// sign-in, not because anything reads them by hand.
type handshake struct {
	Provider string `json:"p"`
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	// Return is where the person was heading before the handshake, already
	// validated by safeReturnPath at the point it was still ours. It is
	// re-validated before it reaches a Location header (socialLandingPath,
	// socialFail), so a value that somehow got in here cannot become a redirect.
	Return string `json:"r,omitempty"`
	// Expires is the authority on freshness. The cookie's own Max-Age is a
	// request the browser may ignore and an attacker certainly will.
	Expires int64 `json:"e"`
}

// handshakeCodec seals and opens handshakes.
//
// AES-GCM rather than a bare HMAC: authentication is what this needs, but the
// payload also carries the PKCE verifier and the nonce, which under the previous
// design never left the process. Encrypting keeps that true of the wire and of
// anything that logs a cookie, at the cost of one extra primitive.
type handshakeCodec struct {
	aead cipher.AEAD
}

// newHandshakeCodec derives the handshake key from the instance session secret.
//
// The secret is already required to exist and to be non-trivial before the
// process boots (config.ValidateSessionSecret), so there is no fallback here: a
// codec either has a real key or is not built at all, and socialStart refuses
// rather than issuing something nobody signed.
func newHandshakeCodec(sessionSecret string) (*handshakeCodec, error) {
	if len(sessionSecret) < 32 {
		return nil, errors.New("social handshake: session secret too short to derive a key from")
	}
	// No salt: the input is a high-entropy instance secret, and a random salt
	// would have to be stored somewhere for the callback to derive the same key,
	// which is the server-side state this whole change removes.
	key, err := hkdf.Key(sha256.New, []byte(sessionSecret), nil, handshakeKeyInfo, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &handshakeCodec{aead: aead}, nil
}

// seal returns the cookie value for hs, valid for ttl.
func (c *handshakeCodec) seal(hs handshake, ttl time.Duration) (string, error) {
	hs.Expires = time.Now().UTC().Add(ttl).Unix()
	payload, err := json.Marshal(hs)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nonce, nonce, payload, nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// open returns the handshake a cookie carries, or false for anything this
// server did not seal, anything altered since, and anything expired.
//
// It reports one boolean rather than an error on purpose. Every failure here has
// the same answer for the caller (treat it as no handshake at all), and telling
// a caller which check failed invites a branch that treats "tampered" as
// recoverable.
func (c *handshakeCodec) open(raw string) (handshake, bool) {
	if raw == "" || len(raw) > handshakeMaxCookieBytes {
		return handshake{}, false
	}
	sealed, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(sealed) < c.aead.NonceSize() {
		return handshake{}, false
	}
	nonce, ct := sealed[:c.aead.NonceSize()], sealed[c.aead.NonceSize():]
	payload, err := c.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return handshake{}, false
	}
	var hs handshake
	if err := json.Unmarshal(payload, &hs); err != nil {
		return handshake{}, false
	}
	if hs.Expires <= time.Now().UTC().Unix() {
		return handshake{}, false
	}
	return hs, true
}

// SetHandshakeSecret keys the social handshake cookie from the instance session
// secret. Call it after NewHandler, before serving.
//
// Leaving it unset does not degrade to an unsigned cookie: socialStart refuses
// to start a handshake it cannot seal. An unsealed handshake would let the
// caller nominate the provider that verifies their code and the page they land
// on, which is exactly what the seal is for.
func (h *Handler) SetHandshakeSecret(sessionSecret string) error {
	codec, err := newHandshakeCodec(sessionSecret)
	if err != nil {
		return err
	}
	h.handshake = codec
	return nil
}

// putHandshake seals hs into the response cookie.
func (h *Handler) putHandshake(c *gin.Context, hs handshake) error {
	if h.handshake == nil {
		return errors.New("social handshake: no signing key configured")
	}
	value, err := h.handshake.seal(hs, handshakeTTL)
	if err != nil {
		return err
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name:  handshakeCookieName,
		Value: value,
		Path:  handshakeCookiePath,
		// No Domain, so the cookie is host-only: a sibling host on the same
		// registrable domain can neither read it nor plant one.
		MaxAge:   int(handshakeTTL / time.Second),
		HttpOnly: true,
		Secure:   h.secureCookies,
		// Lax, not Strict. The provider returns the browser here with a
		// top-level GET navigation from its own origin, which Lax sends the
		// cookie on and Strict does not: Strict would be a handshake that can
		// never complete, which is not a stricter policy but a broken one.
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// takeHandshake reads the handshake cookie and clears it, so a handshake is
// single-use however the callback ends.
func (h *Handler) takeHandshake(c *gin.Context) (handshake, bool) {
	h.clearHandshake(c)
	if h.handshake == nil {
		return handshake{}, false
	}
	ck, err := c.Request.Cookie(handshakeCookieName)
	if err != nil {
		return handshake{}, false
	}
	return h.handshake.open(ck.Value)
}

// clearHandshake expires the cookie. Same name and path as the one that was
// set, or the browser keeps the original alongside the deletion.
func (h *Handler) clearHandshake(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     handshakeCookieName,
		Value:    "",
		Path:     handshakeCookiePath,
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}
