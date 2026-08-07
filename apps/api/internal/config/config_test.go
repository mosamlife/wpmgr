package config

import (
	"strings"
	"testing"
	"time"
)

// TestValidateSessionSecret checks that empty, placeholder, and short session
// secrets are rejected while adequate-length secrets are accepted.
func TestValidateSessionSecret(t *testing.T) {
	tests := []struct {
		name    string
		secret  string
		wantErr bool
	}{
		{"empty", "", true},
		{"placeholder", "change-me-32-bytes-base64", true},
		{"too short", "short", true},
		{"exactly 32 bytes", strings.Repeat("a", 32), false},
		{"long random", strings.Repeat("z", 64), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{Auth: AuthConfig{SessionSecret: tt.secret}}
			err := c.ValidateSessionSecret()
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateSessionSecret() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestValidateAgentSigningKey verifies production rejects committed dev keys
// while permitting fresh keys or empty keys, and that development allows dev keys.
func TestValidateAgentSigningKey(t *testing.T) {
	devKey := devAgentSigningPrivateKeys[0]
	freshKey := "ZZZZ1W3DSfBwuE/V/H9BEmV9IAJfK5d6F2RDfYSj/raBW+b26qHT3spd1gHSw7aXEXxZkg9E9WMspibSjSFsnQ=="
	tests := []struct {
		name    string
		env     string
		key     string
		wantErr bool
	}{
		{"production with dev key rejected", "production", devKey, true},
		{"production with fresh key ok", "production", freshKey, false},
		{"production with empty key ok", "production", "", false},
		{"development with dev key ok", "development", devKey, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{Env: tt.env, Agent: AgentConfig{SigningPrivateKey: tt.key}}
			err := c.ValidateAgentSigningKey()
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateAgentSigningKey() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestMigrateDSNFallback verifies the migration DSN falls back to the app DSN
// when no separate migration DSN is configured.
func TestMigrateDSNFallback(t *testing.T) {
	d := DBConfig{Host: "h", Port: 5432, User: "u", Password: "p", Name: "n", SSLMode: "disable"}
	if got := d.MigrateDSN(); got != d.DSN() {
		t.Fatalf("MigrateDSN should fall back to DSN when MigrationDSN unset: %q", got)
	}
	d.MigrationDSN = "postgres://owner@host/db"
	if got := d.MigrateDSN(); got != "postgres://owner@host/db" {
		t.Fatalf("MigrateDSN should use MigrationDSN when set: %q", got)
	}
}

// TestLoadUpdateApplyHTTPTimeoutDefault is the GH #208 Bug 2 regression lock:
// with no WPMGR_UPDATE_APPLY_HTTP_TIMEOUT env configured, Update.ApplyHTTPTimeout
// must default to a value LONGER than the shared 30s Update.HTTPTimeout (the
// whole point of the dedicated knob) but no longer than the 10m Backup.HTTPTimeout
// (an update apply + its mandatory pre-update snapshot is lighter than a
// full-site backup).
func TestLoadUpdateApplyHTTPTimeoutDefault(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Update.ApplyHTTPTimeout; got <= cfg.Update.HTTPTimeout {
		t.Fatalf("Update.ApplyHTTPTimeout = %v, want > Update.HTTPTimeout (%v) — otherwise the dedicated knob does nothing", got, cfg.Update.HTTPTimeout)
	}
	if got := cfg.Update.ApplyHTTPTimeout; got > cfg.Backup.HTTPTimeout {
		t.Fatalf("Update.ApplyHTTPTimeout = %v, want <= Backup.HTTPTimeout (%v) — an update apply is lighter than a full-site backup", got, cfg.Backup.HTTPTimeout)
	}
	if got := cfg.Update.ApplyHTTPTimeout; got != 8*time.Minute {
		t.Fatalf("Update.ApplyHTTPTimeout = %v, want 8m default", got)
	}
}

// TestLoadUpdateApplyHTTPTimeoutEnv verifies WPMGR_UPDATE_APPLY_HTTP_TIMEOUT is
// loaded from the environment when set.
func TestLoadUpdateApplyHTTPTimeoutEnv(t *testing.T) {
	t.Setenv("WPMGR_UPDATE_APPLY_HTTP_TIMEOUT", "3m")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Update.ApplyHTTPTimeout; got != 3*time.Minute {
		t.Fatalf("Update.ApplyHTTPTimeout = %v, want 3m", got)
	}
}

// TestLoadAgentMirrorDefaults is the GH #302 off-by-default lock. The upstream
// agent-release mirror is the one job that fetches from the public internet and
// writes a binary into the operator's own storage, so merging it must change
// nothing until an operator explicitly opts in. Owner/repo default to the
// upstream project so an operator who opts in has nothing else to configure.
func TestLoadAgentMirrorDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Update.AgentMirrorEnabled {
		t.Fatal("Update.AgentMirrorEnabled = true by default; the upstream mirror must ship OFF")
	}
	if got := cfg.Update.AgentMirrorOwner; got != "mosamlife" {
		t.Fatalf("Update.AgentMirrorOwner = %q, want the upstream owner", got)
	}
	if got := cfg.Update.AgentMirrorRepo; got != "wpmgr" {
		t.Fatalf("Update.AgentMirrorRepo = %q, want the upstream repo", got)
	}
	// The mirror only ever moves this install's published agent version FORWARD.
	// Rollback is the deliberate escape hatch for a genuine upstream yank, and it
	// must be something an operator asks for: left on by default it is what would
	// let a yanked-then-restored upstream flap the published version.
	if cfg.Update.AgentMirrorAllowRollback {
		t.Fatal("Update.AgentMirrorAllowRollback = true by default; the mirror must refuse an older upstream unless asked")
	}
}

// TestLoadAgentMirrorEnv verifies the three mirror knobs are readable from the
// environment, including pointing a fork at its own releases.
func TestLoadAgentMirrorEnv(t *testing.T) {
	t.Setenv("WPMGR_UPDATE_AGENT_MIRROR_ENABLED", "true")
	t.Setenv("WPMGR_UPDATE_AGENT_MIRROR_OWNER", "some-fork")
	t.Setenv("WPMGR_UPDATE_AGENT_MIRROR_REPO", "their-wpmgr")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Update.AgentMirrorEnabled {
		t.Fatal("Update.AgentMirrorEnabled = false, want true from the environment")
	}
	if got := cfg.Update.AgentMirrorOwner; got != "some-fork" {
		t.Fatalf("Update.AgentMirrorOwner = %q, want some-fork", got)
	}
	if got := cfg.Update.AgentMirrorRepo; got != "their-wpmgr" {
		t.Fatalf("Update.AgentMirrorRepo = %q, want their-wpmgr", got)
	}
}

// TestLoadAgentMirrorAllowRollbackEnv proves the escape hatch is reachable: an
// operator following a genuine upstream rollback has one switch to set, so the
// strictly-newer rule is deliberate rather than unrecoverable.
func TestLoadAgentMirrorAllowRollbackEnv(t *testing.T) {
	t.Setenv("WPMGR_UPDATE_AGENT_MIRROR_ALLOW_ROLLBACK", "true")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Update.AgentMirrorAllowRollback {
		t.Fatal("Update.AgentMirrorAllowRollback = false, want true from the environment")
	}
}

// TestLoadBackupStallDefaults verifies the GH #279 two-tier progress watchdog
// defaults: a 3m soft threshold and a 30m hard threshold, with hard well
// above soft (the whole point of a two-tier policy).
func TestLoadBackupStallDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Backup.StallSoftTimeout; got != 3*time.Minute {
		t.Fatalf("Backup.StallSoftTimeout = %v, want 3m default", got)
	}
	if got := cfg.Backup.StallHardTimeout; got != 30*time.Minute {
		t.Fatalf("Backup.StallHardTimeout = %v, want 30m default", got)
	}
	if cfg.Backup.StallHardTimeout <= cfg.Backup.StallSoftTimeout {
		t.Fatalf("Backup.StallHardTimeout (%v) must be > StallSoftTimeout (%v)", cfg.Backup.StallHardTimeout, cfg.Backup.StallSoftTimeout)
	}
}

// TestLoadBackupStallTimeoutsEnv verifies WPMGR_BACKUP_STALL_SOFT_TIMEOUT and
// WPMGR_BACKUP_STALL_HARD_TIMEOUT are loaded from the environment when set.
func TestLoadBackupStallTimeoutsEnv(t *testing.T) {
	t.Setenv("WPMGR_BACKUP_STALL_SOFT_TIMEOUT", "90s")
	t.Setenv("WPMGR_BACKUP_STALL_HARD_TIMEOUT", "10m")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Backup.StallSoftTimeout; got != 90*time.Second {
		t.Fatalf("Backup.StallSoftTimeout = %v, want 90s", got)
	}
	if got := cfg.Backup.StallHardTimeout; got != 10*time.Minute {
		t.Fatalf("Backup.StallHardTimeout = %v, want 10m", got)
	}
}

// TestLoadRiverMediaSchemaDefault verifies the media River schema defaults to
// a dedicated "media_encoder" schema when the env var is unset (GH #205: a
// shared default schema lets the media-encoder silently steal River
// leadership and stop the API's fleet periodics, so isolation is now the
// binary default rather than opt-in).
func TestLoadRiverMediaSchemaDefault(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.River.MediaSchema; got != "media_encoder" {
		t.Fatalf("River.MediaSchema = %q, want media_encoder default", got)
	}
}

// TestLoadRiverMediaSchemaExplicitEmpty verifies an operator can still
// explicitly opt back into the legacy shared/public schema by setting the
// env var to an empty string.
func TestLoadRiverMediaSchemaExplicitEmpty(t *testing.T) {
	t.Setenv("WPMGR_RIVER_MEDIA_SCHEMA", "")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.River.MediaSchema; got != "" {
		t.Fatalf("River.MediaSchema = %q, want empty when explicitly set", got)
	}
}

// TestLoadRiverMediaSchemaEnv verifies WPMGR_RIVER_MEDIA_SCHEMA is loaded from
// the environment when set.
func TestLoadRiverMediaSchemaEnv(t *testing.T) {
	t.Setenv("WPMGR_RIVER_MEDIA_SCHEMA", "media_encoder")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.River.MediaSchema; got != "media_encoder" {
		t.Fatalf("River.MediaSchema = %q, want media_encoder", got)
	}
}

// TestValidateRiverMediaSchema verifies that an invalid WPMGR_RIVER_MEDIA_SCHEMA
// surfaces as a config Issue (so the server parks in readyz-degraded) while
// empty, public, and valid identifiers are accepted.
func TestValidateRiverMediaSchema(t *testing.T) {
	base := Config{Auth: AuthConfig{SessionSecret: strings.Repeat("a", 32)}}
	tests := []struct {
		name      string
		schema    string
		wantIssue bool
	}{
		{"empty default", "", false},
		{"public", "public", false},
		{"valid identifier", "media_encoder", false},
		{"hyphen rejected", "media-encoder", true},
		{"dotted rejected", "public.river", true},
		{"leading digit rejected", "1schema", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			cfg.River = RiverConfig{MediaSchema: tt.schema}
			gotIssue := false
			for _, is := range Validate(cfg) {
				if is.Name == "WPMGR_RIVER_MEDIA_SCHEMA" {
					gotIssue = true
				}
			}
			if gotIssue != tt.wantIssue {
				t.Fatalf("Validate() river schema issue = %v, want %v", gotIssue, tt.wantIssue)
			}
		})
	}
}

// TestOIDCEnabled verifies the OIDC provider is enabled only when an issuer
// URL is configured.
func TestOIDCEnabled(t *testing.T) {
	if (OIDCConfig{}).Enabled() {
		t.Fatal("empty issuer should be disabled")
	}
	if !(OIDCConfig{Issuer: "https://issuer"}).Enabled() {
		t.Fatal("set issuer should be enabled")
	}
}

// TestLoadHostedDefaultDisabled is the #131-class boot-regression guard for
// M16 Phase A: with zero WPMGR_HOSTED* env configured (the state of every
// self-host and current-prod deployment today), Load must succeed and
// Hosted.Enabled must default to false, and Validate must report zero
// issues — turning on the entitlement substrate must never require any new
// env var to boot.
func TestLoadHostedDefaultDisabled(t *testing.T) {
	t.Setenv("WPMGR_HOSTED", "")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Hosted.Enabled {
		t.Fatal("Hosted.Enabled should default to false")
	}
	cfg.Auth.SessionSecret = strings.Repeat("a", 32) // satisfy the unrelated session-secret check
	if issues := Validate(cfg); len(issues) != 0 {
		t.Fatalf("Validate() with hosted billing unconfigured returned issues: %+v", issues)
	}
}

// TestLoadHostedEnvOverride verifies WPMGR_HOSTED=true is loaded into
// Hosted.Enabled.
func TestLoadHostedEnvOverride(t *testing.T) {
	t.Setenv("WPMGR_HOSTED", "true")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Hosted.Enabled {
		t.Fatal("Hosted.Enabled should be true when WPMGR_HOSTED=true")
	}
}

// TestValidateHostedWithNoBillingProviderIsLegal extends the Phase A boot-
// green guard to M16 Phase B: WPMGR_HOSTED=true with ZERO
// WPMGR_BILLING_STRIPE_* variables set must still boot green — "hosted with
// no payment provider configured" degrades checkout/portal to a clean 503,
// it is never a boot-time config error.
func TestValidateHostedWithNoBillingProviderIsLegal(t *testing.T) {
	cfg := Config{Auth: AuthConfig{SessionSecret: strings.Repeat("a", 32)}}
	cfg.Hosted.Enabled = true
	if issues := Validate(cfg); len(issues) != 0 {
		t.Fatalf("Validate() with hosted=true and no Stripe config returned issues: %+v", issues)
	}
}

// TestValidateHostedWithFullStripeConfigIsLegal proves a completely-filled-in
// Stripe config passes cleanly.
func TestValidateHostedWithFullStripeConfigIsLegal(t *testing.T) {
	cfg := Config{Auth: AuthConfig{SessionSecret: strings.Repeat("a", 32)}}
	cfg.Hosted.Enabled = true
	cfg.Billing.Stripe = StripeConfig{
		SecretKey: "sk_live_x", WebhookSecret: "whsec_x",
		PriceStarter: "price_1", PriceAgency: "price_2", PriceScale: "price_3",
	}
	if issues := Validate(cfg); len(issues) != 0 {
		t.Fatalf("Validate() with a fully-configured Stripe returned issues: %+v", issues)
	}
}

// TestValidateHostedWithPartialStripeConfigIsRejected is the core Phase B
// guard: an operator who has started configuring Stripe (ANY one field set)
// but left the rest blank must be refused at boot — a half-wired provider
// would otherwise register cleanly and fail confusingly on first
// checkout/webhook instead of at boot, where the problem is obvious.
func TestValidateHostedWithPartialStripeConfigIsRejected(t *testing.T) {
	cfg := Config{Auth: AuthConfig{SessionSecret: strings.Repeat("a", 32)}}
	cfg.Hosted.Enabled = true
	cfg.Billing.Stripe = StripeConfig{SecretKey: "sk_live_x"} // only one of five fields set

	issues := Validate(cfg)
	if len(issues) != 4 {
		t.Fatalf("Validate() with a partial Stripe config returned %d issues, want 4 (the four unset fields): %+v", len(issues), issues)
	}
	for _, is := range issues {
		if is.Name == "WPMGR_BILLING_STRIPE_SECRET_KEY" {
			t.Fatalf("the ONE field that IS set should not itself be reported as an issue: %+v", issues)
		}
	}
}

// TestValidateStripeConfig_NotHostedNeverValidated proves the Stripe checks
// are skipped entirely when hosted billing is off, even with a partial
// config present — an unhosted deployment's environment is none of this
// package's business until WPMGR_HOSTED is turned on.
func TestValidateStripeConfig_NotHostedNeverValidated(t *testing.T) {
	cfg := Config{Auth: AuthConfig{SessionSecret: strings.Repeat("a", 32)}}
	cfg.Hosted.Enabled = false
	cfg.Billing.Stripe = StripeConfig{SecretKey: "sk_live_x"} // partial, but hosted is off

	if issues := Validate(cfg); len(issues) != 0 {
		t.Fatalf("Validate() should skip Stripe checks entirely when Hosted.Enabled is false, got: %+v", issues)
	}
}

// TestValidateHostedWithFullRazorpayConfigIsLegal proves a completely-filled-
// in Razorpay config (all 3 credentials + all 6 dual-currency plan ids)
// passes cleanly.
func TestValidateHostedWithFullRazorpayConfigIsLegal(t *testing.T) {
	cfg := Config{Auth: AuthConfig{SessionSecret: strings.Repeat("a", 32)}}
	cfg.Hosted.Enabled = true
	cfg.Billing.Razorpay = RazorpayConfig{
		KeyID: "rzp_live_x", KeySecret: "secret_x", WebhookSecret: "whsec_x",
		PlanStarterUSD: "plan_su", PlanStarterINR: "plan_si",
		PlanAgencyUSD: "plan_au", PlanAgencyINR: "plan_ai",
		PlanScaleUSD: "plan_scu", PlanScaleINR: "plan_sci",
	}
	if issues := Validate(cfg); len(issues) != 0 {
		t.Fatalf("Validate() with a fully-configured Razorpay returned issues: %+v", issues)
	}
}

// TestValidateHostedWithPartialRazorpayConfigIsRejected mirrors the Stripe
// partial-config guard: an operator who has started configuring Razorpay
// (ANY one of the nine fields set) but left the rest blank must be refused
// at boot, one Issue per unset field.
func TestValidateHostedWithPartialRazorpayConfigIsRejected(t *testing.T) {
	cfg := Config{Auth: AuthConfig{SessionSecret: strings.Repeat("a", 32)}}
	cfg.Hosted.Enabled = true
	cfg.Billing.Razorpay = RazorpayConfig{KeyID: "rzp_live_x"} // only one of nine fields set

	issues := Validate(cfg)
	if len(issues) != 8 {
		t.Fatalf("Validate() with a partial Razorpay config returned %d issues, want 8 (the eight unset fields): %+v", len(issues), issues)
	}
	for _, is := range issues {
		if is.Name == "WPMGR_BILLING_RAZORPAY_KEY_ID" {
			t.Fatalf("the ONE field that IS set should not itself be reported as an issue: %+v", issues)
		}
	}
}

// TestValidateRazorpayConfig_NotHostedNeverValidated proves the Razorpay
// checks are skipped entirely when hosted billing is off, even with a
// partial config present.
func TestValidateRazorpayConfig_NotHostedNeverValidated(t *testing.T) {
	cfg := Config{Auth: AuthConfig{SessionSecret: strings.Repeat("a", 32)}}
	cfg.Hosted.Enabled = false
	cfg.Billing.Razorpay = RazorpayConfig{KeyID: "rzp_live_x"} // partial, but hosted is off

	if issues := Validate(cfg); len(issues) != 0 {
		t.Fatalf("Validate() should skip Razorpay checks entirely when Hosted.Enabled is false, got: %+v", issues)
	}
}

// TestLoadRazorpayEnvMapping proves every WPMGR_BILLING_RAZORPAY_* variable
// resolves through mapEnvKey to billing.razorpay.* — without that mapping
// case, koanf's env provider silently drops every one of these vars (they'd
// fall through to the default passthrough key, which Unmarshal simply
// ignores), and Razorpay would appear permanently unconfigured no matter what
// an operator sets.
func TestLoadRazorpayEnvMapping(t *testing.T) {
	t.Setenv("WPMGR_BILLING_RAZORPAY_KEY_ID", "rzp_live_x")
	t.Setenv("WPMGR_BILLING_RAZORPAY_KEY_SECRET", "secret_x")
	t.Setenv("WPMGR_BILLING_RAZORPAY_WEBHOOK_SECRET", "whsec_x")
	t.Setenv("WPMGR_BILLING_RAZORPAY_PLAN_STARTER_USD", "plan_su")
	t.Setenv("WPMGR_BILLING_RAZORPAY_PLAN_STARTER_INR", "plan_si")
	t.Setenv("WPMGR_BILLING_RAZORPAY_PLAN_AGENCY_USD", "plan_au")
	t.Setenv("WPMGR_BILLING_RAZORPAY_PLAN_AGENCY_INR", "plan_ai")
	t.Setenv("WPMGR_BILLING_RAZORPAY_PLAN_SCALE_USD", "plan_scu")
	t.Setenv("WPMGR_BILLING_RAZORPAY_PLAN_SCALE_INR", "plan_sci")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := RazorpayConfig{
		KeyID: "rzp_live_x", KeySecret: "secret_x", WebhookSecret: "whsec_x",
		PlanStarterUSD: "plan_su", PlanStarterINR: "plan_si",
		PlanAgencyUSD: "plan_au", PlanAgencyINR: "plan_ai",
		PlanScaleUSD: "plan_scu", PlanScaleINR: "plan_sci",
	}
	if cfg.Billing.Razorpay != want {
		t.Fatalf("Billing.Razorpay = %+v, want %+v", cfg.Billing.Razorpay, want)
	}
}

// TestPrivilegeProbeGate verifies that the two-DSN gate logic (MigrationDSN != "")
// correctly identifies when the privilege probe should run. In single-DSN mode
// (MigrationDSN empty) the app connects as the migration runner, so the probe is
// skipped. In two-DSN mode the app role is distinct from the migration runner and
// must hold wpmgr_app privileges — the probe must run.
func TestPrivilegeProbeGate(t *testing.T) {
	tests := []struct {
		name         string
		migrationDSN string
		wantProbe    bool
	}{
		{
			name:         "single-DSN mode: MigrationDSN empty, probe skipped",
			migrationDSN: "",
			wantProbe:    false,
		},
		{
			name:         "two-DSN mode: MigrationDSN set, probe runs",
			migrationDSN: "postgres://owner:secret@localhost/wpmgr",
			wantProbe:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := DBConfig{MigrationDSN: tt.migrationDSN}
			got := d.MigrationDSN != ""
			if got != tt.wantProbe {
				t.Fatalf("probe gate: MigrationDSN=%q → want probe=%v, got %v",
					tt.migrationDSN, tt.wantProbe, got)
			}
		})
	}
}

// TestLoadSocialProvidersFromEnv is the regression for the defect that made
// social sign-in impossible to enable at all.
//
// mapEnvKey rewrites a flat WPMGR_FOO_BAR name into the dotted koanf path that
// matches the struct. Every nested config block needs its own prefix case;
// there was none for social_, so WPMGR_SOCIAL_GOOGLE_CLIENT_ID lowercased to
// social_google_client_id, hit the default passthrough, and was loaded as a
// flat top-level key that unmarshal ignores. The result was a feature that
// shipped, deployed, and could never be switched on: setting all four variables
// correctly still left both providers disabled, with no error anywhere.
//
// A bare "social_" case would NOT fix it: that yields social.google_client_id,
// which is still not nested and still does not bind.
func TestLoadSocialProvidersFromEnv(t *testing.T) {
	t.Setenv("WPMGR_SOCIAL_GOOGLE_CLIENT_ID", "123.apps.googleusercontent.com")
	t.Setenv("WPMGR_SOCIAL_GOOGLE_CLIENT_SECRET", "GOCSPX-google-secret")
	t.Setenv("WPMGR_SOCIAL_GITHUB_CLIENT_ID", "Iv23liGitHubClient")
	t.Setenv("WPMGR_SOCIAL_GITHUB_CLIENT_SECRET", "github-secret")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := cfg.Social.Google.ClientID; got != "123.apps.googleusercontent.com" {
		t.Errorf("Social.Google.ClientID = %q, want the value from the environment", got)
	}
	if got := cfg.Social.Google.ClientSecret; got != "GOCSPX-google-secret" {
		t.Errorf("Social.Google.ClientSecret = %q, want the value from the environment", got)
	}
	if got := cfg.Social.GitHub.ClientID; got != "Iv23liGitHubClient" {
		t.Errorf("Social.GitHub.ClientID = %q, want the value from the environment", got)
	}
	if got := cfg.Social.GitHub.ClientSecret; got != "github-secret" {
		t.Errorf("Social.GitHub.ClientSecret = %q, want the value from the environment", got)
	}

	// The property that actually matters: with credentials present, the
	// provider must report itself enabled, because that is what decides whether
	// a button ever renders.
	if !cfg.Social.Google.Enabled() {
		t.Error("Social.Google.Enabled() = false with both credentials set; the provider can never be turned on")
	}
	if !cfg.Social.GitHub.Enabled() {
		t.Error("Social.GitHub.Enabled() = false with both credentials set; the provider can never be turned on")
	}
}

// Half-configured must stay off rather than half-on.
func TestLoadSocialProviderNeedsBothCredentials(t *testing.T) {
	t.Setenv("WPMGR_SOCIAL_GOOGLE_CLIENT_ID", "123.apps.googleusercontent.com")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Social.Google.Enabled() {
		t.Error("Google reported enabled with an id but no secret; a half-configured provider must stay off")
	}
}
