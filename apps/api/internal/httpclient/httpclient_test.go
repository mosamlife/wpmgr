package httpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestSSRFBlocksPrivateAndLoopback proves the SSRF-hardened client REFUSES to
// connect to private/loopback/link-local destinations — the key M3 security
// guarantee, since site URLs are attacker-influenced. The default client (guard
// ON, ports 80/443) must reject these at dial time.
func TestSSRFBlocksPrivateAndLoopback(t *testing.T) {
	c := New(DefaultConfig())

	targets := []string{
		"http://127.0.0.1/",       // loopback
		"http://169.254.169.254/", // link-local (cloud metadata)
		"http://10.0.0.1/",        // private
		"http://192.168.1.1/",     // private
		"http://[::1]/",           // IPv6 loopback
		"http://localhost/",       // resolves to loopback
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			resp, err := c.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if err == nil {
				t.Fatalf("expected SSRF block dialing %s, got a successful connection", target)
			}
			if !IsSSRFBlocked(err) {
				t.Fatalf("error dialing %s was not classified as an SSRF block: %v", target, err)
			}
		})
	}
}

// TestSSRFAllowsLoopbackOnlyWithEscapeHatch proves the test-only escape hatch
// (AllowPrivateNetworks) lets a loopback httptest server be reached, so the
// worker integration tests can stand in a fake agent — and that this is OFF by
// default.
func TestSSRFAllowsLoopbackOnlyWithEscapeHatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Default config (guard on) must block the loopback test server.
	blocked := New(DefaultConfig())
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	if resp, err := blocked.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatal("default client should block loopback httptest server")
	}

	// Escape-hatch client must reach it.
	allowed := New(Config{AllowPrivateNetworks: true, Timeout: 5 * time.Second})
	req2, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := allowed.Do(req2)
	if err != nil {
		t.Fatalf("escape-hatch client should reach loopback: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
