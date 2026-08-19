package rum

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// singleEventBucketCounts returns a NumBuckets-length int32 slice with a 1 at
// the bucket index for valueMilli and zeros elsewhere. Used by WriteEvent to
// build the minimal histogram row for the per-beacon upsert.
func singleEventBucketCounts(valueMilli int32) []int32 {
	counts := make([]int32, NumBuckets)
	counts[BucketForValue(valueMilli)] = 1
	return counts
}

// StorePostgres implements Store using the Postgres sqlc-generated queries.
// Ingest writes run under InRumIngestTx; rollup upserts run under InRumIngestTx
// (same GUC — the ingest policy covers both raw-event and rollup writes because
// both are INSERT-only paths for the browser-beacon write channel). Dashboard
// reads run under InTenantTx. GC runs under InAgentTx.
type StorePostgres struct {
	pool *db.Pool
}

// NewStorePostgres constructs a StorePostgres.
func NewStorePostgres(pool *db.Pool) *StorePostgres {
	return &StorePostgres{pool: pool}
}

// Compile-time assertion: StorePostgres implements Store.
var _ Store = (*StorePostgres)(nil)

// WriteEvent appends one validated RUM event and additively upserts the
// corresponding hourly and daily rollup rows, all within a single InRumIngestTx.
//
// The rollup rows are keyed by (tenant_id, site_id, url_pattern, metric, device,
// country, bucket_hour/bucket_day). The upsert increments sample_count by 1,
// adds 1 to the bucket_counts element for the CrUX bucket that contains
// p.ValueMilli (via BucketForValue), and accumulates sum/min/max. The
// ON CONFLICT DO UPDATE expressions in UpsertRumRollupHourly/Daily are additive,
// so concurrent per-beacon inserts converge correctly without any coordination.
//
// RLS: InRumIngestTx sets app.rum_ingest='on', app.tenant_id, and app.site_id.
// The rollup tables' _rum_ingest INSERT policies and _tenant_isolation
// USING+WITH CHECK policies cover both the INSERT and the ON CONFLICT DO UPDATE
// arm (the UPDATE arm inherits the session GUCs set at transaction start).
func (s *StorePostgres) WriteEvent(ctx context.Context, p IngestParams) error {
	now := time.Now().UTC()
	bucketHour := now.Truncate(time.Hour)

	bucketDay := pgtype.Date{}
	_ = bucketDay.Scan(now.Format("2006-01-02"))

	bucketCounts := singleEventBucketCounts(p.ValueMilli)

	return s.pool.InRumIngestTx(ctx, p.TenantID, p.SiteID, func(tx pgx.Tx) error {
		q := sqlc.New(tx)

		// 1. Append the raw event.
		if err := q.InsertRumEvent(ctx, sqlc.InsertRumEventParams{
			TenantID:   p.TenantID,
			SiteID:     p.SiteID,
			UrlPattern: p.URLPattern,
			Metric:     p.Metric,
			ValueMilli: p.ValueMilli,
			Device:     p.Device,
			Country:    p.Country,
			Conn:       p.Conn,
		}); err != nil {
			return err
		}

		// 2. Upsert the hourly rollup (additive ON CONFLICT DO UPDATE).
		if err := q.UpsertRumRollupHourly(ctx, sqlc.UpsertRumRollupHourlyParams{
			TenantID:     p.TenantID,
			SiteID:       p.SiteID,
			UrlPattern:   p.URLPattern,
			Metric:       p.Metric,
			Device:       p.Device,
			Country:      p.Country,
			BucketHour:   bucketHour,
			SampleCount:  1,
			SampleRate:   p.SampleRate,
			BucketCounts: bucketCounts,
			SumValue:     int64(p.ValueMilli),
			MinValue:     p.ValueMilli,
			MaxValue:     p.ValueMilli,
		}); err != nil {
			return err
		}

		// 3. Upsert the daily rollup (additive ON CONFLICT DO UPDATE).
		return q.UpsertRumRollupDaily(ctx, sqlc.UpsertRumRollupDailyParams{
			TenantID:     p.TenantID,
			SiteID:       p.SiteID,
			UrlPattern:   p.URLPattern,
			Metric:       p.Metric,
			Device:       p.Device,
			Country:      p.Country,
			BucketDay:    bucketDay,
			SampleCount:  1,
			SampleRate:   p.SampleRate,
			BucketCounts: bucketCounts,
			SumValue:     int64(p.ValueMilli),
			MinValue:     p.ValueMilli,
			MaxValue:     p.ValueMilli,
		})
	})
}

// UpsertRollupHourly writes one pre-computed hourly rollup row under InRumIngestTx.
// The rollup worker builds the histogram from rum_events_raw and calls this.
func (s *StorePostgres) UpsertRollupHourly(ctx context.Context, r HourlyRollup) error {
	return s.pool.InRumIngestTx(ctx, r.TenantID, r.SiteID, func(tx pgx.Tx) error {
		return sqlc.New(tx).UpsertRumRollupHourly(ctx, sqlc.UpsertRumRollupHourlyParams{
			TenantID:     r.TenantID,
			SiteID:       r.SiteID,
			UrlPattern:   r.URLPattern,
			Metric:       r.Metric,
			Device:       r.Device,
			Country:      r.Country,
			BucketHour:   r.BucketHour,
			SampleCount:  r.SampleCount,
			SampleRate:   r.SampleRate,
			BucketCounts: r.BucketCounts,
			SumValue:     r.SumValue,
			MinValue:     r.MinValue,
			MaxValue:     r.MaxValue,
		})
	})
}

// UpsertRollupDaily writes one pre-computed daily rollup row under InRumIngestTx.
func (s *StorePostgres) UpsertRollupDaily(ctx context.Context, r DailyRollup) error {
	bd := pgtype.Date{}
	_ = bd.Scan(r.BucketDay.Format("2006-01-02"))
	return s.pool.InRumIngestTx(ctx, r.TenantID, r.SiteID, func(tx pgx.Tx) error {
		return sqlc.New(tx).UpsertRumRollupDaily(ctx, sqlc.UpsertRumRollupDailyParams{
			TenantID:     r.TenantID,
			SiteID:       r.SiteID,
			UrlPattern:   r.URLPattern,
			Metric:       r.Metric,
			Device:       r.Device,
			Country:      r.Country,
			BucketDay:    bd,
			SampleCount:  r.SampleCount,
			SampleRate:   r.SampleRate,
			BucketCounts: r.BucketCounts,
			SumValue:     r.SumValue,
			MinValue:     r.MinValue,
			MaxValue:     r.MaxValue,
		})
	})
}

// GetHourlyRollups returns hourly rollup rows for the site since the given time.
// Runs under InTenantTx.
func (s *StorePostgres) GetHourlyRollups(ctx context.Context, siteID, tenantID uuid.UUID, since time.Time) ([]HourlyRollup, error) {
	var out []HourlyRollup
	err := s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).GetRumRollupHourly(ctx, sqlc.GetRumRollupHourlyParams{
			SiteID:   siteID,
			TenantID: tenantID,
			Since:    since,
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return nil
			}
			return qerr
		}
		out = make([]HourlyRollup, len(rows))
		for i, r := range rows {
			out[i] = HourlyRollup{
				RollupKey: RollupKey{
					SiteID:     r.SiteID,
					TenantID:   r.TenantID,
					URLPattern: r.UrlPattern,
					Metric:     r.Metric,
					Device:     r.Device,
					Country:    r.Country,
				},
				BucketHour:   r.BucketHour,
				SampleCount:  r.SampleCount,
				SampleRate:   r.SampleRate,
				BucketCounts: r.BucketCounts,
				SumValue:     r.SumValue,
				MinValue:     r.MinValue,
				MaxValue:     r.MaxValue,
			}
		}
		return nil
	})
	return out, err
}

// GetDailyRollups returns daily rollup rows for the site since the given date.
// Runs under InTenantTx.
func (s *StorePostgres) GetDailyRollups(ctx context.Context, siteID, tenantID uuid.UUID, sinceDay time.Time) ([]DailyRollup, error) {
	sinceDate := pgtype.Date{}
	_ = sinceDate.Scan(sinceDay.Format("2006-01-02"))

	var out []DailyRollup
	err := s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).GetRumRollupDaily(ctx, sqlc.GetRumRollupDailyParams{
			SiteID:   siteID,
			TenantID: tenantID,
			SinceDay: sinceDate,
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return nil
			}
			return qerr
		}
		out = make([]DailyRollup, len(rows))
		for i, r := range rows {
			var bd time.Time
			if r.BucketDay.Valid {
				bd = r.BucketDay.Time
			}
			out[i] = DailyRollup{
				RollupKey: RollupKey{
					SiteID:     r.SiteID,
					TenantID:   r.TenantID,
					URLPattern: r.UrlPattern,
					Metric:     r.Metric,
					Device:     r.Device,
					Country:    r.Country,
				},
				BucketDay:    bd,
				SampleCount:  r.SampleCount,
				SampleRate:   r.SampleRate,
				BucketCounts: r.BucketCounts,
				SumValue:     r.SumValue,
				MinValue:     r.MinValue,
				MaxValue:     r.MaxValue,
			}
		}
		return nil
	})
	return out, err
}

// GetHourlyRollupsForSites returns hourly rollup rows for a set of sites within
// a tenant since the given timestamp. Used by the fleet RUM aggregate endpoint
// to compute cross-site p75 without N+1 DB round-trips. Runs under InTenantTx.
func (s *StorePostgres) GetHourlyRollupsForSites(ctx context.Context, tenantID uuid.UUID, siteIDs []uuid.UUID, since time.Time) ([]HourlyRollup, error) {
	var out []HourlyRollup
	err := s.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qerr := sqlc.New(tx).GetRumRollupHourlyForSites(ctx, sqlc.GetRumRollupHourlyForSitesParams{
			TenantID: tenantID,
			SiteIds:  siteIDs,
			Since:    since,
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return nil
			}
			return qerr
		}
		out = make([]HourlyRollup, len(rows))
		for i, r := range rows {
			out[i] = HourlyRollup{
				RollupKey: RollupKey{
					SiteID:     r.SiteID,
					TenantID:   r.TenantID,
					URLPattern: r.UrlPattern,
					Metric:     r.Metric,
					Device:     r.Device,
					Country:    r.Country,
				},
				BucketHour:   r.BucketHour,
				SampleCount:  r.SampleCount,
				SampleRate:   r.SampleRate,
				BucketCounts: r.BucketCounts,
				SumValue:     r.SumValue,
				MinValue:     r.MinValue,
				MaxValue:     r.MaxValue,
			}
		}
		return nil
	})
	return out, err
}

// ComputeP75 interpolates the 75th percentile from a slice of hourly rollup rows.
// The algorithm:
//  1. Group rows by (metric, device, country).
//  2. For each group, element-wise sum the bucket_counts arrays across all hours.
//  3. Locate the bucket containing the 75th-percentile sample using cumulative counts.
//  4. Linearly interpolate within that bucket to produce a millisecond estimate.
//
// Groups with SampleCount < minSampleCount are suppressed (P75Milli == 0).
// ComputeP75 makes no DB calls — it is pure computation on the in-memory rows.
func (s *StorePostgres) ComputeP75(rollups []HourlyRollup, minSampleCount int) []P75Result {
	return computeP75(rollups, minSampleCount)
}

// PruneRawEvents deletes raw RUM events older than cutoff. Runs under InAgentTx.
func (s *StorePostgres) PruneRawEvents(ctx context.Context, cutoff time.Time) (int64, error) {
	var n int64
	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		count, qerr := sqlc.New(tx).DeleteOldRumEvents(ctx, cutoff)
		if qerr != nil {
			return qerr
		}
		n = count
		return nil
	})
	return n, err
}

// PruneHourlyRollups deletes hourly rollup rows older than cutoff. Runs under InAgentTx.
func (s *StorePostgres) PruneHourlyRollups(ctx context.Context, cutoff time.Time) (int64, error) {
	var n int64
	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		count, qerr := sqlc.New(tx).DeleteOldRumHourlyRollups(ctx, cutoff)
		if qerr != nil {
			return qerr
		}
		n = count
		return nil
	})
	return n, err
}

// PruneDailyRollups deletes daily rollup rows older than sinceDay. Runs under InAgentTx.
func (s *StorePostgres) PruneDailyRollups(ctx context.Context, sinceDay time.Time) (int64, error) {
	sinceDate := pgtype.Date{}
	_ = sinceDate.Scan(sinceDay.Format("2006-01-02"))

	var n int64
	err := s.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		count, qerr := sqlc.New(tx).DeleteOldRumDailyRollups(ctx, sinceDate)
		if qerr != nil {
			return qerr
		}
		n = count
		return nil
	})
	return n, err
}
