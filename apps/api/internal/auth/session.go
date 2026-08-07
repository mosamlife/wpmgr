package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/alexedwards/scs/redisstore"
	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"
	"github.com/gomodule/redigo/redis"
	"github.com/google/uuid"
)

// Session GUC/key names stored in the SCS session.
const (
	sessKeyUserID         = "user_id"
	sessKeyActiveTenantID = "active_tenant_id"
	// sessKeyAuthAt holds the RFC3339 timestamp the session authenticated. The
	// Authenticator rejects sessions whose auth_at predates the user's
	// password_changed_at (ADR-045 Phase 2 session invalidation).
	sessKeyAuthAt = "auth_at"
	// sessKeyOAuthState/Nonce/Verifier hold the transient OIDC handshake values.
	sessKeyOAuthState    = "oauth_state"
	sessKeyOAuthNonce    = "oauth_nonce"
	sessKeyOAuthVerifier = "oauth_verifier"
	// Which provider the in-flight handshake belongs to. Without this the
	// callback cannot know whether to verify a Google ID token or call the
	// GitHub API, and a single shared callback would have to guess.
	sessKeyOAuthProvider = "oauth_provider"
	// sessKeyPendingSocialLink parks an approved-but-unwritten identity link
	// across the two-factor round trip. See putPendingSocialLink.
	sessKeyPendingSocialLink = "pending_social_link"
	// Where the browser was heading when it started the handshake, so a shared
	// deep link survives a provider round trip. Held here rather than round
	// tripped through the provider (state, or a query parameter on the callback)
	// because the callback would then have to trust a value an attacker controls,
	// and the whole point of a redirect target is that we send the browser there.
	sessKeyOAuthReturn = "oauth_return"
)

// pendingSocialLinkTTL bounds how long an approved link may sit unwritten. It
// covers one 2FA prompt, nothing more: a link nobody finished authenticating
// for should expire, not wait indefinitely for the account's next login.
const pendingSocialLinkTTL = 10 * time.Minute

// SessionManager wraps SCS with the WPMgr cookie policy. The opaque session
// cookie is HttpOnly + SameSite=Lax, Secure in production, with idle and
// absolute lifetimes. The backing store is Redis (ADR: scs/redisstore); the
// caller may pass a pgxstore-style store as a fallback.
type SessionManager struct {
	scs *scs.SessionManager
}

// NewRedisPool builds a redigo connection pool for the session store.
//
// Idle/timeout tuning is load-bearing for dashboard latency: Memorystore (and
// the network path) silently drops idle TCP connections, but redigo keeps them
// cached. Without IdleTimeout the first request after the dashboard sits idle
// borrows a dead connection, and TestOnBorrow's PING then blocks on the dead
// socket until the OS TCP retransmit timeout (~7-8s) before the pool redials —
// surfacing as a one-off ~8s stall on the first request after idle while every
// subsequent request is fast. The fixes:
//   - IdleTimeout (4m, below the network/Memorystore idle drop) so the pool
//     proactively closes idle connections before they go stale and are never
//     borrowed dead.
//   - MaxConnLifetime so connections rotate rather than accumulate.
//   - Dial connect/read/write timeouts so a borrowed-dead PING (or a slow dial)
//     fails in seconds, not on the multi-second OS TCP timeout.
//   - TestOnBorrow skips the PING for recently-used connections (cheap path).
func NewRedisPool(addr, password string) *redis.Pool {
	return &redis.Pool{
		MaxIdle:         10,
		MaxActive:       64,
		IdleTimeout:     4 * time.Minute,
		MaxConnLifetime: 30 * time.Minute,
		Wait:            true,
		Dial: func() (redis.Conn, error) {
			opts := []redis.DialOption{
				redis.DialConnectTimeout(5 * time.Second),
				redis.DialReadTimeout(3 * time.Second),
				redis.DialWriteTimeout(3 * time.Second),
			}
			if password != "" {
				opts = append(opts, redis.DialPassword(password))
			}
			return redis.Dial("tcp", addr, opts...)
		},
		TestOnBorrow: func(c redis.Conn, lastUsed time.Time) error {
			// Recently-exercised connections are trusted without a round-trip;
			// only idle-ish ones get a PING, which is now bounded by the dial
			// read timeout so it can never hang on a dead socket.
			if time.Since(lastUsed) < time.Minute {
				return nil
			}
			_, err := c.Do("PING")
			return err
		},
	}
}

// NewRedisSessionManager builds a SessionManager backed by Redis.
func NewRedisSessionManager(pool *redis.Pool, idle, absolute time.Duration, secure bool) *SessionManager {
	m := scs.New()
	m.Store = redisstore.New(pool)
	m.IdleTimeout = idle
	m.Lifetime = absolute
	m.Cookie.Name = "wpmgr_session"
	m.Cookie.HttpOnly = true
	m.Cookie.SameSite = http.SameSiteLaxMode
	m.Cookie.Secure = secure
	m.Cookie.Path = "/"
	return &SessionManager{scs: m}
}

// SCS exposes the underlying manager (for tests / advanced wiring).
func (m *SessionManager) SCS() *scs.SessionManager { return m.scs }

// NewSessionManagerWithStore builds a SessionManager around a pre-built SCS
// manager (e.g. with an in-memory store). Used in tests so they don't require
// a live Redis. The cookie policy still applies.
func NewSessionManagerWithStore(scsManager *scs.SessionManager, secure bool) *SessionManager {
	scsManager.Cookie.Name = "wpmgr_session"
	scsManager.Cookie.HttpOnly = true
	scsManager.Cookie.SameSite = http.SameSiteLaxMode
	scsManager.Cookie.Secure = secure
	scsManager.Cookie.Path = "/"
	return &SessionManager{scs: scsManager}
}

// LoadAndSave returns Gin middleware that loads the session for each request
// and commits it afterwards. Rather than wrapping the ResponseWriter (which
// fights Gin's own writer), it uses SCS's lower-level Load + Commit primitives
// and writes the session cookie via a response hook fired just before the first
// byte is written, so the Set-Cookie header lands on every response (including
// streamed/aborted ones).
func (m *SessionManager) LoadAndSave() gin.HandlerFunc {
	return func(c *gin.Context) {
		var token string
		cookie, err := c.Request.Cookie(m.scs.Cookie.Name)
		if err == nil {
			token = cookie.Value
		}

		ctx, err := m.scs.Load(c.Request.Context(), token)
		if err != nil {
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		c.Request = c.Request.WithContext(ctx)

		// Commit + emit the cookie exactly once, just before the response is
		// written. Gin's BeforeWrite hook fires on the first Write/WriteHeader.
		committed := false
		commit := func() {
			if committed {
				return
			}
			committed = true
			m.writeSessionCookie(ctx, c)
		}
		c.Writer = &commitWriter{ResponseWriter: c.Writer, commit: commit}

		c.Next()

		// Ensure commit happens even if nothing was written (e.g. 204).
		commit()
	}
}

// writeSessionCookie persists the session and sets/clears the cookie header
// according to the session's current status.
func (m *SessionManager) writeSessionCookie(ctx context.Context, c *gin.Context) {
	switch m.scs.Status(ctx) {
	case scs.Modified:
		tok, expiry, err := m.scs.Commit(ctx)
		if err != nil {
			return
		}
		m.setCookie(c, tok, expiry)
	case scs.Destroyed:
		m.setCookie(c, "", time.Unix(1, 0))
	}
}

func (m *SessionManager) setCookie(c *gin.Context, token string, expiry time.Time) {
	ck := m.scs.Cookie
	cookie := &http.Cookie{
		Name:     ck.Name,
		Value:    token,
		Path:     ck.Path,
		Domain:   ck.Domain,
		HttpOnly: ck.HttpOnly,
		Secure:   ck.Secure,
		SameSite: ck.SameSite,
	}
	if token == "" {
		cookie.Expires = time.Unix(1, 0)
		cookie.MaxAge = -1
	} else if !expiry.IsZero() {
		cookie.Expires = expiry.UTC()
		cookie.MaxAge = int(time.Until(expiry).Seconds())
	}
	http.SetCookie(c.Writer, cookie)
}

// commitWriter fires the session-commit hook on the first response write so the
// Set-Cookie header is in place before the status/body are flushed.
type commitWriter struct {
	gin.ResponseWriter
	commit func()
}

func (w *commitWriter) WriteHeader(code int) {
	w.commit()
	w.ResponseWriter.WriteHeader(code)
}

func (w *commitWriter) Write(b []byte) (int, error) {
	w.commit()
	return w.ResponseWriter.Write(b)
}

func (w *commitWriter) WriteString(s string) (int, error) {
	w.commit()
	return w.ResponseWriter.WriteString(s)
}

// Login establishes an authenticated session for the user with the chosen
// active tenant. It renews the session token to prevent fixation.
func (m *SessionManager) Login(ctx context.Context, userID, activeTenant uuid.UUID) error {
	if err := m.scs.RenewToken(ctx); err != nil {
		return err
	}
	m.scs.Put(ctx, sessKeyUserID, userID.String())
	m.scs.Put(ctx, sessKeyActiveTenantID, activeTenant.String())
	m.scs.Put(ctx, sessKeyAuthAt, time.Now().UTC().Format(time.RFC3339))
	return nil
}

// SetActiveTenant updates the active tenant on the current session.
func (m *SessionManager) SetActiveTenant(ctx context.Context, tenantID uuid.UUID) {
	m.scs.Put(ctx, sessKeyActiveTenantID, tenantID.String())
}

// AuthAt returns the session's authentication timestamp (zero time when absent,
// e.g. sessions created before this field existed). The Authenticator compares
// it against the user's password_changed_at to reject stale sessions.
func (m *SessionManager) AuthAt(ctx context.Context) time.Time {
	s := m.scs.GetString(ctx, sessKeyAuthAt)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// RefreshAuthAt re-stamps the current session's auth time to now. Called after a
// successful change-password so the acting user's own session survives the
// password_changed_at invalidation that logs out their other sessions.
func (m *SessionManager) RefreshAuthAt(ctx context.Context) {
	m.scs.Put(ctx, sessKeyAuthAt, time.Now().UTC().Format(time.RFC3339))
}

// Destroy logs the user out by discarding the session.
func (m *SessionManager) Destroy(ctx context.Context) error {
	return m.scs.Destroy(ctx)
}

// Current returns the session's user and active tenant, if any.
func (m *SessionManager) Current(ctx context.Context) (userID, activeTenant uuid.UUID, ok bool) {
	uidStr := m.scs.GetString(ctx, sessKeyUserID)
	if uidStr == "" {
		return uuid.Nil, uuid.Nil, false
	}
	uid, err := uuid.Parse(uidStr)
	if err != nil {
		return uuid.Nil, uuid.Nil, false
	}
	tid, _ := uuid.Parse(m.scs.GetString(ctx, sessKeyActiveTenantID))
	return uid, tid, true
}

// putOAuth stores the transient OIDC handshake values on the session.
func (m *SessionManager) putOAuth(ctx context.Context, state, nonce, verifier string) {
	m.scs.Put(ctx, sessKeyOAuthState, state)
	m.scs.Put(ctx, sessKeyOAuthNonce, nonce)
	m.scs.Put(ctx, sessKeyOAuthVerifier, verifier)
	// Clear any provider or return path left by an abandoned social handshake.
	// The two flows share this state, so without it a generic-OIDC handshake
	// started after a social one would carry the social values forward.
	m.scs.Remove(ctx, sessKeyOAuthProvider)
	m.scs.Remove(ctx, sessKeyOAuthReturn)
	// A new handshake supersedes any link approved by an abandoned one. Without
	// this, a link the person walked away from mid two-factor would still be
	// sitting there waiting for their next challenge to apply it.
	m.scs.Remove(ctx, sessKeyPendingSocialLink)
}

// putSocial stores the same handshake values plus the provider the flow was
// started for and where to land afterwards. Both are server-side state on
// purpose: taking the provider from the callback URL would let anyone who can
// reach the callback nominate which adapter verifies their code, and taking the
// return path from there would turn the callback into an open redirect.
//
// returnTo must already be a validated same-origin path (see safeReturnPath);
// an empty string means "land wherever the callback defaults to".
func (m *SessionManager) putSocial(ctx context.Context, provider, state, nonce, verifier, returnTo string) {
	m.putOAuth(ctx, state, nonce, verifier)
	m.scs.Put(ctx, sessKeyOAuthProvider, provider)
	if returnTo != "" {
		m.scs.Put(ctx, sessKeyOAuthReturn, returnTo)
	}
}

// takeSocial reads and clears the handshake, including the provider and the
// return path.
func (m *SessionManager) takeSocial(ctx context.Context) (provider, state, nonce, verifier, returnTo string) {
	provider = m.scs.PopString(ctx, sessKeyOAuthProvider)
	returnTo = m.scs.PopString(ctx, sessKeyOAuthReturn)
	state, nonce, verifier = m.takeOAuth(ctx)
	return
}

// pendingSocialLinkEnvelope is what gets parked.
//
// Three things travel with the identity, and each one refuses a different way
// of applying it to the wrong login. The user id refuses a different account.
// The challenge id refuses a different challenge for the SAME account. The
// absolute expiry refuses a much later one.
type pendingSocialLinkEnvelope struct {
	UserID        string `json:"user_id"`
	ChallengeID   string `json:"challenge_id"`
	Provider      string `json:"provider"`
	Subject       string `json:"subject"`
	Issuer        string `json:"issuer"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	ExpiresAt     int64  `json:"expires_at"`
}

// putPendingSocialLink parks an identity link that the sign-in policy approved
// but that must not be written until a second factor is proven.
//
// The session is the right place for it precisely because it is server-side:
// the browser holds an opaque token and cannot read, forge or move the parked
// link, and the value never appears in a URL or a form the person completing
// the challenge could edit. It is not a credential on its own either. Applying
// it still requires completing the challenge it names, as the user it names.
//
// challengeID IS THE BINDING THAT MATTERS, and the user id is not a substitute
// for it. A browser can hold several live challenges for one account: opening
// the sign-in page again issues another, and password login and a provider
// callback both issue their own. Binding to the user alone would let ANY of
// them apply this link, so a person who abandoned the provider flow, went back
// to their password, and completed that challenge instead would silently gain a
// provider binding from a handshake they walked away from. The link is applied
// only by the exact challenge the handshake that approved it produced.
func (m *SessionManager) putPendingSocialLink(ctx context.Context, userID, challengeID uuid.UUID, ident Identity) {
	if userID == uuid.Nil || challengeID == uuid.Nil {
		return
	}
	blob, err := json.Marshal(pendingSocialLinkEnvelope{
		UserID:        userID.String(),
		ChallengeID:   challengeID.String(),
		Provider:      ident.Provider,
		Subject:       ident.Subject,
		Issuer:        ident.Issuer,
		Email:         ident.Email,
		EmailVerified: ident.EmailVerified,
		ExpiresAt:     time.Now().UTC().Add(pendingSocialLinkTTL).Unix(),
	})
	if err != nil {
		return
	}
	m.scs.Put(ctx, sessKeyPendingSocialLink, string(blob))
}

// takePendingSocialLink pops a parked link for userID, completing challengeID.
// It returns ok=false, and still clears the slot, when nothing is parked, when
// the park has expired, when it names a different account, or when it names a
// different challenge: a link approved by one handshake must be applied by that
// handshake's challenge or by none.
//
// Clearing on refusal is deliberate. A link that survived a mismatch would sit
// there waiting for a challenge that does match, which is exactly the reuse the
// mismatch check exists to stop.
func (m *SessionManager) takePendingSocialLink(ctx context.Context, userID, challengeID uuid.UUID) (Identity, bool) {
	raw := m.scs.PopString(ctx, sessKeyPendingSocialLink)
	if raw == "" {
		return Identity{}, false
	}
	var env pendingSocialLinkEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return Identity{}, false
	}
	if env.ExpiresAt <= time.Now().UTC().Unix() {
		return Identity{}, false
	}
	parked, err := uuid.Parse(env.UserID)
	if err != nil || parked != userID || parked == uuid.Nil {
		return Identity{}, false
	}
	parkedChallenge, err := uuid.Parse(env.ChallengeID)
	if err != nil || parkedChallenge != challengeID || parkedChallenge == uuid.Nil {
		return Identity{}, false
	}
	return Identity{
		UserID: parked, Provider: env.Provider, Subject: env.Subject,
		Issuer: env.Issuer, Email: env.Email, EmailVerified: env.EmailVerified,
	}, true
}

// takeOAuth reads and clears the transient OIDC handshake values.
func (m *SessionManager) takeOAuth(ctx context.Context) (state, nonce, verifier string) {
	state = m.scs.PopString(ctx, sessKeyOAuthState)
	nonce = m.scs.PopString(ctx, sessKeyOAuthNonce)
	verifier = m.scs.PopString(ctx, sessKeyOAuthVerifier)
	return
}
