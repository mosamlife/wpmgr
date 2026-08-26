package perf

// cdn_creds_errormap_test.go — Greptile finding on PR #533 (GH #522 follow-up):
// resolveCDNCiphertext's carry-forward branch returned the bare repo error
// from GetCDNCredentialsCiphertext unchanged. EnableCache/DisableCache render
// non-domain errors as HTTP 200 {"ok":false,"detail":err.Error()}, so an
// operator saw e.g. `detail = "tenant tx failed: connection reset"` —
// persistence-layer internals, indistinguishable from an ordinary rejection.
//
// These tests pin:
//   - the raw string never reaches the client, on all three operator-facing
//     call sites (EnableCache, DisableCache, RotateBeaconKey);
//   - the raw diagnostic IS still captured, once, in the server log;
//   - a genuine domain rejection (crypto unwired, validation) is untouched;
//   - an agent-side refusal still surfaces with its own wording, unchanged;
//   - the healthy/success path is untouched (already covered by
//     cdn_creds_carryforward_test.go's "HealthyRead" cases).

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

const wantSanitizedMsg = "Could not read this site's stored CDN credentials, so nothing was changed. Your stored credentials are intact — please try again in a moment."

// errAgentRefused stands in for the agent's own rejection wording (e.g. "cache
// directory not writable"), NOT a CDN-credentials failure — this fix must
// never touch this path.
var errAgentRefused = errors.New("agent refused: cache directory not writable")

// bufLogger returns a *slog.Logger writing to buf so a test can assert on the
// server-side log line without touching stdout/stderr.
func bufLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// ---------------------------------------------------------------------------
// RED/GREEN: the raw repo error must never reach the caller, on all three
// operator-facing call sites, and must still be logged once, raw.
// ---------------------------------------------------------------------------

func TestEnableCache_CredentialsReadFails_SanitizesErrorAndLogsRaw(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := credsRepo(t, tenantID, siteID, storedCreds, errCredRead)
	logger, logBuf := bufLogger()
	svc := NewService(repo, nil, &fakeEvents{}, logger)

	_, err := svc.EnableCache(context.Background(), tenantID, siteID)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("raw repo error leaked into the returned error: %v", err)
	}
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("expected a domain error, got %T: %v", err, err)
	}
	if de.Code != "perf_cdn_credentials_read_failed" {
		t.Errorf("code = %q, want %q", de.Code, "perf_cdn_credentials_read_failed")
	}
	if de.Message != wantSanitizedMsg {
		t.Errorf("message = %q, want %q", de.Message, wantSanitizedMsg)
	}
	if status := domain.HTTPStatus(err); status != http.StatusServiceUnavailable {
		t.Errorf("HTTP status = %d, want %d", status, http.StatusServiceUnavailable)
	}
	if !strings.Contains(logBuf.String(), "connection reset") {
		t.Errorf("raw diagnostic must still be logged; log = %q", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), siteID.String()) {
		t.Errorf("log line must carry the site id; log = %q", logBuf.String())
	}
}

func TestDisableCache_CredentialsReadFails_SanitizesErrorAndLogsRaw(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := credsRepo(t, tenantID, siteID, storedCreds, errCredRead)
	repo.config.CacheEnabled = true
	logger, logBuf := bufLogger()
	svc := NewService(repo, nil, &fakeEvents{}, logger)

	_, err := svc.DisableCache(context.Background(), tenantID, siteID)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("raw repo error leaked into the returned error: %v", err)
	}
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("expected a domain error, got %T: %v", err, err)
	}
	if de.Code != "perf_cdn_credentials_read_failed" {
		t.Errorf("code = %q, want %q", de.Code, "perf_cdn_credentials_read_failed")
	}
	if !strings.Contains(logBuf.String(), "connection reset") {
		t.Errorf("raw diagnostic must still be logged; log = %q", logBuf.String())
	}
}

func TestRotateBeaconKey_CredentialsReadFails_SanitizesErrorAndLogsRaw(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := credsRepo(t, tenantID, siteID, storedCreds, errCredRead)
	logger, logBuf := bufLogger()
	svc := NewService(repo, nil, &fakeEvents{}, logger)
	svc.SetAgentClient(&recordingAgent{}, &fakeSites{url: "https://example.com"})
	svc.SetBeaconKeyRepo(&fakeBeaconKeyRotator{}, "https://manage.example.com")

	err := svc.RotateBeaconKey(context.Background(), tenantID, siteID)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("raw repo error leaked into the returned error: %v", err)
	}
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("expected a domain error, got %T: %v", err, err)
	}
	if de.Code != "perf_cdn_credentials_read_failed" {
		t.Errorf("code = %q, want %q", de.Code, "perf_cdn_credentials_read_failed")
	}
	if !strings.Contains(logBuf.String(), "connection reset") {
		t.Errorf("raw diagnostic must still be logged; log = %q", logBuf.String())
	}
}

// TestEnableCacheEndpoint_CredentialsReadFails_ResponseIsSanitized drives the
// real HTTP route end to end and pins that the response body the operator
// actually receives contains the sanitized message and NOT the raw string.
func TestEnableCacheEndpoint_CredentialsReadFails_ResponseIsSanitized(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := credsRepo(t, tenantID, siteID, storedCreds, errCredRead)
	svc := NewService(repo, nil, &fakeEvents{}, nil)

	p := domain.Principal{TenantID: tenantID, Role: string(authz.RoleOperator)}
	engine := buildRotateKeyEngine(t, svc, p)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sites/"+siteID.String()+"/perf/cache/enable", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "connection reset") {
		t.Fatalf("raw repo error leaked into the HTTP response; body = %s", body)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusServiceUnavailable, body)
	}
	if !strings.Contains(body, "perf_cdn_credentials_read_failed") {
		t.Fatalf("expected the stable code in the envelope; body = %s", body)
	}
	if !strings.Contains(body, "please try again in a moment") {
		t.Fatalf("expected the operator-facing sanitized message; body = %s", body)
	}
}

// ---------------------------------------------------------------------------
// OVER-FIRE: this must not flatten every failure into one opaque sentence.
// ---------------------------------------------------------------------------

// TestUpdateConfig_CryptoUnwired_MessageUnchanged: the raw != nil branch of
// resolveCDNCiphertext (encrypting freshly supplied credentials) is untouched
// by this fix — its existing domain error must read exactly as before.
func TestUpdateConfig_CryptoUnwired_MessageUnchanged(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := credsRepo(t, tenantID, siteID, nil, nil)
	svc := NewService(repo, nil, &fakeEvents{}, nil) // decryptor: nil

	_, err := svc.UpdateConfig(context.Background(), tenantID, siteID, UpdateConfigInput{
		Config:            Config{CacheRefreshInterval: "2hours", JSDelayMethod: "defer"},
		CDNCredentialsRaw: &CDNCredentials{APIToken: "tok"},
	})
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("expected a domain error, got %T: %v", err, err)
	}
	if de.Code != "perf_crypto_unwired" {
		t.Errorf("code = %q, want %q (unchanged by this fix)", de.Code, "perf_crypto_unwired")
	}
	if de.Message != "credential encryption is not configured" {
		t.Errorf("message = %q, want unchanged %q", de.Message, "credential encryption is not configured")
	}
}

// TestUpdateConfig_InvalidRefreshInterval_MessageUnchanged: an ordinary
// validation rejection, unrelated to CDN credentials at all, must read
// exactly as before.
func TestUpdateConfig_InvalidRefreshInterval_MessageUnchanged(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := credsRepo(t, tenantID, siteID, nil, nil)
	svc := NewService(repo, nil, &fakeEvents{}, nil)

	_, err := svc.UpdateConfig(context.Background(), tenantID, siteID, UpdateConfigInput{
		Config: Config{CacheRefreshInterval: "bogus-interval", JSDelayMethod: "defer"},
	})
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("expected a domain error, got %T: %v", err, err)
	}
	if de.Code != "invalid_cache_refresh_interval" {
		t.Errorf("code = %q, want %q (unchanged by this fix)", de.Code, "invalid_cache_refresh_interval")
	}
	if !strings.Contains(de.Message, "bogus-interval") {
		t.Errorf("message = %q, want it to still name the rejected value", de.Message)
	}
}

// TestEnableCacheEndpoint_AgentRefusal_UnchangedDetail: an agent-side refusal
// (a non-domain error from the agent push, downstream of a HEALTHY
// credentials read) must keep surfacing exactly as before — HTTP 200
// {"ok":false,"detail":"<agent's own wording>"} — because this fix only
// touches resolveCDNCiphertext's error path, not the agent push's.
func TestEnableCacheEndpoint_AgentRefusal_UnchangedDetail(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := credsRepo(t, tenantID, siteID, storedCreds, nil) // healthy read
	svc := NewService(repo, nil, &fakeEvents{}, nil)
	svc.SetAgentClient(&fakeAgent{
		cacheEnableErr: errAgentRefused,
	}, &fakeSites{url: "https://example.com"})

	p := domain.Principal{TenantID: tenantID, Role: string(authz.RoleOperator)}
	engine := buildRotateKeyEngine(t, svc, p)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sites/"+siteID.String()+"/perf/cache/enable", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("agent refusal must still be a 200 {ok:false} envelope; status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"ok":false`) {
		t.Fatalf("expected ok:false; body = %s", body)
	}
	if !strings.Contains(body, errAgentRefused.Error()) {
		t.Fatalf("agent's own wording must pass through unchanged; body = %s", body)
	}
}
