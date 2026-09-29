package admingate

// gate_test.go: hermetic tests for CanManageInstanceEmail, the one decision
// behind both the /api/v1/settings/smtp route gate and the Me capability
// can_manage_instance_email. Real rows are covered by
// tests/settings_smtp_instance_authority_integration_test.go.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

type fakeStore struct {
	superadmin    bool
	superadminErr error
	soleOwner     bool
	soleOwnerErr  error
	calls         int

	// The install-owner facts. selfHosted false is the hosted answer, the
	// same default the production store's zero value has.
	selfHosted        bool
	installOwner      bool
	installHome       uuid.UUID
	installOwnerErr   error
	installOwnerCalls int
}

func (f *fakeStore) SelfHosted() bool { return f.selfHosted }

func (f *fakeStore) InstallOwnerAuditTenant(context.Context, uuid.UUID) (bool, uuid.UUID, error) {
	f.calls++
	f.installOwnerCalls++
	if f.installOwnerErr != nil {
		return false, uuid.Nil, f.installOwnerErr
	}
	if !f.installOwner {
		return false, uuid.Nil, nil
	}
	return true, f.installHome, nil
}

var _ InstanceEmailStore = (*fakeStore)(nil)

func (f *fakeStore) IsSuperadmin(context.Context, uuid.UUID) (bool, error) {
	f.calls++
	return f.superadmin, f.superadminErr
}

// soleTenantID is the organisation the fake store names when soleOwner is set.
var soleTenantID = uuid.MustParse("5a1e0000-0000-4000-8000-000000000001")

func (f *fakeStore) SoleLiveTenantOwnedBy(context.Context, uuid.UUID) (uuid.UUID, error) {
	f.calls++
	if !f.soleOwner {
		return uuid.Nil, f.soleOwnerErr
	}
	return soleTenantID, f.soleOwnerErr
}

func withPrincipal(p domain.Principal) context.Context {
	return domain.WithPrincipal(context.Background(), p)
}

func userPrincipal(tenant uuid.UUID) domain.Principal {
	return domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenant, Scope: domain.ScopeOrg}
}

func TestCanManageInstanceEmail(t *testing.T) {
	siteScoped := userPrincipal(uuid.New())
	siteScoped.Scope = domain.ScopeSite
	siteScoped.AllowedSiteIDs = []uuid.UUID{uuid.New()}

	allowlistOnly := userPrincipal(uuid.New())
	allowlistOnly.Scope = ""
	allowlistOnly.AllowedSiteIDs = []uuid.UUID{uuid.New()}

	apiKey := userPrincipal(uuid.New())
	apiKey.Type = domain.PrincipalAPIKey
	apiKey.APIKeyID = uuid.New()

	cases := []struct {
		name      string
		ctx       context.Context
		store     *fakeStore
		nilStore  bool
		want      bool
		wantReads bool
	}{
		{"superadmin with an active organisation", withPrincipal(userPrincipal(uuid.New())), &fakeStore{superadmin: true}, false, true, true},
		{"superadmin with no active organisation", withPrincipal(userPrincipal(uuid.Nil)), &fakeStore{superadmin: true}, false, true, true},
		{"sole live organisation owner", withPrincipal(userPrincipal(uuid.New())), &fakeStore{soleOwner: true}, false, true, true},
		{"neither fact", withPrincipal(userPrincipal(uuid.New())), &fakeStore{}, false, false, true},
		{"no active organisation and neither fact", withPrincipal(userPrincipal(uuid.Nil)), &fakeStore{}, false, false, true},
		{"superadmin read error", withPrincipal(userPrincipal(uuid.New())), &fakeStore{superadminErr: errors.New("boom"), soleOwner: true}, false, false, true},
		{"organisation count read error", withPrincipal(userPrincipal(uuid.New())), &fakeStore{soleOwner: true, soleOwnerErr: errors.New("boom")}, false, false, true},
		{"nil store", withPrincipal(userPrincipal(uuid.New())), nil, true, false, false},
		{"site-scoped superadmin", withPrincipal(siteScoped), &fakeStore{superadmin: true}, false, false, false},
		{"allowlist without a scope label", withPrincipal(allowlistOnly), &fakeStore{superadmin: true}, false, false, false},
		{"API key", withPrincipal(apiKey), &fakeStore{superadmin: true, soleOwner: true}, false, false, false},
		{"no principal", context.Background(), &fakeStore{superadmin: true}, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var store InstanceEmailStore
			if !tc.nilStore {
				store = tc.store
			}
			if got := CanManageInstanceEmail(tc.ctx, store); got != tc.want {
				t.Errorf("CanManageInstanceEmail = %v, want %v", got, tc.want)
			}
			if tc.store != nil && !tc.wantReads && tc.store.calls != 0 {
				t.Errorf("store read %d times; want 0 for a principal refused before the store", tc.store.calls)
			}
		})
	}
}

// The decision reports the arm that admitted. For the owner arm the
// organisation is the one the store's statement named, whatever organisation
// the request has active.
func TestInstanceEmailAuthority_ReportsTheAdmittingArm(t *testing.T) {
	cases := []struct {
		name       string
		tenant     uuid.UUID
		store      *fakeStore
		wantArm    Arm
		wantTenant uuid.UUID
	}{
		{"superadmin", uuid.New(), &fakeStore{superadmin: true}, ArmSuperadmin, uuid.Nil},
		{"superadmin who also owns the sole organisation", uuid.New(), &fakeStore{superadmin: true, soleOwner: true}, ArmSuperadmin, uuid.Nil},
		{"sole owner, another organisation active", uuid.New(), &fakeStore{soleOwner: true}, ArmSoleLiveTenantOwner, soleTenantID},
		{"sole owner, no active organisation", uuid.Nil, &fakeStore{soleOwner: true}, ArmSoleLiveTenantOwner, soleTenantID},
		{"neither fact, no active organisation", uuid.Nil, &fakeStore{}, ArmNone, uuid.Nil},
		{"organisation count read error", uuid.Nil, &fakeStore{soleOwner: true, soleOwnerErr: errors.New("boom")}, ArmNone, uuid.Nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := InstanceEmailAuthority(withPrincipal(userPrincipal(tc.tenant)), tc.store)
			if got.Arm != tc.wantArm || got.TenantID != tc.wantTenant {
				t.Errorf("InstanceEmailAuthority = %+v, want arm %d tenant %s", got, tc.wantArm, tc.wantTenant)
			}
			if got.Admitted() != (tc.wantArm != ArmNone) {
				t.Errorf("Admitted() = %v for arm %d", got.Admitted(), got.Arm)
			}
		})
	}
}
