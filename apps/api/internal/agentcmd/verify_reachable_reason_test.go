package agentcmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestVerifyReachableWithReason_PingAlive proves the alive-via-ping path
// classifies as ReasonAlive, matching the plain VerifyReachable outcome.
func TestVerifyReachableWithReason_PingAlive(t *testing.T) {
	siteID := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/wp-json/wpmgr/v1/command/ping" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true,"agent_version":"0.44.0"}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := buildTestAgentClient(t, srv)
	alive, fallback, reason, err := client.VerifyReachableWithReason(context.Background(), siteID, srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !alive || fallback {
		t.Fatalf("alive=%v fallback=%v, want true/false", alive, fallback)
	}
	if reason != ReasonAlive {
		t.Fatalf("reason = %q, want %q", reason, ReasonAlive)
	}
}

// TestVerifyReachableWithReason_MetadataFallbackAlive proves the old-agent
// fallback path (ping 404, metadata 200) also classifies as ReasonAlive.
func TestVerifyReachableWithReason_MetadataFallbackAlive(t *testing.T) {
	siteID := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/wp-json/wpmgr/v1/command/ping":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"rest_no_route"}`))
		case "/wp-json/wpmgr/v1/command/metadata":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true,"wp_version":"6.5.0"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := buildTestAgentClient(t, srv)
	alive, fallback, reason, err := client.VerifyReachableWithReason(context.Background(), siteID, srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !alive || !fallback {
		t.Fatalf("alive=%v fallback=%v, want true/true", alive, fallback)
	}
	if reason != ReasonAlive {
		t.Fatalf("reason = %q, want %q", reason, ReasonAlive)
	}
}

// TestVerifyReachableWithReason_AgentAbsent404 proves that when BOTH ping and
// the metadata fallback 404, the reason distinguishes "nothing here at all"
// (an uninstalled agent) from a broken one, per GH #291 Task 3's stated goal.
func TestVerifyReachableWithReason_AgentAbsent404(t *testing.T) {
	siteID := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"rest_no_route"}`))
	}))
	defer srv.Close()

	client := buildTestAgentClient(t, srv)
	alive, fallback, reason, err := client.VerifyReachableWithReason(context.Background(), siteID, srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if alive || !fallback {
		t.Fatalf("alive=%v fallback=%v, want false/true", alive, fallback)
	}
	if reason != ReasonAgentAbsent404 {
		t.Fatalf("reason = %q, want %q", reason, ReasonAgentAbsent404)
	}
}

// TestVerifyReachableWithReason_NotAgentShaped proves the captive-portal case
// (a 2xx that is not agent-shaped JSON on both ping and metadata) is
// classified as ReasonNotAgentShaped: the URL answers, just not with our
// agent, which is a materially different situation from a 404 or a 5xx.
func TestVerifyReachableWithReason_NotAgentShaped(t *testing.T) {
	siteID := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"welcome","login":"/portal"}`))
	}))
	defer srv.Close()

	client := buildTestAgentClient(t, srv)
	alive, fallback, reason, err := client.VerifyReachableWithReason(context.Background(), siteID, srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if alive || !fallback {
		t.Fatalf("alive=%v fallback=%v, want false/true", alive, fallback)
	}
	if reason != ReasonNotAgentShaped {
		t.Fatalf("reason = %q, want %q", reason, ReasonNotAgentShaped)
	}
}

// TestVerifyReachableWithReason_HTTP5xx proves a hard 5xx on ping (no
// 404/400 fallback trigger) classifies as ReasonHTTP5xx: an installed agent
// that is answering, but broken. This is the case GH #291 needs a name for.
func TestVerifyReachableWithReason_HTTP5xx(t *testing.T) {
	siteID := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":"service_unavailable"}`))
	}))
	defer srv.Close()

	client := buildTestAgentClient(t, srv)
	alive, fallback, reason, err := client.VerifyReachableWithReason(context.Background(), siteID, srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if alive || fallback {
		t.Fatalf("alive=%v fallback=%v, want false/false", alive, fallback)
	}
	if reason != ReasonHTTP5xx {
		t.Fatalf("reason = %q, want %q", reason, ReasonHTTP5xx)
	}
}

// TestVerifyReachableWithReason_HTTP4xx proves a non-404 client error on ping
// (e.g. a security plugin's 403) classifies as ReasonHTTP4xx, not as
// ReasonAgentAbsent404 or a generic unreachable.
func TestVerifyReachableWithReason_HTTP4xx(t *testing.T) {
	siteID := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"rest_forbidden"}`))
	}))
	defer srv.Close()

	client := buildTestAgentClient(t, srv)
	alive, fallback, reason, err := client.VerifyReachableWithReason(context.Background(), siteID, srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if alive || fallback {
		t.Fatalf("alive=%v fallback=%v, want false/false", alive, fallback)
	}
	if reason != ReasonHTTP4xx {
		t.Fatalf("reason = %q, want %q", reason, ReasonHTTP4xx)
	}
}

// TestVerifyReachableWithReason_Timeout proves a request that exceeds the
// caller's context deadline classifies as ReasonTimeout, not the generic
// ReasonUnreachable catch-all.
func TestVerifyReachableWithReason_Timeout(t *testing.T) {
	siteID := uuid.New()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	client := buildTestAgentClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	alive, fallback, reason, err := client.VerifyReachableWithReason(ctx, siteID, srv.URL)
	if err != nil {
		t.Fatalf("VerifyReachableWithReason must return (false,false,ReasonTimeout,nil) on a deadline, got err=%v", err)
	}
	if alive || fallback {
		t.Fatalf("alive=%v fallback=%v, want false/false", alive, fallback)
	}
	if reason != ReasonTimeout {
		t.Fatalf("reason = %q, want %q", reason, ReasonTimeout)
	}
}

// TestVerifyReachableWithReason_TLSError proves a certificate the client does
// not trust (an httptest TLS server's self-signed leaf, verified without the
// test-only InsecureSkipTLSVerify escape hatch) classifies as ReasonTLSError.
func TestVerifyReachableWithReason_TLSError(t *testing.T) {
	siteID := uuid.New()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	// buildTestAgentClient deliberately does NOT set InsecureSkipTLSVerify, so
	// the self-signed httptest certificate fails normal chain verification.
	client := buildTestAgentClient(t, srv)
	alive, fallback, reason, err := client.VerifyReachableWithReason(context.Background(), siteID, srv.URL)
	if err != nil {
		t.Fatalf("VerifyReachableWithReason must return (false,false,ReasonTLSError,nil) on a cert failure, got err=%v", err)
	}
	if alive || fallback {
		t.Fatalf("alive=%v fallback=%v, want false/false", alive, fallback)
	}
	if reason != ReasonTLSError {
		t.Fatalf("reason = %q, want %q", reason, ReasonTLSError)
	}
}

// TestVerifyReachableWithReason_MatchesVerifyReachable proves VerifyReachable
// and VerifyReachableWithReason never disagree on alive/fallbackUsed/err. The
// former must remain a pure delegate of the latter (GH #291 Task 3's "do not
// change the existing boolean contract" requirement).
func TestVerifyReachableWithReason_MatchesVerifyReachable(t *testing.T) {
	siteID := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := buildTestAgentClient(t, srv)
	wantAlive, wantFallback, wantErr := client.VerifyReachable(context.Background(), siteID, srv.URL)
	gotAlive, gotFallback, reason, gotErr := client.VerifyReachableWithReason(context.Background(), siteID, srv.URL)

	if wantAlive != gotAlive || wantFallback != gotFallback {
		t.Fatalf("VerifyReachable=(%v,%v) VerifyReachableWithReason=(%v,%v), must match",
			wantAlive, wantFallback, gotAlive, gotFallback)
	}
	if (wantErr == nil) != (gotErr == nil) {
		t.Fatalf("VerifyReachable err=%v VerifyReachableWithReason err=%v, must match presence", wantErr, gotErr)
	}
	if reason != ReasonHTTP5xx {
		t.Fatalf("reason = %q, want %q", reason, ReasonHTTP5xx)
	}
}
