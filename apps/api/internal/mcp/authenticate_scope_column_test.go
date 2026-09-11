// The failure direction m136's read exists to close, executed through
// Service.Authenticate rather than argued about in a comment.
//
// WHY THESE PLANT THROUGH THE FAKE STORE AND NOT THROUGH POSTGRES. The value
// under test is UNSTORABLE: mcp_grants_oauth_scopes_vocabulary_check refuses an
// unrecognised scope and mcp_grants_oauth_scopes_not_empty_check refuses an
// empty set, and the integration suite proves both of those refusals are live
// as wpmgr_app. That is exactly why the Go half has to be tested here: m136
// DECISION 5(d) says the read "must not depend on the database being the only
// writer", and a test that can only plant what the database permits cannot
// check what happens when something else writes the row. The fake store is the
// only place a row the CHECK forbids can be put in front of the code that reads
// it.
package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// authWithStoredScopes builds a live, authorized recheck row carrying an
// arbitrary oauth_scopes column, and returns the service in front of it. Every
// other field is the ordinary healthy connection, so the only variable across
// the tests below is the scope column.
func authWithStoredScopes(storedScopes []string) *Service {
	tenantID := uuid.New()
	tok := liveToken(tenantID)
	return NewService(&fakeStore{
		tokenOK: true, token: tok,
		recheckOK: true,
		recheck: sqlc.ReCheckMCPRequestAuthorizationInTenantTxRow{
			GrantID: tok.GrantID, GrantStatus: "active",
			SiteScopeMode: "all", TokenID: tok.ID, TokenStatus: "active",
			TokenExpiresAt:    tok.ExpiresAt,
			GrantCapabilities: []string{string(CapSitesRead)},
			GrantOauthScopes:  storedScopes,
			GrantExpiresAt:    time.Now().UTC().Add(90 * 24 * time.Hour),
			Authorized:        true,
		},
		scopeSites: []uuid.UUID{uuid.New()},
	})
}

// TestAuthenticateRefusesAGrantHoldingAnUnrecognisedScope IS THE PROOF THAT
// MATTERS, because it is the direction where the wrong answer still works.
//
// The row holds {mcp:read, mcp:not-a-scope}. The tempting implementation trims
// the unknown entry and authenticates the connection on {mcp:read} alone: it is
// fail-closed relative to the row, every client keeps working, and NOBODY EVER
// LEARNS that the credential's stored terms and its resolved authority
// disagree. That is the failure. A scope this build does not recognise is a
// grant this build cannot correctly evaluate, and the honest answer to "I
// cannot tell what this grant may do" is to refuse it.
//
// It is the same stance one layer up: ParseRequestedScopes refuses the WHOLE
// request on an unrecognised scope rather than dropping the token from it, and
// NarrowTo refuses a capability the ceiling does not hold rather than
// intersecting it away.
func TestAuthenticateRefusesAGrantHoldingAnUnrecognisedScope(t *testing.T) {
	svc := authWithStoredScopes([]string{string(ScopeRead), "mcp:not-a-scope"})

	auth, err := svc.Authenticate(context.Background(), "the-bearer-token")
	if err == nil {
		t.Fatalf("a grant whose oauth_scopes column holds an unrecognised scope "+
			"AUTHENTICATED, resolving to %v.\n"+
			"The row says {mcp:read, mcp:not-a-scope} and the connection was "+
			"admitted as though it said {mcp:read}. That is grantScopes having "+
			"trimmed the unknown entry, or OrgDefaultCapabilities having skipped "+
			"it -- either way the stored terms and the granted authority "+
			"disagree and nothing anywhere says so.", auth.Capabilities.Sorted())
	}

	// AND IT IS THE RIGHT REFUSAL, not an incidental one. A 500 here would also
	// make the assertion above pass while telling the operator nothing, so the
	// domain error is checked by code.
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("the refusal is not a domain error (%v), so it renders as a 500 "+
			"and names nothing the operator can act on", err)
	}
	if de.Code != ErrCodeCapabilityUnmapped {
		t.Fatalf("refusal code = %q, want %q", de.Code, ErrCodeCapabilityUnmapped)
	}
	t.Logf("a grant holding an unrecognised scope was refused: %v", err)
}

// TestAuthenticateRefusesAGrantWhoseScopeColumnIsEmpty covers the other
// direction absence can fail in. An empty or nil column must mean NO AUTHORITY;
// the bug it guards against is a future reader deciding that "no scope
// restriction recorded" means "no restriction".
//
// mcp_grants_oauth_scopes_not_empty_check makes the value unrepresentable, so
// reaching this state means a writer outside that constraint -- which is
// precisely the case the Go read may not assume away.
func TestAuthenticateRefusesAGrantWhoseScopeColumnIsEmpty(t *testing.T) {
	for name, stored := range map[string][]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			auth, err := authWithStoredScopes(stored).
				Authenticate(context.Background(), "the-bearer-token")
			if err == nil {
				t.Fatalf("a grant with a %s oauth_scopes column authenticated, "+
					"resolving to %v. Absence must never widen.",
					name, auth.Capabilities.Sorted())
			}
			de, ok := domain.AsDomain(err)
			if !ok || de.Code != ErrCodeCapabilityUnmapped {
				t.Fatalf("refusal = %v, want a %q domain error",
					err, ErrCodeCapabilityUnmapped)
			}
		})
	}
}

// TestAuthenticateGivesTheDefaultGrantExactlyWhatItHasToday is the
// no-regression pin, and it is the reason the three tests above are safe to
// assert: a change that refuses too much would be caught here rather than in
// production.
//
// Every grant this surface has ever minted holds {mcp:read} on the scope axis
// and {mcp.sites.read} on the capability axis, and m136's backfill wrote the
// first of those onto every existing row. This asserts the resolved set is
// EXACTLY that -- not merely that it contains it. A wider resolved set would be
// the widening this whole commit exists to make impossible, arriving at read
// time instead of at write time.
func TestAuthenticateGivesTheDefaultGrantExactlyWhatItHasToday(t *testing.T) {
	svc := authWithStoredScopes(scopeNames(DefaultGrantScopes()))

	auth, err := svc.Authenticate(context.Background(), "the-bearer-token")
	if err != nil {
		t.Fatalf("the ordinary grant every live connection is no longer "+
			"authenticates: %v", err)
	}
	if !auth.Capabilities.Allows(CapSitesRead) {
		t.Fatalf("resolved capabilities = %v, want to hold %q",
			auth.Capabilities.Sorted(), CapSitesRead)
	}
	if auth.Capabilities.Len() != 1 {
		t.Fatalf("a grant whose row holds {mcp:read} on the scope axis and "+
			"{mcp.sites.read} on the capability axis resolved to %v.\n"+
			"Exactly one, or this commit has widened a credential nobody "+
			"re-consented to.", auth.Capabilities.Sorted())
	}
}
