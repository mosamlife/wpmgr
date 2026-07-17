// Package config loads WPMgr control-plane configuration using koanf, with a
// defaults < file < env precedence and the WPMGR_ env prefix (ADR-007).
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// Config is the fully-typed application configuration.
type Config struct {
	Env        string           `koanf:"env"`
	HTTPAddr   string           `koanf:"http_addr"`
	LogLevel   string           `koanf:"log_level"`
	DB         DBConfig         `koanf:"db"`
	Redis      RedisConfig      `koanf:"redis"`
	Auth       AuthConfig       `koanf:"auth"`
	OIDC       OIDCConfig       `koanf:"oidc"`
	OTel       OTelConfig       `koanf:"otel"`
	Shutdown   ShutdownConfig   `koanf:"shutdown"`
	Agent      AgentConfig      `koanf:"agent"`
	Update     UpdateConfig     `koanf:"update"`
	S3         S3Config         `koanf:"s3"`
	Backup     BackupConfig     `koanf:"backup"`
	ClickHouse ClickHouseConfig `koanf:"clickhouse"`
	SMTP       SMTPConfig       `koanf:"smtp"`
	Uptime     UptimeConfig     `koanf:"uptime"`
	River      RiverConfig      `koanf:"river"`
	Autologin  AutologinConfig  `koanf:"autologin"`
	Conn       ConnConfig       `koanf:"conn"`
	Hosted     HostedConfig     `koanf:"hosted"`
	Billing    BillingConfig    `koanf:"billing"`
}

// BillingConfig gates the M16 Phase B payment-provider integration
// (internal/billing's Provider registry). Every field defaults to "": an
// unhosted or hosted-but-unconfigured boot sees zero required env vars — the
// registry simply registers no provider (see stripe.Config.Configured and
// Validate's billing checks below), matching Phase A's "hosted with zero
// providers is legal" behavior.
type BillingConfig struct {
	Stripe   StripeConfig   `koanf:"stripe"`
	Razorpay RazorpayConfig `koanf:"razorpay"`
}

// StripeConfig holds the Stripe adapter's credentials and tier->price
// mapping. All five fields are required TOGETHER once ANY one of them is
// set (see Validate) — a partially-configured Stripe is refused at boot
// rather than silently registering a broken provider.
type StripeConfig struct {
	// SecretKey is the Stripe secret API key. Env: WPMGR_BILLING_STRIPE_SECRET_KEY.
	SecretKey string `koanf:"secret_key"`
	// WebhookSecret is the Stripe webhook signing secret (whsec_...) used to
	// verify POST /webhooks/billing/stripe. Env: WPMGR_BILLING_STRIPE_WEBHOOK_SECRET.
	WebhookSecret string `koanf:"webhook_secret"`
	// PriceStarter/PriceAgency/PriceScale are the Stripe recurring Price ids
	// for the three paid tiers (created once in the Stripe Dashboard/API —
	// this control plane never creates prices itself). Env:
	// WPMGR_BILLING_STRIPE_PRICE_STARTER / _PRICE_AGENCY / _PRICE_SCALE.
	PriceStarter string `koanf:"price_starter"`
	PriceAgency  string `koanf:"price_agency"`
	PriceScale   string `koanf:"price_scale"`
}

// RazorpayConfig holds the Razorpay adapter's credentials and its
// DUAL-CURRENCY tier->plan mapping: one Razorpay Plan per currency per tier
// (Razorpay has no single multi-currency price object the way Stripe does).
// All nine fields are required TOGETHER once ANY one of them is set (see
// Validate/validateRazorpayConfig) — a partially-configured Razorpay is
// refused at boot rather than silently registering a broken provider.
type RazorpayConfig struct {
	// KeyID is Razorpay's PUBLIC key id, also handed to the frontend's
	// Checkout.js modal. Env: WPMGR_BILLING_RAZORPAY_KEY_ID.
	KeyID string `koanf:"key_id"`
	// KeySecret is Razorpay's secret API key (used for the Subscriptions/Plans
	// REST API's Basic Auth, and for verifying the browser checkout callback's
	// signature). Env: WPMGR_BILLING_RAZORPAY_KEY_SECRET.
	KeySecret string `koanf:"key_secret"`
	// WebhookSecret is the Razorpay webhook signing secret, DISTINCT from
	// KeySecret, used to verify POST /webhooks/billing/razorpay. Env:
	// WPMGR_BILLING_RAZORPAY_WEBHOOK_SECRET.
	WebhookSecret string `koanf:"webhook_secret"`
	// PlanStarterUSD/PlanStarterINR/... are the Razorpay recurring Plan ids
	// for the three paid tiers, ONE PER CURRENCY (created once in the
	// Razorpay Dashboard/API — this control plane never creates plans
	// itself). Env: WPMGR_BILLING_RAZORPAY_PLAN_STARTER_USD /
	// _PLAN_STARTER_INR / _PLAN_AGENCY_USD / _PLAN_AGENCY_INR /
	// _PLAN_SCALE_USD / _PLAN_SCALE_INR.
	PlanStarterUSD string `koanf:"plan_starter_usd"`
	PlanStarterINR string `koanf:"plan_starter_inr"`
	PlanAgencyUSD  string `koanf:"plan_agency_usd"`
	PlanAgencyINR  string `koanf:"plan_agency_inr"`
	PlanScaleUSD   string `koanf:"plan_scale_usd"`
	PlanScaleINR   string `koanf:"plan_scale_inr"`
}

// HostedConfig gates the M16 Phase A hosted-billing entitlement substrate
// (internal/billing). Enabled defaults to FALSE: self-host and current prod
// see zero behavior change — every entitlement check no-ops (Unlimited()) —
// until an operator explicitly sets WPMGR_HOSTED=true. There is no payment-
// provider integration yet (Phase B); this phase is the plan/site-cap
// substrate only.
type HostedConfig struct {
	Enabled bool `koanf:"enabled"`
}

// ConnConfig holds the M21 connection-lifecycle sweeper tunables (M58).
//
// DegradeAfter is how long a site's last_seen_at may be stale before the
// sweeper considers it overdue. DegradeMissThreshold is the number of
// consecutive overdue evaluations required before the site is transitioned to
// degraded (the hysteresis counter — prevents one-late-beat flaps on
// traffic-gated wp-cron sites). DisconnectAfter is the hard cutoff for the
// degraded→disconnected transition; it remains a single-evaluation threshold.
type ConnConfig struct {
	// DegradeAfter is the staleness window before a connected site is considered
	// overdue. Default 300s (5×60s beats). Env: WPMGR_CONN_DEGRADE_AFTER.
	DegradeAfter time.Duration `koanf:"degrade_after"`
	// DegradeMissThreshold is the consecutive-miss count before degrading.
	// Default 3. Env: WPMGR_CONN_DEGRADE_MISS_THRESHOLD.
	DegradeMissThreshold int `koanf:"degrade_miss_threshold"`
	// DisconnectAfter is the staleness window before a degraded site is
	// disconnected. Default 900s. Env: WPMGR_CONN_DISCONNECT_AFTER.
	DisconnectAfter time.Duration `koanf:"disconnect_after"`
	// ActiveVerify enables CP-initiated active liveness checks (0.44.0).
	// When true (the default) the sweeper dials the agent before executing the
	// degrade or disconnect transition; a successful ping/metadata response
	// resets the miss counter and skips the transition.
	// Set WPMGR_SWEEP_ACTIVE_VERIFY=false to revert to the passive sweeper.
	ActiveVerify bool `koanf:"active_verify"`
	// VerifyTimeout is the per-dial timeout for active liveness checks.
	// Default 8s. Env: WPMGR_SWEEP_VERIFY_TIMEOUT.
	VerifyTimeout time.Duration `koanf:"verify_timeout"`
	// VerifyConcurrency is the maximum number of concurrent active-verify dials
	// per sweep tick. Default 8. Env: WPMGR_SWEEP_VERIFY_CONCURRENCY.
	VerifyConcurrency int `koanf:"verify_concurrency"`
}

// AutologinConfig holds the Phase 5.5 one-click login tunables (ADR-031).
//
// Require2FAStepUp is a GLOBAL kill-switch that masks the per-site policy's
// require_2fa_step_up column: when FALSE (the V0 default), the service ignores
// the per-site flag entirely because the 2FA enrollment system is not built
// yet. Flipping it to TRUE after 2FA ships does NOT require a schema change —
// the per-site column is already in place. Today the 409 "2fa_required" path
// is unreachable; that is intentional and tested.
type AutologinConfig struct {
	Require2FAStepUp bool `koanf:"require_2fa_step_up"`
}

// ClickHouseConfig holds the metrics-store connection (ADR-028). ClickHouse is
// metrics-only (uptime check time-series); Postgres remains the system of
// record. Addr is host:port (clickhouse native protocol). When Addr is empty
// the metrics store is disabled cleanly: the probe worker no-ops its writes and
// uptime queries return empty so the stack still runs without ClickHouse.
type ClickHouseConfig struct {
	Addr     string `koanf:"addr"`
	Database string `koanf:"db"`
	Username string `koanf:"username"`
	Password string `koanf:"password"`
}

// Enabled reports whether the ClickHouse metrics store is configured.
func (c ClickHouseConfig) Enabled() bool { return c.Addr != "" }

// SMTPConfig holds the self-host SMTP relay used for downtime/recovery alert
// emails (ADR-029, go-mail). When Host is empty, email alerts no-op (logged);
// webhook alerts still fire. Password is a credential — never log it.
type SMTPConfig struct {
	Host     string `koanf:"host"`
	Port     int    `koanf:"port"`
	Username string `koanf:"username"`
	Password string `koanf:"password"`
	From     string `koanf:"from"`
	// TLSMode selects the transport security: "starttls" (default), "tls"
	// (implicit TLS / SMTPS), or "none" (plaintext; dev only).
	TLSMode string `koanf:"tls_mode"`
}

// Enabled reports whether SMTP is configured for email alerts.
func (s SMTPConfig) Enabled() bool { return s.Host != "" }

// RiverConfig holds River queue/storage settings shared by API and worker
// binaries.
type RiverConfig struct {
	// MediaSchema moves media-encoder-owned jobs into a dedicated Postgres
	// schema, isolating River leader election from the API's own schema (GH
	// #205: the media-encoder runs workers, making it a leadership candidate
	// on whatever schema it shares — on the API's default/public schema it
	// can silently win leadership and stop the API's entire fleet cron).
	// Defaults to "media_encoder"; empty or "public" keeps the legacy
	// single-schema behavior, which the media-encoder binary now refuses to
	// boot on.
	MediaSchema string `koanf:"media_schema"`
}

// UptimeConfig tunes the M5 uptime monitoring: the probe cadence, the per-probe
// HTTP timeout, the alert-evaluation cadence, and the consecutive-down threshold
// that fires a downtime alert.
type UptimeConfig struct {
	// ProbeInterval is how often the periodic probe job runs (≈60s).
	ProbeInterval time.Duration `koanf:"probe_interval"`
	// ProbeTimeout bounds a single site probe.
	ProbeTimeout time.Duration `koanf:"probe_timeout"`
	// ProbeConcurrency caps how many sites are probed concurrently in one sweep.
	ProbeConcurrency int `koanf:"probe_concurrency"`
	// AlertInterval is how often the alert evaluator runs.
	AlertInterval time.Duration `koanf:"alert_interval"`
	// DownThreshold is the number of consecutive DOWN checks that fires a downtime
	// alert (default 2 — "down > 2 consecutive checks" means the 3rd consecutive).
	DownThreshold int `koanf:"down_threshold"`

	// CronKickEnabled enables the low-frequency cron-kick pass (P4b). When true
	// (the default) the CP periodically fires a GET to each enrolled site's
	// wp-cron.php so that fully page-cached sites boot PHP and drain WP-Cron even
	// with zero PHP-booting organic traffic. Set WPMGR_CRON_KICK_ENABLED=false to
	// disable (e.g. in environments where the page-cache drop-in is not deployed or
	// where the active-verify cadence is already sufficient).
	CronKickEnabled bool `koanf:"cron_kick_enabled"`
	// CronKickInterval is how often the cron-kick periodic job fires.
	// Default 5m. Env: WPMGR_CRON_KICK_INTERVAL.
	CronKickInterval time.Duration `koanf:"cron_kick_interval"`
	// CronKickTimeout bounds a single site kick GET. Default 5s.
	// Env: WPMGR_CRON_KICK_TIMEOUT.
	CronKickTimeout time.Duration `koanf:"cron_kick_timeout"`
	// CronKickConcurrency caps how many concurrent kicks fire per pass.
	// Default 10. Env: WPMGR_CRON_KICK_CONCURRENCY.
	CronKickConcurrency int `koanf:"cron_kick_concurrency"`

	// KeepWarmEnabled gates the INTERIM /sites uptime keep-warm refresher
	// (site.UptimeKeepWarmer) — a stopgap for the recurring 7-8s GET
	// /api/v1/sites latency after ~15-30 min idle (cold Postgres buffer cache
	// + expired 60s result cache backing QueryFleetUptime). Defaults to true;
	// set WPMGR_UPTIME_KEEPWARM=false to disable. REMOVE this flag together
	// with internal/site/uptime_keepwarm.go once the site_uptime_daily rollup
	// lands and becomes the authoritative source for /sites uptime enrichment.
	KeepWarmEnabled bool `koanf:"keepwarm"`
}

// S3Config holds the S3-compatible object-storage configuration (ADR-010).
// WPMgr stores ONLY ciphertext chunks (client-side age-encrypted on the agent)
// at content-addressed keys; the control plane issues presigned PUT/GET URLs so
// the agent transfers bytes directly to/from storage. Endpoint + ForcePathStyle
// support self-hosted SeaweedFS/MinIO as well as managed AWS S3. AccessKey and
// SecretKey are static credentials; never log them.
type S3Config struct {
	Endpoint       string `koanf:"endpoint"`
	Region         string `koanf:"region"`
	Bucket         string `koanf:"bucket"`
	AccessKey      string `koanf:"access_key"`
	SecretKey      string `koanf:"secret_key"`
	ForcePathStyle bool   `koanf:"force_path_style"`
}

// Enabled reports whether object storage is configured (a bucket is the minimum
// requirement). When disabled, backup endpoints return 501.
func (s S3Config) Enabled() bool { return s.Bucket != "" }

// BackupConfig tunes the backup/restore feature: presigned URL TTLs, the
// retention policy (a rolling daily window plus a monthly-archive keep count),
// and the cadence of the scheduler/GC periodic jobs.
type BackupConfig struct {
	// PresignTTL bounds how long a presigned PUT/GET URL stays valid; it must be
	// long enough for the agent to upload/download a chunk but short enough to
	// limit exposure of a leaked URL.
	PresignTTL time.Duration `koanf:"presign_ttl"`
	// RetentionDays is the rolling window: snapshots older than this are pruned by
	// the GC job (unless kept by the monthly-archive rule).
	RetentionDays int `koanf:"retention_days"`
	// MonthlyArchiveKeep is how many monthly-archive snapshots to keep beyond the
	// rolling window (the newest snapshot in each of the last N calendar months).
	MonthlyArchiveKeep int `koanf:"monthly_archive_keep"`
	// ScheduleInterval is how often the scheduler periodic job runs to enqueue due
	// backups from backup_schedules.
	ScheduleInterval time.Duration `koanf:"schedule_interval"`
	// GCInterval is how often the retention GC job runs.
	GCInterval time.Duration `koanf:"gc_interval"`
	// HTTPTimeout bounds a single CP->agent backup/restore command request. It
	// MUST be longer than the agent takes to walk the site, dump the DB, chunk +
	// encrypt, and PUT to S3 — for real sites that easily exceeds the default
	// update HTTPTimeout (30s). Defaults to 10m. The SSRF dialer + per-attempt
	// safety bounds still apply; this only relaxes the wait-for-headers/body cap
	// for the long-running command channel (a separate httpclient.Client is built
	// for backup/restore so the snappy update path is unaffected).
	HTTPTimeout time.Duration `koanf:"http_timeout"`
}

// UpdateConfig holds the M3 bulk-update orchestration tuning.
//
// PerTenantParallelism caps how many of one tenant's update tasks run
// concurrently so a tenant with many sites cannot starve other tenants of the
// shared worker pool (enforced via per-tenant River queue shards plus an
// in-worker guard). HTTPTimeout/HTTPRetries tune the SSRF-hardened client used
// for CP->agent commands and post-update health probes.
//
// ApplyHTTPTimeout (GH #208 Bug 2) is a SEPARATE, longer per-attempt cap used
// only for the update-apply commander (the actual Update/Rollback dispatch),
// not the lighter uses of the shared HTTPTimeout (refresh-inventory probes,
// scan). A real update is heavy and synchronous on the agent — a mandatory
// pre-update snapshot, download, extract, and core/plugin/theme DB migration
// all happen inline in one request — and routinely exceeds the snappy 30s
// HTTPTimeout, which previously drove a spurious CP-recorded "Failed" even
// though the agent had actually finished. Defaults to 5m: longer than the
// 30s shared update timeout, shorter than the 10m backup timeout (an update
// apply + its mandatory snapshot is lighter than a full-site backup), and
// leaves headroom under the agent's own apply script cap
// (set_time_limit(900)) for network/transfer overhead on top of the agent's
// own execution time.
type UpdateConfig struct {
	PerTenantParallelism int           `koanf:"per_tenant_parallelism"`
	HTTPTimeout          time.Duration `koanf:"http_timeout"`
	HTTPRetries          int           `koanf:"http_retries"`
	ApplyHTTPTimeout     time.Duration `koanf:"apply_http_timeout"`
}

// AgentConfig holds the control-plane agent-protocol configuration.
//
// SigningPrivateKey / SigningPublicKey are the control-plane's OWN Ed25519
// keypair (base64 std), used to sign CP->agent commands; the public half is
// returned to the agent at enrollment so it can verify those commands. They are
// distinct from each site's agent_public_key (agent->CP direction).
//
// SignatureSkew bounds how far a signed agent request's timestamp may differ
// from now (anti-replay window). StaleAfter is the agent-heartbeat freshness
// threshold: a site whose last_seen_at is older is marked unreachable by the
// periodic health job. HealthInterval is how often that job runs.
type AgentConfig struct {
	SigningPrivateKey string        `koanf:"signing_private_key"`
	SigningPublicKey  string        `koanf:"signing_public_key"`
	SignatureSkew     time.Duration `koanf:"signature_skew"`
	StaleAfter        time.Duration `koanf:"stale_after"`
	HealthInterval    time.Duration `koanf:"health_interval"`
}

// DBConfig holds Postgres connection parts.
//
// The application connects with the DSN built from these parts (a NOSUPERUSER
// NOBYPASSRLS role in any sane deployment). Migrations, which must CREATE ROLE
// and run privileged DDL, use MigrationDSN when set; otherwise they fall back
// to the app DSN. See apps/api/README.md "Two-DSN model".
type DBConfig struct {
	Host     string `koanf:"host"`
	Port     int    `koanf:"port"`
	User     string `koanf:"user"`
	Password string `koanf:"password"`
	Name     string `koanf:"name"`
	SSLMode  string `koanf:"sslmode"`
	// MigrationDSN is an explicit owner/superuser connection string used ONLY
	// to run migrations (which provision roles and privileged DDL). Empty means
	// "use the app DSN for migrations too" (single-DSN dev fallback).
	MigrationDSN string `koanf:"migration_dsn"`
	// AllowRLSBypassRole is the escape hatch that downgrades the
	// superuser/BYPASSRLS startup check from a hard failure to a loud warning.
	// Intended only for single-node dev where the app shares the bootstrap
	// superuser. Defaults to false (hard fail) — never enable in production.
	AllowRLSBypassRole bool `koanf:"allow_rls_bypass_role"`
}

// RedisConfig holds the Redis connection used for the session store (SCS).
type RedisConfig struct {
	Addr     string `koanf:"addr"`
	Password string `koanf:"password"`
}

// AuthConfig holds session/cookie keying and lifetimes.
type AuthConfig struct {
	// SessionSecret keys the session store. It MUST be a non-placeholder value
	// of at least 32 bytes; the server refuses to boot otherwise.
	SessionSecret  string        `koanf:"session_secret"`
	IdleTimeout    time.Duration `koanf:"idle_timeout"`
	AbsoluteExpiry time.Duration `koanf:"absolute_expiry"`

	// WebAuthn relying party configuration (ADR-056 Phase 1).
	// RPID is the effective domain for WebAuthn (e.g. "manage.wpmgr.app").
	// RPOrigins is a comma-separated list of allowed origins
	// (e.g. "https://manage.wpmgr.app"). Self-hosted operators MUST set these
	// to match their WPMGR_PUBLIC_BASE_URL; defaults cover the hosted instance.
	// RPDisplayName is the human-readable site name shown in passkey prompts.
	WebAuthnRPID          string `koanf:"webauthn_rpid"`
	WebAuthnRPOrigins     string `koanf:"webauthn_rp_origins"`
	WebAuthnRPDisplayName string `koanf:"webauthn_rp_display_name"`
}

// OIDCConfig holds the OpenID Connect relying-party configuration. When Issuer
// is empty the OIDC routes are disabled cleanly (email+password still works).
type OIDCConfig struct {
	Issuer       string `koanf:"issuer"`
	ClientID     string `koanf:"client_id"`
	ClientSecret string `koanf:"client_secret"`
	RedirectURL  string `koanf:"redirect_url"`
}

// Enabled reports whether OIDC is configured.
func (o OIDCConfig) Enabled() bool { return o.Issuer != "" }

// OTelConfig holds OpenTelemetry export configuration.
type OTelConfig struct {
	OTLPEndpoint string `koanf:"exporter_otlp_endpoint"`
	ServiceName  string `koanf:"service_name"`
}

// ShutdownConfig controls graceful-shutdown timing.
type ShutdownConfig struct {
	Timeout time.Duration `koanf:"timeout"`
}

// DSN renders the application libpq/pgx connection string from the DB parts.
func (d DBConfig) DSN() string {
	return fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s?sslmode=%s",
		d.User, d.Password, d.Host, d.Port, d.Name, d.SSLMode,
	)
}

// MigrateDSN returns the connection string used to run migrations: the explicit
// MigrationDSN (owner/superuser) when set, otherwise the app DSN (dev fallback).
//
// Migrations perform privileged DDL — CREATE ROLE wpmgr_app (m1), ALTER DEFAULT
// PRIVILEGES, CREATE POLICY, GRANT/REVOKE — so they must run as an owner/
// superuser role, not the unprivileged app role. In production set the owner
// DSN; in single-DSN dev the bootstrap superuser doubles as both. The
// plugin_signatures seed (m40.1) is authored to be resilient to either model:
// it INSERTs the corpus rows while wpmgr_app still holds the m1 default INSERT
// grant, then REVOKEs that grant, so the seed succeeds whether the runner is the
// owner or wpmgr_app itself, ending with wpmgr_app SELECT-only either way.
func (d DBConfig) MigrateDSN() string {
	if d.MigrationDSN != "" {
		return d.MigrationDSN
	}
	return d.DSN()
}

// IsProduction reports whether we should emit JSON logs and stricter behavior.
func (c Config) IsProduction() bool {
	return strings.EqualFold(c.Env, "production") || strings.EqualFold(c.Env, "prod")
}

// ValidateSessionSecret refuses weak/placeholder session secrets. The secret
// keys the session store; an empty, placeholder, or short value is a security
// hole, so the server must not boot with one.
func (c Config) ValidateSessionSecret() error {
	s := c.Auth.SessionSecret
	if s == "" {
		return fmt.Errorf("WPMGR_SESSION_SECRET is empty: set a random secret of at least 32 bytes")
	}
	if strings.HasPrefix(s, "change-me") {
		return fmt.Errorf("WPMGR_SESSION_SECRET still holds the placeholder value: set a real random secret of at least 32 bytes")
	}
	if len(s) < 32 {
		return fmt.Errorf("WPMGR_SESSION_SECRET is too short (%d bytes): use at least 32 bytes", len(s))
	}
	return nil
}

// devAgentSigningPrivateKeys is the hardcoded list of known committed dev
// control-plane signing private keys (base64 std). These ship in .env.example
// for local development; booting in production with one of them would let
// anyone who read the public repo forge CP->agent commands, so the server
// refuses to start. Add any future dev/sample keys here.
var devAgentSigningPrivateKeys = []string{
	"aWuH1W3DSfBwuE/V/H9BEmV9IAJfK5d6F2RDfYSj/raBW+b26qHT3spd1gHSw7aXEXxZkg9E9WMspibSjSFsnQ==",
}

// ValidateAgentSigningKey refuses to boot in production with a known committed
// dev control-plane signing private key. An empty key keeps the OIDC/CP-signing
// disabled behavior unchanged (dev convenience), and the check is enforced only
// in production so dev keeps working with the .env.example value.
func (c Config) ValidateAgentSigningKey() error {
	if !c.IsProduction() {
		return nil
	}
	k := c.Agent.SigningPrivateKey
	if k == "" {
		// Empty = CP signing disabled; left to other startup wiring.
		return nil
	}
	for _, dev := range devAgentSigningPrivateKeys {
		if k == dev {
			return fmt.Errorf("WPMGR_AGENT_SIGNING_PRIVATE_KEY holds a known committed dev key: generate a fresh control-plane Ed25519 keypair for production")
		}
	}
	return nil
}

func defaults() map[string]any {
	return map[string]any{
		"env":                      "development",
		"http_addr":                ":8080",
		"log_level":                "info",
		"db.host":                  "localhost",
		"db.port":                  5432,
		"db.user":                  "wpmgr",
		"db.password":              "wpmgr",
		"db.name":                  "wpmgr",
		"db.sslmode":               "disable",
		"db.migration_dsn":         "",
		"db.allow_rls_bypass_role": false,
		"redis.addr":               "localhost:6379",
		"redis.password":           "",
		"auth.session_secret":      "",
		"auth.idle_timeout":        "168h", // 7 days idle
		"auth.absolute_expiry":     "720h", // 30 days hard cap
		// ADR-056: WebAuthn relying party defaults (hosted instance).
		// Self-hosted operators override via WPMGR_AUTH_WEBAUTHN_RPID etc.
		"auth.webauthn_rpid":                "manage.wpmgr.app",
		"auth.webauthn_rp_origins":          "https://manage.wpmgr.app",
		"auth.webauthn_rp_display_name":     "WPMgr",
		"oidc.issuer":                       "",
		"oidc.client_id":                    "",
		"oidc.client_secret":                "",
		"oidc.redirect_url":                 "",
		"otel.exporter_otlp_endpoint":       "",
		"otel.service_name":                 "wpmgr-api",
		"shutdown.timeout":                  "15s",
		"agent.signing_private_key":         "",
		"agent.signing_public_key":          "",
		"agent.signature_skew":              "5m",
		"agent.stale_after":                 "10m", // ~2 missed 5-min heartbeats
		"agent.health_interval":             "5m",
		"update.per_tenant_parallelism":     5,
		"update.http_timeout":               "30s",
		"update.http_retries":               2,
		"update.apply_http_timeout":         "5m",
		"s3.endpoint":                       "",
		"s3.region":                         "us-east-1",
		"s3.bucket":                         "",
		"s3.access_key":                     "",
		"s3.secret_key":                     "",
		"s3.force_path_style":               true,
		"backup.presign_ttl":                "1h",
		"backup.retention_days":             30,
		"backup.monthly_archive_keep":       12,
		"backup.schedule_interval":          "5m",
		"backup.gc_interval":                "1h",
		"backup.http_timeout":               "10m",
		"clickhouse.addr":                   "",
		"clickhouse.db":                     "wpmgr_metrics",
		"clickhouse.username":               "default",
		"clickhouse.password":               "",
		"smtp.host":                         "",
		"smtp.port":                         587,
		"smtp.username":                     "",
		"smtp.password":                     "",
		"smtp.from":                         "",
		"smtp.tls_mode":                     "starttls",
		"uptime.probe_interval":             "60s",
		"uptime.probe_timeout":              "15s",
		"uptime.probe_concurrency":          10,
		"uptime.alert_interval":             "60s",
		"uptime.down_threshold":             2,
		"uptime.cron_kick_enabled":          true,
		"uptime.cron_kick_interval":         "5m",
		"uptime.cron_kick_timeout":          "5s",
		"uptime.cron_kick_concurrency":      10,
		"uptime.keepwarm":                   true,
		"river.media_schema":                "media_encoder",
		"autologin.require_2fa_step_up":     false,
		"conn.degrade_after":                "300s",
		"conn.degrade_miss_threshold":       3,
		"conn.disconnect_after":             "900s",
		"conn.active_verify":                true,
		"conn.verify_timeout":               "8s",
		"conn.verify_concurrency":           8,
		"hosted.enabled":                    false,
		"billing.stripe.secret_key":         "",
		"billing.stripe.webhook_secret":     "",
		"billing.stripe.price_starter":      "",
		"billing.stripe.price_agency":       "",
		"billing.stripe.price_scale":        "",
		"billing.razorpay.key_id":           "",
		"billing.razorpay.key_secret":       "",
		"billing.razorpay.webhook_secret":   "",
		"billing.razorpay.plan_starter_usd": "",
		"billing.razorpay.plan_starter_inr": "",
		"billing.razorpay.plan_agency_usd":  "",
		"billing.razorpay.plan_agency_inr":  "",
		"billing.razorpay.plan_scale_usd":   "",
		"billing.razorpay.plan_scale_inr":   "",
	}
}

// Load builds Config from defaults, an optional YAML file, then WPMGR_ env vars.
// The path may be empty to skip file loading.
func Load(path string) (Config, error) {
	k := koanf.New(".")

	if err := k.Load(confmap.Provider(defaults(), "."), nil); err != nil {
		return Config{}, fmt.Errorf("load defaults: %w", err)
	}

	if path != "" {
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
			return Config{}, fmt.Errorf("load config file %q: %w", path, err)
		}
	}

	// Env: WPMGR_DB_HOST -> db.host, WPMGR_HTTP_ADDR -> http_addr, etc.
	// We strip the WPMGR_ prefix, lowercase, then map the documented
	// double-underscore-free names by replacing the first underscore segment.
	envProvider := env.ProviderWithValue("WPMGR_", ".", func(key, value string) (string, any) {
		k := strings.ToLower(strings.TrimPrefix(key, "WPMGR_"))
		k = mapEnvKey(k)
		return k, value
	})
	if err := k.Load(envProvider, nil); err != nil {
		return Config{}, fmt.Errorf("load env: %w", err)
	}

	var cfg Config
	if err := k.Unmarshal("", &cfg); err != nil {
		return Config{}, fmt.Errorf("unmarshal config: %w", err)
	}
	return cfg, nil
}

// mapEnvKey maps the flat WPMGR_* env names (see .env.example) to the nested
// koanf key path. Only the variables this service consumes are mapped; unknown
// keys pass through unchanged (and are ignored on unmarshal).
func mapEnvKey(k string) string {
	switch {
	case k == "http_addr":
		return "http_addr"
	case k == "log_level":
		return "log_level"
	case k == "env":
		return "env"
	// Escape hatch: WPMGR_ALLOW_RLS_BYPASS_ROLE -> db.allow_rls_bypass_role.
	case k == "allow_rls_bypass_role":
		return "db.allow_rls_bypass_role"
	// WPMGR_SESSION_SECRET -> auth.session_secret.
	case k == "session_secret":
		return "auth.session_secret"
	case strings.HasPrefix(k, "auth_"):
		return "auth." + strings.TrimPrefix(k, "auth_")
	case strings.HasPrefix(k, "oidc_"):
		return "oidc." + strings.TrimPrefix(k, "oidc_")
	case strings.HasPrefix(k, "redis_"):
		return "redis." + strings.TrimPrefix(k, "redis_")
	case strings.HasPrefix(k, "db_"):
		return "db." + strings.TrimPrefix(k, "db_")
	case strings.HasPrefix(k, "otel_"):
		return "otel." + strings.TrimPrefix(k, "otel_")
	case strings.HasPrefix(k, "agent_"):
		return "agent." + strings.TrimPrefix(k, "agent_")
	case strings.HasPrefix(k, "update_"):
		return "update." + strings.TrimPrefix(k, "update_")
	case strings.HasPrefix(k, "s3_"):
		return "s3." + strings.TrimPrefix(k, "s3_")
	case strings.HasPrefix(k, "backup_"):
		return "backup." + strings.TrimPrefix(k, "backup_")
	case strings.HasPrefix(k, "clickhouse_"):
		return "clickhouse." + strings.TrimPrefix(k, "clickhouse_")
	case strings.HasPrefix(k, "smtp_"):
		return "smtp." + strings.TrimPrefix(k, "smtp_")
	case strings.HasPrefix(k, "uptime_"):
		return "uptime." + strings.TrimPrefix(k, "uptime_")
	case strings.HasPrefix(k, "river_"):
		return "river." + strings.TrimPrefix(k, "river_")
	case strings.HasPrefix(k, "autologin_"):
		return "autologin." + strings.TrimPrefix(k, "autologin_")
	case strings.HasPrefix(k, "conn_"):
		return "conn." + strings.TrimPrefix(k, "conn_")
	// WPMGR_SWEEP_ACTIVE_VERIFY -> conn.active_verify (0.44.0 active liveness).
	case k == "sweep_active_verify":
		return "conn.active_verify"
	// WPMGR_SWEEP_VERIFY_TIMEOUT -> conn.verify_timeout.
	case k == "sweep_verify_timeout":
		return "conn.verify_timeout"
	// WPMGR_SWEEP_VERIFY_CONCURRENCY -> conn.verify_concurrency.
	case k == "sweep_verify_concurrency":
		return "conn.verify_concurrency"
	// WPMGR_CRON_KICK_ENABLED -> uptime.cron_kick_enabled (P4b).
	case k == "cron_kick_enabled":
		return "uptime.cron_kick_enabled"
	// WPMGR_CRON_KICK_INTERVAL -> uptime.cron_kick_interval.
	case k == "cron_kick_interval":
		return "uptime.cron_kick_interval"
	// WPMGR_CRON_KICK_TIMEOUT -> uptime.cron_kick_timeout.
	case k == "cron_kick_timeout":
		return "uptime.cron_kick_timeout"
	// WPMGR_CRON_KICK_CONCURRENCY -> uptime.cron_kick_concurrency.
	case k == "cron_kick_concurrency":
		return "uptime.cron_kick_concurrency"
	// WPMGR_HOSTED -> hosted.enabled (M16 Phase A: hosted-billing entitlement
	// substrate). A single flat flag, not "hosted_enabled", since it is the
	// only knob this phase introduces.
	case k == "hosted":
		return "hosted.enabled"
	// WPMGR_BILLING_STRIPE_* -> billing.stripe.* (M16 Phase B).
	case strings.HasPrefix(k, "billing_stripe_"):
		return "billing.stripe." + strings.TrimPrefix(k, "billing_stripe_")
	// WPMGR_BILLING_RAZORPAY_* -> billing.razorpay.* (M16 Phase B, Razorpay
	// adapter). Without this case every WPMGR_BILLING_RAZORPAY_* variable is
	// silently dropped by koanf's env provider (falls through to the default
	// passthrough below, which unmarshal simply ignores).
	case strings.HasPrefix(k, "billing_razorpay_"):
		return "billing.razorpay." + strings.TrimPrefix(k, "billing_razorpay_")
	default:
		return k
	}
}
