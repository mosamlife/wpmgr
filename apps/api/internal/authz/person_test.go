package authz

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Loosening an AI control needs a signed-in person. The admitted cases are
// here so the check cannot pass by refusing everyone.
func TestLooseningAnAIControlNeedsASignedInPerson(t *testing.T) {
	tenant, user := uuid.New(), uuid.New()
	owner := string(RoleOwner)
	cases := []struct {
		p    domain.Principal
		want bool
	}{
		{domain.Principal{Type: domain.PrincipalAPIKey, APIKeyID: uuid.New(), TenantID: tenant, Role: owner}, false},
		{domain.Principal{Type: domain.PrincipalAPIKey, APIKeyID: uuid.New(), UserID: user, TenantID: tenant, Role: owner}, false},
		{domain.Principal{Type: domain.PrincipalUser, TenantID: tenant, Role: owner}, false},
		{domain.Principal{Type: domain.PrincipalUser, UserID: user, Role: owner}, false},
		{domain.Principal{UserID: user, TenantID: tenant, Role: owner}, false},
		{domain.Principal{Type: "other", UserID: user, TenantID: tenant, Role: owner}, false},
		{domain.Principal{}, false},
		{domain.Principal{Type: domain.PrincipalUser, UserID: user, TenantID: tenant, Role: owner}, true},
		{domain.Principal{Type: domain.PrincipalUser, UserID: user, TenantID: tenant, Role: owner, Scope: domain.ScopeOrg}, true},
	}
	for i, tc := range cases {
		t.Run(fmt.Sprintf("case_%d", i+1), func(t *testing.T) {
			err := AuthorizeLoosening(tc.p)
			if tc.want {
				if err != nil {
					t.Fatalf("a signed-in person was refused: %v", err)
				}
				return
			}
			de, ok := domain.AsDomain(err)
			if !ok || de.Kind != domain.KindForbidden || de.Code != "session_required" {
				t.Fatalf("a credential that is not a person loosened a control: %v", err)
			}
		})
	}
}
