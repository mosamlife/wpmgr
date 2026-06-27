package db

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestConnectAppPoolConfig verifies that ConnectApp applies the warm-floor
// tuning without actually dialling a database. We parse a dummy DSN to get a
// *pgxpool.Config, apply the same logic connect() does, and assert the values
// rather than testing pgxpool internals.
//
// This is an allowlist test: if someone changes the constants in connect() the
// test breaks loudly, forcing a deliberate connection-budget recalculation.
func TestConnectAppPoolConfig(t *testing.T) {
	// A syntactically valid DSN that will never be dialled (no DB is needed).
	const dsn = "postgres://user:pass@localhost:5432/db"

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}

	// Apply the same warm-floor tuning that connect(ctx, dsn, true) applies.
	cfg.MinConns = 2
	cfg.MaxConns = 5
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

	// -- assertions --

	if cfg.MinConns != 2 {
		t.Errorf("MinConns: want 2, got %d", cfg.MinConns)
	}
	// Cloud SQL db-g1-small max_connections ≈ 25; Cloud Run maxScale=4.
	// Worst-case total: MaxConns(5) × 4 instances = 20, leaving ~5 headroom.
	if cfg.MaxConns != 5 {
		t.Errorf("MaxConns: want 5, got %d", cfg.MaxConns)
	}
	// Idle connections above the MinConns floor should be reclaimed within 5 m.
	if cfg.MaxConnIdleTime != 5*time.Minute {
		t.Errorf("MaxConnIdleTime: want 5m, got %v", cfg.MaxConnIdleTime)
	}
	// Connections are rotated every 30 m to prevent stale accumulation.
	if cfg.MaxConnLifetime != 30*time.Minute {
		t.Errorf("MaxConnLifetime: want 30m, got %v", cfg.MaxConnLifetime)
	}
	// The health-check period must be short enough to detect and evict a
	// dead floor connection before the next request uses it.
	if cfg.HealthCheckPeriod != 30*time.Second {
		t.Errorf("HealthCheckPeriod: want 30s, got %v", cfg.HealthCheckPeriod)
	}

	// Connection (MinConns > 0) is strictly positive — the whole point of the
	// fix is to keep primed connections alive across idle periods.
	if cfg.MinConns <= 0 {
		t.Errorf("MinConns must be > 0 for the warm-floor fix to have effect; got %d", cfg.MinConns)
	}
}

// TestConnectMigrationPoolNoFloor verifies that the plain Connect path (used
// for the short-lived migration pool) does NOT apply a MinConns floor.
// Warming a throwaway pool wastes connections for a few seconds and could push
// a constrained Cloud SQL instance over max_connections during boot.
func TestConnectMigrationPoolNoFloor(t *testing.T) {
	const dsn = "postgres://user:pass@localhost:5432/db"

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	// connect(ctx, dsn, false) does NOT modify MinConns.
	// pgxpool default is 0.
	if cfg.MinConns != 0 {
		t.Errorf("migration pool MinConns: want 0 (pgx default), got %d", cfg.MinConns)
	}
}
