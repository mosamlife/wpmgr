package perf

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/rum"
)

// ---------------------------------------------------------------------------
// GH #195 — fleet CWV pass-rate + worst-offenders must consider all three
// core metrics (LCP, INP, CLS), not just LCP.
// ---------------------------------------------------------------------------

// fleetSiteIDsStub is a minimal SiteLookup that returns a fixed site set,
// ignoring the tenantID argument (fine for a single-tenant unit test).
type fleetSiteIDsStub struct{ ids []uuid.UUID }

func (s *fleetSiteIDsStub) GetSiteURL(context.Context, uuid.UUID, uuid.UUID) (string, error) {
	return "", nil
}
func (s *fleetSiteIDsStub) ListSiteIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return s.ids, nil
}

// fleetInt32Counts builds a NumBuckets int32 slice with a single bucket index
// holding the full sample count.
func fleetInt32Counts(idx int, count int32) []int32 {
	c := make([]int32, rum.NumBuckets)
	c[idx] = count
	return c
}

// newFleetRumHandler builds a Handler wired for rumFleet: ListAllSiteIDs
// returns siteIDs (via fleetSiteIDsStub), and GetHourlyRollupsForSites always
// returns the given rollups regardless of the requested tenant/site/window.
func newFleetRumHandler(siteIDs []uuid.UUID, rollups []rum.HourlyRollup) *Handler {
	svc := NewService(&fakeRepo{}, nil, nil, nil)
	svc.SetAgentClient(nil, &fleetSiteIDsStub{ids: siteIDs})
	reader := &RumResultsReader{
		GetHourlyRollupsForSites: func(context.Context, uuid.UUID, []uuid.UUID, time.Time) ([]rum.HourlyRollup, error) {
			return rollups, nil
		},
	}
	return &Handler{svc: svc, rum: reader}
}

// callRumFleet issues GET /perf/rum/fleet against h and decodes the response.
func callRumFleet(t *testing.T, h *Handler) FleetRumResponse {
	t.Helper()
	engine := gin.New()
	engine.GET("/perf/rum/fleet", func(c *gin.Context) {
		injectPrincipal(c)
		h.rumFleet(c)
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/perf/rum/fleet", nil)
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("rumFleet: expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	var resp FleetRumResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode FleetRumResponse: %v (body: %s)", err, w.Body.String())
	}
	return resp
}

// findOffender returns the worst_offenders entry for siteID, or nil.
func findOffender(resp FleetRumResponse, siteID uuid.UUID) *fleetRumWorstOffender {
	for i := range resp.WorstOffenders {
		if resp.WorstOffenders[i].SiteID == siteID {
			return &resp.WorstOffenders[i]
		}
	}
	return nil
}

// TestRumFleet_goodLCPPoorCLS_excludedFromPassAndFlaggedAsOffender is the core
// GH #195 regression test: a site with a "good" LCP but a "poor" CLS must (a)
// NOT count toward the fleet pass rate and (b) DOES appear in worst_offenders
// with overall_rating reflecting the worst band (CLS "poor"), not LCP's "good".
// Before the fix, the pass-rate loop and offender selection only looked at
// LCP, so this exact site inflated the pass rate and never appeared as an
// offender — contradicting its own per-site dashboard page.
func TestRumFleet_goodLCPPoorCLS_excludedFromPassAndFlaggedAsOffender(t *testing.T) {
	siteA := uuid.New() // good LCP, good INP, poor CLS
	siteB := uuid.New() // good LCP only; INP/CLS never reported (missing data)

	// All samples in bucket 0 ([0,200)) interpolate to p75≈150ms — "good" for
	// both LCP (<=2500) and INP (<=200).
	goodCounts := fleetInt32Counts(0, 50)
	// All samples in bucket 2 ([300,400)) interpolate to p75≈376 milli-units —
	// "poor" for CLS (>250).
	poorCLSCounts := fleetInt32Counts(2, 50)

	rollups := []rum.HourlyRollup{
		{RollupKey: rum.RollupKey{SiteID: siteA, Metric: "lcp"}, SampleCount: 50, BucketCounts: goodCounts, MaxValue: 190},
		{RollupKey: rum.RollupKey{SiteID: siteA, Metric: "inp"}, SampleCount: 50, BucketCounts: goodCounts, MaxValue: 190},
		{RollupKey: rum.RollupKey{SiteID: siteA, Metric: "cls"}, SampleCount: 50, BucketCounts: poorCLSCounts, MaxValue: 390},
		{RollupKey: rum.RollupKey{SiteID: siteB, Metric: "lcp"}, SampleCount: 50, BucketCounts: goodCounts, MaxValue: 190},
	}

	h := newFleetRumHandler([]uuid.UUID{siteA, siteB}, rollups)
	resp := callRumFleet(t, h)

	// --- pass rate: site A must NOT count as passing; site B (missing
	// INP/CLS) must be excluded from the denominator entirely, so the only
	// core-rated site is A, and it fails -> fleet_pass_pct must be 0.
	if resp.FleetPassPct == nil {
		t.Fatalf("expected fleet_pass_pct to be non-nil (site A is core-rated), got nil")
	}
	if *resp.FleetPassPct != 0 {
		t.Errorf("fleet_pass_pct = %v, want 0 (site A fails on CLS, site B is not core-rated so must not count as passing)", *resp.FleetPassPct)
	}

	// --- worst offenders: site A must be present with overall_rating "poor"
	// (the worst of good/good/poor), not omitted (the pre-fix bug) and not
	// mislabeled "good" (LCP's own band).
	off := findOffender(resp, siteA)
	if off == nil {
		t.Fatalf("expected site A (good LCP, poor CLS) in worst_offenders, got none; worst_offenders=%+v", resp.WorstOffenders)
	}
	if off.OverallRating != "poor" {
		t.Errorf("site A overall_rating = %q, want %q (worst of lcp=good/inp=good/cls=poor)", off.OverallRating, "poor")
	}
	if off.LCPP75 == nil || off.INPP75 == nil || off.CLSP75 == nil {
		t.Errorf("expected all three p75 fields populated for site A, got lcp=%v inp=%v cls=%v", off.LCPP75, off.INPP75, off.CLSP75)
	}

	// --- site B (missing INP/CLS, good LCP only) must NOT appear as an
	// offender: its only rated metric (LCP) is "good", so nothing about it is
	// poor/needs-improvement.
	if bOff := findOffender(resp, siteB); bOff != nil {
		t.Errorf("site B (LCP-only, good) should not appear in worst_offenders, got %+v", bOff)
	}
}

// TestRumFleet_lcpOnlyPoor_offenderButExcludedFromPassDenominator verifies the
// two selection criteria are independently correct: a site rated poor on ONLY
// LCP (INP/CLS never reported) IS an offender (any rated core metric poor/NI
// qualifies), but is excluded from the fleet-pass-rate denominator because it
// is not core-rated (not all three metrics present).
func TestRumFleet_lcpOnlyPoor_offenderButExcludedFromPassDenominator(t *testing.T) {
	siteF := uuid.New()

	// Bucket 17 lower bound is 4500ms, already > LCP's poor threshold (4000),
	// so this is poor regardless of within-bucket interpolation.
	poorLCPCounts := fleetInt32Counts(17, 50)

	rollups := []rum.HourlyRollup{
		{RollupKey: rum.RollupKey{SiteID: siteF, Metric: "lcp"}, SampleCount: 50, BucketCounts: poorLCPCounts, MaxValue: 4900},
	}

	h := newFleetRumHandler([]uuid.UUID{siteF}, rollups)
	resp := callRumFleet(t, h)

	// No core-rated sites at all (siteF is missing INP/CLS) -> pass pct nil.
	if resp.FleetPassPct != nil {
		t.Errorf("fleet_pass_pct = %v, want nil (no site is core-rated: siteF is missing INP/CLS)", *resp.FleetPassPct)
	}

	off := findOffender(resp, siteF)
	if off == nil {
		t.Fatalf("expected site F (poor LCP, no INP/CLS data) in worst_offenders, got none")
	}
	if off.OverallRating != "poor" {
		t.Errorf("site F overall_rating = %q, want %q", off.OverallRating, "poor")
	}
	if off.INPP75 != nil || off.CLSP75 != nil {
		t.Errorf("expected inp_p75/cls_p75 nil for site F (never reported), got inp=%v cls=%v", off.INPP75, off.CLSP75)
	}
}

// TestRumFleet_normalizeRatingHyphenation verifies the fleet response never
// emits the underscore form "needs_improvement" — the frontend contract uses
// the hyphenated "needs-improvement".
func TestRumFleet_normalizeRatingHyphenation(t *testing.T) {
	siteG := uuid.New()
	// LCP bucket 13 ([2500,3000)) is wholly "needs_improvement" per
	// cwvThresholds (lo=2500 >= goodUpper, hi=3000 <= niUpper) regardless of
	// within-bucket interpolation, so cwvRating returns the underscore form
	// that normalizeRating/worstOfThree must convert to "needs-improvement".
	niLCPCounts := fleetInt32Counts(13, 50)

	rollups := []rum.HourlyRollup{
		{RollupKey: rum.RollupKey{SiteID: siteG, Metric: "lcp"}, SampleCount: 50, BucketCounts: niLCPCounts, MaxValue: 2900},
	}
	h := newFleetRumHandler([]uuid.UUID{siteG}, rollups)
	resp := callRumFleet(t, h)

	off := findOffender(resp, siteG)
	if off == nil {
		t.Fatalf("expected site G in worst_offenders")
	}
	if off.OverallRating != "needs-improvement" {
		t.Errorf("overall_rating = %q, want hyphenated %q", off.OverallRating, "needs-improvement")
	}
}
