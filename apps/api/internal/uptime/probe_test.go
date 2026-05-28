package uptime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
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
