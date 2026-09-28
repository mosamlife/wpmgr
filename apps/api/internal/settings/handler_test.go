package settings

// handler_test.go: hermetic tests for the instance-authority gate on the SMTP
// settings routes. They drive the REAL Register wiring on a real Gin engine with
// a fake admingate.Store, so they pin the fail-closed paths a real database
// cannot be made to produce on demand: each store read erroring on its own, a
// nil store, and a non-user principal. Admission and refusal against real rows,
// through the real authentication middleware, is proven in
// tests/settings_smtp_instance_authority_integration_test.go.
//
// Every request carries a body that is not JSON. A request the gate admits
// therefore stops in the handler at 422 invalid_body without touching the
// service, and a refusal is the gate's 403. The two outcomes cannot be
// confused, and no case here needs a database.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/admingate"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

type fakeInstanceGate struct {
	superadmin    bool
	superadminErr error
	soleOwner     bool
	soleOwnerErr  error

	superadminCalls int
	soleOwnerCalls  int
}

func (f *fakeInstanceGate) IsSuperadmin(context.Context, uuid.UUID) (bool, error) {
	f.superadminCalls++
	return f.superadmin, f.superadminErr
}

func (f *fakeInstanceGate) IsSoleLiveTenantOwner(context.Context, uuid.UUID) (bool, error) {
	f.soleOwnerCalls++
	return f.soleOwner, f.soleOwnerErr
}

var _ admingate.Store = (*fakeInstanceGate)(nil)

// pastTheGateCode is what PUT and POST /test answer once the gate has let a
// request through carrying a non-JSON body.
const pastTheGateCode = "invalid_body"

// writeRoutes are the two routes whose handler can be reached without a
// service. GET is covered for refusal only: an admitted GET needs a real
// service and is proven in the integration test.
var writeRoutes = []struct{ method, path string }{
	{http.MethodPut, "/api/v1/settings/smtp"},
	{http.MethodPost, "/api/v1/settings/smtp/test"},
}

var allRoutes = append([]struct{ method, path string }{
	{http.MethodGet, "/api/v1/settings/smtp"},
}, writeRoutes...)

func gatedSettingsEngine(gate admingate.Store) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewHandler(nil, nil, gate).Register(r.Group("/api/v1"))
	return r
}

func callSettings(t *testing.T, engine *gin.Engine, method, path string, p *domain.Principal) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	if p != nil {
		req = req.WithContext(domain.WithPrincipal(req.Context(), *p))
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body.Code
}

func orgUser() *domain.Principal {
	return &domain.Principal{
		Type:     domain.PrincipalUser,
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Role:     "owner",
		Scope:    "org",
	}
}

func requireRefused(t *testing.T, engine *gin.Engine, p *domain.Principal, routes []struct{ method, path string }) {
	t.Helper()
	for _, rt := range routes {
		status, code := callSettings(t, engine, rt.method, rt.path, p)
		if status != http.StatusForbidden || code != InstanceAuthorityRequiredCode {
			t.Errorf("%s %s: got %d %q, want 403 %q", rt.method, rt.path, status, code, InstanceAuthorityRequiredCode)
		}
	}
}

func requireAdmitted(t *testing.T, engine *gin.Engine, p *domain.Principal) {
	t.Helper()
	for _, rt := range writeRoutes {
		status, code := callSettings(t, engine, rt.method, rt.path, p)
		if status != http.StatusUnprocessableEntity || code != pastTheGateCode {
			t.Errorf("%s %s: got %d %q, want 422 %q (admitted, stopped in the handler)", rt.method, rt.path, status, code, pastTheGateCode)
		}
	}
}

// Positive controls: without these, every refusal below could be a gate that
// refuses everything.
func TestSMTPGate_SuperadminAdmitted(t *testing.T) {
	gate := &fakeInstanceGate{superadmin: true}
	requireAdmitted(t, gatedSettingsEngine(gate), orgUser())
	if gate.soleOwnerCalls != 0 {
		t.Errorf("owner arm consulted %d times for a superadmin; want 0", gate.soleOwnerCalls)
	}
}

func TestSMTPGate_SoleLiveTenantOwnerAdmitted(t *testing.T) {
	requireAdmitted(t, gatedSettingsEngine(&fakeInstanceGate{soleOwner: true}), orgUser())
}

func TestSMTPGate_NeitherFactRefused(t *testing.T) {
	requireRefused(t, gatedSettingsEngine(&fakeInstanceGate{}), orgUser(), allRoutes)
}

// A failed superadmin read is a refusal, and does not fall through to the owner
// arm even when that arm would have admitted.
func TestSMTPGate_SuperadminReadErrorRefuses(t *testing.T) {
	gate := &fakeInstanceGate{superadminErr: errors.New("users read failed"), soleOwner: true}
	requireRefused(t, gatedSettingsEngine(gate), orgUser(), allRoutes)
	if gate.soleOwnerCalls != 0 {
		t.Errorf("owner arm consulted %d times after a failed superadmin read; want 0", gate.soleOwnerCalls)
	}
}

// A failed organisation-count read is a refusal, even when the value returned
// alongside the error says yes.
func TestSMTPGate_TenantCountReadErrorRefuses(t *testing.T) {
	gate := &fakeInstanceGate{soleOwner: true, soleOwnerErr: errors.New("tenant count failed")}
	requireRefused(t, gatedSettingsEngine(gate), orgUser(), allRoutes)
}

func TestSMTPGate_NilStoreRefuses(t *testing.T) {
	requireRefused(t, gatedSettingsEngine(nil), orgUser(), allRoutes)
}

// An API-key principal is refused even when both facts would admit, and the
// store is never read for it.
func TestSMTPGate_APIKeyPrincipalRefused(t *testing.T) {
	gate := &fakeInstanceGate{superadmin: true, soleOwner: true}
	p := orgUser()
	p.Type = domain.PrincipalAPIKey
	requireRefused(t, gatedSettingsEngine(gate), p, allRoutes)
	if gate.superadminCalls+gate.soleOwnerCalls != 0 {
		t.Errorf("store read %d times for an API-key principal; want 0", gate.superadminCalls+gate.soleOwnerCalls)
	}
}

// A site-scoped principal is stopped by RequireOrgScope before the instance
// gate, even when it would otherwise qualify.
func TestSMTPGate_SiteScopedPrincipalRefused(t *testing.T) {
	gate := &fakeInstanceGate{superadmin: true}
	p := orgUser()
	p.Scope = "site"
	p.AllowedSiteIDs = []uuid.UUID{uuid.New()}
	for _, rt := range allRoutes {
		status, code := callSettings(t, gatedSettingsEngine(gate), rt.method, rt.path, p)
		if status != http.StatusForbidden || code != "org_scope_required" {
			t.Errorf("%s %s: got %d %q, want 403 org_scope_required", rt.method, rt.path, status, code)
		}
	}
}
