package agentcmd

// redirect_test.go: a signed command is sent once, to the saved address, and
// a 3xx answer comes back as a typed *RedirectError. Everything here runs
// through the real httpclient.New and a real Signer, because a fake Doer
// cannot show what the transport does with a redirect.
//
// The two servers are addressed as 127.0.0.1 and localhost, which resolve to
// loopback without any DNS configuration, so the "target received nothing"
// counts mean the same thing on every machine. The apex/www naming itself is
// covered by the pure newRedirectError table further down.

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
)

// agentLikeServer behaves like the agent's command route: POST only, a Bearer
// token required. It counts every request, whatever the method.
type agentLikeServer struct {
	srv  *httptest.Server
	hits atomic.Int64
}

func newAgentLikeServer(t *testing.T, tls bool) *agentLikeServer {
	t.Helper()
	a := &agentLikeServer{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.hits.Add(1)
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"rest_no_route"}`))
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	if tls {
		a.srv = httptest.NewTLSServer(h)
	} else {
		a.srv = httptest.NewServer(h)
	}
	t.Cleanup(a.srv.Close)
	return a
}

// redirectingServer answers every request with code and the given Location.
type redirectingServer struct {
	srv  *httptest.Server
	hits atomic.Int64
}

func newRedirectingServer(t *testing.T, tls bool, code int, location func() string) *redirectingServer {
	t.Helper()
	rs := &redirectingServer{}
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rs.hits.Add(1)
		if loc := location(); loc != "" {
			w.Header().Set("Location", loc)
		}
		w.WriteHeader(code)
	})
	if tls {
		rs.srv = httptest.NewTLSServer(h)
	} else {
		rs.srv = httptest.NewServer(h)
	}
	t.Cleanup(rs.srv.Close)
	return rs
}

// realCommandClient is the production client shape: httpclient.New plus a
// real Signer. InsecureSkipTLSVerify lets it reach the httptest TLS servers.
func realCommandClient(t *testing.T) *Client {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	hc := httpclient.New(httpclient.Config{
		AllowPrivateNetworks:  true, // test-only: loopback targets
		InsecureSkipTLSVerify: true, // test-only: httptest TLS servers
	})
	return NewClient(hc, &Signer{priv: priv})
}

// withHost swaps the host name of a loopback httptest URL, keeping its port.
func withHost(t *testing.T, raw, host string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	u.Host = host + ":" + u.Port()
	return u.String()
}

const backupRoute = "/wp-json/wpmgr/v1/command/backup"

// assertNotMisread fails when a redirect's error text could be read by the
// string classifiers of the canonical agent-reject format.
func assertNotMisread(t *testing.T, err error) {
	t.Helper()
	msg := err.Error()
	for _, bad := range []string{"status 404", "rejected by agent"} {
		if strings.Contains(msg, bad) {
			t.Errorf("error text %q contains %q", msg, bad)
		}
	}
	if code, ok := extractHTTPStatus(err); ok {
		t.Errorf("extractHTTPStatus read status %d from a redirect error %q", code, msg)
	}
}

func TestPostRaw_RefusesRedirectWithTypedError(t *testing.T) {
	statuses := []int{
		http.StatusMovedPermanently,  // 301
		http.StatusFound,             // 302
		http.StatusSeeOther,          // 303
		http.StatusTemporaryRedirect, // 307
		http.StatusPermanentRedirect, // 308
	}
	// Two host names for one loopback interface stand in for the two names a
	// site answers on. Each direction is a host change, as apex to www is.
	directions := []struct {
		name     string
		fromHost string
		toHost   string
	}{
		{"apex to www", "127.0.0.1", "localhost"},
		{"www to apex", "localhost", "127.0.0.1"},
	}
	for _, d := range directions {
		for _, code := range statuses {
			t.Run(fmt.Sprintf("%s/%d", d.name, code), func(t *testing.T) {
				target := newAgentLikeServer(t, false)
				targetBase := withHost(t, target.srv.URL, d.toHost)
				origin := newRedirectingServer(t, false, code, func() string { return targetBase + backupRoute })
				siteURL := withHost(t, origin.srv.URL, d.fromHost)

				_, err := realCommandClient(t).Backup(context.Background(), uuid.New(), siteURL, BackupRequest{})

				var re *RedirectError
				if !errors.As(err, &re) {
					t.Fatalf("want *RedirectError, got %T: %v", err, err)
				}
				if re.Status != code {
					t.Errorf("Status = %d, want %d", re.Status, code)
				}
				if want := targetBase + backupRoute; re.To != want {
					t.Errorf("To = %q, want %q", re.To, want)
				}
				// Another port is never an address the saved one may become.
				if re.SuggestedSiteURL != "" {
					t.Errorf("SuggestedSiteURL = %q, want empty for a target on another port", re.SuggestedSiteURL)
				}
				if re.From != siteURL+backupRoute {
					t.Errorf("From = %q, want %q", re.From, siteURL+backupRoute)
				}
				if re.Command != "backup" {
					t.Errorf("Command = %q, want backup", re.Command)
				}
				if n := target.hits.Load(); n != 0 {
					t.Errorf("redirect target received %d requests, want 0 (the command was re-sent)", n)
				}
				if n := origin.hits.Load(); n != 1 {
					t.Errorf("saved address received %d requests, want exactly 1", n)
				}
				assertNotMisread(t, err)
				if !strings.Contains(re.OperatorMessage("Backup"), targetBase) {
					t.Errorf("operator message %q does not name the target %q", re.OperatorMessage("Backup"), targetBase)
				}
			})
		}
	}

	t.Run("http to https on another port is not followed", func(t *testing.T) {
		target := newAgentLikeServer(t, true)
		origin := newRedirectingServer(t, false, http.StatusMovedPermanently, func() string { return target.srv.URL + backupRoute })

		_, err := realCommandClient(t).Backup(context.Background(), uuid.New(), origin.srv.URL, BackupRequest{})

		re, ok := AsRedirect(err)
		if !ok {
			t.Fatalf("want *RedirectError, got %T: %v", err, err)
		}
		if re.SuggestedSiteURL != "" {
			t.Errorf("SuggestedSiteURL = %q, want empty for an https address on another port", re.SuggestedSiteURL)
		}
		if n := target.hits.Load(); n != 0 {
			t.Errorf("https target received %d requests, want 0", n)
		}
		assertNotMisread(t, err)
	})

	t.Run("https to http downgrade is never suggested", func(t *testing.T) {
		target := newAgentLikeServer(t, false)
		origin := newRedirectingServer(t, true, http.StatusPermanentRedirect, func() string { return target.srv.URL + backupRoute })

		_, err := realCommandClient(t).Backup(context.Background(), uuid.New(), origin.srv.URL, BackupRequest{})

		re, ok := AsRedirect(err)
		if !ok {
			t.Fatalf("want *RedirectError, got %T: %v", err, err)
		}
		if !re.Downgrade() {
			t.Errorf("Downgrade() = false for %s -> %s", re.From, re.To)
		}
		if re.SuggestedSiteURL != "" {
			t.Errorf("SuggestedSiteURL = %q, want empty for a downgrade", re.SuggestedSiteURL)
		}
		if n := target.hits.Load(); n != 0 {
			t.Errorf("http target received %d requests, want 0", n)
		}
		if msg := re.OperatorMessage("Backup"); !strings.Contains(msg, "drops HTTPS") || strings.Contains(msg, "updates to") {
			t.Errorf("downgrade message %q should name HTTPS and not offer the downgraded address", msg)
		}
		assertNotMisread(t, err)
	})

	t.Run("foreign page is named but not suggested", func(t *testing.T) {
		target := newAgentLikeServer(t, false)
		foreign := withHost(t, target.srv.URL, "localhost") + "/login"
		origin := newRedirectingServer(t, false, http.StatusFound, func() string { return foreign })

		_, err := realCommandClient(t).Backup(context.Background(), uuid.New(), origin.srv.URL, BackupRequest{})

		re, ok := AsRedirect(err)
		if !ok {
			t.Fatalf("want *RedirectError, got %T: %v", err, err)
		}
		if re.To != foreign {
			t.Errorf("To = %q, want %q", re.To, foreign)
		}
		if re.SuggestedSiteURL != "" {
			t.Errorf("SuggestedSiteURL = %q, want empty when the target is not the command route", re.SuggestedSiteURL)
		}
		if msg := re.OperatorMessage("Backup"); !strings.Contains(msg, foreign) || !strings.Contains(msg, "/wp-json/wpmgr/") {
			t.Errorf("message %q should name %q and the route to exempt", msg, foreign)
		}
		if n := target.hits.Load(); n != 0 {
			t.Errorf("foreign target received %d requests, want 0", n)
		}
	})

	t.Run("location is sanitised", func(t *testing.T) {
		target := newAgentLikeServer(t, false)
		clean := withHost(t, target.srv.URL, "localhost")
		u, _ := url.Parse(clean)
		dirty := "http://admin:hunter2@" + u.Host + backupRoute + "?token=secret&x=1#frag"
		origin := newRedirectingServer(t, false, http.StatusMovedPermanently, func() string { return dirty })

		_, err := realCommandClient(t).Backup(context.Background(), uuid.New(), origin.srv.URL, BackupRequest{})

		re, ok := AsRedirect(err)
		if !ok {
			t.Fatalf("want *RedirectError, got %T: %v", err, err)
		}
		if want := clean + backupRoute; re.To != want {
			t.Errorf("To = %q, want %q", re.To, want)
		}
		for _, leaked := range []string{"admin", "hunter2", "token", "secret", "frag", "?", "#", "@"} {
			if strings.Contains(re.To, leaked) || strings.Contains(re.SuggestedSiteURL, leaked) ||
				strings.Contains(err.Error(), leaked) || strings.Contains(re.OperatorMessage("Backup"), leaked) {
				t.Errorf("%q survived sanitising: To=%q err=%q", leaked, re.To, err.Error())
			}
		}
		if n := target.hits.Load(); n != 0 {
			t.Errorf("target received %d requests, want 0", n)
		}
	})

	t.Run("relative location resolves against the saved address", func(t *testing.T) {
		origin := newRedirectingServer(t, false, http.StatusMovedPermanently, func() string { return "/blog" + backupRoute })

		_, err := realCommandClient(t).Backup(context.Background(), uuid.New(), origin.srv.URL, BackupRequest{})

		re, ok := AsRedirect(err)
		if !ok {
			t.Fatalf("want *RedirectError, got %T: %v", err, err)
		}
		if want := origin.srv.URL + "/blog" + backupRoute; re.To != want {
			t.Errorf("To = %q, want %q", re.To, want)
		}
		// Another path is never an address the saved one may become.
		if re.SuggestedSiteURL != "" {
			t.Errorf("SuggestedSiteURL = %q, want empty for another path", re.SuggestedSiteURL)
		}
		if n := origin.hits.Load(); n != 1 {
			t.Errorf("origin received %d requests, want exactly 1", n)
		}
	})

	t.Run("non-http location and missing location leave To empty", func(t *testing.T) {
		for _, loc := range []string{"javascript:alert(1)", "ftp://localhost/x", ""} {
			origin := newRedirectingServer(t, false, http.StatusFound, func() string { return loc })
			_, err := realCommandClient(t).Backup(context.Background(), uuid.New(), origin.srv.URL, BackupRequest{})
			re, ok := AsRedirect(err)
			if !ok {
				t.Fatalf("Location %q: want *RedirectError, got %T: %v", loc, err, err)
			}
			if re.To != "" || re.SuggestedSiteURL != "" {
				t.Errorf("Location %q: To=%q Suggested=%q, want both empty", loc, re.To, re.SuggestedSiteURL)
			}
			if msg := re.OperatorMessage("Backup"); !strings.Contains(msg, "names no usable address") {
				t.Errorf("Location %q: message %q", loc, msg)
			}
			assertNotMisread(t, err)
		}
	})
}

// TestNewRedirectError_ApexAndWww pins the naming contract for the real-world
// shape, apex to www and back, without needing those names to resolve.
func TestNewRedirectError_ApexAndWww(t *testing.T) {
	cases := []struct {
		name, from, location, wantTo, wantSuggested string
	}{
		{"apex to www", "https://example.com" + backupRoute, "https://www.example.com" + backupRoute,
			"https://www.example.com" + backupRoute, "https://www.example.com"},
		{"www to apex", "https://www.example.com" + backupRoute, "https://example.com" + backupRoute,
			"https://example.com" + backupRoute, "https://example.com"},
		{"http apex to https www", "http://example.com" + backupRoute, "https://www.example.com" + backupRoute + "/",
			"https://www.example.com" + backupRoute + "/", "https://www.example.com"},
		{"host is lowercased", "https://example.com" + backupRoute, "https://WWW.Example.COM" + backupRoute,
			"https://www.example.com" + backupRoute, "https://www.example.com"},
		{"another command's route is not a suggestion", "https://example.com" + backupRoute, "https://www.example.com/wp-json/wpmgr/v1/command/ping",
			"https://www.example.com/wp-json/wpmgr/v1/command/ping", ""},
		{"downgrade", "https://example.com" + backupRoute, "http://www.example.com" + backupRoute,
			"http://www.example.com" + backupRoute, ""},
		{"overlong target is cut to its origin", "https://example.com" + backupRoute, "https://www.example.com/" + strings.Repeat("a", 400),
			"https://www.example.com", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, tc.from, nil)
			resp := &http.Response{StatusCode: http.StatusMovedPermanently, Header: http.Header{"Location": {tc.location}}, Request: req}
			re := newRedirectError("backup", tc.from, resp)
			if re.To != tc.wantTo {
				t.Errorf("To = %q, want %q", re.To, tc.wantTo)
			}
			if re.SuggestedSiteURL != tc.wantSuggested {
				t.Errorf("SuggestedSiteURL = %q, want %q", re.SuggestedSiteURL, tc.wantSuggested)
			}
			if len(re.To) > maxRedirectTargetLen {
				t.Errorf("To is %d bytes, cap is %d", len(re.To), maxRedirectTargetLen)
			}
			assertNotMisread(t, re)
		})
	}

	// The operator copy for the reporter's case, in full.
	req, _ := http.NewRequest(http.MethodPost, "https://example.com"+backupRoute, nil)
	re := newRedirectError("backup", "https://example.com"+backupRoute, &http.Response{
		StatusCode: http.StatusMovedPermanently,
		Header:     http.Header{"Location": {"https://www.example.com" + backupRoute}},
		Request:    req,
	})
	want := "Backup not started. https://example.com redirects to https://www.example.com, so no command was sent. If WordPress on the site reports https://www.example.com as its address, the saved address updates to https://www.example.com automatically at a later check-in from the site. That update does not happen while another site in this workspace uses https://www.example.com: if one does, remove or change the duplicate site."
	if got := re.OperatorMessage("Backup"); got != want {
		t.Errorf("OperatorMessage:\n got %q\nwant %q", got, want)
	}
	if got := re.SavedSiteURL(); got != "https://example.com" {
		t.Errorf("SavedSiteURL = %q", got)
	}
}
