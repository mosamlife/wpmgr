package auth

import (
	"context"
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
	// sessKeyOAuthState/Nonce/Verifier hold the transient OIDC handshake values.
	sessKeyOAuthState    = "oauth_state"
	sessKeyOAuthNonce    = "oauth_nonce"
	sessKeyOAuthVerifier = "oauth_verifier"
)

// SessionManager wraps SCS with the WPMgr cookie policy. The opaque session
// cookie is HttpOnly + SameSite=Lax, Secure in production, with idle and
// absolute lifetimes. The backing store is Redis (ADR: scs/redisstore); the
// caller may pass a pgxstore-style store as a fallback.
type SessionManager struct {
	scs *scs.SessionManager
}

// NewRedisPool builds a redigo connection pool for the session store.
func NewRedisPool(addr, password string) *redis.Pool {
	return &redis.Pool{
		MaxIdle: 10,
		Dial: func() (redis.Conn, error) {
			opts := []redis.DialOption{}
			if password != "" {
				opts = append(opts, redis.DialPassword(password))
			}
			return redis.Dial("tcp", addr, opts...)
		},
		TestOnBorrow: func(c redis.Conn, _ time.Time) error {
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
	return nil
}

// SetActiveTenant updates the active tenant on the current session.
func (m *SessionManager) SetActiveTenant(ctx context.Context, tenantID uuid.UUID) {
	m.scs.Put(ctx, sessKeyActiveTenantID, tenantID.String())
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
}

// takeOAuth reads and clears the transient OIDC handshake values.
func (m *SessionManager) takeOAuth(ctx context.Context) (state, nonce, verifier string) {
	state = m.scs.PopString(ctx, sessKeyOAuthState)
	nonce = m.scs.PopString(ctx, sessKeyOAuthNonce)
	verifier = m.scs.PopString(ctx, sessKeyOAuthVerifier)
	return
}
