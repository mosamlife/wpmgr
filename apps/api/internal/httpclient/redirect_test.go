package httpclient

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// redirectPair is an origin that answers every request with code and a
// Location pointing at target, plus a target that counts what reaches it.
func redirectPair(t *testing.T, code int) (origin *httptest.Server, targetHits *atomic.Int64) {
	t.Helper()
	targetHits = &atomic.Int64{}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		_, _ = w.Write([]byte("target"))
	}))
	t.Cleanup(target.Close)
	origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/landed", code)
	}))
	t.Cleanup(origin.Close)
	return origin, targetHits
}

func loopbackClient() *Client { return New(Config{AllowPrivateNetworks: true}) }

// TestDoOnce_DoesNotFollowRedirect: DoOnce hands the 3xx back with its
// Location and sends nothing to the target, for every redirect status.
func TestDoOnce_DoesNotFollowRedirect(t *testing.T) {
	for _, code := range []int{301, 302, 303, 307, 308} {
		origin, targetHits := redirectPair(t, code)
		req, _ := http.NewRequest(http.MethodPost, origin.URL+"/cmd", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer x")
		resp, err := loopbackClient().DoOnce(req)
		if err != nil {
			t.Fatalf("%d: DoOnce error: %v", code, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != code {
			t.Errorf("%d: DoOnce returned status %d, want the redirect itself", code, resp.StatusCode)
		}
		if !strings.HasSuffix(resp.Header.Get("Location"), "/landed") {
			t.Errorf("%d: Location = %q, want it preserved", code, resp.Header.Get("Location"))
		}
		if n := targetHits.Load(); n != 0 {
			t.Errorf("%d: redirect target received %d requests through DoOnce, want 0", code, n)
		}
	}
}

// TestDo_StillFollowsRedirect is the over-fire guard: Do and HTTPClient, which
// the uptime probe, the app probe and the cron kick use, keep following
// redirects exactly as before.
func TestDo_StillFollowsRedirect(t *testing.T) {
	for _, code := range []int{301, 302, 307, 308} {
		origin, targetHits := redirectPair(t, code)
		c := loopbackClient()

		req, _ := http.NewRequest(http.MethodGet, origin.URL+"/", nil)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("%d: Do error: %v", code, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "target" {
			t.Errorf("%d: Do ended at status %d body %q, want the target's 200", code, resp.StatusCode, body)
		}

		req2, _ := http.NewRequest(http.MethodGet, origin.URL+"/", nil)
		resp2, err := c.HTTPClient().Do(req2)
		if err != nil {
			t.Fatalf("%d: HTTPClient().Do error: %v", code, err)
		}
		_ = resp2.Body.Close()
		if resp2.StatusCode != http.StatusOK {
			t.Errorf("%d: HTTPClient().Do ended at status %d, want the target's 200", code, resp2.StatusCode)
		}
		if n := targetHits.Load(); n != 2 {
			t.Errorf("%d: target received %d requests, want 2 (one via Do, one via HTTPClient)", code, n)
		}
	}
}
