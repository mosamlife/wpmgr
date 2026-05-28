package authz

import "testing"

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
		{"viewer cannot autologin", RoleViewer, PermSiteAutologin, false},
		{"operator cannot autologin", RoleOperator, PermSiteAutologin, false},
		{"admin can autologin", RoleAdmin, PermSiteAutologin, true},
		{"owner can autologin", RoleOwner, PermSiteAutologin, true},
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
