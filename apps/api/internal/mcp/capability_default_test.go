// The three sets this package keeps separate, and the tests that keep them
// separate.
//
//	capabilityVocabulary       what may be spelled at all
//	scopeCapabilities          what a scope confers, a CEILING
//	DefaultGrantCapabilities   what an unasked grant receives
//
// The vocabulary is wider than the ceiling by one deliberately unreachable
// member, mcp.content.read (m131): no scope confers it, so it can be neither
// minted nor authenticated. mcp.cache.purge (m135) is conferred by mcp:cache
// (m150) and by nothing else, and never by default; see policy.go.
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

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
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
	ceiling, err := OrgDefaultCapabilities(DefaultGrantScopes())
	if err != nil {
		t.Fatalf("OrgDefaultCapabilities(DefaultGrantScopes()): %v", err)
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

	set, err := svc.resolveGrantCapabilities(DefaultGrantScopes(), nil)
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
	asked, err := svc.resolveGrantCapabilities(DefaultGrantScopes(), &wider)
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

// TestVocabularyIsM154sEleven pins the Go half of the two-place closed set by
// value. The DATABASE half is proved separately and against the live
// constraint, by TestCapabilityVocabularyMatchesTheDatabaseCheckAsAppRole in
// apps/api/tests -- this one cannot see the database and does not pretend to.
//
// It was TestVocabularyIsM131sEight until m135 seated mcp.cache.purge. The
// rename is deliberate rather than a silent edit of the literal: the name of
// this test is the only place the vocabulary's SIZE is asserted in prose, and a
// test called "IsM131sEight" passing over nine members is the drift this
// project keeps finding. It became IsM154sEleven when m154 seated the two
// ability-engine capabilities.
func TestVocabularyIsM154sEleven(t *testing.T) {
	want := []Capability{
		CapAbilityRead,
		CapAbilityRequest,
		CapActivityRead,
		CapBackupsRead,
		CapCachePurge,
		CapContentRead,
		CapDiagnosticsRead,
		CapPerformanceRead,
		CapSecurityRead,
		CapSitesRead,
		CapUptimeRead,
	}
	if !sameCaps(AllCapabilities(), want) {
		t.Fatalf("AllCapabilities() = %v, want exactly %v (m154's seated vocabulary, "+
			"alphabetical, which is the order the constraint lists them in)",
			capsToStrings(AllCapabilities()), capsToStrings(want))
	}
}

// nonReadCapabilities is the ENUMERATED allowlist of vocabulary members that
// are not reads. It is a literal and not a pattern, so seating a second write
// capability is a diff that has to name it here.
//
// It exists because m135 ended the pure pattern test below it. Until then
// "this build seats no write capability" was checkable by suffix, which was
// exactly as strong as it was true; the honest successor is not a weaker
// pattern but a named exception list, so the property becomes "the only
// non-read members are the ones a reviewer wrote down".
var nonReadCapabilities = map[Capability]struct{}{
	CapAbilityRequest: {},
	CapCachePurge:     {},
}

// TestEveryRequestCapabilityNamesACreatorPermission: every request-effect
// capability names the operator permission a grant's creator must hold, and
// the gate refuses a principal lacking it.
func TestEveryRequestCapabilityNamesACreatorPermission(t *testing.T) {
	for _, c := range AllCapabilities() {
		if e, _ := CapabilityEffect(c); e != EffectRequest {
			continue
		}
		if _, ok := requestCapabilityPermission[c]; !ok {
			t.Errorf("request capability %q has no creator permission", c)
		}
	}
	if got := requestCapabilityPermission[CapAbilityRequest].perm; got != authz.PermSiteContentEdit {
		t.Fatalf("mcp.ability.request creator permission = %q, want %q", got, authz.PermSiteContentEdit)
	}
}

// TestEveryCapabilityIsAReadOrAnEnumeratedWrite makes "this build seats no
// UNREVIEWED write capability" a property that is checked rather than claimed.
// m131 DECISION 2 froze the three-segment form specifically so this could be a
// suffix test; m135 seated the first member that fails it, so the suffix rule
// now has one enumerated exception and gains nothing else.
//
// It was TestEveryCapabilityIsARead. The rename is deliberate: the old name
// asserts something this build no longer does, and a test whose name is a false
// claim is worse than no test, because the next session greps for the claim and
// believes it.
//
// It also fails on an empty vocabulary, because a loop over an empty set passes
// having checked nothing -- the exact shape this brief names as failure mode 3
// on the database side.
func TestEveryCapabilityIsAReadOrAnEnumeratedWrite(t *testing.T) {
	all := AllCapabilities()
	if len(all) == 0 {
		t.Fatal("the vocabulary is empty, so this test ranged over nothing and " +
			"would have reported a pass having checked no capability at all")
	}
	if len(nonReadCapabilities) == 0 {
		t.Fatal("nonReadCapabilities is empty, so the exception arm below checked " +
			"nothing; if the last write capability was removed, delete the arm " +
			"rather than leaving it to pass vacuously")
	}
	reads := 0
	for _, c := range all {
		if !strings.HasPrefix(string(c), "mcp.") {
			t.Fatalf("capability %q does not carry the frozen 'mcp.' prefix. The "+
				"wireframes' 'site.*' labels are the SCREEN's, not the stored "+
				"string's; adopting one here strands every live grant.", c)
		}
		if strings.HasSuffix(string(c), ".read") {
			reads++
			if _, listed := nonReadCapabilities[c]; listed {
				t.Fatalf("capability %q ends in '.read' and is ALSO listed in "+
					"nonReadCapabilities. One of the two is wrong, and while they "+
					"disagree the exception list is not describing the vocabulary.", c)
			}
			continue
		}
		if _, listed := nonReadCapabilities[c]; !listed {
			t.Fatalf("capability %q does not end in '.read' and is not named in "+
				"nonReadCapabilities.\n"+
				"If this is a new write capability, it needs the review m124 "+
				"DECISION 1 built the closed CHECK to force -- and it must not be "+
				"conferred by ScopeRead, which is a read scope. Name it in "+
				"nonReadCapabilities in the same diff that seats it.", c)
		}
	}

	// EVERY NAMED EXCEPTION MUST STILL BE IN THE VOCABULARY. Without this arm a
	// capability could be deleted from the vocabulary and left in the list, and
	// the list would go on describing a member that no longer exists.
	for c := range nonReadCapabilities {
		if !KnownCapability(c) {
			t.Fatalf("nonReadCapabilities names %q, which is not in the vocabulary. "+
				"Remove it here in the diff that removes it there.", c)
		}
	}
	t.Logf("%d seated capabilities: %d reads and %d enumerated writes (%v)",
		len(all), reads, len(nonReadCapabilities), capsToStrings(all))
}

// TestReadScopeConfersOnlyReads is the rule the scopeCapabilities literal for
// ScopeRead encodes, stated as a property rather than a list: every capability
// the read scope confers is a read, and the cache capability is not among them.
//
// It REPLACES TestCachePurgeIsKnownButConferredByNoScope, which pinned "no
// scope confers mcp.cache.purge" and whose comment required the diff that
// conferred it to delete it rather than edit it. m150 confers it through
// mcp:cache. What must still hold is that the READ scope, which every live
// grant carries, never does: conferring it there would hand the power to ask
// for a cache clear to every grant ever minted, with no second consent.
func TestReadScopeConfersOnlyReads(t *testing.T) {
	set, err := OrgDefaultCapabilities([]Scope{ScopeRead})
	if err != nil {
		t.Fatalf("OrgDefaultCapabilities([mcp:read]): %v", err)
	}
	if set.Len() == 0 {
		t.Fatal("the read scope confers nothing, so the loop below would pass " +
			"having checked no capability")
	}
	for _, c := range set.Sorted() {
		e, ok := CapabilityEffect(c)
		if !ok || e != EffectRead {
			t.Errorf("the read scope confers %q, whose effect is %q (known=%v); the "+
				"read scope confers reads only", c, e, ok)
		}
	}
	if set.Allows(CapCachePurge) {
		t.Fatalf("the read scope confers %q. Every live grant holds mcp:read, so "+
			"this widens every grant ever minted with no second consent", CapCachePurge)
	}
}

// TestCacheScopeConfersExactlyCachePurge pins the other half of the mapping:
// mcp:cache confers mcp.cache.purge and nothing else, it is the ONLY scope that
// confers it, and the no-request preset still does not reach it.
func TestCacheScopeConfersExactlyCachePurge(t *testing.T) {
	set, err := OrgDefaultCapabilities([]Scope{ScopeCache})
	if err != nil {
		t.Fatalf("OrgDefaultCapabilities([mcp:cache]): %v", err)
	}
	if want := []Capability{CapCachePurge}; !sameCaps(set.Sorted(), want) {
		t.Fatalf("the mcp:cache ceiling = %v, want exactly %v",
			capsToStrings(set.Sorted()), capsToStrings(want))
	}

	// BOTH SCOPES are the union, by value, and content.read is still absent.
	both, err := OrgDefaultCapabilities([]Scope{ScopeRead, ScopeCache})
	if err != nil {
		t.Fatalf("OrgDefaultCapabilities([mcp:read mcp:cache]): %v", err)
	}
	wantBoth := []Capability{
		CapActivityRead,
		CapBackupsRead,
		CapCachePurge,
		CapDiagnosticsRead,
		CapPerformanceRead,
		CapSecurityRead,
		CapSitesRead,
		CapUptimeRead,
	}
	if !sameCaps(both.Sorted(), wantBoth) {
		t.Fatalf("the mcp:read + mcp:cache ceiling = %v, want exactly %v",
			capsToStrings(both.Sorted()), capsToStrings(wantBoth))
	}

	// EXACTLY ONE CONFERRER. Ranging over the map, not naming ScopeRead, so a
	// third scope that also confers it goes red here too.
	var conferrers []Scope
	for s, caps := range scopeCapabilities {
		for _, c := range caps {
			if c == CapCachePurge {
				conferrers = append(conferrers, s)
			}
		}
	}
	if len(conferrers) != 1 || conferrers[0] != ScopeCache {
		t.Fatalf("%q is conferred by %v, want exactly [%s]", CapCachePurge, conferrers, ScopeCache)
	}

	// THE PRESET DOES NOT REACH IT. A grant nobody stated terms for holds
	// mcp:read, so its ceiling refuses the cache capability whole rather than
	// trimming the request down to the reads.
	ceiling, err := OrgDefaultCapabilities(DefaultGrantScopes())
	if err != nil {
		t.Fatalf("OrgDefaultCapabilities(DefaultGrantScopes()): %v", err)
	}
	if ceiling.Allows(CapCachePurge) {
		t.Fatalf("the default-scope ceiling confers %q", CapCachePurge)
	}
	if _, err := ceiling.NarrowTo([]Capability{CapSitesRead, CapCachePurge}); err == nil {
		t.Fatalf("a request naming %q under the default-scope ceiling was accepted; "+
			"it must be refused whole", CapCachePurge)
	}
	for _, c := range DefaultGrantCapabilities() {
		if c == CapCachePurge {
			t.Fatalf("DefaultGrantCapabilities() hands out %q; an operator ticks it, "+
				"it is never a default", CapCachePurge)
		}
	}
}

// TestCapabilityEffectIsExhaustive holds capabilityEffect total over the
// vocabulary and in agreement with nonReadCapabilities: a capability seated
// without an effect goes red rather than rendering as a read, and the only
// request capabilities are the ones a reviewer enumerated.
func TestCapabilityEffectIsExhaustive(t *testing.T) {
	all := AllCapabilities()
	if len(all) == 0 {
		t.Fatal("the vocabulary is empty, so this test would pass having checked nothing")
	}
	requests := 0
	for _, c := range all {
		e, ok := CapabilityEffect(c)
		if !ok {
			t.Errorf("capability %q has no effect in capabilityEffect", c)
			continue
		}
		if e != EffectRead && e != EffectRequest {
			t.Errorf("capability %q has effect %q, outside the closed set {%q, %q}",
				c, e, EffectRead, EffectRequest)
		}
		_, enumerated := nonReadCapabilities[c]
		if (e == EffectRequest) != enumerated {
			t.Errorf("capability %q has effect %q but nonReadCapabilities membership "+
				"is %v; the two must agree", c, e, enumerated)
		}
		if e == EffectRequest {
			requests++
		}
	}
	if requests == 0 {
		t.Error("no capability has a request effect, so the agreement arm above " +
			"checked only one direction")
	}
	for c := range capabilityEffect {
		if !KnownCapability(c) {
			t.Errorf("capabilityEffect names %q, which is not in the vocabulary", c)
		}
	}
	if _, ok := CapabilityEffect(Capability("mcp.unknown.read")); ok {
		t.Error("CapabilityEffect reported an effect for a capability outside the vocabulary")
	}
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
	ceiling, err := OrgDefaultCapabilities(DefaultGrantScopes())
	if err != nil {
		t.Fatalf("OrgDefaultCapabilities(DefaultGrantScopes()): %v", err)
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
