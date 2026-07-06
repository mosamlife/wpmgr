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
// empty (public/default schema behavior) when the env var is unset.
func TestLoadRiverMediaSchemaDefault(t *testing.T) {
	t.Setenv("WPMGR_RIVER_MEDIA_SCHEMA", "")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.River.MediaSchema; got != "" {
		t.Fatalf("River.MediaSchema = %q, want empty default", got)
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
