package admin

// vuln_feed_gate_test.go: who may reach the vulnerability-feed key routes, and
// where an admitted change is recorded (GH #361).
//
// The rule (owner ruling, 2026-10-09): whoever may manage the instance email
// settings may manage the feed key. The decision is
// admingate.InstanceEmailAuthority, which also answers /settings/smtp and
// me.can_manage_instance_email.
//
// Everything here drives the REAL Register wiring on a real Gin engine with a
// fake gate store and fake audit sinks, so it proves which gate each route
// carries. The SQL behind the store is proven against real rows, as wpmgr_app,
// in tests/vuln_feed_instance_authority_integration_test.go.

import (
	"bytes"
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
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// fakeInstanceStore answers every fact admingate.InstanceEmailAuthority reads.
// It also satisfies admingate.Store, so the superadmin group's gate reads the
// same facts and a route mounted there is judged on the same inputs.
type fakeInstanceStore struct {
	superadmin    bool
	superadminErr error
	soleTenant    uuid.UUID // uuid.Nil: the caller does not own the only live organisation
	soleErr       error
	selfHosted    bool
	installOwner  bool
	installTenant uuid.UUID
	installErr    error

	superadminCalls int
	soleCalls       int
	installCalls    int
}

func (f *fakeInstanceStore) IsSuperadmin(context.Context, uuid.UUID) (bool, error) {
	f.superadminCalls++
	return f.superadmin, f.superadminErr
}

func (f *fakeInstanceStore) SoleLiveTenantOwnedBy(context.Context, uuid.UUID) (uuid.UUID, error) {
	f.soleCalls++
	return f.soleTenant, f.soleErr
}

func (f *fakeInstanceStore) SelfHosted() bool { return f.selfHosted }

func (f *fakeInstanceStore) InstallOwnerAuditTenant(context.Context, uuid.UUID) (bool, uuid.UUID, error) {
	f.installCalls++
	return f.installOwner, f.installTenant, f.installErr
}

func (f *fakeInstanceStore) reads() int { return f.superadminCalls + f.soleCalls + f.installCalls }

type sysAuditEvent struct {
	actor  uuid.UUID
	action string
	raw    []byte
	meta   map[string]any
}

type fakeSystemAudit struct{ events []sysAuditEvent }

func (f *fakeSystemAudit) RecordSystemAudit(_ context.Context, actorID uuid.UUID, action string, meta []byte) error {
	var m map[string]any
	_ = json.Unmarshal(meta, &m)
	f.events = append(f.events, sysAuditEvent{actor: actorID, action: action, raw: meta, meta: m})
	return nil
}

type fakeOrgAudit struct{ events []audit.Event }

func (f *fakeOrgAudit) Record(_ context.Context, e audit.Event) (audit.Entry, error) {
	f.events = append(f.events, e)
	return audit.Entry{}, nil
}

// vulnFeedRig is one engine with the vuln-feed routes wired the way
// cmd/wpmgr/main.go wires them, plus everything a test observes.
type vulnFeedRig struct {
	engine *gin.Engine
	store  *fakeInstanceStore
	repo   *fakeInstSettingsRepo
	enq    *captureFeedEnqueuer
	sys    *fakeSystemAudit
	org    *fakeOrgAudit
}

const testFeedKey = "feed-key-under-test-1234"

func newVulnFeedRig(t *testing.T, store *fakeInstanceStore) *vulnFeedRig {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rig := &vulnFeedRig{
		store: store,
		repo:  newFakeInstSettingsRepo(),
		enq:   &captureFeedEnqueuer{},
		sys:   &fakeSystemAudit{},
		org:   &fakeOrgAudit{},
	}
	keySvc := NewVulnFeedKeyService(rig.repo, newTestAgeIdentity(t), "", rig.enq, nil)
	h := NewHandler(nil, nil)
	h.gate = store
	h.sysAudit = rig.sys
	h.auditRec = rig.org
	h.SetVulnFeed(&fakeFeedMetaReader{}, keySvc, store)
	r := gin.New()
	h.Register(r.Group("/api/v1"))
	rig.engine = r
	return rig
}

type vulnFeedRoute struct {
	method, path, body string
	admitted           int // the handler's status once admitted
}

var vulnFeedRoutes = []vulnFeedRoute{
	{http.MethodGet, "/api/v1/admin/vuln-feed/status", "", http.StatusOK},
	{http.MethodPut, "/api/v1/admin/vuln-feed/key", `{"key":"` + testFeedKey + `"}`, http.StatusOK},
	{http.MethodDelete, "/api/v1/admin/vuln-feed/key", "", http.StatusOK},
	{http.MethodPost, "/api/v1/admin/vuln-feed/sync", "", http.StatusAccepted},
}

func (r *vulnFeedRig) call(method, path, body string, p *domain.Principal) (int, string) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if p != nil {
		req = req.WithContext(domain.WithPrincipal(req.Context(), *p))
	}
	w := httptest.NewRecorder()
	r.engine.ServeHTTP(w, req)
	var env struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	return w.Code, env.Code
}

func requireVulnFeedAdmitted(t *testing.T, rig *vulnFeedRig, p *domain.Principal) {
	t.Helper()
	for _, rt := range vulnFeedRoutes {
		status, code := rig.call(rt.method, rt.path, rt.body, p)
		if status != rt.admitted {
			t.Errorf("%s %s: got %d %q, want %d from the handler", rt.method, rt.path, status, code, rt.admitted)
		}
	}
	// PUT stores the key and queues a sync, POST /sync queues another.
	if rig.enq.calls != 2 {
		t.Errorf("feed refresh queued %d times, want 2 (PUT key, POST sync)", rig.enq.calls)
	}
}

// requireVulnFeedRefused asserts every route answers the admin family's one
// refusal and that nothing was written or queued.
func requireVulnFeedRefused(t *testing.T, rig *vulnFeedRig, p *domain.Principal) {
	t.Helper()
	for _, rt := range vulnFeedRoutes {
		status, code := rig.call(rt.method, rt.path, rt.body, p)
		if status != http.StatusForbidden || code != gateRefusedCode {
			t.Errorf("%s %s: got %d %q, want 403 %q", rt.method, rt.path, status, code, gateRefusedCode)
		}
	}
	if _, stored, _ := rig.repo.Get(context.Background(), instanceSettingKey); stored {
		t.Error("the feed key was stored under a refused request")
	}
	if rig.enq.calls != 0 {
		t.Errorf("feed refresh queued %d times under refused requests, want 0", rig.enq.calls)
	}
	if len(rig.sys.events) != 0 || len(rig.org.events) != 0 {
		t.Errorf("refused requests were audited as changes: %d instance, %d organisation", len(rig.sys.events), len(rig.org.events))
	}
}

var (
	soleOrgID    = uuid.MustParse("5a1e0000-0000-4000-8000-000000000361")
	installOrgID = uuid.MustParse("1a5e0000-0000-4000-8000-000000000361")
)

// The owner of the only live organisation, who is not a superadmin, manages the
// feed key exactly as they manage the instance email settings. Red when the
// routes sit on the superadmin group.
func TestVulnFeedGate_SoleLiveTenantOwnerAdmitted(t *testing.T) {
	store := &fakeInstanceStore{soleTenant: soleOrgID, selfHosted: true}
	rig := newVulnFeedRig(t, store)
	requireVulnFeedAdmitted(t, rig, userPrincipal())
	if store.installCalls != 0 {
		t.Errorf("install-owner fact read %d times for an owner admitted by the sole-owner arm, want 0", store.installCalls)
	}
}

// On a self-hosted install with a second organisation, the account that set up
// the install keeps the feed key, as it keeps the instance email settings. Red
// when the routes sit on the superadmin group.
func TestVulnFeedGate_InstallOwnerAdmittedOnSelfHosted(t *testing.T) {
	store := &fakeInstanceStore{selfHosted: true, installOwner: true, installTenant: installOrgID}
	rig := newVulnFeedRig(t, store)
	requireVulnFeedAdmitted(t, rig, userPrincipal())
}

// Unchanged: a superadmin still manages the key, and the later arms are not
// consulted for them.
func TestVulnFeedGate_SuperadminAdmitted(t *testing.T) {
	store := &fakeInstanceStore{superadmin: true, soleTenant: soleOrgID, selfHosted: true, installOwner: true, installTenant: installOrgID}
	rig := newVulnFeedRig(t, store)
	requireVulnFeedAdmitted(t, rig, userPrincipal())
	if store.soleCalls != 0 || store.installCalls != 0 {
		t.Errorf("later arms consulted for a superadmin: %d sole-owner, %d install-owner reads", store.soleCalls, store.installCalls)
	}
}

func TestVulnFeedGate_RefusedWithoutInstanceEmailAuthority(t *testing.T) {
	siteScoped := userPrincipal()
	siteScoped.Scope = domain.ScopeSite
	siteScoped.AllowedSiteIDs = []uuid.UUID{uuid.New()}
	apiKey := &domain.Principal{Type: domain.PrincipalAPIKey, APIKeyID: uuid.New(), TenantID: soleOrgID}

	cases := []struct {
		name  string
		store *fakeInstanceStore
		p     *domain.Principal
		// noReads: the decision must refuse before reading any fact.
		noReads bool
	}{
		{
			// A second live organisation closes the sole-owner arm, and a
			// member who is not the install owner holds no other arm. Red when
			// the routes are gated on organisation scope alone.
			name:  "member of one of several organisations",
			store: &fakeInstanceStore{selfHosted: true},
			p:     userPrincipal(),
		},
		{
			name:  "install owner on a hosted install",
			store: &fakeInstanceStore{selfHosted: false, installOwner: true, installTenant: installOrgID},
			p:     userPrincipal(),
		},
		{
			name:    "API key minted by the owner of the only organisation",
			store:   &fakeInstanceStore{superadmin: true, soleTenant: soleOrgID, selfHosted: true, installOwner: true, installTenant: installOrgID},
			p:       apiKey,
			noReads: true,
		},
		{
			name:    "site-constrained superadmin",
			store:   &fakeInstanceStore{superadmin: true, selfHosted: true},
			p:       siteScoped,
			noReads: true,
		},
		{
			name:    "no principal",
			store:   &fakeInstanceStore{superadmin: true, selfHosted: true},
			p:       nil,
			noReads: true,
		},
		{
			// A failed is_superadmin read refuses and must not fall through to
			// an arm that would admit.
			name:  "superadmin read fails",
			store: &fakeInstanceStore{superadminErr: errors.New("connection reset"), soleTenant: soleOrgID, selfHosted: true, installOwner: true, installTenant: installOrgID},
			p:     userPrincipal(),
		},
		{
			name:  "install-owner read fails",
			store: &fakeInstanceStore{selfHosted: true, installOwner: true, installTenant: installOrgID, installErr: errors.New("connection reset")},
			p:     userPrincipal(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newVulnFeedRig(t, tc.store)
			requireVulnFeedRefused(t, rig, tc.p)
			if tc.noReads && tc.store.reads() != 0 {
				t.Errorf("gate read %d facts for this principal, want 0", tc.store.reads())
			}
		})
	}
	t.Run("superadmin read fails without consulting a later arm", func(t *testing.T) {
		store := &fakeInstanceStore{superadminErr: errors.New("connection reset"), soleTenant: soleOrgID, selfHosted: true, installOwner: true, installTenant: installOrgID}
		rig := newVulnFeedRig(t, store)
		requireVulnFeedRefused(t, rig, userPrincipal())
		if store.soleCalls != 0 || store.installCalls != 0 {
			t.Errorf("later arms consulted after a failed read: %d sole-owner, %d install-owner", store.soleCalls, store.installCalls)
		}
	})
}

// Containment: wiring the vuln-feed group must not widen any other admin route.
// The owner of the only organisation, admitted to the vuln-feed routes, is
// still refused by the superadmin-only routes.
func TestVulnFeedGate_OtherAdminRoutesStaySuperadminOnly(t *testing.T) {
	store := &fakeInstanceStore{soleTenant: soleOrgID, selfHosted: true, installOwner: true, installTenant: installOrgID}
	rig := newVulnFeedRig(t, store)
	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/stats"},
		{http.MethodGet, "/api/v1/admin/users"},
		{http.MethodGet, "/api/v1/admin/system-audit"},
		{http.MethodGet, "/api/v1/admin/accounts"},
		{http.MethodGet, "/api/v1/admin/revenue"},
	} {
		status, code := rig.call(rt.method, rt.path, "", userPrincipal())
		if status != http.StatusForbidden || code != gateRefusedCode {
			t.Errorf("%s %s: got %d %q, want 403 %q", rt.method, rt.path, status, code, gateRefusedCode)
		}
	}
	if status, _ := rig.call(http.MethodGet, "/api/v1/admin/vuln-feed/status", "", userPrincipal()); status != http.StatusOK {
		t.Fatalf("PREMISE FAILED: the same principal got %d on the vuln-feed status, want 200", status)
	}
}

// Every admitted change is recorded in the instance trail. The owner arms are
// also recorded in the organisation the decision named, never the request's
// active one. A superadmin is recorded in the instance trail only. No record
// carries the key.
func TestVulnFeedAudit_RoutedByAdmittingArm(t *testing.T) {
	writes := []struct {
		method, path, body, action string
	}{
		{http.MethodPut, "/api/v1/admin/vuln-feed/key", `{"key":"` + testFeedKey + `"}`, auditActionVulnFeedKeySet},
		{http.MethodPost, "/api/v1/admin/vuln-feed/sync", "", auditActionVulnFeedSync},
		{http.MethodDelete, "/api/v1/admin/vuln-feed/key", "", auditActionVulnFeedKeyClear},
	}
	cases := []struct {
		name    string
		store   *fakeInstanceStore
		arm     admingate.Arm
		orgCopy uuid.UUID // uuid.Nil: no organisation copy
	}{
		{"superadmin", &fakeInstanceStore{superadmin: true, selfHosted: true}, admingate.ArmSuperadmin, uuid.Nil},
		{"owner of the only organisation", &fakeInstanceStore{soleTenant: soleOrgID, selfHosted: true}, admingate.ArmSoleLiveTenantOwner, soleOrgID},
		{"install owner", &fakeInstanceStore{selfHosted: true, installOwner: true, installTenant: installOrgID}, admingate.ArmInstallOwner, installOrgID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newVulnFeedRig(t, tc.store)
			p := userPrincipal()
			p.TenantID = uuid.New() // the request's active organisation, which must not receive the copy
			for _, w := range writes {
				status, code := rig.call(w.method, w.path, w.body, p)
				if status >= 300 {
					t.Fatalf("%s %s: got %d %q, want success", w.method, w.path, status, code)
				}
			}
			if len(rig.sys.events) != len(writes) {
				t.Fatalf("instance trail records = %d, want %d", len(rig.sys.events), len(writes))
			}
			for i, w := range writes {
				ev := rig.sys.events[i]
				if ev.action != w.action || ev.actor != p.UserID {
					t.Errorf("instance trail record %d = %s by %s, want %s by %s", i, ev.action, ev.actor, w.action, p.UserID)
				}
				if got := ev.meta["admitted_as"]; got != tc.arm.String() {
					t.Errorf("instance trail admitted_as = %v, want %q", got, tc.arm.String())
				}
				if got := ev.meta["target_id"]; got != instanceSettingKey {
					t.Errorf("instance trail target_id = %v, want %q", got, instanceSettingKey)
				}
				if bytes.Contains(ev.raw, []byte(testFeedKey)) {
					t.Errorf("instance trail record %s carries the key", ev.action)
				}
			}
			if tc.orgCopy == uuid.Nil {
				if len(rig.org.events) != 0 {
					t.Fatalf("organisation copies = %d for a superadmin, want 0", len(rig.org.events))
				}
				return
			}
			if len(rig.org.events) != len(writes) {
				t.Fatalf("organisation copies = %d, want %d", len(rig.org.events), len(writes))
			}
			for i, w := range writes {
				ev := rig.org.events[i]
				if ev.TenantID != tc.orgCopy {
					t.Errorf("organisation copy of %s went to %s, want the admitting organisation %s (request named %s)",
						w.action, ev.TenantID, tc.orgCopy, p.TenantID)
				}
				if ev.Action != w.action || ev.ActorType != audit.ActorUser || ev.ActorID != p.UserID.String() {
					t.Errorf("organisation copy %d = %+v, want %s by user %s", i, ev, w.action, p.UserID)
				}
				if ev.Metadata["admitted_as"] != tc.arm.String() {
					t.Errorf("organisation copy admitted_as = %v, want %q", ev.Metadata["admitted_as"], tc.arm.String())
				}
				raw, _ := json.Marshal(ev)
				if bytes.Contains(raw, []byte(testFeedKey)) {
					t.Errorf("organisation copy of %s carries the key", w.action)
				}
			}
		})
	}
}

// A write handler reached without the gate in front of it has no admitting
// decision to route its record by, so it refuses instead of writing an
// unattributed change.
func TestVulnFeedWrites_RefusedWithoutAdmittingDecision(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := newFakeInstSettingsRepo()
	enq := &captureFeedEnqueuer{}
	h := NewHandler(nil, nil)
	h.SetVulnFeed(nil, NewVulnFeedKeyService(repo, newTestAgeIdentity(t), "", enq, nil), nil)
	r := gin.New()
	r.PUT("/key", h.vulnFeedSetKey)
	r.DELETE("/key", h.vulnFeedClearKey)
	r.POST("/sync", h.vulnFeedSync)
	for _, rt := range []struct{ method, path, body string }{
		{http.MethodPut, "/key", `{"key":"` + testFeedKey + `"}`},
		{http.MethodDelete, "/key", ""},
		{http.MethodPost, "/sync", ""},
	} {
		req := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
		req = req.WithContext(domain.WithPrincipal(req.Context(), *userPrincipal()))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s without the gate: got %d, want 403", rt.method, rt.path, w.Code)
		}
	}
	if _, stored, _ := repo.Get(context.Background(), instanceSettingKey); stored || enq.calls != 0 {
		t.Errorf("ungated writes took effect: stored=%v syncs=%d", stored, enq.calls)
	}
}
