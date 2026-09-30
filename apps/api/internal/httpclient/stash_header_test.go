package httpclient

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDoOnce_ServerSentStashHeaderIsNeverALocation: DoOnce keeps a 3xx
// Location under an internal header while the response passes through
// http.Client. A server that sends that header itself must not get its value
// handed back as the Location, whatever the status and whether or not it also
// sends a real Location.
func TestDoOnce_ServerSentStashHeaderIsNeverALocation(t *testing.T) {
	const planted = "https://attacker.test/planted"
	cases := []struct {
		name     string
		status   int
		location string // "" sends none
		want     string // the Location the caller must see
	}{
		{"3xx with no Location", http.StatusFound, "", ""},
		{"3xx with a Location", http.StatusMovedPermanently, "https://example.test/real", "https://example.test/real"},
		{"2xx", http.StatusOK, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(stashedLocationHeader, planted)
				if c.location != "" {
					w.Header().Set("Location", c.location)
				}
				w.WriteHeader(c.status)
			}))
			t.Cleanup(srv.Close)

			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/cmd", strings.NewReader(`{}`))
			resp, err := loopbackClient().DoOnce(req)
			if err != nil {
				t.Fatalf("DoOnce: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != c.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, c.status)
			}
			if got := resp.Header.Get("Location"); got != c.want {
				t.Errorf("Location = %q, want %q", got, c.want)
			}
			for _, v := range resp.Header.Values("Location") {
				if v == planted {
					t.Errorf("the server-sent %s value reached the caller as a Location", stashedLocationHeader)
				}
			}
			if isRedirectStatus(c.status) && resp.Header.Get(stashedLocationHeader) != "" {
				t.Errorf("%s survived on a 3xx: %q", stashedLocationHeader, resp.Header.Get(stashedLocationHeader))
			}
		})
	}
}
