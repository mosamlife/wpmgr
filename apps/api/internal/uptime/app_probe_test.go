package uptime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
)

// buildTestAppProber builds an *AppProber backed by an SSRF-disabled
// httpclient that can reach loopback httptest servers (test-only), mirroring
// internal/agentcmd's buildTestProbe.
func buildTestAppProber(t *testing.T) *AppProber {
	t.Helper()
	hc := httpclient.New(httpclient.Config{Timeout: 5 * time.Second, AllowPrivateNetworks: true})
	return NewAppProber(hc, 5*time.Second)
}

// hitTracker records every request path this test server received, so a test
// can assert exactly which endpoints were (or were not) hit.
type hitTracker struct {
	mu    sync.Mutex
	paths []string
}

func (h *hitTracker) record(path string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.paths = append(h.paths, path)
}

func (h *hitTracker) count(path string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, p := range h.paths {
		if p == path {
			n++
		}
	}
	return n
}

func (h *hitTracker) total() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.paths)
}

// TestAppProber_FreshHeartbeat_ZeroHTTPRequests proves B0 (agent ground
// truth): a heartbeat fresher than the app-probe interval already proves PHP
// booted, so the probe makes ZERO network requests - the server fails the
// test outright if it receives any request at all.
func TestAppProber_FreshHeartbeat_ZeroHTTPRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected HTTP request to %s - B0 must make zero network requests on a fresh heartbeat", r.URL.Path)
	}))
	defer srv.Close()

	p := buildTestAppProber(t)
	lastSeen := time.Now().Add(-30 * time.Second)
	res := p.Probe(context.Background(), srv.URL, &lastSeen, 5*time.Minute, "")

	if res.Up == nil || !*res.Up {
		t.Fatalf("expected Up=true for a fresh heartbeat, got %+v", res)
	}
	if res.Reason != AppProbeReasonAgentFresh {
		t.Fatalf("expected reason %q, got %q", AppProbeReasonAgentFresh, res.Reason)
	}
}

// TestAppProber_StaleHeartbeat_FallsThroughToNetworkProbe proves B0 does NOT
// short-circuit when the heartbeat is older than the app-probe interval -
// exactly when the direct measurement is needed.
func TestAppProber_StaleHeartbeat_FallsThroughToNetworkProbe(t *testing.T) {
	tr := &hitTracker{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr.record(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"Test Site"}`))
	}))
	defer srv.Close()

	p := buildTestAppProber(t)
	stale := time.Now().Add(-10 * time.Minute)
	res := p.Probe(context.Background(), srv.URL, &stale, 5*time.Minute, "")

	if tr.total() == 0 {
		t.Fatal("expected at least one network request for a stale heartbeat")
	}
	if res.Up == nil || !*res.Up {
		t.Fatalf("expected Up=true, got %+v", res)
	}
}

// TestAppProber_RestOK_JSONIndex_AppUpTrue proves B1 against a valid WP REST
// index (200, JSON object body) resolves app_up=true, and that the request
// went to /wp-json/ carrying the cache-buster query parameter.
func TestAppProber_RestOK_JSONIndex_AppUpTrue(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"Test Site","description":"just another WordPress site"}`))
	}))
	defer srv.Close()

	p := buildTestAppProber(t)
	res := p.Probe(context.Background(), srv.URL, nil, 5*time.Minute, "")

	if res.Up == nil || !*res.Up {
		t.Fatalf("expected Up=true for a valid JSON REST index, got %+v", res)
	}
	if res.Reason != AppProbeReasonRestOK {
		t.Fatalf("expected reason %q, got %q", AppProbeReasonRestOK, res.Reason)
	}
	if gotPath != "/wp-json/" {
		t.Fatalf("expected request to /wp-json/, got %q", gotPath)
	}
	if gotQuery == "" {
		t.Fatal("expected a cache-buster query parameter, got none")
	}
}

// TestAppProber_5xx_AppUpFalse proves a 5xx response is conclusively down.
func TestAppProber_5xx_AppUpFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := buildTestAppProber(t)
	res := p.Probe(context.Background(), srv.URL, nil, 5*time.Minute, "")

	if res.Up == nil || *res.Up {
		t.Fatalf("expected Up=false for a 5xx response, got %+v", res)
	}
	if res.Reason != AppProbeReasonRest5xx {
		t.Fatalf("expected reason %q, got %q", AppProbeReasonRest5xx, res.Reason)
	}
}

// TestAppProber_502And504_Unknown proves 502 and 504 classify UNKNOWN with
// their own distinct reasons, never conclusive false - a 502 is the same
// "upstream unreachable" condition this file already classifies UNKNOWN when
// its own client sees a connection refused directly, and a 504 is literally a
// timeout reported by the proxy instead of this probe's own client, which is
// already UNKNOWN when observed directly (AppProbeReasonTimeout). Observing
// either secondhand via the site's reverse proxy is not stronger evidence.
func TestAppProber_502And504_Unknown(t *testing.T) {
	t.Run("502 bad gateway", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer srv.Close()

		p := buildTestAppProber(t)
		res := p.Probe(context.Background(), srv.URL, nil, 5*time.Minute, "")
		if res.Up != nil {
			t.Fatalf("expected Up=nil (unknown) for a 502, got %+v", res)
		}
		if res.Reason != AppProbeReasonRestBadGateway {
			t.Fatalf("expected reason %q, got %q", AppProbeReasonRestBadGateway, res.Reason)
		}
	})

	t.Run("504 gateway timeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusGatewayTimeout)
		}))
		defer srv.Close()

		p := buildTestAppProber(t)
		res := p.Probe(context.Background(), srv.URL, nil, 5*time.Minute, "")
		if res.Up != nil {
			t.Fatalf("expected Up=nil (unknown) for a 504, got %+v", res)
		}
		if res.Reason != AppProbeReasonRestGatewayTimeout {
			t.Fatalf("expected reason %q, got %q", AppProbeReasonRestGatewayTimeout, res.Reason)
		}
	})
}

// TestAppProber_500_StillConclusiveFalse proves the 502/504 UNKNOWN carve-outs
// did not weaken the 500 case: PHP itself returning an error remains the
// positive evidence of the application running and failing that this signal
// exists to capture, so it must stay a conclusive false with the original
// AppProbeReasonRest5xx reason.
func TestAppProber_500_StillConclusiveFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := buildTestAppProber(t)
	res := p.Probe(context.Background(), srv.URL, nil, 5*time.Minute, "")

	if res.Up == nil || *res.Up {
		t.Fatalf("expected Up=false for a 500, got %+v", res)
	}
	if res.Reason != AppProbeReasonRest5xx {
		t.Fatalf("expected reason %q, got %q", AppProbeReasonRest5xx, res.Reason)
	}
}

// TestAppProber_MalformedSiteURL_Unknown proves a site URL that buildURL
// cannot turn into a request (no host here) classifies UNKNOWN with a reason
// that names the real cause, never a conclusive false - a malformed stored
// site URL is a configuration problem, not evidence the application ran and
// failed. Covers both the B1 default-target call site and the B3 override
// call site, since Probe returns early from each independently.
func TestAppProber_MalformedSiteURL_Unknown(t *testing.T) {
	t.Run("B1 default target", func(t *testing.T) {
		p := buildTestAppProber(t)
		res := p.Probe(context.Background(), "not-a-valid-url", nil, 5*time.Minute, "")
		if res.Up != nil {
			t.Fatalf("expected Up=nil (unknown) for a malformed site URL, got %+v", res)
		}
		if res.Reason != AppProbeReasonInvalidSiteURL {
			t.Fatalf("expected reason %q, got %q", AppProbeReasonInvalidSiteURL, res.Reason)
		}
	})

	t.Run("B3 override target", func(t *testing.T) {
		p := buildTestAppProber(t)
		res := p.Probe(context.Background(), "not-a-valid-url", nil, 5*time.Minute, "/healthz")
		if res.Up != nil {
			t.Fatalf("expected Up=nil (unknown) for a malformed site URL, got %+v", res)
		}
		if res.Reason != AppProbeReasonInvalidSiteURL {
			t.Fatalf("expected reason %q, got %q", AppProbeReasonInvalidSiteURL, res.Reason)
		}
	})
}

// TestAppProber_WPFatalPage200_ConclusiveFalse proves the path GH #291 review
// flagged as untested: an HTTP 200 whose body carries the WordPress
// fatal-error page signature (headers already sent before wp_die() ran) is a
// conclusive app_up=false, not merely UNKNOWN - this is the exact case Phase 3
// turns into a page, so it must be exercised directly rather than only
// documented.
func TestAppProber_WPFatalPage200_ConclusiveFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(wpDieCriticalErrorHTML))
	}))
	defer srv.Close()

	p := buildTestAppProber(t)
	res := p.Probe(context.Background(), srv.URL, nil, 5*time.Minute, "")

	if res.Up == nil || *res.Up {
		t.Fatalf("expected Up=false for a 200 WP fatal-error page, got %+v", res)
	}
	if res.Reason != AppProbeReasonWPFatalError {
		t.Fatalf("expected reason %q, got %q", AppProbeReasonWPFatalError, res.Reason)
	}
}

// TestAppProber_ConnectionFailure_Unknown proves a transport-level failure
// (nothing listening) classifies UNKNOWN, never a conclusive false - GH #291
// review finding 2: a connection refused is arguably conclusive for the SITE
// as a whole, but that judgment belongs to the reachability prober
// (probe.go), which already records the site DOWN independently of this
// probe. This narrower application-health signal never proves the
// application itself ran and failed from a bare transport error, so it must
// not manufacture a false positive.
func TestAppProber_ConnectionFailure_Unknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	deadURL := srv.URL
	srv.Close() // nothing is listening anymore.

	p := buildTestAppProber(t)
	res := p.Probe(context.Background(), deadURL, nil, 5*time.Minute, "")

	if res.Up != nil {
		t.Fatalf("expected Up=nil (unknown) for a connection failure, got %+v", res)
	}
	if res.Reason != AppProbeReasonUnreachable {
		t.Fatalf("expected reason %q, got %q", AppProbeReasonUnreachable, res.Reason)
	}
}

// TestAppProber_Timeout_Unknown proves a request that blows the probe's own
// context deadline classifies UNKNOWN with its own distinct reason, never a
// conclusive false - GH #291 review finding 2: a merely SLOW site (a heavy
// plugin, a cold PHP-FPM worker) is common and healthy, and treating a
// timeout as application-down would be a false positive the moment a later
// phase turns verdicts into pages.
func TestAppProber_Timeout_Unknown(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-block // never respond within the probe's timeout.
	}))
	// srv.Close() blocks until every in-flight handler returns, so the block
	// channel MUST be closed (unblocking the handler goroutine still waiting
	// on the timed-out request) BEFORE Close() is called, not via a separate
	// t.Cleanup - t.Cleanup hooks run AFTER this function's own defers, which
	// would call Close() first and deadlock forever.
	defer func() {
		close(block)
		srv.Close()
	}()

	hc := httpclient.New(httpclient.Config{Timeout: 5 * time.Second, AllowPrivateNetworks: true})
	p := NewAppProber(hc, 100*time.Millisecond)
	res := p.Probe(context.Background(), srv.URL, nil, 5*time.Minute, "")

	if res.Up != nil {
		t.Fatalf("expected Up=nil (unknown) for a timeout, got %+v", res)
	}
	if res.Reason != AppProbeReasonTimeout {
		t.Fatalf("expected reason %q, got %q", AppProbeReasonTimeout, res.Reason)
	}
}

// TestAppProber_503_MaintenanceUnknown proves HTTP 503 classifies UNKNOWN
// with its own reason, never a conclusive false - GH #291 review finding 3:
// 503 is exactly what WordPress core's own maintenance mode emits via the
// .maintenance file, so any site this product is actively updating would
// otherwise be painted application-down purely because the update is
// running as designed.
func TestAppProber_503_MaintenanceUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p := buildTestAppProber(t)
	res := p.Probe(context.Background(), srv.URL, nil, 5*time.Minute, "")

	if res.Up != nil {
		t.Fatalf("expected Up=nil (unknown) for a 503, got %+v", res)
	}
	if res.Reason != AppProbeReasonRestMaintenance {
		t.Fatalf("expected reason %q, got %q", AppProbeReasonRestMaintenance, res.Reason)
	}
}

// TestAppProber_B2GetsOwnTimeoutBudget_NotStarvedByB1 proves GH #291 review
// finding 4: B1 and B2 each get their OWN full timeout budget rather than
// sharing one deadline. A B1 target that consumes almost the entire budget
// (but still answers, inconclusively, in time to trigger the B2 fallback)
// must not leave B2 starved - B2 must still have enough of ITS OWN budget
// left to complete a fast response and produce its own genuine verdict,
// which is only possible if B2's context is a fresh one, not B1's leftover.
func TestAppProber_B2GetsOwnTimeoutBudget_NotStarvedByB1(t *testing.T) {
	const timeout = 300 * time.Millisecond
	tr := &hitTracker{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr.record(r.URL.Path)
		if r.URL.Path == "/wp-json/" {
			// Consume most of a single attempt's budget before answering
			// inconclusively (404), triggering the B2 fallback.
			time.Sleep(timeout - 50*time.Millisecond)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// B2 fallback target: answers fast, well within a FRESH budget, but
		// would be starved if it only inherited whatever remained of B1's
		// already-mostly-consumed deadline.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"Test Site (fallback)"}`))
	}))
	defer srv.Close()

	hc := httpclient.New(httpclient.Config{Timeout: 5 * time.Second, AllowPrivateNetworks: true})
	p := NewAppProber(hc, timeout)
	res := p.Probe(context.Background(), srv.URL, nil, 5*time.Minute, "")

	if tr.count("/") == 0 {
		t.Fatalf("expected the B2 fallback to be attempted, got requests: %v", tr.paths)
	}
	if res.Up == nil || !*res.Up || res.Reason != AppProbeReasonRestOK {
		t.Fatalf("expected B2's own successful verdict (not starved by B1's near-timeout budget), got %+v", res)
	}
}

// TestAppProber_CacheHit_Unknown proves the GH #291 Phase 2 rule that matters
// most: a detected cache HIT is UNKNOWN, never healthy - even when the
// cached body happens to be valid JSON. Silently reporting healthy when the
// bypass was defeated would be worse than the bug this phase fixes.
func TestAppProber_CacheHit_Unknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Cache-Status", "HIT")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"Test Site"}`))
	}))
	defer srv.Close()

	p := buildTestAppProber(t)
	res := p.Probe(context.Background(), srv.URL, nil, 5*time.Minute, "")

	if res.Up != nil {
		t.Fatalf("expected Up=nil (unknown) for a cache HIT, got %+v", res)
	}
	if res.Reason != AppProbeReasonCacheHit {
		t.Fatalf("expected reason %q, got %q", AppProbeReasonCacheHit, res.Reason)
	}
}

// TestAppProber_401And404_Unknown proves 401 and 404 are UNKNOWN, never
// app-down - a 401 rest_not_logged_in is extremely common (security plugins,
// including WPMgr's own suite) and a 404 is normal on a REST-disabled
// install. Treating either as down would cause a fleet-wide false-alarm
// storm.
func TestAppProber_401And404_Unknown(t *testing.T) {
	t.Run("401 forbidden", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()

		p := buildTestAppProber(t)
		res := p.Probe(context.Background(), srv.URL, nil, 5*time.Minute, "")
		if res.Up != nil {
			t.Fatalf("expected Up=nil (unknown) for a 401, got %+v", res)
		}
		if res.Reason != AppProbeReasonRestForbidden {
			t.Fatalf("expected reason %q, got %q", AppProbeReasonRestForbidden, res.Reason)
		}
	})

	t.Run("404 on both wp-json and the rest_route fallback", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		p := buildTestAppProber(t)
		res := p.Probe(context.Background(), srv.URL, nil, 5*time.Minute, "")
		if res.Up != nil {
			t.Fatalf("expected Up=nil (unknown) for a 404, got %+v", res)
		}
		if res.Reason != AppProbeReasonRestAbsent {
			t.Fatalf("expected reason %q, got %q", AppProbeReasonRestAbsent, res.Reason)
		}
	})
}

// TestAppProber_B2Fallback_OnlyOn404OrNonJSON proves the fallback rule
// precisely: B2 (?rest_route=/) fires when B1 (/wp-json/) returns 404 or a
// non-JSON 200, and NEVER fires for any other B1 outcome (401, 5xx, or a
// conclusive JSON 200) - retrying an unreachable/forbidden/broken backend a
// second way would not change the verdict and would double the load.
func TestAppProber_B2Fallback_OnlyOn404OrNonJSON(t *testing.T) {
	cases := []struct {
		name            string
		b1Status        int
		b1ContentType   string
		b1Body          string
		wantFallbackHit bool
	}{
		{"404 triggers fallback", http.StatusNotFound, "", "", true},
		{"non-JSON 200 triggers fallback", http.StatusOK, "text/html", "<html>not json</html>", true},
		{"401 does not trigger fallback", http.StatusUnauthorized, "", "", false},
		{"403 does not trigger fallback", http.StatusForbidden, "", "", false},
		{"5xx does not trigger fallback", http.StatusInternalServerError, "", "", false},
		{"other 4xx does not trigger fallback", http.StatusTooManyRequests, "", "", false},
		{"valid JSON 200 does not trigger fallback", http.StatusOK, "application/json", `{"name":"Test"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := &hitTracker{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tr.record(r.URL.Path)
				if r.URL.Path == "/wp-json/" {
					if tc.b1ContentType != "" {
						w.Header().Set("Content-Type", tc.b1ContentType)
					}
					w.WriteHeader(tc.b1Status)
					_, _ = w.Write([]byte(tc.b1Body))
					return
				}
				// The B2 fallback target (rest_route=/ on the site root):
				// always answer with a valid JSON index so a fired fallback
				// is unambiguously observable via its OWN distinct verdict.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"name":"Test Site (fallback)"}`))
			}))
			defer srv.Close()

			p := buildTestAppProber(t)
			res := p.Probe(context.Background(), srv.URL, nil, 5*time.Minute, "")

			fallbackHit := tr.count("/") > 0
			if fallbackHit != tc.wantFallbackHit {
				t.Fatalf("fallback hit = %v, want %v (result: %+v, requests: %v)", fallbackHit, tc.wantFallbackHit, res, tr.paths)
			}
			if tc.wantFallbackHit && (res.Up == nil || !*res.Up || res.Reason != AppProbeReasonRestOK) {
				t.Fatalf("expected the fallback's own successful verdict to win, got %+v", res)
			}
		})
	}
}

// TestAppProber_Override_ReplacesDefaultTarget proves B3: a per-site
// override path is requested INSTEAD of /wp-json/, with no B2 fallback even
// on a 404 - an operator who configured a custom health-check path
// presumably did so because the default targets are unreliable for this
// site.
func TestAppProber_Override_ReplacesDefaultTarget(t *testing.T) {
	tr := &hitTracker{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr.record(r.URL.Path)
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	p := buildTestAppProber(t)
	res := p.Probe(context.Background(), srv.URL, nil, 5*time.Minute, "/healthz")

	if res.Up == nil || !*res.Up {
		t.Fatalf("expected Up=true via the override path, got %+v", res)
	}
	if tr.count("/wp-json/") != 0 {
		t.Fatalf("expected the default /wp-json/ target to never be requested when an override is set, got requests: %v", tr.paths)
	}
	if tr.count("/") != 0 {
		t.Fatalf("expected no B2 fallback when an override is set, got requests: %v", tr.paths)
	}
	if tr.count("/healthz") != 1 {
		t.Fatalf("expected exactly one request to the override path, got requests: %v", tr.paths)
	}
}

// TestAppProbeDue_DefaultRatio_RoughlyOneInFive proves the stateless cadence
// check's arithmetic at the documented default ratio (300s / 60s = 5): over
// 5 consecutive sweep ticks, a given site is due on EXACTLY one of them -
// the "roughly 4 of every 5 rollup upserts carry app_up = NULL" the design
// doc describes.
func TestAppProbeDue_DefaultRatio_RoughlyOneInFive(t *testing.T) {
	siteID := uuid.New()
	probeInterval := time.Minute
	appProbeInterval := 5 * time.Minute
	base := time.Now().Truncate(time.Minute)

	dueCount := 0
	for i := 0; i < 5; i++ {
		tick := base.Add(time.Duration(i) * probeInterval)
		if appProbeDue(siteID, tick, probeInterval, appProbeInterval) {
			dueCount++
		}
	}
	if dueCount != 1 {
		t.Fatalf("expected exactly 1 due tick out of 5 consecutive ticks, got %d", dueCount)
	}
}

// TestAppProbeDue_Deterministic proves the same (siteID, now, intervals)
// input always produces the same output - required for the cadence check to
// be safely stateless/restart-safe (no shared memory, no DB round trip).
func TestAppProbeDue_Deterministic(t *testing.T) {
	siteID := uuid.New()
	now := time.Now()
	a := appProbeDue(siteID, now, time.Minute, 5*time.Minute)
	b := appProbeDue(siteID, now, time.Minute, 5*time.Minute)
	if a != b {
		t.Fatalf("expected deterministic result, got %v then %v", a, b)
	}
}

// TestAppProbeDue_DistributesAcrossSites proves different sites are not all
// due on the same tick (which would turn the app probe into a periodic
// thundering herd instead of a steady trickle) - at least two distinct
// due/not-due outcomes across a handful of sites on the SAME tick.
func TestAppProbeDue_DistributesAcrossSites(t *testing.T) {
	now := time.Now()
	dueTrue, dueFalse := false, false
	for i := 0; i < 50; i++ {
		if appProbeDue(uuid.New(), now, time.Minute, 5*time.Minute) {
			dueTrue = true
		} else {
			dueFalse = true
		}
		if dueTrue && dueFalse {
			return
		}
	}
	t.Fatal("expected both due and not-due sites among 50 random UUIDs on the same tick - cadence may not be spreading load")
}

// TestAppProbeDue_ZeroOrNegativeIntervals_DefaultsSafely proves
// non-positive intervals fall back to sane defaults rather than panicking
// (e.g. a division by zero) or making every site due on every tick forever.
func TestAppProbeDue_ZeroOrNegativeIntervals_DefaultsSafely(t *testing.T) {
	siteID := uuid.New()
	now := time.Now()
	// Must not panic.
	_ = appProbeDue(siteID, now, 0, 0)
	_ = appProbeDue(siteID, now, -time.Minute, -time.Minute)
}

// TestAppProbeDue_SubSecondProbeInterval_DoesNotPanic proves a probeInterval
// that is POSITIVE but under one second does not panic: appProbeDue's own
// divisor is int64(probeInterval.Seconds()), which truncates a value like
// 500ms to 0 - the `probeInterval <= 0` guard alone does not catch this,
// since 500ms is not <= 0, so the divisor still needs its own floor.
func TestAppProbeDue_SubSecondProbeInterval_DoesNotPanic(t *testing.T) {
	siteID := uuid.New()
	now := time.Now()
	// Must not panic with an integer divide-by-zero.
	_ = appProbeDue(siteID, now, 500*time.Millisecond, 5*time.Minute)
	_ = appProbeDue(siteID, now, 1*time.Nanosecond, 5*time.Minute)
}
