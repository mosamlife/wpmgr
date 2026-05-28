package autologin

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// SiteLookup is the narrow site-resolution surface the autologin service needs;
// satisfied by an adapter over the site service in main (no site import here).
//
//	GetSiteForAutologin returns the canonical site URL the redirect points at,
//	plus the tenant the site belongs to. A site not in tenantID returns
//	ok=false (the service maps it to 404 site_not_found).
type SiteLookup interface {
	GetSiteForAutologin(ctx context.Context, tenantID, siteID uuid.UUID) (siteURL string, ok bool, err error)
}

// Signer is the subset of *agentcmd.Signer the service needs. Defined as an
// interface (not an import) so tests can inject a fake without touching
// ed25519 keys; the production signer is the same *agentcmd.Signer used for
// M3/M4 commands and is satisfied by it transparently.
type Signer interface {
	MintAutologin(now time.Time, aud, targetWPUser string) (token, jti string, err error)
}

// Recorder is the narrow audit surface used by the service (so tests can pass
// a no-op).
type Recorder interface {
	Record(ctx context.Context, e audit.Event) (audit.Entry, error)
}

// Config tunes the service. Require2FAStepUp is the GLOBAL feature flag (from
// WPMGR_AUTOLOGIN_REQUIRE_2FA_STEP_UP); when false (default), the per-site
// policy column is IGNORED. AutologinPathFormat is the agent's REST path the
// redirect points at (kept configurable for testing, but its production value
// is fixed by the agent's class-router.php route).
type Config struct {
	Require2FAStepUp    bool
	AutologinPathFormat string // default "/wp-json/wpmgr/v1/autologin"
}

// Service implements the Phase 5.5 mint and consume flows.
type Service struct {
	repo     Repo
	store    NonceStore
	signer   Signer
	sites    SiteLookup
	limiter  Limiter
	rec      Recorder
	clock    domain.Clock
	cfg      Config
	pathTmpl string
}

// NewService builds the service. A nil Limiter is replaced by an unlimited
// limiter (NoopLimiter) — production wires the MemoryLimiter.
func NewService(repo Repo, store NonceStore, signer Signer, sites SiteLookup, limiter Limiter, rec Recorder, clock domain.Clock, cfg Config) *Service {
	if limiter == nil {
		limiter = noopLimiter{}
	}
	if store == nil {
		store = NoopStore{}
	}
	if clock == nil {
		clock = domain.SystemClock{}
	}
	if cfg.AutologinPathFormat == "" {
		cfg.AutologinPathFormat = "/wp-json/wpmgr/v1/autologin"
	}
	return &Service{
		repo: repo, store: store, signer: signer, sites: sites,
		limiter: limiter, rec: rec, clock: clock, cfg: cfg,
		pathTmpl: cfg.AutologinPathFormat,
	}
}

// noopLimiter is the default when no limiter is wired (tests / disabled paths).
type noopLimiter struct{}

func (noopLimiter) Allow(context.Context, string, int) (bool, time.Duration) { return true, 0 }

// Mint executes the operator-facing mint flow:
//  1. resolve the site within the tenant (404 on miss),
//  2. ensure the per-site policy exists (auto-create defaults on first read),
//  3. enforce policy.enabled and the 2FA step-up gate (feature-flagged off),
//  4. enforce per-(initiator,site) and per-site rate limits,
//  5. mint a base64url(32B) nonce + EdDSA JWT bound to (siteID, "autologin"),
//  6. persist {PG row, Redis hot-path key} both with EX=JWTTTL,
//  7. record audit.autologin.requested (nonce id only — never the JWT),
//  8. return the redirect URL the operator's browser should follow.
//
// Any failure path records audit.autologin.failed before returning.
func (s *Service) Mint(ctx context.Context, req MintRequest) (Token, error) {
	if req.TenantID == uuid.Nil || req.InitiatorID == uuid.Nil || req.SiteID == uuid.Nil {
		return Token{}, s.failMint(ctx, req, "", domain.Forbidden("tenant_required", "tenant, site, and initiator are required"))
	}
	siteURL, ok, err := s.sites.GetSiteForAutologin(ctx, req.TenantID, req.SiteID)
	if err != nil {
		return Token{}, s.failMint(ctx, req, "", err)
	}
	if !ok {
		return Token{}, s.failMint(ctx, req, "", domain.NotFound("site_not_found", "site not found"))
	}
	if siteURL == "" {
		return Token{}, s.failMint(ctx, req, "", domain.Conflict("site_url_missing", "site has no URL for the autologin redirect"))
	}

	policy, err := s.repo.GetOrCreatePolicy(ctx, req.TenantID, req.SiteID)
	if err != nil {
		return Token{}, s.failMint(ctx, req, "", err)
	}
	if !policy.Enabled {
		return Token{}, s.failMint(ctx, req, "", domain.Forbidden("policy_disabled", "autologin is disabled for this site by policy"))
	}
	if s.cfg.Require2FAStepUp && policy.Require2FAStepUp {
		// V0 reaches here only if the global feature flag AND the per-site policy
		// are both true AND a future 2FA system rejects the operator's session
		// age. 2FA enrollment is NOT BUILT today, so the check below is the
		// minimum gate — it returns 409 once the conditions ever align.
		if policy.MaxSessionAgeMinutes > 0 && req.SessionAge > time.Duration(policy.MaxSessionAgeMinutes)*time.Minute {
			return Token{}, s.failMint(ctx, req, "", domain.Conflict("2fa_required", "a recent two-factor authentication step-up is required"))
		}
	}

	// Rate limit: per (initiator, site) first (tighter cap), then per site.
	initiatorKey := req.InitiatorID.String() + "|" + req.SiteID.String()
	if allowed, retry := s.limiter.Allow(ctx, "init:"+initiatorKey, LimitInitiatorSitePerMin); !allowed {
		return Token{}, s.failMint(ctx, req, "", rateLimited(retry))
	}
	if allowed, retry := s.limiter.Allow(ctx, "site:"+req.SiteID.String(), LimitSitePerMin); !allowed {
		return Token{}, s.failMint(ctx, req, "", rateLimited(retry))
	}

	now := s.clock.Now().UTC()
	expiresAt := now.Add(JWTTTL)

	jwtToken, nonceID, err := s.signer.MintAutologin(now, req.SiteID.String(), req.TargetWPUser)
	if err != nil {
		return Token{}, s.failMint(ctx, req, "", domain.Internal("autologin_mint_failed", "failed to mint autologin token").WithCause(err))
	}

	// PG row first (durable). Then Redis (best-effort hot path; a Redis failure
	// is logged via audit but does not abort — the consume path will fall back
	// to PG and the operator's URL still works).
	if err := s.repo.InsertToken(ctx, InsertTokenInput{
		NonceID:            nonceID,
		TenantID:           req.TenantID,
		SiteID:             req.SiteID,
		InitiatorUserID:    req.InitiatorID,
		TargetWPUserLogin:  req.TargetWPUser,
		InitiatorIP:        req.IP,
		InitiatorUserAgent: truncate(req.UserAgent, 512),
		ExpiresAt:          expiresAt,
	}); err != nil {
		return Token{}, s.failMint(ctx, req, nonceID, err)
	}
	_ = s.store.Set(ctx, nonceID, RedisPayload{
		TenantID:          req.TenantID,
		SiteID:            req.SiteID,
		TargetWPUserLogin: req.TargetWPUser,
	}, JWTTTL)

	redirectURL, err := buildRedirectURL(siteURL, s.pathTmpl, jwtToken, req.RedirectTo)
	if err != nil {
		return Token{}, s.failMint(ctx, req, nonceID, domain.Internal("autologin_redirect_failed", "failed to build autologin redirect URL").WithCause(err))
	}

	if s.rec != nil {
		_, _ = s.rec.Record(ctx, audit.Event{
			TenantID:   req.TenantID,
			ActorType:  audit.ActorUser,
			ActorID:    req.InitiatorID.String(),
			Action:     audit.ActionAutologinRequested,
			TargetType: "site",
			TargetID:   req.SiteID.String(),
			Metadata: map[string]any{
				"nonce_id":             nonceID,
				"site_id":              req.SiteID.String(),
				"target_wp_user_login": req.TargetWPUser,
				"initiator_ip":         req.IP,
				"initiator_user_agent": truncate(req.UserAgent, 256),
				"expires_at":           expiresAt.Format(time.RFC3339),
			},
		})
	}

	return Token{
		NonceID:     nonceID,
		JWT:         jwtToken,
		RedirectURL: redirectURL,
		ExpiresAt:   expiresAt,
	}, nil
}

// Consume executes the agent-facing consume callback. agentSiteID is the
// site_id derived from the agent's verified Ed25519 identity (NOT a header).
// claimedSiteID is the site_id the agent POSTed in the body; the two MUST
// match or the consume is rejected as a possible cross-site replay attempt.
//
// The atomic-once invariant is enforced first via Redis GETDEL (sub-ms; only
// one caller sees the value), then via a single PG UPDATE ... RETURNING that
// also wins exactly once across concurrent agents. Whichever path produced the
// row also drives the PG mark-consumed write (idempotent) so the durable
// audit trail is complete.
func (s *Service) Consume(ctx context.Context, agentSiteID, claimedSiteID uuid.UUID, nonceID, consumedFromIP string) (ConsumeResult, error) {
	if nonceID == "" {
		return ConsumeResult{}, s.failConsume(ctx, uuid.Nil, agentSiteID, "", domain.Validation("nonce_required", "nonce is required"))
	}
	if claimedSiteID != uuid.Nil && claimedSiteID != agentSiteID {
		// The agent's verified identity overrides any body claim. Reject to
		// surface the mismatch loudly rather than silently using one over the
		// other.
		return ConsumeResult{}, s.failConsume(ctx, uuid.Nil, agentSiteID, nonceID, domain.Forbidden("site_mismatch", "claimed site_id does not match the verified agent identity"))
	}

	// Hot path: Redis GETDEL. The Redis store is atomic-once on its own
	// (exactly one caller across the cluster sees the value), but we still
	// MUST drive the PG atomic UPDATE so that:
	//
	//   (a) the consumed_at column reflects truth (auditability), AND
	//   (b) a concurrent caller that missed Redis cannot also win via the PG
	//       fallback. The PG predicate (consumed_at IS NULL) is the single
	//       global arbiter — Redis is merely an optimisation that pre-fetches
	//       the payload so we don't have to round-trip through PG when the
	//       cluster has Redis available.
	//
	// If the Redis-winning caller's PG UPDATE returns 0 rows, it means another
	// caller raced through the PG fallback and won FIRST. In that case the
	// nonce is already consumed elsewhere; we surface 410 rather than letting
	// two callers see "success" for the same nonce.
	payload, foundInRedis, redisErr := s.store.ConsumeOnce(ctx, nonceID)
	if foundInRedis {
		if payload.SiteID != agentSiteID {
			// Some other site's payload sat under this nonce. Treat as
			// site_mismatch — never confirm the existence of another site's
			// nonce. Note: GETDEL has already deleted the Redis key, so a
			// subsequent legitimate consume by the right site will fall
			// through to the PG path and succeed there. That's the correct
			// behaviour: a single wrong-site attempt does not silently
			// invalidate the nonce.
			return ConsumeResult{}, s.failConsume(ctx, payload.TenantID, agentSiteID, nonceID, domain.Forbidden("site_mismatch", "nonce was minted for a different site"))
		}
		row, foundInPG, err := s.repo.ConsumeToken(ctx, nonceID, agentSiteID, consumedFromIP)
		if err != nil {
			return ConsumeResult{}, s.failConsume(ctx, payload.TenantID, agentSiteID, nonceID, err)
		}
		if !foundInPG {
			// PG arbiter says someone else already consumed it. Honour that.
			return ConsumeResult{}, s.failConsume(ctx, payload.TenantID, agentSiteID, nonceID, gone("nonce_unavailable", "autologin nonce is unknown, already consumed, or expired"))
		}
		return s.finishConsume(ctx, row.TenantID, agentSiteID, nonceID, row.TargetWPUserLogin, consumedFromIP, HotPathRedis)
	}
	// Redis miss / transport error: fall back to PG single-shot consume.
	_ = redisErr // transport errors are intentionally swallowed; PG is authoritative.

	row, foundInPG, err := s.repo.ConsumeToken(ctx, nonceID, agentSiteID, consumedFromIP)
	if err != nil {
		return ConsumeResult{}, s.failConsume(ctx, uuid.Nil, agentSiteID, nonceID, err)
	}
	if !foundInPG {
		// Three possibilities: never existed, already consumed, expired, or the
		// site_id binding mismatched. Return 410 so the agent surfaces "the link
		// already worked / is no longer valid" rather than treating it as a hard
		// error.
		return ConsumeResult{}, s.failConsume(ctx, uuid.Nil, agentSiteID, nonceID, gone("nonce_unavailable", "autologin nonce is unknown, already consumed, or expired"))
	}
	return s.finishConsume(ctx, row.TenantID, agentSiteID, nonceID, row.TargetWPUserLogin, consumedFromIP, HotPathPostgres)
}

func (s *Service) finishConsume(ctx context.Context, tenantID, siteID uuid.UUID, nonceID, target, consumedFromIP, hotPath string) (ConsumeResult, error) {
	// Look up the per-site policy under the agent path. Defaults apply when no
	// row exists (the mint path would normally have auto-created one, but the
	// agent must not depend on that).
	roles := DefaultAllowedWPRoles
	if policy, ok, perr := s.repo.GetPolicyForAgent(ctx, siteID); perr == nil && ok {
		if len(policy.AllowedWPRoles) > 0 {
			roles = policy.AllowedWPRoles
		}
	}
	auditID := uuid.Nil
	if s.rec != nil {
		entry, _ := s.rec.Record(ctx, audit.Event{
			TenantID:   tenantID,
			ActorType:  audit.ActorSystem, // the agent acts on behalf of the system here
			ActorID:    siteID.String(),
			Action:     audit.ActionAutologinConsumed,
			TargetType: "site",
			TargetID:   siteID.String(),
			Metadata: map[string]any{
				"nonce_id":             nonceID,
				"site_id":              siteID.String(),
				"target_wp_user_login": target,
				"consumed_from_ip":     consumedFromIP,
				"hot_path":             hotPath,
			},
		})
		auditID = entry.ID
	}
	return ConsumeResult{
		NonceID:      nonceID,
		TargetWPUser: target,
		AllowedRoles: roles,
		AuditID:      auditID,
		HotPath:      hotPath,
	}, nil
}

// failMint records autologin.failed for any mint failure and returns the
// original error. The nonceID may be empty when the failure preceded mint.
func (s *Service) failMint(ctx context.Context, req MintRequest, nonceID string, err error) error {
	s.recordFailure(ctx, req.TenantID, req.SiteID, nonceID, "mint", err)
	return err
}

func (s *Service) failConsume(ctx context.Context, tenantID, siteID uuid.UUID, nonceID string, err error) error {
	s.recordFailure(ctx, tenantID, siteID, nonceID, "consume", err)
	return err
}

func (s *Service) recordFailure(ctx context.Context, tenantID, siteID uuid.UUID, nonceID, stage string, err error) {
	if s.rec == nil || tenantID == uuid.Nil {
		// We cannot tenant-scope an audit insert without a tenant; the upstream
		// failures that lack one (e.g. unknown nonce) intentionally skip audit.
		return
	}
	code := "unknown_error"
	if de, ok := domain.AsDomain(err); ok {
		code = de.Code
	}
	_, _ = s.rec.Record(ctx, audit.Event{
		TenantID:   tenantID,
		ActorType:  audit.ActorSystem,
		Action:     audit.ActionAutologinFailed,
		TargetType: "site",
		TargetID:   siteID.String(),
		Metadata: map[string]any{
			"nonce_id": nonceID,
			"site_id":  siteID.String(),
			"code":     code,
			"stage":    stage,
		},
	})
}

// rateLimited wraps the rate-limit rejection in a typed domain error. The
// retry-after seconds are encoded in the message in the canonical form
// "retry after <N> seconds" so the handler can surface it as a structured
// field (and as a Retry-After header) without inventing a side channel.
func rateLimited(retry time.Duration) *domain.Error {
	return domain.RateLimited("rate_limited", fmt.Sprintf("rate limited; retry after %d seconds", retryAfterSeconds(retry)))
}

// retryAfterSeconds rounds a Duration up to whole seconds, never below 1.
func retryAfterSeconds(retry time.Duration) int {
	sec := int(retry.Round(time.Second) / time.Second)
	if sec < 1 {
		sec = 1
	}
	return sec
}

// RetryAfterFromError extracts the retry-after seconds from a rate-limited
// domain error's message; returns 0 if not a rate-limited error or unparseable.
// Exposed so the handler can copy the seconds into the JSON body and the
// Retry-After header.
func RetryAfterFromError(err error) int {
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindRateLimited {
		return 0
	}
	var sec int
	if _, perr := fmt.Sscanf(de.Message, "rate limited; retry after %d seconds", &sec); perr != nil {
		return 0
	}
	return sec
}

// gone returns a 410-mapped domain error. The nonce table never reveals
// "unknown vs expired vs already-consumed" so all three failures share one code.
func gone(code, msg string) *domain.Error {
	return domain.Gone(code, msg)
}

// buildRedirectURL composes the operator-facing URL the browser will follow.
// The path is appended to the site's URL with the JWT in the `token` query
// string and the optional redirect_to as `redirect_to`.
func buildRedirectURL(siteURL, pathTmpl, jwt, redirectTo string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(siteURL))
	if err != nil {
		return "", fmt.Errorf("parse site url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid site url scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("site url has no host")
	}
	u.Path = strings.TrimRight(u.Path, "/") + pathTmpl
	q := url.Values{}
	q.Set("token", jwt)
	if redirectTo != "" {
		q.Set("redirect_to", redirectTo)
	}
	u.RawQuery = q.Encode()
	u.Fragment = ""
	return u.String(), nil
}

// truncate clips s to at most n bytes. Avoids unbounded user-agent strings
// in audit metadata; never errors.
func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n]
}
