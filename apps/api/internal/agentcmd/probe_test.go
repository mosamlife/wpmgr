package agentcmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
)

// buildTestProbe builds a *Probe backed by an SSRF-disabled httpclient that
// can reach loopback httptest servers (test-only).
func buildTestProbe(t *testing.T) *Probe {
	t.Helper()
	hc := httpclient.New(httpclient.Config{AllowPrivateNetworks: true})
	return NewProbe(hc)
}

// TestProbeGet_AppendsCacheBusterQueryParam proves GH #291 Phase 4 Change 3:
// every probe request carries the wpmgr_hc cache-busting query parameter, and
// a fresh value on every call so a query-string-keyed cache treats it as a
// new object each time.
func TestProbeGet_AppendsCacheBusterQueryParam(t *testing.T) {
	var seenQueries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenQueries = append(seenQueries, r.URL.RawQuery)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := buildTestProbe(t)
	for i := 0; i < 2; i++ {
		if _, err := p.Get(context.Background(), srv.URL); err != nil {
			t.Fatalf("Get: %v", err)
		}
	}

	if len(seenQueries) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(seenQueries))
	}
	for i, q := range seenQueries {
		if q == "" {
			t.Fatalf("request %d: expected a %s query parameter, got no query string at all", i, cacheBusterParam)
		}
		values, err := url.ParseQuery(q)
		if err != nil {
			t.Fatalf("request %d: parse query %q: %v", i, q, err)
		}
		if values.Get(cacheBusterParam) == "" {
			t.Fatalf("request %d: expected %s in query %q", i, cacheBusterParam, q)
		}
	}
	if seenQueries[0] == seenQueries[1] {
		t.Fatalf("expected a fresh cache-buster value per request, got the same query twice: %q", seenQueries[0])
	}
}

// TestProbeGet_DetectsCacheHitViaCfCacheStatus proves a Cloudflare
// cf-cache-status: HIT response is flagged CacheHit even though it is a plain
// 200, so the caller does not mistake it for proof of a fresh render.
func TestProbeGet_DetectsCacheHitViaCfCacheStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cf-Cache-Status", "HIT")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>ok</html>"))
	}))
	defer srv.Close()

	p := buildTestProbe(t)
	res, err := p.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !res.CacheHit {
		t.Fatalf("expected CacheHit=true for cf-cache-status: HIT, got %+v", res)
	}
	if !res.Healthy() {
		t.Fatalf("Healthy() must remain unchanged (still true for a cache-hit 200): %+v", res)
	}
}

// TestProbeGet_DetectsCacheHitViaXCacheStatusHeader proves the standard
// nginx `add_header X-Cache-Status $upstream_cache_status` form is
// recognized (fix 4's header widening), not just the vendor-specific headers
// already covered above.
func TestProbeGet_DetectsCacheHitViaXCacheStatusHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Cache-Status", "HIT")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := buildTestProbe(t)
	res, err := p.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !res.CacheHit {
		t.Fatalf("expected CacheHit=true for X-Cache-Status: HIT, got %+v", res)
	}
}

// TestProbeGet_DetectsCacheHitViaAgeHeader proves the Age > 0 backstop: a
// cache that does not set any of the named vendor headers but does report its
// own Age is still flagged CacheHit.
func TestProbeGet_DetectsCacheHitViaAgeHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Age", "42")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := buildTestProbe(t)
	res, err := p.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !res.CacheHit {
		t.Fatalf("expected CacheHit=true for Age: 42, got %+v", res)
	}
}

// TestProbeGet_NoCacheHeaders_NotFlagged proves a plain, uncached response is
// NOT flagged CacheHit (no false positives on a normal fresh render).
func TestProbeGet_NoCacheHeaders_NotFlagged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>ok</html>"))
	}))
	defer srv.Close()

	p := buildTestProbe(t)
	res, err := p.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if res.CacheHit {
		t.Fatalf("expected CacheHit=false for a response with no cache headers, got %+v", res)
	}
}

// TestDetectCacheHit_KnownVendorHeader proves the explicit cacheHitHeaders
// list still works after generalizing DetectCacheHit (GH #291 Phase 2 change
// 3): a named vendor header (LiteSpeed) with a HIT value is still detected.
func TestDetectCacheHit_KnownVendorHeader(t *testing.T) {
	h := http.Header{}
	h.Set("X-Litespeed-Cache", "hit")
	hit, detail := DetectCacheHit(h)
	if !hit {
		t.Fatalf("expected CacheHit=true for a known vendor header, got hit=%v detail=%q", hit, detail)
	}
}

// TestDetectCacheHit_UnknownVendorShapeHeader proves the GH #291 Phase 2
// fix: a cache-status header NOT in cacheHitHeaders, but matching the common
// x-<something>-cache shape with a HIT-like value, is still detected. This is
// the exact gap the reporter's fleet fell into - a real stack's cache-status
// header name matched neither vendor list, so the OLD detectCacheHit would
// have silently missed the HIT on exactly the setup that produced the bug.
func TestDetectCacheHit_UnknownVendorShapeHeader(t *testing.T) {
	cases := []struct {
		name, header, value string
	}{
		{"x-<something>-cache shape", "X-Swarm-Cache", "HIT"},
		{"x-cache-<something> shape", "X-Cache-Hits", "STALE"},
		{"updating value", "X-Rocket-Cache", "UPDATING"},
		{"revalidated value", "X-Edge-Cache", "REVALIDATED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			h.Set(tc.header, tc.value)
			hit, detail := DetectCacheHit(h)
			if !hit {
				t.Fatalf("expected CacheHit=true for %s: %s, got hit=%v detail=%q", tc.header, tc.value, hit, detail)
			}
		})
	}
}

// TestDetectCacheHit_ShapeRegexDoesNotFalsePositive proves the shape fallback
// is not trigger-happy: an unrelated header (even one that merely contains
// "cache" somewhere) and a shape-matching header with a MISS/BYPASS value
// (the cache was NOT the source of the response - proof the bypass worked)
// are both left undetected.
func TestDetectCacheHit_ShapeRegexDoesNotFalsePositive(t *testing.T) {
	cases := []struct {
		name, header, value string
	}{
		{"unrelated header merely containing cache", "X-My-Cacheable-Widget", "HIT"},
		{"shape match but MISS value", "X-Swarm-Cache", "MISS"},
		{"shape match but BYPASS value", "X-Cache-Status", "BYPASS"},
		{"shape match but DYNAMIC value", "X-Edge-Cache", "DYNAMIC"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			h.Set(tc.header, tc.value)
			hit, detail := DetectCacheHit(h)
			if hit {
				t.Fatalf("expected CacheHit=false for %s: %s, got hit=%v detail=%q", tc.header, tc.value, hit, detail)
			}
		})
	}
}

// TestProbeGet_FatalSignatureStillDetectedAlongsideBuster proves the existing
// fatal-error body scan is unaffected by the cache-buster/cache-hit additions.
func TestProbeGet_FatalSignatureStillDetectedAlongsideBuster(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>Fatal error: something broke</html>"))
	}))
	defer srv.Close()

	p := buildTestProbe(t)
	res, err := p.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !res.Fatal {
		t.Fatalf("expected Fatal=true for a fatal-error body signature, got %+v", res)
	}
	if res.Healthy() {
		t.Fatalf("Healthy() must be false for a fatal response: %+v", res)
	}
}
