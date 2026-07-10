package config

import (
	"strings"
	"testing"
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
