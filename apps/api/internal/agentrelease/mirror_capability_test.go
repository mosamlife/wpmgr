package agentrelease_test

// mirror_capability_test.go: agent_mirror.can_check_now on GET
// /api/v1/fleet/agents (GH #322).
//
// The field is what the Sites page's Agent column popover renders its "Check
// now" button from. It must be the SAME answer POST
// /api/v1/admin/agent-mirror/check's own gate gives, which is why both go
// through admingate.CanRunAgentMirrorCheck rather than each deciding for
// itself. These tests drive the REAL route and read the REAL wire field.
//
// WHICH OF THESE FAIL AGAINST THE PRE-CHANGE CODE. All of them, including
// every case that expects FALSE, and that is deliberate. Before this change
// can_check_now did not exist on the wire at all, so a test that decoded into
// a bool and asserted false would have passed against the missing field and
// proven nothing. Every assertion below therefore checks the key is PRESENT in
// the agent_mirror object first, and only then checks its value; the pre-change
// response has no such key and fails at the presence check.
//
// WHAT THIS FILE CANNOT PROVE. With a fake store, "owner of one of two live
// organisations" and "non-owner member of the only organisation" are the same
// input (the store answers false) and differ only inside the SQL. That
// distinction is proven against real rows in
// mirror_capability_integration_test.go, and the SQL itself is shared with the
// route gate, which exercises it again in internal/admin/gate_integration_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/admingate"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentrelease"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// errTestStore stands in for any database failure on either gate read.
var errTestStore = errors.New("connection reset")

// fakeCheckGate is an admingate.Store test double. The two facts are set
// independently so a test can assert which one was read.
type fakeCheckGate struct {
	superadmin    bool
	superadminErr error
	soleOwner     bool
	soleOwnerErr  error

	superadminCalls int
	soleOwnerCalls  int
}

func (f *fakeCheckGate) IsSuperadmin(context.Context, uuid.UUID) (bool, error) {
	f.superadminCalls++
	return f.superadmin, f.superadminErr
}

func (f *fakeCheckGate) IsSoleLiveTenantOwner(context.Context, uuid.UUID) (bool, error) {
	f.soleOwnerCalls++
	return f.soleOwner, f.soleOwnerErr
}

var _ admingate.Store = (*fakeCheckGate)(nil)

// capabilityEngine mounts the REAL /fleet/agents route with a fake rollup
// (this file is about the permission, not the version comparison), the mirror
// switched on or off as asked, and the given gate store.
func capabilityEngine(t *testing.T, mirrorEnabled bool, gate admingate.Store) (*gin.Engine, uuid.UUID) {
	t.Helper()
	tenantID := uuid.New()
	repo := &fakeSiteLister{byTenant: map[uuid.UUID][]agentrelease.SiteAgentVersion{
		tenantID: {{SiteID: uuid.New(), SiteName: "site", AgentVersion: "0.61.120"}},
	}}
	h := agentrelease.NewHandler(
		agentrelease.NewService(repo, &fakeVersionReader{version: "0.61.120"}),
		false,
	)
	h.SetMirror(&fakeMirrorStateReader{}, mirrorEnabled)
	if gate != nil {
		h.SetMirrorCheckGate(gate)
	}
	return newTestEngine(h), tenantID
}

// readCanCheckNow drives GET /fleet/agents as the given principal and returns
// (value, present). present is false when the key is missing from the
// agent_mirror object, which is exactly the pre-change response shape and must
// fail every test here rather than silently read as false.
func readCanCheckNow(t *testing.T, engine *gin.Engine, p domain.Principal) (value bool, present bool) {
	t.Helper()
	w := doRequest(engine, http.MethodGet, "/api/v1/fleet/agents", p)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /fleet/agents = %d; want 200, body=%s", w.Code, w.Body.String())
	}
	var body struct {
		AgentMirror map[string]json.RawMessage `json:"agent_mirror"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode fleet response: %v", err)
	}
	raw, ok := body.AgentMirror["can_check_now"]
	if !ok {
		return false, false
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("can_check_now is not a boolean (%s): %v", raw, err)
	}
	return value, true
}

func orgUser(tenantID, userID uuid.UUID) domain.Principal {
	return domain.Principal{
		TenantID: tenantID,
		Type:     domain.PrincipalUser,
		UserID:   userID,
		Scope:    "",
		Role:     string(authz.RoleOwner),
	}
}

// assertCanCheckNow is the single assertion helper. It insists the field is on
// the wire before judging its value, so an absent field can never be mistaken
// for a settled false.
func assertCanCheckNow(t *testing.T, engine *gin.Engine, p domain.Principal, want bool, why string) {
	t.Helper()
	got, present := readCanCheckNow(t, engine, p)
	if !present {
		t.Fatalf("%s: agent_mirror has no can_check_now field at all; the capability must always be emitted so a client never has to treat absent and false as the same thing", why)
	}
	if got != want {
		t.Fatalf("%s: can_check_now = %v, want %v", why, got, want)
	}
}

// --- T1: superadmin ---------------------------------------------------------

// TestCanCheckNow_SuperadminIsTrue. A superadmin may run the check on any
// install, so the capability is true. The widened arm must not even be
// consulted for them, matching the route gate's own short circuit.
func TestCanCheckNow_SuperadminIsTrue(t *testing.T) {
	gate := &fakeCheckGate{superadmin: true, soleOwner: false}
	engine, tenantID := capabilityEngine(t, true, gate)

	assertCanCheckNow(t, engine, orgUser(tenantID, uuid.New()), true, "superadmin")

	if gate.soleOwnerCalls != 0 {
		t.Fatalf("sole-owner lookup ran %d times for a superadmin; want 0", gate.soleOwnerCalls)
	}
}

// --- T2: owner of the only live organisation --------------------------------

// TestCanCheckNow_SoleLiveTenantOwnerIsTrue is the whole point of the change:
// the person 0.61.123 granted the permission to must now be offered the button.
func TestCanCheckNow_SoleLiveTenantOwnerIsTrue(t *testing.T) {
	gate := &fakeCheckGate{superadmin: false, soleOwner: true}
	engine, tenantID := capabilityEngine(t, true, gate)

	assertCanCheckNow(t, engine, orgUser(tenantID, uuid.New()), true,
		"owner of the only live organisation")

	if gate.soleOwnerCalls != 1 {
		t.Fatalf("sole-owner lookup ran %d times; want exactly 1 (read at request time, never cached)", gate.soleOwnerCalls)
	}
}

// --- T3 + T4: refused when either half of the condition is false ------------

// TestCanCheckNow_FalseWhenNotSoleOwner covers both the "owner of one of
// several live organisations" (T3) and the "member but not owner" (T4) shapes
// at this layer: whatever the reason, the shared decision answers false and no
// button is offered. Which INPUTS produce which answer is a property of the SQL
// and is proven against real rows in mirror_capability_integration_test.go.
func TestCanCheckNow_FalseWhenNotSoleOwner(t *testing.T) {
	gate := &fakeCheckGate{superadmin: false, soleOwner: false}
	engine, tenantID := capabilityEngine(t, true, gate)

	assertCanCheckNow(t, engine, orgUser(tenantID, uuid.New()), false,
		"neither superadmin nor owner of a sole live organisation")
}

// --- T5: the mirror is off --------------------------------------------------

// TestCanCheckNow_FalseWhenMirrorDisabled: with mirroring off there is no run
// to trigger, so nobody may trigger one, however privileged. Asserted for BOTH
// callers who would otherwise be true, so a future implementation that returns
// the raw permission and forgets the enabled term fails here.
//
// It also pins the cheap path: the gate store must not be read at all when the
// mirror is off, so the hosted service (mirroring disabled by default) pays no
// extra query per fleet request for a field that can only be false.
func TestCanCheckNow_FalseWhenMirrorDisabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		gate *fakeCheckGate
	}{
		{"superadmin", &fakeCheckGate{superadmin: true}},
		{"sole live tenant owner", &fakeCheckGate{soleOwner: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, tenantID := capabilityEngine(t, false, tc.gate)

			assertCanCheckNow(t, engine, orgUser(tenantID, uuid.New()), false,
				tc.name+" on an install with mirroring disabled")

			if tc.gate.superadminCalls != 0 || tc.gate.soleOwnerCalls != 0 {
				t.Fatalf("gate was consulted (%d superadmin, %d sole-owner) with the mirror disabled; want 0 and 0",
					tc.gate.superadminCalls, tc.gate.soleOwnerCalls)
			}
		})
	}
}

// --- containment ------------------------------------------------------------

// TestCanCheckNow_APIKeyPrincipalIsFalse: an API key never gets the capability,
// even on a single-organisation install where the key maps to that owner. This
// matches the route gate exactly (it refuses API keys before either lookup), so
// the flag cannot advertise something the endpoint would refuse.
func TestCanCheckNow_APIKeyPrincipalIsFalse(t *testing.T) {
	gate := &fakeCheckGate{superadmin: true, soleOwner: true} // both true: only the principal type may refuse
	engine, tenantID := capabilityEngine(t, true, gate)

	p := domain.Principal{
		TenantID: tenantID,
		Type:     domain.PrincipalAPIKey,
		APIKeyID: uuid.New(),
		UserID:   uuid.New(),
		Scope:    "",
		Role:     string(authz.RoleOwner),
	}
	assertCanCheckNow(t, engine, p, false, "api-key principal")

	if gate.superadminCalls != 0 || gate.soleOwnerCalls != 0 {
		t.Fatalf("gate was consulted (%d superadmin, %d sole-owner) for a non-user principal; want 0 and 0",
			gate.superadminCalls, gate.soleOwnerCalls)
	}
}

// TestCanCheckNow_FailsClosedOnStoreError: a database blip must hide the button,
// never reveal it. Both reads are covered, and the is_superadmin failure must
// NOT fall through to the widened arm.
func TestCanCheckNow_FailsClosedOnStoreError(t *testing.T) {
	t.Run("sole-owner read fails", func(t *testing.T) {
		gate := &fakeCheckGate{soleOwner: true, soleOwnerErr: errTestStore}
		engine, tenantID := capabilityEngine(t, true, gate)
		assertCanCheckNow(t, engine, orgUser(tenantID, uuid.New()), false,
			"the organisation-count read failed")
	})

	t.Run("is_superadmin read fails", func(t *testing.T) {
		gate := &fakeCheckGate{superadminErr: errTestStore, soleOwner: true}
		engine, tenantID := capabilityEngine(t, true, gate)
		assertCanCheckNow(t, engine, orgUser(tenantID, uuid.New()), false,
			"the is_superadmin read failed")
		if gate.soleOwnerCalls != 0 {
			t.Fatalf("consulted the widened arm after an is_superadmin read error; want 0 calls, got %d",
				gate.soleOwnerCalls)
		}
	})
}

// TestCanCheckNow_FalseWhenGateNotWired: an install where the capability was
// never wired reports false rather than panicking or omitting the field. The
// freshness half of agent_mirror must keep working regardless, since the two
// are independent facts with independent wiring.
func TestCanCheckNow_FalseWhenGateNotWired(t *testing.T) {
	engine, tenantID := capabilityEngine(t, true, nil)
	assertCanCheckNow(t, engine, orgUser(tenantID, uuid.New()), false, "gate not wired")

	w := doRequest(engine, http.MethodGet, "/api/v1/fleet/agents", orgUser(tenantID, uuid.New()))
	var body struct {
		AgentMirror struct {
			Enabled bool   `json:"enabled"`
			Status  string `json:"status"`
		} `json:"agent_mirror"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.AgentMirror.Enabled || body.AgentMirror.Status == "" {
		t.Fatalf("freshness degraded with the capability unwired: enabled=%v status=%q",
			body.AgentMirror.Enabled, body.AgentMirror.Status)
	}
}
