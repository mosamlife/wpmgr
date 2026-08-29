package auth

// Regression pins for the address the authentication rate limiters key on.
//
// Two auth limiters make a DECISION from the requesting address:
//
//   - reset.go  "pwreset-consume:"+ip  (resetPerMinute)
//   - twofa.go  "2fa-ip:"+ip           (twoFAIPLockoutPerMinute)
//
// Both must key on limiterAddr, which selects the entry the infrastructure
// appended (see proxyHops in handler.go), not on clientAddr, which resolves the
// leftmost forwarded entry and is therefore chosen by the caller.
//
// Coverage is deliberately in three layers, because the first two each miss
// something the other catches:
//
//  1. limiterAddr's own behaviour — chain shapes, duplicate header lines, IPv6.
//  2. The registered route — TestResetPasswordRouteKeysOnAppendedClient drives
//     the real gin route through h.Register into the real Service and the real
//     limiter, so the wiring is covered and not just the helper.
//  3. TestDecisionSitesUseLimiterAddr parses the source and pins that all four
//     decision sites call limiterAddr. Layer 2 cannot reach the three 2FA sites
//     without a database (their limiter sits behind a challenge lookup), so
//     without this a revert of those three would be silent.
//
// On over-firing: a single-client arm CANNOT detect a remedy that keys every
// caller onto one shared value — such a remedy still refuses past the cap and
// still serves below it, so every single-client pin here passes while the whole
// fleet is locked out. Only TestResetRouteSeparatesDistinctClients catches it,
// because catching it requires two distinct legitimate clients in one arm. Do
// not read the controls in the other tests as over-fire protection; they are
// not, and they were once wrongly described that way.

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/autologin"
)

const (
	// simulatedClient is the address the balancer observes and appends. It is
	// the value every limiter key must reduce to.
	simulatedClient = "203.0.113.200"
	// simulatedOtherClient is a second, unrelated legitimate client. Used to
	// prove distinct callers get distinct buckets.
	simulatedOtherClient = "203.0.113.201"
	// simulatedBalancer is the balancer's own frontend address, appended last.
	simulatedBalancer = "192.0.2.1"
)

func engineAsProductionBuildsIt() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	return gin.New()
}

// handlerWithHops builds a Handler that resolves addresses for a deployment
// with n appending proxies.
func handlerWithHops(n int) *Handler {
	h := &Handler{}
	h.SetProxyHops(n)
	return h
}

// defaultHopsHandler matches the hosted topology (a load balancer appending the
// client address then its own), which is the default and what most pins here
// exercise. Deployments with a different shape are covered by
// TestLimiterAddrHonoursConfiguredHopCount.
var defaultHopsHandler = handlerWithHops(2)

// simulateBalancerFor builds the X-Forwarded-For chain as it arrives at the
// container: whatever the caller sent, then the observed client address, then
// the balancer's own.
func simulateBalancerFor(client, callerSupplied string) string {
	if callerSupplied == "" {
		return client + ", " + simulatedBalancer
	}
	return callerSupplied + ", " + client + ", " + simulatedBalancer
}

func simulateBalancer(callerSupplied string) string {
	return simulateBalancerFor(simulatedClient, callerSupplied)
}

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

// TestLimiterAddrSelectsTheAppendedClientEntry: whatever the caller prepends,
// the limiter address must reduce to the entry the balancer appended.
func TestLimiterAddrSelectsTheAppendedClientEntry(t *testing.T) {
	e := engineAsProductionBuildsIt()
	e.GET("/probe", func(c *gin.Context) {
		c.String(http.StatusOK, "%s", defaultHopsHandler.limiterAddr(c).String())
	})
	srv := httptest.NewServer(e)
	defer srv.Close()

	for _, callerSupplied := range []string{
		"",
		"203.0.113.9",
		"198.51.100.7, 10.0.0.1",
		"1.2.3.4, 203.0.113.9, 10.0.0.1",
		// A caller echoing the real shape back, to check the selection is
		// positional from the right rather than a value match.
		simulatedClient + ", " + simulatedBalancer,
		// A caller padding to exactly proxyHops, which is the shape that would
		// defeat the short-chain fallback if the fallback were load-bearing.
		"9.9.9.9, 9.9.9.10",
	} {
		_, got := probeOnce(t, srv.Client(), "GET", srv.URL+"/probe", simulateBalancer(callerSupplied))
		if got != simulatedClient {
			t.Errorf("caller-supplied prefix %q produced limiter address %s, want %s",
				callerSupplied, got, simulatedClient)
		}
	}
}

// TestLimiterAddrReadsEveryForwardedHeaderLine pins that a caller cannot hide
// the appended entries by sending the header more than once. Header.Get returns
// only the first line; the chain is every line joined.
func TestLimiterAddrReadsEveryForwardedHeaderLine(t *testing.T) {
	e := engineAsProductionBuildsIt()
	e.GET("/probe", func(c *gin.Context) {
		c.String(http.StatusOK, "%s", defaultHopsHandler.limiterAddr(c).String())
	})
	srv := httptest.NewServer(e)
	defer srv.Close()

	for _, callerLines := range [][]string{
		{"9.9.9.9"},
		{"9.9.9.9", "9.9.9.10"},
		{"9.9.9.9, 9.9.9.10"},
	} {
		req, err := http.NewRequest("GET", srv.URL+"/probe", nil)
		if err != nil {
			t.Fatal(err)
		}
		// Every caller line first, then the line the infrastructure appends.
		for _, l := range callerLines {
			req.Header.Add("X-Forwarded-For", l)
		}
		req.Header.Add("X-Forwarded-For", simulatedClient+", "+simulatedBalancer)

		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 128)
		n, _ := resp.Body.Read(buf)
		resp.Body.Close()
		if got := string(buf[:n]); got != simulatedClient {
			t.Errorf("caller lines %q produced limiter address %s, want %s; "+
				"only the first header line was read", callerLines, got, simulatedClient)
		}
	}
}

// TestLimiterAddrHandlesIPv6Forms pins the v6 spellings. There is a v6
// forwarding rule in front of this service, so v6 is real traffic; if these
// fell back to the peer address every v6 client would share one bucket.
func TestLimiterAddrHandlesIPv6Forms(t *testing.T) {
	e := engineAsProductionBuildsIt()
	e.GET("/probe", func(c *gin.Context) {
		c.String(http.StatusOK, "%s", defaultHopsHandler.limiterAddr(c).String())
	})
	srv := httptest.NewServer(e)
	defer srv.Close()

	cases := []struct{ entry, want string }{
		{"2001:db8::1", "2001:db8::1"},
		{"[2001:db8::1]", "2001:db8::1"},
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"198.51.100.7:443", "198.51.100.7"},
		{" 2001:db8::2 ", "2001:db8::2"},
	}
	for _, tc := range cases {
		fwd := "9.9.9.9, " + tc.entry + ", " + simulatedBalancer
		_, got := probeOnce(t, srv.Client(), "GET", srv.URL+"/probe", fwd)
		if got != tc.want {
			t.Errorf("appended entry %q produced limiter address %s, want %s", tc.entry, got, tc.want)
		}
	}
}

// TestLimiterAddrFallsBackToPeerOnShortChain pins that an honest short chain
// resolves from the connection rather than from caller data. This is a
// correctness pin, not a security guard — see the note on limiterAddr.
func TestLimiterAddrFallsBackToPeerOnShortChain(t *testing.T) {
	e := engineAsProductionBuildsIt()
	e.GET("/probe", func(c *gin.Context) {
		c.String(http.StatusOK, "%s|%s", defaultHopsHandler.limiterAddr(c).String(), c.RemoteIP())
	})
	srv := httptest.NewServer(e)
	defer srv.Close()

	for _, fwd := range []string{"", "198.51.100.5"} {
		_, got := probeOnce(t, srv.Client(), "GET", srv.URL+"/probe", fwd)
		addr, peer, ok := strings.Cut(got, "|")
		if !ok {
			t.Fatalf("malformed probe response %q", got)
		}
		if addr != peer {
			t.Errorf("chain %q gave limiter address %s, want the peer address %s", fwd, addr, peer)
		}
	}
}

// TestLimiterAddrHonoursConfiguredHopCount is the pin that the positional rule
// is not hard-wired to one deployment.
//
// Before the hop count was configurable this selection was fixed at 2, which is
// correct only for the hosted topology. On a single-proxy install it made honest
// clients — who send no forwarded header of their own, producing a chain of one
// — fall back to the proxy's address and share a single limiter key, locking
// them out, while a caller supplying one entry still chose its own key. That is
// strictly worse than not fixing anything, so each supported shape is pinned.
func TestLimiterAddrHonoursConfiguredHopCount(t *testing.T) {
	cases := []struct {
		name  string
		hops  int
		chain string // as it arrives at this process
		want  string // "" means "the peer address"
	}{
		{"two appending proxies, honest client", 2, simulatedClient + ", " + simulatedBalancer, simulatedClient},
		{"two appending proxies, caller prepends", 2, "9.9.9.9, " + simulatedClient + ", " + simulatedBalancer, simulatedClient},

		// One appending proxy: the honest chain is just the client, and it must
		// resolve to the client rather than collapsing onto the peer.
		{"one appending proxy, honest client", 1, simulatedClient, simulatedClient},
		{"one appending proxy, caller prepends", 1, "9.9.9.9, " + simulatedClient, simulatedClient},

		// Nothing appends: the header is caller-supplied in its entirety and
		// carries no evidence, so it must be ignored and the peer used.
		{"no proxy, no header", 0, "", ""},
		{"no proxy, caller supplies a full chain", 0, "9.9.9.9, 8.8.8.8", ""},
		{"no proxy, caller mimics the hosted shape", 0, simulatedClient + ", " + simulatedBalancer, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := handlerWithHops(tc.hops)
			e := engineAsProductionBuildsIt()
			e.GET("/probe", func(c *gin.Context) {
				c.String(http.StatusOK, "%s|%s", h.limiterAddr(c).String(), c.RemoteIP())
			})
			srv := httptest.NewServer(e)
			defer srv.Close()

			_, body := probeOnce(t, srv.Client(), "GET", srv.URL+"/probe", tc.chain)
			got, peer, ok := strings.Cut(body, "|")
			if !ok {
				t.Fatalf("malformed probe response %q", body)
			}
			want := tc.want
			if want == "" {
				want = peer
			}
			if got != want {
				t.Errorf("hops=%d chain=%q gave %s, want %s", tc.hops, tc.chain, got, want)
			}
		})
	}
}

// TestUnconfiguredHandlerDoesNotReadHeaderAsIfNothingAppends pins that a
// Handler nobody configured behaves as the default topology rather than as
// hops=0. The distinction matters because 0 is a meaningful setting, so a
// zero-valued field must not be mistaken for it.
func TestUnconfiguredHandlerDoesNotReadHeaderAsIfNothingAppends(t *testing.T) {
	h := &Handler{} // never told its shape
	e := engineAsProductionBuildsIt()
	e.GET("/probe", func(c *gin.Context) {
		c.String(http.StatusOK, "%s", h.limiterAddr(c).String())
	})
	srv := httptest.NewServer(e)
	defer srv.Close()

	_, got := probeOnce(t, srv.Client(), "GET", srv.URL+"/probe", simulateBalancer("9.9.9.9"))
	if got != simulatedClient {
		t.Errorf("unconfigured handler resolved %s, want %s: an unset hop count must mean "+
			"the default, not the explicit 0 that ignores the header", got, simulatedClient)
	}
}

// newResetRoute builds the real /auth/password/reset route: the real Handler,
// the real Service, the real limiter. Requests past the cap are refused by the
// limiter before any database access, which is what makes this reachable
// without one. Requests below the cap continue into the repo layer, which is
// absent here, so Recovery turns those into 500s — the 429/500 split is the
// signal this test reads.
func newResetRoute(t *testing.T, lim *autologin.MemoryLimiter) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	e := gin.New()
	e.Use(gin.Recovery())
	h := &Handler{svc: &Service{limiter: lim}}
	h.SetProxyHops(2) // the shape simulateBalancer produces
	h.Register(e)
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	return srv
}

func postReset(t *testing.T, srv *httptest.Server, fwd string) int {
	t.Helper()
	body := strings.NewReader(`{"token":"not-a-real-token","password":"correct-horse-battery"}`)
	req, err := http.NewRequest("POST", srv.URL+"/auth/password/reset", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", fwd)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestResetPasswordRouteKeysOnAppendedClient drives the REGISTERED route, not a
// stand-in handler. Reverting the decision site to clientAddr makes this fail.
func TestResetPasswordRouteKeysOnAppendedClient(t *testing.T) {
	attempts := resetPerMinute * 4

	run := func(vary bool) (refused int) {
		lim := autologin.NewMemoryLimiter()
		defer lim.Stop()
		srv := newResetRoute(t, lim)
		for i := 0; i < attempts; i++ {
			callerSupplied := ""
			if vary {
				callerSupplied = fmt.Sprintf("203.0.113.%d", i+1)
			}
			if postReset(t, srv, simulateBalancer(callerSupplied)) == http.StatusTooManyRequests {
				refused++
			}
		}
		return refused
	}

	variedRefused := run(true)
	ctrlRefused := run(false)
	t.Logf("through the registered route: varied refused=%d | control refused=%d (cap=%d/min over %d attempts)",
		variedRefused, ctrlRefused, resetPerMinute, attempts)

	if ctrlRefused == 0 {
		t.Fatalf("CONTROL BROKEN: the route refused nothing on the control arm; this test proves nothing")
	}
	if variedRefused != ctrlRefused {
		t.Errorf("varying the caller-supplied prefix changed the outcome through the real route: "+
			"refused %d vs control %d", variedRefused, ctrlRefused)
	}
}

// TestResetRouteSeparatesDistinctClients is the over-fire guard, and it needs
// TWO legitimate clients to work. A remedy that keyed every caller onto one
// value would close the bypass and pass every single-client test in this file
// while locking out the entire fleet; this is the pin that refuses it.
func TestResetRouteSeparatesDistinctClients(t *testing.T) {
	lim := autologin.NewMemoryLimiter()
	defer lim.Stop()
	srv := newResetRoute(t, lim)

	// Each client sends exactly its own cap. Independent buckets ⇒ nothing is
	// refused. A shared bucket ⇒ the second client is refused outright.
	var refusedFor = map[string]int{}
	for _, client := range []string{simulatedClient, simulatedOtherClient} {
		for i := 0; i < resetPerMinute; i++ {
			if postReset(t, srv, simulateBalancerFor(client, "")) == http.StatusTooManyRequests {
				refusedFor[client]++
			}
		}
	}
	t.Logf("refusals within cap: %s=%d %s=%d",
		simulatedClient, refusedFor[simulatedClient], simulatedOtherClient, refusedFor[simulatedOtherClient])

	for client, n := range refusedFor {
		if n > 0 {
			t.Errorf("OVER-FIRES: client %s was refused %d times inside its own cap of %d; "+
				"distinct clients are sharing a bucket", client, n, resetPerMinute)
		}
	}
}

// TestTwoFAPerIPLimiterKeysOnAppendedClient covers the 2FA key format and
// constant. The registered 2FA routes reach their limiter only after a
// challenge lookup, so this drives the limiter directly; the wiring of those
// three sites is pinned by TestDecisionSitesUseLimiterAddr instead.
func TestTwoFAPerIPLimiterKeysOnAppendedClient(t *testing.T) {
	attempts := twoFAIPLockoutPerMinute * 4

	run := func(vary bool) (allowed, refused int) {
		lim := autologin.NewMemoryLimiter()
		defer lim.Stop()
		e := engineAsProductionBuildsIt()
		e.POST("/auth/2fa/verify", func(c *gin.Context) {
			ip := defaultHopsHandler.limiterAddr(c)
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

		for i := 0; i < attempts; i++ {
			callerSupplied := ""
			if vary {
				callerSupplied = fmt.Sprintf("198.51.100.%d", (i%254)+1)
			}
			code, _ := probeOnce(t, srv.Client(), "POST", srv.URL+"/auth/2fa/verify", simulateBalancer(callerSupplied))
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
		t.Fatalf("CONTROL BROKEN: the limiter refused nothing on the control arm (allowed=%d of %d)",
			ctrlAllowed, attempts)
	}
	if variedRefused == 0 {
		t.Errorf("limiter did not bind: %d of %d attempts from ONE peer were allowed", variedAllowed, attempts)
	}
	if variedAllowed != ctrlAllowed {
		t.Errorf("varying the caller-supplied prefix changed the outcome: allowed %d vs control %d",
			variedAllowed, ctrlAllowed)
	}
}

// decisionSites are the handlers whose address feeds a rate-limit decision.
var decisionSites = map[string][]string{
	"handler.go":       {"resetPassword"},
	"twofa_handler.go": {"twoFATOTPComplete", "twoFARecoveryComplete", "twoFAWebAuthnFinish"},
}

// TestDecisionSitesUseLimiterAddr pins the wiring of every decision site,
// including the three that a unit test cannot reach without a database.
//
// Without this, reverting those three to clientAddr reintroduces the defect and
// every test in this package still passes.
func TestDecisionSitesUseLimiterAddr(t *testing.T) {
	fset := token.NewFileSet()
	checked := 0

	for file, funcs := range decisionSites {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		want := map[string]bool{}
		for _, fn := range funcs {
			want[fn] = true
		}

		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Name == nil || !want[fd.Name.Name] {
				continue
			}
			delete(want, fd.Name.Name)
			checked++

			var uses []string
			note := func(name string) {
				if name == "limiterAddr" || name == "clientAddr" {
					uses = append(uses, name)
				}
			}
			ast.Inspect(fd, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.Ident: // clientAddr(c)
					note(fun.Name)
				case *ast.SelectorExpr: // h.limiterAddr(c)
					note(fun.Sel.Name)
				}
				return true
			})

			for _, u := range uses {
				if u == "clientAddr" {
					t.Errorf("%s: %s calls clientAddr; a decision site must use limiterAddr, "+
						"because clientAddr resolves an entry the caller supplies", file, fd.Name.Name)
				}
			}
			if len(uses) == 0 {
				t.Errorf("%s: %s derives no address at all; expected a limiterAddr call",
					file, fd.Name.Name)
			}
		}

		for fn := range want {
			t.Errorf("%s: decision site %s not found — it was renamed or removed, and this pin "+
				"stopped covering it", file, fn)
		}
	}

	if checked != 4 {
		t.Fatalf("checked %d decision sites, want 4; the pin is not covering what it claims", checked)
	}
}
