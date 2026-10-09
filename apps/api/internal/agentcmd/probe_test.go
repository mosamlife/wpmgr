package agentcmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

// TestProbeGet_FatalSignatureStillDetectedAlongsideBuster proves WordPress's
// error screen is still recognised alongside the cache-buster/cache-hit
// additions.
func TestProbeGet_FatalSignatureStillDetectedAlongsideBuster(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(wpErrorScreenHTML))
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

// wpErrorScreenHTML is the document WordPress's default wp_die() handler
// renders for its critical-error screen.
const wpErrorScreenHTML = `<!DOCTYPE html>
<html lang="en-US">
<head>
	<meta http-equiv="Content-Type" content="text/html; charset=utf-8" />
	<meta name="viewport" content="width=device-width, initial-scale=1.0">
	<meta name='robots' content='noindex, follow' />
	<title>WordPress &rsaquo; Error</title>
	<style type="text/css">
		html { background: #f1f1f1; }
		body { background: #fff; border: 1px solid #ccd0d4; color: #444; margin: 2em auto; padding: 1em 2em; max-width: 700px; }
	</style>
</head>
<body id="error-page">
	<div class="wp-die-message"><p>There has been a critical error on this website.</p><p><a href="https://wordpress.org/documentation/article/faq-troubleshooting/">Learn more about troubleshooting WordPress.</a></p></div></body>
</html>
`

// wpMaintenanceScreenHTML is the same wp_die() document carrying WordPress's
// maintenance-mode message, which WordPress serves with HTTP 503.
const wpMaintenanceScreenHTML = `<!DOCTYPE html>
<html lang="en-US">
<head>
	<meta http-equiv="Content-Type" content="text/html; charset=utf-8" />
	<title>Maintenance</title>
</head>
<body id="error-page">
	<div class="wp-die-message">Briefly unavailable for scheduled maintenance. Check back in a minute.</div></body>
</html>
`

// wpRefusalScreenHTML is the same wp_die() document carrying a refusal a site
// sends on purpose, with its own 4xx status.
const wpRefusalScreenHTML = `<!DOCTYPE html>
<html lang="en-US">
<head>
	<meta http-equiv="Content-Type" content="text/html; charset=utf-8" />
	<title>Members only</title>
</head>
<body id="error-page">
	<div class="wp-die-message">This site is only available to signed-in members.</div></body>
</html>
`

// healthyHomepageHTML is a fully rendered, healthy homepage.
const healthyHomepageHTML = `<!DOCTYPE html>
<html lang="en-US">
<head>
<meta charset="UTF-8" />
<title>Northwind Studio &#8211; Notes from a small web studio</title>
<link rel="stylesheet" href="/wp-content/themes/northwind/style.css" />
</head>
<body class="home blog wp-embed-responsive">
<header class="site-header"><p class="site-title"><a href="/">Northwind Studio</a></p></header>
<main id="main" class="site-main">
<article class="post-41 post type-post status-publish format-standard hentry">
<h2 class="entry-title"><a href="/2026/09/memory-limits/">How we fixed &#8220;Fatal error: Allowed memory size exhausted&#8221; on a client shop</a></h2>
<div class="entry-summary"><p>The checkout page stopped loading after a plugin update. Here is what the log said and how we raised the limit.</p></div>
</article>
<article class="post-38 post type-post status-publish format-standard hentry">
<h2 class="entry-title"><a href="/2026/08/reading-stack-traces/">Reading a PHP stack trace</a></h2>
<div class="entry-summary"><pre><code>PHP Fatal error:  Uncaught Error: Call to undefined function northwind_menu()</code></pre><p>Every trace starts with the line that failed.</p></div>
</article>
</main>
<footer class="site-footer"><p>&copy; 2026 Northwind Studio</p></footer>
</body>
</html>
`

// probeServed serves body with the given status and Content-Type (plus any
// extra headers) at the site root, and returns the probe's result for it.
func probeServed(t *testing.T, status int, contentType, body string, headers map[string]string) ProbeResult {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	res, err := buildTestProbe(t).Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return res
}

const htmlType = "text/html; charset=UTF-8"

// TestProbeGet_HealthyPageIsNotACrash: a healthy page is never treated as a
// crash.
func TestProbeGet_HealthyPageIsNotACrash(t *testing.T) {
	res := probeServed(t, http.StatusOK, htmlType, healthyHomepageHTML, nil)
	if res.Fatal || !res.Healthy() {
		t.Fatalf("a healthy page must read as healthy, got %+v", res)
	}
}

// TestProbeGet_ErrorScreenIsACrash: WordPress's own error screen, recognised
// by its structure, is a crash when the response otherwise reports success,
// wherever it sits in the body the probe reads, including after more than
// 64 KB of page output.
func TestProbeGet_ErrorScreenIsACrash(t *testing.T) {
	pageOutput := `<!DOCTYPE html><html lang="en-US"><head><title>Northwind Studio</title></head><body class="home">` +
		strings.Repeat(`<div class="wp-block-group"><p>Studio news, case studies and notes.</p></div>`+"\n", 1200)
	if len(pageOutput) <= 64<<10 {
		t.Fatalf("page output is %d bytes, want more than 64 KB", len(pageOutput))
	}
	cases := []struct {
		name string
		body string
	}{
		{"error screen alone", wpErrorScreenHTML},
		{"error screen after page output", pageOutput + wpErrorScreenHTML},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := probeServed(t, http.StatusOK, htmlType, tc.body, nil)
			if !res.Fatal || res.Healthy() {
				t.Fatalf("WordPress's error screen must read as a crash, got %+v", res)
			}
		})
	}
}

// TestProbeGet_ServerErrorIsReportedByStatus: a server error status is
// reported as that status and left to the caller's status rule, whatever the
// body says.
func TestProbeGet_ServerErrorIsReportedByStatus(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		headers map[string]string
	}{
		{"empty 500", http.StatusInternalServerError, "", nil},
		{"500 with the error screen", http.StatusInternalServerError, wpErrorScreenHTML, nil},
		{"503 maintenance screen", http.StatusServiceUnavailable, wpMaintenanceScreenHTML, map[string]string{"Retry-After": "600"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := probeServed(t, tc.status, htmlType, tc.body, tc.headers)
			if res.StatusCode != tc.status {
				t.Fatalf("StatusCode = %d, want %d", res.StatusCode, tc.status)
			}
			if res.Fatal {
				t.Fatalf("a server error status is decided by its status, got Fatal=true: %+v", res)
			}
			if res.Healthy() {
				t.Fatalf("Healthy() must be false for a server error: %+v", res)
			}
		})
	}
}

// TestProbeGet_RefusalIsNotACrash: a page a site refuses on purpose, with its
// own 4xx status, is not a crash, even when WordPress rendered it.
func TestProbeGet_RefusalIsNotACrash(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		res := probeServed(t, status, htmlType, wpRefusalScreenHTML, nil)
		if res.Fatal || !res.Healthy() {
			t.Fatalf("status %d: a deliberate refusal must not read as a crash, got %+v", status, res)
		}
	}
}

// TestProbeGet_NonHTMLIsNotScanned: only an HTML response can be WordPress's
// error screen.
func TestProbeGet_NonHTMLIsNotScanned(t *testing.T) {
	res := probeServed(t, http.StatusOK, "text/plain; charset=UTF-8", wpErrorScreenHTML, nil)
	if res.Fatal || !res.Healthy() {
		t.Fatalf("a non-HTML response must not read as a crash, got %+v", res)
	}
}
