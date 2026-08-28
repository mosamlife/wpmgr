package mcp

import (
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ErrCodeInvalidScope is the domain error code the OAuth error response renders
// as RFC 6749 section 5.2 `invalid_scope`. httpx maps KindValidation to 400,
// which is the correct status for both the authorize and the token endpoint.
const ErrCodeInvalidScope = "mcp_invalid_scope"

// ErrCodeInvalidSiteScope is the domain error code for a site-scope request
// that names a mode and no payload, or a payload its mode forbids.
const ErrCodeInvalidSiteScope = "mcp_invalid_site_scope"

// recognisedScopes is the CLOSED registry of scopes this surface grants.
//
// It is closed in the strong sense: an unrecognised scope is a REFUSAL of the
// whole request, not a token quietly dropped from an otherwise-honoured one.
// Dropping is the tempting behaviour because it "still works" for well-behaved
// clients, and it is wrong for the same reason a nullable tenant_id is wrong --
// it lets the operator consent to a scope set that is not the one the client
// asked for, and neither party ever learns they disagreed.
//
// The registry holds exactly one entry and the read-only surface is the whole
// security claim of the feature (m124 obligation 5). A write scope arrives with
// its own migration and its own review, never by being appended here.
var recognisedScopes = map[Scope]struct{}{
	ScopeRead: {},
}

// ParseRequestedScopes parses the RFC 6749 section 3.3 space-delimited `scope`
// request parameter into the scopes this surface will grant.
//
// THIS IS S6's EXIT GATE (m124 obligation 3). A client that requests no
// recognised scope is REFUSED. There is no default, no fallback and no
// widening, and in particular:
//
//   - a missing, empty or whitespace-only parameter is an ABSENCE, and absence
//     is refusal, never "everything we have";
//   - an unrecognised scope refuses the WHOLE request rather than being
//     silently dropped from it;
//   - matching is exact and case-sensitive, per RFC 6749 section 3.3, so
//     "MCP:READ" is unrecognised and is refused rather than normalised into a
//     grant the client did not spell.
//
// The refusal path returns a nil slice as well as an error. A caller that
// ignores the error still gets no authority, which is the fail-closed
// direction; the schema half of this gate (m124 DECISION 1) makes the same
// value unstorable.
func ParseRequestedScopes(raw string) ([]Scope, error) {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return nil, domain.Validation(ErrCodeInvalidScope,
			"the scope parameter is required and must name at least one recognised scope").
			WithDetails(map[string]any{"supported_scopes": SupportedScopes()})
	}

	seen := make(map[Scope]struct{}, len(fields))
	out := make([]Scope, 0, len(fields))
	for _, field := range fields {
		s := Scope(field)
		if _, ok := recognisedScopes[s]; !ok {
			// Refuse the whole request. Naming the offending scope back to the
			// client is safe (it is the client's own input) and is what makes
			// the failure debuggable instead of mysterious.
			return nil, domain.Validation(ErrCodeInvalidScope,
				fmt.Sprintf("scope %q is not recognised by this server", field)).
				WithDetails(map[string]any{
					"unrecognised_scope": field,
					"supported_scopes":   SupportedScopes(),
				})
		}
		if _, dup := seen[s]; dup {
			continue // RFC 6749 permits repetition; it adds no authority
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}

	// Belt and braces. Reaching here with an empty slice would mean the loop
	// above accepted every field and appended none, which cannot happen -- but
	// "cannot happen" returning a silently empty grant is precisely this
	// project's signature defect, so it is checked rather than assumed.
	if len(out) == 0 {
		return nil, domain.Validation(ErrCodeInvalidScope,
			"no recognised scope was requested")
	}
	return out, nil
}

// SupportedScopes lists the registry for discovery documents and error
// details, sorted so the output is stable across map iterations.
func SupportedScopes() []string {
	out := make([]string, 0, len(recognisedScopes))
	for s := range recognisedScopes {
		out = append(out, string(s))
	}
	sort.Strings(out)
	return out
}

// ValidateSiteScopeRequest refuses an incoherent or empty site-scope request
// BEFORE it reaches the database.
//
// This mirrors mcp_grants_site_scope_payload_check in Go. The redundancy is
// deliberate and is not defensive clutter: the CHECK guarantees such a row
// cannot be STORED, which turns the bug into a bare SQLSTATE 23514 at INSERT
// time, and a 23514 surfacing as a 500 tells the operator nothing about which
// field was wrong. More importantly the CHECK cannot see a request that never
// reaches an INSERT -- the authorize endpoint validates the requested scope
// long before any row exists.
//
// "Requested a scope and named nothing" is refused in both places. An empty
// mode is NOT read as SiteScopeModeAll: there is deliberately no DEFAULT 'all'
// in the schema and there is deliberately no zero-value default here.
func ValidateSiteScopeRequest(req SiteScopeRequest) error {
	switch req.Mode {
	case SiteScopeModeAll:
		if len(req.TagIDs) > 0 || len(req.SiteIDs) > 0 {
			return domain.Validation(ErrCodeInvalidSiteScope,
				"site scope mode 'all' must not name tags or sites")
		}
		return nil

	case SiteScopeModeTags:
		if len(req.TagIDs) == 0 {
			return domain.Validation(ErrCodeInvalidSiteScope,
				"site scope mode 'tags' must name at least one tag; "+
					"an empty tag list grants nothing and is not a way to request every site")
		}
		if len(req.SiteIDs) > 0 {
			return domain.Validation(ErrCodeInvalidSiteScope,
				"site scope mode 'tags' must not also name sites")
		}
		return validateNoNilIDs(req.TagIDs, "tag")

	case SiteScopeModeList:
		if len(req.SiteIDs) == 0 {
			return domain.Validation(ErrCodeInvalidSiteScope,
				"site scope mode 'list' must name at least one site; "+
					"an empty allowlist grants nothing and is not a way to request every site")
		}
		if len(req.TagIDs) > 0 {
			return domain.Validation(ErrCodeInvalidSiteScope,
				"site scope mode 'list' must not also name tags")
		}
		return validateNoNilIDs(req.SiteIDs, "site")

	default:
		// Covers the empty mode and every unrecognised string. An absent mode
		// is a caller that did not say what the grant covers, and the answer
		// to that is 400, not 'all'.
		return domain.Validation(ErrCodeInvalidSiteScope,
			fmt.Sprintf("site scope mode %q is not recognised; expected one of 'all', 'tags', 'list'",
				req.Mode)).
			WithDetails(map[string]any{"supported_modes": []string{
				string(SiteScopeModeAll), string(SiteScopeModeTags), string(SiteScopeModeList),
			}})
	}
}

// validateNoNilIDs refuses uuid.Nil in a scope payload. uuid.Nil is what a
// failed parse decays to in Go, so accepting it would let a malformed request
// through as a well-formed id that simply matches no row -- an absence wearing
// the shape of a value.
func validateNoNilIDs(ids []uuid.UUID, kind string) error {
	for _, id := range ids {
		if id == uuid.Nil {
			return domain.Validation(ErrCodeInvalidSiteScope,
				fmt.Sprintf("site scope names an empty %s id", kind))
		}
	}
	return nil
}
