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
	Env      string         `koanf:"env"`
	HTTPAddr string         `koanf:"http_addr"`
	LogLevel string         `koanf:"log_level"`
	DB       DBConfig       `koanf:"db"`
	Redis    RedisConfig    `koanf:"redis"`
	Auth     AuthConfig     `koanf:"auth"`
	OIDC     OIDCConfig     `koanf:"oidc"`
	OTel     OTelConfig     `koanf:"otel"`
	Shutdown ShutdownConfig `koanf:"shutdown"`
	Agent    AgentConfig    `koanf:"agent"`
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
		"env":                         "development",
		"http_addr":                   ":8080",
		"log_level":                   "info",
		"db.host":                     "localhost",
		"db.port":                     5432,
		"db.user":                     "wpmgr",
		"db.password":                 "wpmgr",
		"db.name":                     "wpmgr",
		"db.sslmode":                  "disable",
		"db.migration_dsn":            "",
		"db.allow_rls_bypass_role":    false,
		"redis.addr":                  "localhost:6379",
		"redis.password":              "",
		"auth.session_secret":         "",
		"auth.idle_timeout":           "168h", // 7 days idle
		"auth.absolute_expiry":        "720h", // 30 days hard cap
		"oidc.issuer":                 "",
		"oidc.client_id":              "",
		"oidc.client_secret":          "",
		"oidc.redirect_url":           "",
		"otel.exporter_otlp_endpoint": "",
		"otel.service_name":           "wpmgr-api",
		"shutdown.timeout":            "15s",
		"agent.signing_private_key":   "",
		"agent.signing_public_key":    "",
		"agent.signature_skew":        "5m",
		"agent.stale_after":           "10m", // ~2 missed 5-min heartbeats
		"agent.health_interval":       "5m",
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
	default:
		return k
	}
}
