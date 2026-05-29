// Package metrics is the WPMgr uptime metrics store: a thin abstraction over
// the time-series backend used for uptime check results. Two backends are
// supported:
//
//   - ClickHouse (the original M5 design — ADR-028) for high-volume deployments
//     that already run a ClickHouse cluster. Constructed via metrics.New().
//   - Postgres (the M6 GCP-cutover default) which writes one row per probe into
//     the site_uptime_probes table. Constructed via metrics.NewPostgres().
//     Postgres is the system of record at WPMgr's scale and avoids requiring a
//     second datastore in the deployment.
//
// The choice is made at boot in main.go: when WPMGR_CLICKHOUSE_ADDR is set we
// connect to ClickHouse; otherwise we fall back to Postgres so the dashboard
// always has data. Every query is scoped by tenant_id (and usually site_id) —
// the caller verifies tenant ownership in Postgres first.
package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
)

// Config configures the ClickHouse connection.
type Config struct {
	Addr     string
	Database string
	Username string
	Password string
}

// Check is one uptime probe result written to the store.
type Check struct {
	CheckedAt  time.Time
	TenantID   uuid.UUID
	SiteID     uuid.UUID
	Up         bool
	HTTPStatus uint16
	DNSMs      float64
	ConnectMs  float64
	TLSMs      float64
	TTFBMs     float64
	TotalMs    float64
	// TLSExpiry is the leaf certificate's NotAfter. Zero when the probe was not
	// HTTPS or the cert could not be read.
	TLSExpiry time.Time
	// TLSIssuer is the leaf certificate issuer CommonName. Empty when not HTTPS.
	TLSIssuer string
	// TLSSubject is the leaf certificate subject CommonName. Empty when not HTTPS.
	TLSSubject string
	Error      string
}

// Aggregate is the windowed uptime summary for a single site.
type Aggregate struct {
	// Checks is the number of probe rows in the window.
	Checks uint64
	// UpChecks is how many of those were up.
	UpChecks uint64
	// UptimePct is UpChecks/Checks*100 (0 when Checks==0).
	UptimePct float64
	// AvgLatencyMs is the mean total_ms over the window (0 when no rows).
	AvgLatencyMs float64
}

// Point is one downsampled time-bucket in a series.
type Point struct {
	Bucket       time.Time
	Checks       uint64
	UpChecks     uint64
	AvgLatencyMs float64
}

// Latest is the most recent probe result for a site.
type Latest struct {
	CheckedAt  time.Time
	Up         bool
	HTTPStatus uint16
	TotalMs    float64
	TLSExpiry  time.Time
	TLSIssuer  string
	TLSSubject string
	Error      string
	Found      bool
}

// Store is the uptime metrics store contract. Backends implement it for
// ClickHouse and Postgres. A disabled backend no-ops every operation and
// reports Enabled()==false.
type Store interface {
	Enabled() bool
	Close() error
	InsertChecks(ctx context.Context, checks []Check) error
	QueryAggregate(ctx context.Context, tenantID, siteID uuid.UUID, window time.Duration) (Aggregate, error)
	QueryLatest(ctx context.Context, tenantID, siteID uuid.UUID) (Latest, error)
	QuerySeries(ctx context.Context, tenantID, siteID uuid.UUID, window time.Duration, buckets int) ([]Point, error)
}

// chStore is the ClickHouse-backed metrics store (ADR-028). The original M5
// implementation.
type chStore struct {
	conn    driver.Conn
	db      string
	enabled bool
	logger  *slog.Logger
}

// retentionDays is the uptime_checks TTL window (~90d).
const retentionDays = 90

// New connects to ClickHouse and ensures the schema exists. When cfg.Addr is
// empty it returns a disabled Store (Enabled()==false) that no-ops, so the
// stack runs without ClickHouse. A configured-but-unreachable ClickHouse is a
// hard error (misconfiguration should fail fast).
//
// In M6 the boot path prefers metrics.NewPostgres when WPMGR_CLICKHOUSE_ADDR is
// empty, so the disabled-Store branch here is now only used in tests; it is
// preserved for backwards compatibility with the integration tests.
func New(ctx context.Context, cfg Config, logger *slog.Logger) (Store, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Addr == "" {
		logger.Warn("WPMGR_CLICKHOUSE_ADDR is empty: ClickHouse metrics store disabled")
		return &chStore{enabled: false, logger: logger}, nil
	}
	db := cfg.Database
	if db == "" {
		db = "wpmgr_metrics"
	}

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{cfg.Addr},
		Auth: clickhouse.Auth{
			// Connect to the default database first; we CREATE DATABASE below.
			Database: "default",
			Username: cfg.Username,
			Password: cfg.Password,
		},
		DialTimeout: 10 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("clickhouse open: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := conn.Ping(pingCtx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clickhouse ping: %w", err)
	}

	s := &chStore{conn: conn, db: db, enabled: true, logger: logger}
	if err := s.ensureSchema(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	logger.Info("uptime metrics store ready (clickhouse)", slog.String("clickhouse_db", db))
	return s, nil
}

// Enabled reports whether the store is backed by a live ClickHouse connection.
func (s *chStore) Enabled() bool { return s != nil && s.enabled }

// Close releases the ClickHouse connection (no-op when disabled).
func (s *chStore) Close() error {
	if !s.Enabled() {
		return nil
	}
	return s.conn.Close()
}

// ensureSchema creates the database and the uptime_checks table if absent. The
// table is a MergeTree ordered by (tenant_id, site_id, checked_at) — the exact
// prefix every tenant-scoped per-site query filters on — with a 90-day TTL on
// checked_at so old rows are reclaimed automatically.
func (s *chStore) ensureSchema(ctx context.Context) error {
	if err := s.conn.Exec(ctx, fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", s.db)); err != nil {
		return fmt.Errorf("clickhouse create database: %w", err)
	}
	ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.uptime_checks (
    checked_at  DateTime,
    tenant_id   UUID,
    site_id     UUID,
    up          UInt8,
    http_status UInt16,
    dns_ms      Float64,
    connect_ms  Float64,
    tls_ms      Float64,
    ttfb_ms     Float64,
    total_ms    Float64,
    tls_expiry  DateTime,
    error       String
) ENGINE = MergeTree()
ORDER BY (tenant_id, site_id, checked_at)
TTL checked_at + INTERVAL %d DAY`, s.db, retentionDays)
	if err := s.conn.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("clickhouse create uptime_checks: %w", err)
	}
	return nil
}

// chTime keeps a DateTime value within ClickHouse's representable range and at
// second resolution. A zero/sentinel TLS expiry maps to the ClickHouse epoch.
func chTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Unix(0, 0).UTC()
	}
	return t.UTC().Truncate(time.Second)
}

// InsertChecks batch-inserts probe results via PrepareBatch. No-ops (returns
// nil) when the store is disabled or the batch is empty.
func (s *chStore) InsertChecks(ctx context.Context, checks []Check) error {
	if !s.Enabled() || len(checks) == 0 {
		return nil
	}
	batch, err := s.conn.PrepareBatch(ctx, fmt.Sprintf("INSERT INTO %s.uptime_checks", s.db))
	if err != nil {
		return fmt.Errorf("clickhouse prepare batch: %w", err)
	}
	for _, c := range checks {
		up := uint8(0)
		if c.Up {
			up = 1
		}
		if err := batch.Append(
			chTime(c.CheckedAt),
			c.TenantID,
			c.SiteID,
			up,
			c.HTTPStatus,
			c.DNSMs,
			c.ConnectMs,
			c.TLSMs,
			c.TTFBMs,
			c.TotalMs,
			chTime(c.TLSExpiry),
			c.Error,
		); err != nil {
			_ = batch.Abort()
			return fmt.Errorf("clickhouse append row: %w", err)
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("clickhouse send batch: %w", err)
	}
	return nil
}

// QueryAggregate returns the windowed uptime % and average latency for one site,
// always filtered by tenant_id + site_id. Returns a zero Aggregate (no error)
// when the store is disabled.
func (s *chStore) QueryAggregate(ctx context.Context, tenantID, siteID uuid.UUID, window time.Duration) (Aggregate, error) {
	var agg Aggregate
	if !s.Enabled() {
		return agg, nil
	}
	since := time.Now().Add(-window)
	row := s.conn.QueryRow(ctx, fmt.Sprintf(`
SELECT count() AS checks,
       sum(up) AS up_checks,
       avgOrNull(total_ms) AS avg_latency
FROM %s.uptime_checks
WHERE tenant_id = ? AND site_id = ? AND checked_at >= ?`, s.db),
		tenantID, siteID, chTime(since))

	var checks, upChecks uint64
	var avg *float64
	if err := row.Scan(&checks, &upChecks, &avg); err != nil {
		return agg, fmt.Errorf("clickhouse aggregate scan: %w", err)
	}
	agg.Checks = checks
	agg.UpChecks = upChecks
	if checks > 0 {
		agg.UptimePct = float64(upChecks) / float64(checks) * 100
	}
	if avg != nil {
		agg.AvgLatencyMs = *avg
	}
	return agg, nil
}

// QueryLatest returns the most recent probe result for one site (tenant-scoped).
func (s *chStore) QueryLatest(ctx context.Context, tenantID, siteID uuid.UUID) (Latest, error) {
	var l Latest
	if !s.Enabled() {
		return l, nil
	}
	row := s.conn.QueryRow(ctx, fmt.Sprintf(`
SELECT checked_at, up, http_status, total_ms, tls_expiry, error
FROM %s.uptime_checks
WHERE tenant_id = ? AND site_id = ?
ORDER BY checked_at DESC
LIMIT 1`, s.db), tenantID, siteID)

	var up uint8
	var checkedAt, tlsExpiry time.Time
	var httpStatus uint16
	var totalMs float64
	var errStr string
	if err := row.Scan(&checkedAt, &up, &httpStatus, &totalMs, &tlsExpiry, &errStr); err != nil {
		// No rows yet for this site: not an error, just no data.
		return Latest{Found: false}, nil
	}
	l = Latest{
		CheckedAt:  checkedAt,
		Up:         up == 1,
		HTTPStatus: httpStatus,
		TotalMs:    totalMs,
		Error:      errStr,
		Found:      true,
	}
	if tlsExpiry.Unix() > 0 {
		l.TLSExpiry = tlsExpiry
	}
	return l, nil
}

// QuerySeries returns a downsampled per-bucket series for one site over a window
// (tenant-scoped). buckets controls the target resolution; the bucket width is
// window/buckets rounded to whole seconds (min 1 minute).
func (s *chStore) QuerySeries(ctx context.Context, tenantID, siteID uuid.UUID, window time.Duration, buckets int) ([]Point, error) {
	if !s.Enabled() {
		return nil, nil
	}
	if buckets <= 0 {
		buckets = 100
	}
	bucketSecs := int64(window.Seconds()) / int64(buckets)
	if bucketSecs < 60 {
		bucketSecs = 60
	}
	since := time.Now().Add(-window)
	rows, err := s.conn.Query(ctx, fmt.Sprintf(`
SELECT toStartOfInterval(checked_at, INTERVAL %d SECOND) AS bucket,
       count() AS checks,
       sum(up) AS up_checks,
       avg(total_ms) AS avg_latency
FROM %s.uptime_checks
WHERE tenant_id = ? AND site_id = ? AND checked_at >= ?
GROUP BY bucket
ORDER BY bucket ASC`, bucketSecs, s.db), tenantID, siteID, chTime(since))
	if err != nil {
		return nil, fmt.Errorf("clickhouse series query: %w", err)
	}
	defer rows.Close()

	var out []Point
	for rows.Next() {
		var p Point
		if err := rows.Scan(&p.Bucket, &p.Checks, &p.UpChecks, &p.AvgLatencyMs); err != nil {
			return nil, fmt.Errorf("clickhouse series scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
