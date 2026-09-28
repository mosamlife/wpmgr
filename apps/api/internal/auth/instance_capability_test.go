package auth

// instance_capability_test.go: Me.can_manage_instance_email is always set, is
// the answer admingate.CanManageInstanceEmail gives for the same principal and
// store (the function the /api/v1/settings/smtp gate calls), and is false on
// every path where that decision cannot be made. GET /auth/me serving it
// through the real router is proven in
// tests/settings_smtp_instance_authority_integration_test.go.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/admingate"
	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

type fakeInstanceStore struct {
	superadmin    bool
	superadminErr error
	soleOwner     bool
	soleOwnerErr  error
}

func (f fakeInstanceStore) IsSuperadmin(context.Context, uuid.UUID) (bool, error) {
	return f.superadmin, f.superadminErr
}

func (f fakeInstanceStore) SoleLiveTenantOwnedBy(context.Context, uuid.UUID) (uuid.UUID, error) {
	if !f.soleOwner {
		return uuid.Nil, f.soleOwnerErr
	}
	return uuid.New(), f.soleOwnerErr
}

func TestSetInstanceCapabilities_EqualsTheRouteDecision(t *testing.T) {
	orgUser := func(tenant uuid.UUID) domain.Principal {
		return domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenant, Scope: domain.ScopeOrg}
	}
	siteUser := orgUser(uuid.New())
	siteUser.Scope = domain.ScopeSite
	siteUser.AllowedSiteIDs = []uuid.UUID{uuid.New()}

	cases := []struct {
		name  string
		p     domain.Principal
		store admingate.Store
		want  bool
	}{
		{"superadmin, no active organisation", orgUser(uuid.Nil), fakeInstanceStore{superadmin: true}, true},
		{"superadmin, active organisation", orgUser(uuid.New()), fakeInstanceStore{superadmin: true}, true},
		{"sole organisation owner", orgUser(uuid.New()), fakeInstanceStore{soleOwner: true}, true},
		{"organisation member without authority", orgUser(uuid.New()), fakeInstanceStore{}, false},
		{"site-scoped superadmin", siteUser, fakeInstanceStore{superadmin: true}, false},
		{"superadmin read error", orgUser(uuid.New()), fakeInstanceStore{superadminErr: errors.New("boom"), soleOwner: true}, false},
		{"organisation count read error", orgUser(uuid.New()), fakeInstanceStore{soleOwner: true, soleOwnerErr: errors.New("boom")}, false},
		{"store not wired", orgUser(uuid.New()), nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{}
			if tc.store != nil {
				h.SetInstanceAuthorityGate(tc.store)
			}
			ctx := domain.WithPrincipal(context.Background(), tc.p)
			var me gen.Me
			h.setInstanceCapabilities(ctx, &me)
			got, ok := me.CanManageInstanceEmail.Get()
			if !ok {
				t.Fatal("can_manage_instance_email was not set")
			}
			if got != tc.want {
				t.Errorf("can_manage_instance_email = %v, want %v", got, tc.want)
			}
			if route := admingate.CanManageInstanceEmail(ctx, tc.store); got != route {
				t.Errorf("can_manage_instance_email = %v but the route decision is %v", got, route)
			}
		})
	}
}
