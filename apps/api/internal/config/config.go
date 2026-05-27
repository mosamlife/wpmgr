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
	OTel     OTelConfig     `koanf:"otel"`
	Shutdown ShutdownConfig `koanf:"shutdown"`
}

// DBConfig holds Postgres connection parts.
type DBConfig struct {
	Host     string `koanf:"host"`
	Port     int    `koanf:"port"`
	User     string `koanf:"user"`
	Password string `koanf:"password"`
	Name     string `koanf:"name"`
	SSLMode  string `koanf:"sslmode"`
}

// OTelConfig holds OpenTelemetry export configuration.
type OTelConfig struct {
	OTLPEndpoint string `koanf:"exporter_otlp_endpoint"`
	ServiceName  string `koanf:"service_name"`
}

// ShutdownConfig controls graceful-shutdown timing.
type ShutdownConfig struct {
	Timeout time.Duration `koanf:"timeout"`
}

// DSN renders a libpq/pgx connection string from the DB parts.
func (d DBConfig) DSN() string {
	return fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s?sslmode=%s",
		d.User, d.Password, d.Host, d.Port, d.Name, d.SSLMode,
	)
}

// IsProduction reports whether we should emit JSON logs and stricter behavior.
func (c Config) IsProduction() bool {
	return strings.EqualFold(c.Env, "production") || strings.EqualFold(c.Env, "prod")
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
		"otel.exporter_otlp_endpoint": "",
		"otel.service_name":           "wpmgr-api",
		"shutdown.timeout":            "15s",
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
	case strings.HasPrefix(k, "db_"):
		return "db." + strings.TrimPrefix(k, "db_")
	case strings.HasPrefix(k, "otel_"):
		return "otel." + strings.TrimPrefix(k, "otel_")
	default:
		return k
	}
}
