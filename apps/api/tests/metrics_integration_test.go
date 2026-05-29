package tests

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mosamlife/wpmgr/apps/api/internal/metrics"
)

// startClickHouse spins up an ephemeral ClickHouse and returns a metrics.Store
// connected to it (schema auto-created). Skips when Docker is unavailable.
func startClickHouse(t *testing.T) metrics.Store {
	t.Helper()
	ctx := context.Background()

	container, err := tcclickhouse.Run(ctx,
		"clickhouse/clickhouse-server:24.3-alpine",
		tcclickhouse.WithUsername("wpmgr"),
		tcclickhouse.WithPassword("wpmgr"),
		tcclickhouse.WithDatabase("default"),
		// CH redirects its logs to file, so a log-based wait cannot see "Ready
		// for connections". Wait for the native-protocol port; we then poll the
		// driver below to absorb the post-entrypoint restart race.
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

	// The entrypoint restarts the server after first-run init; poll until the
	// driver succeeds (port-listening fires before the restart completes).
	var store metrics.Store
	deadline := time.Now().Add(60 * time.Second)
	for {
		store, err = metrics.New(ctx, metrics.Config{
			Addr:     host,
			Database: "wpmgr_metrics",
			Username: "wpmgr",
			Password: "wpmgr",
		}, nil)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("metrics.New (schema auto-create) never succeeded: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !store.Enabled() {
		t.Fatal("expected store enabled")
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestMetricsInsertAndAggregate inserts a known mix of up/down checks for a site
// and asserts the windowed uptime % + average latency aggregate is correct, and
// that schema auto-create succeeded (New would have failed otherwise).
func TestMetricsInsertAndAggregate(t *testing.T) {
	store := startClickHouse(t)
	ctx := context.Background()

	tenantID, siteID := uuid.New(), uuid.New()
	other := uuid.New() // a second site to prove site scoping.
	now := time.Now()

	var checks []metrics.Check
	// 8 up, 2 down for siteID ⇒ 80% uptime; avg latency over the 10 = 100ms.
	for i := 0; i < 10; i++ {
		checks = append(checks, metrics.Check{
			CheckedAt: now.Add(-time.Duration(i) * time.Minute),
			TenantID:  tenantID,
			SiteID:    siteID,
			Up:        i >= 2, // first two (i=0,1) down.
			TotalMs:   100,
		})
	}
	// Noise for another site: must NOT affect siteID's aggregate.
	for i := 0; i < 5; i++ {
		checks = append(checks, metrics.Check{
			CheckedAt: now.Add(-time.Duration(i) * time.Minute),
			TenantID:  tenantID,
			SiteID:    other,
			Up:        false,
			TotalMs:   999,
		})
	}
	if err := store.InsertChecks(ctx, checks); err != nil {
		t.Fatalf("insert checks: %v", err)
	}

	agg, err := store.QueryAggregate(ctx, tenantID, siteID, 24*time.Hour)
	if err != nil {
		t.Fatalf("query aggregate: %v", err)
	}
	if agg.Checks != 10 {
		t.Fatalf("expected 10 checks for site, got %d (site scoping leak?)", agg.Checks)
	}
	if agg.UpChecks != 8 || agg.UptimePct < 79.9 || agg.UptimePct > 80.1 {
		t.Fatalf("expected 80%% uptime, got %.2f%% (%d/%d)", agg.UptimePct, agg.UpChecks, agg.Checks)
	}
	if agg.AvgLatencyMs < 99.9 || agg.AvgLatencyMs > 100.1 {
		t.Fatalf("expected avg latency ~100ms, got %.2f", agg.AvgLatencyMs)
	}

	// Latest should reflect the most recent (i=0 ⇒ down) check.
	latest, err := store.QueryLatest(ctx, tenantID, siteID)
	if err != nil {
		t.Fatalf("query latest: %v", err)
	}
	if !latest.Found || latest.Up {
		t.Fatalf("expected latest found+down, got %+v", latest)
	}

	// A foreign tenant sees nothing for this site (tenant scoping).
	aggForeign, err := store.QueryAggregate(ctx, uuid.New(), siteID, 24*time.Hour)
	if err != nil {
		t.Fatalf("query aggregate (foreign tenant): %v", err)
	}
	if aggForeign.Checks != 0 {
		t.Fatalf("expected 0 checks for foreign tenant, got %d (tenant scoping leak?)", aggForeign.Checks)
	}
}

// TestMetricsDisabledNoop asserts an unconfigured store no-ops cleanly.
func TestMetricsDisabledNoop(t *testing.T) {
	store, err := metrics.New(context.Background(), metrics.Config{Addr: ""}, nil)
	if err != nil {
		t.Fatalf("disabled metrics.New: %v", err)
	}
	if store.Enabled() {
		t.Fatal("expected disabled store")
	}
	if err := store.InsertChecks(context.Background(), []metrics.Check{{SiteID: uuid.New()}}); err != nil {
		t.Fatalf("disabled InsertChecks should no-op, got %v", err)
	}
	agg, err := store.QueryAggregate(context.Background(), uuid.New(), uuid.New(), time.Hour)
	if err != nil || agg.Checks != 0 {
		t.Fatalf("disabled QueryAggregate should return zero, got %+v err=%v", agg, err)
	}
}
