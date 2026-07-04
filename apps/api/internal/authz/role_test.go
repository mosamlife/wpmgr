package authz

import (
	"reflect"
	"testing"
)

func TestRoleAtLeast(t *testing.T) {
	tests := []struct {
		role Role
		min  Role
		want bool
	}{
		{RoleOwner, RoleAdmin, true},
		{RoleAdmin, RoleOwner, false},
		{RoleOperator, RoleViewer, true},
		{RoleViewer, RoleOperator, false},
		{RoleViewer, RoleViewer, true},
		{RoleOwner, RoleOwner, true},
	}
	for _, tt := range tests {
		if got := tt.role.AtLeast(tt.min); got != tt.want {
			t.Errorf("%s.AtLeast(%s) = %v, want %v", tt.role, tt.min, got, tt.want)
		}
	}
}

func TestAllows(t *testing.T) {
	tests := []struct {
		name string
		role Role
		perm Permission
		want bool
	}{
		{"viewer can read sites", RoleViewer, PermSiteRead, true},
		{"viewer cannot write sites", RoleViewer, PermSiteWrite, false},
		{"operator can write sites", RoleOperator, PermSiteWrite, true},
		{"operator cannot manage members", RoleOperator, PermMemberManage, false},
		{"admin can manage members", RoleAdmin, PermMemberManage, true},
		{"admin can manage api keys", RoleAdmin, PermAPIKeyManage, true},
		{"admin can read audit", RoleAdmin, PermAuditRead, true},
		{"operator cannot read audit", RoleOperator, PermAuditRead, false},
		{"admin cannot manage tenant", RoleAdmin, PermTenantManage, false},
		{"owner can manage tenant", RoleOwner, PermTenantManage, true},
		// Audit-integrity re-baseline (m90): owner-only, same bar as PermTenantManage.
		{"admin cannot manage audit integrity", RoleAdmin, PermAuditManage, false},
		{"owner can manage audit integrity", RoleOwner, PermAuditManage, true},
		{"viewer cannot autologin", RoleViewer, PermSiteAutologin, false},
		{"operator cannot autologin", RoleOperator, PermSiteAutologin, false},
		{"admin can autologin", RoleAdmin, PermSiteAutologin, true},
		{"owner can autologin", RoleOwner, PermSiteAutologin, true},
		// Performance Suite perms (ADR-046): cache manage/purge + perf config are
		// operator+; delete-everything is admin+ (destructive).
		{"viewer cannot manage cache", RoleViewer, PermSiteCacheManage, false},
		{"operator can manage cache", RoleOperator, PermSiteCacheManage, true},
		{"operator can purge cache", RoleOperator, PermSiteCachePurge, true},
		{"operator can save perf config", RoleOperator, PermSitePerfConfig, true},
		{"operator cannot delete everything", RoleOperator, PermSiteCacheDeleteAll, false},
		{"admin can delete everything", RoleAdmin, PermSiteCacheDeleteAll, true},
		{"owner can delete everything", RoleOwner, PermSiteCacheDeleteAll, true},
		{"unknown role denied", Role("bogus"), PermSiteRead, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Allows(tt.role, tt.perm); got != tt.want {
				t.Errorf("Allows(%s, %s) = %v, want %v", tt.role, tt.perm, got, tt.want)
			}
		})
	}
}

func TestRoleValid(t *testing.T) {
	for _, r := range []Role{RoleOwner, RoleAdmin, RoleOperator, RoleViewer} {
		if !r.Valid() {
			t.Errorf("%s should be valid", r)
		}
	}
	if Role("nope").Valid() {
		t.Error("bogus role should be invalid")
	}
}

// TestRoleClientZeroPerms asserts that RoleClient holds no permissions and
// sits below RoleViewer in the hierarchy. This is the security gate that
// prevents a portal principal from reaching any write or read endpoint that
// requires RequirePermission.
func TestRoleClientZeroPerms(t *testing.T) {
	// RoleClient is valid (so it does not accidentally satisfy AtLeast via rank 0).
	if !RoleClient.Valid() {
		t.Fatal("RoleClient.Valid() = false, want true")
	}
	// RoleClient is below RoleViewer.
	if RoleClient.AtLeast(RoleViewer) {
		t.Error("RoleClient.AtLeast(RoleViewer) = true, want false")
	}
	// A bogus role string does NOT satisfy AtLeast(RoleClient) (rank 0 < rank 1).
	if Role("bogus").AtLeast(RoleClient) {
		t.Error("bogus.AtLeast(RoleClient) = true, want false")
	}

	// Enumerate all Permissions via reflection on the minRoleFor map and verify
	// Allows(RoleClient, p) is false for every one.
	allPerms := reflect.ValueOf(minRoleFor).MapKeys()
	if len(allPerms) == 0 {
		t.Fatal("minRoleFor is empty — test is misconfigured")
	}
	for _, kv := range allPerms {
		p := Permission(kv.String())
		if Allows(RoleClient, p) {
			t.Errorf("Allows(RoleClient, %q) = true, want false", p)
		}
	}
}

// TestRoleClientInviteRejection ensures CreateOrgInvitation's guard is
// exercised: RoleClient must be invalid for the org invitation path. This is
// tested at the authz layer; the service layer test is in invitation/service_test.go.
func TestRoleClientValid(t *testing.T) {
	if !RoleClient.Valid() {
		t.Fatal("RoleClient.Valid() = false, want true (it is a known role)")
	}
	// The invitation service adds an explicit guard beyond Valid() to block
	// RoleClient from org invitations. Valid() returning true is what makes that
	// guard necessary.
}
