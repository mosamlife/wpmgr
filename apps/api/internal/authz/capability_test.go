package authz

import (
	"os"
	"regexp"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

var permDeclRe = regexp.MustCompile(`Permission = "([^"]+)"`)

// TestCapabilityVocabularyCoversEveryDeclaredPermission is the drift guard for
// the derivation in capability.go. permissionVocabulary is built from
// minRoleFor, which means a permission constant declared in role.go but never
// added to the role matrix would be invisible to it — and a capability naming
// that permission would be refused at mint time with "unknown capability",
// which is a confusing way to discover an incomplete matrix.
//
// This reads the declarations out of the source file rather than trusting a
// hard-coded count, and REFUSES to pass when it matches nothing: a renamed
// declaration style or a moved file must redden here, not silently assert
// nothing over an empty set.
func TestCapabilityVocabularyCoversEveryDeclaredPermission(t *testing.T) {
	src, err := os.ReadFile("role.go")
	if err != nil {
		t.Fatalf("read role.go: %v", err)
	}
	matches := permDeclRe.FindAllStringSubmatch(string(src), -1)
	if len(matches) == 0 {
		t.Fatal("matched zero permission declarations in role.go — the pattern or the path is wrong; " +
			"an empty match set must not read as 'nothing to check'")
	}

	declared := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		declared[m[1]] = struct{}{}
	}

	for p := range declared {
		if !KnownPermission(Permission(p)) {
			t.Errorf("permission %q is declared in role.go but missing from minRoleFor, "+
				"so it is absent from the capability vocabulary and cannot be granted to a key", p)
		}
	}
	if len(declared) != len(permissionVocabulary) {
		t.Errorf("declared permissions = %d, vocabulary = %d — the two must match exactly",
			len(declared), len(permissionVocabulary))
	}
	// Every element of the format check must accept every real permission; if a
	// future permission uses a character the format rejects, minting a key with
	// it would fail even though it is legitimate.
	for p := range declared {
		if !validCapabilityFormat(p) {
			t.Errorf("declared permission %q is rejected by validCapabilityFormat", p)
		}
	}
}

func TestValidateCapabilities(t *testing.T) {
	tests := []struct {
		name     string
		caps     []string
		wantCode string // "" means accept
	}{
		{"empty set is valid — zero authority", []string{}, ""},
		{"single known", []string{"site.files.read"}, ""},
		{"several known", []string{"site:read", "site.files.read", "member:read"}, ""},
		{"nil is refused", nil, "apikey_capabilities_required"},
		{"unknown string", []string{"site:superuser"}, "apikey_capability_unknown"},
		{"plausible but undeclared", []string{"site.files.read_all"}, "apikey_capability_unknown"},
		{"uppercase", []string{"Site.Files.Read"}, "apikey_capability_malformed"},
		{"trailing space", []string{"site.files.read "}, "apikey_capability_malformed"},
		{"leading space", []string{" site.files.read"}, "apikey_capability_malformed"},
		{"empty element", []string{""}, "apikey_capability_malformed"},
		{"leading separator", []string{".site.files.read"}, "apikey_capability_malformed"},
		{"trailing separator", []string{"site.files.read."}, "apikey_capability_malformed"},
		{"doubled separator", []string{"site..files.read"}, "apikey_capability_malformed"},
		{"duplicate", []string{"site:read", "site:read"}, "apikey_capability_duplicate"},
		{"one bad among good", []string{"site:read", "nope:nope"}, "apikey_capability_unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCapabilities(tt.caps)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("ValidateCapabilities(%v) = %v, want accept", tt.caps, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateCapabilities(%v) accepted, want refusal %q", tt.caps, tt.wantCode)
			}
			de, ok := domain.AsDomain(err)
			if !ok {
				t.Fatalf("error is not a domain error: %v", err)
			}
			if de.Code != tt.wantCode {
				t.Fatalf("code = %q, want %q", de.Code, tt.wantCode)
			}
		})
	}
}

func TestValidateCapabilitiesRejectsOversizedSet(t *testing.T) {
	caps := make([]string, 0, MaxCapabilities+1)
	// Use distinct malformed-free strings; the size check must fire before the
	// vocabulary check so the error names the real problem.
	for i := 0; i <= MaxCapabilities; i++ {
		caps = append(caps, "site:read")
	}
	err := ValidateCapabilities(caps)
	if err == nil {
		t.Fatal("oversized capability set accepted")
	}
	de, _ := domain.AsDomain(err)
	if de == nil || de.Code != "apikey_capabilities_too_many" {
		t.Fatalf("got %v, want apikey_capabilities_too_many", err)
	}
}

// TestPrincipalAllowsRoleModelUnchanged is the over-fire control at the unit
// level: for every role and every permission in the vocabulary, PrincipalAllows
// over a role principal must equal Allows exactly. If this reddens, existing
// authority moved.
func TestPrincipalAllowsRoleModelUnchanged(t *testing.T) {
	roles := []Role{RoleClient, RoleViewer, RoleOperator, RoleAdmin, RoleOwner}
	for _, r := range roles {
		for _, perm := range AllPermissions() {
			// Explicit role model.
			p := domain.Principal{Role: string(r), AuthModel: domain.AuthModelRole}
			if got, want := PrincipalAllows(p, perm), Allows(r, perm); got != want {
				t.Errorf("role=%s perm=%s: explicit-model got %v want %v", r, perm, got, want)
			}
			// Zero-value AuthModel — every principal built before m120.
			z := domain.Principal{Role: string(r)}
			if got, want := PrincipalAllows(z, perm), Allows(r, perm); got != want {
				t.Errorf("role=%s perm=%s: zero-value-model got %v want %v", r, perm, got, want)
			}
		}
	}
}

// TestCapabilityPrincipalNeverConsultsRole is the assertion #510 exists for.
func TestCapabilityPrincipalNeverConsultsRole(t *testing.T) {
	// An owner-role principal — the maximum role rank — carrying exactly one
	// capability. If role were consulted at all, this principal would hold
	// every permission in the vocabulary.
	p := domain.Principal{
		Type:         domain.PrincipalAPIKey,
		APIKeyID:     uuid.New(),
		Role:         string(RoleOwner),
		AuthModel:    domain.AuthModelCapability,
		Capabilities: []string{string(PermSiteFilesRead)},
	}

	if !PrincipalAllows(p, PermSiteFilesRead) {
		t.Fatal("capability principal denied its own granted capability")
	}
	if PrincipalAllows(p, PermMemberManage) {
		t.Fatal("capability principal with only site.files.read was allowed member:manage — role fallback")
	}
	for _, perm := range AllPermissions() {
		want := perm == PermSiteFilesRead
		if got := PrincipalAllows(p, perm); got != want {
			t.Errorf("perm=%s: got %v want %v (role=owner must be ignored)", perm, got, want)
		}
	}
}

// TestEmptyCapabilitySetIsZeroAuthority proves the NULL/'{}' collapse is not a
// fail-open in Go either: an owner-role principal on the capability model with
// an empty set holds nothing, while the same principal on the role model holds
// everything. Both have len(Capabilities) == 0.
func TestEmptyCapabilitySetIsZeroAuthority(t *testing.T) {
	empty := domain.Principal{
		Role:         string(RoleOwner),
		AuthModel:    domain.AuthModelCapability,
		Capabilities: []string{},
	}
	nilCaps := domain.Principal{
		Role:      string(RoleOwner),
		AuthModel: domain.AuthModelCapability,
		// Capabilities nil — indistinguishable from the above by length.
	}
	roleKey := domain.Principal{Role: string(RoleOwner), AuthModel: domain.AuthModelRole}

	for _, perm := range AllPermissions() {
		if PrincipalAllows(empty, perm) {
			t.Errorf("empty capability set allowed %s", perm)
		}
		if PrincipalAllows(nilCaps, perm) {
			t.Errorf("nil capability set on capability model allowed %s", perm)
		}
	}
	if !PrincipalAllows(roleKey, PermMemberManage) {
		t.Fatal("owner role principal lost member:manage — discriminator read backwards")
	}
}

// TestPrincipalAllowsDeniesUnknownPermission guards the check-time half: a
// capability string is only ever honoured when it names a permission this
// binary actually knows.
func TestPrincipalAllowsDeniesUnknownPermission(t *testing.T) {
	p := domain.Principal{
		AuthModel:    domain.AuthModelCapability,
		Capabilities: []string{"site:retired_permission"},
	}
	if PrincipalAllows(p, Permission("site:retired_permission")) {
		t.Fatal("a capability naming a permission outside the vocabulary authorised an action")
	}
}
