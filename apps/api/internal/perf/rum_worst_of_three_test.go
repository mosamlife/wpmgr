package perf

import (
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/rum"
)

// ---------------------------------------------------------------------------
// GH #391 — worstOfThree's tie-break order (lcp, then inp, then cls; first
// metric at max severity wins) must be pinned. A reviewer reversed the order
// once and the rest of the suite stayed green, because none of it exercised a
// genuine tie: with only three metrics and three bands, two sites out of
// three routinely land two metrics in the same band, so an unstable tie-break
// would flicker the distribution bar between refreshes — precisely the
// confusion #391 was filed about. See the worstOfThree doc comment.
// ---------------------------------------------------------------------------

// TestWorstOfThree_tieBreakOrder pins the order directly against the helper.
// Reversing the iteration order in worstOfThree (lcp, inp, cls) flips every
// one of these.
func TestWorstOfThree_tieBreakOrder(t *testing.T) {
	tests := []struct {
		name          string
		lcp, inp, cls string
		wantRating    string
		wantMetric    string
	}{
		{
			name:       "lcp and inp both worst (poor), cls good: lcp wins",
			lcp:        "poor",
			inp:        "poor",
			cls:        "good",
			wantRating: "poor",
			wantMetric: "lcp",
		},
		{
			name:       "inp and cls both worst (poor), lcp better: inp wins",
			lcp:        "needs-improvement",
			inp:        "poor",
			cls:        "poor",
			wantRating: "poor",
			wantMetric: "inp",
		},
		{
			name:       "all three tied at the same band: lcp wins",
			lcp:        "poor",
			inp:        "poor",
			cls:        "poor",
			wantRating: "poor",
			wantMetric: "lcp",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRating, gotMetric := worstOfThree(tt.lcp, tt.inp, tt.cls)
			if gotRating != tt.wantRating || gotMetric != tt.wantMetric {
				t.Errorf("worstOfThree(%q, %q, %q) = (%q, %q), want (%q, %q)",
					tt.lcp, tt.inp, tt.cls, gotRating, gotMetric, tt.wantRating, tt.wantMetric)
			}
		})
	}
}

// TestRumFleet_tieBreak_distributionMatchesWinningMetric drives the tie
// through the real handler path (not just the worstOfThree helper) and
// verifies the winner is the metric whose DISTRIBUTION is actually returned,
// not merely the one named in distribution_metric. LCP and INP are both
// rated "poor" (a genuine tie), so worstOfThree must pick lcp — and the
// histogram folded into the response must be LCP's own (100% poor, 0% good),
// not INP's (which is deliberately built with a different split: 20% good /
// 80% poor) so the two cannot be confused for one another by accident.
func TestRumFleet_tieBreak_distributionMatchesWinningMetric(t *testing.T) {
	siteA := uuid.New()

	// LCP: wholly poor. Bucket 17 lower bound (4500ms) is already > LCP's
	// poor threshold (4000), so this is 100% poor regardless of
	// within-bucket interpolation.
	lcpPoorCounts := fleetInt32Counts(17, 50)

	// INP: also rated "poor" at p75 (tied with LCP), but built from a
	// DIFFERENT distribution so a swapped-source bug is visible: 10 samples
	// in bucket 0 ([0,200), wholly good for INP's <=200 threshold) plus 40
	// samples in bucket 4 (lower bound 500, wholly poor for INP's >=500
	// threshold). p75 (the 38th of 50 samples) falls among the 40 poor
	// samples, so INP's rating is "poor" — tied with LCP — but its folded
	// distribution is 20% good / 80% poor, nothing like LCP's 100% poor.
	inpCounts := make([]int32, rum.NumBuckets)
	inpCounts[0] = 10
	inpCounts[4] = 40

	rollups := []rum.HourlyRollup{
		{RollupKey: rum.RollupKey{SiteID: siteA, Metric: "lcp"}, SampleCount: 50, BucketCounts: lcpPoorCounts, MaxValue: 4900},
		{RollupKey: rum.RollupKey{SiteID: siteA, Metric: "inp"}, SampleCount: 50, BucketCounts: inpCounts, MaxValue: 590},
	}

	h := newFleetRumHandler([]uuid.UUID{siteA}, rollups)
	resp := callRumFleet(t, h)

	off := findOffender(resp, siteA)
	if off == nil {
		t.Fatalf("expected site A (lcp=poor, inp=poor tie) in worst_offenders, got none")
	}
	if off.OverallRating != "poor" {
		t.Fatalf("overall_rating = %q, want %q (both lcp and inp are poor)", off.OverallRating, "poor")
	}
	if off.DistributionMetric != "lcp" {
		t.Fatalf("distribution_metric = %q, want %q (tie-break: lcp wins over inp)", off.DistributionMetric, "lcp")
	}
	if off.Distribution == nil {
		t.Fatalf("distribution is nil, want a populated split")
	}
	// This is the load-bearing assertion: the bar must carry LCP's own
	// histogram (100% poor), not INP's (20% good / 80% poor) even though
	// both are named-eligible via the tie. A bug that names "lcp" correctly
	// but sources the histogram from the wrong accumulator would pass the
	// DistributionMetric check above and fail only here.
	if off.Distribution.PoorPct != 100 || off.Distribution.GoodPct != 0 {
		t.Errorf("distribution = %+v, want 100%% poor / 0%% good (LCP's own data) — INP's data (20%% good / 80%% poor) must not leak in on the tie", off.Distribution)
	}
	if off.DistributionSampleCount != 50 {
		t.Errorf("distribution_sample_count = %d, want 50 (LCP's own sample count)", off.DistributionSampleCount)
	}
}
