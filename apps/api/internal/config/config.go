// Package config loads WPMgr control-plane configuration using koanf, with a
// defaults < file < env precedence and the WPMGR_ env prefix (ADR-007).
package config

import (
	"fmt"
	"strconv"
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
	Env      string `koanf:"env"`
	HTTPAddr string `koanf:"http_addr"`
	LogLevel string `koanf:"log_level"`
	// PublicBaseURL (WPMGR_PUBLIC_BASE_URL) is the externally reachable origin
	// of this control plane: the origin every link the product hands out is
	// built from (password reset, invitations, agent callbacks, the derived
	// social sign-in redirect_uri).
	//
	// It is typed here so there is ONE value. It used to be read with
	// os.Getenv at sixteen call sites, which meant a YAML-configured install
	// had a public_base_url nothing read, and any check of the variable judged
	// a string no consumer used. Load normalizes it (see
	// NormalizePublicBaseURL) so the value checked and the value used are the
	// same string, byte for byte.
	PublicBaseURL string           `koanf:"public_base_url"`
	DB            DBConfig         `koanf:"db"`
	Redis         RedisConfig      `koanf:"redis"`
	Auth          AuthConfig       `koanf:"auth"`
	OIDC          OIDCConfig       `koanf:"oidc"`
	Social        SocialConfig     `koanf:"social"`
	OTel          OTelConfig       `koanf:"otel"`
	Shutdown      ShutdownConfig   `koanf:"shutdown"`
	Agent         AgentConfig      `koanf:"agent"`
	Update        UpdateConfig     `koanf:"update"`
	S3            S3Config         `koanf:"s3"`
	Backup        BackupConfig     `koanf:"backup"`
	ClickHouse    ClickHouseConfig `koanf:"clickhouse"`
	SMTP          SMTPConfig       `koanf:"smtp"`
	Uptime        UptimeConfig     `koanf:"uptime"`
	River         RiverConfig      `koanf:"river"`
	Autologin     AutologinConfig  `koanf:"autologin"`
	Conn          ConnConfig       `koanf:"conn"`
	Hosted        HostedConfig     `koanf:"hosted"`
	Billing       BillingConfig    `koanf:"billing"`
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
	// for the three paid tiers (created once in the Stripe Dashboard/API, // this control plane never creates prices itself). Env:
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
// see zero behavior change, every entitlement check no-ops (Unlimited()), // until an operator explicitly sets WPMGR_HOSTED=true. There is no payment-
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
// yet. Flipping it to TRUE after 2FA ships does NOT require a schema change, // the per-site column is already in place. Today the 409 "2fa_required" path
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

	// AppProbeEnabled enables the GH #291 Phase 2 application-health probe
	// (B0-B3, see internal/uptime/app_probe.go). Default true - the design
	// is "measure and display from day one", with alerting (a later phase)
	// staying opt-in separately. Set WPMGR_UPTIME_APP_PROBE_ENABLED=false to
	// disable entirely (the reachability probe and everything it feeds are
	// completely unaffected either way).
	AppProbeEnabled bool `koanf:"app_probe_enabled"`
	// AppProbeInterval is the desired app-probe cadence, piggybacked onto the
	// existing reachability sweep via the stateless appProbeDue cadence
	// check rather than its own periodic job. Default 300s (slower than the
	// 60s reachability probe on purpose - see the design doc's "measure
	// first" rollout notes). Env: WPMGR_UPTIME_APP_PROBE_INTERVAL.
	AppProbeInterval time.Duration `koanf:"app_probe_interval"`
	// AppProbeTimeout bounds a single app-probe HTTP attempt (B1, B2, or the
	// B3 override - each gets its own timeout, not a combined one). Default
	// 10s. Env: WPMGR_UPTIME_APP_PROBE_TIMEOUT.
	AppProbeTimeout time.Duration `koanf:"app_probe_timeout"`

	// AppAlertThreshold (GH #291 Phase 3) is the number of consecutive
	// CONCLUSIVE-false app-probe verdicts that fires an app-down alert.
	// Default 5 (~25 minutes at the documented 300s app-probe cadence).
	// Env: WPMGR_UPTIME_APP_ALERT_THRESHOLD.
	AppAlertThreshold int `koanf:"app_alert_threshold"`
	// AppAlertBreakerRatio (GH #291 Phase 3) is the fleet circuit breaker's
	// trip ratio: when MORE than this fraction of a tenant's alert-eligible
	// sites are simultaneously app-down, individual per-site alerts collapse
	// into one aggregate notification. Default 0.25 (25%).
	// Env: WPMGR_UPTIME_APP_ALERT_BREAKER_RATIO.
	AppAlertBreakerRatio float64 `koanf:"app_alert_breaker_ratio"`

	// MaxFleetSize is the fleet size the probe sweep's River job-level
	// Timeout() budget is sized against (see uptime.DeriveProbeJobTimeout).
	// It is deliberately NOT "however many sites are enrolled right now", a
	// live count would go stale as the fleet grows and could silently
	// reintroduce the exact job-timeout mismatch this budget exists to fix.
	// Default 2000 (uptime.DefaultMaxFleetSizeForProbeTimeout), which
	// comfortably exceeds any fleet this deployment has been operated
	// against to date; raise it only if a single deployment's enrolled site
	// count genuinely approaches that ceiling. A fleet that outgrows this
	// value is still not silently broken: the sweep's own admission-control
	// backstop degrades it to a partial-but-recorded result instead of an
	// abrupt cancellation (see uptime.ProbeWorker.Sweep). <= 0 falls back to
	// the default. Env: WPMGR_UPTIME_MAX_FLEET_SIZE.
	MaxFleetSize int `koanf:"max_fleet_size"`
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
	// StallSoftTimeout is the GH #279 two-tier progress watchdog's SOFT
	// deadline: a running snapshot whose progress has gone quiet this long is
	// stamped stalled_at (status stays 'running', a slow-but-alive run can
	// still complete) and the UI shows a "taking longer than expected" hint.
	// Cleared by the next proof of life. Defaults to 3m.
	StallSoftTimeout time.Duration `koanf:"stall_soft_timeout"`
	// StallHardTimeout is the two-tier watchdog's HARD deadline: a running
	// snapshot whose progress has gone quiet this long is actually failed,
	// with a distinct stall-timeout reason. MUST be generous enough to cover
	// the agent's worst-case total silent gap on a very large site. Defaults
	// to 30m.
	StallHardTimeout time.Duration `koanf:"stall_hard_timeout"`
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
// though the agent had actually finished.
//
// This budget also covers the agent's OWN self-update apply. On a SAPI that
// cannot detach the connection (mod_php, plain CGI), the control plane holds
// this same connection open for the agent's entire apply, not just the
// package download: the agent's own package-download budget alone
// (PACKAGE_TIMEOUT, 300s) can consume nearly all of a 5m cap and leave
// nothing for the unzip and swap that follow it. Defaults to 8m: still well
// inside the 10m backup timeout (an update apply + its mandatory snapshot is
// lighter than a full-site backup), longer than the 30s shared update
// timeout, and leaves real headroom over the agent's 300s download budget for
// the unzip/swap phase.
//
// It deliberately does NOT try to cover the agent's own 900s apply cap, and
// it could not: the range this value is pinned to tops out below that. On a
// slow non-detaching host this deadline can therefore expire while the apply
// is still legitimately running. That is not treated as a failure, because it
// is not evidence of one. A timeout here resolves the task as UNCERTAIN and
// enqueues the same confirmation poll a normal arm would, so the outcome is
// decided by the site reporting its own new version rather than by whether
// one HTTP read outlasted one file swap. Making the control plane wait out
// the whole apply would be the wrong fix for the same reason: the connection
// is not the success signal.
//
// AgentSelfUpdateEnabled (WPMGR_UPDATE_AGENT_SELF_UPDATE_ENABLED) is the
// fleet-wide kill switch for the agent's OWN upgrade channel, and it DEFAULTS
// TO FALSE. While false, no agent self-update command is sent to any site,
// whatever runs already exist and whatever jobs are already enqueued: the
// worker checks it immediately before dispatch, not merely at run creation.
//
// It has to stop DISPATCH rather than work through the release channel,
// because repointing the published manifest at an older build does not
// un-brick anyone, the agent's downgrade guard refuses to install anything
// older than what it is already running. Turning this off is therefore the
// only thing that actually stops an agent rollout in progress.
//
// AgentMirrorEnabled (WPMGR_UPDATE_AGENT_MIRROR_ENABLED) is the switch for the
// upstream agent-release MIRROR (GH #302, internal/agentupstream), and it too
// DEFAULTS TO FALSE. While false the mirror job no-ops: nothing is fetched from
// the public internet and nothing is written into the operator's bucket.
//
// It exists because a self-hosted install has no published agent release at all:
// the release pipeline writes into the hosted service's bucket, and nothing ever
// writes into a self-hoster's own storage, so their dashboard has no reference
// version and their sites are never offered an upgrade. Turning this on makes the
// control plane read the public GitHub release, verify it end to end, and publish
// the very same two objects into its OWN storage, after which every existing path
// is unchanged and unaware.
//
// AgentMirrorOwner/AgentMirrorRepo (WPMGR_UPDATE_AGENT_MIRROR_OWNER /
// WPMGR_UPDATE_AGENT_MIRROR_REPO) name the upstream GitHub repository, so a fork
// can mirror its own releases instead. They default to the upstream project. Both
// are interpolated into a URL path and are shape-validated before use.
//
// AgentMirrorAllowRollback (WPMGR_UPDATE_AGENT_MIRROR_ALLOW_ROLLBACK) relaxes
// the mirror's strictly-newer rule, and DEFAULTS TO FALSE. Normally the mirror
// only ever moves this install's published agent version forward: an upstream
// release older than the one already mirrored is refused, because repointing
// backwards does not downgrade any site (the agent refuses to install something
// older than it is running) but it does make the fleet dashboard report the
// wrong reference version and offer a newly enrolled site the wrong build.
//
// The one case that rule gets wrong is a GENUINE upstream rollback: a bad
// release is yanked, /releases/latest starts answering with the previous one,
// and an operator who wants their install to follow it back needs to be able to
// say so. This is that switch. It is deliberately explicit rather than the
// default, because left on permanently it is also what would let a
// yanked-then-restored upstream flap the published version. It does NOT let the
// mirror overwrite a pointer it did not publish; that protection has no switch.
//
// AgentPackageServeEnabled (WPMGR_UPDATE_AGENT_PACKAGE_SERVE_ENABLED) is the
// second half of GH #302 and it too DEFAULTS TO FALSE. While false the signed
// manifest's package_url stays a presigned object-storage URL, exactly as before,
// and the control plane's own package route refuses everything. Turning it on
// makes the control plane SERVE the mirrored package itself, which is what
// removes the per-site WPMGR_AGENT_PACKAGE_HOST edit: a mirrored object presigns
// onto the operator's own storage host, which differs per install and so can
// never be an agent default, whereas the control plane's host is one the agent is
// already enrolled with.
//
// It defaults to false because of ROLLOUT ORDER, not doubt about the design. Only
// agents new enough to trust their own control-plane host accept a CP-hosted
// package_url; an older agent refuses it on its host allowlist. Flipping this on
// for a fleet whose agents predate that change would stop their self-update
// instead of starting it, so the switch belongs to the operator, who knows which
// build their sites are on.
//
// AgentPackageBaseURL (WPMGR_UPDATE_AGENT_PACKAGE_BASE_URL) optionally pins the
// public origin used to build that URL. Left empty, the origin is derived from
// the control-plane URL the agent itself used to fetch the manifest, so a
// self-hosted install needs no value here at all.
type UpdateConfig struct {
	PerTenantParallelism     int           `koanf:"per_tenant_parallelism"`
	HTTPTimeout              time.Duration `koanf:"http_timeout"`
	HTTPRetries              int           `koanf:"http_retries"`
	ApplyHTTPTimeout         time.Duration `koanf:"apply_http_timeout"`
	AgentSelfUpdateEnabled   bool          `koanf:"agent_self_update_enabled"`
	AgentMirrorEnabled       bool          `koanf:"agent_mirror_enabled"`
	AgentMirrorOwner         string        `koanf:"agent_mirror_owner"`
	AgentMirrorRepo          string        `koanf:"agent_mirror_repo"`
	AgentMirrorAllowRollback bool          `koanf:"agent_mirror_allow_rollback"`
	AgentPackageServeEnabled bool          `koanf:"agent_package_serve_enabled"`
	AgentPackageBaseURL      string        `koanf:"agent_package_base_url"`
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

	// ProxyHops (WPMGR_AUTH_PROXY_HOPS) is the number of X-Forwarded-For
	// entries the infrastructure in front of this process appends to whatever
	// the caller sent. The authentication rate limiters key on the entry that
	// many positions from the right; everything to its left is caller-supplied
	// and must not decide whether a request is refused.
	//
	// Set it to the number of proxies that append, counting from this process
	// outward:
	//
	//	0  nothing appends — this process terminates connections directly, and
	//	   the peer address IS the client. The forwarded header is ignored
	//	   entirely, which is the only safe reading when nothing is trusted.
	//	1  a single reverse proxy that appends the client address (the bundled
	//	   nginx compose deployment).
	//	2  a cloud load balancer that appends the client address and then its
	//	   own frontend address. This is the default because it is correct for
	//	   the hosted deployment.
	//
	// Getting this wrong is not subtle in one direction and silent in the
	// other: too high and the limiters key on caller-supplied data and stop
	// binding; too low and every client collapses onto one key and legitimate
	// users are refused. Neither is guessable from inside the process, so it is
	// configuration rather than a constant, and the effective value is logged
	// at startup.
	ProxyHops int `koanf:"proxy_hops"`

	// BootstrapClaimSecret (WPMGR_BOOTSTRAP_CLAIM_SECRET) is the provisioning
	// claim the installer mints and hands to the person who is entitled to own
	// the install. First-run ownership — the very first organisation and its
	// owner membership — is granted only to a caller that presents it.
	//
	// It is a LITERAL value only; this file has no file-indirection convention
	// for any other secret and inventing one here would be a second mechanism.
	//
	// UNSET MEANS "NOBODY MAY CLAIM THIS INSTALL", never "anybody may". An
	// install that boots without it can still serve every other route; it
	// simply has no route to first-run ownership until the operator sets the
	// variable and restarts. That direction is deliberate: the failure mode of
	// a misconfigured deployment must be an install that cannot be claimed,
	// not an install that can be claimed freely.
	//
	// It is a credential, so it is never logged, never echoed into a response
	// and never recorded in audit metadata; only its NAME appears in operator
	// guidance.
	BootstrapClaimSecret string `koanf:"bootstrap_claim_secret"`

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

	// PreviousIssuer (WPMGR_OIDC_PREVIOUS_ISSUER) is the issuer this install
	// used BEFORE Issuer, declared by the operator when they repoint SSO at a
	// new IdP hostname. Empty on every install that has never moved, which is
	// almost all of them.
	//
	// IT EXISTS BECAUSE THE ALTERNATIVES ARE BOTH BAD. An identity is
	// (provider, subject, issuer), and subject is unique only within its
	// issuer, so the key cannot drop issuer without letting two IdPs collide
	// onto one account. But with issuer in the key, changing this variable
	// strands every generic-OIDC identity at once: every SSO user on the
	// install stops being recognised on the same deploy.
	//
	// Declaring the old value turns that into a migration the operator asked
	// for. Each identity is moved to the new issuer once, on that person's next
	// sign-in, and the move is audited. It is NEVER used to verify a token:
	// only the current Issuer can do that. It only says "identities stored
	// under this issuer are the same people as the ones arriving from the
	// current one", which is a statement only the operator is in a position to
	// make.
	PreviousIssuer string `koanf:"previous_issuer"`
}

// Enabled reports whether OIDC is configured.
func (o OIDCConfig) Enabled() bool { return o.Issuer != "" }

// SocialConfig holds the consumer identity providers offered on the sign-in
// page. Each is independently optional: configuring neither leaves email and
// password as the only way in, which is the correct default for a self-hosted
// install whose operator has not registered an OAuth application anywhere.
//
// RedirectURL is derived from the public base URL rather than configured, so an
// operator cannot set it to something the provider will reject or, worse, to a
// host they do not control.
type SocialConfig struct {
	Google GoogleConfig `koanf:"google"`
	GitHub GitHubConfig `koanf:"github"`
}

// Configured reports whether the operator has started configuring ANY social
// provider, which is a different question from Enabled.
//
// Enabled asks "will this provider work", and is what decides whether a button
// renders. Configured asks "did somebody intend social sign-in here", and is
// what Validate uses to decide whether a half-entered credential or a missing
// public base URL is a problem worth reporting. Keeping them apart is what
// stops an install with no social configuration at all from being told about
// requirements that do not apply to it.
func (s SocialConfig) Configured() bool {
	return s.Google.Configured() || s.GitHub.Configured()
}

// GoogleConfig is a standard OIDC relying-party registration. Google publishes
// a discovery document and issues ID tokens carrying an email_verified claim,
// so no bespoke handling is needed beyond checking that claim.
type GoogleConfig struct {
	ClientID     string `koanf:"client_id"`
	ClientSecret string `koanf:"client_secret"`
}

// Enabled reports whether Google sign-in will work: both halves of the
// credential are present. A half-entered credential is deliberately NOT enabled
// (no button that fails at the provider), but it is also not silent: Validate
// reports it, because the operator plainly meant to switch this on.
func (g GoogleConfig) Enabled() bool { return g.ClientID != "" && g.ClientSecret != "" }

// Configured reports whether either half of the credential is present.
func (g GoogleConfig) Configured() bool { return g.ClientID != "" || g.ClientSecret != "" }

// GitHubConfig is a plain OAuth 2.0 registration. GitHub is NOT an OpenID
// Connect provider: there is no discovery document, no ID token and no
// email_verified claim, so its adapter has to call the API and derive the
// verified address itself.
type GitHubConfig struct {
	ClientID     string `koanf:"client_id"`
	ClientSecret string `koanf:"client_secret"`
}

// Enabled reports whether GitHub sign-in will work. Same split as Google: see
// GoogleConfig.Enabled and Configured.
func (g GitHubConfig) Enabled() bool { return g.ClientID != "" && g.ClientSecret != "" }

// Configured reports whether either half of the credential is present.
func (g GitHubConfig) Configured() bool { return g.ClientID != "" || g.ClientSecret != "" }

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

// ValidateSessionSecret refuses weak/placeholder/published session secrets. The
// secret keys the session store; an empty, placeholder, short, or publicly
// known value is a security hole, so the server must not boot with one.
func (c Config) ValidateSessionSecret() error {
	s := c.Auth.SessionSecret
	if s == "" {
		return fmt.Errorf("WPMGR_SESSION_SECRET is empty: set a random secret of at least 32 bytes")
	}
	if strings.HasPrefix(s, "change-me") {
		return fmt.Errorf("WPMGR_SESSION_SECRET still holds the placeholder value: set a real random secret of at least 32 bytes")
	}
	// Before the length check, deliberately. A published secret is long and
	// well-formed, so it would otherwise sail past every remaining check; and if
	// a future published value ever were short, "this one is public" is the more
	// useful thing to tell the operator than "this one is short".
	if reason := publishedSessionSecretRefusal(s); reason != "" {
		return fmt.Errorf("WPMGR_SESSION_SECRET %s", reason)
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
		"env":       "development",
		"http_addr": ":8080",
		"log_level": "info",
		// Empty rather than a guessed origin: a wrong public base URL mints
		// links and redirect URIs pointing at somebody else's host, so the only
		// safe default is one Validate can recognise as unset.
		"public_base_url":          "",
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
		"auth.session_secret": "",
		// 2 is correct for the hosted deployment (load balancer appends the
		// client address then its own). Every other topology must set this;
		// see AuthConfig.ProxyHops and the startup log line that names it.
		"auth.proxy_hops":   2,
		"auth.idle_timeout": "168h", // 7 days idle
		"auth.absolute_expiry":     "720h", // 30 days hard cap
		// ADR-056: WebAuthn relying party defaults (hosted instance).
		// Self-hosted operators override via WPMGR_AUTH_WEBAUTHN_RPID etc.
		"auth.webauthn_rpid":            "manage.wpmgr.app",
		"auth.webauthn_rp_origins":      "https://manage.wpmgr.app",
		"auth.webauthn_rp_display_name": "WPMgr",
		"oidc.issuer":                   "",
		"oidc.client_id":                "",
		"oidc.client_secret":            "",
		"oidc.redirect_url":             "",
		"oidc.previous_issuer":          "",
		"otel.exporter_otlp_endpoint":   "",
		"otel.service_name":             "wpmgr-api",
		"shutdown.timeout":              "15s",
		"agent.signing_private_key":     "",
		"agent.signing_public_key":      "",
		"agent.signature_skew":          "5m",
		"agent.stale_after":             "10m", // ~2 missed 5-min heartbeats
		"agent.health_interval":         "5m",
		"update.per_tenant_parallelism": 5,
		"update.http_timeout":           "30s",
		"update.http_retries":           2,
		"update.apply_http_timeout":     "8m",
		// Fleet-wide kill switch for the agent's own upgrade channel. Ships
		// DISABLED: merging the channel changes nothing until an operator
		// explicitly turns it on. See UpdateConfig.AgentSelfUpdateEnabled.
		"update.agent_self_update_enabled": false,
		// GH #302 — mirror the public upstream agent release into THIS install's
		// own object storage. Ships DISABLED: an install that already has a
		// release channel (the hosted service) does not need it, and an install
		// that does need it should opt in. Owner/repo default to the upstream
		// project; a fork points them at its own releases.
		"update.agent_mirror_enabled": false,
		"update.agent_mirror_owner":   "mosamlife",
		"update.agent_mirror_repo":    "wpmgr",
		// The mirror only ever moves the published version FORWARD. This is the
		// deliberate escape hatch for a genuine upstream rollback (a yanked
		// release), and it is off unless an operator asks for it.
		"update.agent_mirror_allow_rollback": false,
		// GH #302: serve that mirrored package from the control plane itself, so
		// no site needs a package-host override in wp-config.php. Ships DISABLED
		// for rollout order: only agents new enough to trust their own
		// control-plane host accept a CP-hosted package_url. See
		// UpdateConfig.AgentPackageServeEnabled. An empty base URL means "derive
		// the origin from the request the agent made".
		"update.agent_package_serve_enabled": false,
		"update.agent_package_base_url":      "",
		"s3.endpoint":                        "",
		"s3.region":                          "us-east-1",
		"s3.bucket":                          "",
		"s3.access_key":                      "",
		"s3.secret_key":                      "",
		"s3.force_path_style":                true,
		"backup.presign_ttl":                 "1h",
		"backup.retention_days":              30,
		"backup.monthly_archive_keep":        12,
		"backup.schedule_interval":           "5m",
		"backup.gc_interval":                 "1h",
		"backup.http_timeout":                "10m",
		"backup.stall_soft_timeout":          "3m",
		"backup.stall_hard_timeout":          "30m",
		"clickhouse.addr":                    "",
		"clickhouse.db":                      "wpmgr_metrics",
		"clickhouse.username":                "default",
		"clickhouse.password":                "",
		"smtp.host":                          "",
		"smtp.port":                          587,
		"smtp.username":                      "",
		"smtp.password":                      "",
		"smtp.from":                          "",
		"smtp.tls_mode":                      "starttls",
		"uptime.probe_interval":              "60s",
		"uptime.probe_timeout":               "15s",
		"uptime.probe_concurrency":           10,
		"uptime.alert_interval":              "60s",
		"uptime.down_threshold":              2,
		"uptime.cron_kick_enabled":           true,
		"uptime.cron_kick_interval":          "5m",
		"uptime.cron_kick_timeout":           "5s",
		"uptime.cron_kick_concurrency":       10,
		"uptime.app_probe_enabled":           true,
		"uptime.app_probe_interval":          "300s",
		"uptime.app_probe_timeout":           "10s",
		"uptime.app_alert_threshold":         5,
		"uptime.app_alert_breaker_ratio":     0.25,
		"uptime.max_fleet_size":              2000,
		"river.media_schema":                 "media_encoder",
		"autologin.require_2fa_step_up":      false,
		"conn.degrade_after":                 "300s",
		"conn.degrade_miss_threshold":        3,
		"conn.disconnect_after":              "900s",
		"conn.active_verify":                 true,
		"conn.verify_timeout":                "8s",
		"conn.verify_concurrency":            8,
		"hosted.enabled":                     false,
		"billing.stripe.secret_key":          "",
		"billing.stripe.webhook_secret":      "",
		"billing.stripe.price_starter":       "",
		"billing.stripe.price_agency":        "",
		"billing.stripe.price_scale":         "",
		"billing.razorpay.key_id":            "",
		"billing.razorpay.key_secret":        "",
		"billing.razorpay.webhook_secret":    "",
		"billing.razorpay.plan_starter_usd":  "",
		"billing.razorpay.plan_starter_inr":  "",
		"billing.razorpay.plan_agency_usd":   "",
		"billing.razorpay.plan_agency_inr":   "",
		"billing.razorpay.plan_scale_usd":    "",
		"billing.razorpay.plan_scale_inr":    "",
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

	// rawProxyHops holds the unconverted WPMGR_AUTH_PROXY_HOPS string when the
	// variable is present at all, including when it is present and empty.
	var rawProxyHops *string
	// Env: WPMGR_DB_HOST -> db.host, WPMGR_HTTP_ADDR -> http_addr, etc.
	// We strip the WPMGR_ prefix, lowercase, then map the documented
	// double-underscore-free names by replacing the first underscore segment.
	envProvider := env.ProviderWithValue("WPMGR_", ".", func(key, value string) (string, any) {
		k := strings.ToLower(strings.TrimPrefix(key, "WPMGR_"))
		k = mapEnvKey(k)
		// Capture the RAW string for the proxy hop count. Everything below this
		// point is typed, and the typing is what loses the distinction that
		// matters: mapstructure's weak input rewrites "" to 0, so an unset outer
		// variable in WPMGR_AUTH_PROXY_HOPS=${SOMETHING}, or an empty ConfigMap
		// entry, would arrive as a deliberate 0 and be indistinguishable from
		// one. See parseProxyHops.
		if k == "auth.proxy_hops" {
			v := value
			rawProxyHops = &v
		}
		return k, value
	})
	if err := k.Load(envProvider, nil); err != nil {
		return Config{}, fmt.Errorf("load env: %w", err)
	}

	// Before unmarshal, so the operator gets a message naming the variable and
	// saying what is wrong with it. Left until after, the generic decode failure
	// ("decoding failed due to the following error(s)") is all they would see,
	// and the one spelling that does NOT fail to decode — the empty string — is
	// the dangerous one.
	proxyHops := -1
	if rawProxyHops != nil {
		n, err := parseProxyHops(*rawProxyHops)
		if err != nil {
			return Config{}, fmt.Errorf("WPMGR_AUTH_PROXY_HOPS: %w", err)
		}
		proxyHops = n
	} else {
		// The env var is not the only source. A YAML file supplies the same key,
		// and reaches the same weak decode: a quoted "010" becomes 8, a bare
		// `proxy_hops:` becomes 0, `true` becomes 1. The null shapes are what a
		// templating tool renders for a value it could not resolve, which is the
		// same accident as an unset ${VAR} and needs the same answer.
		n, err := proxyHopsFromValue(k.Get("auth.proxy_hops"))
		if err != nil {
			source := "auth.proxy_hops"
			if path != "" {
				source = fmt.Sprintf("auth.proxy_hops in %s", path)
			}
			return Config{}, fmt.Errorf("%s: %w", source, err)
		}
		proxyHops = n
	}

	var cfg Config
	if err := k.Unmarshal("", &cfg); err != nil {
		return Config{}, fmt.Errorf("unmarshal config: %w", err)
	}
	if proxyHops >= 0 {
		cfg.Auth.ProxyHops = proxyHops
	}
	// Normalize ONCE, here, so every consumer and every check sees the same
	// string. Consumers append paths to this value; a check that trimmed and a
	// consumer that did not would disagree about what was configured, and the
	// check would then approve a value nothing uses.
	cfg.PublicBaseURL = NormalizePublicBaseURL(cfg.PublicBaseURL)
	return cfg, nil
}

// parseProxyHops converts WPMGR_AUTH_PROXY_HOPS, accepting only a plain decimal
// integer: no empty string, no sign, no surrounding whitespace, no 0x form and
// no leading zero.
//
// It is this strict because every rejected spelling has a plausible reading that
// is not what the operator meant, and the cost of guessing wrong is not a
// startup warning. The hop count decides which forwarded entry the auth rate
// limiters key on: too low and every caller in the fleet shares one bucket and
// legitimate sign-ins are refused; too high and the key comes from
// caller-supplied data and the limits stop binding. Neither is visible from
// inside the process.
//
// The empty string is the case worth naming. It is the ordinary output of
// WPMGR_AUTH_PROXY_HOPS=${SOMETHING} with SOMETHING unset, and of an empty
// value in a ConfigMap or .env file. Weak typing turns it into 0, which is a
// meaningful and very different setting — "nothing is in front of this process"
// — so accepting it would silently reconfigure a load-balanced deployment onto
// a single shared limiter key. Absence and emptiness are not the same thing:
// remove the variable to take the default.
// proxyHopsFromValue converts the hop count as it sits in koanf after the
// defaults and any config file have loaded, before the typed decode.
//
// It exists because the decode is weakly typed and will turn a null, a bool or
// a quoted numeral into a plausible-looking count. Every conversion it would
// perform silently is refused here instead, for the same reason the environment
// string is parsed strictly: the value decides which forwarded entry the auth
// rate limiters key on, and both directions of being wrong are invisible from
// inside the process.
//
// A quoted numeral is parsed rather than rejected outright — "2" is unambiguous
// — but through the same strict parser as the environment path, so the octal,
// hex and empty spellings are refused identically wherever they are written.
func proxyHopsFromValue(v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case string:
		return parseProxyHops(n)
	case nil:
		return 0, fmt.Errorf("empty. An empty value is not 0 — 0 means nothing in front of this process appends to X-Forwarded-For. Remove the key to take the default, or set an explicit number")
	case bool:
		return 0, fmt.Errorf("%v is a boolean; this is a count of proxies in front of this process, so write a bare number", n)
	case float64:
		return 0, fmt.Errorf("%v is not a whole number; this is a count of proxies, so write a bare integer", n)
	default:
		return 0, fmt.Errorf("%v (%T) is not a whole number; this is a count of proxies, so write a bare integer", v, v)
	}
}

func parseProxyHops(raw string) (int, error) {
	if raw == "" {
		return 0, fmt.Errorf("present but empty. An empty value is not 0 — 0 means nothing in front of this process appends to X-Forwarded-For. Remove the variable to take the default, or set an explicit number")
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, fmt.Errorf("%q is not a plain decimal integer: no signs, spaces, decimal points or 0x forms", raw)
		}
	}
	if len(raw) > 1 && raw[0] == '0' {
		return 0, fmt.Errorf("%q has a leading zero, which reads as octal in some tooling. Write it as a plain decimal number", raw)
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%q is not a valid number: %w", raw, err)
	}
	return n, nil
}

// NormalizePublicBaseURL is the canonical form of WPMGR_PUBLIC_BASE_URL: no
// surrounding whitespace (a stray space or newline survives .env parsing and
// docker-compose interpolation) and no trailing slash (every consumer appends
// an absolute path, so a trailing slash yields a doubled separator).
func NormalizePublicBaseURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
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
	// WPMGR_BOOTSTRAP_CLAIM_SECRET -> auth.bootstrap_claim_secret. An explicit
	// case, not the "auth_" prefix, because the operator-facing name has no
	// AUTH_ segment. Without this line the key falls through the default
	// passthrough below and unmarshal silently ignores it — the exact shape
	// that left social sign-in configured-but-disabled with no error to notice,
	// and here it would leave a correctly-configured install unable to be
	// claimed by anyone.
	case k == "bootstrap_claim_secret":
		return "auth.bootstrap_claim_secret"
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
	// Social sign-in, one case PER PROVIDER for the same reason billing needs
	// one per gateway: the struct nests two levels, so the rewrite has to
	// produce social.google.client_id. A single "social_" case would yield
	// social.google_client_id, which is still flat and still does not bind.
	//
	// Their absence is why this feature shipped, deployed and could not be
	// switched on: all four variables set correctly still left both providers
	// disabled, because the keys fell through the default passthrough below and
	// unmarshal ignored them. There was no error to notice.
	case strings.HasPrefix(k, "social_google_"):
		return "social.google." + strings.TrimPrefix(k, "social_google_")
	case strings.HasPrefix(k, "social_github_"):
		return "social.github." + strings.TrimPrefix(k, "social_github_")
	default:
		return k
	}
}
