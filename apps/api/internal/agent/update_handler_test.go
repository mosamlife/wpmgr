package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/blobstore"
)

// fakeManifestStore is a ManifestStore double.
type fakeManifestStore struct {
	getErr        error
	body          []byte
	presignURL    string
	presignErr    error
	gotPresignKey string
}

func (f *fakeManifestStore) GetViaPresign(_ context.Context, _ string) (io.ReadCloser, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return io.NopCloser(bytes.NewReader(f.body)), nil
}

func (f *fakeManifestStore) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	f.gotPresignKey = key
	if f.presignErr != nil {
		return "", f.presignErr
	}
	return f.presignURL, nil
}

const goodSHA = "4896fd2f4cc3a7d10d16d12566a81a37ba3d278d2300663bb500f632fe309955"

func validLatestJSON(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(releaseManifest{
		Slug:             "wpmgr-agent",
		Plugin:           "wpmgr-agent/wpmgr-agent.php",
		Version:          "0.10.6-test",
		MinVersion:       "0.0.0",
		PackageObjectKey: "agent-releases/0.10.6-test/wpmgr-agent.zip",
		PackageSHA256:    goodSHA,
		PackageSize:      359578,
		Requires:         "6.0",
		RequiresPHP:      "8.1",
		Tested:           "6.8",
		Sections:         map[string]string{"description": "WPMgr Agent."},
	})
	if err != nil {
		t.Fatalf("marshal latest.json: %v", err)
	}
	return b
}

// newTestSigner returns a Signer + its public key for verification.
func newTestSigner(t *testing.T) (*agentcmd.Signer, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	signer, err := agentcmd.NewSigner(base64.StdEncoding.EncodeToString(priv))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return signer, pub
}

// callManifest drives the handler through a real gin engine (so gin flushes the
// status/body to the recorder exactly as in production — a direct handler call
// leaves a body-less c.Status() unflushed). A middleware injects the verified
// identity the agent auth would normally attach.
func callManifest(t *testing.T, h *UpdateHandler, siteID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/agent/v1")
	g.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(WithIdentity(c.Request.Context(), Identity{SiteID: siteID, TenantID: uuid.New()}))
		c.Next()
	})
	h.Register(g)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agent/v1/update/manifest", nil)
	r.ServeHTTP(w, req)
	return w
}

func TestUpdateHandler_NoLatest_204(t *testing.T) {
	signer, _ := newTestSigner(t)
	store := &fakeManifestStore{getErr: blobstore.ErrNotFound}
	h := NewUpdateHandler(store, signer, time.Minute)

	w := callManifest(t, h, uuid.New())
	if w.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestUpdateHandler_InvalidManifest_500(t *testing.T) {
	signer, _ := newTestSigner(t)
	// Wrong slug → rejected as invalid.
	bad, _ := json.Marshal(releaseManifest{Slug: "not-wpmgr", Version: "1.0", PackageObjectKey: "agent-releases/x/p.zip", PackageSHA256: goodSHA, PackageSize: 1})
	store := &fakeManifestStore{body: bad, presignURL: "https://example/x"}
	h := NewUpdateHandler(store, signer, time.Minute)

	w := callManifest(t, h, uuid.New())
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 for malformed manifest, got %d", w.Code)
	}
}

func TestUpdateHandler_Valid_SignedManifest(t *testing.T) {
	signer, pub := newTestSigner(t)
	const presigned = "https://storage.googleapis.com/wpmgr-chunks-prod/agent-releases/0.10.6-test/wpmgr-agent.zip?X-Amz-Signature=abc"
	store := &fakeManifestStore{body: validLatestJSON(t), presignURL: presigned}
	ttl := 90 * time.Second
	h := NewUpdateHandler(store, signer, ttl)

	siteID := uuid.New()
	before := time.Now()
	w := callManifest(t, h, siteID)
	after := time.Now()

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", w.Code, w.Body.String())
	}

	// Decode the envelope.
	var env struct {
		Manifest  string `json:"manifest"`
		Signature string `json:"signature"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(env.Manifest)
	if err != nil {
		t.Fatalf("decode manifest b64: %v", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(env.Signature)
	if err != nil {
		t.Fatalf("decode sig b64: %v", err)
	}

	// MUST-1: the signature verifies over the exact payload bytes.
	if !ed25519.Verify(pub, payload, sig) {
		t.Fatal("manifest signature does not verify under CP public key")
	}
	// Tamper one byte → verification must fail.
	bad := append([]byte(nil), payload...)
	bad[0] ^= 0xFF
	if ed25519.Verify(pub, bad, sig) {
		t.Fatal("tampered manifest verified — signature is not binding")
	}

	var claims signedManifestClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	if claims.Aud != siteID.String() {
		t.Errorf("aud = %q, want caller site %q", claims.Aud, siteID)
	}
	if claims.Cmd != agentcmd.CmdUpdateManifest {
		t.Errorf("cmd = %q, want %q", claims.Cmd, agentcmd.CmdUpdateManifest)
	}
	if claims.Slug != "wpmgr-agent" || claims.Version != "0.10.6-test" {
		t.Errorf("slug/version = %q/%q", claims.Slug, claims.Version)
	}
	if claims.PackageURL != presigned {
		t.Errorf("package_url = %q, want presigned", claims.PackageURL)
	}
	if claims.PackageSHA256 != goodSHA || claims.PackageSize != 359578 {
		t.Errorf("sha/size = %q/%d", claims.PackageSHA256, claims.PackageSize)
	}
	if claims.Jti == "" {
		t.Error("jti is empty")
	}
	// exp window: now < exp <= now+ttl (+ a second of slack on each side).
	minExp := before.Add(ttl).Add(-time.Second).Unix()
	maxExp := after.Add(ttl).Add(time.Second).Unix()
	if claims.Exp < minExp || claims.Exp > maxExp {
		t.Errorf("exp %d outside [%d,%d]", claims.Exp, minExp, maxExp)
	}
	if claims.Iat < before.Add(-time.Second).Unix() || claims.Iat > after.Add(time.Second).Unix() {
		t.Errorf("iat %d not ~now", claims.Iat)
	}
	// The presign was minted for the versioned package key, not latest.json.
	if store.gotPresignKey != "agent-releases/0.10.6-test/wpmgr-agent.zip" {
		t.Errorf("presigned key = %q", store.gotPresignKey)
	}
}

func TestUpdateHandler_TTLClamp(t *testing.T) {
	signer, _ := newTestSigner(t)
	store := &fakeManifestStore{getErr: blobstore.ErrNotFound}
	// Over-long + non-positive TTLs both clamp to 5m.
	for _, in := range []time.Duration{0, -time.Second, time.Hour} {
		h := NewUpdateHandler(store, signer, in)
		if h.presignTTL != 5*time.Minute {
			t.Errorf("ttl %v clamped to %v, want 5m", in, h.presignTTL)
		}
	}
	// In-range TTL is preserved.
	h := NewUpdateHandler(store, signer, 2*time.Minute)
	if h.presignTTL != 2*time.Minute {
		t.Errorf("ttl preserved = %v, want 2m", h.presignTTL)
	}
}
