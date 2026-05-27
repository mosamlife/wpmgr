package agent

import (
	"bytes"
	"context"
	"io"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// maxAgentBody bounds the request body the agent-auth middleware will buffer to
// hash for signature verification (also the practical cap on agent payloads).
const maxAgentBody = 4 << 20 // 4 MiB

// Identity is the verified agent identity resolved from a signed request: the
// site it represents and that site's tenant.
type Identity struct {
	SiteID   uuid.UUID
	TenantID uuid.UUID
}

type identityCtxKey struct{}

// WithIdentity stashes a verified agent identity on the context.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityCtxKey{}, id)
}

// IdentityFromContext returns the verified agent identity, if present.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityCtxKey{}).(Identity)
	return id, ok
}

// SiteResolver resolves an enrolled site by its agent public key and records
// anti-replay nonces. It is satisfied by the site domain (wired in main),
// keeping the agent package free of a site import (no cycle).
type SiteResolver interface {
	// ResolveByAgentKey returns the site id + tenant id for the given base64
	// Ed25519 public key, or an unauthorized domain error if unknown.
	ResolveByAgentKey(ctx context.Context, agentPublicKey string) (Identity, error)
	// RecordNonce persists a (site, nonce) pair, returning false if the nonce was
	// already seen (a replay).
	RecordNonce(ctx context.Context, siteID uuid.UUID, nonce string) (bool, error)
}

// Authenticator verifies the Ed25519 signed-request scheme for agent->CP calls.
type Authenticator struct {
	resolver SiteResolver
	clock    domain.Clock
	skew     time.Duration
}

// NewAuthenticator builds an agent Authenticator. skew bounds the timestamp
// freshness window (anti-replay together with the nonce).
func NewAuthenticator(resolver SiteResolver, clock domain.Clock, skew time.Duration) *Authenticator {
	if skew <= 0 {
		skew = 5 * time.Minute
	}
	return &Authenticator{resolver: resolver, clock: clock, skew: skew}
}

// Authenticate is Gin middleware that authenticates the agent by verifying the
// request signature against the site's stored public key, enforces the
// timestamp window and nonce single-use, then attaches the verified Identity to
// the request context. It aborts 401 on any failure. The site/tenant are
// resolved from the verified key — NEVER from a client-supplied tenant header.
func (a *Authenticator) Authenticate() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()

		keyB64 := c.GetHeader(HeaderAgentKey)
		tsStr := c.GetHeader(HeaderTimestamp)
		nonce := c.GetHeader(HeaderNonce)
		sigB64 := c.GetHeader(HeaderSignature)
		if keyB64 == "" || tsStr == "" || nonce == "" || sigB64 == "" {
			a.fail(c, "agent_unauthenticated", "missing agent signature headers")
			return
		}
		if len(nonce) < 8 || len(nonce) > 256 {
			a.fail(c, "agent_unauthenticated", "invalid nonce")
			return
		}

		// Timestamp freshness (bounds the replay window).
		ts, err := ParseUnixSeconds(tsStr)
		if err != nil {
			a.fail(c, "agent_unauthenticated", "invalid timestamp")
			return
		}
		now := a.clock.Now()
		delta := now.Sub(time.Unix(ts, 0))
		if delta < 0 {
			delta = -delta
		}
		if delta > a.skew {
			a.fail(c, "agent_unauthenticated", "request timestamp outside allowed window")
			return
		}

		// Buffer the body to hash it, then restore it for the handler.
		body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxAgentBody))
		if err != nil {
			a.fail(c, "agent_unauthenticated", "failed to read request body")
			return
		}
		_ = c.Request.Body.Close()
		c.Request.Body = io.NopCloser(bytes.NewReader(body))

		// Verify the signature against the presented public key.
		if !VerifySignature(keyB64, sigB64, c.Request.Method, c.Request.URL.Path, tsStr, nonce, body) {
			a.fail(c, "agent_unauthenticated", "invalid signature")
			return
		}

		// Resolve the site/tenant from the (now cryptographically proven) key.
		id, err := a.resolver.ResolveByAgentKey(ctx, keyB64)
		if err != nil {
			httpx.Error(c, err)
			c.Abort()
			return
		}

		// Anti-replay: the nonce must be unseen for this site.
		fresh, err := a.resolver.RecordNonce(ctx, id.SiteID, nonce)
		if err != nil {
			httpx.Error(c, err)
			c.Abort()
			return
		}
		if !fresh {
			a.fail(c, "agent_replay", "request nonce has already been used")
			return
		}

		c.Request = c.Request.WithContext(WithIdentity(ctx, id))
		c.Next()
	}
}

func (a *Authenticator) fail(c *gin.Context, code, msg string) {
	httpx.Error(c, domain.Unauthorized(code, msg))
	c.Abort()
}
