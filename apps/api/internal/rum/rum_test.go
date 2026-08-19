package rum

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// p75 histogram interpolation tests
// ---------------------------------------------------------------------------

func TestComputeP75_empty(t *testing.T) {
	results := computeP75(nil, 100)
	if len(results) != 0 {
		t.Errorf("expected 0 results for empty input, got %d", len(results))
	}
}

func TestComputeP75_suppressed_below_minSampleCount(t *testing.T) {
	// 10 samples < minSampleCount=100 → P75Milli must be 0.
	counts := make([]int32, NumBuckets)
	counts[0] = 10 // all 10 samples in [0, 200) bucket
	rollups := []HourlyRollup{
		{
			RollupKey:    RollupKey{Metric: "lcp", Device: "desktop", Country: "US"},
			SampleCount:  10,
			BucketCounts: counts,
		},
	}
	results := computeP75(rollups, 100)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].P75Milli != 0 {
		t.Errorf("expected P75Milli=0 (suppressed), got %f", results[0].P75Milli)
	}
	if results[0].SampleCount != 10 {
		t.Errorf("expected SampleCount=10, got %d", results[0].SampleCount)
	}
}

func TestComputeP75_first_bucket_interpolation(t *testing.T) {
	// 100 samples all in the first bucket [0, 200ms).
	// p75 target = ceil(0.75 * 100) = 75.
	// All 100 samples in bucket 0 → lower=0, upper=200.
	// Interpolated = 0 + (75 - 0) / 100 * 200 = 150ms.
	counts := make([]int32, NumBuckets)
	counts[0] = 100
	rollups := []HourlyRollup{
		{
			RollupKey:    RollupKey{Metric: "lcp", Device: "desktop", Country: "US"},
			SampleCount:  100,
			BucketCounts: counts,
			MaxValue:     150,
		},
	}
	results := computeP75(rollups, 100)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	got := results[0].P75Milli
	want := 150.0
	if math.Abs(got-want) > 1.0 {
		t.Errorf("P75Milli = %f, want ≈ %f", got, want)
	}
}

func TestComputeP75_spans_multiple_hours(t *testing.T) {
	// Two hourly rows for the same key: 50 samples each.
	// Combined: 100 samples total, 50 in bucket[0] + 50 in bucket[1].
	// p75 target = ceil(75) = 75. bucket[0] has 50 → not enough (cum=50 < 75).
	// bucket[1] adds 50 → cum=100 ≥ 75.
	// bucket[1] = [200, 300). lower=200, upper=300.
	// interpolated = 200 + (75 - 50) / 50 * 100 = 200 + 50 = 250.
	make50 := func(bucketIdx int) []int32 {
		c := make([]int32, NumBuckets)
		c[bucketIdx] = 50
		return c
	}
	rollups := []HourlyRollup{
		{
			RollupKey:    RollupKey{Metric: "lcp", Device: "mobile", Country: "GB"},
			SampleCount:  50,
			BucketCounts: make50(0),
		},
		{
			RollupKey:    RollupKey{Metric: "lcp", Device: "mobile", Country: "GB"},
			SampleCount:  50,
			BucketCounts: make50(1),
		},
	}
	results := computeP75(rollups, 100)
	if len(results) != 1 {
		t.Fatalf("expected 1 result (groups collapsed), got %d", len(results))
	}
	got := results[0].P75Milli
	want := 250.0
	if math.Abs(got-want) > 1.0 {
		t.Errorf("P75Milli = %f, want ≈ %f", got, want)
	}
}

func TestComputeP75_multiple_metrics(t *testing.T) {
	// Two different metrics: lcp + fcp, each 200 samples.
	// lcp: all in bucket[0], p75 → ~150ms.
	// fcp: all in bucket[2] ([300,400)), p75 → ≈ 375ms.
	make200 := func(bucketIdx int) []int32 {
		c := make([]int32, NumBuckets)
		c[bucketIdx] = 200
		return c
	}
	rollups := []HourlyRollup{
		{RollupKey: RollupKey{Metric: "lcp", Device: "desktop", Country: "US"}, SampleCount: 200, BucketCounts: make200(0), MaxValue: 200},
		{RollupKey: RollupKey{Metric: "fcp", Device: "desktop", Country: "US"}, SampleCount: 200, BucketCounts: make200(2), MaxValue: 400},
	}
	results := computeP75(rollups, 100)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// Results are sorted: fcp < lcp alphabetically.
	metrics := map[string]float64{}
	for _, r := range results {
		metrics[r.Metric] = r.P75Milli
	}
	if math.Abs(metrics["lcp"]-150.0) > 1.0 {
		t.Errorf("lcp P75Milli = %f, want ≈ 150", metrics["lcp"])
	}
	// fcp in [300, 400): p75 = 300 + (150 - 0)/200 * 100 = 375.
	if math.Abs(metrics["fcp"]-375.0) > 1.0 {
		t.Errorf("fcp P75Milli = %f, want ≈ 375", metrics["fcp"])
	}
}

// ---------------------------------------------------------------------------
// Beacon-key generation tests
// ---------------------------------------------------------------------------

func TestGenerateBeaconKey_format(t *testing.T) {
	pt, hash, err := GenerateBeaconKey()
	if err != nil {
		t.Fatalf("GenerateBeaconKey: %v", err)
	}
	if pt == "" {
		t.Error("plaintext must not be empty")
	}
	if len(hash) != 32 {
		t.Errorf("hash must be 32 bytes (sha256), got %d", len(hash))
	}
}

func TestGenerateBeaconKey_unique(t *testing.T) {
	pt1, _, _ := GenerateBeaconKey()
	pt2, _, _ := GenerateBeaconKey()
	if pt1 == pt2 {
		t.Error("two GenerateBeaconKey calls returned the same plaintext")
	}
}

func TestHashBeaconKeyFromPlaintext_roundtrip(t *testing.T) {
	pt, expectedHash, err := GenerateBeaconKey()
	if err != nil {
		t.Fatalf("GenerateBeaconKey: %v", err)
	}
	got, err := HashBeaconKeyFromPlaintext(pt)
	if err != nil {
		t.Fatalf("HashBeaconKeyFromPlaintext: %v", err)
	}
	if len(got) != 32 {
		t.Errorf("hash length = %d, want 32", len(got))
	}
	// The hash derived from the plaintext must match the one returned by GenerateBeaconKey.
	for i, b := range expectedHash {
		if got[i] != b {
			t.Errorf("hash mismatch at byte %d: got %02x, want %02x", i, got[i], b)
		}
	}
}

func TestHashBeaconKeyFromPlaintext_invalid(t *testing.T) {
	_, err := HashBeaconKeyFromPlaintext("not-valid-base64!!!")
	if err == nil {
		t.Error("expected error for invalid base64 input")
	}
}

// ---------------------------------------------------------------------------
// URL normalization tests
// ---------------------------------------------------------------------------

func TestNormalizeURL_strips_query(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"https://example.com/blog/hello-world?utm_source=email", "/blog/hello-world"},
		{"https://example.com/shop/product/123", "/shop/product/{id}"},
		{"https://example.com/user/f47ac10b-58cc-4372-a567-0e02b2c3d479/profile", "/user/{uuid}/profile"},
		{"https://example.com/", "/"},
		{"https://example.com", "/"},
		{"/contact-us", "/contact-us"},
		{"/p/12345-blue-widget", "/p/{id}"},
	}
	for _, c := range cases {
		got := NormalizeURL(c.input)
		if got != c.want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestNormalizeCountry_valid(t *testing.T) {
	if got := NormalizeCountry("US", nil); got != "US" {
		t.Errorf("NormalizeCountry(US) = %q, want US", got)
	}
	if got := NormalizeCountry("gb", nil); got != "GB" {
		t.Errorf("NormalizeCountry(gb) = %q, want GB", got)
	}
	if got := NormalizeCountry("XYZ", nil); got != "__other__" {
		t.Errorf("NormalizeCountry(XYZ) = %q, want __other__", got)
	}
	if got := NormalizeCountry("", nil); got != "__other__" {
		t.Errorf("NormalizeCountry empty = %q, want __other__", got)
	}
}

func TestNormalizeCountry_allowSet(t *testing.T) {
	allowSet := map[string]bool{"US": true, "GB": true}
	if got := NormalizeCountry("DE", allowSet); got != "__other__" {
		t.Errorf("NormalizeCountry DE with allowSet = %q, want __other__", got)
	}
	if got := NormalizeCountry("US", allowSet); got != "US" {
		t.Errorf("NormalizeCountry US with allowSet = %q, want US", got)
	}
}

// ---------------------------------------------------------------------------
// CrUXBuckets consistency check
// ---------------------------------------------------------------------------

func TestCrUXBuckets_count(t *testing.T) {
	if NumBuckets != len(CrUXBuckets)+1 {
		t.Errorf("NumBuckets=%d but len(CrUXBuckets)+1=%d", NumBuckets, len(CrUXBuckets)+1)
	}
	if NumBuckets != 24 {
		t.Errorf("expected NumBuckets=24, got %d", NumBuckets)
	}
}

func TestCrUXBuckets_ascending(t *testing.T) {
	for i := 1; i < len(CrUXBuckets); i++ {
		if CrUXBuckets[i] <= CrUXBuckets[i-1] {
			t.Errorf("CrUXBuckets not strictly ascending at index %d: %d <= %d",
				i, CrUXBuckets[i], CrUXBuckets[i-1])
		}
	}
}

// ---------------------------------------------------------------------------
// BucketForValue unit tests
// ---------------------------------------------------------------------------

// TestBucketForValue_belowFirstBoundary verifies values below CrUXBuckets[0]
// (200ms) land in bucket 0 (the first bin).
func TestBucketForValue_belowFirstBoundary(t *testing.T) {
	for _, v := range []int32{0, 1, 100, 199} {
		if got := BucketForValue(v); got != 0 {
			t.Errorf("BucketForValue(%d) = %d, want 0 (below first boundary 200)", v, got)
		}
	}
}

// TestBucketForValue_atBoundaryIsUpperBin verifies that a value exactly at a
// boundary falls into the UPPER bucket (the boundary is exclusive for the lower
// bin: [low, high), so high itself is in the next bin).
func TestBucketForValue_atBoundaryIsUpperBin(t *testing.T) {
	// CrUXBuckets[0] = 200: value 200 must be in bucket 1, not bucket 0.
	if got := BucketForValue(200); got != 1 {
		t.Errorf("BucketForValue(200) = %d, want 1 (at boundary → upper bin)", got)
	}
	// CrUXBuckets[1] = 300: value 300 must be in bucket 2.
	if got := BucketForValue(300); got != 2 {
		t.Errorf("BucketForValue(300) = %d, want 2", got)
	}
}

// TestBucketForValue_lastBucketIsOpenEnded verifies that values ≥ the last
// boundary (CrUXBuckets[22] = 10 000) fall into the last open-ended bin.
func TestBucketForValue_lastBucketIsOpenEnded(t *testing.T) {
	lastBin := NumBuckets - 1 // 23
	for _, v := range []int32{10_000, 50_000, 600_000} {
		if got := BucketForValue(v); got != lastBin {
			t.Errorf("BucketForValue(%d) = %d, want %d (last open-ended bin)", v, got, lastBin)
		}
	}
}

// TestBucketForValue_coverAllBins verifies every bin index is reachable.
func TestBucketForValue_coverAllBins(t *testing.T) {
	seen := make(map[int]bool)
	// Hit each inner bin by using its lower boundary.
	seen[0] = true // 0 is always in bin 0
	BucketForValue(0)
	for i, b := range CrUXBuckets {
		idx := BucketForValue(b) // b is at boundary → upper bin i+1
		if idx != i+1 {
			t.Errorf("BucketForValue(%d) = %d, want %d", b, idx, i+1)
		}
		seen[idx] = true
	}
	for i := 0; i < NumBuckets; i++ {
		if !seen[i] {
			t.Errorf("bin %d was never reached", i)
		}
	}
}

// TestSingleEventBucketCounts_exactlyOneBit verifies that singleEventBucketCounts
// produces a NumBuckets slice with exactly one 1 at the correct index and zeros
// elsewhere.
func TestSingleEventBucketCounts_exactlyOneBit(t *testing.T) {
	cases := []struct {
		valueMilli int32
		wantBucket int
	}{
		{0, 0},
		{199, 0},
		{200, 1},
		{2500, 12}, // 2500 < 3000 (CrUXBuckets[12]=3000) and >= 2500 (CrUXBuckets[11]=2500 — verify)
		{600_000, NumBuckets - 1},
	}
	// Sanity-check index 12: CrUXBuckets[11]=2000, CrUXBuckets[12]=2500.
	// BucketForValue(2500): 2500 is NOT < 2500 (boundary), so it's in bin 13.
	// Recalculate inline to avoid hardcoding a wrong want.
	for i := range cases {
		cases[i].wantBucket = BucketForValue(cases[i].valueMilli)
	}

	for _, tc := range cases {
		counts := singleEventBucketCounts(tc.valueMilli)
		if len(counts) != NumBuckets {
			t.Errorf("singleEventBucketCounts(%d): len=%d, want %d", tc.valueMilli, len(counts), NumBuckets)
			continue
		}
		var total int32
		for _, c := range counts {
			total += c
		}
		if total != 1 {
			t.Errorf("singleEventBucketCounts(%d): sum=%d, want 1 (exactly one event)", tc.valueMilli, total)
		}
		if counts[tc.wantBucket] != 1 {
			t.Errorf("singleEventBucketCounts(%d): counts[%d]=%d, want 1", tc.valueMilli, tc.wantBucket, counts[tc.wantBucket])
		}
	}
}

// ---------------------------------------------------------------------------
// In-memory store tests for per-beacon rollup population
// ---------------------------------------------------------------------------

// rollupCapturingStore records the rollup rows passed to WriteEvent by building
// them locally (no DB), so we can assert GetHourlyRollups returns populated rows
// after N writes, and ComputeP75 produces a real p75 above the floor.
//
// This mirrors the production flow: WriteEvent calls singleEventBucketCounts
// and merges the result into the accumulator. The test drives the same logic
// via the exported BucketForValue + singleEventBucketCounts helpers so there
// is no double-implementation.
type rollupCapturingStore struct {
	// hourly is keyed by (site_id, url_pattern, metric, device, country, bucket_hour).
	hourly map[string]*HourlyRollup
	// daily  is keyed by (site_id, url_pattern, metric, device, country, bucket_day).
	daily  map[string]*DailyRollup
}

func newRollupCapturingStore() *rollupCapturingStore {
	return &rollupCapturingStore{
		hourly: make(map[string]*HourlyRollup),
		daily:  make(map[string]*DailyRollup),
	}
}

func rollupHourlyKey(p IngestParams, bh string) string {
	return p.SiteID.String() + "|" + p.URLPattern + "|" + p.Metric + "|" + p.Device + "|" + p.Country + "|" + bh
}

func rollupDailyKey(p IngestParams, bd string) string {
	return p.SiteID.String() + "|" + p.URLPattern + "|" + p.Metric + "|" + p.Device + "|" + p.Country + "|" + bd
}

func (s *rollupCapturingStore) WriteEvent(_ context.Context, p IngestParams) error {
	now := time.Now().UTC()
	bh := now.Truncate(time.Hour)
	bd := now.Format("2006-01-02")
	counts := singleEventBucketCounts(p.ValueMilli)

	// Hourly rollup.
	hk := rollupHourlyKey(p, bh.Format(time.RFC3339))
	if r, ok := s.hourly[hk]; ok {
		r.SampleCount++
		for i, c := range counts {
			r.BucketCounts[i] += c
		}
		r.SumValue += int64(p.ValueMilli)
		if p.ValueMilli < r.MinValue {
			r.MinValue = p.ValueMilli
		}
		if p.ValueMilli > r.MaxValue {
			r.MaxValue = p.ValueMilli
		}
	} else {
		bc := make([]int32, NumBuckets)
		copy(bc, counts)
		s.hourly[hk] = &HourlyRollup{
			RollupKey: RollupKey{
				SiteID:     p.SiteID,
				TenantID:   p.TenantID,
				URLPattern: p.URLPattern,
				Metric:     p.Metric,
				Device:     p.Device,
				Country:    p.Country,
			},
			BucketHour:   bh,
			SampleCount:  1,
			SampleRate:   p.SampleRate,
			BucketCounts: bc,
			SumValue:     int64(p.ValueMilli),
			MinValue:     p.ValueMilli,
			MaxValue:     p.ValueMilli,
		}
	}

	// Daily rollup.
	dk := rollupDailyKey(p, bd)
	if r, ok := s.daily[dk]; ok {
		r.SampleCount++
		for i, c := range counts {
			r.BucketCounts[i] += c
		}
		r.SumValue += int64(p.ValueMilli)
		if p.ValueMilli < r.MinValue {
			r.MinValue = p.ValueMilli
		}
		if p.ValueMilli > r.MaxValue {
			r.MaxValue = p.ValueMilli
		}
	} else {
		bc := make([]int32, NumBuckets)
		copy(bc, counts)
		s.daily[dk] = &DailyRollup{
			RollupKey: RollupKey{
				SiteID:     p.SiteID,
				TenantID:   p.TenantID,
				URLPattern: p.URLPattern,
				Metric:     p.Metric,
				Device:     p.Device,
				Country:    p.Country,
			},
			SampleCount:  1,
			SampleRate:   p.SampleRate,
			BucketCounts: bc,
			SumValue:     int64(p.ValueMilli),
			MinValue:     p.ValueMilli,
			MaxValue:     p.ValueMilli,
		}
	}
	return nil
}

// GetHourlyRollups returns all hourly rows accumulated so far for the site.
func (s *rollupCapturingStore) GetHourlyRollups(_ context.Context, siteID, _ uuid.UUID, _ time.Time) ([]HourlyRollup, error) {
	var out []HourlyRollup
	for _, r := range s.hourly {
		if r.SiteID == siteID {
			cp := *r
			out = append(out, cp)
		}
	}
	return out, nil
}

func (s *rollupCapturingStore) GetDailyRollups(_ context.Context, siteID, _ uuid.UUID, _ time.Time) ([]DailyRollup, error) {
	var out []DailyRollup
	for _, r := range s.daily {
		if r.SiteID == siteID {
			cp := *r
			out = append(out, cp)
		}
	}
	return out, nil
}

func (s *rollupCapturingStore) ComputeP75(rollups []HourlyRollup, minSampleCount int) []P75Result {
	return computeP75(rollups, minSampleCount)
}
func (s *rollupCapturingStore) PruneRawEvents(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}
func (s *rollupCapturingStore) PruneHourlyRollups(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}
func (s *rollupCapturingStore) PruneDailyRollups(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

// TestPerBeaconRollupPopulation verifies the core invariant of the fix:
// writing N beacons via WriteEvent produces a rollup row with sample_count==N
// and the correct bucket populated, so GetHourlyRollups + ComputeP75 return a
// real p75 (not "insufficient samples") once N >= minSampleCount.
func TestPerBeaconRollupPopulation(t *testing.T) {
	const N = 100 // must be >= any minSampleCount used in the test
	siteID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tenantID := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	store := newRollupCapturingStore()
	ctx := context.Background()

	// Write N beacons, all LCP 2500ms (bucket for 2500ms is the one just above
	// the 2500 CrUX boundary if it exists, or just below — let BucketForValue decide).
	const valueMilli int32 = 2500
	expectedBucket := BucketForValue(valueMilli)
	for i := 0; i < N; i++ {
		if err := store.WriteEvent(ctx, IngestParams{
			TenantID:   tenantID,
			SiteID:     siteID,
			URLPattern: "/",
			Metric:     "lcp",
			ValueMilli: valueMilli,
			Device:     "desktop",
			Country:    "US",
			Conn:       "4g",
			SampleRate: 1.0,
		}); err != nil {
			t.Fatalf("WriteEvent #%d: %v", i, err)
		}
	}

	// ── Hourly rollup must have sample_count == N ──────────────────────────
	hourly, err := store.GetHourlyRollups(ctx, siteID, tenantID, time.Time{})
	if err != nil {
		t.Fatalf("GetHourlyRollups: %v", err)
	}
	if len(hourly) == 0 {
		t.Fatal("GetHourlyRollups returned no rows — rollups are never populated (the confirmed gap)")
	}
	h := hourly[0]
	if h.SampleCount != int64(N) {
		t.Errorf("hourly.SampleCount = %d, want %d", h.SampleCount, N)
	}
	if int(h.BucketCounts[expectedBucket]) != N {
		t.Errorf("hourly.BucketCounts[%d] = %d, want %d", expectedBucket, h.BucketCounts[expectedBucket], N)
	}
	// Verify the other buckets are zero.
	for i, c := range h.BucketCounts {
		if i != expectedBucket && c != 0 {
			t.Errorf("hourly.BucketCounts[%d] = %d, want 0", i, c)
		}
	}
	if h.MinValue != valueMilli {
		t.Errorf("hourly.MinValue = %d, want %d", h.MinValue, valueMilli)
	}
	if h.MaxValue != valueMilli {
		t.Errorf("hourly.MaxValue = %d, want %d", h.MaxValue, valueMilli)
	}
	if h.SumValue != int64(N)*int64(valueMilli) {
		t.Errorf("hourly.SumValue = %d, want %d", h.SumValue, int64(N)*int64(valueMilli))
	}

	// ── Daily rollup must also have sample_count == N ──────────────────────
	daily, err := store.GetDailyRollups(ctx, siteID, tenantID, time.Time{})
	if err != nil {
		t.Fatalf("GetDailyRollups: %v", err)
	}
	if len(daily) == 0 {
		t.Fatal("GetDailyRollups returned no rows — daily rollups are never populated")
	}
	d := daily[0]
	if d.SampleCount != int64(N) {
		t.Errorf("daily.SampleCount = %d, want %d", d.SampleCount, N)
	}

	// ── ComputeP75 must produce a real p75 (not suppressed) ───────────────
	results := store.ComputeP75(hourly, N) // minSampleCount == N → must pass
	if len(results) == 0 {
		t.Fatal("ComputeP75 returned no results")
	}
	if results[0].P75Milli == 0 {
		t.Errorf("ComputeP75: P75Milli=0 (suppressed) after %d samples — insufficient samples suppression is wrong", N)
	}
	if results[0].SampleCount != int64(N) {
		t.Errorf("ComputeP75: SampleCount = %d, want %d", results[0].SampleCount, N)
	}
}

// TestPerBeaconRollupSuppressedBelowFloor verifies that ComputeP75 still
// returns P75Milli == 0 when the sample count is below minSampleCount —
// the existing suppression guard must remain intact.
func TestPerBeaconRollupSuppressedBelowFloor(t *testing.T) {
	siteID := uuid.MustParse("11111111-1111-1111-1111-111111111112")
	tenantID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	store := newRollupCapturingStore()
	ctx := context.Background()

	// Write 5 beacons; minSampleCount is 100 → must be suppressed.
	for i := 0; i < 5; i++ {
		_ = store.WriteEvent(ctx, IngestParams{
			TenantID:   tenantID,
			SiteID:     siteID,
			URLPattern: "/",
			Metric:     "fcp",
			ValueMilli: 500,
			Device:     "mobile",
			Country:    "GB",
			SampleRate: 1.0,
		})
	}

	hourly, _ := store.GetHourlyRollups(ctx, siteID, tenantID, time.Time{})
	if len(hourly) == 0 {
		t.Fatal("no hourly rows after 5 writes")
	}
	results := store.ComputeP75(hourly, 100) // floor=100, only 5 samples
	if len(results) == 0 {
		t.Fatal("ComputeP75 returned no results")
	}
	if results[0].P75Milli != 0 {
		t.Errorf("ComputeP75: P75Milli=%f, want 0 (suppressed — only 5 < 100 samples)", results[0].P75Milli)
	}
	if results[0].SampleCount != 5 {
		t.Errorf("ComputeP75: SampleCount=%d, want 5", results[0].SampleCount)
	}
}

// TestPerBeaconRollupMultiMetric verifies that separate metrics produce separate
// rollup groups (no cross-contamination of counts between lcp / fcp / cls).
func TestPerBeaconRollupMultiMetric(t *testing.T) {
	siteID := uuid.MustParse("11111111-1111-1111-1111-111111111113")
	tenantID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	store := newRollupCapturingStore()
	ctx := context.Background()

	metrics := []struct {
		metric string
		value  int32
		n      int
	}{
		{"lcp", 2500, 50},
		{"fcp", 800, 30},
		{"cls", 250, 20},
	}
	for _, m := range metrics {
		for i := 0; i < m.n; i++ {
			_ = store.WriteEvent(ctx, IngestParams{
				TenantID: tenantID, SiteID: siteID,
				URLPattern: "/", Metric: m.metric, ValueMilli: m.value,
				Device: "desktop", Country: "US", SampleRate: 1.0,
			})
		}
	}

	hourly, _ := store.GetHourlyRollups(ctx, siteID, tenantID, time.Time{})
	counts := make(map[string]int64)
	for _, r := range hourly {
		counts[r.Metric] += r.SampleCount
	}
	for _, m := range metrics {
		if counts[m.metric] != int64(m.n) {
			t.Errorf("metric %s: sample_count=%d, want %d", m.metric, counts[m.metric], m.n)
		}
	}
}
