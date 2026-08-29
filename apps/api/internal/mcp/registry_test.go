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
// the registry gate with a chosen capability set. Both fields are set
// explicitly: the zero value of each allows nothing, so a test that forgot one
// would prove the gate works for a reason it did not intend.
func authWith(caps CapabilitySet, siteIDs ...uuid.UUID) AuthorizedRequest {
	return AuthorizedRequest{
		TenantID:     uuid.New(),
		GrantID:      uuid.New(),
		TokenID:      uuid.New(),
		Sites:        NewSiteSet(siteIDs),
		Capabilities: caps,
	}
}

// TestExitGate_GuessedToolNameIsUnreachable is THE proof for this slice.
//
// A connection holds NO capability. tools/list therefore shows it nothing. It
// then names a tool anyway -- the registered name, and a set of plausible
// guesses a model would produce from the product's vocabulary. Every one must
// refuse.
//
// The registered name is in the table on purpose and it is the load-bearing
// case. A gate that only refuses names it has never heard of is not a gate: it
// is a typo checker. The failure this test exists to catch is a tools/call path
// that resolves a REAL registry entry and dispatches on it without asking
// whether this connection may reach it, which is exactly what the pre-S7 switch
// statement did.
func TestExitGate_GuessedToolNameIsUnreachable(t *testing.T) {
	// No capability, but a non-empty site scope -- so nothing about this
	// refusal can be attributed to the site axis.
	auth := authWith(CapabilitySet{}, uuid.New())

	if got := VisibleTools(auth); len(got) != 0 {
		t.Fatalf("a connection holding no capability was shown %d tools: %+v", len(got), got)
	}

	for _, name := range []string{
		ToolListSites, // REGISTERED, and still unreachable. See above.
		"list_sites_all",
		"sites.restart",
		"restart_site",
		"update_plugin",
		"run_backup",
		"delete_site",
		"", // the empty name, which must not match a zero-value entry
	} {
		t.Run(name, func(t *testing.T) {
			entry, reason, err := AuthorizeTool(name, auth)
			if err == nil {
				t.Fatalf("AuthorizeTool(%q) GRANTED: entry=%+v", name, entry)
			}
			if entry.invoke != nil {
				t.Fatalf("AuthorizeTool(%q) returned an invocable entry alongside its error", name)
			}
			de, ok := domain.AsDomain(err)
			if !ok || de.Code != ErrCodeToolNotAvailable {
				t.Fatalf("AuthorizeTool(%q) err = %v, want a %s domain error", name, err, ErrCodeToolNotAvailable)
			}
			if reason == "" {
				t.Fatalf("AuthorizeTool(%q) produced no operator-facing reason", name)
			}
		})
	}
}

// TestExitGate_RefusalIsNotAnExistenceOracle is the disclosure half.
//
// The registered name and a name that has never existed must be
// INDISTINGUISHABLE on the wire: same code, same message, same data. If they
// diverge, a caller enumerates the product's tool surface with a wordlist.
//
// The operator-facing reason must diverge, because that is where the typed
// refusal actually lives.
func TestExitGate_RefusalIsNotAnExistenceOracle(t *testing.T) {
	auth := authWith(CapabilitySet{}, uuid.New())

	_, realReason, realErr := AuthorizeTool(ToolListSites, auth)
	_, fakeReason, fakeErr := AuthorizeTool("definitely_not_a_tool_xyzzy", auth)

	realDE, ok1 := domain.AsDomain(realErr)
	fakeDE, ok2 := domain.AsDomain(fakeErr)
	if !ok1 || !ok2 {
		t.Fatalf("both refusals must be domain errors; got %v and %v", realErr, fakeErr)
	}
	if realDE.Code != fakeDE.Code {
		t.Fatalf("caller-visible code differs: registered=%q unregistered=%q -- this is an existence oracle",
			realDE.Code, fakeDE.Code)
	}

	// The only permitted difference in the message is the echoed name, which is
	// the caller's own input. Strip it and the two must be identical.
	realMsg := strings.Replace(realDE.Message, ToolListSites, "NAME", 1)
	fakeMsg := strings.Replace(fakeDE.Message, "definitely_not_a_tool_xyzzy", "NAME", 1)
	if realMsg != fakeMsg {
		t.Fatalf("caller-visible message differs beyond the echoed name -- this is an existence oracle:\n"+
			"registered:   %s\nunregistered: %s", realMsg, fakeMsg)
	}

	if realReason == fakeReason {
		t.Fatalf("the OPERATOR-facing reason must distinguish the two cases; both were %q", realReason)
	}
	if realReason != reasonCapabilityNotHeld {
		t.Fatalf("registered-but-not-held reason = %q, want %q", realReason, reasonCapabilityNotHeld)
	}
	if fakeReason != reasonUnregistered {
		t.Fatalf("unregistered reason = %q, want %q", fakeReason, reasonUnregistered)
	}
}

// TestExitGate_VisibilityAndCallabilityAgree walks the whole registry against
// a connection holding each capability in turn and asserts the two paths never
// disagree: everything visible is callable, and everything callable was
// visible.
//
// This is the invariant that made the pre-S7 arrangement unsafe. It held then
// only for a client that called tools/list and believed the answer; here it is
// a property of the code, because both paths call visible().
func TestExitGate_VisibilityAndCallabilityAgree(t *testing.T) {
	site := uuid.New()

	// Every subset of the vocabulary that a single capability can produce, plus
	// the empty set and the full set.
	sets := []CapabilitySet{{}, NewCapabilitySet(AllCapabilities())}
	for _, c := range AllCapabilities() {
		sets = append(sets, NewCapabilitySet([]Capability{c}))
	}

	for _, caps := range sets {
		auth := authWith(caps, site)

		seen := map[string]bool{}
		for _, d := range VisibleTools(auth) {
			seen[d.Name] = true
			if _, _, err := AuthorizeTool(d.Name, auth); err != nil {
				t.Fatalf("caps=%v: %q was VISIBLE but not callable: %v", caps.Sorted(), d.Name, err)
			}
		}

		for _, e := range nonEmptyRegistry(t) {
			_, _, err := AuthorizeTool(e.Name, auth)
			if err == nil && !seen[e.Name] {
				t.Fatalf("caps=%v: %q was CALLABLE but never shown by tools/list", caps.Sorted(), e.Name)
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

	if got := VisibleTools(auth); len(got) != 1 || got[0].Name != ToolListSites {
		t.Fatalf("an empty site scope hid the tool from tools/list: %+v", got)
	}

	_, _, err := AuthorizeTool(ToolListSites, auth)
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != ErrCodeScopeEmpty {
		t.Fatalf("err = %v, want a %s domain error", err, ErrCodeScopeEmpty)
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

	entry, _, err := AuthorizeTool(ToolListSites, auth)
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
func TestNoRegisteredToolIsWriteShaped(t *testing.T) {
	forbidden := []string{
		"create", "update", "delete", "remove", "restart", "reboot",
		"install", "uninstall", "activate", "deactivate", "write",
		"set_", "purge", "restore", "rollback", "run_", "exec",
	}
	for _, e := range nonEmptyRegistry(t) {
		lower := strings.ToLower(e.Name)
		for _, verb := range forbidden {
			if strings.Contains(lower, verb) {
				t.Errorf("tool %q contains the write-shaped verb %q. The MCP surface is read-only "+
					"BY CONSTRUCTION (m124 DECISION 1) -- a write tool arrives with its own "+
					"capability, its own migration and its own security review, never by being "+
					"appended to registryTools.", e.Name, verb)
			}
		}
		if e.OperatorPermission == authz.PermSiteWrite {
			t.Errorf("tool %q declares the site:write operator permission on a read-only surface", e.Name)
		}
	}
}

// TestVisibleToolNamesDiscloseOnlyTheConnectionsOwnSurface guards the error
// body. It carries a tool list, and that list must be the connection's own
// tools/list answer -- never the full registry, which is what the pre-S7
// "unknown tool" error returned.
func TestVisibleToolNamesDiscloseOnlyTheConnectionsOwnSurface(t *testing.T) {
	auth := authWith(CapabilitySet{}, uuid.New())
	if got := visibleToolNames(auth); len(got) != 0 {
		t.Fatalf("a connection holding no capability was told about %v", got)
	}

	// len(got) == len(registry) is satisfied by 0 == 0, so the registry is
	// asserted non-empty first: otherwise this guard reports PASS on a server
	// with no tools at all.
	want := len(nonEmptyRegistry(t))
	full := authWith(NewCapabilitySet(AllCapabilities()), uuid.New())
	if got := visibleToolNames(full); len(got) != want {
		t.Fatalf("a fully-capable connection saw %d of %d tools", len(got), want)
	}
}
