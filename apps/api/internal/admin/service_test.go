package admin

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// fakeStore is an in-memory userStore so the real Service guard + orphan-cleanup
// logic can be exercised without a DB or pgx pool.
type fakeStore struct {
	users map[uuid.UUID]AdminUser
	// sole maps a userID to the tenants it is the sole member of.
	sole map[uuid.UUID][]OrphanTenant
	// deletedTenants records tenant IDs passed to DeleteEmptyTenant that were
	// actually removed (site_count == 0).
	deletedTenants []uuid.UUID
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		users: map[uuid.UUID]AdminUser{},
		sole:  map[uuid.UUID][]OrphanTenant{},
	}
}

func (f *fakeStore) ListUsers(_ context.Context, _ string, _, _ int32) ([]AdminUser, error) {
	out := make([]AdminUser, 0, len(f.users))
	for _, u := range f.users {
		out = append(out, u)
	}
	return out, nil
}

func (f *fakeStore) GetUser(_ context.Context, id uuid.UUID) (AdminUser, error) {
	u, ok := f.users[id]
	if !ok {
		return AdminUser{}, domain.NotFound("user_not_found", "user not found")
	}
	return u, nil
}

func (f *fakeStore) DeleteUser(_ context.Context, id uuid.UUID) error {
	if _, ok := f.users[id]; !ok {
		return domain.NotFound("user_not_found", "user not found")
	}
	delete(f.users, id)
	return nil
}

func (f *fakeStore) SetStatus(_ context.Context, id uuid.UUID, status string) (AdminUser, error) {
	u, ok := f.users[id]
	if !ok {
		return AdminUser{}, domain.NotFound("user_not_found", "user not found")
	}
	u.Status = status
	f.users[id] = u
	return u, nil
}

func (f *fakeStore) Stats(_ context.Context) (AdminStats, error) {
	return AdminStats{Users: int64(len(f.users))}, nil
}

func (f *fakeStore) SoleTenants(_ context.Context, userID uuid.UUID) ([]OrphanTenant, error) {
	return f.sole[userID], nil
}

func (f *fakeStore) DeleteEmptyTenant(_ context.Context, tenantID uuid.UUID) (bool, error) {
	// Mirror the SQL guard: only orgs with site_count == 0 are removable. Look up
	// the orphan record across all captured sole-tenant lists.
	for _, list := range f.sole {
		for _, o := range list {
			if o.ID == tenantID {
				if o.SiteCount > 0 {
					return false, nil
				}
				f.deletedTenants = append(f.deletedTenants, tenantID)
				return true, nil
			}
		}
	}
	return false, nil
}

func (f *fakeStore) SiteTenancy(_ context.Context, _, siteID uuid.UUID) (SiteTenancyReport, error) {
	return SiteTenancyReport{SiteID: siteID}, nil
}

func (f *fakeStore) GrantSelfOwnerMembership(_ context.Context, _, _ uuid.UUID) (uuid.UUID, string, bool, error) {
	return uuid.New(), "Test Org", true, nil
}

func (f *fakeStore) ListSitesByUser(_ context.Context, _ uuid.UUID) ([]AdminUserSite, error) {
	return []AdminUserSite{}, nil
}

func (f *fakeStore) ListSystemAuditEvents(_ context.Context, _, _ int32) ([]SystemAuditEvent, int64, error) {
	return []SystemAuditEvent{}, 0, nil
}

func (f *fakeStore) AccountsTenancy(_ context.Context, emailSubstr string) (AccountsTenancyReport, error) {
	// Return users that contain emailSubstr in their email, with no memberships.
	var users []AccountUser
	for _, u := range f.users {
		if emailSubstr == "" || contains(u.Email, emailSubstr) {
			users = append(users, AccountUser{
				ID:           u.ID,
				Email:        u.Email,
				IsSuperadmin: u.IsSuperadmin,
				Memberships:  []AccountUserMembership{},
			})
		}
	}
	if users == nil {
		users = []AccountUser{}
	}
	return AccountsTenancyReport{
		Users: users,
		Orgs:  []AccountOrg{},
	}, nil
}

// contains is a simple case-insensitive substring check for the fake store.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (substr == "" ||
		func() bool {
			for i := 0; i <= len(s)-len(substr); i++ {
				match := true
				for j := 0; j < len(substr); j++ {
					a, b := s[i+j], substr[j]
					if a >= 'A' && a <= 'Z' {
						a += 32
					}
					if b >= 'A' && b <= 'Z' {
						b += 32
					}
					if a != b {
						match = false
						break
					}
				}
				if match {
					return true
				}
			}
			return false
		}())
}

func newService(f *fakeStore) *Service {
	return &Service{repo: f}
}

func TestDeleteUser_CannotDeleteSelf(t *testing.T) {
	id := uuid.New()
	f := newFakeStore()
	f.users[id] = AdminUser{ID: id, Email: "a@example.com", Status: "active"}
	_, err := newService(f).DeleteUser(context.Background(), id, id)
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindForbidden || de.Code != "cannot_delete_self" {
		t.Fatalf("expected KindForbidden/cannot_delete_self, got %v", err)
	}
}

func TestDeleteUser_CannotDeleteSuperadmin(t *testing.T) {
	actor, target := uuid.New(), uuid.New()
	f := newFakeStore()
	f.users[actor] = AdminUser{ID: actor, Status: "active", IsSuperadmin: true}
	f.users[target] = AdminUser{ID: target, Status: "active", IsSuperadmin: true}
	_, err := newService(f).DeleteUser(context.Background(), actor, target)
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindForbidden || de.Code != "cannot_delete_superadmin" {
		t.Fatalf("expected KindForbidden/cannot_delete_superadmin, got %v", err)
	}
}

func TestDeleteUser_Success_NoOrphans(t *testing.T) {
	actor, target := uuid.New(), uuid.New()
	f := newFakeStore()
	f.users[actor] = AdminUser{ID: actor, Status: "active", IsSuperadmin: true}
	f.users[target] = AdminUser{ID: target, Status: "active"}
	res, err := newService(f).DeleteUser(context.Background(), actor, target)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := f.users[target]; ok {
		t.Fatal("user should have been deleted")
	}
	if res.DeletedOrgs != 0 || len(res.KeptOrgs) != 0 {
		t.Fatalf("expected no org cleanup, got %+v", res)
	}
}

// A user that solely owns an EMPTY org: the org should be auto-deleted.
func TestDeleteUser_RemovesEmptyOrphanedOrg(t *testing.T) {
	actor, target := uuid.New(), uuid.New()
	org := uuid.New()
	f := newFakeStore()
	f.users[actor] = AdminUser{ID: actor, Status: "active", IsSuperadmin: true}
	f.users[target] = AdminUser{ID: target, Status: "active"}
	f.sole[target] = []OrphanTenant{{ID: org, Name: "Solo Org", SiteCount: 0}}

	res, err := newService(f).DeleteUser(context.Background(), actor, target)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.DeletedOrgs != 1 {
		t.Fatalf("expected 1 deleted org, got %d", res.DeletedOrgs)
	}
	if len(res.KeptOrgs) != 0 {
		t.Fatalf("expected no kept orgs, got %+v", res.KeptOrgs)
	}
	if len(f.deletedTenants) != 1 || f.deletedTenants[0] != org {
		t.Fatalf("expected org %s to be deleted, got %v", org, f.deletedTenants)
	}
}

// A user that solely owns an org WITH sites: keep the org, do not delete it, and
// report it so the operator can act.
func TestDeleteUser_KeepsOrphanedOrgWithSites(t *testing.T) {
	actor, target := uuid.New(), uuid.New()
	org := uuid.New()
	f := newFakeStore()
	f.users[actor] = AdminUser{ID: actor, Status: "active", IsSuperadmin: true}
	f.users[target] = AdminUser{ID: target, Status: "active"}
	f.sole[target] = []OrphanTenant{{ID: org, Name: "Has Sites", SiteCount: 3}}

	res, err := newService(f).DeleteUser(context.Background(), actor, target)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.DeletedOrgs != 0 {
		t.Fatalf("expected 0 deleted orgs, got %d", res.DeletedOrgs)
	}
	if len(res.KeptOrgs) != 1 || res.KeptOrgs[0].ID != org || res.KeptOrgs[0].SiteCount != 3 {
		t.Fatalf("expected org %s kept with 3 sites, got %+v", org, res.KeptOrgs)
	}
	if len(f.deletedTenants) != 0 {
		t.Fatalf("org with sites must not be deleted, got %v", f.deletedTenants)
	}
	// The user itself is always removed regardless of org outcome.
	if _, ok := f.users[target]; ok {
		t.Fatal("user should have been deleted even when an org was kept")
	}
}

// Mixed: one empty org (deleted) + one org with sites (kept) for the same user.
func TestDeleteUser_MixedOrphans(t *testing.T) {
	actor, target := uuid.New(), uuid.New()
	empty, withSites := uuid.New(), uuid.New()
	f := newFakeStore()
	f.users[actor] = AdminUser{ID: actor, Status: "active", IsSuperadmin: true}
	f.users[target] = AdminUser{ID: target, Status: "active"}
	f.sole[target] = []OrphanTenant{
		{ID: empty, Name: "Empty", SiteCount: 0},
		{ID: withSites, Name: "Full", SiteCount: 2},
	}

	res, err := newService(f).DeleteUser(context.Background(), actor, target)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.DeletedOrgs != 1 {
		t.Fatalf("expected 1 deleted org, got %d", res.DeletedOrgs)
	}
	if len(res.KeptOrgs) != 1 || res.KeptOrgs[0].ID != withSites {
		t.Fatalf("expected org %s kept, got %+v", withSites, res.KeptOrgs)
	}
}

func TestSetStatus_InvalidStatus(t *testing.T) {
	actor, target := uuid.New(), uuid.New()
	f := newFakeStore()
	f.users[target] = AdminUser{ID: target, Status: "active"}
	_, err := newService(f).SetStatus(context.Background(), actor, target, "banned")
	if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindValidation {
		t.Fatalf("expected KindValidation, got %v", err)
	}
}

func TestSetStatus_CannotModifySelf(t *testing.T) {
	id := uuid.New()
	f := newFakeStore()
	f.users[id] = AdminUser{ID: id, Status: "active"}
	_, err := newService(f).SetStatus(context.Background(), id, id, "disabled")
	if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindForbidden {
		t.Fatalf("expected KindForbidden, got %v", err)
	}
}

func TestSetStatus_CannotModifySuperadmin(t *testing.T) {
	actor, target := uuid.New(), uuid.New()
	f := newFakeStore()
	f.users[target] = AdminUser{ID: target, Status: "active", IsSuperadmin: true}
	_, err := newService(f).SetStatus(context.Background(), actor, target, "disabled")
	if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindForbidden {
		t.Fatalf("expected KindForbidden, got %v", err)
	}
}

func TestSetStatus_Success(t *testing.T) {
	actor, target := uuid.New(), uuid.New()
	f := newFakeStore()
	f.users[target] = AdminUser{ID: target, Status: "active"}
	updated, err := newService(f).SetStatus(context.Background(), actor, target, "disabled")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updated.Status != "disabled" {
		t.Fatalf("expected status disabled, got %s", updated.Status)
	}
}

func TestAccountsTenancy_FiltersByEmail(t *testing.T) {
	f := newFakeStore()
	id1, id2 := uuid.New(), uuid.New()
	f.users[id1] = AdminUser{ID: id1, Email: "alice@oscod.dev", Status: "active"}
	f.users[id2] = AdminUser{ID: id2, Email: "bob@pan.org", Status: "active"}

	rep, err := newService(f).AccountsTenancy(context.Background(), "oscod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rep.Users) != 1 {
		t.Fatalf("expected 1 user matching 'oscod', got %d", len(rep.Users))
	}
	if rep.Users[0].Email != "alice@oscod.dev" {
		t.Fatalf("expected alice@oscod.dev, got %s", rep.Users[0].Email)
	}
	if rep.Users[0].Memberships == nil {
		t.Fatal("memberships must not be nil (must be an empty slice)")
	}
	if rep.Orgs == nil {
		t.Fatal("orgs must not be nil (must be an empty slice)")
	}
}

func TestAccountsTenancy_EmptySubstrReturnsAll(t *testing.T) {
	f := newFakeStore()
	for i := 0; i < 3; i++ {
		id := uuid.New()
		f.users[id] = AdminUser{ID: id, Email: "user" + string(rune('0'+i)) + "@example.com", Status: "active"}
	}
	rep, err := newService(f).AccountsTenancy(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rep.Users) != 3 {
		t.Fatalf("expected 3 users for empty substr, got %d", len(rep.Users))
	}
}

func TestAccountsTenancy_NoMatchReturnsEmptySlices(t *testing.T) {
	f := newFakeStore()
	id := uuid.New()
	f.users[id] = AdminUser{ID: id, Email: "someone@example.com", Status: "active"}

	rep, err := newService(f).AccountsTenancy(context.Background(), "zzznomatch")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rep.Users) != 0 {
		t.Fatalf("expected 0 users, got %d", len(rep.Users))
	}
	if rep.Users == nil {
		t.Fatal("users must be a non-nil empty slice")
	}
	if rep.Orgs == nil {
		t.Fatal("orgs must be a non-nil empty slice")
	}
}

// guard against a silent signature/contract drift: *Repo must satisfy userStore.
var _ userStore = (*Repo)(nil)

// TestSuperadminIsForbiddenFromAPIStatusChange is the regression guard for
// issue #100. It verifies two things:
//
//  1. SetStatus refuses to modify a superadmin account (guard already in place).
//     This ensures there is no API-exploitable path that could silently clear
//     is_superadmin by first disabling the account via the status endpoint.
//
//  2. The userStore interface has no SetSuperadmin / GrantSuperadmin method.
//     The only supported paths for mutating is_superadmin are the boot-time
//     env seeders WPMGR_SUPERADMIN_EMAILS (grant) and
//     WPMGR_SUPERADMIN_REVOKE_EMAILS (revoke), both in cmd/wpmgr/main.go,
//     operating directly on the owner DSN (bypasses RLS).
func TestSuperadminIsForbiddenFromAPIStatusChange(t *testing.T) {
	actor, target := uuid.New(), uuid.New()
	f := newFakeStore()
	f.users[actor] = AdminUser{ID: actor, Status: "active", IsSuperadmin: true}
	f.users[target] = AdminUser{ID: target, Status: "active", IsSuperadmin: true, Email: "sa@example.com"}

	// Attempting to change a superadmin's status via the service API must fail
	// with a Forbidden error. This is the guard that prevents is_superadmin from
	// being side-stepped through account suspension.
	_, err := newService(f).SetStatus(context.Background(), actor, target, "disabled")
	if err == nil {
		t.Fatal("SetStatus on a superadmin must return an error, got nil")
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindForbidden {
		t.Fatalf("expected KindForbidden, got: %v", err)
	}
	if de.Code != "cannot_modify_superadmin" {
		t.Errorf("expected code 'cannot_modify_superadmin', got %q", de.Code)
	}

	// The target must remain unchanged in the store.
	u := f.users[target]
	if !u.IsSuperadmin {
		t.Error("is_superadmin must not have been changed by a failed SetStatus call")
	}
	if u.Status != "active" {
		t.Errorf("status must remain 'active', got %q", u.Status)
	}
}
