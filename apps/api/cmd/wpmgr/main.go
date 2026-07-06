// Command wpmgr is the WPMgr control-plane API server: it loads config,
// initializes telemetry, connects to Postgres, applies migrations, wires the
// domains, and serves the Gin HTTP API with graceful shutdown.
package main

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/mosamlife/wpmgr/apps/api/internal/activity"
	"github.com/mosamlife/wpmgr/apps/api/internal/admin"
	"github.com/mosamlife/wpmgr/apps/api/internal/agent"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/apikey"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/auth/twofactor"
	"github.com/mosamlife/wpmgr/apps/api/internal/autologin"
	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	billingstripe "github.com/mosamlife/wpmgr/apps/api/internal/billing/stripe"
	"github.com/mosamlife/wpmgr/apps/api/internal/blobstore"
	clientpkg "github.com/mosamlife/wpmgr/apps/api/internal/client"
	"github.com/mosamlife/wpmgr/apps/api/internal/config"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/dbclean"
	"github.com/mosamlife/wpmgr/apps/api/internal/diagnostics"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/email"
	"github.com/mosamlife/wpmgr/apps/api/internal/files"
	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
	"github.com/mosamlife/wpmgr/apps/api/internal/invitation"
	"github.com/mosamlife/wpmgr/apps/api/internal/ipprovider"
	"github.com/mosamlife/wpmgr/apps/api/internal/loginbrand"
	"github.com/mosamlife/wpmgr/apps/api/internal/mailer"
	"github.com/mosamlife/wpmgr/apps/api/internal/media"
	mediafont "github.com/mosamlife/wpmgr/apps/api/internal/media/font"
	mediahandler "github.com/mosamlife/wpmgr/apps/api/internal/media/handler"
	mediamodel "github.com/mosamlife/wpmgr/apps/api/internal/media/model"
	mediarepo "github.com/mosamlife/wpmgr/apps/api/internal/media/repo"
	mediaservice "github.com/mosamlife/wpmgr/apps/api/internal/media/service"
	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
	"github.com/mosamlife/wpmgr/apps/api/internal/middleware"
	"github.com/mosamlife/wpmgr/apps/api/internal/objectcache"
	"github.com/mosamlife/wpmgr/apps/api/internal/org"
	"github.com/mosamlife/wpmgr/apps/api/internal/perf"
	portalpkg "github.com/mosamlife/wpmgr/apps/api/internal/portal"
	reportpkg "github.com/mosamlife/wpmgr/apps/api/internal/report"
	reporthtml "github.com/mosamlife/wpmgr/apps/api/internal/report/render/html"
	reportpdf "github.com/mosamlife/wpmgr/apps/api/internal/report/render/pdf"
	"github.com/mosamlife/wpmgr/apps/api/internal/riverutil"
	rucssrepo "github.com/mosamlife/wpmgr/apps/api/internal/rucss/repo"
	rucssservice "github.com/mosamlife/wpmgr/apps/api/internal/rucss/service"
	rucssworker "github.com/mosamlife/wpmgr/apps/api/internal/rucss/worker"
	"github.com/mosamlife/wpmgr/apps/api/internal/rum"
	"github.com/mosamlife/wpmgr/apps/api/internal/scan"
	"github.com/mosamlife/wpmgr/apps/api/internal/screenshot"
	"github.com/mosamlife/wpmgr/apps/api/internal/screenshotadapter"
	"github.com/mosamlife/wpmgr/apps/api/internal/security"
	"github.com/mosamlife/wpmgr/apps/api/internal/server"
	"github.com/mosamlife/wpmgr/apps/api/internal/settings"
	"github.com/mosamlife/wpmgr/apps/api/internal/sharing"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	siteevents "github.com/mosamlife/wpmgr/apps/api/internal/site/events"
	"github.com/mosamlife/wpmgr/apps/api/internal/sitedestination"
	"github.com/mosamlife/wpmgr/apps/api/internal/telemetry"
	"github.com/mosamlife/wpmgr/apps/api/internal/tenant"
	"github.com/mosamlife/wpmgr/apps/api/internal/update"
	"github.com/mosamlife/wpmgr/apps/api/internal/uptime"
	"github.com/mosamlife/wpmgr/apps/api/internal/vuln"
)

// version is overridden at build time via -ldflags.
var version = "0.0.0-dev"

func main() {
	// Load config and initialize the logger as early as possible so all boot
	// paths have structured output.
	cfg, err := config.Load(os.Getenv("WPMGR_CONFIG_FILE"))
	if err != nil {
		slog.Error("fatal: config load failed", slog.Any("error", err))
		os.Exit(1)
	}

	logger := newLogger(cfg)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Aggregate ALL config issues before touching the DB or starting any server.
	// On any issue we park in degraded mode (no crash-loop) so an operator can
	// curl /readyz to read which env vars need fixing.
	if issues := config.Validate(cfg); len(issues) > 0 {
		if err := serveDegraded(ctx, cfg.HTTPAddr, issues); err != nil {
			slog.Error("degraded server error", slog.Any("error", err))
			os.Exit(1)
		}
		return
	}

	if err := run(ctx, cfg, logger); err != nil {
		slog.Error("fatal", slog.Any("error", err))
		os.Exit(1)
	}
}

// run bootstraps the control plane: migrations, services, River worker pool,
// media schema isolation wiring, and the HTTP server.
func run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	slog.SetDefault(logger)

	tp, err := telemetry.Init(ctx, telemetry.Config{
		ServiceName:  cfg.OTel.ServiceName,
		OTLPEndpoint: cfg.OTel.OTLPEndpoint,
	})
	if err != nil {
		return err
	}
	defer func() { _ = tp.Shutdown(context.Background()) }()

	// Defense-in-depth: individual guards still run inside run() so any future
	// caller of run() that skips the validateConfig pre-check still hard-fails
	// cleanly rather than proceeding with a bad config.
	//
	// Refuse to boot with a weak/placeholder session secret.
	if err := cfg.ValidateSessionSecret(); err != nil {
		return err
	}

	// Refuse to boot in production with a known committed dev control-plane
	// signing key (no-op in development).
	if err := cfg.ValidateAgentSigningKey(); err != nil {
		return err
	}

	mediaRiverSchema, err := riverutil.NormalizeSchema(cfg.River.MediaSchema)
	if err != nil {
		return err
	}
	// Log the resolved media River schema. A mismatch between this value and the
	// media-encoder's silently strands media/screenshot jobs, so emitting it on
	// each process makes an operator eyeball-diff trivial.
	mediaSchemaLog := mediaRiverSchema
	if riverutil.IsDefaultSchema(mediaRiverSchema) {
		mediaSchemaLog = "public"
	}
	logger.Info("media River schema resolved", slog.String("media_river_schema", mediaSchemaLog))

	// Migrations run with the owner/superuser DSN (creates the app role +
	// privileged DDL); the application connects with the unprivileged app DSN.
	// River's own schema is migrated here too, with the same owner DSN. In
	// single-DSN dev, MigrateDSN() falls back to the app DSN — the migrations
	// are authored to remain applicable in that mode (e.g. the plugin_signatures
	// seed inserts corpus rows before revoking wpmgr_app's DML).
	migPool, err := db.Connect(ctx, cfg.DB.MigrateDSN())
	if err != nil {
		return err
	}
	if err := migPool.Migrate(ctx); err != nil {
		migPool.Close()
		return err
	}
	if err := migrateRiver(ctx, migPool.Pool); err != nil {
		migPool.Close()
		return err
	}
	if err := riverutil.EnsureSchema(ctx, migPool.Pool, mediaRiverSchema, cfg.DB.User); err != nil {
		migPool.Close()
		return err
	}

	// Seed superadmin accounts from WPMGR_SUPERADMIN_EMAILS (comma-separated).
	// Additive: sets is_superadmin=true for existing accounts; no-op for unknown
	// emails. Never auto-demotes. Done before closing migPool (owner DSN, bypasses
	// RLS). Runs after migrations so the is_superadmin column is guaranteed to exist.
	if raw := os.Getenv("WPMGR_SUPERADMIN_EMAILS"); raw != "" {
		saBaseURL := strings.TrimRight(os.Getenv("WPMGR_PUBLIC_BASE_URL"), "/")
		for _, email := range strings.Split(raw, ",") {
			// Emails are persisted lowercased (normalizeEmail), so match
			// case-insensitively.
			email = strings.ToLower(strings.TrimSpace(email))
			if email == "" {
				continue
			}
			// Allowlisted superadmins are trusted at the infrastructure level, so
			// also activate + mark them verified — they need not receive a
			// verification email (their mailbox domain may not even accept mail).
			tag, err := migPool.Pool.Exec(ctx,
				`UPDATE users
				    SET is_superadmin = true,
				        status = 'active',
				        email_verified_at = COALESCE(email_verified_at, now()),
				        updated_at = now()
				  WHERE lower(email) = $1`, email,
			)
			switch {
			case err != nil:
				logger.Warn("superadmin seed failed", slog.String("email", email), slog.Any("error", err))
			case tag.RowsAffected() > 0:
				logger.Info("superadmin granted to existing account", slog.String("email", email))
			default:
				// No account yet: create one (active + verified + superadmin) with
				// a random password the operator never learns, and mint a one-time
				// set-password link so they choose their own password. The link is
				// logged because the account's mailbox may not accept mail.
				if err := seedSuperadminAccount(ctx, migPool.Pool, logger, saBaseURL, email); err != nil {
					logger.Warn("superadmin account create failed", slog.String("email", email), slog.Any("error", err))
				}
			}
		}
	}

	// One-shot revoke: WPMGR_SUPERADMIN_REVOKE_EMAILS = "email[,email2]".
	// Sets is_superadmin=false for each listed email. A no-op for unknown emails
	// (no account to demote) and for emails already not superadmin. This is the
	// intentional mirror of WPMGR_SUPERADMIN_EMAILS: grants are never implicit
	// in this seeder and revokes are never implicit in the grant seeder.
	// REMOVE this env var after it runs — re-revocation on every boot is harmless
	// but noisy. is_superadmin is NOT API-settable; this boot hook is the only
	// supported revoke path.
	if raw := os.Getenv("WPMGR_SUPERADMIN_REVOKE_EMAILS"); raw != "" {
		for _, email := range strings.Split(raw, ",") {
			email = strings.ToLower(strings.TrimSpace(email))
			if email == "" {
				continue
			}
			tag, err := migPool.Pool.Exec(ctx,
				`UPDATE users
				    SET is_superadmin = false,
				        updated_at    = now()
				  WHERE lower(email) = $1 AND is_superadmin = true`, email,
			)
			switch {
			case err != nil:
				logger.Warn("superadmin revoke failed", slog.String("email", email), slog.Any("error", err))
			case tag.RowsAffected() > 0:
				logger.Info("superadmin revoked", slog.String("email", email))
			default:
				logger.Info("superadmin revoke: account not superadmin or not found, no change", slog.String("email", email))
			}
		}
	}

	// One-shot escape hatch: mint a fresh set-password link for these (existing)
	// superadmin accounts and log it. Set this when an operator needs to (re)claim
	// an account whose password is unknown — e.g. one seeded before a fix — then
	// remove the env var so it does not mint a link on every boot.
	if raw := os.Getenv("WPMGR_SUPERADMIN_RESET_EMAILS"); raw != "" {
		rsBaseURL := strings.TrimRight(os.Getenv("WPMGR_PUBLIC_BASE_URL"), "/")
		for _, email := range strings.Split(raw, ",") {
			email = strings.ToLower(strings.TrimSpace(email))
			if email == "" {
				continue
			}
			var uid uuid.UUID
			if err := migPool.Pool.QueryRow(ctx, `SELECT id FROM users WHERE lower(email) = $1`, email).Scan(&uid); err != nil {
				logger.Warn("superadmin reset link: no account with that email", slog.String("email", email), slog.Any("error", err))
				continue
			}
			if err := mintSetPasswordLink(ctx, migPool.Pool, logger, rsBaseURL, uid, email, "superadmin set-password requested"); err != nil {
				logger.Warn("superadmin reset link failed", slog.String("email", email), slog.Any("error", err))
			}
		}
	}

	// One-shot account recovery: WPMGR_RECOVER_ACCOUNTS = "email:org[,email2:org2]"
	// where org is a tenant slug or name. Recreates a deleted user (active +
	// verified), attaches it to the EXISTING org as owner, and logs a one-time
	// set-password link. Use this to recover an account whose org + sites are
	// intact but whose user row was deleted. Idempotent. REMOVE the env after use —
	// it re-mints a link on every boot otherwise.
	if raw := os.Getenv("WPMGR_RECOVER_ACCOUNTS"); raw != "" {
		rcBaseURL := strings.TrimRight(os.Getenv("WPMGR_PUBLIC_BASE_URL"), "/")
		for _, entry := range strings.Split(raw, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			// Email cannot contain ':', so split on the first colon.
			parts := strings.SplitN(entry, ":", 2)
			if len(parts) != 2 {
				logger.Warn("recover account: bad entry, want email:org", slog.String("entry", entry))
				continue
			}
			email := strings.ToLower(strings.TrimSpace(parts[0]))
			orgRef := strings.TrimSpace(parts[1])
			if email == "" || orgRef == "" {
				logger.Warn("recover account: empty email or org", slog.String("entry", entry))
				continue
			}
			if err := recoverAccountIntoOrg(ctx, migPool.Pool, logger, rcBaseURL, email, orgRef); err != nil {
				logger.Warn("recover account failed", slog.String("email", email), slog.String("org", orgRef), slog.Any("error", err))
			}
		}
	}

	// One-shot membership reconciliation: WPMGR_GRANT_MEMBERSHIPS =
	// "email:tenant_uuid[:role][,email2:tenant_uuid2[:role2]]". Ensures an EXISTING
	// user is a member of an EXISTING org (addressed by tenant UUID, so there is no
	// name ambiguity) — the fix for a recovery that attached an account to the
	// wrong org. Unlike WPMGR_RECOVER_ACCOUNTS it NEVER creates a user or mints a
	// password link; it is pure, idempotent membership upsert. Role defaults to
	// 'owner'. Safe to re-run; remove the env after use for cleanliness.
	if raw := os.Getenv("WPMGR_GRANT_MEMBERSHIPS"); raw != "" {
		for _, entry := range strings.Split(raw, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			parts := strings.SplitN(entry, ":", 3)
			if len(parts) < 2 {
				logger.Warn("grant membership: bad entry, want email:tenant_uuid[:role]", slog.String("entry", entry))
				continue
			}
			email := strings.ToLower(strings.TrimSpace(parts[0]))
			tenantID, perr := uuid.Parse(strings.TrimSpace(parts[1]))
			if perr != nil {
				logger.Warn("grant membership: tenant must be a UUID", slog.String("entry", entry))
				continue
			}
			role := "owner"
			if len(parts) == 3 && strings.TrimSpace(parts[2]) != "" {
				role = strings.TrimSpace(parts[2])
			}
			if err := grantMembership(ctx, migPool.Pool, logger, email, tenantID, role); err != nil {
				logger.Warn("grant membership failed", slog.String("email", email), slog.String("tenant_id", tenantID.String()), slog.Any("error", err))
			}
		}
	}

	// One-shot membership revocation: WPMGR_REVOKE_MEMBERSHIPS =
	// "email:tenant_uuid[,...]". Removes a user's membership in an org — e.g. to
	// drop a stray empty org left by a recovery so the user's remaining org
	// becomes their login default (login picks the first membership; there is no
	// org switcher yet). Idempotent. Never deletes the org itself, only the
	// membership row. Remove the env after use.
	if raw := os.Getenv("WPMGR_REVOKE_MEMBERSHIPS"); raw != "" {
		for _, entry := range strings.Split(raw, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			parts := strings.SplitN(entry, ":", 2)
			if len(parts) != 2 {
				logger.Warn("revoke membership: bad entry, want email:tenant_uuid", slog.String("entry", entry))
				continue
			}
			email := strings.ToLower(strings.TrimSpace(parts[0]))
			tenantID, perr := uuid.Parse(strings.TrimSpace(parts[1]))
			if perr != nil {
				logger.Warn("revoke membership: tenant must be a UUID", slog.String("entry", entry))
				continue
			}
			if err := revokeMembership(ctx, migPool.Pool, logger, email, tenantID); err != nil {
				logger.Warn("revoke membership failed", slog.String("email", email), slog.String("tenant_id", tenantID.String()), slog.Any("error", err))
			}
		}
	}

	migPool.Close()
	logger.Info("migrations applied")

	pool, err := db.ConnectApp(ctx, cfg.DB.DSN())
	if err != nil {
		return err
	}
	defer pool.Close()

	// Hard-fail if the application role bypasses RLS (overridable for dev via
	// WPMGR_ALLOW_RLS_BYPASS_ROLE=true).
	if err := pool.EnforceRLSRole(ctx, logger, cfg.DB.AllowRLSBypassRole); err != nil {
		return err
	}

	// Safety guard for self-hosters: verify the app DSN role actually holds the
	// privileges that the migrations grant to wpmgr_app. A self-hoster who sets
	// WPMGR_DB_USER to a role that was never granted wpmgr_app privileges will hit
	// cryptic "permission denied" on every table access. This functional probe
	// catches the misconfiguration at boot — before any traffic is served — with a
	// clear, actionable message. The probe runs only in two-DSN mode (a separate
	// WPMGR_DB_MIGRATION_DSN is set), because in single-DSN mode the app connects
	// as the migration runner and trivially has all the privileges it just created.
	// has_table_privilege always returns true for superusers, so the probe cannot
	// block a correctly-configured instance.
	if cfg.DB.MigrationDSN != "" {
		if err := pool.ProbeTablePrivilege(ctx, logger); err != nil {
			return err
		}
	}

	validator := domain.NewValidator()
	clock := domain.SystemClock{}

	tenantSvc := tenant.NewService(tenant.NewRepo(pool), validator, clock)
	siteSvc := site.NewService(site.NewRepo(pool), validator, clock)
	siteSvc.SetLogger(logger)
	auditRec := audit.NewRecorder(pool, clock)

	// A narrow tenant-creation capability handed to the auth domain (bootstrap +
	// OIDC first-login) without coupling it to the tenant package internals.
	newTenant := func(ctx context.Context, name, slug string) (uuid.UUID, error) {
		t, err := tenantSvc.Create(ctx, tenant.CreateInput{Name: name, Slug: slug})
		if err != nil {
			return uuid.Nil, err
		}
		return t.ID, nil
	}

	authRepo := auth.NewRepo(pool)
	authSvc := auth.NewService(authRepo, auditRec, validator)
	apiKeySvc := apikey.NewService(pool)

	oidcProvider, err := auth.NewOIDCProvider(ctx, cfg.OIDC)
	if err != nil {
		// Discovery failure should not silently disable OIDC; surface it.
		return err
	}
	if oidcProvider.Enabled() {
		logger.Info("OIDC relying party enabled", slog.String("issuer", cfg.OIDC.Issuer))
	} else {
		logger.Info("OIDC disabled (no issuer configured); email+password only")
	}

	redisPool := auth.NewRedisPool(cfg.Redis.Addr, cfg.Redis.Password)
	sessions := auth.NewRedisSessionManager(redisPool, cfg.Auth.IdleTimeout, cfg.Auth.AbsoluteExpiry, cfg.IsProduction())

	// M16 Phase A — hosted-billing entitlement substrate (WPMGR_HOSTED, default
	// false: every check below no-ops until an operator turns it on). Reuses
	// the session Redis pool for the 5-minute entitlements cache (a distinct
	// "ent:" key prefix keeps it from colliding with session keys); a nil/down
	// Redis still resolves correctly, just uncached, from Postgres.
	//
	// SetBillingGate is called TWICE — here on siteSvc's own repo, and again
	// below (once siteRepo exists) on the SEPARATE repo instance handed to
	// site.NewConnectionService — because cmd/wpmgr constructs two distinct
	// *pgRepo instances (see BillingGate's doc comment in internal/site/repo.go
	// for the exact wiring gotcha this mirrors, first hit by the screenshot
	// enricher in v0.49.1).
	billingSvc := billing.New(pool, redisPool, cfg.Hosted.Enabled, clock, logger)
	siteSvc.SetBillingGate(billingSvc)

	// M16 Phase B — payment-provider integration. The registry is built from
	// config REGARDLESS of WPMGR_HOSTED (an empty registry is harmless — every
	// billing.Service method already treats "no providers configured" as a
	// clean, documented no-op/503, never a crash). Stripe is registered ONLY
	// when its five WPMGR_BILLING_STRIPE_* variables are ALL present
	// (StripeConfig.Configured — config.Validate refuses a PARTIAL Stripe
	// config at boot, so by the time we get here it is always all-or-nothing).
	var billingProviders []billing.Provider
	stripeCfg := billingstripe.Config{
		SecretKey:       cfg.Billing.Stripe.SecretKey,
		WebhookSecret:   cfg.Billing.Stripe.WebhookSecret,
		PriceStarter:    cfg.Billing.Stripe.PriceStarter,
		PriceAgency:     cfg.Billing.Stripe.PriceAgency,
		PriceScale:      cfg.Billing.Stripe.PriceScale,
		PortalReturnURL: strings.TrimRight(os.Getenv("WPMGR_PUBLIC_BASE_URL"), "/") + "/billing",
	}
	if stripeCfg.Configured() {
		billingProviders = append(billingProviders, billingstripe.New(stripeCfg))
		logger.Info("billing: Stripe provider registered")
	} else if cfg.Hosted.Enabled {
		logger.Warn("billing: hosted billing is enabled but no payment provider is configured — checkout/portal will return 503")
	}
	billingSvc.SetProviders(billing.NewRegistry(billingProviders...), "stripe")
	billingSvc.SetAudit(auditRec)

	// The billing HTTP handlers are always CONSTRUCTED (cheap, stateless) but
	// only MOUNTED when hosted billing is enabled (see the server.Deps wiring
	// near the bottom of this function) — that nil/non-nil split is the
	// routes-contract 404-when-unhosted guarantee.
	billingPublicBaseURL := strings.TrimRight(os.Getenv("WPMGR_PUBLIC_BASE_URL"), "/")
	billingH := billing.NewHandler(billingSvc, func(ctx context.Context, userID uuid.UUID) (string, error) {
		u, err := authRepo.GetUserByID(ctx, userID)
		if err != nil {
			return "", err
		}
		return u.Email, nil
	}, billingPublicBaseURL)
	billingWebhookH := billing.NewWebhookHandler(billingSvc, logger)
	billingReconcileWorker := billing.NewReconcileWorker(billingSvc, logger)

	// Both handlers are mounted ONLY when hosted billing is enabled — this
	// nil/non-nil split is the routes-contract 404-when-unhosted guarantee
	// (see server.Deps.BillingH/BillingWebhookH's doc comments).
	var billingHForRoutes *billing.Handler
	var billingWebhookHForRoutes *billing.WebhookHandler
	// M16 Phase C1 — the superadmin hard-lockout enforcement middleware
	// (tenants.suspended_at, m92). Same nil/non-nil split: self-host
	// (unhosted) never even constructs the closure, matching every other
	// hosted-only wiring in this block. billingSvc.SuspensionGate() also
	// carries its own in-body s.enabled guard (belt-and-braces).
	var billingSuspensionGateForRoutes gin.HandlerFunc
	if cfg.Hosted.Enabled {
		billingHForRoutes = billingH
		billingWebhookHForRoutes = billingWebhookH
		billingSuspensionGateForRoutes = billingSvc.SuspensionGate()
	}

	authn := middleware.NewAuthenticator(sessions, authSvc, apiKeySvc, pool)

	// Agent protocol: the control plane's PUBLIC signing key is handed to agents
	// at enrollment so they can verify CP->agent commands. Validate the keypair
	// up front so misconfiguration fails fast rather than at first enroll.
	cpPublicKey, err := agentSigningPublicKey(cfg.Agent)
	if err != nil {
		return err
	}
	agentAuthn := agent.NewAuthenticator(siteSvc, clock, cfg.Agent.SignatureSkew)
	agentH := agent.NewHandler(siteSvc)

	// M3 bulk updates: the SSRF-hardened HTTP client (ADR-009) for all outbound
	// calls to agent/site URLs, the CP->agent command client (mints the signed
	// EdDSA JWT), the post-update health prober, the in-process SSE hub, and the
	// tenant-scoped update repo. The command signer is built from the CP signing
	// private key; an empty key disables minting (the worker will then fail
	// update commands loudly rather than send unsigned ones).
	ssrfClient := httpclient.New(httpclient.Config{
		Timeout:    cfg.Update.HTTPTimeout,
		MaxRetries: cfg.Update.HTTPRetries,
	})
	var commander update.Commander
	var cmdSigner *agentcmd.Signer
	if cfg.Agent.SigningPrivateKey != "" {
		signer, serr := agentcmd.NewSigner(cfg.Agent.SigningPrivateKey)
		if serr != nil {
			return fmt.Errorf("build command signer: %w", serr)
		}
		cmdSigner = signer
		commander = agentcmd.NewClient(ssrfClient, signer)
	} else {
		logger.Warn("WPMGR_AGENT_SIGNING_PRIVATE_KEY is empty: CP->agent update commands are disabled")
		commander = disabledCommander{}
	}
	prober := agentcmd.NewProbe(ssrfClient)
	updateHub := update.NewHub()
	updateRepo := update.NewRepo(pool)
	sitesLookup := newSiteLookup(siteSvc)

	// P1 File Manager (read-only). Dedicated signed agent client (same pattern as
	// the other domains). The download presigner is wired from the blobstore in
	// the backup block below; it stays nil (download degrades to not-configured)
	// when object storage is off.
	filesSvc := files.NewService(pool)
	if cmdSigner != nil {
		filesSvc.SetAgentClient(agentcmd.NewClient(ssrfClient, cmdSigner), newPerfSiteAdapter(siteSvc))
	}
	// File-transfers GC worker. The ObjectDeleter is wired from the blobstore
	// alongside filesSvc.SetPresigner (below). It stays nil when object storage
	// is not configured — in that case only DB rows are pruned.
	fileTransfersGCWorker := files.NewFileTransfersGCWorker(pool, nil, logger) // deleter wired below if S3 enabled

	// m85 — Uptime-probe retention GC. Always wired; runs daily and deletes
	// site_uptime_probes rows older than 90 days under InAgentTx (cross-tenant).
	// Bounding the table size is the root fix for why the 30-day aggregate scan
	// grows expensive over time.
	uptimeProbeGCWorker := metrics.NewUptimeProbeGCWorker(pool, logger)

	updateWorker := update.NewWorker(updateRepo, sitesLookup, commander, prober, updateHub, auditRec, logger, cfg.Update.PerTenantParallelism)
	// #131 follow-up — periodic reaper for update_tasks stuck in pending/running
	// past staleTaskThreshold (a worker crash mid-task, or a failed enqueue that
	// leaves a task pending). Without this, such a task permanently occupies its
	// (tenant, site, target_type, target_slug) slot in the
	// update_tasks_inflight_target_idx partial unique index (m88), and every
	// future update attempt for that target 409s targets_in_flight forever.
	// Always wired: it reuses updateWorker's repo/hub/audit/logger, needs no
	// signing key (it only reads/writes update_tasks), and reaping is itself the
	// backstop for MF-1 (the m88 migration's pre-dedup) going forward.
	updateReaperWorker := update.NewReaperWorker(updateWorker)
	// Updates feature (Track B): the refresh-inventory worker dispatches signed
	// CP->agent commands to re-pull a site's inventory. It satisfies River's
	// JobArgs interface so the per-tenant queue shard bounds its concurrency
	// alongside the update tasks. A nil commander cleanly cancels the job (no
	// unsigned commands ever sent).
	var refreshCmd update.RefreshCommander
	if rc, ok := commander.(update.RefreshCommander); ok {
		refreshCmd = rc
	}
	refreshWorker := update.NewRefreshInventoryWorker(refreshCmd, auditRec, logger)
	refreshDebouncer := update.NewRefreshDebouncer(30 * time.Second)

	// M4 backups: an S3-compatible blobstore (ADR-010) for presigned chunk
	// upload/download (only ciphertext is ever stored), the backup command client
	// (mints signed `backup`/`restore` JWTs; reuses the SSRF client), and the
	// tenant-scoped backup repo+service. When no bucket is configured the backup
	// feature is disabled cleanly (the endpoints 501 and no workers/periodics
	// run). The CP base URL is where the agent calls back for presign/manifest.
	var backupSvc *backup.Service
	var backupH *backup.Handler
	var backupAgentH *backup.AgentHandler
	var restoreRunH *backup.RestoreRunHandler
	var scheduleRunH *backup.ScheduleRunHandler
	var backupWorker *backup.BackupWorker
	var restoreWorker *backup.RestoreWorker
	var gcWorker *backup.GCWorker
	var scheduleWorker *backup.ScheduleWorker
	var progressWatchdog *backup.ProgressWatchdogWorker
	// M6 / Track 4: SQL inspection legacy worker. V1 has no plaintext source
	// or CP-side cache writer wired yet (the agent ships its own inspection
	// artifact in the manifest; the CP-legacy parser is a future fallback for
	// snapshots that pre-date that). The worker is still added to the River
	// pool + queue so any spurious enqueue surfaces a clear River failure
	// metric ("plaintext source or cache unwired") rather than a stuck job.
	var sqlInspectLegacyWorker *backup.SqlInspectLegacyWorker
	// M6 / Track 4: inspection-handler deps, populated below alongside the
	// backup feature gate. The handler.RegisterInspection mount in server.go
	// uses these to fetch agent-supplied sql-inspection artifacts from the
	// chunk store on demand. PlaintextSource + CacheWriter (the legacy-parser
	// tier) stay nil in V1 — those wires light up in a future track.
	var inspectionDeps backup.InspectionDeps
	// M5.6 backup-progress SSE hub: in-process pub/sub keyed by snapshot ID.
	// Service.Publish fans transitions out; Handler.events subscribes per stream.
	backupHub := backup.NewHub()
	if cfg.S3.Enabled() {
		store, serr := blobstore.New(blobstore.Config{
			Endpoint:       cfg.S3.Endpoint,
			Region:         cfg.S3.Region,
			Bucket:         cfg.S3.Bucket,
			AccessKey:      cfg.S3.AccessKey,
			SecretKey:      cfg.S3.SecretKey,
			ForcePathStyle: cfg.S3.ForcePathStyle,
		})
		if serr != nil {
			return fmt.Errorf("blobstore init: %w", serr)
		}
		// EnsureBucket is best-effort (it already returns nil on all error paths)
		// and involves a public-internet HTTPS round-trip to the S3 endpoint.
		// Running it synchronously on every cold boot adds ~0.5-2 s of latency
		// before the HTTP listener opens. Mirror the ADR-042 self-update probe
		// pattern: run off the critical path in a background goroutine.
		go func(s *blobstore.Store) {
			if berr := s.EnsureBucket(context.Background()); berr != nil {
				slog.Warn("blobstore: EnsureBucket boot probe failed", slog.Any("error", berr))
			}
		}(store)

		// File Manager download path stages bytes through the same blobstore.
		filesSvc.SetPresigner(store)
		// Wire the deleter for the file-transfers GC (same Store satisfies the
		// files.ObjectDeleter interface via its Delete method).
		fileTransfersGCWorker = files.NewFileTransfersGCWorker(pool, store, logger)

		var backupCmd backup.Commander
		if cfg.Agent.SigningPrivateKey != "" {
			signer, _ := agentcmd.NewSigner(cfg.Agent.SigningPrivateKey)
			// Backup/restore commands run synchronously on the agent today (the
			// PHP backup walks the site, chunk-encrypts, and PUTs to S3 inline
			// before responding). On real sites that easily exceeds the snappy
			// 30s update timeout — so we build a SEPARATE SSRF-hardened client
			// with a much longer per-attempt cap just for the backup commander.
			// MaxRetries is 0: the agent's JWT jti is single-use (DoOnce already
			// enforces no auto-retry), and the River job mints a fresh JWT on
			// the next attempt.
			backupSSRFClient := httpclient.New(httpclient.Config{
				Timeout:    cfg.Backup.HTTPTimeout,
				MaxRetries: 0,
			})
			backupCmd = agentcmd.NewClient(backupSSRFClient, signer)
		} else {
			backupCmd = disabledBackupCommander{}
		}
		backupRepo := backup.NewRepo(pool)
		backupSvc = backup.NewService(backupRepo, newBackupSiteLookup(siteSvc), nil, store, clock, backup.Config{
			PresignTTL:         cfg.Backup.PresignTTL,
			RetentionDays:      cfg.Backup.RetentionDays,
			MonthlyArchiveKeep: cfg.Backup.MonthlyArchiveKeep,
		})
		backupSvc.SetHub(backupHub)
		// m16 — Restore Runs + Logs: wire the restore-run repo into the backup
		// service so CreateRestore + RecordProgress persist durable run entities.
		backupSvc.SetRestoreRunStore(backup.NewRestoreRunRepo(pool))
		cpBaseURL := os.Getenv("WPMGR_PUBLIC_BASE_URL")
		// River's default per-job context deadline is 60s — far too short for a
		// real-site backup. Override with the configured backup HTTPTimeout plus
		// a 2-minute buffer so the http.Client's per-attempt timeout (which has
		// a clearer "awaiting headers" diagnostic) fires first when the agent
		// genuinely stalls.
		backupJobTimeout := cfg.Backup.HTTPTimeout + 2*time.Minute
		backupWorker = backup.NewBackupWorker(backupSvc, backupCmd, auditRec, logger, cpBaseURL, backupJobTimeout)
		restoreWorker = backup.NewRestoreWorker(backupSvc, backupCmd, auditRec, logger, cpBaseURL, backupJobTimeout)
		gcWorker = backup.NewGCWorker(backupSvc, logger)
		scheduleWorker = backup.NewScheduleWorker(backupSvc, logger)
		scheduleWorker.SetPool(pool)
		// M5.6 progress watchdog: 120s stall threshold (longest natural silent
		// gap in the phpbu pipeline is age-encrypt for a multi-GB site).
		progressWatchdog = backup.NewProgressWatchdogWorker(backupSvc, 120*time.Second, logger)
		backupH = backup.NewHandler(backupSvc, backupHub, auditRec)
		backupAgentH = backup.NewAgentHandler(backupSvc, auditRec)
		restoreRunH = backup.NewRestoreRunHandler(backupSvc)
		// Wire the auth service as the UserDirectory so restore run DTOs resolve
		// triggered_by UUIDs to human-readable email + name.
		restoreRunH.SetUserDirectory(authSvc)
		// M17 — Schedule run queue (upcoming + past history).
		scheduleRunH = backup.NewScheduleRunHandler(backupSvc)
		scheduleRunH.SetUserDirectory(authSvc)
		// Wire the schedule-run store into the backup service so the scheduler
		// materializes run rows and the reconciliation hooks update them.
		backupSvc.SetScheduleRunStore(backup.NewScheduleRunRepo(pool))
		// M6 / Track 4: agent-supplied inspection artifact fetcher. Streams the
		// ordered chunks of the manifest's `sql-inspection.json` entry from the
		// blobstore and validates the result is JSON. V0 agents ship the report
		// as plaintext chunks (ENCRYPT_CHUNKS=false), so no age decryption is
		// performed here. Cache + Enqueuer stay nil in V1 — legacy snapshots
		// (no inspection entry) return 503 `inspection_unwired` until the
		// CP-side cache backend and the SqlInspectLegacy plaintext source land.
		manifestInspectionFetcher := backup.NewManifestInspectionFetcher(store, backupRepo)
		inspectionDeps = backup.InspectionDeps{
			ManifestFetch: manifestInspectionFetcher,
			Logger:        logger,
		}
		// ADR-037 Sprint 1, 1D — environment fingerprint. Reuses the SQL-
		// inspection fetcher adapter (it's artifact-agnostic — concatenates
		// chunk ciphertext and probes JSON) for the agent-shipped
		// environment.json manifest entry.
		backupH.SetEnvironmentFetcher(manifestInspectionFetcher)
		// M6 / Track 4: SQL inspection legacy parser worker. V1 wires nil for
		// both InspectionPlaintextSource (no agent-side decrypted-dump endpoint
		// yet) and InspectionCacheWriter (no CP-side cache backend yet). The
		// worker.Work method short-circuits with a stable error in that case so
		// any enqueue surfaces a clear River failure metric rather than silently
		// looping. The handler's GET path remains operational: snapshots whose
		// manifest carries an agent-supplied inspection artifact resolve via
		// the ManifestInspectionFetcher path; legacy snapshots return 503
		// "inspection_unwired" until the source/cache deps are filled in.
		sqlInspectLegacyWorker = backup.NewSqlInspectLegacyWorker(nil, nil, logger)
		logger.Info("backups enabled", slog.String("s3_bucket", cfg.S3.Bucket))
	} else {
		logger.Warn("WPMGR_S3_BUCKET is empty: backup/restore endpoints are disabled")
	}

	// ADR-036 P1: per-site destination service + presign registry. Always wired
	// even when backups are disabled so the destinations CRUD is reachable for
	// configuration ahead of enabling backups. The registry is bound to the
	// backup service via SetRegistry below.
	siteDestRepo := sitedestination.NewRepo(pool)
	// ADR-045: refuse to boot in production without a stable age secret. An empty
	// secret yields a fresh ephemeral key on every restart, which would orphan
	// every stored SMTP password and site-destination secret (mirrors the
	// session-secret guard).
	if cfg.IsProduction() && strings.TrimSpace(os.Getenv("WPMGR_SITE_DEST_AGE_SECRET")) == "" {
		return fmt.Errorf("WPMGR_SITE_DEST_AGE_SECRET is required in production: an empty secret uses an ephemeral key that orphans stored SMTP passwords and site-destination secrets on restart")
	}
	siteDestAgeID, err := sitedestination.NewAgeIdentity(os.Getenv("WPMGR_SITE_DEST_AGE_SECRET"))
	if err != nil {
		return fmt.Errorf("site destination age identity: %w", err)
	}
	siteDestSvc := sitedestination.NewService(siteDestRepo, siteDestAgeID, logger)
	siteDestH := sitedestination.NewHandler(siteDestSvc, auditRec)

	// ADR-045 — transactional mailer (UI-configured instance SMTP) + the SMTP
	// settings domain. The mailer resolves its transport from the smtp_settings
	// DB row first (age-decrypting the password with the shared cryptbox
	// identity), falling back to the env SMTP config as a bootstrap default.
	emailRenderer, err := mailer.NewTemplateRenderer()
	if err != nil {
		return fmt.Errorf("email templates: %w", err)
	}
	emailResolver := mailer.NewDBResolver(pool, siteDestAgeID, mailer.EnvSMTP{
		Host:     cfg.SMTP.Host,
		Port:     cfg.SMTP.Port,
		Username: cfg.SMTP.Username,
		Password: cfg.SMTP.Password,
		From:     cfg.SMTP.From,
		TLSMode:  cfg.SMTP.TLSMode,
	})
	supportEmail := os.Getenv("WPMGR_SUPPORT_EMAIL")
	if supportEmail == "" {
		supportEmail = "support@wpmgr.app"
	}
	mailerSvc := mailer.NewService(emailResolver, emailRenderer, pool, os.Getenv("WPMGR_PUBLIC_BASE_URL"), supportEmail, logger)
	sendEmailWorker := mailer.NewSendEmailWorker(mailerSvc)
	smtpSettingsSvc := settings.NewService(settings.NewRepo(pool), siteDestAgeID, mailerSvc, logger)
	smtpSettingsH := settings.NewHandler(smtpSettingsSvc, auditRec)

	// m59 — per-site email management. Shares the same age identity as the
	// instance SMTP settings (siteDestAgeID). The agent command client is wired
	// post-River-start when the commander supports the email command verbs.
	// The SSE publisher (siteEventsPub) is wired below after it is constructed
	// (line ~711) via SetPublisher — same deferred-wiring pattern as SetAgentClient.
	emailRepo := email.NewRepo(pool)
	emailSvc := email.NewService(emailRepo, siteDestAgeID, logger)
	emailH := email.NewHandler(emailSvc, auditRec)
	// m61: set the public base URL on the email handler so GET config responses
	// can include the webhook_url field for the UI.
	emailPublicBase := os.Getenv("WPMGR_PUBLIC_BASE_URL")
	if emailPublicBase != "" {
		emailH.SetPublicBase(emailPublicBase)
	}
	// Phase 3: agent log ingest handler + retention GC worker.
	emailAgentH := email.NewAgentHandler(emailSvc)
	emailLogGCWorker := email.NewEmailLogGCWorker(emailSvc, logger)
	// m61: webhook handler — now safe to mount (cross-tenant forgery fixed).
	// Uses the same svc and publicBase; no instance-wide signing keys.
	emailWebhookH := email.NewWebhookHandler(emailSvc, emailPublicBase, logger)

	// m63 — agency clients. Stateless service + handler; no background workers.
	clientRepo := clientpkg.NewRepo(pool)
	clientSvc := clientpkg.NewService(clientRepo)
	clientH := clientpkg.NewHandler(clientSvc, auditRec)

	// m64 — white-label client reports. Object storage is required to store HTML/PDF
	// blobs and mint presigned URLs. When S3 is not configured the service degrades
	// gracefully (GenerateNow returns 503 "object_storage_required").
	var reportBlobStore reportpkg.BlobStorer
	if cfg.S3.Enabled() {
		rs, rerr := blobstore.New(blobstore.Config{
			Endpoint:       cfg.S3.Endpoint,
			Region:         cfg.S3.Region,
			Bucket:         cfg.S3.Bucket,
			AccessKey:      cfg.S3.AccessKey,
			SecretKey:      cfg.S3.SecretKey,
			ForcePathStyle: cfg.S3.ForcePathStyle,
		})
		if rerr != nil {
			return fmt.Errorf("report blobstore init: %w", rerr)
		}
		reportBlobStore = rs
	}
	reportRepo := reportpkg.NewRepo(pool)
	reportSvc := reportpkg.NewService(reportRepo, reportBlobStore)
	reportHTMLRenderer, rerr := reporthtml.NewRenderer()
	if rerr != nil {
		return fmt.Errorf("report html renderer init: %w", rerr)
	}
	reportPDFRenderer := reportpdf.NewFpdfRenderer()
	reportH := reportpkg.NewHandler(reportSvc, auditRec)

	if backupSvc != nil {
		registry := blobstore.NewRegistry(nil, siteDestSvc) // defaultStore wired below
		// Bind the legacy CP-global store as the registry's default. Built
		// fresh from cfg.S3 because the original `store` variable is only in
		// scope inside the `if cfg.S3.Enabled()` block above. When backups
		// are enabled, S3 IS configured, so this rebuild always succeeds.
		defStore, derr := blobstore.New(blobstore.Config{
			Endpoint:       cfg.S3.Endpoint,
			Region:         cfg.S3.Region,
			Bucket:         cfg.S3.Bucket,
			AccessKey:      cfg.S3.AccessKey,
			SecretKey:      cfg.S3.SecretKey,
			ForcePathStyle: cfg.S3.ForcePathStyle,
		})
		if derr != nil {
			return fmt.Errorf("registry default store: %w", derr)
		}
		registry = blobstore.NewRegistry(defStore, siteDestSvc)
		backupSvc.SetRegistry(&registryAdapter{r: registry})
		// M5.7 P4: wire the manifest index writer so SubmitManifest writes
		// tenant/<tenantID>/site/<siteID>/backup/<snapshotID>/manifest.json
		// via the same CP-global store used for presigning. Best-effort:
		// failures are logged and never fail the backup. Uses defStore (the
		// rebuilt CP-global *blobstore.Store) which satisfies IndexPutter.
		backupSvc.SetIndexPutter(defStore)
		// Wire the manifest deleter so snapshot deletions (operator delete + GC
		// prune) also remove the corresponding manifest.json object. Uses the same
		// defStore which satisfies IndexDeleter via its Delete method.
		backupSvc.SetIndexDeleter(defStore)
	}

	// M5/M6 uptime monitoring: the uptime metrics store, the SSRF-hardened
	// probe, the alert dispatcher (email via go-mail/ADR-029 + signed webhook
	// over the SSRF client), and the tenant-scoped uptime repo/service/handler.
	// The probe worker runs on a periodic River job; it writes time-series to
	// the metrics store, refreshes each site's Postgres health_status, and
	// fires downtime/recovery alerts on transition (de-duped).
	//
	// Backend selection (M6, GCP cutover): when WPMGR_CLICKHOUSE_ADDR is set we
	// use the original ClickHouse store (ADR-028). When it is empty we fall
	// back to the Postgres-backed store added in the M6 migration. Postgres is
	// the M6 default because the GCP managed deployment does not run a
	// ClickHouse cluster — before this fix the empty addr produced a disabled
	// store whose writes/queries no-op'd, so the dashboard had no status, no
	// graph, and no cert data.
	var metricsStore metrics.Store
	if cfg.ClickHouse.Enabled() {
		s, err := metrics.New(ctx, metrics.Config{
			Addr:     cfg.ClickHouse.Addr,
			Database: cfg.ClickHouse.Database,
			Username: cfg.ClickHouse.Username,
			Password: cfg.ClickHouse.Password,
		}, logger)
		if err != nil {
			return err
		}
		metricsStore = s
	} else {
		metricsStore = metrics.NewPostgres(pool, logger)
	}
	defer func() { _ = metricsStore.Close() }()

	uptimeRepo := uptime.NewRepo(pool)
	uptimeSiteAdapter := newUptimeSiteAdapter(siteSvc)
	uptimeProber := uptime.NewProber(ssrfClient, cfg.Uptime.ProbeTimeout)
	var uptimeMailer uptime.Mailer
	if cfg.SMTP.Enabled() {
		uptimeMailer = uptime.NewSMTPMailer(cfg.SMTP, logger)
		logger.Info("uptime alert email enabled", slog.String("smtp_host", cfg.SMTP.Host))
	} else {
		uptimeMailer = uptime.NewNoopMailer(logger)
		logger.Warn("WPMGR_SMTP_HOST is empty: uptime alert emails disabled (webhooks still fire)")
	}
	webhookPoster := uptime.NewSSRFWebhookPoster(ssrfClient)
	uptimeDispatcher := uptime.NewDispatcher(uptimeMailer, webhookPoster, auditRec, logger)
	uptimeWorker := uptime.NewProbeWorker(uptimeRepo, uptimeProber, metricsStore, uptimeDispatcher, uptimeSiteAdapter, logger, cfg.Uptime.ProbeConcurrency, cfg.Uptime.DownThreshold)
	uptimeSvc := uptime.NewService(uptimeRepo, metricsStore, uptimeSiteAdapter)
	uptimeH := uptime.NewHandler(uptimeSvc, auditRec)
	// Wire the metrics store into the site service so site-list uptime fields
	// are sourced from the active backend (ClickHouse or Postgres) rather than
	// a direct read of site_uptime_probes (which is empty on ClickHouse installs).
	siteSvc.SetUptimeStore(metricsStore)

	// P4b — cron kick: periodically fire a GET to wp-cron.php for all enrolled
	// sites so fully page-cached sites boot PHP and drain WP-Cron even with zero
	// PHP-booting organic traffic. Reuses the SSRF-hardened ssrfClient (ADR-009).
	// Disabled via WPMGR_CRON_KICK_ENABLED=false.
	var cronKickWorker *uptime.CronKicker
	var cronKickInterval time.Duration
	if cfg.Uptime.CronKickEnabled {
		cronKickWorker = uptime.NewCronKicker(
			uptimeRepo,
			ssrfClient,
			cfg.Uptime.CronKickTimeout,
			cfg.Uptime.CronKickConcurrency,
		)
		cronKickWorker.SetLogger(logger)
		cronKickInterval = cfg.Uptime.CronKickInterval
		if cronKickInterval <= 0 {
			cronKickInterval = 5 * time.Minute
		}
		logger.Info("uptime cron kick enabled",
			slog.Duration("interval", cronKickInterval),
			slog.Duration("timeout", cfg.Uptime.CronKickTimeout),
			slog.Int("concurrency", cfg.Uptime.CronKickConcurrency),
		)
	} else {
		logger.Info("uptime cron kick disabled (WPMGR_CRON_KICK_ENABLED=false)")
	}

	// ADR-037 Sprint 2 — diagnostics + php-error monitor repo. Built here
	// (before River) so the phpErrorsGCWorker can be registered at River start.
	// The service, handler, and enqueuer wiring continues after River starts.
	diagnosticsRepo := diagnostics.NewRepo(pool)

	// River: connection-health worker pool plus the M3 update-task workers and the
	// M4 backup/restore/GC/scheduler workers. The health job marks a site
	// unreachable when its agent heartbeat goes stale (freshness-based). The M5
	// probe job actively probes every enrolled site (~60s). Update tasks run on
	// per-tenant queue shards so one tenant cannot starve another. Started below,
	// stopped on shutdown.
	siteRepo := site.NewRepo(pool)
	// M16 Phase A — wire the billing gate onto THIS repo instance too (the one
	// connSvc below is constructed with): CreatePending/ConsumeEnrollmentCode/
	// Restore are served by siteRepo, not by siteSvc's own repo. See the
	// SetBillingGate(billingSvc) call above for the full wiring-gotcha note.
	site.SetBillingGate(siteRepo, billingSvc)
	healthChecker := site.NewHealthChecker(siteRepo, cfg.Agent.StaleAfter, cfg.Agent.SignatureSkew)

	// M21 — Live enrollment + connection lifecycle (ADR-038/039/040/041).
	// Event bus: tenant-keyed SSE Hub + durable site_events journal + LISTEN
	// fan-out. The connection service is the single owner of every state
	// transition; the sweeper is the only caller of the degraded/disconnected
	// transitions. The Listener goroutine is started below (after the pool is up).
	siteEventsHub := siteevents.NewHub()
	siteEventsPub := siteevents.NewPublisher(pool, clock)
	// m59 Phase 4 SSE: wire the email publisher now that siteEventsPub is
	// available. Mirrors the SetAgentClient deferred-wiring pattern used for
	// the agent command client below.
	emailSvc.SetPublisher(siteEventsPub)
	emailH.SetPublisher(siteEventsPub)
	emailAgentH.SetPublisher(siteEventsPub)
	// Revoke-token minter (Phase 6 finding B): reuse the agentcmd Ed25519 signer
	// to sign the "revoke" instruction. Keep it a true nil interface when the CP
	// has no signing key, so connService falls back to an unsigned instruction
	// rather than calling Mint on a typed-nil *Signer.
	var revokeMinter site.RevokeTokenMinter
	if cmdSigner, serr := agentcmd.NewSigner(cfg.Agent.SigningPrivateKey); serr == nil {
		revokeMinter = cmdSigner
	}
	connSvc := site.NewConnectionService(siteRepo, validator, auditRec, siteEventsPub, clock, revokeMinter)
	// Inject the lifecycle service into the enroll branch (site-bound consume)
	// and the agent heartbeat/disconnect handler.
	siteSvc.SetConnectionService(connSvc)
	agentH.SetLifecycleSink(site.NewAgentLifecycleAdapter(connSvc))
	// Timeout sweeper (every 15s) + site_events prune (every minute).
	// M58: wire env-configurable thresholds (WPMGR_CONN_DEGRADE_AFTER,
	// WPMGR_CONN_DISCONNECT_AFTER, WPMGR_CONN_DEGRADE_MISS_THRESHOLD) and the
	// consecutive-miss counter incrementer so the sweeper uses hysteresis.
	// 0.44.0: wire the active-verify dialer (WPMGR_SWEEP_ACTIVE_VERIFY,
	// WPMGR_SWEEP_VERIFY_TIMEOUT, WPMGR_SWEEP_VERIFY_CONCURRENCY).
	siteSweeper := site.NewSweeper(siteRepo, connSvc.(site.SweeperTransitioner), siteEventsPub)
	if missInc, ok := siteRepo.(site.MissIncrementer); ok {
		siteSweeper.SetMissIncrementer(missInc)
	}
	if cfg.Conn.DegradeAfter > 0 || cfg.Conn.DisconnectAfter > 0 {
		siteSweeper.SetThresholds(cfg.Conn.DegradeAfter, cfg.Conn.DisconnectAfter, 0)
	}
	if cfg.Conn.DegradeMissThreshold > 0 {
		siteSweeper.SetDegradeMissThreshold(cfg.Conn.DegradeMissThreshold)
	}
	// 0.44.0 active verify: wire the agent command client as the dialer when the
	// CP signing key is configured (same guard as the recheck handler). The
	// ConnectionService satisfies HeartbeatRecorder so RecordHeartbeat is reused
	// exactly as the recheck_handler does (ADR-039: single recovery writer).
	siteSweeper.SetActiveVerify(cfg.Conn.ActiveVerify)
	if cfg.Conn.VerifyTimeout > 0 {
		siteSweeper.SetVerifyTimeout(cfg.Conn.VerifyTimeout)
	}
	if cfg.Conn.VerifyConcurrency > 0 {
		siteSweeper.SetVerifyConcurrency(cfg.Conn.VerifyConcurrency)
	}
	if rec, ok := connSvc.(site.HeartbeatRecorder); ok {
		siteSweeper.SetHeartbeatRecorder(rec)
	}
	if cfg.Conn.ActiveVerify {
		if cmdSigner, serr := agentcmd.NewSigner(cfg.Agent.SigningPrivateKey); serr == nil {
			sweepVerifier := agentcmd.NewClient(ssrfClient, cmdSigner)
			siteSweeper.SetVerifier(sweepVerifier)
			logger.Info("sweep active verify enabled")
		} else {
			logger.Warn("sweep active verify disabled: CP signing key not configured or invalid; passive sweeper mode active")
		}
	}
	siteSweeper.SetLogger(logger)
	siteSweepWorker := site.NewSweepWorker(siteSweeper)
	siteEventPruneWorker := site.NewEventPruneWorker(siteSweeper)
	// SSE endpoint + the dedicated LISTEN listener.
	siteEventsH := siteevents.NewHandler(pool, siteEventsHub)
	siteEventsListener := siteevents.NewListener(pool, siteEventsHub, logger)
	go siteEventsListener.Run(ctx)

	// S1.1 (D) — PHP-error retention GC. Always wired (the table always exists);
	// runs once per hour sweeping rows older than 30 days.
	phpErrorsGCWorker := diagnostics.NewErrorsGCWorker(diagnosticsRepo, 30*24*time.Hour, logger)

	// S3 — Malware / File-Integrity Scan. Workers are built here (before River)
	// with a nil enqueuer; the enqueuer is wired post-River-start via SetEnqueuer
	// so the worker can re-enqueue partial iterations using the started River client.
	scanRepo := scan.NewRepo(pool)
	scanSvc := scan.NewService(scanRepo, auditRec)
	scanH := scan.NewHandler(scanSvc)
	scanChecksums := scan.NewChecksumProvider(scanRepo, ssrfClient)
	scanSiteAdapter := newScanSiteAdapter(siteSvc)
	var scanWorker *scan.ScanRunWorker
	var scanHashGCWorker *scan.HashGCWorker
	if scanCmd, ok := commander.(scan.AgentScanClient); ok {
		scanWorker = scan.NewScanRunWorker(scanRepo, scanChecksums, scanCmd, scanSiteAdapter, nil, auditRec, logger)
		scanHashGCWorker = scan.NewHashGCWorker(scanRepo, 24*time.Hour, logger)
		scanSvc.SetAgentClient(scanCmd, scanSiteAdapter)
		logger.Info("scan agent client wired")
	} else {
		logger.Warn("scan agent client not wired: CP->agent commander unavailable (signing key empty?)")
	}

	// M23 — Media Optimizer (ADR-043). The service + handlers are built here
	// (before River) so the dashboard GETs work as soon as the agent syncs; the
	// EncodeArgs enqueuer is wired post-River-start (insert-only in the API; the
	// separate media-encoder process runs the actual encoders). NO encoder import
	// reaches this binary: the API only client.Inserts model.EncodeArgs (a pure-Go
	// River job type).
	mediaRepo := mediarepo.NewRepo(pool)
	var mediaStore *blobstore.Store
	if cfg.S3.Enabled() {
		ms, merr := blobstore.New(blobstore.Config{
			Endpoint:       cfg.S3.Endpoint,
			Region:         cfg.S3.Region,
			Bucket:         cfg.S3.Bucket,
			AccessKey:      cfg.S3.AccessKey,
			SecretKey:      cfg.S3.SecretKey,
			ForcePathStyle: cfg.S3.ForcePathStyle,
		})
		if merr != nil {
			return fmt.Errorf("media blobstore init: %w", merr)
		}
		mediaStore = ms
	}
	mediaCPBaseURL := os.Getenv("WPMGR_PUBLIC_BASE_URL")
	mediaSvc := mediaservice.NewService(mediaRepo, mediaStore, siteEventsPub, auditRec, clock, mediaservice.Config{
		PresignTTL:    cfg.Backup.PresignTTL,
		CPBaseURL:     mediaCPBaseURL,
		RatePerSite:   200,
		RatePerTenant: 1000,
		RateWindow:    time.Minute,
	}, logger)
	mediaSiteAdapterImpl := newMediaSiteAdapter(siteSvc)
	// The media_optimize/sync/restore/delete dispatch is a fire-and-forget ack:
	// the agent should ack fast (it offloads the heavy enumerate/upload work), but
	// a large bulk batch on a slow host can make the ack take longer than the 30s
	// update http_timeout the shared `commander` uses. That 30s `http.Client.Timeout`
	// is what surfaced as "Client.Timeout exceeded" and drove the spurious failJob
	// over the whole batch. Build a DEDICATED commander on its own SSRF client with
	// a defensive (bounded) 120s timeout so a slightly slow ack does not spuriously
	// time out; the success/fail race is independently closed by the guarded
	// FinalizeJobAgent + failJob, this just stops the timeout firing in the first
	// place. Falls back to the shared commander when no dedicated signer exists.
	var mediaCommander mediaservice.AgentMediaClient
	if cmdSigner != nil {
		mediaSSRFClient := httpclient.New(httpclient.Config{
			Timeout:    120 * time.Second,
			MaxRetries: cfg.Update.HTTPRetries,
		})
		mediaCommander = agentcmd.NewClient(mediaSSRFClient, cmdSigner)
	} else if mc, ok := commander.(mediaservice.AgentMediaClient); ok {
		mediaCommander = mc
	}
	if mediaCommander != nil {
		mediaSvc.SetAgentClient(mediaCommander, mediaSiteAdapterImpl)
		logger.Info("media optimizer agent client wired")
	} else {
		logger.Warn("media optimizer agent client not wired: CP->agent commander unavailable (signing key empty?)")
	}
	mediaH := mediahandler.NewHandler(mediaSvc)
	mediaAgentH := mediahandler.NewAgentHandler(mediaSvc)

	// ---------------------------------------------------------------------------
	// M72 — Site Screenshots. The capture worker runs in cmd/media-encoder (the
	// only binary with headless Chromium). The API binary only client.Inserts
	// screenshot.CaptureArgs. The screenshot blobstore reuses the same S3/GCS
	// bucket as the Media Optimizer (mediaStore); when S3 is not configured,
	// screenshots are disabled (service is built with nil enqueuer/store).
	// ---------------------------------------------------------------------------
	screenshotRepo := screenshot.NewRepo(pool)
	var screenshotSvc *screenshot.Service
	var screenshotH *screenshot.Handler
	if mediaStore != nil {
		screenshotSvc = screenshot.NewService(screenshotRepo, mediaStore, nil, nil) // waker wired below after mediaWaker is built
		// Wire the screenshotadapter enricher so repo.List populates screenshot
		// fields (status, presigned URL 1x/2x, captured_at) on every site list call.
		screenshotEnricher := screenshotadapter.New(screenshotRepo, mediaStore)
		// Wire onto the SERVICE's own repo — siteSvc.List() is served by the repo
		// instance held inside siteSvc (constructed at NewService), NOT by the
		// separate siteRepo created later for the connection/health machinery.
		// Wiring the enricher onto siteRepo silently no-ops the list enrichment.
		siteSvc.SetScreenshotEnricher(screenshotEnricher)
		logger.Info("screenshots enabled: enricher wired, capture queue: site_screenshot")
	} else {
		// S3 not configured: wire a no-store service so the handler returns 501 cleanly.
		screenshotSvc = screenshot.NewService(screenshotRepo, nil, nil, nil)
		logger.Warn("WPMGR_S3_BUCKET is empty: site screenshots disabled")
	}
	screenshotH = screenshot.NewHandler(screenshotSvc, siteRepo)

	// ---------------------------------------------------------------------------
	// Performance Suite (ADR-046, Phase 6): RUCSS engine/worker + perf control
	// plane. The RUCSS used-CSS objects + the agent-posted HTML/CSS source bundles
	// reuse the same blobstore as the Media Optimizer (mediaStore). The RUCSS
	// worker is constructed BEFORE startRiver (it needs the service, not the
	// client); its enqueuer is wired after startRiver returns.
	// ---------------------------------------------------------------------------
	rucssRepo := rucssrepo.NewRepo(pool)
	// rucssStore + bundle store are the same blobstore the Media Optimizer uses.
	// A nil mediaStore (S3 not configured) leaves RUCSS degraded: the worker is
	// not registered and the agent ingest endpoint keeps serving full CSS.
	var (
		rucssSvc         *rucssservice.Service
		rucssBundleStore perf.RucssBundleStore
		rucssWorker      *rucssworker.Worker
		rucssSweepWorker *rucssworker.RucssSweepWorker
	)
	if mediaStore != nil {
		rucssSvc = rucssservice.NewService(rucssRepo, mediaStore, clock, logger)
		rucssBundleStore = mediaStore
		// Closing-loop re-warm: after the worker stores a result it purges + re-
		// computes the URL so the agent re-renders a CP cache HIT and caches the
		// OPTIMIZED page (an async RUCSS pipeline otherwise leaves the un-optimized
		// 202 render cached forever). Built from the same agent commander + site
		// lookup the perf service uses; nil when the commander can't push commands
		// (signing key empty) — the worker then relies on the organic-visit backstop.
		var rucssReheat rucssworker.CacheReheater
		if perfCmd, ok := commander.(perf.AgentPerfClient); ok {
			rucssReheat = perf.NewRucssReheater(perfCmd, newPerfSiteAdapter(siteSvc), logger)
			logger.Info("rucss reheat re-warm enabled")
		} else {
			// Loud, not silent: without this the post-compute cache re-warm is a
			// no-op and optimized pages only land via organic re-visits.
			logger.Warn("rucss reheat re-warm DISABLED: agent command client unwired (signing key empty?)")
		}
		rucssWorker = rucssworker.NewWorker(
			rucssSvc,
			perf.NewRucssSourceFetcher(rucssBundleStore),
			rucssRepo,
			siteEventsPub,
			rucssReheat,
			logger,
		)
		// FIX 1 backstop: reap orphaned source bundles (page HTML) under rucss-src/
		// directly via the same blobstore the bundles live in.
		rucssSweepWorker = rucssworker.NewRucssSweepWorker(mediaStore, rucssworker.RucssSweepMaxAge, logger)
	}

	perfRepo := perf.NewRepo(pool)
	perfSvc := perf.NewService(perfRepo, siteDestAgeID, siteEventsPub, logger)
	perfSiteAdapterImpl := newPerfSiteAdapter(siteSvc)
	if perfCmd, ok := commander.(perf.AgentPerfClient); ok {
		perfSvc.SetAgentClient(perfCmd, perfSiteAdapterImpl)
		logger.Info("perf agent client wired")
	} else {
		logger.Warn("perf agent client not wired: CP->agent commander unavailable (signing key empty?)")
	}
	// CDN purge is best-effort over the shared SSRF-hardened client.
	perfSvc.SetCDNPurger(perf.NewCDNPurger(ssrfClient))
	// Phase 2.2 — backup recency check for drop/empty advisory warning.
	if backupSvc != nil {
		perfSvc.SetBackupChecker(newBackupCheckerAdapter(backupSvc))
	}

	// M38 — CP-owned db-clean scheduling workers.
	// Both workers are always registered so scheduled auto-clean works whenever
	// the signing key is configured. The schedule worker's enqueuer is wired
	// after River starts (mirrors the backup ScheduleWorker pattern).
	dbCleanWorker := perf.NewDBCleanWorker(perfSvc, logger)
	dbCleanScheduleWorker := perf.NewDBCleanScheduleWorker(perfSvc, logger)
	// M39 — watchdog for stalled db_clean/db_scan jobs (always wired; no
	// signing key required since it only reads perf_config + emits SSE).
	dbCleanWatchdogWorker := perf.NewDBCleanWatchdogWorker(perfSvc, logger)

	// P3.8 — watchdog for stalled db_orphan_delete jobs (always wired; no
	// signing key required — reads perf_config + emits SSE only). Runs every
	// 2 minutes (same as db_clean watchdog); stall threshold is 5 minutes.
	dbOrphanDeleteWatchdogWorker := perf.NewDBOrphanDeleteWatchdogWorker(perfSvc, logger)

	// M42 — DB-size history GC: sweeps site_db_size_history rows older than
	// 120 days, once per day. Always wired (no signing key required).
	dbSizeHistoryGCWorker := perf.NewDBSizeHistoryGCWorker(perfRepo, logger)

	// M52 / #162 — cache hit-ratio history GC: sweeps
	// site_cache_hit_ratio_history rows older than 120 days, once per day.
	// Always wired (no signing key required).
	cacheHitRatioHistoryGCWorker := perf.NewCacheHitRatioHistoryGCWorker(perfRepo, logger)

	// m68 — Object Cache (P0+P1). Shares the same age identity as the
	// Performance Suite (siteDestAgeID). The agent command client is wired only
	// when the CP signing key is available (same guard as every other domain that
	// pushes signed commands). A nil cmdClient causes the service to return an
	// error on any command attempt — the handler surfaces it as a warning header
	// (non-domain error path) consistent with other perf commands.
	ocRepo := objectcache.NewRepo(pool)
	var ocCmdClient *agentcmd.Client
	if cmdSigner != nil {
		ocCmdClient = agentcmd.NewClient(ssrfClient, cmdSigner)
	}
	ocSvc := objectcache.NewService(ocRepo, siteDestAgeID, ocCmdClient, perfSiteAdapterImpl, siteEventsPub)
	ocH := objectcache.NewHandler(ocSvc, auditRec)
	ocGCWorker := objectcache.NewObjectCacheStatsHistoryGCWorker(ocRepo, logger)

	// M56 — Real User Monitoring (RUM).
	// The Postgres store is always wired; ClickHouse is a Phase 2+ opt-in
	// (mirroring the internal/metrics dual-backend pattern).
	rumStore := rum.NewStorePostgres(pool)
	rumBeaconRepo := rum.NewBeaconKeyRepo(pool)
	rumRetention := rum.DefaultRetention(cfg)
	rumGCWorker := rum.NewRumGCWorker(rumStore, rumRetention, logger)
	rumRollupWorker := rum.NewRumRollupWorker(rumStore, logger)
	// Wire the site event publisher so the ingest handler can emit the throttled
	// rum.rollup_updated SSE after each beacon commit. siteEventsPub is wired
	// before this point (line ~701) and satisfies rum.EventPublisher.
	rumH := rum.NewHandlerWithPublisher(rumStore, rumBeaconRepo, siteEventsPub, logger)

	// m64 — build report workers now that rumStore and other sources are available.
	// Both workers are nil when S3 is not configured (reports require object storage).
	// portalReportSources is hoisted here so the portal handler can reference it
	// for /summary; it is set only when reportBlobStore != nil.
	var portalReportSources *reportpkg.Sources
	var reportGenerateWorker *reportpkg.GenerateWorker
	reportScheduleScanWorker := reportpkg.NewScheduleScanWorker(reportRepo, logger)
	if reportBlobStore != nil {
		// Build aggregator Sources — adapters that bridge the report aggregator's
		// from/to range API onto the existing metrics/rum/email stores.
		reportSources := reportpkg.Sources{
			ListClientSites: func(ctx context.Context, tenantID, clientID uuid.UUID) ([]site.Site, error) {
				return siteSvc.List(ctx, site.ListInput{TenantID: tenantID, ClientID: &clientID})
			},
			QueryUptimeAggregateRange: func(ctx context.Context, tenantID, siteID uuid.UUID, from, to time.Time) (metrics.Aggregate, error) {
				return metricsStore.QueryAggregate(ctx, tenantID, siteID, to.Sub(from))
			},
			QueryUptimeSeriesRange: func(ctx context.Context, tenantID, siteID uuid.UUID, from, to time.Time) ([]metrics.Point, error) {
				buckets := int(to.Sub(from).Hours())
				if buckets < 1 {
					buckets = 1
				}
				if buckets > 720 {
					buckets = 720
				}
				return metricsStore.QuerySeries(ctx, tenantID, siteID, to.Sub(from), buckets)
			},
			QueryUptimeLatest: metricsStore.QueryLatest,
			GetBackupReportStats: func(ctx context.Context, tenantID, siteID uuid.UUID, from, to time.Time) (sqlc.GetBackupReportStatsRow, error) {
				var row sqlc.GetBackupReportStatsRow
				err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
					var qerr error
					row, qerr = sqlc.New(tx).GetBackupReportStats(ctx, sqlc.GetBackupReportStatsParams{
						TenantID: tenantID,
						SiteID:   siteID,
						FromTime: pgtype.Timestamptz{Time: from, Valid: true},
						ToTime:   pgtype.Timestamptz{Time: to, Valid: true},
					})
					return qerr
				})
				return row, err
			},
			GetUpdateReportStats: func(ctx context.Context, tenantID, siteID uuid.UUID, from, to time.Time) ([]sqlc.GetUpdateReportStatsRow, error) {
				var rows []sqlc.GetUpdateReportStatsRow
				err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
					var qerr error
					rows, qerr = sqlc.New(tx).GetUpdateReportStats(ctx, sqlc.GetUpdateReportStatsParams{
						TenantID: tenantID,
						SiteID:   siteID,
						FromTime: pgtype.Timestamptz{Time: from, Valid: true},
						ToTime:   pgtype.Timestamptz{Time: to, Valid: true},
					})
					return qerr
				})
				return rows, err
			},
			GetDailyRollups: rumStore.GetDailyRollups,
			GetFleetStatsBySite: func(ctx context.Context, tenantID uuid.UUID, from, to time.Time, limit int32) ([]email.SiteStatsRow, error) {
				return emailRepo.GetFleetStatsBySite(ctx, tenantID, from, to, limit)
			},
		}
		// FIX-1: build a dedicated SSRF-hardened client for logo fetching.
		// A 5s timeout is enough for an image fetch; retries are off because the
		// worker already retries the whole job on failure (MaxAttempts=3).
		logoSSRFClient := httpclient.New(httpclient.Config{
			Timeout:    5 * time.Second,
			MaxRetries: 0,
		})
		reportGenerateWorker = reportpkg.NewGenerateWorker(reportRepo, reportSvc, reportSources, reportHTMLRenderer, reportPDFRenderer, logoSSRFClient, logger)
		// Share aggregator sources with the portal summary handler (email source
		// is disabled inside SetReportSources — never exposed in the portal).
		portalReportSources = &reportSources
	}

	// m79 — Vulnerability Scanner. The repo, service, and workers are all
	// constructed before River so they can be handed to the riverDeps struct.
	// The rescan enqueuer (which needs a started riverClient) is wired after
	// startRiver returns using vuln.Service.SetEnqueuer (see below).
	//
	// m80 — The API key is now UI-configurable via the superadmin area
	// (instance_settings table, encrypted at rest). VulnFeedKeyService satisfies
	// vuln.APIKeyResolver and is passed as the resolver to NewFeedWorker so the
	// worker resolves the key at job-run time (UI key > env key > no-op).
	// The feed refresh enqueuer is wired into the key service after River starts.
	vulnRepo := vuln.NewRepo(pool)
	adminInstSettingsRepo := admin.NewInstanceSettingsRepo(pool)
	vulnFeedKeySvc := admin.NewVulnFeedKeyService(
		adminInstSettingsRepo,
		siteDestAgeID, // same cryptbox identity used for SMTP + site destinations
		os.Getenv("WPMGR_WORDFENCE_API_KEY"),
		nil, // enqueuer wired below after River starts
		logger,
	)
	vulnFeedWorker := vuln.NewFeedWorker(vulnRepo, pool, nil /*svc: wired below*/, vulnFeedKeySvc, ssrfClient, logger)
	vulnRescanWorker := vuln.NewRescanSiteWorker(nil /*svc: wired below*/, logger)

	riverClient, err := startRiver(ctx, pool.Pool, logger, riverDeps{
		healthChecker:          healthChecker,
		healthInterval:         cfg.Agent.HealthInterval,
		siteSweepWorker:        siteSweepWorker,
		siteEventPruneWorker:   siteEventPruneWorker,
		updateWorker:           updateWorker,
		updateReaperWorker:     updateReaperWorker,
		refreshWorker:          refreshWorker,
		perTenantParallelism:   cfg.Update.PerTenantParallelism,
		backupWorker:           backupWorker,
		restoreWorker:          restoreWorker,
		gcWorker:               gcWorker,
		scheduleWorker:         scheduleWorker,
		progressWatchdog:       progressWatchdog,
		sqlInspectLegacyWorker: sqlInspectLegacyWorker,
		scheduleInterval:       cfg.Backup.ScheduleInterval,
		gcInterval:             cfg.Backup.GCInterval,
		uptimeWorker:           uptimeWorker,
		probeInterval:          cfg.Uptime.ProbeInterval,
		phpErrorsGCWorker:      phpErrorsGCWorker,
		// S3 scan workers (nil when signing key is not configured).
		scanRunWorker:    scanWorker,
		scanHashGCWorker: scanHashGCWorker,
		// ADR-045 — transactional email worker (always wired).
		sendEmailWorker: sendEmailWorker,
		// ADR-046 Performance Suite — RUCSS worker (nil when S3 not configured).
		rucssWorker:        rucssWorker,
		rucssQueueParallel: 4,
		// FIX 1 backstop sweeper (nil when S3 not configured).
		rucssSweepWorker: rucssSweepWorker,
		// M38 — CP-owned db-clean scheduling workers.
		dbCleanWorker:         dbCleanWorker,
		dbCleanScheduleWorker: dbCleanScheduleWorker,
		// M39 — watchdog for stalled db_clean/db_scan jobs.
		dbCleanWatchdogWorker: dbCleanWatchdogWorker,
		// P3.8 — watchdog for stalled db_orphan_delete jobs.
		dbOrphanDeleteWatchdogWorker: dbOrphanDeleteWatchdogWorker,
		// M42 — DB-size history GC (always wired).
		dbSizeHistoryGCWorker: dbSizeHistoryGCWorker,
		// M52 / #162 — cache hit-ratio history GC (always wired).
		cacheHitRatioHistoryGCWorker: cacheHitRatioHistoryGCWorker,
		// m68 — Object Cache stats history GC (always wired; 7-day raw retention).
		ocStatsHistoryGCWorker: ocGCWorker,
		// M56 — RUM GC + rollup workers (always wired).
		rumGCWorker:     rumGCWorker,
		rumRollupWorker: rumRollupWorker,
		// m59 Phase 3 — email log retention GC (always wired).
		emailLogGCWorker: emailLogGCWorker,
		// m62 — org-config propagation + hourly digest workers (always wired).
		emailOrgPropagateWorker: email.NewOrgConfigPropagateWorker(emailSvc, logger),
		emailDigestWorker:       email.NewDigestWorker(emailSvc, logger),
		// m64 — report generation + schedule-scan workers (nil when S3 not configured).
		reportGenerateWorker:     reportGenerateWorker,
		reportScheduleScanWorker: reportScheduleScanWorker,
		// P4b — cron kick (nil when WPMGR_CRON_KICK_ENABLED=false).
		cronKickWorker:   cronKickWorker,
		cronKickInterval: cronKickInterval,
		// m79 — vulnerability scanner workers (always non-nil; feed worker no-ops
		// when WPMGR_WORDFENCE_API_KEY is unset).
		vulnFeedWorker:   vulnFeedWorker,
		vulnRescanWorker: vulnRescanWorker,
		// m82 — file-transfers GC (always non-nil; object deletion is a no-op
		// when S3 is not configured).
		fileTransfersGCWorker: fileTransfersGCWorker,
		// m85 — uptime-probe retention GC (always non-nil).
		uptimeProbeGCWorker: uptimeProbeGCWorker,
		// M16 Phase B — daily billing drift-repair sweep (always wired; the
		// worker's own Reconcile call no-ops cleanly when hosted billing is
		// disabled or no provider is registered).
		billingReconcileWorker: billingReconcileWorker,
	})
	if err != nil {
		return err
	}
	mediaRiverClient, err := newMediaRiverClient(pool.Pool, logger, riverClient, mediaRiverSchema)
	if err != nil {
		return err
	}

	// The enqueuer needs the started River client; the update service needs the
	// enqueuer. Wire them after the client is up. The same enqueuer also serves
	// the post-update inventory-refresh path (via the update Worker) and the
	// operator-facing refresh route on the site handler (via siteRefreshAdapter).
	updateEnqueuer := update.NewRiverEnqueuer(riverClient)
	updateSvc := update.NewService(updateRepo, sitesLookup, updateEnqueuer, validator, clock)
	updateH := update.NewHandler(updateSvc, updateHub, auditRec)
	updateWorker.SetRefreshEnqueuer(updateEnqueuer, refreshDebouncer)
	siteH := site.NewHandler(siteSvc, auditRec, cpPublicKey)
	siteH.SetRefreshEnqueuer(newSiteRefreshAdapter(updateEnqueuer), cfg.Agent.StaleAfter)
	// M21: enable the site-first create + revoke/archive/restore/re-enroll routes.
	siteH.SetConnectionService(connSvc)
	// M58: wire the re-check client when the commander satisfies AgentRechecker
	// (i.e. the CP signing key is configured). commander is *agentcmd.Client when
	// both the SSRF client and the signing key are available.
	if recheckCmd, ok := commander.(site.AgentRechecker); ok {
		siteH.SetRechecker(recheckCmd)
	}
	// M58 rate-limit: per-(tenant,site) in-memory limiter for the Re-check
	// connection endpoint. Wired unconditionally (not gated on the signing key)
	// so the limit applies even in edge-case configurations where the limiter
	// starts before the rechecker is available. The limiter is safe to wire with
	// a nil rechecker — the handler checks rechecker nil before the limit fires.
	recheckLimiter := autologin.NewMemoryLimiter()
	siteH.SetRecheckLimiter(recheckLimiter)
	if backupSvc != nil {
		backupSvc.SetEnqueuer(backup.NewRiverEnqueuer(riverClient))
		// Issue #68 — data-heal: run once at boot (non-blocking) to
		// (a) reconcile duplicate in-flight snapshots so the partial-unique index
		//     applied by migration m75 finds no conflicting rows, and
		// (b) advance any overdue enabled schedules to their next future slot so
		//     the scheduler does not immediately fire stale rows on the first tick.
		go backupSvc.HealOverdueSchedulesAndSnapshots(context.Background(), logger)
	}

	// S3 scan: wire the River enqueuer into the service + worker now that River
	// has started. The scan service needs it for StartRun; the worker needs it
	// to re-enqueue partial iterations.
	scanEnqueuer := scan.NewRiverEnqueuer(riverClient)
	scanSvc.SetEnqueuer(scanEnqueuer)
	if scanWorker != nil {
		scanWorker.SetEnqueuer(scanEnqueuer)
	}

	// m79 — complete the vuln domain wiring now that riverClient is available.
	// The service is the hub: it wires repo + pool + site-adapter + update-creator
	// + rescan-enqueuer. The feed worker and rescan worker are registered in River
	// (via riverDeps above); here we complete their service pointer so they can call
	// through to RescanSite/RescanAll after a feed refresh or on demand.
	vulnRescanEnq := vuln.NewRiverRescanEnqueuer(riverClient)
	vulnFeedRefreshEnq := vuln.NewRiverFeedRefreshEnqueuer(riverClient)
	vulnSiteAdapterImpl := newVulnSiteAdapter(siteSvc)
	vulnSvc := vuln.NewService(vulnRepo, pool, vulnSiteAdapterImpl, updateSvc, vulnRescanEnq, logger)
	vulnFeedWorker.SetService(vulnSvc)
	vulnRescanWorker.SetService(vulnSvc)
	// m80 — wire the feed-refresh enqueuer into the key service so the admin
	// PUT /admin/vuln-feed/key endpoint can trigger an immediate sync.
	vulnFeedKeySvc.SetEnqueuer(vulnFeedRefreshEnq)
	vulnH := vuln.NewHandler(vulnSvc, vulnRescanEnq, auditRec)

	// M23 Media Optimizer: wire the EncodeArgs enqueuer now that River has
	// started. The enqueuer lives in the PURE media package (no encoder import),
	// so this binary still has no CGO dependency.
	// Encoder-owned queues must use mediaRiverClient so the media-encoder sees
	// them in its configured River schema.
	mediaSvc.SetEnqueuer(media.NewRiverEnqueuer(mediaRiverClient))

	// M72 Site Screenshots: wire the River enqueuer into the screenshot service
	// now that the client is started. The site_screenshot queue is registered in
	// cmd/media-encoder only; the API uses SkipUnknownJobCheck so Insert still works.
	if screenshotSvc != nil && mediaStore != nil {
		screenshotEnqueuer := screenshot.NewEnqueuer(mediaRiverClient)
		screenshotSvc.SetEnqueuer(screenshotEnqueuer)
		// Hook into the connection service so the first enrollment triggers a capture.
		if cs, ok := connSvc.(interface {
			SetOnEnrollHook(hook site.OnEnrollHook)
		}); ok {
			cs.SetOnEnrollHook(func(ctx context.Context, tenantID, siteID uuid.UUID, siteURL string) {
				if _, err := screenshotSvc.EnqueueCapture(ctx, tenantID, siteID, siteURL, screenshot.ReasonEnroll); err != nil {
					logger.Warn("screenshot: enroll trigger failed",
						slog.String("site_id", siteID.String()),
						slog.Any("error", err))
				}
			})
			logger.Info("screenshot: post-enroll capture trigger wired")
		}
	}

	// Cloud scale-to-zero: the media-encoder is a separate, min-instances=0 Cloud
	// Run service running a PULL River worker. Nothing cold-starts it when we
	// enqueue (enqueue is a DB write, not an HTTP call to the encoder), so a waker
	// reconcile loop holds a /internal/drain request open to keep the cold-started
	// instance alive until the media_encode queue drains. WPMGR_MEDIA_ENCODER_URL
	// is the encoder's Cloud Run URL; unset on self-host (the always-on `media`
	// compose profile), where the waker disables itself.
	mediaWaker := media.NewEncoderWaker(pool, os.Getenv("WPMGR_MEDIA_ENCODER_URL"), logger, mediaRiverSchema, mediamodel.MediaEncodeQueue, screenshot.ScreenshotQueue, mediafont.FontTranscodeQueue)
	mediaSvc.SetWaker(mediaWaker)
	// M72: wire the same waker into the screenshot service so enqueuing a capture
	// also cold-starts the scale-to-zero encoder (it runs both media_encode and
	// site_screenshot queues).
	if screenshotSvc != nil {
		screenshotSvc.SetWaker(mediaWaker)
	}
	go mediaWaker.Run(ctx)

	// m59 — wire the email agent command client now that River has started and
	// the commander is available. The agentcmd.Client satisfies email.AgentEmailClient
	// via the SyncEmailConfig and SendTestEmail methods added in email_contract.go.
	if emailCmd, ok := commander.(email.AgentEmailClient); ok {
		emailSvc.SetAgentClient(emailCmd, newPerfSiteAdapter(siteSvc))
		logger.Info("email agent client wired")
	} else {
		logger.Warn("email agent client not wired: CP->agent commander unavailable (signing key empty?)")
	}

	// M38 — wire the db-clean schedule worker's enqueuer + cpBaseURL now that
	// River has started. The schedule worker finds due sites and enqueues
	// DBCleanArgs River jobs; the dispatch worker calls perfSvc.DBCleanScheduled.
	dbCleanEnqueuer := perf.NewDBCleanRiverEnqueuer(riverClient)
	dbCleanScheduleWorker.SetEnqueuer(dbCleanEnqueuer, os.Getenv("WPMGR_PUBLIC_BASE_URL"))

	// ADR-046 Performance Suite: wire the RUCSS enqueuer + perf ingest service
	// now that River has started. The ingest service stashes the agent-posted
	// HTML/CSS bundle in object storage and enqueues the rucss_process job (the
	// agent never blocks). When S3 is not configured (rucssBundleStore == nil)
	// the ingest service is built with nil plumbing and reports "not processing"
	// so the agent keeps serving full CSS.
	var rucssIngestSvc *perf.RucssIngestService
	if rucssBundleStore != nil {
		rucssEnqueuer := rucssworker.NewRiverEnqueuer(riverClient)
		rucssIngestSvc = perf.NewRucssIngestService(rucssRepo, rucssBundleStore, rucssEnqueuer, clock, logger)
	} else {
		rucssIngestSvc = perf.NewRucssIngestService(rucssRepo, nil, nil, clock, logger)
	}
	// The operator-facing RUCSS results list reads through the rucss repo; map
	// the rucss model.Result to the perf DTO here so the perf handler does not
	// import the rucss model.
	perfRucssReader := &perf.RucssResultsReader{
		List: func(ctx context.Context, tenantID, siteID uuid.UUID, limit, offset int32) ([]perf.RucssResultDTO, error) {
			rows, lerr := rucssRepo.ListForSite(ctx, tenantID, siteID, limit, offset)
			if lerr != nil {
				return nil, lerr
			}
			out := make([]perf.RucssResultDTO, 0, len(rows))
			for _, r := range rows {
				dto := perf.RucssResultDTO{
					ID:            r.ID.String(),
					StructureHash: r.StructureHash,
					URL:           r.URL,
					OriginalBytes: r.OriginalCSSBytes,
					UsedBytes:     r.UsedCSSBytes,
					ReductionPct:  r.ReductionPct,
					S3Key:         r.UsedCSSS3Key,
				}
				if !r.LastUsedAt.IsZero() {
					dto.LastUsedAt = r.LastUsedAt.UTC().Format(time.RFC3339)
				}
				out = append(out, dto)
			}
			return out, nil
		},
		Clear: func(ctx context.Context, tenantID, siteID uuid.UUID) (int, error) {
			return rucssRepo.DeleteForSite(ctx, tenantID, siteID)
		},
	}
	perfH := perf.NewHandler(perfSvc, perfRucssReader, auditRec)
	perfH.SetCPBaseURL(os.Getenv("WPMGR_PUBLIC_BASE_URL"))
	// P3.5 — wire the corpus reader so the orphans classification endpoint can
	// classify stored scan candidates against the live plugin_signatures corpus.
	perfH.SetCorpusSource(dbclean.NewCorpusPostgresReader(sqlc.New(pool)))
	// M55 — wire the font results list reader for GET /perf/fonts.
	perfFontResultsReader := &perf.FontResultsReader{
		List: func(ctx context.Context, tenantID, siteID uuid.UUID, limit, offset int32) ([]perf.FontResultDTO, error) {
			rows, lerr := perfRepo.ListFontResultsForSite(ctx, tenantID, siteID, limit, offset)
			if lerr != nil {
				return nil, lerr
			}
			out := make([]perf.FontResultDTO, 0, len(rows))
			for _, r := range rows {
				out = append(out, perf.ToFontResultDTO(r))
			}
			return out, nil
		},
	}
	perfH.SetFontResultsReader(perfFontResultsReader)
	// M56 — Wire the RUM results reader for GET /perf/rum, /perf/rum/summary,
	// and /perf/rum/trend (dashboard redesign: distribution + 28-day trend).
	perfRumResultsReader := &perf.RumResultsReader{
		GetHourlyRollups:         rumStore.GetHourlyRollups,
		ComputeP75:               rumStore.ComputeP75,
		GetDailyRollups:          rumStore.GetDailyRollups,
		GetHourlyRollupsForSites: rumStore.GetHourlyRollupsForSites,
	}
	perfH.SetRumResultsReader(perfRumResultsReader)
	// M56 — Wire the RUM beacon key repo so UpdateConfig generates keys on first enable.
	perfSvc.SetBeaconKeyRepo(rumBeaconRepo, os.Getenv("WPMGR_PUBLIC_BASE_URL"))
	fontResultsAgentH := perf.NewFontResultsAgentHandler(perfRepo)
	perfAgentH := perf.NewAgentHandler(perfSvc, rucssIngestSvc, ocSvc)

	// ADR-045 Phase 2 — wire the auth service's transactional mailer (password
	// reset link + change-password notification) + an in-memory rate limiter now
	// that River has started.
	authSvc.SetMailer(mailer.NewEnqueuer(mailerSvc, riverClient), os.Getenv("WPMGR_PUBLIC_BASE_URL"), autologin.NewMemoryLimiter())
	// Track B (m49) — wire the backup-event mailer now that River has started.
	// The BackupMailer interface is satisfied by *mailer.Enqueuer. Emails are
	// best-effort (sendBackupEmail swallows errors); nil mailer = no emails.
	if backupSvc != nil {
		backupSvc.SetMailer(mailer.NewEnqueuer(mailerSvc, riverClient))
	}

	// m62 — wire the email service's post-River dependencies: the River enqueuer
	// (org-config propagation), the mailer enqueuer + status (alert/digest), and
	// the public base URL (for alert deep-link URLs in email bodies).
	emailSvc.SetEnqueuer(email.NewRiverEnqueuer(riverClient))
	emailSvc.SetMailer(mailer.NewEnqueuer(mailerSvc, riverClient))
	emailSvc.SetMailerStatus(mailerSvc)
	if emailPublicBase != "" {
		emailSvc.SetPublicBase(emailPublicBase)
	}

	// m64 — wire the report service's post-River dependencies.
	// The ScheduleScanWorker gets the started River client so it can Insert
	// GenerateArgs jobs.
	reportScheduleScanWorker.SetRiverClient(riverClient)
	// FIX-4: wire the enqueuer so GenerateNow actually inserts the River job.
	reportSvc.SetEnqueuer(reportpkg.NewRiverEnqueuer(riverClient))
	// Wire the mailer enqueuer + status so completed reports send notifications.
	reportSvc.SetMailer(mailer.NewEnqueuer(mailerSvc, riverClient))
	reportSvc.SetMailerStatus(mailerSvc)
	// Wire the SSE publisher so completed reports fan out report.completed events.
	reportSvc.SetPublisher(siteEventsPub)

	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), cfg.Shutdown.Timeout)
		defer cancel()
		if err := riverClient.Stop(stopCtx); err != nil {
			logger.Warn("river stop", slog.Any("error", err))
		}
	}()

	// Phase 5.5 One-Click Login (ADR-030/031). Mint+consume require the CP
	// signing key (the JWT is Ed25519-signed by the same control-plane keypair
	// used for M3/M4 commands). When the key is empty in dev, the mint endpoint
	// is wired but every mint will return 500 (the signer interface is satisfied
	// by a small refusing shim so the rest of the boot still completes). Redis
	// is the hot-path consume store; when WPMGR_REDIS_ADDR is empty the Redigo
	// pool still constructs but every Set/GETDEL no-ops -> the service falls
	// back to the durable PG single-shot consume on every callback.
	var autologinH *autologin.MintHandler
	var autologinAgentH *autologin.AgentHandler
	{
		var signer autologin.Signer
		if cfg.Agent.SigningPrivateKey != "" {
			s, serr := agentcmd.NewSigner(cfg.Agent.SigningPrivateKey)
			if serr != nil {
				return fmt.Errorf("build autologin signer: %w", serr)
			}
			signer = s
		} else {
			logger.Warn("WPMGR_AGENT_SIGNING_PRIVATE_KEY is empty: autologin mint is disabled (the endpoint will return 500)")
			signer = disabledAutologinSigner{}
		}
		store := autologin.NonceStore(autologin.NewRedigoStore(redisPool))
		if cfg.Redis.Addr == "" {
			store = autologin.NoopStore{}
		}
		limiter := autologin.NewMemoryLimiter()
		// Janitor stops with the process — no explicit Stop() is required because
		// the process lives until shutdown; the goroutine is bounded and idle.
		autologinSvc := autologin.NewService(
			autologin.NewRepo(pool),
			store,
			signer,
			newAutologinSiteAdapter(siteSvc),
			limiter,
			auditRec,
			clock,
			autologin.Config{Require2FAStepUp: cfg.Autologin.Require2FAStepUp},
		)
		autologinH = autologin.NewMintHandler(autologinSvc)
		autologinAgentH = autologin.NewAgentHandler(autologinSvc)
	}

	// ADR-037 Sprint 2 — diagnostics + php-error monitor wiring. The service
	// is always built (the operator GET endpoints work as soon as the agent
	// ships its first payload).
	//
	// v0.9.13 (CP-side fix B): wire the on-demand RefreshEnqueuer when the
	// commander supports the `diagnostics` agentcmd verb (every real-mode
	// build does; the disabledCommander does NOT, in which case /refresh
	// keeps returning the legacy 503 unwired sentinel). The enqueuer issues
	// one signed POST to the agent's /wp-json/wpmgr/v1/command/diagnostics
	// route, reads the agent's synchronous 14-category response body, and
	// feeds it into the same IngestDiagnostics splitter the daily cron-push
	// path uses — so the operator's "Re-run check" click renders fresh data
	// on the next GET /diagnostics.
	// diagnosticsRepo is already built above (before River start) so we reuse it.
	diagnosticsSvc := diagnostics.NewService(diagnosticsRepo)
	// M28 — offline IP -> hosting-provider resolver. Self-disables (no-op) if the
	// embedded DB-IP ASN database fails to open; never blocks boot.
	if ipResolver, ipErr := ipprovider.New(); ipErr != nil {
		logger.Warn("ipprovider disabled: could not open ASN database", "error", ipErr)
	} else {
		diagnosticsSvc.SetHostResolver(ipResolver)
		logger.Info("ipprovider enabled", "db_release", ipResolver.Resolve("8.8.8.8").DBRelease)
	}
	diagnosticsH := diagnostics.NewHandler(diagnosticsSvc, auditRec)
	diagnosticsAgentH := agent.NewDiagnosticsHandler(diagnosticsSvc)
	// M28 — also resolve the host provider on the metadata push (30-min cadence +
	// plugin events), not just the daily diagnostics push, so the inferred host
	// populates within ~30 min instead of up to a day.
	agentH.SetHostResolver(diagnosticsSvc)
	errorsAgentH := agent.NewErrorsHandler(diagnosticsSvc)
	diagSiteAdapter := newDiagnosticsSiteAdapter(siteSvc)
	if diagCmd, ok := commander.(diagnostics.AgentDiagnosticsClient); ok {
		diagEnq := diagnostics.NewRefreshEnqueuer(
			diagCmd,
			diagSiteAdapter,
			diagnosticsSvc,
		)
		if diagEnq != nil {
			diagnosticsSvc.SetRefreshEnqueuer(diagEnq)
			logger.Info("diagnostics refresh enqueuer wired")
		}
	} else {
		logger.Warn("diagnostics refresh enqueuer not wired: CP->agent commander unavailable (signing key empty?)")
	}
	// S1.2 — error config push: wire the agentcmd client when the commander
	// supports the sync_error_config verb. The same SSRF client and signer
	// used for the diagnostics refresh are reused here (the update commander
	// is the full agentcmd.Client in real-mode builds; it now also satisfies
	// AgentErrorConfigClient via SyncErrorConfig).
	if errCfgCmd, ok := commander.(diagnostics.AgentErrorConfigClient); ok {
		diagnosticsSvc.SetErrorConfigClient(errCfgCmd, diagSiteAdapter)
		logger.Info("error config sync client wired")
	} else {
		logger.Warn("error config sync client not wired: CP->agent commander unavailable (signing key empty?)")
	}

	// ADR-037 Sprint 3 — WordPress activity log. The CP re-verifies the agent's
	// hash chain at ingest (tamper-evidence) and routes high-severity events into
	// the EXISTING uptime alert Dispatcher (no parallel notification system). The
	// security alerter loads the tenant's AlertConfig and gates on its
	// notify_security flag before dispatching email + webhook.
	activityRepo := activity.NewRepo(pool)
	activitySecAlerter := newActivitySecurityAlerter(uptimeRepo, uptimeDispatcher, clock, logger)
	activitySvc := activity.NewService(activityRepo, activitySecAlerter, newActivitySiteAdapter(siteSvc))
	activityH := activity.NewHandler(activitySvc)
	activityAgentH := agent.NewActivityHandler(activitySvc)

	// S2 — Login Protection + IP store. The security service stores per-site
	// login-protection config, pushes it to the agent via the signed
	// `sync_security_config` command, ingests login events, and exposes an
	// unblock-IP action. The agent client is wired when the commander supports
	// the security command verbs (every real-mode build does).
	//
	// ADR-057 Phase 1: the same service also owns the hardening config + ban
	// list. The hardening client is wired separately so each interface can be
	// satisfied independently (the disabledCommander satisfies both or neither).
	securityRepo := security.NewRepo(pool)
	securitySvc := security.NewService(securityRepo)
	securityH := security.NewHandler(securitySvc, auditRec)
	securityAgentH := agent.NewSecurityLoginEventsHandler(securitySvc)
	secSiteAdapter := newSecuritySiteAdapter(siteSvc)
	if secCmd, ok := commander.(security.AgentSecurityClient); ok {
		securitySvc.SetAgentClient(secCmd, secSiteAdapter)
		logger.Info("security agent client wired")
	} else {
		logger.Warn("security agent client not wired: CP->agent commander unavailable (signing key empty?)")
	}
	if hardeningCmd, ok := commander.(security.AgentHardeningClient); ok {
		securitySvc.SetHardeningClient(hardeningCmd, secSiteAdapter)
		logger.Info("security hardening agent client wired")
	} else {
		logger.Warn("security hardening agent client not wired: CP->agent commander unavailable (signing key empty?)")
	}
	// ADR-059 Phase 3 — wire the policy push client (sync_security_policy) using
	// the same EdDSA-signed agentcmd.Client pattern as SetHardeningClient above.
	// *agentcmd.Client satisfies AgentPolicyClient via its SyncSecurityPolicy method.
	if policyCmd, ok := commander.(security.AgentPolicyClient); ok {
		securitySvc.SetPolicyClient(policyCmd, secSiteAdapter)
		logger.Info("security policy agent client wired")
	} else {
		logger.Warn("security policy agent client not wired: CP->agent commander unavailable (signing key empty?)")
	}
	// ADR-059 Phase 3 — wire the SSRF-safe HIBP doer. Reuse the same ssrfClient
	// used by the scan checksums, uptime prober, and all other outbound CP calls.
	// No new outbound client is created; the shared ssrfClient already enforces
	// SSRF guards (ADR-009) and the configured timeout + retry policy.
	securitySvc.SetHIBPDoer(ssrfClient)
	logger.Info("HIBP doer wired (ssrfClient)")
	// ADR-059 Phase 3 — HIBP agent-authenticated handler. The route is registered
	// in the agentGroup in server.go (GET /agent/v1/security/hibp/range/:prefix).
	hibpAgentH := agent.NewHIBPHandler(securitySvc)

	// M14 — Login Whitelabel. The loginbrand service stores per-site login brand
	// config (logo URL, logo link, message) and pushes it to the agent via the
	// signed `sync_login_brand` command. The agent client is wired when the
	// commander supports the SyncLoginBrand method (every real-mode build does).
	loginBrandRepo := loginbrand.NewRepo(pool)
	loginBrandSvc := loginbrand.NewService(loginBrandRepo)
	loginBrandH := loginbrand.NewHandler(loginBrandSvc, auditRec)
	loginBrandSiteAdapter := newLoginBrandSiteAdapter(siteSvc)
	if lbCmd, ok := commander.(loginbrand.AgentLoginBrandClient); ok {
		loginBrandSvc.SetAgentClient(lbCmd, loginBrandSiteAdapter)
		logger.Info("login brand agent client wired")
	} else {
		logger.Warn("login brand agent client not wired: CP->agent commander unavailable (signing key empty?)")
	}

	// M5.7 — Orgs + Sharing + Invitations.
	publicBaseURL := os.Getenv("WPMGR_PUBLIC_BASE_URL")

	// Build the sharing mailer (reuse SMTP config; may be nil/noop).
	var sharingMailer sharing.Mailer
	if cfg.SMTP.Enabled() {
		sharingMailer = uptime.NewSMTPMailer(cfg.SMTP, logger)
	}
	sharingSvc := sharing.NewService(pool, authRepo, auditRec, sharingMailer, publicBaseURL)
	// ADR-045 — site shares now notify the grantee by email via the DB-configured
	// SMTP: a branded "site_invite" link for a new user, or a "site_shared"
	// notification for an existing one (who gets immediate access).
	sharingSvc.SetShareEnqueuer(mailer.NewEnqueuer(mailerSvc, riverClient))
	sharingH := sharing.NewHandler(sharingSvc)

	// Org handler: create org + activate.
	orgTenantCreator := &orgTenantAdapter{svc: tenantSvc}
	orgH := org.NewHandler(pool, orgTenantCreator, sessions, authSvc, auditRec)

	// Invitation service + handler.
	var invitationMailer invitation.Mailer
	if cfg.SMTP.Enabled() {
		invitationMailer = uptime.NewSMTPMailer(cfg.SMTP, logger)
	}
	invitationSvc := invitation.NewService(pool, authRepo, auditRec, sessions, invitationMailer, publicBaseURL)
	// ADR-045 Phase 3 — org invitations send the branded "invite" template via
	// the DB-configured SMTP (the legacy env mailer is nil once SMTP moved to the
	// UI), and always return the accept link.
	invitationSvc.SetInviteEnqueuer(mailer.NewEnqueuer(mailerSvc, riverClient))
	invitationH := invitation.NewHandler(invitationSvc)

	// m66 — client portal. Member management mounts under the existing client
	// handler (RequireOrgScope group); the read-only /portal group is gated by
	// RequireClientPortal. authRepo (not authSvc) carries GetUsersByIDs. The
	// portal gets its own backup repo: the backup-service one is scoped inside
	// the S3-enabled block, and the portal's snapshot listing must work (as
	// empty history) even when backups are disabled.
	clientMemberH := clientpkg.NewMemberHandler(pool, authRepo, invitationSvc, auditRec, publicBaseURL)
	clientH.SetMemberHandler(clientMemberH)
	portalH := portalpkg.NewHandler(pool, siteSvc, uptimeSvc, backup.NewRepo(pool), reportSvc, rumStore)
	// Wire the metrics store for cheap per-site aggregate queries on /portal/sites
	// (uptime_30d_pct + tls_expires_at) and for the /portal/summary fleet series.
	// metricsStore is always available (Postgres fallback).
	portalH.SetMetricsStore(metricsStore)
	// Wire report sources for /portal/summary when object storage is configured.
	// SetReportSources nulls the email source — it is never exposed in the portal.
	if portalReportSources != nil {
		portalH.SetReportSources(*portalReportSources)
	}

	// m33 — superadmin instance-management area.
	// authSvc satisfies admin.VerificationResender via ResendVerificationByID.
	adminRepo := admin.NewRepo(pool)
	adminSvc := admin.NewService(adminRepo, authSvc)
	adminH := admin.NewHandler(adminSvc, pool)
	adminH.SetAuditRecorder(auditRec)
	// m80 — wire the vuln-feed key management into the admin handler.
	// vulnFeedKeySvc already has its feed-refresh enqueuer set (wired in the
	// vuln River block above), so this call sees a fully-wired service.
	adminH.SetVulnFeed(vulnRepo, vulnFeedKeySvc)

	// M16 Phase C1 — superadmin billing-admin panel (accounts / account
	// detail / revenue / manual controls). Wired unconditionally (cheap,
	// stateless, mirrors the billing.Handler construction above): superadmin
	// reads work regardless of WPMGR_HOSTED, and every mutation degrades to a
	// clean "not configured" only if billingRepo itself were nil, which it
	// never is here. stripeTestMode drives the account-detail subscription
	// card's Stripe-dashboard deep link (test vs live URL prefix).
	stripeTestMode := strings.HasPrefix(cfg.Billing.Stripe.SecretKey, "sk_test_")
	adminBillingRepo := admin.NewBillingRepo(pool)
	adminSvc.SetBillingPanel(adminBillingRepo, billingSvc, auditRec, stripeTestMode)

	// ADR-042 — CP-driven agent self-update manifest handler. Needs object
	// storage (to read agent-releases/latest.json + presign the package) AND the
	// CP signing key (to sign the manifest). When either is absent the handler
	// stays nil and the /agent/v1/update/manifest route is simply not mounted.
	// The store is built fresh from cfg.S3 because the earlier defStore is scoped
	// inside the backup block.
	var updateAgentH *agent.UpdateHandler
	if cfg.S3.Enabled() && cfg.Agent.SigningPrivateKey != "" {
		manifestStore, merr := blobstore.New(blobstore.Config{
			Endpoint:       cfg.S3.Endpoint,
			Region:         cfg.S3.Region,
			Bucket:         cfg.S3.Bucket,
			AccessKey:      cfg.S3.AccessKey,
			SecretKey:      cfg.S3.SecretKey,
			ForcePathStyle: cfg.S3.ForcePathStyle,
		})
		if merr != nil {
			return fmt.Errorf("update manifest store: %w", merr)
		}
		manifestSigner, serr := agentcmd.NewSigner(cfg.Agent.SigningPrivateKey)
		if serr != nil {
			return fmt.Errorf("update manifest signer: %w", serr)
		}
		// Clamp the package presign + manifest exp window to <=5min (ADR-042 §1).
		manifestTTL := cfg.Backup.PresignTTL
		if manifestTTL <= 0 || manifestTTL > 5*time.Minute {
			manifestTTL = 5 * time.Minute
		}
		updateAgentH = agent.NewUpdateHandler(manifestStore, manifestSigner, manifestTTL)

		// Boot probe: exercise the exact storage ops the manifest handler relies
		// on (read agent-releases/latest.json + mint a presigned GET) so a
		// misconfiguration surfaces in the startup log instead of as an opaque
		// 500 on the agent's first poll. Runs once, off the hot path.
		ms := manifestStore
		go func() {
			pctx := context.Background()
			if rc, gerr := ms.GetViaPresign(pctx, "agent-releases/latest.json"); gerr != nil {
				logger.Error("ADR-042 self-update boot probe: fetch latest.json failed", "err", gerr.Error())
			} else {
				_ = rc.Close()
				logger.Info("ADR-042 self-update boot probe: fetch latest.json OK")
			}
		}()
	} else {
		logger.Warn("ADR-042 self-update disabled: object storage or WPMGR_AGENT_SIGNING_PRIVATE_KEY not configured")
	}

	// ADR-056 Phase 3 — wire two-factor authentication into the auth handler.
	// TOTPFactor and WebAuthnFactor are stateless and shared across goroutines.
	// The same siteDestAgeID (age X25519) used for SMTP credential encryption
	// protects TOTP secrets at rest (same threat model: protection against a
	// DB dump, not a fully-compromised CP process).
	totpFactor := twofactor.NewTOTPFactor(cfg.Auth.WebAuthnRPDisplayName)
	waInstance, waErr := twofactor.NewWebAuthn(twofactor.Config{
		RPID:          cfg.Auth.WebAuthnRPID,
		RPOrigins:     twofactor.ParseRPOrigins(cfg.Auth.WebAuthnRPOrigins),
		RPDisplayName: cfg.Auth.WebAuthnRPDisplayName,
	})
	if waErr != nil {
		return fmt.Errorf("webauthn config: %w", waErr)
	}
	waFactor := twofactor.NewWebAuthnFactor(waInstance)
	authSvc.SetTwoFactorDeps(totpFactor, waFactor, siteDestAgeID)

	authH := auth.NewHandler(authSvc, sessions, oidcProvider, newTenant)
	authH.SetSecureCookies(cfg.IsProduction())
	authH.SetHosted(cfg.Hosted.Enabled)

	filesH := files.NewHandler(filesSvc, auditRec)

	srv := server.New(server.Deps{
		Config:          cfg,
		Logger:          logger,
		Pool:            pool,
		Sessions:        sessions,
		Auth:            authn,
		AuthH:           authH,
		MembersH:        auth.NewMembersHandler(authSvc, invitationSvc),
		APIKeyH:         apikey.NewHandler(apiKeySvc, auditRec),
		AuditH:          audit.NewHandler(auditRec),
		TenantH:         tenant.NewHandler(tenantSvc, auditRec),
		SiteH:           siteH,
		SiteEventsH:     siteEventsH,
		FilesH:          filesH,
		UpdateH:         updateH,
		BackupH:         backupH,
		BackupAgentH:    backupAgentH,
		InspectionDeps:  inspectionDeps,
		UptimeH:         uptimeH,
		AutologinH:      autologinH,
		AutologinAgentH: autologinAgentH,
		AgentAuth:       agentAuthn,
		AgentH:          agentH,
		UpdateAgentH:    updateAgentH,
		SiteDestH:       siteDestH,
		// ADR-045 — instance SMTP settings.
		SettingsH: smtpSettingsH,
		// ADR-037 Sprint 2 wiring.
		DiagnosticsH:      diagnosticsH,
		DiagnosticsAgentH: diagnosticsAgentH,
		ErrorsAgentH:      errorsAgentH,
		// ADR-037 Sprint 3 wiring — activity log + agent ingest.
		ActivityH:      activityH,
		ActivityAgentH: activityAgentH,
		// S2 — Login Protection + IP store.
		SecurityH:      securityH,
		SecurityAgentH: securityAgentH,
		// M14 — Login Whitelabel.
		LoginBrandH: loginBrandH,
		// S3 — Malware / File-Integrity Scan.
		ScanH: scanH,
		// m16 — Restore Runs + Logs.
		RestoreRunH: restoreRunH,
		// M17 — Schedule Run queue.
		ScheduleRunH: scheduleRunH,
		// M5.7 — Orgs + Sharing + Invitations.
		OrgH:        orgH,
		SharingH:    sharingH,
		InvitationH: invitationH,
		// M23 — Media Optimizer.
		MediaH:      mediaH,
		MediaAgentH: mediaAgentH,
		// m36 / ADR-046 — Performance Suite.
		PerfH:             perfH,
		PerfAgentH:        perfAgentH,
		FontResultsAgentH: fontResultsAgentH,
		// m68 — Object Cache operator routes.
		ObjectCacheH: ocH,
		// m59 — per-site email management + Phase 3 log ingest.
		EmailH:      emailH,
		EmailAgentH: emailAgentH,
		// m61 — webhook handler is now mounted (security hardened).
		EmailWebhookH: emailWebhookH,
		// m33 — superadmin instance-management area.
		AdminH: adminH,
		// M56 — RUM ingest endpoint (public, no auth).
		RumH: rumH,
		// M72 — site screenshots.
		ScreenshotH: screenshotH,
		// m63 — agency clients.
		ClientH: clientH,
		// m64 — white-label client reports.
		ReportH: reportH,
		// m66 — read-only client portal.
		PortalH: portalH,
		// ADR-059 Phase 3 — HIBP breach-password range proxy (agent-authenticated).
		HIBPAgentH: hibpAgentH,
		// m79 — vulnerability scanner: fleet rollup + per-site finding management.
		VulnH: vulnH,
		// M16 Phase B — hosted billing. Both nil unless WPMGR_HOSTED is
		// enabled: this is the routes-contract 404-when-unhosted guarantee
		// (the webhook path 404s the same way — ProcessWebhook would also
		// refuse an unrecognized provider, but there is no reason to mount an
		// unreachable public endpoint at all on a self-host/unhosted boot).
		BillingH:              billingHForRoutes,
		BillingWebhookH:       billingWebhookHForRoutes,
		BillingSuspensionGate: billingSuspensionGateForRoutes,
		ServiceName:           cfg.OTel.ServiceName,
		Version:               version,
	})

	return srv.Run(ctx)
}

// orgTenantAdapter adapts tenant.Service to the org.TenantCreator interface
// (which takes (name, slug) directly instead of tenant.CreateInput).
type orgTenantAdapter struct {
	svc *tenant.Service
}

// Create adapts tenant.Service.Create to the org.TenantCreator interface,
// translating a (name, slug) pair into a tenant creation call.
func (a *orgTenantAdapter) Create(ctx context.Context, name, slug string) (uuid.UUID, error) {
	t, err := a.svc.Create(ctx, tenant.CreateInput{Name: name, Slug: slug})
	if err != nil {
		return uuid.Nil, err
	}
	return t.ID, nil
}

// registryAdapter bridges the blobstore.Registry (which knows about Stores in
// blobstore terms) into the backup.PresignerForSnapshot interface (which works
// in backup terms, so the backup package needs no import cycle on blobstore).
// ADR-036 P1 storage adapter routing.
type registryAdapter struct {
	r *blobstore.Registry
}

// PresignerForSnapshot resolves the blobstore Registry entry for a snapshot so
// backup operations can mint presigned URLs without importing blobstore types.
func (a *registryAdapter) PresignerForSnapshot(ctx context.Context, snap backup.Snapshot) (backup.Presigner, error) {
	store, err := a.r.StoreForSnapshot(ctx, blobstore.SnapshotLike{
		TenantID:      snap.TenantID,
		SiteID:        snap.SiteID,
		DestinationID: snap.DestinationID,
	})
	if err != nil {
		return nil, err
	}
	if store == nil {
		return nil, nil
	}
	return store, nil
}

// disabledAutologinSigner refuses to mint when no CP signing key is
// configured, mirroring disabledCommander for M3/M4.
type disabledAutologinSigner struct{}

func (disabledAutologinSigner) MintAutologin(_ time.Time, _, _ string) (string, string, error) {
	return "", "", fmt.Errorf("autologin is disabled: no CP signing key configured")
}

// agentSigningPublicKey validates the control-plane Ed25519 signing keypair from
// config and returns the base64 public half handed to agents at enrollment. An
// empty keypair is permitted in dev (returns ""), but a malformed one fails.
func agentSigningPublicKey(cfg config.AgentConfig) (string, error) {
	if cfg.SigningPublicKey == "" && cfg.SigningPrivateKey == "" {
		return "", nil
	}
	if _, err := agent.DecodePublicKey(cfg.SigningPublicKey); err != nil {
		return "", fmt.Errorf("invalid WPMGR_AGENT_SIGNING_PUBLIC_KEY: %w", err)
	}
	return cfg.SigningPublicKey, nil
}

// migrateRiver applies River's own schema using the migration-owner pool.
// seedSuperadminAccount provisions a superadmin account that does not exist yet
// (the operator could not self-register, e.g. their mailbox domain does not
// accept mail). It creates the user as active + email-verified + is_superadmin
// with a RANDOM password no one is told, then mints a one-time, 24h
// password-reset token and logs the set-password URL so the operator chooses
// their own password. Runs on the owner pool (superuser, bypasses RLS).
func seedSuperadminAccount(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, baseURL, email string) error {
	pwBuf := make([]byte, 24)
	if _, err := crand.Read(pwBuf); err != nil {
		return fmt.Errorf("random password: %w", err)
	}
	hash, err := auth.HashPassword(base64.RawURLEncoding.EncodeToString(pwBuf))
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	var userID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, name, status, email_verified_at, is_superadmin)
		 VALUES ($1, $2, '', 'active', now(), true)
		 RETURNING id`, email, hash).Scan(&userID); err != nil {
		return fmt.Errorf("insert user: %w", err)
	}
	return mintSetPasswordLink(ctx, pool, logger, baseURL, userID, email, "superadmin account CREATED")
}

// mintSetPasswordLink writes a one-time, 24h password-reset token for an account
// and logs the set-password URL. password_reset_tokens has FORCE RLS gated on
// app.agent='on', and the owner role does not bypass RLS, so the insert must run
// inside a transaction that sets the GUC.
func mintSetPasswordLink(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, baseURL string, userID uuid.UUID, email, label string) error {
	tokBuf := make([]byte, 32)
	if _, err := crand.Read(tokBuf); err != nil {
		return fmt.Errorf("random token: %w", err)
	}
	rawTok := base64.RawURLEncoding.EncodeToString(tokBuf)
	sum := sha256.Sum256([]byte(rawTok))

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.agent', 'on', true)"); err != nil {
		return fmt.Errorf("set app.agent: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO password_reset_tokens (user_id, token_hash, expires_at)
		 VALUES ($1, $2, now() + interval '24 hours')`, userID, sum[:]); err != nil {
		return fmt.Errorf("insert reset token: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	logger.Warn(label+" — set your password via this one-time link (valid 24h)",
		slog.String("email", email),
		slog.String("set_password_url", baseURL+"/reset-password?token="+rawTok))
	return nil
}

// recoverAccountIntoOrg recreates a (possibly deleted) user and attaches it as
// OWNER of an existing org identified by slug or name, then mints a one-time
// set-password link. It recovers an account whose org + sites are still intact
// but whose user row (and thus membership) was deleted. Idempotent: an existing
// user is reactivated; an existing membership is upgraded to owner. Runs on the
// owner pool. tenants + users have no RLS, but memberships has FORCE RLS gated on
// app.tenant_id, and the owner role does not bypass it, so the membership INSERT
// sets the GUC inside its tx (mirrors mintSetPasswordLink).
func recoverAccountIntoOrg(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, baseURL, email, orgRef string) error {
	var tenantID uuid.UUID
	var tenantName string
	if err := pool.QueryRow(ctx,
		`SELECT id, name FROM tenants WHERE slug = $1 OR lower(name) = lower($1) ORDER BY created_at LIMIT 1`,
		orgRef).Scan(&tenantID, &tenantName); err != nil {
		return fmt.Errorf("resolve org %q: %w", orgRef, err)
	}

	var userID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE lower(email) = $1`, email).Scan(&userID); err != nil {
		// No account: create one (active + verified) with a random password the
		// operator never learns — they set their own via the link below.
		pwBuf := make([]byte, 24)
		if _, rerr := crand.Read(pwBuf); rerr != nil {
			return fmt.Errorf("random password: %w", rerr)
		}
		hash, herr := auth.HashPassword(base64.RawURLEncoding.EncodeToString(pwBuf))
		if herr != nil {
			return fmt.Errorf("hash password: %w", herr)
		}
		if ierr := pool.QueryRow(ctx,
			`INSERT INTO users (email, password_hash, name, status, email_verified_at)
			 VALUES ($1, $2, '', 'active', now()) RETURNING id`, email, hash).Scan(&userID); ierr != nil {
			return fmt.Errorf("create user: %w", ierr)
		}
		logger.Info("recover account: user created", slog.String("email", email))
	} else {
		if _, uerr := pool.Exec(ctx,
			`UPDATE users SET status = 'active', email_verified_at = COALESCE(email_verified_at, now()), updated_at = now()
			 WHERE id = $1`, userID); uerr != nil {
			return fmt.Errorf("reactivate user: %w", uerr)
		}
		logger.Info("recover account: existing user reactivated", slog.String("email", email))
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID.String()); err != nil {
		return fmt.Errorf("set app.tenant_id: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'owner')
		 ON CONFLICT (user_id, tenant_id) DO UPDATE SET role = 'owner', updated_at = now()`,
		tenantID, userID); err != nil {
		return fmt.Errorf("attach membership: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit membership: %w", err)
	}
	logger.Info("recover account: attached as owner",
		slog.String("email", email), slog.String("org", tenantName), slog.String("tenant_id", tenantID.String()))

	return mintSetPasswordLink(ctx, pool, logger, baseURL, userID, email, "account recovery requested")
}

// grantMembership idempotently ensures the user with `email` is a member of
// tenantID with `role`. Both the user and the tenant must already exist — it
// never creates either and never touches passwords. The INSERT sets
// app.tenant_id so the memberships tenant_isolation WITH CHECK passes; the
// ON CONFLICT keeps it idempotent + keeps the role current.
func grantMembership(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, email string, tenantID uuid.UUID, role string) error {
	var userID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE lower(email) = $1`, email).Scan(&userID); err != nil {
		return fmt.Errorf("resolve user %q: %w", email, err)
	}
	var tenantName string
	if err := pool.QueryRow(ctx, `SELECT name FROM tenants WHERE id = $1`, tenantID).Scan(&tenantName); err != nil {
		return fmt.Errorf("resolve tenant %s: %w", tenantID, err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID.String()); err != nil {
		return fmt.Errorf("set app.tenant_id: %w", err)
	}
	ct, err := tx.Exec(ctx,
		`INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, $3)
		 ON CONFLICT (user_id, tenant_id) DO UPDATE SET role = EXCLUDED.role, updated_at = now()`,
		tenantID, userID, role)
	if err != nil {
		return fmt.Errorf("upsert membership: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit membership: %w", err)
	}
	logger.Info("grant membership: ensured",
		slog.String("email", email),
		slog.String("org", tenantName),
		slog.String("tenant_id", tenantID.String()),
		slog.String("role", role),
		slog.Int64("rows", ct.RowsAffected()))
	return nil
}

// revokeMembership idempotently removes the membership of `email`'s user in
// tenantID. The DELETE runs under app.tenant_id = tenantID so the memberships
// USING policy exposes the row. Never deletes the org itself.
func revokeMembership(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, email string, tenantID uuid.UUID) error {
	var userID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE lower(email) = $1`, email).Scan(&userID); err != nil {
		return fmt.Errorf("resolve user %q: %w", email, err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID.String()); err != nil {
		return fmt.Errorf("set app.tenant_id: %w", err)
	}
	ct, err := tx.Exec(ctx, `DELETE FROM memberships WHERE user_id = $1 AND tenant_id = $2`, userID, tenantID)
	if err != nil {
		return fmt.Errorf("delete membership: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit revoke: %w", err)
	}
	logger.Info("revoke membership: done",
		slog.String("email", email),
		slog.String("tenant_id", tenantID.String()),
		slog.Int64("rows", ct.RowsAffected()))
	return nil
}

// migrateRiver applies River's own schema migrations using the migration-owner
// pool, matching the ownership model used for WPMgr app migrations.
func migrateRiver(ctx context.Context, pool *pgxpool.Pool) error {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("river migrate: %w", err)
	}
	return nil
}

// newMediaRiverClient returns an insert-only River client for encoder-owned
// queues when media schema isolation is enabled. When the schema is empty or
// public it reuses the default client so existing behavior is preserved.
func newMediaRiverClient(pool *pgxpool.Pool, logger *slog.Logger, defaultClient *river.Client[pgx.Tx], schema string) (*river.Client[pgx.Tx], error) {
	if riverutil.IsDefaultSchema(schema) {
		return defaultClient, nil
	}
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Logger:              logger,
		Schema:              schema,
		SkipUnknownJobCheck: true,
	})
	if err != nil {
		return nil, fmt.Errorf("media river client: %w", err)
	}
	logger.Info("media river insert client configured", slog.String("schema", schema))
	return client, nil
}

// riverDeps bundles everything startRiver needs: the M2 health checker, the M3
// update worker, and the M4 backup/restore/GC/scheduler workers (any of which
// may be nil when the corresponding feature is disabled).
type riverDeps struct {
	healthChecker  *site.HealthChecker
	healthInterval time.Duration
	// M21 connection lifecycle: the timeout sweeper (15s) + site_events prune (1m).
	siteSweepWorker      *site.SweepWorker
	siteEventPruneWorker *site.EventPruneWorker
	updateWorker         *update.Worker
	// #131 follow-up — periodic reaper for update_tasks stuck in
	// pending/running past the stale-task threshold (always wired).
	updateReaperWorker     *update.ReaperWorker
	refreshWorker          *update.RefreshInventoryWorker
	perTenantParallelism   int
	backupWorker           *backup.BackupWorker
	restoreWorker          *backup.RestoreWorker
	gcWorker               *backup.GCWorker
	scheduleWorker         *backup.ScheduleWorker
	progressWatchdog       *backup.ProgressWatchdogWorker
	sqlInspectLegacyWorker *backup.SqlInspectLegacyWorker
	scheduleInterval       time.Duration
	gcInterval             time.Duration
	uptimeWorker           *uptime.ProbeWorker
	probeInterval          time.Duration
	// S1.1 (D) — PHP-error retention GC. Always non-nil (wired unconditionally).
	phpErrorsGCWorker *diagnostics.ErrorsGCWorker
	// S3 — Malware / File-Integrity Scan workers (nil when signing key empty).
	scanRunWorker    *scan.ScanRunWorker
	scanHashGCWorker *scan.HashGCWorker
	// ADR-045 — transactional email worker (reset / activation / invite sends).
	sendEmailWorker *mailer.SendEmailWorker
	// ADR-046 Performance Suite — pure-Go RUCSS computation worker (nil when S3
	// is not configured; the agent ingest endpoint then serves full CSS).
	rucssWorker        *rucssworker.Worker
	rucssQueueParallel int
	// FIX 1 backstop: reaps orphaned RUCSS source bundles (page HTML stashed on a
	// cache miss whose job never ran). nil when S3 is not configured.
	rucssSweepWorker *rucssworker.RucssSweepWorker
	// M38 — CP-owned db-clean scheduling workers (always wired when agent client
	// is configured; nil when the signing key is empty).
	dbCleanWorker         *perf.DBCleanWorker
	dbCleanScheduleWorker *perf.DBCleanScheduleWorker
	// M39 — watchdog for stalled db_clean + db_scan jobs (always wired).
	dbCleanWatchdogWorker *perf.DBCleanWatchdogWorker
	// P3.8 — watchdog for stalled db_orphan_delete jobs (always wired).
	dbOrphanDeleteWatchdogWorker *perf.DBOrphanDeleteWatchdogWorker
	// M42 — DB-size history GC (always wired).
	dbSizeHistoryGCWorker *perf.DBSizeHistoryGCWorker
	// M52 / #162 — cache hit-ratio history GC (always wired).
	cacheHitRatioHistoryGCWorker *perf.CacheHitRatioHistoryGCWorker
	// m68 — Object Cache stats history GC (always wired; 7-day raw retention).
	ocStatsHistoryGCWorker *objectcache.ObjectCacheStatsHistoryGCWorker
	// M56 — RUM retention-GC + rollup workers (always wired).
	rumGCWorker     *rum.RumGCWorker
	rumRollupWorker *rum.RumRollupWorker
	// m59 Phase 3 — email log retention GC (always wired).
	emailLogGCWorker *email.EmailLogGCWorker
	// m62 — org-config propagation worker + hourly digest worker (always wired).
	emailOrgPropagateWorker *email.OrgConfigPropagateWorker
	emailDigestWorker       *email.DigestWorker
	// m64 — client report generation + schedule-scan workers.
	// Both are nil when object storage is not configured (reports require S3).
	reportGenerateWorker     *reportpkg.GenerateWorker
	reportScheduleScanWorker *reportpkg.ScheduleScanWorker
	// P4b — cron kick: best-effort wp-cron.php GET for fully page-cached sites.
	// nil when WPMGR_CRON_KICK_ENABLED=false.
	cronKickWorker   *uptime.CronKicker
	cronKickInterval time.Duration
	// m79 — vulnerability scanner: feed refresh worker + per-site rescan worker.
	// Both are always wired; the feed worker no-ops cleanly when
	// WPMGR_WORDFENCE_API_KEY is not set.
	vulnFeedWorker   *vuln.FeedWorker
	vulnRescanWorker *vuln.RescanSiteWorker
	// m82 — File-transfers GC: deletes stale file_transfers rows (and best-effort
	// deletes their staged objects) once per day. Always wired; object deletion is
	// a no-op when the object store is not configured.
	fileTransfersGCWorker *files.FileTransfersGCWorker
	// m85 — Uptime-probe retention GC: prunes site_uptime_probes rows older than
	// 90 days once per day. Always wired; prevents the table from growing
	// unbounded (the root reason the 30-day aggregate window becomes expensive).
	uptimeProbeGCWorker *metrics.UptimeProbeGCWorker
	// M16 Phase B — daily payment-provider drift-repair sweep. Always wired
	// (like phpErrorsGCWorker/fileTransfersGCWorker above): Service.Reconcile
	// itself no-ops cleanly when hosted billing is disabled or no provider is
	// registered, so there is nothing to gate here.
	billingReconcileWorker *billing.ReconcileWorker
}

// startRiver builds and starts the River client with the health-check worker, a
// periodic health job, the M3 update-task worker on per-tenant queue shards, and
// (when backups are enabled) the M4 backup/restore/GC/scheduler workers plus the
// periodic scheduler and retention-GC jobs. The client uses the application pool
// (RLS-bound); cross-tenant jobs (health/scheduler/GC enumeration) run under the
// app.agent GUC, while backup/restore/update work runs tenant-scoped (the worker
// sets app.tenant_id per job via the repo). perTenantParallelism caps each
// update tenant shard's concurrent workers so one tenant cannot starve others.
func startRiver(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, d riverDeps) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, site.NewHealthCheckWorker(d.healthChecker))
	river.AddWorker(workers, d.updateWorker)
	if d.updateReaperWorker != nil {
		river.AddWorker(workers, d.updateReaperWorker)
	}
	if d.refreshWorker != nil {
		river.AddWorker(workers, d.refreshWorker)
	}
	if d.sendEmailWorker != nil {
		river.AddWorker(workers, d.sendEmailWorker)
	}

	interval := d.healthInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	perTenantParallelism := d.perTenantParallelism
	if perTenantParallelism <= 0 {
		perTenantParallelism = 5
	}

	queues := map[string]river.QueueConfig{
		river.QueueDefault: {MaxWorkers: 5},
		// ADR-045 — dedicated email queue so a slow SMTP relay can't starve
		// other work.
		mailer.EmailQueue: {MaxWorkers: 2},
		// M23 Media Optimizer (ADR-043): the API does NOT register the
		// media_encode queue — it only client.Inserts model.EncodeArgs, and Insert
		// works for any queue name without registering it. River REJECTS a
		// MaxWorkers=0 queue (client.go: MaxWorkers must be >= 1), so registering
		// it here would crash API boot. The EncodeWorker (CGO lilliput) registers
		// + works media_encode ONLY in the separate cmd/media-encoder process,
		// which keeps the API CGO_ENABLED=0 / distroless-static.
	}
	// One bounded queue per tenant shard: MaxWorkers caps a single tenant's
	// concurrency to the per-tenant parallelism limit.
	for _, q := range update.QueueNames() {
		queues[q] = river.QueueConfig{MaxWorkers: perTenantParallelism}
	}

	periodics := []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(interval),
			func() (river.JobArgs, *river.InsertOpts) {
				return site.HealthCheckArgs{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: true},
		),
	}

	// M21 connection-lifecycle timeout sweeper (every 15s, ADR-039) + the
	// site_events ring-buffer prune (every minute, ADR-038). The sweeper is the
	// ONLY caller of the degraded/disconnected transitions.
	if d.siteSweepWorker != nil {
		river.AddWorker(workers, d.siteSweepWorker)
		periodics = append(periodics, river.NewPeriodicJob(
			river.PeriodicInterval(15*time.Second),
			func() (river.JobArgs, *river.InsertOpts) { return site.SweepArgs{}, nil },
			&river.PeriodicJobOpts{RunOnStart: true},
		))
	}
	if d.siteEventPruneWorker != nil {
		river.AddWorker(workers, d.siteEventPruneWorker)
		periodics = append(periodics, river.NewPeriodicJob(
			river.PeriodicInterval(time.Minute),
			func() (river.JobArgs, *river.InsertOpts) { return site.EventPruneArgs{}, nil },
			nil,
		))
	}

	// #131 follow-up — periodic reaper for update_tasks stuck in pending/running
	// past the stale-task threshold (45 min; see update.staleTaskThreshold).
	// Always registered; cross-tenant, no signing key required. RunOnStart:
	// true — unlike the perf watchdogs (which skip a fresh-boot false positive
	// since nothing could be in flight yet), a task already stuck for 45+
	// minutes should be reaped immediately on boot, not held for up to a full
	// tick interval. 10-minute interval keeps the sweep cheap while unblocking
	// a stuck (site, target) reasonably promptly.
	if d.updateReaperWorker != nil {
		periodics = append(periodics, river.NewPeriodicJob(
			river.PeriodicInterval(10*time.Minute),
			func() (river.JobArgs, *river.InsertOpts) { return update.ReapStaleTasksArgs{}, nil },
			&river.PeriodicJobOpts{RunOnStart: true},
		))
	}

	backupsEnabled := d.backupWorker != nil
	if backupsEnabled {
		river.AddWorker(workers, d.backupWorker)
		river.AddWorker(workers, d.restoreWorker)
		river.AddWorker(workers, d.gcWorker)
		river.AddWorker(workers, d.scheduleWorker)
		// M6 / Track 4: SQL inspection legacy parser. Pinned to its own queue
		// (sql_inspect_legacy) with MaxWorkers=1 per CP instance — a streaming
		// SQL parse is CPU-heavy and the operator-poll cadence is generous, so
		// queue depth >1 doesn't help any one user and would risk OOM on a
		// multi-GB dump if two ran in parallel.
		if d.sqlInspectLegacyWorker != nil {
			river.AddWorker(workers, d.sqlInspectLegacyWorker)
			queues[backup.SqlInspectLegacyQueue] = river.QueueConfig{MaxWorkers: 1}
		}

		schedInterval := d.scheduleInterval
		if schedInterval <= 0 {
			schedInterval = 5 * time.Minute
		}
		gcInterval := d.gcInterval
		if gcInterval <= 0 {
			gcInterval = time.Hour
		}
		periodics = append(periodics,
			river.NewPeriodicJob(
				river.PeriodicInterval(schedInterval),
				func() (river.JobArgs, *river.InsertOpts) {
					return backup.ScheduleArgs{}, &river.InsertOpts{
						// Deduplicate: at most one pending/running backup_scheduler job
						// at a time across all CP instances. ByArgs keys on the (empty)
						// ScheduleArgs JSON {}; ByPeriod caps one per schedInterval window.
						// This prevents RunOnStart from enqueuing a second job while the
						// previous tick is still running, and prevents rolling-deploy
						// double-fires.
						UniqueOpts: river.UniqueOpts{
							ByArgs:   true,
							ByPeriod: schedInterval,
						},
					}
				},
				&river.PeriodicJobOpts{RunOnStart: true},
			),
			river.NewPeriodicJob(
				river.PeriodicInterval(gcInterval),
				func() (river.JobArgs, *river.InsertOpts) { return backup.GCArgs{}, nil },
				nil,
			),
		)
		if d.progressWatchdog != nil {
			river.AddWorker(workers, d.progressWatchdog)
			// 30s tick is half the stall threshold — guarantees we detect a stall
			// within at most threshold+30s. Cheap (a single indexed SELECT).
			periodics = append(periodics, river.NewPeriodicJob(
				river.PeriodicInterval(30*time.Second),
				func() (river.JobArgs, *river.InsertOpts) { return backup.ProgressWatchdogArgs{}, nil },
				nil,
			))
		}
	}

	// M5 uptime probe: a periodic job (~60s) that probes every enrolled site,
	// records the time-series, refreshes health_status, and evaluates alerts.
	uptimeEnabled := d.uptimeWorker != nil
	if uptimeEnabled {
		river.AddWorker(workers, d.uptimeWorker)
		probeInterval := d.probeInterval
		if probeInterval <= 0 {
			probeInterval = time.Minute
		}
		periodics = append(periodics,
			river.NewPeriodicJob(
				river.PeriodicInterval(probeInterval),
				func() (river.JobArgs, *river.InsertOpts) { return uptime.ProbeArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true},
			),
		)
	}

	// S1.1 (D) — PHP-error retention GC: always wired, runs once per hour.
	// Deletes agent_php_errors rows with last_seen_at older than 30 days
	// (configured on the worker). Cross-tenant under app.agent GUC.
	if d.phpErrorsGCWorker != nil {
		river.AddWorker(workers, d.phpErrorsGCWorker)
		periodics = append(periodics, river.NewPeriodicJob(
			river.PeriodicInterval(time.Hour),
			func() (river.JobArgs, *river.InsertOpts) { return diagnostics.ErrorsGCArgs{}, nil },
			nil,
		))
	}

	// S3 — Malware / File-Integrity Scan. The scan_run worker drives the
	// multi-step hash-streaming loop; the hash GC worker sweeps orphan
	// staging rows every hour.
	if d.scanRunWorker != nil {
		river.AddWorker(workers, d.scanRunWorker)
		queues[scan.ScanRunQueue] = river.QueueConfig{MaxWorkers: 4}
	}
	if d.scanHashGCWorker != nil {
		river.AddWorker(workers, d.scanHashGCWorker)
		periodics = append(periodics, river.NewPeriodicJob(
			river.PeriodicInterval(time.Hour),
			func() (river.JobArgs, *river.InsertOpts) { return scan.HashGCArgs{}, nil },
			nil,
		))
	}

	// ADR-046 Performance Suite — pure-Go RUCSS computation worker on its own
	// bounded queue. A purge is CPU-bound (HTML parse + cascadia matching), so a
	// small worker pool keeps an agent burst from starving other work.
	if d.rucssWorker != nil {
		rucssworker.RegisterWorker(workers, d.rucssWorker)
		for q, cfg := range rucssworker.Queues(d.rucssQueueParallel) {
			queues[q] = cfg
		}
	}

	// FIX 1 backstop: a periodic sweeper that reaps orphaned RUCSS source bundles
	// (page HTML) under "rucss-src/" older than ~60s — the safety net for jobs
	// whose inline self-delete never ran (enqueue failed / River row lost). Runs
	// on the default queue every 30s (half the max-age window so an orphan is
	// reaped within at most ~90s). An object-storage lifecycle rule on the bucket
	// is the recommended alternative on managed S3/GCS; this exists so the
	// guarantee also holds on lifecycle-less backends (SeaweedFS/MinIO).
	if d.rucssSweepWorker != nil {
		rucssworker.RegisterSweepWorker(workers, d.rucssSweepWorker)
		periodics = append(periodics, river.NewPeriodicJob(
			river.PeriodicInterval(30*time.Second),
			func() (river.JobArgs, *river.InsertOpts) { return rucssworker.RucssSweepArgs{}, nil },
			nil,
		))
	}

	// M38 — CP-owned db-clean scheduling.
	// DBCleanWorker dispatches a single site's cleanup (enqueued by the schedule
	// sweeper or the operator-facing ad-hoc route via River).
	// DBCleanScheduleWorker runs every 5 minutes, sweeps site_perf_config for
	// due auto-clean sites, enqueues a dispatch job per site, and advances
	// next_db_clean_at (so the CP fully owns the auto-clean schedule).
	if d.dbCleanWorker != nil {
		river.AddWorker(workers, d.dbCleanWorker)
	}
	if d.dbCleanScheduleWorker != nil {
		river.AddWorker(workers, d.dbCleanScheduleWorker)
		periodics = append(periodics, river.NewPeriodicJob(
			river.PeriodicInterval(5*time.Minute),
			func() (river.JobArgs, *river.InsertOpts) { return perf.DBCleanScheduleArgs{}, nil },
			&river.PeriodicJobOpts{RunOnStart: false},
		))
	}

	// M39 — watchdog for stalled db_clean (>10 min) + db_scan (>3 min) jobs.
	// Always registered: the watchdog runs cross-tenant and does not need the
	// agent signing key. Runs every 2 minutes; RunOnStart: false avoids a false
	// positive on fresh CP boots where no jobs could be in flight yet.
	if d.dbCleanWatchdogWorker != nil {
		river.AddWorker(workers, d.dbCleanWatchdogWorker)
		periodics = append(periodics, river.NewPeriodicJob(
			river.PeriodicInterval(2*time.Minute),
			func() (river.JobArgs, *river.InsertOpts) { return perf.DBCleanWatchdogArgs{}, nil },
			&river.PeriodicJobOpts{RunOnStart: false},
		))
	}

	// P3.8 — watchdog for stalled db_orphan_delete (>5 min) jobs. Always
	// registered; cross-tenant; no signing key required. Runs every 2 minutes;
	// RunOnStart: false for the same reason as the db_clean watchdog.
	if d.dbOrphanDeleteWatchdogWorker != nil {
		river.AddWorker(workers, d.dbOrphanDeleteWatchdogWorker)
		periodics = append(periodics, river.NewPeriodicJob(
			river.PeriodicInterval(2*time.Minute),
			func() (river.JobArgs, *river.InsertOpts) {
				return perf.DBOrphanDeleteWatchdogArgs{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: false},
		))
	}

	// M42 — DB-size history GC: prune site_db_size_history rows older than
	// 120 days. Always registered; runs once per day cross-tenant (InAgentTx).
	// RunOnStart: false — the table is empty on a fresh deploy; no rush.
	if d.dbSizeHistoryGCWorker != nil {
		river.AddWorker(workers, d.dbSizeHistoryGCWorker)
		periodics = append(periodics, river.NewPeriodicJob(
			river.PeriodicInterval(24*time.Hour),
			func() (river.JobArgs, *river.InsertOpts) { return perf.DBSizeHistoryGCArgs{}, nil },
			&river.PeriodicJobOpts{RunOnStart: false},
		))
	}

	// M52 / #162 — cache hit-ratio history GC: prune
	// site_cache_hit_ratio_history rows older than 120 days. Always
	// registered; runs once per day cross-tenant (InAgentTx).
	// RunOnStart: false — the table is empty on a fresh deploy; no rush.
	if d.cacheHitRatioHistoryGCWorker != nil {
		river.AddWorker(workers, d.cacheHitRatioHistoryGCWorker)
		periodics = append(periodics, river.NewPeriodicJob(
			river.PeriodicInterval(24*time.Hour),
			func() (river.JobArgs, *river.InsertOpts) {
				return perf.CacheHitRatioHistoryGCArgs{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: false},
		))
	}

	// m68 — Object Cache stats history GC: prune
	// site_object_cache_stats_history rows older than 7 days (raw retention D4).
	// Always registered; runs once per day cross-tenant (InAgentTx).
	// RunOnStart: false — table is empty on a fresh deploy.
	if d.ocStatsHistoryGCWorker != nil {
		river.AddWorker(workers, d.ocStatsHistoryGCWorker)
		periodics = append(periodics, river.NewPeriodicJob(
			river.PeriodicInterval(24*time.Hour),
			func() (river.JobArgs, *river.InsertOpts) {
				return objectcache.ObjectCacheStatsHistoryGCArgs{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: false},
		))
	}

	// M56 — RUM retention GC (always wired): sweeps raw events (every 30m),
	// hourly rollups (daily), and daily rollups (daily). Cross-tenant InAgentTx.
	// RunOnStart: false — tables are empty on fresh deploy.
	if d.rumGCWorker != nil {
		river.AddWorker(workers, d.rumGCWorker)
		periodics = append(periodics, river.NewPeriodicJob(
			river.PeriodicInterval(30*time.Minute),
			func() (river.JobArgs, *river.InsertOpts) { return rum.RumGCArgs{}, nil },
			&river.PeriodicJobOpts{RunOnStart: false},
		))
	}
	// M56 — RUM rollup worker (always wired): folds raw events into hourly/daily
	// rollup tables. Jobs are enqueued by the ingest handler (one per site per hour).
	if d.rumRollupWorker != nil {
		river.AddWorker(workers, d.rumRollupWorker)
	}

	// m59 Phase 3 — email log retention GC: sweeps site_email_log rows older
	// than the per-site retention_days (default 14) once per hour.
	// RunOnStart: false — avoids a GC sweep on every deploy/restart.
	if d.emailLogGCWorker != nil {
		river.AddWorker(workers, d.emailLogGCWorker)
		periodics = append(periodics, river.NewPeriodicJob(
			river.PeriodicInterval(1*time.Hour),
			func() (river.JobArgs, *river.InsertOpts) { return email.EmailLogGCArgs{}, nil },
			&river.PeriodicJobOpts{RunOnStart: false},
		))
	}

	// m62 — org-config propagation worker (on-demand, enqueued by UpsertOrgConfig).
	if d.emailOrgPropagateWorker != nil {
		river.AddWorker(workers, d.emailOrgPropagateWorker)
	}

	// m62 — hourly digest worker: fires once per hour, scans due tenant digests.
	// RunOnStart: false — avoids sending a digest on every deploy/restart.
	if d.emailDigestWorker != nil {
		river.AddWorker(workers, d.emailDigestWorker)
		periodics = append(periodics, river.NewPeriodicJob(
			river.PeriodicInterval(1*time.Hour),
			func() (river.JobArgs, *river.InsertOpts) { return email.DigestArgs{}, nil },
			&river.PeriodicJobOpts{RunOnStart: false},
		))
	}

	// m64 — report generation worker (enqueued on-demand + by schedule scanner).
	// Both workers are nil when S3 is not configured (reports require blob storage).
	if d.reportGenerateWorker != nil {
		river.AddWorker(workers, d.reportGenerateWorker)
	}
	if d.reportScheduleScanWorker != nil {
		river.AddWorker(workers, d.reportScheduleScanWorker)
		// Scan every 5 minutes for due report schedules, mirroring the email digest
		// cadence. RunOnStart: false — avoids kicking off reports on every restart.
		periodics = append(periodics, river.NewPeriodicJob(
			river.PeriodicInterval(5*time.Minute),
			func() (river.JobArgs, *river.InsertOpts) { return reportpkg.ScheduleScanArgs{}, nil },
			&river.PeriodicJobOpts{RunOnStart: false},
		))
	}

	// P4b — cron kick: periodically GET wp-cron.php for all enrolled sites so
	// fully page-cached sites boot PHP and drain their WP-Cron queue. Nil when
	// WPMGR_CRON_KICK_ENABLED=false. Feeds NO metrics; does not affect
	// health_status or connection_state.
	if d.cronKickWorker != nil {
		river.AddWorker(workers, d.cronKickWorker)
		kickInterval := d.cronKickInterval
		if kickInterval <= 0 {
			kickInterval = 5 * time.Minute
		}
		periodics = append(periodics, river.NewPeriodicJob(
			river.PeriodicInterval(kickInterval),
			func() (river.JobArgs, *river.InsertOpts) { return uptime.CronKickArgs{}, nil },
			&river.PeriodicJobOpts{RunOnStart: false},
		))
	}

	// m79 — Vulnerability scanner: feed refresh worker + per-site rescan worker.
	// The feed worker no-ops cleanly when WPMGR_WORDFENCE_API_KEY is not set.
	// Both workers are registered unconditionally (always non-nil) so jobs
	// already in the queue are processed even during a rolling redeploy where the
	// key was only just configured.
	if d.vulnFeedWorker != nil {
		river.AddWorker(workers, d.vulnFeedWorker)
		queues[vuln.FeedRefreshQueue] = river.QueueConfig{MaxWorkers: 1}
		// Hourly feed refresh. RunOnStart: false — the first boot does not
		// trigger an immediate full-dump request (respects Wordfence rate limits).
		periodics = append(periodics, river.NewPeriodicJob(
			river.PeriodicInterval(time.Hour),
			func() (river.JobArgs, *river.InsertOpts) {
				return vuln.FeedRefreshArgs{}, &river.InsertOpts{
					Queue: vuln.FeedRefreshQueue,
					// Deduplicate: at most one pending/running feed-refresh job.
					UniqueOpts: river.UniqueOpts{
						ByArgs:   true,
						ByPeriod: time.Hour,
					},
				}
			},
			&river.PeriodicJobOpts{RunOnStart: false},
		))
	}
	if d.vulnRescanWorker != nil {
		river.AddWorker(workers, d.vulnRescanWorker)
		queues[vuln.RescanSiteQueue] = river.QueueConfig{MaxWorkers: 8}
	}

	// m82 — file-transfers GC: prunes stale file_transfers rows (and their
	// staged objects) once per day. Always wired; the worker no-ops cleanly
	// when the deleter is nil (no object storage). RunOnStart: false — tables
	// are small on a fresh deploy.
	if d.fileTransfersGCWorker != nil {
		river.AddWorker(workers, d.fileTransfersGCWorker)
		periodics = append(periodics, river.NewPeriodicJob(
			river.PeriodicInterval(24*time.Hour),
			func() (river.JobArgs, *river.InsertOpts) { return files.FileTransfersGCArgs{}, nil },
			&river.PeriodicJobOpts{RunOnStart: false},
		))
	}

	// m85 — uptime-probe retention GC: prunes site_uptime_probes rows older
	// than 90 days once per day. Bounding the table is the long-term fix for
	// why the 30-day aggregate window grows expensive as rows accumulate.
	// RunOnStart: false — on a fresh deploy or right after the m85 covering
	// index migration the table may be large; let Postgres settle before the
	// first GC pass so the migration boot does not compete with the GC DELETE.
	if d.uptimeProbeGCWorker != nil {
		river.AddWorker(workers, d.uptimeProbeGCWorker)
		periodics = append(periodics, river.NewPeriodicJob(
			river.PeriodicInterval(24*time.Hour),
			func() (river.JobArgs, *river.InsertOpts) { return metrics.UptimeProbeGCArgs{}, nil },
			&river.PeriodicJobOpts{RunOnStart: false},
		))
	}

	// M16 Phase B — daily payment-provider drift-repair sweep. RunOnStart:
	// true — a missed webhook could otherwise leave a tenant's plan wrong for
	// up to a full 24h before the first sweep; running once immediately on
	// boot closes that window without any real cost (the tenant set is small).
	if d.billingReconcileWorker != nil {
		river.AddWorker(workers, d.billingReconcileWorker)
		queues[billing.ReconcileQueue] = river.QueueConfig{MaxWorkers: 1}
		periodics = append(periodics, river.NewPeriodicJob(
			river.PeriodicInterval(24*time.Hour),
			func() (river.JobArgs, *river.InsertOpts) { return billing.ReconcileArgs{}, nil },
			&river.PeriodicJobOpts{RunOnStart: true},
		))
	}

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Logger:       logger,
		Queues:       queues,
		Workers:      workers,
		PeriodicJobs: periodics,
		// M23 Media Optimizer (ADR-043): the API client.Inserts model.EncodeArgs
		// (kind "media_encode") but does NOT register its worker — the CGO
		// EncodeWorker runs only in cmd/media-encoder. Since this client HAS other
		// workers (so it is not "insert-only"), River's validateJobArgs() would
		// otherwise reject the unknown "media_encode" kind with UnknownJobKindError,
		// failing /agent/v1/media/encode-ready with a 500 and leaving every optimize
		// stuck. Skip the check: the separate encoder process is the real worker.
		SkipUnknownJobCheck: true,
	})
	if err != nil {
		return nil, fmt.Errorf("river client: %w", err)
	}
	if err := client.Start(ctx); err != nil {
		return nil, fmt.Errorf("river start: %w", err)
	}
	logger.Info("river worker pool started",
		slog.Duration("health_interval", interval),
		slog.Int("update_per_tenant_parallelism", perTenantParallelism),
		slog.Bool("backups_enabled", backupsEnabled))
	return client, nil
}

// disabledBackupCommander refuses to send backup/restore commands when no CP
// signing key is configured (rather than sending unsigned ones).
type disabledBackupCommander struct{}

func (disabledBackupCommander) Backup(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.BackupRequest) (agentcmd.BackupResponse, error) {
	return agentcmd.BackupResponse{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func (disabledBackupCommander) IncrementalBackup(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.IncrementalBackupRequest) (agentcmd.BackupResponse, error) {
	return agentcmd.BackupResponse{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func (disabledBackupCommander) Restore(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.RestoreRequest) (agentcmd.RestoreResponse, error) {
	return agentcmd.RestoreResponse{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

// disabledCommander is the no-op Commander used when no CP signing key is
// configured: it refuses to send commands rather than sending unsigned ones.
type disabledCommander struct{}

func (disabledCommander) Update(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.UpdateRequest) (agentcmd.UpdateResponse, error) {
	return agentcmd.UpdateResponse{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func (disabledCommander) Rollback(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.RollbackRequest) (agentcmd.RollbackResponse, error) {
	return agentcmd.RollbackResponse{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func (disabledCommander) SyncErrorConfig(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.ErrorConfigRequest) (agentcmd.ErrorConfigResult, error) {
	return agentcmd.ErrorConfigResult{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func (disabledCommander) SyncSecurityConfig(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.SecurityConfigRequest) (agentcmd.SecurityConfigResult, error) {
	return agentcmd.SecurityConfigResult{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func (disabledCommander) SyncSecurityHardening(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.HardeningRequest) (agentcmd.HardeningResult, error) {
	return agentcmd.HardeningResult{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func (disabledCommander) UnblockIP(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.UnblockIPRequest) (agentcmd.UnblockIPResult, error) {
	return agentcmd.UnblockIPResult{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func (disabledCommander) SyncLoginBrand(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.LoginBrandRequest) (agentcmd.LoginBrandResult, error) {
	return agentcmd.LoginBrandResult{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func (disabledCommander) Scan(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.ScanRequest) (agentcmd.ScanResponse, error) {
	return agentcmd.ScanResponse{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func (disabledCommander) GetFile(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.GetFileRequest) (agentcmd.GetFileResponse, error) {
	return agentcmd.GetFileResponse{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

// Media Optimizer (ADR-043) — the disabledCommander refuses every media command
// so the build still satisfies media.AgentMediaClient when no signing key is set.
func (disabledCommander) MediaOptimize(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.MediaOptimizeRequest) (agentcmd.MediaOptimizeResponse, error) {
	return agentcmd.MediaOptimizeResponse{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func (disabledCommander) MediaApply(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.MediaApplyRequest) (agentcmd.MediaApplyResponse, error) {
	return agentcmd.MediaApplyResponse{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func (disabledCommander) MediaSync(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.MediaSyncRequest) (agentcmd.MediaSyncResponse, error) {
	return agentcmd.MediaSyncResponse{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func (disabledCommander) MediaRestore(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.MediaRestoreRequest) (agentcmd.MediaRestoreResponse, error) {
	return agentcmd.MediaRestoreResponse{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func (disabledCommander) MediaDeleteOriginals(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.MediaDeleteOriginalsRequest) (agentcmd.MediaDeleteOriginalsResponse, error) {
	return agentcmd.MediaDeleteOriginalsResponse{}, fmt.Errorf("CP->agent commands are disabled: no signing key configured")
}

func newLogger(cfg config.Config) *slog.Logger {
	level := slog.LevelInfo
	_ = level.UnmarshalText([]byte(cfg.LogLevel))

	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if cfg.IsProduction() {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}
