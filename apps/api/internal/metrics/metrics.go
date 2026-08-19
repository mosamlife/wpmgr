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
	"strings"
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
	// AppUp is the GH #291 Phase 2 application-health verdict (three-valued:
	// true / false / nil=unknown), piggybacked onto whichever reachability
	// Check this sweep tick also attempted an app probe for - see
	// uptime.ProbeWorker.Sweep / appProbeDue. nil means "no app probe was
	// attempted on THIS check" (the common case: the app probe runs on a
	// slower cadence than the reachability probe), which is a DIFFERENT
	// meaning than "attempted, verdict unknown" - see AppProbeReason, which
	// disambiguates the two. Orthogonal to Up: never derived from it, never
	// feeds site_uptime_probes.up / site_uptime_daily's up_checks /
	// total_checks / site_uptime_status.latest_up.
	AppUp *bool
	// AppProbeReason is the machine-readable reason for AppUp's verdict (one
	// of the AppProbeReason* constants in package uptime - this package does
	// not import that one, to avoid a cross-domain dependency for a handful
	// of string constants). Empty means "no app probe was attempted on this
	// check" - the sentinel every write path (InsertChecks, UpsertRollup)
	// uses to tell that apart from "attempted, verdict unknown" (a non-empty
	// reason with a nil AppUp).
	AppProbeReason string
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

// ProbeSample is one raw probe result returned by QueryProbeWindow — the
// per-incident probe timeline consumed by the fleet incident-detail endpoint
// (GH #148 part 1).
type ProbeSample struct {
	ProbedAt   time.Time
	Up         bool
	HTTPStatus uint16
	TotalMs    float64
	Error      string
}

// FleetUptimeRow is the per-site aggregate returned by QueryFleetUptime.
// All pointer fields are nil when the site has no probe data in the window.
type FleetUptimeRow struct {
	// Up is the result of the most-recent probe (nil = never probed).
	Up *bool
	// LastProbeAt is the timestamp of the most-recent probe (nil = never probed).
	LastProbeAt *time.Time
	// UptimePct7d is the 7-day uptime percentage (nil = no data).
	UptimePct7d *float64
	// AvgLatencyMs is the 7-day average total_ms over successful probes (nil = no data).
	AvgLatencyMs *float64
	// TLSExpiry is the cert NotAfter from the most-recent probe (nil = non-HTTPS or no probes).
	TLSExpiry *time.Time
	// AppUp is the GH #291 Phase 2 application-health verdict from the most
	// recent app probe: true, false, or nil (never probed, or the most
	// recent probe was inconclusive - see AppProbeReason). Independent of Up:
	// a cached 200 (Up=true) can coexist with AppUp=false when a page cache
	// is masking a dead PHP backend - the literal GH #291 incident.
	AppUp *bool
	// AppProbeReason is the machine-readable reason for AppUp's most recent
	// verdict (one of package uptime's AppProbeReason* constants). Empty
	// when no app probe has run yet.
	AppProbeReason string
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
	// QueryFleetUptime returns a per-site uptime aggregate for a batch of sites in
	// a single query. Missing sites (no probe data) are absent from the map.
	// Always scoped to tenantID; siteIDs must belong to that tenant (the caller
	// verifies ownership in Postgres before reaching this).
	QueryFleetUptime(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID, window time.Duration) (map[uuid.UUID]FleetUptimeRow, error)
	// QueryFleetDailySeries returns one Point per (site, UTC day) for a batch
	// of sites in a SINGLE query — the fleet-wide daily availability strip
	// (GH #460). Points for one site are ordered oldest-first.
	//
	// A site with no probes in the window is ABSENT from the map, and a day
	// with no probes produces NO point, exactly like QuerySeries. Neither is
	// zero-filled: "we did not measure" and "we measured zero" are different
	// facts about a site, and a strip that renders a gap as 0% tells an
	// operator their site was down when we simply never looked. Callers
	// densify the requested window into one entry per UTC day and mark the
	// absent ones as no-data.
	//
	// Batching is part of the contract, not an implementation detail: this
	// exists so the fleet strip is one round trip. A caller that loops it per
	// site reintroduces the pre-m99 per-site scan the rollup was built to
	// remove.
	//
	// Always scoped to tenantID; siteIDs must belong to that tenant (the
	// caller verifies ownership in Postgres before reaching this).
	QueryFleetDailySeries(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID, window time.Duration) (map[uuid.UUID][]Point, error)
	// QueryProbeWindow returns up to limitN raw probe rows for one site within
	// [from, to], most-recent-first — the probe timeline for the fleet
	// incident-detail endpoint (GH #148 part 1). GRACEFUL DEGRADATION:
	// returns an empty, non-nil slice (never an error) when the window has no
	// data — retention-aged rows, a disabled ClickHouse backend, or a site
	// with no probes in range — so the incident summary can still render
	// without a timeline. Always scoped to tenantID; the caller verifies site
	// ownership in Postgres before reaching this.
	QueryProbeWindow(ctx context.Context, tenantID, siteID uuid.UUID, from, to time.Time, limitN int) ([]ProbeSample, error)
}

// RollupWriter is an OPTIONAL capability implemented by Store backends that
// maintain a pre-aggregated per-site uptime rollup for QueryFleetUptime (m99
// — see site_uptime_daily / site_uptime_status in db/schema.sql). Only the
// Postgres backend (pgStore) implements it today: ClickHouse deployments
// already read fleet uptime from ClickHouse's own aggregation and never scan
// site_uptime_probes, so they have no need for the rollup.
//
// It is deliberately NOT part of the Store interface: adding it there would
// force every Store implementation (chStore, and every test double across
// internal/site and internal/uptime) to implement a method that only one
// backend needs. Callers (uptime.ProbeWorker.Sweep) type-assert instead —
// see that file for the call site — so unrelated backends/fakes are
// unaffected.
type RollupWriter interface {
	// UpsertRollup folds one sweep's probe results into site_uptime_daily
	// (additive per-day counters) and site_uptime_status (the latest-probe
	// snapshot, freshness-guarded against out-of-order writes). Best-effort
	// from the caller's perspective: a failure here must never prevent or
	// roll back the raw probe write (InsertChecks) that already committed —
	// callers should log and continue, never treat this as fatal to the
	// sweep. No-ops on empty input.
	UpsertRollup(ctx context.Context, checks []Check) error
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

// uptimeChecksColumn is one column of the uptime_checks table: its name and
// its ClickHouse type.
type uptimeChecksColumn struct {
	name string
	typ  string
}

// uptimeChecksColumns is the single declared source of truth for the
// uptime_checks table shape. Both ensureSchema (CREATE TABLE and the
// per-column ADD COLUMN IF NOT EXISTS convergence) and InsertChecks (the
// column-explicit INSERT column list and the matching Append order) are
// driven from this one list, so the DDL, the migration path and the insert
// path cannot drift apart. Adding a column here is the only step required to
// both create it on a fresh table and backfill it onto an existing one; see
// ensureSchema.
var uptimeChecksColumns = []uptimeChecksColumn{
	{"checked_at", "DateTime"},
	{"tenant_id", "UUID"},
	{"site_id", "UUID"},
	{"up", "UInt8"},
	{"http_status", "UInt16"},
	{"dns_ms", "Float64"},
	{"connect_ms", "Float64"},
	{"tls_ms", "Float64"},
	{"ttfb_ms", "Float64"},
	{"total_ms", "Float64"},
	{"tls_expiry", "DateTime"},
	{"error", "String"},
	// m107 (GH #291 Phase 2). This table has no Nullable column anywhere else
	// (tls_expiry uses the chTime epoch-0 sentinel instead), so app_up
	// follows that same convention rather than introducing the first
	// Nullable(...) column: Int8 with -1 = unknown/not-probed-this-row,
	// 1 = true, 0 = false. See chAppUp / the Check.AppUp doc comment for why
	// "not probed this row" and "probed, verdict unknown" are both -1 here -
	// unlike the Postgres side, ClickHouse's uptime_checks has no separate
	// rollup-status table where that distinction matters for a COALESCE
	// guard, so collapsing them is safe.
	{"app_up", "Int8"},
	{"app_probe_reason", "String"},
}

// chAppUp maps a Check.AppUp tri-state onto the Int8 sentinel scheme
// uptime_checks.app_up uses (see uptimeChecksColumns): -1 unknown/not
// probed, 1 true, 0 false.
func chAppUp(v *bool) int8 {
	if v == nil {
		return -1
	}
	if *v {
		return 1
	}
	return 0
}

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

// ensureSchema creates the database and the uptime_checks table if absent,
// then converges an already-existing table's COLUMN SET to the currently
// declared uptimeChecksColumns. The table is created as a MergeTree ordered
// by (tenant_id, site_id, checked_at), the exact prefix every tenant-scoped
// per-site query filters on, with a 90-day TTL on checked_at so old rows are
// reclaimed automatically, but that only applies to a fresh CREATE TABLE.
// ONLY the column set converges on every boot. The engine, the ORDER BY, the
// TTL clause, and the declared TYPE of an already-existing column do NOT
// converge. An operator who hand-alters uptime_checks (a different ORDER BY,
// a wider or narrower column type, TTL removed) will find that drift persists
// across restarts; this function never re-issues CREATE TABLE and never runs
// an ALTER ... MODIFY on an existing table, it only ADDs a COLUMN for a name
// it cannot find.
//
// CREATE TABLE IF NOT EXISTS is a no-op against a table that already exists,
// so a column appended to uptimeChecksColumns after a deployment's table was
// first created would never land there on its own: InsertChecks would then
// try to write more values than the physical table has columns and fail
// outright. To make new columns safe to add in a later phase, every boot
// reads the table's actual column names from system.columns ONCE and issues
// an idempotent ALTER TABLE ... ADD COLUMN IF NOT EXISTS only for declared
// columns that read is missing, so a table that has already converged costs
// one cheap read and zero DDL, instead of 12 unconditional round trips every
// boot. Both the CREATE TABLE column list and the convergence loop are driven
// from the same uptimeChecksColumns list, so the create path and the
// migration path cannot drift apart.
//
// Every ALTER in the convergence loop is NON-FATAL. An older ClickHouse that
// rejects this ALTER syntax, or a role without ALTER privilege on this table,
// must not prevent the control plane from starting over an uptime-metrics
// nicety. This follows the same precedent as the vulnerability feed, which
// degrades to a clean no-op instead of blocking boot when it cannot run (see
// internal/admin/vuln_feed.go). Failures are logged at error level with the
// column name so the gap is loud rather than silent, and if any declared
// column is still missing once the loop finishes, one summary error names all
// of them: InsertChecks will fail on this table until that is resolved, and
// that must be visible in the logs even though it did not stop boot. If the
// system.columns read itself fails (e.g. no SELECT grant on system tables),
// convergence falls back to attempting every declared column unconditionally,
// same as before this fix, still non-fatal, just without the zero-DDL fast
// path for a table that was already converged.
func (s *chStore) ensureSchema(ctx context.Context) error {
	if err := s.conn.Exec(ctx, fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", s.db)); err != nil {
		return fmt.Errorf("clickhouse create database: %w", err)
	}

	columnDefs := make([]string, len(uptimeChecksColumns))
	for i, c := range uptimeChecksColumns {
		columnDefs[i] = fmt.Sprintf("    %s %s", c.name, c.typ)
	}
	ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.uptime_checks (
%s
) ENGINE = MergeTree()
ORDER BY (tenant_id, site_id, checked_at)
TTL checked_at + INTERVAL %d DAY`, s.db, strings.Join(columnDefs, ",\n"), retentionDays)
	if err := s.conn.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("clickhouse create uptime_checks: %w", err)
	}

	existing, err := s.existingUptimeChecksColumns(ctx)
	if err != nil {
		s.logger.Error("clickhouse read existing uptime_checks columns failed; falling back to attempting every declared column",
			slog.String("clickhouse_db", s.db), slog.Any("error", err))
		existing = map[string]bool{}
	}

	var stillMissing []string
	for _, c := range uptimeChecksColumns {
		if existing[c.name] {
			continue
		}
		alter := fmt.Sprintf("ALTER TABLE %s.uptime_checks ADD COLUMN IF NOT EXISTS %s %s", s.db, c.name, c.typ)
		if err := s.conn.Exec(ctx, alter); err != nil {
			s.logger.Error("clickhouse add column failed; boot continues, but this column will not be available until resolved",
				slog.String("clickhouse_db", s.db), slog.String("column", c.name), slog.Any("error", err))
			stillMissing = append(stillMissing, c.name)
		}
	}
	if len(stillMissing) > 0 {
		s.logger.Error("uptime_checks is missing columns InsertChecks requires; probe inserts will fail until this is resolved",
			slog.String("clickhouse_db", s.db), slog.Any("missing_columns", stillMissing))
	}
	return nil
}

// existingUptimeChecksColumns reads the actual column names of
// <db>.uptime_checks from system.columns, so ensureSchema's convergence loop
// can skip ADD COLUMN for names that already exist instead of issuing all 12
// unconditionally on every boot.
func (s *chStore) existingUptimeChecksColumns(ctx context.Context) (map[string]bool, error) {
	rows, err := s.conn.Query(ctx,
		"SELECT name FROM system.columns WHERE database = ? AND table = ?",
		s.db, "uptime_checks")
	if err != nil {
		return nil, fmt.Errorf("clickhouse read system.columns: %w", err)
	}
	defer rows.Close()

	out := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("clickhouse scan system.columns row: %w", err)
		}
		out[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse iterate system.columns: %w", err)
	}
	return out, nil
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
//
// The INSERT statement names every column explicitly (from
// uptimeChecksColumns) instead of relying on the table's physical column
// order. The ClickHouse driver resorts the batch to the named column order,
// so batch.Append below must supply values in that exact order too; a
// positional-only insert is what turns a schema drift (an ALTER that adds or
// reorders a column) into a silent value-shift or a column-count mismatch.
func (s *chStore) InsertChecks(ctx context.Context, checks []Check) error {
	if !s.Enabled() || len(checks) == 0 {
		return nil
	}
	columnNames := make([]string, len(uptimeChecksColumns))
	for i, c := range uptimeChecksColumns {
		columnNames[i] = c.name
	}
	insertSQL := fmt.Sprintf("INSERT INTO %s.uptime_checks (%s)", s.db, strings.Join(columnNames, ", "))
	batch, err := s.conn.PrepareBatch(ctx, insertSQL)
	if err != nil {
		return fmt.Errorf("clickhouse prepare batch: %w", err)
	}
	for _, c := range checks {
		up := uint8(0)
		if c.Up {
			up = 1
		}
		// Append order must match uptimeChecksColumns / columnNames above
		// exactly: checked_at, tenant_id, site_id, up, http_status, dns_ms,
		// connect_ms, tls_ms, ttfb_ms, total_ms, tls_expiry, error, app_up,
		// app_probe_reason.
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
			chAppUp(c.AppUp),
			c.AppProbeReason,
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

// QueryFleetUptime returns a batch uptime aggregate for many sites in one
// ClickHouse query. Uses argMax to pick the latest probe per site (up,
// checked_at, tls_expiry) within the same scan that computes the 7d aggregate,
// so the whole fleet resolves in a single round-trip. Returns a zero map (no
// error) when the store is disabled.
func (s *chStore) QueryFleetUptime(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID, window time.Duration) (map[uuid.UUID]FleetUptimeRow, error) {
	if !s.Enabled() || len(siteIDs) == 0 {
		return map[uuid.UUID]FleetUptimeRow{}, nil
	}
	since := time.Now().Add(-window)
	// GH #291 Phase 2: the "latest app verdict" is joined from a SEPARATE
	// subquery, pre-filtered to app_probe_reason != '' (a real app-probe
	// attempt), rather than argMax'd in the same GROUP BY as the
	// reachability aggregate above. The app probe runs on a slower cadence
	// than the reachability probe this table's grain follows, so most rows
	// in the window never attempted one - an unfiltered
	// argMax(app_up, checked_at) would almost always resolve to the
	// "not probed this row" sentinel from the globally-latest row even when
	// a real verdict exists a few rows earlier. Both app_up and
	// app_probe_reason are explicitly cast toNullable(...) so an unmatched
	// site (never app-probed) reads back as a real SQL NULL from the LEFT
	// JOIN regardless of the server's join_use_nulls setting, which a plain
	// (non-Nullable) column's join-fill behavior does not guarantee.
	rows, err := s.conn.Query(ctx, fmt.Sprintf(`
SELECT
    r.site_id,
    r.latest_up, r.latest_at, r.latest_tls, r.checks, r.up_checks, r.avg_latency,
    a.latest_app_up, a.latest_app_reason
FROM (
    SELECT
        site_id,
        argMax(up,        checked_at) AS latest_up,
        max(checked_at)               AS latest_at,
        argMax(tls_expiry, checked_at) AS latest_tls,
        count()                        AS checks,
        sum(up)                        AS up_checks,
        avgOrNullIf(total_ms, up = 1)  AS avg_latency
    FROM %[1]s.uptime_checks
    WHERE tenant_id = ?
      AND site_id IN ?
      AND checked_at >= ?
    GROUP BY site_id
) r
LEFT JOIN (
    SELECT
        site_id,
        toNullable(argMax(app_up,           checked_at)) AS latest_app_up,
        toNullable(argMax(app_probe_reason, checked_at)) AS latest_app_reason
    FROM %[1]s.uptime_checks
    WHERE tenant_id = ?
      AND site_id IN ?
      AND checked_at >= ?
      AND app_probe_reason != ''
    GROUP BY site_id
) a ON a.site_id = r.site_id`, s.db),
		tenantID, siteIDs, chTime(since), tenantID, siteIDs, chTime(since))
	if err != nil {
		return nil, fmt.Errorf("clickhouse fleet uptime query: %w", err)
	}
	defer rows.Close()

	out := make(map[uuid.UUID]FleetUptimeRow, len(siteIDs))
	for rows.Next() {
		var (
			siteID          uuid.UUID
			latestUpU       uint8
			latestAt        time.Time
			latestTLS       time.Time
			checks          uint64
			upChecks        uint64
			avgLatency      *float64
			latestAppUp     *int8
			latestAppReason *string
		)
		if err := rows.Scan(&siteID, &latestUpU, &latestAt, &latestTLS, &checks, &upChecks, &avgLatency, &latestAppUp, &latestAppReason); err != nil {
			return nil, fmt.Errorf("clickhouse fleet uptime scan: %w", err)
		}
		row := FleetUptimeRow{}
		up := latestUpU == 1
		row.Up = &up
		if !latestAt.IsZero() {
			t := latestAt
			row.LastProbeAt = &t
		}
		if latestTLS.Unix() > 0 {
			t := latestTLS
			row.TLSExpiry = &t
		}
		if checks > 0 {
			pct := float64(upChecks) / float64(checks) * 100
			row.UptimePct7d = &pct
		}
		if avgLatency != nil {
			row.AvgLatencyMs = avgLatency
		}
		if latestAppReason != nil && *latestAppReason != "" {
			row.AppProbeReason = *latestAppReason
			if latestAppUp != nil && *latestAppUp != -1 {
				b := *latestAppUp == 1
				row.AppUp = &b
			}
		}
		out[siteID] = row
	}
	return out, rows.Err()
}

// QueryProbeWindow returns up to limitN raw probe rows for one site within
// [from, to], most-recent-first (tenant-scoped). Returns an empty slice (no
// error) when the store is disabled or the window has no data.
func (s *chStore) QueryProbeWindow(ctx context.Context, tenantID, siteID uuid.UUID, from, to time.Time, limitN int) ([]ProbeSample, error) {
	if !s.Enabled() {
		return []ProbeSample{}, nil
	}
	if limitN <= 0 {
		limitN = 40
	}
	rows, err := s.conn.Query(ctx, fmt.Sprintf(`
SELECT checked_at, up, http_status, total_ms, error
FROM %s.uptime_checks
WHERE tenant_id = ? AND site_id = ? AND checked_at BETWEEN ? AND ?
ORDER BY checked_at DESC
LIMIT ?`, s.db), tenantID, siteID, chTime(from), chTime(to), limitN)
	if err != nil {
		return nil, fmt.Errorf("clickhouse probe window query: %w", err)
	}
	defer rows.Close()

	out := make([]ProbeSample, 0, limitN)
	for rows.Next() {
		var (
			checkedAt  time.Time
			upU        uint8
			httpStatus uint16
			totalMs    float64
			errStr     string
		)
		if err := rows.Scan(&checkedAt, &upU, &httpStatus, &totalMs, &errStr); err != nil {
			return nil, fmt.Errorf("clickhouse probe window scan: %w", err)
		}
		out = append(out, ProbeSample{
			ProbedAt:   checkedAt,
			Up:         upU == 1,
			HTTPStatus: httpStatus,
			TotalMs:    totalMs,
			Error:      errStr,
		})
	}
	return out, rows.Err()
}

// QueryFleetDailySeries returns one Point per (site, UTC day) for a batch of
// sites in one query — the ClickHouse half of the fleet daily strip (GH #460).
//
// uptime_checks is a flat per-probe table with no rollup, so unlike the
// Postgres backend there is no three-part decomposition to reuse: a single
// GROUP BY over the window is already the cheapest correct read here, and it
// is exact for the same reason the raw edge days are exact on Postgres.
//
// toStartOfDay(checked_at, 'UTC') pins the bucket to UTC EXPLICITLY rather
// than inheriting the ClickHouse server's timezone, so a non-UTC deployment
// labels the same probe with the same day as the Postgres backend does. The
// existing chStore.QuerySeries uses interval buckets and the server default;
// this one must agree day-for-day with site_uptime_daily, so it cannot.
//
// Latency semantics match the Postgres daily path (mean over SUCCESSFUL
// probes with a non-zero reading), NOT chStore.QuerySeries's avg over every
// probe — a 5xx still records a real total_ms, and folding those into an
// availability strip's latency would make the two backends disagree.
//
// A site with no probes in the window is absent from the map; a day with no
// probes yields no point. Neither is zero-filled — see the Store interface.
func (s *chStore) QueryFleetDailySeries(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID, window time.Duration) (map[uuid.UUID][]Point, error) {
	out := make(map[uuid.UUID][]Point, len(siteIDs))
	if !s.Enabled() || len(siteIDs) == 0 {
		return out, nil
	}
	since := time.Now().UTC().Add(-window)
	rows, err := s.conn.Query(ctx, fmt.Sprintf(`
SELECT site_id,
       toStartOfDay(checked_at, 'UTC') AS bucket,
       count()                         AS checks,
       sum(up)                         AS up_checks,
       sumIf(total_ms, up = 1 AND total_ms != 0)   AS sum_latency_ms,
       countIf(up = 1 AND total_ms != 0)           AS latency_samples
FROM %s.uptime_checks
WHERE tenant_id = ? AND site_id IN (?) AND checked_at >= ?
GROUP BY site_id, bucket
ORDER BY site_id, bucket ASC`, s.db), tenantID, siteIDs, chTime(since))
	if err != nil {
		return nil, fmt.Errorf("clickhouse fleet daily series query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			siteID         uuid.UUID
			bucket         time.Time
			checks         uint64
			upChecks       uint64
			sumLatencyMs   float64
			latencySamples uint64
		)
		if err := rows.Scan(&siteID, &bucket, &checks, &upChecks, &sumLatencyMs, &latencySamples); err != nil {
			return nil, fmt.Errorf("clickhouse fleet daily series scan: %w", err)
		}
		p := Point{
			Bucket:   bucket.UTC(),
			Checks:   checks,
			UpChecks: upChecks,
		}
		if latencySamples > 0 {
			p.AvgLatencyMs = sumLatencyMs / float64(latencySamples)
		}
		out[siteID] = append(out[siteID], p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
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
