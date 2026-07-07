package security

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// ---------------------------------------------------------------------------
// In-memory fake policy service (no DB, no RLS, no agent push)
// ---------------------------------------------------------------------------

type fakePolicyService struct {
	policies map[uuid.UUID]SiteSecurityPolicy // keyed by siteID
	groups   map[uuid.UUID][]PolicyGroup      // keyed by siteID
	pushLog  []agentcmd.SecurityPolicyRequest  // records what was pushed
}

func newFakePolicyService() *fakePolicyService {
	return &fakePolicyService{
		policies: make(map[uuid.UUID]SiteSecurityPolicy),
		groups:   make(map[uuid.UUID][]PolicyGroup),
	}
}

func (s *fakePolicyService) getSiteSecurityPolicy(_ context.Context, tenantID, siteID uuid.UUID) (SiteSecurityPolicy, error) {
	if p, ok := s.policies[siteID]; ok {
		return p, nil
	}
	return DefaultSiteSecurityPolicy(tenantID, siteID), nil
}

func (s *fakePolicyService) saveSiteSecurityPolicy(_ context.Context, tenantID, siteID uuid.UUID, pol SiteSecurityPolicy, actorType, actorID string) (SiteSecurityPolicy, error) {
	// Run the same validation as the real service so tests cover the paths.
	if pol.TwoFactorGraceLogins < 0 || pol.TwoFactorGraceLogins > 100 {
		return SiteSecurityPolicy{}, domain.Validation("invalid_grace_logins",
			"two_factor_grace_logins must be between 0 and 100")
	}
	if pol.TwoFactorRememberDeviceDays < 0 || pol.TwoFactorRememberDeviceDays > 365 {
		return SiteSecurityPolicy{}, domain.Validation("invalid_remember_device_days",
			"two_factor_remember_device_days must be between 0 and 365")
	}
	for _, m := range pol.TwoFactorMethods {
		if m != "totp" && m != "email" && m != "backup" {
			return SiteSecurityPolicy{}, domain.Validation("invalid_2fa_method",
				"unknown 2FA method: "+m)
		}
	}
	// Mirror the deliverability-floor guard from the real service.
	if len(pol.TwoFactorRequiredRoles) > 0 && len(pol.TwoFactorMethods) > 0 {
		hasNonEmail := false
		for _, m := range pol.TwoFactorMethods {
			if m == "totp" || m == "backup" {
				hasNonEmail = true
				break
			}
		}
		if !hasNonEmail {
			return SiteSecurityPolicy{}, domain.Validation("2fa_email_only_required",
				"when two_factor_required_roles is set, at least one non-email method (totp or backup) must be included")
		}
	}
	if pol.PasswordMinZxcvbnScore < 0 || pol.PasswordMinZxcvbnScore > 4 {
		return SiteSecurityPolicy{}, domain.Validation("invalid_zxcvbn_score",
			"password_min_zxcvbn_score must be between 0 and 4")
	}
	if pol.PasswordMaxAgeDays < 0 || pol.PasswordMaxAgeDays > 3650 {
		return SiteSecurityPolicy{}, domain.Validation("invalid_max_age_days",
			"password_max_age_days must be between 0 and 3650")
	}
	if pol.HideBackendEnabled && pol.HideBackendSlug != "" {
		if !hideBackendSlugRe.MatchString(pol.HideBackendSlug) {
			return SiteSecurityPolicy{}, domain.Validation("invalid_hide_backend_slug",
				"hide_backend_slug must match ^[a-z0-9-]{4,64}$")
		}
	}
	pol.TenantID = tenantID
	pol.SiteID = siteID
	pol.ActorType = actorType
	pol.ActorID = actorID
	pol.UpdatedAt = time.Now()
	if pol.TwoFactorMethods == nil {
		pol.TwoFactorMethods = []string{}
	}
	if pol.TwoFactorRequiredRoles == nil {
		pol.TwoFactorRequiredRoles = []string{}
	}
	if pol.PasswordMinZxcvbnRoles == nil {
		pol.PasswordMinZxcvbnRoles = []string{}
	}
	if pol.PasswordExpiryRoles == nil {
		pol.PasswordExpiryRoles = []string{}
	}
	s.policies[siteID] = pol
	return pol, nil
}

func (s *fakePolicyService) getPolicyGroups(_ context.Context, _, siteID uuid.UUID) ([]PolicyGroup, error) {
	if g, ok := s.groups[siteID]; ok {
		return g, nil
	}
	return []PolicyGroup{}, nil
}

func (s *fakePolicyService) upsertPolicyGroup(_ context.Context, tenantID, siteID uuid.UUID, g PolicyGroup) (PolicyGroup, error) {
	if strings.TrimSpace(g.Role) == "" {
		return PolicyGroup{}, domain.Validation("invalid_role", "role is required")
	}
	if g.MinZxcvbnScore != nil && (*g.MinZxcvbnScore < 0 || *g.MinZxcvbnScore > 4) {
		return PolicyGroup{}, domain.Validation("invalid_zxcvbn_score", "min_zxcvbn_score must be between 0 and 4")
	}
	g.TenantID = tenantID
	g.SiteID = siteID
	g.ID = uuid.New()
	g.CreatedAt = time.Now()
	// Replace or add by role.
	existing := s.groups[siteID]
	replaced := false
	for i, eg := range existing {
		if eg.Role == g.Role {
			existing[i] = g
			replaced = true
			break
		}
	}
	if !replaced {
		existing = append(existing, g)
	}
	s.groups[siteID] = existing
	return g, nil
}

func (s *fakePolicyService) deletePolicyGroup(_ context.Context, _, siteID uuid.UUID, role string) error {
	existing := s.groups[siteID]
	for i, g := range existing {
		if g.Role == role {
			s.groups[siteID] = append(existing[:i], existing[i+1:]...)
			return nil
		}
	}
	return domain.NotFound("policy_group_not_found", "policy group not found for this role")
}

// ---------------------------------------------------------------------------
// Tests: policy default-OFF
// ---------------------------------------------------------------------------

// TestPolicyDefaultOFF verifies that a site with no stored policy returns the
// safe default (everything OFF / zero).
func TestPolicyDefaultOFF(t *testing.T) {
	svc := newFakePolicyService()
	tenantID := uuid.New()
	siteID := uuid.New()

	pol, err := svc.getSiteSecurityPolicy(context.Background(), tenantID, siteID)
	if err != nil {
		t.Fatalf("GetSiteSecurityPolicy: %v", err)
	}
	if pol.TwoFactorEnabled {
		t.Error("default: two_factor_enabled should be false")
	}
	if pol.PasswordBlockCompromised {
		t.Error("default: password_block_compromised should be false")
	}
	if pol.PasswordMaxAgeDays != 0 {
		t.Errorf("default: password_max_age_days want 0, got %d", pol.PasswordMaxAgeDays)
	}
	if pol.HideBackendEnabled {
		t.Error("default: hide_backend_enabled should be false")
	}
	if pol.PasswordMinZxcvbnScore != 0 {
		t.Errorf("default: password_min_zxcvbn_score want 0, got %d", pol.PasswordMinZxcvbnScore)
	}
	if len(pol.TwoFactorRequiredRoles) != 0 {
		t.Errorf("default: two_factor_required_roles want [], got %v", pol.TwoFactorRequiredRoles)
	}
	// Defaults that are non-zero but still safe/permissive.
	if pol.TwoFactorGraceLogins != 3 {
		t.Errorf("default: two_factor_grace_logins want 3, got %d", pol.TwoFactorGraceLogins)
	}
	if pol.TwoFactorRememberDeviceDays != 30 {
		t.Errorf("default: two_factor_remember_device_days want 30, got %d", pol.TwoFactorRememberDeviceDays)
	}
}

// ---------------------------------------------------------------------------
// Tests: policy put/get round-trip
// ---------------------------------------------------------------------------

func TestPolicyPutGetRoundTrip(t *testing.T) {
	svc := newFakePolicyService()
	tenantID := uuid.New()
	siteID := uuid.New()
	ctx := context.Background()

	input := SiteSecurityPolicy{
		TwoFactorEnabled:            true,
		TwoFactorMethods:            []string{"totp", "backup"},
		TwoFactorRequiredRoles:      []string{"administrator"},
		TwoFactorGraceLogins:        5,
		TwoFactorRememberDeviceDays: 14,
		BlockXMLRPCFor2FAUsers:      true,
		PasswordMinZxcvbnScore:      3,
		PasswordBlockCompromised:    true,
		PasswordReuseBlockCount:     5,
		PasswordMaxAgeDays:          90,
		HideBackendEnabled:          true,
		HideBackendSlug:             "my-secret-login",
		HideBackendRedirect:         "https://example.com",
	}

	saved, err := svc.saveSiteSecurityPolicy(ctx, tenantID, siteID, input, "user", "u1")
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := svc.getSiteSecurityPolicy(ctx, tenantID, siteID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if !got.TwoFactorEnabled {
		t.Error("two_factor_enabled should be true after save")
	}
	if !got.PasswordBlockCompromised {
		t.Error("password_block_compromised should be true after save")
	}
	if got.PasswordMaxAgeDays != 90 {
		t.Errorf("password_max_age_days: want 90, got %d", got.PasswordMaxAgeDays)
	}
	if got.HideBackendSlug != "my-secret-login" {
		t.Errorf("hide_backend_slug: want 'my-secret-login', got %q", got.HideBackendSlug)
	}
	if got.TenantID != tenantID {
		t.Errorf("tenant_id: want %s, got %s", tenantID, got.TenantID)
	}
	if got.TwoFactorGraceLogins != 5 {
		t.Errorf("two_factor_grace_logins: want 5, got %d", got.TwoFactorGraceLogins)
	}
	if saved.TwoFactorEnabled != got.TwoFactorEnabled {
		t.Error("saved and get mismatch for two_factor_enabled")
	}
}

// ---------------------------------------------------------------------------
// Tests: tenant isolation
// ---------------------------------------------------------------------------

func TestPolicyTenantIsolation(t *testing.T) {
	svc := newFakePolicyService()
	tenantA := uuid.New()
	tenantB := uuid.New()
	siteA := uuid.New()
	siteB := uuid.New()
	ctx := context.Background()

	polA := SiteSecurityPolicy{
		TwoFactorEnabled: true,
		TwoFactorMethods: []string{"totp"},
		PasswordMaxAgeDays: 30,
	}
	savedA, err := svc.saveSiteSecurityPolicy(ctx, tenantA, siteA, polA, "user", "ua")
	if err != nil {
		t.Fatalf("save A: %v", err)
	}
	if savedA.TenantID != tenantA {
		t.Errorf("savedA.TenantID want %s got %s", tenantA, savedA.TenantID)
	}

	polB := SiteSecurityPolicy{
		TwoFactorEnabled: false,
		TwoFactorMethods: []string{"email"},
	}
	savedB, err := svc.saveSiteSecurityPolicy(ctx, tenantB, siteB, polB, "user", "ub")
	if err != nil {
		t.Fatalf("save B: %v", err)
	}
	if savedB.TenantID != tenantB {
		t.Errorf("savedB.TenantID want %s got %s", tenantB, savedB.TenantID)
	}

	// Site A still belongs to tenant A and has 2FA enabled.
	gotA, _ := svc.getSiteSecurityPolicy(ctx, tenantA, siteA)
	if gotA.TenantID != tenantA {
		t.Errorf("gotA.TenantID: want %s got %s", tenantA, gotA.TenantID)
	}
	if !gotA.TwoFactorEnabled {
		t.Error("tenant A 2FA should still be true")
	}

	// Site B belongs to tenant B and has 2FA disabled.
	gotB, _ := svc.getSiteSecurityPolicy(ctx, tenantB, siteB)
	if gotB.TenantID != tenantB {
		t.Errorf("gotB.TenantID: want %s got %s", tenantB, gotB.TenantID)
	}
	if gotB.TwoFactorEnabled {
		t.Error("tenant B 2FA should be false")
	}
}

// ---------------------------------------------------------------------------
// Tests: policy group CRUD + tenant isolation
// ---------------------------------------------------------------------------

func TestPolicyGroupCRUD(t *testing.T) {
	svc := newFakePolicyService()
	tenantID := uuid.New()
	siteID := uuid.New()
	ctx := context.Background()

	// Empty list.
	groups, err := svc.getPolicyGroups(ctx, tenantID, siteID)
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("want empty list, got %d", len(groups))
	}

	// Upsert an admin group.
	require2fa := true
	score := 3
	maxAge := 90
	g, err := svc.upsertPolicyGroup(ctx, tenantID, siteID, PolicyGroup{
		Role:             "administrator",
		Require2FA:       &require2fa,
		MinZxcvbnScore:   &score,
		MaxAgeDays:       &maxAge,
		AllowedMethods:   []string{"totp", "backup"},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if g.Role != "administrator" {
		t.Errorf("role: want 'administrator', got %q", g.Role)
	}
	if g.ID == uuid.Nil {
		t.Error("ID should be non-zero after upsert")
	}

	// List should have 1 group.
	groups, _ = svc.getPolicyGroups(ctx, tenantID, siteID)
	if len(groups) != 1 {
		t.Fatalf("want 1 group, got %d", len(groups))
	}

	// Upsert again — should update.
	score2 := 4
	g2, err := svc.upsertPolicyGroup(ctx, tenantID, siteID, PolicyGroup{
		Role:           "administrator",
		MinZxcvbnScore: &score2,
	})
	if err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	if *g2.MinZxcvbnScore != 4 {
		t.Errorf("min_zxcvbn_score after update: want 4, got %d", *g2.MinZxcvbnScore)
	}

	// List still 1 (upsert, not insert).
	groups, _ = svc.getPolicyGroups(ctx, tenantID, siteID)
	if len(groups) != 1 {
		t.Fatalf("want 1 group after update, got %d", len(groups))
	}

	// Delete.
	if err := svc.deletePolicyGroup(ctx, tenantID, siteID, "administrator"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	groups, _ = svc.getPolicyGroups(ctx, tenantID, siteID)
	if len(groups) != 0 {
		t.Fatalf("want 0 groups after delete, got %d", len(groups))
	}

	// Delete again — not found.
	err = svc.deletePolicyGroup(ctx, tenantID, siteID, "administrator")
	if err == nil {
		t.Fatal("want NotFound error for second delete, got nil")
	}
	if domain.HTTPStatus(err) != http.StatusNotFound {
		t.Errorf("want 404, got %d", domain.HTTPStatus(err))
	}
}

func TestPolicyGroupTenantIsolation(t *testing.T) {
	svc := newFakePolicyService()
	tenantA := uuid.New()
	siteA := uuid.New()
	tenantB := uuid.New()
	siteB := uuid.New()
	ctx := context.Background()

	require2fa := true
	gA, err := svc.upsertPolicyGroup(ctx, tenantA, siteA, PolicyGroup{
		Role: "editor", Require2FA: &require2fa,
	})
	if err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	if gA.TenantID != tenantA {
		t.Errorf("gA.TenantID: want %s got %s", tenantA, gA.TenantID)
	}

	// Tenant B's site B has no groups.
	groupsB, _ := svc.getPolicyGroups(ctx, tenantB, siteB)
	if len(groupsB) != 0 {
		t.Errorf("tenant B should see no groups, got %d", len(groupsB))
	}

	// Tenant A's site A has 1 group.
	groupsA, _ := svc.getPolicyGroups(ctx, tenantA, siteA)
	if len(groupsA) != 1 {
		t.Errorf("tenant A should see 1 group, got %d", len(groupsA))
	}
}

// ---------------------------------------------------------------------------
// Tests: validation errors
// ---------------------------------------------------------------------------

func TestPolicyValidationErrors(t *testing.T) {
	svc := newFakePolicyService()
	tenantID := uuid.New()
	siteID := uuid.New()
	ctx := context.Background()

	cases := []struct {
		name    string
		pol     SiteSecurityPolicy
		wantCode string
	}{
		{
			name:     "grace_logins out of range",
			pol:      SiteSecurityPolicy{TwoFactorGraceLogins: 200},
			wantCode: "invalid_grace_logins",
		},
		{
			name:     "remember_device_days out of range",
			pol:      SiteSecurityPolicy{TwoFactorRememberDeviceDays: 400},
			wantCode: "invalid_remember_device_days",
		},
		{
			name:     "invalid 2FA method",
			pol:      SiteSecurityPolicy{TwoFactorMethods: []string{"sms"}},
			wantCode: "invalid_2fa_method",
		},
		{
			name:     "zxcvbn score out of range",
			pol:      SiteSecurityPolicy{PasswordMinZxcvbnScore: 5},
			wantCode: "invalid_zxcvbn_score",
		},
		{
			name:     "max_age_days out of range",
			pol:      SiteSecurityPolicy{PasswordMaxAgeDays: 9999},
			wantCode: "invalid_max_age_days",
		},
		{
			name:     "hide_backend_slug invalid",
			pol:      SiteSecurityPolicy{HideBackendEnabled: true, HideBackendSlug: "NO UPPERCASE"},
			wantCode: "invalid_hide_backend_slug",
		},
		{
			// Deliverability floor: a required-2FA role with email-only methods
			// must be rejected so users cannot be hard-locked out by mail failure.
			name: "required roles with email-only methods",
			pol: SiteSecurityPolicy{
				TwoFactorRequiredRoles: []string{"administrator"},
				TwoFactorMethods:       []string{"email"},
			},
			wantCode: "2fa_email_only_required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.saveSiteSecurityPolicy(ctx, tenantID, siteID, tc.pol, "user", "u1")
			if err == nil {
				t.Fatalf("want error, got nil for %+v", tc.pol)
			}
			de, ok := domain.AsDomain(err)
			if !ok {
				t.Fatalf("want domain error, got %T: %v", err, err)
			}
			if domain.HTTPStatus(err) != http.StatusUnprocessableEntity {
				t.Errorf("want 422, got %d", domain.HTTPStatus(err))
			}
			if de.Code != tc.wantCode {
				t.Errorf("want domain code %q, got %q", tc.wantCode, de.Code)
			}
		})
	}
}

func TestPolicyGroupValidationErrors(t *testing.T) {
	svc := newFakePolicyService()
	tenantID := uuid.New()
	siteID := uuid.New()
	ctx := context.Background()

	// Empty role.
	_, err := svc.upsertPolicyGroup(ctx, tenantID, siteID, PolicyGroup{Role: ""})
	if err == nil {
		t.Fatal("want error for empty role, got nil")
	}
	if domain.HTTPStatus(err) != http.StatusUnprocessableEntity {
		t.Errorf("want 422 for empty role, got %d", domain.HTTPStatus(err))
	}

	// Out-of-range min_zxcvbn_score.
	score := 9
	_, err = svc.upsertPolicyGroup(ctx, tenantID, siteID, PolicyGroup{
		Role:           "editor",
		MinZxcvbnScore: &score,
	})
	if err == nil {
		t.Fatal("want error for out-of-range score, got nil")
	}
	if domain.HTTPStatus(err) != http.StatusUnprocessableEntity {
		t.Errorf("want 422 for out-of-range score, got %d", domain.HTTPStatus(err))
	}
}

// ---------------------------------------------------------------------------
// Tests: sync_security_policy JSON contract
// ---------------------------------------------------------------------------

// TestSecurityPolicyContractSerialization verifies that SecurityPolicyRequest
// round-trips through JSON with the exact field names documented in ADR-059.
func TestSecurityPolicyContractSerialization(t *testing.T) {
	require2fa := true
	score := 3
	maxAge := 90
	blockComp := true

	req := agentcmd.SecurityPolicyRequest{
		Policy: agentcmd.SecurityPolicy{
			TwoFactorEnabled:            true,
			TwoFactorMethods:            []string{"totp", "backup"},
			TwoFactorRequiredRoles:      []string{"administrator"},
			TwoFactorGraceLogins:        5,
			TwoFactorRememberDeviceDays: 14,
			BlockXMLRPCFor2FAUsers:      true,
			PasswordMinZxcvbnScore:      3,
			PasswordBlockCompromised:    true,
			PasswordReuseBlockCount:     5,
			PasswordMaxAgeDays:          90,
			HideBackendEnabled:          true,
			HideBackendSlug:             "my-secret-login",
			HideBackendRedirect:         "",
		},
		Groups: []agentcmd.SecurityPolicyGroup{
			{
				Role:             "administrator",
				Require2FA:       &require2fa,
				AllowedMethods:   []string{"totp", "backup"},
				MinZxcvbnScore:   &score,
				BlockCompromised: &blockComp,
				MaxAgeDays:       &maxAge,
			},
		},
		ForcePasswordChange: []agentcmd.ForcePasswordChangeEntry{
			{UserLogin: "johndoe", Reason: "admin_reset"},
		},
	}

	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded agentcmd.SecurityPolicyRequest
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Check top-level keys.
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for _, key := range []string{"policy", "groups", "force_password_change"} {
		if _, ok := m[key]; !ok {
			t.Errorf("wire JSON missing top-level key %q", key)
		}
	}

	// Check policy field names.
	polMap, _ := m["policy"].(map[string]any)
	for _, key := range []string{
		"two_factor_enabled", "two_factor_methods", "two_factor_required_roles",
		"two_factor_grace_logins", "two_factor_remember_device_days",
		"block_xmlrpc_for_2fa_users",
		"password_min_zxcvbn_score", "password_block_compromised",
		"password_reuse_block_count", "password_max_age_days",
		"hide_backend_enabled", "hide_backend_slug", "hide_backend_redirect",
	} {
		if _, ok := polMap[key]; !ok {
			t.Errorf("policy JSON missing key %q", key)
		}
	}

	// Check group field names.
	groupsRaw, _ := m["groups"].([]any)
	if len(groupsRaw) != 1 {
		t.Fatalf("want 1 group, got %d", len(groupsRaw))
	}
	grpMap, _ := groupsRaw[0].(map[string]any)
	for _, key := range []string{"role", "require_2fa", "allowed_methods",
		"min_zxcvbn_score", "block_compromised", "max_age_days"} {
		if _, ok := grpMap[key]; !ok {
			t.Errorf("group JSON missing key %q", key)
		}
	}

	// Check force_password_change.
	fpcRaw, _ := m["force_password_change"].([]any)
	if len(fpcRaw) != 1 {
		t.Fatalf("want 1 force_password_change entry, got %d", len(fpcRaw))
	}
	fpcMap, _ := fpcRaw[0].(map[string]any)
	for _, key := range []string{"user_login", "reason"} {
		if _, ok := fpcMap[key]; !ok {
			t.Errorf("force_password_change JSON missing key %q", key)
		}
	}

	// Decoded values.
	if !decoded.Policy.TwoFactorEnabled {
		t.Error("decoded two_factor_enabled should be true")
	}
	if decoded.Policy.HideBackendSlug != "my-secret-login" {
		t.Errorf("decoded hide_backend_slug: want 'my-secret-login', got %q", decoded.Policy.HideBackendSlug)
	}
	if decoded.Groups[0].Role != "administrator" {
		t.Errorf("decoded group role: want 'administrator', got %q", decoded.Groups[0].Role)
	}
	if decoded.Groups[0].Require2FA == nil || !*decoded.Groups[0].Require2FA {
		t.Error("decoded group require_2fa should be true")
	}
	if decoded.ForcePasswordChange[0].UserLogin != "johndoe" {
		t.Errorf("decoded force_password_change user_login: want 'johndoe', got %q",
			decoded.ForcePasswordChange[0].UserLogin)
	}
}

// TestSecurityPolicyResultSerialization verifies the response contract round-trip.
func TestSecurityPolicyResultSerialization(t *testing.T) {
	enrolled := 2
	required := 2
	total := 3
	result := agentcmd.SecurityPolicyResult{
		OK:     true,
		Detail: "applied",
		EnrollmentSummary: &agentcmd.EnrollmentSummary{
			PerRole: map[string]agentcmd.RoleEnrollment{
				"administrator": {Enrolled: enrolled, Required: required, Total: total},
			},
		},
	}

	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded agentcmd.SecurityPolicyResult
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !decoded.OK {
		t.Error("decoded ok should be true")
	}
	if decoded.Detail != "applied" {
		t.Errorf("decoded detail: want 'applied', got %q", decoded.Detail)
	}
	if decoded.EnrollmentSummary == nil {
		t.Fatal("decoded enrollment_summary should not be nil")
	}
	adminEnroll := decoded.EnrollmentSummary.PerRole["administrator"]
	if adminEnroll.Enrolled != 2 || adminEnroll.Required != 2 || adminEnroll.Total != 3 {
		t.Errorf("enrollment counts mismatch: %+v", adminEnroll)
	}
}

// TestSecurityPolicyResultPerRoleEmptyTolerance is the GH #170 regression: an
// agent-serialized empty enrollment_summary.per_role can arrive as a JSON
// array `[]` (PHP json_encode of an empty associative array), an empty
// object `{}`, or be omitted entirely as `null`. All three must decode
// cleanly to an empty (non-nil-panicking) map — same defense-in-depth class
// as GH #148 — and a populated map must still decode unchanged.
func TestSecurityPolicyResultPerRoleEmptyTolerance(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		wantLen int
	}{
		{
			// The EXACT #170 payload: an older/pre-fix agent's PHP json_encode
			// of an empty associative array for per_role.
			name:    "per_role empty array (exact #170 payload)",
			payload: `{"ok":true,"detail":"applied","enrollment_summary":{"per_role":[]}}`,
			wantLen: 0,
		},
		{
			name:    "per_role empty object",
			payload: `{"ok":true,"detail":"applied","enrollment_summary":{"per_role":{}}}`,
			wantLen: 0,
		},
		{
			name:    "per_role null",
			payload: `{"ok":true,"detail":"applied","enrollment_summary":{"per_role":null}}`,
			wantLen: 0,
		},
		{
			name:    "per_role populated (unchanged control)",
			payload: `{"ok":true,"detail":"applied","enrollment_summary":{"per_role":{"administrator":{"enrolled":2,"required":2,"total":3}}}}`,
			wantLen: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var decoded agentcmd.SecurityPolicyResult
			if err := json.Unmarshal([]byte(tc.payload), &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !decoded.OK {
				t.Error("decoded ok should be true")
			}
			if decoded.EnrollmentSummary == nil {
				t.Fatal("decoded enrollment_summary should not be nil")
			}
			if got := len(decoded.EnrollmentSummary.PerRole); got != tc.wantLen {
				t.Errorf("PerRole len: want %d, got %d (%+v)", tc.wantLen, got, decoded.EnrollmentSummary.PerRole)
			}
			if tc.wantLen == 1 {
				admin := decoded.EnrollmentSummary.PerRole["administrator"]
				if admin.Enrolled != 2 || admin.Required != 2 || admin.Total != 3 {
					t.Errorf("enrollment counts mismatch: %+v", admin)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Tests: HIBP proxy — cache hit/miss + fail-open + only-prefix-sent
// ---------------------------------------------------------------------------

// fakeHIBPDoer is a test double for HIBPDoer that records what URL was called.
type fakeHIBPDoer struct {
	body       string
	statusCode int
	calls      []string
	err        error
}

func (f *fakeHIBPDoer) Do(req *http.Request) (*http.Response, error) {
	f.calls = append(f.calls, req.URL.String())
	if f.err != nil {
		return nil, f.err
	}
	return &http.Response{
		StatusCode: f.statusCode,
		Body:       io.NopCloser(strings.NewReader(f.body)),
	}, nil
}

// fakeHIBPRepo simulates the hibp cache layer for the service under test.
type fakeHIBPRepo struct {
	cache map[string]string // prefix -> body
}

func newFakeHIBPRepo() *fakeHIBPRepo {
	return &fakeHIBPRepo{cache: make(map[string]string)}
}

func (r *fakeHIBPRepo) get(prefix string) (string, bool) {
	v, ok := r.cache[prefix]
	return v, ok
}

func (r *fakeHIBPRepo) set(prefix, body string) {
	r.cache[prefix] = body
}

// hibpFakeService wires the same logic as Service.GetHIBPRange but with fake
// repo + doer, so we test the cache/fetch/fail-open logic without a DB.
type hibpFakeService struct {
	repo  *fakeHIBPRepo
	doer  *fakeHIBPDoer
}

func (s *hibpFakeService) GetHIBPRange(ctx context.Context, prefix string) (string, error) {
	prefix = strings.ToUpper(strings.TrimSpace(prefix))
	if !hibpPrefixRe.MatchString(prefix) {
		return "", domain.Validation("invalid_hibp_prefix",
			"HIBP prefix must be exactly 5 uppercase hex characters")
	}
	// Cache hit.
	if cached, ok := s.repo.get(prefix); ok {
		return cached, nil
	}
	// Cache miss — fetch.
	if s.doer == nil || s.doer.err != nil {
		// Fail-open.
		return "", nil
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.pwnedpasswords.com/range/"+prefix, nil)
	req.Header.Set("Add-Padding", "true")
	resp, fetchErr := s.doer.Do(req)
	if fetchErr != nil {
		return "", nil // fail-open
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", nil // fail-open
	}
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)
	s.repo.set(prefix, body)
	return body, nil
}

// TestHIBPCacheHit verifies that a prefix already in cache is returned without
// calling the live HIBP API.
func TestHIBPCacheHit(t *testing.T) {
	doer := &fakeHIBPDoer{body: "THISHOULD:0\nNOTBE:1\n", statusCode: 200}
	repo := newFakeHIBPRepo()
	repo.set("AABB1", "FAKECACHED:5\n")
	svc := &hibpFakeService{repo: repo, doer: doer}

	body, err := svc.GetHIBPRange(context.Background(), "AABB1")
	if err != nil {
		t.Fatalf("GetHIBPRange: %v", err)
	}
	if body != "FAKECACHED:5\n" {
		t.Errorf("want cached body, got %q", body)
	}
	if len(doer.calls) != 0 {
		t.Errorf("want 0 HIBP calls on cache hit, got %d", len(doer.calls))
	}
}

// TestHIBPCacheMiss verifies that on a cache miss the live HIBP API is called
// and the result is stored.
func TestHIBPCacheMiss(t *testing.T) {
	rangeBody := "1234567890ABCDEF1234567890ABCDEF1234567890A:3\n"
	doer := &fakeHIBPDoer{body: rangeBody, statusCode: 200}
	repo := newFakeHIBPRepo()
	svc := &hibpFakeService{repo: repo, doer: doer}

	body, err := svc.GetHIBPRange(context.Background(), "AABB1")
	if err != nil {
		t.Fatalf("GetHIBPRange: %v", err)
	}
	if body != rangeBody {
		t.Errorf("want live HIBP body, got %q", body)
	}
	if len(doer.calls) != 1 {
		t.Errorf("want 1 HIBP call on cache miss, got %d", len(doer.calls))
	}
	// Verify the result was cached.
	cached, ok := repo.get("AABB1")
	if !ok || cached != rangeBody {
		t.Errorf("want result cached, got ok=%v cached=%q", ok, cached)
	}
}

// TestHIBPOnlyPrefixSent verifies that the HIBP URL contains only the 5-char
// prefix and nothing else (no password, no full hash).
func TestHIBPOnlyPrefixSent(t *testing.T) {
	doer := &fakeHIBPDoer{body: "", statusCode: 200}
	repo := newFakeHIBPRepo()
	svc := &hibpFakeService{repo: repo, doer: doer}

	_, _ = svc.GetHIBPRange(context.Background(), "A1B2C")

	if len(doer.calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(doer.calls))
	}
	called := doer.calls[0]
	wantSuffix := "/range/A1B2C"
	if !strings.HasSuffix(called, wantSuffix) {
		t.Errorf("URL %q should end with %q (only prefix sent)", called, wantSuffix)
	}
	// Ensure no extra query parameters or path segments.
	if strings.Contains(called, "?") {
		t.Errorf("HIBP URL must not contain query parameters, got %q", called)
	}
}

// TestHIBPFailOpen verifies that when the HIBP API is unreachable, the proxy
// returns an empty body (not an error) so the agent treats the prefix as not
// breached (fail-open).
func TestHIBPFailOpen(t *testing.T) {
	import_err := strings.NewReader("") // unused sentinel
	_ = import_err
	doer := &fakeHIBPDoer{err: io.ErrUnexpectedEOF}
	repo := newFakeHIBPRepo()
	svc := &hibpFakeService{repo: repo, doer: doer}

	body, err := svc.GetHIBPRange(context.Background(), "AABB1")
	if err != nil {
		t.Fatalf("GetHIBPRange should be fail-open, got error: %v", err)
	}
	if body != "" {
		t.Errorf("fail-open: want empty body, got %q", body)
	}
}

// TestHIBPPrefixValidation verifies that the service rejects invalid prefixes.
func TestHIBPPrefixValidation(t *testing.T) {
	doer := &fakeHIBPDoer{statusCode: 200, body: ""}
	repo := newFakeHIBPRepo()
	svc := &hibpFakeService{repo: repo, doer: doer}

	cases := []struct{ prefix string }{
		{prefix: "AAAA"},    // too short
		{prefix: "AABB12"},  // too long
		{prefix: "aabb1"},   // lowercase (tolerated by ToUpper — but test via service directly)
		{prefix: "AABB!"},   // non-hex character
		{prefix: ""},        // empty
	}
	for _, tc := range cases {
		// Lowercase is normalised to uppercase before the regex check; test the
		// non-normalised invalid cases only.
		if tc.prefix == "aabb1" {
			// service normalises to uppercase, so "aabb1" → "AABB1" is valid
			_, err := svc.GetHIBPRange(context.Background(), tc.prefix)
			if err != nil {
				t.Errorf("normalised lowercase %q should succeed, got %v", tc.prefix, err)
			}
			continue
		}
		_, err := svc.GetHIBPRange(context.Background(), tc.prefix)
		if err == nil {
			t.Errorf("want validation error for prefix %q, got nil", tc.prefix)
			continue
		}
		if domain.HTTPStatus(err) != http.StatusUnprocessableEntity {
			t.Errorf("want 422 for invalid prefix %q, got %d", tc.prefix, domain.HTTPStatus(err))
		}
	}
}

// ---------------------------------------------------------------------------
// HTTP handler tests: GET/PUT /security/policy
// ---------------------------------------------------------------------------

func TestHandlerGetPolicyReturnsDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	siteID := uuid.New()
	fSvc := newFakePolicyService()

	engine := gin.New()
	engine.Use(principalMiddleware(tenantID))
	engine.GET("/sites/:siteId/security/policy", func(c *gin.Context) {
		p, _ := domain.PrincipalFromContext(c.Request.Context())
		sid, _ := uuid.Parse(c.Param("siteId"))
		pol, err := fSvc.getSiteSecurityPolicy(c.Request.Context(), p.TenantID, sid)
		if err != nil {
			httpx.Error(c, err)
			return
		}
		c.JSON(http.StatusOK, toPolicyDTO(pol))
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/sites/"+siteID.String()+"/security/policy", nil)
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp policyDTO
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.TwoFactorEnabled {
		t.Error("default two_factor_enabled should be false")
	}
	if resp.PasswordBlockCompromised {
		t.Error("default password_block_compromised should be false")
	}
	// Nil-safe: empty slices serialise as []
	if resp.TwoFactorMethods == nil {
		t.Error("two_factor_methods should never be null in the response")
	}
}

func TestHandlerPutPolicyValidationError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	siteID := uuid.New()
	fSvc := newFakePolicyService()

	engine := gin.New()
	engine.Use(principalMiddleware(tenantID))
	engine.PUT("/sites/:siteId/security/policy", func(c *gin.Context) {
		p, _ := domain.PrincipalFromContext(c.Request.Context())
		sid, _ := uuid.Parse(c.Param("siteId"))
		var body policyDTO
		if err := bindJSON(c, &body); err != nil {
			httpx.Error(c, err)
			return
		}
		pol := fromPolicyDTO(body, p.TenantID, sid)
		saved, saveErr := fSvc.saveSiteSecurityPolicy(c.Request.Context(), p.TenantID, sid, pol, "user", "u1")
		if saveErr != nil {
			if _, ok := domain.AsDomain(saveErr); ok {
				httpx.Error(c, saveErr)
				return
			}
			c.JSON(http.StatusOK, toPolicyDTO(saved))
			return
		}
		c.JSON(http.StatusOK, toPolicyDTO(saved))
	})

	// Invalid zxcvbn score.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut,
		"/sites/"+siteID.String()+"/security/policy",
		strings.NewReader(`{"password_min_zxcvbn_score": 9}`))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for invalid score, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandlerPutPolicyRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	siteID := uuid.New()
	fSvc := newFakePolicyService()

	engine := gin.New()
	engine.Use(principalMiddleware(tenantID))
	engine.PUT("/sites/:siteId/security/policy", func(c *gin.Context) {
		p, _ := domain.PrincipalFromContext(c.Request.Context())
		sid, _ := uuid.Parse(c.Param("siteId"))
		var body policyDTO
		if err := bindJSON(c, &body); err != nil {
			httpx.Error(c, err)
			return
		}
		pol := fromPolicyDTO(body, p.TenantID, sid)
		saved, saveErr := fSvc.saveSiteSecurityPolicy(c.Request.Context(), p.TenantID, sid, pol, "user", "u1")
		if saveErr != nil {
			if _, ok := domain.AsDomain(saveErr); ok {
				httpx.Error(c, saveErr)
				return
			}
		}
		c.JSON(http.StatusOK, toPolicyDTO(saved))
	})

	body := `{
		"two_factor_enabled": true,
		"two_factor_methods": ["totp","backup"],
		"two_factor_required_roles": ["administrator"],
		"two_factor_grace_logins": 5,
		"two_factor_remember_device_days": 14,
		"block_xmlrpc_for_2fa_users": true,
		"password_min_zxcvbn_score": 3,
		"password_block_compromised": true,
		"password_max_age_days": 90,
		"hide_backend_enabled": true,
		"hide_backend_slug": "my-secret-login"
	}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut,
		"/sites/"+siteID.String()+"/security/policy",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp policyDTO
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.TwoFactorEnabled {
		t.Error("two_factor_enabled should be true in response")
	}
	if !resp.PasswordBlockCompromised {
		t.Error("password_block_compromised should be true in response")
	}
	if resp.HideBackendSlug != "my-secret-login" {
		t.Errorf("hide_backend_slug: want 'my-secret-login', got %q", resp.HideBackendSlug)
	}
	if resp.TwoFactorMethods == nil {
		t.Error("two_factor_methods must not be null in response")
	}
}

// TestPolicyDTOFieldNames verifies the JSON wire names match ADR-059 exactly.
func TestPolicyDTOFieldNames(t *testing.T) {
	pol := DefaultSiteSecurityPolicy(uuid.New(), uuid.New())
	dto := toPolicyDTO(pol)
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for _, key := range []string{
		"two_factor_enabled",
		"two_factor_methods",
		"two_factor_required_roles",
		"two_factor_grace_logins",
		"two_factor_remember_device_days",
		"block_xmlrpc_for_2fa_users",
		"password_min_zxcvbn_score",
		"password_min_zxcvbn_roles",
		"password_block_compromised",
		"password_reuse_block_count",
		"password_max_age_days",
		"password_expiry_roles",
		"hide_backend_enabled",
		"hide_backend_slug",
		"hide_backend_redirect",
	} {
		if _, ok := m[key]; !ok {
			t.Errorf("policy DTO JSON missing key %q", key)
		}
	}
	// updated_at is omitempty — present only after a save.
	if _, ok := m["updated_at"]; ok {
		t.Error("updated_at should be omitted for zero-value UpdatedAt")
	}
}

// ---------------------------------------------------------------------------
// Tests: deliverability-floor guard (email-only with required roles)
// ---------------------------------------------------------------------------

// TestDeliverabilityFloorRejectsEmailOnly verifies that a policy that requires
// 2FA for a role AND restricts methods to email-only is rejected. This prevents
// a configuration where mail delivery failure hard-locks the required user out.
func TestDeliverabilityFloorRejectsEmailOnly(t *testing.T) {
	svc := newFakePolicyService()
	tenantID := uuid.New()
	siteID := uuid.New()
	ctx := context.Background()

	_, err := svc.saveSiteSecurityPolicy(ctx, tenantID, siteID, SiteSecurityPolicy{
		TwoFactorEnabled:       true,
		TwoFactorRequiredRoles: []string{"administrator"},
		TwoFactorMethods:       []string{"email"},
	}, "user", "u1")
	if err == nil {
		t.Fatal("want error for email-only methods with required roles, got nil")
	}
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("want domain error, got %T: %v", err, err)
	}
	if de.Code != "2fa_email_only_required" {
		t.Errorf("want code %q, got %q", "2fa_email_only_required", de.Code)
	}
	if domain.HTTPStatus(err) != http.StatusUnprocessableEntity {
		t.Errorf("want 422, got %d", domain.HTTPStatus(err))
	}
}

// TestDeliverabilityFloorAllowsEmailWithTotp verifies that email + totp together
// is accepted (totp provides a recoverable method beyond email).
func TestDeliverabilityFloorAllowsEmailWithTotp(t *testing.T) {
	svc := newFakePolicyService()
	ctx := context.Background()

	_, err := svc.saveSiteSecurityPolicy(ctx, uuid.New(), uuid.New(), SiteSecurityPolicy{
		TwoFactorEnabled:       true,
		TwoFactorRequiredRoles: []string{"administrator"},
		TwoFactorMethods:       []string{"email", "totp"},
	}, "user", "u1")
	if err != nil {
		t.Fatalf("email+totp with required roles should be accepted, got: %v", err)
	}
}

// TestDeliverabilityFloorAllowsEmailWithBackup verifies that email + backup is
// accepted (backup codes provide a recoverable non-email method).
func TestDeliverabilityFloorAllowsEmailWithBackup(t *testing.T) {
	svc := newFakePolicyService()
	ctx := context.Background()

	_, err := svc.saveSiteSecurityPolicy(ctx, uuid.New(), uuid.New(), SiteSecurityPolicy{
		TwoFactorEnabled:       true,
		TwoFactorRequiredRoles: []string{"editor"},
		TwoFactorMethods:       []string{"email", "backup"},
	}, "user", "u1")
	if err != nil {
		t.Fatalf("email+backup with required roles should be accepted, got: %v", err)
	}
}

// TestDeliverabilityFloorNoRolesNoFloor verifies that when no roles are required
// (TwoFactorRequiredRoles empty), an email-only methods list is accepted — the
// floor only applies when enforcement is required.
func TestDeliverabilityFloorNoRolesNoFloor(t *testing.T) {
	svc := newFakePolicyService()
	ctx := context.Background()

	_, err := svc.saveSiteSecurityPolicy(ctx, uuid.New(), uuid.New(), SiteSecurityPolicy{
		TwoFactorEnabled:       true,
		TwoFactorRequiredRoles: []string{}, // nobody required
		TwoFactorMethods:       []string{"email"},
	}, "user", "u1")
	if err != nil {
		t.Fatalf("email-only with no required roles should be accepted, got: %v", err)
	}
}

// TestDeliverabilityFloorEmptyMethodsNoFloor verifies that an empty methods list
// (meaning "all methods are allowed") with required roles is accepted — the floor
// only triggers when the allowed set is explicitly restricted to email only.
func TestDeliverabilityFloorEmptyMethodsNoFloor(t *testing.T) {
	svc := newFakePolicyService()
	ctx := context.Background()

	_, err := svc.saveSiteSecurityPolicy(ctx, uuid.New(), uuid.New(), SiteSecurityPolicy{
		TwoFactorEnabled:       true,
		TwoFactorRequiredRoles: []string{"administrator"},
		TwoFactorMethods:       []string{}, // empty = all methods allowed
	}, "user", "u1")
	if err != nil {
		t.Fatalf("empty methods (all-allowed) with required roles should be accepted, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tests: policy push is invoked on save (via handler + fakePolicyService)
// ---------------------------------------------------------------------------

// fakePushPolicyClient records sync_security_policy calls so the handler test
// can assert the push fires when a valid policy is saved through the HTTP layer.
type fakePushPolicyClient struct {
	calls []agentcmd.SecurityPolicyRequest
}

func (f *fakePushPolicyClient) SyncSecurityPolicy(_ context.Context, _ uuid.UUID, _ string, req agentcmd.SecurityPolicyRequest) (agentcmd.SecurityPolicyResult, error) {
	f.calls = append(f.calls, req)
	return agentcmd.SecurityPolicyResult{OK: true, Detail: "applied"}, nil
}

// fakePolicyServiceWithPush extends fakePolicyService to also call a push
// client on every save, mirroring what the real Service.SaveSiteSecurityPolicy
// does after upsert.
type fakePolicyServiceWithPush struct {
	*fakePolicyService
	pushClient *fakePushPolicyClient
}

func (s *fakePolicyServiceWithPush) saveSiteSecurityPolicy(ctx context.Context, tenantID, siteID uuid.UUID, pol SiteSecurityPolicy, actorType, actorID string) (SiteSecurityPolicy, error) {
	saved, err := s.fakePolicyService.saveSiteSecurityPolicy(ctx, tenantID, siteID, pol, actorType, actorID)
	if err != nil {
		return SiteSecurityPolicy{}, err
	}
	// Simulate the push that the real service fires.
	if s.pushClient != nil {
		req := agentcmd.SecurityPolicyRequest{
			Policy: agentcmd.SecurityPolicy{
				TwoFactorEnabled:       saved.TwoFactorEnabled,
				TwoFactorMethods:       coalesceSlice(saved.TwoFactorMethods),
				TwoFactorRequiredRoles: coalesceSlice(saved.TwoFactorRequiredRoles),
			},
			Groups: []agentcmd.SecurityPolicyGroup{},
		}
		_, _ = s.pushClient.SyncSecurityPolicy(ctx, siteID, "https://example.com", req)
	}
	return saved, nil
}

// TestPolicyPushFiredOnSave verifies that a PUT /security/policy call triggers
// the push client. The push is simulated via fakePolicyServiceWithPush so no
// DB or SSRF client is required.
func TestPolicyPushFiredOnSave(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	siteID := uuid.New()

	pushClient := &fakePushPolicyClient{}
	fSvc := &fakePolicyServiceWithPush{
		fakePolicyService: newFakePolicyService(),
		pushClient:        pushClient,
	}

	engine := gin.New()
	engine.Use(principalMiddleware(tenantID))
	engine.PUT("/sites/:siteId/security/policy", func(c *gin.Context) {
		p, _ := domain.PrincipalFromContext(c.Request.Context())
		sid, _ := uuid.Parse(c.Param("siteId"))
		var body policyDTO
		if err := bindJSON(c, &body); err != nil {
			httpx.Error(c, err)
			return
		}
		pol := fromPolicyDTO(body, p.TenantID, sid)
		saved, saveErr := fSvc.saveSiteSecurityPolicy(c.Request.Context(), p.TenantID, sid, pol, "user", "u1")
		if saveErr != nil {
			httpx.Error(c, saveErr)
			return
		}
		c.JSON(http.StatusOK, toPolicyDTO(saved))
	})

	body := `{
		"two_factor_enabled": true,
		"two_factor_methods": ["totp","backup"],
		"two_factor_required_roles": ["administrator"]
	}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut,
		"/sites/"+siteID.String()+"/security/policy",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(pushClient.calls) != 1 {
		t.Fatalf("want 1 push call after save, got %d", len(pushClient.calls))
	}
	pushed := pushClient.calls[0]
	if !pushed.Policy.TwoFactorEnabled {
		t.Error("pushed policy.two_factor_enabled should be true")
	}
	if len(pushed.Policy.TwoFactorRequiredRoles) != 1 || pushed.Policy.TwoFactorRequiredRoles[0] != "administrator" {
		t.Errorf("pushed two_factor_required_roles: want [administrator], got %v", pushed.Policy.TwoFactorRequiredRoles)
	}
}
