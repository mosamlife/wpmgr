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
}

func (f *fakeStore) IsSuperadmin(context.Context, uuid.UUID) (bool, error) {
	f.calls++
	return f.superadmin, f.superadminErr
}

func (f *fakeStore) IsSoleLiveTenantOwner(context.Context, uuid.UUID) (bool, error) {
	f.calls++
	return f.soleOwner, f.soleOwnerErr
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
			var store Store
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
