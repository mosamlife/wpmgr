package server

import (
	"net/http"
	"testing"
	"time"
)

// TestHTTPServerTimeoutPosture pins the server-level timeout decision, because
// the obvious "harden the HTTP server" reflex is to add a WriteTimeout and that
// would silently break every long-lived response in the process.
//
// A server-level WriteTimeout is a whole-response deadline on EVERY handler. The
// SSE endpoints (update runs, backup runs, the connection-lifecycle bus) hold
// one response open for minutes by design, and the agent package download holds
// one open for as long as a slow site needs. Neither wants a duration cap; both
// want a PROGRESS bound, which is applied per response where it belongs.
//
// If a future change genuinely needs one of these, it has to come here, read the
// reasoning on newHTTPServer, and change this test on purpose.
func TestHTTPServerTimeoutPosture(t *testing.T) {
	srv := newHTTPServer(":0", http.NewServeMux())

	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %s, want 10s (a slow-headers client must not pin a goroutine)", srv.ReadHeaderTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %s, want 0: it is a whole-response deadline on every handler and would cut the SSE streams and the package download", srv.WriteTimeout)
	}
	if srv.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %s, want 0: it covers reading the request body and would cap large agent uploads", srv.ReadTimeout)
	}
	if srv.IdleTimeout != 0 {
		t.Errorf("IdleTimeout = %s, want 0: anything under the load balancer's own 600s idle timeout races it into 502s", srv.IdleTimeout)
	}
}
