package agentcmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

// TestVerifyReachableWithReason_RedirectIsReasonRedirected: a ping answered
// with a redirect classifies as ReasonRedirected, not as an old agent, and the
// metadata fallback is not attempted.
func TestVerifyReachableWithReason_RedirectIsReasonRedirected(t *testing.T) {
	var metadataHits, targetHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/wp-json/wpmgr/v1/command/metadata" {
			metadataHits.Add(1)
		}
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusMovedPermanently)
	}))
	defer srv.Close()

	client := buildTestAgentClient(t, srv)
	alive, fallback, reason, err := client.VerifyReachableWithReason(context.Background(), uuid.New(), srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if alive || fallback {
		t.Fatalf("alive=%v fallback=%v, want false/false", alive, fallback)
	}
	if reason != ReasonRedirected {
		t.Fatalf("reason = %q, want %q", reason, ReasonRedirected)
	}
	if n := metadataHits.Load(); n != 0 {
		t.Errorf("metadata fallback sent %d requests, want 0", n)
	}
	if n := targetHits.Load(); n != 0 {
		t.Errorf("redirect target received %d requests, want 0", n)
	}
	if got := classifyTransportErr(&RedirectError{Command: "ping", Status: 302}); got != ReasonRedirected {
		t.Errorf("classifyTransportErr(RedirectError) = %q, want %q", got, ReasonRedirected)
	}
}
