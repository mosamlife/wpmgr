package mcp

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// THE EXIT GATE (m124 obligation 3).
//
// "A client that requests no recognised scope is REFUSED, not granted a
// default." The migration makes the unset value unstorable; this is the
// REQUEST side of the same gate, and it is S6's stated exit criterion.
//
// The failure this differentiates against is the project's signature defect:
// an absence coerced into a plausible value and reported as success. Here that
// is a missing, blank or unrecognised `scope` parameter read as full access.
// ---------------------------------------------------------------------------

func TestParseRequestedScopes_NoRecognisedScopeIsRefusedNotDefaulted(t *testing.T) {
	// Every one of these is an ABSENCE. None of them may produce a grant.
	cases := []struct {
		name string
		raw  string
	}{
		{"parameter omitted entirely", ""},
		{"single space", " "},
		{"tabs and newlines only", "\t\n  \r"},
		{"unrecognised scope alone", "fleet:admin"},
		{"unrecognised scope, plausible prefix", "mcp:"},
		{"unrecognised scope, plausible suffix", "read"},
		{"case-mismatched scope", "MCP:READ"},
		{"write scope, which this surface never grants", "mcp:write"},
		{"only separators", "   "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRequestedScopes(tc.raw)

			if err == nil {
				t.Fatalf("ParseRequestedScopes(%q) returned no error and scopes %v; "+
					"a request naming no recognised scope MUST be refused, never "+
					"granted a default", tc.raw, got)
			}
			if len(got) != 0 {
				t.Fatalf("ParseRequestedScopes(%q) refused with err=%v but still "+
					"returned %d scope(s) %v; a refused request must carry no "+
					"authority at all", tc.raw, err, len(got), got)
			}

			// The refusal must be a typed domain error the OAuth error
			// response can render as RFC 6749 `invalid_scope`, not an
			// unclassified infra error that httpx would turn into a 500.
			var domErr *domain.Error
			if !errors.As(err, &domErr) {
				t.Fatalf("ParseRequestedScopes(%q) refused with a non-domain error %T (%v); "+
					"the OAuth error response cannot classify it", tc.raw, err, err)
			}
			if domErr.Code != ErrCodeInvalidScope {
				t.Errorf("ParseRequestedScopes(%q) refused with code %q, want %q",
					tc.raw, domErr.Code, ErrCodeInvalidScope)
			}
		})
	}
}

// A partially-recognised request must be refused WHOLE. Silently dropping the
// unrecognised half and granting the remainder is the same fail-open shape
// wearing a different hat: the client asked for something we did not
// understand, and proceeding means the operator consents to a scope set that
// is not the one that was requested.
func TestParseRequestedScopes_UnrecognisedScopeRefusesTheWholeRequest(t *testing.T) {
	got, err := ParseRequestedScopes(string(ScopeRead) + " fleet:destroy")
	if err == nil {
		t.Fatalf("a request mixing a recognised and an unrecognised scope returned "+
			"%v with no error; it must be refused whole, not silently narrowed", got)
	}
	if len(got) != 0 {
		t.Fatalf("refused request still returned %v", got)
	}
}

// The gate must not over-fire: a correct request still works. A guard that
// reddens correct work gets switched off, and then it guards nothing.
func TestParseRequestedScopes_RecognisedScopeIsGranted(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"exact", "mcp:read"},
		{"leading and trailing whitespace", "  mcp:read  "},
		{"repeated, per RFC 6749 space delimiting", "mcp:read mcp:read"},
		{"multiple internal spaces", "mcp:read   mcp:read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRequestedScopes(tc.raw)
			if err != nil {
				t.Fatalf("ParseRequestedScopes(%q) refused a valid request: %v", tc.raw, err)
			}
			if len(got) != 1 || got[0] != ScopeRead {
				t.Fatalf("ParseRequestedScopes(%q) = %v, want exactly [%s]", tc.raw, got, ScopeRead)
			}
		})
	}
}

// The registry is closed and read-only by construction (m124 DECISION 1: "no
// capability column exists, and that is a decision"). If a write scope ever
// appears in the registry it must arrive with its own review, not by drifting
// in. This test is the tripwire.
func TestRecognisedScopes_AreReadOnlyAndClosed(t *testing.T) {
	if len(recognisedScopes) != 1 {
		t.Fatalf("recognised scope registry has %d entries, want exactly 1; a new "+
			"scope on the read-only MCP surface needs its own review",
			len(recognisedScopes))
	}
	for s := range recognisedScopes {
		if strings.Contains(string(s), "write") || strings.Contains(string(s), "admin") {
			t.Errorf("scope %q implies mutation; the MCP surface is read-only by "+
				"construction and exposes no write tool", s)
		}
	}
}

// ---------------------------------------------------------------------------
// THE EMPTY-SET TRAP (m124 obligation 2).
//
// "An empty resolved set must mean NO SITES, never every site." The CHECK
// stops an empty payload being STORED; nothing stops Go computing an empty set
// from a tag that matches no site and then reading it as absence of a filter.
// ---------------------------------------------------------------------------

func TestSiteSet_EmptyAllowsNothing(t *testing.T) {
	var zero SiteSet // the zero value is the one a bug reaches by accident
	if zero.Len() != 0 {
		t.Fatalf("zero SiteSet has Len() = %d, want 0", zero.Len())
	}
	if zero.Allows(uuid.New()) {
		t.Fatal("the ZERO SiteSet allowed a site; empty must mean NO sites, never all")
	}

	empty := NewSiteSet(nil)
	if empty.Allows(uuid.New()) {
		t.Fatal("an empty resolved SiteSet allowed a site; a tag matching no site " +
			"must grant nothing, not everything")
	}
	if empty.Allows(uuid.Nil) {
		t.Fatal("an empty resolved SiteSet allowed the nil UUID")
	}
}

func TestSiteSet_AllowsOnlyResolvedMembers(t *testing.T) {
	member, foreign := uuid.New(), uuid.New()
	set := NewSiteSet([]uuid.UUID{member})

	if !set.Allows(member) {
		t.Error("a resolved member was refused; the set must not over-fire")
	}
	if set.Allows(foreign) {
		t.Error("a site outside the resolved set was allowed")
	}
	if set.Len() != 1 {
		t.Errorf("Len() = %d, want 1", set.Len())
	}
}

// A grant whose stored payload names sites can still resolve to nothing once
// `sites` RLS has dropped every foreign or deleted UUID. That grant authorizes
// no read at all, and the caller must be told so explicitly rather than
// receiving an empty slice it might read as "unfiltered".
func TestSiteSet_ResolvesToNothingIsDetectable(t *testing.T) {
	set := NewSiteSet([]uuid.UUID{})
	if !set.IsEmpty() {
		t.Fatal("a set resolved from zero surviving ids does not report IsEmpty()")
	}
	if set.Len() != 0 {
		t.Fatalf("Len() = %d on an empty set", set.Len())
	}
}

// ---------------------------------------------------------------------------
// The requested site scope, validated BEFORE it reaches the database. This
// mirrors mcp_grants_site_scope_payload_check in Go so an incoherent request is
// refused with a 400 naming the problem rather than a bare 23514 from Postgres
// -- and, more importantly, so "requested a scope and named nothing" is caught
// on the request side too.
// ---------------------------------------------------------------------------

func TestValidateSiteScopeRequest_ModeWithEmptyPayloadIsRefused(t *testing.T) {
	cases := []struct {
		name string
		req  SiteScopeRequest
	}{
		{"mode omitted entirely", SiteScopeRequest{}},
		{"unrecognised mode", SiteScopeRequest{Mode: "everything"}},
		{"mode all is not spelled with an empty string", SiteScopeRequest{Mode: ""}},
		{"tags with no tags", SiteScopeRequest{Mode: SiteScopeModeTags}},
		{"list with no sites", SiteScopeRequest{Mode: SiteScopeModeList}},
		{"tags mode carrying only sites", SiteScopeRequest{
			Mode: SiteScopeModeTags, SiteIDs: []uuid.UUID{uuid.New()}}},
		{"list mode carrying only tags", SiteScopeRequest{
			Mode: SiteScopeModeList, TagIDs: []uuid.UUID{uuid.New()}}},
		{"all mode carrying a payload it must not have", SiteScopeRequest{
			Mode: SiteScopeModeAll, SiteIDs: []uuid.UUID{uuid.New()}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSiteScopeRequest(tc.req)
			if err == nil {
				t.Fatalf("ValidateSiteScopeRequest(%+v) accepted an incoherent or "+
					"empty scope request", tc.req)
			}
			var domErr *domain.Error
			if !errors.As(err, &domErr) {
				t.Fatalf("refusal was not a domain error: %T %v", err, err)
			}
		})
	}
}

func TestValidateSiteScopeRequest_CoherentRequestsAreAccepted(t *testing.T) {
	cases := []struct {
		name string
		req  SiteScopeRequest
	}{
		{"all", SiteScopeRequest{Mode: SiteScopeModeAll}},
		{"tags", SiteScopeRequest{Mode: SiteScopeModeTags, TagIDs: []uuid.UUID{uuid.New()}}},
		{"list", SiteScopeRequest{Mode: SiteScopeModeList, SiteIDs: []uuid.UUID{uuid.New()}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateSiteScopeRequest(tc.req); err != nil {
				t.Fatalf("ValidateSiteScopeRequest(%+v) refused a coherent request: %v",
					tc.req, err)
			}
		})
	}
}
