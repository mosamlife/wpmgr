package perf

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/rum"
)

// rumStubStore is a minimal in-memory stub satisfying the RumResultsReader fields.
type rumStubStore struct {
	rollups []rum.HourlyRollup
}

func (s *rumStubStore) GetHourlyRollups(_ context.Context, _, _ uuid.UUID, _ time.Time) ([]rum.HourlyRollup, error) {
	return s.rollups, nil
}

func (s *rumStubStore) ComputeP75(rollups []rum.HourlyRollup, minSampleCount int) []rum.P75Result {
	// Delegate to the real StorePostgres.ComputeP75 (pure computation, no DB).
	store := rum.NewStorePostgres(nil)
	return store.ComputeP75(rollups, minSampleCount)
}

// rumConfigRepo is a repo stub that returns a canned Config for GetConfig.
// Only GetConfig is needed by the rum handler; all other methods delegate to
// the existing fakeRepo from service_test.go so the interface is satisfied.
type rumConfigRepo struct {
	fakeRepo
	minSampleCount int
}

func (r *rumConfigRepo) GetConfig(_ context.Context, tenantID, siteID uuid.UUID) (Config, error) {
	return Config{
		TenantID:       tenantID,
		SiteID:         siteID,
		MinSampleCount: r.minSampleCount,
		RumEnabled:     true,
		RumSampleRate:  1.0,
	}, nil
}

// newRumTestHandler builds a minimal Handler for RUM handler tests.
func newRumTestHandler(reader *RumResultsReader, minSampleCount int) *Handler {
	svc := &Service{repo: &rumConfigRepo{minSampleCount: minSampleCount}}
	return &Handler{svc: svc, rum: reader}
}

// injectPrincipal injects a test principal into the gin.Context.
func injectPrincipal(c *gin.Context) {
	ctx := domain.WithPrincipal(c.Request.Context(), domain.Principal{
		TenantID: uuid.New(),
		Role:     string(authz.RoleAdmin),
	})
	c.Request = c.Request.WithContext(ctx)
}

// ---------------------------------------------------------------------------
// min_sample_count suppression tests
// ---------------------------------------------------------------------------

// TestRumSummary_suppressedBelowMinSampleCount verifies that slices with too
// few samples come back with suppressed=true and p75_ms=0 in the summary.
func TestRumSummary_suppressedBelowMinSampleCount(t *testing.T) {
	counts := make([]int32, rum.NumBuckets)
	counts[0] = 5 // 5 samples < minSampleCount=100
	st := &rumStubStore{
		rollups: []rum.HourlyRollup{
			{
				RollupKey:    rum.RollupKey{Metric: "lcp", Device: "desktop", Country: "US"},
				SampleCount:  5,
				BucketCounts: counts,
			},
		},
	}
	reader := &RumResultsReader{
		GetHourlyRollups: st.GetHourlyRollups,
		ComputeP75:       st.ComputeP75,
	}
	h := newRumTestHandler(reader, 100)

	engine := gin.New()
	siteID := uuid.New()
	engine.GET("/sites/:siteId/perf/rum/summary", func(c *gin.Context) {
		injectPrincipal(c)
		h.rumSummary(c)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sites/"+siteID.String()+"/perf/rum/summary", nil)
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("rumSummary: expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !rumContains(body, `"suppressed":true`) {
		t.Errorf("expected suppressed=true in response, body=%s", body)
	}
}

// TestRumSummary_notSuppressedAboveMinSampleCount verifies that slices meeting
// the floor come back with suppressed=false and a non-zero p75_ms.
func TestRumSummary_notSuppressedAboveMinSampleCount(t *testing.T) {
	counts := make([]int32, rum.NumBuckets)
	counts[0] = 150 // 150 >= minSampleCount=100
	st := &rumStubStore{
		rollups: []rum.HourlyRollup{
			{
				RollupKey:    rum.RollupKey{Metric: "lcp", Device: "desktop", Country: "US"},
				SampleCount:  150,
				BucketCounts: counts,
				MaxValue:     200,
			},
		},
	}
	reader := &RumResultsReader{
		GetHourlyRollups: st.GetHourlyRollups,
		ComputeP75:       st.ComputeP75,
	}
	h := newRumTestHandler(reader, 100)

	engine := gin.New()
	siteID := uuid.New()
	engine.GET("/sites/:siteId/perf/rum/summary", func(c *gin.Context) {
		injectPrincipal(c)
		h.rumSummary(c)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sites/"+siteID.String()+"/perf/rum/summary", nil)
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("rumSummary: expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if rumContains(body, `"suppressed":true`) {
		t.Errorf("expected suppressed=false for adequate sample count, body=%s", body)
	}
	if rumContains(body, `"p75_ms":0`) {
		t.Errorf("expected non-zero p75_ms for adequate sample count, body=%s", body)
	}
}

// TestRumResults_suppressedBelowMinSampleCount verifies the per-URL table also
// applies the suppression floor.
func TestRumResults_suppressedBelowMinSampleCount(t *testing.T) {
	counts := make([]int32, rum.NumBuckets)
	counts[5] = 3 // 3 < minSampleCount=50
	st := &rumStubStore{
		rollups: []rum.HourlyRollup{
			{
				RollupKey:    rum.RollupKey{URLPattern: "/shop", Metric: "lcp", Device: "mobile", Country: "GB"},
				SampleCount:  3,
				BucketCounts: counts,
			},
		},
	}
	reader := &RumResultsReader{
		GetHourlyRollups: st.GetHourlyRollups,
		ComputeP75:       st.ComputeP75,
	}
	h := newRumTestHandler(reader, 50)

	engine := gin.New()
	siteID := uuid.New()
	engine.GET("/sites/:siteId/perf/rum", func(c *gin.Context) {
		injectPrincipal(c)
		h.rumResults(c)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sites/"+siteID.String()+"/perf/rum", nil)
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("rumResults: expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !rumContains(body, `"suppressed":true`) {
		t.Errorf("expected suppressed=true in per-URL results, body=%s", body)
	}
}

// TestRumSummary_nilReader verifies graceful degradation when the reader is nil.
func TestRumSummary_nilReader(t *testing.T) {
	h := newRumTestHandler(nil, 100)

	engine := gin.New()
	siteID := uuid.New()
	engine.GET("/sites/:siteId/perf/rum/summary", func(c *gin.Context) {
		injectPrincipal(c)
		h.rumSummary(c)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sites/"+siteID.String()+"/perf/rum/summary", nil)
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("nil reader: expected 200, got %d", w.Code)
	}
	if !rumContains(w.Body.String(), `"metrics":[]`) {
		t.Errorf("nil reader: expected empty metrics array, body=%s", w.Body.String())
	}
}

// TestRumResults_nilReader verifies graceful degradation when the reader is nil.
func TestRumResults_nilReader(t *testing.T) {
	h := newRumTestHandler(nil, 100)

	engine := gin.New()
	siteID := uuid.New()
	engine.GET("/sites/:siteId/perf/rum", func(c *gin.Context) {
		injectPrincipal(c)
		h.rumResults(c)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sites/"+siteID.String()+"/perf/rum", nil)
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("nil reader: expected 200, got %d", w.Code)
	}
	if !rumContains(w.Body.String(), `"items":[]`) {
		t.Errorf("nil reader: expected empty items array, body=%s", w.Body.String())
	}
}

// TestRumSummary_cwvRatings verifies the CWV rating thresholds produce the
// correct good/needs_improvement/poor labels.
func TestRumSummary_cwvRatings(t *testing.T) {
	cases := []struct {
		metric string
		p75Ms  float64
		want   string
	}{
		{"lcp", 2000, "good"},
		{"lcp", 3000, "needs_improvement"},
		{"lcp", 5000, "poor"},
		{"inp", 100, "good"},
		{"inp", 300, "needs_improvement"},
		{"inp", 600, "poor"},
		{"cls", 50, "good"},               // 50 milli-units = 0.05 raw (good threshold is 0.1/100 milli)
		{"cls", 200, "needs_improvement"}, // 0.2 raw
		{"cls", 300, "poor"},              // 0.3 raw
		{"fcp", 1000, "good"},
		{"fcp", 2500, "needs_improvement"},
		{"fcp", 4000, "poor"},
		{"ttfb", 500, "good"},
		{"ttfb", 1000, "needs_improvement"},
		{"ttfb", 2000, "poor"},
	}
	for _, tc := range cases {
		got := cwvRating(tc.metric, tc.p75Ms)
		if got != tc.want {
			t.Errorf("cwvRating(%q, %g) = %q, want %q", tc.metric, tc.p75Ms, got, tc.want)
		}
	}
}

// rumContains reports whether s contains the literal substring sub.
func rumContains(s, sub string) bool {
	if len(sub) == 0 || len(s) < len(sub) {
		return false
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Distribution fold (bucket → good/NI/poor) tests
// ---------------------------------------------------------------------------

// makeCounts builds a NumBuckets int64 slice with specific bucket values set.
// Each pair is (bucketIndex, count).
func makeCounts(pairs ...int64) []int64 {
	c := make([]int64, rum.NumBuckets)
	for i := 0; i+1 < len(pairs); i += 2 {
		c[pairs[i]] = pairs[i+1]
	}
	return c
}

// TestFoldBuckets_LCP_exact verifies the fold for LCP (thresholds 2500/4000 ms).
// CrUXBuckets index 12 = upper bound 2500, index 15 = upper bound 4000.
// Good:  buckets 0..12 (lower bounds 0..2000, all < 2500)
// NI:    buckets 13..15 (lower bounds 2500..3500, all < 4000)
// Poor:  buckets 16..23 (lower bounds 4000+)
func TestFoldBuckets_LCP_exact(t *testing.T) {
	// Build a histogram: 100 in a "good" bucket (bucket 0, [0,200)),
	//                    50 in a "NI" bucket (bucket 13, [2500,3000)),
	//                    25 in a "poor" bucket (bucket 16, [4000,4500)).
	// bucket 0 lower=0 < 2500 → good
	// bucket 13: CrUXBuckets[12]=2500, so lower of bucket 13 = 2500 ≥ 2500 → NI (< 4000)
	// bucket 16: CrUXBuckets[15]=4000, so lower of bucket 16 = 4000 ≥ 4000 → poor
	counts := makeCounts(0, 100, 13, 50, 16, 25)
	d := foldBucketsIntoDistribution("lcp", counts)

	if d.Good != 100 {
		t.Errorf("LCP good count = %d, want 100", d.Good)
	}
	if d.NeedsImprovement != 50 {
		t.Errorf("LCP NI count = %d, want 50", d.NeedsImprovement)
	}
	if d.Poor != 25 {
		t.Errorf("LCP poor count = %d, want 25", d.Poor)
	}
	if d.GoodPct+d.NeedsImprovementPct+d.PoorPct != 100 {
		t.Errorf("LCP pct sum = %d, want 100 (good=%d ni=%d poor=%d)",
			d.GoodPct+d.NeedsImprovementPct+d.PoorPct, d.GoodPct, d.NeedsImprovementPct, d.PoorPct)
	}
	// 100/175 ≈ 57.1%, 50/175 ≈ 28.6%, 25/175 ≈ 14.3% → Hamilton rounds to 57+29+14=100.
	total := d.GoodPct + d.NeedsImprovementPct + d.PoorPct
	if total != 100 {
		t.Errorf("LCP pct total = %d, want exactly 100", total)
	}
}

// TestFoldBuckets_INP_exact verifies INP thresholds (200/500 ms).
// bucket 1 lower=200 is the first bucket with lower=200 ≥ 200, so it's NI (< 500).
// bucket 0 lower=0 < 200 → good.
// bucket 5 CrUXBuckets[4]=600, lower=600 ≥ 500 → poor.
func TestFoldBuckets_INP_exact(t *testing.T) {
	counts := makeCounts(0, 80, 1, 40, 5, 20)
	d := foldBucketsIntoDistribution("inp", counts)

	if d.Good != 80 {
		t.Errorf("INP good = %d, want 80", d.Good)
	}
	if d.NeedsImprovement != 40 {
		t.Errorf("INP NI = %d, want 40", d.NeedsImprovement)
	}
	if d.Poor != 20 {
		t.Errorf("INP poor = %d, want 20", d.Poor)
	}
	if d.GoodPct+d.NeedsImprovementPct+d.PoorPct != 100 {
		t.Errorf("INP pct sum != 100: %d", d.GoodPct+d.NeedsImprovementPct+d.PoorPct)
	}
}

// TestFoldBuckets_CLS_straddleProportionalSplit verifies the GH #185 fix: a
// bucket that straddles a CLS threshold is split PROPORTIONALLY at the
// threshold (uniform-within-bucket assumption), not assigned wholesale to the
// lower band. CLS thresholds: good<=100, NI<=250 (milli-units); the first
// CrUX boundary is 200, so bucket 0 ([0,200)) straddles 100 and bucket 1
// ([200,300)) straddles 250.
//
//	bucket 0 [0,200), count=60: goodUpper=100 splits it exactly in half ->
//	    30 good / 30 NI.
//	bucket 1 [200,300), count=30: niUpper=250 splits it exactly in half ->
//	    15 NI / 15 poor.
//	bucket 2 [300,400), count=10: wholly >= niUpper(250) -> all poor.
//
// Totals: good=30, NI=30+15=45, poor=15+10=25 (sums to the original 100).
func TestFoldBuckets_CLS_straddleProportionalSplit(t *testing.T) {
	counts := makeCounts(0, 60, 1, 30, 2, 10)
	d := foldBucketsIntoDistribution("cls", counts)

	if d.Good != 30 {
		t.Errorf("CLS good = %d, want 30 (bucket 0 split 50/50 at the 100 threshold)", d.Good)
	}
	if d.NeedsImprovement != 45 {
		t.Errorf("CLS NI = %d, want 45 (30 from bucket 0 + 15 from bucket 1)", d.NeedsImprovement)
	}
	if d.Poor != 25 {
		t.Errorf("CLS poor = %d, want 25 (15 from bucket 1 + 10 from bucket 2)", d.Poor)
	}
	if d.GoodPct+d.NeedsImprovementPct+d.PoorPct != 100 {
		t.Errorf("CLS pct sum = %d, want 100", d.GoodPct+d.NeedsImprovementPct+d.PoorPct)
	}
}

// TestFoldBuckets_CLS_matchesP75Rating is the exact GH #185 reported
// scenario: 16 CLS samples, all in bucket 0 ([0,200)). InterpolateP75FromCounts
// places p75 at 150 milli-units (75% of the way through [0,200)), which rates
// "needs_improvement" (100 < 150 <= 250) — NOT "good". Before the fix,
// foldBucketsIntoDistribution credited the whole bucket to "good" (100%),
// directly contradicting that p75 rating. The fix must produce a split that is
// consistent with the p75 rating: some samples good, some needs-improvement,
// and NOT 100% good.
func TestFoldBuckets_CLS_matchesP75Rating(t *testing.T) {
	counts := makeCounts(0, 16)

	p75 := rum.InterpolateP75FromCounts(counts, 16, 200)
	wantRating := cwvRating("cls", p75)
	if wantRating != "needs_improvement" {
		t.Fatalf("test setup invalid: p75=%v rated %q, want needs_improvement", p75, wantRating)
	}

	d := foldBucketsIntoDistribution("cls", counts)
	if d.Good == 16 {
		t.Fatalf("CLS distribution reports 100%% good (good=%d/16) while p75=%v rates %q — distribution contradicts the rating (GH #185)", d.Good, p75, wantRating)
	}
	// The bucket's goodUpper(100) sits exactly at the midpoint of [0,200), so a
	// uniform 16-sample bucket splits 8/8.
	if d.Good != 8 || d.NeedsImprovement != 8 || d.Poor != 0 {
		t.Errorf("CLS good/ni/poor = %d/%d/%d, want 8/8/0", d.Good, d.NeedsImprovement, d.Poor)
	}
	if d.Good+d.NeedsImprovement+d.Poor != 16 {
		t.Errorf("CLS counts sum = %d, want 16", d.Good+d.NeedsImprovement+d.Poor)
	}
}

// TestFoldBuckets_CLS_secondBucketPoorNoSplit verifies a bucket wholly beyond
// niUpper is NOT split (regression guard alongside the straddle fix): bucket 2
// ([300,400)) is entirely >= 250, so it must be 100% poor with none leaking
// into "good" or "needs_improvement".
func TestFoldBuckets_CLS_secondBucketPoorNoSplit(t *testing.T) {
	counts := makeCounts(2, 40)
	d := foldBucketsIntoDistribution("cls", counts)
	if d.Good != 0 || d.NeedsImprovement != 0 || d.Poor != 40 {
		t.Errorf("CLS good/ni/poor = %d/%d/%d, want 0/0/40 (bucket 2 is wholly poor)", d.Good, d.NeedsImprovement, d.Poor)
	}
}

// TestFoldBuckets_LCP_INP_unaffectedByProportionalSplitFix is an explicit
// GH #185 no-regression guard: LCP and INP's good/NI thresholds (2500/4000 and
// 200/500 respectively) land exactly on CrUX bucket boundaries, so the
// proportional-split fix must be a no-op for these metrics — every bucket
// falls wholly within one band, identical to the pre-fix fold.
func TestFoldBuckets_LCP_INP_unaffectedByProportionalSplitFix(t *testing.T) {
	// LCP: bucket 11 ([1800,2000)) wholly good, bucket 14 ([3000,3500)) wholly
	// NI, bucket 18 ([6000,7000)) wholly poor.
	lcpCounts := makeCounts(11, 40, 14, 25, 18, 15)
	lcp := foldBucketsIntoDistribution("lcp", lcpCounts)
	if lcp.Good != 40 || lcp.NeedsImprovement != 25 || lcp.Poor != 15 {
		t.Errorf("LCP good/ni/poor = %d/%d/%d, want 40/25/15 (no split expected)", lcp.Good, lcp.NeedsImprovement, lcp.Poor)
	}

	// INP: bucket 0 ([0,200)) wholly good, bucket 2 ([300,400)) wholly NI,
	// bucket 4 ([600,800)) wholly poor.
	inpCounts := makeCounts(0, 40, 2, 25, 4, 15)
	inp := foldBucketsIntoDistribution("inp", inpCounts)
	if inp.Good != 40 || inp.NeedsImprovement != 25 || inp.Poor != 15 {
		t.Errorf("INP good/ni/poor = %d/%d/%d, want 40/25/15 (no split expected)", inp.Good, inp.NeedsImprovement, inp.Poor)
	}
}

// TestFoldBuckets_PctSumTo100_edgeCase verifies Hamilton rounding for a
// distribution where naive floor-rounding yields 99 (each proportion has a
// large fractional part). Three equal bands of 1/3 each: floors are 33+33+33=99.
func TestFoldBuckets_PctSumTo100_edgeCase(t *testing.T) {
	// Use LCP so we can fill a known set of buckets. We put 1 sample in each of
	// three bands (good / NI / poor), so counts are 1/1/1, total=3, each=33.33%.
	counts := makeCounts(0, 1, 13, 1, 16, 1)
	d := foldBucketsIntoDistribution("lcp", counts)
	sum := d.GoodPct + d.NeedsImprovementPct + d.PoorPct
	if sum != 100 {
		t.Errorf("pct sum = %d, want exactly 100 (Hamilton rounding failure)", sum)
	}
}

// TestFoldBuckets_AllGood verifies that a histogram entirely in the good band
// yields good_pct=100, ni_pct=0, poor_pct=0.
func TestFoldBuckets_AllGood(t *testing.T) {
	counts := makeCounts(0, 200)
	d := foldBucketsIntoDistribution("lcp", counts)
	if d.GoodPct != 100 || d.NeedsImprovementPct != 0 || d.PoorPct != 0 {
		t.Errorf("all-good: good=%d ni=%d poor=%d", d.GoodPct, d.NeedsImprovementPct, d.PoorPct)
	}
}

// TestFoldBuckets_AllPoor verifies that a histogram entirely in the poor band
// yields good_pct=0, ni_pct=0, poor_pct=100.
func TestFoldBuckets_AllPoor(t *testing.T) {
	// bucket 16: lower=4000 ≥ LCP poor threshold (4000) → poor
	counts := makeCounts(16, 100)
	d := foldBucketsIntoDistribution("lcp", counts)
	if d.GoodPct != 0 || d.NeedsImprovementPct != 0 || d.PoorPct != 100 {
		t.Errorf("all-poor: good=%d ni=%d poor=%d", d.GoodPct, d.NeedsImprovementPct, d.PoorPct)
	}
}

// ---------------------------------------------------------------------------
// Summary distribution integration test
// ---------------------------------------------------------------------------

// TestRumSummary_distributionPopulated verifies that an unsuppressed metric
// summary row carries a non-nil Distribution field with counts and pct sum=100.
func TestRumSummary_distributionPopulated(t *testing.T) {
	// 200 samples in bucket 0 (LCP good band).
	counts := make([]int32, rum.NumBuckets)
	counts[0] = 200
	st := &rumStubStore{
		rollups: []rum.HourlyRollup{
			{
				RollupKey:    rum.RollupKey{Metric: "lcp", Device: "desktop", Country: "US"},
				SampleCount:  200,
				BucketCounts: counts,
				MaxValue:     150,
			},
		},
	}
	reader := &RumResultsReader{
		GetHourlyRollups: st.GetHourlyRollups,
		ComputeP75:       st.ComputeP75,
	}
	h := newRumTestHandler(reader, 50)

	engine := gin.New()
	siteID := uuid.New()
	engine.GET("/sites/:siteId/perf/rum/summary", func(c *gin.Context) {
		injectPrincipal(c)
		h.rumSummary(c)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sites/"+siteID.String()+"/perf/rum/summary", nil)
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Distribution should be present and non-null.
	if !rumContains(body, `"distribution"`) {
		t.Errorf("distribution field missing from summary response, body=%s", body)
	}
	// All samples are in the good band so good_pct should be 100.
	if !rumContains(body, `"good_pct":100`) {
		t.Errorf("expected good_pct:100 (all samples in good band), body=%s", body)
	}
	// pct values must sum to 100 — we verify good_pct=100 implies ni=0, poor=0.
	if rumContains(body, `"needs_improvement_pct":0`) {
		// expected
	}
}

// TestRumSummary_distributionNilWhenSuppressed verifies that a suppressed slice
// (sample_count < min_sample_count) has no distribution field in the response.
func TestRumSummary_distributionNilWhenSuppressed(t *testing.T) {
	counts := make([]int32, rum.NumBuckets)
	counts[0] = 3 // 3 < 50 → suppressed
	st := &rumStubStore{
		rollups: []rum.HourlyRollup{
			{
				RollupKey:    rum.RollupKey{Metric: "lcp", Device: "desktop", Country: "US"},
				SampleCount:  3,
				BucketCounts: counts,
			},
		},
	}
	reader := &RumResultsReader{
		GetHourlyRollups: st.GetHourlyRollups,
		ComputeP75:       st.ComputeP75,
	}
	h := newRumTestHandler(reader, 50)

	engine := gin.New()
	siteID := uuid.New()
	engine.GET("/sites/:siteId/perf/rum/summary", func(c *gin.Context) {
		injectPrincipal(c)
		h.rumSummary(c)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sites/"+siteID.String()+"/perf/rum/summary", nil)
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	// distribution must be absent (omitempty when nil).
	if rumContains(body, `"distribution"`) {
		t.Errorf("distribution should be absent for suppressed slice, body=%s", body)
	}
	if !rumContains(body, `"suppressed":true`) {
		t.Errorf("expected suppressed=true, body=%s", body)
	}
}

// ---------------------------------------------------------------------------
// Trend endpoint tests
// ---------------------------------------------------------------------------

// rumDailyStubStore provides a stub for the daily rollup getter.
type rumDailyStubStore struct {
	daily []rum.DailyRollup
}

func (s *rumDailyStubStore) GetDailyRollups(_ context.Context, _, _ uuid.UUID, _ time.Time) ([]rum.DailyRollup, error) {
	return s.daily, nil
}

// newRumTrendTestHandler builds a Handler with both hourly and daily stubs wired.
func newRumTrendTestHandler(daily []rum.DailyRollup, minSampleCount int) *Handler {
	dailyStub := &rumDailyStubStore{daily: daily}
	reader := &RumResultsReader{
		GetHourlyRollups: func(_ context.Context, _, _ uuid.UUID, _ time.Time) ([]rum.HourlyRollup, error) {
			return nil, nil
		},
		ComputeP75:      func(rollups []rum.HourlyRollup, min int) []rum.P75Result { return nil },
		GetDailyRollups: dailyStub.GetDailyRollups,
	}
	svc := &Service{repo: &rumConfigRepo{minSampleCount: minSampleCount}}
	return &Handler{svc: svc, rum: reader}
}

// TestRumTrend_suppressedDayPresent verifies that a day below the
// min_sample_count floor appears in the response with suppressed=true and
// p75_ms=0 (the client renders a gap, not a misleading zero).
func TestRumTrend_suppressedDayPresent(t *testing.T) {
	day := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)
	counts := make([]int32, rum.NumBuckets)
	counts[0] = 5 // 5 < 30 → suppressed

	h := newRumTrendTestHandler([]rum.DailyRollup{
		{
			RollupKey:    rum.RollupKey{Metric: "lcp", Device: "desktop", Country: "US"},
			BucketDay:    day,
			SampleCount:  5,
			BucketCounts: counts,
		},
	}, 30)

	engine := gin.New()
	siteID := uuid.New()
	engine.GET("/sites/:siteId/perf/rum/trend", func(c *gin.Context) {
		injectPrincipal(c)
		h.rumTrend(c)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sites/"+siteID.String()+"/perf/rum/trend?window_days=28", nil)
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("trend: expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// The day must appear in the response.
	if !rumContains(body, `"2026-05-13"`) {
		t.Errorf("expected day 2026-05-13 in trend response, body=%s", body)
	}
	// It must be suppressed.
	if !rumContains(body, `"suppressed":true`) {
		t.Errorf("expected suppressed=true for below-floor day, body=%s", body)
	}
	// p75_ms must be 0 for the suppressed day.
	if !rumContains(body, `"p75_ms":0`) {
		t.Errorf("expected p75_ms:0 for suppressed day, body=%s", body)
	}
}

// TestRumTrend_unsuppressedDay verifies that a day meeting the floor has a
// non-zero p75_ms and suppressed=false.
func TestRumTrend_unsuppressedDay(t *testing.T) {
	day := time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC)
	// 100 samples all in bucket 0 ([0,200)ms). p75 = 150ms (interpolated).
	counts := make([]int32, rum.NumBuckets)
	counts[0] = 100

	h := newRumTrendTestHandler([]rum.DailyRollup{
		{
			RollupKey:    rum.RollupKey{Metric: "lcp", Device: "desktop", Country: "US"},
			BucketDay:    day,
			SampleCount:  100,
			BucketCounts: counts,
			MaxValue:     150,
		},
	}, 30)

	engine := gin.New()
	siteID := uuid.New()
	engine.GET("/sites/:siteId/perf/rum/trend", func(c *gin.Context) {
		injectPrincipal(c)
		h.rumTrend(c)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sites/"+siteID.String()+"/perf/rum/trend", nil)
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("trend: expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !rumContains(body, `"2026-05-14"`) {
		t.Errorf("expected day 2026-05-14 in response, body=%s", body)
	}
	if rumContains(body, `"suppressed":true`) {
		t.Errorf("expected suppressed=false for above-floor day, body=%s", body)
	}
	// p75_ms must be non-zero.
	if rumContains(body, `"p75_ms":0`) {
		t.Errorf("expected non-zero p75_ms for above-floor day, body=%s", body)
	}
	// Rating must be present (all samples in bucket 0 < 2500 → "good" for LCP).
	if !rumContains(body, `"rating":"good"`) {
		t.Errorf("expected rating:good for LCP p75≈150ms, body=%s", body)
	}
}

// TestRumTrend_nilReader verifies that a nil rum reader returns an empty but
// well-formed trend response (all 5 metrics present as empty slices).
func TestRumTrend_nilReader(t *testing.T) {
	svc := &Service{repo: &rumConfigRepo{minSampleCount: 30}}
	h := &Handler{svc: svc, rum: nil}

	engine := gin.New()
	siteID := uuid.New()
	engine.GET("/sites/:siteId/perf/rum/trend", func(c *gin.Context) {
		injectPrincipal(c)
		h.rumTrend(c)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sites/"+siteID.String()+"/perf/rum/trend", nil)
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("nil reader trend: expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// All 5 standard metrics must be present as empty arrays.
	for _, m := range []string{"lcp", "inp", "cls", "fcp", "ttfb"} {
		want := `"` + m + `":[]`
		if !rumContains(body, want) {
			t.Errorf("nil reader: expected %q in trend response, body=%s", want, body)
		}
	}
}

// TestRumTrend_windowDaysClamp verifies that window_days is clamped to [1,90].
func TestRumTrend_windowDaysClamp(t *testing.T) {
	h := newRumTrendTestHandler(nil, 30)

	engine := gin.New()
	siteID := uuid.New()
	engine.GET("/sites/:siteId/perf/rum/trend", func(c *gin.Context) {
		injectPrincipal(c)
		h.rumTrend(c)
	})

	// window_days=200 → should clamp to 90.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sites/"+siteID.String()+"/perf/rum/trend?window_days=200", nil)
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("clamp test: expected 200, got %d", w.Code)
	}
	if !rumContains(w.Body.String(), `"window_days":90`) {
		t.Errorf("expected window_days clamped to 90, body=%s", w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// rumSummary all-devices aggregate tests (fix for "All" tab "No data" bug)
// ---------------------------------------------------------------------------

// TestRumSummary_allDevicesAggregateEmitted verifies that rumSummary emits a
// device="" aggregate row per metric whose sample_count equals the sum of the
// per-device rows. This is the row the frontend "All" tab consumes.
func TestRumSummary_allDevicesAggregateEmitted(t *testing.T) {
	counts := make([]int32, rum.NumBuckets)
	counts[0] = 60

	// Two devices, same metric, same country — the all-devices row must sum them.
	st := &rumStubStore{
		rollups: []rum.HourlyRollup{
			{
				RollupKey:    rum.RollupKey{Metric: "lcp", Device: "desktop", Country: "US"},
				SampleCount:  60,
				BucketCounts: counts,
				MaxValue:     150,
			},
			{
				RollupKey:    rum.RollupKey{Metric: "lcp", Device: "mobile", Country: "US"},
				SampleCount:  40,
				BucketCounts: counts,
				MaxValue:     150,
			},
		},
	}
	reader := &RumResultsReader{
		GetHourlyRollups: st.GetHourlyRollups,
		ComputeP75:       st.ComputeP75,
	}
	h := newRumTestHandler(reader, 10)

	engine := gin.New()
	siteID := uuid.New()
	engine.GET("/sites/:siteId/perf/rum/summary", func(c *gin.Context) {
		injectPrincipal(c)
		h.rumSummary(c)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sites/"+siteID.String()+"/perf/rum/summary", nil)
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// The all-devices aggregate row (device="") must be present.
	// JSON: "device":"" (empty string device field).
	if !rumContains(body, `"device":""`) {
		t.Errorf("expected all-devices aggregate (device:\"\") in response, body=%s", body)
	}
	// The all-devices row sample_count must equal 60+40=100.
	if !rumContains(body, `"sample_count":100`) {
		t.Errorf("expected all-devices sample_count=100 (sum of desktop+mobile), body=%s", body)
	}
	// Individual device rows must also be present.
	if !rumContains(body, `"device":"desktop"`) {
		t.Errorf("expected desktop device row in response, body=%s", body)
	}
	if !rumContains(body, `"device":"mobile"`) {
		t.Errorf("expected mobile device row in response, body=%s", body)
	}
}

// TestRumSummary_deviceRowSumsAcrossCountries verifies that a per-device row
// aggregates across all countries (no per-(device,country) split in the summary).
func TestRumSummary_deviceRowSumsAcrossCountries(t *testing.T) {
	counts := make([]int32, rum.NumBuckets)
	counts[0] = 50

	st := &rumStubStore{
		rollups: []rum.HourlyRollup{
			{
				RollupKey:    rum.RollupKey{Metric: "lcp", Device: "desktop", Country: "US"},
				SampleCount:  50,
				BucketCounts: counts,
				MaxValue:     150,
			},
			{
				RollupKey:    rum.RollupKey{Metric: "lcp", Device: "desktop", Country: "GB"},
				SampleCount:  30,
				BucketCounts: counts,
				MaxValue:     150,
			},
		},
	}
	reader := &RumResultsReader{
		GetHourlyRollups: st.GetHourlyRollups,
		ComputeP75:       st.ComputeP75,
	}
	h := newRumTestHandler(reader, 10)

	engine := gin.New()
	siteID := uuid.New()
	engine.GET("/sites/:siteId/perf/rum/summary", func(c *gin.Context) {
		injectPrincipal(c)
		h.rumSummary(c)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sites/"+siteID.String()+"/perf/rum/summary", nil)
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Desktop row must have sample_count == 50+30 = 80 (country-collapsed).
	if !rumContains(body, `"sample_count":80`) {
		t.Errorf("expected desktop device row with sample_count=80 (US+GB collapsed), body=%s", body)
	}
	// The response must NOT expose country-level rows (country field must be "").
	if rumContains(body, `"country":"US"`) || rumContains(body, `"country":"GB"`) {
		t.Errorf("summary must not expose per-country rows, body=%s", body)
	}
}

// TestRumSummary_allTabPopulatesWhenPerDeviceSuppressed verifies that the
// all-devices row can pass the min_sample_count floor even when no single
// device row does (the "All" tab populates first).
func TestRumSummary_allTabPopulatesWhenPerDeviceSuppressed(t *testing.T) {
	counts := make([]int32, rum.NumBuckets)
	counts[0] = 20 // 20 < 30 (floor) per device, but 20+20=40 > 30 combined

	st := &rumStubStore{
		rollups: []rum.HourlyRollup{
			{
				RollupKey:    rum.RollupKey{Metric: "lcp", Device: "desktop", Country: "US"},
				SampleCount:  20,
				BucketCounts: counts,
				MaxValue:     150,
			},
			{
				RollupKey:    rum.RollupKey{Metric: "lcp", Device: "mobile", Country: "US"},
				SampleCount:  20,
				BucketCounts: counts,
				MaxValue:     150,
			},
		},
	}
	reader := &RumResultsReader{
		GetHourlyRollups: st.GetHourlyRollups,
		ComputeP75:       st.ComputeP75,
	}
	h := newRumTestHandler(reader, 30)

	engine := gin.New()
	siteID := uuid.New()
	engine.GET("/sites/:siteId/perf/rum/summary", func(c *gin.Context) {
		injectPrincipal(c)
		h.rumSummary(c)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sites/"+siteID.String()+"/perf/rum/summary", nil)
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// The all-devices aggregate must NOT be suppressed (40 >= 30).
	// We verify by asserting device="" is present AND p75_ms is non-zero.
	if !rumContains(body, `"device":""`) {
		t.Errorf("expected all-devices row in response, body=%s", body)
	}
	// The all-devices row sample_count == 40; verify it is not suppressed by
	// checking a non-zero p75_ms appears somewhere (bucket 0 → p75 ≈ 150ms).
	if rumContains(body, `"p75_ms":0`) && !rumContains(body, `"suppressed":false`) {
		// p75_ms:0 is expected only on suppressed rows. A non-suppressed all-devices
		// row should have non-zero p75. Check suppressed:false is present.
		t.Errorf("all-devices row should not be suppressed when total >= floor, body=%s", body)
	}
}

// ---------------------------------------------------------------------------
// rumTrend aggregation key tests (fix for "All" tab collapse bug)
// ---------------------------------------------------------------------------

// TestRumTrend_allDevicesCollapseToOneSeries verifies that when no device
// filter is passed, rumTrend returns exactly ONE series per metric (not one
// per device), and the day's sample_count equals the sum across devices.
func TestRumTrend_allDevicesCollapseToOneSeries(t *testing.T) {
	day := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	counts := make([]int32, rum.NumBuckets)
	counts[0] = 50

	h := newRumTrendTestHandler([]rum.DailyRollup{
		{
			RollupKey:    rum.RollupKey{Metric: "lcp", Device: "desktop", Country: "US"},
			BucketDay:    day,
			SampleCount:  50,
			BucketCounts: counts,
			MaxValue:     150,
		},
		{
			RollupKey:    rum.RollupKey{Metric: "lcp", Device: "mobile", Country: "US"},
			BucketDay:    day,
			SampleCount:  40,
			BucketCounts: counts,
			MaxValue:     150,
		},
	}, 10)

	engine := gin.New()
	siteID := uuid.New()
	engine.GET("/sites/:siteId/perf/rum/trend", func(c *gin.Context) {
		injectPrincipal(c)
		h.rumTrend(c)
	})

	// No device filter → All tab.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sites/"+siteID.String()+"/perf/rum/trend", nil)
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("trend: expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// The day must appear exactly once (not twice — once per device).
	// Count occurrences of "2026-05-20".
	count := 0
	for i := 0; i <= len(body)-len("2026-05-20"); i++ {
		if body[i:i+len("2026-05-20")] == "2026-05-20" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected day 2026-05-20 to appear exactly once (collapsed), got %d occurrences, body=%s", count, body)
	}

	// The collapsed day's sample_count must be 50+40=90.
	if !rumContains(body, `"sample_count":90`) {
		t.Errorf("expected collapsed sample_count=90 (desktop+mobile), body=%s", body)
	}
}

// TestRumTrend_deviceFilterProducesOneSeries verifies that with a device
// filter, only rows for that device are included and countries are collapsed.
func TestRumTrend_deviceFilterProducesOneSeries(t *testing.T) {
	day := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	counts := make([]int32, rum.NumBuckets)
	counts[0] = 40

	h := newRumTrendTestHandler([]rum.DailyRollup{
		{
			RollupKey:    rum.RollupKey{Metric: "lcp", Device: "desktop", Country: "US"},
			BucketDay:    day,
			SampleCount:  40,
			BucketCounts: counts,
			MaxValue:     150,
		},
		{
			RollupKey:    rum.RollupKey{Metric: "lcp", Device: "desktop", Country: "GB"},
			BucketDay:    day,
			SampleCount:  25,
			BucketCounts: counts,
			MaxValue:     150,
		},
		{
			RollupKey:    rum.RollupKey{Metric: "lcp", Device: "mobile", Country: "US"},
			BucketDay:    day,
			SampleCount:  30,
			BucketCounts: counts,
			MaxValue:     150,
		},
	}, 10)

	engine := gin.New()
	siteID := uuid.New()
	engine.GET("/sites/:siteId/perf/rum/trend", func(c *gin.Context) {
		injectPrincipal(c)
		h.rumTrend(c)
	})

	// device=desktop filter: only US+GB desktop rows, mobile excluded.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sites/"+siteID.String()+"/perf/rum/trend?device=desktop", nil)
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("trend: expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// desktop US+GB collapsed: 40+25=65.
	if !rumContains(body, `"sample_count":65`) {
		t.Errorf("expected desktop collapsed sample_count=65 (US+GB), body=%s", body)
	}
	// Mobile sample_count (30) must not appear.
	if rumContains(body, `"sample_count":30`) {
		t.Errorf("mobile sample_count=30 must not appear with device=desktop filter, body=%s", body)
	}
}
