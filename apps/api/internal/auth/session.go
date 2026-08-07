package auth

import (
	"context"
	"crypto/subtle"
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
	// sessKeyHandshakes holds the in-flight authorization-code handshakes as
	// JSON. It replaces the oauth_state/nonce/verifier/provider quartet: a
	// quartet can hold exactly one handshake, so STARTING a second one silently
	// destroyed the first, and any request that reached a callback popped
	// whatever was there. Both are the same denial of service, reachable by a
	// cross-site top-level navigation, which SameSite=Lax still sends the
	// session cookie on. See putHandshake and takeHandshake.
	//
	// Sessions written before this change carry the old keys. They are never
	// read again, so a handshake started within seconds of a deploy fails once
	// with "that sign-in link expired, please try again", which is the truth and
	// resolves itself on the retry.
	sessKeyHandshakes = "oauth_handshakes"
)

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

// oauthHandshake is one in-flight authorization-code exchange.
//
// Provider is the social provider key, or "" for the operator-configured OIDC
// issuer. It is server-side state on purpose: taking it from the callback URL
// instead would let anyone who can reach a callback nominate which adapter
// verifies their code.
type oauthHandshake struct {
	Provider string `json:"p"`
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
}

// maxInFlightHandshakes bounds what one session may accumulate. More than one
// because a person can legitimately have two open: they click Google, leave the
// consent screen sitting in that tab, come back and click GitHub. Three because
// beyond that it is a loop, not a person, and each entry is store space bought
// by an unauthenticated GET. The oldest is dropped, so the newest start always
// works.
const maxInFlightHandshakes = 3

// socialHandshakeTTL is how long an ABANDONED social handshake keeps its
// session record alive. See putSocial.
const socialHandshakeTTL = 15 * time.Minute

// handshakes reads the in-flight set. Unreadable JSON is treated as an empty
// set: the only cost is a sign-in that has to be started again, and the
// alternative is a session no one can ever use.
func (m *SessionManager) handshakes(ctx context.Context) []oauthHandshake {
	raw := m.scs.GetString(ctx, sessKeyHandshakes)
	if raw == "" {
		return nil
	}
	var hs []oauthHandshake
	if err := json.Unmarshal([]byte(raw), &hs); err != nil {
		return nil
	}
	return hs
}

// putHandshake records a new handshake WITHOUT destroying the ones already in
// flight, which is the whole reason this is a list.
func (m *SessionManager) putHandshake(ctx context.Context, h oauthHandshake) {
	hs := append([]oauthHandshake{h}, m.handshakes(ctx)...)
	if len(hs) > maxInFlightHandshakes {
		hs = hs[:maxInFlightHandshakes]
	}
	m.storeHandshakes(ctx, hs)
}

func (m *SessionManager) storeHandshakes(ctx context.Context, hs []oauthHandshake) {
	if len(hs) == 0 {
		m.scs.Remove(ctx, sessKeyHandshakes)
		return
	}
	b, err := json.Marshal(hs)
	if err != nil {
		// Only reachable if the struct stops being serialisable, which is a
		// compile-time-ish fact; dropping the set beats writing a corrupt one.
		m.scs.Remove(ctx, sessKeyHandshakes)
		return
	}
	m.scs.Put(ctx, sessKeyHandshakes, string(b))
}

// takeHandshake consumes the ONE handshake this callback is for, and only that
// one.
//
// CONSUMING BEFORE CHECKING WAS A WAY TO KILL SOMEBODY ELSE'S SIGN-IN. The old
// code popped the handshake and then asked whether it belonged to this
// callback, so any request that reached any callback emptied the session: a
// stale link, the other provider's callback, or a cross-site top-level
// navigation, which SameSite=Lax still attaches the cookie to. The real
// callback then arrived to find nothing.
//
// A state that does not match is equally not-this-handshake, so it is left
// alone too. The state is a one-time CSRF binder, not a credential: leaving it
// in place cannot authorise anything, while burning it hands anyone who can
// cause a navigation a denial of service against an in-flight sign-in.
func (m *SessionManager) takeHandshake(ctx context.Context, provider, state string) (oauthHandshake, bool) {
	if state == "" {
		return oauthHandshake{}, false
	}
	hs := m.handshakes(ctx)
	for i, h := range hs {
		if h.Provider != provider {
			continue
		}
		// Constant time because the stored state is a secret the caller is
		// trying to present, and a byte-by-byte compare is the shape that leaks
		// it.
		if subtle.ConstantTimeCompare([]byte(h.State), []byte(state)) != 1 {
			continue
		}
		m.storeHandshakes(ctx, append(hs[:i:i], hs[i+1:]...))
		return h, true
	}
	return oauthHandshake{}, false
}

// putOAuth stores a handshake for the operator-configured OIDC issuer.
func (m *SessionManager) putOAuth(ctx context.Context, state, nonce, verifier string) {
	m.putHandshake(ctx, oauthHandshake{State: state, Nonce: nonce, Verifier: verifier})
}

// putSocial stores a handshake for a consumer provider.
func (m *SessionManager) putSocial(ctx context.Context, provider, state, nonce, verifier string) {
	m.putHandshake(ctx, oauthHandshake{Provider: provider, State: state, Nonce: nonce, Verifier: verifier})

	// AN UNAUTHENTICATED GET MUST NOT MINT A LONG-LIVED SESSION. /start is
	// public, so every crawler, scanner and retry loop that touches it writes a
	// session record, and each one inherited the full signed-in lifetime: at the
	// default idle timeout that is a week of store space bought with one
	// anonymous request.
	//
	// A handshake is worth minutes, so an abandoned one expires in minutes.
	// Completing the sign-in calls RenewToken, which resets the deadline to the
	// configured lifetime, so nobody who actually signs in is affected.
	//
	// The signed-in case is left alone. There is no connect-another-provider
	// flow in the product today, so this is not protecting a feature: it is
	// making sure a rule written for anonymous traffic can never truncate a
	// session that a real sign-in already paid for, whether that request comes
	// from a signed-in visitor who navigated to /start by hand or from a linking
	// flow added later.
	if _, _, signedIn := m.Current(ctx); !signedIn {
		m.scs.SetDeadline(ctx, time.Now().Add(socialHandshakeTTL).UTC())
	}
}

// takeSocialFor consumes the handshake for this provider and state, if there is
// one. See takeHandshake for why it will not touch anything else.
func (m *SessionManager) takeSocialFor(ctx context.Context, provider, state string) (nonce, verifier string, ok bool) {
	h, ok := m.takeHandshake(ctx, provider, state)
	return h.Nonce, h.Verifier, ok
}

// takeOAuthFor is the same for the operator-configured OIDC issuer.
func (m *SessionManager) takeOAuthFor(ctx context.Context, state string) (nonce, verifier string, ok bool) {
	h, ok := m.takeHandshake(ctx, "", state)
	return h.Nonce, h.Verifier, ok
}
