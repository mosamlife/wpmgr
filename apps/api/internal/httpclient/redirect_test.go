package httpclient

import (
	"errors"
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

// TestDoOnce_MalformedLocationIsReturnedAsThe3xx: net/http parses Location
// before it consults CheckRedirect, so without the stash a malformed Location
// comes back as a transport error and the 3xx is lost. DoOnce must hand back
// the response itself, with Location exactly as sent and no stash header.
func TestDoOnce_MalformedLocationIsReturnedAsThe3xx(t *testing.T) {
	const malformed = "http://[::1"
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Location", malformed)
		w.Header().Set(stashedLocationHeader, "http://planted.example/")
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	t.Cleanup(srv.Close)

	c := New(Config{AllowPrivateNetworks: true})
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.DoOnce(req)
	if err != nil {
		t.Fatalf("DoOnce returned error %v, want the 301 response", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", resp.StatusCode)
	}
	if got := resp.Header.Values("Location"); len(got) != 1 || got[0] != malformed {
		t.Errorf("Location = %q, want exactly [%q]", got, malformed)
	}
	if _, ok := resp.Header[stashedLocationHeader]; ok {
		t.Errorf("stash header %q survived: %q", stashedLocationHeader, resp.Header.Values(stashedLocationHeader))
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("server received %d requests, want 1", n)
	}
}

// TestRefuseRedirect is the backstop policy on its own: it must hand the last
// response back rather than allow a follow.
func TestRefuseRedirect(t *testing.T) {
	if err := refuseRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("refuseRedirect = %v, want http.ErrUseLastResponse", err)
	}
}
