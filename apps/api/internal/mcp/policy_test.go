package mcp

import (
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// The capability set: zero value allows nothing, and there is no widening path.
// ---------------------------------------------------------------------------

// TestZeroCapabilitySetAllowsNothing is the SiteSet proof on the other axis.
// The whole reason CapabilitySet is a struct is that a []Capability's zero
// value reads as "no filter" at any call site that forgets to check it.
func TestZeroCapabilitySetAllowsNothing(t *testing.T) {
	var zero CapabilitySet
	empty := NewCapabilitySet(nil)

	for _, s := range []CapabilitySet{zero, empty, NewCapabilitySet([]Capability{})} {
		if !s.IsEmpty() || s.Len() != 0 {
			t.Fatalf("set reports IsEmpty=%v Len=%d, want empty", s.IsEmpty(), s.Len())
		}
		for _, c := range AllCapabilities() {
			if s.Allows(c) {
				t.Fatalf("an empty capability set allowed %q", c)
			}
		}
		// An unknown name must not be allowed either -- an unrecognised
		// capability is not a weaker grant, it is no grant.
		if s.Allows("mcp.anything.at.all") {
			t.Fatal("an empty capability set allowed an unknown capability")
		}
	}
}

// TestUnknownCapabilityIsNotAdmitted proves NewCapabilitySet drops names
// outside the vocabulary rather than carrying them. Dropping only ever removes
// authority here; the place where an unknown name must REFUSE is NarrowTo, and
// TestNarrowToRefusesWiderThanDefault covers that.
func TestUnknownCapabilityIsNotAdmitted(t *testing.T) {
	s := NewCapabilitySet([]Capability{CapSitesRead, "mcp.sites.restart", ""})
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1; unknown names were admitted: %v", s.Len(), s.Sorted())
	}
	if s.Allows("mcp.sites.restart") || s.Allows("") {
		t.Fatal("an unknown capability was admitted into the set")
	}
	if !s.Allows(CapSitesRead) {
		t.Fatal("the known capability was dropped")
	}
}

// ---------------------------------------------------------------------------
// Org default
// ---------------------------------------------------------------------------

// TestOrgDefaultRefusesAbsence: no scopes means no capabilities AND an error,
// never the full set. Absence is refusal, exactly as in ParseRequestedScopes.
func TestOrgDefaultRefusesAbsence(t *testing.T) {
	for _, scopes := range [][]Scope{nil, {}} {
		set, err := OrgDefaultCapabilities(scopes)
		if err == nil {
			t.Fatalf("OrgDefaultCapabilities(%v) succeeded with %v", scopes, set.Sorted())
		}
		if !set.IsEmpty() {
			t.Fatalf("the refusal path returned a non-empty set: %v", set.Sorted())
		}
		de, ok := domain.AsDomain(err)
		if !ok || de.Code != ErrCodeCapabilityUnmapped {
			t.Fatalf("err = %v, want %s", err, ErrCodeCapabilityUnmapped)
		}
	}
}

// TestOrgDefaultRefusesAnUnmappedScope: a scope with no capability mapping is
// refused, NOT skipped. Skipping is fail-closed and still wrong -- see the
// comment on scopeCapabilities.
func TestOrgDefaultRefusesAnUnmappedScope(t *testing.T) {
	set, err := OrgDefaultCapabilities([]Scope{ScopeRead, "mcp:write"})
	if err == nil {
		t.Fatalf("an unmapped scope was silently skipped; got %v", set.Sorted())
	}
	if !set.IsEmpty() {
		t.Fatalf("the refusal path returned a non-empty set: %v", set.Sorted())
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != ErrCodeCapabilityUnmapped {
		t.Fatalf("err = %v, want %s", err, ErrCodeCapabilityUnmapped)
	}
}

// TestEveryRecognisedScopeHasACapabilityMapping is the drift guard that makes
// scopeCapabilities total over recognisedScopes. Without it, adding a scope and
// forgetting the mapping turns every connection holding it into a 500 at
// Authenticate -- discovered in production rather than here.
func TestEveryRecognisedScopeHasACapabilityMapping(t *testing.T) {
	if len(recognisedScopes) == 0 {
		t.Fatal("recognisedScopes is empty; this guard would pass vacuously")
	}
	for s := range recognisedScopes {
		caps, ok := scopeCapabilities[s]
		if !ok {
			t.Errorf("recognised scope %q has no capability mapping", s)
			continue
		}
		if len(caps) == 0 {
			t.Errorf("recognised scope %q maps to no capability, so it confers nothing", s)
		}
		for _, c := range caps {
			if !KnownCapability(c) {
				t.Errorf("scope %q maps to %q, which is not in the capability vocabulary", s, c)
			}
		}
	}
}

// TestGrantScopesIsExactOnlyWhileOneScopeExists is a TRIPWIRE, not a property
// test. It asserts the precondition that makes grantScopes' constant honest.
//
// Authenticate hands OrgDefaultCapabilities a constant instead of the grant's
// own scopes, which is exact only while every live grant holds the same single
// scope. Adding a second recognised scope silently converts that constant into
// a WIDENING: a connection granted only the new scope keeps being handed
// ScopeRead's capabilities, and no other test in this package notices, because
// TestEveryRecognisedScopeHasACapabilityMapping pins the map's totality rather
// than Authenticate's input.
//
// So this fails the moment recognisedScopes grows. The fix when it does is NOT
// to update the number here: it is to store the grant's scopes and read them,
// then delete this test.
func TestGrantScopesIsExactOnlyWhileOneScopeExists(t *testing.T) {
	if len(recognisedScopes) != 1 {
		t.Fatalf("recognisedScopes now holds %d scopes, so grantScopes()'s constant is no longer "+
			"exact: a connection granted only one of them is still handed %v's capabilities by "+
			"Authenticate, which WIDENS it. Read the grant's own scopes instead of a constant "+
			"(mcp_grants needs the column first -- see m124 DECISION 1), then delete this test. "+
			"Do not just update the count.", len(recognisedScopes), ScopeRead)
	}
	got := grantScopes()
	if len(got) != 1 || got[0] != ScopeRead {
		t.Fatalf("grantScopes() = %v, want exactly [%v]", got, ScopeRead)
	}
	if _, ok := recognisedScopes[got[0]]; !ok {
		t.Fatalf("grantScopes() returns %v, which is not a recognised scope", got[0])
	}
}

// TestOrgDefaultForTheReadScope pins the actual Phase 1 answer.
func TestOrgDefaultForTheReadScope(t *testing.T) {
	set, err := OrgDefaultCapabilities([]Scope{ScopeRead})
	if err != nil {
		t.Fatalf("OrgDefaultCapabilities: %v", err)
	}
	if !set.Allows(CapSitesRead) {
		t.Fatalf("mcp:read did not confer %q; got %v", CapSitesRead, set.Sorted())
	}
	// Duplication adds no authority, matching ParseRequestedScopes.
	dup, err := OrgDefaultCapabilities([]Scope{ScopeRead, ScopeRead})
	if err != nil || dup.Len() != set.Len() {
		t.Fatalf("a repeated scope changed the set: %v vs %v (err %v)", dup.Sorted(), set.Sorted(), err)
	}
}

// ---------------------------------------------------------------------------
// Per-connection narrowing
// ---------------------------------------------------------------------------

// TestNarrowToHonoursANarrowerConnection: a connection narrower than the org
// default keeps exactly what it asked for.
func TestNarrowToHonoursANarrowerConnection(t *testing.T) {
	// A two-capability default, built directly so the proof does not depend on
	// the Phase 1 vocabulary happening to hold more than one entry.
	def := NewCapabilitySet(AllCapabilities())
	if def.IsEmpty() {
		t.Fatal("the vocabulary is empty; this proof would be vacuous")
	}

	first := AllCapabilities()[0]
	narrowed, err := def.NarrowTo([]Capability{first})
	if err != nil {
		t.Fatalf("NarrowTo: %v", err)
	}
	if !narrowed.Allows(first) {
		t.Fatalf("the requested capability was not kept: %v", narrowed.Sorted())
	}
	if narrowed.Len() != 1 {
		t.Fatalf("narrowing produced %d capabilities, want 1: %v", narrowed.Len(), narrowed.Sorted())
	}
	// The default is untouched: narrowing must not mutate the ceiling it
	// narrows, or one connection's configuration would silently reshape another's.
	if def.Len() != len(AllCapabilities()) {
		t.Fatalf("NarrowTo mutated the org default: %v", def.Sorted())
	}
}

// TestNarrowToRefusesWiderThanDefault is the other half of the narrowing proof
// the slice owes: a per-connection set WIDER than the org default is REFUSED,
// never granted, and never quietly reduced to the intersection.
func TestNarrowToRefusesWiderThanDefault(t *testing.T) {
	// A default that holds nothing at all: every request is wider.
	narrowDefault := NewCapabilitySet(nil)
	if _, err := narrowDefault.NarrowTo([]Capability{CapSitesRead}); err == nil {
		t.Fatal("an empty default granted a capability it does not hold")
	}

	// A default that holds CapSitesRead, asked for something outside the
	// vocabulary entirely. Refuse, do not drop.
	def := NewCapabilitySet([]Capability{CapSitesRead})
	got, err := def.NarrowTo([]Capability{CapSitesRead, "mcp.sites.restart"})
	if err == nil {
		t.Fatalf("a capability outside the default was silently dropped; got %v", got.Sorted())
	}
	if !got.IsEmpty() {
		t.Fatalf("the refusal path returned a non-empty set: %v", got.Sorted())
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != ErrCodeCapabilityWiderThanDefault {
		t.Fatalf("err = %v, want %s", err, ErrCodeCapabilityWiderThanDefault)
	}
}

// TestNarrowToRefusesAnEmptyRequest: "narrow me to nothing" is not read as
// "keep the default" (an absence widening a grant) and is not read as a live
// connection that reaches nothing. It is refused.
func TestNarrowToRefusesAnEmptyRequest(t *testing.T) {
	def := NewCapabilitySet([]Capability{CapSitesRead})
	for _, req := range [][]Capability{nil, {}} {
		got, err := def.NarrowTo(req)
		if err == nil {
			t.Fatalf("NarrowTo(%v) succeeded with %v", req, got.Sorted())
		}
		if got.Allows(CapSitesRead) {
			t.Fatal("an empty narrowing request returned the org default")
		}
	}
}
