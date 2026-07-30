package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/blobstore"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// packageStore is a ManifestStore double that also records EVERY key it is
// asked for, which is how the "no caller-supplied value reaches the object key"
// guarantee is asserted rather than assumed.
type packageStore struct {
	mu          sync.Mutex
	manifest    []byte
	manifestErr error
	object      []byte
	objectErr   error

	// hold, when non-nil, blocks every package-object read until it is closed.
	// Used to pin streams in flight so the concurrency cap can be observed.
	hold chan struct{}

	gotKeys []string
	// streamedKeys records the keys read through the LARGE-object path, so a
	// test can prove the package object never goes through the small-object,
	// whole-request-capped client.
	streamedKeys []string
}

func (s *packageStore) GetViaPresign(_ context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	s.gotKeys = append(s.gotKeys, key)
	manifest, manifestErr := s.manifest, s.manifestErr
	object, objectErr := s.object, s.objectErr
	s.mu.Unlock()

	if key == updateManifestKey {
		if manifestErr != nil {
			return nil, manifestErr
		}
		return io.NopCloser(bytes.NewReader(manifest)), nil
	}
	if objectErr != nil {
		return nil, objectErr
	}
	return io.NopCloser(bytes.NewReader(object)), nil
}

func (s *packageStore) GetStreamViaPresign(ctx context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	s.streamedKeys = append(s.streamedKeys, key)
	hold := s.hold
	s.mu.Unlock()

	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.GetViaPresign(ctx, key)
}

func (s *packageStore) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	s.mu.Lock()
	s.gotKeys = append(s.gotKeys, key)
	s.mu.Unlock()
	return "https://storage.example.test/" + key, nil
}

// keys returns a copy of the recorded keys (the store is read concurrently by
// the concurrency-cap test).
func (s *packageStore) keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.gotKeys...)
}

func (s *packageStore) streamed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.streamedKeys...)
}

const testPackageVersion = "0.10.6-test"

var testPackageBytes = []byte("PK\x03\x04 pretend this is the agent zip")

func packageManifestJSON(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(releaseManifest{
		Slug:             expectedAgentSlug,
		Plugin:           "wpmgr-agent/wpmgr-agent.php",
		Version:          testPackageVersion,
		MinVersion:       "0.0.0",
		PackageObjectKey: updatePackagePrefix + testPackageVersion + "/" + expectedAgentSlug + ".zip",
		PackageSHA256:    goodSHA,
		PackageSize:      int64(len(testPackageBytes)),
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	return b
}

// newPackageHandler builds an UpdateHandler with control-plane serving ARMED.
func newPackageHandler(t *testing.T) (*UpdateHandler, *packageStore, *agentcmd.Signer) {
	t.Helper()
	signer, _ := newTestSigner(t)
	store := &packageStore{manifest: packageManifestJSON(t), object: testPackageBytes}
	h := NewUpdateHandler(store, signer, time.Minute)
	h.EnablePackageServing(signer, "https://cp.example.test")
	return h, store, signer
}

// callPackage drives the download route through a real Gin engine mounted the
// way production mounts it: on the ROOT engine, with no auth middleware at all.
func callPackage(t *testing.T, h *UpdateHandler, siteID uuid.UUID, token string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h.RegisterPublic(r)

	target := PackageRoutePrefix + "/" + siteID.String()
	if token != "" {
		target += "?" + PackageTokenQueryParam + "=" + url.QueryEscape(token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

// ---------------------------------------------------------------------------
// Contract pinning
// ---------------------------------------------------------------------------

// TestPackageDownloadWireContract pins every literal that makes up the URL the
// mint side writes into package_url and the download side serves. GH #302 is
// built as two halves in parallel; a rename on either side must fail here.
func TestPackageDownloadWireContract(t *testing.T) {
	cases := []struct{ got, want, name string }{
		{PackageRoutePrefix, "/agent/v1/update/package", "PackageRoutePrefix"},
		{PackageSiteIDParam, "siteId", "PackageSiteIDParam"},
		{PackageRoutePath, "/agent/v1/update/package/:siteId", "PackageRoutePath"},
		{PackageTokenQueryParam, "token", "PackageTokenQueryParam"},
		{PackageDownloadFilename, "wpmgr-agent.zip", "PackageDownloadFilename"},
		{PackageContentType, "application/zip", "PackageContentType"},
		{agentcmd.CmdUpdatePackage, "update_package", "agentcmd.CmdUpdatePackage"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// TestPackageURLShape pins the exact URL the signed manifest hands the agent:
// origin + prefix + "/" + siteId + "?token=". The agent fetches this verbatim.
func TestPackageURLShape(t *testing.T) {
	h, _, _ := newPackageHandler(t)
	siteID := uuid.New()

	w := callManifest(t, h, siteID)
	if w.Code != http.StatusOK {
		t.Fatalf("manifest: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	claims := decodeManifestClaims(t, w.Body.Bytes())

	wantPrefix := "https://cp.example.test/agent/v1/update/package/" + siteID.String() + "?token="
	if !strings.HasPrefix(claims.PackageURL, wantPrefix) {
		t.Fatalf("package_url = %q, want prefix %q", claims.PackageURL, wantPrefix)
	}

	// The token in that URL must be a real, verifiable package token bound to
	// this site and this version, and it must actually redeem against the live
	// download route. This is the end-to-end mint-then-download contract.
	u, err := url.Parse(claims.PackageURL)
	if err != nil {
		t.Fatalf("parse package_url: %v", err)
	}
	token := u.Query().Get(PackageTokenQueryParam)
	if token == "" {
		t.Fatal("package_url carries no token")
	}
	dl := callPackage(t, h, siteID, token)
	if dl.Code != http.StatusOK {
		t.Fatalf("the token from package_url did not redeem: %d (%s)", dl.Code, dl.Body.String())
	}
	if !bytes.Equal(dl.Body.Bytes(), testPackageBytes) {
		t.Fatalf("redeemed body = %q, want the package bytes", dl.Body.Bytes())
	}
}

// TestPackageURL_ExpirySemanticsUnchanged: the manifest's exp and the token's
// exp are the SAME window (presignTTL), so nothing downstream has to learn that
// the origin moved.
func TestPackageURL_ExpirySemanticsUnchanged(t *testing.T) {
	signer, _ := newTestSigner(t)
	store := &packageStore{manifest: packageManifestJSON(t), object: testPackageBytes}
	const ttl = 90 * time.Second
	h := NewUpdateHandler(store, signer, ttl)
	h.EnablePackageServing(signer, "https://cp.example.test")

	siteID := uuid.New()
	w := callManifest(t, h, siteID)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	claims := decodeManifestClaims(t, w.Body.Bytes())

	if got := claims.Exp - claims.Iat; got != int64(ttl.Seconds()) {
		t.Fatalf("manifest window = %ds, want %ds", got, int64(ttl.Seconds()))
	}
	u, _ := url.Parse(claims.PackageURL)
	tok, err := signer.VerifyPackageToken(time.Now(), u.Query().Get(PackageTokenQueryParam))
	if err != nil {
		t.Fatalf("verify minted token: %v", err)
	}
	if got := tok.ExpiresAt.Sub(tok.IssuedAt); got != ttl {
		t.Fatalf("token window = %s, want %s", got, ttl)
	}
}

// TestPackageURL_DisabledStaysPresigned: without EnablePackageServing the mint
// path is byte-for-byte what it was before GH #302.
func TestPackageURL_DisabledStaysPresigned(t *testing.T) {
	signer, _ := newTestSigner(t)
	store := &packageStore{manifest: packageManifestJSON(t), object: testPackageBytes}
	h := NewUpdateHandler(store, signer, time.Minute)

	w := callManifest(t, h, uuid.New())
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", w.Code, w.Body.String())
	}
	claims := decodeManifestClaims(t, w.Body.Bytes())
	if !strings.HasPrefix(claims.PackageURL, "https://storage.example.test/") {
		t.Fatalf("package_url = %q, want the presigned storage URL", claims.PackageURL)
	}
}

// TestPackageOriginDerivedFromRequest: with no configured base URL the origin
// comes from the control-plane URL the agent itself used, which is what makes a
// self-hosted install need no configuration.
func TestPackageOriginDerivedFromRequest(t *testing.T) {
	signer, _ := newTestSigner(t)
	store := &packageStore{manifest: packageManifestJSON(t), object: testPackageBytes}
	h := NewUpdateHandler(store, signer, time.Minute)
	h.EnablePackageServing(signer, "")

	siteID := uuid.New()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/agent/v1")
	g.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(WithIdentity(c.Request.Context(), Identity{SiteID: siteID, TenantID: uuid.New()}))
		c.Next()
	})
	h.Register(g)

	req := httptest.NewRequest(http.MethodGet, "/agent/v1/update/manifest", nil)
	req.Host = "selfhosted.example.internal"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	claims := decodeManifestClaims(t, w.Body.Bytes())
	if !strings.HasPrefix(claims.PackageURL, "https://selfhosted.example.internal/agent/v1/update/package/") {
		t.Fatalf("package_url = %q, want it derived from the request host over https", claims.PackageURL)
	}
}

// ---------------------------------------------------------------------------
// The download route
// ---------------------------------------------------------------------------

func TestPackageDownload_ValidTokenStreamsTheBytes(t *testing.T) {
	h, store, signer := newPackageHandler(t)
	siteID := uuid.New()
	token, _, err := signer.MintPackageToken(time.Now(), siteID.String(), testPackageVersion, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	w := callPackage(t, h, siteID, token)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), testPackageBytes) {
		t.Fatalf("streamed %q, want %q", w.Body.Bytes(), testPackageBytes)
	}
	if ct := w.Header().Get("Content-Type"); ct != PackageContentType {
		t.Errorf("Content-Type = %q, want %q", ct, PackageContentType)
	}
	if cl := w.Header().Get("Content-Length"); cl != strconv.Itoa(len(testPackageBytes)) {
		t.Errorf("Content-Length = %q, want %q", cl, strconv.Itoa(len(testPackageBytes)))
	}
	if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename="wpmgr-agent.zip"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Errorf("Cache-Control = %q, want no-store (the URL is a bearer credential)", cc)
	}

	// The object key is the pinned, version-derived one and nothing else.
	wantKey := updatePackagePrefix + testPackageVersion + "/" + expectedAgentSlug + ".zip"
	if len(store.gotKeys) != 2 || store.gotKeys[0] != updateManifestKey || store.gotKeys[1] != wantKey {
		t.Fatalf("store was asked for %v, want [%s %s]", store.gotKeys, updateManifestKey, wantKey)
	}
}

// TestPackageDownload_ShortBodyIsLoggedAsAnError pins that a TRUNCATED serve is
// not reported as a success.
//
// streamPackage reports io.EOF when the published object holds fewer bytes than
// the manifest declares, and the handler treats io.EOF as "not an abort", so
// that case used to fall straight through to the INFO success line: the log said
// "served agent package" at INFO with a byte count below the Content-Length the
// same handler had already promised. The site refuses that body on its size
// check and retries every cycle forever, so it is the opposite of a success and
// the only place it can ever be noticed is this log. It also cannot be caused by
// a request: it means the published manifest and the published object disagree,
// which no retry can fix and somebody has to go and repair.
func TestPackageDownload_ShortBodyIsLoggedAsAnError(t *testing.T) {
	const declared = 4096 // the manifest promises this many bytes...
	short := []byte("PK\x03\x04 truncated object")

	manifest, err := json.Marshal(releaseManifest{
		Slug:             expectedAgentSlug,
		Plugin:           "wpmgr-agent/wpmgr-agent.php",
		Version:          testPackageVersion,
		MinVersion:       "0.0.0",
		PackageObjectKey: updatePackagePrefix + testPackageVersion + "/" + expectedAgentSlug + ".zip",
		PackageSHA256:    goodSHA,
		PackageSize:      declared,
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	signer, _ := newTestSigner(t)
	h := NewUpdateHandler(&packageStore{manifest: manifest, object: short}, signer, time.Minute)
	h.EnablePackageServing(signer, "https://cp.example.test")

	siteID := uuid.New()
	token, _, err := signer.MintPackageToken(time.Now(), siteID.String(), testPackageVersion, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	logs := captureSlog(t)
	w := callPackage(t, h, siteID, token)

	// The response itself is unchanged: 200 and the declared Content-Length were
	// flushed before the body ran short, which is exactly why the log matters.
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if got := w.Body.Len(); got != len(short) {
		t.Fatalf("served %d bytes, want the %d the object actually holds", got, len(short))
	}

	lines := logs.forSite(siteID.String())
	if len(lines) == 0 {
		t.Fatal("a short serve produced no log line at all naming the site")
	}
	for _, line := range lines {
		if strings.Contains(line, "served agent package from the control plane") {
			t.Fatalf("a truncated serve was logged as a SUCCESS: %s", line)
		}
	}
	found := false
	for _, line := range lines {
		if strings.Contains(line, `level=ERROR`) && strings.Contains(line, "short body") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a truncated serve was not logged at ERROR; got %v", lines)
	}
}

func TestPackageDownload_ExpiredTokenRefused(t *testing.T) {
	h, _, signer := newPackageHandler(t)
	siteID := uuid.New()
	token, _, err := signer.MintPackageToken(time.Now().Add(-10*time.Minute), siteID.String(), testPackageVersion, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	w := callPackage(t, h, siteID, token)
	assertPackageUnauthorized(t, w)
}

func TestPackageDownload_TokenForAnotherSiteRefused(t *testing.T) {
	h, store, signer := newPackageHandler(t)
	tokenSite := uuid.New()
	requestSite := uuid.New()
	token, _, err := signer.MintPackageToken(time.Now(), tokenSite.String(), testPackageVersion, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	w := callPackage(t, h, requestSite, token)
	assertPackageUnauthorized(t, w)
	if len(store.gotKeys) != 0 {
		t.Fatalf("storage was touched on a refused request: %v", store.gotKeys)
	}
}

func TestPackageDownload_TokenForAnotherVersionRefused(t *testing.T) {
	h, store, signer := newPackageHandler(t)
	siteID := uuid.New()
	token, _, err := signer.MintPackageToken(time.Now(), siteID.String(), "9.9.9-not-published", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	w := callPackage(t, h, siteID, token)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d (%s)", w.Code, w.Body.String())
	}
	// The manifest was read (that is how the published version is known), but no
	// package object was ever fetched for the version the token named.
	for _, k := range store.gotKeys {
		if strings.Contains(k, "9.9.9-not-published") {
			t.Fatalf("a token version reached the object key: %v", store.gotKeys)
		}
	}
}

func TestPackageDownload_ForgedTokenRefused(t *testing.T) {
	h, store, _ := newPackageHandler(t)
	siteID := uuid.New()

	// Signed by a completely different key.
	attacker, _ := newTestSigner(t)
	token, _, err := attacker.MintPackageToken(time.Now(), siteID.String(), testPackageVersion, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	w := callPackage(t, h, siteID, token)
	assertPackageUnauthorized(t, w)
	if len(store.gotKeys) != 0 {
		t.Fatalf("storage was touched on a forged token: %v", store.gotKeys)
	}
}

func TestPackageDownload_TamperedTokenRefused(t *testing.T) {
	h, _, signer := newPackageHandler(t)
	siteID := uuid.New()
	token, _, err := signer.MintPackageToken(time.Now(), siteID.String(), testPackageVersion, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	parts := strings.Split(token, ".")
	claimsJSON, _ := base64.RawURLEncoding.DecodeString(parts[1])
	// Re-point the token at a different object by editing its version claim.
	tampered := strings.Replace(string(claimsJSON), testPackageVersion, "../../secrets", 1)
	forged := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(tampered)) + "." + parts[2]

	w := callPackage(t, h, siteID, forged)
	assertPackageUnauthorized(t, w)
}

func TestPackageDownload_MissingTokenRefused(t *testing.T) {
	h, _, _ := newPackageHandler(t)
	assertPackageUnauthorized(t, callPackage(t, h, uuid.New(), ""))
}

func TestPackageDownload_NonPackageCommandTokenRefused(t *testing.T) {
	h, _, signer := newPackageHandler(t)
	siteID := uuid.New()
	// A genuine, unexpired `update` command token minted by the SAME key.
	cmdToken, _, err := signer.Mint(time.Now(), siteID.String(), "update")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	assertPackageUnauthorized(t, callPackage(t, h, siteID, cmdToken))
}

func TestPackageDownload_BadSiteIDRefused(t *testing.T) {
	h, _, signer := newPackageHandler(t)
	siteID := uuid.New()
	token, _, err := signer.MintPackageToken(time.Now(), siteID.String(), testPackageVersion, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	h.RegisterPublic(r)
	w := httptest.NewRecorder()
	// A traversal-shaped site segment: refused as unauthenticated, and it never
	// reaches storage.
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		PackageRoutePrefix+"/..%2f..%2fetc?"+PackageTokenQueryParam+"="+url.QueryEscape(token), nil))
	if w.Code != http.StatusUnauthorized && w.Code != http.StatusNotFound && w.Code != http.StatusMovedPermanently {
		t.Fatalf("want a refusal for a non-UUID site segment, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestPackageDownload_ServingDisabledRefusesEverything(t *testing.T) {
	signer, _ := newTestSigner(t)
	store := &packageStore{manifest: packageManifestJSON(t), object: testPackageBytes}
	h := NewUpdateHandler(store, signer, time.Minute)
	// EnablePackageServing deliberately NOT called.

	siteID := uuid.New()
	token, _, err := signer.MintPackageToken(time.Now(), siteID.String(), testPackageVersion, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// domain.Unavailable is 501 ("feature disabled"), the same kind the sibling
	// manifest route uses for update_unwired.
	w := callPackage(t, h, siteID, token)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("want 501, got %d (%s)", w.Code, w.Body.String())
	}
	if len(store.gotKeys) != 0 {
		t.Fatalf("storage was touched while serving is disabled: %v", store.gotKeys)
	}
}

func TestPackageDownload_NoPublishedRelease404(t *testing.T) {
	h, store, signer := newPackageHandler(t)
	store.manifestErr = blobstore.ErrNotFound
	siteID := uuid.New()
	token, _, err := signer.MintPackageToken(time.Now(), siteID.String(), testPackageVersion, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if w := callPackage(t, h, siteID, token); w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d (%s)", w.Code, w.Body.String())
	}
}

// TestPackageDownload_KeyIsNotCallerInfluenced sweeps a set of hostile inputs
// across BOTH caller-supplied surfaces (the path site id and the query token)
// and asserts the store is never asked for anything but the two pinned keys.
func TestPackageDownload_KeyIsNotCallerInfluenced(t *testing.T) {
	wantPackageKey := updatePackagePrefix + testPackageVersion + "/" + expectedAgentSlug + ".zip"

	hostile := []string{
		"../../etc/passwd",
		"agent-releases/latest.json",
		"%2e%2e%2f%2e%2e%2fsecret",
		"*",
		"..",
		strings.Repeat("a", 300),
	}

	for _, bad := range hostile {
		h, store, signer := newPackageHandler(t)
		siteID := uuid.New()

		// Hostile value in the token's version claim (properly signed, so it gets
		// past the signature check and is refused on the version comparison).
		token, _, err := signer.MintPackageToken(time.Now(), siteID.String(), bad, time.Minute)
		if err != nil {
			// An empty-ish version is refused at mint; that is also fine.
			continue
		}
		_ = callPackage(t, h, siteID, token)

		// Hostile value in the path.
		gin.SetMode(gin.TestMode)
		r := gin.New()
		h.RegisterPublic(r)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
			PackageRoutePrefix+"/"+url.PathEscape(bad)+"?"+PackageTokenQueryParam+"="+url.QueryEscape(token), nil))

		for _, k := range store.gotKeys {
			if k != updateManifestKey && k != wantPackageKey {
				t.Fatalf("input %q made the store read %q; only %q and %q are reachable",
					bad, k, updateManifestKey, wantPackageKey)
			}
		}
	}
}

// TestPackageDownload_MalformedPublishedVersionRefused: even a manifest that
// somehow published a traversal-shaped version cannot aim this route at another
// object.
func TestPackageDownload_MalformedPublishedVersionRefused(t *testing.T) {
	signer, _ := newTestSigner(t)
	// A manifest whose package_object_key is internally consistent with a
	// traversal-shaped version, so readLatest's key pin passes.
	bad := "../../evil"
	man, err := json.Marshal(releaseManifest{
		Slug:             expectedAgentSlug,
		Version:          bad,
		MinVersion:       "0.0.0",
		PackageObjectKey: updatePackagePrefix + bad + "/" + expectedAgentSlug + ".zip",
		PackageSHA256:    goodSHA,
		PackageSize:      int64(len(testPackageBytes)),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	store := &packageStore{manifest: man, object: testPackageBytes}
	h := NewUpdateHandler(store, signer, time.Minute)
	h.EnablePackageServing(signer, "https://cp.example.test")

	siteID := uuid.New()
	token, _, err := signer.MintPackageToken(time.Now(), siteID.String(), bad, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	w := callPackage(t, h, siteID, token)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d (%s)", w.Code, w.Body.String())
	}
	for _, k := range store.gotKeys {
		if k != updateManifestKey {
			t.Fatalf("a traversal-shaped published version reached storage: %v", store.gotKeys)
		}
	}
}

// ---------------------------------------------------------------------------
// H2: the package object must not ride the small-object, whole-request-capped
// read path
// ---------------------------------------------------------------------------

// TestPackageDownload_UsesTheStreamingRead pins WHICH store method carries the
// package. GetViaPresign runs on a client with a 15s whole-request Timeout that
// covers reading the body, and this handler copies that body onto the site's
// connection, so routing the package through it makes a slow site kill its own
// storage read and receive a truncated zip. The manifest (tiny, consumed
// immediately) legitimately stays on the capped path.
func TestPackageDownload_UsesTheStreamingRead(t *testing.T) {
	h, store, signer := newPackageHandler(t)
	siteID := uuid.New()
	token, _, err := signer.MintPackageToken(time.Now(), siteID.String(), testPackageVersion, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if w := callPackage(t, h, siteID, token); w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", w.Code, w.Body.String())
	}

	wantKey := updatePackagePrefix + testPackageVersion + "/" + expectedAgentSlug + ".zip"
	streamed := store.streamed()
	if len(streamed) != 1 || streamed[0] != wantKey {
		t.Fatalf("streamed keys = %v, want exactly [%s]", streamed, wantKey)
	}
	for _, k := range streamed {
		if k == updateManifestKey {
			t.Fatalf("the manifest was read through the large-object path: %v", streamed)
		}
	}
}

// TestPackageDownload_SlowConsumerGetsTheWholePackage is the H2 regression test
// against a REAL HTTP body and a REAL blobstore.Store rather than a fake store,
// which is precisely what the existing handler tests could not catch: the defect
// lived entirely in the HTTP client the store fetches with, so any in-memory
// double reported success.
//
// The consumer here reads in small chunks with a pause between them, exactly the
// shape of a shared-hosting site pulling a multi-megabyte zip. Under the capped
// read path the storage side dies mid-body once the consumer's pace pushes the
// transfer past the client's whole-request deadline, and because the handler has
// already written 200 plus a Content-Length, the site sees a short body rather
// than an error. The whole package must arrive, byte for byte.
func TestPackageDownload_SlowConsumerGetsTheWholePackage(t *testing.T) {
	const packageSize = 1 << 20 // 1 MiB, trickled both in and out

	pkg := make([]byte, packageSize)
	for i := range pkg {
		pkg[i] = byte(i % 251)
	}
	manifest, err := json.Marshal(releaseManifest{
		Slug:             expectedAgentSlug,
		Plugin:           "wpmgr-agent/wpmgr-agent.php",
		Version:          testPackageVersion,
		MinVersion:       "0.0.0",
		PackageObjectKey: updatePackagePrefix + testPackageVersion + "/" + expectedAgentSlug + ".zip",
		PackageSHA256:    goodSHA,
		PackageSize:      packageSize,
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	// Stand-in for object storage. It answers the presigned GETs the store mints
	// (SigV4 is computed offline, so no real S3 is involved) and trickles the
	// package out in flushed chunks the way a real backend does.
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "latest.json") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(manifest)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Length", strconv.Itoa(len(pkg)))
		const chunk = 64 << 10
		for off := 0; off < len(pkg); off += chunk {
			end := off + chunk
			if end > len(pkg) {
				end = len(pkg)
			}
			if _, werr := w.Write(pkg[off:end]); werr != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(2 * time.Millisecond)
		}
	}))
	defer storage.Close()

	store, err := blobstore.New(blobstore.Config{
		Bucket:         "test-bucket",
		Endpoint:       storage.URL,
		Region:         "us-east-1",
		AccessKey:      "test-access-key",
		SecretKey:      "test-secret-key",
		ForcePathStyle: true,
	})
	if err != nil {
		t.Fatalf("blobstore.New: %v", err)
	}

	signer, _ := newTestSigner(t)
	h := NewUpdateHandler(store, signer, time.Minute)
	h.EnablePackageServing(signer, "")

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	h.RegisterPublic(engine)
	cp := httptest.NewServer(engine)
	defer cp.Close()

	siteID := uuid.New()
	token, _, err := signer.MintPackageToken(time.Now(), siteID.String(), testPackageVersion, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	resp, err := http.Get(cp.URL + PackageRoutePrefix + "/" + siteID.String() +
		"?" + PackageTokenQueryParam + "=" + url.QueryEscape(token))
	if err != nil {
		t.Fatalf("GET package: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Length"); got != strconv.Itoa(packageSize) {
		t.Fatalf("Content-Length = %q, want %d", got, packageSize)
	}

	// The slow consumer.
	var got []byte
	buf := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		got = append(got, buf[:n]...)
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			t.Fatalf("read at %d of %d bytes: %v (a truncated body is the H2 defect)", len(got), packageSize, rerr)
		}
		time.Sleep(4 * time.Millisecond)
	}
	if len(got) != packageSize {
		t.Fatalf("received %d bytes, want %d (short body)", len(got), packageSize)
	}
	if !bytes.Equal(got, pkg) {
		t.Fatal("received body differs from the published package")
	}
}

// ---------------------------------------------------------------------------
// H3: the route is bounded on both axes
// ---------------------------------------------------------------------------

// TestPackageDownload_ConcurrentStreamsAreCapped: with every slot on this
// instance held, a further download is refused with 429 immediately rather than
// queued. Queueing would be the amplification, since a queued request still pins
// a goroutine and a storage connection.
func TestPackageDownload_ConcurrentStreamsAreCapped(t *testing.T) {
	h, store, signer := newPackageHandler(t)
	hold := make(chan struct{})
	store.hold = hold

	var wg sync.WaitGroup
	codes := make([]int, maxConcurrentPackageStreams)
	for i := 0; i < maxConcurrentPackageStreams; i++ {
		siteID := uuid.New()
		token, _, err := signer.MintPackageToken(time.Now(), siteID.String(), testPackageVersion, time.Minute)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		wg.Add(1)
		go func(idx int, site uuid.UUID, tok string) {
			defer wg.Done()
			codes[idx] = callPackage(t, h, site, tok).Code
		}(i, siteID, token)
	}

	// Wait for every slot to be taken (each of those goroutines is parked inside
	// the store read).
	deadline := time.Now().Add(5 * time.Second)
	for h.streams.inFlight() < maxConcurrentPackageStreams {
		if time.Now().After(deadline) {
			close(hold)
			wg.Wait()
			t.Fatalf("only %d of %d slots were taken", h.streams.inFlight(), maxConcurrentPackageStreams)
		}
		time.Sleep(time.Millisecond)
	}

	// One more, with a perfectly valid token of its own.
	extraSite := uuid.New()
	extraToken, _, err := signer.MintPackageToken(time.Now(), extraSite.String(), testPackageVersion, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	w := callPackage(t, h, extraSite, extraToken)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429 over the cap, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "update_package_busy") {
		t.Fatalf("want the update_package_busy code, got %s", w.Body.String())
	}
	if ra := w.Header().Get("Retry-After"); ra == "" {
		t.Error("a 429 on this route must carry Retry-After")
	}

	close(hold)
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("in-flight download %d ended %d, want 200", i, code)
		}
	}
	// Slots are returned once the streams finish, so the next caller is served.
	if h.streams.inFlight() != 0 {
		t.Fatalf("in-flight = %d after every stream finished, want 0", h.streams.inFlight())
	}
	if w := callPackage(t, h, extraSite, extraToken); w.Code != http.StatusOK {
		t.Fatalf("after the cap cleared, want 200, got %d (%s)", w.Code, w.Body.String())
	}
}

// TestPackageDownload_TokenRedemptionsAreCapped: one token is good for a small
// number of downloads inside its window (retries are legitimate), not for an
// unlimited replay. The cap is per token, so it never spills onto another site.
func TestPackageDownload_TokenRedemptionsAreCapped(t *testing.T) {
	h, _, signer := newPackageHandler(t)
	siteID := uuid.New()
	token, _, err := signer.MintPackageToken(time.Now(), siteID.String(), testPackageVersion, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	for i := 0; i < maxPackageTokenRedemptions; i++ {
		if w := callPackage(t, h, siteID, token); w.Code != http.StatusOK {
			t.Fatalf("redemption %d: want 200, got %d (%s)", i+1, w.Code, w.Body.String())
		}
	}

	w := callPackage(t, h, siteID, token)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("redemption %d: want 429, got %d (%s)", maxPackageTokenRedemptions+1, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "update_package_token_exhausted") {
		t.Fatalf("want the update_package_token_exhausted code, got %s", w.Body.String())
	}

	// A freshly minted token for the SAME site is unaffected: the budget is per
	// token, so a legitimate agent that re-reads the manifest is never stuck.
	fresh, _, err := signer.MintPackageToken(time.Now(), siteID.String(), testPackageVersion, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if w := callPackage(t, h, siteID, fresh); w.Code != http.StatusOK {
		t.Fatalf("a fresh token was refused: %d (%s)", w.Code, w.Body.String())
	}
}

// TestPackageDownload_ExhaustedTokenTouchesNoStorage: the redemption bound is
// checked before any storage work, so replaying a spent token costs one
// signature verification and nothing else.
func TestPackageDownload_ExhaustedTokenTouchesNoStorage(t *testing.T) {
	h, store, signer := newPackageHandler(t)
	siteID := uuid.New()
	token, _, err := signer.MintPackageToken(time.Now(), siteID.String(), testPackageVersion, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	for i := 0; i < maxPackageTokenRedemptions; i++ {
		if w := callPackage(t, h, siteID, token); w.Code != http.StatusOK {
			t.Fatalf("redemption %d: want 200, got %d", i+1, w.Code)
		}
	}
	before := len(store.keys())
	if w := callPackage(t, h, siteID, token); w.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d", w.Code)
	}
	if after := len(store.keys()); after != before {
		t.Fatalf("a refused replay read storage: %v", store.keys()[before:])
	}
}

func TestPackageRedemptions_SweepExpiredEntries(t *testing.T) {
	r := newPackageTokenRedemptions(maxPackageTokenRedemptions)
	now := time.Now()
	exp := now.Add(time.Minute)

	if got := r.redeem(now, "jti-a", exp); !got.Allowed || got.Count != 1 || !got.Tracked {
		t.Fatalf("first redemption = %+v, want allowed/tracked count 1", got)
	}
	if r.tracked() != 1 {
		t.Fatalf("tracked = %d, want 1", r.tracked())
	}

	// Past the token's own exp, and past a sweep interval: the entry is gone, so
	// the table cannot grow without bound.
	later := exp.Add(packageRedemptionSweepEvery + time.Second)
	r.redeem(later, "jti-b", later.Add(time.Minute))
	if r.tracked() != 1 {
		t.Fatalf("tracked = %d after the sweep, want 1 (only the live token)", r.tracked())
	}

	// An empty jti is nothing to count, and must not be recorded.
	if got := r.redeem(later, "", later.Add(time.Minute)); !got.Allowed || got.Tracked {
		t.Fatalf("empty jti = %+v, want allowed and untracked", got)
	}
}

// TestPackageDownload_BusyRefusalDoesNotSpendARedemption pins the ORDER of the
// two bounds.
//
// A busy refusal is the CONTROL PLANE saying "not now": it serves zero bytes and
// the caller did nothing wrong. When the redemption was counted first, five 429s
// during a saturated patch burned the token's whole budget, so the very next
// attempt, against a completely idle instance, came back
// update_package_token_exhausted having never received a byte. That defeats the
// budget's own stated rationale, which is to permit exactly the retries the
// token is deliberately not single-use for.
func TestPackageDownload_BusyRefusalDoesNotSpendARedemption(t *testing.T) {
	h, store, signer := newPackageHandler(t)
	hold := make(chan struct{})
	store.hold = hold

	// Saturate every slot with OTHER sites' downloads.
	var wg sync.WaitGroup
	for i := 0; i < maxConcurrentPackageStreams; i++ {
		site := uuid.New()
		tok, _, err := signer.MintPackageToken(time.Now(), site.String(), testPackageVersion, time.Minute)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		wg.Add(1)
		go func(site uuid.UUID, tok string) {
			defer wg.Done()
			callPackage(t, h, site, tok)
		}(site, tok)
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.streams.inFlight() < maxConcurrentPackageStreams {
		if time.Now().After(deadline) {
			close(hold)
			wg.Wait()
			t.Fatalf("only %d of %d slots were taken", h.streams.inFlight(), maxConcurrentPackageStreams)
		}
		time.Sleep(time.Millisecond)
	}

	// Our site retries through the whole saturated window with ONE token, the way
	// an agent honouring Retry-After does.
	site := uuid.New()
	token, _, err := signer.MintPackageToken(time.Now(), site.String(), testPackageVersion, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	for i := 0; i < maxPackageTokenRedemptions; i++ {
		w := callPackage(t, h, site, token)
		if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "update_package_busy") {
			t.Fatalf("refusal %d: want 429 update_package_busy, got %d (%s)", i+1, w.Code, w.Body.String())
		}
	}

	close(hold)
	wg.Wait()
	if h.streams.inFlight() != 0 {
		t.Fatalf("in-flight = %d after every stream finished, want 0", h.streams.inFlight())
	}

	// The instance is idle now. That token was never served a single byte, so its
	// entire budget must still be there.
	for i := 0; i < maxPackageTokenRedemptions; i++ {
		w := callPackage(t, h, site, token)
		if w.Code != http.StatusOK {
			t.Fatalf("download %d after %d busy refusals: want 200, got %d (%s); a refusal spent the caller's retry budget",
				i+1, maxPackageTokenRedemptions, w.Code, w.Body.String())
		}
	}
	// And the budget is still a budget: the redemptions it did use are counted.
	if w := callPackage(t, h, site, token); w.Code != http.StatusTooManyRequests ||
		!strings.Contains(w.Body.String(), "update_package_token_exhausted") {
		t.Fatalf("want 429 update_package_token_exhausted once the budget is genuinely spent, got %d (%s)", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// The progress bound: a stream that stops moving must not hold its slot
// ---------------------------------------------------------------------------

// TestPackageDownload_SilentStorageDoesNotHoldASlot is the reproduction of the
// denial the uncapped streaming client opened up.
//
// Storage here is ALIVE: it answers with headers and one chunk from a real
// socket and then never sends another byte. The transport's
// ResponseHeaderTimeout is already spent, the keepalive sees a healthy socket,
// and the request context is a connected client's, so before the progress bound
// this request held one of sixteen slots forever. Sixteen of these, mintable
// from four ordinary manifest fetches, denied the update channel to every site
// on the instance.
//
// The whole path is real: a real object-storage stand-in, a real blobstore.Store
// (with its stall window compressed so the test takes a fraction of a second),
// the real handler on a real Gin engine, and a real HTTP client.
func TestPackageDownload_SilentStorageDoesNotHoldASlot(t *testing.T) {
	const (
		stall       = 300 * time.Millisecond
		packageSize = 1 << 20
	)

	manifest, err := json.Marshal(releaseManifest{
		Slug:             expectedAgentSlug,
		Plugin:           "wpmgr-agent/wpmgr-agent.php",
		Version:          testPackageVersion,
		MinVersion:       "0.0.0",
		PackageObjectKey: updatePackagePrefix + testPackageVersion + "/" + expectedAgentSlug + ".zip",
		PackageSHA256:    goodSHA,
		PackageSize:      packageSize,
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	release := make(chan struct{})
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "latest.json") {
			_, _ = w.Write(manifest)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(packageSize))
		_, _ = w.Write(make([]byte, 64<<10))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release // connected, and silent for the rest of time
	}))
	defer func() {
		close(release)
		storage.Close()
	}()

	store, err := blobstore.New(blobstore.Config{
		Bucket:             "test-bucket",
		Endpoint:           storage.URL,
		Region:             "us-east-1",
		AccessKey:          "test-access-key",
		SecretKey:          "test-secret-key",
		ForcePathStyle:     true,
		StreamStallTimeout: stall,
	})
	if err != nil {
		t.Fatalf("blobstore.New: %v", err)
	}

	signer, _ := newTestSigner(t)
	h := NewUpdateHandler(store, signer, time.Minute)
	h.EnablePackageServing(signer, "")

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	h.RegisterPublic(engine)
	cp := httptest.NewServer(engine)
	defer cp.Close()

	siteID := uuid.New()
	token, _, err := signer.MintPackageToken(time.Now(), siteID.String(), testPackageVersion, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	start := time.Now()
	resp, err := http.Get(cp.URL + PackageRoutePrefix + "/" + siteID.String() +
		"?" + PackageTokenQueryParam + "=" + url.QueryEscape(token))
	if err != nil {
		t.Fatalf("GET package: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The client stays connected and keeps reading: nothing on this side asks for
	// the request to end. Only the progress bound can end it.
	got, rerr := io.ReadAll(resp.Body)
	if rerr == nil && len(got) == packageSize {
		t.Fatal("the silent storage peer somehow delivered the whole package")
	}
	if elapsed := time.Since(start); elapsed > 10*stall {
		t.Fatalf("the stalled stream ran for %s, want it torn down after about %s", elapsed, stall)
	}

	// The point of the whole exercise: the slot came back.
	slotDeadline := time.Now().Add(5 * time.Second)
	for h.streams.inFlight() != 0 {
		if time.Now().After(slotDeadline) {
			t.Fatalf("in-flight = %d of %d after the stream stalled: a silent peer holds its slot indefinitely",
				h.streams.inFlight(), h.streams.capacity())
		}
		time.Sleep(5 * time.Millisecond)
	}

	// And a legitimate site is served immediately afterwards, which is what the
	// denial took away.
	nextSite := uuid.New()
	nextToken, _, err := signer.MintPackageToken(time.Now(), nextSite.String(), testPackageVersion, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	next, err := http.Get(cp.URL + PackageRoutePrefix + "/" + nextSite.String() +
		"?" + PackageTokenQueryParam + "=" + url.QueryEscape(nextToken))
	if err != nil {
		t.Fatalf("GET package (second site): %v", err)
	}
	defer func() { _ = next.Body.Close() }()
	if next.StatusCode == http.StatusTooManyRequests {
		t.Fatal("a legitimate site was refused with 429 while a stalled stream held its slot")
	}
}

// TestPackageDownload_SilentConsumerDoesNotHoldASlot covers the other direction.
// Storage is perfectly healthy; the SITE is the one that connects, sends the
// request and then never reads a byte. Once the socket buffers fill, the
// handler's write parks in the kernel, and with no server WriteTimeout (which we
// deliberately do not add, because it would cut the SSE streams) nothing in the
// process ends it. The per-response write deadline armed by streamPackage is
// what releases the slot.
func TestPackageDownload_SilentConsumerDoesNotHoldASlot(t *testing.T) {
	const (
		stall = 300 * time.Millisecond
		// Larger than any plausible socket buffer, so the write genuinely blocks.
		packageSize = 8 << 20
	)

	restore := packageStreamStall
	packageStreamStall = stall
	defer func() { packageStreamStall = restore }()

	pkg := make([]byte, packageSize)
	manifest, err := json.Marshal(releaseManifest{
		Slug:             expectedAgentSlug,
		Plugin:           "wpmgr-agent/wpmgr-agent.php",
		Version:          testPackageVersion,
		MinVersion:       "0.0.0",
		PackageObjectKey: updatePackagePrefix + testPackageVersion + "/" + expectedAgentSlug + ".zip",
		PackageSHA256:    goodSHA,
		PackageSize:      packageSize,
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	signer, _ := newTestSigner(t)
	store := &packageStore{manifest: manifest, object: pkg}
	h := NewUpdateHandler(store, signer, time.Minute)
	h.EnablePackageServing(signer, "")

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	h.RegisterPublic(engine)
	cp := httptest.NewServer(engine)
	defer cp.Close()

	siteID := uuid.New()
	token, _, err := signer.MintPackageToken(time.Now(), siteID.String(), testPackageVersion, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	// A raw connection, so "connected but never reads" is expressible at all: any
	// HTTP client drains for you.
	conn, err := net.Dial("tcp", cp.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprintf(conn, "GET %s/%s?%s=%s HTTP/1.1\r\nHost: cp.test\r\nConnection: close\r\n\r\n",
		PackageRoutePrefix, siteID.String(), PackageTokenQueryParam, url.QueryEscape(token)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// The request reaches the handler and takes its slot.
	deadline := time.Now().Add(5 * time.Second)
	for h.streams.inFlight() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the request never took a slot")
		}
		time.Sleep(time.Millisecond)
	}

	// Now nobody reads. Only the write deadline can end this.
	start := time.Now()
	slotDeadline := time.Now().Add(5 * time.Second)
	for h.streams.inFlight() != 0 {
		if time.Now().After(slotDeadline) {
			t.Fatalf("in-flight = %d of %d after %s with a connected, non-reading consumer: the write side has no progress bound",
				h.streams.inFlight(), h.streams.capacity(), time.Since(start))
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Prove the test was not vacuous: if the whole body fit in socket buffers the
	// write never blocked and nothing was demonstrated.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _ := io.Copy(io.Discard, conn)
	if n >= packageSize {
		t.Fatalf("the consumer received %d bytes without ever reading during the stream; raise packageSize so the write actually blocks", n)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// slogCapture collects the process's slog output for the duration of one test.
//
// It is mutex guarded and filtered by site id on the way out, because some tests
// in this package run parallel subtests that outlive their parent and log while
// this is installed. Filtering, rather than assuming exclusive ownership of the
// default logger, is what keeps this from being a source of flakes.
type slogCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *slogCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

// forSite returns the captured lines naming this site id.
func (c *slogCapture) forSite(siteID string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, line := range strings.Split(c.buf.String(), "\n") {
		if strings.Contains(line, siteID) {
			out = append(out, line)
		}
	}
	return out
}

// captureSlog redirects the default logger into a buffer for this test.
func captureSlog(t *testing.T) *slogCapture {
	t.Helper()
	c := &slogCapture{}
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(c, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(restore) })
	return c
}

func assertPackageUnauthorized(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "update_package_unauthorized") {
		t.Fatalf("want the opaque update_package_unauthorized code, got %s", w.Body.String())
	}
}

// decodeManifestClaims unwraps the {manifest, signature} envelope and returns
// the signed claims.
func decodeManifestClaims(t *testing.T, body []byte) signedManifestClaims {
	t.Helper()
	var env struct {
		Manifest  string `json:"manifest"`
		Signature string `json:"signature"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(env.Manifest)
	if err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	var claims signedManifestClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	return claims
}
