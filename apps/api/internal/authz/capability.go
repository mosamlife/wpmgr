package authz

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// MaxCapabilities caps a single key's capability set. It mirrors the
// cardinality bound in the m120 CHECK (api_keys_capabilities_shape_check) so a
// set refused by the database is refused here first, with a domain error the
// caller can render, rather than as a 23514 surfacing from the repo layer.
const MaxCapabilities = 64

// permissionVocabulary is the set of every permission the control plane knows.
//
// It is DERIVED from minRoleFor rather than maintained as a second list, so it
// cannot drift from the role matrix: a permission that is not in minRoleFor is
// one that Allows() already returns false for unconditionally, and admitting it
// as a capability string would mint a key holding an authority no role can
// grant and no route checks. TestCapabilityVocabularyCoversEveryDeclaredPermission
// reads the declarations out of role.go and fails if a constant is declared
// without a matching minRoleFor entry, which is the drift this derivation
// cannot detect on its own.
//
// The database deliberately does NOT enumerate these strings. Migrations apply
// inside main() at boot, so a CHECK listing the vocabulary would make a new
// binary's writes fail 23514 against a database that has not caught up yet —
// exactly when a feature ships. Vocabulary validation is therefore Go's, and
// only Go's, which is why it has to actually happen here.
var permissionVocabulary = func() map[Permission]struct{} {
	m := make(map[Permission]struct{}, len(minRoleFor))
	for p := range minRoleFor {
		m[p] = struct{}{}
	}
	return m
}()

// AllPermissions returns every permission in the vocabulary, sorted, so tests
// and admin surfaces can enumerate the capability space deterministically.
func AllPermissions() []Permission {
	out := make([]Permission, 0, len(permissionVocabulary))
	for p := range permissionVocabulary {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// KnownPermission reports whether p is in the control plane's vocabulary.
func KnownPermission(p Permission) bool {
	_, ok := permissionVocabulary[p]
	return ok
}

// validCapabilityFormat reports whether s has the shape of a permission string:
// lowercase alphanumerics separated by '.', '_', ':' or '-', with no leading,
// trailing or doubled separator and no surrounding whitespace.
//
// The vocabulary check below subsumes this — nothing malformed can be in the
// vocabulary — but the two failures are reported separately on purpose. A
// malformed string is a caller bug (a trailing space, a copy-paste artefact, a
// capitalised constant name); an unknown well-formed string is a version skew
// between the caller and this binary. Collapsing them into one error makes the
// first indistinguishable from the second at exactly the moment someone is
// debugging why their key will not mint.
func validCapabilityFormat(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	if s != strings.TrimSpace(s) {
		return false
	}
	prevSep := true // treat position 0 as following a separator: forbids a leading one
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			prevSep = false
		case c == '.' || c == '_' || c == ':' || c == '-':
			if prevSep {
				return false // leading or doubled separator
			}
			prevSep = true
		default:
			return false // uppercase, whitespace, punctuation, non-ASCII
		}
	}
	return !prevSep // forbids a trailing separator
}

// ValidateCapabilities checks a capability set for shape and vocabulary and
// returns a domain validation error describing the first problem it finds.
//
// A nil slice is rejected: the caller must pass a non-nil (possibly empty) set
// when minting a capability key, because nil is how a ROLE key is represented
// and silently accepting it here would mint a role key under a capability
// label. That is the same NULL/'{}' collapse the auth_model column exists to
// prevent, arriving through the front door instead.
//
// An unknown capability is REFUSED, never dropped. Silently skipping it would
// mint a key its owner believes holds a permission it does not, and the gap
// only surfaces as a 403 at some unpredictable later moment.
func ValidateCapabilities(caps []string) error {
	if caps == nil {
		return domain.Validation("apikey_capabilities_required",
			"a capability key requires an explicit capability set (use an empty set for zero authority)")
	}
	if len(caps) > MaxCapabilities {
		return domain.Validation("apikey_capabilities_too_many",
			fmt.Sprintf("a capability set may hold at most %d capabilities, got %d", MaxCapabilities, len(caps)))
	}
	seen := make(map[string]struct{}, len(caps))
	for _, c := range caps {
		if !validCapabilityFormat(c) {
			return domain.Validation("apikey_capability_malformed",
				fmt.Sprintf("malformed capability %q", c))
		}
		if _, dup := seen[c]; dup {
			return domain.Validation("apikey_capability_duplicate",
				fmt.Sprintf("capability %q listed more than once", c))
		}
		seen[c] = struct{}{}
		if !KnownPermission(Permission(c)) {
			return domain.Validation("apikey_capability_unknown",
				fmt.Sprintf("unknown capability %q", c))
		}
	}
	return nil
}

// PrincipalAllows reports whether principal p may perform perm.
//
// For an AuthModelCapability principal the capability set is the ONLY input:
// the role is not consulted, not as a fallback, not when the set is empty, and
// not when the set is nil. That is the entire point of #510. api_keys.role is a
// rank in a totally ordered hierarchy, so granting a machine principal one
// admin-tier permission grants it every admin-and-below permission too; a
// capability key exists precisely to escape that coupling, and any fallback
// path would silently restore it.
//
// For every other principal — sessions, legacy keys, anything whose AuthModel
// is the empty zero value — this is exactly Allows(Role, perm), unchanged.
func PrincipalAllows(p domain.Principal, perm Permission) bool {
	if p.IsCapabilityScoped() {
		// An unknown permission is denied before the set is even scanned, so a
		// stale capability string that happens to match a retired permission
		// name cannot authorise anything.
		if !KnownPermission(perm) {
			return false
		}
		for _, c := range p.Capabilities {
			if Permission(c) == perm {
				return true
			}
		}
		return false
	}
	return Allows(Role(p.Role), perm)
}
