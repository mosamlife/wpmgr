package settings

// handler_install_owner_test.go: the SMTP settings routes and their audit
// routing for a caller admitted as the install owner (admingate.ArmInstallOwner).
// The gate is a fake here; the same arm against real rows, through the real
// router as wpmgr_app, is proven in
// tests/settings_smtp_instance_authority_integration_test.go.

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/admingate"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// installHomeID is the organisation the fake gate names as the install owner's
// home.
var installHomeID = uuid.MustParse("1a5e0000-0000-4000-8000-0000000000c1")

func installOwnerGate(home uuid.UUID) *fakeInstanceGate {
	return &fakeInstanceGate{selfHosted: true, installOwner: true, installHome: home}
}

func TestSMTPGate_InstallOwnerAdmittedOnSelfHosted(t *testing.T) {
	gate := installOwnerGate(installHomeID)
	requireAdmitted(t, gatedSettingsEngine(gate), orgUser())
	if gate.installOwnerCalls == 0 {
		t.Error("admitted without reading the install-owner fact")
	}
}

func TestSMTPGate_InstallOwnerRefusedOnHosted(t *testing.T) {
	gate := installOwnerGate(installHomeID)
	gate.selfHosted = false
	requireRefused(t, gatedSettingsEngine(gate), orgUser(), allRoutes)
	if gate.installOwnerCalls != 0 {
		t.Errorf("install-owner read %d times on a hosted install; want 0", gate.installOwnerCalls)
	}
}

func TestSMTPGate_InstallOwnerAPIKeyRefused(t *testing.T) {
	gate := installOwnerGate(installHomeID)
	p := orgUser()
	p.Type = domain.PrincipalAPIKey
	requireRefused(t, gatedSettingsEngine(gate), p, allRoutes)
	if gate.installOwnerCalls != 0 {
		t.Errorf("install-owner read %d times for an API key; want 0", gate.installOwnerCalls)
	}
}

// The install owner's change is recorded in the instance trail and in the
// home organisation named by the decision, never in the one the request names.
// Catches the organisation copy staying sole-owner-only, and catches routing it
// by the request's tenant.
func TestSMTPPutAudit_InstallOwnerRecordsHomeOrganisation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tenant uuid.UUID
	}{
		{"with the request naming another organisation", uuid.New()},
		{"with no active organisation", uuid.Nil},
		{"with the home organisation active", installHomeID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, rec := &fakeSMTPService{}, &fakeTenantRecorder{}
			e := auditEngine(installOwnerGate(installHomeID), svc, rec, nil)
			p := principalWithTenant(tc.tenant)
			if status := putAs(t, e, p); status != http.StatusOK {
				t.Fatalf("PUT: got %d, want 200", status)
			}
			requireInstanceEvent(t, svc, p.UserID)
			if got := svc.instanceEvents[0].meta["admitted_as"]; got != admingate.ArmInstallOwner.String() {
				t.Errorf("instance trail admitted_as = %v, want %q", got, admingate.ArmInstallOwner.String())
			}
			if len(rec.events) != 1 {
				t.Fatalf("organisation audit records = %d, want 1", len(rec.events))
			}
			got := rec.events[0]
			if got.TenantID != installHomeID {
				t.Errorf("organisation audit record went to %s, want the home organisation %s", got.TenantID, installHomeID)
			}
			if got.Metadata["admitted_as"] != admingate.ArmInstallOwner.String() {
				t.Errorf("organisation record admitted_as = %v, want %q", got.Metadata["admitted_as"], admingate.ArmInstallOwner.String())
			}
		})
	}
}

// An install owner who has lost the home organisation but still owns another
// live one is admitted, and the organisation copy goes to the one the gate
// names, never to the request's active organisation.
func TestSMTPPutAudit_InstallOwnerRecordsTheOwnedOrganisationTheGateNames(t *testing.T) {
	other := uuid.MustParse("fa000000-0000-4000-8000-0000000000c2")
	svc, rec := &fakeSMTPService{}, &fakeTenantRecorder{}
	e := auditEngine(installOwnerGate(other), svc, rec, nil)
	p := principalWithTenant(uuid.New())
	if status := putAs(t, e, p); status != http.StatusOK {
		t.Fatalf("PUT: got %d, want 200", status)
	}
	requireInstanceEvent(t, svc, p.UserID)
	if len(rec.events) != 1 || rec.events[0].TenantID != other {
		t.Fatalf("organisation audit records = %+v, want exactly one in %s", rec.events, other)
	}
}

// An install owner who owns no live organisation (removed from every one, or
// owning only soft-deleted ones) is refused on every route, and nothing is
// written or recorded. Catches granting the arm with no organisation to
// record the change in.
func TestSMTPGate_InstallOwnerOwningNoLiveOrganisationRefused(t *testing.T) {
	// The PUT runs first, against an engine with a working service, so an arm
	// that wrongly admits fails here as an assertion rather than reaching the
	// service-less engine below.
	svc, rec := &fakeSMTPService{}, &fakeTenantRecorder{}
	e := auditEngine(installOwnerGate(uuid.Nil), svc, rec, nil)
	if status := putAs(t, e, principalWithTenant(uuid.New())); status != http.StatusForbidden {
		t.Fatalf("PUT: got %d, want 403", status)
	}
	if len(svc.instanceEvents) != 0 || len(rec.events) != 0 {
		t.Errorf("refused PUT recorded instance %d, organisation %d events; want 0 and 0", len(svc.instanceEvents), len(rec.events))
	}

	gate := installOwnerGate(uuid.Nil)
	requireRefused(t, gatedSettingsEngine(gate), orgUser(), allRoutes)
	if gate.installOwnerCalls == 0 {
		t.Error("refused without reading the install-owner fact; the refusal must come from the read")
	}
}

// admitted_as tells the arms apart in the instance trail.
func TestSMTPPutAudit_AdmittedAsNamesTheArm(t *testing.T) {
	for _, tc := range []struct {
		name string
		gate *fakeInstanceGate
		want admingate.Arm
	}{
		{"superadmin", &fakeInstanceGate{superadmin: true}, admingate.ArmSuperadmin},
		{"sole owner", &fakeInstanceGate{soleOwner: true}, admingate.ArmSoleLiveTenantOwner},
		{"install owner", installOwnerGate(installHomeID), admingate.ArmInstallOwner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, rec := &fakeSMTPService{}, &fakeTenantRecorder{}
			e := auditEngine(tc.gate, svc, rec, nil)
			p := principalWithTenant(uuid.New())
			if status := putAs(t, e, p); status != http.StatusOK {
				t.Fatalf("PUT: got %d, want 200", status)
			}
			requireInstanceEvent(t, svc, p.UserID)
			if got := svc.instanceEvents[0].meta["admitted_as"]; got != tc.want.String() {
				t.Errorf("admitted_as = %v, want %q", got, tc.want.String())
			}
		})
	}
}
