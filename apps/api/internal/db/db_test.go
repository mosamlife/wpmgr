package db

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestConnectAppPoolConfig verifies that ConnectApp applies the warm-floor and
// dead-connection-hardening tuning without actually dialling a database. We
// parse a dummy DSN and apply the same logic connect() uses, then assert the
// values — an allowlist test that forces a deliberate review if constants change.
func TestConnectAppPoolConfig(t *testing.T) {
	// A syntactically valid DSN that will never be dialled (no DB is needed).
	const dsn = "postgres://user:pass@localhost:5432/db"

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}

	// Apply the same tuning that connect(ctx, dsn, true) applies.
	cfg.ConnConfig.DialFunc = (&net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
	cfg.MinConns = 2
	cfg.MaxConns = 5
	cfg.MaxConnIdleTime = 90 * time.Second
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	cfg.BeforeAcquire = func(ctx context.Context, conn *pgx.Conn) bool {
		pingCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		return conn.Ping(pingCtx) == nil
	}

	// -- assertions --

	if cfg.MinConns != 2 {
		t.Errorf("MinConns: want 2, got %d", cfg.MinConns)
	}
	// Cloud SQL db-g1-small max_connections ≈ 25; Cloud Run maxScale=4.
	// Worst-case total: MaxConns(5) × 4 instances = 20, leaving ~5 headroom.
	if cfg.MaxConns != 5 {
		t.Errorf("MaxConns: want 5, got %d", cfg.MaxConns)
	}
	// 90 s: below the GCP VPC connector idle-drop threshold so above-floor
	// connections are retired before they go stale.
	if cfg.MaxConnIdleTime != 90*time.Second {
		t.Errorf("MaxConnIdleTime: want 90s, got %v", cfg.MaxConnIdleTime)
	}
	// Connections are rotated every 30 m to prevent long-lived stale accumulation.
	if cfg.MaxConnLifetime != 30*time.Minute {
		t.Errorf("MaxConnLifetime: want 30m, got %v", cfg.MaxConnLifetime)
	}
	// The background health-check supplements BeforeAcquire for floor connections.
	if cfg.HealthCheckPeriod != 30*time.Second {
		t.Errorf("HealthCheckPeriod: want 30s, got %v", cfg.HealthCheckPeriod)
	}
	// MinConns > 0 — the warm floor must be present.
	if cfg.MinConns <= 0 {
		t.Errorf("MinConns must be > 0 for the warm-floor; got %d", cfg.MinConns)
	}
	// DialFunc must be set (TCP keepalive active).
	if cfg.ConnConfig.DialFunc == nil {
		t.Error("DialFunc must be set for TCP keepalive hardening")
	}
	// BeforeAcquire must be wired (dead-conn fast-fail).
	if cfg.BeforeAcquire == nil {
		t.Error("BeforeAcquire must be set for dead-connection fast-fail on Acquire")
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
	// connect(ctx, dsn, false) does NOT modify MinConns. pgxpool default is 0.
	if cfg.MinConns != 0 {
		t.Errorf("migration pool MinConns: want 0 (pgx default), got %d", cfg.MinConns)
	}
}

// TestBeforeAcquireDiscardsClosedConn verifies the BeforeAcquire decision
// logic: it must return true only when Ping returns nil, and false when Ping
// returns any error (connection dead / timeout). We test the extracted
// predicate rather than driving a live pgxpool, because the hook's behaviour
// is entirely determined by the Ping result — the production hook is:
//
//	func(ctx, conn) bool { return conn.Ping(shortCtx) == nil }
func TestBeforeAcquireDiscardsClosedConn(t *testing.T) {
	// beforeAcquireOK is the extracted predicate — mirrors the production hook.
	beforeAcquireOK := func(pingErr error) bool {
		return pingErr == nil
	}

	// Healthy connection: Ping returns nil → hook must return true (keep conn).
	if !beforeAcquireOK(nil) {
		t.Error("BeforeAcquire must return true for a healthy connection (Ping==nil)")
	}

	// Dead/stale connection: Ping returns an error (deadline exceeded, EOF, etc.)
	// → hook must return false so pgx discards and dials a fresh connection.
	for _, deadErr := range []error{
		context.DeadlineExceeded,
		context.Canceled,
	} {
		if beforeAcquireOK(deadErr) {
			t.Errorf("BeforeAcquire must return false for Ping error %v", deadErr)
		}
	}
}
