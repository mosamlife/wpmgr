package auth

// Regression pins for the address the authentication rate limiters key on.
//
// Two auth limiters make a DECISION from the caller's address rather than
// merely recording it:
//
//   - reset.go  "pwreset-consume:"+ip  (resetPerMinute)          — the only
//     rate limit on password-reset token guessing.
//   - twofa.go  "2fa-ip:"+ip           (twoFAIPLockoutPerMinute) — the
//     cross-account cap, whose stated purpose (twofa.go:51-54) is to stop a
//     single host cycling through accounts.
//
// Both key on clientAddr(), which is gin's c.ClientIP(). An engine built the
// way server.New builds it resolves that from a request header, so the value
// is chosen by the caller and each request can land in a fresh bucket. The
// sibling keys "pwreset:"+email and "2fa-user:"+userID are NOT address-keyed
// and still bound a single-account attack; what these tests pin is the
// cross-account limiter.
//
// THESE TESTS ARE EXPECTED TO FAIL until the remedy lands. That is deliberate:
// their absence is what let this ship. Each spoofed arm is paired with an
// honest control in the same run, so a limiter that stops refusing altogether
// fails the control rather than passing as a "fix".
//
// LOCKSTEP: engineAsProductionBuildsIt below mirrors server.New's engine
// construction. If the remedy is applied there (e.g. by declaring which
// proxies are trusted), this helper must be updated in the same commit or
// these pins keep testing an engine production no longer builds.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/autologin"
)

// engineAsProductionBuildsIt mirrors server.New: gin.New(), with no declaration
// of which proxies are trusted. See LOCKSTEP above.
func engineAsProductionBuildsIt() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	return gin.New()
}

// probe issues one request and returns the response body.
func probe(t *testing.T, client *http.Client, method, url, xff string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
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

// TestClientAddrIgnoresCallerSuppliedForwardedFor is the root-cause pin: the
// address the limiters key on must not be one the caller wrote into the
// request. Every request here comes from the same real TCP peer.
func TestClientAddrIgnoresCallerSuppliedForwardedFor(t *testing.T) {
	e := engineAsProductionBuildsIt()
	e.GET("/probe", func(c *gin.Context) {
		c.String(http.StatusOK, "%s", clientAddr(c).String())
	})
	srv := httptest.NewServer(e)
	defer srv.Close()

	// Establish the honest baseline: what the same peer resolves to with no
	// header at all. This is the value every spoofed request must also yield.
	_, baseline := probe(t, srv.Client(), "GET", srv.URL+"/probe", "")
	if baseline == "" {
		t.Fatal("baseline clientAddr is empty; the probe is not measuring anything")
	}
	t.Logf("honest baseline (no X-Forwarded-For): clientAddr=%s", baseline)

	for _, xff := range []string{
		"203.0.113.9",
		"198.51.100.7, 10.0.0.1",
		"1.2.3.4, 203.0.113.9, 10.0.0.1",
	} {
		_, got := probe(t, srv.Client(), "GET", srv.URL+"/probe", xff)
		if got != baseline {
			t.Errorf("X-Forwarded-For %q changed the limiter address to %s (baseline %s): "+
				"the caller chooses its own rate-limit bucket", xff, got, baseline)
		}
	}
}

// TestPasswordResetConsumeLimiterResistsForwardedForSpoofing drives the real
// production limiter with the real key format and constant from reset.go,
// from one TCP peer, past the point where the honest control is refused.
func TestPasswordResetConsumeLimiterResistsForwardedForSpoofing(t *testing.T) {
	// 4x the limit: far enough past the cap that a working limiter must refuse.
	attempts := resetPerMinute * 4

	run := func(spoof bool) (allowed, refused int) {
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
			xff := ""
			if spoof {
				xff = fmt.Sprintf("203.0.113.%d", i+1)
			}
			code, _ := probe(t, client, "POST", srv.URL+"/auth/password/reset", xff)
			if code == http.StatusTooManyRequests {
				refused++
			} else {
				allowed++
			}
		}
		return allowed, refused
	}

	spoofAllowed, spoofRefused := run(true)
	ctrlAllowed, ctrlRefused := run(false)

	t.Logf("spoofed: allowed=%d refused=%d | control: allowed=%d refused=%d (cap=%d/min over %d attempts)",
		spoofAllowed, spoofRefused, ctrlAllowed, ctrlRefused, resetPerMinute, attempts)

	// Control first: a limiter that refuses nothing would make the spoof
	// assertion below pass for the wrong reason.
	if ctrlRefused == 0 {
		t.Fatalf("CONTROL BROKEN: the limiter refused nothing without spoofing (allowed=%d of %d); "+
			"this test cannot detect a bypass", ctrlAllowed, attempts)
	}
	// And the control must not over-fire: the first resetPerMinute attempts
	// are legitimate and must be served.
	if ctrlAllowed < resetPerMinute {
		t.Errorf("CONTROL OVER-FIRES: only %d of the first %d legitimate attempts were allowed",
			ctrlAllowed, resetPerMinute)
	}
	if spoofRefused == 0 {
		t.Errorf("BYPASS: %d of %d reset-token attempts from ONE peer were allowed by varying "+
			"X-Forwarded-For (honest control was refused %d times); token guessing is unbounded",
			spoofAllowed, attempts, ctrlRefused)
	}
}

// TestTwoFAPerIPLimiterResistsForwardedForSpoofing does the same for the 2FA
// cross-account cap.
func TestTwoFAPerIPLimiterResistsForwardedForSpoofing(t *testing.T) {
	attempts := twoFAIPLockoutPerMinute * 4

	run := func(spoof bool) (allowed, refused int) {
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
			xff := ""
			if spoof {
				xff = fmt.Sprintf("198.51.100.%d", (i%254)+1)
			}
			code, _ := probe(t, client, "POST", srv.URL+"/auth/2fa/verify", xff)
			if code == http.StatusTooManyRequests {
				refused++
			} else {
				allowed++
			}
		}
		return allowed, refused
	}

	spoofAllowed, spoofRefused := run(true)
	ctrlAllowed, ctrlRefused := run(false)

	t.Logf("spoofed: allowed=%d refused=%d | control: allowed=%d refused=%d (cap=%d/min over %d attempts)",
		spoofAllowed, spoofRefused, ctrlAllowed, ctrlRefused, twoFAIPLockoutPerMinute, attempts)

	if ctrlRefused == 0 {
		t.Fatalf("CONTROL BROKEN: the limiter refused nothing without spoofing (allowed=%d of %d); "+
			"this test cannot detect a bypass", ctrlAllowed, attempts)
	}
	if ctrlAllowed < twoFAIPLockoutPerMinute {
		t.Errorf("CONTROL OVER-FIRES: only %d of the first %d legitimate attempts were allowed",
			ctrlAllowed, twoFAIPLockoutPerMinute)
	}
	if spoofRefused == 0 {
		t.Errorf("BYPASS: %d of %d 2FA attempts from ONE peer were allowed by varying "+
			"X-Forwarded-For (honest control was refused %d times); the cross-account cap "+
			"described at twofa.go:51-54 does not bind", spoofAllowed, attempts, ctrlRefused)
	}
}
