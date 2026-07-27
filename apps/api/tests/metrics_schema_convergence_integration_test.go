package tests

// GH #291 Phase 0 regression test: proves ensureSchema converges an existing
// ClickHouse uptime_checks table (created with only a subset of the declared
// columns, simulating an older deployment that predates a later added
// column) to the current column set via ADD COLUMN IF NOT EXISTS, that a
// converged table accepts a full-shape InsertChecks without a column-count
// mismatch, and that a second ensureSchema run is a clean no-op. Requires
// Docker and skips when it is unavailable (same pattern as
// metrics_integration_test.go).

import (
	"context"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
)

func TestMetricsClickHouseSchemaConvergence(t *testing.T) {
	ctx := context.Background()

	container, err := tcclickhouse.Run(ctx,
		"clickhouse/clickhouse-server:24.3-alpine",
		tcclickhouse.WithUsername("wpmgr"),
		tcclickhouse.WithPassword("wpmgr"),
		tcclickhouse.WithDatabase("default"),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("9000/tcp").WithStartupTimeout(120*time.Second),
		),
	)
	if err != nil {
		t.Skipf("skipping: cannot start clickhouse container (docker unavailable?): %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	host, err := container.ConnectionHost(ctx)
	if err != nil {
		t.Fatalf("clickhouse connection host: %v", err)
	}

	rawConn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{host},
		Auth: clickhouse.Auth{Database: "default", Username: "wpmgr", Password: "wpmgr"},
	})
	if err != nil {
		t.Fatalf("raw connect: %v", err)
	}
	defer rawConn.Close()

	// The entrypoint restarts the server after first-run init; poll until the
	// driver succeeds, same race the real integration test absorbs.
	deadline := time.Now().Add(60 * time.Second)
	for {
		if perr := rawConn.Ping(ctx); perr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("raw driver never became ready")
		}
		time.Sleep(500 * time.Millisecond)
	}

	if err := rawConn.Exec(ctx, "CREATE DATABASE IF NOT EXISTS wpmgr_metrics"); err != nil {
		t.Fatalf("create db: %v", err)
	}
	// Legacy shape: missing tls_ms, ttfb_ms, tls_expiry, error (simulates a
	// table created before those columns existed).
	legacyDDL := `CREATE TABLE wpmgr_metrics.uptime_checks (
    checked_at  DateTime,
    tenant_id   UUID,
    site_id     UUID,
    up          UInt8,
    http_status UInt16,
    dns_ms      Float64,
    connect_ms  Float64,
    total_ms    Float64
) ENGINE = MergeTree()
ORDER BY (tenant_id, site_id, checked_at)`
	if err := rawConn.Exec(ctx, legacyDDL); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}

	// Sanity: the legacy table is missing columns our full Check struct needs.
	before, err := columnSet(ctx, rawConn)
	if err != nil {
		t.Fatalf("describe legacy: %v", err)
	}
	if before["tls_expiry"] || before["error"] {
		t.Fatalf("legacy table unexpectedly already has full shape: %+v", before)
	}
	t.Logf("legacy columns before convergence: %v", before)

	// Now point metrics.New at this SAME container/db. ensureSchema must run
	// CREATE TABLE IF NOT EXISTS (no-op, table exists) then ADD COLUMN IF NOT
	// EXISTS for every declared column, converging the legacy table.
	store, err := metrics.New(ctx, metrics.Config{
		Addr:     host,
		Database: "wpmgr_metrics",
		Username: "wpmgr",
		Password: "wpmgr",
	}, nil)
	if err != nil {
		t.Fatalf("metrics.New against legacy table: %v", err)
	}
	defer store.Close()

	after, err := columnSet(ctx, rawConn)
	if err != nil {
		t.Fatalf("describe after convergence: %v", err)
	}
	for _, want := range []string{"checked_at", "tenant_id", "site_id", "up", "http_status", "dns_ms", "connect_ms", "tls_ms", "ttfb_ms", "total_ms", "tls_expiry", "error"} {
		if !after[want] {
			t.Fatalf("expected column %q after convergence, got columns: %v", want, after)
		}
	}
	t.Logf("columns after convergence: %v", after)

	// A converged table must accept a full-shape insert without a
	// column-count mismatch.
	tenantID, siteID := uuid.New(), uuid.New()
	err = store.InsertChecks(ctx, []metrics.Check{{
		CheckedAt:  time.Now(),
		TenantID:   tenantID,
		SiteID:     siteID,
		Up:         true,
		HTTPStatus: 200,
		TotalMs:    42,
		Error:      "",
	}})
	if err != nil {
		t.Fatalf("insert after convergence: %v", err)
	}

	agg, err := store.QueryAggregate(ctx, tenantID, siteID, time.Hour)
	if err != nil {
		t.Fatalf("query aggregate after convergence: %v", err)
	}
	if agg.Checks != 1 || agg.UpChecks != 1 {
		t.Fatalf("expected 1/1 after convergence insert, got %+v", agg)
	}

	// Re-running ensureSchema again (simulating a second boot) must be a
	// pure no-op: no error, same columns.
	store2, err := metrics.New(ctx, metrics.Config{
		Addr:     host,
		Database: "wpmgr_metrics",
		Username: "wpmgr",
		Password: "wpmgr",
	}, nil)
	if err != nil {
		t.Fatalf("metrics.New second boot (idempotency): %v", err)
	}
	defer store2.Close()
}

func columnSet(ctx context.Context, conn driver.Conn) (map[string]bool, error) {
	rows, err := conn.Query(ctx, "SELECT name FROM system.columns WHERE database = 'wpmgr_metrics' AND table = 'uptime_checks'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}
