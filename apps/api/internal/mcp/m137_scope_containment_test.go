// m137 made the requestable scope set PER-CLIENT: Authorize and Approve both
// refuse a scope the client's own mcp_oauth_clients.registered_scopes does not
// contain. This file proves both refuse it, and that neither refuses the
// honest request.
//
// WHY THIS CANNOT BE A tests/ INTEGRATION FILE, WHICH IS THE FIRST THING A
// READER WILL ASK. The behaviour under test is a DISAGREEMENT between two
// closed sets -- "a scope this server recognises" and "a scope this client
// registered" -- and with one member in recognisedScopes the two sets accept
// identical values. Postgres cannot be made to hold the divergence either:
// mcp_oauth_clients_registered_scopes_vocabulary_check refuses a scope Go does
// not recognise, and the present check refuses '{}'. There is no pair of
// values, storable in the real database, where containment and registry
// membership give different answers. The divergence has to be manufactured in
// the test process.
//
// THAT IS THE MOVE authenticate_scope_column_test.go MAKES, AND FOR THE SAME
// REASON, in the opposite direction. That file plants a STORED scope the CHECK
// forbids, because -- in its words -- "a test that can only plant what the
// database permits cannot check what happens when something else writes the
// row". Here the unplantable value is on the REQUEST side, so what gets planted
// is a second member of recognisedScopes, via the withRecognisedScope helper
// consent_ticket_test.go already established for exactly this; the fake store
// then supplies a client row registered for only the first.
//
// THE REFUSALS THE REAL DATABASE PERFORMS ARE NOT THIS FILE'S JOB. Those are
// tests/mcp_m137_client_scope_vocabulary_parity_test.go and the m136 pair one
// table over. This file proves the GO read, which is the half no constraint can
// cover: the column is an input to an authorisation comparison, and a
// comparison is not a constraint.
//
// NOTHING HERE MAY CALL t.Parallel: planting mutates package-level maps.
package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// plantForContainment arms the two-scope world every test below needs, and
// proves the plant actually took before any of them relies on it.
//
// BOTH PLANTS ARE LOAD-BEARING AND NEITHER IS DECORATION.
//
// withRecognisedScope is what makes the planted scope REQUESTABLE. Without it
// ParseRequestedScopes refuses first, on registry membership, and every refusal
// in this file would be a registry refusal wearing a containment refusal's
// clothes -- passing with the containment check deleted.
//
// withScopeConferring is what makes the PLANTED FAILURE HONEST, and the reason
// is consent_ticket_test.go's, unchanged: a scope that confers no capability is
// refused a few lines further on by resolveGrantCapabilities, so the Approve
// test below would still pass with the containment check removed, refused for a
// reason that has nothing to do with what it claims to prove. Conferring an
// existing capability removes that accidental backstop so deleting the check
// really does mint the grant. scopeCapabilities is unchanged in the shipped
// build; nothing here is conferred outside this test's lifetime.
func plantForContainment(t *testing.T) Scope {
	t.Helper()
	withRecognisedScope(t, plantedScope)
	withScopeConferring(t, plantedScope, CapSitesRead)

	// The plant is only useful if it widened the registry. Checked rather than
	// assumed: this is precisely the "guard that cannot find its subject"
	// shape, and it would make every assertion below vacuous.
	if _, err := ParseRequestedScopes(string(plantedScope)); err != nil {
		t.Fatalf("planting %q did not make it requestable: ParseRequestedScopes "+
			"still refuses it (%v), so every refusal in this file would be a "+
			"registry refusal and would prove nothing about m137 containment",
			string(plantedScope), err)
	}
	return plantedScope
}

// clientRegisteredForReadOnly is the whole point of the fixture: a client whose
// registration names mcp:read and nothing else, while the registry recognises
// more than that.
//
// RegisteredScopes IS SET EXPLICITLY AND NOT LEFT TO liveClient. liveClient
// fills it from registeredScopesForOmittedRequest(), which reads the registry
// AT CALL TIME -- so after a plant it would return BOTH scopes and the client
// would be registered for the very scope the test is about to prove it cannot
// request. That bug presents as "containment does not refuse", which is
// indistinguishable from the defect this file exists to catch.
func clientRegisteredForReadOnly() *fakeStore {
	client := liveClient(registeredRedirect)
	client.RegisteredScopes = []string{string(ScopeRead)}
	return &fakeStore{clientOK: true, client: client}
}

// authorizeReqWithScope builds the same request authorizeForTest builds, but
// returns it rather than running it: authorizeForTest calls t.Fatalf on a
// refusal, and every case below is testing for one.
func authorizeReqWithScope(scope string) AuthorizeRequest {
	return AuthorizeRequest{
		ResponseType:        "code",
		ClientID:            registeredClientID,
		RedirectURI:         registeredRedirect,
		Scope:               scope,
		State:               "opaque-state",
		CodeChallenge:       "a-real-challenge-value",
		CodeChallengeMethod: CodeChallengeMethodS256,
	}
}

// TestAuthorizeRefusesAScopeTheClientNeverRegistered is the first entry point,
// and the one that stops the escalation at its source: no consent ticket is
// sealed, so Approve is never reached with a set to spend.
func TestAuthorizeRefusesAScopeTheClientNeverRegistered(t *testing.T) {
	planted := plantForContainment(t)

	cases := map[string]string{
		// The escalation proper: smuggle the unregistered scope alongside the
		// one the client is entitled to. This is the shape that survives a
		// careless "does the request contain mcp:read" check.
		"registered scope plus an unregistered one": string(ScopeRead) + " " + string(planted),
		// The bare form: nothing the client registered for at all.
		"only an unregistered scope": string(planted),
		// Order must not matter. A check that returns on the first MATCH rather
		// than checking every member passes one of these and fails the other.
		"unregistered scope first": string(planted) + " " + string(ScopeRead),
	}

	for name, scope := range cases {
		t.Run(name, func(t *testing.T) {
			store := clientRegisteredForReadOnly()

			got, err := consentSvc(store).Authorize(context.Background(),
				authorizeReqWithScope(scope))
			if err == nil {
				t.Fatalf("Authorize ACCEPTED scope %q for a client registered only "+
					"for %v, returning scopes %v.\n"+
					"The registry recognises the planted scope and the client does "+
					"not, so this is the containment check missing, not the registry "+
					"check: the requested set is bounded by what THIS SERVER knows "+
					"rather than by what THIS CLIENT registered.",
					scope, store.client.RegisteredScopes, got.Scopes)
			}

			// AND IT IS THE RIGHT REFUSAL. RFC 6749 section 5.2 invalid_scope,
			// which the OAuth handlers map explicitly to 400. Another code falls
			// through to the generic renderer and answers 422; a non-domain
			// error renders 500. Both satisfy the assertion above while telling
			// the client nothing it can act on.
			de, ok := domain.AsDomain(err)
			if !ok {
				t.Fatalf("the refusal is not a domain error (%v), so it renders as "+
					"a 500 rather than invalid_scope", err)
			}
			if de.Code != ErrCodeInvalidScope {
				t.Fatalf("refusal code = %q, want %q", de.Code, ErrCodeInvalidScope)
			}

			// NO TICKET, AND THIS IS THE LOAD-BEARING HALF. The ticket is the
			// durable artefact of an authorize call: Approve measures the
			// approval body against it, so a ticket sealing an unregistered
			// scope would outlive this refusal and stay spendable for its whole
			// TTL. Refusing while still issuing one would be no refusal at all.
			if got.ConsentTicket != "" {
				t.Fatal("Authorize refused the request but still issued a consent " +
					"ticket; the ticket seals the scope set Approve will store, so " +
					"the refusal must precede it")
			}
			t.Logf("Authorize refused %q: %v", scope, err)
		})
	}
}

// TestAuthorizeStillAcceptsTheScopeTheClientRegistered is the accept arm, and it
// is not optional: a containment check that refuses everything closes the
// escalation and the product with it, and gets switched off by the first person
// to notice. Same fixture, same planted registry -- only the requested scope
// differs.
func TestAuthorizeStillAcceptsTheScopeTheClientRegistered(t *testing.T) {
	plantForContainment(t)
	store := clientRegisteredForReadOnly()

	got, err := consentSvc(store).Authorize(context.Background(),
		authorizeReqWithScope(string(ScopeRead)))
	if err != nil {
		t.Fatalf("Authorize refused %q for a client registered for exactly that "+
			"scope: %v. Containment must bound the request, not forbid it.",
			string(ScopeRead), err)
	}
	if len(got.Scopes) != 1 || got.Scopes[0] != ScopeRead {
		t.Fatalf("consent context scopes = %v, want [%s]", got.Scopes, string(ScopeRead))
	}
	if got.ConsentTicket == "" {
		t.Fatal("an accepted authorize call issued no consent ticket, so no " +
			"approval could ever be made against it")
	}
}

// TestApproveRefusesAScopeTheClientNeverRegistered is the second entry point,
// and the one it is easy to argue away.
//
// THE ARGUMENT THAT IT IS REDUNDANT, AND WHY IT FAILS. With Authorize
// constrained, no ticket for an unregistered scope can be minted -- so how does
// a body carrying one ever reach here? Because the ticket is stateless, signed
// and TTL'd: one issued by a build that PREDATES the Authorize check is still
// valid after deploy, and the approval it authorises is exactly the escalation.
// The ticket attests that some authorize call validated a pair, not that the
// call which validated it applied today's rules. Approve holds the client row
// already -- it calls LookupClient to re-match the redirect for the same class
// of reason -- so this check costs a map lookup and removes the question.
//
// The ticket below is minted DIRECTLY rather than obtained from Authorize, for
// precisely that reason: it stands in for one Authorize would no longer issue.
func TestApproveRefusesAScopeTheClientNeverRegistered(t *testing.T) {
	planted := plantForContainment(t)
	store := clientRegisteredForReadOnly()

	req := validApproval()
	req.Consent.Scopes = []Scope{ScopeRead, planted}
	// Sealed over BOTH scopes, so authorizedScopes is satisfied: the body
	// matches its ticket exactly. Every check that stood here before m137
	// passes, which is what makes this an honest test of the new one. A ticket
	// sealing only mcp:read would be refused by the binding first and would
	// prove nothing about containment.
	req.Consent.ConsentTicket = issueTestTicket(
		registeredClientID, []Scope{ScopeRead, planted}, time.Now())

	got, err := auditedService(store).Approve(context.Background(), req)
	if err == nil {
		t.Fatalf("Approve MINTED code %q for scope set %v against a client "+
			"registered only for %v.\n"+
			"The grant row would carry the unregistered scope into "+
			"mcp_grants.oauth_scopes, and the capability ceiling is derived from "+
			"that column -- so the escalation outlives the request.",
			got.Code, req.Consent.Scopes, store.client.RegisteredScopes)
	}
	if got.Code != "" {
		t.Fatal("a refused approval still returned a code")
	}

	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("the refusal is not a domain error (%v), so it renders as a 500", err)
	}
	if de.Code != ErrCodeInvalidScope {
		t.Fatalf("refusal code = %q, want %q.\n"+
			"A refusal from the ticket binding (%q) instead would mean this test "+
			"never reached the containment check and proves nothing about it.",
			de.Code, ErrCodeInvalidScope, ErrCodeScopeNotAuthorized)
	}

	// NOTHING WAS WRITTEN. A minted grant is not undone by refusing afterwards,
	// which is the standing lesson of this package's review findings.
	for _, c := range store.callLog() {
		if c == "CreateGrantWithCode" {
			t.Fatal("a refused approval still created a grant")
		}
	}
	t.Logf("Approve refused an unregistered scope carried by a valid ticket: %v", err)
}

// TestApproveStillAcceptsTheScopeTheClientRegistered is Approve's accept arm,
// under the same planted registry.
func TestApproveStillAcceptsTheScopeTheClientRegistered(t *testing.T) {
	plantForContainment(t)
	store := clientRegisteredForReadOnly()

	got, err := auditedService(store).Approve(context.Background(), validApproval())
	if err != nil {
		t.Fatalf("Approve refused an approval for %q against a client registered "+
			"for exactly that scope: %v", string(ScopeRead), err)
	}
	if got.Code == "" {
		t.Fatal("an accepted approval minted no code")
	}
}

// TestContainmentRefusesAClientRegisteredForNothing pins m137 section (5)(d)
// and DECISION 4: an empty registration means NO AUTHORITY, never "no
// restriction recorded, therefore unrestricted".
//
// The database makes '{}' unstorable, and this test does not depend on that --
// which is the point. The read must not assume the database is the only writer,
// and the wrong implementation (`if len(registered) == 0 { skip }`) passes every
// other test in this file while turning containment off entirely for exactly
// the row that has no business authorising anything.
//
// It needs no plant: with an empty registration even mcp:read is outside the
// client's set, so the divergence exists at one recognised scope.
func TestContainmentRefusesAClientRegisteredForNothing(t *testing.T) {
	for name, registered := range map[string][]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			store := clientRegisteredForReadOnly()
			store.client.RegisteredScopes = registered

			got, err := consentSvc(store).Authorize(context.Background(),
				authorizeReqWithScope(string(ScopeRead)))
			if err == nil {
				t.Fatalf("a client with a %s registered_scopes column was authorized "+
					"for %v. Absence must never widen: this is the len()==0 skip that "+
					"reinstates the global registry.", name, got.Scopes)
			}
			de, ok := domain.AsDomain(err)
			if !ok || de.Code != ErrCodeInvalidScope {
				t.Fatalf("refusal = %v, want a %q domain error", err, ErrCodeInvalidScope)
			}
		})
	}
}

// TestRegisterRecordsAConcreteScopeSet holds the other end of the bound.
//
// Containment is only worth having if registration writes something narrower
// than "everything". This asserts the INSERT names the column at all -- it was
// a 23502 between m137 and this change -- and that the value is neither empty
// (a client that could never authorize) nor divorced from the registry (a
// 23514 against the real vocabulary CHECK).
//
// It deliberately does NOT hard-code {mcp:read}: the decision recorded at
// registeredScopesForOmittedRequest is "the full recognised registry", and a
// test asserting the literal would pass unchanged if someone replaced that
// function with a constant. The tripwire below covers the day the registry grows.
func TestRegisterRecordsAConcreteScopeSet(t *testing.T) {
	store := &fakeStore{registerRows: 1}

	if _, err := NewService(store).Register(context.Background(), RegistrationRequest{
		RedirectURIs: []string{registeredRedirect},
		ClientName:   "Claude Desktop",
	}); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	if len(store.client.RegisteredScopes) == 0 {
		t.Fatal("Register wrote an empty registered_scopes; the column is NOT NULL " +
			"with a cardinality >= 1 check, so against the real database this is a " +
			"23502 or a 23514, and a client that could never authorize if it landed")
	}
	for _, s := range store.client.RegisteredScopes {
		if _, ok := recognisedScopes[Scope(s)]; !ok {
			t.Fatalf("Register wrote %q, which is not a recognised scope; the "+
				"vocabulary CHECK refuses it with 23514", s)
		}
	}
	t.Logf("registration recorded registered_scopes=%v", store.client.RegisteredScopes)
}

// TestOmittedRegistrationScopeMustBeRevisitedWhenTheRegistryGrows is a
// TRIPWIRE, not an assertion about today.
//
// registeredScopesForOmittedRequest resolves an omitted RFC 7591 `scope` to the
// FULL RECOGNISED REGISTRY. That is safe and behaviour-preserving while the
// registry holds one member -- it is exactly {mcp:read}, the value m137
// backfilled onto every client that already existed. It stops being safe the
// moment a second scope is recognised, because a client that says nothing would
// then be registered for the new scope too and containment would have nothing
// to bite on. That is the trap m137 DECISION 3 refused to build into the schema
// as a DEFAULT, rebuilt one layer up.
//
// A comment asking to be revisited is a wish. This is the mechanism: adding the
// second scope turns this red and the author has to go and choose.
//
// HOW TO MAKE IT GREEN AGAIN, since a red test with no stated remedy gets
// deleted: decide what an omitted `scope` means with more than one scope on the
// menu -- refuse the registration, parse RFC 7591 `scope` for real, or pin the
// default to a named conservative subset -- change
// registeredScopesForOmittedRequest to match, and rewrite this test to hold the
// new rule. Do not widen the bound to silence it.
//
// IT MUST NOT PLANT. Every other test here widens the registry deliberately;
// this one reads the SHIPPED registry, so it must run with no plant in force.
// t.Cleanup restores the map after each planting test, and nothing in this
// package calls t.Parallel, so by the time this runs the registry is the real one.
func TestOmittedRegistrationScopeMustBeRevisitedWhenTheRegistryGrows(t *testing.T) {
	if len(recognisedScopes) > 1 {
		t.Fatalf("recognisedScopes now holds %d scopes (%v), and "+
			"registeredScopesForOmittedRequest still resolves an omitted RFC 7591 "+
			"`scope` to ALL of them.\n"+
			"A client that names no scope would be registered for the new one, so "+
			"the containment check in Authorize and Approve would bound it to "+
			"everything -- which is no bound. Read the comment on "+
			"registeredScopesForOmittedRequest and choose again.",
			len(recognisedScopes), SupportedScopes())
	}
}
