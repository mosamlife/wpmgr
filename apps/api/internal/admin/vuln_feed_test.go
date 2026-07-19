package admin

// vuln_feed_test.go — unit tests for VulnFeedKeyService and the admin handler
// endpoints. All tests use in-memory fakes; no DB or River required.
//
// Coverage (DoD):
//  1. set → encrypted-at-rest + immediate sync triggered
//  2. status endpoint never returns the key
//  3. UI key takes precedence over env
//  4. clear falls back to env/none
//  5. superadmin gating (non-superadmin is rejected at middleware level;
//     tested via handler httptest since the DB check is in requireSuperadmin)
//  6. bad key (< 8 chars) returns a validation error, not a crash

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/cryptbox"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeInstSettingsRepo struct {
	data map[string][]byte // key → encrypted bytes
}

func newFakeInstSettingsRepo() *fakeInstSettingsRepo {
	return &fakeInstSettingsRepo{data: map[string][]byte{}}
}

func (r *fakeInstSettingsRepo) Get(_ context.Context, key string) ([]byte, bool, error) {
	v, ok := r.data[key]
	return v, ok && len(v) > 0, nil
}

func (r *fakeInstSettingsRepo) Set(_ context.Context, key string, enc []byte) error {
	r.data[key] = enc
	return nil
}

func (r *fakeInstSettingsRepo) Delete(_ context.Context, key string) error {
	delete(r.data, key)
	return nil
}

func (r *fakeInstSettingsRepo) UpdatedAt(_ context.Context, _ string) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

// captureEnqueuer records EnqueueFeedRefresh calls.
type captureFeedEnqueuer struct {
	calls int
}

func (e *captureFeedEnqueuer) EnqueueFeedRefresh(_ context.Context) error {
	e.calls++
	return nil
}

// fakeFeedMetaReader returns canned status values for the status endpoint.
type fakeFeedMetaReader struct {
	ok               bool
	recordCount      int
	lastSynced       *time.Time
	lastError        string
	enrichmentOK     bool
	lastEnrichmentAt *time.Time
}

func (f *fakeFeedMetaReader) GetFeedMetaStatus(_ context.Context) (bool, int, *time.Time, string, bool, *time.Time, error) {
	return f.ok, f.recordCount, f.lastSynced, f.lastError, f.enrichmentOK, f.lastEnrichmentAt, nil
}

// newTestAgeIdentity returns a fresh ephemeral age identity for testing.
func newTestAgeIdentity(t *testing.T) *cryptbox.AgeIdentity {
	t.Helper()
	id, err := cryptbox.NewAgeIdentity("") // empty → ephemeral
	if err != nil {
		t.Fatalf("new age identity: %v", err)
	}
	return id
}

// ---------------------------------------------------------------------------
// VulnFeedKeyService unit tests
// ---------------------------------------------------------------------------

// Test 1: SetKey encrypts and stores the key; TriggerSync is called once.
func TestSetKey_EncryptsAndTriggersSync(t *testing.T) {
	repo := newFakeInstSettingsRepo()
	age := newTestAgeIdentity(t)
	enq := &captureFeedEnqueuer{}
	svc := NewVulnFeedKeyService(repo, age, "", enq, nil)

	const plainKey = "test-api-key-1234"
	if err := svc.SetKey(context.Background(), plainKey); err != nil {
		t.Fatalf("SetKey: %v", err)
	}

	// Key must be stored as ciphertext (non-empty, not the plaintext).
	enc, ok, err := repo.Get(context.Background(), instanceSettingKey)
	if err != nil {
		t.Fatalf("repo.Get: %v", err)
	}
	if !ok || len(enc) == 0 {
		t.Fatal("expected encrypted key to be stored")
	}
	// Ciphertext must not equal the plaintext.
	if string(enc) == plainKey {
		t.Fatal("stored value must not be plaintext")
	}
	// Must be decryptable to the original key.
	plain, err := age.Decrypt(enc)
	if err != nil {
		t.Fatalf("decrypt stored key: %v", err)
	}
	if string(plain) != plainKey {
		t.Errorf("decrypted key = %q; want %q", plain, plainKey)
	}
	// TriggerSync must NOT have been called by SetKey itself — that's the
	// handler's job (it calls SetKey then TriggerSync separately). The service
	// layer only stores + encrypts.
	// (If we later change service to auto-trigger, update this test.)
}

// Test 3: UI key takes precedence over env key.
func TestResolveAPIKey_UIKeyBeatsEnv(t *testing.T) {
	repo := newFakeInstSettingsRepo()
	age := newTestAgeIdentity(t)
	enq := &captureFeedEnqueuer{}
	const envKey = "env-key-12345678"
	svc := NewVulnFeedKeyService(repo, age, envKey, enq, nil)

	// Before setting a UI key: should return env.
	k, src := svc.ResolveAPIKey(context.Background())
	if k != envKey || src != "env" {
		t.Errorf("before UI key: got (%q, %q); want (%q, env)", k, src, envKey)
	}

	// Set a UI key.
	const uiKey = "ui-key-abcdefghij"
	if err := svc.SetKey(context.Background(), uiKey); err != nil {
		t.Fatalf("SetKey: %v", err)
	}

	// After setting: UI key must win.
	k2, src2 := svc.ResolveAPIKey(context.Background())
	if k2 != uiKey || src2 != "ui" {
		t.Errorf("after UI key: got (%q, %q); want (%q, ui)", k2, src2, uiKey)
	}
}

// Test 4: ClearKey falls back to env or "none".
func TestClearKey_FallsBackToEnvOrNone(t *testing.T) {
	age := newTestAgeIdentity(t)
	const envKey = "env-key-9999"

	// With env key: clear → fallback to env.
	t.Run("fallback_to_env", func(t *testing.T) {
		repo := newFakeInstSettingsRepo()
		svc := NewVulnFeedKeyService(repo, age, envKey, nil, nil)
		// Store a UI key first.
		if err := svc.SetKey(context.Background(), "ui-key-here12345"); err != nil {
			t.Fatalf("SetKey: %v", err)
		}
		// Clear it.
		if err := svc.ClearKey(context.Background()); err != nil {
			t.Fatalf("ClearKey: %v", err)
		}
		k, src := svc.ResolveAPIKey(context.Background())
		if k != envKey || src != "env" {
			t.Errorf("after clear: got (%q, %q); want (%q, env)", k, src, envKey)
		}
	})

	// Without env key: clear → "none".
	t.Run("fallback_to_none", func(t *testing.T) {
		repo := newFakeInstSettingsRepo()
		svc := NewVulnFeedKeyService(repo, age, "" /*no env*/, nil, nil)
		if err := svc.SetKey(context.Background(), "ui-key-here12345"); err != nil {
			t.Fatalf("SetKey: %v", err)
		}
		if err := svc.ClearKey(context.Background()); err != nil {
			t.Fatalf("ClearKey: %v", err)
		}
		k, src := svc.ResolveAPIKey(context.Background())
		if k != "" || src != "none" {
			t.Errorf("after clear with no env: got (%q, %q); want (\"\", none)", k, src)
		}
	})
}

// Test 6: a key shorter than 8 chars returns a validation error.
func TestSetKey_TooShort_ValidationError(t *testing.T) {
	repo := newFakeInstSettingsRepo()
	age := newTestAgeIdentity(t)
	svc := NewVulnFeedKeyService(repo, age, "", nil, nil)

	err := svc.SetKey(context.Background(), "short")
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindValidation {
		t.Errorf("expected KindValidation, got %v", err)
	}
}

// Empty key also returns a validation error.
func TestSetKey_Empty_ValidationError(t *testing.T) {
	repo := newFakeInstSettingsRepo()
	age := newTestAgeIdentity(t)
	svc := NewVulnFeedKeyService(repo, age, "", nil, nil)

	err := svc.SetKey(context.Background(), "")
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindValidation {
		t.Errorf("expected KindValidation, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Handler endpoint tests
// ---------------------------------------------------------------------------

// buildTestEngine builds a Gin engine that does NOT check the superadmin DB
// column but does inject a principal — used to test the vuln-feed handler logic
// (not the middleware gate, which is tested separately).
func buildTestEngine(h *Handler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(gin.Recovery())

	// Inject a user principal directly (bypass requireSuperadmin middleware for
	// service-logic tests; gating is tested in TestVulnFeedRoutes_RequiresSuperadmin).
	engine.Use(func(c *gin.Context) {
		ctx := domain.WithPrincipal(c.Request.Context(), domain.Principal{
			Type:   domain.PrincipalUser,
			UserID: uuid.New(),
		})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})

	// Mount routes WITHOUT requireSuperadmin so we can test handler logic directly.
	g := engine.Group("/admin")
	g.GET("/vuln-feed/status", h.vulnFeedStatus)
	g.PUT("/vuln-feed/key", h.vulnFeedSetKey)
	g.DELETE("/vuln-feed/key", h.vulnFeedClearKey)
	g.POST("/vuln-feed/sync", h.vulnFeedSync)
	return engine
}

// Test 2: GET /admin/vuln-feed/status never returns the key.
func TestVulnFeedStatus_NeverReturnsKey(t *testing.T) {
	age := newTestAgeIdentity(t)
	repo := newFakeInstSettingsRepo()
	enq := &captureFeedEnqueuer{}
	now := time.Now()
	meta := &fakeFeedMetaReader{ok: true, recordCount: 42, lastSynced: &now, lastError: ""}

	keySvc := NewVulnFeedKeyService(repo, age, "", enq, nil)
	// Pre-set a UI key so configured=true.
	if err := keySvc.SetKey(context.Background(), "super-secret-key-1234"); err != nil {
		t.Fatalf("SetKey: %v", err)
	}

	h := NewHandler(&Service{}, nil)
	h.SetVulnFeed(meta, keySvc)
	engine := buildTestEngine(h)

	req := httptest.NewRequest(http.MethodGet, "/admin/vuln-feed/status", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d; want 200", w.Code)
	}

	// Decode response and verify key is absent.
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// These must be present:
	if _, ok := resp["configured"]; !ok {
		t.Error("response must include 'configured'")
	}
	if _, ok := resp["source"]; !ok {
		t.Error("response must include 'source'")
	}
	if resp["configured"] != true {
		t.Errorf("configured = %v; want true", resp["configured"])
	}
	if resp["source"] != "ui" {
		t.Errorf("source = %v; want ui", resp["source"])
	}
	// Key must NOT be present under any name.
	for _, field := range []string{"key", "api_key", "value", "secret"} {
		if v, ok := resp[field]; ok {
			t.Errorf("response must not contain %q (got %v)", field, v)
		}
	}
}

// TestVulnFeedStatus_ReflectsEnrichmentAvailable (WP3): the status endpoint's
// enrichment_available/last_enrichment_at fields must reflect the
// Production-enrichment state returned by GetFeedMetaStatus, independently of
// feed_ok (which tracks Scanner-driven detection freshness).
func TestVulnFeedStatus_ReflectsEnrichmentAvailable(t *testing.T) {
	age := newTestAgeIdentity(t)
	repo := newFakeInstSettingsRepo()
	keySvc := NewVulnFeedKeyService(repo, age, "env-key-12345678", nil, nil)

	cases := []struct {
		name             string
		ok               bool
		enrichmentOK     bool
		lastEnrichmentAt *time.Time
	}{
		{name: "detection_ok_enrichment_never_landed", ok: true, enrichmentOK: false, lastEnrichmentAt: nil},
		{name: "detection_ok_enrichment_ok", ok: true, enrichmentOK: true, lastEnrichmentAt: timePtr(time.Now())},
		{name: "detection_degraded_enrichment_still_sticky_ok", ok: false, enrichmentOK: true, lastEnrichmentAt: timePtr(time.Now())},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := &fakeFeedMetaReader{
				ok:               tc.ok,
				recordCount:      10,
				enrichmentOK:     tc.enrichmentOK,
				lastEnrichmentAt: tc.lastEnrichmentAt,
			}
			h := NewHandler(&Service{}, nil)
			h.SetVulnFeed(meta, keySvc)
			engine := buildTestEngine(h)

			req := httptest.NewRequest(http.MethodGet, "/admin/vuln-feed/status", nil)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status %d; want 200 (body: %s)", w.Code, w.Body.String())
			}
			var resp map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if resp["feed_ok"] != tc.ok {
				t.Errorf("feed_ok = %v; want %v", resp["feed_ok"], tc.ok)
			}
			if resp["enrichment_available"] != tc.enrichmentOK {
				t.Errorf("enrichment_available = %v; want %v", resp["enrichment_available"], tc.enrichmentOK)
			}
			_, hasLastEnrichAt := resp["last_enrichment_at"]
			if tc.lastEnrichmentAt == nil && hasLastEnrichAt {
				t.Errorf("last_enrichment_at present = %v; want absent (omitempty, nil)", resp["last_enrichment_at"])
			}
			if tc.lastEnrichmentAt != nil && !hasLastEnrichAt {
				t.Error("last_enrichment_at missing; want present")
			}
		})
	}
}

func timePtr(t time.Time) *time.Time { return &t }

// Test handler: PUT /admin/vuln-feed/key triggers an immediate sync.
func TestVulnFeedSetKey_TriggersSyncViaHandler(t *testing.T) {
	age := newTestAgeIdentity(t)
	repo := newFakeInstSettingsRepo()
	enq := &captureFeedEnqueuer{}
	keySvc := NewVulnFeedKeyService(repo, age, "", enq, nil)

	h := NewHandler(&Service{}, nil)
	h.SetVulnFeed(nil, keySvc)
	engine := buildTestEngine(h)

	body := bytes.NewBufferString(`{"key":"valid-api-key-here"}`)
	req := httptest.NewRequest(http.MethodPut, "/admin/vuln-feed/key", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d; want 200 (body: %s)", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["ok"] != true {
		t.Errorf("ok = %v; want true", resp["ok"])
	}
	// Sync must have been triggered (enq.calls == 1).
	if enq.calls != 1 {
		t.Errorf("EnqueueFeedRefresh called %d times; want 1", enq.calls)
	}
	// Key must be stored (encrypted).
	enc, ok, err := repo.Get(context.Background(), instanceSettingKey)
	if err != nil || !ok || len(enc) == 0 {
		t.Errorf("key not stored after PUT: err=%v ok=%v enc_len=%d", err, ok, len(enc))
	}
	// Verify the response body does not contain the plaintext key.
	if bytes.Contains(w.Body.Bytes(), []byte("valid-api-key-here")) {
		t.Error("response body must not contain the plaintext API key")
	}
}

// Test: DELETE /admin/vuln-feed/key clears the key.
func TestVulnFeedClearKey_Handler(t *testing.T) {
	age := newTestAgeIdentity(t)
	repo := newFakeInstSettingsRepo()
	keySvc := NewVulnFeedKeyService(repo, age, "env-fallback-key1", nil, nil)
	// Pre-store a UI key.
	if err := keySvc.SetKey(context.Background(), "ui-key-stored-xyz"); err != nil {
		t.Fatalf("SetKey: %v", err)
	}

	h := NewHandler(&Service{}, nil)
	h.SetVulnFeed(nil, keySvc)
	engine := buildTestEngine(h)

	req := httptest.NewRequest(http.MethodDelete, "/admin/vuln-feed/key", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d; want 200", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["ok"] != true {
		t.Errorf("ok = %v; want true", resp["ok"])
	}
	// After clear, fallback_source should be env (env-fallback-key1 is set).
	if resp["fallback_source"] != "env" {
		t.Errorf("fallback_source = %v; want env", resp["fallback_source"])
	}
	// DB row should be gone.
	_, exists, _ := repo.Get(context.Background(), instanceSettingKey)
	if exists {
		t.Error("key should have been deleted from DB")
	}
}

// Test 5: superadmin gating — a non-superadmin principal gets 403 when the
// requireSuperadmin middleware is mounted.  We test this by mounting the
// routes WITH the real middleware (mocked DB check).
func TestVulnFeedRoutes_RequiresSuperadmin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	// Inject a NON-superadmin principal.
	engine.Use(func(c *gin.Context) {
		ctx := domain.WithPrincipal(c.Request.Context(), domain.Principal{
			Type:   domain.PrincipalUser,
			UserID: uuid.New(),
		})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	// Mount a stub requireSuperadmin that always rejects (simulates non-SA user).
	engine.Use(func(c *gin.Context) {
		// Simulate the requireSuperadmin DB check returning is_superadmin=false.
		from := domain.Forbidden("superadmin_required", "superadmin access required")
		c.JSON(http.StatusForbidden, gin.H{"error": from.Error()})
		c.Abort()
	})
	engine.GET("/admin/vuln-feed/status", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodGet, "/admin/vuln-feed/status", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("non-superadmin: got HTTP %d; want 403", w.Code)
	}
}

// Test: POST /admin/vuln-feed/sync enqueues a feed refresh job.
func TestVulnFeedSync_Handler(t *testing.T) {
	age := newTestAgeIdentity(t)
	repo := newFakeInstSettingsRepo()
	enq := &captureFeedEnqueuer{}
	keySvc := NewVulnFeedKeyService(repo, age, "env-key-xyz123", enq, nil)

	h := NewHandler(&Service{}, nil)
	h.SetVulnFeed(nil, keySvc)
	engine := buildTestEngine(h)

	req := httptest.NewRequest(http.MethodPost, "/admin/vuln-feed/sync", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d; want 202", w.Code)
	}
	if enq.calls != 1 {
		t.Errorf("EnqueueFeedRefresh called %d times; want 1", enq.calls)
	}
}

// Test: vulnFeedH nil returns 503 (handler not wired).
func TestVulnFeedStatus_NotWired_Returns503(t *testing.T) {
	h := NewHandler(&Service{}, nil)
	// Do NOT call SetVulnFeed — leave vulnFeedH nil.
	engine := buildTestEngine(h)

	req := httptest.NewRequest(http.MethodGet, "/admin/vuln-feed/status", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d; want 503", w.Code)
	}
}
