package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// S7 EXIT GATE -- a capability absent from the registry is unreachable by a
// schema-guessing model, and discovery never grants.
// ---------------------------------------------------------------------------

// nonEmptyRegistry returns the registry, having first asserted it is not empty.
//
// EVERY GUARD IN THIS FILE THAT LOOPS OVER THE REGISTRY GOES THROUGH IT. A
// range over an empty slice succeeds having checked nothing, so a guard written
// as a bare loop reports PASS on an empty registry -- which is CLAUDE.md's rule
// verbatim: a guard that finds nothing must go red, not green.
//
// This is the second vacuous-guard finding on this file (TestRegistryIsClosed
// was the first, and it was misnamed rather than empty), so the fix is one
// accessor the guards share rather than one assertion pasted into each, which
// would drift the moment somebody adds a guard and forgets.
func nonEmptyRegistry(t *testing.T) []ToolPolicy {
	t.Helper()
	entries := registryTools()
	if len(entries) == 0 {
		t.Fatal("the registry is empty, so every guard that ranges over it would " +
			"pass having checked nothing")
	}
	return entries
}

// authWith builds an AuthorizedRequest directly, which is how a test reaches
// the registry gate with a chosen capability set. Every field is set
// explicitly: the zero value of each allows nothing, so a test that forgot one
// would prove the gate works for a reason it did not intend.
//
// THE CEILING IS THE PRODUCTION CEILING, not the grant. authWith models the
// ordinary connection -- one whose organisation has everything the surface
// offers enabled and whose own grant may be narrower. That is what
// Authenticate builds today, because OrgDefaultCapabilities over grantScopes()
// resolves to the whole vocabulary while exactly one scope is recognised.
//
// A test that needs a NARROWER ceiling than the vocabulary -- the org-switched-
// off case -- must say so, and authWithCeiling is how.
func authWith(caps CapabilitySet, siteIDs ...uuid.UUID) AuthorizedRequest {
	ceiling, err := OrgDefaultCapabilities(grantScopes())
	if err != nil {
		// Not t.Fatal: authWith has no *testing.T and this is unreachable while
		// scopeCapabilities is total over recognisedScopes, which
		// TestEveryRecognisedScopeHasACapabilityMapping pins. Panicking beats
		// returning a zero ceiling, which would silently list nothing and make
		// every visibility test pass for the wrong reason.
		panic("authWith: org ceiling did not resolve: " + err.Error())
	}
	return authWithCeiling(ceiling, caps, siteIDs...)
}

// authWithCeiling builds an AuthorizedRequest with an EXPLICIT org ceiling, so
// a test can construct the case where the organisation's ceiling is narrower
// than the capability vocabulary.
//
// That case is not reachable through Authenticate today: the ceiling is derived
// from the closed scope registry, recognisedScopes holds one entry, and
// scopeCapabilities maps it to the whole vocabulary -- so the production
// ceiling and the vocabulary coincide and every tool is inside every ceiling.
// It becomes reachable when the vocabulary widens and org policy can select
// within it. The structure is proven now so it is correct then.
func authWithCeiling(ceiling, caps CapabilitySet, siteIDs ...uuid.UUID) AuthorizedRequest {
	return AuthorizedRequest{
		TenantID:     uuid.New(),
		GrantID:      uuid.New(),
		TokenID:      uuid.New(),
		Sites:        NewSiteSet(siteIDs),
		Capabilities: caps,
		OrgCeiling:   ceiling,
	}
}

// TestExitGate_GuessedToolNameIsUnreachable is THE proof for this slice.
//
// A connection holds NO capability. It names a tool anyway -- the registered
// name, and a set of plausible guesses a model would produce from the product's
// vocabulary. Every one must refuse, and the two KINDS of refusal must be the
// two the D1 ruling defines.
//
// The registered name is in the table on purpose and it is the load-bearing
// case. A gate that only refuses names it has never heard of is not a gate: it
// is a typo checker. The failure this test exists to catch is a tools/call path
// that resolves a REAL registry entry and dispatches on it without asking
// whether this connection may reach it, which is exactly what the pre-S7 switch
// statement did -- and which listing the tool unconditionally would make easier
// to reach, not harder.
func TestExitGate_GuessedToolNameIsUnreachable(t *testing.T) {
	// No capability, but a non-empty site scope -- so nothing about this
	// refusal can be attributed to the site axis.
	auth := authWith(CapabilitySet{}, uuid.New())

	// The tool is LISTED. That is the ruling, and it is asserted here rather
	// than only in its own test so that this proof cannot be read as "nothing
	// was reachable because nothing was shown".
	// ASSERTED AS "THE WHOLE REGISTRY IS LISTED", not as "exactly one tool
	// exists". The earlier form pinned len(got) == 1, which made a correct
	// second read tool fail a proof about the CAPABILITY axis -- a guard
	// reddening on work it was never meant to police is a guard that gets
	// switched off. What the ruling actually claims is that no entry is hidden
	// by a capability the grant lacks, so that is what is compared.
	if got := VisibleTools(auth); !sameToolNames(got, registryToolNames()) {
		t.Fatalf("a connection holding no capability was shown %d tools, want the whole "+
			"registry listed and annotated: %+v", len(got), got)
	}

	cases := []struct {
		name string
		want string
	}{
		// REGISTERED, and still unreachable. See above.
		{ToolFleetSitesList, ErrCodeCapabilityNotGranted},
		// Near misses of the LIVE name. These were variants of list_sites
		// until the rename, at which point they stopped probing the name the
		// registry carries and became ordinary unregistered strings.
		{"fleet_sites_list_all", ErrCodeToolNotAvailable},
		{"fleetSitesList", ErrCodeToolNotAvailable},
		{"FLEET_SITES_LIST", ErrCodeToolNotAvailable},
		{"fleet-sites-list", ErrCodeToolNotAvailable},
		{"fleet_site_list", ErrCodeToolNotAvailable},
		// The retired name must refuse, not alias.
		{"list_sites", ErrCodeToolNotAvailable},
		{"list_sites_all", ErrCodeToolNotAvailable},
		{"sites.restart", ErrCodeToolNotAvailable},
		{"restart_site", ErrCodeToolNotAvailable},
		{"update_plugin", ErrCodeToolNotAvailable},
		{"run_backup", ErrCodeToolNotAvailable},
		{"delete_site", ErrCodeToolNotAvailable},
		// The empty name, which must not match a zero-value entry.
		{"", ErrCodeToolNotAvailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, reason, err := AuthorizeTool(tc.name, auth)
			if err == nil {
				t.Fatalf("AuthorizeTool(%q) GRANTED: entry=%+v", tc.name, entry)
			}
			if entry.invoke != nil {
				t.Fatalf("AuthorizeTool(%q) returned an invocable entry alongside its error", tc.name)
			}
			de, ok := domain.AsDomain(err)
			if !ok {
				t.Fatalf("AuthorizeTool(%q) err = %v, want a domain error", tc.name, err)
			}
			// ASSERTED BY VALUE. "err != nil" would pass here even if the
			// capability case had been answered by the site axis, or by the
			// unregistered arm, which are different bugs with different fixes.
			if de.Code != tc.want {
				t.Fatalf("AuthorizeTool(%q) code = %q, want %q", tc.name, de.Code, tc.want)
			}
			if de.Kind != domain.KindForbidden {
				t.Fatalf("AuthorizeTool(%q) kind = %v, want KindForbidden -- a 401 here is the "+
					"shape that makes clients re-run an OAuth flow that cannot help",
					tc.name, de.Kind)
			}
			if reason == "" {
				t.Fatalf("AuthorizeTool(%q) produced no operator-facing reason", tc.name)
			}
		})
	}
}

// TestCapabilityRefusalIsTypedTerminalAndNamesTheCapability is the D1 ruling
// stated as a test.
//
// It replaces TestExitGate_RefusalIsNotAnExistenceOracle, which asserted the
// OPPOSITE -- that a registered name and a never-existed name must be
// indistinguishable. That property was deliberately given up: the owner ruled
// that an unticked capability refuses rather than hides, and once tools/list
// stops filtering there is no surface left for the blur to protect.
//
// Every assertion here is by VALUE. A refusal that merely errors, or that
// errors with the uniform code, or that errors without naming the capability,
// is the behaviour this change exists to remove.
func TestCapabilityRefusalIsTypedTerminalAndNamesTheCapability(t *testing.T) {
	auth := authWith(CapabilitySet{}, uuid.New())

	_, reason, err := AuthorizeTool(ToolFleetSitesList, auth)
	if err == nil {
		t.Fatal("a connection holding no capability was granted the tool")
	}
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("err = %v, want a domain error", err)
	}
	if de.Code != ErrCodeCapabilityNotGranted {
		t.Fatalf("code = %q, want %q -- the capability refusal must not share the uniform "+
			"not-available code, which tells the model to ask tools/list for a tool "+
			"tools/list already showed it", de.Code, ErrCodeCapabilityNotGranted)
	}
	if reason != reasonCapabilityNotHeld {
		t.Fatalf("operator reason = %q, want %q", reason, reasonCapabilityNotHeld)
	}

	// It NAMES the capability, in the message, where a model reads it.
	if !strings.Contains(de.Message, string(CapSitesRead)) {
		t.Fatalf("the refusal does not name the missing capability %q: %s",
			CapSitesRead, de.Message)
	}

	// It says the refusal is PERMANENT. A model that reads a refusal as
	// transient retries; the empty-capability 401 made clients re-run an OAuth
	// handshake that could not change the answer.
	if !strings.Contains(de.Message, "permanent") {
		t.Fatalf("the refusal does not tell the model it is permanent: %s", de.Message)
	}
	if de.Details == nil {
		t.Fatal("the refusal carries no details, so a client has to parse English to " +
			"learn whether retrying could help")
	}
	if got := de.Details["retryable"]; got != false {
		t.Fatalf("details[retryable] = %v, want false", got)
	}
	if got := de.Details["required_capability"]; got != string(CapSitesRead) {
		t.Fatalf("details[required_capability] = %v, want %q", got, CapSitesRead)
	}
	// The held set is a fact about the caller's own grant, and it is what lets
	// a model tell its user what this connection CAN do.
	held, ok := de.Details["held_capabilities"].([]string)
	if !ok {
		t.Fatalf("details[held_capabilities] = %#v, want []string", de.Details["held_capabilities"])
	}
	if len(held) != 0 {
		t.Fatalf("held_capabilities = %v for a connection holding none", held)
	}
}

// TestCapabilityRefusalIsListedNotHidden is the "does not hide" half, on the
// discovery path.
//
// A connection holding nothing must still be SHOWN the tool, and the descriptor
// it is shown must say -- in the description, the one field every MCP client
// puts in front of the model -- that the tool is not available, why, and that
// retrying will not help.
func TestCapabilityRefusalIsListedNotHidden(t *testing.T) {
	auth := authWith(CapabilitySet{}, uuid.New())

	// Asserted against the registry rather than against a literal 1, so that
	// adding a second tool does not quietly narrow what this proves.
	want := len(nonEmptyRegistry(t))
	got := VisibleTools(auth)
	if len(got) != want {
		t.Fatalf("tools/list showed %d of %d tools to a connection holding no capability; "+
			"an unticked capability must refuse, not hide", len(got), want)
	}

	d := got[0]
	if d.Name != ToolFleetSitesList {
		t.Fatalf("listed tool = %q, want %q", d.Name, ToolFleetSitesList)
	}
	for _, must := range []string{
		"NOT AVAILABLE TO THIS CONNECTION",
		string(CapSitesRead),
		ErrCodeCapabilityNotGranted,
		"permanent",
	} {
		if !strings.Contains(d.Description, must) {
			t.Fatalf("the listed descriptor does not mention %q, so a model reading tools/list "+
				"cannot tell this tool from a callable one:\n%s", must, d.Description)
		}
	}

	// AND IT DOES NOT OVER-FIRE. A connection that HOLDS the capability is
	// shown the plain description, with no notice at all -- a guard that
	// annotates correct work is a guard that gets switched off.
	full := authWith(NewCapabilitySet(AllCapabilities()), uuid.New())
	fd := VisibleTools(full)
	if len(fd) != want {
		t.Fatalf("a fully-capable connection saw %d of %d tools", len(fd), want)
	}
	if strings.Contains(fd[0].Description, "NOT AVAILABLE TO THIS CONNECTION") {
		t.Fatalf("a connection that HOLDS %q was told the tool is unavailable:\n%s",
			CapSitesRead, fd[0].Description)
	}
	if _, _, err := AuthorizeTool(ToolFleetSitesList, authWith(
		NewCapabilitySet(AllCapabilities()), uuid.New())); err != nil {
		t.Fatalf("a connection holding every capability and a site was refused: %v", err)
	}
}

// TestOrgCeilingHidesWhileTheGrantRefuses is the ruling's asymmetry, in one
// test, so that collapsing the two predicates into one cannot pass.
//
// The same registry, the same tool, two different connections:
//
//	ceiling holds it, grant does not -> LISTED, annotated, refuses by name
//	ceiling does not hold it         -> ABSENT, refuses as unregistered
//
// CI does not run the integration package, so this is the fast guard for the
// ceiling arm; the wpmgr_app proof is
// TestS7CapabilityOutsideTheOrgCeilingIsNotListedAsAppRole in
// apps/api/tests/adr064_s7_mcp_tool_registry_rls_test.go.
//
// The narrow ceiling is the EMPTY set because that is the only proper subset of
// a one-entry vocabulary, and it is not reachable through Authenticate today.
// See authWithCeiling.
func TestOrgCeilingHidesWhileTheGrantRefuses(t *testing.T) {
	want := len(nonEmptyRegistry(t))
	site := uuid.New()

	// INSIDE the ceiling, NOT held by the grant: listed and annotated.
	inside := authWith(CapabilitySet{}, site)
	got := VisibleTools(inside)
	if len(got) != want {
		t.Fatalf("a capability the ORG allows but the grant lacks was hidden: showed %d "+
			"of %d; that boundary must refuse visibly, not vanish", len(got), want)
	}
	if !strings.Contains(got[0].Description, "NOT AVAILABLE TO THIS CONNECTION") {
		t.Fatalf("the listed tool is not annotated:\n%s", got[0].Description)
	}
	_, _, err := AuthorizeTool(ToolFleetSitesList, inside)
	ide, ok := domain.AsDomain(err)
	if !ok || ide.Code != ErrCodeCapabilityNotGranted {
		t.Fatalf("in-ceiling refusal = %v, want code %q", err, ErrCodeCapabilityNotGranted)
	}

	// OUTSIDE the ceiling, and the grant DOES hold it -- so an absence can only
	// be the ceiling. Nothing here is attributable to the grant axis.
	outside := authWithCeiling(NewCapabilitySet(nil), NewCapabilitySet(AllCapabilities()), site)
	if !outside.Capabilities.Allows(CapSitesRead) {
		t.Fatalf("the grant does not hold %q, so this would prove the GRANT arm", CapSitesRead)
	}
	if hidden := VisibleTools(outside); len(hidden) != 0 {
		t.Fatalf("a capability OUTSIDE the org ceiling was listed: %+v; a token holder must "+
			"not be able to enumerate what the organisation switched off", hidden)
	}

	// And the gate agrees with the listing, by value and by prose.
	_, _, err = AuthorizeTool(ToolFleetSitesList, outside)
	ode, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("out-of-ceiling refusal = %v, want a domain error", err)
	}
	if ode.Code != ErrCodeToolNotAvailable {
		t.Fatalf("out-of-ceiling code = %q, want %q", ode.Code, ErrCodeToolNotAvailable)
	}
	if ode.Code == ErrCodeCapabilityNotGranted {
		t.Fatal("the ceiling refusal named the capability the organisation disabled")
	}
	if strings.Contains(ode.Message, string(CapSitesRead)) {
		t.Fatalf("the ceiling refusal names the disabled capability: %s", ode.Message)
	}

	// Identical to a name that was never registered. A divergence here is an
	// oracle for the org's disabled capabilities.
	//
	// The comparison is on the TEMPLATE, not the literal string: both messages
	// echo the name the caller asked for, and that difference is the caller's
	// own input rather than a disclosure. Substituting it back is what isolates
	// the property -- everything OTHER than the echoed name must match.
	const guessed = "sites_restart_everything"
	_, _, guess := AuthorizeTool(guessed, outside)
	gde, ok := domain.AsDomain(guess)
	if !ok {
		t.Fatalf("guessed name err = %v, want a domain error", guess)
	}
	if gde.Code != ode.Code {
		t.Fatalf("a disabled capability answers %q and a guessed name answers %q; "+
			"the difference tells a caller which capabilities exist", ode.Code, gde.Code)
	}
	if want := strings.ReplaceAll(gde.Message, guessed, ToolFleetSitesList); ode.Message != want {
		t.Fatalf("the two refusals differ beyond the echoed name, which is an oracle:\n"+
			"disabled: %s\nexpected: %s", ode.Message, want)
	}

	// AND IT DOES NOT OVER-FIRE. The ordinary connection -- real ceiling, full
	// grant -- still sees every tool and still calls it.
	full := authWith(NewCapabilitySet(AllCapabilities()), site)
	if fd := VisibleTools(full); len(fd) != want {
		t.Fatalf("an ordinary fully-granted connection saw %d of %d tools", len(fd), want)
	}
	if _, _, err := AuthorizeTool(ToolFleetSitesList, full); err != nil {
		t.Fatalf("an ordinary fully-granted connection was refused: %v", err)
	}
}

// TestUnregisteredNameIsNotACapabilityRefusal keeps the two answers apart in
// the other direction.
//
// A guessed name must NOT be reported as a capability problem: that would send
// an operator to widen a grant for a tool that does not exist, and it would
// hand a wordlist a "this name is real" signal that the registry does not have.
func TestUnregisteredNameIsNotACapabilityRefusal(t *testing.T) {
	// A FULLY capable connection, so nothing here can be attributed to a
	// missing capability.
	auth := authWith(NewCapabilitySet(AllCapabilities()), uuid.New())

	_, reason, err := AuthorizeTool("definitely_not_a_tool_xyzzy", auth)
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("err = %v, want a domain error", err)
	}
	if de.Code != ErrCodeToolNotAvailable {
		t.Fatalf("code = %q, want %q", de.Code, ErrCodeToolNotAvailable)
	}
	if reason != reasonUnregistered {
		t.Fatalf("operator reason = %q, want %q", reason, reasonUnregistered)
	}
	if strings.Contains(de.Message, string(CapSitesRead)) {
		t.Fatalf("a guessed name was refused with a capability the caller never lacked: %s",
			de.Message)
	}
}

// TestExitGate_ListedIsNotCallable walks the whole registry against a
// connection holding each capability in turn and asserts the ONE-DIRECTIONAL
// invariant the D1 ruling leaves standing:
//
//	NOTHING IS CALLABLE THAT WAS NOT LISTED.
//
// The converse -- everything listed is callable -- is deliberately gone. That
// identity is what made an unticked capability HIDE a tool, and restoring it is
// the mutation this test exists to catch: a connection holding no capability
// must be shown the tool AND refused it.
//
// The direction kept is the one that matters. It held before S7 only for a
// client that called tools/list and believed the answer; here it is a property
// of the code, because AuthorizeTool derives the capability answer itself
// rather than trusting the listing.
func TestExitGate_ListedIsNotCallable(t *testing.T) {
	site := uuid.New()
	registry := nonEmptyRegistry(t)

	// Every subset of the vocabulary that a single capability can produce, plus
	// the empty set and the full set.
	sets := []CapabilitySet{{}, NewCapabilitySet(AllCapabilities())}
	for _, c := range AllCapabilities() {
		sets = append(sets, NewCapabilitySet([]Capability{c}))
	}

	for _, caps := range sets {
		auth := authWith(caps, site)

		// Everything is listed, whatever the capability set. This is the
		// "does not hide" invariant, asserted for every subset rather than
		// only for the empty one.
		listed := map[string]bool{}
		for _, d := range VisibleTools(auth) {
			listed[d.Name] = true
		}
		if len(listed) != len(registry) {
			t.Fatalf("caps=%v: tools/list showed %d of %d tools; an unticked capability "+
				"must refuse, not hide", caps.Sorted(), len(listed), len(registry))
		}

		for _, e := range registry {
			_, _, err := AuthorizeTool(e.Name, auth)
			if err == nil && !listed[e.Name] {
				t.Fatalf("caps=%v: %q was CALLABLE but never shown by tools/list",
					caps.Sorted(), e.Name)
			}
			// And the capability really gates the call, whatever the listing
			// said. Asserted by value: an entry callable without its capability
			// is the pre-S7 defect returning.
			if !caps.Allows(e.Capability) {
				de, ok := domain.AsDomain(err)
				if !ok || de.Code != ErrCodeCapabilityNotGranted {
					t.Fatalf("caps=%v: %q (requires %q) refused with %v, want %s",
						caps.Sorted(), e.Name, e.Capability, err, ErrCodeCapabilityNotGranted)
				}
			}
		}
	}
}

// TestSiteScopeIsRefusedByName proves the deliberate asymmetry: the site axis
// does NOT hide a tool, and it refuses by name rather than uniformly.
//
// A connection holding the capability with an EMPTY site scope must still see
// list_sites (its org enabled it, its capabilities cover it), and calling it
// must produce the named mcp_scope_empty refusal -- not the uniform
// not-available one, which would send an operator hunting a capability problem
// they do not have.
func TestSiteScopeIsRefusedByName(t *testing.T) {
	auth := authWith(NewCapabilitySet([]Capability{CapSitesRead})) // no sites

	// SITE-SCOPE VISIBILITY IS OUT OF SCOPE FOR THE D1 RULING AND MUST NOT HAVE
	// MOVED. The tool is listed, exactly as before, and the descriptor carries
	// NO capability notice -- this connection holds the capability, and telling
	// it otherwise would send an operator hunting the wrong axis.
	got := VisibleTools(auth)
	if !sameToolNames(got, registryToolNames()) {
		t.Fatalf("an empty site scope hid a tool from tools/list: %+v", got)
	}
	// EVERY descriptor is checked, not got[0]. Checking only the first left the
	// assertion silently covering less of the surface as the registry grew,
	// which is the shape where a guard keeps passing while what it guards
	// shrinks underneath it.
	for _, d := range got {
		if strings.Contains(d.Description, "NOT AVAILABLE TO THIS CONNECTION") {
			t.Fatalf("an empty SITE scope produced a CAPABILITY notice on %q:\n%s", d.Name, d.Description)
		}
	}

	_, reason, err := AuthorizeTool(ToolFleetSitesList, auth)
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("err = %v, want a domain error", err)
	}
	// By value, and by BOTH codes: this assertion stayed green under the
	// capability refusal too until it named the code it does not want.
	if de.Code != ErrCodeScopeEmpty {
		t.Fatalf("code = %q, want %q", de.Code, ErrCodeScopeEmpty)
	}
	if de.Code == ErrCodeCapabilityNotGranted {
		t.Fatalf("the site axis answered with the CAPABILITY refusal: %s", de.Message)
	}
	if reason != reasonSiteScopeEmpty {
		t.Fatalf("operator reason = %q, want %q", reason, reasonSiteScopeEmpty)
	}
}

// ---------------------------------------------------------------------------
// Registry drift guards
// ---------------------------------------------------------------------------

// TestRegistryEntriesAreWellFormed is the drift guard. Each clause catches a
// way an entry can be added that compiles, passes every other test, and is
// wrong at runtime.
func TestRegistryEntriesAreWellFormed(t *testing.T) {
	entries := nonEmptyRegistry(t)

	names := map[string]bool{}
	for _, e := range entries {
		if strings.TrimSpace(e.Name) == "" {
			t.Errorf("a registry entry has a blank name")
			continue
		}
		if names[e.Name] {
			// Two entries with one name means AuthorizeTool's first match wins
			// and the second is dead -- including if the second is the
			// restrictive one.
			t.Errorf("%q is registered twice", e.Name)
		}
		names[e.Name] = true

		if e.invoke == nil {
			t.Errorf("%q has no implementation; it would resolve and then fail internally", e.Name)
		}
		if !KnownCapability(e.Capability) {
			// The zero-value Capability lands here, which is the point: an
			// entry that forgets to declare one must be unreachable, and this
			// says so at test time rather than leaving it silently dead.
			t.Errorf("%q declares capability %q, which is not in the vocabulary", e.Name, e.Capability)
		}
		if !authz.KnownPermission(e.OperatorPermission) {
			t.Errorf("%q declares operator permission %q, which is not in the control plane's vocabulary",
				e.Name, e.OperatorPermission)
		}
		if len(e.InputSchema) == 0 || !json.Valid(e.InputSchema) {
			t.Errorf("%q has an absent or invalid input schema", e.Name)
		}
		if strings.TrimSpace(e.Description) == "" {
			t.Errorf("%q has no description", e.Name)
		}
	}
}

// TestRegistryIsNotAliasedAcrossCalls proves registryTools returns a FRESH
// slice, so mutating what one caller got is not observable by the next.
//
// IT IS NAMED FOR WHAT IT TESTS. It was called TestRegistryIsClosed, which
// overclaimed: a review mutation that added a write-shaped delete_site tool to
// the literal passed this test unchanged, and was caught by
// TestToolsList_IsToolsOnlyAndReadOnly instead. Aliasing and closure are
// different properties -- this one covers runtime mutation of a returned slice,
// and the read-only claim about the literal's CONTENTS belongs to that other
// test. A test whose name claims the stronger property is worse than no test,
// because the next reader stops looking.
func TestRegistryIsNotAliasedAcrossCalls(t *testing.T) {
	before := len(nonEmptyRegistry(t))

	stolen := registryTools()
	stolen = append(stolen, ToolPolicy{Name: "smuggled_write_tool", Capability: CapSitesRead})
	stolen[0].Name = "hijacked"
	_ = stolen

	after := registryTools()
	if len(after) != before {
		t.Fatalf("registry length moved from %d to %d after a caller appended to its slice", before, len(after))
	}
	for _, e := range after {
		if e.Name == "hijacked" || e.Name == "smuggled_write_tool" {
			t.Fatalf("a caller mutated the registry: %q is now registered", e.Name)
		}
	}

	// Tools() is the unfiltered operator view and must be closed the same way.
	t1 := Tools()
	t1 = append(t1, ToolDescriptor{Name: "smuggled"})
	if len(Tools()) != before {
		t.Fatalf("Tools() is not a fresh slice")
	}
	_ = t1
}

// TestSchemaBytesAreNotSharedAcrossCallers is the other half of the closure
// property, and the half that was missing.
//
// A fresh slice of descriptors is not enough. json.RawMessage is a []byte, so
// before this the schema bytes were shared with every descriptor the package
// ever handed out: nobody could add a tool, and anybody holding one returned
// descriptor could rewrite what an existing tool claims to accept, for every
// later caller on the instance. The container was immutable, which is exactly
// what made the sharing invisible.
func TestSchemaBytesAreNotSharedAcrossCallers(t *testing.T) {
	first := Tools()
	if len(first) == 0 {
		t.Fatal("Tools() is empty, so this guard checks nothing")
	}
	if len(first[0].InputSchema) == 0 {
		t.Fatal("the first tool has no schema, so this guard checks nothing")
	}

	original := string(first[0].InputSchema)

	// A caller scribbles on the schema it was handed.
	for i := range first[0].InputSchema {
		first[0].InputSchema[i] = 'X'
	}

	if got := string(Tools()[0].InputSchema); got != original {
		t.Fatalf("mutating one caller's schema changed the package's copy:\n got  %s\n want %s", got, original)
	}

	// VisibleTools and AuthorizeTool hand out the same bytes and must be
	// independent too.
	auth := authWith(NewCapabilitySet(AllCapabilities()), uuid.New())
	vis := VisibleTools(auth)
	if len(vis) == 0 {
		t.Fatal("a fully-capable connection saw no tools, so this half checks nothing")
	}
	for i := range vis[0].InputSchema {
		vis[0].InputSchema[i] = 'Y'
	}
	if got := string(VisibleTools(auth)[0].InputSchema); got != original {
		t.Fatalf("VisibleTools shares schema bytes across callers:\n got  %s\n want %s", got, original)
	}

	entry, _, err := AuthorizeTool(ToolFleetSitesList, auth)
	if err != nil {
		t.Fatalf("AuthorizeTool: %v", err)
	}
	for i := range entry.InputSchema {
		entry.InputSchema[i] = 'Z'
	}
	if got := string(Tools()[0].InputSchema); got != original {
		t.Fatalf("AuthorizeTool shares schema bytes with the registry:\n got  %s\n want %s", got, original)
	}
}

// TestNoRegisteredToolIsWriteShaped is the closure half the aliasing test above
// does NOT cover, kept here so the registry file carries its own read-only
// guard rather than relying on a transport test to catch a registry change.
//
// It is a NAME-SHAPE heuristic and says so: it cannot tell what a tool does, it
// can only refuse verbs a read tool has no business declaring. That is enough
// to make a write tool arrive as a visible, deliberate act -- someone has to
// delete a case from this list, in a diff, with a reason.
// MATCHING IS PER UNDERSCORE-SEPARATED SEGMENT AND NO LONGER BY SUBSTRING, and
// the change was forced by a true negative rather than chosen for elegance.
//
// The substring form failed fleet_updates_pending -- a read tool whose name is
// the wireframe catalogue's own ("Plugin, theme and core updates outstanding,
// per site") -- because the plural NOUN "updates" contains the verb "update".
// The available responses were to rename the tool away from the catalogue, to
// delete "update" from the list, or to make the match mean what the comment
// already claimed it meant. The first two are both worse: one puts the wire
// name out of step with the screen the operator reads, and the other stops
// propose_update being caught, which is the exact tool this guard exists for.
//
// Segment matching keeps every real catch. A write tool's name carries the verb
// AS A SEGMENT -- propose_update, site_delete, fleet_restore, run_exec -- because
// that is how a verb is spelled when it is the action rather than the object.
// "updates" as a plural noun is a segment of its own and is not the verb.
//
// THE CAPABILITY ASSERTION BELOW IS THE PART THAT DOES NOT DEPEND ON SPELLING,
// and it is new. A name heuristic can always be evaded by naming a tool well;
// a write tool cannot avoid requiring a capability that is not a read, because
// the capability is what the operator ticked and what the grant stores. Every
// capability this surface has is `mcp.<group>.read` (policy.go), so requiring
// the suffix catches a write tool however it is spelled -- including one added
// with a name this list has never heard of.
func TestNoRegisteredToolIsWriteShaped(t *testing.T) {
	forbidden := map[string]struct{}{}
	for _, v := range []string{
		"create", "update", "delete", "remove", "restart", "reboot",
		"install", "uninstall", "activate", "deactivate", "write",
		"set", "purge", "restore", "rollback", "run", "exec",
		"apply", "propose", "approve",
	} {
		forbidden[v] = struct{}{}
	}
	for _, e := range nonEmptyRegistry(t) {
		for _, seg := range strings.Split(strings.ToLower(e.Name), "_") {
			if _, bad := forbidden[seg]; bad {
				t.Errorf("tool %q carries the write-shaped verb %q as a name segment. The MCP "+
					"surface is read-only BY CONSTRUCTION (m124 DECISION 1) -- a write tool "+
					"arrives with its own capability, its own migration and its own security "+
					"review, never by being appended to registryTools.", e.Name, seg)
			}
		}
		if !strings.HasSuffix(string(e.Capability), ".read") {
			t.Errorf("tool %q requires capability %q, which is not a .read capability. Every tool "+
				"on this surface reads; a capability that is not a read is how a write tool "+
				"would have to arrive, whatever it is named.", e.Name, e.Capability)
		}
		if e.OperatorPermission == authz.PermSiteWrite {
			t.Errorf("tool %q declares the site:write operator permission on a read-only surface", e.Name)
		}
	}
}

// TestVisibleToolNamesMatchTheListing guards the error body. It carries a tool
// list, and that list must be exactly what this connection's tools/list
// returned -- so a model that mistyped a name can correct from it in one round
// trip, and so the body never claims a surface the listing did not.
//
// Under the D1 ruling the listing is the whole registry, so this is now an
// AGREEMENT check rather than a filtering one. It is still worth asserting: the
// two are separate functions, and an error body that diverges from tools/list
// tells the model two different stories about the same server.
func TestVisibleToolNamesMatchTheListing(t *testing.T) {
	// len(got) == len(registry) is satisfied by 0 == 0, so the registry is
	// asserted non-empty first: otherwise this guard reports PASS on a server
	// with no tools at all.
	want := len(nonEmptyRegistry(t))

	for _, caps := range []CapabilitySet{{}, NewCapabilitySet(AllCapabilities())} {
		auth := authWith(caps, uuid.New())
		names := visibleToolNames(auth)
		if len(names) != want {
			t.Fatalf("caps=%v: the error body named %d of %d tools", caps.Sorted(), len(names), want)
		}
		listed := VisibleTools(auth)
		for i, d := range listed {
			if names[i] != d.Name {
				t.Fatalf("caps=%v: error body names %v, tools/list showed %q at %d",
					caps.Sorted(), names, d.Name, i)
			}
		}
	}
}

// registryToolNames is what the WHOLE registry holds, and sameToolNames
// compares a listing against it order-insensitively.
//
// THEY EXIST SO A PROOF ABOUT ONE AXIS SAYS "NOTHING WAS HIDDEN" INSTEAD OF
// PINNING A COUNT. Several proofs about the capability axis and the site axis
// asserted len(tools) == 1, which is a fact about how many tools had been built
// on the day they were written and not about the boundary under test. Every one
// of them went red on a correct second read tool, and a guard that reddens on
// correct work is a guard that gets deleted rather than read.
func registryToolNames() []string {
	out := []string{}
	for _, e := range registryTools() {
		out = append(out, e.Name)
	}
	return out
}

func sameToolNames(got []ToolDescriptor, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, d := range got {
		seen[d.Name]++
	}
	for _, n := range want {
		seen[n]--
		if seen[n] < 0 {
			return false
		}
	}
	return true
}
