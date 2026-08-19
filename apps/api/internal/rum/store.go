// Package rum is the Real User Monitoring (RUM) control-plane domain (M56).
// It provides:
//
//   - RumStore: the Postgres-backed store interface (StorePostgres) for
//     ingest-write, rollup reads, and dashboard p75 queries.
//   - Beacon-key lifecycle: 128-bit random key generation, sha256 storage, and
//     grace-window rotation (SetBeaconKeyHash / ClearBeaconKeyHashPrev).
//   - URL normalization: strips query strings and templates dynamic path segments
//     so distinct page variants collapse to a single pattern for rollup.
//   - Cardinality control: country top-N cap (default MaxDistinctCountries) with
//     the remainder bucketed as "__other__".
//   - p75 histogram interpolation: computed at read time across the
//     CrUX-anchored integer bucket boundaries; no tdigest extension required.
package rum

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// CrUXBuckets are the bucket upper boundaries (exclusive) in milliseconds for
// all Web Vitals metrics. These match the Chrome UX Report histogram breakpoints
// so that externally-derived p75 values can be compared directly.
//
// The bucket layout is:
//
//	[0, b0), [b0, b1), …, [b_{n-2}, b_{n-1}), [b_{n-1}, +∞)
//
// There are len(CrUXBuckets)+1 buckets in total. bucket_counts slices have
// exactly NumBuckets elements.
var CrUXBuckets = []int32{
	// LCP/FCP/TTFB: coarser below 10 s, then an open-ended final bin.
	200, 300, 400, 500, 600, 800, 1000, 1200, 1400, 1600,
	1800, 2000, 2500, 3000, 3500, 4000, 4500, 5000, 6000, 7000,
	8000, 9000, 10000,
}

// NumBuckets is the number of histogram buckets (one more than the 23-boundary
// count, for the open-ended final bin). Must match len(CrUXBuckets)+1.
const NumBuckets = 24 // len(CrUXBuckets) + 1

// IngestParams carries a validated, normalised RUM event ready for storage.
// All fields come from the ingest handler after beacon-key resolution, sampling,
// URL normalization, and country cardinality checking.
type IngestParams struct {
	TenantID   uuid.UUID
	SiteID     uuid.UUID
	URLPattern string
	Metric     string  // lcp | inp | cls | ttfb | fcp
	ValueMilli int32
	Device     string  // desktop | mobile | tablet
	Country    string  // ISO-3166-1 alpha-2 or "__other__"
	Conn       string  // 4g | 3g | 2g | slow-2g | offline | unknown
	SampleRate float32 // fraction in [0,1] from site config; stored on rollup rows
}

// BucketForValue returns the CrUX histogram bucket index (0-based) for the
// given metric value in milli-units. The bucket index is the position in the
// NumBuckets-wide bucket_counts array that the value falls into:
//
//	bucket 0  : [0,         CrUXBuckets[0])
//	bucket i  : [CrUXBuckets[i-1], CrUXBuckets[i])
//	bucket N-1: [CrUXBuckets[N-2], +∞)            (open-ended last bin)
//
// Exported so tests in other packages (e.g. handler_test) can verify the
// mapping without duplicating the boundary slice.
func BucketForValue(valueMilli int32) int {
	for i, boundary := range CrUXBuckets {
		if valueMilli < boundary {
			return i
		}
	}
	return NumBuckets - 1 // open-ended last bin
}

// RollupKey identifies one rollup cell (the PRIMARY KEY columns minus the time
// dimension).
type RollupKey struct {
	SiteID     uuid.UUID
	TenantID   uuid.UUID
	URLPattern string
	Metric     string
	Device     string
	Country    string
}

// HourlyRollup is one aggregated hourly row returned by GetHourlyRollups.
type HourlyRollup struct {
	RollupKey
	BucketHour   time.Time
	SampleCount  int64
	SampleRate   float32
	BucketCounts []int32 // len == NumBuckets
	SumValue     int64
	MinValue     int32
	MaxValue     int32
}

// DailyRollup is one aggregated daily row returned by GetDailyRollups.
type DailyRollup struct {
	RollupKey
	BucketDay    time.Time
	SampleCount  int64
	SampleRate   float32
	BucketCounts []int32 // len == NumBuckets
	SumValue     int64
	MinValue     int32
	MaxValue     int32
}

// P75Result is the p75 estimate for one (metric, device, country) combination,
// computed by linear interpolation across the histogram bucket boundaries.
type P75Result struct {
	Metric  string
	Device  string
	Country string
	// P75Milli is the interpolated 75th-percentile value in milliseconds.
	// Zero when SampleCount < MinSampleCount (result suppressed).
	P75Milli float64
	// SampleCount is the raw (pre-scale) sample count summed from the rollup rows.
	SampleCount int64
}

// Store is the RUM data backend interface. StorePostgres is the sole
// implementation, backed by the sqlc-generated histogram rollups.
//
// The interface is intentionally narrow: the ingest handler needs WriteEvent;
// the dashboard handler needs GetHourlyRollups / GetDailyRollups + ComputeP75.
//
// The three Prune* methods below survived the FoldHourly/FoldDaily removal
// in this same PR because RumGCWorker.Work (worker.go) actually calls all
// three on every periodic sweep registered in cmd/wpmgr/main.go, whereas
// FoldHourly/FoldDaily had zero callers anywhere in the tree.
type Store interface {
	// WriteEvent appends one validated event and additively upserts the
	// corresponding hourly and daily rollup rows, all within a single
	// InRumIngestTx. The rollup upsert increments sample_count by 1, adds 1
	// to the bucket_counts element at BucketForValue(p.ValueMilli), and
	// accumulates sum/min/max. This is the per-beacon rollup path (V1 mechanism);
	// the RumRollupWorker is reserved for a future batch-fold optimisation.
	WriteEvent(ctx context.Context, p IngestParams) error

	// GetHourlyRollups returns hourly rollup rows for the site since the given
	// timestamp. Runs under InTenantTx (dashboard read path).
	GetHourlyRollups(ctx context.Context, siteID, tenantID uuid.UUID, since time.Time) ([]HourlyRollup, error)

	// GetDailyRollups returns daily rollup rows for the site since the given date.
	// Runs under InTenantTx (dashboard read path).
	GetDailyRollups(ctx context.Context, siteID, tenantID uuid.UUID, sinceDay time.Time) ([]DailyRollup, error)

	// ComputeP75 interpolates the 75th percentile from a slice of hourly or daily
	// rollup rows. Returns one P75Result per (metric, device, country) combination.
	// Rows whose SampleCount < minSampleCount are suppressed (P75Milli == 0).
	// This method is pure computation; it makes no DB calls.
	ComputeP75(rollups []HourlyRollup, minSampleCount int) []P75Result

	// PruneRawEvents deletes raw events older than cutoff. Returns the count of
	// deleted rows. Runs under InAgentTx (cross-tenant GC). Capped at 5000 rows
	// per call to keep transactions short.
	PruneRawEvents(ctx context.Context, cutoff time.Time) (int64, error)

	// PruneHourlyRollups deletes hourly rollup rows older than cutoff.
	PruneHourlyRollups(ctx context.Context, cutoff time.Time) (int64, error)

	// PruneDailyRollups deletes daily rollup rows older than sinceDay.
	PruneDailyRollups(ctx context.Context, sinceDay time.Time) (int64, error)
}
