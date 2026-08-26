package perf

// cdn_creds_carryforward_test.go — GH #522: toggling site cache silently
// destroyed a site's stored CDN credentials and reported success.
//
// The three carry-forward call sites (EnableCache, DisableCache and the
// beacon-key rotate, whose comment said it "mirrors EnableCache/DisableCache
// exactly") discarded the error from GetCDNCredentialsCiphertext:
//
//	ct, _, _ := s.repo.GetCDNCredentialsCiphertext(ctx, tenantID, siteID)
//
// The repo returns (nil, "", err) on a genuine failure, so a read error was
// indistinguishable from "no credentials stored". The nil then flowed into an
// UPSERT that writes cdn_credentials_encrypted unconditionally
// (db/query/perf.sql), replacing the age-encrypted blob with NULL while the
// endpoint reported success.
//
// These tests pin BOTH directions:
//   - a FAILED read aborts the write and the stored ciphertext survives;
//   - a healthy read (with credentials, and with none) still toggles, keeps
//     the credentials, and is never treated as an error.
//
// The fakeRepo's UpsertConfig assigns r.ciphertext = in.CDNCredentialsEncrypted,
// which is exactly the unconditional column write the real SQL performs — so
// "the stored ciphertext survived" is a real assertion here, not a stub.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// errCredRead stands in for a genuine infra failure inside the tenant tx
// (pool exhausted, query error, RLS-scoped tx failure) — NOT ErrNotFound and
// NOT "no credentials configured".
var errCredRead = errors.New("tenant tx failed: connection reset")

// storedCreds is a stand-in for the age-encrypted CDN credentials blob.
var storedCreds = []byte("age-encrypted-cdn-credentials")

func credsRepo(t *testing.T, tenantID, siteID uuid.UUID, ct []byte, readErr error) *fakeRepo {
	t.Helper()
	return &fakeRepo{
		config:        Config{TenantID: tenantID, SiteID: siteID, ConfigVersion: 7, RumEnabled: true, BeaconKeySet: true},
		configFound:   true,
		ciphertext:    ct,
		provider:      "cloudflare",
		ciphertextErr: readErr,
	}
}

func (r *fakeRepo) storedCiphertext() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ciphertext
}

func (r *fakeRepo) upsertCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.upserts)
}

func (r *fakeRepo) lastUpsert(t *testing.T) UpsertConfigInput {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.upserts) == 0 {
		t.Fatal("expected an UpsertConfig call, got none")
	}
	return r.upserts[len(r.upserts)-1]
}

// ---------------------------------------------------------------------------
// RED: a failed credentials read must abort the write (all three call sites)
// ---------------------------------------------------------------------------

// TestEnableCache_CredentialsReadFails_AbortsWriteAndKeepsCiphertext is the
// regression for service.go EnableCache. Before the fix this returned
// ("cache enabled", nil) after NULLing the stored ciphertext.
func TestEnableCache_CredentialsReadFails_AbortsWriteAndKeepsCiphertext(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := credsRepo(t, tenantID, siteID, storedCreds, errCredRead)
	svc := NewService(repo, nil, &fakeEvents{}, nil)

	detail, err := svc.EnableCache(context.Background(), tenantID, siteID)
	if err == nil {
		t.Fatalf("EnableCache must FAIL when the credentials read fails; got detail=%q, err=nil", detail)
	}
	if !errors.Is(err, errCredRead) {
		t.Errorf("error must carry the underlying read failure; got %v", err)
	}
	if detail == "cache enabled" {
		t.Error("a failed read must never report success")
	}
	if n := repo.upsertCount(); n != 0 {
		t.Errorf("no config write may happen after a failed credentials read; got %d upserts", n)
	}
	if got := repo.storedCiphertext(); string(got) != string(storedCreds) {
		t.Fatalf("stored CDN ciphertext was destroyed: got %q, want %q", got, storedCreds)
	}
}

// TestDisableCache_CredentialsReadFails_AbortsWriteAndKeepsCiphertext is the
// regression for service.go DisableCache.
func TestDisableCache_CredentialsReadFails_AbortsWriteAndKeepsCiphertext(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := credsRepo(t, tenantID, siteID, storedCreds, errCredRead)
	svc := NewService(repo, nil, &fakeEvents{}, nil)

	detail, err := svc.DisableCache(context.Background(), tenantID, siteID)
	if err == nil {
		t.Fatalf("DisableCache must FAIL when the credentials read fails; got detail=%q, err=nil", detail)
	}
	if !errors.Is(err, errCredRead) {
		t.Errorf("error must carry the underlying read failure; got %v", err)
	}
	if detail == "cache disabled" {
		t.Error("a failed read must never report success")
	}
	if n := repo.upsertCount(); n != 0 {
		t.Errorf("no config write may happen after a failed credentials read; got %d upserts", n)
	}
	if got := repo.storedCiphertext(); string(got) != string(storedCreds) {
		t.Fatalf("stored CDN ciphertext was destroyed: got %q, want %q", got, storedCreds)
	}
}

// TestRotateBeaconKey_CredentialsReadFails_AbortsWriteAndKeepsCiphertext is the
// regression for the THIRD site (service.go RotateBeaconKey), which copied the
// pattern deliberately. It also pins that the read happens BEFORE the mint, so
// the failure cannot leave a rotated-but-unpushed hash behind.
func TestRotateBeaconKey_CredentialsReadFails_AbortsWriteAndKeepsCiphertext(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := credsRepo(t, tenantID, siteID, storedCreds, errCredRead)
	rotator := &fakeBeaconKeyRotator{}
	svc := NewService(repo, nil, &fakeEvents{}, nil)
	svc.SetAgentClient(&recordingAgent{}, &fakeSites{url: "https://example.com"})
	svc.SetBeaconKeyRepo(rotator, "https://manage.example.com")

	err := svc.RotateBeaconKey(context.Background(), tenantID, siteID)
	if err == nil {
		t.Fatal("RotateBeaconKey must FAIL when the credentials read fails")
	}
	if !errors.Is(err, errCredRead) {
		t.Errorf("error must carry the underlying read failure; got %v", err)
	}
	if n := repo.upsertCount(); n != 0 {
		t.Errorf("no config write may happen after a failed credentials read; got %d upserts", n)
	}
	if got := repo.storedCiphertext(); string(got) != string(storedCreds) {
		t.Fatalf("stored CDN ciphertext was destroyed: got %q, want %q", got, storedCreds)
	}
	if n := rotator.calls(); n != 0 {
		t.Errorf("the hash must not be rotated when a precondition read failed; got %d rotate calls", n)
	}
}

// TestEnableCacheEndpoint_CredentialsReadFails_DoesNotReportSuccess drives the
// real HTTP route (permission + site-access middleware included) and pins that
// the response no longer says "cache enabled" while the credentials survive.
// These two routes report non-domain failures as 200 {"ok":false,...} by house
// convention; what matters is that ok is false, the detail is the failure, and
// nothing was written.
func TestEnableCacheEndpoint_CredentialsReadFails_DoesNotReportSuccess(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := credsRepo(t, tenantID, siteID, storedCreds, errCredRead)
	svc := NewService(repo, nil, &fakeEvents{}, nil)

	p := domain.Principal{TenantID: tenantID, Role: string(authz.RoleOperator)}
	engine := buildRotateKeyEngine(t, svc, p)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sites/"+siteID.String()+"/perf/cache/enable", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, `"ok":true`) || strings.Contains(body, "cache enabled") {
		t.Fatalf("endpoint reported success over a failed credentials read; body = %s", body)
	}
	if !strings.Contains(body, `"ok":false`) {
		t.Fatalf("expected ok:false; body = %s", body)
	}
	if n := repo.upsertCount(); n != 0 {
		t.Errorf("no config write may happen; got %d upserts", n)
	}
	if got := repo.storedCiphertext(); string(got) != string(storedCreds) {
		t.Fatalf("stored CDN ciphertext was destroyed: got %q, want %q", got, storedCreds)
	}
}

// ---------------------------------------------------------------------------
// OVER-FIRE: a healthy read must still toggle, and keep the credentials
// ---------------------------------------------------------------------------

// TestEnableCache_HealthyRead_TogglesAndKeepsCredentials verifies the fix does
// not block correct work: a site WITH stored credentials still enables cache,
// and the ciphertext is carried forward byte-for-byte.
func TestEnableCache_HealthyRead_TogglesAndKeepsCredentials(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := credsRepo(t, tenantID, siteID, storedCreds, nil)
	svc := NewService(repo, nil, &fakeEvents{}, nil)

	detail, err := svc.EnableCache(context.Background(), tenantID, siteID)
	if err != nil {
		t.Fatalf("EnableCache with a healthy read must succeed: %v", err)
	}
	if detail != "cache enabled" {
		t.Errorf("detail = %q, want %q", detail, "cache enabled")
	}
	in := repo.lastUpsert(t)
	if !in.Config.CacheEnabled {
		t.Error("cache_enabled must be true after EnableCache")
	}
	if in.Config.ConfigVersion != 8 {
		t.Errorf("config_version = %d, want 8 (bumped from 7)", in.Config.ConfigVersion)
	}
	if string(in.CDNCredentialsEncrypted) != string(storedCreds) {
		t.Errorf("carried-forward ciphertext = %q, want %q", in.CDNCredentialsEncrypted, storedCreds)
	}
	if got := repo.storedCiphertext(); string(got) != string(storedCreds) {
		t.Fatalf("stored CDN ciphertext changed: got %q, want %q", got, storedCreds)
	}
}

// TestDisableCache_HealthyRead_TogglesAndKeepsCredentials — same, for disable.
func TestDisableCache_HealthyRead_TogglesAndKeepsCredentials(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := credsRepo(t, tenantID, siteID, storedCreds, nil)
	repo.config.CacheEnabled = true
	svc := NewService(repo, nil, &fakeEvents{}, nil)

	detail, err := svc.DisableCache(context.Background(), tenantID, siteID)
	if err != nil {
		t.Fatalf("DisableCache with a healthy read must succeed: %v", err)
	}
	if detail != "cache disabled" {
		t.Errorf("detail = %q, want %q", detail, "cache disabled")
	}
	in := repo.lastUpsert(t)
	if in.Config.CacheEnabled {
		t.Error("cache_enabled must be false after DisableCache")
	}
	if string(in.CDNCredentialsEncrypted) != string(storedCreds) {
		t.Errorf("carried-forward ciphertext = %q, want %q", in.CDNCredentialsEncrypted, storedCreds)
	}
	if got := repo.storedCiphertext(); string(got) != string(storedCreds) {
		t.Fatalf("stored CDN ciphertext changed: got %q, want %q", got, storedCreds)
	}
}

// TestRotateBeaconKey_HealthyRead_RotatesAndKeepsCredentials — same, for the
// third site: the rotate still mints, pushes and carries the ciphertext.
func TestRotateBeaconKey_HealthyRead_RotatesAndKeepsCredentials(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	repo := credsRepo(t, tenantID, siteID, storedCreds, nil)
	rotator := &fakeBeaconKeyRotator{}
	svc := NewService(repo, nil, &fakeEvents{}, nil)
	svc.SetAgentClient(&recordingAgent{}, &fakeSites{url: "https://example.com"})
	svc.SetBeaconKeyRepo(rotator, "https://manage.example.com")

	if err := svc.RotateBeaconKey(context.Background(), tenantID, siteID); err != nil {
		t.Fatalf("RotateBeaconKey with a healthy read must succeed: %v", err)
	}
	if n := rotator.calls(); n != 1 {
		t.Errorf("rotate calls = %d, want 1", n)
	}
	in := repo.lastUpsert(t)
	if string(in.CDNCredentialsEncrypted) != string(storedCreds) {
		t.Errorf("carried-forward ciphertext = %q, want %q", in.CDNCredentialsEncrypted, storedCreds)
	}
	if got := repo.storedCiphertext(); string(got) != string(storedCreds) {
		t.Fatalf("stored CDN ciphertext changed: got %q, want %q", got, storedCreds)
	}
}

// TestCarryForward_NoCredentialsStored_IsNeverAnError is the other over-fire
// direction, and the one that would break every site without a CDN configured:
// a genuine "no credentials" read returns (nil, "", nil) and must stay a
// success on all three paths, writing a nil ciphertext as before.
func TestCarryForward_NoCredentialsStored_IsNeverAnError(t *testing.T) {
	t.Run("enable", func(t *testing.T) {
		tenantID, siteID := uuid.New(), uuid.New()
		repo := credsRepo(t, tenantID, siteID, nil, nil)
		svc := NewService(repo, nil, &fakeEvents{}, nil)

		detail, err := svc.EnableCache(context.Background(), tenantID, siteID)
		if err != nil {
			t.Fatalf("a site with NO CDN credentials must still enable cache: %v", err)
		}
		if detail != "cache enabled" {
			t.Errorf("detail = %q, want %q", detail, "cache enabled")
		}
		if in := repo.lastUpsert(t); in.CDNCredentialsEncrypted != nil {
			t.Errorf("ciphertext = %q, want nil", in.CDNCredentialsEncrypted)
		}
	})

	t.Run("disable", func(t *testing.T) {
		tenantID, siteID := uuid.New(), uuid.New()
		repo := credsRepo(t, tenantID, siteID, nil, nil)
		svc := NewService(repo, nil, &fakeEvents{}, nil)

		detail, err := svc.DisableCache(context.Background(), tenantID, siteID)
		if err != nil {
			t.Fatalf("a site with NO CDN credentials must still disable cache: %v", err)
		}
		if detail != "cache disabled" {
			t.Errorf("detail = %q, want %q", detail, "cache disabled")
		}
		if in := repo.lastUpsert(t); in.CDNCredentialsEncrypted != nil {
			t.Errorf("ciphertext = %q, want nil", in.CDNCredentialsEncrypted)
		}
	})

	t.Run("rotate", func(t *testing.T) {
		tenantID, siteID := uuid.New(), uuid.New()
		repo := credsRepo(t, tenantID, siteID, nil, nil)
		rotator := &fakeBeaconKeyRotator{}
		svc := NewService(repo, nil, &fakeEvents{}, nil)
		svc.SetAgentClient(&recordingAgent{}, &fakeSites{url: "https://example.com"})
		svc.SetBeaconKeyRepo(rotator, "https://manage.example.com")

		if err := svc.RotateBeaconKey(context.Background(), tenantID, siteID); err != nil {
			t.Fatalf("a site with NO CDN credentials must still rotate: %v", err)
		}
		if n := rotator.calls(); n != 1 {
			t.Errorf("rotate calls = %d, want 1", n)
		}
		if in := repo.lastUpsert(t); in.CDNCredentialsEncrypted != nil {
			t.Errorf("ciphertext = %q, want nil", in.CDNCredentialsEncrypted)
		}
	})
}
