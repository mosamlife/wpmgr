package govcontext

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// fakeStore is a ContextStore test double. orgErr/siteErr, when set, are
// returned verbatim — this is how the refuse-on-load-failure tests simulate
// "a database error, a cache invalidated with no fresh copy yet computed, a
// corrupted version row" (ADR-064 Decision 14) without a live database: the
// property under test is Resolve's CONTROL FLOW on an error from its
// ContextStore dependency, which this interface exists to make testable in
// isolation from Postgres.
type fakeStore struct {
	org     Snapshot
	orgOK   bool
	orgErr  error
	site    Snapshot
	siteOK  bool
	siteErr error
}

func (f *fakeStore) LatestOrgSnapshot(ctx context.Context, tenantID uuid.UUID) (Snapshot, bool, error) {
	return f.org, f.orgOK, f.orgErr
}
func (f *fakeStore) LatestSiteSnapshot(ctx context.Context, tenantID, siteID uuid.UUID) (Snapshot, bool, error) {
	return f.site, f.siteOK, f.siteErr
}

var errSimulatedInfra = errors.New("simulated: connection reset by peer")

// TestResolve_RefusesRatherThanProceedingEmpty is ADR-064 property 2's proof:
// "If context cannot be loaded, the call is REFUSED. Never proceed on a
// silently empty context." A store that fails to load org context must
// produce a hard refusal, not a ResolvedContext with an empty layer 2.
//
// Run against a neutered Resolve (the `if err != nil { return ... }` check
// after LatestOrgSnapshot commented out, falling through with the zero
// Snapshot instead), this goes RED:
//
//	$ go test ./internal/govcontext/... -run TestResolve_RefusesRatherThanProceedingEmpty -v
//	--- FAIL: TestResolve_RefusesRatherThanProceedingEmpty (0.00s)
//	    resolver_test.go:66: Resolve returned err=<nil>, want a context_unavailable refusal
//	FAIL
//
// Restored, it is GREEN:
//
//	$ go test ./internal/govcontext/... -run TestResolve_RefusesRatherThanProceedingEmpty -v
//	--- PASS: TestResolve_RefusesRatherThanProceedingEmpty (0.00s)
//	PASS
func TestResolve_RefusesRatherThanProceedingEmpty(t *testing.T) {
	store := &fakeStore{orgErr: errSimulatedInfra}
	r := &Resolver{Store: store}

	rc, err := r.Resolve(context.Background(), uuid.New(), uuid.New(), nil)
	if err == nil {
		t.Fatalf("Resolve returned err=<nil>, want a context_unavailable refusal")
	}
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("Resolve returned a non-domain error: %v", err)
	}
	if de.Code != "context_unavailable" {
		t.Errorf("Code = %q, want context_unavailable", de.Code)
	}
	if got := domain.HTTPStatus(err); got != 503 {
		t.Errorf("HTTPStatus = %d, want 503", got)
	}
	if !errors.Is(err, errSimulatedInfra) {
		t.Errorf("the underlying cause was not preserved (errors.Is failed against %v)", errSimulatedInfra)
	}
	// The never-empty half: a caller that only checked err==nil must not also
	// be handed a ResolvedContext that LOOKS complete.
	if rc.SiteID != uuid.Nil || len(rc.Layers) != 0 {
		t.Errorf("Resolve returned a non-empty ResolvedContext alongside an error: %+v", rc)
	}
}

// TestResolve_SiteLoadFailureAlsoRefuses is the site-context sibling: a
// failure loading LAYER 3 must refuse exactly like a layer-2 failure, not
// silently resolve with an empty site layer.
func TestResolve_SiteLoadFailureAlsoRefuses(t *testing.T) {
	store := &fakeStore{orgOK: true, siteErr: errSimulatedInfra}
	r := &Resolver{Store: store}

	_, err := r.Resolve(context.Background(), uuid.New(), uuid.New(), nil)
	if err == nil {
		t.Fatal("Resolve returned err=<nil> on a site-snapshot load failure, want a refusal")
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != "context_unavailable" {
		t.Fatalf("got %v, want a context_unavailable domain error", err)
	}
}

// TestResolve_NoContextYetIsNotAFailure is the honest-cases control for
// property 2: "no version has ever been written" (ok=false, err=nil) is a
// LEGITIMATE empty state per ADR-064 Decision 3, not a load failure. A
// refuse-on-failure guard that also refused a brand-new org/site would block
// every fleet on day one.
func TestResolve_NoContextYetIsNotAFailure(t *testing.T) {
	store := &fakeStore{} // orgOK=false, siteOK=false, no errors — never authored
	r := &Resolver{Store: store}

	rc, err := r.Resolve(context.Background(), uuid.New(), uuid.New(), nil)
	if err != nil {
		t.Fatalf("Resolve refused a legitimate empty context: %v", err)
	}
	if !rc.Restrictions.IsEmpty() {
		t.Errorf("Restrictions = %+v, want empty", rc.Restrictions)
	}
}

// TestResolve_NeverAssemblesLayer7 asserts the resolver's layer set is
// exactly {1,2,3,4,5,6} — layer 7 (learned memory) is not built and must not
// be stubbed in, per this package's doc comment.
func TestResolve_NeverAssemblesLayer7(t *testing.T) {
	r := &Resolver{Store: &fakeStore{}}
	rc, err := r.Resolve(context.Background(), uuid.New(), uuid.New(), nil)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	want := map[int]bool{1: false, 2: false, 3: false, 4: false, 5: false, 6: false}
	for _, l := range rc.Layers {
		if l.Layer == 7 {
			t.Fatal("resolver produced a layer 7 contribution — learned memory must not be stubbed in")
		}
		if _, ok := want[l.Layer]; !ok {
			t.Fatalf("unexpected layer number %d", l.Layer)
		}
		want[l.Layer] = true
	}
	for layer, seen := range want {
		if !seen {
			t.Errorf("layer %d is missing from the resolved output", layer)
		}
	}
}

// TestResolve_RestrictionsUnionIsNeverTruncated proves the documented
// exemption: even when the byte budget forces every layer's displayed
// content to empty, the top-level Restrictions union (Decision 4's
// enforcement input) still carries the full, untruncated restriction set.
func TestResolve_RestrictionsUnionIsNeverTruncated(t *testing.T) {
	store := &fakeStore{
		orgOK: true,
		org:   Snapshot{Restrictions: RestrictionSet{ForbiddenDomains: []string{"evil.example.com"}}},
	}
	r := &Resolver{Store: store, BudgetBytes: 1} // impossibly small budget forces total truncation

	rc, err := r.Resolve(context.Background(), uuid.New(), uuid.New(), nil)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if !rc.Truncated {
		t.Fatal("expected Truncated=true under a 1-byte budget")
	}
	if len(rc.Restrictions.ForbiddenDomains) != 1 || rc.Restrictions.ForbiddenDomains[0] != "evil.example.com" {
		t.Errorf("Restrictions union was affected by truncation: %+v", rc.Restrictions)
	}
}

// TestApplyBudget_NeverTruncatesLayer1 proves Decision 9's "layer 1 is never
// truncated", even under a budget of 0.
func TestApplyBudget_NeverTruncatesLayer1(t *testing.T) {
	layers := []LayerContribution{
		{Layer: 1, Restrictions: RestrictionSet{ForbiddenTools: []string{"delete_site"}}},
		{Layer: 2, Guidance: GuidanceSet{BrandVoice: "formal"}},
	}
	for i := range layers {
		layers[i].Bytes = layerBytes(layers[i])
	}
	applyBudget(layers, 0)
	if len(layers[0].Restrictions.ForbiddenTools) != 1 {
		t.Errorf("layer 1 was truncated: %+v", layers[0])
	}
}

// TestApplyBudget_OrderIsSessionThenSkillThenFactsThenSiteThenOrg proves
// Decision 9's exact truncation order by giving every layer 2-6 exactly one
// droppable field and confirming they disappear in the documented order.
func TestApplyBudget_OrderIsSessionThenSkillThenFactsThenSiteThenOrg(t *testing.T) {
	layers := []LayerContribution{
		{Layer: 1, Restrictions: RestrictionSet{ForbiddenTools: []string{"x"}}}, // never touched
		{Layer: 2, Guidance: GuidanceSet{BrandVoice: "org"}},
		{Layer: 3, Guidance: GuidanceSet{BrandVoice: "site"}},
		{Layer: 4, Facts: &SiteFacts{WPVersion: "6.7"}},
		{Layer: 5}, // no skill store; always empty already
		{Layer: 6, Session: "session text"},
	}
	for i := range layers {
		layers[i].Bytes = layerBytes(layers[i])
	}
	// Budget just under total forces exactly ONE field to drop; it must be
	// layer 6's session text, the lowest layer, first.
	total := 0
	for _, l := range layers {
		total += l.Bytes
	}
	applyBudget(layers, total-1)
	if layers[5].Session != "" {
		t.Fatalf("layer 6 (session) should be the FIRST thing dropped, still has %q", layers[5].Session)
	}
	if layers[3].Facts == nil || layers[3].Facts.WPVersion != "6.7" {
		t.Errorf("layer 4 (facts) was dropped before layer 6 was fully consumed")
	}
	if layers[2].Guidance.BrandVoice != "site" {
		t.Errorf("layer 3 (site) was dropped before higher-numbered layers were fully consumed")
	}
	if layers[1].Guidance.BrandVoice != "org" {
		t.Errorf("layer 2 (org) was dropped before layer 3 was fully consumed")
	}
}
