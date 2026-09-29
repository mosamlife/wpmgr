package admingate

// install_owner_test.go: hermetic tests for ArmInstallOwner, the third arm of
// InstanceEmailAuthority. Each test names the mutation it exists to catch. The
// same arm against real rows, through the real router as wpmgr_app, is proven
// in tests/settings_smtp_instance_authority_integration_test.go.

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// installHomeTenantID is the organisation the fake names as the install
// owner's home.
var installHomeTenantID = uuid.MustParse("1a5e0000-0000-4000-8000-0000000000b1")

// selfHostedInstallOwner is a store in which the caller is the install owner
// of a self-hosted install and holds neither earlier fact.
func selfHostedInstallOwner() *fakeStore {
	return &fakeStore{selfHosted: true, installOwner: true, installHome: installHomeTenantID}
}

// Positive control: without it every refusal below could be an arm that never
// admits anyone.
func TestInstallOwnerArm_SelfHostedInstallOwnerAdmitted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tenant uuid.UUID
		home   uuid.UUID
	}{
		{"another organisation active", uuid.New(), installHomeTenantID},
		{"no active organisation", uuid.Nil, installHomeTenantID},
		{"audit copy in another owned organisation", uuid.New(), highOrgID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := selfHostedInstallOwner()
			store.installHome = tc.home
			got := InstanceEmailAuthority(withPrincipal(userPrincipal(tc.tenant)), store)
			if got.Arm != ArmInstallOwner || got.TenantID != tc.home {
				t.Fatalf("InstanceEmailAuthority = %+v, want arm %s tenant %s", got, ArmInstallOwner, tc.home)
			}
			if !CanManageInstanceEmail(withPrincipal(userPrincipal(tc.tenant)), store) {
				t.Error("CanManageInstanceEmail = false for the install owner of a self-hosted install")
			}
		})
	}
}

// lowOrgID sorts before installHomeTenantID and highOrgID after it, bytewise,
// so a rule that ignored the home preference, or picked by anything but the
// lowest id, names the wrong one.
var (
	lowOrgID  = uuid.MustParse("0a000000-0000-4000-8000-0000000000b2")
	highOrgID = uuid.MustParse("fa000000-0000-4000-8000-0000000000b3")
)

// installOwnerAuditTenant is the rule; this table is its contract.
func TestInstallOwnerAuditTenant_Rule(t *testing.T) {
	home := installHomeTenantID
	for _, tc := range []struct {
		name  string
		owned []ownedOrganisation
		want  uuid.UUID
	}{
		{"no owner membership", nil, uuid.Nil},
		{"owner membership only in a deleted organisation", []ownedOrganisation{{lowOrgID, false}}, uuid.Nil},
		{"owns only the home organisation, which is deleted", []ownedOrganisation{{home, false}}, uuid.Nil},
		{"still owns the live home organisation", []ownedOrganisation{{home, true}}, home},
		{"home preferred over a lower-id owned organisation", []ownedOrganisation{{lowOrgID, true}, {home, true}, {highOrgID, true}}, home},
		{"owns another organisation only", []ownedOrganisation{{highOrgID, true}}, highOrgID},
		{"home deleted, lowest live id wins", []ownedOrganisation{{highOrgID, true}, {home, false}, {lowOrgID, true}}, lowOrgID},
		{"deleted lower id is skipped", []ownedOrganisation{{lowOrgID, false}, {highOrgID, true}}, highOrgID},
		{"a nil id is never chosen", []ownedOrganisation{{uuid.Nil, true}}, uuid.Nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := installOwnerAuditTenant(home, tc.owned); got != tc.want {
				t.Fatalf("installOwnerAuditTenant = %s, want %s", got, tc.want)
			}
		})
	}
}

// The rule and the gate together, with only the SQL read faked: the four
// cases the arm turns on. Catches granting the arm when the install owner
// owns no live organisation (a NULL audit organisation), and catches the
// audit copy going anywhere but the organisation the rule names.
func TestInstallOwnerArm_RequiresALiveOwnedOrganisation(t *testing.T) {
	home := installHomeTenantID
	for _, tc := range []struct {
		name       string
		owned      []ownedOrganisation
		wantArm    Arm
		wantTenant uuid.UUID
	}{
		{"no owner membership is refused", nil, ArmNone, uuid.Nil},
		{"owner membership only in a deleted organisation is refused", []ownedOrganisation{{lowOrgID, false}, {home, false}}, ArmNone, uuid.Nil},
		{"still owning the home organisation is admitted, audit copy there", []ownedOrganisation{{lowOrgID, true}, {home, true}}, ArmInstallOwner, home},
		{"owning another organisation only is admitted, audit copy there", []ownedOrganisation{{highOrgID, true}}, ArmInstallOwner, highOrgID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := selfHostedInstallOwner()
			store.installHome = installOwnerAuditTenant(home, tc.owned)
			got := InstanceEmailAuthority(withPrincipal(userPrincipal(uuid.New())), store)
			if got.Arm != tc.wantArm || got.TenantID != tc.wantTenant {
				t.Fatalf("InstanceEmailAuthority = %+v, want arm %s tenant %s", got, tc.wantArm, tc.wantTenant)
			}
			if store.installOwnerCalls != 1 {
				t.Errorf("install-owner read %d times; want 1", store.installOwnerCalls)
			}
		})
	}
}

// The store reports the install owner but names no organisation: refused.
// This is the gate's own check, independent of the rule above.
func TestInstallOwnerArm_NilAuditOrganisationRefused(t *testing.T) {
	store := selfHostedInstallOwner()
	store.installHome = uuid.Nil
	ctx := withPrincipal(userPrincipal(uuid.New()))
	if got := InstanceEmailAuthority(ctx, store); got.Admitted() {
		t.Fatalf("install owner with no live owned organisation admitted as %s (tenant %s); want ArmNone", got.Arm, got.TenantID)
	}
	if CanManageInstanceEmail(ctx, store) {
		t.Error("CanManageInstanceEmail = true for an install owner with no live owned organisation")
	}
}

// U1. Catches removing the SelfHosted check: on hosted the arm admits nobody,
// and is not even read.
func TestInstallOwnerArm_HostedRefuses(t *testing.T) {
	store := selfHostedInstallOwner()
	store.selfHosted = false
	got := InstanceEmailAuthority(withPrincipal(userPrincipal(uuid.New())), store)
	if got.Admitted() {
		t.Fatalf("hosted install admitted the install owner as %s; want ArmNone", got.Arm)
	}
	if store.installOwnerCalls != 0 {
		t.Errorf("install-owner read %d times on a hosted install; want 0", store.installOwnerCalls)
	}
}

// U2. Catches moving the new arm ahead of the existing two: a caller admitted
// today keeps the arm (and so the audit routing) it had.
func TestInstallOwnerArm_ExistingArmsDecideFirst(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func(*fakeStore)
		wantArm    Arm
		wantTenant uuid.UUID
	}{
		{"superadmin who is also the install owner", func(f *fakeStore) { f.superadmin = true }, ArmSuperadmin, uuid.Nil},
		{"sole owner who is also the install owner", func(f *fakeStore) { f.soleOwner = true }, ArmSoleLiveTenantOwner, soleTenantID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := selfHostedInstallOwner()
			tc.mutate(store)
			got := InstanceEmailAuthority(withPrincipal(userPrincipal(uuid.New())), store)
			if got.Arm != tc.wantArm || got.TenantID != tc.wantTenant {
				t.Fatalf("InstanceEmailAuthority = %+v, want arm %s tenant %s", got, tc.wantArm, tc.wantTenant)
			}
			if store.installOwnerCalls != 0 {
				t.Errorf("install-owner read %d times for a caller an earlier arm admitted; want 0", store.installOwnerCalls)
			}
		})
	}
}

// U3. Catches collapsing a failed read into "not admitted" and falling through
// to the new arm (for instance by calling ResolveInstanceAuthority, which maps
// an error to ArmNone).
func TestInstallOwnerArm_EarlierReadErrorDoesNotFallThrough(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*fakeStore)
	}{
		{"superadmin read error", func(f *fakeStore) { f.superadminErr = errors.New("users read failed") }},
		{"sole owner read error", func(f *fakeStore) { f.soleOwnerErr = errors.New("tenant count failed") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := selfHostedInstallOwner()
			tc.mutate(store)
			if got := InstanceEmailAuthority(withPrincipal(userPrincipal(uuid.New())), store); got.Admitted() {
				t.Fatalf("admitted as %s after a failed read; want ArmNone", got.Arm)
			}
			if store.installOwnerCalls != 0 {
				t.Errorf("install-owner read %d times after a failed earlier read; want 0", store.installOwnerCalls)
			}
		})
	}
}

// U4. Catches dropping the PrincipalUser check from InstanceEmailAuthority: an
// API key whose UserID is the install owner is refused, and no fact is read.
func TestInstallOwnerArm_APIKeyRefused(t *testing.T) {
	store := selfHostedInstallOwner()
	p := userPrincipal(uuid.New())
	p.Type = domain.PrincipalAPIKey
	p.APIKeyID = uuid.New()
	if got := InstanceEmailAuthority(withPrincipal(p), store); got.Admitted() {
		t.Fatalf("API key of the install owner admitted as %s; want ArmNone", got.Arm)
	}
	if store.installOwnerCalls != 0 || store.calls != 0 {
		t.Errorf("store read %d times (install-owner %d) for an API key; want 0", store.calls, store.installOwnerCalls)
	}
}

// U5. Catches dropping IsSiteConstrained: a site-constrained install owner is
// refused before any read.
func TestInstallOwnerArm_SiteConstrainedRefused(t *testing.T) {
	siteScoped := userPrincipal(uuid.New())
	siteScoped.Scope = domain.ScopeSite
	siteScoped.AllowedSiteIDs = []uuid.UUID{uuid.New()}
	allowlistOnly := userPrincipal(uuid.New())
	allowlistOnly.Scope = ""
	allowlistOnly.AllowedSiteIDs = []uuid.UUID{uuid.New()}
	for name, p := range map[string]domain.Principal{"site scope": siteScoped, "allowlist only": allowlistOnly} {
		t.Run(name, func(t *testing.T) {
			store := selfHostedInstallOwner()
			if got := InstanceEmailAuthority(withPrincipal(p), store); got.Admitted() {
				t.Fatalf("site-constrained install owner admitted as %s; want ArmNone", got.Arm)
			}
			if store.calls != 0 {
				t.Errorf("store read %d times for a site-constrained principal; want 0", store.calls)
			}
		})
	}
}

// U6. A failed install-owner read refuses.
func TestInstallOwnerArm_ReadErrorRefuses(t *testing.T) {
	store := selfHostedInstallOwner()
	store.installOwnerErr = errors.New("install_owner read failed")
	if got := InstanceEmailAuthority(withPrincipal(userPrincipal(uuid.New())), store); got.Admitted() {
		t.Fatalf("admitted as %s after a failed install-owner read; want ArmNone", got.Arm)
	}
	if store.installOwnerCalls != 1 {
		t.Errorf("install-owner read %d times; want 1 (the error must come from the read, not a skip)", store.installOwnerCalls)
	}
}

// Not the install owner, on a self-hosted install, holding neither earlier
// fact: refused.
func TestInstallOwnerArm_OtherUserRefused(t *testing.T) {
	store := &fakeStore{selfHosted: true}
	if got := InstanceEmailAuthority(withPrincipal(userPrincipal(uuid.New())), store); got.Admitted() {
		t.Fatalf("a user who is not the install owner was admitted as %s", got.Arm)
	}
	if store.installOwnerCalls != 1 {
		t.Errorf("install-owner read %d times; want 1", store.installOwnerCalls)
	}
}

// U7. Catches adding the arm to ResolveInstanceAuthority: the agent-mirror
// check, which holds only a Store, still refuses the self-hosted install owner.
func TestInstallOwnerArm_AgentMirrorCheckUnchanged(t *testing.T) {
	store := selfHostedInstallOwner()
	ctx := withPrincipal(userPrincipal(uuid.New()))
	if CanRunAgentMirrorCheck(ctx, store) {
		t.Fatal("CanRunAgentMirrorCheck admitted the install owner; the ruling covers instance email only")
	}
	if got := ResolveInstanceAuthority(ctx, store); got.Admitted() {
		t.Fatalf("ResolveInstanceAuthority admitted the install owner as %s", got.Arm)
	}
	if store.installOwnerCalls != 0 {
		t.Errorf("install-owner read %d times by the agent-mirror decision; want 0", store.installOwnerCalls)
	}
}

// resolveInstanceAuthority keeps a failed read apart from a refusal; the
// exported wrapper maps both to ArmNone.
func TestResolveInstanceAuthority_ErrorIsNotARefusal(t *testing.T) {
	ctx := withPrincipal(userPrincipal(uuid.New()))
	if _, err := resolveInstanceAuthority(ctx, &fakeStore{}); err != nil {
		t.Errorf("neither fact: err = %v, want nil", err)
	}
	for name, store := range map[string]*fakeStore{
		"superadmin": {superadminErr: errors.New("boom")},
		"sole owner": {soleOwnerErr: errors.New("boom")},
	} {
		a, err := resolveInstanceAuthority(ctx, store)
		if err == nil || !errors.Is(err, errStoreRead) || a.Admitted() {
			t.Errorf("%s read error: got %+v, %v; want ArmNone with errStoreRead", name, a, err)
		}
		if got := ResolveInstanceAuthority(ctx, store); got.Admitted() {
			t.Errorf("%s read error: wrapper admitted as %s", name, got.Arm)
		}
	}
}

// A nil store is refused before anything else.
func TestInstallOwnerArm_NilStore(t *testing.T) {
	var store InstanceEmailStore
	if got := InstanceEmailAuthority(withPrincipal(userPrincipal(uuid.New())), store); got.Admitted() {
		t.Fatalf("nil store admitted as %s", got.Arm)
	}
}

// The zero value of the production store is hosted: the arm is off unless the
// constructor was told otherwise.
func TestInstanceEmailPoolStore_ZeroValueIsHosted(t *testing.T) {
	if (InstanceEmailPoolStore{}).SelfHosted() {
		t.Error("zero InstanceEmailPoolStore reports self-hosted; want hosted (arm off)")
	}
	if !NewInstanceEmailPoolStore(nil, false).SelfHosted() {
		t.Error("NewInstanceEmailPoolStore(hosted=false) reports hosted")
	}
	if NewInstanceEmailPoolStore(nil, true).SelfHosted() {
		t.Error("NewInstanceEmailPoolStore(hosted=true) reports self-hosted")
	}
}

// The names are written into audit rows, so they are pinned.
func TestArmString(t *testing.T) {
	for arm, want := range map[Arm]string{
		ArmNone:                "none",
		ArmSuperadmin:          "superadmin",
		ArmSoleLiveTenantOwner: "sole_live_tenant_owner",
		ArmInstallOwner:        "install_owner",
		Arm(99):                "arm(99)",
	} {
		if got := arm.String(); got != want {
			t.Errorf("Arm(%d).String() = %q, want %q", int(arm), got, want)
		}
	}
}
