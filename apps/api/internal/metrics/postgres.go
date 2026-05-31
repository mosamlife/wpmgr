package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

// pgStore is the Postgres-backed metrics store added in M6 (post-GCP cutover).
// One row per probe lands in public.site_uptime_probes; aggregates and series
// are computed at query time. The implementation uses the InAgentTx scope for
// writes/cross-tenant queries because the probe worker writes every site's
// row in one sweep (no per-row tenant scope is available cheaply), and the
// agent-side RLS policy permits it. Per-tenant reads use InAgentTx as well —
// the metric queries are always filtered by an explicit tenant_id parameter,
// and the SiteVerifier in uptime.Service has already proved tenant ownership
// before any query reaches the store.
type pgStore struct {
	pool   *db.Pool
	logger *slog.Logger
}

// NewPostgres returns a Postgres-backed metrics store. The required schema
// (site_uptime_probes) is provisioned by the M6 migration that runs at boot.
func NewPostgres(pool *db.Pool, logger *slog.Logger) Store {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("uptime metrics store ready (postgres)")
	return &pgStore{pool: pool, logger: logger}
}

// Enabled always reports true: the Postgres backend is always usable once the
// pool is wired (the migration guarantees the table exists by boot time).
func (s *pgStore) Enabled() bool { return true }

// Close is a no-op for the Postgres backend — the db.Pool is owned and closed
// at the process level.
func (s *pgStore) Close() error { return nil }

// InsertChecks batch-inserts probe results. Runs under InAgentTx so the probe
// worker (which sweeps every tenant) can write without a per-row tenant scope;
// the site_uptime_probes_agent RLS policy permits this. No-ops on empty input.
func (s *pgStore) InsertChecks(ctx context.Context, checks []Check) error {
	if len(checks) == 0 {
		return nil
	}
	return s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		batch := &pgx.Batch{}
		for _, c := range checks {
			batch.Queue(`INSERT INTO site_uptime_probes
(tenant_id, site_id, probed_at, up, http_status, dns_ms, connect_ms, tls_ms, ttfb_ms, total_ms,
 tls_expiry, tls_issuer, tls_subject, error_text)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
				c.TenantID,
				c.SiteID,
				ifZeroNow(c.CheckedAt),
				c.Up,
				int32(c.HTTPStatus),
				c.DNSMs,
				c.ConnectMs,
				c.TLSMs,
				c.TTFBMs,
				c.TotalMs,
				nullableTime(c.TLSExpiry),
				c.TLSIssuer,
				c.TLSSubject,
				c.Error,
			)
		}
		br := tx.SendBatch(ctx, batch)
		defer func() { _ = br.Close() }()
		for range checks {
			if _, err := br.Exec(); err != nil {
				return fmt.Errorf("postgres insert probe: %w", err)
			}
		}
		return nil
	})
}

// QueryAggregate returns the windowed uptime % and average latency for one
// site, filtered by tenant_id + site_id.
func (s *pgStore) QueryAggregate(ctx context.Context, tenantID, siteID uuid.UUID, window time.Duration) (Aggregate, error) {
	var agg Aggregate
	since := time.Now().Add(-window)
	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
SELECT COUNT(*)                                     AS checks,
       COUNT(*) FILTER (WHERE up)                   AS up_checks,
       COALESCE(AVG(NULLIF(total_ms, 0))::float8, 0) AS avg_latency
FROM site_uptime_probes
WHERE tenant_id = $1 AND site_id = $2 AND probed_at >= $3`,
			tenantID, siteID, since)
		var checks, upChecks int64
		var avgLatency float64
		if err := row.Scan(&checks, &upChecks, &avgLatency); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("postgres aggregate scan: %w", err)
		}
		agg.Checks = uint64(checks)
		agg.UpChecks = uint64(upChecks)
		if checks > 0 {
			agg.UptimePct = float64(upChecks) / float64(checks) * 100
		}
		agg.AvgLatencyMs = avgLatency
		return nil
	})
	return agg, err
}

// QueryLatest returns the most recent probe row for one site (tenant-scoped).
// Backed by the (site_id, probed_at DESC) index so the LIMIT 1 is a cheap
// index-only seek even as the table grows.
func (s *pgStore) QueryLatest(ctx context.Context, tenantID, siteID uuid.UUID) (Latest, error) {
	var l Latest
	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
SELECT probed_at, up, http_status, total_ms, tls_expiry, tls_issuer, tls_subject, error_text
FROM site_uptime_probes
WHERE tenant_id = $1 AND site_id = $2
ORDER BY probed_at DESC
LIMIT 1`, tenantID, siteID)
		var (
			probedAt   time.Time
			up         bool
			httpStatus int32
			totalMs    float64
			tlsExpiry  *time.Time
			tlsIssuer  string
			tlsSubject string
			errText    string
		)
		if err := row.Scan(&probedAt, &up, &httpStatus, &totalMs, &tlsExpiry, &tlsIssuer, &tlsSubject, &errText); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("postgres latest scan: %w", err)
		}
		l = Latest{
			CheckedAt:  probedAt,
			Up:         up,
			HTTPStatus: uint16(httpStatus),
			TotalMs:    totalMs,
			TLSIssuer:  tlsIssuer,
			TLSSubject: tlsSubject,
			Error:      errText,
			Found:      true,
		}
		if tlsExpiry != nil {
			l.TLSExpiry = *tlsExpiry
		}
		return nil
	})
	return l, err
}

// QuerySeries returns a downsampled per-bucket series for one site over the
// window. Buckets are date_trunc-aligned: width = window/buckets rounded to
// whole seconds (min 60s). We use to_timestamp(floor(extract(epoch))/W*W) to
// avoid date_trunc's fixed-width restriction.
func (s *pgStore) QuerySeries(ctx context.Context, tenantID, siteID uuid.UUID, window time.Duration, buckets int) ([]Point, error) {
	if buckets <= 0 {
		buckets = 100
	}
	bucketSecs := int64(window.Seconds()) / int64(buckets)
	if bucketSecs < 60 {
		bucketSecs = 60
	}
	since := time.Now().Add(-window)

	var out []Point
	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, fmt.Sprintf(`
SELECT to_timestamp(floor(extract(epoch FROM probed_at) / %d) * %d) AS bucket,
       COUNT(*)                                     AS checks,
       COUNT(*) FILTER (WHERE up)                   AS up_checks,
       COALESCE(AVG(NULLIF(total_ms, 0))::float8, 0) AS avg_latency
FROM site_uptime_probes
WHERE tenant_id = $1 AND site_id = $2 AND probed_at >= $3
GROUP BY bucket
ORDER BY bucket ASC`, bucketSecs, bucketSecs),
			tenantID, siteID, since)
		if err != nil {
			return fmt.Errorf("postgres series query: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				bucket           time.Time
				checks, upChecks int64
				avgLatency       float64
			)
			if err := rows.Scan(&bucket, &checks, &upChecks, &avgLatency); err != nil {
				return fmt.Errorf("postgres series scan: %w", err)
			}
			out = append(out, Point{
				Bucket:       bucket,
				Checks:       uint64(checks),
				UpChecks:     uint64(upChecks),
				AvgLatencyMs: avgLatency,
			})
		}
		return rows.Err()
	})
	return out, err
}

func ifZeroNow(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}

// nullableTime maps a zero-value time.Time to a Postgres NULL, so the column
// (declared NULL-able) stays NULL when the probe carried no TLS cert.
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
