package agentupstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/blobstore"
)

// ---------------------------------------------------------------------------
// Fixtures + fakes
// ---------------------------------------------------------------------------

const (
	testOwner   = "mosamlife"
	testRepo    = "wpmgr"
	testTag     = "v0.61.102"
	testVersion = "0.61.99"
)

// fixture builds a self-consistent upstream release: package bytes, the manifest
// that describes them, and the API document whose per-asset digest + size agree
// with both. Each test then breaks exactly one link in that chain.
type fixture struct {
	pkg      []byte
	pkgSHA   string
	manifest []byte
	api      []byte
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pkg := bytes.Repeat([]byte("PK\x03\x04wpmgr-agent"), 512)
	sum := sha256.Sum256(pkg)
	sha := hex.EncodeToString(sum[:])
	f := &fixture{pkg: pkg, pkgSHA: sha}
	f.setManifest(manifestJSON(testVersion, sha, int64(len(pkg)), packageObjectKey(testVersion)))
	return f
}

// setManifest replaces the manifest bytes AND re-derives the release document, so
// the manifest asset's advertised digest and size keep agreeing with the bytes
// the fake upstream serves.
//
// It exists because agent-release.json is digest-verified exactly like the zip:
// a test that swapped f.manifest on its own would be refused for the manifest
// digest rather than for the thing it meant to test.
func (f *fixture) setManifest(body []byte) {
	f.manifest = body
	f.api = f.apiDoc(testTag, "sha256:"+f.pkgSHA, int64(len(f.pkg)))
}

// apiDoc renders the release document for a chosen tag and a chosen account of
// the ZIP asset, always telling the truth about the manifest asset.
func (f *fixture) apiDoc(tag, pkgDigest string, pkgSize int64) []byte {
	sum := sha256.Sum256(f.manifest)
	return apiJSON(tag, "sha256:"+hex.EncodeToString(sum[:]), int64(len(f.manifest)), pkgDigest, pkgSize)
}

// manifestJSON renders an agent-release.json carrying the same extra fields the
// real one does, so a test can prove they survive the mirror.
func manifestJSON(version, sha string, size int64, key string) []byte {
	return []byte(fmt.Sprintf(`{
  "slug": "wpmgr-agent",
  "plugin": "wpmgr-agent/wpmgr-agent.php",
  "version": %q,
  "min_version": "0.61.50",
  "package_object_key": %q,
  "package_sha256": %q,
  "package_size": %d,
  "requires": "6.0",
  "requires_php": "8.1",
  "tested": "6.8",
  "sections": {"description": "WPMgr Agent."}
}`, version, key, sha, size))
}

// mirroredManifestJSON renders a pointer manifest as THIS MIRROR would have
// published it: the same document plus the provenance stamp. Tests that stage a
// previous mirror must use this, because a pointer without the stamp is one the
// mirror refuses to overwrite (see mayReplace).
func mirroredManifestJSON(version, sha string, size int64, key string) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(manifestJSON(version, sha, size, key), &fields); err != nil {
		panic("mirroredManifestJSON: " + err.Error())
	}
	fields[provenanceField] = json.RawMessage(`"` + provenanceMarker + `"`)
	out, err := json.Marshal(fields)
	if err != nil {
		panic("mirroredManifestJSON: " + err.Error())
	}
	return out
}

// apiJSON renders the releases-latest document. browser_download_url and url are
// deliberately hostile: a mirror that ever follows a URL out of this document
// would be caught by TestMirrorNeverFollowsURLsFromFetchedJSON.
func apiJSON(tag, manDigest string, manSize int64, pkgDigest string, pkgSize int64) []byte {
	return []byte(fmt.Sprintf(`{
  "tag_name": %q,
  "url": "https://evil.example.com/api",
  "assets": [
    {
      "name": "agent-release.json",
      "size": %d,
      "digest": %q,
      "url": "https://evil.example.com/manifest",
      "browser_download_url": "https://evil.example.com/manifest.json"
    },
    {
      "name": "wpmgr-agent.zip",
      "size": %d,
      "digest": %q,
      "url": "https://evil.example.com/zip",
      "browser_download_url": "https://evil.example.com/agent.zip"
    }
  ]
}`, tag, manSize, manDigest, pkgSize, pkgDigest))
}

// fakeStore is an in-memory object store recording write ORDER, which is
// load-bearing (package first, pointer last).
type fakeStore struct {
	mu       sync.Mutex
	objects  map[string][]byte
	putOrder []string
	getErr   error
	putErr   map[string]error
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: map[string][]byte{}, putErr: map[string]error{}}
}

func (s *fakeStore) GetViaPresign(_ context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return nil, s.getErr
	}
	b, ok := s.objects[key]
	if !ok {
		// The REAL store distinguishes these (blobstore.GetViaPresign returns
		// ErrNotFound on a 404 and a plain error otherwise), and the mirror now
		// acts on the difference: absent means "nothing published here yet",
		// unreadable means "cannot tell whose pointer that is". A fake that
		// collapsed the two would let a test pass on the wrong branch.
		return nil, blobstore.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (s *fakeStore) Put(_ context.Context, key string, body io.Reader, _ int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err, ok := s.putErr[key]; ok && err != nil {
		return err
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.objects[key] = b
	s.putOrder = append(s.putOrder, key)
	return nil
}

func (s *fakeStore) get(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[key]
	return b, ok
}

func (s *fakeStore) writes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.putOrder...)
}

// fakeDoer answers requests from a URL-keyed table and records every URL asked
// for, so a test can assert BOTH what was fetched and what was never fetched.
type fakeDoer struct {
	mu       sync.Mutex
	handlers map[string]func(*http.Request) (*http.Response, error)
	asked    []string
}

func newFakeDoer() *fakeDoer {
	return &fakeDoer{handlers: map[string]func(*http.Request) (*http.Response, error){}}
}

func (d *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.asked = append(d.asked, req.URL.String())
	h, ok := d.handlers[req.URL.String()]
	d.mu.Unlock()
	if ok {
		return h(req)
	}
	// An unregistered URL is a test failure by construction: it means the mirror
	// fetched something it was never supposed to fetch.
	return nil, fmt.Errorf("fakeDoer: unexpected request to %s", req.URL)
}

func (d *fakeDoer) urls() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.asked...)
}

func okResponse(body []byte, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     header,
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

// hdr builds a header via Set so the key is canonicalized exactly the way
// net/http canonicalizes a real response's headers. A raw map literal is NOT
// equivalent: http.Header.Get("ETag") looks up the canonical "Etag", so a literal
// "ETag" key would silently never be found and the test would assert nothing.
func hdr(key, value string) http.Header {
	h := http.Header{}
	h.Set(key, value)
	return h
}

func statusResponse(code int, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: code, Header: header, Body: io.NopCloser(bytes.NewReader(nil))}
}

// wire builds a doer serving the standard three URLs from f, with optional
// overrides applied afterwards.
func wire(f *fixture) *fakeDoer {
	d := newFakeDoer()
	d.handlers[apiURL(testOwner, testRepo)] = func(*http.Request) (*http.Response, error) {
		return okResponse(f.api, hdr("ETag", `W/"abc"`)), nil
	}
	d.handlers[downloadURL(testOwner, testRepo, testTag, manifestAssetName)] = func(*http.Request) (*http.Response, error) {
		return okResponse(f.manifest, nil), nil
	}
	d.handlers[downloadURL(testOwner, testRepo, testTag, packageAssetName)] = func(*http.Request) (*http.Response, error) {
		return okResponse(f.pkg, nil), nil
	}
	return d
}

func newTestMirror(store Store, doer HTTPDoer) *Mirror {
	return NewMirror(store, doer, testOwner, testRepo, nil)
}

// ---------------------------------------------------------------------------
// Happy path
// ---------------------------------------------------------------------------

// TestMirrorHappyPathWritesPackageBeforePointer pins the whole chain AND the
// write ORDER. Order is not cosmetic: latest.json must never name an object that
// is not yet in place, or an agent reading the pointer in that window is offered
// a package that 404s (scripts/release-agent.sh documents the same ordering).
func TestMirrorHappyPathWritesPackageBeforePointer(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()
	m := newTestMirror(store, wire(f))

	res, err := m.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Mirrored || res.Version != testVersion {
		t.Fatalf("Run = %+v, want mirrored version %s", res, testVersion)
	}

	writes := store.writes()
	wantOrder := []string{packageObjectKey(testVersion), ManifestKey}
	if len(writes) != 2 || writes[0] != wantOrder[0] || writes[1] != wantOrder[1] {
		t.Fatalf("write order = %v, want %v (package FIRST, pointer LAST)", writes, wantOrder)
	}

	gotPkg, ok := store.get(packageObjectKey(testVersion))
	if !ok || !bytes.Equal(gotPkg, f.pkg) {
		t.Fatalf("mirrored package bytes do not match upstream")
	}
}

// TestMirrorPublishesEveryUpstreamFieldPlusItsProvenanceStamp: the mirrored
// latest.json must carry every field upstream published, with its value
// untouched, and exactly one addition of this mirror's own.
//
// The pass-through half is the original point: min_version in particular is a
// real control ("you must already be at least X to take this"), and a mirror
// that re-serialised only the fields it validates would silently permit a jump
// the release meant to forbid. The addition is the provenance stamp, which is
// what stops a later run from overwriting a pointer this mirror did not write.
func TestMirrorPublishesEveryUpstreamFieldPlusItsProvenanceStamp(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()
	m := newTestMirror(store, wire(f))

	if _, err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, ok := store.get(ManifestKey)
	if !ok {
		t.Fatal("latest.json was not written")
	}

	var upstream, decoded map[string]any
	if err := json.Unmarshal(f.manifest, &upstream); err != nil {
		t.Fatalf("fixture manifest is not valid JSON: %v", err)
	}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("mirrored latest.json is not valid JSON: %v", err)
	}
	for k, want := range upstream {
		if !reflect.DeepEqual(decoded[k], want) {
			t.Fatalf("field %q = %#v, want it passed through as %#v", k, decoded[k], want)
		}
	}
	if decoded["min_version"] != "0.61.50" {
		t.Fatalf("min_version = %v, want it preserved through the mirror", decoded["min_version"])
	}
	if decoded["tested"] != "6.8" || decoded["requires_php"] != "8.1" {
		t.Fatalf("manifest metadata lost through the mirror: %v", decoded)
	}
	if decoded[provenanceField] != provenanceMarker {
		t.Fatalf("%s = %v, want the provenance stamp %q", provenanceField, decoded[provenanceField], provenanceMarker)
	}
	if decoded[provenanceSourceField] != testOwner+"/"+testRepo+"@"+testTag {
		t.Fatalf("%s = %v, want the upstream release it came from", provenanceSourceField, decoded[provenanceSourceField])
	}
	// The stamp is the ONLY addition: nothing else appears out of nowhere.
	if len(decoded) != len(upstream)+2 {
		t.Fatalf("mirrored manifest has %d fields, want the upstream %d plus the two provenance fields", len(decoded), len(upstream))
	}
	// And the published document still passes the validation every consumer of
	// latest.json applies, stamp and all.
	if _, err := validateManifest(got); err != nil {
		t.Fatalf("the stamped manifest no longer validates: %v", err)
	}
}

// TestMirrorNeverFollowsURLsFromFetchedJSON is the core SSRF/redirection control:
// every URL is CONSTRUCTED from configured owner/repo + the validated tag + a
// fixed asset name. The fixture's release document advertises evil.example.com in
// url and browser_download_url for both assets; nothing may ever be fetched from
// there. It also pins the /releases/download/<tag>/<name> form (one redirect)
// over /releases/latest/download/<name> (two).
func TestMirrorNeverFollowsURLsFromFetchedJSON(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()
	doer := wire(f)
	m := newTestMirror(store, doer)

	if _, err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []string{
		"https://api.github.com/repos/mosamlife/wpmgr/releases/latest",
		"https://github.com/mosamlife/wpmgr/releases/download/v0.61.102/agent-release.json",
		"https://github.com/mosamlife/wpmgr/releases/download/v0.61.102/wpmgr-agent.zip",
	}
	got := doer.urls()
	if len(got) != len(want) {
		t.Fatalf("requested %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("request %d = %s, want %s", i, got[i], want[i])
		}
	}
	for _, u := range got {
		if strings.Contains(u, "evil.example.com") {
			t.Fatalf("followed a URL taken from the fetched JSON: %s", u)
		}
		if strings.Contains(u, "/releases/latest/download/") {
			t.Fatalf("used the two-redirect /releases/latest/download form: %s", u)
		}
	}
}

// ---------------------------------------------------------------------------
// Cross-check: manifest vs the API's per-asset digest and size
// ---------------------------------------------------------------------------

// TestMirrorDigestMismatchRefusesAndPreservesPreviousMirror is the headline
// guard. A manifest sha256 that disagrees with the API's per-asset digest means
// the asset was replaced after publish, or the upload was partial. The run must
// refuse, download nothing, and leave the PREVIOUS mirror exactly as it was.
func TestMirrorDigestMismatchRefusesAndPreservesPreviousMirror(t *testing.T) {
	f := newFixture(t)
	// The API advertises a different digest than the manifest claims.
	f.api = f.apiDoc(testTag, "sha256:"+strings.Repeat("b", 64), int64(len(f.pkg)))

	store := newFakeStore()
	// A previous, good mirror is already in place.
	prevManifest := manifestJSON("0.61.90", strings.Repeat("c", 64), 1234, packageObjectKey("0.61.90"))
	prevPkg := []byte("previous package bytes")
	store.objects[ManifestKey] = prevManifest
	store.objects[packageObjectKey("0.61.90")] = prevPkg

	doer := wire(f)
	m := newTestMirror(store, doer)

	_, err := m.Run(context.Background())
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("Run err = %v, want ErrRefused", err)
	}
	if w := store.writes(); len(w) != 0 {
		t.Fatalf("wrote %v on a refused release; nothing may be written", w)
	}
	if got, _ := store.get(ManifestKey); !bytes.Equal(got, prevManifest) {
		t.Fatal("previous latest.json was modified by a refused run")
	}
	if got, _ := store.get(packageObjectKey("0.61.90")); !bytes.Equal(got, prevPkg) {
		t.Fatal("previous package object was modified by a refused run")
	}
	// The package must never have been downloaded: the cross-check happens first.
	for _, u := range doer.urls() {
		if strings.HasSuffix(u, packageAssetName) {
			t.Fatalf("downloaded the package despite a digest mismatch: %s", u)
		}
	}
}

// TestMirrorSizeMismatchRefuses: the same guard on the other field. A manifest
// size that disagrees with the API's asset size is a partial upload.
func TestMirrorSizeMismatchRefuses(t *testing.T) {
	f := newFixture(t)
	f.api = f.apiDoc(testTag, "sha256:"+f.pkgSHA, int64(len(f.pkg))-1)

	store := newFakeStore()
	doer := wire(f)
	m := newTestMirror(store, doer)

	_, err := m.Run(context.Background())
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("Run err = %v, want ErrRefused", err)
	}
	if w := store.writes(); len(w) != 0 {
		t.Fatalf("wrote %v on a size mismatch", w)
	}
	for _, u := range doer.urls() {
		if strings.HasSuffix(u, packageAssetName) {
			t.Fatalf("downloaded the package despite a size mismatch: %s", u)
		}
	}
}

// TestMirrorMissingOrMalformedDigestRefuses: the cross-check must not be
// skippable by omission. An asset with no digest (or a digest this package cannot
// parse) is unverifiable, and unverifiable is refused, never mirrored on trust.
func TestMirrorMissingOrMalformedDigestRefuses(t *testing.T) {
	cases := []struct{ name, digest string }{
		{"absent", ""},
		{"unprefixed", strings.Repeat("a", 64)},
		{"wrong algorithm", "sha1:" + strings.Repeat("a", 40)},
		{"short hex", "sha256:" + strings.Repeat("a", 63)},
		{"uppercase hex", "sha256:" + strings.ToUpper(strings.Repeat("ab", 32))},
		{"not hex", "sha256:" + strings.Repeat("z", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.api = f.apiDoc(testTag, tc.digest, int64(len(f.pkg)))
			store := newFakeStore()
			m := newTestMirror(store, wire(f))

			if _, err := m.Run(context.Background()); !errors.Is(err, ErrRefused) {
				t.Fatalf("Run err = %v, want ErrRefused", err)
			}
			if w := store.writes(); len(w) != 0 {
				t.Fatalf("wrote %v for digest %q", w, tc.digest)
			}
		})
	}
}

// TestMirrorManifestAssetIsDigestVerifiedToo: agent-release.json gets the same
// treatment as the zip.
//
// The justification for cross-checking the package is that the API response and
// the asset bytes are independent attestations. That applies at least as much to
// the manifest, which is the document every other check here derives FROM: the
// version that becomes an object key, the sha256 the package is held to, the
// min_version an agent enforces. Fetching its digest and discarding it left all
// of those resting on an unverified input.
func TestMirrorManifestAssetIsDigestVerifiedToo(t *testing.T) {
	cases := []struct {
		name    string
		breakIt func(f *fixture, d *fakeDoer)
	}{
		{"served bytes differ from the advertised digest", func(f *fixture, d *fakeDoer) {
			// The API keeps advertising the digest of the real manifest while the
			// asset host serves a different document of the same length.
			swapped := bytes.Replace(f.manifest, []byte(`"min_version": "0.61.50"`), []byte(`"min_version": "0.00.00"`), 1)
			if len(swapped) != len(f.manifest) {
				t.Fatalf("test bug: swapped manifest changed length")
			}
			d.handlers[downloadURL(testOwner, testRepo, testTag, manifestAssetName)] = func(*http.Request) (*http.Response, error) {
				return okResponse(swapped, nil), nil
			}
		}},
		{"truncated manifest download", func(f *fixture, d *fakeDoer) {
			d.handlers[downloadURL(testOwner, testRepo, testTag, manifestAssetName)] = func(*http.Request) (*http.Response, error) {
				return okResponse(f.manifest[:len(f.manifest)-10], nil), nil
			}
		}},
		{"manifest asset carries no digest", func(f *fixture, _ *fakeDoer) {
			f.api = apiJSON(testTag, "", int64(len(f.manifest)), "sha256:"+f.pkgSHA, int64(len(f.pkg)))
		}},
		{"manifest asset digest is malformed", func(f *fixture, _ *fakeDoer) {
			f.api = apiJSON(testTag, "sha1:"+strings.Repeat("a", 40), int64(len(f.manifest)), "sha256:"+f.pkgSHA, int64(len(f.pkg)))
		}},
		{"manifest asset size is non-positive", func(f *fixture, _ *fakeDoer) {
			sum := sha256.Sum256(f.manifest)
			f.api = apiJSON(testTag, "sha256:"+hex.EncodeToString(sum[:]), 0, "sha256:"+f.pkgSHA, int64(len(f.pkg)))
		}},
		{"manifest asset is over the cap", func(f *fixture, _ *fakeDoer) {
			sum := sha256.Sum256(f.manifest)
			f.api = apiJSON(testTag, "sha256:"+hex.EncodeToString(sum[:]), maxManifestBytes+1, "sha256:"+f.pkgSHA, int64(len(f.pkg)))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			store := newFakeStore()
			doer := wire(f)
			tc.breakIt(f, doer)
			// wire() captured f by pointer, so an f.api swap above is already live.
			m := newTestMirror(store, doer)

			if _, err := m.Run(context.Background()); !errors.Is(err, ErrRefused) {
				t.Fatalf("Run err = %v, want ErrRefused", err)
			}
			if w := store.writes(); len(w) != 0 {
				t.Fatalf("wrote %v for an unverifiable manifest asset", w)
			}
			for _, u := range doer.urls() {
				if strings.HasSuffix(u, packageAssetName) {
					t.Fatalf("downloaded the package on the strength of an unverified manifest: %s", u)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Manifest validation (same rules as internal/agent/update_handler.go readLatest)
// ---------------------------------------------------------------------------

// TestMirrorRejectsInvalidManifest walks every rule readLatest applies. The
// object-key cases are the security-relevant ones: latest.json's
// package_object_key is what the control plane later presigns, so a manifest
// naming any other object inside the agent-releases/ prefix (a stale zip,
// latest.json itself, another version's package) must never be mirrored.
//
// The "hostile version" cases below are the ones the key rule alone does NOT
// catch, and they matter here more than anywhere else because this is the
// package that performs the WRITE. Checking that package_object_key equals the
// key built from the version proves the document agrees with itself, and both
// halves come out of the same fetched file: a self-consistent hostile pair
// passes. A version of "../../backups/tenant-a/2026-07" with the matching key
// would have been written to
// "agent-releases/../../backups/tenant-a/2026-07/wpmgr-agent.zip". Reaching this
// needs control of agent-release.json on the configured owner/repo, and pointing
// the mirror at a fork is a supported configuration, so the version is
// shape-checked with the same validator the serving route applies.
func TestMirrorRejectsInvalidManifest(t *testing.T) {
	good := packageObjectKey(testVersion)
	sha := strings.Repeat("a", 64)
	// hostileVersion renders a manifest that is SELF-CONSISTENT around a hostile
	// version: package_object_key is exactly the key that version builds, so the
	// key rule has nothing to object to.
	hostileVersion := func(version string) []byte {
		return []byte(`{"slug":"wpmgr-agent","version":` + strconv.Quote(version) +
			`,"package_object_key":` + strconv.Quote(packageObjectKey(version)) +
			`,"package_sha256":"` + sha + `","package_size":10}`)
	}
	cases := []struct {
		name string
		body []byte
	}{
		{"version traverses out of the prefix", hostileVersion("../../backups/tenant-a/2026-07")},
		{"version traverses mid-string", hostileVersion("1.0.0/../../secrets")},
		{"version has path separators", hostileVersion("a/b/c")},
		{"version has spaces", hostileVersion("a b c")},
		{"version has a query string", hostileVersion("v1?x=1")},
		{"version is url-encoded traversal", hostileVersion("%2e%2e%2f")},
		{"version is a bare dot-run", hostileVersion("..")},
		{"version is absurdly long", hostileVersion(strings.Repeat("9", 101))},
		// The cases below contain only characters that are legal in a key
		// segment, so nothing about the CHARACTER set refuses them: they are
		// refused for not being versions at all. "latest.json" is the one that
		// matters, because it is the pointer object's own name and would build
		// agent-releases/latest.json/wpmgr-agent.zip, a key that reads as a
		// child of the pointer sitting beside it.
		{"version is a bare dot", hostileVersion(".")},
		{"version is the pointer's own name", hostileVersion("latest.json")},
		{"version is a digitless word", hostileVersion("nightly")},
		{"version leads with a separator", hostileVersion(".json")},
		{"version leads with a dash", hostileVersion("-1.0.0")},
		{"wrong slug", []byte(`{"slug":"fleet-agent-site-manager","version":"0.61.99","package_object_key":"` + good + `","package_sha256":"` + sha + `","package_size":10}`)},
		{"empty slug", []byte(`{"slug":"","version":"0.61.99","package_object_key":"` + good + `","package_sha256":"` + sha + `","package_size":10}`)},
		{"empty version", []byte(`{"slug":"wpmgr-agent","version":"","package_object_key":"` + good + `","package_sha256":"` + sha + `","package_size":10}`)},
		{"sha too short", []byte(`{"slug":"wpmgr-agent","version":"0.61.99","package_object_key":"` + good + `","package_sha256":"abc","package_size":10}`)},
		{"sha uppercase", []byte(`{"slug":"wpmgr-agent","version":"0.61.99","package_object_key":"` + good + `","package_sha256":"` + strings.ToUpper(strings.Repeat("ab", 32)) + `","package_size":10}`)},
		{"sha not hex", []byte(`{"slug":"wpmgr-agent","version":"0.61.99","package_object_key":"` + good + `","package_sha256":"` + strings.Repeat("z", 64) + `","package_size":10}`)},
		{"zero size", []byte(`{"slug":"wpmgr-agent","version":"0.61.99","package_object_key":"` + good + `","package_sha256":"` + sha + `","package_size":0}`)},
		{"negative size", []byte(`{"slug":"wpmgr-agent","version":"0.61.99","package_object_key":"` + good + `","package_sha256":"` + sha + `","package_size":-1}`)},
		{"size over cap", []byte(`{"slug":"wpmgr-agent","version":"0.61.99","package_object_key":"` + good + `","package_sha256":"` + sha + `","package_size":34000000}`)},
		{"key is latest.json", []byte(`{"slug":"wpmgr-agent","version":"0.61.99","package_object_key":"agent-releases/latest.json","package_sha256":"` + sha + `","package_size":10}`)},
		{"key for another version", []byte(`{"slug":"wpmgr-agent","version":"0.61.99","package_object_key":"agent-releases/0.61.98/wpmgr-agent.zip","package_sha256":"` + sha + `","package_size":10}`)},
		{"key outside the prefix", []byte(`{"slug":"wpmgr-agent","version":"0.61.99","package_object_key":"backups/evil.zip","package_sha256":"` + sha + `","package_size":10}`)},
		{"key traverses out of the prefix", []byte(`{"slug":"wpmgr-agent","version":"0.61.99","package_object_key":"agent-releases/../secrets.zip","package_sha256":"` + sha + `","package_size":10}`)},
		{"key with a different filename", []byte(`{"slug":"wpmgr-agent","version":"0.61.99","package_object_key":"agent-releases/0.61.99/other.zip","package_sha256":"` + sha + `","package_size":10}`)},
		{"not json", []byte(`not json at all`)},
		{"empty body", []byte(``)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := validateManifest(tc.body); !errors.Is(err, ErrRefused) {
				t.Fatalf("validateManifest err = %v, want ErrRefused", err)
			}
		})
	}
}

// TestMirrorBadObjectKeyIsRejectedEndToEnd proves the object-key rule refuses the
// whole RUN, not just the parse: nothing is downloaded and nothing is written.
func TestMirrorBadObjectKeyIsRejectedEndToEnd(t *testing.T) {
	f := newFixture(t)
	f.setManifest(manifestJSON(testVersion, f.pkgSHA, int64(len(f.pkg)), "agent-releases/latest.json"))

	store := newFakeStore()
	doer := wire(f)
	m := newTestMirror(store, doer)

	if _, err := m.Run(context.Background()); !errors.Is(err, ErrRefused) {
		t.Fatalf("Run err = %v, want ErrRefused", err)
	}
	if w := store.writes(); len(w) != 0 {
		t.Fatalf("wrote %v for a manifest naming a bad object key", w)
	}
	for _, u := range doer.urls() {
		if strings.HasSuffix(u, packageAssetName) {
			t.Fatalf("downloaded the package despite an invalid manifest: %s", u)
		}
	}
}

// TestMirrorNeverInfersVersionFromTag: an unreadable manifest STOPS the run. The
// release tag is the control plane's release number and no longer tracks the
// agent's own version, so synthesising a version from it would publish the wrong
// number for the code in the zip, and the agent's downgrade guard would then
// refuse every later, correctly numbered offer.
func TestMirrorNeverInfersVersionFromTag(t *testing.T) {
	f := newFixture(t)
	f.setManifest([]byte(`{"slug":"wpmgr-agent"}`))

	store := newFakeStore()
	m := newTestMirror(store, wire(f))

	res, err := m.Run(context.Background())
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("Run err = %v, want ErrRefused", err)
	}
	if res.Version != "" {
		t.Fatalf("Result.Version = %q, want empty (no version may be inferred)", res.Version)
	}
	if w := store.writes(); len(w) != 0 {
		t.Fatalf("wrote %v with no valid version", w)
	}
	if _, ok := store.get(packageObjectKey(strings.TrimPrefix(testTag, "v"))); ok {
		t.Fatal("mirrored under a version derived from the tag")
	}
}

// ---------------------------------------------------------------------------
// Download integrity
// ---------------------------------------------------------------------------

// TestMirrorTruncatedDownloadWritesNothing: a body that ends early hashes
// differently AND falls short of the declared size. Either way nothing reaches
// storage, so a half-downloaded zip can never be offered to a site.
func TestMirrorTruncatedDownloadWritesNothing(t *testing.T) {
	f := newFixture(t)
	doer := wire(f)
	doer.handlers[downloadURL(testOwner, testRepo, testTag, packageAssetName)] = func(*http.Request) (*http.Response, error) {
		return okResponse(f.pkg[:len(f.pkg)-100], nil), nil
	}

	store := newFakeStore()
	m := newTestMirror(store, doer)

	_, err := m.Run(context.Background())
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("Run err = %v, want ErrRefused", err)
	}
	if w := store.writes(); len(w) != 0 {
		t.Fatalf("wrote %v for a truncated download; nothing may be written", w)
	}
}

// TestMirrorCorruptedDownloadWritesNothing: same size, different bytes. Only the
// hash catches this one, which is exactly why the hash is computed over the
// stream rather than trusted from the manifest.
func TestMirrorCorruptedDownloadWritesNothing(t *testing.T) {
	f := newFixture(t)
	corrupt := append([]byte(nil), f.pkg...)
	corrupt[len(corrupt)/2] ^= 0xFF

	doer := wire(f)
	doer.handlers[downloadURL(testOwner, testRepo, testTag, packageAssetName)] = func(*http.Request) (*http.Response, error) {
		return okResponse(corrupt, nil), nil
	}

	store := newFakeStore()
	m := newTestMirror(store, doer)

	if _, err := m.Run(context.Background()); !errors.Is(err, ErrRefused) {
		t.Fatalf("Run err = %v, want ErrRefused", err)
	}
	if w := store.writes(); len(w) != 0 {
		t.Fatalf("wrote %v for a corrupted download", w)
	}
}

// TestMirrorOversizeDownloadWritesNothing: a body LONGER than the manifest
// declares is refused at the cap rather than silently truncated to a hash that
// would then fail for the wrong reason. This is the guard that bounds memory.
func TestMirrorOversizeDownloadWritesNothing(t *testing.T) {
	f := newFixture(t)
	doer := wire(f)
	doer.handlers[downloadURL(testOwner, testRepo, testTag, packageAssetName)] = func(*http.Request) (*http.Response, error) {
		return okResponse(append(append([]byte(nil), f.pkg...), bytes.Repeat([]byte("x"), 4096)...), nil), nil
	}

	store := newFakeStore()
	m := newTestMirror(store, doer)

	if _, err := m.Run(context.Background()); !errors.Is(err, ErrRefused) {
		t.Fatalf("Run err = %v, want ErrRefused", err)
	}
	if w := store.writes(); len(w) != 0 {
		t.Fatalf("wrote %v for an oversize download", w)
	}
}

// TestMirrorRefusesAssetOverTheHardCap: a manifest/asset declaring more than
// MaxPackageBytes is refused BEFORE any download starts, which is what keeps the
// buffered read bounded.
func TestMirrorRefusesAssetOverTheHardCap(t *testing.T) {
	f := newFixture(t)
	f.api = f.apiDoc(testTag, "sha256:"+f.pkgSHA, MaxPackageBytes+1)

	store := newFakeStore()
	doer := wire(f)
	m := newTestMirror(store, doer)

	if _, err := m.Run(context.Background()); !errors.Is(err, ErrRefused) {
		t.Fatalf("Run err = %v, want ErrRefused", err)
	}
	for _, u := range doer.urls() {
		if strings.HasSuffix(u, packageAssetName) {
			t.Fatalf("started downloading an over-cap asset: %s", u)
		}
	}
}

// ---------------------------------------------------------------------------
// Idempotency
// ---------------------------------------------------------------------------

// TestMirrorAlreadyCurrentIsANoOp: when the mirrored version already matches
// upstream, the run downloads nothing and writes nothing. Re-downloading a
// multi-MB zip every 6 hours forever would be the obvious way to get this
// feature turned off again.
func TestMirrorAlreadyCurrentIsANoOp(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()
	store.objects[ManifestKey] = f.manifest
	store.objects[packageObjectKey(testVersion)] = f.pkg

	doer := wire(f)
	m := newTestMirror(store, doer)

	res, err := m.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Mirrored {
		t.Fatal("re-mirrored a version that was already current")
	}
	if res.Reason != "already_current" || res.Version != testVersion {
		t.Fatalf("Run = %+v, want already_current at %s", res, testVersion)
	}
	if w := store.writes(); len(w) != 0 {
		t.Fatalf("wrote %v when already current", w)
	}
	for _, u := range doer.urls() {
		if strings.HasSuffix(u, packageAssetName) {
			t.Fatalf("re-downloaded the package when already current: %s", u)
		}
	}
}

// TestMirrorSameVersionDifferentBytesRefuses: a published version is immutable by
// construction (its object key contains the version). Upstream naming the same
// version with different bytes is the "replaced after publish" case again, and
// overwriting would rewrite a version some sites have already installed and
// verified.
func TestMirrorSameVersionDifferentBytesRefuses(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()
	store.objects[ManifestKey] = mirroredManifestJSON(testVersion, strings.Repeat("d", 64), 4242, packageObjectKey(testVersion))

	doer := wire(f)
	m := newTestMirror(store, doer)

	_, err := m.Run(context.Background())
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("Run err = %v, want ErrRefused", err)
	}
	// Specifically the republish rule, not the provenance rule: the pointer in
	// place is one this mirror wrote, so the only thing left to refuse it is the
	// version being republished with different bytes.
	if errors.Is(err, ErrForeignChannel) {
		t.Fatalf("Run err = %v, want the republish refusal, not the provenance one", err)
	}
	if w := store.writes(); len(w) != 0 {
		t.Fatalf("overwrote an already-mirrored version: %v", w)
	}
}

// TestMirrorNewVersionOverAnOldMirrorWrites: the ordinary upgrade case. The old
// version's package object is left alone (it is immutable and other sites may
// still be installing it); only the new package and the pointer are written.
func TestMirrorNewVersionOverAnOldMirrorWrites(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()
	oldPkg := []byte("old package")
	store.objects[ManifestKey] = mirroredManifestJSON("0.61.90", strings.Repeat("c", 64), int64(len(oldPkg)), packageObjectKey("0.61.90"))
	store.objects[packageObjectKey("0.61.90")] = oldPkg

	m := newTestMirror(store, wire(f))
	res, err := m.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Mirrored {
		t.Fatalf("Run = %+v, want a new version mirrored", res)
	}
	if got, _ := store.get(packageObjectKey("0.61.90")); !bytes.Equal(got, oldPkg) {
		t.Fatal("the previous version's package object was disturbed")
	}
}

// TestMirrorNotModifiedIsANoOp: the 304 short-circuit. It is what keeps this job
// comfortably inside the unauthenticated 60-requests-per-hour-per-IP limit.
func TestMirrorNotModifiedIsANoOp(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()
	doer := wire(f)
	doer.handlers[apiURL(testOwner, testRepo)] = func(*http.Request) (*http.Response, error) {
		return statusResponse(http.StatusNotModified, nil), nil
	}
	m := newTestMirror(store, doer)

	res, err := m.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Mirrored || res.Reason != "not_modified" {
		t.Fatalf("Run = %+v, want a not_modified no-op", res)
	}
	if len(doer.urls()) != 1 {
		t.Fatalf("made %v requests after a 304; want only the API request", doer.urls())
	}
	if w := store.writes(); len(w) != 0 {
		t.Fatalf("wrote %v after a 304", w)
	}
}

// TestMirrorSendsIfNoneMatchAfterSeeingAnETag: the ETag from the first run is
// replayed on the next, which is what makes the 304 above reachable at all.
func TestMirrorSendsIfNoneMatchAfterSeeingAnETag(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()
	var seen []string
	doer := wire(f)
	doer.handlers[apiURL(testOwner, testRepo)] = func(r *http.Request) (*http.Response, error) {
		seen = append(seen, r.Header.Get("If-None-Match"))
		return okResponse(f.api, hdr("ETag", `W/"etag-1"`)), nil
	}
	m := newTestMirror(store, doer)

	if _, err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	// The spacing guard would block a second immediate run, so clear it: this
	// test is about the ETag, and the spacing guard has its own test.
	m.mu.Lock()
	m.lastRequestAt = m.lastRequestAt.Add(-2 * minRequestSpacing)
	m.mu.Unlock()

	if _, err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("saw %d API requests, want 2", len(seen))
	}
	if seen[0] != "" {
		t.Fatalf("first request sent If-None-Match %q, want none", seen[0])
	}
	if seen[1] != `W/"etag-1"` {
		t.Fatalf("second request sent If-None-Match %q, want the ETag from the first response", seen[1])
	}
}

// conditionalAPI wires an upstream that behaves like GitHub does: it answers 304
// to a request whose If-None-Match matches the ETag it last served, and 200 with
// the full document otherwise. It records every If-None-Match it saw.
func conditionalAPI(f *fixture, etag string, seen *[]string) *fakeDoer {
	d := wire(f)
	d.handlers[apiURL(testOwner, testRepo)] = func(r *http.Request) (*http.Response, error) {
		inm := r.Header.Get("If-None-Match")
		*seen = append(*seen, inm)
		if inm == etag {
			return statusResponse(http.StatusNotModified, hdr("ETag", etag)), nil
		}
		return okResponse(f.api, hdr("ETag", etag)), nil
	}
	return d
}

// TestMirrorFailedRunDoesNotBankTheETag is the anti-wedge test.
//
// The ETag used to be stored the moment the release document parsed, which is
// BEFORE the manifest is validated, before the package is downloaded, and before
// either write. So one transient failure (here: the package write) banked it
// anyway, the next run sent If-None-Match, got a 304, and that version was never
// mirrored again for the life of the process. On a self-hosted control plane
// that runs for months, "restart it" is not a recovery.
//
// Run 1 fetches successfully and then fails its package write. Run 2 must
// therefore re-fetch the FULL document (no If-None-Match) and publish.
func TestMirrorFailedRunDoesNotBankTheETag(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()
	store.putErr[packageObjectKey(testVersion)] = errors.New("storage unavailable")

	var seen []string
	m := newTestMirror(store, conditionalAPI(f, `W/"etag-1"`, &seen))

	if _, err := m.Run(context.Background()); err == nil {
		t.Fatal("Run 1 err = nil, want the staged package-write failure")
	}

	// The spacing guard has its own test; this one is about the ETag.
	m.mu.Lock()
	m.lastRequestAt = m.lastRequestAt.Add(-2 * minRequestSpacing)
	m.mu.Unlock()

	// Storage recovers.
	store.mu.Lock()
	delete(store.putErr, packageObjectKey(testVersion))
	store.mu.Unlock()

	res, err := m.Run(context.Background())
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("saw %d API requests, want 2", len(seen))
	}
	if seen[1] != "" {
		t.Fatalf("run 2 sent If-None-Match %q after run 1 FAILED; a failed run must not bank the ETag", seen[1])
	}
	if !res.Mirrored || res.Version != testVersion {
		t.Fatalf("Run 2 = %+v, want the release finally mirrored", res)
	}
}

// TestMirrorRefusedRunDoesNotBankTheETag: the same rule for the other family of
// non-terminal outcomes. A release refused today may be republished correctly
// tomorrow under the same ETag, and the next run has to be able to see it.
func TestMirrorRefusedRunDoesNotBankTheETag(t *testing.T) {
	f := newFixture(t)
	f.setManifest(manifestJSON(testVersion, f.pkgSHA, int64(len(f.pkg)), "agent-releases/latest.json"))

	var seen []string
	store := newFakeStore()
	m := newTestMirror(store, conditionalAPI(f, `W/"etag-1"`, &seen))

	if _, err := m.Run(context.Background()); !errors.Is(err, ErrRefused) {
		t.Fatalf("Run 1 err = %v, want ErrRefused", err)
	}
	m.mu.Lock()
	m.lastRequestAt = m.lastRequestAt.Add(-2 * minRequestSpacing)
	m.mu.Unlock()

	if _, err := m.Run(context.Background()); !errors.Is(err, ErrRefused) {
		t.Fatalf("Run 2 err = %v, want the same refusal re-evaluated from a full body", err)
	}
	if len(seen) != 2 || seen[1] != "" {
		t.Fatalf("If-None-Match seen = %v, want run 2 to re-fetch the full document", seen)
	}
}

// TestMirrorAlreadyCurrentBanksTheETag: "already current" IS a terminal success,
// so it does bank the ETag. That is what keeps the steady state (nothing new for
// weeks) cheap against a 60-requests-per-hour budget.
func TestMirrorAlreadyCurrentBanksTheETag(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()
	store.objects[ManifestKey] = mirroredManifestJSON(testVersion, f.pkgSHA, int64(len(f.pkg)), packageObjectKey(testVersion))

	var seen []string
	m := newTestMirror(store, conditionalAPI(f, `W/"etag-1"`, &seen))

	res, err := m.Run(context.Background())
	if err != nil || res.Reason != "already_current" {
		t.Fatalf("Run 1 = (%+v, %v), want already_current", res, err)
	}
	m.mu.Lock()
	m.lastRequestAt = m.lastRequestAt.Add(-2 * minRequestSpacing)
	m.mu.Unlock()

	res, err = m.Run(context.Background())
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if res.Reason != "not_modified" {
		t.Fatalf("Run 2 = %+v, want the cheap 304 path", res)
	}
	if len(seen) != 2 || seen[1] != `W/"etag-1"` {
		t.Fatalf("If-None-Match seen = %v, want run 2 to replay the ETag", seen)
	}
}

// allowAnotherRequest winds back the spacing guard so a test can run a second
// cycle immediately. The guard has its own test (TestMirrorMinRequestSpacing);
// every test that calls this is about something else.
func allowAnotherRequest(m *Mirror) {
	m.mu.Lock()
	m.lastRequestAt = m.lastRequestAt.Add(-2 * minRequestSpacing)
	m.mu.Unlock()
}

// TestMirrorNotModifiedCannotHideADeletedPointer is the anti-wedge test for the
// OTHER half of the decision, and it is the one the mirror's own log promised.
//
// The stand-down message tells the operator, verbatim, to remove
// agent-releases/latest.json to hand the channel over. While the 304
// short-circuit ran before the store was ever read, an operator who did exactly
// that got not_modified forever: the ETag says "upstream is unchanged", which is
// TRUE and beside the point, because the pointer moved and the pointer is half
// of the decision. It self-healed only on a process restart (the ETag is in
// memory) or the next upstream release, which on a self-hosted control plane can
// be weeks away. The same shape covers a bucket restore that loses the prefix.
func TestMirrorNotModifiedCannotHideADeletedPointer(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()
	var seen []string
	m := newTestMirror(store, conditionalAPI(f, `W/"etag-1"`, &seen))

	res, err := m.Run(context.Background())
	if err != nil || !res.Mirrored {
		t.Fatalf("run 1 = (%+v, %v), want the release published", res, err)
	}
	allowAnotherRequest(m)

	// The operator does exactly what the stand-down remedy tells them to do.
	store.mu.Lock()
	delete(store.objects, ManifestKey)
	store.mu.Unlock()

	res, err = m.Run(context.Background())
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if !res.Mirrored {
		t.Fatalf("run 2 = %+v, want the pointer republished; a 304 must not answer for local state that changed", res)
	}
	if _, ok := store.get(ManifestKey); !ok {
		t.Fatal("the deleted pointer was never restored, so the documented remedy does not work")
	}
	if len(seen) != 2 || seen[1] != "" {
		t.Fatalf("If-None-Match seen = %v, want run 2 to drop the banked ETag and re-fetch the full document", seen)
	}
}

// TestMirrorNotModifiedCannotHideAForeignPointer: the same blindness in the
// direction that costs an operator their release channel silently.
//
// If an operator publishes their own build over the mirror's pointer, the mirror
// must NOTICE and stand down loudly (ErrForeignChannel). Answering not_modified
// instead reports "nothing to do" about a channel that just changed hands, and
// the operator gets no signal at all until something upstream moves.
func TestMirrorNotModifiedCannotHideAForeignPointer(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()
	var seen []string
	m := newTestMirror(store, conditionalAPI(f, `W/"etag-1"`, &seen))

	if res, err := m.Run(context.Background()); err != nil || !res.Mirrored {
		t.Fatalf("run 1 = (%+v, %v), want the release published", res, err)
	}
	allowAnotherRequest(m)

	// An operator publishes their own build over it: same key, no stamp.
	foreign := manifestJSON("0.61.90", strings.Repeat("c", 64), 4242, packageObjectKey("0.61.90"))
	store.mu.Lock()
	store.objects[ManifestKey] = foreign
	store.mu.Unlock()

	res, err := m.Run(context.Background())
	if res.Reason == "not_modified" {
		t.Fatalf("run 2 = %+v, want the foreign pointer noticed; a 304 must not answer for a channel that changed hands", res)
	}
	if !errors.Is(err, ErrForeignChannel) {
		t.Fatalf("run 2 err = %v, want ErrForeignChannel", err)
	}
	if got, _ := store.get(ManifestKey); !bytes.Equal(got, foreign) {
		t.Fatal("the operator's pointer was modified")
	}
	if len(seen) != 2 || seen[1] != "" {
		t.Fatalf("If-None-Match seen = %v, want run 2 to re-fetch the full document", seen)
	}
}

// TestMirrorNotModifiedCannotHideAWipedPrefix is the storage-migration case: not
// just the pointer but the whole agent-releases/ prefix is gone. Both objects
// must come back, in the load-bearing order.
func TestMirrorNotModifiedCannotHideAWipedPrefix(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()
	var seen []string
	m := newTestMirror(store, conditionalAPI(f, `W/"etag-1"`, &seen))

	if res, err := m.Run(context.Background()); err != nil || !res.Mirrored {
		t.Fatalf("run 1 = (%+v, %v), want the release published", res, err)
	}
	allowAnotherRequest(m)

	store.mu.Lock()
	for key := range store.objects {
		if strings.HasPrefix(key, packagePrefix) {
			delete(store.objects, key)
		}
	}
	store.putOrder = nil
	store.mu.Unlock()

	res, err := m.Run(context.Background())
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if !res.Mirrored {
		t.Fatalf("run 2 = %+v, want the whole prefix republished", res)
	}
	writes := store.writes()
	if len(writes) != 2 || writes[0] != packageObjectKey(testVersion) || writes[1] != ManifestKey {
		t.Fatalf("writes = %v, want the package then the pointer", writes)
	}
}

// TestMirrorSteadyStateAfterPublishStillUsesTheETag is the other side of the
// rule, and it is what stops the fix above from being paid for on every run.
//
// A publish CHANGES the local state, so the ETag has to be banked against the
// pointer this run just wrote, not the one it read on the way in. Bank the wrong
// one and every later run sees a mismatch, drops the ETag and pulls a full body
// forever, spending the 60-requests-per-hour budget to learn nothing.
func TestMirrorSteadyStateAfterPublishStillUsesTheETag(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()
	var seen []string
	m := newTestMirror(store, conditionalAPI(f, `W/"etag-1"`, &seen))

	if res, err := m.Run(context.Background()); err != nil || !res.Mirrored {
		t.Fatalf("run 1 = (%+v, %v), want the release published", res, err)
	}
	allowAnotherRequest(m)

	res, err := m.Run(context.Background())
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if res.Reason != "not_modified" {
		t.Fatalf("run 2 = %+v, want the cheap 304 path when nothing moved on either side", res)
	}
	if len(seen) != 2 || seen[1] != `W/"etag-1"` {
		t.Fatalf("If-None-Match seen = %v, want run 2 to replay the ETag banked by the publish", seen)
	}
}

// ---------------------------------------------------------------------------
// Degradation: unreachable, rate limited, unconfigured
// ---------------------------------------------------------------------------

// TestMirrorUnreachableUpstreamDegradesQuietly: GitHub being down (or blocked by
// egress rules, the self-hosted norm) must leave the mirror untouched and report
// a plain degradation, never a refusal and never a panic.
func TestMirrorUnreachableUpstreamDegradesQuietly(t *testing.T) {
	cases := []struct {
		name    string
		handler func(*http.Request) (*http.Response, error)
		wantErr error
	}{
		{"transport error", func(*http.Request) (*http.Response, error) {
			return nil, errors.New("dial tcp: connection refused")
		}, ErrUpstreamUnavailable},
		{"502 from GitHub", func(*http.Request) (*http.Response, error) {
			return statusResponse(http.StatusBadGateway, nil), nil
		}, ErrUpstreamUnavailable},
		{"404 unknown repo", func(*http.Request) (*http.Response, error) {
			return statusResponse(http.StatusNotFound, nil), nil
		}, ErrUpstreamUnavailable},
		{"429 rate limited", func(*http.Request) (*http.Response, error) {
			return statusResponse(http.StatusTooManyRequests, nil), nil
		}, ErrRateLimited},
		{"403 with budget exhausted", func(*http.Request) (*http.Response, error) {
			return statusResponse(http.StatusForbidden, hdr("X-RateLimit-Remaining", "0")), nil
		}, ErrRateLimited},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			store := newFakeStore()
			prev := manifestJSON("0.61.90", strings.Repeat("c", 64), 99, packageObjectKey("0.61.90"))
			store.objects[ManifestKey] = prev

			doer := wire(f)
			doer.handlers[apiURL(testOwner, testRepo)] = tc.handler
			m := newTestMirror(store, doer)

			_, err := m.Run(context.Background())
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Run err = %v, want %v", err, tc.wantErr)
			}
			if errors.Is(err, ErrRefused) {
				t.Fatal("an unreachable upstream must not be reported as a refusal")
			}
			if w := store.writes(); len(w) != 0 {
				t.Fatalf("wrote %v when upstream was unreachable", w)
			}
			if got, _ := store.get(ManifestKey); !bytes.Equal(got, prev) {
				t.Fatal("previous mirror was disturbed by an unreachable upstream")
			}
		})
	}
}

// TestMirrorMidDownloadFailureDegradesQuietly: the package download failing after
// a good API response is still just "no new release mirrored".
func TestMirrorMidDownloadFailureDegradesQuietly(t *testing.T) {
	f := newFixture(t)
	doer := wire(f)
	doer.handlers[downloadURL(testOwner, testRepo, testTag, packageAssetName)] = func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection reset by peer")
	}
	store := newFakeStore()
	m := newTestMirror(store, doer)

	if _, err := m.Run(context.Background()); !errors.Is(err, ErrUpstreamUnavailable) {
		t.Fatalf("Run err = %v, want ErrUpstreamUnavailable", err)
	}
	if w := store.writes(); len(w) != 0 {
		t.Fatalf("wrote %v after a failed download", w)
	}
}

// TestMirrorNotConfigured: no store, no client, or an owner/repo that is not a
// usable URL path segment. None of these may reach the network, and the last one
// is the guard that stops a hostile config value from redirecting the fetch.
func TestMirrorNotConfigured(t *testing.T) {
	f := newFixture(t)

	t.Run("nil store", func(t *testing.T) {
		m := NewMirror(nil, wire(f), testOwner, testRepo, nil)
		if _, err := m.Run(context.Background()); !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("Run err = %v, want ErrNotConfigured", err)
		}
	})
	t.Run("nil client", func(t *testing.T) {
		m := NewMirror(newFakeStore(), nil, testOwner, testRepo, nil)
		if _, err := m.Run(context.Background()); !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("Run err = %v, want ErrNotConfigured", err)
		}
	})

	bad := []struct{ name, owner, repo string }{
		{"empty owner", "", testRepo},
		{"empty repo", testOwner, ""},
		{"owner with a slash", "mosamlife/wpmgr", testRepo},
		{"repo with a slash", testOwner, "wpmgr/releases"},
		{"repo traverses", testOwner, ".."},
		{"owner is a host", "evil.example.com/x", testRepo},
		{"repo has a query", testOwner, "wpmgr?x=1"},
		{"repo is url-encoded", testOwner, "wpmgr%2F.."},
		{"owner has whitespace", "mosam life", testRepo},
		{"owner starts with a dash", "-mosamlife", testRepo},
		{"repo has a newline", testOwner, "wpmgr\nHost: evil"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			doer := wire(f)
			m := NewMirror(newFakeStore(), doer, tc.owner, tc.repo, nil)
			if _, err := m.Run(context.Background()); !errors.Is(err, ErrNotConfigured) {
				t.Fatalf("Run err = %v, want ErrNotConfigured", err)
			}
			if len(doer.urls()) != 0 {
				t.Fatalf("made requests %v with an unusable owner/repo", doer.urls())
			}
		})
	}
}

// TestMirrorRejectsUnusableTag: the tag is the ONE value from fetched JSON that
// reaches a URL, so it gets the same shape check as owner/repo. A tag with a
// slash would otherwise walk the download URL onto another path.
func TestMirrorRejectsUnusableTag(t *testing.T) {
	bad := []string{
		"",
		"../../evil",
		"v1/../../../etc/passwd",
		"v1.0 with spaces",
		"tag?x=1",
		"tag#frag",
		"tag%2f..",
		"https://evil.example.com/x",
		strings.Repeat("v", 200),
	}
	for _, tag := range bad {
		t.Run(tag, func(t *testing.T) {
			f := newFixture(t)
			f.api = f.apiDoc(tag, "sha256:"+f.pkgSHA, int64(len(f.pkg)))
			store := newFakeStore()
			doer := wire(f)
			m := newTestMirror(store, doer)

			if _, err := m.Run(context.Background()); !errors.Is(err, ErrRefused) {
				t.Fatalf("Run err = %v, want ErrRefused for tag %q", err, tag)
			}
			// Only the API request may have been made.
			if len(doer.urls()) != 1 {
				t.Fatalf("requested %v after an unusable tag; want only the API request", doer.urls())
			}
			if w := store.writes(); len(w) != 0 {
				t.Fatalf("wrote %v for an unusable tag", w)
			}
		})
	}
}

// TestMirrorRequiresBothAssets: a release missing either fixed asset is not one
// this channel can mirror. The manifest asset in particular is what carries
// min_version, which the API response cannot supply.
//
// Every case here is otherwise PERFECTLY valid: the listed zip carries the real
// digest and size, and the fake upstream still serves both download URLs. So the
// only thing that can refuse these runs is the asset-presence check itself, which
// is what makes this a real test of that check rather than of the cross-check
// further down.
func TestMirrorRequiresBothAssets(t *testing.T) {
	f0 := newFixture(t)
	zipAsset := fmt.Sprintf(`{"name":"wpmgr-agent.zip","size":%d,"digest":"sha256:%s"}`, len(f0.pkg), f0.pkgSHA)
	manAsset := `{"name":"agent-release.json","size":512,"digest":"sha256:` + strings.Repeat("a", 64) + `"}`

	cases := []struct{ name, assets string }{
		{"no assets at all", ``},
		{"only the zip", zipAsset},
		{"only the manifest", manAsset},
		{"similar but wrong names", strings.ReplaceAll(zipAsset, "wpmgr-agent.zip", "wpmgr-agent.zip.sig") + "," +
			strings.ReplaceAll(manAsset, "agent-release.json", "agent-release.json.bak")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.api = []byte(`{"tag_name":"` + testTag + `","assets":[` + tc.assets + `]}`)
			store := newFakeStore()
			m := newTestMirror(store, wire(f))

			if _, err := m.Run(context.Background()); !errors.Is(err, ErrRefused) {
				t.Fatalf("Run err = %v, want ErrRefused", err)
			}
			if w := store.writes(); len(w) != 0 {
				t.Fatalf("wrote %v for an incomplete release", w)
			}
		})
	}
}

// TestMirrorMinRequestSpacing: a second run immediately after the first spends no
// GitHub request. The job's own 6-hour cadence is nowhere near the 60/hour limit;
// this guard only stops a duplicate or manual trigger from burning budget for no
// new information.
func TestMirrorMinRequestSpacing(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()
	doer := wire(f)
	m := newTestMirror(store, doer)

	if _, err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	before := len(doer.urls())

	_, err := m.Run(context.Background())
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("Run 2 err = %v, want ErrRateLimited", err)
	}
	if got := len(doer.urls()); got != before {
		t.Fatalf("made %d requests on the too-soon run; want 0 more than %d", got-before, before)
	}
}

// ---------------------------------------------------------------------------
// Storage write failures
// ---------------------------------------------------------------------------

// TestMirrorPackageWriteFailureLeavesPointerUntouched: the whole reason the
// package is written FIRST. If it cannot be stored, latest.json is never
// repointed and the previous release stays served.
func TestMirrorPackageWriteFailureLeavesPointerUntouched(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()
	prev := mirroredManifestJSON("0.61.90", strings.Repeat("c", 64), 99, packageObjectKey("0.61.90"))
	store.objects[ManifestKey] = prev
	store.putErr[packageObjectKey(testVersion)] = errors.New("storage unavailable")

	m := newTestMirror(store, wire(f))
	if _, err := m.Run(context.Background()); err == nil {
		t.Fatal("Run err = nil, want a storage error")
	}
	if w := store.writes(); len(w) != 0 {
		t.Fatalf("recorded writes %v after a failed package write", w)
	}
	if got, _ := store.get(ManifestKey); !bytes.Equal(got, prev) {
		t.Fatal("latest.json was repointed even though the package write failed")
	}
}

// TestMirrorPointerWriteFailureLeavesPreviousPointer: the pointer write failing
// leaves the new package as an orphan object (harmless, overwritten next run)
// while the previous pointer still names a package that is still there.
func TestMirrorPointerWriteFailureLeavesPreviousPointer(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()
	prev := mirroredManifestJSON("0.61.90", strings.Repeat("c", 64), 99, packageObjectKey("0.61.90"))
	store.objects[ManifestKey] = prev
	store.putErr[ManifestKey] = errors.New("storage unavailable")

	m := newTestMirror(store, wire(f))
	if _, err := m.Run(context.Background()); err == nil {
		t.Fatal("Run err = nil, want a storage error")
	}
	if got, _ := store.get(ManifestKey); !bytes.Equal(got, prev) {
		t.Fatal("latest.json changed even though its write failed")
	}
	if _, ok := store.get(packageObjectKey(testVersion)); !ok {
		t.Fatal("the package should already be stored when the pointer write fails")
	}
}

// TestMirrorNoPointerYetPublishes is the case the whole feature exists for: a
// self-hosted install whose storage has never held an agent release. Nothing is
// there to protect, so the verified upstream release is published.
func TestMirrorNoPointerYetPublishes(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()

	m := newTestMirror(store, wire(f))
	res, err := m.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Mirrored {
		t.Fatalf("Run = %+v, want the release published into an empty channel", res)
	}
}

// TestMirrorUnreadableCurrentPointerWritesNothing: a storage read failure that is
// NOT "no pointer published" leaves this run unable to tell whether the pointer
// in place is its own or an operator's. Assuming its own is exactly how a release
// channel gets taken over on the strength of a blip, so the run writes nothing
// and defers to the next one. It is a degradation, not a refusal, and it cannot
// wedge: any successful read resumes normal service.
func TestMirrorUnreadableCurrentPointerWritesNothing(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()
	store.getErr = errors.New("storage blip")

	m := newTestMirror(store, wire(f))
	_, err := m.Run(context.Background())
	if !errors.Is(err, ErrPointerUnreadable) {
		t.Fatalf("Run err = %v, want ErrPointerUnreadable", err)
	}
	if errors.Is(err, ErrRefused) {
		t.Fatal("an unreadable pointer must not be reported as a refused upstream release")
	}
	if w := store.writes(); len(w) != 0 {
		t.Fatalf("wrote %v without being able to read the pointer it would replace", w)
	}
}

// ---------------------------------------------------------------------------
// Publish decision: direction and provenance
// ---------------------------------------------------------------------------

// TestMirrorRefusesAnOlderUpstream is the downgrade guard.
//
// Two real inputs produce it. A YANK: upstream deletes its newest release and
// /releases/latest starts answering with an older one. And the case that matters
// more, an operator who publishes their OWN build to this same key at a higher
// version. Repointing backwards bricks nothing (the agent refuses to install
// something older than it is running) but it makes the dashboard LIE: a site
// ahead of the reference classifies as "current" (agentrelease.Classify), so
// every card reads as up to date on a version no site is running, and a newly
// enrolled site is offered the wrong build.
func TestMirrorRefusesAnOlderUpstream(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()
	// Already published here: a NEWER build than upstream's.
	prev := mirroredManifestJSON("9.9.9", strings.Repeat("c", 64), 4242, packageObjectKey("9.9.9"))
	store.objects[ManifestKey] = prev

	doer := wire(f)
	m := newTestMirror(store, doer)

	res, err := m.Run(context.Background())
	if !errors.Is(err, ErrDowngrade) {
		t.Fatalf("Run err = %v, want ErrDowngrade", err)
	}
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("Run err = %v, want it to also read as a refusal", err)
	}
	if res.Mirrored {
		t.Fatal("Run reported a mirrored release while walking the version backwards")
	}
	if w := store.writes(); len(w) != 0 {
		t.Fatalf("wrote %v while walking the published version backwards", w)
	}
	if got, _ := store.get(ManifestKey); !bytes.Equal(got, prev) {
		t.Fatal("the newer published pointer was replaced by an older upstream release")
	}
	// Refused before spending a multi-MB download.
	for _, u := range doer.urls() {
		if strings.HasSuffix(u, packageAssetName) {
			t.Fatalf("downloaded the package for a release it was never going to publish: %s", u)
		}
	}
}

// TestMirrorRollbackEscapeHatchPublishesAnOlderUpstream: the strictly-newer rule
// must not be unrecoverable. When a release really is yanked and the operator
// wants their install to follow it back, one explicit switch does it.
func TestMirrorRollbackEscapeHatchPublishesAnOlderUpstream(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()
	store.objects[ManifestKey] = mirroredManifestJSON("9.9.9", strings.Repeat("c", 64), 4242, packageObjectKey("9.9.9"))

	m := NewMirrorWithRollback(store, wire(f), testOwner, testRepo, true, nil)
	res, err := m.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Mirrored || res.Version != testVersion {
		t.Fatalf("Run = %+v, want the older upstream release published at %s", res, testVersion)
	}
	writes := store.writes()
	if len(writes) != 2 || writes[1] != ManifestKey {
		t.Fatalf("writes = %v, want package then pointer", writes)
	}
}

// TestMirrorRefusesAnUpstreamItCannotOrder pins the rule that UNORDERABLE IS A
// REFUSAL, and names where each shape is caught.
//
// The guard exists to stop the channel moving backwards, and it used to be
// bypassed by any version string the comparator could not parse: "publish
// whenever either side is not well-formed" meant the ordering check was skipped
// entirely rather than failed. Every string below was mirrored over 0.61.99 with
// the rollback hatch OFF.
//
// The four are refused (or not) at three different points, which is the part
// worth pinning:
//
//   - "nightly" never reaches the ordering rule at all. It carries no digit, so
//     it is not a usable object-key segment and validateManifest refuses it
//     first. The hatch does not relax that, and should not: the hatch is about
//     DIRECTION, not about what may become an object key.
//   - "0" is a legal key segment but not an orderable version (one numeric
//     segment, no dot), so it is the ordering rule that refuses it.
//   - "v0.61.98" is an ordinary release tag with our own leading "v". It is
//     normalised once, in orderableVersion, and then compares as what it is: an
//     OLDER release. So it is refused as a downgrade, precisely, instead of
//     silently downgrading the channel because nothing could parse it.
//   - "2026.07.30-hotfix" is not a bypass and is not refused. It is well-formed,
//     and 2026.07.30 really is strictly newer than 0.61.99, so the strictly-newer
//     rule publishes it. Refusing it would contradict the rule rather than
//     enforce it.
func TestMirrorRefusesAnUpstreamItCannotOrder(t *testing.T) {
	cases := []struct {
		version string
		// strict is the sentinel expected with the hatch OFF; nil means the
		// release is published.
		strict error
		// hatch is the sentinel expected with the hatch ON; nil means published.
		hatch error
	}{
		{version: "nightly", strict: ErrRefused, hatch: ErrRefused},
		{version: "0", strict: ErrUnorderable, hatch: nil},
		{version: "v0.61.98", strict: ErrDowngrade, hatch: nil},
		{version: "2026.07.30-hotfix", strict: nil, hatch: nil},
	}
	for _, tc := range cases {
		for _, allowRollback := range []bool{false, true} {
			want := tc.strict
			name := tc.version + "/strict"
			if allowRollback {
				want = tc.hatch
				name = tc.version + "/rollback allowed"
			}
			t.Run(name, func(t *testing.T) {
				f := newFixture(t)
				f.setManifest(manifestJSON(tc.version, f.pkgSHA, int64(len(f.pkg)), packageObjectKey(tc.version)))

				store := newFakeStore()
				mirrored := mirroredManifestJSON(testVersion, strings.Repeat("c", 64), 4242, packageObjectKey(testVersion))
				store.objects[ManifestKey] = mirrored

				m := NewMirrorWithRollback(store, wire(f), testOwner, testRepo, allowRollback, nil)
				res, err := m.Run(context.Background())

				if want == nil {
					if err != nil || !res.Mirrored {
						t.Fatalf("Run = (%+v, %v), want %q published", res, err, tc.version)
					}
					return
				}
				if !errors.Is(err, want) {
					t.Fatalf("Run err = %v, want %v", err, want)
				}
				// Every refusal here is also a refusal in the general sense, so
				// the worker logs it as one and the previous mirror stands.
				if !errors.Is(err, ErrRefused) {
					t.Fatalf("Run err = %v, want it to also read as ErrRefused", err)
				}
				if res.Mirrored {
					t.Fatalf("Run = %+v, reported a mirrored release it refused", res)
				}
				if w := store.writes(); len(w) != 0 {
					t.Fatalf("wrote %v for a release it refused", w)
				}
				if got, _ := store.get(ManifestKey); !bytes.Equal(got, mirrored) {
					t.Fatalf("the published pointer was replaced by %q", tc.version)
				}
			})
		}
	}
}

// TestMirrorRefusesWhenTheMirroredVersionCannotBeOrdered covers the side the old
// rule was really written for: a pointer already in place at a version nothing
// can order, e.g. one written before any of these rules existed. Upstream is
// perfectly ordinary; it is the LOCAL half that cannot be compared.
//
// Refusing was previously rejected as a design because it looked like it would
// wedge the channel on a value nothing could ever make comparable. It does not,
// and this pins both ways out, because a refusal with no remedy is the thing
// that would actually be unsafe:
//
//   - remove the pointer, and the next run publishes into an empty channel;
//   - or set the rollback hatch, and it publishes without an ordering proof.
//
// Removing the pointer is only a remedy if the mirror can SEE that it is gone,
// which is the property TestMirrorNotModifiedCannotHideADeletedPointer pins. It
// holds here for a second reason as well: a refused run banks no ETag, so there
// is no 304 available to short-circuit the next one either way.
func TestMirrorRefusesWhenTheMirroredVersionCannotBeOrdered(t *testing.T) {
	// stage builds an install whose own pointer names an unorderable version.
	stage := func() (*fixture, *fakeStore) {
		f := newFixture(t)
		store := newFakeStore()
		store.objects[ManifestKey] = mirroredManifestJSON("nightly", strings.Repeat("c", 64), 4242, packageObjectKey("nightly"))
		return f, store
	}

	t.Run("strict refuses", func(t *testing.T) {
		f, store := stage()
		m := newTestMirror(store, wire(f))
		res, err := m.Run(context.Background())
		if !errors.Is(err, ErrUnorderable) {
			t.Fatalf("Run err = %v, want ErrUnorderable", err)
		}
		if res.Mirrored {
			t.Fatalf("Run = %+v, want no publish without an ordering proof", res)
		}
		if w := store.writes(); len(w) != 0 {
			t.Fatalf("wrote %v without being able to order the two versions", w)
		}
	})

	t.Run("remedy: remove the pointer", func(t *testing.T) {
		f, store := stage()
		m := newTestMirror(store, wire(f))
		if _, err := m.Run(context.Background()); !errors.Is(err, ErrUnorderable) {
			t.Fatalf("Run 1 err = %v, want ErrUnorderable", err)
		}
		allowAnotherRequest(m)

		store.mu.Lock()
		delete(store.objects, ManifestKey)
		store.mu.Unlock()

		res, err := m.Run(context.Background())
		if err != nil || !res.Mirrored {
			t.Fatalf("Run 2 = (%+v, %v), want the release published into the emptied channel", res, err)
		}
	})

	t.Run("remedy: the rollback hatch", func(t *testing.T) {
		f, store := stage()
		m := NewMirrorWithRollback(store, wire(f), testOwner, testRepo, true, nil)
		res, err := m.Run(context.Background())
		if err != nil || !res.Mirrored {
			t.Fatalf("Run = (%+v, %v), want the release published under the hatch", res, err)
		}
	})
}

// TestMirrorNeverOverwritesAnOperatorPublishedPointer is the protection that
// actually covers channel takeover, and it is NOT the same as refusing a
// downgrade: here the operator's build is OLDER than upstream, so strictly-newer
// would happily replace it.
//
// scripts/release-agent.sh (make agent-release) writes this exact key. An
// operator who builds their own agent owns agent-releases/latest.json, and a
// mirror that overwrote it would undo every release they publish, six hours
// later, forever, with nothing obvious to look at. So the mirror stands down.
func TestMirrorNeverOverwritesAnOperatorPublishedPointer(t *testing.T) {
	// The escape hatch is about DIRECTION, not ownership: neither setting may
	// let the mirror take over a channel it does not publish.
	for _, allowRollback := range []bool{false, true} {
		name := "strict"
		if allowRollback {
			name = "rollback allowed"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			store := newFakeStore()
			// An operator-published pointer: no provenance stamp, and OLDER than
			// upstream, so nothing else in the chain would refuse it.
			operatorPointer := manifestJSON("0.61.90", strings.Repeat("c", 64), 4242, packageObjectKey("0.61.90"))
			store.objects[ManifestKey] = operatorPointer

			doer := wire(f)
			m := NewMirrorWithRollback(store, doer, testOwner, testRepo, allowRollback, nil)

			res, err := m.Run(context.Background())
			if !errors.Is(err, ErrForeignChannel) {
				t.Fatalf("Run err = %v, want ErrForeignChannel", err)
			}
			if res.Mirrored {
				t.Fatal("Run reported a mirrored release over an operator-published pointer")
			}
			if w := store.writes(); len(w) != 0 {
				t.Fatalf("wrote %v over an operator-published channel", w)
			}
			if got, _ := store.get(ManifestKey); !bytes.Equal(got, operatorPointer) {
				t.Fatal("the operator's latest.json was modified")
			}
			for _, u := range doer.urls() {
				if strings.HasSuffix(u, packageAssetName) {
					t.Fatalf("downloaded the package for a channel it may not publish to: %s", u)
				}
			}
		})
	}
}

// TestMirrorPublishesOverItsOwnOlderPointer is the ordinary upgrade, stated in
// terms of the two rules: the pointer in place carries this mirror's stamp, and
// upstream is strictly newer, so it publishes and re-stamps.
func TestMirrorPublishesOverItsOwnOlderPointer(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()
	store.objects[ManifestKey] = mirroredManifestJSON("0.61.90", strings.Repeat("c", 64), 4242, packageObjectKey("0.61.90"))

	m := newTestMirror(store, wire(f))
	res, err := m.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Mirrored {
		t.Fatalf("Run = %+v, want the newer release published over the mirror's own pointer", res)
	}
	got, _ := store.get(ManifestKey)
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("published pointer is not valid JSON: %v", err)
	}
	if decoded["version"] != testVersion {
		t.Fatalf("published version = %v, want %s", decoded["version"], testVersion)
	}
	if decoded[provenanceField] != provenanceMarker {
		t.Fatal("the replacement pointer lost its provenance stamp; the next run would stand down against itself")
	}
}

// TestMirrorAlreadyCurrentWhoeverPublishedIt: a pointer that already names
// exactly this release, bytes and all, is a no-op rather than a refusal. Nothing
// is being replaced, so there is nothing to protect, and reporting a stand-down
// for a channel that is already in the right state would be noise.
func TestMirrorAlreadyCurrentWhoeverPublishedIt(t *testing.T) {
	f := newFixture(t)
	store := newFakeStore()
	// Deliberately UNSTAMPED: the same release, published by someone else.
	store.objects[ManifestKey] = f.manifest
	store.objects[packageObjectKey(testVersion)] = f.pkg

	m := newTestMirror(store, wire(f))
	res, err := m.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reason != "already_current" || res.Mirrored {
		t.Fatalf("Run = %+v, want an already_current no-op", res)
	}
	if w := store.writes(); len(w) != 0 {
		t.Fatalf("wrote %v when the channel already named this exact release", w)
	}
}

// ---------------------------------------------------------------------------
// Unit-level helpers
// ---------------------------------------------------------------------------

// TestPackageObjectKeyMatchesTheAgentFacingForm pins this package's derived key
// to the one internal/agent/update_handler.go pins latest.json to. If these two
// ever drift, every mirrored release is rejected by the mint path.
func TestPackageObjectKeyMatchesTheAgentFacingForm(t *testing.T) {
	if got, want := packageObjectKey("0.61.99"), "agent-releases/0.61.99/wpmgr-agent.zip"; got != want {
		t.Fatalf("packageObjectKey = %q, want %q", got, want)
	}
	if ManifestKey != "agent-releases/latest.json" {
		t.Fatalf("ManifestKey = %q, want agent-releases/latest.json", ManifestKey)
	}
	if packageAssetName != "wpmgr-agent.zip" {
		t.Fatalf("packageAssetName = %q, want wpmgr-agent.zip", packageAssetName)
	}
}

// TestIsPathSegment covers the shape check applied to owner, repo and tag.
func TestIsPathSegment(t *testing.T) {
	valid := []string{"wpmgr", "mosamlife", "v0.61.102", "0.61.102", "my-fork_1", "a", "v1.0.0+build.5"}
	for _, v := range valid {
		if !isPathSegment(v) {
			t.Errorf("isPathSegment(%q) = false, want true", v)
		}
	}
	invalid := []string{"", "..", "a/b", "a\\b", "a b", "a?b", "a#b", "a%2f", "-leading", ".leading", "a\nb", strings.Repeat("a", 101)}
	for _, v := range invalid {
		if isPathSegment(v) {
			t.Errorf("isPathSegment(%q) = true, want false", v)
		}
	}
}

// TestSha256FromDigest covers GitHub's per-asset digest parsing.
func TestSha256FromDigest(t *testing.T) {
	good := strings.Repeat("ab", 32)
	got, err := sha256FromDigest("sha256:" + good)
	if err != nil || got != good {
		t.Fatalf("sha256FromDigest = (%q, %v), want (%q, nil)", got, err, good)
	}
	for _, bad := range []string{"", "   ", good, "sha256:", "sha256:zz", "md5:" + good, "sha256:" + strings.ToUpper(good)} {
		if _, err := sha256FromDigest(bad); err == nil {
			t.Errorf("sha256FromDigest(%q) = nil error, want an error", bad)
		}
	}
}
