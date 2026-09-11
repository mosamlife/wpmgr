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
	// The loops below range over AllCapabilities(), so an empty vocabulary
	// would make this report PASS having checked nothing. Same class as the
	// registry guards -- see nonEmptyRegistry in registry_test.go.
	if len(AllCapabilities()) == 0 {
		t.Fatal("the capability vocabulary is empty, so the loops below check nothing")
	}

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

// TestGrantScopesIsAPerGrantReadAndNeverAConstant IS THE RENAMED TRIPWIRE.
//
// It used to be TestGrantScopesIsExactOnlyWhileOneScopeExists, and it was a
// tripwire rather than a property test: it asserted `len(recognisedScopes) == 1`
// so that adding a second scope went red and forced whoever did it to replace
// grantScopes' constant with a real per-grant read. m136 added
// mcp_grants.oauth_scopes and that read now exists, so the precondition the old
// name asserted is no longer the thing being defended -- and a test whose name
// asserts something the build no longer does is worse than no test.
//
// IT IS NOT DELETED AND IT IS NOT WEAKER. The old test's own instructions said
// to delete it once the read landed; that would have left the read itself
// unpinned, and the read is where the failure direction lives. So it is
// renamed and repointed at the property the constant's removal bought:
// grantScopes ANSWERS FROM ITS ARGUMENT. Every case below fails against the old
// `return []Scope{ScopeRead}` body.
func TestGrantScopesIsAPerGrantReadAndNeverAConstant(t *testing.T) {
	// 1. IT REFLECTS THE ROW. A constant returns {mcp:read} for every input,
	// so a row holding something else proves the argument is read at all.
	got := grantScopes([]string{"mcp:write-someday"})
	if len(got) != 1 || got[0] != Scope("mcp:write-someday") {
		t.Fatalf("grantScopes([mcp:write-someday]) = %v, want exactly "+
			"[mcp:write-someday].\nIf this returned [%v] the function is still "+
			"answering from a constant and every grant shares one ceiling.",
			got, ScopeRead)
	}

	// 2. IT DOES NOT FILTER. This is the failure direction that matters and it
	// is capabilitiesFromColumn's rule verbatim: trimming the unknown entry
	// would hand OrgDefaultCapabilities {mcp:read} alone, and the grant would
	// authenticate as though its row carried only the known scope.
	mixed := grantScopes([]string{string(ScopeRead), "mcp:not-a-scope"})
	if len(mixed) != 2 {
		t.Fatalf("grantScopes dropped an unrecognised scope: got %v from "+
			"{mcp:read, mcp:not-a-scope}.\nA trimmed read is a WIDENING wearing "+
			"the shape of a narrowing: the row says two things, the ceiling is "+
			"computed from one, and nobody learns the two disagreed.", mixed)
	}

	// 3. AND THE UNKNOWN ENTRY REFUSES THE WHOLE SET rather than shrinking it.
	// Step 2 only proves the read carried it; this proves carrying it matters.
	if set, err := OrgDefaultCapabilities(mixed); err == nil {
		t.Fatalf("a scope set containing an unrecognised scope resolved to %v.\n"+
			"It must REFUSE. Resolving it means a row carrying a scope outside "+
			"this build's registry authenticates holding the known scopes' "+
			"capabilities, which is exactly what not filtering the read exists "+
			"to prevent.", set.Sorted())
	}

	// 4. NIL AND EMPTY ARE NO AUTHORITY, NEVER UNRESTRICTED. The database makes
	// the value unrepresentable (m136 DECISION 4), so reaching either means a
	// writer outside that constraint -- and the answer to "I cannot tell what
	// this grant may do" is nothing at all.
	for _, stored := range [][]string{nil, {}} {
		if sc := grantScopes(stored); len(sc) != 0 {
			t.Fatalf("grantScopes(%v) = %v, want an empty slice", stored, sc)
		}
		if set, err := OrgDefaultCapabilities(grantScopes(stored)); err == nil {
			t.Fatalf("an empty scope column resolved to %v instead of refusing.\n"+
				"Absence must never widen.", set.Sorted())
		}
	}
}

// TestDefaultGrantScopesIsTheReadScopeAndNothingElse pins what a grant nobody
// stated terms for RECEIVES, so a future edit cannot widen it silently.
//
// This is the pin the old tripwire's `len(recognisedScopes) == 1` assertion
// used to provide by accident, moved to the thing it was actually protecting.
// recognisedScopes may now grow -- that is what m136 made safe -- but a wider
// REGISTRY must not become a wider DEFAULT: every grant this surface has ever
// minted holds exactly {mcp:read}, which is what m136's backfill wrote down,
// and the answer to "nobody asked" is never "everything we have".
func TestDefaultGrantScopesIsTheReadScopeAndNothingElse(t *testing.T) {
	got := DefaultGrantScopes()
	if len(got) != 1 || got[0] != ScopeRead {
		t.Fatalf("DefaultGrantScopes() = %v, want exactly [%v].\n"+
			"Widening the default hands new authority to every grant minted "+
			"without an explicit request, with no second consent from anyone. A "+
			"new scope belongs in recognisedScopes and in an operator's explicit "+
			"choice, not in the preset.", got, ScopeRead)
	}
	if _, ok := recognisedScopes[got[0]]; !ok {
		t.Fatalf("DefaultGrantScopes() returns %v, which is not a recognised scope", got[0])
	}

	// It must also be REACHABLE: a preset that resolves to no capability mints
	// a credential that authenticates and reaches nothing.
	set, err := OrgDefaultCapabilities(got)
	if err != nil {
		t.Fatalf("the default scope set %v confers no capability: %v", got, err)
	}
	if set.IsEmpty() {
		t.Fatalf("the default scope set %v resolved to an empty ceiling", got)
	}
}

// TestScopeNamesRoundTripsThroughTheColumn pins the pair that crosses the
// database boundary. scopeNames writes the column and grantScopes reads it, and
// a mismatch between them is invisible to the compiler.
func TestScopeNamesRoundTripsThroughTheColumn(t *testing.T) {
	in := []Scope{ScopeRead}
	out := grantScopes(scopeNames(in))
	if len(out) != len(in) || out[0] != in[0] {
		t.Fatalf("scopeNames -> grantScopes round trip changed %v into %v", in, out)
	}
	if names := scopeNames(nil); len(names) != 0 {
		t.Fatalf("scopeNames(nil) = %v, want an empty slice -- a nil column is "+
			"refused by the not-empty CHECK, not repaired here", names)
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
