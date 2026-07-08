package perf

// agent_configack_test.go — tests for the GH #174 PINNED config-ack contract:
// POST /agent/v1/perf/config-ack carries an OPTIONAL boolean
// `rum_beacon_present`. The agent sends true iff it currently holds a
// non-empty rum_beacon_key. The CP NEVER accepts (or could accept — the field
// does not exist on the wire contract) the plaintext key back on this
// endpoint; only this boolean.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agent"
)

// TestConfigAck_RumBeaconPresentAbsent_NoSignalNoReconcile verifies a
// pre-#174 agent that omits rum_beacon_present entirely: MarkConfigApplied
// treats this as "no signal" — it never calls UpdateBeaconKeyAcked and never
// enqueues a reconcile, even on a hash-set rum-enabled site.
func TestConfigAck_RumBeaconPresentAbsent_NoSignalNoReconcile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &fakeRepo{config: Config{RumEnabled: true, BeaconKeySet: true}, configFound: true}
	enq := &fakeReconcileEnqueuer{}
	svc := NewService(repo, nil, nil, nil)
	svc.SetRumBeaconReconcileEnqueuer(enq)
	h := NewAgentHandler(svc, nil, nil)

	eng := gin.New()
	siteID, tenantID := uuid.New(), uuid.New()
	id := agent.Identity{SiteID: siteID, TenantID: tenantID}
	eng.POST("/agent/v1/perf/config-ack", withIdentity(id, h.configAck))

	body := `{"config_version":1,"server_software":"nginx","dropin_installed":true,"wp_cache_constant_set":true,"htaccess_managed":true}`
	req := httptest.NewRequest(http.MethodPost, "/agent/v1/perf/config-ack", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	eng.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	repo.mu.Lock()
	ackedCalls := len(repo.beaconAckedCalls)
	repo.mu.Unlock()
	if ackedCalls != 0 {
		t.Errorf("UpdateBeaconKeyAcked must not be called when rum_beacon_present is absent, got %d calls", ackedCalls)
	}
	if got := enq.count(); got != 0 {
		t.Errorf("reconcile enqueue count = %d, want 0", got)
	}
}

// TestConfigAck_RumBeaconPresentFalse_TriggersReconcile verifies the wire
// contract end-to-end: an agent explicitly reporting rum_beacon_present:false
// on an already hash-set, rum-enabled site triggers the ack-based reconcile.
func TestConfigAck_RumBeaconPresentFalse_TriggersReconcile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &fakeRepo{config: Config{RumEnabled: true, BeaconKeySet: true}, configFound: true}
	enq := &fakeReconcileEnqueuer{}
	svc := NewService(repo, nil, nil, nil)
	svc.SetRumBeaconReconcileEnqueuer(enq)
	h := NewAgentHandler(svc, nil, nil)

	eng := gin.New()
	siteID, tenantID := uuid.New(), uuid.New()
	id := agent.Identity{SiteID: siteID, TenantID: tenantID}
	eng.POST("/agent/v1/perf/config-ack", withIdentity(id, h.configAck))

	body, _ := json.Marshal(map[string]any{
		"config_version":        2,
		"server_software":       "nginx",
		"dropin_installed":      true,
		"wp_cache_constant_set": true,
		"htaccess_managed":      true,
		"rum_beacon_present":    false,
	})
	req := httptest.NewRequest(http.MethodPost, "/agent/v1/perf/config-ack", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	eng.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	repo.mu.Lock()
	ackedCalls := append([]bool(nil), repo.beaconAckedCalls...)
	repo.mu.Unlock()
	if len(ackedCalls) != 1 || ackedCalls[0] != false {
		t.Fatalf("beaconAckedCalls = %v, want [false]", ackedCalls)
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
}

// TestConfigAck_RumBeaconPresentTrue_NoReconcile verifies present=true is a
// healthy signal that never enqueues a reconcile.
func TestConfigAck_RumBeaconPresentTrue_NoReconcile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &fakeRepo{config: Config{RumEnabled: true, BeaconKeySet: true}, configFound: true}
	enq := &fakeReconcileEnqueuer{}
	svc := NewService(repo, nil, nil, nil)
	svc.SetRumBeaconReconcileEnqueuer(enq)
	h := NewAgentHandler(svc, nil, nil)

	eng := gin.New()
	siteID, tenantID := uuid.New(), uuid.New()
	id := agent.Identity{SiteID: siteID, TenantID: tenantID}
	eng.POST("/agent/v1/perf/config-ack", withIdentity(id, h.configAck))

	body, _ := json.Marshal(map[string]any{
		"config_version":     3,
		"rum_beacon_present": true,
	})
	req := httptest.NewRequest(http.MethodPost, "/agent/v1/perf/config-ack", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	eng.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if got := enq.count(); got != 0 {
		t.Fatalf("reconcile enqueue count = %d, want 0", got)
	}
}

// TestConfigAckBody_NoPlaintextKeyField is a SECURITY regression guard: the
// wire contract has no field capable of carrying a plaintext beacon key back
// from the agent. Even if a compromised/buggy agent sends one under any
// plausible field name, it is silently dropped by json.Unmarshal (Go ignores
// unknown JSON fields by default) and never reaches configAckBody.
// RumBeaconPresent — the ONLY beacon-related field this struct exposes — stays
// exactly what a well-behaved agent sent.
func TestConfigAckBody_NoPlaintextKeyField(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &fakeRepo{config: Config{RumEnabled: true, BeaconKeySet: true}, configFound: true}
	svc := NewService(repo, nil, nil, nil)
	h := NewAgentHandler(svc, nil, nil)

	eng := gin.New()
	siteID, tenantID := uuid.New(), uuid.New()
	id := agent.Identity{SiteID: siteID, TenantID: tenantID}
	eng.POST("/agent/v1/perf/config-ack", withIdentity(id, h.configAck))

	// A malicious/buggy agent attempts to smuggle a plaintext key back on the
	// ack. None of these field names exist on configAckBody, so they must be
	// silently ignored, never surfaced anywhere, and never cause an error.
	body, _ := json.Marshal(map[string]any{
		"config_version":     4,
		"rum_beacon_present": true,
		"rum_beacon_key":     "smuggled-plaintext-should-be-ignored",
		"beacon_key":         "also-smuggled",
	})
	req := httptest.NewRequest(http.MethodPost, "/agent/v1/perf/config-ack", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	eng.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("smuggled")) {
		t.Errorf("response must never echo back any part of the request body; got %s", rec.Body.String())
	}
}
