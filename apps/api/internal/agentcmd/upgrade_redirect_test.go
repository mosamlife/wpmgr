package agentcmd

// upgrade_redirect_test.go: the one redirect a signed command follows, a
// same-host http to https upgrade, proved through the real httpclient.New and
// a real Signer against one port that speaks both http and https, so "same
// host, same port" is literally true.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
)

// seenRequest is what the dual-protocol handler records per request.
type seenRequest struct {
	TLS    bool
	Method string
	Path   string
	Query  string
	Auth   string
	Body   []byte
}

// dualServer serves http and https on one 127.0.0.1 port.
type dualServer struct {
	Port string
	mu   sync.Mutex
	seen []seenRequest
}

func (d *dualServer) requests() []seenRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]seenRequest(nil), d.seen...)
}

// peekConn replays the byte peeked to choose the protocol.
type peekConn struct {
	net.Conn
	r *bufio.Reader
}

func (c peekConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// dualListener hands http.Server a TLS server conn when the first byte is a
// TLS handshake record (0x16), and the plain conn otherwise.
type dualListener struct {
	net.Listener
	cfg *tls.Config
}

func (l dualListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	r := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, err := r.Peek(1)
	_ = c.SetReadDeadline(time.Time{})
	pc := peekConn{Conn: c, r: r}
	if err == nil && b[0] == 0x16 {
		return tls.Server(pc, l.cfg), nil
	}
	return pc, nil
}

// newDualServer starts the server. handle answers each request after it is
// recorded; n is the 1-based request number.
func newDualServer(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, n int, port string)) *dualServer {
	t.Helper()
	// A started-then-closed httptest TLS server supplies a working cert.
	tmp := httptest.NewTLSServer(http.NotFoundHandler())
	cfg := tmp.TLS.Clone()
	tmp.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	d := &dualServer{Port: port}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			d.mu.Lock()
			d.seen = append(d.seen, seenRequest{
				TLS: r.TLS != nil, Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
				Auth: r.Header.Get("Authorization"), Body: body,
			})
			n := len(d.seen)
			d.mu.Unlock()
			handle(w, r, n, port)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(dualListener{Listener: ln, cfg: cfg}) }()
	t.Cleanup(func() { _ = srv.Close() })
	return d
}

// newRealSignerClient builds the production client shape with a Signer made
// by NewSigner, and returns the public key the agent would verify with.
func newRealSignerClient(t *testing.T) (*Client, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := NewSigner(base64.StdEncoding.EncodeToString(priv))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	hc := httpclient.New(httpclient.Config{AllowPrivateNetworks: true, InsecureSkipTLSVerify: true})
	return NewClient(hc, signer), pub
}

const pingRoute = "/wp-json/wpmgr/v1/command/ping"

func TestPostRaw_FollowsOneSameHostHTTPSUpgrade(t *testing.T) {
	for _, code := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			d := newDualServer(t, func(w http.ResponseWriter, r *http.Request, n int, port string) {
				if r.TLS == nil {
					w.Header().Set("Location", "https://127.0.0.1:"+port+r.URL.Path)
					w.WriteHeader(code)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true,"agent_version":"9.9.9"}`))
			})
			c, pub := newRealSignerClient(t)
			siteID := uuid.New()

			out, err := c.Ping(context.Background(), siteID, "http://127.0.0.1:"+d.Port)
			if err != nil {
				t.Fatalf("Ping: %v", err)
			}
			if !out.OK || out.AgentVersion != "9.9.9" {
				t.Errorf("decoded %+v, want the https hop's answer", out)
			}
			seen := d.requests()
			if len(seen) != 2 {
				t.Fatalf("server saw %d requests, want exactly 2", len(seen))
			}
			first, second := seen[0], seen[1]
			if first.TLS || !second.TLS {
				t.Errorf("TLS per hop = %v, %v; want plain then TLS", first.TLS, second.TLS)
			}
			if second.Method != http.MethodPost || second.Path != pingRoute {
				t.Errorf("retry = %s %s, want POST %s", second.Method, second.Path, pingRoute)
			}
			if !bytes.Equal(first.Body, second.Body) {
				t.Errorf("retry body %q differs from the first %q", second.Body, first.Body)
			}
			if first.Auth == "" || first.Auth != second.Auth {
				t.Errorf("Authorization differs between hops (the token was re-minted or dropped)")
			}
			token := strings.TrimPrefix(second.Auth, "Bearer ")
			verifyLikeAgent(t, token, pub, time.Now(), siteID.String(), "ping")
		})
	}
}

func TestPostRaw_UpgradeRetryIsNeverFollowed(t *testing.T) {
	d := newDualServer(t, func(w http.ResponseWriter, r *http.Request, n int, port string) {
		if r.TLS == nil {
			w.Header().Set("Location", "https://127.0.0.1:"+port+r.URL.Path)
		} else {
			w.Header().Set("Location", "https://127.0.0.1:"+port+"/elsewhere"+r.URL.Path)
		}
		w.WriteHeader(http.StatusPermanentRedirect)
	})
	c, _ := newRealSignerClient(t)

	_, err := c.Ping(context.Background(), uuid.New(), "http://127.0.0.1:"+d.Port)

	re, ok := AsRedirect(err)
	if !ok {
		t.Fatalf("want *RedirectError, got %T: %v", err, err)
	}
	if want := "https://127.0.0.1:" + d.Port + "/elsewhere" + pingRoute; re.To != want {
		t.Errorf("To = %q, want %q (resolved against the https hop)", re.To, want)
	}
	if want := "http://127.0.0.1:" + d.Port + pingRoute; re.From != want {
		t.Errorf("From = %q, want the saved address %q", re.From, want)
	}
	if n := len(d.requests()); n != 2 {
		t.Errorf("server saw %d requests, want exactly 2", n)
	}
}

func TestPostRaw_UpgradeRefusals(t *testing.T) {
	type row struct {
		name  string
		https bool // the saved address is https
		loc   func(port, path string) string
	}
	rows := []row{
		{"another host name", false, func(p, path string) string { return "https://localhost:" + p + path }},
		{"another port", false, func(p, path string) string { return "https://127.0.0.1:1" + path }},
		{"another path", false, func(p, path string) string { return "https://127.0.0.1:" + p + "/blog" + path }},
		{"an added query", false, func(p, path string) string { return "https://127.0.0.1:" + p + path + "?x=1" }},
		{"userinfo", false, func(p, path string) string { return "https://u:p@127.0.0.1:" + p + path }},
		{"a downgrade from a saved https address", true, func(p, path string) string { return "http://127.0.0.1:" + p + path }},
	}
	for _, rw := range rows {
		t.Run(rw.name, func(t *testing.T) {
			d := newDualServer(t, func(w http.ResponseWriter, r *http.Request, _ int, port string) {
				w.Header().Set("Location", rw.loc(port, r.URL.Path))
				w.WriteHeader(http.StatusMovedPermanently)
			})
			c, _ := newRealSignerClient(t)
			scheme := "http"
			if rw.https {
				scheme = "https"
			}
			_, err := c.Ping(context.Background(), uuid.New(), scheme+"://127.0.0.1:"+d.Port)
			if _, ok := AsRedirect(err); !ok {
				t.Fatalf("want *RedirectError, got %T: %v", err, err)
			}
			if n := len(d.requests()); n != 1 {
				t.Errorf("server saw %d requests, want exactly 1", n)
			}
		})
	}
}

func TestSameHostHTTPSUpgrade(t *testing.T) {
	cases := []struct {
		from, loc string
		ok        bool
	}{
		{"http://Example.com/p", "https://example.COM/p", true},
		{"http://example.com:80/p", "https://example.com/p", true},
		{"http://example.com/p", "https://example.com:443/p", true},
		{"http://example.com:8080/p", "https://example.com:8080/p", true},
		{"http://example.com:8080/p", "https://example.com/p", false},
		{"http://example.com/p", "https://example.com:8443/p", false},
		{"http://example.com:80/p", "https://example.com:80/p", false},
		{"http://example.com/p", "https://www.example.com/p", false},
		{"http://example.com/p", "https://example.com/q", false},
		{"http://example.com/p", "https://example.com/p?x=1", false},
		{"http://example.com/p", "https://user@example.com/p", false},
		{"http://example.com/p", "https://example.com/p#f", false},
		{"http://example.com/p", "http://example.com/p", false},
		{"https://example.com/p", "https://example.com/p", false},
		{"https://example.com/p", "http://example.com/p", false},
		{"http://example.com/p", "", false},
		{"http://example.com/p", "http://[::1", false},
	}
	for _, c := range cases {
		from, err := url.Parse(c.from)
		if err != nil {
			t.Fatal(err)
		}
		_, ok := sameHostHTTPSUpgrade(from, c.loc)
		if ok != c.ok {
			t.Errorf("sameHostHTTPSUpgrade(%q, %q) = %v, want %v", c.from, c.loc, ok, c.ok)
		}
	}
}

// TestPostRaw_MalformedLocationIsARedirect: a Location that does not parse
// comes back as a typed RedirectError with no target, never as a transport
// error (which the job layer would retry).
func TestPostRaw_MalformedLocationIsARedirect(t *testing.T) {
	origin := newRedirectingServer(t, false, http.StatusMovedPermanently, func() string { return "http://[::1" })
	_, err := realCommandClient(t).Backup(context.Background(), uuid.New(), origin.srv.URL, BackupRequest{})
	var re *RedirectError
	if !errors.As(err, &re) {
		t.Fatalf("want *RedirectError, got %T: %v", err, err)
	}
	if re.To != "" || re.SuggestedSiteURL != "" {
		t.Errorf("To=%q Suggested=%q, want both empty", re.To, re.SuggestedSiteURL)
	}
	if n := origin.hits.Load(); n != 1 {
		t.Errorf("origin received %d requests, want 1", n)
	}
}
