package auth

// Regression pins for the address the authentication rate limiters key on.
//
// Two auth limiters make a DECISION from the caller's address rather than
// merely recording it:
//
//   - reset.go  "pwreset-consume:"+ip  (resetPerMinute)
//   - twofa.go  "2fa-ip:"+ip           (twoFAIPLockoutPerMinute)
//
// Both key on clientAddr(), i.e. gin's c.ClientIP(). What that resolves to
// depends on how the engine is configured, and nothing in the suite pinned it.
// These tests pin the requirement: the key must be the peer address, so that a
// single peer gets a single bucket regardless of what it sends.
//
// Each pin pairs its arm with an honest control in the same run. The control
// asserts BOTH that the limiter still refuses past the cap and that it does not
// refuse below the cap, so a limiter that stopped working — in either direction
// — fails the control rather than passing as a fix.
//
// THESE PINS ARE EXPECTED RED until the limiters are keyed on the peer address.
// Keying them correctly depends on a deployment fact (how many proxy hops sit in
// front of the process) that is not recorded in this repository, so the pins
// land first and the remedy follows once that is established.
//
// LOCKSTEP: engineAsProductionBuildsIt below mirrors server.New's engine
// construction. If the remedy is applied there, this helper must be updated in
// the same commit or these pins keep exercising an engine production no longer
// builds.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/autologin"
)

// engineAsProductionBuildsIt mirrors server.New's engine construction: gin.New(),
// with no declaration of which proxies are trusted. See LOCKSTEP above.
func engineAsProductionBuildsIt() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	return gin.New()
}

// probeOnce issues one request and returns its status and body.
func probeOnce(t *testing.T, client *http.Client, method, url, fwd string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fwd != "" {
		req.Header.Set("X-Forwarded-For", fwd)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 256)
	n, _ := resp.Body.Read(buf)
	return resp.StatusCode, string(buf[:n])
}

// TestClientAddrResolvesToPeerAddress is the root-cause pin: the address the
// limiters key on must be the peer's, not a value carried in the request. Every
// request here comes from the same real TCP peer.
func TestClientAddrResolvesToPeerAddress(t *testing.T) {
	e := engineAsProductionBuildsIt()
	e.GET("/probe", func(c *gin.Context) {
		c.String(http.StatusOK, "%s", clientAddr(c).String())
	})
	srv := httptest.NewServer(e)
	defer srv.Close()

	// Establish the honest baseline: what this peer resolves to with no
	// forwarding metadata at all. Every other request must yield the same.
	_, baseline := probeOnce(t, srv.Client(), "GET", srv.URL+"/probe", "")
	if baseline == "" {
		t.Fatal("baseline clientAddr is empty; the probe is not measuring anything")
	}
	t.Logf("honest baseline (no forwarding metadata): clientAddr=%s", baseline)

	for _, fwd := range []string{
		"203.0.113.9",
		"198.51.100.7, 10.0.0.1",
		"1.2.3.4, 203.0.113.9, 10.0.0.1",
	} {
		_, got := probeOnce(t, srv.Client(), "GET", srv.URL+"/probe", fwd)
		if got != baseline {
			t.Errorf("request metadata %q changed the limiter address to %s (peer baseline %s); "+
				"the limiter key must not vary with request content", fwd, got, baseline)
		}
	}
}

// TestPasswordResetConsumeLimiterKeysOnPeerAddress drives the real production
// limiter with the real key format and constant from reset.go, from one TCP
// peer, well past the point where the honest control is refused.
func TestPasswordResetConsumeLimiterKeysOnPeerAddress(t *testing.T) {
	// 4x the cap: far enough past it that a working limiter must refuse.
	attempts := resetPerMinute * 4

	run := func(vary bool) (allowed, refused int) {
		lim := autologin.NewMemoryLimiter()
		defer lim.Stop()
		e := engineAsProductionBuildsIt()
		e.POST("/auth/password/reset", func(c *gin.Context) {
			ip := clientAddr(c)
			if ip.IsValid() {
				if ok, _ := lim.Allow(context.Background(), "pwreset-consume:"+ip.String(), resetPerMinute); !ok {
					c.String(http.StatusTooManyRequests, "429")
					return
				}
			}
			c.String(http.StatusOK, "200")
		})
		srv := httptest.NewServer(e)
		defer srv.Close()

		client := srv.Client()
		for i := 0; i < attempts; i++ {
			fwd := ""
			if vary {
				fwd = fmt.Sprintf("203.0.113.%d", i+1)
			}
			code, _ := probeOnce(t, client, "POST", srv.URL+"/auth/password/reset", fwd)
			if code == http.StatusTooManyRequests {
				refused++
			} else {
				allowed++
			}
		}
		return allowed, refused
	}

	variedAllowed, variedRefused := run(true)
	ctrlAllowed, ctrlRefused := run(false)

	t.Logf("varied: allowed=%d refused=%d | control: allowed=%d refused=%d (cap=%d/min over %d attempts)",
		variedAllowed, variedRefused, ctrlAllowed, ctrlRefused, resetPerMinute, attempts)

	// Control first: a limiter that refuses nothing would make the assertion
	// below pass for the wrong reason.
	if ctrlRefused == 0 {
		t.Fatalf("CONTROL BROKEN: the limiter refused nothing on the control arm (allowed=%d of %d); "+
			"this test cannot detect a regression", ctrlAllowed, attempts)
	}
	// And the control must not over-fire: the first resetPerMinute attempts are
	// legitimate and must be served.
	if ctrlAllowed < resetPerMinute {
		t.Errorf("CONTROL OVER-FIRES: only %d of the first %d legitimate attempts were allowed",
			ctrlAllowed, resetPerMinute)
	}
	if variedRefused == 0 {
		t.Errorf("limiter did not bind: %d of %d attempts from ONE peer were allowed, "+
			"while the same peer was refused %d times on the control arm",
			variedAllowed, attempts, ctrlRefused)
	}
}

// TestTwoFAPerIPLimiterKeysOnPeerAddress does the same for the 2FA
// cross-account cap.
func TestTwoFAPerIPLimiterKeysOnPeerAddress(t *testing.T) {
	attempts := twoFAIPLockoutPerMinute * 4

	run := func(vary bool) (allowed, refused int) {
		lim := autologin.NewMemoryLimiter()
		defer lim.Stop()
		e := engineAsProductionBuildsIt()
		e.POST("/auth/2fa/verify", func(c *gin.Context) {
			ip := clientAddr(c)
			if ip.IsValid() {
				if ok, _ := lim.Allow(context.Background(), "2fa-ip:"+ip.String(), twoFAIPLockoutPerMinute); !ok {
					c.String(http.StatusTooManyRequests, "429")
					return
				}
			}
			c.String(http.StatusOK, "200")
		})
		srv := httptest.NewServer(e)
		defer srv.Close()

		client := srv.Client()
		for i := 0; i < attempts; i++ {
			fwd := ""
			if vary {
				fwd = fmt.Sprintf("198.51.100.%d", (i%254)+1)
			}
			code, _ := probeOnce(t, client, "POST", srv.URL+"/auth/2fa/verify", fwd)
			if code == http.StatusTooManyRequests {
				refused++
			} else {
				allowed++
			}
		}
		return allowed, refused
	}

	variedAllowed, variedRefused := run(true)
	ctrlAllowed, ctrlRefused := run(false)

	t.Logf("varied: allowed=%d refused=%d | control: allowed=%d refused=%d (cap=%d/min over %d attempts)",
		variedAllowed, variedRefused, ctrlAllowed, ctrlRefused, twoFAIPLockoutPerMinute, attempts)

	if ctrlRefused == 0 {
		t.Fatalf("CONTROL BROKEN: the limiter refused nothing on the control arm (allowed=%d of %d); "+
			"this test cannot detect a regression", ctrlAllowed, attempts)
	}
	if ctrlAllowed < twoFAIPLockoutPerMinute {
		t.Errorf("CONTROL OVER-FIRES: only %d of the first %d legitimate attempts were allowed",
			ctrlAllowed, twoFAIPLockoutPerMinute)
	}
	if variedRefused == 0 {
		t.Errorf("limiter did not bind: %d of %d attempts from ONE peer were allowed, "+
			"while the same peer was refused %d times on the control arm",
			variedAllowed, attempts, ctrlRefused)
	}
}
