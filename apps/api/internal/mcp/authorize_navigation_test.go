package mcp

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// browserNavigationAccept is the Accept header a browser sends on a top-level
// navigation.
const browserNavigationAccept = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"

// navigationProbeQuery is a raw authorize query in the order and encoding a
// client sends it. Nothing below may reorder or re-encode it.
const navigationProbeQuery = "response_type=code&client_id=c1&state=a~b-_c&scope=mcp%3Aread%20mcp%3Acache"

// reachedAuthorize is what the stand-in handler answers, so a request the
// middleware let through is recognisable by its body.
const reachedAuthorize = `{"reached":"authorize"}`

// navigationEngine mounts the middleware as server.New does, on the root
// engine ahead of every route, with a stand-in for Handler.authorize on
// AuthorizePath and a POST on the same path for the method check.
func navigationEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.Use(AuthorizeNavigationRedirect())
	answer := func(c *gin.Context) {
		c.Data(http.StatusOK, "application/json; charset=utf-8", []byte(reachedAuthorize))
	}
	e.GET(AuthorizePath, answer)
	e.POST(AuthorizePath, answer)
	e.GET(AuthorizePath+"/x", answer)
	e.GET(ConsentPath, answer)
	return e
}

func serveNavigation(e *gin.Engine, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

// varyNames is every field name the response's Vary header lists, lower-cased.
func varyNames(h http.Header) []string {
	var out []string
	for _, v := range h.Values("Vary") {
		for _, name := range strings.Split(v, ",") {
			if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

// requireNavigationVary fails unless the response says it varies on every
// header the decision reads. Without it a cache may hand the JSON answer to a
// browser navigation, or the redirect to the screen's own fetch.
func requireNavigationVary(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	got := varyNames(w.Header())
	for _, want := range []string{"accept", "sec-fetch-mode", "sec-fetch-dest"} {
		if !slices.Contains(got, want) {
			t.Errorf("Vary = %q, want it to name %s", w.Header().Values("Vary"), want)
		}
	}
}

// TestAuthorizeNavigationRedirect_Decision pins which requests on the authorize
// path are browser navigations. Sec-Fetch-Mode decides whenever it is present;
// without it a document destination does; with no Fetch Metadata at all only an
// Accept that lists text/html does. Everything else is the JSON the consent
// screen fetches.
func TestAuthorizeNavigationRedirect_Decision(t *testing.T) {
	cases := []struct {
		name     string
		headers  map[string]string
		redirect bool
	}{
		{"browser navigation", map[string]string{
			"Accept": browserNavigationAccept, "Sec-Fetch-Mode": "navigate",
			"Sec-Fetch-Dest": "document", "Sec-Fetch-Site": "none"}, true},
		{"navigate mode alone", map[string]string{"Sec-Fetch-Mode": "navigate"}, true},
		{"navigate mode decides over a JSON accept", map[string]string{
			"Accept": "application/json", "Sec-Fetch-Mode": "navigate"}, true},
		{"the consent screen's own fetch", map[string]string{
			"Accept": "application/json", "Sec-Fetch-Mode": "cors",
			"Sec-Fetch-Dest": "empty", "Sec-Fetch-Site": "same-origin"}, false},
		{"cors mode decides over an html accept", map[string]string{
			"Accept": browserNavigationAccept, "Sec-Fetch-Mode": "cors"}, false},
		{"no-cors mode decides over an html accept", map[string]string{
			"Accept": browserNavigationAccept, "Sec-Fetch-Mode": "no-cors"}, false},
		{"same-origin mode decides over an html accept", map[string]string{
			"Accept": browserNavigationAccept, "Sec-Fetch-Mode": "same-origin"}, false},
		{"cors mode decides over a document destination", map[string]string{
			"Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "document"}, false},
		{"document destination without a mode", map[string]string{"Sec-Fetch-Dest": "document"}, true},
		{"html accept without fetch metadata", map[string]string{"Accept": browserNavigationAccept}, true},
		{"bare text/html accept", map[string]string{"Accept": "text/html"}, true},
		{"text/html in upper case", map[string]string{"Accept": "TEXT/HTML"}, true},
		{"text/html refused with q=0", map[string]string{"Accept": "text/html;q=0, application/json"}, false},
		{"no headers at all", map[string]string{}, false},
		{"wildcard accept", map[string]string{"Accept": "*/*"}, false},
		{"json accept", map[string]string{"Accept": "application/json"}, false},
		{"text wildcard accept", map[string]string{"Accept": "text/*"}, false},
		{"xhtml without text/html", map[string]string{"Accept": "application/xhtml+xml"}, false},
	}
	e := navigationEngine(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := serveNavigation(e, http.MethodGet, AuthorizePath+"?"+navigationProbeQuery, tc.headers)
			requireNavigationVary(t, w)
			if !tc.redirect {
				if w.Code != http.StatusOK || w.Body.String() != reachedAuthorize {
					t.Fatalf("got %d %q (Location %q); want the request to reach the authorize handler",
						w.Code, w.Body.String(), w.Header().Get("Location"))
				}
				return
			}
			if w.Code != http.StatusSeeOther {
				t.Fatalf("status = %d, want 303 See Other; body %q", w.Code, w.Body.String())
			}
			if got, want := w.Header().Get("Location"), ConsentScreenPath+"?"+navigationProbeQuery; got != want {
				t.Fatalf("Location = %q, want %q byte for byte", got, want)
			}
			if got := w.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			if strings.Contains(w.Body.String(), "reached") {
				t.Error("a redirected navigation still ran the authorize handler")
			}
		})
	}
}

// A navigation with no query lands on the bare screen path, never on a
// dangling "?".
func TestAuthorizeNavigationRedirect_EmptyQuery(t *testing.T) {
	w := serveNavigation(navigationEngine(t), http.MethodGet, AuthorizePath,
		map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	if got := w.Header().Get("Location"); got != ConsentScreenPath {
		t.Fatalf("Location = %q, want %q", got, ConsentScreenPath)
	}
}

// Only a GET on exactly AuthorizePath is ever redirected. A longer path, the
// same path under another method, and a sibling path all reach their own
// handler with no Vary added.
func TestAuthorizeNavigationRedirect_OnlyGETOnTheExactPath(t *testing.T) {
	browser := map[string]string{
		"Accept": browserNavigationAccept, "Sec-Fetch-Mode": "navigate",
		"Sec-Fetch-Dest": "document", "Sec-Fetch-Site": "none"}
	e := navigationEngine(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, AuthorizePath},
		{http.MethodGet, AuthorizePath + "/x"},
		{http.MethodGet, ConsentPath},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			w := serveNavigation(e, tc.method, tc.path+"?"+navigationProbeQuery, browser)
			if w.Code != http.StatusOK || w.Body.String() != reachedAuthorize {
				t.Fatalf("got %d (Location %q); want the route's own handler", w.Code, w.Header().Get("Location"))
			}
			if v := w.Header().Values("Vary"); len(v) != 0 {
				t.Errorf("Vary = %q on a request the middleware does not decide", v)
			}
		})
	}
}
