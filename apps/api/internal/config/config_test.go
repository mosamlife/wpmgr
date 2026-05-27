package config

import (
	"strings"
	"testing"
)

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

func TestOIDCEnabled(t *testing.T) {
	if (OIDCConfig{}).Enabled() {
		t.Fatal("empty issuer should be disabled")
	}
	if !(OIDCConfig{Issuer: "https://issuer"}).Enabled() {
		t.Fatal("set issuer should be enabled")
	}
}
