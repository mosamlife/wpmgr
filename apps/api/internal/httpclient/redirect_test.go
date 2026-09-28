package httpclient

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

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

// TestDoOnce_ValidLocationIsNotFollowed: a well-formed Location is returned
// verbatim and nothing is sent to it.
func TestDoOnce_ValidLocationIsNotFollowed(t *testing.T) {
	var targetHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
	}))
	t.Cleanup(target.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL+"/y")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)

	c := New(Config{AllowPrivateNetworks: true})
	req, _ := http.NewRequest(http.MethodPost, origin.URL+"/x", nil)
	resp, err := c.DoOnce(req)
	if err != nil {
		t.Fatalf("DoOnce: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || resp.Header.Get("Location") != target.URL+"/y" {
		t.Errorf("got %d Location=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if n := targetHits.Load(); n != 0 {
		t.Errorf("redirect target received %d requests, want 0", n)
	}
}

// TestRefuseRedirect is the backstop policy on its own: it must hand the last
// response back rather than allow a follow.
func TestRefuseRedirect(t *testing.T) {
	if err := refuseRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("refuseRedirect = %v, want http.ErrUseLastResponse", err)
	}
}
