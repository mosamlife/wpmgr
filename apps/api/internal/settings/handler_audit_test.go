package settings

// handler_audit_test.go: where a successful PUT is recorded. Every admitted
// PUT is recorded in the instance trail (system_audit_log, through
// Service.RecordInstanceEvent). A caller admitted as owner of the only live
// organisation is also recorded in that organisation's audit_log, the
// organisation the admitting decision named, never the request's active one.
// A superadmin is recorded in the instance trail only. The service and the
// organisation recorder are fakes here; the same routing against real rows is
// proven in tests/settings_smtp_instance_authority_integration_test.go.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/admingate"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

type instanceEvent struct {
	actor  uuid.UUID
	action string
}

type fakeSMTPService struct {
	updateCalls    int
	instanceEvents []instanceEvent
}

func (f *fakeSMTPService) Get(context.Context) (SMTPSettings, error) { return SMTPSettings{}, nil }

func (f *fakeSMTPService) Update(_ context.Context, in SMTPUpdate, _ uuid.UUID) (SMTPSettings, error) {
	f.updateCalls++
	return SMTPSettings{Enabled: in.Enabled, Host: in.Host, TLSMode: in.TLSMode}, nil
}

func (f *fakeSMTPService) SendTest(context.Context, string) error { return nil }

func (f *fakeSMTPService) RecordInstanceEvent(_ context.Context, actorID uuid.UUID, action string, _ map[string]any) {
	f.instanceEvents = append(f.instanceEvents, instanceEvent{actor: actorID, action: action})
}

type fakeTenantRecorder struct {
	err    error
	events []audit.Event
}

func (f *fakeTenantRecorder) Record(_ context.Context, e audit.Event) (audit.Entry, error) {
	f.events = append(f.events, e)
	return audit.Entry{}, f.err
}

const auditPutBody = `{"enabled":false,"host":"relay.example.test","port":587,"tls_mode":"starttls"}`

func auditEngine(gate admingate.Store, svc *fakeSMTPService, rec *fakeTenantRecorder, log *slog.Logger) *gin.Engine {
	gin.SetMode(gin.TestMode)
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	r := gin.New()
	h := &Handler{svc: svc, audit: rec, gate: gate, log: log}
	h.Register(r.Group("/api/v1"))
	return r
}

func putAs(t *testing.T, e *gin.Engine, p domain.Principal) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/smtp", strings.NewReader(auditPutBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(domain.WithPrincipal(req.Context(), p))
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w.Code
}

func principalWithTenant(tenant uuid.UUID) domain.Principal {
	p := domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenant}
	if tenant != uuid.Nil {
		p.Role = "viewer"
		p.Scope = domain.ScopeOrg
	}
	return p
}

func requireInstanceEvent(t *testing.T, svc *fakeSMTPService, actor uuid.UUID) {
	t.Helper()
	if len(svc.instanceEvents) != 1 {
		t.Fatalf("instance trail records = %d, want 1", len(svc.instanceEvents))
	}
	if got := svc.instanceEvents[0]; got.actor != actor || got.action != AuditActionUpdate {
		t.Errorf("instance trail record = %+v, want actor %s action %q", got, actor, AuditActionUpdate)
	}
}

// A superadmin's change is recorded in the instance trail and in no
// organisation's audit_log, including the one the request has active.
func TestSMTPPutAudit_SuperadminRecordsInstanceTrailOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tenant uuid.UUID
	}{
		{"with an active organisation", uuid.New()},
		{"with no active organisation", uuid.Nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, rec := &fakeSMTPService{}, &fakeTenantRecorder{}
			// soleOwner is also true: the superadmin arm decides first, and its
			// record must not follow the owner arm's.
			e := auditEngine(&fakeInstanceGate{superadmin: true, soleOwner: true}, svc, rec, nil)
			p := principalWithTenant(tc.tenant)
			if status := putAs(t, e, p); status != http.StatusOK {
				t.Fatalf("PUT: got %d, want 200", status)
			}
			requireInstanceEvent(t, svc, p.UserID)
			if len(rec.events) != 0 {
				t.Errorf("organisation audit records = %d (%+v), want 0 for a superadmin", len(rec.events), rec.events)
			}
		})
	}
}

// The owner of the only live organisation is recorded in the instance trail
// and in that organisation's audit_log. The organisation is the one the
// admitting decision named, whatever the request has active.
func TestSMTPPutAudit_SoleOwnerRecordsBothTrails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tenant uuid.UUID
	}{
		{"with the request naming another organisation", uuid.New()},
		{"with no active organisation", uuid.Nil},
		{"with the sole organisation active", gateSoleTenantID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, rec := &fakeSMTPService{}, &fakeTenantRecorder{}
			e := auditEngine(&fakeInstanceGate{soleOwner: true}, svc, rec, nil)
			p := principalWithTenant(tc.tenant)
			if status := putAs(t, e, p); status != http.StatusOK {
				t.Fatalf("PUT: got %d, want 200", status)
			}
			requireInstanceEvent(t, svc, p.UserID)
			if len(rec.events) != 1 {
				t.Fatalf("organisation audit records = %d, want 1", len(rec.events))
			}
			got := rec.events[0]
			if got.TenantID != gateSoleTenantID {
				t.Errorf("organisation audit record went to %s, want the sole organisation %s", got.TenantID, gateSoleTenantID)
			}
			if got.Action != AuditActionUpdate || got.ActorType != audit.ActorUser || got.ActorID != p.UserID.String() {
				t.Errorf("organisation audit record = %+v, want action %q by user %s", got, AuditActionUpdate, p.UserID)
			}
		})
	}
}

// A failed organisation append does not fail the change, is logged, and does
// not cost the instance trail its record.
func TestSMTPPutAudit_FailedOrganisationAppendIsLogged(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	svc, rec := &fakeSMTPService{}, &fakeTenantRecorder{err: errors.New("audit append failed")}
	e := auditEngine(&fakeInstanceGate{soleOwner: true}, svc, rec, log)
	p := principalWithTenant(uuid.Nil)
	if status := putAs(t, e, p); status != http.StatusOK {
		t.Fatalf("PUT: got %d, want 200", status)
	}
	requireInstanceEvent(t, svc, p.UserID)
	if out := buf.String(); !strings.Contains(out, "organisation audit record failed") || !strings.Contains(out, gateSoleTenantID.String()) {
		t.Errorf("failed organisation append was not logged; log output: %q", out)
	}
}

// A refused PUT writes nothing and records nothing.
func TestSMTPPutAudit_RefusedRecordsNothing(t *testing.T) {
	svc, rec := &fakeSMTPService{}, &fakeTenantRecorder{}
	e := auditEngine(&fakeInstanceGate{}, svc, rec, nil)
	if status := putAs(t, e, principalWithTenant(uuid.Nil)); status != http.StatusForbidden {
		t.Fatalf("PUT: got %d, want 403", status)
	}
	if svc.updateCalls != 0 || len(svc.instanceEvents) != 0 || len(rec.events) != 0 {
		t.Errorf("refused PUT: updates=%d instance records=%d organisation records=%d, want 0 each",
			svc.updateCalls, len(svc.instanceEvents), len(rec.events))
	}
}

// The handler refuses a PUT that reaches it without the instance gate's
// decision, rather than writing a change it cannot route to a trail.
func TestSMTPPutAudit_WithoutGateDecisionRefused(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, rec := &fakeSMTPService{}, &fakeTenantRecorder{}
	h := &Handler{svc: svc, audit: rec, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := gin.New()
	r.PUT("/api/v1/settings/smtp", h.put)
	if status := putAs(t, r, principalWithTenant(uuid.New())); status != http.StatusForbidden {
		t.Fatalf("PUT without the gate: got %d, want 403", status)
	}
	if svc.updateCalls != 0 {
		t.Errorf("PUT without the gate reached the service %d times, want 0", svc.updateCalls)
	}
}
