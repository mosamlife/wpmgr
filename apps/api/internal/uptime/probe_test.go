package uptime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
)

// testClient builds an SSRF client that ALLOWS loopback (so an httptest server
// is reachable) on any port. This mirrors the update integration tests' use of
// the AllowPrivateNetworks escape hatch.
func testClient() *httpclient.Client {
	return httpclient.New(httpclient.Config{
		Timeout:              5 * time.Second,
		AllowPrivateNetworks: true,
	})
}

// guardedClient builds the production-posture SSRF client (guard ON) so a
// loopback target is BLOCKED.
func guardedClient() *httpclient.Client {
	return httpclient.New(httpclient.Config{Timeout: 5 * time.Second})
}

func TestProbeUp200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	res := NewProber(testClient(), 5*time.Second).Probe(context.Background(), srv.URL)
	if !res.Up {
		t.Fatalf("expected up, got %+v", res)
	}
	if res.HTTPStatus != 200 {
		t.Fatalf("expected 200, got %d", res.HTTPStatus)
	}
	if res.TotalMs <= 0 || res.ConnectMs <= 0 {
		t.Fatalf("expected populated timings, got total=%v connect=%v", res.TotalMs, res.ConnectMs)
	}
}

func TestProbeDown500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	res := NewProber(testClient(), 5*time.Second).Probe(context.Background(), srv.URL)
	if res.Up {
		t.Fatalf("expected down on 500, got %+v", res)
	}
	if res.HTTPStatus != 500 {
		t.Fatalf("expected 500, got %d", res.HTTPStatus)
	}
	if res.Error == "" {
		t.Fatalf("expected an error string for a down probe")
	}
}

// TestProbeTLSExpiry verifies the leaf certificate expiry is parsed for an HTTPS
// test server and the TLS handshake timing is populated.
func TestProbeTLSExpiry(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// The httptest TLS server uses a self-signed cert; allow it (test-only).
	tlsClient := httpclient.New(httpclient.Config{
		Timeout:               5 * time.Second,
		AllowPrivateNetworks:  true,
		InsecureSkipTLSVerify: true,
	})
	res := NewProber(tlsClient, 5*time.Second).Probe(context.Background(), srv.URL)
	if !res.Up {
		t.Fatalf("expected up over TLS, got %+v", res)
	}
	if res.TLSExpiry.IsZero() {
		t.Fatalf("expected a non-zero TLS expiry from the test cert")
	}
	// httptest's cert is valid into the future; assert it parsed something sane.
	if !res.TLSExpiry.After(time.Now()) {
		t.Fatalf("expected TLS expiry in the future, got %v", res.TLSExpiry)
	}
	if res.TLSMs <= 0 {
		t.Fatalf("expected a TLS handshake timing, got %v", res.TLSMs)
	}
}

// TestProbeSSRFBlocked asserts the production-posture SSRF guard refuses a
// loopback (127.0.0.1) target: the probe records DOWN with an ssrf_blocked
// error and never connects.
func TestProbeSSRFBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// The httptest server binds 127.0.0.1; the guarded client must block it. Sanity
	// check the host is loopback so the test asserts what it claims.
	u, _ := url.Parse(srv.URL)
	if ap, err := netip.ParseAddrPort(u.Host); err == nil && !ap.Addr().IsLoopback() {
		t.Skipf("test server not on loopback (%s); SSRF assertion not meaningful", u.Host)
	}

	res := NewProber(guardedClient(), 5*time.Second).Probe(context.Background(), srv.URL)
	if res.Up {
		t.Fatalf("expected SSRF-blocked loopback probe to be DOWN, got %+v", res)
	}
	if res.HTTPStatus != 0 {
		t.Fatalf("expected no HTTP status on a blocked probe, got %d", res.HTTPStatus)
	}
	if !contains(res.Error, "ssrf_blocked") {
		t.Fatalf("expected ssrf_blocked error, got %q", res.Error)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// --- issue #132: wp-fatal-page detection ------------------------------------

// wpDieCriticalErrorHTML is a realistic rendering of WordPress core's
// _default_wp_die_handler() output for the "critical error" screen: both
// structural markers (id="error-page", class="wp-die-message") and the
// English phrase co-occur, well within the 16 KB scan window.
const wpDieCriticalErrorHTML = `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"><title>WordPress &rsaquo; Error</title>
<style type="text/css">body{background:#f1f1f1;}</style>
</head>
<body id="error-page">
<div class="wp-die-message">There has been a critical error on this website.</div>
<p><a href="https://wordpress.org/support/article/faq-troubleshooting/">Learn more about troubleshooting WordPress.</a></p>
</body>
</html>`

// wpDbErrorHTML is the classic "Error establishing a database connection"
// screen (wpdb::bail()), which WordPress renders before translations are
// loaded, so it carries no wp_die structural markers.
const wpDbErrorHTML = `<!DOCTYPE html>
<html>
<head><title>Database Error</title></head>
<body>
<h1>Error establishing a database connection</h1>
</body>
</html>`

func htmlServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestProbeWpFatalErrorDetected asserts a real wp_die critical-error page
// (structural marker + phrase, HTTP 200) is reclassified DOWN with reason
// "wp_fatal_error".
func TestProbeWpFatalErrorDetected(t *testing.T) {
	srv := htmlServer(t, http.StatusOK, wpDieCriticalErrorHTML)

	res := NewProber(testClient(), 5*time.Second).Probe(context.Background(), srv.URL)
	if res.Up {
		t.Fatalf("expected a wp_die critical-error page to be classified DOWN, got %+v", res)
	}
	if res.HTTPStatus != http.StatusOK {
		t.Fatalf("expected the underlying HTTP status to still be 200, got %d", res.HTTPStatus)
	}
	if res.Error != "wp_fatal_error" {
		t.Fatalf("expected Error=wp_fatal_error, got %q", res.Error)
	}
}

// TestProbeWpDbErrorDetected asserts the "Error establishing a database
// connection" screen (HTTP 200, no wp_die markers) is reclassified DOWN with
// reason "wp_db_error".
func TestProbeWpDbErrorDetected(t *testing.T) {
	srv := htmlServer(t, http.StatusOK, wpDbErrorHTML)

	res := NewProber(testClient(), 5*time.Second).Probe(context.Background(), srv.URL)
	if res.Up {
		t.Fatalf("expected a db-error page to be classified DOWN, got %+v", res)
	}
	if res.Error != "wp_db_error" {
		t.Fatalf("expected Error=wp_db_error, got %q", res.Error)
	}
}

// TestProbeWpFatalFalsePositiveGuard asserts a full-theme page (header/nav/
// hero filler) that merely quotes the critical-error phrase, with no
// structural wp_die marker anywhere in the document, stays UP: the
// marker-AND-phrase co-occurrence requirement (not the size of the scanned
// window) is what guards this case.
func TestProbeWpFatalFalsePositiveGuard(t *testing.T) {
	filler := strings.Repeat("<p>Lorem ipsum dolor sit amet, consectetur adipiscing elit. </p>\n", 400)
	body := "<html><head><title>My Blog</title></head><body><header>Nav/hero chrome</header>" +
		filler +
		"<p>In this post we explain what it means when WordPress says " +
		"\"there has been a critical error on this website\" and how to fix it.</p>" +
		"</body></html>"
	srv := htmlServer(t, http.StatusOK, body)

	res := NewProber(testClient(), 5*time.Second).Probe(context.Background(), srv.URL)
	if !res.Up {
		t.Fatalf("expected a full-theme page quoting the phrase with no wp_die marker to stay UP, got %+v", res)
	}
}

// TestProbeWpFatalNormalPage asserts an ordinary HTML 200 page (no fatal
// signature at all) stays UP.
func TestProbeWpFatalNormalPage(t *testing.T) {
	srv := htmlServer(t, http.StatusOK, "<html><body><h1>Welcome to my site</h1></body></html>")

	res := NewProber(testClient(), 5*time.Second).Probe(context.Background(), srv.URL)
	if !res.Up {
		t.Fatalf("expected a normal page to be UP, got %+v", res)
	}
}

// TestProbeWpFatalPhraseWithoutMarkerStaysUp asserts the English phrase alone,
// without either wp_die structural marker, does NOT trip detection: both are
// required to co-occur.
func TestProbeWpFatalPhraseWithoutMarkerStaysUp(t *testing.T) {
	body := "<html><body><p>There has been a critical error on this website.</p></body></html>"
	srv := htmlServer(t, http.StatusOK, body)

	res := NewProber(testClient(), 5*time.Second).Probe(context.Background(), srv.URL)
	if !res.Up {
		t.Fatalf("expected phrase-without-marker to stay UP, got %+v", res)
	}
}

// TestProbeWpFatalNonHTMLContentTypeSkipped asserts a JSON body containing the
// markers/phrase is never scanned (wrong Content-Type), so it stays UP.
func TestProbeWpFatalNonHTMLContentTypeSkipped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"error-page","class":"wp-die-message","msg":"there has been a critical error on this website"}`))
	}))
	defer srv.Close()

	res := NewProber(testClient(), 5*time.Second).Probe(context.Background(), srv.URL)
	if !res.Up {
		t.Fatalf("expected a non-HTML content-type to skip the scan and stay UP, got %+v", res)
	}
}

// TestProbeWpFatalKillSwitch asserts WPMGR_UPTIME_WPFATAL_DETECT=0 disables
// the scan entirely, even against a real fatal page: the prober reads the
// env var once at construction.
func TestProbeWpFatalKillSwitch(t *testing.T) {
	t.Setenv(wpFatalDetectEnv, "0")
	srv := htmlServer(t, http.StatusOK, wpDieCriticalErrorHTML)

	res := NewProber(testClient(), 5*time.Second).Probe(context.Background(), srv.URL)
	if !res.Up {
		t.Fatalf("expected detection disabled by the kill-switch to stay UP, got %+v", res)
	}
}

// --- issue #132 re-verification: M2/S1/S2 ------------------------------------

// TestProbeWpDbErrorPhraseAlonePageStaysUp asserts a healthy support/blog
// article whose own title and content discuss "the error establishing a
// database connection" (a very common WordPress how-to topic) is NOT
// reclassified DOWN: the bare phrase alone, without WordPress core's
// dead_db() <title>Database Error</title> structural marker, must not trip
// wp_db_error (M2).
func TestProbeWpDbErrorPhraseAlonePageStaysUp(t *testing.T) {
	body := "<html><head><title>How to Fix the Error Establishing a Database Connection in WordPress - My Blog</title></head>" +
		"<body><h1>How to Fix the Error Establishing a Database Connection in WordPress</h1>" +
		"<p>If your site shows \"error establishing a database connection\", here is how to fix it: " +
		"check your wp-config.php credentials and confirm the database server is reachable.</p>" +
		"</body></html>"
	srv := htmlServer(t, http.StatusOK, body)

	res := NewProber(testClient(), 5*time.Second).Probe(context.Background(), srv.URL)
	if !res.Up {
		t.Fatalf("expected a healthy how-to article mentioning the db-error phrase to stay UP, got %+v", res)
	}
}

// TestProbeWpDbErrorStructuralPageDetected asserts WordPress core's
// chrome-less dead_db() page — <title>Database Error</title> co-located with
// the lone <h1>Error establishing a database connection</h1> — is still
// reclassified DOWN with wp_db_error now that the bare-phrase path is gated
// behind a structural marker (M2 regression guard for the real case).
func TestProbeWpDbErrorStructuralPageDetected(t *testing.T) {
	srv := htmlServer(t, http.StatusOK, wpDbErrorHTML)

	res := NewProber(testClient(), 5*time.Second).Probe(context.Background(), srv.URL)
	if res.Up {
		t.Fatalf("expected the real dead_db() structural page to be classified DOWN, got %+v", res)
	}
	if res.Error != "wp_db_error" {
		t.Fatalf("expected Error=wp_db_error, got %q", res.Error)
	}
}

// TestProbeWpFatalAppendedAfterFlushedHead asserts a wp_die critical-error
// document appended AFTER ~30 KB of already-flushed theme head content
// (simulating a modern block theme's inline global-styles CSS, the primary
// real-world shape of a 200-status WordPress fatal) is still detected: the
// scan must cover the full buffered read window, not just the first 16 KB
// (S1).
func TestProbeWpFatalAppendedAfterFlushedHead(t *testing.T) {
	head := strings.Repeat("<div>Block theme inline global-styles CSS chrome. </div>\n", 600)
	if len(head) <= 30<<10 {
		t.Fatalf("test head filler too small to exceed 30KB: %d bytes", len(head))
	}
	body := "<html><head><title>My Site</title></head><body>" + head + wpDieCriticalErrorHTML + "</body></html>"
	if len(body) >= maxProbeBody {
		t.Fatalf("test body too large, exceeds maxProbeBody: %d bytes", len(body))
	}
	srv := htmlServer(t, http.StatusOK, body)

	res := NewProber(testClient(), 5*time.Second).Probe(context.Background(), srv.URL)
	if res.Up {
		t.Fatalf("expected a wp_die doc appended after 30KB+ of flushed head content to be classified DOWN, got %+v", res)
	}
	if res.Error != "wp_fatal_error" {
		t.Fatalf("expected Error=wp_fatal_error, got %q", res.Error)
	}
}

// TestProbeWpFatalProximityGuard asserts a tutorial article that discusses
// the critical-error phrase in its intro, then separately quotes the
// id="error-page" marker in an unrelated <pre><code> markup sample several
// KB later, stays UP: the marker and phrase must be DOM-co-located (within
// fatalProximityBytes), not merely both present anywhere in the buffered
// response (S2).
func TestProbeWpFatalProximityGuard(t *testing.T) {
	intro := "<p>WordPress sometimes shows a critical error page. In this article we explain what " +
		"\"there has been a critical error on this website\" means and how to fix it.</p>\n"
	filler := strings.Repeat("<p>Lorem ipsum dolor sit amet, consectetur adipiscing elit. </p>\n", 60)
	if len(filler) <= fatalProximityBytes {
		t.Fatalf("test filler too small to exceed fatalProximityBytes: %d bytes", len(filler))
	}
	codeSample := `<p>The error page markup looks like this:</p><pre><code>&lt;body id="error-page"&gt;...&lt;/body&gt;</code></pre>`
	body := "<html><head><title>Understanding the WordPress Critical Error Screen</title></head><body>" +
		intro + filler + codeSample + "</body></html>"
	srv := htmlServer(t, http.StatusOK, body)

	res := NewProber(testClient(), 5*time.Second).Probe(context.Background(), srv.URL)
	if !res.Up {
		t.Fatalf("expected a marker and phrase separated by unrelated article body to stay UP, got %+v", res)
	}
}
