package perf

// beacon_reconcile_test.go — tests for GH #174: a RUM beacon key stuck
// permanently empty on the agent because the ONE best-effort mint+push on
// first RUM-enable was lost (agent down/unreachable), with no recovery path.
//
// Covers:
//   - The mint-gate fix in UpdateConfig: rum-enabled + hash-set + acked-absent
//     re-mints; acked-present does NOT churn; rum-disabled never mints.
//   - RotateBeaconKey: unconditionally mints + rotates hash->prev + pushes,
//     regardless of prior BeaconKeySet/BeaconKeyAckedPresent state.
//   - The rotate-key HTTP endpoint: permission-gated, tenant-scoped, and NEVER
//     returns the plaintext key.
//   - MarkConfigApplied's ack-based reconcile trigger: rum_beacon_present=false
//     on an already-provisioned rum-enabled site enqueues a reconcile job;
//     rum_beacon_present=true does nothing; a nil signal (pre-#174 agent) does
//     nothing either.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// fakeBeaconKeyRotator is a minimal in-memory BeaconKeyRotator stub (no DB
// required) satisfying what Service needs from *rum.BeaconKeyRepo.
type fakeBeaconKeyRotator struct {
	mu          sync.Mutex
	rotateCalls int
	lastHash    []byte
	err         error
}

func (f *fakeBeaconKeyRotator) RotateBeaconKey(_ context.Context, _, _ uuid.UUID, newKeyHash []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rotateCalls++
	f.lastHash = newKeyHash
	return f.err
}

func (f *fakeBeaconKeyRotator) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rotateCalls
}

// fakeReconcileEnqueuer is a minimal in-memory RumBeaconReconcileEnqueuer stub.
type fakeReconcileEnqueuer struct {
	mu    sync.Mutex
	calls []RumBeaconReconcileArgs
	err   error
}

func (f *fakeReconcileEnqueuer) EnqueueRumBeaconReconcile(_ context.Context, args RumBeaconReconcileArgs) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	return f.err
}

func (f *fakeReconcileEnqueuer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// tenantScopedRepo wraps fakeRepo and only returns a config hit for
// ownerTenantID — the unit-level stand-in for the real tenant_isolation RLS
// policy (a wrong-tenant SELECT returns zero rows -> ErrNotFound), so
// RotateBeaconKey's tenant-scoping can be asserted without a live Postgres.
type tenantScopedRepo struct {
	fakeRepo
	ownerTenantID uuid.UUID
}

func (r *tenantScopedRepo) GetConfig(ctx context.Context, tenantID, siteID uuid.UUID) (Config, error) {
	if tenantID != r.ownerTenantID {
		return Config{}, ErrNotFound
	}
	return r.fakeRepo.GetConfig(ctx, tenantID, siteID)
}

// ---------------------------------------------------------------------------
// mint gate (UpdateConfig) — GH #174 fix to the service.go:269 condition
// ---------------------------------------------------------------------------

// TestUpdateConfig_MintGate_RemintsWhenAckedAbsent verifies the core bug fix:
// a site that is rum-enabled with a hash already minted (BeaconKeySet=true)
// but whose most recent ack said the agent does NOT hold the key
// (BeaconKeyAckedPresent=false) re-mints on the next operator save. Before the
// fix this state was PERMANENTLY stuck (the old gate was `!BeaconKeySet`
// only, which is already false).
func TestUpdateConfig_MintGate_RemintsWhenAckedAbsent(t *testing.T) {
	repo := &fakeRepo{
		config:      Config{RumEnabled: true, BeaconKeySet: true, BeaconKeyAckedPresent: false},
		configFound: true,
	}
	rotator := &fakeBeaconKeyRotator{}
	agent := &recordingAgent{}
	svc := NewService(repo, nil, &fakeEvents{}, nil)
	svc.SetAgentClient(agent, &fakeSites{url: "https://example.com"})
	svc.SetBeaconKeyRepo(rotator, "https://manage.example.com")

	tenantID, siteID := uuid.New(), uuid.New()
	in := UpdateConfigInput{Config: Config{TenantID: tenantID, SiteID: siteID, RumEnabled: true, RumSampleRate: 1.0}}

	if _, err := svc.UpdateConfig(context.Background(), tenantID, siteID, in); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if got := rotator.calls(); got != 1 {
		t.Fatalf("rotateCalls = %d, want 1 (acked-absent must re-mint — this is the GH #174 fix)", got)
	}
	agent.mu.Lock()
	pushed := agent.lastPerfReq
	agent.mu.Unlock()
	if pushed.RumBeaconKey == "" {
		t.Error("push payload must carry a fresh plaintext beacon key when re-minting")
	}
}

// TestUpdateConfig_MintGate_NoChurnWhenAckedPresent verifies a healthy site
// (hash set AND acked present) does not churn the key on every routine
// operator save.
func TestUpdateConfig_MintGate_NoChurnWhenAckedPresent(t *testing.T) {
	repo := &fakeRepo{
		config:      Config{RumEnabled: true, BeaconKeySet: true, BeaconKeyAckedPresent: true},
		configFound: true,
	}
	rotator := &fakeBeaconKeyRotator{}
	agent := &recordingAgent{}
	svc := NewService(repo, nil, &fakeEvents{}, nil)
	svc.SetAgentClient(agent, &fakeSites{url: "https://example.com"})
	svc.SetBeaconKeyRepo(rotator, "https://manage.example.com")

	tenantID, siteID := uuid.New(), uuid.New()
	in := UpdateConfigInput{Config: Config{TenantID: tenantID, SiteID: siteID, RumEnabled: true, RumSampleRate: 1.0}}

	if _, err := svc.UpdateConfig(context.Background(), tenantID, siteID, in); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if got := rotator.calls(); got != 0 {
		t.Fatalf("rotateCalls = %d, want 0 (a healthy acked-present save must not churn the key)", got)
	}
	agent.mu.Lock()
	pushed := agent.lastPerfReq
	agent.mu.Unlock()
	if pushed.RumBeaconKey != "" {
		t.Error("push payload must NOT carry a plaintext key on a no-churn save")
	}
}

// TestUpdateConfig_MintGate_NoMintWhenRumDisabled verifies RUM-disabled sites
// never mint, regardless of BeaconKeySet/BeaconKeyAckedPresent state.
func TestUpdateConfig_MintGate_NoMintWhenRumDisabled(t *testing.T) {
	repo := &fakeRepo{
		config:      Config{RumEnabled: false, BeaconKeySet: false, BeaconKeyAckedPresent: false},
		configFound: true,
	}
	rotator := &fakeBeaconKeyRotator{}
	svc := NewService(repo, nil, &fakeEvents{}, nil)
	svc.SetAgentClient(&recordingAgent{}, &fakeSites{url: "https://example.com"})
	svc.SetBeaconKeyRepo(rotator, "https://manage.example.com")

	tenantID, siteID := uuid.New(), uuid.New()
	in := UpdateConfigInput{Config: Config{TenantID: tenantID, SiteID: siteID, RumEnabled: false}}

	if _, err := svc.UpdateConfig(context.Background(), tenantID, siteID, in); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if got := rotator.calls(); got != 0 {
		t.Fatalf("rotateCalls = %d, want 0 (rum disabled must never mint)", got)
	}
}

// ---------------------------------------------------------------------------
// RotateBeaconKey
// ---------------------------------------------------------------------------

// TestRotateBeaconKey_UnconditionallyMintsRotatesAndPushes verifies the
// primitive rotates even when the site is ALREADY healthy (hash set, acked
// present) — this is what makes it a deterministic escape hatch usable at any
// time, not just when the mint gate would fire on its own.
func TestRotateBeaconKey_UnconditionallyMintsRotatesAndPushes(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := &fakeRepo{
		config: Config{
			TenantID: tenantID, SiteID: siteID,
			RumEnabled: true, BeaconKeySet: true, BeaconKeyAckedPresent: true,
			ConfigVersion: 3,
		},
		configFound: true,
	}
	rotator := &fakeBeaconKeyRotator{}
	agent := &recordingAgent{}
	svc := NewService(repo, nil, &fakeEvents{}, nil)
	svc.SetAgentClient(agent, &fakeSites{url: "https://example.com"})
	svc.SetBeaconKeyRepo(rotator, "https://manage.example.com")

	if err := svc.RotateBeaconKey(context.Background(), tenantID, siteID); err != nil {
		t.Fatalf("RotateBeaconKey: %v", err)
	}
	if got := rotator.calls(); got != 1 {
		t.Fatalf("rotateCalls = %d, want 1 (must rotate even though already healthy)", got)
	}
	rotator.mu.Lock()
	hashLen := len(rotator.lastHash)
	rotator.mu.Unlock()
	if hashLen == 0 {
		t.Error("RotateBeaconKey must pass a non-empty new key hash to the repo")
	}

	agent.mu.Lock()
	pushed := agent.lastPerfReq
	agent.mu.Unlock()
	if pushed.RumBeaconKey == "" {
		t.Error("push payload must carry the fresh plaintext beacon key")
	}
	if pushed.ConfigVersion != 4 {
		t.Errorf("push ConfigVersion = %d, want 4 (bumped so the agent cannot treat this as a repeat)", pushed.ConfigVersion)
	}

	repo.mu.Lock()
	stored := repo.config
	repo.mu.Unlock()
	if stored.ConfigVersion != 4 {
		t.Errorf("stored ConfigVersion = %d, want 4", stored.ConfigVersion)
	}
}

// TestRotateBeaconKey_AgentNotWired_NeverRotatesHash verifies that when the
// agent client isn't configured, RotateBeaconKey fails BEFORE touching the
// hash — rotating a key it already knows it cannot deliver would recreate the
// exact stuck-empty bug deterministically.
func TestRotateBeaconKey_AgentNotWired_NeverRotatesHash(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := &fakeRepo{config: Config{TenantID: tenantID, SiteID: siteID, RumEnabled: true}, configFound: true}
	rotator := &fakeBeaconKeyRotator{}
	svc := NewService(repo, nil, &fakeEvents{}, nil)
	svc.SetBeaconKeyRepo(rotator, "https://manage.example.com")
	// agent/sites deliberately left unwired.

	err := svc.RotateBeaconKey(context.Background(), tenantID, siteID)
	if err == nil {
		t.Fatal("expected an error when the agent client is not wired")
	}
	if _, isDomain := domain.AsDomain(err); !isDomain {
		t.Errorf("expected a domain error (so the handler 4xx/5xx's cleanly), got %T: %v", err, err)
	}
	if got := rotator.calls(); got != 0 {
		t.Fatalf("rotateCalls = %d, want 0 (must not rotate a key it cannot deliver)", got)
	}
}

// TestRotateBeaconKey_UnknownSite_ReturnsNotFound verifies RotateBeaconKey
// never partially succeeds against a site with no config row.
func TestRotateBeaconKey_UnknownSite_ReturnsNotFound(t *testing.T) {
	repo := &fakeRepo{configFound: false}
	svc := NewService(repo, nil, &fakeEvents{}, nil)
	svc.SetAgentClient(&recordingAgent{}, &fakeSites{url: "https://example.com"})
	svc.SetBeaconKeyRepo(&fakeBeaconKeyRotator{}, "https://manage.example.com")

	err := svc.RotateBeaconKey(context.Background(), uuid.New(), uuid.New())
	de, isDomain := domain.AsDomain(err)
	if !isDomain {
		t.Fatalf("expected a domain error, got %T: %v", err, err)
	}
	if de.Code != "perf_config_not_found" {
		t.Errorf("domain error code = %q, want perf_config_not_found", de.Code)
	}
}

// ---------------------------------------------------------------------------
// rotate-key HTTP endpoint — permission gate, tenant scoping, no plaintext
// ---------------------------------------------------------------------------

// buildRotateKeyEngine builds a gin engine mounting ONLY the rotate-key route
// (mirrors Handler.Register's group + middleware chain) behind a test
// principal-injection middleware, so permission/site-access gating exercises
// the REAL authz middleware, not a hand-rolled stand-in.
func buildRotateKeyEngine(t *testing.T, svc *Service, p domain.Principal) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	})
	h := &Handler{svc: svc}
	v1 := engine.Group("/api/v1")
	h.Register(v1)
	return engine
}

// TestRotateRumBeaconKeyEndpoint_InsufficientPermission verifies a viewer
// (below PermSitePerfConfig, which requires operator+) is rejected 403.
func TestRotateRumBeaconKeyEndpoint_InsufficientPermission(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	svc := NewService(&fakeRepo{}, nil, &fakeEvents{}, nil)
	p := domain.Principal{TenantID: tenantID, Role: string(authz.RoleViewer)}
	engine := buildRotateKeyEngine(t, svc, p)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sites/"+siteID.String()+"/perf/rum/rotate-key", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
}

// TestRotateRumBeaconKeyEndpoint_SiteScopedPrincipalOutsideAllowlist verifies
// a site-scoped collaborator without this site in their allowlist gets 404
// (not 403 — mirrors RequireSiteAccess's "don't confirm the site exists" UX).
func TestRotateRumBeaconKeyEndpoint_SiteScopedPrincipalOutsideAllowlist(t *testing.T) {
	tenantID, siteID, otherSiteID := uuid.New(), uuid.New(), uuid.New()
	svc := NewService(&fakeRepo{}, nil, &fakeEvents{}, nil)
	p := domain.Principal{
		TenantID:       tenantID,
		Role:           string(authz.RoleOperator),
		Scope:          domain.ScopeSite,
		AllowedSiteIDs: []uuid.UUID{otherSiteID}, // siteID is NOT in the allowlist
	}
	engine := buildRotateKeyEngine(t, svc, p)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sites/"+siteID.String()+"/perf/rum/rotate-key", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}

// TestRotateRumBeaconKeyEndpoint_CrossTenantSiteNotVisible verifies that even
// past the permission + site-allowlist gates, RotateBeaconKey itself never
// succeeds for the wrong tenant — the tenantScopedRepo stands in for the real
// RLS tenant_isolation policy hiding the row.
func TestRotateRumBeaconKeyEndpoint_CrossTenantSiteNotVisible(t *testing.T) {
	ownerTenantID, callerTenantID, siteID := uuid.New(), uuid.New(), uuid.New()
	repo := &tenantScopedRepo{
		fakeRepo:      fakeRepo{config: Config{RumEnabled: true, BeaconKeySet: true}, configFound: true},
		ownerTenantID: ownerTenantID,
	}
	svc := NewService(repo, nil, &fakeEvents{}, nil)
	svc.SetAgentClient(&recordingAgent{}, &fakeSites{url: "https://example.com"})
	svc.SetBeaconKeyRepo(&fakeBeaconKeyRotator{}, "https://manage.example.com")

	// The caller's principal carries a DIFFERENT tenant than the row's owner —
	// an org-scoped principal (no site allowlist check) so the request reaches
	// the service layer, where RLS-equivalent scoping must reject it.
	p := domain.Principal{TenantID: callerTenantID, Role: string(authz.RoleOperator)}
	engine := buildRotateKeyEngine(t, svc, p)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sites/"+siteID.String()+"/perf/rum/rotate-key", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (cross-tenant rotate must be rejected); body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "beacon_key_set\":true") {
		t.Errorf("a rejected cross-tenant request must not report beacon_key_set:true; body = %s", rec.Body.String())
	}
}

// TestRotateRumBeaconKeyEndpoint_Success verifies the PINNED success response
// shape {"ok":true,"beacon_key_set":true} and that no plaintext key ever
// appears in the response body.
func TestRotateRumBeaconKeyEndpoint_Success(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := &fakeRepo{
		config:      Config{TenantID: tenantID, SiteID: siteID, RumEnabled: true, BeaconKeySet: true},
		configFound: true,
	}
	agent := &recordingAgent{}
	svc := NewService(repo, nil, &fakeEvents{}, nil)
	svc.SetAgentClient(agent, &fakeSites{url: "https://example.com"})
	svc.SetBeaconKeyRepo(&fakeBeaconKeyRotator{}, "https://manage.example.com")

	p := domain.Principal{TenantID: tenantID, Role: string(authz.RoleOperator)}
	engine := buildRotateKeyEngine(t, svc, p)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sites/"+siteID.String()+"/perf/rum/rotate-key", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"ok":true`) || !strings.Contains(body, `"beacon_key_set":true`) {
		t.Errorf("body must be the pinned {ok:true, beacon_key_set:true} shape, got %s", body)
	}
	// SECURITY: the plaintext key the agent received must never appear in the
	// HTTP response — only the confirmation boolean.
	agent.mu.Lock()
	plaintext := agent.lastPerfReq.RumBeaconKey
	agent.mu.Unlock()
	if plaintext == "" {
		t.Fatal("test setup error: agent did not receive a plaintext key to check against")
	}
	if strings.Contains(body, plaintext) {
		t.Errorf("response body leaked the plaintext beacon key: %s", body)
	}
}

// ---------------------------------------------------------------------------
// MarkConfigApplied — ack-based reconcile trigger
// ---------------------------------------------------------------------------

func boolPtr(b bool) *bool { return &b }

// TestMarkConfigApplied_AckAbsent_EnqueuesReconcile verifies an ack reporting
// rum_beacon_present=false on an already hash-set, rum-enabled site enqueues a
// reconcile job — the systemic self-heal that makes the stuck-empty state
// unable to survive past one ack cycle.
func TestMarkConfigApplied_AckAbsent_EnqueuesReconcile(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := &fakeRepo{
		config:      Config{RumEnabled: true, BeaconKeySet: true},
		configFound: true,
	}
	enq := &fakeReconcileEnqueuer{}
	svc := NewService(repo, nil, &fakeEvents{}, nil)
	svc.SetRumBeaconReconcileEnqueuer(enq)

	if err := svc.MarkConfigApplied(context.Background(), tenantID, siteID, "nginx", true, true, true, boolPtr(false)); err != nil {
		t.Fatalf("MarkConfigApplied: %v", err)
	}
	if got := enq.count(); got != 1 {
		t.Fatalf("reconcile enqueue count = %d, want 1", got)
	}
	enq.mu.Lock()
	args := enq.calls[0]
	enq.mu.Unlock()
	if args.SiteID != siteID || args.TenantID != tenantID {
		t.Errorf("enqueued args = %+v, want site=%s tenant=%s", args, siteID, tenantID)
	}
	repo.mu.Lock()
	ackedCalls := append([]bool(nil), repo.beaconAckedCalls...)
	repo.mu.Unlock()
	if len(ackedCalls) != 1 || ackedCalls[0] != false {
		t.Errorf("beaconAckedCalls = %v, want [false]", ackedCalls)
	}
}

// TestMarkConfigApplied_AckPresent_NoReconcile verifies present=true never
// enqueues a reconcile job.
func TestMarkConfigApplied_AckPresent_NoReconcile(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := &fakeRepo{
		config:      Config{RumEnabled: true, BeaconKeySet: true},
		configFound: true,
	}
	enq := &fakeReconcileEnqueuer{}
	svc := NewService(repo, nil, &fakeEvents{}, nil)
	svc.SetRumBeaconReconcileEnqueuer(enq)

	if err := svc.MarkConfigApplied(context.Background(), tenantID, siteID, "nginx", true, true, true, boolPtr(true)); err != nil {
		t.Fatalf("MarkConfigApplied: %v", err)
	}
	if got := enq.count(); got != 0 {
		t.Fatalf("reconcile enqueue count = %d, want 0 (present=true must never trigger reconcile)", got)
	}
}

// TestMarkConfigApplied_AckNil_NoSignal verifies a pre-#174 agent (which omits
// rum_beacon_present entirely, decoding to a nil pointer) never triggers a
// reconcile — nil is "no signal", not "absent".
func TestMarkConfigApplied_AckNil_NoSignal(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := &fakeRepo{
		config:      Config{RumEnabled: true, BeaconKeySet: true},
		configFound: true,
	}
	enq := &fakeReconcileEnqueuer{}
	svc := NewService(repo, nil, &fakeEvents{}, nil)
	svc.SetRumBeaconReconcileEnqueuer(enq)

	if err := svc.MarkConfigApplied(context.Background(), tenantID, siteID, "nginx", true, true, true, nil); err != nil {
		t.Fatalf("MarkConfigApplied: %v", err)
	}
	if got := enq.count(); got != 0 {
		t.Fatalf("reconcile enqueue count = %d, want 0 (nil signal must be a no-op)", got)
	}
	repo.mu.Lock()
	ackedCalls := len(repo.beaconAckedCalls)
	repo.mu.Unlock()
	if ackedCalls != 0 {
		t.Errorf("UpdateBeaconKeyAcked must not be called when the ack omits the field, got %d calls", ackedCalls)
	}
}

// TestMarkConfigApplied_NotRumEnabled_NoReconcileEvenIfAbsent verifies a
// present=false ack on a site that is NOT rum-enabled never enqueues a
// reconcile (there is nothing to heal).
func TestMarkConfigApplied_NotRumEnabled_NoReconcileEvenIfAbsent(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := &fakeRepo{
		config:      Config{RumEnabled: false, BeaconKeySet: false},
		configFound: true,
	}
	enq := &fakeReconcileEnqueuer{}
	svc := NewService(repo, nil, &fakeEvents{}, nil)
	svc.SetRumBeaconReconcileEnqueuer(enq)

	if err := svc.MarkConfigApplied(context.Background(), tenantID, siteID, "nginx", true, true, true, boolPtr(false)); err != nil {
		t.Fatalf("MarkConfigApplied: %v", err)
	}
	if got := enq.count(); got != 0 {
		t.Fatalf("reconcile enqueue count = %d, want 0 (rum disabled has nothing to heal)", got)
	}
}
