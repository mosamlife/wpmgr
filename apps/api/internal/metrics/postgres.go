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

// UpsertRollup implements metrics.RollupWriter (m99). It folds one sweep's
// checks into site_uptime_daily (additive per-UTC-day counters) and
// site_uptime_status (the latest-probe snapshot) in a SINGLE InAgentTx batch
// — the same batching shape as InsertChecks, so one sweep costs one extra
// round trip regardless of fleet size. Deliberately a SEPARATE transaction
// from InsertChecks (not merged into it): a rollup-write failure must never
// roll back the raw probe insert that already committed, so probing/alerting
// keep working even if this best-effort write fails — see the RollupWriter
// doc comment and the caller (uptime.ProbeWorker.Sweep), which logs and
// continues on error rather than treating it as fatal.
//
// Per-check math mirrors exactly what the old QueryFleetUptime computed at
// read time from raw rows, so the rollup and a direct aggregate over
// site_uptime_probes agree (see the m99 migration's backfill comment and the
// equivalence test in tests/metrics_integration_test.go):
//   - up_checks/total_checks: count(*) [FILTER (WHERE up)]
//   - sum_latency_ms/latency_samples: sum/count of total_ms, but ONLY for
//     checks where up AND total_ms != 0 — reproducing the old
//     AVG(NULLIF(total_ms, 0)) FILTER (WHERE up) exactly (NULLIF turns a
//     zero latency reading into NULL, which AVG excludes from both the sum
//     and the divisor).
//
// site_uptime_status's UPDATE is freshness-guarded (WHERE
// EXCLUDED.last_probed_at >= site_uptime_status.last_probed_at) so an
// overlapping or delayed sweep (the probe worker's periodic job has no
// uniqueness constraint preventing two sweeps from running concurrently
// during a slow/fleet-wide-outage sweep) can never regress a fresher status
// with stale data — the same overlapping-sweep hazard TransitionAlertState's
// doc comment describes for alert state, applied here to the status stamp.
// site_uptime_daily's counters need no such guard: addition is commutative,
// so two overlapping sweeps' increments land correctly regardless of order.
func (s *pgStore) UpsertRollup(ctx context.Context, checks []Check) error {
	if len(checks) == 0 {
		return nil
	}
	return s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		batch := &pgx.Batch{}
		for _, c := range checks {
			checkedAt := ifZeroNow(c.CheckedAt)
			day := checkedAt.UTC().Truncate(24 * time.Hour)

			var upInc int32
			if c.Up {
				upInc = 1
			}
			var latencyMs float64
			var latencySample int32
			if c.Up && c.TotalMs != 0 {
				latencyMs = c.TotalMs
				latencySample = 1
			}

			batch.Queue(`INSERT INTO site_uptime_daily
(tenant_id, site_id, day, up_checks, total_checks, sum_latency_ms, latency_samples)
VALUES ($1, $2, $3, $4, 1, $5, $6)
ON CONFLICT (site_id, day) DO UPDATE SET
    up_checks       = site_uptime_daily.up_checks + EXCLUDED.up_checks,
    total_checks    = site_uptime_daily.total_checks + EXCLUDED.total_checks,
    sum_latency_ms  = site_uptime_daily.sum_latency_ms + EXCLUDED.sum_latency_ms,
    latency_samples = site_uptime_daily.latency_samples + EXCLUDED.latency_samples,
    updated_at      = now()`,
				c.TenantID, c.SiteID, day, upInc, latencyMs, latencySample,
			)

			batch.Queue(`INSERT INTO site_uptime_status
(site_id, tenant_id, latest_up, last_probed_at, tls_expiry, updated_at)
VALUES ($1, $2, $3, $4, $5, now())
ON CONFLICT (site_id) DO UPDATE SET
    tenant_id      = EXCLUDED.tenant_id,
    latest_up      = EXCLUDED.latest_up,
    last_probed_at = EXCLUDED.last_probed_at,
    tls_expiry     = EXCLUDED.tls_expiry,
    updated_at     = now()
WHERE EXCLUDED.last_probed_at >= site_uptime_status.last_probed_at`,
				c.SiteID, c.TenantID, c.Up, checkedAt, nullableTime(c.TLSExpiry),
			)
		}
		br := tx.SendBatch(ctx, batch)
		defer func() { _ = br.Close() }()
		// Two statements queued per check (daily upsert, then status upsert).
		for range checks {
			if _, err := br.Exec(); err != nil {
				return fmt.Errorf("postgres upsert rollup daily: %w", err)
			}
			if _, err := br.Exec(); err != nil {
				return fmt.Errorf("postgres upsert rollup status: %w", err)
			}
		}
		return nil
	})
}

var _ RollupWriter = (*pgStore)(nil)

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

// fleetUptimeQuery is the m99 HYBRID fleet-uptime query: EXACT equivalence to
// the old rolling window (probed_at >= now-window) at O(~2 days-of-probes +
// days-in-window rollup rows) per site instead of O(probes-in-window)
// (~43k rows/site for a 30-day window at the ~60s probe cadence).
//
// The product decision (see the m99 follow-up adversarial review) is to keep
// uptime numbers EXACT, not day-granular — a day-bucketed approximation was
// rejected because it can misreport an outage that falls in a boundary day's
// out-of-window portion. The window [start, now] is decomposed into three
// NON-OVERLAPPING, GAP-FREE parts, summed together:
//
//  1. ROLLUP middle — site_uptime_daily WHERE day > boundaryDay AND day <
//     today: complete UTC days strictly inside the window. Exact (a full day
//     is a full day) and immune to today's live write races since "today" is
//     always excluded here and read live in part 3.
//  2. RAW boundary tail — site_uptime_probes WHERE probed_at >= start AND
//     probed_at < min(boundaryDay+1, today): the in-window slice of the
//     OLDEST (possibly partial) day. This is the read that a day-granular
//     rollup-only approach cannot do exactly — it is the exact fix.
//  3. RAW today — site_uptime_probes WHERE probed_at >= max(start, today)
//     AND probed_at <= now: today's head, read LIVE (not from the rollup) so
//     a rare best-effort rollup-upsert miss on today's probe can never make
//     today's counters read low.
//
// Parts 2 and 3 are each bounded to at most ~1 UTC day of rows (~1,440 probes
// at the 60s cadence) and are served index-only by the m85 covering index
// site_uptime_probes_agg_idx (site_id, tenant_id, probed_at DESC) INCLUDE
// (up, total_ms) — see TestQueryFleetUptime_RawReadsAreIndexBounded for the
// EXPLAIN proof. This is what keeps the hybrid fast: at most ~2 days of raw
// rows are ever scanned, never the full window.
//
// All three parts reproduce the OLD single-pass aggregate's semantics
// exactly: total_checks=COUNT(*), up_checks=COUNT(*) FILTER (WHERE up),
// sum_latency_ms/latency_samples reproduce AVG(NULLIF(total_ms,0)) FILTER
// (WHERE up) (zero-latency-while-up probes excluded from both).
//
// Degenerate windows collapse correctly with no special-casing: when
// boundaryDay == today (window < 1 day, or any window entirely within
// today), start >= today's UTC midnight, which makes the boundary-tail
// predicate (probed_at >= start AND probed_at < today) unsatisfiable — an
// empty (not erroring) result — and the RAW today part's lower bound becomes
// max(start, today) = start, so the ENTIRE window is served by part 3 alone.
// When boundaryDay == today-1, the ROLLUP middle predicate (day > boundaryDay
// AND day < today) is likewise unsatisfiable — empty — and the window is
// served by parts 2+3 only.
//
// Every date/timestamp parameter is computed in Go from now.UTC() (never
// server-side now()/CURRENT_DATE, never an implicit timestamptz->date cast)
// and bound with an explicit ::date/::timestamptz cast, so the decomposition
// is identical regardless of the Postgres session's TimeZone GUC — a non-UTC
// self-host would otherwise shift the UTC-day boundaries used to key
// site_uptime_daily.day.
//
// The latest-probe snapshot (site_uptime_status) is joined with a
// last_probed_at >= retentionCutoff guard so a site whose only history has
// aged past uptime probe retention (probeRetention, shared with
// UptimeProbeGCWorker) reads as "no data" — exactly like the old query,
// where a GC'd site's last raw probe row would already be gone.
const fleetUptimeQuery = `
SELECT
    s.id AS site_id,
    st.latest_up,
    st.last_probed_at,
    st.tls_expiry,
    agg.up_checks,
    agg.total_checks,
    agg.sum_latency_ms,
    agg.latency_samples
FROM unnest($2::uuid[]) AS s(id)
LEFT JOIN site_uptime_status st
    ON st.site_id = s.id
   AND st.tenant_id = $1
   AND st.last_probed_at >= $7::timestamptz
LEFT JOIN LATERAL (
    SELECT
        COALESCE(SUM(parts.up_checks), 0)::bigint       AS up_checks,
        COALESCE(SUM(parts.total_checks), 0)::bigint    AS total_checks,
        COALESCE(SUM(parts.sum_latency_ms), 0)          AS sum_latency_ms,
        COALESCE(SUM(parts.latency_samples), 0)::bigint AS latency_samples
    FROM (
        -- 1. ROLLUP middle: complete UTC days strictly inside the window.
        SELECT
            COALESCE(SUM(up_checks), 0)      AS up_checks,
            COALESCE(SUM(total_checks), 0)   AS total_checks,
            COALESCE(SUM(sum_latency_ms), 0) AS sum_latency_ms,
            COALESCE(SUM(latency_samples), 0) AS latency_samples
        FROM site_uptime_daily
        WHERE site_id = s.id AND tenant_id = $1
          AND day > $3::date AND day < $4::date

        UNION ALL

        -- 2. RAW boundary tail: in-window slice of the oldest partial day.
        SELECT
            count(*) FILTER (WHERE up) AS up_checks,
            count(*)                   AS total_checks,
            COALESCE(sum(total_ms) FILTER (WHERE up AND total_ms <> 0), 0) AS sum_latency_ms,
            count(*) FILTER (WHERE up AND total_ms <> 0) AS latency_samples
        FROM site_uptime_probes
        WHERE site_id = s.id AND tenant_id = $1
          AND probed_at >= $5::timestamptz AND probed_at < $6::timestamptz

        UNION ALL

        -- 3. RAW today: today's head, read live.
        SELECT
            count(*) FILTER (WHERE up) AS up_checks,
            count(*)                   AS total_checks,
            COALESCE(sum(total_ms) FILTER (WHERE up AND total_ms <> 0), 0) AS sum_latency_ms,
            count(*) FILTER (WHERE up AND total_ms <> 0) AS latency_samples
        FROM site_uptime_probes
        WHERE site_id = s.id AND tenant_id = $1
          AND probed_at >= $8::timestamptz AND probed_at <= $9::timestamptz
    ) parts
) agg ON true`

// fleetUptimeParams computes the nine bound parameters fleetUptimeQuery needs
// from the caller's window, entirely in Go from now.UTC() — never a
// server-side now()/CURRENT_DATE and never an implicit TZ-dependent cast (see
// fleetUptimeQuery's doc comment). Exported as its own function so the
// decomposition math is independently unit-testable without a database.
func fleetUptimeParams(now time.Time, window time.Duration) (boundaryDay, today, tailLower, tailUpper, retentionCutoff, todayLower, nowParam time.Time) {
	nowUTC := now.UTC()
	today = nowUTC.Truncate(24 * time.Hour)
	tailLower = nowUTC.Add(-window)
	boundaryDay = tailLower.Truncate(24 * time.Hour)
	boundaryDayNext := boundaryDay.Add(24 * time.Hour)

	tailUpper = boundaryDayNext
	if today.Before(tailUpper) {
		tailUpper = today
	}
	// Degenerate guard: when boundaryDay == today (the whole window falls
	// within today, e.g. a sub-day window), today's midnight can be BEFORE
	// tailLower (which sits somewhere later inside today), which would
	// otherwise make tailUpper < tailLower — an inverted range. SQL treats an
	// inverted "probed_at >= tailLower AND probed_at < tailUpper" as simply
	// empty (never an error), so this clamp is defense-in-depth for
	// readability/invariants rather than a query-correctness fix — but it
	// keeps the Go-level invariant tailLower <= tailUpper <= todayLower <= now
	// true unconditionally, which every caller (and the decomposition tests)
	// can rely on.
	if tailUpper.Before(tailLower) {
		tailUpper = tailLower
	}
	todayLower = tailLower
	if today.After(todayLower) {
		todayLower = today
	}
	retentionCutoff = nowUTC.Add(-probeRetention)
	return boundaryDay, today, tailLower, tailUpper, retentionCutoff, todayLower, nowUTC
}

// QueryFleetUptime returns a batch uptime aggregate for many sites in one
// Postgres query using unnest as the anchor so sites with zero probes are
// absent from the result map (caller treats missing == no data). Runs under
// InAgentTx (same as QueryLatest/QueryAggregate) with an explicit tenant_id
// predicate for defense-in-depth and index coverage.
//
// m99: the hybrid decomposition (see fleetUptimeQuery's doc comment) reads
// the full site_uptime_probes table only for a ~2-day bounded slice per site
// (the two window edges) plus the small site_uptime_daily rollup for
// everything in between — never the old O(days-in-window * probes/day) full
// scan — while staying numerically EXACT to the old rolling
// probed_at >= now-window aggregate.
func (s *pgStore) QueryFleetUptime(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID, window time.Duration) (map[uuid.UUID]FleetUptimeRow, error) {
	if len(siteIDs) == 0 {
		return map[uuid.UUID]FleetUptimeRow{}, nil
	}
	boundaryDay, today, tailLower, tailUpper, retentionCutoff, todayLower, now := fleetUptimeParams(time.Now(), window)
	out := make(map[uuid.UUID]FleetUptimeRow, len(siteIDs))

	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, fleetUptimeQuery,
			tenantID, siteIDs, boundaryDay, today, tailLower, tailUpper, retentionCutoff, todayLower, now)
		if err != nil {
			return fmt.Errorf("postgres fleet uptime query: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var (
				siteID         uuid.UUID
				latestUp       *bool
				latestAt       *time.Time
				latestTLS      *time.Time
				upChecks       int64
				totalChecks    int64
				sumLatencyMs   float64
				latencySamples int64
			)
			if err := rows.Scan(&siteID, &latestUp, &latestAt, &latestTLS, &upChecks, &totalChecks, &sumLatencyMs, &latencySamples); err != nil {
				return fmt.Errorf("postgres fleet uptime scan: %w", err)
			}
			// Site has no status row at all (never probed, or its only
			// history has aged past retention) — omit from map, same
			// contract the old raw-probe query had.
			if latestUp == nil {
				continue
			}
			row := FleetUptimeRow{
				Up:          latestUp,
				LastProbeAt: latestAt,
				TLSExpiry:   latestTLS,
			}
			if totalChecks > 0 {
				pct := float64(upChecks) / float64(totalChecks) * 100
				row.UptimePct7d = &pct
			}
			if latencySamples > 0 {
				avg := sumLatencyMs / float64(latencySamples)
				row.AvgLatencyMs = &avg
			}
			out[siteID] = row
		}
		return rows.Err()
	})
	return out, err
}

// QueryProbeWindow returns up to limitN raw probe rows for one site within
// [from, to], most-recent-first (tenant-scoped, InAgentTx with an explicit
// tenant_id predicate — same convention as every other pgStore method).
// Returns an empty, non-nil slice (no error) when the window has no rows —
// this is a plain SELECT, so zero matching rows is never a Postgres error;
// the incident-detail service layer relies on this for graceful degradation.
func (s *pgStore) QueryProbeWindow(ctx context.Context, tenantID, siteID uuid.UUID, from, to time.Time, limitN int) ([]ProbeSample, error) {
	if limitN <= 0 {
		limitN = 40
	}
	out := make([]ProbeSample, 0, limitN)
	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
SELECT probed_at, up, http_status, total_ms, error_text
FROM site_uptime_probes
WHERE tenant_id = $1 AND site_id = $2 AND probed_at BETWEEN $3 AND $4
ORDER BY probed_at DESC
LIMIT $5`, tenantID, siteID, from, to, limitN)
		if err != nil {
			return fmt.Errorf("postgres probe window query: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				probedAt   time.Time
				up         bool
				httpStatus int32
				totalMs    float64
				errText    string
			)
			if err := rows.Scan(&probedAt, &up, &httpStatus, &totalMs, &errText); err != nil {
				return fmt.Errorf("postgres probe window scan: %w", err)
			}
			out = append(out, ProbeSample{
				ProbedAt:   probedAt,
				Up:         up,
				HTTPStatus: uint16(httpStatus),
				TotalMs:    totalMs,
				Error:      errText,
			})
		}
		return rows.Err()
	})
	return out, err
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
