package billing

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/gomodule/redigo/redis"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// cacheTTLSeconds is the Redis TTL for a resolved Entitlements value.
const cacheTTLSeconds = 300 // 5 minutes

// Service is the entitlement resolver and site-cap gate. It is the ONLY
// wiring point the rest of the control plane depends on; see the package doc
// for why plan comparisons never leak outside this package.
//
// Enabled=false (the WPMGR_HOSTED default) makes every method a no-op:
// Entitlements returns Unlimited() and CheckSiteCreate always succeeds,
// without touching the database or Redis. This means call sites never need
// their own hosted/self-host branch — see site.BillingGate's callers.
type Service struct {
	pool    *db.Pool
	redis   *redis.Pool // optional; nil disables the entitlements cache (still correct, just uncached)
	enabled bool
	clock   domain.Clock
	logger  *slog.Logger

	// M16 Phase B — payment-provider integration. All wired via setters AFTER
	// construction (mirrors SetBillingGate/SetService elsewhere in this
	// codebase), so New's signature — already called from cmd/wpmgr/main.go
	// and every Phase A test — never has to change.
	registry        *Registry
	defaultProvider string
	auditRec        *audit.Recorder
}

// SetProviders wires the payment-provider registry built at boot from config
// (see cmd/wpmgr/main.go). defaultProvider is the provider name a tenant with
// no billing_provider yet resolves to on its FIRST checkout (today always
// "stripe" — a future second adapter, e.g. Razorpay for India, would pick
// its default by locale/currency without this package changing). Safe to call
// with registry=nil (checkout/portal then degrade to a clean
// ServiceUnavailable, matching "hosted with zero providers configured is
// legal").
func (s *Service) SetProviders(registry *Registry, defaultProvider string) {
	s.registry = registry
	s.defaultProvider = defaultProvider
}

// SetAudit wires the audit Recorder used for billing.checkout.started,
// billing.portal.opened, and billing.subscription.changed entries. A nil
// Recorder (the zero value) makes every audit call a silent no-op — mirrors
// every other domain's optional-audit pattern (e.g. perf.Handler.record).
func (s *Service) SetAudit(rec *audit.Recorder) {
	s.auditRec = rec
}

// recordAudit is a best-effort audit write: a failure is logged, never
// returned, so an audit-recorder hiccup can never fail a billing operation
// that itself succeeded.
func (s *Service) recordAudit(ctx context.Context, tenantID uuid.UUID, actorType, actorID, action string, meta map[string]any) {
	if s.auditRec == nil {
		return
	}
	if _, err := s.auditRec.Record(ctx, audit.Event{
		TenantID:   tenantID,
		ActorType:  actorType,
		ActorID:    actorID,
		Action:     action,
		TargetType: "tenant",
		TargetID:   tenantID.String(),
		Metadata:   meta,
	}); err != nil {
		s.logger.Warn("billing: audit record failed", slog.String("action", action), slog.Any("error", err))
	}
}

// New builds a billing Service. redisPool may be nil (the cache is then
// always a miss — every Entitlements() call resolves fresh from Postgres,
// which is correct, just slower; it is never a reason to fail). enabled
// should be cfg.Hosted.Enabled.
func New(pool *db.Pool, redisPool *redis.Pool, enabled bool, clock domain.Clock, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{pool: pool, redis: redisPool, enabled: enabled, clock: clock, logger: logger}
}

// Enabled reports whether hosted billing is turned on (WPMGR_HOSTED).
func (s *Service) Enabled() bool { return s.enabled }

// Entitlements resolves a tenant's current plan limits: ladder[plan] ->
// plan_overrides delta -> plan_status/grace_until gate. Backed by a 5-minute
// Redis cache (key "ent:<tenantID>"); a Redis outage fails OPEN (logged
// warning, resolve fresh from Postgres) — Redis being down must never block
// an enroll or any other entitlement-gated action.
//
// Returns Unlimited() immediately, without any I/O, when hosted billing is
// disabled.
func (s *Service) Entitlements(ctx context.Context, tenantID uuid.UUID) (Entitlements, error) {
	if !s.enabled {
		return Unlimited(), nil
	}
	if ent, ok := s.getCached(ctx, tenantID); ok {
		return ent, nil
	}
	row, err := fetchTenantBilling(ctx, s.pool, tenantID)
	if err != nil {
		return Entitlements{}, domain.Internal("billing_tenant_lookup_failed", "failed to load tenant billing state").WithCause(err)
	}
	ent, err := resolve(row, s.clock.Now())
	if err != nil {
		return Entitlements{}, err
	}
	s.setCached(ctx, tenantID, ent)
	return ent, nil
}

// CheckSiteCreate enforces the site cap for one more active (non-archived)
// site. It MUST be called inside the SAME transaction as the site
// INSERT/UPDATE that would grow the active count (the site row's creation,
// or an archived site's un-archive/restore) — it takes the tx directly for
// exactly that reason.
//
// It first acquires a per-tenant pg_advisory_xact_lock (mirrors the audit
// hash-chain's per-tenant lock, internal/audit.lockChain) so two concurrent
// callers for the same tenant serialize: the first to commit wins the last
// slot, the second correctly sees the incremented count. The lock is scoped
// to the transaction and releases automatically on commit or rollback.
//
// It then re-reads the tenant's plan/status/overrides and the active site
// count FRESH, inside this same locked transaction — deliberately bypassing
// the Entitlements() Redis cache, so a stale cache entry can never let an
// over-cap create through (or block a just-upgraded tenant).
//
// Returns nil immediately when hosted billing is disabled.
func (s *Service) CheckSiteCreate(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	if !s.enabled {
		return nil
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('wpmgr_quota:' || $1))", tenantID.String()); err != nil {
		return domain.Internal("billing_lock_failed", "failed to acquire site-quota lock").WithCause(err)
	}

	row, err := fetchTenantBilling(ctx, tx, tenantID)
	if err != nil {
		return domain.Internal("billing_tenant_lookup_failed", "failed to load tenant billing state").WithCause(err)
	}
	ent, err := resolve(row, s.clock.Now())
	if err != nil {
		return err
	}

	count, err := sqlc.New(tx).CountActiveSitesForBilling(ctx, tenantID)
	if err != nil {
		return domain.Internal("billing_count_failed", "failed to count active sites").WithCause(err)
	}

	if int(count) >= ent.MaxSites {
		return domain.PaymentRequired("site_limit_reached",
			"this workspace is at its site limit. Upgrade in the WPMgr dashboard to add more sites.").
			WithDetails(map[string]any{
				"limit": ent.MaxSites,
				"usage": int(count),
				"plan":  string(ent.Plan),
			})
	}
	return nil
}

// fetchTenantBilling loads the plan-resolution fields via any DBTX (the pool
// for the uncached general path, or a tx for the locked CheckSiteCreate
// path) and maps them to the DB-free tenantBillingRow shape.
func fetchTenantBilling(ctx context.Context, dbtx sqlc.DBTX, tenantID uuid.UUID) (tenantBillingRow, error) {
	row, err := sqlc.New(dbtx).GetTenantBilling(ctx, tenantID)
	if err != nil {
		return tenantBillingRow{}, err
	}
	out := tenantBillingRow{
		Plan:          row.Plan,
		PlanStatus:    row.PlanStatus,
		PlanOverrides: row.PlanOverrides,
	}
	if row.GraceUntil.Valid {
		t := row.GraceUntil.Time
		out.GraceUntil = &t
	}
	return out, nil
}

// cacheKey is the Redis key an tenant's resolved Entitlements are cached
// under.
func cacheKey(tenantID uuid.UUID) string { return "ent:" + tenantID.String() }

// getCached returns the cached Entitlements for tenantID, or (zero, false) on
// a cache miss OR any Redis failure. Every failure path logs a warning and
// falls through to the DB — Redis must never be a hard dependency.
func (s *Service) getCached(ctx context.Context, tenantID uuid.UUID) (Entitlements, bool) {
	if s.redis == nil {
		return Entitlements{}, false
	}
	conn, err := s.redis.GetContext(ctx)
	if err != nil {
		s.logger.Warn("billing: redis unavailable, resolving entitlements from the database", slog.Any("error", err))
		return Entitlements{}, false
	}
	defer conn.Close()

	raw, err := redis.Bytes(conn.Do("GET", cacheKey(tenantID)))
	if err != nil {
		if err != redis.ErrNil {
			s.logger.Warn("billing: redis GET failed, resolving entitlements from the database", slog.Any("error", err))
		}
		return Entitlements{}, false
	}
	var ent Entitlements
	if err := json.Unmarshal(raw, &ent); err != nil {
		s.logger.Warn("billing: cached entitlements were corrupt, recomputing", slog.Any("error", err))
		return Entitlements{}, false
	}
	return ent, true
}

// setCached best-effort writes the resolved Entitlements to Redis with a
// 5-minute TTL. Any failure is logged and swallowed: a cache write is never
// allowed to fail the request that just resolved the entitlements correctly.
func (s *Service) setCached(ctx context.Context, tenantID uuid.UUID, ent Entitlements) {
	if s.redis == nil {
		return
	}
	conn, err := s.redis.GetContext(ctx)
	if err != nil {
		s.logger.Warn("billing: redis unavailable, skipping entitlements cache write", slog.Any("error", err))
		return
	}
	defer conn.Close()

	raw, err := json.Marshal(ent)
	if err != nil {
		s.logger.Warn("billing: failed to marshal entitlements for caching", slog.Any("error", err))
		return
	}
	if _, err := conn.Do("SETEX", cacheKey(tenantID), cacheTTLSeconds, raw); err != nil {
		s.logger.Warn("billing: redis SETEX failed", slog.Any("error", err))
	}
}

// invalidateCache best-effort deletes a tenant's cached Entitlements so the
// NEXT read resolves fresh from Postgres. Called after the Phase B webhook
// state machine changes tenants.plan/plan_status — without this, a tenant
// could keep observing a stale (pre-upgrade or pre-downgrade) entitlement for
// up to the 5-minute cache TTL. A cache-delete failure is logged and
// swallowed: the TTL is still a correctness backstop even if this best-effort
// invalidation fails.
func (s *Service) invalidateCache(ctx context.Context, tenantID uuid.UUID) {
	if s.redis == nil {
		return
	}
	conn, err := s.redis.GetContext(ctx)
	if err != nil {
		s.logger.Warn("billing: redis unavailable, skipping entitlements cache invalidation", slog.Any("error", err))
		return
	}
	defer conn.Close()
	if _, err := conn.Do("DEL", cacheKey(tenantID)); err != nil {
		s.logger.Warn("billing: redis DEL failed", slog.Any("error", err))
	}
}
