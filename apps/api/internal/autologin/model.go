// Package autologin implements the Phase 5.5 One-Click Login feature
// (ADR-030/031). An operator with the site:autologin permission mints a
// single-use, short-TTL nonce; the control plane stores it in Postgres (source
// of truth) AND Redis (sub-millisecond hot path), bakes the nonce id into an
// Ed25519 JWT (cmd="autologin", aud=<site UUID>, tgt=<wp user login>), and
// returns a redirect URL of the form
//
//	{site.url}/wp-json/wpmgr/v1/autologin?token=<jwt>&redirect_to=<urlencoded>
//
// The WordPress agent verifies the JWT, calls back the control plane's
// /agent/v1/autologin/consume endpoint to atomically consume the nonce
// (GETDEL on Redis, falling back to a single UPDATE...RETURNING on PG), and
// then establishes a wp-admin session as the target user.
//
// SECURITY POSTURE
//
//   - The JWT is the only thing the operator's browser ever sees; the nonce id
//     is identical to the JWT jti, so the audit trail keys on a non-secret
//     value. The minted JWT is NEVER recorded in the audit log.
//   - The mint runs in app.tenant_id RLS; the consume runs cross-tenant under
//     app.agent because the agent's identity (site_id+tenant_id) is verified
//     before any tenant scope exists (mirrors agent_nonces / sites_agent).
//   - Rate-limited per (initiator_user_id, site_id) and per site_id.
//   - The site_id on the consume callback MUST equal the agent's verified
//     site_id; the handler re-asserts this in addition to the SQL filter.
package autologin

import (
	"net/netip"
	"time"

	"github.com/google/uuid"
)

// JWTTTL is the lifetime of a minted autologin JWT/nonce. It is aligned with
// the agent's MAX_FUTURE_EXP window and the Redis EX value so the three time
// budgets agree.
const JWTTTL = 60 * time.Second

// RedisKeyPrefix is the prefix for the per-nonce Redis hot-path key. The full
// key is RedisKeyPrefix + nonce_id (base64url, URL-safe characters only).
const RedisKeyPrefix = "autologin:"

// Token is the result of a successful mint: the nonce id (= JWT jti), the
// minted JWT itself, the redirect URL the operator's browser should visit, and
// the expiry timestamp. The JWT MUST NOT be logged or recorded in the audit
// trail — only the nonce id is.
type Token struct {
	NonceID     string
	JWT         string
	RedirectURL string
	ExpiresAt   time.Time
}

// MintRequest is the validated mint input.
//
//	TargetWPUser  the WordPress login the agent should log in as. An empty
//	              string means "agent picks the first administrator".
//	RedirectTo    where the agent should send the operator's browser after the
//	              session is established. Empty means "wp-admin home". Validated
//	              loosely (only the http/https scheme is checked; the agent will
//	              additionally rewrite to its own admin URL if necessary).
type MintRequest struct {
	TenantID     uuid.UUID
	SiteID       uuid.UUID
	InitiatorID  uuid.UUID
	TargetWPUser string
	RedirectTo   string
	IP           string
	UserAgent    string
	SessionAge   time.Duration // age of the operator's current session (for 2FA step-up check)
}

// ConsumeResult is what the consume handler returns to the agent on success.
//
//	NonceID       the consumed nonce id (= audit correlator).
//	TargetWPUser  the WP login the agent should log in as ("" = first admin).
//	AllowedRoles  the WP roles the agent is permitted to log in as (per policy).
//	AuditID       the audit entry id recorded for the consume.
//	HotPath       which path won the consume race ("redis"|"postgres") — useful
//	              for observability but otherwise meaningless to the agent.
type ConsumeResult struct {
	NonceID      string
	TargetWPUser string
	AllowedRoles []string
	AuditID      uuid.UUID
	HotPath      string
}

// HotPathRedis / HotPathPostgres are the recorded HotPath values.
const (
	HotPathRedis    = "redis"
	HotPathPostgres = "postgres"
)

// Policy mirrors the autologin_policies row in domain types.
type Policy struct {
	SiteID               uuid.UUID
	TenantID             uuid.UUID
	Enabled              bool
	AllowedWPRoles       []string
	Require2FAStepUp     bool
	MaxSessionAgeMinutes int32
	UpdatedAt            time.Time
}

// DefaultAllowedWPRoles is the policy default when no policy row exists.
var DefaultAllowedWPRoles = []string{"administrator"}

// parseIP converts a string IP into a netip.Addr usable by the inet PG column.
// Empty / unparsable input yields a nil pointer (so the column is NULL).
func parseIP(s string) *netip.Addr {
	if s == "" {
		return nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return nil
	}
	return &a
}
