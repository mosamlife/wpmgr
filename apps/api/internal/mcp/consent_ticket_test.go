// The binding between an authorize call and the approval that follows it,
// executed rather than argued about in a comment.
//
// WHY THESE TESTS PLANT A SECOND RECOGNISED SCOPE. The property under test --
// "the stored set is the set the authorize call carried" -- is only
// DISTINGUISHABLE from "the stored set is a set the registry recognises" when
// the registry has more than one member. With one member the two rules accept
// exactly the same values, so a test written against the shipped registry
// passes whether the binding exists or not, which is the vacuous pass this
// project treats as a defect.
//
// So these tests plant the second scope IN THE TEST PROCESS, the way
// authenticate_scope_column_test.go plants a grant row the database's CHECK
// constraint forbids: the value under test is unreachable through the shipped
// configuration, and the code that must handle it cannot be reached any other
// way. Nothing is added to the shipped registry -- withRecognisedScope restores
// it, and scope.go is unchanged -- and the scope named here is deliberately not
// a scope anyone intends to ship, so a future write scope cannot inherit a
// passing test it never ran.
package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// testSessionSecret keys the consent ticket codec for every test in this
// package. Any 32+ byte value works; it is a key, not a fixture with meaning.
const testSessionSecret = "test-session-secret-0123456789-abcdefghij"

// testTicketCodec is the codec validConsent() mints with and consentSvc()
// verifies with. One key on both sides is what makes a hand-built
// ConsentContext behave like one this server issued.
var testTicketCodec = func() *consentTicketCodec {
	c, err := newConsentTicketCodec(testSessionSecret)
	if err != nil {
		panic("mcp tests: " + err.Error())
	}
	return c
}()

// issueTestTicket seals a ticket for clientID over scopes, as of now.
func issueTestTicket(clientID string, scopes []Scope, now time.Time) string {
	tkt, err := testTicketCodec.issue(clientID, scopes, now)
	if err != nil {
		panic("mcp tests: issue consent ticket: " + err.Error())
	}
	return tkt
}

// consentSvc is NewService plus the shared ticket key.
//
// Every test that drives Approve goes through it rather than through
// NewService, because NewService arms a FRESH RANDOM key: a ticket minted
// anywhere else would be refused, and a test that cannot present a valid ticket
// proves nothing about what happens when someone presents an invalid one.
func consentSvc(store Store) *Service {
	// The audit recorder is not incidental: A10 makes an unaudited Service
	// refuse to approve, so a test that omitted it would be asserting about the
	// wrong refusal.
	return consentKeyed(NewService(store).withAuditRecorder(&capturingRecorder{}))
}

// consentKeyed arms svc with the shared test ticket key, preserving whatever
// else the caller has already wired onto it. Every test constructor that drives
// the approval path goes through it, so this package's fixtures present tickets
// a real authorize call would have issued.
func consentKeyed(svc *Service) *Service {
	if err := svc.SetConsentSigningSecret(testSessionSecret); err != nil {
		panic("mcp tests: " + err.Error())
	}
	return svc
}

// withRecognisedScope adds sc to the closed registry for the duration of one
// test and removes it afterwards.
//
// NOT PARALLEL-SAFE, deliberately and unavoidably: recognisedScopes is package
// state. No test in this file calls t.Parallel, and one that did would be
// mutating the registry underneath every other test in the package.
func withRecognisedScope(t *testing.T, sc Scope) {
	t.Helper()
	if _, exists := recognisedScopes[sc]; exists {
		t.Fatalf("withRecognisedScope: %q is already in the shipped registry; "+
			"this helper exists to plant a scope the build does NOT recognise, "+
			"and planting a real one would make the test assert nothing", sc)
	}
	recognisedScopes[sc] = struct{}{}
	t.Cleanup(func() { delete(recognisedScopes, sc) })
}

// withScopeConferring makes sc confer caps for the duration of one test, and
// restores the shipped map afterwards.
//
// IT IS WHAT MAKES THE PLANTED FAILURE HONEST. A scope that confers nothing is
// refused a few lines further on by the capability resolver, so an escalation
// test built on one would keep passing with the binding deleted -- refused for
// a reason that has nothing to do with what it claims to prove. Conferring an
// EXISTING capability on the planted scope removes that accidental backstop, so
// deleting the binding actually mints the grant. scopeCapabilities itself is
// unchanged; nothing here is conferred in the shipped build.
func withScopeConferring(t *testing.T, sc Scope, caps ...Capability) {
	t.Helper()
	if _, exists := scopeCapabilities[sc]; exists {
		t.Fatalf("withScopeConferring: %q already confers in the shipped map", sc)
	}
	scopeCapabilities[sc] = caps
	t.Cleanup(func() { delete(scopeCapabilities, sc) })
}

// plantedScope is the second registry member these tests run against. The
// spelling is not a scope anyone plans to ship.
const plantedScope = Scope("mcp:planted-test-only")

// authorizeForTest runs the real authorize path and returns the consent context
// it produced -- ticket included. Tests build their approval from THIS, the way
// the dashboard builds its POST from the consent response, so the honest path
// under test is the honest path in production.
func authorizeForTest(t *testing.T, svc *Service, scope string) ConsentContext {
	t.Helper()
	consent, err := svc.Authorize(context.Background(), AuthorizeRequest{
		ResponseType:        "code",
		ClientID:            registeredClientID,
		RedirectURI:         registeredRedirect,
		Scope:               scope,
		State:               "opaque-state",
		CodeChallenge:       "a-real-challenge-value",
		CodeChallengeMethod: CodeChallengeMethodS256,
	})
	if err != nil {
		t.Fatalf("Authorize(%q) refused a well-formed request: %v", scope, err)
	}
	if consent.ConsentTicket == "" {
		t.Fatal("Authorize returned no consent ticket, so nothing downstream can " +
			"tell what this request asked for")
	}
	return consent
}

func approvalFor(consent ConsentContext) ApprovalRequest {
	req := validApproval()
	req.Consent = consent
	return req
}

func mustNotHaveCreatedAGrant(t *testing.T, store *fakeStore) {
	t.Helper()
	for _, c := range store.calls {
		if c == "CreateGrantWithCode" {
			t.Fatal("a refused approval still created a grant; a minted grant and " +
				"an issued token are not undone by refusing afterwards")
		}
	}
}

// ---------------------------------------------------------------------------
// THE PROOF THAT MATTERS
// ---------------------------------------------------------------------------

// TestApprove_RefusesAScopeTheAuthorizeCallDidNotCarry is the direction where
// the wrong answer still "works": every value involved is well-formed, the
// client is registered, the redirect matches, the PKCE challenge is real, and
// the scope named in the body is one the server recognises. The only thing
// wrong with it is that the authorize call never asked for it and the operator
// was never shown it.
//
// Registry membership cannot see that. The ticket can.
func TestApprove_RefusesAScopeTheAuthorizeCallDidNotCarry(t *testing.T) {
	withRecognisedScope(t, plantedScope)
	// And it confers something, so nothing downstream refuses this approval for
	// an unrelated reason. Without this the test would pass with the binding
	// deleted.
	withScopeConferring(t, plantedScope, CapSitesRead)

	cases := map[string][]Scope{
		"substituted for the authorized scope": {plantedScope},
		"added alongside the authorized scope": {ScopeRead, plantedScope},
	}

	for name, tampered := range cases {
		t.Run(name, func(t *testing.T) {
			store := approvalStore()
			svc := consentSvc(store)

			// The operator saw, and approved, a screen that said mcp:read.
			consent := authorizeForTest(t, svc, string(ScopeRead))

			// The body that comes back names something else. Everything else on
			// it -- the ticket included -- is exactly what the server issued.
			req := approvalFor(consent)
			req.Consent.Scopes = tampered

			got, err := svc.Approve(context.Background(), req)
			if err == nil {
				t.Fatalf("Approve minted code %q for scopes %v, which the authorize "+
					"call never carried. The grant now holds authority the operator "+
					"was not shown and did not approve.", got.Code, tampered)
			}
			mustNotHaveCreatedAGrant(t, store)

			de, ok := domain.AsDomain(err)
			if !ok {
				t.Fatalf("the refusal is not a domain error (%v), so it renders as a "+
					"500 and names nothing the caller can act on", err)
			}
			if de.Code != ErrCodeScopeNotAuthorized {
				t.Fatalf("refusal code = %q, want %q. A caller told only that it was "+
					"denied retries the identical request forever.",
					de.Code, ErrCodeScopeNotAuthorized)
			}
			if !strings.Contains(de.Message, string(plantedScope)) {
				t.Fatalf("refusal message %q does not name the offending scope %q; "+
					"the whole point of a specific code is a specific remedy",
					de.Message, plantedScope)
			}
		})
	}
}

// TestApprove_StoresTheAuthorizedScopeSet is the over-fire test: the honest
// path, unchanged. A consent echoed back exactly as it was issued produces the
// same grant it always did.
//
// It asserts the STORED COLUMN, not merely that no error was returned. "It did
// not refuse" is compatible with having stored the wrong thing.
func TestApprove_StoresTheAuthorizedScopeSet(t *testing.T) {
	store := approvalStore()
	svc := consentSvc(store)

	consent := authorizeForTest(t, svc, string(ScopeRead))
	got, err := svc.Approve(context.Background(), approvalFor(consent))
	if err != nil {
		t.Fatalf("Approve refused an approval that matches its own authorize call: %v", err)
	}
	if got.Code == "" {
		t.Fatal("Approve returned no code on the honest path")
	}
	if len(store.approved) != 1 {
		t.Fatalf("CreateGrantWithCode called %d times, want 1", len(store.approved))
	}
	if want := []string{string(ScopeRead)}; !equalStrings(store.approved[0].OauthScopes, want) {
		t.Fatalf("stored oauth_scopes = %v, want %v", store.approved[0].OauthScopes, want)
	}
	if len(store.approved[0].Capabilities) == 0 {
		t.Fatal("the honest path stored an empty capability set; the binding must " +
			"not have narrowed what the grant may do")
	}
}

// TestApprove_AcceptsARepeatedScope is the over-fire test on the production
// path: RFC 6749 permits repetition and it adds no authority, so a body that
// repeats what it was shown has changed nothing and must not be refused. A
// guard that reddens correct work gets switched off, and then it guards
// nothing.
func TestApprove_AcceptsARepeatedScope(t *testing.T) {
	store := approvalStore()
	svc := consentSvc(store)

	consent := authorizeForTest(t, svc, string(ScopeRead))
	req := approvalFor(consent)
	req.Consent.Scopes = []Scope{ScopeRead, ScopeRead}

	if _, err := svc.Approve(context.Background(), req); err != nil {
		t.Fatalf("Approve refused a body that repeated the authorized scope: %v", err)
	}
	if want := []string{string(ScopeRead)}; !equalStrings(store.approved[0].OauthScopes, want) {
		t.Fatalf("stored oauth_scopes = %v, want %v", store.approved[0].OauthScopes, want)
	}
}

// TestAuthorizedScopes_ComparesSetsNotSequences pins the comparison itself over
// a two-member set, which is the only shape where ordering is observable.
//
// It exercises the binding DIRECTLY rather than through Approve, because a
// planted scope confers no capability -- OrgDefaultCapabilities refuses it a few
// lines later, by design, and that refusal would mask whichever answer this
// comparison gave. The capability ceiling is not what is under test here; what
// the binding accepts is.
func TestAuthorizedScopes_ComparesSetsNotSequences(t *testing.T) {
	withRecognisedScope(t, plantedScope)

	svc := consentSvc(approvalStore())
	consent := authorizeForTest(t, svc, string(ScopeRead)+" "+string(plantedScope))
	// Same set, reversed, with one member repeated.
	consent.Scopes = []Scope{plantedScope, ScopeRead, ScopeRead}

	got, err := svc.authorizedScopes(consent)
	if err != nil {
		t.Fatalf("the binding refused a body naming the same SET in a different order: %v", err)
	}
	// Canonical order is the ticket's, which is sorted: the planted spelling
	// sorts before mcp:read.
	want := []string{string(plantedScope), string(ScopeRead)}
	if !equalStrings(scopeNames(got), want) {
		t.Fatalf("resolved scopes = %v, want %v (the ticket's set, canonically ordered, "+
			"not the body's ordering)", scopeNames(got), want)
	}
}

// TestApprove_RefusesABodyNarrowerThanTheAuthorizeCall. Narrowing is not
// escalation, and it is still refused: the consent screen offers no
// scope-narrowing control, so a narrower body is not an operator's decision. It
// is the round trip disagreeing with itself, and accepting it means the screen
// said one thing and the grant recorded another.
func TestApprove_RefusesABodyNarrowerThanTheAuthorizeCall(t *testing.T) {
	withRecognisedScope(t, plantedScope)
	withScopeConferring(t, plantedScope, CapSitesRead)

	store := approvalStore()
	svc := consentSvc(store)

	consent := authorizeForTest(t, svc, string(ScopeRead)+" "+string(plantedScope))
	req := approvalFor(consent)
	req.Consent.Scopes = []Scope{ScopeRead}

	_, err := svc.Approve(context.Background(), req)
	if err == nil {
		t.Fatal("Approve accepted a body that silently dropped a scope the consent " +
			"screen displayed")
	}
	mustNotHaveCreatedAGrant(t, store)

	de, ok := domain.AsDomain(err)
	if !ok || de.Code != ErrCodeScopeNotAuthorized {
		t.Fatalf("refusal = %v, want a domain error coded %q", err, ErrCodeScopeNotAuthorized)
	}
	if !strings.Contains(de.Message, string(plantedScope)) {
		t.Fatalf("refusal message %q does not name the dropped scope %q", de.Message, plantedScope)
	}
}

// ---------------------------------------------------------------------------
// THE TICKET ITSELF -- absent, altered, expired, or issued for someone else.
// Each is a refusal with its own code, because each has its own remedy.
// ---------------------------------------------------------------------------

func TestApprove_RefusesAnApprovalWithNoUsableConsentTicket(t *testing.T) {
	honest := func(svc *Service, t *testing.T) ConsentContext {
		return authorizeForTest(t, svc, string(ScopeRead))
	}

	cases := map[string]func(*Service, *testing.T) ConsentContext{
		"no ticket at all": func(svc *Service, t *testing.T) ConsentContext {
			c := honest(svc, t)
			c.ConsentTicket = ""
			return c
		},
		"a ticket this server never issued": func(svc *Service, t *testing.T) ConsentContext {
			c := honest(svc, t)
			c.ConsentTicket = "ZXlKaklqb2lZeUo5.bm90LWEtcmVhbC1tYWM"
			return c
		},
		"a ticket whose signature no longer matches its payload": func(svc *Service, t *testing.T) ConsentContext {
			c := honest(svc, t)
			body, sig, _ := strings.Cut(c.ConsentTicket, ".")
			// Re-seal the payload of a DIFFERENT request under the old
			// signature: the classic split-the-token substitution.
			other := issueTestTicket(registeredClientID, []Scope{ScopeRead},
				time.Now().Add(time.Minute))
			otherBody, _, _ := strings.Cut(other, ".")
			if otherBody == body {
				t.Skip("the two payloads collided; nothing to substitute")
			}
			c.ConsentTicket = otherBody + "." + sig
			return c
		},
		"an expired ticket": func(svc *Service, t *testing.T) ConsentContext {
			c := honest(svc, t)
			c.ConsentTicket = issueTestTicket(registeredClientID, []Scope{ScopeRead},
				time.Now().Add(-consentTicketTTL-time.Minute))
			return c
		},
		"a ticket issued for another client": func(svc *Service, t *testing.T) ConsentContext {
			c := honest(svc, t)
			c.ConsentTicket = issueTestTicket("some-other-registered-client",
				[]Scope{ScopeRead}, time.Now())
			return c
		},
	}

	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			store := approvalStore()
			svc := consentSvc(store)

			req := approvalFor(build(svc, t))
			got, err := svc.Approve(context.Background(), req)
			if err == nil {
				t.Fatalf("Approve minted code %q for an approval carrying %s; the "+
					"scope set it stored is then whatever the body said", got.Code, name)
			}
			mustNotHaveCreatedAGrant(t, store)

			de, ok := domain.AsDomain(err)
			if !ok || de.Code != ErrCodeConsentTicketInvalid {
				t.Fatalf("refusal = %v, want a domain error coded %q",
					err, ErrCodeConsentTicketInvalid)
			}
		})
	}
}

// TestConsentTicketIsNotVerifiableWithADifferentKey pins the key derivation
// itself: two instances that do not share the session secret must not accept
// each other's tickets, which is the property that makes the shared secret
// (rather than a per-process key) the correct wiring.
func TestConsentTicketIsNotVerifiableWithADifferentKey(t *testing.T) {
	other, err := newConsentTicketCodec("a-completely-different-session-secret-32b")
	if err != nil {
		t.Fatalf("newConsentTicketCodec: %v", err)
	}
	tkt := issueTestTicket(registeredClientID, []Scope{ScopeRead}, time.Now())
	if _, ok := other.open(tkt, time.Now()); ok {
		t.Fatal("a ticket verified under a key it was not issued with")
	}
	if _, ok := testTicketCodec.open(tkt, time.Now()); !ok {
		t.Fatal("a ticket did not verify under its own key")
	}
}

// TestNewConsentTicketCodecRefusesAWeakSecret. The alternative to refusing is
// deriving a key from a short string, which is a signature nobody can rely on
// wearing the shape of one they can.
func TestNewConsentTicketCodecRefusesAWeakSecret(t *testing.T) {
	if _, err := newConsentTicketCodec("too-short"); err == nil {
		t.Fatal("newConsentTicketCodec accepted a secret too short to derive a key from")
	}
}

// TestNewServiceArmsAConsentTicketCodec. There must be no Service anywhere that
// skips the check for want of a key: an unarmed codec would be a wiring failure
// presenting as a working endpoint, which is the same reason NewService arms
// its own rate limiter.
func TestNewServiceArmsAConsentTicketCodec(t *testing.T) {
	svc := NewService(approvalStore())
	if svc.consentTickets == nil {
		t.Fatal("NewService left the consent ticket codec nil")
	}
	consent := authorizeForTest(t, svc, string(ScopeRead))
	if _, ok := svc.consentTickets.open(consent.ConsentTicket, time.Now()); !ok {
		t.Fatal("a Service could not verify the ticket it had just issued")
	}
	// And it is not the shared test key: an unwired Service is alone, not open.
	if _, ok := testTicketCodec.open(consent.ConsentTicket, time.Now()); ok {
		t.Fatal("NewService's ephemeral codec produced a ticket verifiable under " +
			"another key, so its key is not ephemeral")
	}
}

func equalStrings(got, want []string) bool {
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
