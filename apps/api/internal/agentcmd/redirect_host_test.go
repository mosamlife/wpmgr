package agentcmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/net/idna"
)

// foldEqualASCIIDifferent are host pairs Unicode case folding treats as equal
// whose IDNA lookup forms differ: each pair names two registrable domains.
var foldEqualASCIIDifferent = [][2]string{
	{"straße.de", "straẞe.de"}, // capital sharp s maps to "ss"
	{"σ.gr", "ς.gr"},           // final sigma keeps its own label
}

// TestSameHostHTTPSUpgrade_ComparesTheDialledForm: the https retry is followed
// only when both hosts reduce to the same ASCII form. A pair that
// strings.EqualFold calls equal but that converts to two different domains is
// refused; spellings that convert to one domain are still followed.
func TestSameHostHTTPSUpgrade_ComparesTheDialledForm(t *testing.T) {
	for _, pair := range foldEqualASCIIDifferent {
		a, errA := idna.Lookup.ToASCII(pair[0])
		b, errB := idna.Lookup.ToASCII(pair[1])
		if !strings.EqualFold(pair[0], pair[1]) || errA != nil || errB != nil || a == b {
			t.Fatalf("precondition: %q and %q must fold equal and convert apart (%q %v, %q %v)", pair[0], pair[1], a, errA, b, errB)
		}
		from := mustParse("http://" + pair[0] + "/p")
		if _, ok := sameHostHTTPSUpgrade(from, "https://"+pair[1]+"/p"); ok {
			t.Errorf("sameHostHTTPSUpgrade(%q -> %q) followed a redirect to another domain (%s vs %s)", pair[0], pair[1], a, b)
		}
	}

	for _, c := range []struct{ from, loc string }{
		{"http://bücher.de/p", "https://BÜCHER.de/p"},
		{"http://bücher.de/p", "https://xn--bcher-kva.de/p"},
		{"http://[::1]:8080/p", "https://[::1]:8080/p"},
		{"http://127.0.0.1:8080/p", "https://127.0.0.1:8080/p"},
	} {
		if _, ok := sameHostHTTPSUpgrade(mustParse(c.from), c.loc); !ok {
			t.Errorf("sameHostHTTPSUpgrade(%q, %q) refused a same-host upgrade", c.from, c.loc)
		}
	}

	// A host IDNA refuses is never the same host, so no retry is sent.
	if _, ok := sameHostHTTPSUpgrade(mustParse("http://my_site.test/p"), "https://my_site.test/p"); ok {
		t.Error("sameHostHTTPSUpgrade followed an upgrade for a host that does not convert")
	}
}

// TestRedirectSuggestion_ComparesTheDialledForm: a redirect to the "www."
// sibling of a folded spelling of the saved host suggests nothing, and its
// copy offers no automatic update.
func TestRedirectSuggestion_ComparesTheDialledForm(t *testing.T) {
	re := redirectFor(t, "ping", "https://straße.de", "https://www.straẞe.de"+pingRoute, 301)
	if re.SuggestedSiteURL != "" {
		t.Errorf("SuggestedSiteURL = %q, want none for another domain", re.SuggestedSiteURL)
	}
	if got := re.Explanation(); strings.Contains(got, "updates to") || !strings.Contains(got, exemptAdvice) {
		t.Errorf("Explanation = %q, want the exempt advice and no update", got)
	}
}

// TestRedirectExplanation_AdoptNamesTheStoredAddress: case A names the
// address that would be stored, not the Location as sent, when the two
// differ (an explicit default port).
func TestRedirectExplanation_AdoptNamesTheStoredAddress(t *testing.T) {
	re := redirectFor(t, "ping", "https://x.test", "https://www.x.test:443"+pingRoute, 301)
	if re.SuggestedSiteURL != "https://www.x.test" {
		t.Fatalf("SuggestedSiteURL = %q, want https://www.x.test", re.SuggestedSiteURL)
	}
	want := "https://x.test redirects to https://www.x.test:443, so no command was sent. If WordPress on the site reports https://www.x.test as its address, the saved address updates to https://www.x.test automatically at a later check-in from the site. That update does not happen while another site in this workspace uses https://www.x.test: if one does, remove or change the duplicate site."
	if got := re.Explanation(); got != want {
		t.Errorf("Explanation:\n got %q\nwant %q", got, want)
	}
	for _, bad := range []string{"reports https://www.x.test:443", "updates to https://www.x.test:443", "uses https://www.x.test:443"} {
		if strings.Contains(re.Explanation(), bad) {
			t.Errorf("Explanation names the raw target: contains %q", bad)
		}
	}
}

// TestRedirectExplanation_AdoptStatesTheDuplicateCondition: the copy is
// rendered when the command fails, before any adoption is attempted, so it
// cannot know whether another site already holds the address. It must hold
// either way: it names the duplicate condition and what to do about it, and
// never promises the update unconditionally.
func TestRedirectExplanation_AdoptStatesTheDuplicateCondition(t *testing.T) {
	for _, c := range []struct{ saved, loc, to string }{
		{"https://example.com", "https://www.example.com" + pingRoute, "https://www.example.com"},
		{"http://example.com", "https://example.com" + pingRoute, "https://example.com"},
	} {
		got := redirectFor(t, "ping", c.saved, c.loc, 301).Explanation()
		for _, want := range []string{
			"updates to " + c.to + " automatically",
			"That update does not happen while another site in this workspace uses " + c.to,
			"remove or change the duplicate site.",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: Explanation %q lacks %q", c.saved, got, want)
			}
		}
		if strings.HasSuffix(got, "check-in.") || strings.Contains(got, "next daily check-in") {
			t.Errorf("%s: Explanation promises the update unconditionally: %q", c.saved, got)
		}
		if strings.ContainsAny(got, "–—") {
			t.Errorf("%s: Explanation contains an en or em dash: %q", c.saved, got)
		}
	}
}

// TestCommandPingOK: true only for a 2xx the agent answered with ok: true; a
// redirect, including a same-host upgrade offered by an https address, is
// false and nothing is sent to its target.
func TestCommandPingOK(t *testing.T) {
	c := realCommandClient(t)
	ctx := context.Background()

	agent := newAgentLikeServer(t, true)
	if !c.CommandPingOK(ctx, uuid.New(), agent.srv.URL) {
		t.Error("CommandPingOK = false for an agent answering 2xx ok:true")
	}

	target := newAgentLikeServer(t, true)
	origin := newRedirectingServer(t, true, http.StatusMovedPermanently, func() string { return target.srv.URL + pingRoute })
	if c.CommandPingOK(ctx, uuid.New(), origin.srv.URL) {
		t.Error("CommandPingOK = true for a redirect")
	}
	if n := target.hits.Load(); n != 0 {
		t.Errorf("redirect target received %d requests, want 0", n)
	}

	notAgent := httpServerAnswering(t, http.StatusOK, `{"ok":false}`)
	if c.CommandPingOK(ctx, uuid.New(), notAgent) {
		t.Error("CommandPingOK = true for a 2xx without ok:true")
	}
	failing := httpServerAnswering(t, http.StatusInternalServerError, `{}`)
	if c.CommandPingOK(ctx, uuid.New(), failing) {
		t.Error("CommandPingOK = true for a 5xx")
	}
}

// httpServerAnswering is a TLS server answering every request with status
// and body. It returns the server's URL.
func httpServerAnswering(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}
