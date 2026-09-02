// The three sets this package keeps separate, and the tests that keep them
// separate.
//
//	capabilityVocabulary       what may be spelled at all      -- 8
//	scopeCapabilities          what a scope confers, a CEILING -- 7
//	DefaultGrantCapabilities   what an unasked grant receives  -- 1
//
// Before m131 all three held one member, so all three were the same list and
// nothing in the tree could tell them apart. The identity was TRUE, and every
// test that ranged over one of them would have passed just as well ranging over
// either other. That is why these tests assert EXACT SETS BY VALUE rather than
// membership or length: a proof written as "the default contains
// mcp.sites.read" stays green against a default of all eight, which is the
// exact widening this file exists to catch.
package mcp

import (
	"strings"
	"testing"
)

// capsToStrings renders a capability list for comparison and for failure
// messages, so a failure prints the sets rather than a bare count.
func capsToStrings(caps []Capability) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, string(c))
	}
	return out
}

// sameCaps reports set-and-order equality against an expected literal.
func sameCaps(got []Capability, want []Capability) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// THE TRAP TEST.
// ---------------------------------------------------------------------------

// TestDefaultGrantCapabilitiesIsThePresetNotTheVocabulary is the test that
// would have caught the widening m131's closing note warns about, and it is the
// reason this file exists.
//
// The failure it guards is not a bug anyone would write on purpose. It is
// `func DefaultGrantCapabilities() []Capability { return AllCapabilities() }`
// left untouched while capabilityVocabulary grows from one member to eight --
// a one-line map edit, no compiler error, no existing test red, and every grant
// minted afterwards silently carrying all eight capabilities including a
// content group ADR-062 holds behind ship blockers.
//
// SO IT ASSERTS THE EXACT SET BY VALUE. `set.Allows(CapSitesRead)` would be
// green against all eight. `len(set) > 0` would be green against all eight.
// Only naming the whole expected list catches it.
func TestDefaultGrantCapabilitiesIsThePresetNotTheVocabulary(t *testing.T) {
	want := []Capability{CapSitesRead}

	got := DefaultGrantCapabilities()
	if !sameCaps(got, want) {
		t.Fatalf("DefaultGrantCapabilities() = %v, want exactly %v.\n"+
			"A grant minted with no explicit capability request receives this set "+
			"verbatim (Service.Approve and Service.MintConnection), so anything "+
			"wider here is authority stamped onto a credential whose operator was "+
			"never shown it.",
			capsToStrings(got), capsToStrings(want))
	}

	// THE SECOND HALF, AND THE ONE THAT NAMES THE WIDENING. The equality above
	// pins today's value; this pins the RELATIONSHIP, so restoring
	// `return AllCapabilities()` fails with a message that says what happened
	// rather than only printing two differing lists.
	if len(got) >= len(AllCapabilities()) {
		t.Fatalf("DefaultGrantCapabilities() holds %d of the vocabulary's %d "+
			"capabilities -- the default is the whole vocabulary again.\n"+
			"default=%v\nvocabulary=%v\n"+
			"DefaultGrantCapabilities() must not be AllCapabilities(). That identity "+
			"was true and safe while the vocabulary held one member; against m131's "+
			"eight it stamps every capability onto every newly minted grant, which "+
			"is a credential widening delivered by a map edit.",
			len(got), len(AllCapabilities()),
			capsToStrings(got), capsToStrings(AllCapabilities()))
	}

	// The default must be REACHABLE, or every grant minted without an explicit
	// request authenticates and then reaches nothing -- the half-working
	// connection m127 DECISION 3 forbids. Checked against the ceiling, which is
	// what Authenticate narrows the stored column against.
	ceiling, err := OrgDefaultCapabilities(grantScopes())
	if err != nil {
		t.Fatalf("OrgDefaultCapabilities(grantScopes()): %v", err)
	}
	if _, err := ceiling.NarrowTo(got); err != nil {
		t.Fatalf("the default preset %v is not held by the organisation ceiling %v: %v.\n"+
			"Every grant minted without an explicit request would store this set and "+
			"then be refused by Authenticate on every request.",
			capsToStrings(got), capsToStrings(ceiling.Sorted()), err)
	}
}

// TestMintWithNoRequestedCapabilitiesGetsThePresetNotTheCeiling is the same
// proof one function over, on the OTHER path that mints a grant.
//
// resolveGrantCapabilities returned the CEILING for an empty request, which was
// the default only while the ceiling held one member. Against a seven-member
// ceiling that line is a second, independent copy of the same widening --
// reached by the mint endpoint rather than by the consent screen, so a proof
// aimed only at DefaultGrantCapabilities would miss it entirely.
func TestMintWithNoRequestedCapabilitiesGetsThePresetNotTheCeiling(t *testing.T) {
	// resolveGrantCapabilities reads no Service field, so the zero value is the
	// honest fixture rather than a stub standing in for one.
	svc := &Service{}

	set, err := svc.resolveGrantCapabilities(nil)
	if err != nil {
		t.Fatalf("resolveGrantCapabilities(nil): %v", err)
	}
	if !sameCaps(set.Sorted(), DefaultGrantCapabilities()) {
		t.Fatalf("an empty capability request minted %v, want exactly %v.\n"+
			"An operator who POSTs no capability list has chosen nothing, and the "+
			"answer to 'nobody asked' is the preset, never the widest set the "+
			"organisation ceiling allows.",
			capsToStrings(set.Sorted()), capsToStrings(DefaultGrantCapabilities()))
	}

	// The ceiling is still reachable BY ASKING, which is the other half of the
	// decision: this is a narrower default, not a narrower surface.
	wider := []Capability{CapSitesRead, CapUptimeRead, CapBackupsRead}
	asked, err := svc.resolveGrantCapabilities(&wider)
	if err != nil {
		t.Fatalf("resolveGrantCapabilities(%v): %v -- an operator who explicitly asks "+
			"for a seated, conferred capability must receive it", capsToStrings(wider), err)
	}
	if asked.Len() != len(wider) {
		t.Fatalf("an explicit request for %v minted %v", capsToStrings(wider), capsToStrings(asked.Sorted()))
	}
}

// ---------------------------------------------------------------------------
// The vocabulary and the ceiling, pinned by value.
// ---------------------------------------------------------------------------

// TestVocabularyIsM131sEight pins the Go half of the two-place closed set by
// value. The DATABASE half is proved separately and against the live
// constraint, by TestCapabilityVocabularyMatchesTheDatabaseCheckAsAppRole in
// apps/api/tests -- this one cannot see the database and does not pretend to.
func TestVocabularyIsM131sEight(t *testing.T) {
	want := []Capability{
		CapActivityRead,
		CapBackupsRead,
		CapContentRead,
		CapDiagnosticsRead,
		CapPerformanceRead,
		CapSecurityRead,
		CapSitesRead,
		CapUptimeRead,
	}
	if !sameCaps(AllCapabilities(), want) {
		t.Fatalf("AllCapabilities() = %v, want exactly %v (m131's seated vocabulary, "+
			"alphabetical, which is the order the constraint lists them in)",
			capsToStrings(AllCapabilities()), capsToStrings(want))
	}
}

// TestEveryCapabilityIsARead makes "this build seats no write capability" a
// property that is checked rather than claimed. m131 DECISION 2 froze the
// three-segment form specifically so this is a pattern test: the write side of
// the design uses '.propose' and '.write', and the first member that does not
// end in '.read' is the one that needs the write review.
//
// It also fails on an empty vocabulary, because a loop over an empty set passes
// having checked nothing -- the exact shape this brief names as failure mode 3
// on the database side.
func TestEveryCapabilityIsARead(t *testing.T) {
	all := AllCapabilities()
	if len(all) == 0 {
		t.Fatal("the vocabulary is empty, so this test ranged over nothing and " +
			"would have reported a pass having checked no capability at all")
	}
	for _, c := range all {
		if !strings.HasSuffix(string(c), ".read") {
			t.Fatalf("capability %q does not end in '.read'.\n"+
				"If this is the first write capability, it needs the review m124 "+
				"DECISION 1 built the closed CHECK to force -- and it must not be "+
				"conferred by ScopeRead, which is a read scope.", c)
		}
		if !strings.HasPrefix(string(c), "mcp.") {
			t.Fatalf("capability %q does not carry the frozen 'mcp.' prefix. The "+
				"wireframes' 'site.*' labels are the SCREEN's, not the stored "+
				"string's; adopting one here strands every live grant.", c)
		}
	}
	t.Logf("all %d seated capabilities carry the mcp. prefix and the .read suffix: %v",
		len(all), capsToStrings(all))
}

// TestScopeReadConfersTheSevenReachableGroups pins the CEILING by value, and it
// is deliberately not written as a comparison against AllCapabilities().
//
// `ScopeRead: AllCapabilities()` is the tempting spelling of this map and it
// rebuilds the trap one function over: the first '.propose' capability seated
// in the vocabulary would become conferred by the READ scope silently. The
// literal below is the rule -- the read scope confers the read capabilities --
// and a member that arrives in the vocabulary without arriving here goes red at
// TestContentReadIsKnownButConferredByNoScope's sibling arm rather than being
// conferred by default.
func TestScopeReadConfersTheSevenReachableGroups(t *testing.T) {
	want := []Capability{
		CapActivityRead,
		CapBackupsRead,
		CapDiagnosticsRead,
		CapPerformanceRead,
		CapSecurityRead,
		CapSitesRead,
		CapUptimeRead,
	}
	set, err := OrgDefaultCapabilities([]Scope{ScopeRead})
	if err != nil {
		t.Fatalf("OrgDefaultCapabilities([mcp:read]): %v", err)
	}
	if !sameCaps(set.Sorted(), want) {
		t.Fatalf("the mcp:read ceiling = %v, want exactly %v",
			capsToStrings(set.Sorted()), capsToStrings(want))
	}
}

// TestContentReadIsKnownButConferredByNoScope is the seated-and-unreachable
// stance, held on the Go side, and it is the one asymmetry between the two
// closed sets that is deliberate.
//
// m131 DECISION 3 seats mcp.content.read in the database while nothing can
// grant it: no post or page table exists, no agent command returns post
// content, and ADR-062 holds the content work behind ship blockers including a
// plugin privacy disclosure. The database's version of "nothing writes it" is
// that no INSERT names it; Go's version is that no SCOPE confers it, so
// NarrowTo refuses it and no mint path can store it.
//
// The alternative -- putting it in the ceiling -- lets an operator mint a
// connection holding a capability that reaches nothing today and reaches real
// post content the day those tools land, with no second consent and no
// disclosure. That is a widening arriving as a side effect of a registry edit,
// which is the same shape as the default trap this file's first test guards.
func TestContentReadIsKnownButConferredByNoScope(t *testing.T) {
	// KNOWN: it must be in the vocabulary, because the database CHECK holds it
	// and a name the database accepts and Go does not is refused at a different
	// layer with a different error.
	if !KnownCapability(CapContentRead) {
		t.Fatalf("%q is not in capabilityVocabulary, but m131 seats it in "+
			"mcp_grants_capabilities_vocabulary_check; the two closed sets must "+
			"agree exactly", CapContentRead)
	}

	// NOT CONFERRED: no scope hands it out, so no grant can be minted holding it.
	ceiling, err := OrgDefaultCapabilities(grantScopes())
	if err != nil {
		t.Fatalf("OrgDefaultCapabilities(grantScopes()): %v", err)
	}
	if ceiling.Allows(CapContentRead) {
		t.Fatalf("the organisation ceiling confers %q. Nothing serves it: the tool "+
			"registry registers no tool requiring it, and ADR-062 holds the content "+
			"work behind ship blockers. Confer it in the same diff that ships those "+
			"tools, not before.", CapContentRead)
	}

	// AND THE REFUSAL IS LOUD, not a silent drop. An operator asking for it is
	// told, by name, rather than handed a quietly smaller connection.
	if _, err := ceiling.NarrowTo([]Capability{CapSitesRead, CapContentRead}); err == nil {
		t.Fatalf("a mint request naming %q was accepted; it must be refused whole, "+
			"not trimmed down to the capabilities the ceiling does hold", CapContentRead)
	}
}
