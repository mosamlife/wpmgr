package govcontext

import (
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// TestCheckNoWiden_RefusesRemovingAHigherLayerRestriction is ADR-064
// property 1's core-logic proof: a lower layer attempting to widen a
// restriction is refused, with the reason naming the restriction and the
// layer that set it.
//
// This test was run against a neutered checkNoWiden (widen.go's body
// replaced with `return nil`) to confirm it goes RED first:
//
//	$ go test ./internal/govcontext/... -run TestCheckNoWiden_RefusesRemovingAHigherLayerRestriction -v
//	--- FAIL: TestCheckNoWiden_RefusesRemovingAHigherLayerRestriction (0.00s)
//	    widen_test.go:41: checkNoWiden returned nil, want a context_widen_forbidden error
//	FAIL
//
// Restored, it is GREEN:
//
//	$ go test ./internal/govcontext/... -run TestCheckNoWiden_RefusesRemovingAHigherLayerRestriction -v
//	--- PASS: TestCheckNoWiden_RefusesRemovingAHigherLayerRestriction (0.00s)
//	PASS
func TestCheckNoWiden_RefusesRemovingAHigherLayerRestriction(t *testing.T) {
	higher := []namedLayer{
		{Layer: 2, Name: "organisation default", Restrictions: RestrictionSet{
			ForbiddenDomains: []string{"evil.example.com", "also-evil.example.com"},
		}},
	}
	// The proposed site-level write drops "evil.example.com" — exactly the
	// widen ADR-064 Decision 4 exists to refuse.
	proposed := RestrictionSet{ForbiddenDomains: []string{"also-evil.example.com"}}

	err := checkNoWiden(proposed, higher)
	if err == nil {
		t.Fatal("checkNoWiden returned nil, want a context_widen_forbidden error")
	}
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("checkNoWiden returned a non-domain error: %v", err)
	}
	if de.Kind != domain.KindConflict {
		t.Errorf("Kind = %v, want KindConflict (409)", de.Kind)
	}
	if de.Code != "context_widen_forbidden" {
		t.Errorf("Code = %q, want context_widen_forbidden", de.Code)
	}
	if got := domain.HTTPStatus(err); got != 409 {
		t.Errorf("HTTPStatus = %d, want 409", got)
	}
	// The reason must name the restriction (the field) and the layer that set
	// it — Decision 13: "the reason names the field and the layer that
	// blocked it".
	if de.Details["field"] != "forbidden_domains" {
		t.Errorf("Details[field] = %v, want forbidden_domains", de.Details["field"])
	}
	if de.Details["layer"] != 2 {
		t.Errorf("Details[layer] = %v, want 2", de.Details["layer"])
	}
	if de.Details["layer_name"] != "organisation default" {
		t.Errorf("Details[layer_name] = %v, want %q", de.Details["layer_name"], "organisation default")
	}
	items, _ := de.Details["removed_items"].([]string)
	if len(items) != 1 || items[0] != "evil.example.com" {
		t.Errorf("Details[removed_items] = %v, want [evil.example.com]", de.Details["removed_items"])
	}
}

// TestCheckNoWiden_ChecksEveryHigherLayerNotOnlyTheNearest proves Decision
// 4's "against every layer above it, not only against the nearest one": a
// site write that satisfies its organisation's policy but still drops
// something WPmgr's layer-1 policy set must still be refused, naming layer 1.
func TestCheckNoWiden_ChecksEveryHigherLayerNotOnlyTheNearest(t *testing.T) {
	higher := []namedLayer{
		{Layer: 1, Name: "WPmgr security policy", Restrictions: RestrictionSet{ForbiddenTools: []string{"delete_site"}}},
		{Layer: 2, Name: "organisation default", Restrictions: RestrictionSet{}}, // org sets nothing
	}
	proposed := RestrictionSet{} // drops layer 1's forbidden tool entirely

	err := checkNoWiden(proposed, higher)
	if err == nil {
		t.Fatal("checkNoWiden returned nil, want a context_widen_forbidden error naming layer 1")
	}
	de, _ := domain.AsDomain(err)
	if de.Details["layer"] != 1 {
		t.Errorf("Details[layer] = %v, want 1 (the ONLY layer that actually set anything)", de.Details["layer"])
	}
}

// TestCheckNoWiden_AllowsNarrowingOrAdding is the honest-cases control: a
// write that ADDS restrictions beyond what a higher layer set, or leaves a
// field untouched, must NOT be refused. A guard that reddens correct work
// guards nothing.
func TestCheckNoWiden_AllowsNarrowingOrAdding(t *testing.T) {
	higher := []namedLayer{
		{Layer: 2, Name: "organisation default", Restrictions: RestrictionSet{
			ForbiddenTopics: []string{"medical_advice"},
		}},
	}
	cases := []struct {
		name     string
		proposed RestrictionSet
	}{
		{"exact match", RestrictionSet{ForbiddenTopics: []string{"medical_advice"}}},
		{"adds a further restriction", RestrictionSet{ForbiddenTopics: []string{"medical_advice", "legal_advice"}}},
		{"different field entirely, untouched field matches", RestrictionSet{
			ForbiddenTopics: []string{"medical_advice"}, ForbiddenTools: []string{"delete_media"},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := checkNoWiden(c.proposed, higher); err != nil {
				t.Errorf("checkNoWiden rejected a non-widening write: %v", err)
			}
		})
	}
}

// TestCheckNoWiden_NoGuidanceEquivalent documents, rather than merely
// asserting by omission, that GuidanceSet has no widen-check. ADR-064
// Decision 1 and the Consequences section are explicit that "wider" and
// "narrower" are not defined relations over free text, and that building one
// would be "a check that always passes without ever having tested the thing
// it claims to test" — worse than the honest absence. This test exists so a
// future reader who searches for "widen" and finds only RestrictionSet
// covered does not read the gap as an oversight.
func TestCheckNoWiden_NoGuidanceEquivalent(t *testing.T) {
	// checkNoWiden's signature only accepts RestrictionSet — there is no
	// GuidanceSet-shaped overload to call. This test is intentionally a
	// compile-time statement as much as a runtime one: the absence IS the
	// assertion.
	var _ = func(proposed RestrictionSet, higher []namedLayer) *domain.Error {
		return checkNoWiden(proposed, higher)
	}
}
