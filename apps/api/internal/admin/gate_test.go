package admin

// gate_test.go: hermetic tests for the two /admin route gates (GH #322).
//
// These drive the REAL Register wiring on a real Gin engine with a fake
// adminGateStore, so they prove which route got the widened gate and which
// routes did not. The SQL behind adminGateStore (tenant counting, the owner
// role, soft-deleted organisations) is a separate question and is proven
// against a real Postgres in gate_integration_test.go; a fake cannot answer it.
//
// Which of these fail against the PRE-change gate (agent-mirror/check mounted
// on the requireSuperadmin group like every other admin route):
//
//	TestAgentMirrorGate_SoleTenantOwnerAllowed          FAILS pre-change (403, gate refused)
//	TestAgentMirrorGate_SoleTenantOwnerReachesHandler   FAILS pre-change (403, never reached the handler)
//	everything else                                     passes either way, by design:
//	  they pin behaviour that must NOT have changed (superadmin still in,
//	  non-owner out, API key out, DB error out, other admin routes untouched).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// fakeGateStore is an adminGateStore test double. Both facts are set
// independently so a test can assert the gate reads the RIGHT one.
type fakeGateStore struct {
	superadmin    bool
	superadminErr error
	soleOwner     bool
	soleOwnerErr  error

	superadminCalls int
	soleOwnerCalls  int
}

func (f *fakeGateStore) IsSuperadmin(context.Context, uuid.UUID) (bool, error) {
	f.superadminCalls++
	return f.superadmin, f.superadminErr
}

func (f *fakeGateStore) IsSoleLiveTenantOwner(context.Context, uuid.UUID) (bool, error) {
	f.soleOwnerCalls++
	return f.soleOwner, f.soleOwnerErr
}

const (
	// mirrorCheckPath is the ONE route that carries the widened gate.
	mirrorCheckPath = "/api/v1/admin/agent-mirror/check"
	// gateRefusedCode is the single refusal code every gate path emits. It must
	// stay identical on the widened path so a refusal cannot be used to probe
	// how many organisations this install has.
	gateRefusedCode = "superadmin_required"
	// pastTheGateCode is what the agent-mirror handler returns once the gate has
	// let the request through, with the service wired disabled in these tests.
	// Seeing it is proof the request reached the handler; it is not 403, and it
	// is not a 404 from a route that was never mounted.
	pastTheGateCode = "agent_mirror_disabled"
)

// newGatedEngine mounts the REAL admin routes (h.Register) with the given fake
// gate store, exactly as internal/server/server.go mounts them on v1Auth.
func newGatedEngine(t *testing.T, store adminGateStore) *gin.Engine {
	t.Helper()
	h := NewHandler(nil, nil)
	h.gate = store
	// Mirrors cmd/wpmgr/main.go: without SetAgentMirror the route is not
	// mounted at all. Disabled+unwired, so the handler refuses with
	// pastTheGateCode rather than touching River or object storage.
	h.SetAgentMirror(NewAgentMirrorCheckService(false, false, nil, nil))

	r := gin.New()
	h.Register(r.Group("/api/v1"))
	return r
}

// callAs issues a request carrying the given principal (nil for none) and
// returns the status plus the parsed error code ("" when the body is not an
// error envelope).
func callAs(t *testing.T, engine *gin.Engine, method, path string, p *domain.Principal) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
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

func userPrincipal() *domain.Principal {
	return &domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New()}
}

// --- T1: superadmin still allowed ------------------------------------------

// TestAgentMirrorGate_SuperadminAllowed pins the unchanged half of the change.
// A superadmin must still reach the handler, and the widened arm must not even
// be consulted for them.
func TestAgentMirrorGate_SuperadminAllowed(t *testing.T) {
	store := &fakeGateStore{superadmin: true, soleOwner: false}
	status, code := callAs(t, newGatedEngine(t, store), http.MethodPost, mirrorCheckPath, userPrincipal())

	if status == http.StatusForbidden {
		t.Fatalf("superadmin was refused (%d %s); this behaviour must not have changed", status, code)
	}
	if code != pastTheGateCode {
		t.Fatalf("code = %q, want %q (the request must reach the handler)", code, pastTheGateCode)
	}
	if store.soleOwnerCalls != 0 {
		t.Fatalf("sole-owner lookup ran %d times for a superadmin; want 0", store.soleOwnerCalls)
	}
}

// --- T2: owner of the ONLY tenant is allowed -------------------------------

// TestAgentMirrorGate_SoleTenantOwnerAllowed is the whole point of GH #322.
// FAILS against the pre-change gate: there, a non-superadmin gets 403 here.
func TestAgentMirrorGate_SoleTenantOwnerAllowed(t *testing.T) {
	store := &fakeGateStore{superadmin: false, soleOwner: true}
	status, code := callAs(t, newGatedEngine(t, store), http.MethodPost, mirrorCheckPath, userPrincipal())

	if status == http.StatusForbidden {
		t.Fatalf("owner of the only organisation was refused (%d %s); GH #322 requires this to pass", status, code)
	}
	if store.soleOwnerCalls != 1 {
		t.Fatalf("sole-owner lookup ran %d times; want exactly 1 (read at request time, never cached)", store.soleOwnerCalls)
	}
}

// TestAgentMirrorGate_SoleTenantOwnerReachesHandler is the same allow-path
// asserted on the response the HANDLER produces rather than on the absence of a
// 403, so a future refusal that happened to use a different status could not
// quietly satisfy the test above.
// FAILS against the pre-change gate.
func TestAgentMirrorGate_SoleTenantOwnerReachesHandler(t *testing.T) {
	store := &fakeGateStore{superadmin: false, soleOwner: true}
	_, code := callAs(t, newGatedEngine(t, store), http.MethodPost, mirrorCheckPath, userPrincipal())

	if code != pastTheGateCode {
		t.Fatalf("code = %q, want %q (the owner's request must reach the handler)", code, pastTheGateCode)
	}
}

// --- T3 + T4: refused when either half of the condition is false -----------

// TestAgentMirrorGate_RefusedWhenNotSoleOwner covers both the "owner of one of
// several organisations" and the "member but not owner" shapes at the gate
// layer: whatever the reason, the store answers false and the gate refuses.
// Which inputs make the store answer false is proven in
// gate_integration_test.go against real rows.
func TestAgentMirrorGate_RefusedWhenNotSoleOwner(t *testing.T) {
	store := &fakeGateStore{superadmin: false, soleOwner: false}
	status, code := callAs(t, newGatedEngine(t, store), http.MethodPost, mirrorCheckPath, userPrincipal())

	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if code != gateRefusedCode {
		t.Fatalf("code = %q, want %q (a refusal must not say why)", code, gateRefusedCode)
	}
}

// --- T5: API-key principal is refused --------------------------------------

// TestAgentMirrorGate_APIKeyPrincipalRefused pins that an API key never takes
// the widened path, even on a single-organisation install where the key's own
// tenant is that organisation. The gate must not perform either lookup.
func TestAgentMirrorGate_APIKeyPrincipalRefused(t *testing.T) {
	store := &fakeGateStore{superadmin: true, soleOwner: true} // both true: only the principal type may refuse
	p := &domain.Principal{Type: domain.PrincipalAPIKey, APIKeyID: uuid.New(), TenantID: uuid.New()}
	status, code := callAs(t, newGatedEngine(t, store), http.MethodPost, mirrorCheckPath, p)

	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for an API-key principal", status)
	}
	if code != gateRefusedCode {
		t.Fatalf("code = %q, want %q", code, gateRefusedCode)
	}
	if store.superadminCalls != 0 || store.soleOwnerCalls != 0 {
		t.Fatalf("gate hit the store (%d superadmin, %d sole-owner) for a non-user principal; want 0 and 0",
			store.superadminCalls, store.soleOwnerCalls)
	}
}

// TestAgentMirrorGate_NoPrincipalRefused: an unauthenticated request never
// reaches either lookup.
func TestAgentMirrorGate_NoPrincipalRefused(t *testing.T) {
	store := &fakeGateStore{superadmin: true, soleOwner: true}
	status, code := callAs(t, newGatedEngine(t, store), http.MethodPost, mirrorCheckPath, nil)

	if status != http.StatusForbidden || code != gateRefusedCode {
		t.Fatalf("status/code = %d/%q, want 403/%q", status, code, gateRefusedCode)
	}
	if store.superadminCalls != 0 || store.soleOwnerCalls != 0 {
		t.Fatal("gate hit the store without a principal")
	}
}

// --- T7: fail closed on a DB error -----------------------------------------

// TestAgentMirrorGate_SoleOwnerReadErrorRefuses: the count/ownership read
// failing is a refusal, never an allow. This is the one that turns a database
// blip into a 403 instead of into an open door.
func TestAgentMirrorGate_SoleOwnerReadErrorRefuses(t *testing.T) {
	store := &fakeGateStore{
		superadmin:   false,
		soleOwner:    true, // would allow, but the error must win
		soleOwnerErr: errors.New("connection reset"),
	}
	status, code := callAs(t, newGatedEngine(t, store), http.MethodPost, mirrorCheckPath, userPrincipal())

	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 when the tenant-count read fails", status)
	}
	if code != gateRefusedCode {
		t.Fatalf("code = %q, want %q", code, gateRefusedCode)
	}
}

// TestAgentMirrorGate_SuperadminReadErrorRefuses: the users read failing is
// also a refusal, and must NOT fall through to the widened arm. Falling through
// would mean a broken users table silently downgraded this route's gate.
func TestAgentMirrorGate_SuperadminReadErrorRefuses(t *testing.T) {
	store := &fakeGateStore{
		superadminErr: errors.New("connection reset"),
		soleOwner:     true, // would allow if the gate wrongly fell through
	}
	status, code := callAs(t, newGatedEngine(t, store), http.MethodPost, mirrorCheckPath, userPrincipal())

	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 when the is_superadmin read fails", status)
	}
	if code != gateRefusedCode {
		t.Fatalf("code = %q, want %q", code, gateRefusedCode)
	}
	if store.soleOwnerCalls != 0 {
		t.Fatalf("gate consulted the widened arm after an is_superadmin read error; want 0 calls, got %d",
			store.soleOwnerCalls)
	}
}

// --- T8: no OTHER admin route gained the widened path ----------------------

// TestOtherAdminRoutes_DidNotGainTheWidenedPath is the containment test. The
// same principal that IS allowed through agent-mirror/check (owner of the only
// organisation, not a superadmin) must still be refused by every other admin
// route. Asserting 403 rather than "not 200" also proves the routes are still
// mounted: an unmounted route would answer 404 and fail here.
func TestOtherAdminRoutes_DidNotGainTheWidenedPath(t *testing.T) {
	others := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/admin/stats"},
		{http.MethodGet, "/api/v1/admin/users"},
		{http.MethodGet, "/api/v1/admin/accounts-tenancy"},
		{http.MethodGet, "/api/v1/admin/accounts"},
		{http.MethodGet, "/api/v1/admin/revenue"},
	}

	for _, tc := range others {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			store := &fakeGateStore{superadmin: false, soleOwner: true}
			status, code := callAs(t, newGatedEngine(t, store), tc.method, tc.path, userPrincipal())

			if status != http.StatusForbidden {
				t.Fatalf("status = %d (code %q), want 403: only agent-mirror/check may admit the owner of a single-organisation install",
					status, code)
			}
			if code != gateRefusedCode {
				t.Fatalf("code = %q, want %q", code, gateRefusedCode)
			}
			if store.soleOwnerCalls != 0 {
				t.Fatalf("%s %s consulted the single-tenant arm (%d calls); the widened gate must be mounted on agent-mirror/check ONLY",
					tc.method, tc.path, store.soleOwnerCalls)
			}
		})
	}
}

// TestAgentMirrorGate_NotMountedWithoutSetAgentMirror: without
// SetAgentMirror the route does not exist at all, so the widened group is never
// created either. Guards against the wider gate being mounted on a group that
// later acquires other routes.
func TestAgentMirrorGate_NotMountedWithoutSetAgentMirror(t *testing.T) {
	store := &fakeGateStore{superadmin: false, soleOwner: true}
	h := NewHandler(nil, nil)
	h.gate = store
	r := gin.New()
	h.Register(r.Group("/api/v1"))

	status, _ := callAs(t, r, http.MethodPost, mirrorCheckPath, userPrincipal())
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when SetAgentMirror was never called", status)
	}
}
